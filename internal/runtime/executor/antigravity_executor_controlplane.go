package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// The Antigravity CLI surrounds every generation with a set of companion calls on the same
// v1internal endpoint: it loads the account tier, admin controls, model catalog and quota, then
// traces the conversation back through writeTrajectoryAcls and recordTrajectoryAnalytics. A
// client that only posts generateContent looks like a plain API caller rather than the agent,
// so the proxy mirrors the same calls, throttled per credential.
const (
	// antigravityControlPlaneRefreshInterval bounds how often the companion calls run for one
	// credential. The CLI repeats them roughly every three minutes.
	antigravityControlPlaneRefreshInterval = 5 * time.Minute
	// antigravityControlPlaneRetryInterval is the shorter backoff after a failed cycle so a
	// transient upstream error does not silence the companions for a full interval.
	antigravityControlPlaneRetryInterval = time.Minute
	// antigravityControlPlaneCacheSize bounds the number of credentials tracked.
	antigravityControlPlaneCacheSize = 128
	// antigravityControlPlaneFallbackProject is the project the CLI resolves from
	// loadCodeAssist for consumer accounts. Upstream does not validate the project of these
	// calls, so it only serves as a placeholder when the credential has none.
	antigravityControlPlaneFallbackProject = "aicode-consumers"
	// antigravityControlPlaneCLITimeout bounds one companion refresh cycle. It applies to the
	// control-plane metadata calls only, following the credits refresh precedent.
	antigravityControlPlaneCLITimeout = 15 * time.Second
)

var antigravityControlPlaneCache = cache.NewBoundedLRU[string, *antigravityControlPlaneState](antigravityControlPlaneCacheSize, nil)

// antigravityControlPlaneState is the account metadata the companion calls return and the
// trajectory report needs. One entry exists per credential and upstream endpoint.
type antigravityControlPlaneState struct {
	mu             sync.Mutex
	nextRefresh    time.Time
	regionCode     string
	userTierID     string
	experimentIDs  []int64
	telemetryKnown bool
	telemetryOn    bool
}

// antigravityControlPlaneStateFor returns the tracked state of one credential and endpoint.
func antigravityControlPlaneStateFor(key string) *antigravityControlPlaneState {
	return antigravityControlPlaneCache.GetOrAdd(key, func() *antigravityControlPlaneState {
		return &antigravityControlPlaneState{}
	})
}

// claim reports whether this caller owns the next refresh cycle. The deadline is moved before
// the calls are issued so concurrent requests on the same credential do not all fan out.
func (s *antigravityControlPlaneState) claim(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nextRefresh.After(now) {
		return false
	}
	s.nextRefresh = now.Add(antigravityControlPlaneRefreshInterval)
	return true
}

// fail shortens the window before the next attempt after an unsuccessful cycle.
func (s *antigravityControlPlaneState) fail(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if deadline := now.Add(antigravityControlPlaneRetryInterval); deadline.Before(s.nextRefresh) {
		s.nextRefresh = deadline
	}
}

// store records the account metadata of a successful cycle.
func (s *antigravityControlPlaneState) store(regionCode, userTierID string, experimentIDs []int64, telemetryEnabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.regionCode = regionCode
	s.userTierID = userTierID
	s.experimentIDs = experimentIDs
	s.telemetryKnown = true
	s.telemetryOn = telemetryEnabled
}

// telemetryReportsEnabled mirrors the CLI, which reads fetchUserInfo before deciding whether to
// trace a conversation. An account whose setting is not known yet still reports, because the
// CLI sends the report on the same turn it learns the setting.
func (s *antigravityControlPlaneState) telemetryReportsEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.telemetryKnown || s.telemetryOn
}

// snapshot returns the account metadata for one trajectory report. The experiment list is
// never nil because the CLI always sends a JSON array there.
func (s *antigravityControlPlaneState) snapshot() (regionCode, userTierID string, experimentIDs []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	experimentIDs = make([]int64, 0, len(s.experimentIDs))
	return s.regionCode, s.userTierID, append(experimentIDs, s.experimentIDs...)
}

