package pricing

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
)

const (
	catalogKey             = "usage-pricing:catalog"
	catalogEncodingVersion = byte(1)
	maxCatalogModels       = 10000
	maxCatalogStringBytes  = 4096
	maxCatalogEncodedBytes = 8 << 20
	catalogRefreshInterval = 30 * time.Second
)

type modelIdentity struct {
	provider string
	model    string
}

type snapshot struct {
	revision int64
	models   []ModelConfig
	byKey    map[modelIdentity]ModelConfig
}

// Catalog atomically publishes validated pricing snapshots persisted in the active KV store.
type Catalog struct {
	current atomic.Pointer[snapshot]
	mu      sync.Mutex
}

// NewCatalog loads the current persisted pricing snapshot when KV is available.
func NewCatalog(ctx context.Context) (*Catalog, error) {
	catalog := &Catalog{}
	catalog.current.Store(newSnapshot(State{}))
	if errReload := catalog.Reload(ctx); errReload != nil && !errors.Is(errReload, ErrPersistenceDisabled) {
		return nil, errReload
	}
	return catalog, nil
}

// Run periodically refreshes snapshots written by another process sharing Home KV.
func (c *Catalog) Run(ctx context.Context) {
	if c == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(catalogRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = c.Reload(ctx)
		}
	}
}

// State returns a detached copy of the current snapshot.
func (c *Catalog) State() State {
	if c == nil {
		return State{Models: []ModelConfig{}}
	}
	current := c.current.Load()
	if current == nil {
		return State{Models: []ModelConfig{}}
	}
	return State{Revision: current.revision, Models: cloneModels(current.models)}
}

