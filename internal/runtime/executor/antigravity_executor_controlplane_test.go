package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type recordedCompanionCall struct {
	method string
	body   string
	header http.Header
}

// antigravityNoControlPlane disables the CLI companion calls so unit tests that count upstream
// requests observe only the generation call itself.
func antigravityNoControlPlane(cfg *config.Config) *config.Config {
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.Antigravity.DisableControlPlane = true
	return cfg
}

// antigravityCompanionServer records the companion requests and answers with the fields the
// proxy consumes.
func antigravityCompanionServer(t *testing.T, calls *[]recordedCompanionCall, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read request body: %v", errRead)
		}
		method := strings.TrimPrefix(r.URL.Path, "/v1internal:")
		mu.Lock()
		*calls = append(*calls, recordedCompanionCall{method: method, body: string(body), header: r.Header.Clone()})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "loadCodeAssist":
			_, _ = io.WriteString(w, `{"currentTier":{"id":"g1-ultra-tier"},"cloudaicompanionProject":"aicode-consumers"}`)
		case "fetchUserInfo":
			_, _ = io.WriteString(w, `{"userSettings":{"telemetryEnabled":true},"regionCode":"SG"}`)
		case "listExperiments":
			_, _ = io.WriteString(w, `{"experimentIds":[1714249,106838234]}`)
		case "retrieveUserQuotaSummary":
			_, _ = io.WriteString(w, `{"groups":[{"buckets":[{"bucketId":"gemini-weekly","remainingFraction":0.5}]}]}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
}

func antigravityCompanionAuth(serverURL, id string) *coreauth.Auth {
	return &coreauth.Auth{
		ID:         id,
		Attributes: map[string]string{"base_url": serverURL},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
}

func TestAntigravityControlPlaneMirrorsCLICalls(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []recordedCompanionCall
	)
	server := antigravityCompanionServer(t, &calls, &mu)
	defer server.Close()

	executor := NewAntigravityExecutor(&config.Config{})
	auth := antigravityCompanionAuth(server.URL, "control-plane-cycle")
	state := antigravityControlPlaneStateFor(auth.ID + "|" + server.URL)
	executor.refreshAntigravityControlPlane(context.Background(), auth, "token", server.URL, state)

	var methods []string
	for _, call := range calls {
		methods = append(methods, call.method)
	}
	wantMethods := []string{"loadCodeAssist", "fetchAdminControls", "retrieveUserQuotaSummary", "fetchUserInfo", "listExperiments"}
	if strings.Join(methods, ",") != strings.Join(wantMethods, ",") {
		t.Fatalf("companion calls = %v, want %v", methods, wantMethods)
	}

	for _, call := range calls {
		if got := call.header.Get("Accept"); got != "" {
			t.Errorf("%s sent Accept %q, want the CLI header set without it", call.method, got)
		}
		if !strings.HasPrefix(call.header.Get("User-Agent"), "antigravity/cli/") {
			t.Errorf("%s User-Agent = %q, want the Antigravity CLI fingerprint", call.method, call.header.Get("User-Agent"))
		}
		if call.header.Get("Authorization") != "Bearer token" || call.header.Get("Content-Type") != "application/json" {
			t.Errorf("%s headers = %v, want the CLI request headers", call.method, call.header)
		}
		want := `{"project":"project-1"}`
		if call.method == "loadCodeAssist" {
			want = `{"metadata":{"ideType":"ANTIGRAVITY"}}`
		}
		if call.body != want {
			t.Errorf("%s body = %s, want %s", call.method, call.body, want)
		}
	}

	regionCode, userTierID, experimentIDs := state.snapshot()
	if regionCode != "SG" || userTierID != "g1-ultra-tier" {
		t.Fatalf("state = region %q tier %q, want SG and g1-ultra-tier", regionCode, userTierID)
	}
	if len(experimentIDs) != 2 || experimentIDs[0] != 1714249 {
		t.Fatalf("experiment IDs = %v, want the two reported experiments", experimentIDs)
	}
	if !state.telemetryReportsEnabled() {
		t.Fatal("telemetryReportsEnabled() = false, want true for an account with telemetry enabled")
	}
}

func TestAntigravityControlPlaneThrottledPerCredential(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []recordedCompanionCall
	)
	server := antigravityCompanionServer(t, &calls, &mu)
	defer server.Close()

	executor := NewAntigravityExecutor(&config.Config{})
	auth := antigravityCompanionAuth(server.URL, "control-plane-throttle")
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.DefaultTransport)
	for i := 0; i < 3; i++ {
		executor.maybeRefreshAntigravityControlPlane(ctx, auth, "token")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(calls)
		mu.Unlock()
		if count > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	methods := map[string]int{}
	for _, call := range calls {
		methods[call.method]++
	}
	if methods["fetchUserInfo"] != 1 {
		t.Fatalf("fetchUserInfo calls = %d, want one cycle for three requests: %v", methods["fetchUserInfo"], methods)
	}
}

// antigravityReportRequest returns a request carrying the identity chain of a conversation turn.
func antigravityReportRequest(t *testing.T, sessionID string, turn int, step, modelCalls uint64) *http.Request {
	t.Helper()
	doc := ojson.NewDocString(`{"model":"claude-opus-5-5-high","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)
	call := nextAntigravityCallIdentity(doc, sessionID+"|report", "claude-opus-5-5-high")
	call.lastStepIndex = step
	call.modelCalls = modelCalls
	httpReq, errReq := http.NewRequest(http.MethodPost, "http://127.0.0.1", strings.NewReader("{}"))
	if errReq != nil {
		t.Fatalf("http.NewRequest() error = %v", errReq)
	}
	return httpReq.WithContext(withAntigravityCallIdentity(httpReq.Context(), call))
}

func TestAntigravityTrajectoryReportWritesACLsOncePerTurn(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []recordedCompanionCall
	)
	server := antigravityCompanionServer(t, &calls, &mu)
	defer server.Close()

	executor := NewAntigravityExecutor(&config.Config{})
	auth := antigravityCompanionAuth(server.URL, "trajectory-report")
	state := antigravityControlPlaneStateFor(auth.ID + "|" + server.URL)
	state.store("SG", "free-tier", []int64{1714249}, true)

	detail := usage.Detail{InputTokens: 12, OutputTokens: 34}
	first := antigravityReportRequest(t, "session-acl", 1, 0, 0)
	second := antigravityReportRequest(t, "session-acl", 2, 2, 1)
	executor.reportAntigravityAgentTurn(context.Background(), auth, "token", first, "claude-opus-5-5-high", detail)
	executor.reportAntigravityAgentTurn(context.Background(), auth, "token", second, "claude-opus-5-5-high", detail)

	mu.Lock()
	defer mu.Unlock()
	var acls, reports int
	var reportBodies []string
	for _, call := range calls {
		switch call.method {
		case "writeTrajectoryAcls":
			acls++
			if !strings.HasPrefix(call.body, `{"trajectoryId":"`) {
				t.Fatalf("writeTrajectoryAcls body = %s, want the trajectory ID of the turn", call.body)
			}
		case "recordTrajectoryAnalytics":
			reports++
			reportBodies = append(reportBodies, call.body)
		}
	}
	if acls != 1 || reports != 2 {
		t.Fatalf("acls = %d reports = %d, want one ACL write for the turn and one report per request", acls, reports)
	}

	report := ojson.NewDocString(reportBodies[0])
	if got := report.Get("trajectory.trajectoryType").String(); got != "CORTEX_TRAJECTORY_TYPE_CASCADE" {
		t.Errorf("trajectoryType = %q, want the CLI cascade type", got)
	}
	if got := report.Get("trajectory.source").String(); got != "CORTEX_TRAJECTORY_SOURCE_CLI" {
		t.Errorf("source = %q, want the CLI source", got)
	}
	if got := report.Get("metadata.userTierId").String(); got != "free-tier" {
		t.Errorf("userTierId = %q, want the tier reported by loadCodeAssist", got)
	}
	if got := report.Get("metadata.os").String(); got != "linux" {
		t.Errorf("os = %q, want the os_type of the CLI user agent", got)
	}
	if got := report.Get("trajectory.generatorMetadata.0.chatModel.usage.inputTokens").String(); got != "12" {
		t.Errorf("inputTokens = %q, want the counted prompt tokens", got)
	}
	if steps := report.Get("trajectory.steps").Array(); len(steps) != 2 {
		t.Fatalf("steps = %d, want the user input and the model response", len(steps))
	}
	if ids := report.Get("mendelExperimentIds").Array(); len(ids) != 1 || ids[0].Int() != 1714249 {
		t.Errorf("mendelExperimentIds = %v, want the experiments of the account", report.Get("mendelExperimentIds").Raw)
	}
}

