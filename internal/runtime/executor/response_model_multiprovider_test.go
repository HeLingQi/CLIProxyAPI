package executor

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type multiProviderUsageCapture struct {
	alias   string
	records chan coreusage.Record
}

func (c *multiProviderUsageCapture) HandleUsage(_ context.Context, record coreusage.Record) {
	if record.Alias != c.alias {
		return
	}
	select {
	case c.records <- record:
	default:
	}
}

type multiProviderNoopUsagePlugin struct{}

func (multiProviderNoopUsagePlugin) HandleUsage(context.Context, coreusage.Record) {}

func (c *multiProviderUsageCapture) await(t *testing.T) coreusage.Record {
	t.Helper()
	select {
	case record := <-c.records:
		return record
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the usage record")
		return coreusage.Record{}
	}
}

func TestClaudeUsageRecordCarriesResponseModel(t *testing.T) {
	const alias = "claude-response-model-test"
	capture := &multiProviderUsageCapture{alias: alias, records: make(chan coreusage.Record, 4)}
	coreusage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() {
		coreusage.RegisterNamedPlugin(t.Name(), multiProviderNoopUsagePlugin{})
	})

	ctx := coreusage.WithRequestedModelAlias(context.Background(), alias)
	auth := &cliproxyauth.Auth{ID: "claude-auth-1", Index: "auth-claude-1", Provider: "claude"}
	reporter := helps.NewExecutorUsageReporter(ctx, NewClaudeExecutor(&config.Config{}), "claude-opus-5", auth)

	reporter.ObserveResponseModel([]byte(`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5"}}`))
	reporter.Publish(ctx, coreusage.Detail{InputTokens: 10, OutputTokens: 20})

	record := capture.await(t)
	if record.Model != "claude-opus-5" {
		t.Fatalf("record model = %q, want claude-opus-5", record.Model)
	}
	if record.ResponseModel != "claude-sonnet-5" {
		t.Fatalf("record response model = %q, want claude-sonnet-5", record.ResponseModel)
	}
}

func TestGeminiUsageRecordCarriesResponseModel(t *testing.T) {
	const alias = "gemini-response-model-test"
	capture := &multiProviderUsageCapture{alias: alias, records: make(chan coreusage.Record, 4)}
	coreusage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() {
		coreusage.RegisterNamedPlugin(t.Name(), multiProviderNoopUsagePlugin{})
	})

	ctx := coreusage.WithRequestedModelAlias(context.Background(), alias)
	auth := &cliproxyauth.Auth{ID: "gemini-auth-1", Index: "auth-gemini-1", Provider: "gemini"}
	reporter := helps.NewExecutorUsageReporter(ctx, NewGeminiExecutor(&config.Config{}), "gemini-2.5-pro", auth)

	reporter.ObserveResponseModel([]byte(`data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}],"modelVersion":"gemini-2.5-flash"}`))
	reporter.Publish(ctx, coreusage.Detail{InputTokens: 15, OutputTokens: 25})

	record := capture.await(t)
	if record.Model != "gemini-2.5-pro" {
		t.Fatalf("record model = %q, want gemini-2.5-pro", record.Model)
	}
	if record.ResponseModel != "gemini-2.5-flash" {
		t.Fatalf("record response model = %q, want gemini-2.5-flash", record.ResponseModel)
	}
}

func TestOpenAICompatUsageRecordCarriesResponseModel(t *testing.T) {
	const alias = "openai-response-model-test"
	capture := &multiProviderUsageCapture{alias: alias, records: make(chan coreusage.Record, 4)}
	coreusage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() {
		coreusage.RegisterNamedPlugin(t.Name(), multiProviderNoopUsagePlugin{})
	})

	ctx := coreusage.WithRequestedModelAlias(context.Background(), alias)
	auth := &cliproxyauth.Auth{ID: "openai-auth-1", Index: "auth-openai-1", Provider: "openai"}
	reporter := helps.NewExecutorUsageReporter(ctx, NewOpenAICompatExecutor("openai", &config.Config{}), "gpt-4o", auth)

	reporter.ObserveResponseModel([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o-2024-08-06","choices":[]}`))
	reporter.Publish(ctx, coreusage.Detail{InputTokens: 30, OutputTokens: 40})

	record := capture.await(t)
	if record.Model != "gpt-4o" {
		t.Fatalf("record model = %q, want gpt-4o", record.Model)
	}
	if record.ResponseModel != "gpt-4o-2024-08-06" {
		t.Fatalf("record response model = %q, want gpt-4o-2024-08-06", record.ResponseModel)
	}
}

func TestGeminiInteractionsUsageRecordCarriesResponseModel(t *testing.T) {
	const alias = "gemini-interactions-response-model-test"
	capture := &multiProviderUsageCapture{alias: alias, records: make(chan coreusage.Record, 4)}
	coreusage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() {
		coreusage.RegisterNamedPlugin(t.Name(), multiProviderNoopUsagePlugin{})
	})

	ctx := coreusage.WithRequestedModelAlias(context.Background(), alias)
	auth := &cliproxyauth.Auth{ID: "gemini-auth-interactions", Index: "auth-gemini-interactions-1", Provider: "gemini"}
	reporter := helps.NewExecutorUsageReporter(ctx, NewGeminiExecutor(&config.Config{}), "gemini-2.5-pro", auth)

	sseChunk := []byte("data: {\"event_type\":\"interaction.completed\",\"interaction\":{\"id\":\"i1\",\"status\":\"completed\",\"service_tier\":\"standard\",\"model\":\"gemini-3.1-flash-lite\",\"usage\":{\"total_input_tokens\":2,\"total_output_tokens\":3,\"total_tokens\":5}}}\n\n")
	reporter.ObserveResponseModel(sseChunk)
	detail, ok := helps.ParseInteractionsStreamUsage(sseChunk)
	if !ok {
		t.Fatalf("ParseInteractionsStreamUsage failed to parse detail")
	}
	reporter.Publish(ctx, detail)

	record := capture.await(t)
	if record.Model != "gemini-2.5-pro" {
		t.Fatalf("record model = %q, want gemini-2.5-pro", record.Model)
	}
	if record.ResponseModel != "gemini-3.1-flash-lite" {
		t.Fatalf("record response model = %q, want gemini-3.1-flash-lite", record.ResponseModel)
	}
}