// maybeRefreshAntigravityControlPlane mirrors the CLI companion calls at most once per refresh
// interval per credential. Calls are issued off the request path so user latency is unaffected.
func (e *AntigravityExecutor) maybeRefreshAntigravityControlPlane(ctx context.Context, auth *coreauth.Auth, accessToken string) {
	if e == nil || auth == nil || !antigravityControlPlaneEnabled(e.cfg) {
		return
	}
	authID := strings.TrimSpace(auth.ID)
	accessToken = strings.TrimSpace(accessToken)
	if authID == "" || accessToken == "" || (ctx != nil && ctx.Err() != nil) {
		return
	}
	baseURL := antigravityLoadCodeAssistBaseURL(auth)
	state := antigravityControlPlaneStateFor(authID + "|" + baseURL)
	if !state.claim(time.Now()) {
		return
	}
	refreshCtx := context.Background()
	if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
		refreshCtx = context.WithValue(refreshCtx, "cliproxy.roundtripper", rt)
	}
	refreshCtx, cancel := context.WithTimeout(refreshCtx, antigravityControlPlaneCLITimeout)
	authCopy := auth.Clone()
	go func() {
		defer cancel()
		e.refreshAntigravityControlPlane(refreshCtx, authCopy, accessToken, baseURL, state)
	}()
}

// refreshAntigravityControlPlane performs one companion cycle in the order the CLI uses.
func (e *AntigravityExecutor) refreshAntigravityControlPlane(ctx context.Context, auth *coreauth.Auth, accessToken, baseURL string, state *antigravityControlPlaneState) {
	userAgent := resolveLoadCodeAssistUserAgent(auth)
	project := antigravityControlPlaneProjectID(auth)
	body, errMarshal := json.Marshal(map[string]string{"project": project})
	if errMarshal != nil {
		log.Debugf("antigravity executor: marshal control-plane request failed: %v", errMarshal)
		return
	}
	loadBody, errMarshalLoad := json.Marshal(map[string]any{"metadata": map[string]string{"ideType": "ANTIGRAVITY"}})
	if errMarshalLoad != nil {
		log.Debugf("antigravity executor: marshal loadCodeAssist request failed: %v", errMarshalLoad)
		return
	}

	// The CLI starts every cycle with loadCodeAssist; its tier is part of the trajectory
	// report metadata, so the response is consumed even though the credits hint already
	// reads the same endpoint for billing.
	loadResp, errLoad := e.postAntigravityControlPlane(ctx, auth, accessToken, baseURL, userAgent, "loadCodeAssist", loadBody, "control_plane_tier")
	userTierID := ""
	if errLoad == nil {
		userTierID = strings.TrimSpace(ojson.GetBytesNoCopy(loadResp, "currentTier.id").String())
	}

	if _, errControls := e.postAntigravityControlPlane(ctx, auth, accessToken, baseURL, userAgent, "fetchAdminControls", body, "control_plane_admin"); errControls != nil {
		state.fail(time.Now())
		return
	}
	quotaResp, errQuota := e.postAntigravityControlPlane(ctx, auth, accessToken, baseURL, userAgent, "retrieveUserQuotaSummary", body, "control_plane_quota")
	if errQuota == nil {
		log.Debugf("antigravity executor: quota summary buckets=%d", len(ojson.GetBytesNoCopy(quotaResp, "groups").Array()))
	}

	userInfoResp, errUserInfo := e.postAntigravityControlPlane(ctx, auth, accessToken, baseURL, userAgent, "fetchUserInfo", body, "control_plane_user")
	if errUserInfo != nil {
		state.fail(time.Now())
		return
	}
	regionCode := strings.TrimSpace(ojson.GetBytesNoCopy(userInfoResp, "regionCode").String())
	telemetryEnabled := ojson.GetBytesNoCopy(userInfoResp, "userSettings.telemetryEnabled").Bool()

	experimentIDs := []int64{}
	if experimentsResp, errExperiments := e.postAntigravityControlPlane(ctx, auth, accessToken, baseURL, userAgent, "listExperiments", body, "control_plane_experiments"); errExperiments == nil {
		for _, item := range ojson.GetBytesNoCopy(experimentsResp, "experimentIds").Array() {
			if item.Type == ojson.Number {
				experimentIDs = append(experimentIDs, item.Int())
			}
		}
	}

	state.store(regionCode, userTierID, experimentIDs, telemetryEnabled)
}