func TestAntigravityTrajectoryReportSkippedWhenTelemetryDisabled(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []recordedCompanionCall
	)
	server := antigravityCompanionServer(t, &calls, &mu)
	defer server.Close()

	executor := NewAntigravityExecutor(&config.Config{})
	auth := antigravityCompanionAuth(server.URL, "trajectory-off")
	state := antigravityControlPlaneStateFor(auth.ID + "|" + server.URL)
	state.store("SG", "free-tier", nil, false)

	executor.reportAntigravityAgentTurn(context.Background(), auth, "token", antigravityReportRequest(t, "session-off", 1, 0, 0), "claude-opus-5-5-high", usage.Detail{})

	mu.Lock()
	defer mu.Unlock()
	for _, call := range calls {
		if call.method == "recordTrajectoryAnalytics" || call.method == "writeTrajectoryAcls" {
			t.Fatalf("account with telemetry disabled still sent %s", call.method)
		}
	}
}

func TestAntigravityTrajectoryReportIgnoredWithoutIdentity(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []recordedCompanionCall
	)
	server := antigravityCompanionServer(t, &calls, &mu)
	defer server.Close()

	executor := NewAntigravityExecutor(&config.Config{})
	auth := antigravityCompanionAuth(server.URL, "trajectory-no-identity")
	httpReq, errReq := http.NewRequest(http.MethodPost, server.URL, strings.NewReader("{}"))
	if errReq != nil {
		t.Fatalf("http.NewRequest() error = %v", errReq)
	}
	executor.reportAntigravityAgentTurn(context.Background(), auth, "token", httpReq, "claude-opus-5-5-high", usage.Detail{})

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 0 {
		t.Fatalf("companion calls = %v, want none for a request without an agent identity", calls)
	}
}

