package executor

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// Official 2.31.2 sends these nine label keys on every agent request; the
// values are all strings and the JSON keys are sorted, which Go's map encoder
// reproduces.
var antigravityOfficialLabelKeys = []string{
	"cascade_id",
	"last_step_index",
	"model_enum",
	"request_id",
	"root_cascade_id",
	"trajectory_id",
	"used_claude",
	"used_claude_conservative",
	"used_non_gemini_model",
}

func TestAntigravityBuildRequestSendsOfficialLabelSet(t *testing.T) {
	t.Parallel()

	body := buildRequestBodyWithDerivedSession(t, &AntigravityExecutor{}, &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "project-1"}}, "gemini-3.8-flash-tiered", []byte(`{"request":{"systemInstruction":{"parts":[{"text":"be terse"}]},"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`), "-123456789")
	request, ok := body["request"].(map[string]any)
	if !ok {
		t.Fatalf("request missing: %v", body["request"])
	}

	labels, ok := request["labels"].(map[string]any)
	if !ok {
		t.Fatalf("request.labels missing: %v", request["labels"])
	}
	if len(labels) != len(antigravityOfficialLabelKeys) {
		t.Fatalf("label keys = %d, want %d (%v)", len(labels), len(antigravityOfficialLabelKeys), antigravityOfficialLabelKeys)
	}
	for _, key := range antigravityOfficialLabelKeys {
		value, okValue := labels[key].(string)
		if !okValue || value == "" {
			t.Fatalf("label %q must be a non-empty string, got %#v", key, labels[key])
		}
	}
	if labels["model_enum"] != "MODEL_PLACEHOLDER_M322" {
		t.Fatalf("model_enum = %v, want MODEL_PLACEHOLDER_M322", labels["model_enum"])
	}
	if labels["root_cascade_id"] != labels["cascade_id"] {
		t.Fatalf("root_cascade_id = %v, want cascade_id %v", labels["root_cascade_id"], labels["cascade_id"])
	}
	for _, key := range []string{"cascade_id", "root_cascade_id", "trajectory_id"} {
		if !uuidV4Pattern.MatchString(labels[key].(string)) {
			t.Fatalf("label %q = %v, want an RFC 4122 v4 shaped uuid", key, labels[key])
		}
	}
	if _, ok := request["systemInstruction"].(map[string]any)["role"]; !ok {
		t.Fatal("systemInstruction must carry a role")
	}
	if got := request["systemInstruction"].(map[string]any)["role"]; got != "user" {
		t.Fatalf("systemInstruction.role = %v, want user", got)
	}
}

