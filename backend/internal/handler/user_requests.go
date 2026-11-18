package handler

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// ListRequests serves customer-safe metadata. Existing success and error detail
// endpoints independently enforce ownership and error visibility.
func (h *UsageHandler) ListRequests(c *gin.Context) {
	filters, ok := h.parseUserUsageFilters(c, true)
	if !ok {
		return
	}
	if !filters.StartTime.Before(filters.EndTime) {
		response.BadRequest(c, "Invalid date range")
		return
	}
	page, size := response.ParsePagination(c)
	if size > 100 {
		size = 100
	}
	kind := c.DefaultQuery("kind", "all")
	if kind != "all" && kind != "error" && kind != "success" {
		response.BadRequest(c, "Invalid request kind")
		return
	}
	if h.opsService == nil {
		response.InternalError(c, "Request history unavailable")
		return
	}
	ctx := c.Request.Context()
	allowErrors := h.settingService != nil && h.settingService.IsUserErrorViewAllowed(ctx)
	filter := service.OpsRequestDetailFilter{
		Page: page, PageSize: size, Kind: kind,
		StartTime: &filters.StartTime, EndTime: &filters.EndTime,
		Model: filters.Filters.Model, RequestID: strings.TrimSpace(c.Query("request_id")),
	}
	if filters.Filters.APIKeyID > 0 {
		filter.APIKeyID = &filters.Filters.APIKeyID
	}
	rows, total, err := h.opsService.ListUserRequests(ctx, filters.Filters.UserID, allowErrors, filter)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Paginated(c, rows, total, page, size)
}
