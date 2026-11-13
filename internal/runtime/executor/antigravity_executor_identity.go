package executor

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// The Antigravity CLI identifies every agent request with a cascade, a trajectory and a step
// counter, and mirrors those identifiers into request.labels. Upstream appears to read them as
// the agent fingerprint, so the proxy derives the same identity chain from the conversation
// instead of sending a fresh random ID per request.
const (
	// antigravityIdentityCacheSize bounds the conversation identity cache; entries are tiny
	// and only live as long as a conversation stays active.
	antigravityIdentityCacheSize = 4096
	// antigravityMaxRequestSteps caps the counters so they cannot grow past the upstream
	// request ID budget.
	antigravityMaxRequestSteps = uint64(1) << 40
)

var (
	antigravityIdentityCache = cache.NewBoundedLRU[string, *antigravityConversationIdentity](antigravityIdentityCacheSize, nil)
	antigravityIdentityNow   = func() int64 { return time.Now().UnixMilli() }
)

// antigravityCallIdentity is the identity chain of one upstream request. The trajectory
// analytics report of the same conversation must reuse these identifiers verbatim.
type antigravityCallIdentity struct {
	cascadeID     string
	rootCascadeID string
	trajectoryID  string
	// sessionID is the upstream conversation ID; the report repeats it in the usage header.
	sessionID string
	// lastStepIndex and modelCalls mirror the CLI counters of the request they belong to.
	lastStepIndex uint64
	modelCalls    uint64
}

// antigravityConversationIdentity is the state the CLI keeps per conversation: one cascade for
// the whole conversation, the trajectory of the current user turn, and the counters that advance
// with every request of that turn. The cascade restarts when the conversation switches model,
// which is how upstream tells a continuing conversation from a new agent.
type antigravityConversationIdentity struct {
	cascadeID       string
	trajectoryID    string
	nextStep        uint64
	modelCalls      uint64
	turnCount       int
	generationModel string
}

// antigravityIdentityKey is the context key carrying the identity of the in-flight request so
// the post-response report can reuse it without widening every call signature.
type antigravityIdentityKey struct{}

// withAntigravityCallIdentity stores the identity chain of a request in its context.
func withAntigravityCallIdentity(ctx context.Context, call antigravityCallIdentity) context.Context {
	return context.WithValue(ctx, antigravityIdentityKey{}, call)
}

// antigravityCallIdentityFromContext returns the identity chain of the in-flight request.
func antigravityCallIdentityFromContext(ctx context.Context) (antigravityCallIdentity, bool) {
	if ctx == nil {
		return antigravityCallIdentity{}, false
	}
	call, ok := ctx.Value(antigravityIdentityKey{}).(antigravityCallIdentity)
	return call, ok && call.cascadeID != ""
}

// newAntigravityCallIdentity returns fresh identifiers for a request that does not belong to a
// tracked agent conversation, mirroring the CLI starting a new cascade whenever it cannot
// continue the previous one.
func newAntigravityCallIdentity() antigravityCallIdentity {
	cascadeID := uuid.NewString()
	return antigravityCallIdentity{
		cascadeID:     cascadeID,
		rootCascadeID: cascadeID,
		trajectoryID:  uuid.NewString(),
	}
}

// stableAntigravityUUID maps an identity input to a UUID, following the same provider-scoped
// scheme used for derived session IDs so the identifiers stay reproducible across restarts.
func stableAntigravityUUID(kind string, identityValue string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(strings.Join([]string{"cli-proxy-api", "antigravity", kind, identityValue}, "\x00"))).String()
}

// deriveAntigravityConversationIdentity rebuilds the identity chain of a conversation turn.
// OpenAI style clients keep no upstream state between turns, so every field has to be
// reproducible from data the proxy already holds. The unreachable turn count forces the first
// request to open a trajectory.
func newAntigravityConversationIdentity(sessionID string) *antigravityConversationIdentity {
	return &antigravityConversationIdentity{
		cascadeID: stableAntigravityUUID("cascade", sessionID),
		nextStep:  1,
		turnCount: -1,
	}
}