func TestAntigravityBuildRequestStepsForwardOverToolRounds(t *testing.T) {
	t.Parallel()

	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "project-1"}}
	firstStep := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`)
	nextStep := []byte(`{"request":{"contents":[` +
		`{"role":"user","parts":[{"text":"hello"}]},` +
		`{"role":"model","parts":[{"functionCall":{"name":"read_file","args":{"path":"a.go"}}}]},` +
		`{"role":"user","parts":[{"functionResponse":{"name":"read_file","response":{"result":"ok"}}}]}]}}`)
	secondTurn := []byte(`{"request":{"contents":[` +
		`{"role":"user","parts":[{"text":"hello"}]},` +
		`{"role":"model","parts":[{"functionCall":{"name":"read_file","args":{"path":"a.go"}}}]},` +
		`{"role":"user","parts":[{"functionResponse":{"name":"read_file","response":{"result":"ok"}}}]},` +
		`{"role":"model","parts":[{"text":"done"}]},` +
		`{"role":"user","parts":[{"text":"again"}]}]}}`)

	first := buildRequestBodyWithDerivedSession(t, executor, auth, "gemini-3.8-flash-tiered", firstStep, "-123456789")
	next := buildRequestBodyWithDerivedSession(t, executor, auth, "gemini-3.8-flash-tiered", nextStep, "-123456789")
	followup := buildRequestBodyWithDerivedSession(t, executor, auth, "gemini-3.8-flash-tiered", secondTurn, "-123456789")
	firstRequest := first["request"].(map[string]any)
	nextRequest := next["request"].(map[string]any)
	followupRequest := followup["request"].(map[string]any)
	firstLabels := firstRequest["labels"].(map[string]any)
	nextLabels := nextRequest["labels"].(map[string]any)
	followupLabels := followupRequest["labels"].(map[string]any)

	if nextLabels["cascade_id"] != firstLabels["cascade_id"] || nextLabels["trajectory_id"] != firstLabels["trajectory_id"] {
		t.Fatalf("tool results must stay inside one trajectory: %v vs %v", firstLabels, nextLabels)
	}
	if nextLabels["last_step_index"] != "1" {
		t.Fatalf("last_step_index = %v, want 1 after one completed tool round", nextLabels["last_step_index"])
	}
	if !strings.HasSuffix(next["requestId"].(string), "/2") {
		t.Fatalf("requestId = %v, want step last_step_index+1", next["requestId"])
	}
	// Captured official steps keep labels.request_id on the turn that opened the
	// trajectory; only a new user message advances the suffix.
	if nextLabels["request_id"] != firstLabels["request_id"] {
		t.Fatalf("request_id must not move within one user turn: %v vs %v", nextLabels["request_id"], firstLabels["request_id"])
	}
	if followupLabels["cascade_id"] != firstLabels["cascade_id"] || followupLabels["trajectory_id"] != firstLabels["trajectory_id"] {
		t.Fatalf("a follow-up user message stays in the same conversation: %v vs %v", followupLabels, firstLabels)
	}
	if followupLabels["request_id"] != antigravityLabelString(followupLabels["trajectory_id"])+"-1" {
		t.Fatalf("request_id = %v, want <trajectory_id>-1 for the second user turn", followupLabels["request_id"])
	}
	if followupLabels["last_step_index"] != "1" {
		t.Fatalf("last_step_index = %v, want the one completed tool round", followupLabels["last_step_index"])
	}
}

func TestAntigravityBuildRequestCheckpointUsesCheckpointIdentity(t *testing.T) {
	t.Parallel()

	body := buildRequestBodyWithDerivedSession(t, &AntigravityExecutor{}, &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "project-1"}}, "gemini-3.8-flash-tiered", []byte(`{"requestType":"checkpoint","request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`), "-123456789")
	request := body["request"].(map[string]any)

	if _, ok := request["labels"]; ok {
		t.Fatalf("official checkpoint requests carry no labels, got %v", request["labels"])
	}
	if !strings.HasPrefix(body["requestId"].(string), "checkpoint/") {
		t.Fatalf("requestId = %v, want checkpoint/<uuid>", body["requestId"])
	}
	if request["sessionId"] != misc.AntigravitySessionID {
		t.Fatalf("request.sessionId = %v, want %s", request["sessionId"], misc.AntigravitySessionID)
	}
	if generation, ok := request["generationConfig"].(map[string]any); ok {
		if _, okValue := generation["maxOutputTokens"]; okValue {
			t.Fatalf("official checkpoint requests send no output ceiling, got %v", generation)
		}
	}
}

func TestAntigravityGeminiGenerationShapeKeepsCeilingAndDropsLegacyBudget(t *testing.T) {
	t.Parallel()

	doc := ojson.NewDoc([]byte(`{"requestType":"agent","request":{"generationConfig":{"maxOutputTokens":32000,"thinkingConfig":{"includeThoughts":true,"thinkingBudget":0,"thinkingLevel":"HIGH"}}}}`))
	applyAntigravityGeminiGenerationShapeDoc(doc, "gemini-3.8-flash-tiered")

	result := ojson.ParseBytes(doc.Bytes())
	if got := result.Get("request.generationConfig.maxOutputTokens").Int(); got != 32000 {
		t.Fatalf("maxOutputTokens = %d, want the caller ceiling to survive", got)
	}
	if result.Get("request.generationConfig.thinkingConfig.thinkingBudget").Exists() {
		t.Fatal("thinkingBudget must be dropped once thinkingLevel is present")
	}
	if !result.Get("request.generationConfig.thinkingConfig.thinkingLevel").Exists() {
		t.Fatal("thinkingLevel must be kept")
	}
}

