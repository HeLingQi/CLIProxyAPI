package management

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pricing"
	log "github.com/sirupsen/logrus"
)

type pricingModelWriteRequest struct {
	Revision int64 `json:"revision"`
	pricing.ModelConfig
}

// GetPricingModels returns the active immutable pricing catalog revision.
func (h *Handler) GetPricingModels(c *gin.Context) {
	catalog := h.currentPricingCatalog()
	if catalog == nil {
		writePricingError(c, http.StatusServiceUnavailable, "pricing_unavailable", "Pricing catalog is unavailable")
		return
	}
	if errReload := catalog.Reload(c.Request.Context()); errReload != nil && !errors.Is(errReload, pricing.ErrPersistenceDisabled) {
		log.WithError(errReload).Error("pricing catalog reload failed")
		writePricingError(c, http.StatusInternalServerError, "internal_error", "Internal server error")
		return
	}
	c.JSON(http.StatusOK, catalog.State())
}

// PutPricingModel validates and saves one provider/model base-price configuration.
func (h *Handler) PutPricingModel(c *gin.Context) {
	catalog := h.currentPricingCatalog()
	if catalog == nil {
		writePricingError(c, http.StatusServiceUnavailable, "pricing_unavailable", "Pricing catalog is unavailable")
		return
	}
	var request pricingModelWriteRequest
	if errBind := c.ShouldBindJSON(&request); errBind != nil {
		writePricingError(c, http.StatusBadRequest, "invalid_pricing", "Invalid pricing model")
		return
	}
	state, errUpsert := catalog.Upsert(c.Request.Context(), request.Revision, request.ModelConfig)
	if errUpsert != nil {
		writePricingCatalogError(c, errUpsert)
		return
	}
	c.JSON(http.StatusOK, state)
}

// DeletePricingModel removes one provider/model configuration at an expected revision.
func (h *Handler) DeletePricingModel(c *gin.Context) {
	catalog := h.currentPricingCatalog()
	if catalog == nil {
		writePricingError(c, http.StatusServiceUnavailable, "pricing_unavailable", "Pricing catalog is unavailable")
		return
	}
	revision, errRevision := strconv.ParseInt(strings.TrimSpace(c.Query("revision")), 10, 64)
	if errRevision != nil || revision < 0 {
		writePricingError(c, http.StatusBadRequest, "invalid_revision", "Pricing revision must be a non-negative integer")
		return
	}
	state, errDelete := catalog.Delete(c.Request.Context(), revision, c.Query("provider"), c.Query("model"))
	if errDelete != nil {
		writePricingCatalogError(c, errDelete)
		return
	}
	c.JSON(http.StatusOK, state)
}

func (h *Handler) currentPricingCatalog() *pricing.Catalog {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	catalog := h.pricingCatalog
	h.mu.Unlock()
	return catalog
}

func writePricingCatalogError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, pricing.ErrRevisionConflict):
		writePricingError(c, http.StatusConflict, "pricing_revision_conflict", "Pricing catalog changed; reload and retry")
	case errors.Is(err, pricing.ErrModelNotFound):
		writePricingError(c, http.StatusNotFound, "pricing_model_not_found", "Pricing model was not found")
	case errors.Is(err, pricing.ErrPersistenceDisabled):
		writePricingError(c, http.StatusServiceUnavailable, "pricing_persistence_disabled", "Pricing persistence is disabled")
	case errors.Is(err, pricing.ErrInvalidModel):
		writePricingError(c, http.StatusBadRequest, "invalid_pricing", "Invalid pricing model")
	default:
		log.WithError(err).Error("pricing catalog request failed")
		writePricingError(c, http.StatusInternalServerError, "internal_error", "Internal server error")
	}
}

func writePricingError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
