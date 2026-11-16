package executor

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func (e *AntigravityExecutor) buildRequest(ctx context.Context, auth *cliproxyauth.Auth, token, modelName string, payload []byte, stream bool, alt, baseURL string, derivedSessionIDs ...string) (*http.Request, error) {
	doc := ojson.NewDoc(payload)
	httpReq, _, err := e.buildRequestDoc(ctx, auth, token, modelName, doc, stream, alt, baseURL, derivedSessionIDs...)
	return httpReq, err
}

func (e *AntigravityExecutor) buildRequestDoc(ctx context.Context, auth *cliproxyauth.Auth, token, modelName string, doc *ojson.Doc, stream bool, alt, baseURL string, derivedSessionIDs ...string) (*http.Request, []byte, error) {
	if token == "" {
		return nil, nil, statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
	}
	if doc == nil {
		doc = ojson.NewDoc(nil)
	}

	base := strings.TrimSuffix(baseURL, "/")
	if base == "" {
		base = resolveAntigravityRequestBaseURL(auth)
	}
	path := antigravityGeneratePath
	if stream {
		path = antigravityStreamPath
	}
	var requestURL strings.Builder
	requestURL.WriteString(base)
	requestURL.WriteString(path)
	if stream {
		if alt != "" {
			requestURL.WriteString("?$alt=")
			requestURL.WriteString(url.QueryEscape(alt))
		} else {
			requestURL.WriteString("?alt=sse")
		}
	} else if alt != "" {
		requestURL.WriteString("?$alt=")
		requestURL.WriteString(url.QueryEscape(alt))
	}

	projectID, errProject := e.projectIDForRequest(ctx, auth, token)
	if errProject != nil {
		return nil, nil, errProject
	}
	geminiToAntigravityDoc(doc, modelName, projectID, derivedSessionIDs...)
	promptStructuredOutput := helps.PrepareAntigravityClaudeStructuredOutputDoc(modelName, doc)

	// Cap maxOutputTokens to model's output ceiling from registry
	if maxOut := doc.Get("request.generationConfig.maxOutputTokens"); maxOut.Exists() && maxOut.Type == ojson.Number {
		if maxCap := antigravityOutputCeiling(lookupAntigravityModelInfo(modelName)); maxCap > 0 {
			if int(maxOut.Int()) > maxCap {
				_ = doc.Set("request.generationConfig.maxOutputTokens", maxCap)
			}
		}
	}

	lowerModel := strings.ToLower(strings.TrimSpace(modelName))
	isClaudeModel := strings.Contains(lowerModel, "claude")
	useAntigravitySchema := isClaudeModel || strings.Contains(lowerModel, "gemini-3-pro") || strings.Contains(lowerModel, "gemini-3.1-pro")
	if antigravityRequestNeedsSchemaSanitizationDoc(doc) {
		sanitizeAntigravityRequestSchemasForModelDoc(doc, useAntigravitySchema, isClaudeModel)
	}
	if isClaudeModel {
		if !promptStructuredOutput {
			_ = doc.Set("request.toolConfig.functionCallingConfig.mode", "VALIDATED")
		}
		if reqType := strings.ToLower(strings.TrimSpace(doc.Get("requestType").String())); reqType != "" && reqType != "agent" {
			_ = doc.Delete("request.generationConfig.maxOutputTokens")
		}
		if doc.Get("request.generationConfig.thinkingConfig.thinkingLevel").Exists() {
			_ = doc.Delete("request.generationConfig.thinkingConfig.thinkingBudget")
			if !doc.Get("request.generationConfig.thinkingConfig.includeThoughts").Exists() {
				_ = doc.Set("request.generationConfig.thinkingConfig.includeThoughts", true)
			}
		}
	} else {
		applyAntigravityGeminiGenerationShapeDoc(doc, modelName)
	}
	applyAntigravitySystemInstructionRoleDoc(doc)

	applyAntigravityNativeSignatureReplayIfNeededDoc(modelName, doc)
	helps.FinalizePayloadDoc(ctx, doc)
	finalPayload := doc.Bytes()
	var (
		bodyReader io.Reader = bytes.NewReader(finalPayload)
		payloadLog []byte
	)
	if e.cfg != nil && e.cfg.RequestLog {
		payloadLog = append([]byte(nil), finalPayload...)
	}

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), bodyReader)
	if errReq != nil {
		return nil, nil, errReq
	}
	// Deliberately no httpReq.Close: the native Antigravity client omits the
	// Connection header and keeps its HTTP/1.1 connections alive, so forcing
	// "Connection: close" would both deviate from that fingerprint and defeat the
	// shared connection pool by discarding every established TCP + TLS session.
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("User-Agent", resolveUserAgent(auth))
	if host := resolveHost(base); host != "" {
		httpReq.Host = host
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       requestURL.String(),
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      payloadLog,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	return httpReq, finalPayload, nil
}

// sanitizeAntigravityRequestSchemas cleans the JSON schemas carried by an Antigravity request.
//
// Cleaning is applied only to the payload locations that actually hold a JSON schema. The schema
// cleaner rewrites keys such as "title", "format", "default" and "const", which are also ordinary
// data keys inside functionCall arguments replayed from conversation history. Running it over the
// whole document silently mutated that history, so tools lost required argument fields and the
// model imitated the corrupted examples on later turns.
// sanitizeAntigravityRequestSchemas cleans with union-preserving (Gemini) tool semantics.
// Claude tool unions are flattened only when the caller supplies the model flag via
// sanitizeAntigravityRequestSchemasForModel; the placeholder group alone must not decide it,
// because gemini-3-pro models share that group and accept preserved unions.
func sanitizeAntigravityRequestSchemas(payloadStr string, useAntigravitySchema bool) string {
	return sanitizeAntigravityRequestSchemasForModel(payloadStr, useAntigravitySchema, false)
}

