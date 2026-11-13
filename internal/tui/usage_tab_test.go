package tui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagehistory"
)

func TestClientGetUsageHistoryUsesV8ManagementRoutes(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v8/management/observability/usage/summary":
			_, _ = w.Write([]byte(`{"from":"2026-11-01","to":"2026-11-12","timezone":"UTC","totals":{"requests":3,"total_tokens":42,"reported_cost_usd_micros":35471,"estimated_cost_usd_micros":30000,"effective_cost_usd_micros":35471},"days":[]}`))
		case "/v8/management/observability/usage/events":
			_, _ = w.Write([]byte(`{"items":[{"sequence":7,"timestamp":"2026-11-12T10:00:00Z","provider":"codex","model":"gpt-5.4","total_tokens":10,"estimated_cost_usd_micros":12000,"effective_cost_usd_micros":12000,"cost_source":"catalog"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClientWithBaseURL(server.URL, "management-secret")
	summary, errSummary := client.GetUsageSummary("2026-11-01", "2026-11-12")
	if errSummary != nil {
		t.Fatalf("GetUsageSummary() error = %v", errSummary)
	}
	events, errEvents := client.GetUsageEvents(20, 0)
	if errEvents != nil {
		t.Fatalf("GetUsageEvents() error = %v", errEvents)
	}
	if summary.Totals.Requests != 3 || summary.Totals.TotalTokens != 42 || summary.Totals.ReportedCostUSDMicros != 35471 || summary.Totals.EstimatedCostUSDMicros != 30000 || summary.Totals.EffectiveCostUSDMicros != 35471 {
		t.Fatalf("summary = %+v", summary)
	}
	if len(events.Items) != 1 || events.Items[0].Sequence != 7 || events.Items[0].CostSource != "catalog" || events.Items[0].EffectiveCostUSDMicros != 12000 {
		t.Fatalf("events = %+v", events)
	}
	if len(paths) != 2 || paths[0] != "/v8/management/observability/usage/summary?from=2026-11-01&to=2026-11-12" || paths[1] != "/v8/management/observability/usage/events?limit=20" {
		t.Fatalf("paths = %v", paths)
	}
}

func TestUsageTabRendersSummaryAndRecentEvents(t *testing.T) {
	model := newUsageTabModel(nil)
	model.SetSize(120, 40)
	model.lastSummary = usagehistory.Summary{
		From:     "2026-11-01",
		To:       "2026-11-12",
		Timezone: "UTC",
		Totals: usagehistory.DailyAggregate{
			Requests:               3,
			Success:                2,
			Failed:                 1,
			InputTokens:            20,
			OutputTokens:           10,
			ReasoningTokens:        4,
			CacheReadTokens:        5,
			TotalTokens:            30,
			ReportedCostUSDMicros:  35471,
			EstimatedCostUSDMicros: 30000,
			EffectiveCostUSDMicros: 35471,
			LatencyMs:              600,
			TTFTMs:                 120,
		},
		Days: []usagehistory.Day{{Date: "2026-11-12", DailyAggregate: usagehistory.DailyAggregate{Requests: 3, TotalTokens: 30}}},
	}
	model.lastEvents = usagehistory.EventsPage{Items: []usagehistory.Event{{
		Sequence:               7,
		Timestamp:              time.Date(2026, time.November, 12, 10, 0, 0, 0, time.UTC),
		Provider:               "codex",
		Model:                  "gpt-5.4",
		Failed:                 true,
		TotalTokens:            10,
		ReportedCostUSDMicros:  12345,
		EstimatedCostUSDMicros: 12000,
		EffectiveCostUSDMicros: 12345,
		CostSource:             "upstream",
		LatencyMs:              250,
	}}}
	view := model.renderContent()
	for _, want := range []string{"3", "30", "$0.035471", "$0.030000", "2026-11-12", "codex", "gpt-5.4", "$0.012345", "upstream", "250ms"} {
		if !strings.Contains(view, want) {
			t.Fatalf("usage view missing %q:\n%s", want, view)
		}
	}
}
