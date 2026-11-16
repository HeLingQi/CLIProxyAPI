package pricing

import (
	"encoding/binary"
	"testing"
	"time"
)

func TestDynamicCatalogEncodingRoundTripAndQuote(t *testing.T) {
	state := State{Revision: 7, Models: []ModelConfig{{
		Provider: "codex", Model: "gpt-tiered", Expression: `len <= 100 ? tier("short", p * 1) : tier("long", p * 2)`, Source: "basellm",
	}}}
	encoded, errEncode := encodeState(state)
	if errEncode != nil {
		t.Fatalf("encodeState() error = %v", errEncode)
	}
	decoded, errDecode := decodeState(encoded)
	if errDecode != nil {
		t.Fatalf("decodeState() error = %v", errDecode)
	}
	if len(decoded.Models) != 1 || decoded.Models[0].Expression != state.Models[0].Expression || decoded.Models[0].Source != "basellm" {
		t.Fatalf("decoded = %+v", decoded)
	}

	catalog := &Catalog{}
	catalog.current.Store(newSnapshot(decoded))
	quote := catalog.Quote(Subject{Provider: "codex", Model: "gpt-tiered", UncachedInput: 1_000_000, InputLength: 101, At: time.Now()})
	if !quote.Available || quote.USDMicros != 2_000_000 || quote.Tier != "long" || quote.Source != "basellm" {
		t.Fatalf("Quote() = %+v", quote)
	}
}

func TestDecodeVersionOneCatalogRemainsCompatible(t *testing.T) {
	encoded := encodeVersionOneStateForTest(t, State{Revision: 2, Models: []ModelConfig{{
		Provider: "claude", Model: "claude-fixed", BasePrices: BasePrices{Input: 3, Output: 15},
	}}})
	decoded, errDecode := decodeState(encoded)
	if errDecode != nil {
		t.Fatalf("decodeState(v1) error = %v", errDecode)
	}
	if len(decoded.Models) != 1 || decoded.Models[0].Expression != "" || decoded.Models[0].BasePrices.Output != 15 {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func TestNormalizeDynamicModelRejectsInvalidExpression(t *testing.T) {
	if _, errNormalize := normalizeModel(ModelConfig{Provider: "codex", Model: "bad", Expression: `shell("x")`}); errNormalize == nil {
		t.Fatal("normalizeModel() accepted unsafe expression")
	}
}

func encodeVersionOneStateForTest(t *testing.T, state State) []byte {
	t.Helper()
	encoded := []byte{legacyCatalogEncodingVersion}
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(state.Revision))
	encoded = binary.BigEndian.AppendUint32(encoded, uint32(len(state.Models)))
	for _, model := range state.Models {
		encoded = appendCatalogString(encoded, model.Provider)
		encoded = appendCatalogString(encoded, model.Model)
		for _, price := range []USDPerMillion{model.BasePrices.Input, model.BasePrices.Output, model.BasePrices.CacheRead, model.BasePrices.CacheWrite} {
			encoded = binary.BigEndian.AppendUint64(encoded, uint64(price))
		}
	}
	return encoded
}

func TestMergeModelsAtomicallyUpdatesTargetsAndPreservesOthers(t *testing.T) {
	current := []ModelConfig{
		{Provider: "codex", Model: "matched", BasePrices: BasePrices{Input: 1}},
		{Provider: "manual", Model: "keep", BasePrices: BasePrices{Input: 9}},
	}
	merged, errMerge := mergeModels(current, []ModelConfig{{Provider: "codex", Model: "matched", Expression: `tier("standard", p * 2)`, Source: "basellm"}, {Provider: "gemini", Model: "new", Expression: `tier("standard", p * 3)`, Source: "basellm"}})
	if errMerge != nil {
		t.Fatalf("mergeModels() error = %v", errMerge)
	}
	if len(merged) != 3 {
		t.Fatalf("len(merged) = %d, want 3", len(merged))
	}
	byKey := map[modelIdentity]ModelConfig{}
	for _, model := range merged {
		byKey[modelKey(model.Provider, model.Model)] = model
	}
	if byKey[modelKey("codex", "matched")].Expression == "" || byKey[modelKey("manual", "keep")].BasePrices.Input != 9 || byKey[modelKey("gemini", "new")].Expression == "" {
		t.Fatalf("merged = %+v", merged)
	}
}