// sanitizeAntigravityRequestSchemasForModel applies sanitizeAntigravityRequestSchemas with an
// explicit Antigravity Claude flag. Antigravity Claude tool schemas additionally flatten
// multi-branch anyOf/oneOf unions, which the Claude-side upstream rejects (Issue 3662).
func sanitizeAntigravityRequestSchemasForModel(payloadStr string, useAntigravitySchema, isClaudeModel bool) string {
	payloadStr = sanitizeAntigravityToolSchemasForModel(payloadStr, useAntigravitySchema, isClaudeModel)
	return sanitizeAntigravityGenerationSchemas(payloadStr)
}

func sanitizeAntigravityRequestSchemasForModelDoc(doc *ojson.Doc, useAntigravitySchema, isClaudeModel bool) bool {
	if doc == nil {
		return false
	}
	changed := sanitizeAntigravityToolSchemasForModelDoc(doc, useAntigravitySchema, isClaudeModel)
	if sanitizeAntigravityGenerationSchemasDoc(doc) {
		changed = true
	}
	return changed
}

// sanitizeAntigravityToolSchemas applies the existing declaration rewrites to
// a small document containing only request.tools, then replaces that subtree
// once. This preserves rewrite order and bytes without copying the full request
// for every declaration schema.
func sanitizeAntigravityToolSchemas(payloadStr string, useAntigravitySchema bool) string {
	return sanitizeAntigravityToolSchemasForModel(payloadStr, useAntigravitySchema, false)
}

func sanitizeAntigravityToolSchemasForModel(payloadStr string, useAntigravitySchema, isClaudeModel bool) string {
	tools := ojson.Get(payloadStr, "request.tools")
	if !tools.IsArray() {
		return payloadStr
	}

	toolDocument := `{"request":{"tools":` + tools.Raw + `}}`
	toolDocument = sanitizeAntigravityToolSchemaDocumentForModel(toolDocument, useAntigravitySchema, isClaudeModel)
	cleanedTools := ojson.Get(toolDocument, "request.tools")
	if !cleanedTools.IsArray() || cleanedTools.Raw == tools.Raw {
		return payloadStr
	}
	updated, errSet := ojson.SetRawBytes([]byte(payloadStr), "request.tools", []byte(cleanedTools.Raw))
	if errSet != nil {
		log.Debugf("antigravity: failed to write cleaned request.tools: %v", errSet)
		return payloadStr
	}
	return string(updated)
}

func sanitizeAntigravityToolSchemasForModelDoc(doc *ojson.Doc, useAntigravitySchema, isClaudeModel bool) bool {
	if doc == nil {
		return false
	}
	tools := doc.Get("request.tools")
	if !tools.IsArray() {
		return false
	}

	toolDocument := `{"request":{"tools":` + tools.Raw + `}}`
	toolDocument = sanitizeAntigravityToolSchemaDocumentForModel(toolDocument, useAntigravitySchema, isClaudeModel)
	cleanedTools := ojson.Get(toolDocument, "request.tools")
	if !cleanedTools.IsArray() || cleanedTools.Raw == tools.Raw {
		return false
	}
	if errSet := doc.SetRawBytes("request.tools", []byte(cleanedTools.Raw)); errSet != nil {
		log.Debugf("antigravity: failed to write cleaned request.tools: %v", errSet)
		return false
	}
	return true
}

func sanitizeAntigravityToolSchemaDocument(payloadStr string, useAntigravitySchema bool) string {
	return sanitizeAntigravityToolSchemaDocumentForModel(payloadStr, useAntigravitySchema, false)
}

// antigravityDeclarationContainers lists both spellings of the declarations
// array a tool may carry.
var antigravityDeclarationContainers = []string{"functionDeclarations", "function_declarations"}

// sanitizeAntigravityToolSchemaDocumentForModel renames and cleans every
// declaration schema in the tools-only document. Each declaration is edited
// on its own raw bytes and the results are spliced back into the tools array
// in a single pass, so the cost stays linear in the document size instead of
// scaling with declarations x document size (Issue 6714). The emitted bytes
// match the previous whole-document edits: changed elements are spliced at
// their original offsets, so untouched bytes (including whitespace around
// array elements) survive verbatim, and changed nodes go through the same
// sjson edits, scoped to one declaration.
func sanitizeAntigravityToolSchemaDocumentForModel(payloadStr string, useAntigravitySchema, isClaudeModel bool) string {
	toolSchemaCleaner := func(schema string, isParameterSchema bool) string {
		// Only declaration parameter schemas become Claude custom.input_schema upstream,
		// so only they need Claude union flattening. Function return schemas keep the
		// union-preserving cleaning like every other response-side schema.
		if isClaudeModel && isParameterSchema {
			return util.CleanJSONSchemaForAntigravityClaudeTool(schema, useAntigravitySchema)
		}
		return util.CleanJSONSchemaForAntigravityTool(schema, useAntigravitySchema)
	}
	cleanNestedToolSchema := func(schemaRaw string, isParameterSchema bool) string {
		return cleanNestedSchema(func(schema string) string {
			return toolSchemaCleaner(schema, isParameterSchema)
		}, schemaRaw)
	}

	tools := ojson.Parse(payloadStr).Get("request.tools")
	if !tools.IsArray() {
		return payloadStr
	}
	elements := make([]ojson.Result, 0)
	edits := make([]antigravityElementEdit, 0)
	tools.ForEach(func(_, tool ojson.Result) bool {
		elements = append(elements, tool)
		if !tool.IsObject() {
			return true
		}
		var toolDoc *ojson.Doc
		toolChanged := false
		for _, declKey := range antigravityDeclarationContainers {
			decls := tool.Get(declKey)
			if !decls.IsArray() {
				continue
			}
			cleanedDecls, declsChanged := sanitizeAntigravityDeclarations(decls, cleanNestedToolSchema)
			if !declsChanged {
				continue
			}
			if toolDoc == nil {
				toolDoc = ojson.NewDoc([]byte(tool.Raw))
			}
			if errSet := toolDoc.SetRawBytes(declKey, []byte(cleanedDecls)); errSet != nil {
				log.Debugf("antigravity: failed to write cleaned %s: %v", declKey, errSet)
				continue
			}
			toolChanged = true
		}
		if toolChanged && toolDoc != nil {
			toolRaw := string(toolDoc.Bytes())
			if toolRaw != tool.Raw {
				edits = append(edits, antigravityElementEdit{offset: tool.Index - tools.Index, raw: tool.Raw, replacement: toolRaw})
			}
		}
		return true
	})
	if len(edits) == 0 {
		return payloadStr
	}
	updated, errSet := ojson.SetRawBytes([]byte(payloadStr), "request.tools", []byte(applyAntigravityElementEdits(tools.Raw, tools.Index, elements, edits)))
	if errSet != nil {
		log.Debugf("antigravity: failed to write cleaned request.tools: %v", errSet)
		return payloadStr
	}
	return string(updated)
}