// postAntigravityControlPlane performs one companion call and returns the response body. The
// request carries exactly the headers the CLI sends for these endpoints.
func (e *AntigravityExecutor) postAntigravityControlPlane(ctx context.Context, auth *coreauth.Auth, accessToken, baseURL, userAgent, method string, body []byte, label string) ([]byte, error) {
	endpointURL := strings.TrimSuffix(baseURL, "/") + "/v1internal:" + method
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(body))
	if errReq != nil {
		return nil, errReq
	}
	httpReq.Header.Set("Authorization", "Bearer "+accessToken)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", userAgent)

	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	resp, errDo := helps.WithAntigravityHTTPClientTrace(httpClient, auth, label).Do(httpReq)
	if errDo != nil {
		log.Debugf("antigravity executor: %s request error: %v", method, errDo)
		return nil, errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("antigravity executor: close %s response body error: %v", method, errClose)
		}
	}()
	payload, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		log.Debugf("antigravity executor: %s response read error: %v", method, errRead)
		return payload, errRead
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		log.Debugf("antigravity executor: %s returned status %d", method, resp.StatusCode)
		return payload, fmt.Errorf("antigravity %s returned status %d", method, resp.StatusCode)
	}
	return payload, nil
}

// antigravityControlPlaneProjectID is the project value of the companion bodies. Upstream
// ignores the value for these endpoints, so the credential project is preferred and the CLI
// consumer project is the placeholder for credentials without one.
func antigravityControlPlaneProjectID(auth *coreauth.Auth) string {
	if auth != nil && auth.Metadata != nil {
		if project, ok := auth.Metadata["project_id"].(string); ok && strings.TrimSpace(project) != "" {
			return strings.TrimSpace(project)
		}
	}
	return antigravityControlPlaneFallbackProject
}

// reportAntigravityAgentTurn mirrors writeTrajectoryAcls and recordTrajectoryAnalytics for one
// agent request. The report reuses the identifiers the request advertised so upstream can tie
// the trace to the generation call.
func (e *AntigravityExecutor) reportAntigravityAgentTurn(ctx context.Context, auth *coreauth.Auth, accessToken string, httpReq *http.Request, model string, detail usage.Detail) {
	if e == nil || auth == nil || httpReq == nil || !antigravityControlPlaneEnabled(e.cfg) {
		return
	}
	call, ok := antigravityCallIdentityFromContext(httpReq.Context())
	if !ok {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	} else {
		// The report is sent after the response is complete, so a client that already hung up
		// must not cancel it: the CLI traces the turn regardless.
		ctx = context.WithoutCancel(ctx)
	}
	if strings.TrimSpace(accessToken) == "" && auth.Metadata != nil {
		accessToken, _ = auth.Metadata["access_token"].(string)
	}
	if strings.TrimSpace(accessToken) == "" {
		return
	}
	baseURL := antigravityLoadCodeAssistBaseURL(auth)
	state := antigravityControlPlaneStateFor(strings.TrimSpace(auth.ID) + "|" + baseURL)
	if !state.telemetryReportsEnabled() {
		return
	}
	userAgent := resolveLoadCodeAssistUserAgent(auth)
	project := antigravityControlPlaneProjectID(auth)

	// The CLI registers the trajectory access list once per conversation turn before tracing
	// the steps taken in it.
	if call.modelCalls == 0 {
		aclBody, errMarshal := json.Marshal(map[string]string{"trajectoryId": call.trajectoryID})
		if errMarshal != nil {
			log.Debugf("antigravity executor: marshal writeTrajectoryAcls request failed: %v", errMarshal)
			return
		}
		// A duplicate ACL write answers 409 when the trajectory is already registered, which
		// happens on a retried turn. That is not a reason to drop the trace.
		if _, errACL := e.postAntigravityControlPlane(ctx, auth, accessToken, baseURL, userAgent, "writeTrajectoryAcls", aclBody, "control_plane_acl"); errACL != nil {
			log.Debugf("antigravity executor: writeTrajectoryAcls: %v", errACL)
		}
	}

	regionCode, userTierID, experimentIDs := state.snapshot()
	modelEnum := antigravityModelEnum(model)
	customMetadata := map[string]string{
		"cascade_id":               call.cascadeID,
		"root_cascade_id":          call.rootCascadeID,
		"trajectory_id":            call.trajectoryID,
		"request_id":               call.trajectoryID + "-" + strconv.FormatUint(call.modelCalls, 10),
		"last_step_index":          strconv.FormatUint(call.lastStepIndex, 10),
		"model_enum":               modelEnum,
		"used_claude":              strconv.FormatBool(strings.Contains(strings.ToLower(model), "claude")),
		"used_claude_conservative": "false",
		"used_non_gemini_model":    antigravityReportNonGemini(model, modelEnum),
	}
	executionID := uuid.NewString()
	sourceTrajectory := map[string]any{"cascadeId": call.cascadeID, "trajectoryId": call.trajectoryID}
	createdAt := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	modelUsage := map[string]any{
		"apiProvider":  "API_PROVIDER_GOOGLE_GEMINI",
		"inputTokens":  strconv.FormatInt(detail.InputTokens, 10),
		"outputTokens": strconv.FormatInt(detail.OutputTokens, 10),
		"model":        modelEnum,
		"messageId":    "bot-" + uuid.NewString(),
	}
	if call.sessionID != "" {
		modelUsage["responseHeader"] = map[string]any{"sessionID": call.sessionID}
	}
	report := map[string]any{
		"trajectory": map[string]any{
			"cascadeId":         call.cascadeID,
			"trajectoryId":      call.trajectoryID,
			"trajectoryType":    "CORTEX_TRAJECTORY_TYPE_CASCADE",
			"source":            "CORTEX_TRAJECTORY_SOURCE_CLI",
			"executorMetadatas": []any{map[string]any{"cascadeId": call.cascadeID}},
			"metadata": map[string]any{
				"createdAt":             createdAt,
				"initializationStateId": executionID,
				"projectId":             project,
				"rootConversationId":    call.rootCascadeID,
				"mendelExperimentIds":   experimentIDs,
			},
			"generatorMetadata": []any{map[string]any{
				"chatModel": map[string]any{
					"customMetadata": customMetadata,
					"model":          modelEnum,
					"usage":          modelUsage,
				},
			}},
			"steps": []any{
				map[string]any{
					"type":   "CORTEX_STEP_TYPE_USER_INPUT",
					"status": "CORTEX_STEP_STATUS_DONE",
					"metadata": map[string]any{
						"createdAt":                createdAt,
						"executionId":              executionID,
						"source":                   "CORTEX_STEP_SOURCE_USER_EXPLICIT",
						"sourceTrajectoryStepInfo": sourceTrajectory,
					},
				},
				map[string]any{
					"type":   "CORTEX_STEP_TYPE_PLANNER_RESPONSE",
					"status": "CORTEX_STEP_STATUS_DONE",
					"metadata": map[string]any{
						"createdAt":                createdAt,
						"completedAt":              time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00"),
						"executionId":              executionID,
						"generatorModel":           modelEnum,
						"modelUsage":               modelUsage,
						"sourceTrajectoryStepInfo": sourceTrajectory,
					},
				},
			},
		},
		"metadata":            antigravityReportMetadata(userAgent, regionCode, userTierID),
		"mendelExperimentIds": experimentIDs,
	}
	reportBody, errMarshal := json.Marshal(report)
	if errMarshal != nil {
		log.Debugf("antigravity executor: marshal recordTrajectoryAnalytics request failed: %v", errMarshal)
		return
	}
	if _, errReport := e.postAntigravityControlPlane(ctx, auth, accessToken, baseURL, userAgent, "recordTrajectoryAnalytics", reportBody, "control_plane_trajectory"); errReport != nil {
		log.Debugf("antigravity executor: recordTrajectoryAnalytics: %v", errReport)
		return
	}
}