func TestAntigravityUserAgentParam(t *testing.T) {
	agent := "antigravity/cli/1.5.7 (aidev_client; os_type=linux; arch=amd64; cl=1019769835; auth_method=consumer)"
	if got := antigravityUserAgentParam(agent, "arch"); got != "amd64" {
		t.Errorf("arch = %q, want amd64", got)
	}
	if got := antigravityUserAgentParam(agent, "os_type"); got != "linux" {
		t.Errorf("os_type = %q, want linux", got)
	}
	if got := antigravityUserAgentParam("antigravity/hub/1.0.0", "arch"); got != "" {
		t.Errorf("os_type = %q, want empty for an agent without parameters", got)
	}
}

func TestAntigravityAgentIdentityChainMatchesCLIRequestID(t *testing.T) {
	doc := ojson.NewDocString(`{"model":"gemini-3.8-flash-high","request":{"contents":[{"role":"user","parts":[{"text":"first"}]}]}}`)
	sessionID := fmt.Sprintf("identity-session-%d", time.Now().UnixNano())

	first := nextAntigravityCallIdentity(doc, sessionID, "gemini-3.8-flash-high")
	second := nextAntigravityCallIdentity(doc, sessionID, "gemini-3.8-flash-high")
	if first.cascadeID != second.cascadeID || first.trajectoryID != second.trajectoryID {
		t.Fatalf("identity changed within one turn: %+v vs %+v", first, second)
	}
	if second.lastStepIndex != first.lastStepIndex+1 {
		t.Fatalf("step counter = %d, want it to advance from %d", second.lastStepIndex, first.lastStepIndex)
	}

	requestID := antigravityAgentRequestID(first)
	// The CLI renders agent/<uuid>/<unix millis>/<uuid>/<step>.
	pattern := regexp.MustCompile(`^agent/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/\d{13}/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/\d+$`)
	if !pattern.MatchString(requestID) {
		t.Fatalf("requestId = %q, want the CLI agent identity format", requestID)
	}
	parts := strings.Split(requestID, "/")
	if parts[1] != first.cascadeID || parts[3] != first.trajectoryID {
		t.Fatalf("requestId %q does not carry the identity %+v", requestID, first)
	}

	// A new user turn starts a new trajectory, like the CLI does.
	nextTurn := ojson.NewDocString(`{"model":"gemini-3.8-flash-high","request":{"contents":[{"role":"user","parts":[{"text":"first"}]},{"role":"model","parts":[{"text":"answer"}]},{"role":"user","parts":[{"text":"second"}]}]}}`)
	rotated := nextAntigravityCallIdentity(nextTurn, sessionID, "gemini-3.8-flash-high")
	if rotated.trajectoryID == first.trajectoryID {
		t.Fatal("trajectory did not rotate for a new user turn")
	}
	if rotated.cascadeID != first.cascadeID {
		t.Fatal("cascade changed for a model switch that did not happen")
	}

	// Switching model restarts the cascade, which is how the CLI reports an interrupted agent.
	switched := nextAntigravityCallIdentity(nextTurn, sessionID, "claude-opus-5-5-high")
	if switched.cascadeID == rotated.cascadeID {
		t.Fatal("cascade did not restart after the model changed")
	}
}