// nextAntigravityCallIdentity returns the identity chain of the next agent request of the
// conversation identified by sessionID. The CLI rotates the trajectory on every new user turn
// and restarts the step counter there, so the user turn count takes part in the derivation.
// requestModel is the model the client asked for, which is what the CLI uses to decide whether
// a request continues the current cascade.
func nextAntigravityCallIdentity(doc *ojson.Doc, sessionID, requestModel string) antigravityCallIdentity {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return newAntigravityCallIdentity()
	}
	requestModel = strings.ToLower(strings.TrimSpace(requestModel))
	turns := antigravityUserTurnCount(doc)
	identity := antigravityIdentityCache.GetOrAdd(sessionID, func() *antigravityConversationIdentity {
		return newAntigravityConversationIdentity(sessionID)
	})
	if identity.generationModel != "" && identity.generationModel != requestModel {
		// The CLI starts a new cascade when the agent model changes and reports the
		// previous trajectory as interrupted.
		*identity = *newAntigravityConversationIdentity(sessionID + "|" + requestModel)
	}
	identity.generationModel = requestModel
	if identity.turnCount != turns {
		identity.turnCount = turns
		identity.trajectoryID = stableAntigravityUUID("trajectory", sessionID+"|"+strconv.Itoa(turns))
		identity.nextStep = 1
		identity.modelCalls = 0
	}
	step := identity.nextStep
	modelCalls := identity.modelCalls
	if identity.nextStep < antigravityMaxRequestSteps {
		identity.nextStep++
	}
	if identity.modelCalls < antigravityMaxRequestSteps {
		identity.modelCalls++
	}
	return antigravityCallIdentity{
		cascadeID:     identity.cascadeID,
		rootCascadeID: identity.cascadeID,
		trajectoryID:  identity.trajectoryID,
		sessionID:     sessionID,
		lastStepIndex: step - 1,
		modelCalls:    modelCalls,
	}
}

// antigravityUserTurnCount counts the user turns of a request, which is how the CLI bounds the
// trajectory a step counter belongs to.
func antigravityUserTurnCount(doc *ojson.Doc) int {
	if doc == nil {
		return 0
	}
	// The envelope nests the conversation under request, while a payload that still uses the
	// Gemini shape keeps it at the top level.
	contents := doc.Get("request.contents")
	if !contents.IsArray() {
		contents = doc.Get("contents")
	}
	if !contents.IsArray() {
		return 0
	}
	count := 0
	contents.ForEach(func(_, item ojson.Result) bool {
		if strings.EqualFold(item.Get("role").String(), "user") {
			count++
		}
		return true
	})
	return count
}

// antigravityAgentRequestID renders the request ID the CLI sends for an agent request:
// agent/<cascade_id>/<unix_millis>/<trajectory_id>/<step>.
func antigravityAgentRequestID(call antigravityCallIdentity) string {
	return "agent/" + call.cascadeID + "/" + strconv.FormatInt(antigravityIdentityNow(), 10) + "/" + call.trajectoryID + "/" + strconv.FormatUint(call.lastStepIndex+1, 10)
}

// antigravitySetRequestLabels fills request.labels the way the CLI does for agent requests.
// Every value is a string, including the counters and booleans. modelEnum is the internal
// identifier of the routed model when the account catalog reports it.
func antigravitySetRequestLabels(doc *ojson.Doc, call antigravityCallIdentity, modelEnum string) {
	if doc == nil || call.cascadeID == "" {
		return
	}
	usedClaude := strings.Contains(strings.ToLower(doc.Get("model").String()), "claude")
	usedNonGemini := usedClaude
	if modelEnum != "" && !strings.HasPrefix(modelEnum, "MODEL_GEMINI") && !strings.HasPrefix(modelEnum, "MODEL_PLACEHOLDER") {
		usedNonGemini = true
	}
	doc.Set("request.labels.cascade_id", call.cascadeID)
	doc.Set("request.labels.root_cascade_id", call.rootCascadeID)
	doc.Set("request.labels.trajectory_id", call.trajectoryID)
	doc.Set("request.labels.request_id", call.trajectoryID+"-"+strconv.FormatUint(call.modelCalls, 10))
	if modelEnum != "" {
		doc.Set("request.labels.model_enum", modelEnum)
	}
	doc.Set("request.labels.last_step_index", strconv.FormatUint(call.lastStepIndex, 10))
	doc.Set("request.labels.used_claude", strconv.FormatBool(usedClaude))
	doc.Set("request.labels.used_claude_conservative", "false")
	doc.Set("request.labels.used_non_gemini_model", strconv.FormatBool(usedNonGemini))
}

// antigravityModelEnum returns the internal model identifier the account catalog reports for
// modelID. It is empty until the catalog of the credential is known, which mirrors the CLI
// omitting the label for models it cannot resolve.
func antigravityModelEnum(modelID string) string {
	return registry.AntigravityModelEnum(modelID)
}