// Reload publishes the latest persisted snapshot.
func (c *Catalog) Reload(ctx context.Context) error {
	if c == nil {
		return errors.New("pricing catalog is unavailable")
	}
	client, enabled, errClient := kvstore.CurrentKVClient()
	if errClient != nil {
		return errClient
	}
	if !enabled || client == nil {
		return ErrPersistenceDisabled
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, found, errGet := client.KVGet(ctx, catalogKey)
	if errGet != nil {
		return fmt.Errorf("load pricing catalog: %w", errGet)
	}
	state := State{}
	if found {
		var errDecode error
		state, errDecode = decodeState(raw)
		if errDecode != nil {
			return fmt.Errorf("decode pricing catalog: %w", errDecode)
		}
	}
	c.current.Store(newSnapshot(state))
	return nil
}

// Upsert validates and atomically saves one model at the expected catalog revision.
func (c *Catalog) Upsert(ctx context.Context, expectedRevision int64, config ModelConfig) (State, error) {
	normalized, errValidate := normalizeModel(config)
	if errValidate != nil {
		return State{}, errValidate
	}
	return c.mutate(ctx, expectedRevision, func(models []ModelConfig) ([]ModelConfig, error) {
		key := modelKey(normalized.Provider, normalized.Model)
		replaced := false
		for index := range models {
			if modelKey(models[index].Provider, models[index].Model) == key {
				models[index] = normalized
				replaced = true
				break
			}
		}
		if !replaced {
			models = append(models, normalized)
		}
		return models, nil
	})
}

// Delete atomically removes one provider/model price at the expected revision.
func (c *Catalog) Delete(ctx context.Context, expectedRevision int64, provider, model string) (State, error) {
	provider = normalizeName(provider)
	model = normalizeName(model)
	if provider == "" || model == "" {
		return State{}, fmt.Errorf("%w: provider and model are required", ErrInvalidModel)
	}
	key := modelKey(provider, model)
	return c.mutate(ctx, expectedRevision, func(models []ModelConfig) ([]ModelConfig, error) {
		for index := range models {
			if modelKey(models[index].Provider, models[index].Model) == key {
				return append(models[:index], models[index+1:]...), nil
			}
		}
		return nil, ErrModelNotFound
	})
}

func (c *Catalog) mutate(ctx context.Context, expectedRevision int64, apply func([]ModelConfig) ([]ModelConfig, error)) (State, error) {
	if c == nil {
		return State{}, errors.New("pricing catalog is unavailable")
	}
	if expectedRevision < 0 {
		return State{}, fmt.Errorf("%w: pricing revision cannot be negative", ErrInvalidModel)
	}
	client, enabled, errClient := kvstore.CurrentKVClient()
	if errClient != nil {
		return State{}, errClient
	}
	if !enabled || client == nil {
		return State{}, ErrPersistenceDisabled
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	raw, found, errGet := client.KVGet(ctx, catalogKey)
	if errGet != nil {
		return State{}, fmt.Errorf("load pricing catalog: %w", errGet)
	}
	state := State{}
	if found {
		var errDecode error
		state, errDecode = decodeState(raw)
		if errDecode != nil {
			return State{}, fmt.Errorf("decode pricing catalog: %w", errDecode)
		}
	}
	if state.Revision != expectedRevision || state.Revision == math.MaxInt64 {
		return State{}, ErrRevisionConflict
	}
	models, errApply := apply(cloneModels(state.Models))
	if errApply != nil {
		return State{}, errApply
	}
	next := State{Revision: state.Revision + 1, Models: models}
	sortModels(next.Models)
	encoded, errEncode := encodeState(next)
	if errEncode != nil {
		return State{}, errEncode
	}
	swapped, errSwap := client.KVCompareAndSwap(ctx, catalogKey, raw, found, encoded, 0)
	if errSwap != nil {
		return State{}, fmt.Errorf("save pricing catalog: %w", errSwap)
	}
	if !swapped {
		return State{}, ErrRevisionConflict
	}
	c.current.Store(newSnapshot(next))
	return State{Revision: next.Revision, Models: cloneModels(next.Models)}, nil
}

// Quote resolves response model, requested model, then alias and calculates a fixed-point estimate.
func (c *Catalog) Quote(subject Subject) Quote {
	if c == nil || subject.UncachedInput < 0 || subject.CacheReadInput < 0 || subject.CacheWriteInput < 0 || subject.Output < 0 {
		return Quote{}
	}
	current := c.current.Load()
	if current == nil {
		return Quote{}
	}
	result := Quote{Revision: current.revision}
	provider := normalizeName(subject.Provider)
	for _, candidate := range []string{subject.ResponseModel, subject.Model, subject.Alias} {
		model := normalizeName(candidate)
		if model == "" {
			continue
		}
		config, found := current.byKey[modelKey(provider, model)]
		if !found {
			continue
		}
		value, ok := calculateCost(subject, config.BasePrices)
		if !ok {
			return result
		}
		result.Available = true
		result.USDMicros = value
		result.Model = config.Model
		return result
	}
	return result
}

func calculateCost(subject Subject, prices BasePrices) (int64, bool) {
	terms := []struct {
		tokens int64
		price  USDPerMillion
	}{
		{subject.UncachedInput, prices.Input},
		{subject.CacheReadInput, prices.CacheRead},
		{subject.CacheWriteInput, prices.CacheWrite},
		{subject.Output, prices.Output},
	}
	numerator := int64(0)
	for _, term := range terms {
		price := term.price.Micros()
		if term.tokens < 0 || price < 0 || (price != 0 && term.tokens > math.MaxInt64/price) {
			return 0, false
		}
		value := term.tokens * price
		if numerator > math.MaxInt64-value {
			return 0, false
		}
		numerator += value
	}
	if numerator > math.MaxInt64-usdPerMillionScale/2 {
		return 0, false
	}
	return (numerator + usdPerMillionScale/2) / usdPerMillionScale, true
}

func normalizeModel(config ModelConfig) (ModelConfig, error) {
	config.Provider = normalizeName(config.Provider)
	config.Model = normalizeName(config.Model)
	if config.Provider == "" || config.Model == "" {
		return ModelConfig{}, fmt.Errorf("%w: provider and model are required", ErrInvalidModel)
	}
	if !utf8.ValidString(config.Provider) || !utf8.ValidString(config.Model) {
		return ModelConfig{}, fmt.Errorf("%w: provider and model must be valid UTF-8", ErrInvalidModel)
	}
	if len(config.Provider) > maxCatalogStringBytes || len(config.Model) > maxCatalogStringBytes {
		return ModelConfig{}, fmt.Errorf("%w: provider or model is too long", ErrInvalidModel)
	}
	prices := []USDPerMillion{config.BasePrices.Input, config.BasePrices.Output, config.BasePrices.CacheRead, config.BasePrices.CacheWrite}
	for _, price := range prices {
		if price < 0 {
			return ModelConfig{}, fmt.Errorf("%w: prices cannot be negative", ErrInvalidModel)
		}
	}
	return config, nil
}

func normalizeName(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
func modelKey(provider, model string) modelIdentity {
	return modelIdentity{provider: normalizeName(provider), model: normalizeName(model)}
}

func newSnapshot(state State) *snapshot {
	models := cloneModels(state.Models)
	sortModels(models)
	byKey := make(map[modelIdentity]ModelConfig, len(models))
	for _, model := range models {
		byKey[modelKey(model.Provider, model.Model)] = model
	}
	return &snapshot{revision: state.Revision, models: models, byKey: byKey}
}

func cloneModels(models []ModelConfig) []ModelConfig {
	if len(models) == 0 {
		return []ModelConfig{}
	}
	return append([]ModelConfig(nil), models...)
}

func sortModels(models []ModelConfig) {
	sort.Slice(models, func(i, j int) bool {
		left, right := modelKey(models[i].Provider, models[i].Model), modelKey(models[j].Provider, models[j].Model)
		if left.provider != right.provider {
			return left.provider < right.provider
		}
		return left.model < right.model
	})
}

func encodeState(state State) ([]byte, error) {
	if state.Revision < 0 || len(state.Models) > maxCatalogModels {
		return nil, errors.New("invalid pricing catalog state")
	}
	encoded := make([]byte, 1, 16+len(state.Models)*64)
	encoded[0] = catalogEncodingVersion
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(state.Revision))
	encoded = binary.BigEndian.AppendUint32(encoded, uint32(len(state.Models)))
	seen := make(map[modelIdentity]struct{}, len(state.Models))
	for _, model := range state.Models {
		normalized, errNormalize := normalizeModel(model)
		if errNormalize != nil {
			return nil, errNormalize
		}
		key := modelKey(normalized.Provider, normalized.Model)
		if _, duplicate := seen[key]; duplicate {
			return nil, errors.New("duplicate pricing model")
		}
		seen[key] = struct{}{}
		encoded = appendCatalogString(encoded, normalized.Provider)
		encoded = appendCatalogString(encoded, normalized.Model)
		for _, price := range []USDPerMillion{normalized.BasePrices.Input, normalized.BasePrices.Output, normalized.BasePrices.CacheRead, normalized.BasePrices.CacheWrite} {
			encoded = binary.BigEndian.AppendUint64(encoded, uint64(price.Micros()))
		}
	}
	if len(encoded) > maxCatalogEncodedBytes {
		return nil, errors.New("pricing catalog exceeds size limit")
	}
	return encoded, nil
}

func decodeState(encoded []byte) (State, error) {
	if len(encoded) < 13 || len(encoded) > maxCatalogEncodedBytes || encoded[0] != catalogEncodingVersion {
		return State{}, errors.New("invalid pricing catalog encoding")
	}
	offset := 1
	revisionRaw := binary.BigEndian.Uint64(encoded[offset : offset+8])
	offset += 8
	if revisionRaw > math.MaxInt64 {
		return State{}, errors.New("invalid pricing revision")
	}
	count := int(binary.BigEndian.Uint32(encoded[offset : offset+4]))
	offset += 4
	if count > maxCatalogModels {
		return State{}, errors.New("pricing catalog has too many models")
	}
	state := State{Revision: int64(revisionRaw), Models: make([]ModelConfig, 0, count)}
	seen := make(map[modelIdentity]struct{}, count)
	for range count {
		provider, next, errProvider := readCatalogString(encoded, offset)
		if errProvider != nil {
			return State{}, errProvider
		}
		offset = next
		model, next, errModel := readCatalogString(encoded, offset)
		if errModel != nil {
			return State{}, errModel
		}
		offset = next
		if len(encoded)-offset < 32 {
			return State{}, errors.New("truncated pricing catalog prices")
		}
		values := [4]USDPerMillion{}
		for index := range values {
			raw := binary.BigEndian.Uint64(encoded[offset : offset+8])
			offset += 8
			if raw > math.MaxInt64 {
				return State{}, errors.New("invalid pricing value")
			}
			values[index] = USDPerMillion(raw)
		}
		config, errNormalize := normalizeModel(ModelConfig{Provider: provider, Model: model, BasePrices: BasePrices{Input: values[0], Output: values[1], CacheRead: values[2], CacheWrite: values[3]}})
		if errNormalize != nil {
			return State{}, errNormalize
		}
		key := modelKey(config.Provider, config.Model)
		if _, duplicate := seen[key]; duplicate {
			return State{}, errors.New("duplicate pricing model")
		}
		seen[key] = struct{}{}
		state.Models = append(state.Models, config)
	}
	if offset != len(encoded) {
		return State{}, errors.New("pricing catalog contains trailing data")
	}
	sortModels(state.Models)
	return state, nil
}

func appendCatalogString(target []byte, value string) []byte {
	target = binary.AppendUvarint(target, uint64(len(value)))
	return append(target, value...)
}

func readCatalogString(encoded []byte, offset int) (string, int, error) {
	if offset >= len(encoded) {
		return "", offset, errors.New("truncated pricing catalog string")
	}
	length, bytesRead := binary.Uvarint(encoded[offset:])
	if bytesRead <= 0 || length > maxCatalogStringBytes {
		return "", offset, errors.New("invalid pricing catalog string")
	}
	offset += bytesRead
	if length > uint64(len(encoded)-offset) {
		return "", offset, errors.New("truncated pricing catalog string")
	}
	return string(encoded[offset : offset+int(length)]), offset + int(length), nil
}
