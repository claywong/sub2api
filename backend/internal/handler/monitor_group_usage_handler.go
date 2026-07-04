// 私有扩展（不属于 upstream sub2api）。
//
// 功能：GET /api/v1/monitor/group-usage
// 返回登录用户可见分组在近 1 小时的消耗聚合结果。
//
// 认证：走 authenticated 路由组（JWT），从 gin context 拿 UserID。
// merge 策略：新文件；路由注册在 server/routes/monitor_group_usage.go。
package handler

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// MonitorGroupUsageHandler /monitor 页 分组消耗 section 的 HTTP handler。
type MonitorGroupUsageHandler struct {
	svc *service.MonitorGroupUsageService
}

// NewMonitorGroupUsageHandler 构造 handler。
func NewMonitorGroupUsageHandler(svc *service.MonitorGroupUsageService) *MonitorGroupUsageHandler {
	return &MonitorGroupUsageHandler{svc: svc}
}

// monitorGroupUsageResponse 响应结构。
// window_seconds 与 generated_at 便于前端展示窗口和刷新时间。
type monitorGroupUsageResponse struct {
	WindowSeconds int64                          `json:"window_seconds"`
	GeneratedAt   string                         `json:"generated_at"`
	Groups        []*service.MonitorGroupUsage   `json:"groups"`
}

// List GET /api/v1/monitor/group-usage
func (h *MonitorGroupUsageHandler) List(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	groups, err := h.svc.ListForUser(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, monitorGroupUsageResponse{
		WindowSeconds: int64(service.MonitorGroupUsageWindow.Seconds()),
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Groups:        groups,
	})
}
