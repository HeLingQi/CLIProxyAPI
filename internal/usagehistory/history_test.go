package usagehistory

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pricing"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestHistoryPersistsDailySummaryAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage-history.db")
	if errConfigure := kvstore.ConfigureLocal(true, dbPath); errConfigure != nil {
		t.Fatalf("ConfigureLocal() error = %v", errConfigure)
	}
	t.Cleanup(func() { _ = kvstore.ShutdownLocal() })

	history := New(func() bool { return true })
	requestedAt := time.Date(2026, time.November, 12, 3, 4, 5, 0, time.UTC)
	history.HandleUsage(context.Background(), coreusage.Record{
		Provider:     "codex",
		ExecutorType: "codex",
		RequestedAt:  requestedAt,
		Latency:      120 * time.Millisecond,
		TTFT:         40 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:     10,
			OutputTokens:    5,
			ReasoningTokens: 2,
			CacheReadTokens: 3,
			TotalTokens:     15,
			CostDetails:     &coreusage.CostDetails{Cost: coreusage.Float64Ptr(0.035471)},
		},
	})
	history.HandleUsage(context.Background(), coreusage.Record{
		Provider:     "codex",
		ExecutorType: "codex",
		RequestedAt:  requestedAt.Add(time.Hour),
		Latency:      180 * time.Millisecond,
		TTFT:         60 * time.Millisecond,
		Failed:       true,
		Detail: coreusage.Detail{
			InputTokens:  4,
			OutputTokens: 1,
			TotalTokens:  5,
			CostDetails:  &coreusage.CostDetails{CostInUSDTicks: coreusage.Int64Ptr(2_500_000_000)},
		},
	})
	if errFlush := history.Flush(context.Background()); errFlush != nil {
		t.Fatalf("Flush() error = %v", errFlush)
	}

	wantRange := DateRange{From: "2026-11-12", To: "2026-11-12"}
	got, errSummary := history.Summary(context.Background(), wantRange)
	if errSummary != nil {
		t.Fatalf("Summary() error = %v", errSummary)
	}
	assertSummaryTotals(t, got, DailyAggregate{
		Requests:               2,
		Success:                1,
		Failed:                 1,
		InputTokens:            14,
		OutputTokens:           6,
		ReasoningTokens:        2,
		CacheReadTokens:        3,
		TotalTokens:            20,
		ReportedCostUSDMicros:  285471,
		EffectiveCostUSDMicros: 285471,
		LatencyMs:              300,
		TTFTMs:                 100,
	})

	if errClose := history.Close(context.Background()); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}
	if errShutdown := kvstore.ShutdownLocal(); errShutdown != nil {
		t.Fatalf("ShutdownLocal() error = %v", errShutdown)
	}
	if errReopen := kvstore.ConfigureLocal(true, dbPath); errReopen != nil {
		t.Fatalf("ConfigureLocal() reopen error = %v", errReopen)
	}

	reopened := New(func() bool { return true })
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	persisted, errPersisted := reopened.Summary(context.Background(), wantRange)
	if errPersisted != nil {
		t.Fatalf("Summary() after reopen error = %v", errPersisted)
	}
	assertSummaryTotals(t, persisted, got.Totals)
}

