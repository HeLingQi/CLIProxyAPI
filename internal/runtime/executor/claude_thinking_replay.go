package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/MoeForge/moego/ojson"
	internalcache "github.com/router-for-me/CLIProxyAPI/v8/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// claudeThinkingReplayScope carries the bounded replay state for one request.
type claudeThinkingReplayScope struct {
	modelFamily   string
	sessionKey    string
	snapshot      internalcache.ClaudeThinkingReplaySnapshot
	cacheReady    bool
	replayApplied bool
}

func (s claudeThinkingReplayScope) valid() bool {
	return strings.TrimSpace(s.modelFamily) != "" && strings.TrimSpace(s.sessionKey) != ""
}

// isolateReplaySessionKey scopes a replay key to the calling API key so callers
// never share hidden reasoning state through a colliding session key.
func isolateReplaySessionKey(ctx context.Context, sessionKey string) string {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return ""
	}
	if strings.HasPrefix(sessionKey, "execution:") {
		return sessionKey
	}
	apiKey := strings.TrimSpace(helps.APIKeyFromContext(ctx))
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return "caller:" + hex.EncodeToString(sum[:8]) + ":" + sessionKey
}

func claudeThinkingReplayEnabled(auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) bool {
	if auth == nil || !sourceFormatEqual(opts.SourceFormat, sdktranslator.FormatClaude) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") || auth.AuthKind() != cliproxyauth.AuthKindAPIKey {
		return false
	}
	if !helps.APIKeyModelIsCompat(req) {
		return false
	}
	apiKey, _ := claudeCreds(auth)
	return strings.TrimSpace(apiKey) != "" && !isClaudeOAuthToken(apiKey)
}

// A missing session identity intentionally disables replay instead of sharing hidden reasoning across callers.
func claudeThinkingReplayScopeFromRequest(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) claudeThinkingReplayScope {
	sessionKey := codexReasoningReplaySessionKey(ctx, sdktranslator.FormatClaude, req, opts, req.Payload)
	sessionKey = isolateReplaySessionKey(ctx, sessionKey)
	return claudeThinkingReplayScope{
		modelFamily: claudeThinkingReplayModelFamily(auth, req.Model),
		sessionKey:  sessionKey,
	}
}

func claudeThinkingReplayModelFamily(auth *cliproxyauth.Auth, model string) string {
	baseModel := thinking.ParseSuffix(strings.TrimSpace(model)).ModelName
	if baseModel == "" {
		return ""
	}
	identity := ""
	if auth != nil {
		identity = strings.TrimSpace(auth.ID)
		if identity == "" {
			apiKey, baseURL := claudeCreds(auth)
			identity = strings.TrimSpace(baseURL)
			if identity == "" {
				identity = strings.TrimSpace(apiKey)
			}
		}
	}
	if identity == "" {
		return "claude:" + baseModel
	}
	sum := sha256.Sum256([]byte(identity))
	return "claude:" + hex.EncodeToString(sum[:8]) + ":" + baseModel
}

func prepareClaudeThinkingReplayRequest(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Request, claudeThinkingReplayScope) {
	scope := claudeThinkingReplayScopeFromRequest(ctx, auth, req, opts)
	if !scope.valid() {
		return req, scope
	}
	contents, snapshot, found, errGet := internalcache.GetClaudeThinkingReplayWithSnapshotRequired(ctx, scope.modelFamily, scope.sessionKey)
	scope.snapshot = snapshot
	scope.cacheReady = errGet == nil
	if errGet != nil {
		log.Warnf("claude compatible thinking replay cache read failed: %v", errGet)
		return req, scope
	}
	if !found {
		return req, scope
	}
	updated, restored := restoreClaudeThinkingReplayContents(req.Payload, contents)
	if restored {
		req.Payload = updated
		scope.replayApplied = true
	}
	return req, scope
}

func restoreClaudeThinkingReplayContents(body []byte, cachedContents [][]byte) ([]byte, bool) {
	if len(cachedContents) == 0 {
		return body, false
	}
	doc := ojson.NewDoc(body)
	restored := false
	for _, cachedContent := range cachedContents {
		if restoreClaudeThinkingReplayContentDoc(doc, cachedContent) {
			restored = true
		}
	}
	if !restored {
		return body, false
	}
	return doc.Bytes(), true
}

