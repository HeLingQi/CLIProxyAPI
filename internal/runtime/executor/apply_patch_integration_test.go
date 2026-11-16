package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	chatresponses "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/openai/openai/responses"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const task6PatchRequest = `{"input":"patch","tools":[{"type":"custom","name":"apply_patch"}]}`

func task6ProviderFixture(provider, mode, toolName string) string {
	if mode == "empty" {
		return ""
	}
	args := `{"input":7,"secret":"RAW_SECRET"}`
	if mode == "eof" {
		args = `{"input":"`
	}
	switch provider {
	case "claude", "claude-oauth":
		if mode == "nonstream" {
			return fmt.Sprintf(`{"id":"r","type":"message","role":"assistant","content":[{"type":"tool_use","id":"c","name":%q,"input":%s}],"usage":{"input_tokens":5,"output_tokens":3}}`, toolName, args)
		}
		start := fmt.Sprintf("data: {\"type\":\"message_start\",\"message\":{\"id\":\"r\",\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":5}}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"c\",\"name\":%q,\"input\":{}}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":%q}}\n\n", toolName, args)
		if mode != "eof" {
			start += "data: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":3}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
		}
		return start
	case "gemini", "vertex", "antigravity":
		args = `{"input":7,"secret":"RAW_SECRET"}`
		if mode == "eof" {
			args = `{"input":"unfinished"}`
		}
		finish := `,"finishReason":"STOP"`
		if mode == "eof" {
			finish = ""
		}
		body := fmt.Sprintf(`{"responseId":"r","candidates":[{"content":{"parts":[{"functionCall":{"name":"apply_patch","args":%s}}]}%s}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3}}`, args, finish)
		if provider == "antigravity" {
			body = `{"response":` + body + `}`
		}
		if mode != "nonstream" {
			return "data: " + body + "\n\n"
		}
		return body
	case "gemini-interactions":
		if mode == "nonstream" {
			return fmt.Sprintf(`{"id":"r","steps":[{"type":"function_call","id":"c","name":"apply_patch","arguments":%q}],"usage":{"total_input_tokens":5,"total_output_tokens":3}}`, args)
		}
		body := fmt.Sprintf("data: {\"event_type\":\"interaction.created\",\"interaction\":{\"id\":\"r\"}}\n\ndata: {\"event_type\":\"step.start\",\"index\":0,\"step\":{\"type\":\"function_call\",\"id\":\"c\",\"name\":\"apply_patch\"}}\n\ndata: {\"event_type\":\"step.delta\",\"index\":0,\"delta\":{\"type\":\"function_call\",\"arguments\":%q}}\n\n", args)
		if mode != "eof" {
			body += "data: {\"event_type\":\"interaction.completed\",\"interaction\":{\"id\":\"r\"}}\n\ndata: [DONE]\n\n"
		}
		return body
	default:
		if mode == "nonstream" {
			return fmt.Sprintf(`{"id":"r","object":"chat.completion","choices":[{"message":{"tool_calls":[{"id":"c","type":"function","function":{"name":"apply_patch","arguments":%q}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`, args)
		}
		body := fmt.Sprintf("data: {\"id\":\"r\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"type\":\"function\",\"function\":{\"name\":\"apply_patch\",\"arguments\":%q}}]}}]}\n\n", args)
		if mode != "eof" {
			body += "data: {\"id\":\"r\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n"
		}
		return body
	}
}

func task6Executor(provider string) cliproxyauth.ProviderExecutor {
	cfg := &config.Config{}
	switch provider {
	case "claude", "claude-oauth":
		return NewClaudeExecutor(cfg)
	case "gemini":
		return NewGeminiExecutor(cfg)
	case "gemini-interactions":
		return NewGeminiInteractionsExecutor(cfg)
	case "vertex":
		return NewGeminiVertexExecutor(cfg)
	case "antigravity":
		return NewAntigravityExecutor(cfg)
	default:
		return NewOpenAICompatExecutor(provider, cfg)
	}
}

func assertTask6PatchError(t *testing.T, err error) {
	t.Helper()
	status, okStatus := err.(interface{ StatusCode() int })
	if !okStatus || status.StatusCode() != http.StatusBadGateway || err.Error() != "Invalid apply_patch tool arguments received from upstream." {
		t.Fatalf("expected clean 502, got %T %v", err, err)
	}
}