func TestHistoryPersistsRedactedRequestEvents(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage-events.db")
	if errConfigure := kvstore.ConfigureLocal(true, dbPath); errConfigure != nil {
		t.Fatalf("ConfigureLocal() error = %v", errConfigure)
	}
	t.Cleanup(func() { _ = kvstore.ShutdownLocal() })

	history := New(func() bool { return true })
	history.HandleUsage(context.Background(), coreusage.Record{
		RequestID:      "execution-1",
		TraceID:        "trace-1",
		Provider:       "codex",
		Model:          "gpt-5.4",
		Alias:          "coding-model",
		APIKey:         "sk-secret-value",
		AccessProvider: "config-inline",
		AuthID:         "codex-user",
		AuthIndex:      "codex-user.json",
		RequestedAt:    time.Date(2026, time.November, 12, 11, 0, 0, 0, time.UTC),
		Latency:        250 * time.Millisecond,
		TTFT:           70 * time.Millisecond,
		Failed:         true,
		Fail:           coreusage.Failure{StatusCode: 429, Code: "quota"},
		Detail: coreusage.Detail{
			InputTokens:  8,
			OutputTokens: 2,
			TotalTokens:  10,
			CostDetails:  &coreusage.CostDetails{Cost: coreusage.Float64Ptr(0.1)},
		},
	})
	if errFlush := history.Flush(context.Background()); errFlush != nil {
		t.Fatalf("Flush() error = %v", errFlush)
	}

	page, errEvents := history.Events(context.Background(), EventsQuery{Limit: 10})
	if errEvents != nil {
		t.Fatalf("Events() error = %v", errEvents)
	}
	if len(page.Items) != 1 {
		t.Fatalf("Events() items = %d, want 1", len(page.Items))
	}
	event := page.Items[0]
	if event.Sequence != 1 || event.RequestID != "execution-1" || event.TraceID != "trace-1" || event.Model != "gpt-5.4" {
		t.Fatalf("event identity = %+v", event)
	}
	if event.APIKeyFingerprint == "" || event.APIKeyFingerprint == "sk-secret-value" {
		t.Fatalf("API key fingerprint = %q, want non-secret fingerprint", event.APIKeyFingerprint)
	}
	if event.FailureStatusCode != 429 || event.FailureCode != "quota" || event.TotalTokens != 10 || event.ReportedCostUSDMicros != 100000 {
		t.Fatalf("event failure/tokens = %+v", event)
	}

	if errClose := history.Close(context.Background()); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}
	if errShutdown := kvstore.ShutdownLocal(); errShutdown != nil {
		t.Fatalf("ShutdownLocal() error = %v", errShutdown)
	}
	if errReopen := kvstore.ConfigureLocal(true, dbPath); errReopen != nil {
		t.Fatalf("ConfigureLocal() reopen error = %v", errReopen)
	}
	reopened := New(func() bool { return true })
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	persisted, errPersisted := reopened.Events(context.Background(), EventsQuery{Limit: 10})
	if errPersisted != nil || len(persisted.Items) != 1 || persisted.Items[0] != event {
		t.Fatalf("persisted events = %+v error=%v, want %+v", persisted, errPersisted, event)
	}
}

func TestHistoryEventsPaginationAndSparseScanAreBounded(t *testing.T) {
	if errConfigure := kvstore.ConfigureLocal(true, filepath.Join(t.TempDir(), "usage-events.db")); errConfigure != nil {
		t.Fatalf("ConfigureLocal() error = %v", errConfigure)
	}
	t.Cleanup(func() { _ = kvstore.ShutdownLocal() })

	history := New(func() bool { return true })
	t.Cleanup(func() { _ = history.Close(context.Background()) })
	for index := 1; index <= 3; index++ {
		history.HandleUsage(context.Background(), coreusage.Record{
			RequestID:   fmt.Sprintf("request-%d", index),
			RequestedAt: time.Date(2026, time.November, 12, index, 0, 0, 0, time.UTC),
		})
	}
	first, errFirst := history.Events(context.Background(), EventsQuery{Limit: 2})
	if errFirst != nil || len(first.Items) != 2 || first.Items[0].Sequence != 3 || first.Items[1].Sequence != 2 || first.NextBefore != 2 {
		t.Fatalf("first page = %+v error=%v", first, errFirst)
	}
	second, errSecond := history.Events(context.Background(), EventsQuery{Limit: 2, Before: first.NextBefore})
	if errSecond != nil || len(second.Items) != 1 || second.Items[0].Sequence != 1 || second.NextBefore != 0 {
		t.Fatalf("second page = %+v error=%v", second, errSecond)
	}

	client, _, errClient := kvstore.CurrentKVClient()
	if errClient != nil {
		t.Fatalf("CurrentKVClient() error = %v", errClient)
	}
	if _, errSet := client.KVSet(context.Background(), eventSequenceKey, []byte("1000000000"), kvstore.KVSetOptions{}); errSet != nil {
		t.Fatalf("seed sparse sequence: %v", errSet)
	}
	sparse, errSparse := history.Events(context.Background(), EventsQuery{Limit: 1})
	if errSparse != nil || len(sparse.Items) != 0 || sparse.NextBefore != 1000000000-maxEventScanMultiplier+1 {
		t.Fatalf("sparse page = %+v error=%v", sparse, errSparse)
	}
}

