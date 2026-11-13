package usagehistory

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const (
	eventKeyPrefix            = "usage-history:event:"
	eventSequenceKey          = "usage-history:event-sequence"
	eventEncodingVersion      = byte(1)
	eventFixedFieldCount      = 15
	eventStringFieldCount     = 16
	maxEventStringBytes       = 1 << 20
	maxStoredEventStringBytes = 4096
	maxPendingEvents          = 10000
	defaultEventsLimit        = 50
	maxEventsLimit            = 200
	maxEventScanMultiplier    = 10
)

var (
	ErrInvalidEventsQuery  = errors.New("invalid usage history events query")
	ErrPersistenceDisabled = errors.New("usage history persistence is disabled")
)

// Event is a redacted request-level usage record. Raw API keys and failure bodies are never stored.
type Event struct {
	Sequence               int64     `json:"sequence"`
	Timestamp              time.Time `json:"timestamp"`
	RequestID              string    `json:"request_id,omitempty"`
	TraceID                string    `json:"trace_id,omitempty"`
	Provider               string    `json:"provider"`
	ExecutorType           string    `json:"executor_type,omitempty"`
	Model                  string    `json:"model"`
	Alias                  string    `json:"alias,omitempty"`
	AccessProvider         string    `json:"access_provider,omitempty"`
	APIKeyFingerprint      string    `json:"api_key_fingerprint,omitempty"`
	AuthID                 string    `json:"auth_id,omitempty"`
	AuthIndex              string    `json:"auth_index,omitempty"`
	AuthType               string    `json:"auth_type,omitempty"`
	Source                 string    `json:"source,omitempty"`
	FailureCode            string    `json:"failure_code,omitempty"`
	ServiceTier            string    `json:"service_tier,omitempty"`
	ResponseModel          string    `json:"response_model,omitempty"`
	Failed                 bool      `json:"failed"`
	FailureStatusCode      int64     `json:"failure_status_code,omitempty"`
	LatencyMs              int64     `json:"latency_ms"`
	TTFTMs                 int64     `json:"ttft_ms"`
	InputTokens            int64     `json:"input_tokens"`
	OutputTokens           int64     `json:"output_tokens"`
	ReasoningTokens        int64     `json:"reasoning_tokens"`
	CacheReadTokens        int64     `json:"cache_read_tokens"`
	CacheWriteTokens       int64     `json:"cache_write_tokens"`
	TotalTokens            int64     `json:"total_tokens"`
	ReportedCostUSDMicros  int64     `json:"reported_cost_usd_micros"`
	EstimatedCostUSDMicros int64     `json:"estimated_cost_usd_micros"`
	EffectiveCostUSDMicros int64     `json:"effective_cost_usd_micros"`
	PricingRevision        int64     `json:"pricing_revision,omitempty"`
	CostSource             string    `json:"cost_source,omitempty"`
}

// EventsQuery requests records before an exclusive sequence cursor.
type EventsQuery struct {
	Limit  int
	Before int64
}

// EventsPage contains newest-first redacted events and the next exclusive cursor.
type EventsPage struct {
	Items      []Event `json:"items"`
	NextBefore int64   `json:"next_before,omitempty"`
}