func TestAntigravityRequestLabelsUseStringValues(t *testing.T) {
	doc := ojson.NewDocString(`{"model":"claude-opus-5-5-high","request":{}}`)
	call := newAntigravityCallIdentity()
	call.lastStepIndex = 3
	call.modelCalls = 1
	antigravitySetRequestLabels(doc, call, "MODEL_PLACEHOLDER_M402")

	body := doc.Bytes()
	labels := ojson.GetBytesNoCopy(body, "request.labels")
	if !labels.IsObject() {
		t.Fatalf("request.labels = %s, want an object", labels.Raw)
	}
	for _, key := range []string{"cascade_id", "root_cascade_id", "trajectory_id", "request_id", "model_enum", "last_step_index", "used_claude", "used_claude_conservative", "used_non_gemini_model"} {
		value := ojson.GetBytesNoCopy(body, "request.labels."+key)
		if !value.Exists() {
			t.Fatalf("request.labels.%s missing: %s", key, labels.Raw)
		}
		if value.Type != ojson.String {
			t.Errorf("request.labels.%s = %s, want a JSON string like the CLI sends", key, value.Raw)
		}
	}
	if got := ojson.GetBytesNoCopy(body, "request.labels.request_id").String(); got != call.trajectoryID+"-1" {
		t.Errorf("request_id = %q, want the trajectory ID suffixed with the model call index", got)
	}
	if got := ojson.GetBytesNoCopy(body, "request.labels.used_claude").String(); got != "true" {
		t.Errorf("used_claude = %q, want true for a Claude model", got)
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		t.Fatalf("request body is not valid JSON: %v", errUnmarshal)
	}
}

func TestAntigravityModelEnumFromAccountCatalog(t *testing.T) {
	registry.GetGlobalRegistry().RegisterClient("model-enum-auth", "antigravity", []*registry.ModelInfo{
		{ID: "claude-opus-5-5-high", ModelEnum: "MODEL_PLACEHOLDER_M402"},
		{ID: "gemini-3.8-flash-high"},
	})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("model-enum-auth") })

	if got := antigravityModelEnum("claude-opus-5-5-high"); got != "MODEL_PLACEHOLDER_M402" {
		t.Fatalf("antigravityModelEnum() = %q, want the identifier from the account catalog", got)
	}
	if got := antigravityModelEnum("gemini-3.8-flash-high"); got != "" {
		t.Fatalf("antigravityModelEnum() = %q, want empty for a model the catalog did not report", got)
	}
}

func TestAntigravityTrajectoryReportSurvivesDuplicateACL(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []recordedCompanionCall
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		method := strings.TrimPrefix(r.URL.Path, "/v1internal:")
		mu.Lock()
		calls = append(calls, recordedCompanionCall{method: method, body: string(body)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if method == "writeTrajectoryAcls" {
			// A retried turn hits an already registered trajectory.
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":{"code":409}}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()

	executor := NewAntigravityExecutor(&config.Config{})
	auth := antigravityCompanionAuth(server.URL, "trajectory-409")
	state := antigravityControlPlaneStateFor(auth.ID + "|" + server.URL)
	state.store("SG", "free-tier", []int64{1}, true)

	executor.reportAntigravityAgentTurn(context.Background(), auth, "token", antigravityReportRequest(t, "session-409", 1, 0, 0), "gemini-3.8-flash-high", usage.Detail{InputTokens: 3})

	mu.Lock()
	defer mu.Unlock()
	for _, call := range calls {
		if call.method == "recordTrajectoryAnalytics" {
			return
		}
	}
	t.Fatalf("a 409 from writeTrajectoryAcls suppressed the trajectory report: %v", calls)
}