func TestHistorySanitizesOversizedEventFields(t *testing.T) {
	if errConfigure := kvstore.ConfigureLocal(true, filepath.Join(t.TempDir(), "usage-events.db")); errConfigure != nil {
		t.Fatalf("ConfigureLocal() error = %v", errConfigure)
	}
	t.Cleanup(func() { _ = kvstore.ShutdownLocal() })

	history := New(func() bool { return true })
	t.Cleanup(func() { _ = history.Close(context.Background()) })
	history.HandleUsage(context.Background(), coreusage.Record{
		Provider:    strings.Repeat("供", maxStoredEventStringBytes),
		Model:       strings.Repeat("m", maxStoredEventStringBytes+100),
		RequestedAt: time.Date(2026, time.November, 12, 10, 0, 0, 0, time.UTC),
		Fail:        coreusage.Failure{StatusCode: -1},
	})
	page, errEvents := history.Events(context.Background(), EventsQuery{Limit: 1})
	if errEvents != nil || len(page.Items) != 1 {
		t.Fatalf("Events() = %+v error=%v", page, errEvents)
	}
	if len(page.Items[0].Provider) > maxStoredEventStringBytes || len(page.Items[0].Model) > maxStoredEventStringBytes {
		t.Fatalf("oversized event fields were not bounded: provider=%d model=%d", len(page.Items[0].Provider), len(page.Items[0].Model))
	}
	if page.Items[0].FailureStatusCode != 0 {
		t.Fatalf("negative failure status persisted as %d", page.Items[0].FailureStatusCode)
	}
}

func TestHistoryUsesMemorySummaryWhenKVIsDisabled(t *testing.T) {
	if errShutdown := kvstore.ShutdownLocal(); errShutdown != nil {
		t.Fatalf("ShutdownLocal() error = %v", errShutdown)
	}
	history := New(func() bool { return true })
	t.Cleanup(func() { _ = history.Close(context.Background()) })
	history.HandleUsage(context.Background(), coreusage.Record{
		RequestedAt: time.Date(2026, time.November, 12, 10, 0, 0, 0, time.UTC),
		Detail:      coreusage.Detail{TotalTokens: 5},
	})
	summary, errSummary := history.Summary(context.Background(), DateRange{From: "2026-11-12", To: "2026-11-12"})
	if errSummary != nil || summary.Totals.Requests != 1 || summary.Totals.TotalTokens != 5 {
		t.Fatalf("Summary() = %+v error=%v", summary, errSummary)
	}
	if _, errEvents := history.Events(context.Background(), EventsQuery{Limit: 1}); !errors.Is(errEvents, ErrPersistenceDisabled) {
		t.Fatalf("Events() error = %v, want ErrPersistenceDisabled", errEvents)
	}
}

