// 私有扩展（不属于 upstream sub2api）。
//
// 文件作用：为 GatewayHandler 提供会话级模型锁定的小型辅助方法。
// 包含方法：GatewayHandler.applySessionModelLockOrFail。
// merge 策略：全新文件，无 upstream 冲突。
//
// 设计动机：
//   /v1/messages 与 /v1/messages/count_tokens 两个入口都需要在 CheckBillingEligibility
//   之后、Forward/Select 之前调用 SessionModelLockService.Check。
//   把响应翻译统一抽出来，避免在主文件 gateway_handler.go 里产生重复 diff。
package handler

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// applySessionModelLockOrFail 执行会话级模型锁定检查。
//   - 返回 true 表示放行（包括"未配置锁"以及 Redis 故障的 fail-open）。
//   - 返回 false 表示已对外写回 403 响应，调用方应立即 return。
func (h *GatewayHandler) applySessionModelLockOrFail(c *gin.Context, reqLog *zap.Logger, apiKeyGroup *service.Group, parsedReq *service.ParsedRequest) bool {
	if h.sessionModelLockService == nil {
		return true
	}
	err := h.sessionModelLockService.Check(c.Request.Context(), apiKeyGroup, parsedReq)
	if err == nil {
		return true
	}

	var lockErr *service.SessionModelLockError
	if errors.As(err, &lockErr) {
		reqLog.Info("gateway.session_model_lock.blocked",
			zap.String("session_id", lockErr.SessionID),
			zap.String("first_model", lockErr.FirstModel),
			zap.String("current_model", lockErr.CurrentModel),
		)
		h.errorResponse(c, http.StatusForbidden, "permission_error",
			fmt.Sprintf("当前会话首个模型为：%q，不允许切换到锁定模型 %q，如果需要切换，请 /clear 或新开会话后切换", lockErr.FirstModel, lockErr.CurrentModel))
		return false
	}

	// 非业务错误：fail-open，但要记录方便排查
	reqLog.Warn("gateway.session_model_lock.check_error", zap.Error(err))
	return true
}
