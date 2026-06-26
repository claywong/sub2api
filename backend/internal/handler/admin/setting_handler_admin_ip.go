// 私有扩展（不属于 upstream sub2api）
// 功能：管理员 API Key IP 白名单管理接口
// merge 策略：纯增量，SettingHandler 的 companion 方法，不修改 upstream 已有方法
package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

// GetAdminAPIKeyIPWhitelist 获取管理员 API Key IP 白名单
// GET /api/v1/admin/settings/admin-api-key/ip-whitelist
func (h *SettingHandler) GetAdminAPIKeyIPWhitelist(c *gin.Context) {
	list, err := h.settingService.GetAdminAPIKeyIPWhitelist(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"ip_whitelist": list})
}

// UpdateAdminAPIKeyIPWhitelist 更新管理员 API Key IP 白名单
// PUT /api/v1/admin/settings/admin-api-key/ip-whitelist
func (h *SettingHandler) UpdateAdminAPIKeyIPWhitelist(c *gin.Context) {
	var req struct {
		IPWhitelist []string `json:"ip_whitelist"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request body")
		return
	}
	if req.IPWhitelist == nil {
		req.IPWhitelist = []string{}
	}
	if err := h.settingService.SetAdminAPIKeyIPWhitelist(c.Request.Context(), req.IPWhitelist); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"ip_whitelist": req.IPWhitelist})
}

// DeleteAdminAPIKeyIPWhitelist 清空管理员 API Key IP 白名单（恢复不限制 IP）
// DELETE /api/v1/admin/settings/admin-api-key/ip-whitelist
func (h *SettingHandler) DeleteAdminAPIKeyIPWhitelist(c *gin.Context) {
	if err := h.settingService.DeleteAdminAPIKeyIPWhitelist(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "Admin API key IP whitelist cleared"})
}