// Events returns persisted request records newest first.
func (h *History) Events(ctx context.Context, query EventsQuery) (EventsPage, error) {
	if h == nil {
		return EventsPage{}, errors.New("usage history unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	limit, errQuery := normalizeEventsQuery(query)
	if errQuery != nil {
		return EventsPage{}, errQuery
	}

	h.flushMu.Lock()
	defer h.flushMu.Unlock()
	if errFlush := h.flushLocked(ctx); errFlush != nil {
		return EventsPage{}, fmt.Errorf("flush usage history: %w", errFlush)
	}

	client, kvMode, errClient := kvstore.CurrentKVClient()
	if errClient != nil {
		return EventsPage{}, errClient
	}
	if !kvMode || client == nil {
		return EventsPage{}, ErrPersistenceDisabled
	}
	latest, errLatest := latestEventSequence(ctx, client)
	if errLatest != nil {
		return EventsPage{}, errLatest
	}
	cursor := latest
	if query.Before > 0 && query.Before-1 < cursor {
		cursor = query.Before - 1
	}
	if cursor <= 0 {
		return EventsPage{Items: []Event{}}, nil
	}

	page := EventsPage{Items: make([]Event, 0, limit)}
	scanBudget := limit * maxEventScanMultiplier
	scanned := 0
	for cursor > 0 && len(page.Items) < limit && scanned < scanBudget {
		batchSize := limit - len(page.Items)
		if remaining := scanBudget - scanned; batchSize > remaining {
			batchSize = remaining
		}
		if batchSize > maxEventsLimit {
			batchSize = maxEventsLimit
		}
		if int64(batchSize) > cursor {
			batchSize = int(cursor)
		}
		keys := make([]string, batchSize)
		sequences := make([]int64, batchSize)
		for index := 0; index < batchSize; index++ {
			sequence := cursor - int64(index)
			sequences[index] = sequence
			keys[index] = eventStorageKey(sequence)
		}
		values, found, errGet := client.KVMGet(ctx, keys...)
		if errGet != nil {
			return EventsPage{}, fmt.Errorf("read usage events: %w", errGet)
		}
		for index := range values {
			if !found[index] {
				continue
			}
			event, errDecode := decodeEvent(values[index])
			if errDecode != nil {
				return EventsPage{}, fmt.Errorf("decode usage event %d: %w", sequences[index], errDecode)
			}
			page.Items = append(page.Items, event)
		}
		cursor -= int64(batchSize)
		scanned += batchSize
	}
	if cursor > 0 {
		page.NextBefore = cursor + 1
	}
	return page, nil
}

func normalizeEventsQuery(query EventsQuery) (int, error) {
	if query.Before < 0 {
		return 0, fmt.Errorf("%w: before must not be negative", ErrInvalidEventsQuery)
	}
	limit := query.Limit
	if limit == 0 {
		limit = defaultEventsLimit
	}
	if limit < 1 || limit > maxEventsLimit {
		return 0, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidEventsQuery, maxEventsLimit)
	}
	return limit, nil
}

func eventFromRecord(record coreusage.Record, timestamp time.Time, cost recordCost) Event {
	detail := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	breakdown := detail.TokenBreakdown
	serviceTier := strings.TrimSpace(record.EffectiveServiceTier)
	if serviceTier == "" {
		serviceTier = strings.TrimSpace(record.ServiceTier)
	}
	if serviceTier == "" {
		serviceTier = strings.TrimSpace(record.RequestServiceTier)
	}
	return Event{
		Timestamp:              timestamp.UTC(),
		RequestID:              sanitizeEventString(record.RequestID),
		TraceID:                sanitizeEventString(record.TraceID),
		Provider:               normalizedEventValue(record.Provider),
		ExecutorType:           sanitizeEventString(record.ExecutorType),
		Model:                  normalizedEventValue(record.Model),
		Alias:                  sanitizeEventString(record.Alias),
		AccessProvider:         sanitizeEventString(record.AccessProvider),
		APIKeyFingerprint:      apiKeyFingerprint(record.APIKey),
		AuthID:                 sanitizeEventString(record.AuthID),
		AuthIndex:              sanitizeEventString(record.AuthIndex),
		AuthType:               sanitizeEventString(record.AuthType),
		Source:                 sanitizeEventString(record.Source),
		FailureCode:            sanitizeEventString(record.Fail.Code),
		ServiceTier:            sanitizeEventString(serviceTier),
		ResponseModel:          sanitizeEventString(record.ResponseModel),
		Failed:                 record.Failed,
		FailureStatusCode:      nonNegativeInt(record.Fail.StatusCode),
		LatencyMs:              nonNegativeDurationMillis(record.Latency),
		TTFTMs:                 nonNegativeDurationMillis(record.TTFT),
		InputTokens:            breakdown.Input.TotalTokens,
		OutputTokens:           breakdown.Output.TotalTokens,
		ReasoningTokens:        breakdown.Output.ReasoningTokens,
		CacheReadTokens:        breakdown.Input.CacheReadTokens,
		CacheWriteTokens:       breakdown.Input.CacheWriteTokens,
		TotalTokens:            breakdown.TotalTokens,
		ReportedCostUSDMicros:  cost.reported,
		EstimatedCostUSDMicros: cost.estimated,
		EffectiveCostUSDMicros: cost.effective,
		PricingRevision:        cost.pricingRevision,
		CostSource:             cost.source,
	}
}

func normalizedEventValue(value string) string {
	value = sanitizeEventString(value)
	if value == "" {
		return "unknown"
	}
	return value
}

func sanitizeEventString(value string) string {
	value = strings.ToValidUTF8(strings.TrimSpace(value), "�")
	if len(value) <= maxStoredEventStringBytes {
		return value
	}
	end := maxStoredEventStringBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func nonNegativeInt(value int) int64 {
	if value <= 0 {
		return 0
	}
	return int64(value)
}

func apiKeyFingerprint(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(apiKey))
	return "sha256:" + hex.EncodeToString(digest[:8])
}

func flushStoredEvents(ctx context.Context, client kvstore.KVClient, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	end, errSequence := client.KVIncrBy(ctx, eventSequenceKey, int64(len(events)))
	if errSequence != nil {
		return fmt.Errorf("reserve usage event sequence: %w", errSequence)
	}
	start := end - int64(len(events)) + 1
	if start <= 0 {
		return errors.New("usage event sequence overflow")
	}
	pairs := make(map[string][]byte, len(events))
	for index := range events {
		events[index].Sequence = start + int64(index)
		encoded, errEncode := encodeEvent(events[index])
		if errEncode != nil {
			return errEncode
		}
		pairs[eventStorageKey(events[index].Sequence)] = encoded
	}
	if errSet := client.KVMSet(ctx, pairs); errSet != nil {
		return fmt.Errorf("write usage events: %w", errSet)
	}
	return nil
}

func latestEventSequence(ctx context.Context, client kvstore.KVClient) (int64, error) {
	raw, found, errGet := client.KVGet(ctx, eventSequenceKey)
	if errGet != nil {
		return 0, errGet
	}
	if !found {
		return 0, nil
	}
	sequence, errParse := strconv.ParseInt(string(raw), 10, 64)
	if errParse != nil || sequence < 0 {
		return 0, errors.New("invalid usage event sequence")
	}
	return sequence, nil
}

func eventStorageKey(sequence int64) string {
	return eventKeyPrefix + fmt.Sprintf("%020d", sequence)
}

func encodeEvent(event Event) ([]byte, error) {
	encoded := make([]byte, 1, 256)
	encoded[0] = eventEncodingVersion
	fields := []int64{
		event.Sequence,
		event.Timestamp.UnixNano(),
		event.FailureStatusCode,
		event.LatencyMs,
		event.TTFTMs,
		event.InputTokens,
		event.OutputTokens,
		event.ReasoningTokens,
		event.CacheReadTokens,
		event.CacheWriteTokens,
		event.TotalTokens,
		event.ReportedCostUSDMicros,
		event.EstimatedCostUSDMicros,
		event.EffectiveCostUSDMicros,
		event.PricingRevision,
	}
	for _, field := range fields {
		encoded = binary.BigEndian.AppendUint64(encoded, uint64(field))
	}
	if event.Failed {
		encoded = append(encoded, 1)
	} else {
		encoded = append(encoded, 0)
	}
	stringsToEncode := []string{
		event.RequestID,
		event.TraceID,
		event.Provider,
		event.ExecutorType,
		event.Model,
		event.Alias,
		event.AccessProvider,
		event.APIKeyFingerprint,
		event.AuthID,
		event.AuthIndex,
		event.AuthType,
		event.Source,
		event.FailureCode,
		event.ServiceTier,
		event.ResponseModel,
		event.CostSource,
	}
	for _, value := range stringsToEncode {
		if len(value) > maxEventStringBytes {
			return nil, errors.New("usage event string exceeds size limit")
		}
		encoded = binary.AppendUvarint(encoded, uint64(len(value)))
		encoded = append(encoded, value...)
	}
	return encoded, nil
}

func decodeEvent(encoded []byte) (Event, error) {
	fixedSize := 1 + eventFixedFieldCount*8 + 1
	if len(encoded) < fixedSize || encoded[0] != eventEncodingVersion {
		return Event{}, errors.New("invalid usage event encoding")
	}
	offset := 1
	fields := make([]int64, eventFixedFieldCount)
	for index := range fields {
		fields[index] = int64(binary.BigEndian.Uint64(encoded[offset : offset+8]))
		offset += 8
		if fields[index] < 0 && index != 1 {
			return Event{}, errors.New("usage event contains a negative counter")
		}
	}
	failed := encoded[offset] == 1
	if encoded[offset] > 1 {
		return Event{}, errors.New("usage event contains an invalid failure flag")
	}
	offset++
	stringFields := make([]string, eventStringFieldCount)
	for index := range stringFields {
		length, bytesRead := binary.Uvarint(encoded[offset:])
		if bytesRead <= 0 || length > maxEventStringBytes {
			return Event{}, errors.New("invalid usage event string length")
		}
		offset += bytesRead
		if length > uint64(len(encoded)-offset) {
			return Event{}, errors.New("truncated usage event string")
		}
		stringFields[index] = string(encoded[offset : offset+int(length)])
		offset += int(length)
	}
	if offset != len(encoded) {
		return Event{}, errors.New("usage event contains trailing data")
	}
	return Event{
		Sequence:               fields[0],
		Timestamp:              time.Unix(0, fields[1]).UTC(),
		FailureStatusCode:      fields[2],
		LatencyMs:              fields[3],
		TTFTMs:                 fields[4],
		InputTokens:            fields[5],
		OutputTokens:           fields[6],
		ReasoningTokens:        fields[7],
		CacheReadTokens:        fields[8],
		CacheWriteTokens:       fields[9],
		TotalTokens:            fields[10],
		ReportedCostUSDMicros:  fields[11],
		EstimatedCostUSDMicros: fields[12],
		EffectiveCostUSDMicros: fields[13],
		PricingRevision:        fields[14],
		Failed:                 failed,
		RequestID:              stringFields[0],
		TraceID:                stringFields[1],
		Provider:               stringFields[2],
		ExecutorType:           stringFields[3],
		Model:                  stringFields[4],
		Alias:                  stringFields[5],
		AccessProvider:         stringFields[6],
		APIKeyFingerprint:      stringFields[7],
		AuthID:                 stringFields[8],
		AuthIndex:              stringFields[9],
		AuthType:               stringFields[10],
		Source:                 stringFields[11],
		FailureCode:            stringFields[12],
		ServiceTier:            stringFields[13],
		ResponseModel:          stringFields[14],
		CostSource:             stringFields[15],
	}, nil
}