// antigravityElementEdit replaces one array element (raw, at offset within
// the array's raw bytes) with replacement.
type antigravityElementEdit struct {
	offset      int
	raw         string
	replacement string
}

// applyAntigravityElementEdits splices element replacements into an array's
// raw bytes in offset order, keeping every other byte (commas, whitespace,
// unchanged elements) verbatim. Edits whose offsets do not line up with the
// array bytes fall back to joining the elements with commas: still valid
// and semantically identical, just without the original separators. gjson
// reports element offsets into the parsed document, so offsets here are
// relative to the array start.
func applyAntigravityElementEdits(arrayRaw string, arrayIndex int, elements []ojson.Result, edits []antigravityElementEdit) string {
	var rebuilt strings.Builder
	rebuilt.Grow(len(arrayRaw))
	cursor := 0
	for _, edit := range edits {
		if edit.offset < cursor || edit.offset+len(edit.raw) > len(arrayRaw) || arrayRaw[edit.offset:edit.offset+len(edit.raw)] != edit.raw {
			return joinAntigravityElements(arrayIndex, elements, edits)
		}
		rebuilt.WriteString(arrayRaw[cursor:edit.offset])
		rebuilt.WriteString(edit.replacement)
		cursor = edit.offset + len(edit.raw)
	}
	rebuilt.WriteString(arrayRaw[cursor:])
	return rebuilt.String()
}

// joinAntigravityElements rebuilds an array by joining element bytes with
// commas, substituting replacements where recorded. It is the fallback for
// applyAntigravityElementEdits when splice offsets cannot be trusted.
func joinAntigravityElements(arrayIndex int, elements []ojson.Result, edits []antigravityElementEdit) string {
	replacements := make(map[int]string, len(edits))
	for _, edit := range edits {
		replacements[edit.offset] = edit.replacement
	}
	var rebuilt strings.Builder
	rebuilt.WriteByte('[')
	for i, element := range elements {
		if i > 0 {
			rebuilt.WriteByte(',')
		}
		if replacement, ok := replacements[element.Index-arrayIndex]; ok {
			rebuilt.WriteString(replacement)
		} else {
			rebuilt.WriteString(element.Raw)
		}
	}
	rebuilt.WriteByte(']')
	return rebuilt.String()
}

// sanitizeAntigravityDeclarations rebuilds one declarations array, applying
// the rename and schema cleaning to each declaration in isolation. It
// reports whether any declaration actually changed so callers can keep the
// original bytes when the array is already clean.
func sanitizeAntigravityDeclarations(decls ojson.Result, clean func(schemaRaw string, isParameterSchema bool) string) (string, bool) {
	elements := make([]ojson.Result, 0)
	edits := make([]antigravityElementEdit, 0)
	decls.ForEach(func(_, decl ojson.Result) bool {
		elements = append(elements, decl)
		if !decl.IsObject() {
			return true
		}
		cleanedDecl := sanitizeAntigravityDeclaration(decl, clean)
		if cleanedDecl != decl.Raw {
			edits = append(edits, antigravityElementEdit{offset: decl.Index - decls.Index, raw: decl.Raw, replacement: cleanedDecl})
		}
		return true
	})
	if len(edits) == 0 {
		return "", false
	}
	return applyAntigravityElementEdits(decls.Raw, decls.Index, elements, edits), true
}

// sanitizeAntigravityDeclaration applies the declaration-level edits that
// used to run against the whole tools document: parametersJsonSchema is
// renamed onto parameters first, then every object schema is cleaned in
// place. Failed edits keep the declaration as-is and are only logged.
func sanitizeAntigravityDeclaration(decl ojson.Result, clean func(schemaRaw string, isParameterSchema bool) string) string {
	declRaw := decl.Raw
	if decl.Get("parametersJsonSchema").Exists() {
		renamed, errRename := util.RenameKey(declRaw, "parametersJsonSchema", "parameters")
		if errRename != nil {
			log.Debugf("antigravity: failed to rename parametersJsonSchema: %v", errRename)
		} else {
			declRaw = renamed
			decl = ojson.Parse(declRaw)
		}
	}
	var declDoc *ojson.Doc
	changed := false
	for _, key := range antigravityDeclarationSchemaKeys {
		schema := decl.Get(key)
		if !schema.IsObject() {
			continue
		}
		cleanedSchema := clean(schema.Raw, isParameterSchemaPath(key))
		if cleanedSchema == schema.Raw {
			continue
		}
		if declDoc == nil {
			declDoc = ojson.NewDoc([]byte(declRaw))
		}
		if errSet := declDoc.SetRawBytes(key, []byte(cleanedSchema)); errSet != nil {
			log.Debugf("antigravity: failed to write cleaned schema at %s: %v", key, errSet)
			continue
		}
		changed = true
	}
	if changed && declDoc != nil {
		return string(declDoc.Bytes())
	}
	return declRaw
}

