package management

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagehistory"
)

// GetUsageHistorySummary returns persisted native usage aggregates for an inclusive UTC date range.
func (h *Handler) GetUsageHistorySummary(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	h.mu.Lock()
	history := h.usageHistory
	h.mu.Unlock()
	if history == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage history unavailable"})
		return
	}

	summary, errSummary := history.Summary(c.Request.Context(), usagehistory.DateRange{
		From: c.Query("from"),
		To:   c.Query("to"),
	})
	if errSummary != nil {
		if errors.Is(errSummary, usagehistory.ErrInvalidDateRange) {
			c.JSON(http.StatusBadRequest, gin.H{"error": errSummary.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query usage history"})
		return
	}
	c.JSON(http.StatusOK, summary)
}

// GetUsageHistoryEvents returns newest-first redacted request-level usage records.
func (h *Handler) GetUsageHistoryEvents(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	h.mu.Lock()
	history := h.usageHistory
	h.mu.Unlock()
	if history == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage history unavailable"})
		return
	}

	query := usagehistory.EventsQuery{}
	if value := strings.TrimSpace(c.Query("limit")); value != "" {
		limit, errLimit := strconv.Atoi(value)
		if errLimit != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be an integer"})
			return
		}
		query.Limit = limit
	}
	if value := strings.TrimSpace(c.Query("before")); value != "" {
		before, errBefore := strconv.ParseInt(value, 10, 64)
		if errBefore != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "before must be an integer"})
			return
		}
		query.Before = before
	}

	page, errEvents := history.Events(c.Request.Context(), query)
	if errEvents != nil {
		switch {
		case errors.Is(errEvents, usagehistory.ErrInvalidEventsQuery):
			c.JSON(http.StatusBadRequest, gin.H{"error": errEvents.Error()})
		case errors.Is(errEvents, usagehistory.ErrPersistenceDisabled):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": errEvents.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query usage events"})
		}
		return
	}
	c.JSON(http.StatusOK, page)
}
