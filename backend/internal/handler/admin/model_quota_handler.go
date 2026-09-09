package admin

import (
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// ModelQuotaHandler 处理分组级「按模型/模型前缀」配额用量的查看与重置。
type ModelQuotaHandler struct {
	modelQuotaService *service.AdminModelQuotaService
}

// NewModelQuotaHandler 创建 ModelQuotaHandler。
func NewModelQuotaHandler(modelQuotaService *service.AdminModelQuotaService) *ModelQuotaHandler {
	return &ModelQuotaHandler{modelQuotaService: modelQuotaService}
}

// ResetModelQuotaUsageRequest 是重置按模型配额用量的请求体。
type ResetModelQuotaUsageRequest struct {
	// RuleKey 为空表示重置该分组下所有规则的用量。
	RuleKey string `json:"rule_key"`
	Daily   bool   `json:"daily"`
	Weekly  bool   `json:"weekly"`
	Monthly bool   `json:"monthly"`
}

// GetUsage 返回某用户在某分组下各模型配额规则的用量进度。
// GET /api/v1/admin/groups/:id/model-quota-usage?user_id=123
func (h *ModelQuotaHandler) GetUsage(c *gin.Context) {
	groupID, ok := parseModelQuotaGroupID(c)
	if !ok {
		return
	}
	userID, err := strconv.ParseInt(strings.TrimSpace(c.Query("user_id")), 10, 64)
	if err != nil || userID <= 0 {
		response.BadRequest(c, "Invalid or missing user_id")
		return
	}

	progress, err := h.modelQuotaService.GetUsage(c.Request.Context(), userID, groupID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"items": progress})
}

// ResetUsage 强制归零某用户在某分组下的模型配额用量。
// POST /api/v1/admin/groups/:id/model-quota-usage/reset?user_id=123
func (h *ModelQuotaHandler) ResetUsage(c *gin.Context) {
	groupID, ok := parseModelQuotaGroupID(c)
	if !ok {
		return
	}
	userID, err := strconv.ParseInt(strings.TrimSpace(c.Query("user_id")), 10, 64)
	if err != nil || userID <= 0 {
		response.BadRequest(c, "Invalid or missing user_id")
		return
	}

	var req ResetModelQuotaUsageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if !req.Daily && !req.Weekly && !req.Monthly {
		response.BadRequest(c, "At least one of 'daily', 'weekly', or 'monthly' must be true")
		return
	}

	ruleKey := service.ModelQuotaRuleKey(req.RuleKey)
	if err := h.modelQuotaService.ResetUsage(c.Request.Context(), userID, groupID, ruleKey, req.Daily, req.Weekly, req.Monthly); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"reset": true})
}

func parseModelQuotaGroupID(c *gin.Context) (int64, bool) {
	groupID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || groupID <= 0 {
		response.BadRequest(c, "Invalid group ID")
		return 0, false
	}
	return groupID, true
}
