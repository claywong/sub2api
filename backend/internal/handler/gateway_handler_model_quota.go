// 私有扩展（不属于 upstream sub2api）。
//
// 文件作用：为 GatewayHandler 提供受保护模型独立额度检查辅助方法。
//
// 包含方法：
//   - GatewayHandler.applyModelQuotaOrFail  — 请求前额度检查
//   - GatewayHandler.makeModelQuotaHook     — 构造计费成功后的 PostBillingHook
//
// merge 策略：全新文件，无 upstream 冲突。

package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// applyModelQuotaOrFail 在请求进入上游前检查受保护模型的日/周额度。
//   - 返回 true 表示放行（未配置额度、额度未超限、Redis 故障 fail-open）。
//   - 返回 false 表示已写回 429 响应，调用方应立即 return。
func (h *GatewayHandler) applyModelQuotaOrFail(
	c *gin.Context,
	reqLog *zap.Logger,
	group *service.Group,
	model string,
	userID, groupID int64,
) bool {
	if h.modelQuotaService == nil || group == nil {
		return true
	}
	err := h.modelQuotaService.CheckProtectedModelQuota(c.Request.Context(), group, model, userID, groupID)
	if err == nil {
		return true
	}

	if errors.Is(err, service.ErrModelQuotaDailyExceeded) || errors.Is(err, service.ErrModelQuotaWeeklyExceeded) {
		reqLog.Info("gateway.model_quota.blocked",
			zap.String("model", model),
			zap.Int64("user_id", userID),
			zap.Int64("group_id", groupID),
			zap.Error(err),
		)
		h.handleStreamingAwareError(c, http.StatusTooManyRequests, "rate_limit_error",
			err.Error(), false)
		return false
	}

	// 非业务错误：fail-open
	reqLog.Warn("gateway.model_quota.check_error", zap.Error(err))
	return true
}

// makeModelQuotaHook 构造 RecordUsageInput.PostBillingHook。
// 当组未配置 ProtectedModelQuotas 时返回 nil（零开销）。
func (h *GatewayHandler) makeModelQuotaHook(group *service.Group) func(ctx context.Context, userID, groupID int64, model string, cost float64) {
	if h.modelQuotaService == nil || group == nil || len(group.ProtectedModelQuotas) == 0 {
		return nil
	}
	svc := h.modelQuotaService
	return func(ctx context.Context, userID, groupID int64, model string, cost float64) {
		svc.QueueUpdateModelQuotaUsage(ctx, userID, groupID, model, cost)
	}
}