func assertTask6FailedStream(t *testing.T, chunks <-chan cliproxyexecutor.StreamChunk) {
	t.Helper()
	var output []byte
	errors := 0
	failed := 0
	for chunk := range chunks {
		output = append(output, chunk.Payload...)
		for _, line := range bytes.Split(chunk.Payload, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) && ojson.GetBytes(bytes.TrimSpace(line[5:]), "type").String() == "response.failed" {
				failed++
			}
		}
		if chunk.Err != nil {
			errors++
			assertTask6PatchError(t, chunk.Err)
		}
	}
	if failed != 1 || errors != 1 || bytes.Contains(output, []byte(`"type":"response.completed"`)) || bytes.Contains(output, []byte("[DONE]")) || bytes.Contains(output, []byte("RAW_SECRET")) || bytes.Contains(output, []byte(`"input":7`)) {
		t.Fatalf("failure contract: failed=%d errors=%d output=%s", failed, errors, output)
	}
}

func TestApplyPatchActualProviderErrorAndEOF(t *testing.T) {
	for _, provider := range []string{"custom-compat", "claude", "claude-oauth", "gemini", "gemini-interactions", "vertex", "antigravity"} {
		for _, mode := range []string{"nonstream", "stream", "eof", "empty", "scanner"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, errReadAll := io.ReadAll(r.Body)
					if errReadAll != nil {
						t.Error(errReadAll)
						return
					}
					name := "apply_patch"
					if strings.HasPrefix(provider, "claude") {
						name = ojson.GetBytes(body, "tools.0.name").String()
					}
					if mode != "nonstream" {
						w.Header().Set("Content-Type", "text/event-stream")
					}
					if mode == "scanner" {
						w.Header().Set("Content-Length", "999999")
					}
					_, _ = io.WriteString(w, task6ProviderFixture(provider, func() string {
						if mode == "scanner" {
							return "eof"
						}
						if strings.HasPrefix(provider, "claude") && mode == "nonstream" {
							return "stream"
						}
						return mode
					}(), name))
				}))
				defer server.Close()
				exec := task6Executor(provider)
				auth := &cliproxyauth.Auth{ID: "task6", Provider: exec.Identifier(), Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
				if provider == "claude-oauth" {
					auth.Attributes["api_key"] = "sk-ant-oat-test"
					auth.Metadata = map[string]any{"account_uuid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}
				}
				if provider == "antigravity" {
					auth.Metadata = map[string]any{"access_token": "test", "expires_in": 3600, "timestamp": "2099-01-01T00:00:00Z", "project_id": "test"}
				}
				checkUsage := task6CaptureFailureUsage(t, auth.ID)
				defer checkUsage()
				req := cliproxyexecutor.Request{Model: "gemini-3.1-pro-preview", Payload: []byte(task6PatchRequest)}
				if strings.HasPrefix(provider, "claude") {
					req.Model = "claude-sonnet-4-6"
				}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: req.Payload}
				if mode == "nonstream" {
					response, errExecute := exec.Execute(context.Background(), auth, req, opts)
					if len(response.Payload) != 0 {
						t.Errorf("failed response returned payload %s", response.Payload)
					}
					assertTask6PatchError(t, errExecute)
				} else {
					stream, errExecuteStream := exec.ExecuteStream(context.Background(), auth, req, opts)
					if errExecuteStream != nil {
						t.Fatal(errExecuteStream)
					}
					assertTask6FailedStream(t, stream.Chunks)
				}
			})
		}
	}
}

type task6UsageCapture struct {
	id      string
	records chan usage.Record
}

func (p *task6UsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if record.AuthID == p.id || record.RequestID == p.id+"-barrier" {
		p.records <- record
	}
}
func task6CaptureFailureUsage(t *testing.T, id string) func() {
	return task6CaptureFailureUsageWithCheck(t, id, nil)
}

