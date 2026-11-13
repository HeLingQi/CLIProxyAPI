package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestPayloadBarrierCodexImageFilter(t *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		for _, stream := range []bool{false, true} {
			for _, model := range []string{"gpt-5.4", "gpt-5.5", "gpt-6-astra", "gpt-6-sol"} {
				t.Run(transport+"/"+model+map[bool]string{false: "/execute", true: "/stream"}[stream], func(t *testing.T) {
					captured := make(chan []byte, 1)
					completed := []byte(`{"type":"response.completed","response":{"id":"resp_barrier","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if transport == "websocket" {
							upgrader := websocket.Upgrader{}
							conn, errUpgrade := upgrader.Upgrade(w, r, nil)
							if errUpgrade != nil {
								t.Error(errUpgrade)
								return
							}
							defer func() {
								if errClose := conn.Close(); errClose != nil {
									t.Error(errClose)
								}
							}()
							_, body, errRead := conn.ReadMessage()
							if errRead != nil {
								t.Error(errRead)
								return
							}
							captured <- body
							if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
								t.Error(errWrite)
							}
							return
						}
						body, errRead := io.ReadAll(r.Body)
						if errRead != nil {
							t.Error(errRead)
							return
						}
						captured <- body
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write(append(append([]byte("data: "), completed...), '\n', '\n'))
					}))
					defer server.Close()
					cfg := &config.Config{Payload: config.PayloadConfig{Filter: []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "gpt-5.4"}, {Name: "gpt-6-astra"}}, Params: []string{`tools.#(type=="image_generation")#`, "instructions", "prompt_cache_key"}}}}}
					var executor cliproxyauth.ProviderExecutor = NewCodexExecutor(cfg)
					if transport == "websocket" {
						executor = NewCodexWebsocketsExecutor(cfg)
					}
					auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"api_key": "test", "base_url": server.URL, "plan_type": "pro"}}
					req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hello","prompt_cache_key":"injected-cache"}`)}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
					if stream {
						result, errStream := executor.ExecuteStream(context.Background(), auth, req, opts)
						if errStream != nil {
							t.Fatal(errStream)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, errExecute := executor.Execute(context.Background(), auth, req, opts); errExecute != nil {
						t.Fatal(errExecute)
					}
					body := <-captured
					filteredModel := model == "gpt-5.4" || model == "gpt-6-astra"
					image := ojson.GetBytes(body, `tools.#(type=="image_generation")`).Exists()
					if image != !filteredModel {
						t.Fatalf("image tool presence = %v: %s", image, body)
					}
					if filteredModel && (ojson.GetBytes(body, "instructions").Exists() || ojson.GetBytes(body, "prompt_cache_key").Exists()) {
						t.Fatalf("late injection undid filter: %s", body)
					}
				})
			}
		}
	}
}

func TestPayloadBarrierAntigravityRebuiltAttempts(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{
		Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gemini-*"}}, Params: map[string]any{"generationConfig.maxOutputTokens": 123, "toolConfig.functionCallingConfig.mode": "CUSTOM"}}},
		Filter:   []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "gemini-*"}}, Params: []string{"sessionId", "contents.0"}}},
	}}
	req := cliproxyexecutor.Request{Model: "gemini-3.1-pro-preview"}
	body := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"first"}]},{"role":"user","parts":[{"text":"second"}]}]}}`)
	ctx := helps.WithPayloadFinalizer(context.Background(), helps.NewPayloadFinalizer(cfg, "antigravity", req.Model, "antigravity", "request", body, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini}))
	executor := NewAntigravityExecutor(antigravityNoControlPlane(cfg))
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "project-test"}}
	for attempt := 0; attempt < 2; attempt++ {
		request, errBuild := executor.buildRequest(ctx, auth, "token", req.Model, body, attempt == 1, "", "https://example.invalid", "injected-session")
		if errBuild != nil {
			t.Fatal(errBuild)
		}
		wire, errRead := io.ReadAll(request.Body)
		if errRead != nil {
			t.Fatal(errRead)
		}
		if errClose := request.Body.Close(); errClose != nil {
			t.Fatal(errClose)
		}
		if ojson.GetBytes(wire, "request.generationConfig.maxOutputTokens").Int() != 123 {
			t.Fatalf("built-in cleanup undid override: %s", wire)
		}
		if ojson.GetBytes(wire, "request.sessionId").Exists() {
			t.Fatalf("session reinjected: %s", wire)
		}
		if got := ojson.GetBytes(wire, "request.contents.#").Int(); got != 1 {
			t.Fatalf("filter applied more than once: %s", wire)
		}
	}
}

func TestPayloadBarrierAIStudio(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{
		Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"generationConfig.maxOutputTokens": 900000, "generationConfig.thinkingConfig.thinkingLevel": "low"}}},
		Filter:   []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: []string{"contents"}}},
	}}
	executor := NewAIStudioExecutor(cfg, "aistudio", nil)
	for _, stream := range []bool{false, true} {
		body, _, errTranslate := executor.translateRequest(context.Background(), cliproxyexecutor.Request{Model: "gemini-3.1-pro-preview", Payload: []byte(`{"contents":[{"role":"model","parts":[{"text":"hi"}]}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini}, stream)
		if errTranslate != nil {
			t.Fatal(errTranslate)
		}
		if ojson.GetBytes(body, "contents").Exists() || ojson.GetBytes(body, "generationConfig.maxOutputTokens").Int() != 900000 || ojson.GetBytes(body, "generationConfig.thinkingConfig.thinkingLevel").String() != "low" {
			t.Fatalf("late normalization undid rules: %s", body)
		}
	}
}