func cacheClaudeThinkingReplayResponse(ctx context.Context, scope claudeThinkingReplayScope, response []byte) {
	content := ojson.GetBytesNoCopy(response, "content")
	if content.IsArray() {
		cacheClaudeThinkingReplayContent(ctx, scope, []byte(content.Raw))
		return
	}
	accumulator := newClaudeThinkingReplayStreamAccumulator()
	accumulator.observe(response)
	if content, completed := accumulator.content(); completed {
		cacheClaudeThinkingReplayContent(ctx, scope, content)
	}
}

func cacheClaudeThinkingReplayContent(ctx context.Context, scope claudeThinkingReplayScope, content []byte) {
	if !scope.valid() || !scope.cacheReady {
		return
	}
	if claudeThinkingReplayContentIsReplayable(content) {
		if _, errReplace := internalcache.ReplaceClaudeThinkingReplayIfUnchanged(ctx, scope.modelFamily, scope.sessionKey, scope.snapshot, content); errReplace != nil {
			log.Warnf("claude compatible thinking replay cache replace failed: %v", errReplace)
		}
		return
	}
	clearClaudeThinkingReplayContent(ctx, scope)
}

func clearClaudeThinkingReplayContent(ctx context.Context, scope claudeThinkingReplayScope) {
	if !scope.valid() || !scope.cacheReady {
		return
	}
	if _, errDelete := internalcache.DeleteClaudeThinkingReplayIfUnchanged(ctx, scope.modelFamily, scope.sessionKey, scope.snapshot); errDelete != nil {
		log.Warnf("claude compatible thinking replay cache delete failed: %v", errDelete)
	}
}

func wrapClaudeThinkingReplayStream(ctx context.Context, result *cliproxyexecutor.StreamResult, scope claudeThinkingReplayScope) *cliproxyexecutor.StreamResult {
	return wrapThinkingReplayStream(ctx, result, scope, cacheClaudeThinkingReplayContent, clearClaudeThinkingReplayContent)
}

type claudeThinkingReplayStreamBlock struct {
	raw                  []byte
	text                 strings.Builder
	thinking             strings.Builder
	signature            strings.Builder
	input                strings.Builder
	textInitialized      bool
	thinkingInitialized  bool
	signatureInitialized bool
	hasInputDelta        bool
	finished             bool
}

type claudeThinkingReplayStreamAccumulator struct {
	blocks        map[int]*claudeThinkingReplayStreamBlock
	observed      bool
	complete      bool
	upstreamError bool
	abandoned     bool
	bytesUsed     int
}

func newClaudeThinkingReplayStreamAccumulator() *claudeThinkingReplayStreamAccumulator {
	return &claudeThinkingReplayStreamAccumulator{blocks: make(map[int]*claudeThinkingReplayStreamBlock)}
}

func (a *claudeThinkingReplayStreamAccumulator) observe(chunk []byte) {
	for _, line := range bytes.Split(chunk, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if !ojson.ValidBytes(payload) {
			a.abandon()
			continue
		}
		root := util.ParseGJSONBytesNoCopy(payload)
		switch root.Get("type").String() {
		case "message_start":
			a.observed = true
		case "content_block_start":
			if !a.abandoned {
				a.observeBlockStart(root)
			}
		case "content_block_delta":
			if !a.abandoned {
				a.observeBlockDelta(root)
			}
		case "content_block_stop":
			if !a.abandoned {
				a.finishBlock(int(root.Get("index").Int()))
			}
		case "message_stop":
			a.complete = true
		case "error":
			a.upstreamError = true
			a.abandon()
		}
	}
}

func (a *claudeThinkingReplayStreamAccumulator) observeBlockStart(root ojson.Result) {
	index := int(root.Get("index").Int())
	block := root.Get("content_block")
	if !block.IsObject() || len(a.blocks) >= internalcache.ClaudeThinkingReplayCacheMaxBlocksPerTurn {
		a.abandon()
		return
	}
	if _, exists := a.blocks[index]; exists {
		a.abandon()
		return
	}
	raw := []byte(block.Raw)
	if !a.reserveBytes(len(raw)) {
		return
	}
	a.blocks[index] = &claudeThinkingReplayStreamBlock{raw: append([]byte(nil), raw...)}
}