func task6CaptureFailureUsageWithCheck(t *testing.T, id string, check func(usage.Record)) func() {
	t.Helper()
	capture := &task6UsageCapture{id: id, records: make(chan usage.Record, 32)}
	usage.RegisterNamedPlugin("task6-patch-failure", capture)
	return func() {
		usage.PublishRecord(context.Background(), usage.Record{RequestID: id + "-barrier"})
		count := 0
		for {
			select {
			case record := <-capture.records:
				if record.RequestID == id+"-barrier" {
					if count != 1 {
						t.Errorf("usage records=%d, want one", count)
					}
					return
				}
				count++
				if !record.Failed {
					t.Errorf("invalid patch published success usage: %+v", record)
				}
				if check != nil {
					check(record)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("usage barrier did not arrive")
			}
		}
	}
}

func task6Gateway(t *testing.T, exec cliproxyauth.ProviderExecutor, auth *cliproxyauth.Auth, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)
	auth.Status = cliproxyauth.StatusActive
	if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, exec.Identifier(), []*registry.ModelInfo{{ID: "task6-public-patch"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	h := openai.NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.POST("/v1/responses", h.Responses)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"task6-public-patch","input":"patch","stream":%v,"tools":[{"type":"custom","name":"apply_patch"}]}`, stream)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "Codex Desktop/26.803.41515")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestApplyPatchHTTPGatewayErrorMatrix(t *testing.T) {
	for _, provider := range []string{"custom-compat", "claude", "claude-oauth", "gemini", "gemini-interactions", "vertex", "antigravity"} {
		for _, mode := range []string{"nonstream", "stream", "eof"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, errReadAll := io.ReadAll(r.Body)
					if errReadAll != nil {
						t.Error(errReadAll)
						return
					}
					name := "apply_patch"
					actualMode := mode
					if strings.HasPrefix(provider, "claude") {
						name = ojson.GetBytes(body, "tools.0.name").String()
						if mode == "nonstream" {
							actualMode = "stream"
						}
					}
					if actualMode != "nonstream" {
						w.Header().Set("Content-Type", "text/event-stream")
					}
					_, _ = io.WriteString(w, task6ProviderFixture(provider, actualMode, name))
				}))
				defer server.Close()
				exec := task6Executor(provider)
				auth := &cliproxyauth.Auth{ID: "task6-http-" + provider + mode, Provider: exec.Identifier(), Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
				if provider == "claude-oauth" {
					auth.Attributes["api_key"] = "sk-ant-oat-test"
					auth.Metadata = map[string]any{"account_uuid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}
				}
				if provider == "antigravity" {
					auth.Metadata = map[string]any{"access_token": "test", "expires_in": 3600, "timestamp": "2099-01-01T00:00:00Z", "project_id": "test"}
				}
				checkUsage := task6CaptureFailureUsage(t, auth.ID)
				defer checkUsage()
				result := task6Gateway(t, exec, auth, mode != "nonstream")
				body := result.Body.String()
				if mode == "nonstream" {
					if result.Code != http.StatusBadGateway || !strings.Contains(body, "Invalid apply_patch tool arguments received from upstream.") {
						t.Fatalf("nonstream did not return clean 502: %d %s", result.Code, body)
					}
				} else if result.Code != http.StatusOK || strings.Count(body, "event: response.failed") != 1 || strings.Contains(body, "event: response.completed") || strings.Contains(body, "data: [DONE]") || strings.Contains(body, "event: error") {
					t.Fatalf("HTTP stream failure contract: status=%d body=%s", result.Code, body)
				}
				if strings.Contains(body, "RAW_SECRET") || strings.Contains(body, `"input":7`) {
					t.Fatal("raw invalid arguments leaked over HTTP")
				}
			})
		}
	}
}

// task6DrainUsage synchronizes the asynchronous dispatcher before changing observers.
func task6DrainUsage(t *testing.T) {
	t.Helper()
	id := "task6-drain-" + t.Name()
	capture := &task6UsageCapture{id: id, records: make(chan usage.Record, 1)}
	usage.RegisterNamedPlugin("task6-patch-failure", capture)
	usage.PublishRecord(context.Background(), usage.Record{RequestID: id + "-barrier"})
	select {
	case <-capture.records:
	case <-time.After(3 * time.Second):
		t.Fatal("usage drain barrier did not arrive")
	}
}

func TestApplyPatchSDKOriginalRequestFallback(t *testing.T) {
	for _, provider := range []string{"custom-compat", "gemini", "gemini-interactions", "vertex"} {
		t.Run(provider, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, task6ProviderFixture(provider, "nonstream", "apply_patch"))
			}))
			defer server.Close()
			exec := task6Executor(provider)
			auth := &cliproxyauth.Auth{Provider: exec.Identifier(), Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
			response, errExecute := exec.Execute(t.Context(), auth, cliproxyexecutor.Request{Model: "gemini-3.1-pro-preview", Payload: []byte(task6PatchRequest)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
			if len(response.Payload) > 0 {
				t.Error("SDK lost original declaration and returned successful raw arguments")
			}
			assertTask6PatchError(t, errExecute)
		})
	}
}

func TestApplyPatchFailureStopsConsumptionAndNextAttemptIsFresh(t *testing.T) {
	stopped := make(chan struct{})
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls == 0 {
			calls++
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, task6ProviderFixture("custom-compat", "stream", "apply_patch"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			close(stopped)
			return
		}
		_, _ = io.WriteString(w, strings.ReplaceAll(task6ProviderFixture("custom-compat", "nonstream", "apply_patch"), `\"input\":7,\"secret\":\"RAW_SECRET\"`, `\"input\":\"valid patch\"`))
	}))
	defer server.Close()
	exec := NewOpenAICompatExecutor("custom-compat", &config.Config{})
	auth := &cliproxyauth.Auth{Provider: "custom-compat", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{Model: "patch-model", Payload: []byte(task6PatchRequest)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: req.Payload}
	stream, errExecuteStream := exec.ExecuteStream(t.Context(), auth, req, opts)
	if errExecuteStream != nil {
		t.Fatal(errExecuteStream)
	}
	assertTask6FailedStream(t, stream.Chunks)
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("failed attempt kept consuming the upstream")
	}
	response, errExecute := exec.Execute(t.Context(), auth, req, opts)
	if errExecute != nil || ojson.GetBytes(response.Payload, "output.0.input").String() != "valid patch" {
		t.Fatalf("next attempt reused failed state: %s %v", response.Payload, errExecute)
	}
}

