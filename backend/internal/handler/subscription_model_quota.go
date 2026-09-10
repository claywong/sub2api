package handler

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"

	"github.com/gin-gonic/gin"
)

// GetModelQuotaUsage 返回当前用户在某个订阅所属分组下各「按模型配额」规则的用量进度。
// GET /api/v1/subscriptions/:id/model-quota-usage
//
// 与管理端同名接口的区别：userID 只取自认证上下文，不接受任何请求参数，且必须先校验
// 订阅归属当前用户，否则任意用户都能凭订阅 ID 枚举他人用量。
func (h *SubscriptionHandler) GetModelQuotaUsage(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not found in context")
		return
	}

	subscriptionID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || subscriptionID <= 0 {
		response.BadRequest(c, "Invalid subscription ID")
		return
	}

	sub, err := h.subscriptionService.GetByID(c.Request.Context(), subscriptionID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	// 归属不符时返回 404 而非 403：403 会暴露该订阅 ID 确实存在。
	if sub == nil || sub.UserID != subject.UserID {
		response.NotFound(c, "Subscription not found")
		return
	}

	items, err := h.modelQuotaUsageService.GetUsage(c.Request.Context(), subject.UserID, sub.GroupID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"items": items})
}