func TestPayloadBarrierClaudeDoesNotReplayFiltersOrReinjectIdentity(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{
		Filter:   []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: []string{"system", "metadata", "diagnostics", "context_management", "messages.0"}}},
		Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"max_tokens": 1}}},
	}}
	for _, stream := range []bool{false, true} {
		body := executeClaudeContextManagementRequest(t, cfg, []byte(`{"model":"claude-opus-5","thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"second"},{"role":"user","content":"third"}]}`), stream)
		for _, path := range []string{"system", "metadata", "diagnostics", "context_management"} {
			if ojson.GetBytes(body, path).Exists() {
				t.Fatalf("%s was reinjected: %s", path, body)
			}
		}
		if ojson.GetBytes(body, "messages.#").Int() != 2 || ojson.GetBytes(body, "max_tokens").Int() != 1 {
			t.Fatalf("built-in processing changed final rule semantics: %s", body)
		}
	}
}

type captureEffectiveServiceTierPlugin struct {
	authID  string
	records chan usage.Record
}

func (p *captureEffectiveServiceTierPlugin) HandleUsage(_ context.Context, record usage.Record) {
	if p == nil || record.AuthID != p.authID {
		return
	}
	select {
	case p.records <- record:
	default:
	}
}

func TestPayloadBarrierEffectiveServiceTierUsageAfterOverride(t *testing.T) {
	cfg := &config.Config{
		Payload: config.PayloadConfig{
			Override: []config.PayloadRule{{
				Models: []config.PayloadModelRule{
					{Name: "gpt-*", Protocol: "codex"},
					{Name: "gpt-*", Protocol: "openai"},
					{Name: "grok-*", Protocol: "openai"},
					{Name: "*", Protocol: "devin"},
				},
				Params: map[string]any{"service_tier": "priority"},
			}},
		},
	}

	t.Run("codex oauth chat completions override", func(t *testing.T) {
		var captured capturedCodexRequest
		server := newCodexRoutingHintServer(t, &captured)
		defer server.Close()

		for _, stream := range []bool{false, true} {
			t.Run(map[bool]string{false: "execute", true: "stream"}[stream], func(t *testing.T) {
				authID := t.Name()
				plugin := &captureEffectiveServiceTierPlugin{
					authID:  authID,
					records: make(chan usage.Record, 1),
				}
				usage.RegisterPlugin(plugin)

				auth := codexOAuthTestAuth(server.URL)
				auth.ID = authID
				executor := NewCodexExecutor(cfg)
				ctx := usage.WithServiceTier(context.Background(), usage.AutoServiceTier)
				req := cliproxyexecutor.Request{
					Model:   "gpt-6-astra",
					Payload: []byte(`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}]}`),
				}
				opts := cliproxyexecutor.Options{
					SourceFormat: sdktranslator.FormatOpenAI,
					Stream:       stream,
				}
				if stream {
					result, errStream := executor.ExecuteStream(ctx, auth, req, opts)
					if errStream != nil {
						t.Fatalf("ExecuteStream error: %v", errStream)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatalf("stream chunk error: %v", chunk.Err)
						}
					}
				} else if _, errExec := executor.Execute(ctx, auth, req, opts); errExec != nil {
					t.Fatalf("Execute error: %v", errExec)
				}

				if captured.serviceTier.String() != "priority" {
					t.Fatalf("upstream body service_tier = %q, want priority", captured.serviceTier.String())
				}
				record := <-plugin.records
				if record.ServiceTier != "auto" || record.EffectiveServiceTier != "priority" || record.ResponseServiceTier != "default" {
					t.Fatalf("usage tiers = requested:%q effective:%q response:%q, want auto/priority/default", record.ServiceTier, record.EffectiveServiceTier, record.ResponseServiceTier)
				}
			})
		}
	})

	t.Run("openai compat chat completions override", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if got := ojson.GetBytes(body, "service_tier").String(); got != "priority" {
				t.Errorf("upstream body service_tier = %q, want priority", got)
			}
			if r.Header.Get("Accept") == "text/event-stream" {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5.4\",\"service_tier\":\"default\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","model":"gpt-5.4","service_tier":"default","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		}))
		defer server.Close()

		for _, stream := range []bool{false, true} {
			t.Run(map[bool]string{false: "execute", true: "stream"}[stream], func(t *testing.T) {
				authID := t.Name()
				plugin := &captureEffectiveServiceTierPlugin{
					authID:  authID,
					records: make(chan usage.Record, 1),
				}
				usage.RegisterPlugin(plugin)

				auth := &cliproxyauth.Auth{
					ID:         authID,
					Provider:   "openai-compatibility",
					Attributes: map[string]string{"base_url": server.URL, "api_key": "sk-test"},
				}
				executor := NewOpenAICompatExecutor("openai-compatibility", cfg)
				ctx := usage.WithServiceTier(context.Background(), usage.AutoServiceTier)
				req := cliproxyexecutor.Request{
					Model:   "gpt-5.4",
					Payload: []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}`),
				}
				opts := cliproxyexecutor.Options{
					SourceFormat: sdktranslator.FormatOpenAI,
					Stream:       stream,
				}
				if stream {
					result, errStream := executor.ExecuteStream(ctx, auth, req, opts)
					if errStream != nil {
						t.Fatalf("ExecuteStream error: %v", errStream)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatalf("stream chunk error: %v", chunk.Err)
						}
					}
				} else if _, errExec := executor.Execute(ctx, auth, req, opts); errExec != nil {
					t.Fatalf("Execute error: %v", errExec)
				}

				record := <-plugin.records
				if record.ServiceTier != "auto" || record.EffectiveServiceTier != "priority" || record.ResponseServiceTier != "default" {
					t.Fatalf("usage tiers = requested:%q effective:%q response:%q, want auto/priority/default", record.ServiceTier, record.EffectiveServiceTier, record.ResponseServiceTier)
				}
			})
		}
	})
}