func TestApplyPatchNonStreamNativeNilWithoutErrorIs502(t *testing.T) {
	// Preserve the built-in request transform and restore the exact response transforms.
	sdktranslator.Register(sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI, nil, sdktranslator.ResponseTransform{
		Stream:    chatresponses.ConvertOpenAIChatCompletionsResponseToOpenAIResponses,
		NonStream: func(context.Context, string, []byte, []byte, []byte, *any) []byte { return nil },
	})
	defer sdktranslator.Register(sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI, nil, sdktranslator.ResponseTransform{
		Stream:    chatresponses.ConvertOpenAIChatCompletionsResponseToOpenAIResponses,
		NonStream: chatresponses.ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream,
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, task6ProviderFixture("custom-compat", "nonstream", "apply_patch"))
	}))
	defer server.Close()
	exec := NewOpenAICompatExecutor("custom-compat", &config.Config{})
	auth := &cliproxyauth.Auth{ID: "task6-nil-translation", Provider: "custom-compat", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
	checkUsage := task6CaptureFailureUsage(t, auth.ID)
	defer checkUsage()
	response, errExecute := exec.Execute(t.Context(), auth, cliproxyexecutor.Request{Model: "m", Payload: []byte(task6PatchRequest)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if len(response.Payload) != 0 {
		t.Errorf("native nil recovered via usage normalization: %s", response.Payload)
	}
	assertTask6PatchError(t, errExecute)
}

func TestApplyPatchInteractionsSourceFailureIsSealed(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", streaming), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !streaming {
					_, _ = io.WriteString(w, `{"id":"r","status":"failed","error":{"message":"RAW_SECRET"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"event_type\":\"interaction.created\",\"interaction\":{\"id\":\"r\"}}\n\ndata: {\"event_type\":\"interaction.failed\",\"interaction\":{\"id\":\"r\"},\"error\":{\"message\":\"RAW_SECRET\"}}\n\ndata: {\"event_type\":\"interaction.completed\",\"interaction\":{\"id\":\"r\",\"usage\":{\"total_input_tokens\":5}}}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			exec := NewGeminiInteractionsExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{ID: "task6-source-failed", Provider: "gemini-interactions", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
			checkUsage := task6CaptureFailureUsage(t, auth.ID)
			defer checkUsage()
			if !streaming {
				response, errExecute := exec.Execute(t.Context(), auth, cliproxyexecutor.Request{Model: "gemini-3.1-pro-preview", Payload: []byte(task6PatchRequest)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
				if len(response.Payload) > 0 {
					t.Errorf("upstream failed response returned success: %s", response.Payload)
				}
				assertTask6PatchError(t, errExecute)
				return
			}
			stream, errExecuteStream := exec.ExecuteStream(t.Context(), auth, cliproxyexecutor.Request{Model: "gemini-3.1-pro-preview", Payload: []byte(task6PatchRequest)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
			if errExecuteStream != nil {
				t.Fatal(errExecuteStream)
			}
			assertTask6FailedStream(t, stream.Chunks)

		})
	}
}
