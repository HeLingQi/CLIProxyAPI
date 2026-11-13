package usagehistory

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pricing"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	dailyKeyPrefix       = "usage-history:day:"
	dailyEncodingVersion = byte(1)
	dailyFieldCount      = 14
	dailyEncodingSize    = 1 + dailyFieldCount*8
	maxSummaryDays       = 366
	flushInterval        = time.Second
	maxCASAttempts       = 16
)

var ErrInvalidDateRange = errors.New("invalid usage history date range")

// DateRange is an inclusive UTC calendar range in YYYY-MM-DD format.
type DateRange struct {
	From string
	To   string
}

// DailyAggregate stores additive usage counters for one UTC day.
type DailyAggregate struct {
	Requests               int64 `json:"requests"`
	Success                int64 `json:"success"`
	Failed                 int64 `json:"failed"`
	InputTokens            int64 `json:"input_tokens"`
	OutputTokens           int64 `json:"output_tokens"`
	ReasoningTokens        int64 `json:"reasoning_tokens"`
	CacheReadTokens        int64 `json:"cache_read_tokens"`
	CacheWriteTokens       int64 `json:"cache_write_tokens"`
	TotalTokens            int64 `json:"total_tokens"`
	ReportedCostUSDMicros  int64 `json:"reported_cost_usd_micros"`
	EstimatedCostUSDMicros int64 `json:"estimated_cost_usd_micros"`
	EffectiveCostUSDMicros int64 `json:"effective_cost_usd_micros"`
	LatencyMs              int64 `json:"latency_total_ms"`
	TTFTMs                 int64 `json:"ttft_total_ms"`
}

// Day contains one UTC date and its aggregate.
type Day struct {
	Date string `json:"date"`
	DailyAggregate
}

// Summary is the native persisted usage overview returned by the management API.
type Summary struct {
	From     string         `json:"from"`
	To       string         `json:"to"`
	Timezone string         `json:"timezone"`
	Totals   DailyAggregate `json:"totals"`
	Days     []Day          `json:"days"`
}

// History consumes normalized core usage records and batches daily aggregates into the active KV store.
type History struct {
	enabled        func() bool
	now            func() time.Time
	pricingCatalog *pricing.Catalog

	mu            sync.Mutex
	pending       map[string]DailyAggregate
	pendingEvents []Event
	flushMu       sync.Mutex

	startOnce     sync.Once
	closeOnce     sync.Once
	closed        atomic.Bool
	droppedEvents atomic.Uint64
	stop          chan struct{}
	done          chan struct{}
	closeErr      error
}

// New creates and starts a native usage history aggregator.
func New(enabled func() bool, catalogs ...*pricing.Catalog) *History {
	if enabled == nil {
		enabled = func() bool { return true }
	}
	history := &History{
		enabled: enabled,
		now:     time.Now,
		pending: make(map[string]DailyAggregate),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if len(catalogs) > 0 {
		history.pricingCatalog = catalogs[0]
	}
	history.start()
	return history
}

func (h *History) start() {
	if h == nil {
		return
	}
	h.startOnce.Do(func() {
		go h.run()
	})
}

func (h *History) run() {
	defer close(h.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-ticker.C:
			if errFlush := h.Flush(context.Background()); errFlush != nil {
				log.WithError(errFlush).Warn("usage history flush failed")
			}
		}
	}
}

// HandleUsage implements the core usage plugin contract without blocking on disk I/O.
func (h *History) HandleUsage(_ context.Context, record coreusage.Record) {
	if h == nil || h.closed.Load() || !h.enabled() {
		return
	}
	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = h.now()
	}
	day := timestamp.UTC().Format(time.DateOnly)
	cost := resolveRecordCost(record, h.pricingCatalog)
	delta := aggregateRecord(record, cost)
	event := eventFromRecord(record, timestamp, cost)

	h.mu.Lock()
	current := h.pending[day]
	if errMerge := mergeAggregate(&current, delta); errMerge == nil {
		h.pending[day] = current
	} else {
		log.WithError(errMerge).Warn("usage history aggregate overflow")
	}
	if len(h.pendingEvents) < maxPendingEvents {
		h.pendingEvents = append(h.pendingEvents, event)
	} else if dropped := h.droppedEvents.Add(1); dropped == 1 || dropped%1000 == 0 {
		log.WithField("dropped", dropped).Warn("usage history event buffer is full")
	}
	h.mu.Unlock()
}

// Flush merges pending aggregates into the active KV store.
func (h *History) Flush(ctx context.Context) error {
	if h == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.flushMu.Lock()
	defer h.flushMu.Unlock()
	return h.flushLocked(ctx)
}