func TestAntigravityGenerationShapeStillDropsCeilingForOtherRequestTypes(t *testing.T) {
	t.Parallel()

	for _, requestType := range []string{"image_gen", "web_search", "checkpoint"} {
		doc := ojson.NewDoc([]byte(`{"requestType":"` + requestType + `","request":{"generationConfig":{"maxOutputTokens":32000}}}`))
		applyAntigravityGeminiGenerationShapeDoc(doc, "gemini-3.8-flash-tiered")
		if got := ojson.ParseBytes(doc.Bytes()).Get("request.generationConfig.maxOutputTokens"); got.Exists() {
			t.Fatalf("%s: maxOutputTokens = %s, want it removed as before", requestType, got.Raw)
		}
	}
}

// Every captured official thinkingConfig carries includeThoughts, for the agent
// trajectory (HIGH) and for the title call (LOW) alike.
func TestAntigravityGenerationShapeDefaultsIncludeThoughts(t *testing.T) {
	t.Parallel()

	for _, requestType := range []string{"agent", "checkpoint"} {
		doc := ojson.NewDoc([]byte(`{"requestType":"` + requestType + `","request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"HIGH"}}}}`))
		applyAntigravityGeminiGenerationShapeDoc(doc, "gemini-3.8-flash-tiered")
		got := ojson.ParseBytes(doc.Bytes()).Get("request.generationConfig.thinkingConfig.includeThoughts")
		if !got.Exists() || !got.Bool() {
			t.Fatalf("%s: includeThoughts = %s, want true", requestType, got.Raw)
		}
	}

	doc := ojson.NewDoc([]byte(`{"requestType":"agent","request":{"generationConfig":{"thinkingConfig":{"includeThoughts":false,"thinkingLevel":"HIGH"}}}}`))
	applyAntigravityGeminiGenerationShapeDoc(doc, "gemini-3.8-flash-tiered")
	if got := ojson.ParseBytes(doc.Bytes()).Get("request.generationConfig.thinkingConfig.includeThoughts"); !got.Exists() || got.Bool() {
		t.Fatalf("includeThoughts = %s, want the caller's explicit false to survive", got.Raw)
	}
}

