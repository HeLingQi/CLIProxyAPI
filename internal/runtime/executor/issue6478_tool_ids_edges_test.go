package executor

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestIssue6478_ClaudeLegacyEncodingCollision(t *testing.T) {
	ids := []string{issue6478Calls[0].id, issue6478Calls[1].id, "cpa_tid_v1_V3JpdGU6MSNlMWYwZGE4MTE0MzY0OWYzYTM3OGQyNGFmNmZjM2VjOQ", "call_8jAZYBK5veMdr1DD2DqObARK"}
	for _, stream := range []bool{false, true} {
		body := executeClaudeContextManagementRequest(t, &config.Config{}, issue6478History(ids), stream)
		uses := ojson.GetBytes(body, "messages.1.content").Array()
		results := ojson.GetBytes(body, "messages.2.content").Array()
		if len(uses) != 4 || len(results) != 4 {
			t.Fatalf("history lost calls/results: %s", body)
		}
		seen := make(map[string]bool)
		for i, use := range uses {
			id := use.Get("id").String()
			if !regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(id) || seen[id] {
				t.Errorf("legacy normalization produced invalid or colliding ID %q (stream=%v)", id, stream)
			}
			seen[id] = true
			if results[i].Get("tool_use_id").String() != id {
				t.Errorf("legacy collision mapping broke pair[%d]", i)
			}
			if i >= 2 && id != ids[i] {
				t.Errorf("legal/encoded history ID was re-encoded: %q, want %q", id, ids[i])
			}
		}
		// Repairing a repaired transcript must be byte-stable at schema ID fields.
		again := executeClaudeContextManagementRequest(t, &config.Config{}, body, stream)
		if ojson.GetBytes(again, "messages.1.content").Raw != ojson.GetBytes(body, "messages.1.content").Raw {
			t.Errorf("history ID repair is not idempotent")
		}
	}
}

func TestIssue6478_ClaudeUpstreamCountTokensRepairBeforePayload(t *testing.T) {
	ids := []string{issue6478Calls[0].id, issue6478Calls[1].id, issue6478Calls[2].id, issue6478Calls[3].id}
	for _, configured := range []bool{false, true} {
		cfg := &config.Config{}
		if configured {
			cfg.Payload = config.PayloadConfig{Override: []config.PayloadRule{{
				Models: []config.PayloadModelRule{{Name: "*", NotMatch: []map[string]any{{"messages.1.content.0.id": ids[0]}}}},
				Params: map[string]any{"tools.0.description": "observed repaired ID", "messages.1.content.0.id": "configured:id#override", "messages.2.content.0.tool_use_id": "configured:id#override"},
			}}}
		}
		var body []byte
		transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host != "api.anthropic.com" || req.URL.Path != "/v1/messages/count_tokens" {
				t.Errorf("wrong token-count boundary: %s", req.URL)
			}
			var errRead error
			body, errRead = io.ReadAll(req.Body)
			if errRead != nil {
				return nil, errRead
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"input_tokens":12}`)), Request: req}, nil
		})
		ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
		auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test"}}
		_, errCount := NewClaudeExecutor(cfg).CountTokens(ctx, auth, cliproxyexecutor.Request{Model: "claude-opus-5", Payload: issue6478History(ids)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
		if errCount != nil {
			t.Fatal(errCount)
		}
		if configured {
			if ojson.GetBytes(body, "tools.0.description").String() != "observed repaired ID" || ojson.GetBytes(body, "messages.1.content.0.id").String() != "configured:id#override" || ojson.GetBytes(body, "messages.2.content.0.tool_use_id").String() != "configured:id#override" {
				t.Errorf("count_tokens repair must precede final user rules: %s", body)
			}
		} else {
			useIDs := ojson.GetBytes(body, "messages.1.content.#.id").Array()
			resultIDs := ojson.GetBytes(body, "messages.2.content.#.tool_use_id").Array()
			if len(useIDs) != 4 || len(resultIDs) != 4 {
				t.Fatalf("count_tokens lost history calls/results: %s", body)
			}
			for i, id := range useIDs {
				if !regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(id.String()) {
					t.Errorf("count_tokens sent illegal ID[%d] = %q", i, id.String())
				}
				if resultIDs[i].String() != id.String() {
					t.Errorf("count_tokens call/result pair[%d] differs: %q vs %q", i, id.String(), resultIDs[i].String())
				}
				if i >= 2 && id.String() != ids[i] {
					t.Errorf("count_tokens changed legal ID[%d]: %q", i, id.String())
				}
			}
		}
	}
}