func (h *History) flushLocked(ctx context.Context) error {
	h.mu.Lock()
	pending := h.pending
	pendingEvents := h.pendingEvents
	h.pending = make(map[string]DailyAggregate)
	h.pendingEvents = nil
	h.mu.Unlock()
	if len(pending) == 0 && len(pendingEvents) == 0 {
		return nil
	}

	client, kvMode, errClient := kvstore.CurrentKVClient()
	if errClient != nil {
		h.restorePending(pending)
		h.restoreEvents(pendingEvents)
		return errClient
	}
	if !kvMode || client == nil {
		h.restorePending(pending)
		h.restoreEvents(pendingEvents)
		return nil
	}

	for day, delta := range pending {
		if errMerge := mergeStoredDay(ctx, client, day, delta); errMerge != nil {
			h.restorePending(pending)
			h.restoreEvents(pendingEvents)
			return errMerge
		}
		delete(pending, day)
	}
	if errEvents := flushStoredEvents(ctx, client, pendingEvents); errEvents != nil {
		h.restoreEvents(pendingEvents)
		return errEvents
	}
	return nil
}

func (h *History) restorePending(items map[string]DailyAggregate) {
	if len(items) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for day, delta := range items {
		current := h.pending[day]
		if errMerge := mergeAggregate(&current, delta); errMerge != nil {
			log.WithError(errMerge).Warn("usage history pending restore overflow")
			continue
		}
		h.pending[day] = current
	}
}

func (h *History) restoreEvents(items []Event) {
	if len(items) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	available := maxPendingEvents - len(h.pendingEvents)
	if available <= 0 {
		h.droppedEvents.Add(uint64(len(items)))
		return
	}
	if len(items) > available {
		h.droppedEvents.Add(uint64(len(items) - available))
		items = items[:available]
	}
	restored := make([]Event, 0, len(items)+len(h.pendingEvents))
	restored = append(restored, items...)
	restored = append(restored, h.pendingEvents...)
	h.pendingEvents = restored
}