func (a *claudeThinkingReplayStreamAccumulator) observeBlockDelta(root ojson.Result) {
	index := int(root.Get("index").Int())
	block, ok := a.blocks[index]
	if !ok {
		a.abandon()
		return
	}
	delta := root.Get("delta")
	switch delta.Get("type").String() {
	case "text_delta":
		a.appendBlockText(block, &block.text, &block.textInitialized, "text", delta.Get("text").String())
	case "thinking_delta":
		a.appendBlockText(block, &block.thinking, &block.thinkingInitialized, "thinking", delta.Get("thinking").String())
	case "signature_delta":
		a.appendBlockText(block, &block.signature, &block.signatureInitialized, "signature", delta.Get("signature").String())
	case "input_json_delta":
		suffix := delta.Get("partial_json").String()
		if a.reserveBytes(len(suffix)) {
			block.input.WriteString(suffix)
			block.hasInputDelta = true
		}
	default:
		a.abandon()
	}
}

func (a *claudeThinkingReplayStreamAccumulator) appendBlockText(block *claudeThinkingReplayStreamBlock, builder *strings.Builder, initialized *bool, path, suffix string) {
	if !*initialized {
		initial := ojson.GetBytesNoCopy(block.raw, path).String()
		if !a.reserveBytes(len(initial)) {
			return
		}
		builder.WriteString(initial)
		*initialized = true
	}
	if a.reserveBytes(len(suffix)) {
		builder.WriteString(suffix)
	}
}

func (a *claudeThinkingReplayStreamAccumulator) finishBlock(index int) {
	block, ok := a.blocks[index]
	if !ok {
		a.abandon()
		return
	}
	if block.hasInputDelta && !ojson.Valid(block.input.String()) {
		a.abandon()
		return
	}
	block.finished = true
}

func (a *claudeThinkingReplayStreamAccumulator) reserveBytes(count int) bool {
	if count < 0 || a.bytesUsed > internalcache.ClaudeThinkingReplayCacheMaxBytesPerSession-count {
		a.abandon()
		return false
	}
	a.bytesUsed += count
	return true
}

func (a *claudeThinkingReplayStreamAccumulator) abandon() {
	a.abandoned = true
	a.blocks = nil
	a.bytesUsed = 0
}

func (a *claudeThinkingReplayStreamAccumulator) content() ([]byte, bool) {
	if !a.observed || !a.complete || a.upstreamError || a.abandoned {
		return nil, false
	}
	indexes := make([]int, 0, len(a.blocks))
	for index := range a.blocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	parts := make([][]byte, 0, len(indexes))
	for _, index := range indexes {
		block := a.blocks[index]
		if !block.finished {
			a.abandon()
			return nil, false
		}
		if !block.textInitialized && !block.thinkingInitialized && !block.signatureInitialized && !block.hasInputDelta {
			parts = append(parts, append([]byte(nil), block.raw...))
			continue
		}
		blockDoc := ojson.NewDoc(block.raw)
		var errSet error
		if block.textInitialized {
			errSet = blockDoc.Set("text", block.text.String())
		}
		if errSet == nil && block.thinkingInitialized {
			errSet = blockDoc.Set("thinking", block.thinking.String())
		}
		if errSet == nil && block.signatureInitialized {
			errSet = blockDoc.Set("signature", block.signature.String())
		}
		if errSet == nil && block.hasInputDelta {
			errSet = blockDoc.SetRawBytes("input", []byte(block.input.String()))
		}
		if errSet != nil {
			a.abandon()
			return nil, false
		}
		parts = append(parts, blockDoc.Bytes())
	}
	content := helps.JoinRawJSONArray(parts)
	if len(content) > internalcache.ClaudeThinkingReplayCacheMaxBytesPerSession {
		a.abandon()
		return nil, false
	}
	return content, true
}

type claudeReplayContentCacheFunc func(context.Context, claudeThinkingReplayScope, []byte)
type claudeReplayContentClearFunc func(context.Context, claudeThinkingReplayScope)