// sanitizeAntigravityGenerationSchemas batches every schema edit within one
// generation config before replacing that config in the full request.
func sanitizeAntigravityGenerationSchemas(payloadStr string) string {
	doc := ojson.NewDoc([]byte(payloadStr))
	if !sanitizeAntigravityGenerationSchemasDoc(doc) {
		return payloadStr
	}
	return string(doc.Bytes())
}

func sanitizeAntigravityGenerationSchemasDoc(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	changed := false
	for _, container := range antigravityGenerationConfigContainers {
		generationConfig := doc.Get(container)
		if !generationConfig.IsObject() {
			continue
		}
		var configDoc *ojson.Doc
		configChanged := false
		for _, key := range antigravityGenerationSchemaKeys {
			schema := generationConfig.Get(key)
			if !schema.IsObject() {
				continue
			}
			cleanedSchema := util.CleanJSONSchemaForAntigravityResponse(schema.Raw)
			if cleanedSchema == schema.Raw {
				continue
			}
			if configDoc == nil {
				configDoc = ojson.NewDoc([]byte(generationConfig.Raw))
			}
			if errSet := configDoc.SetRawBytes(key, []byte(cleanedSchema)); errSet != nil {
				log.Debugf("antigravity: failed to write cleaned schema at %s.%s: %v", container, key, errSet)
				continue
			}
			configChanged = true
		}
		if !configChanged || configDoc == nil {
			continue
		}
		cleanedConfig := configDoc.Bytes()
		if string(cleanedConfig) == generationConfig.Raw {
			continue
		}
		if errSet := doc.SetRawBytes(container, cleanedConfig); errSet != nil {
			log.Debugf("antigravity: failed to write cleaned %s: %v", container, errSet)
			continue
		}
		changed = true
	}
	return changed
}

// isParameterSchemaPath reports whether a declaration schema path holds tool input
// parameters rather than a function return schema.
func isParameterSchemaPath(schemaPath string) bool {
	key := schemaPath
	if idx := strings.LastIndex(schemaPath, "."); idx >= 0 {
		key = schemaPath[idx+1:]
	}
	return strings.HasPrefix(key, "parameters")
}

func cleanAntigravitySchemasAtPaths(payloadStr string, schemaPaths []string, clean func(string) string) string {
	var doc *ojson.Doc
	changed := false
	for _, schemaPath := range schemaPaths {
		schema := ojson.Get(payloadStr, schemaPath)
		if !schema.Exists() {
			continue
		}
		cleanedSchema := clean(schema.Raw)
		if cleanedSchema == schema.Raw {
			continue
		}
		if doc == nil {
			doc = ojson.NewDoc([]byte(payloadStr))
		}
		if errSet := doc.SetRawBytes(schemaPath, []byte(cleanedSchema)); errSet != nil {
			log.Debugf("antigravity: failed to write cleaned schema at %s: %v", schemaPath, errSet)
			continue
		}
		changed = true
	}
	if !changed || doc == nil {
		return payloadStr
	}
	return string(doc.Bytes())
}

// antigravitySchemaWrapperKey nests a schema during cleaning. It is never sent upstream.
const antigravitySchemaWrapperKey = "schema"

// cleanNestedSchema cleans a schema with it nested one level down, then unwraps it.
//
// The cleaner deliberately skips placeholder insertion for a top-level schema, but Claude's
// VALIDATED mode needs every tool schema to declare at least one required property. Whole-payload
// cleaning always saw tool schemas nested inside the request, so nesting is reproduced here to keep
// the emitted schema byte-identical to the previous behaviour.
func cleanNestedSchema(clean func(string) string, schemaRaw string) string {
	wrapped, errWrap := ojson.SetRaw("{}", antigravitySchemaWrapperKey, schemaRaw)
	if errWrap != nil {
		return clean(schemaRaw)
	}
	if unwrapped := ojson.Get(clean(wrapped), antigravitySchemaWrapperKey); unwrapped.Exists() {
		return unwrapped.Raw
	}
	return clean(schemaRaw)
}

// antigravityFunctionDeclarationPaths returns the path of every function declaration in the request.
// Both the camelCase and snake_case spellings are accepted because callers reach this executor
// through different translators.
func antigravityFunctionDeclarationPaths(payloadStr string) []string {
	tools := ojson.Get(payloadStr, "request.tools")
	if !tools.IsArray() {
		return nil
	}
	paths := make([]string, 0, len(tools.Array()))
	for i, tool := range tools.Array() {
		for _, declKey := range []string{"functionDeclarations", "function_declarations"} {
			decls := tool.Get(declKey)
			if !decls.IsArray() {
				continue
			}
			for j := range decls.Array() {
				paths = append(paths, fmt.Sprintf("request.tools.%d.%s.%d", i, declKey, j))
			}
		}
	}
	return paths
}

// antigravitySchemaPaths returns every payload path that holds a JSON schema document.
// A function declaration may carry a schema for its parameters and for its result, so all of
// them must be cleaned; anything omitted here reaches the upstream API uncleaned.
func antigravitySchemaPaths(payloadStr string) []string {
	paths := antigravityDeclarationSchemaPaths(payloadStr)
	return append(paths, antigravityGenerationSchemaPaths(payloadStr)...)
}

func antigravityDeclarationSchemaPaths(payloadStr string) []string {
	paths := make([]string, 0, 8)
	for _, base := range antigravityFunctionDeclarationPaths(payloadStr) {
		for _, key := range antigravityDeclarationSchemaKeys {
			if ojson.Get(payloadStr, base+"."+key).IsObject() {
				paths = append(paths, base+"."+key)
			}
		}
	}
	return paths
}

