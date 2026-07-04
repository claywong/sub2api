// 私有扩展（不属于 upstream sub2api）。
//
// 功能：注册 /monitor 页 分组消耗 section 的用户路由。
// 挂载点：GET /api/v1/monitor/group-usage
//
// 说明：单独一个函数以便主路由 registerRoutes 只需追加一行调用，
// 不需要动 user.go 里的 upstream 路由分组。
package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"

	"github.com/gin-gonic/gin"
)

// RegisterMonitorGroupUsageRoutes 注册 monitor 分组消耗聚合路由。
// 需要 JWT 认证，用户维度过滤。
func RegisterMonitorGroupUsageRoutes(
	v1 *gin.RouterGroup,
	h *handler.Handlers,
	jwtAuth middleware.JWTAuthMiddleware,
) {
	if h == nil || h.MonitorGroupUsage == nil {
		return
	}

	group := v1.Group("/monitor")
	group.Use(gin.HandlerFunc(jwtAuth))
	group.GET("/group-usage", h.MonitorGroupUsage.List)
}