func TestAntigravityModelEnumFallsBackToVerifiedValues(t *testing.T) {
	t.Parallel()

	if got := antigravityModelEnum("gemini-3.8-flash-tiered"); got != "MODEL_PLACEHOLDER_M322" {
		t.Fatalf("antigravityModelEnum() = %q, want MODEL_PLACEHOLDER_M322", got)
	}
	if got := antigravityModelEnum("gemini-4-unknown"); got != "" {
		t.Fatalf("antigravityModelEnum() = %q, want no invented enum for an unverified model", got)
	}
}

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// The used_* pair is a property of the request that carries it. A Claude turn is
// still an official agent turn, so it is labelled with the same identity and
// reports itself; the Gemini turn beside it keeps reporting false for both, and a
// family the client has no captures for gets no labels at all.
func TestAntigravityLabelsTrackTheRequestModel(t *testing.T) {
	t.Parallel()

	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "project-1"}}
	payload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`)

	cases := []struct {
		model             string
		wantLabels        bool
		wantUsedClaude    string
		wantUsedNonGemini string
	}{
		{model: "gemini-3.8-flash-tiered", wantLabels: true, wantUsedClaude: "false", wantUsedNonGemini: "false"},
		{model: "claude-sonnet-4-5", wantLabels: true, wantUsedClaude: "true", wantUsedNonGemini: "true"},
		{model: "grok-4", wantLabels: false},
	}
	for _, tc := range cases {
		body := buildRequestBodyWithDerivedSession(t, &AntigravityExecutor{}, auth, tc.model, payload, "-123456789")
		request, ok := body["request"].(map[string]any)
		if !ok {
			t.Fatalf("%s: request missing: %v", tc.model, body["request"])
		}
		labels, okLabels := request["labels"].(map[string]any)
		if !tc.wantLabels {
			if okLabels {
				t.Fatalf("%s: must carry no labels, got %v", tc.model, request["labels"])
			}
			continue
		}
		if !okLabels {
			t.Fatalf("%s: request.labels missing: %v", tc.model, request["labels"])
		}
		for _, key := range []string{"cascade_id", "root_cascade_id", "trajectory_id", "request_id", "last_step_index", "used_claude_conservative"} {
			if value, okValue := labels[key].(string); !okValue || value == "" {
				t.Fatalf("%s: label %q must be a non-empty string, got %#v", tc.model, key, labels[key])
			}
		}
		if labels["used_claude"] != tc.wantUsedClaude {
			t.Fatalf("%s: used_claude = %v, want %q", tc.model, labels["used_claude"], tc.wantUsedClaude)
		}
		if labels["used_non_gemini_model"] != tc.wantUsedNonGemini {
			t.Fatalf("%s: used_non_gemini_model = %v, want %q", tc.model, labels["used_non_gemini_model"], tc.wantUsedNonGemini)
		}
	}
}

func TestAntigravityReplaySessionIDSkipsPinnedClientSessionAndReadsCascade(t *testing.T) {
	t.Parallel()

	payloadWithSnakeAndConstant := []byte(`{"request":{"sessionId":"` + misc.AntigravitySessionID + `","session_id":"conv-snake-123"}}`)
	if got := antigravityReplaySessionIDFromPayload(payloadWithSnakeAndConstant); got != "conv-snake-123" {
		t.Fatalf("antigravityReplaySessionIDFromPayload() = %q, want conv-snake-123", got)
	}

	payloadWithCascade := []byte(`{"request":{"sessionId":"` + misc.AntigravitySessionID + `","labels":{"cascade_id":"11111111-2222-4333-8444-555555555555"}}}`)
	if got := antigravityReplaySessionIDFromPayload(payloadWithCascade); got != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("antigravityReplaySessionIDFromPayload(cascade) = %q, want cascade_id", got)
	}
	if got := antigravityReplaySessionIDFromDoc(ojson.NewDoc(payloadWithCascade)); got != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("antigravityReplaySessionIDFromDoc(cascade) = %q, want cascade_id", got)
	}
}

func TestAntigravityParallelToolRoundsAndReminderPartsStayInTurn(t *testing.T) {
	t.Parallel()

	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "project-1"}}
	parallelToolStep := []byte(`{"request":{"contents":[` +
		`{"role":"user","parts":[{"text":"inspect a and b"}]},` +
		`{"role":"model","parts":[{"functionCall":{"name":"read_file","args":{"path":"a.go"}}},{"functionCall":{"name":"read_file","args":{"path":"b.go"}}}]},` +
		`{"role":"user","parts":[{"text":"<system-reminder>stay focused</system-reminder>"},{"functionResponse":{"name":"read_file","response":{"result":"a"}}},{"functionResponse":{"name":"read_file","response":{"result":"b"}}}]}]}}`)

	body := buildRequestBodyWithDerivedSession(t, executor, auth, "gemini-3.8-flash-tiered", parallelToolStep, "-123456789")
	labels := body["request"].(map[string]any)["labels"].(map[string]any)
	if labels["last_step_index"] != "1" {
		t.Fatalf("last_step_index = %v, want 1 for one parallel tool round", labels["last_step_index"])
	}
	if labels["request_id"] != antigravityLabelString(labels["trajectory_id"])+"-0" {
		t.Fatalf("request_id = %v, want turn 0 preserved when functionResponse includes reminder text", labels["request_id"])
	}
}

func TestAntigravityIdentitySeparatesSystemInstructionsAndHonorsExplicitSession(t *testing.T) {
	t.Parallel()

	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "project-1"}}
	wsA := []byte(`{"request":{"systemInstruction":{"parts":[{"text":"workspace A"}]},"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`)
	wsB := []byte(`{"request":{"systemInstruction":{"parts":[{"text":"workspace B"}]},"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`)
	explicit := []byte(`{"request":{"sessionId":"caller-session-42","contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`)

	bodyA := buildRequestBodyWithDerivedSession(t, executor, auth, "gemini-3.8-flash-tiered", wsA, "")
	bodyB := buildRequestBodyWithDerivedSession(t, executor, auth, "gemini-3.8-flash-tiered", wsB, "")
	bodyExplicit := buildRequestBodyWithDerivedSession(t, executor, auth, "gemini-3.8-flash-tiered", explicit, "")

	cascadeA := bodyA["request"].(map[string]any)["labels"].(map[string]any)["cascade_id"]
	cascadeB := bodyB["request"].(map[string]any)["labels"].(map[string]any)["cascade_id"]
	cascadeExplicit := bodyExplicit["request"].(map[string]any)["labels"].(map[string]any)["cascade_id"]
	if cascadeA == cascadeB {
		t.Fatalf("distinct systemInstruction prompts should not share cascade_id: %v", cascadeA)
	}
	if cascadeExplicit != antigravityStableUUID("cascade", "caller-session-42") {
		t.Fatalf("explicit caller sessionId ignored: got %v, want %v", cascadeExplicit, antigravityStableUUID("cascade", "caller-session-42"))
	}
}

func TestAntigravityGeminiGenerationShapeProtectsTinyThinkingCeilingAndPrefixedEnum(t *testing.T) {
	t.Parallel()

	doc := ojson.NewDoc([]byte(`{"requestType":"agent","request":{"generationConfig":{"maxOutputTokens":2048,"thinkingConfig":{"thinkingLevel":"HIGH"}}}}`))
	applyAntigravityGeminiGenerationShapeDoc(doc, "ag/gemini-3.8-flash-tiered")

	got := ojson.ParseBytes(doc.Bytes()).Get("request.generationConfig.maxOutputTokens").Int()
	if got < antigravityThinkingMinSafeMaxTokens {
		t.Fatalf("maxOutputTokens = %d, want raised to at least %d when thinkingLevel is enabled", got, antigravityThinkingMinSafeMaxTokens)
	}
	if enum := antigravityModelEnum("ag/gemini-3.8-flash-tiered"); enum != "MODEL_PLACEHOLDER_M322" {
		t.Fatalf("antigravityModelEnum(prefixed) = %q, want MODEL_PLACEHOLDER_M322", enum)
	}
}

func TestAntigravityProxyChainingPreservesValidCascadeAndTrajectoryUUIDs(t *testing.T) {
	t.Parallel()

	const (
		upstreamCascade    = "11111111-2222-4333-8444-555555555555"
		upstreamTrajectory = "66666666-7777-4888-9999-aaaaaaaaaaaa"
	)
	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "project-1"}}
	chainedPayload := []byte(`{"request":{"sessionId":"` + misc.AntigravitySessionID + `","labels":{"cascade_id":"` + upstreamCascade + `","trajectory_id":"` + upstreamTrajectory + `"},"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`)

	body := buildRequestBodyWithDerivedSession(t, executor, auth, "gemini-3.8-flash-tiered", chainedPayload, "")
	labels := body["request"].(map[string]any)["labels"].(map[string]any)
	if labels["cascade_id"] != upstreamCascade {
		t.Fatalf("proxy-chained cascade_id = %v, want preserved %q", labels["cascade_id"], upstreamCascade)
	}
	if labels["root_cascade_id"] != upstreamCascade {
		t.Fatalf("proxy-chained root_cascade_id = %v, want preserved %q", labels["root_cascade_id"], upstreamCascade)
	}
	if labels["trajectory_id"] != upstreamTrajectory {
		t.Fatalf("proxy-chained trajectory_id = %v, want preserved %q", labels["trajectory_id"], upstreamTrajectory)
	}
}

