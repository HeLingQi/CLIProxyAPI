package pricing

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
)

func TestUSDPerMillionParsesExactJSONDecimals(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{input: "0", want: 0},
		{input: "0.075", want: 75_000},
		{input: "2.5", want: 2_500_000},
		{input: "1000000.000001", want: 1_000_000_000_001},
	}
	for _, test := range tests {
		var price USDPerMillion
		if err := price.UnmarshalJSON([]byte(test.input)); err != nil {
			t.Fatalf("UnmarshalJSON(%q) error = %v", test.input, err)
		}
		if price.Micros() != test.want {
			t.Fatalf("UnmarshalJSON(%q) = %d, want %d", test.input, price.Micros(), test.want)
		}
		encoded, errMarshal := price.MarshalJSON()
		if errMarshal != nil {
			t.Fatalf("MarshalJSON(%q) error = %v", test.input, errMarshal)
		}
		var roundTrip USDPerMillion
		if err := roundTrip.UnmarshalJSON(encoded); err != nil || roundTrip != price {
			t.Fatalf("round trip %q = %q (%d), error=%v", test.input, encoded, roundTrip, err)
		}
	}

	for _, invalid := range []string{"-1", "1.0000001", "1e3", "null", `"1.5"`, "9223372036855"} {
		var price USDPerMillion
		if err := price.UnmarshalJSON([]byte(invalid)); err == nil {
			t.Fatalf("UnmarshalJSON(%q) error = nil", invalid)
		}
	}
}

func TestCatalogPersistsRevisionAndCalculatesFixedPointCost(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pricing.db")
	if errConfigure := kvstore.ConfigureLocal(true, dbPath); errConfigure != nil {
		t.Fatalf("ConfigureLocal() error = %v", errConfigure)
	}
	t.Cleanup(func() { _ = kvstore.ShutdownLocal() })

	catalog, errCatalog := NewCatalog(context.Background())
	if errCatalog != nil {
		t.Fatalf("NewCatalog() error = %v", errCatalog)
	}
	config := ModelConfig{
		Provider: "OpenAI",
		Model:    "gpt-5.4",
		BasePrices: BasePrices{
			Input:      USDPerMillion(1_250_000),
			Output:     USDPerMillion(4_000_000),
			CacheRead:  USDPerMillion(100_000),
			CacheWrite: USDPerMillion(200_000),
		},
	}
	state, errUpsert := catalog.Upsert(context.Background(), 0, config)
	if errUpsert != nil {
		t.Fatalf("Upsert() error = %v", errUpsert)
	}
	if state.Revision != 1 || len(state.Models) != 1 || state.Models[0].Provider != "openai" {
		t.Fatalf("Upsert() state = %+v", state)
	}

	quote := catalog.Quote(Subject{
		Provider:        "OPENAI",
		Model:           "alias",
		ResponseModel:   "gpt-5.4",
		UncachedInput:   1_000_000,
		CacheReadInput:  2_000_000,
		CacheWriteInput: 500_000,
		Output:          250_000,
	})
	if !quote.Available || quote.USDMicros != 2_550_000 || quote.Revision != 1 || quote.Model != "gpt-5.4" {
		t.Fatalf("Quote() = %+v", quote)
	}
	if _, errConflict := catalog.Upsert(context.Background(), 0, config); !errors.Is(errConflict, ErrRevisionConflict) {
		t.Fatalf("stale Upsert() error = %v, want ErrRevisionConflict", errConflict)
	}

	if errShutdown := kvstore.ShutdownLocal(); errShutdown != nil {
		t.Fatalf("ShutdownLocal() error = %v", errShutdown)
	}
	if errReopen := kvstore.ConfigureLocal(true, dbPath); errReopen != nil {
		t.Fatalf("ConfigureLocal() reopen error = %v", errReopen)
	}
	reopened, errReopened := NewCatalog(context.Background())
	if errReopened != nil {
		t.Fatalf("NewCatalog() reopen error = %v", errReopened)
	}
	persisted := reopened.State()
	if persisted.Revision != 1 || len(persisted.Models) != 1 || persisted.Models[0] != state.Models[0] {
		t.Fatalf("persisted state = %+v, want %+v", persisted, state)
	}

	deleted, errDelete := reopened.Delete(context.Background(), 1, "openai", "gpt-5.4")
	if errDelete != nil || deleted.Revision != 2 || len(deleted.Models) != 0 {
		t.Fatalf("Delete() = %+v, error=%v", deleted, errDelete)
	}
}

func TestModelIdentityDoesNotCollideOnSlashes(t *testing.T) {
	left := modelKey("a/b", "c")
	right := modelKey("a", "b/c")
	if left == right {
		t.Fatalf("model identities collided: %+v", left)
	}
}

func TestCatalogRejectsWritesWhenKVIsDisabled(t *testing.T) {
	if errShutdown := kvstore.ShutdownLocal(); errShutdown != nil {
		t.Fatalf("ShutdownLocal() error = %v", errShutdown)
	}
	catalog, errCatalog := NewCatalog(context.Background())
	if errCatalog != nil {
		t.Fatalf("NewCatalog() error = %v", errCatalog)
	}
	_, errUpsert := catalog.Upsert(context.Background(), 0, ModelConfig{Provider: "openai", Model: "gpt"})
	if !errors.Is(errUpsert, ErrPersistenceDisabled) {
		t.Fatalf("Upsert() error = %v, want ErrPersistenceDisabled", errUpsert)
	}
}