// antigravityReportMetadata renders the client fingerprint section of the trajectory report from
// the same CLI user agent used for the request.
func antigravityReportMetadata(userAgent, regionCode, userTierID string) map[string]any {
	metadata := map[string]any{
		"hardware":    antigravityUserAgentParam(userAgent, "arch"),
		"ideName":     "antigravity",
		"ideVersion":  misc.AntigravityVersionFromUserAgent(userAgent),
		"os":          antigravityUserAgentParam(userAgent, "os_type"),
		"productName": "cli",
	}
	if regionCode != "" {
		metadata["regionCode"] = regionCode
	}
	if userTierID != "" {
		metadata["userTierId"] = userTierID
	}
	return metadata
}

// antigravityReportNonGemini reports whether the traced model is a third-party model, which is
// what the CLI records for Claude and GPT-OSS traffic.
func antigravityReportNonGemini(model, modelEnum string) string {
	if strings.Contains(strings.ToLower(model), "claude") {
		return "true"
	}
	if modelEnum != "" && !strings.HasPrefix(modelEnum, "MODEL_GEMINI") && !strings.HasPrefix(modelEnum, "MODEL_PLACEHOLDER") {
		return "true"
	}
	return "false"
}

// antigravityUserAgentParam returns one key of the parenthesised comment of the CLI user agent,
// for example the arch or os_type of the fingerprint.
func antigravityUserAgentParam(userAgent, key string) string {
	open := strings.IndexByte(userAgent, '(')
	close := strings.IndexByte(userAgent, ')')
	if open < 0 || close <= open {
		return ""
	}
	for _, part := range strings.Split(userAgent[open+1:close], ";") {
		name, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if found && strings.TrimSpace(name) == key {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