func TestAntigravityReplayScopePreservesCallerSessionAfterSessionIDRewrite(t *testing.T) {
	t.Parallel()

	origReqPayload := []byte(`{"request":{"sessionId":"caller-conv-alpha","contents":[{"role":"user","parts":[{"text":"same prompt"}]}]}}`)
	otherReqPayload := []byte(`{"request":{"sessionId":"caller-conv-beta","contents":[{"role":"user","parts":[{"text":"same prompt"}]}]}}`)
	// Simulate checkpoint or non-labelled model after geminiToAntigravityDoc rewrote request.sessionId
	// to the pinned official constant without emitting request.labels.cascade_id.
	rewrittenPayload := []byte(`{"requestType":"checkpoint","request":{"sessionId":"` + misc.AntigravitySessionID + `","contents":[{"role":"user","parts":[{"text":"same prompt"}]}]}}`)

	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}
	scopeAlpha := antigravityReasoningReplayScopeFromRequest(
		t.Context(),
		"grok-4",
		cliproxyexecutor.Request{Model: "grok-4", Payload: origReqPayload},
		opts,
		rewrittenPayload,
	)
	scopeBeta := antigravityReasoningReplayScopeFromRequest(
		t.Context(),
		"grok-4",
		cliproxyexecutor.Request{Model: "grok-4", Payload: otherReqPayload},
		opts,
		rewrittenPayload,
	)
	if !strings.Contains(scopeAlpha.sessionKey, "caller-conv-alpha") {
		t.Fatalf("scopeAlpha.sessionKey = %q, want caller-conv-alpha preserved", scopeAlpha.sessionKey)
	}
	if scopeAlpha.sessionKey == scopeBeta.sessionKey {
		t.Fatalf("distinct caller sessionIds collapsed onto one replay scope: %q", scopeAlpha.sessionKey)
	}

	scopeAlphaDoc := antigravityReasoningReplayScopeFromRequestDoc(
		t.Context(),
		"grok-4",
		cliproxyexecutor.Request{Model: "grok-4", Payload: origReqPayload},
		opts,
		ojson.NewDoc(rewrittenPayload),
	)
	if scopeAlphaDoc.sessionKey != scopeAlpha.sessionKey {
		t.Fatalf("doc scope %q != bytes scope %q", scopeAlphaDoc.sessionKey, scopeAlpha.sessionKey)
	}
}