func TestHistoryKeepsReportedAndEstimatedCostsSeparate(t *testing.T) {
	if errConfigure := kvstore.ConfigureLocal(true, filepath.Join(t.TempDir(), "usage-pricing.db")); errConfigure != nil {
		t.Fatalf("ConfigureLocal() error = %v", errConfigure)
	}
	t.Cleanup(func() { _ = kvstore.ShutdownLocal() })
	catalog, errCatalog := pricing.NewCatalog(context.Background())
	if errCatalog != nil {
		t.Fatalf("NewCatalog() error = %v", errCatalog)
	}
	_, errUpsert := catalog.Upsert(context.Background(), 0, pricing.ModelConfig{
		Provider: "openai",
		Model:    "gpt-5.4",
		BasePrices: pricing.BasePrices{
			Input:      pricing.USDPerMillion(1_000_000),
			Output:     pricing.USDPerMillion(2_000_000),
			CacheRead:  pricing.USDPerMillion(100_000),
			CacheWrite: pricing.USDPerMillion(500_000),
		},
	})
	if errUpsert != nil {
		t.Fatalf("Upsert() error = %v", errUpsert)
	}

	history := New(func() bool { return true }, catalog)
	t.Cleanup(func() { _ = history.Close(context.Background()) })
	tokens := coreusage.NewIndependentTokenBreakdown(1000, 200, 100, 500, 0, 1800)
	base := coreusage.Record{
		Provider:      "openai",
		Model:         "alias",
		ResponseModel: "gpt-5.4",
		RequestedAt:   time.Date(2026, time.November, 12, 10, 0, 0, 0, time.UTC),
		Detail:        coreusage.Detail{TokenBreakdown: tokens},
	}
	history.HandleUsage(context.Background(), base)
	reported := base
	reported.RequestedAt = reported.RequestedAt.Add(time.Minute)
	reported.Detail.CostDetails = &coreusage.CostDetails{Cost: coreusage.Float64Ptr(0.01)}
	history.HandleUsage(context.Background(), reported)
	free := base
	free.RequestedAt = free.RequestedAt.Add(2 * time.Minute)
	free.Detail.CostDetails = &coreusage.CostDetails{Cost: coreusage.Float64Ptr(0)}
	history.HandleUsage(context.Background(), free)
	if errFlush := history.Flush(context.Background()); errFlush != nil {
		t.Fatalf("Flush() error = %v", errFlush)
	}

	summary, errSummary := history.Summary(context.Background(), DateRange{From: "2026-11-12", To: "2026-11-12"})
	if errSummary != nil {
		t.Fatalf("Summary() error = %v", errSummary)
	}
	if summary.Totals.ReportedCostUSDMicros != 10_000 || summary.Totals.EstimatedCostUSDMicros != 6_210 || summary.Totals.EffectiveCostUSDMicros != 12_070 {
		t.Fatalf("summary costs = %+v", summary.Totals)
	}
	page, errEvents := history.Events(context.Background(), EventsQuery{Limit: 10})
	if errEvents != nil || len(page.Items) != 3 {
		t.Fatalf("Events() = %+v error=%v", page, errEvents)
	}
	if page.Items[0].CostSource != "upstream" || page.Items[0].EffectiveCostUSDMicros != 0 || page.Items[0].EstimatedCostUSDMicros != 2_070 || page.Items[0].PricingRevision != 1 {
		t.Fatalf("free upstream event costs = %+v", page.Items[0])
	}
	if page.Items[1].CostSource != "upstream" || page.Items[1].EffectiveCostUSDMicros != 10_000 || page.Items[1].EstimatedCostUSDMicros != 2_070 || page.Items[1].PricingRevision != 1 {
		t.Fatalf("reported event costs = %+v", page.Items[1])
	}
	if page.Items[2].CostSource != "catalog" || page.Items[2].EffectiveCostUSDMicros != 2_070 || page.Items[2].PricingRevision != 1 {
		t.Fatalf("estimated event costs = %+v", page.Items[2])
	}
}

func assertSummaryTotals(t *testing.T, got Summary, want DailyAggregate) {
	t.Helper()
	if got.Timezone != "UTC" || got.From != "2026-11-12" || got.To != "2026-11-12" {
		t.Fatalf("Summary range = %+v, want UTC 2026-11-12", got)
	}
	if got.Totals != want {
		t.Fatalf("Summary totals = %+v, want %+v", got.Totals, want)
	}
	if len(got.Days) != 1 || got.Days[0].Date != "2026-11-12" || got.Days[0].DailyAggregate != want {
		t.Fatalf("Summary days = %+v, want one matching day", got.Days)
	}
}