// Summary returns persisted and not-yet-persisted aggregates for an inclusive UTC date range.
func (h *History) Summary(ctx context.Context, dateRange DateRange) (Summary, error) {
	if h == nil {
		return Summary{}, errors.New("usage history unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	from, to, errRange := parseDateRange(dateRange)
	if errRange != nil {
		return Summary{}, errRange
	}

	h.flushMu.Lock()
	defer h.flushMu.Unlock()
	if errFlush := h.flushLocked(ctx); errFlush != nil {
		return Summary{}, fmt.Errorf("flush usage history: %w", errFlush)
	}

	dates := datesInclusive(from, to)
	keys := make([]string, len(dates))
	for index := range dates {
		keys[index] = dailyKeyPrefix + dates[index]
	}

	stored := make([]DailyAggregate, len(dates))
	client, kvMode, errClient := kvstore.CurrentKVClient()
	if errClient != nil {
		return Summary{}, errClient
	}
	if kvMode && client != nil {
		values, found, errGet := client.KVMGet(ctx, keys...)
		if errGet != nil {
			return Summary{}, fmt.Errorf("read usage history: %w", errGet)
		}
		for index := range dates {
			if !found[index] {
				continue
			}
			aggregate, errDecode := decodeDailyAggregate(values[index])
			if errDecode != nil {
				return Summary{}, fmt.Errorf("decode usage history for %s: %w", dates[index], errDecode)
			}
			stored[index] = aggregate
		}
	}

	h.mu.Lock()
	pending := make(map[string]DailyAggregate, len(h.pending))
	for day, aggregate := range h.pending {
		pending[day] = aggregate
	}
	h.mu.Unlock()

	summary := Summary{
		From:     from.Format(time.DateOnly),
		To:       to.Format(time.DateOnly),
		Timezone: "UTC",
		Days:     make([]Day, len(dates)),
	}
	for index, day := range dates {
		aggregate := stored[index]
		if delta, ok := pending[day]; ok {
			if errMerge := mergeAggregate(&aggregate, delta); errMerge != nil {
				return Summary{}, errMerge
			}
		}
		summary.Days[index] = Day{Date: day, DailyAggregate: aggregate}
		if errMerge := mergeAggregate(&summary.Totals, aggregate); errMerge != nil {
			return Summary{}, errMerge
		}
	}
	return summary, nil
}

// Close stops background flushing and persists the final pending batch.
func (h *History) Close(ctx context.Context) error {
	if h == nil {
		return nil
	}
	h.closeOnce.Do(func() {
		h.closed.Store(true)
		close(h.stop)
		if ctx == nil {
			ctx = context.Background()
		}
		select {
		case <-h.done:
			h.closeErr = h.Flush(ctx)
		case <-ctx.Done():
			h.closeErr = ctx.Err()
		}
	})
	return h.closeErr
}

func aggregateRecord(record coreusage.Record, cost recordCost) DailyAggregate {
	detail := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	breakdown := detail.TokenBreakdown
	aggregate := DailyAggregate{
		Requests:               1,
		InputTokens:            breakdown.Input.TotalTokens,
		OutputTokens:           breakdown.Output.TotalTokens,
		ReasoningTokens:        breakdown.Output.ReasoningTokens,
		CacheReadTokens:        breakdown.Input.CacheReadTokens,
		CacheWriteTokens:       breakdown.Input.CacheWriteTokens,
		TotalTokens:            breakdown.TotalTokens,
		ReportedCostUSDMicros:  cost.reported,
		EstimatedCostUSDMicros: cost.estimated,
		EffectiveCostUSDMicros: cost.effective,
		LatencyMs:              nonNegativeDurationMillis(record.Latency),
		TTFTMs:                 nonNegativeDurationMillis(record.TTFT),
	}
	if record.Failed {
		aggregate.Failed = 1
	} else {
		aggregate.Success = 1
	}
	return aggregate
}

func nonNegativeDurationMillis(value time.Duration) int64 {
	if value <= 0 {
		return 0
	}
	return value.Milliseconds()
}

type recordCost struct {
	reported        int64
	estimated       int64
	effective       int64
	source          string
	pricingRevision int64
}

func resolveRecordCost(record coreusage.Record, catalog *pricing.Catalog) recordCost {
	detail := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	reported, hasReported := reportedCost(detail.CostDetails)
	quote := pricing.Quote{}
	if catalog != nil {
		breakdown := detail.TokenBreakdown
		quote = catalog.Quote(pricing.Subject{
			Provider:        record.Provider,
			Model:           record.Model,
			Alias:           record.Alias,
			ResponseModel:   record.ResponseModel,
			UncachedInput:   breakdown.Input.UncachedTokens,
			CacheReadInput:  breakdown.Input.CacheReadTokens,
			CacheWriteInput: breakdown.Input.CacheWriteTokens,
			Output:          breakdown.Output.TotalTokens,
		})
	}
	cost := recordCost{reported: reported, pricingRevision: quote.Revision}
	if quote.Available {
		cost.estimated = quote.USDMicros
	}
	switch {
	case hasReported:
		cost.effective = reported
		cost.source = "upstream"
	case quote.Available:
		cost.effective = quote.USDMicros
		cost.source = "catalog"
	}
	return cost
}

func reportedCost(cost *coreusage.CostDetails) (int64, bool) {
	if cost == nil {
		return 0, false
	}
	if cost.CostInUSDTicks != nil && *cost.CostInUSDTicks >= 0 {
		ticks := *cost.CostInUSDTicks
		micros := ticks / 10_000
		if ticks%10_000 >= 5_000 {
			micros++
		}
		return micros, true
	}
	value := cost.Cost
	if value == nil {
		value = cost.UpstreamInferenceCost
	}
	if value == nil || *value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0) {
		return 0, false
	}
	scaled := *value * 1_000_000
	if scaled >= float64(1<<63-1) {
		return 0, false
	}
	return int64(math.Round(scaled)), true
}

func reportedCostUSDMicros(cost *coreusage.CostDetails) int64 {
	value, _ := reportedCost(cost)
	return value
}

func mergeStoredDay(ctx context.Context, client kvstore.KVClient, day string, delta DailyAggregate) error {
	key := dailyKeyPrefix + day
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		if errContext := ctx.Err(); errContext != nil {
			return errContext
		}
		raw, found, errGet := client.KVGet(ctx, key)
		if errGet != nil {
			return errGet
		}
		current := DailyAggregate{}
		if found {
			var errDecode error
			current, errDecode = decodeDailyAggregate(raw)
			if errDecode != nil {
				return errDecode
			}
		}
		if errMerge := mergeAggregate(&current, delta); errMerge != nil {
			return errMerge
		}
		next := encodeDailyAggregate(current)
		swapped, errSwap := client.KVCompareAndSwap(ctx, key, raw, found, next, 0)
		if errSwap != nil {
			return errSwap
		}
		if swapped {
			return nil
		}
	}
	return errors.New("usage history update contention exceeded retry limit")
}