func TestAntigravityGenerationShapeCapsContextEqualLimitAndCanonicalizesUUIDs(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-ag-context-equal-ceiling"
	reg.RegisterClient(clientID, "antigravity", []*registry.ModelInfo{
		{
			ID:                  "gemini-context-equal-test",
			ContextLength:       1048576,
			MaxCompletionTokens: 1048576,
		},
	})
	t.Cleanup(func() { reg.UnregisterClient(clientID) })

	// 1. Context-equal MaxCompletionTokens (1048576) must cap to 65536, and case-insensitive "AGENT" works.
	doc := ojson.NewDoc([]byte(`{"requestType":"AGENT","request":{"generationConfig":{"maxOutputTokens":500000,"thinkingConfig":{"thinkingBudget":16000}}}}`))
	applyAntigravityGeminiGenerationShapeDoc(doc, "gemini-context-equal-test")
	parsed := ojson.ParseBytes(doc.Bytes())
	if got := parsed.Get("request.generationConfig.maxOutputTokens").Int(); got != 65536 {
		t.Fatalf("maxOutputTokens = %d, want 65536 when MaxCompletionTokens equals 1M ContextLength", got)
	}
	if !parsed.Get("request.generationConfig.thinkingConfig.includeThoughts").Bool() {
		t.Fatal("expected positive thinkingBudget to default includeThoughts=true")
	}

	// 2. Positive thinkingBudget raises undersized maxOutputTokens so thinking budget has output headroom.
	docSmall := ojson.NewDoc([]byte(`{"requestType":"agent","request":{"generationConfig":{"maxOutputTokens":4096,"thinkingConfig":{"thinkingBudget":16000}}}}`))
	applyAntigravityGeminiGenerationShapeDoc(docSmall, "gemini-context-equal-test")
	if got := ojson.ParseBytes(docSmall.Bytes()).Get("request.generationConfig.maxOutputTokens").Int(); got < 20096 {
		t.Fatalf("maxOutputTokens with thinkingBudget=16000 = %d, want >= 20096", got)
	}

	// 3. Unknown model with maxOutputTokens <= 0 drops non-positive ceiling.
	docZero := ojson.NewDoc([]byte(`{"requestType":"agent","request":{"generationConfig":{"maxOutputTokens":0}}}`))
	applyAntigravityGeminiGenerationShapeDoc(docZero, "gemini-unregistered-zero-cap")
	if ojson.ParseBytes(docZero.Bytes()).Get("request.generationConfig.maxOutputTokens").Exists() {
		t.Fatal("expected maxOutputTokens=0 to be removed when model ceiling is unknown")
	}

	// 4. Case-insensitive Claude model name, canonicalized UUIDs (rejecting uuid.Nil), preserved root_cascade_id,
	// and consecutive user messages staying on turn 0.
	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "project-1"}}
	payload := []byte(`{"request":{"labels":{"cascade_id":"00000000-0000-0000-0000-000000000000","root_cascade_id":"{11111111-2222-4333-8444-555555555555}"},"contents":[{"role":"user","parts":[{"text":"part 1"}]},{"role":"user","parts":[{"text":"part 2"}]}]}}`)
	body := buildRequestBodyWithDerivedSession(t, executor, auth, "CLAUDE-SONNET-4-5", payload, "-123456789")
	labels := body["request"].(map[string]any)["labels"].(map[string]any)
	if labels["used_claude"] != "true" {
		t.Fatalf("CLAUDE-SONNET-4-5 used_claude = %v, want \"true\"", labels["used_claude"])
	}
	if labels["cascade_id"] == "00000000-0000-0000-0000-000000000000" {
		t.Fatal("uuid.Nil cascade_id must be rejected and replaced with derived UUID")
	}
	if labels["root_cascade_id"] != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("root_cascade_id = %v, want canonicalized 11111111-2222-4333-8444-555555555555", labels["root_cascade_id"])
	}
	if labels["request_id"] != antigravityLabelString(labels["trajectory_id"])+"-0" {
		t.Fatalf("consecutive user messages without assistant reply incremented turn: request_id=%v", labels["request_id"])
	}
}

func TestAntigravityShortModeHTTP2IdleTimeoutAndTLSServerName(t *testing.T) {
	t.Parallel()

	tr := &http.Transport{}
	applyAntigravityPoolLimits(tr, nil)
	if tr.IdleConnTimeout != time.Second {
		t.Fatalf("shortMode IdleConnTimeout = %v, want 1s so pooled HTTP/2 conns expire", tr.IdleConnTimeout)
	}
	if got := antigravityTLSServerName("1.2.3.4:443"); got != "" {
		t.Fatalf("antigravityTLSServerName(IPv4) = %q, want empty", got)
	}
	if got := antigravityTLSServerName("[fe80::1%en0]:443"); got != "" {
		t.Fatalf("antigravityTLSServerName(IPv6 zone) = %q, want empty", got)
	}
	if got := antigravityTLSServerName("bücher.example.com:443"); got != "xn--bcher-kva.example.com" {
		t.Fatalf("antigravityTLSServerName(IDN) = %q, want xn--bcher-kva.example.com", got)
	}
}