func antigravityGenerationSchemaPaths(payloadStr string) []string {
	paths := make([]string, 0, len(antigravityGenerationConfigContainers)*len(antigravityGenerationSchemaKeys))
	for _, container := range antigravityGenerationConfigContainers {
		for _, key := range antigravityGenerationSchemaKeys {
			path := container + "." + key
			if ojson.Get(payloadStr, path).IsObject() {
				paths = append(paths, path)
			}
		}
	}
	return paths
}

// The upstream API is proto-JSON and accepts either spelling, and the Gemini translator forwards
// whichever one the client sent. Both are therefore cleaned where they sit rather than renamed:
// renaming would alter the body the client asked for, and only the unsupported keywords inside a
// schema cause upstream errors. The one exception is parametersJsonSchema, renamed onto parameters
// above because whole-payload cleaning did the same.
var (
	antigravityDeclarationSchemaKeys = []string{
		"parameters", "parametersJsonSchema", "parameters_json_schema",
		"response", "responseJsonSchema", "response_json_schema",
	}
	antigravityGenerationConfigContainers = []string{
		"request.generationConfig", "request.generation_config",
	}
	antigravityGenerationSchemaKeys = []string{
		"responseSchema", "responseJsonSchema", "response_schema", "response_json_schema",
	}
)

func antigravityRequestNeedsSchemaSanitization(payload []byte) bool {
	if ojson.GetBytesNoCopy(payload, "request.tools.0").Exists() {
		return true
	}
	for _, container := range antigravityGenerationConfigContainers {
		for _, key := range antigravityGenerationSchemaKeys {
			if ojson.GetBytesNoCopy(payload, container+"."+key).Exists() {
				return true
			}
		}
	}
	return false
}

func antigravityRequestNeedsSchemaSanitizationDoc(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	if doc.Get("request.tools.0").Exists() {
		return true
	}
	for _, container := range antigravityGenerationConfigContainers {
		for _, key := range antigravityGenerationSchemaKeys {
			if doc.Get(container + "." + key).Exists() {
				return true
			}
		}
	}
	return false
}

// resolveAntigravityRequestBaseURL selects one request endpoint without cross-tier fallback.
// Consumer credentials default to daily; enterprise/GCP credentials can set base_url explicitly.
func resolveAntigravityRequestBaseURL(auth *cliproxyauth.Auth) string {
	if base := resolveCustomAntigravityBaseURL(auth); base != "" {
		return base
	}
	return antigravityBaseURLDaily
}

func antigravityLoadCodeAssistBaseURL(auth *cliproxyauth.Auth) string {
	return resolveAntigravityRequestBaseURL(auth)
}

func resolveHost(base string) string {
	parsed, errParse := url.Parse(base)
	if errParse != nil {
		return ""
	}
	if parsed.Host != "" {
		return parsed.Host
	}
	return strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
}

func resolveUserAgent(auth *cliproxyauth.Auth) string {
	return misc.AntigravityRequestUserAgent(antigravityConfiguredUserAgent(auth))
}

func resolveLoadCodeAssistUserAgent(auth *cliproxyauth.Auth) string {
	return misc.AntigravityLoadCodeAssistUserAgent(antigravityConfiguredUserAgent(auth))
}

func antigravityConfiguredUserAgent(auth *cliproxyauth.Auth) string {
	raw := ""
	if auth != nil {
		if auth.Attributes != nil {
			if ua := strings.TrimSpace(auth.Attributes["user_agent"]); ua != "" {
				raw = ua
			}
		}
		if raw == "" && auth.Metadata != nil {
			if ua, ok := auth.Metadata["user_agent"].(string); ok && strings.TrimSpace(ua) != "" {
				raw = strings.TrimSpace(ua)
			}
		}
	}
	return raw
}

func resolveCustomAntigravityBaseURL(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["base_url"]); v != "" {
			return strings.TrimSuffix(v, "/")
		}
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["base_url"].(string); ok {
			v = strings.TrimSpace(v)
			if v != "" {
				return strings.TrimSuffix(v, "/")
			}
		}
	}
	return ""
}

func geminiToAntigravity(modelName string, payload []byte, projectID string, derivedSessionIDs ...string) []byte {
	doc := ojson.NewDoc(payload)
	geminiToAntigravityDoc(doc, modelName, projectID, derivedSessionIDs...)
	return doc.Bytes()
}

func geminiToAntigravityDoc(doc *ojson.Doc, modelName string, projectID string, derivedConversationIDs ...string) {
	if doc == nil {
		return
	}
	helps.SetStringIfDifferentDoc(doc, "model", modelName)
	helps.SetStringIfDifferentDoc(doc, "userAgent", "antigravity")

	lowerModel := strings.ToLower(strings.TrimSpace(modelName))
	isImageModel := strings.Contains(lowerModel, "image")
	reqType := strings.TrimSpace(doc.Get("requestType").String())
	if reqType == "" {
		if isImageModel {
			reqType = "image_gen"
		} else {
			reqType = "agent"
		}
		_ = doc.Set("requestType", reqType)
	}
	normalizedReqType := strings.ToLower(reqType)

	if projectID != "" {
		helps.SetStringIfDifferentDoc(doc, "project", projectID)
	} else {
		_ = doc.Delete("project")
	}

	if isImageModel {
		_ = doc.Set("requestId", generateImageGenRequestID())
	} else if normalizedReqType != "web_search" {
		// The official client derives requestId per trajectory as
		// agent/<cascade_id>/<start millis>/<trajectory_id>/<step>, and
		// checkpoint/<uuid> for conversation-title calls. request.sessionId is a
		// client constant that never carries conversation state; conversations
		// are identified by request.labels.cascade_id.
		identity := newAntigravityRequestIdentityDoc(doc, derivedConversationIDs)
		requestID := identity.requestID()
		if normalizedReqType == "checkpoint" {
			requestID = "checkpoint/" + uuid.NewString()
		}
		_ = doc.Set("requestId", requestID)
		if normalizedReqType == "agent" {
			setAntigravityRequestLabelsDoc(doc, modelName, identity)
		}
		_ = doc.Set("request.sessionId", misc.AntigravitySessionID)
	}

	_ = doc.Delete("request.safetySettings")
	if toolConfig := doc.Get("toolConfig"); toolConfig.Exists() && !doc.Get("request.toolConfig").Exists() {
		_ = doc.SetRawBytes("request.toolConfig", []byte(toolConfig.Raw))
		_ = doc.Delete("toolConfig")
	}
}