func mergeAggregate(target *DailyAggregate, delta DailyAggregate) error {
	if target == nil {
		return errors.New("usage history aggregate target is nil")
	}
	pairs := []struct {
		target *int64
		delta  int64
	}{
		{&target.Requests, delta.Requests},
		{&target.Success, delta.Success},
		{&target.Failed, delta.Failed},
		{&target.InputTokens, delta.InputTokens},
		{&target.OutputTokens, delta.OutputTokens},
		{&target.ReasoningTokens, delta.ReasoningTokens},
		{&target.CacheReadTokens, delta.CacheReadTokens},
		{&target.CacheWriteTokens, delta.CacheWriteTokens},
		{&target.TotalTokens, delta.TotalTokens},
		{&target.ReportedCostUSDMicros, delta.ReportedCostUSDMicros},
		{&target.EstimatedCostUSDMicros, delta.EstimatedCostUSDMicros},
		{&target.EffectiveCostUSDMicros, delta.EffectiveCostUSDMicros},
		{&target.LatencyMs, delta.LatencyMs},
		{&target.TTFTMs, delta.TTFTMs},
	}
	for _, pair := range pairs {
		if pair.delta < 0 || *pair.target > math.MaxInt64-pair.delta {
			return errors.New("usage history aggregate overflow")
		}
		*pair.target += pair.delta
	}
	return nil
}

func encodeDailyAggregate(value DailyAggregate) []byte {
	encoded := make([]byte, dailyEncodingSize)
	encoded[0] = dailyEncodingVersion
	fields := []int64{
		value.Requests,
		value.Success,
		value.Failed,
		value.InputTokens,
		value.OutputTokens,
		value.ReasoningTokens,
		value.CacheReadTokens,
		value.CacheWriteTokens,
		value.TotalTokens,
		value.ReportedCostUSDMicros,
		value.EstimatedCostUSDMicros,
		value.EffectiveCostUSDMicros,
		value.LatencyMs,
		value.TTFTMs,
	}
	for index, field := range fields {
		binary.BigEndian.PutUint64(encoded[1+index*8:], uint64(field))
	}
	return encoded
}

func decodeDailyAggregate(encoded []byte) (DailyAggregate, error) {
	if len(encoded) != dailyEncodingSize {
		return DailyAggregate{}, fmt.Errorf("invalid encoded size %d", len(encoded))
	}
	if encoded[0] != dailyEncodingVersion {
		return DailyAggregate{}, fmt.Errorf("unsupported encoding version %d", encoded[0])
	}
	fields := make([]int64, dailyFieldCount)
	for index := range fields {
		value := binary.BigEndian.Uint64(encoded[1+index*8:])
		if value > math.MaxInt64 {
			return DailyAggregate{}, errors.New("encoded aggregate exceeds int64")
		}
		fields[index] = int64(value)
	}
	return DailyAggregate{
		Requests:               fields[0],
		Success:                fields[1],
		Failed:                 fields[2],
		InputTokens:            fields[3],
		OutputTokens:           fields[4],
		ReasoningTokens:        fields[5],
		CacheReadTokens:        fields[6],
		CacheWriteTokens:       fields[7],
		TotalTokens:            fields[8],
		ReportedCostUSDMicros:  fields[9],
		EstimatedCostUSDMicros: fields[10],
		EffectiveCostUSDMicros: fields[11],
		LatencyMs:              fields[12],
		TTFTMs:                 fields[13],
	}, nil
}

func parseDateRange(value DateRange) (time.Time, time.Time, error) {
	fromText := strings.TrimSpace(value.From)
	toText := strings.TrimSpace(value.To)
	if fromText == "" || toText == "" {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: from and to are required", ErrInvalidDateRange)
	}
	from, errFrom := time.Parse(time.DateOnly, fromText)
	if errFrom != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: invalid from date", ErrInvalidDateRange)
	}
	to, errTo := time.Parse(time.DateOnly, toText)
	if errTo != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: invalid to date", ErrInvalidDateRange)
	}
	if to.Before(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: to precedes from", ErrInvalidDateRange)
	}
	if days := int(to.Sub(from)/(24*time.Hour)) + 1; days > maxSummaryDays {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: range exceeds %d days", ErrInvalidDateRange, maxSummaryDays)
	}
	return from, to, nil
}

func datesInclusive(from, to time.Time) []string {
	count := int(to.Sub(from)/(24*time.Hour)) + 1
	dates := make([]string, 0, count)
	for current := from; !current.After(to); current = current.AddDate(0, 0, 1) {
		dates = append(dates, current.Format(time.DateOnly))
	}
	return dates
}
