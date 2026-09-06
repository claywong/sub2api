// 功能：全局 IP 白名单管理接口
// 用于控制整个平台的访问 IP 限制
package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

// GetGlobalIPAllowlist 获取全局 IP 白名单配置
// GET /api/v1/admin/settings/security/ip-allowlist
func (h *SettingHandler) GetGlobalIPAllowlist(c *gin.Context) {
	enabled := h.settingService.IPAllowlistEnabled(c.Request.Context())
	list, err := h.settingService.ListEnabledIPAllowlist(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if list == nil {
		list = []string{}
	}
	response.Success(c, gin.H{
		"enabled":      enabled,
		"ip_allowlist": list,
	})
}

// UpdateGlobalIPAllowlist 更新全局 IP 白名单
// PUT /api/v1/admin/settings/security/ip-allowlist
func (h *SettingHandler) UpdateGlobalIPAllowlist(c *gin.Context) {
	var req struct {
		IPAllowlist []string `json:"ip_allowlist"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request body")
		return
	}
	if req.IPAllowlist == nil {
		req.IPAllowlist = []string{}
	}
	if err := h.settingService.ReplaceIPAllowlist(c.Request.Context(), req.IPAllowlist); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"ip_allowlist": req.IPAllowlist})
}

// DeleteGlobalIPAllowlist 清空全局 IP 白名单（恢复不限制 IP）
// DELETE /api/v1/admin/settings/security/ip-allowlist
func (h *SettingHandler) DeleteGlobalIPAllowlist(c *gin.Context) {
	if err := h.settingService.ReplaceIPAllowlist(c.Request.Context(), []string{}); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "Global IP allowlist cleared"})
}

// ToggleGlobalIPAllowlist 启用/禁用全局 IP 白名单
// PUT /api/v1/admin/settings/security/ip-allowlist/toggle
func (h *SettingHandler) ToggleGlobalIPAllowlist(c *gin.Context) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request body")
		return
	}
	if err := h.settingService.SetIPAllowlistEnabled(c.Request.Context(), req.Enabled); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"enabled": req.Enabled})
}