// antigravityRequestIdentity holds the identifiers the official client derives
// for one conversation (cascade) and one user turn inside it (trajectory).
type antigravityRequestIdentity struct {
	cascadeID     string
	trajectoryID  string
	requestIndex  int
	lastStepIndex int
	startMillis   int64
}

// requestID renders the agent-form requestId. The trailing step is always
// last_step_index + 1, matching every captured official request.
func (id antigravityRequestIdentity) requestID() string {
	return "agent/" + id.cascadeID + "/" + strconv.FormatInt(id.startMillis, 10) + "/" + id.trajectoryID + "/" + strconv.Itoa(id.lastStepIndex+1)
}

// newAntigravityRequestIdentityDoc rebuilds the conversation identity from data a
// stateless proxy can see: the downstream conversation seed when one exists,
// otherwise the first user message (the same seed the stable session id used
// before). Captured official traffic keeps cascade_id and trajectory_id fixed for
// the whole conversation, so both derive from that seed alone; the user turn is
// carried by labels.request_id and the step by labels.last_step_index.
func newAntigravityRequestIdentityDoc(doc *ojson.Doc, derivedConversationIDs []string) antigravityRequestIdentity {
	identity := antigravityRequestIdentity{startMillis: time.Now().UnixMilli()}
	if doc == nil {
		identity.cascadeID = uuid.NewString()
		identity.trajectoryID = uuid.NewString()
		return identity
	}

	conversationSeed := ""
	for _, candidate := range derivedConversationIDs {
		if trimmed := antigravityReplaySessionID(strings.TrimSpace(candidate)); trimmed != "" {
			conversationSeed = trimmed
			break
		}
	}
	if conversationSeed == "" {
		for _, path := range []string{"sessionId", "session_id", "request.sessionId", "request.session_id"} {
			if id := strings.TrimSpace(doc.Get(path).String()); id != "" {
				if resolved := antigravityReplaySessionID(id); resolved != "" {
					conversationSeed = resolved
					break
				}
			}
		}
	}
	if conversationSeed == "" {
		existingCascade := strings.TrimSpace(doc.Get("request.labels.cascade_id").String())
		if canonicalCascade, ok := normalizeAntigravityUUID(existingCascade); ok {
			identity.cascadeID = canonicalCascade
			existingTrajectory := strings.TrimSpace(doc.Get("request.labels.trajectory_id").String())
			if canonicalTrajectory, okTraj := normalizeAntigravityUUID(existingTrajectory); okTraj {
				identity.trajectoryID = canonicalTrajectory
			} else {
				identity.trajectoryID = antigravityStableUUID("trajectory", identity.cascadeID)
			}
			identity.requestIndex = antigravityCurrentUserTurnDoc(doc)
			identity.lastStepIndex = antigravityCompletedToolRoundsDoc(doc)
			return identity
		}
		if existingCascade != "" {
			conversationSeed = existingCascade
		}
	}
	if conversationSeed == "" {
		conversationSeed = generateAntigravityConversationSeedDoc(doc)
	}
	identity.cascadeID = antigravityStableUUID("cascade", conversationSeed)
	identity.trajectoryID = antigravityStableUUID("trajectory", conversationSeed)
	identity.requestIndex = antigravityCurrentUserTurnDoc(doc)
	identity.lastStepIndex = antigravityCompletedToolRoundsDoc(doc)
	return identity
}

func normalizeAntigravityUUID(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	parsed, err := uuid.Parse(value)
	if err != nil || parsed == uuid.Nil {
		return "", false
	}
	return parsed.String(), true
}

func isValidAntigravityUUID(value string) bool {
	_, ok := normalizeAntigravityUUID(value)
	return ok
}

func generateAntigravityConversationSeedDoc(doc *ojson.Doc) string {
	baseSeed := generateStableSessionIDDoc(doc)
	if doc == nil {
		return baseSeed
	}
	sys := doc.Get("request.systemInstruction")
	if !sys.Exists() {
		sys = doc.Get("systemInstruction")
	}
	rawSys := strings.TrimSpace(sys.Raw)
	if rawSys == "" || rawSys == "null" {
		return baseSeed
	}
	sysHash := sha256.Sum256([]byte(rawSys))
	return fmt.Sprintf("%s:sys:%x", baseSeed, sysHash[:8])
}

// antigravityStableUUID maps an identity seed to a random-looking but repeatable
// identifier shaped like the RFC 4122 v4 uuids the official client generates.
func antigravityStableUUID(kind string, seed string) string {
	if strings.TrimSpace(seed) == "" {
		return uuid.NewString()
	}
	sum := sha1.Sum([]byte("cli-proxy-api:antigravity:" + kind + "\x00" + seed))
	raw := sum[:16]
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	derived, errUUID := uuid.FromBytes(raw)
	if errUUID != nil {
		return uuid.NewString()
	}
	return derived.String()
}

func antigravityRequestContentsDoc(doc *ojson.Doc) ojson.Result {
	contents := doc.Get("request.contents")
	if !contents.IsArray() {
		contents = doc.Get("contents")
	}
	return contents
}

