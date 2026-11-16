package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagehistory"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestGetUsageHistorySummaryReturnsPersistedTotals(t *testing.T) {
	if errConfigure := kvstore.ConfigureLocal(true, filepath.Join(t.TempDir(), "usage-history.db")); errConfigure != nil {
		t.Fatalf("ConfigureLocal() error = %v", errConfigure)
	}
	t.Cleanup(func() { _ = kvstore.ShutdownLocal() })

	history := usagehistory.New(func() bool { return true })
	t.Cleanup(func() { _ = history.Close(context.Background()) })
	history.HandleUsage(context.Background(), coreusage.Record{
		Provider:     "codex",
		ExecutorType: "codex",
		RequestedAt:  time.Date(2026, time.November, 12, 10, 0, 0, 0, time.UTC),
		Latency:      90 * time.Millisecond,
		TTFT:         30 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:  7,
			OutputTokens: 3,
			TotalTokens:  10,
		},
	})

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/summary?from=2026-11-12&to=2026-11-12", nil)

	handler := &Handler{}
	handler.SetUsageHistory(history)
	handler.GetUsageHistorySummary(ginContext)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	body := recorder.Body.String()
	if got := ojson.Get(body, "timezone").String(); got != "UTC" {
		t.Fatalf("timezone = %q, want UTC", got)
	}
	if got := ojson.Get(body, "totals.requests").Int(); got != 1 {
		t.Fatalf("totals.requests = %d, want 1", got)
	}
	if got := ojson.Get(body, "totals.total_tokens").Int(); got != 10 {
		t.Fatalf("totals.total_tokens = %d, want 10", got)
	}
}

func TestGetUsageHistoryEventsReturnsRedactedRecords(t *testing.T) {
	if errConfigure := kvstore.ConfigureLocal(true, filepath.Join(t.TempDir(), "usage-events.db")); errConfigure != nil {
		t.Fatalf("ConfigureLocal() error = %v", errConfigure)
	}
	t.Cleanup(func() { _ = kvstore.ShutdownLocal() })

	history := usagehistory.New(func() bool { return true })
	t.Cleanup(func() { _ = history.Close(context.Background()) })
	history.HandleUsage(context.Background(), coreusage.Record{
		RequestID:   "execution-1",
		Provider:    "codex",
		Model:       "gpt-5.4",
		APIKey:      "sk-secret-value",
		RequestedAt: time.Date(2026, time.November, 12, 10, 0, 0, 0, time.UTC),
		Detail:      coreusage.Detail{InputTokens: 7, OutputTokens: 3, TotalTokens: 10},
	})

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/events?limit=10", nil)
	handler := &Handler{}
	handler.SetUsageHistory(history)
	handler.GetUsageHistoryEvents(ginContext)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	body := recorder.Body.String()
	if got := ojson.Get(body, "items.0.request_id").String(); got != "execution-1" {
		t.Fatalf("request_id = %q, want execution-1", got)
	}
	if got := ojson.Get(body, "items.0.api_key_fingerprint").String(); got == "" || got == "sk-secret-value" {
		t.Fatalf("api_key_fingerprint = %q, want redacted fingerprint", got)
	}
	if ojson.Get(body, "items.0.api_key").Exists() {
		t.Fatalf("response exposed raw api_key: %s", body)
	}
	if got := ojson.Get(body, "items.0.api_key_masked").String(); got == "" || got == "sk-secret-value" {
		t.Fatalf("api_key_masked = %q, want redacted masked value", got)
	}
}

func TestGetUsageHistorySummaryAndEventsFilteredByAPIKey(t *testing.T) {
	if errConfigure := kvstore.ConfigureLocal(true, filepath.Join(t.TempDir(), "usage-sk-filter.db")); errConfigure != nil {
		t.Fatalf("ConfigureLocal() error = %v", errConfigure)
	}
	t.Cleanup(func() { _ = kvstore.ShutdownLocal() })

	history := usagehistory.New(func() bool { return true })
	t.Cleanup(func() { _ = history.Close(context.Background()) })
	history.HandleUsage(context.Background(), coreusage.Record{
		RequestID:   "req-1",
		Provider:    "codex",
		Model:       "gpt-5.4",
		APIKey:      "sk-proj-alpha-12345678",
		RequestedAt: time.Date(2026, time.November, 12, 10, 0, 0, 0, time.UTC),
		Detail:      coreusage.Detail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	})
	history.HandleUsage(context.Background(), coreusage.Record{
		RequestID:   "req-2",
		Provider:    "codex",
		Model:       "gpt-5.4",
		APIKey:      "sk-proj-beta-87654321",
		RequestedAt: time.Date(2026, time.November, 12, 10, 1, 0, 0, time.UTC),
		Detail:      coreusage.Detail{InputTokens: 20, OutputTokens: 10, TotalTokens: 30},
	})

	handler := &Handler{}
	handler.SetUsageHistory(history)

	// Summary returns api_keys array with both keys
	recSummary := httptest.NewRecorder()
	cSummary, _ := gin.CreateTestContext(recSummary)
	cSummary.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/summary?from=2026-11-12&to=2026-11-12", nil)
	handler.GetUsageHistorySummary(cSummary)
	if recSummary.Code != http.StatusOK {
		t.Fatalf("summary status = %d, body=%s", recSummary.Code, recSummary.Body.String())
	}
	summaryBody := recSummary.Body.String()
	if got := ojson.Get(summaryBody, "api_keys.#").Int(); got != 2 {
		t.Fatalf("api_keys count = %d, want 2. body=%s", got, summaryBody)
	}

	// Filtered by api_key (raw key query)
	recAlpha := httptest.NewRecorder()
	cAlpha, _ := gin.CreateTestContext(recAlpha)
	cAlpha.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/summary?from=2026-11-12&to=2026-11-12&api_key=sk-proj-alpha-12345678", nil)
	handler.GetUsageHistorySummary(cAlpha)
	if recAlpha.Code != http.StatusOK {
		t.Fatalf("alpha summary status = %d, body=%s", recAlpha.Code, recAlpha.Body.String())
	}
	alphaBody := recAlpha.Body.String()
	if got := ojson.Get(alphaBody, "totals.total_tokens").Int(); got != 15 {
		t.Fatalf("alpha total_tokens = %d, want 15. body=%s", got, alphaBody)
	}

	// Events filtered by api_key
	recEvents := httptest.NewRecorder()
	cEvents, _ := gin.CreateTestContext(recEvents)
	cEvents.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/events?api_key=sk-proj-alpha-12345678", nil)
	handler.GetUsageHistoryEvents(cEvents)
	if recEvents.Code != http.StatusOK {
		t.Fatalf("events status = %d, body=%s", recEvents.Code, recEvents.Body.String())
	}
	eventsBody := recEvents.Body.String()
	if got := ojson.Get(eventsBody, "items.#").Int(); got != 1 {
		t.Fatalf("events items count = %d, want 1. body=%s", got, eventsBody)
	}
	if got := ojson.Get(eventsBody, "items.0.request_id").String(); got != "req-1" {
		t.Fatalf("events items[0].request_id = %q, want req-1", got)
	}
}

func TestGetUsageHistorySummaryRejectsInvalidRange(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/summary?from=2026-11-13&to=2026-11-12", nil)

	history := usagehistory.New(func() bool { return true })
	t.Cleanup(func() { _ = history.Close(context.Background()) })
	handler := &Handler{}
	handler.SetUsageHistory(history)
	handler.GetUsageHistorySummary(ginContext)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}
