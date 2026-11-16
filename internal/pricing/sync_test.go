package pricing

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type pricingRoundTripper func(*http.Request) (*http.Response, error)

func (f pricingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestLoadBaseLLMModelsMatchesConfiguredModelsAndReportsUnmatched(t *testing.T) {
	client := &http.Client{Transport: pricingRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != BaseLLMPricingSource {
			t.Fatalf("URL = %s", request.URL)
		}
		body := `{"success":true,"data":{"billing_expr":{"GPT-X":"tier(\"standard\", p * 2)","unused":"tier(\"standard\", p * 1)"},"billing_mode":{"GPT-X":"tiered_expr"}}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	models, summary, errLoad := LoadBaseLLMModels(context.Background(), client, []SyncTarget{{Provider: "codex", Model: "gpt-x"}, {Provider: "gemini", Model: "missing"}})
	if errLoad != nil {
		t.Fatalf("LoadBaseLLMModels() error = %v", errLoad)
	}
	if len(models) != 1 || models[0].Provider != "codex" || models[0].Source != BaseLLMPricingSource {
		t.Fatalf("models = %+v", models)
	}
	if summary.ConfiguredModels != 2 || summary.Matched != 1 || summary.Updated != 1 || len(summary.Unmatched) != 1 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestLoadBaseLLMModelsRejectsInvalidExpressionWithoutPartialResult(t *testing.T) {
	client := &http.Client{Transport: pricingRoundTripper(func(*http.Request) (*http.Response, error) {
		body := `{"success":true,"data":{"billing_expr":{"good":"tier(\"standard\", p * 2)","bad":"shell(\"x\")"}}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	models, _, errLoad := LoadBaseLLMModels(context.Background(), client, []SyncTarget{{Provider: "codex", Model: "good"}})
	if errLoad == nil || len(models) != 0 {
		t.Fatalf("models = %+v, err = %v", models, errLoad)
	}
}