// antigravityCurrentUserTurnDoc returns the zero-based ordinal of the newest user
// message that carries user prompt content, which official traffic encodes as the
// suffix of labels.request_id. Tool results arrive as user contents carrying
// functionResponse parts (sometimes alongside injected reminder text), so any
// message containing a functionResponse does not advance the user turn ordinal.
func antigravityCurrentUserTurnDoc(doc *ojson.Doc) int {
	contents := antigravityRequestContentsDoc(doc)
	if !contents.IsArray() {
		return 0
	}
	turn := 0
	lastWasUserPayload := false
	for _, content := range contents.Array() {
		if content.Get("role").String() != "user" {
			lastWasUserPayload = false
			continue
		}
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		hasFunctionResponse := false
		hasUserPayload := false
		for _, part := range parts.Array() {
			if part.Get("functionResponse").Exists() {
				hasFunctionResponse = true
				break
			}
			if part.Get("text").String() != "" || part.Get("inlineData").Exists() || part.Get("fileData").Exists() {
				hasUserPayload = true
			}
		}
		if hasFunctionResponse {
			lastWasUserPayload = false
			continue
		}
		if hasUserPayload {
			if !lastWasUserPayload {
				turn++
			}
			lastWasUserPayload = true
		}
	}
	if turn > 0 {
		turn--
	}
	return turn
}

// antigravityCompletedToolRoundsDoc counts the completed tool-execution rounds
// already present in the conversation, which is how many official agent steps
// have completed. Parallel functionResponses returned together in one message
// represent a single completed round.
func antigravityCompletedToolRoundsDoc(doc *ojson.Doc) int {
	contents := antigravityRequestContentsDoc(doc)
	if !contents.IsArray() {
		return 0
	}
	rounds := 0
	for _, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		for _, part := range parts.Array() {
			if part.Get("functionResponse").Exists() {
				rounds++
				break
			}
		}
	}
	return rounds
}

// antigravityModelEnumFallbacks covers the window before an account catalog has
// been fetched. Both values are observed upstream: gemini-3.8-flash-tiered in
// request.labels across official 2.4.2, 2.12.0, 2.24.0 and 2.31.2, and
// gemini-3.8-flash-high in the models.<id>.model field of
// v1internal:fetchAvailableModels. Anything else relies on the fetched catalog.
var antigravityModelEnumFallbacks = map[string]string{
	"gemini-3.8-flash-tiered": "MODEL_PLACEHOLDER_M322",
	"gemini-3.8-flash-high":   "MODEL_PLACEHOLDER_M318",
}

func normalizeAntigravityModelLookupKey(modelName string) string {
	trimmed := strings.TrimSpace(modelName)
	trimmed = strings.TrimPrefix(trimmed, "models/")
	if idx := strings.LastIndex(trimmed, "/"); idx >= 0 && idx+1 < len(trimmed) {
		trimmed = strings.TrimSpace(trimmed[idx+1:])
	}
	return trimmed
}

func lookupAntigravityModelInfo(modelName string) *registry.ModelInfo {
	if modelInfo := registry.LookupModelInfo(modelName, "antigravity"); modelInfo != nil {
		return modelInfo
	}
	normalized := normalizeAntigravityModelLookupKey(modelName)
	if normalized != "" && normalized != modelName {
		return registry.LookupModelInfo(normalized, "antigravity")
	}
	return nil
}

func antigravityModelEnum(modelName string) string {
	if modelInfo := lookupAntigravityModelInfo(modelName); modelInfo != nil {
		if enum := strings.TrimSpace(modelInfo.AntigravityModelEnum); enum != "" {
			return enum
		}
	}
	key := strings.ToLower(normalizeAntigravityModelLookupKey(modelName))
	return antigravityModelEnumFallbacks[key]
}

// setAntigravityRequestLabelsDoc writes the label set captured from official
// agent requests: nine keys, every value a string, on the first step of a
// conversation and on each later step of the same trajectory. Captures cover the
// Gemini family, and the Claude family shares the same agent request shape, so
// both are labelled; any other family gets no labels at all rather than guesses.
// The used_* trio is read off the model of the request that carries it, because
// one upstream request is the only history a proxy can observe: an Antigravity
// conversation reports the model it is driving, never the models a previous step
// used. used_claude_conservative stays false, the value every capture holds,
// since no capture distinguishes it from used_claude. The optional tenth key
// (last_execution_id) only ever appeared in GUI traffic that also carries
// client-side execution records a proxy cannot rebuild.
// model_enum is dropped on its own when neither the account catalog nor the
// verified fallback table knows the model, because an invented enum is worse
// than a missing one.
func setAntigravityRequestLabelsDoc(doc *ojson.Doc, modelName string, identity antigravityRequestIdentity) {
	normalizedModel := strings.ToLower(strings.TrimSpace(modelName))
	usedClaude := strings.Contains(normalizedModel, "claude")
	if !usedClaude && !strings.Contains(normalizedModel, "gemini") {
		return
	}
	rootCascadeID := identity.cascadeID
	if existingRoot, ok := normalizeAntigravityUUID(doc.Get("request.labels.root_cascade_id").String()); ok {
		rootCascadeID = existingRoot
	}
	labels := map[string]string{
		"cascade_id":               identity.cascadeID,
		"last_step_index":          strconv.Itoa(identity.lastStepIndex),
		"request_id":               identity.trajectoryID + "-" + strconv.Itoa(identity.requestIndex),
		"root_cascade_id":          rootCascadeID,
		"trajectory_id":            identity.trajectoryID,
		"used_claude":              strconv.FormatBool(usedClaude),
		"used_claude_conservative": "false",
		"used_non_gemini_model":    strconv.FormatBool(usedClaude),
	}
	if modelEnum := antigravityModelEnum(modelName); modelEnum != "" {
		labels["model_enum"] = modelEnum
	}
	_ = doc.Set("request.labels", labels)
}