func wrapThinkingReplayStream(ctx context.Context, result *cliproxyexecutor.StreamResult, scope claudeThinkingReplayScope, cacheContent claudeReplayContentCacheFunc, clearContent claudeReplayContentClearFunc) *cliproxyexecutor.StreamResult {
	if result == nil || !scope.valid() {
		return result
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		accumulator := newClaudeThinkingReplayStreamAccumulator()
		hasError := false
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				hasError = true
			} else {
				accumulator.observe(chunk.Payload)
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
		}
		if hasError {
			return
		}
		if content, completed := accumulator.content(); completed {
			cacheContent(ctx, scope, content)
			return
		}
		if accumulator.upstreamError && scope.replayApplied {
			clearContent(ctx, scope)
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: result.Headers.Clone(), Chunks: out}
}

func restoreClaudeThinkingReplayContentDoc(doc *ojson.Doc, cachedContent []byte) bool {
	if doc == nil {
		return false
	}
	cachedParts, cachedOK := claudeNonThinkingContentParts(util.ParseGJSONBytesNoCopy(cachedContent))
	if !cachedOK {
		return false
	}
	messages := doc.Get("messages")
	if !messages.IsArray() {
		return false
	}
	messageItems := messages.Array()
	for index := len(messageItems) - 1; index >= 0; index-- {
		message := messageItems[index]
		if !strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "assistant") {
			continue
		}
		currentContent := message.Get("content")
		if claudeReplayJSONEqual([]byte(currentContent.Raw), cachedContent) {
			return false
		}
		if claudeContentHasThinking(currentContent) {
			continue
		}
		currentParts, currentOK := claudeNonThinkingContentParts(currentContent)
		if !currentOK || !claudeCanonicalPartsEqual(currentParts, cachedParts) {
			continue
		}
		return doc.SetRawBytes(fmt.Sprintf("messages.%d.content", index), cachedContent) == nil
	}
	return false
}

func claudeContentHasThinking(content ojson.Result) bool {
	if !content.IsArray() {
		return false
	}
	for _, part := range content.Array() {
		switch strings.TrimSpace(part.Get("type").String()) {
		case "thinking", "redacted_thinking":
			return true
		}
	}
	return false
}

func claudeNonThinkingContentParts(content ojson.Result) ([][]byte, bool) {
	if !content.IsArray() {
		return nil, false
	}
	parts := make([][]byte, 0, len(content.Array()))
	hasToolUse := false
	for _, part := range content.Array() {
		switch strings.TrimSpace(part.Get("type").String()) {
		case "thinking", "redacted_thinking":
			continue
		case "tool_use":
			if strings.TrimSpace(part.Get("id").String()) == "" {
				return nil, false
			}
			hasToolUse = true
		}
		canonical, ok := claudeReplayCanonicalJSON([]byte(part.Raw))
		if !ok {
			return nil, false
		}
		parts = append(parts, canonical)
	}
	return parts, hasToolUse
}

func claudeCanonicalPartsEqual(left, right [][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if !bytes.Equal(left[i], right[i]) {
			return false
		}
	}
	return true
}

func claudeReplayJSONEqual(left, right []byte) bool {
	canonicalLeft, leftOK := claudeReplayCanonicalJSON(left)
	canonicalRight, rightOK := claudeReplayCanonicalJSON(right)
	return leftOK && rightOK && bytes.Equal(canonicalLeft, canonicalRight)
}

func claudeReplayCanonicalJSON(raw []byte) ([]byte, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if errDecode := decoder.Decode(&value); errDecode != nil {
		return nil, false
	}
	canonical, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, false
	}
	return canonical, true
}

func claudeThinkingReplayContentIsReplayable(content []byte) bool {
	root := util.ParseGJSONBytesNoCopy(content)
	if !root.IsArray() {
		return false
	}
	hasSignedThinking := false
	hasToolUse := false
	for _, part := range root.Array() {
		switch strings.TrimSpace(part.Get("type").String()) {
		case "thinking":
			if strings.TrimSpace(part.Get("signature").String()) != "" {
				hasSignedThinking = true
			}
		case "tool_use":
			if strings.TrimSpace(part.Get("id").String()) != "" {
				hasToolUse = true
			}
		}
	}
	return hasSignedThinking && hasToolUse
}

func shouldClearClaudeThinkingReplayAfterError(err error) bool {
	if err == nil {
		return false
	}
	var upstreamStatus statusErr
	if !errors.As(err, &upstreamStatus) {
		return false
	}
	statusCode := upstreamStatus.StatusCode()
	return statusCode == 400 || statusCode == 422
}
