package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pricing"
)

func TestPricingModelsCRUDUsesCatalogRevision(t *testing.T) {
	if errConfigure := kvstore.ConfigureLocal(true, filepath.Join(t.TempDir(), "pricing.db")); errConfigure != nil {
		t.Fatalf("ConfigureLocal() error = %v", errConfigure)
	}
	t.Cleanup(func() { _ = kvstore.ShutdownLocal() })
	catalog, errCatalog := pricing.NewCatalog(context.Background())
	if errCatalog != nil {
		t.Fatalf("NewCatalog() error = %v", errCatalog)
	}
	handler := &Handler{}
	handler.SetPricingCatalog(catalog)

	put := invokePricingHandler(t, http.MethodPut, "/v8/management/observability/pricing/models", `{
		"revision":0,
		"provider":"OpenAI",
		"model":"gpt-5.4",
		"base_prices":{"input":1.25,"output":4,"cache_read":0.1,"cache_write":0.2}
	}`, handler.PutPricingModel)
	if put.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", put.Code, put.Body.String())
	}
	if got := ojson.GetBytes(put.Body.Bytes(), "revision").Int(); got != 1 {
		t.Fatalf("PUT revision = %d, want 1", got)
	}
	if got := ojson.GetBytes(put.Body.Bytes(), "models.0.base_prices.input").Float(); got != 1.25 {
		t.Fatalf("PUT input price = %v, want 1.25", got)
	}

	get := invokePricingHandler(t, http.MethodGet, "/v8/management/observability/pricing/models", "", handler.GetPricingModels)
	if get.Code != http.StatusOK || ojson.GetBytes(get.Body.Bytes(), "models.0.provider").String() != "openai" {
		t.Fatalf("GET status=%d body=%s", get.Code, get.Body.String())
	}

	stale := invokePricingHandler(t, http.MethodPut, "/v8/management/observability/pricing/models", `{
		"revision":0,
		"provider":"openai",
		"model":"gpt-5.4",
		"base_prices":{"input":1,"output":1,"cache_read":0,"cache_write":0}
	}`, handler.PutPricingModel)
	if stale.Code != http.StatusConflict || ojson.GetBytes(stale.Body.Bytes(), "error.code").String() != "pricing_revision_conflict" {
		t.Fatalf("stale PUT status=%d body=%s", stale.Code, stale.Body.String())
	}

	deleted := invokePricingHandler(t, http.MethodDelete, "/v8/management/observability/pricing/models?revision=1&provider=openai&model=gpt-5.4", "", handler.DeletePricingModel)
	if deleted.Code != http.StatusOK || ojson.GetBytes(deleted.Body.Bytes(), "revision").Int() != 2 || ojson.GetBytes(deleted.Body.Bytes(), "models.#").Int() != 0 {
		t.Fatalf("DELETE status=%d body=%s", deleted.Code, deleted.Body.String())
	}
}

func TestPutPricingModelRejectsInvalidExactPrice(t *testing.T) {
	if errConfigure := kvstore.ConfigureLocal(true, filepath.Join(t.TempDir(), "pricing.db")); errConfigure != nil {
		t.Fatalf("ConfigureLocal() error = %v", errConfigure)
	}
	t.Cleanup(func() { _ = kvstore.ShutdownLocal() })
	catalog, errCatalog := pricing.NewCatalog(context.Background())
	if errCatalog != nil {
		t.Fatalf("NewCatalog() error = %v", errCatalog)
	}
	handler := &Handler{}
	handler.SetPricingCatalog(catalog)

	response := invokePricingHandler(t, http.MethodPut, "/v8/management/observability/pricing/models", `{
		"revision":0,
		"provider":"openai",
		"model":"gpt",
		"base_prices":{"input":0.0000001,"output":1,"cache_read":0,"cache_write":0}
	}`, handler.PutPricingModel)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if state := catalog.State(); state.Revision != 0 || len(state.Models) != 0 {
		t.Fatalf("catalog changed after invalid request: %+v", state)
	}
}

func invokePricingHandler(t *testing.T, method, target, body string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		ginContext.Request.Header.Set("Content-Type", "application/json")
	}
	handler(ginContext)
	return recorder
}