const (
	antigravityThinkingSmallMaxTokensThreshold = 8192
	antigravityThinkingMinSafeMaxTokens        = 16384
	antigravityDefaultContextEqualOutputCap    = 65536
	antigravityThinkingBudgetOutputHeadroom    = 4096
)

func antigravityOutputCeiling(modelInfo *registry.ModelInfo) int {
	if modelInfo == nil {
		return 0
	}
	if modelInfo.OutputTokenLimit > 0 {
		return modelInfo.OutputTokenLimit
	}
	maxCap := modelInfo.MaxCompletionTokens
	if maxCap <= 0 {
		return 0
	}
	if modelInfo.ContextLength > 0 && maxCap >= modelInfo.ContextLength && maxCap > antigravityDefaultContextEqualOutputCap {
		return antigravityDefaultContextEqualOutputCap
	}
	return maxCap
}

func applyAntigravityGeminiGenerationShapeDoc(doc *ojson.Doc, modelName string) {
	requestType := strings.ToLower(strings.TrimSpace(doc.Get("requestType").String()))
	hasThinkingLevel := doc.Get("request.generationConfig.thinkingConfig.thinkingLevel").Exists()
	budgetRes := doc.Get("request.generationConfig.thinkingConfig.thinkingBudget")
	thinkingBudget := 0
	hasActiveBudget := false
	if !hasThinkingLevel && budgetRes.Exists() && budgetRes.Type == ojson.Number {
		thinkingBudget = int(budgetRes.Int())
		hasActiveBudget = thinkingBudget != 0
	}
	if requestType != "" && requestType != "agent" {
		_ = doc.Delete("request.generationConfig.maxOutputTokens")
	} else {
		maxCap := antigravityOutputCeiling(lookupAntigravityModelInfo(modelName))
		maxOut := doc.Get("request.generationConfig.maxOutputTokens")
		if !maxOut.Exists() || (maxOut.Type == ojson.Number && maxOut.Int() <= 0) {
			if maxCap > 0 {
				_ = doc.Set("request.generationConfig.maxOutputTokens", maxCap)
			} else if maxOut.Exists() {
				_ = doc.Delete("request.generationConfig.maxOutputTokens")
			}
		} else if maxOut.Type == ojson.Number {
			current := int(maxOut.Int())
			if hasThinkingLevel && current > 0 && current < antigravityThinkingSmallMaxTokensThreshold {
				safeFloor := antigravityThinkingMinSafeMaxTokens
				if maxCap > 0 && maxCap < safeFloor {
					safeFloor = maxCap
				}
				if current < safeFloor {
					current = safeFloor
				}
			}
			if thinkingBudget > 0 && current > 0 && current <= thinkingBudget {
				needed := thinkingBudget + antigravityThinkingBudgetOutputHeadroom
				if maxCap > 0 && needed > maxCap {
					needed = maxCap
				}
				if needed > current {
					current = needed
				}
			}
			if maxCap > 0 && current > maxCap {
				current = maxCap
			}
			if current != int(maxOut.Int()) {
				_ = doc.Set("request.generationConfig.maxOutputTokens", current)
			}
		}
	}
	if hasThinkingLevel {
		_ = doc.Delete("request.generationConfig.thinkingConfig.thinkingBudget")
		if !doc.Get("request.generationConfig.thinkingConfig.includeThoughts").Exists() {
			_ = doc.Set("request.generationConfig.thinkingConfig.includeThoughts", true)
		}
	} else if hasActiveBudget {
		if !doc.Get("request.generationConfig.thinkingConfig.includeThoughts").Exists() {
			_ = doc.Set("request.generationConfig.thinkingConfig.includeThoughts", true)
		}
	}
}

// applyAntigravitySystemInstructionRoleDoc stamps the system instruction with
// role "user", which every official client sends. Inbound translations either
// already do this or copy a caller-provided object without a role.
func applyAntigravitySystemInstructionRoleDoc(doc *ojson.Doc) {
	if doc == nil || !doc.Get("request.systemInstruction").Exists() {
		return
	}
	helps.SetStringIfDifferentDoc(doc, "request.systemInstruction.role", "user")
}

func generateImageGenRequestID() string {
	return fmt.Sprintf("image_gen/%d/%s/12", time.Now().UnixMilli(), uuid.NewString())
}

func generateSessionID() string {
	randSourceMutex.Lock()
	n := randSource.Int63n(9_000_000_000_000_000_000)
	randSourceMutex.Unlock()
	return "-" + strconv.FormatInt(n, 10)
}

func generateStableSessionID(payload []byte) string {
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() {
		contents = util.GetGJSONBytesNoCopy(payload, "contents")
	}
	return generateStableSessionIDFromContents(contents)
}

func generateStableSessionIDDoc(doc *ojson.Doc) string {
	if doc == nil {
		return generateSessionID()
	}
	contents := doc.Get("request.contents")
	if !contents.IsArray() {
		contents = doc.Get("contents")
	}
	return generateStableSessionIDFromContents(contents)
}

func generateStableSessionIDFromContents(contents ojson.Result) string {
	if !contents.IsArray() {
		return generateSessionID()
	}

	stableID := ""
	contents.ForEach(func(_, content ojson.Result) bool {
		if content.Get("role").String() != "user" {
			return true
		}
		text := content.Get("parts.0.text").String()
		if text == "" {
			return true
		}
		hash := sha256.Sum256([]byte(text))
		value := int64(binary.BigEndian.Uint64(hash[:8])) & 0x7FFFFFFFFFFFFFFF
		stableID = "-" + strconv.FormatInt(value, 10)
		return false
	})
	if stableID != "" {
		return stableID
	}
	return generateSessionID()
}
