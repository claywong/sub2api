// 私有扩展（不属于 upstream sub2api）
// 功能：管理员 API Key IP 白名单 CRUD
// merge 策略：纯增量，不修改 upstream 已有方法
package service

import (
	"context"
	"encoding/json"
	"errors"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
)

// GetAdminAPIKeyIPWhitelist 获取管理员 API Key IP 白名单列表。
// 未配置时返回空切片和 nil 错误。
func (s *SettingService) GetAdminAPIKeyIPWhitelist(ctx context.Context) ([]string, error) {
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyAdminAPIKeyIPWhitelist)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return []string{}, nil
		}
		return nil, err
	}
	if raw == "" {
		return []string{}, nil
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return []string{}, nil
	}
	return list, nil
}

// SetAdminAPIKeyIPWhitelist 保存管理员 API Key IP 白名单。
// ips 为空时等同于清空白名单（不限制 IP）。
// 无效的 IP/CIDR 格式会被拒绝。
func (s *SettingService) SetAdminAPIKeyIPWhitelist(ctx context.Context, ips []string) error {
	if invalid := ip.ValidateIPPatterns(ips); len(invalid) > 0 {
		return infraerrors.BadRequest("INVALID_IP_PATTERN", "invalid IP/CIDR patterns: "+joinStrings(invalid))
	}
	if len(ips) == 0 {
		return s.settingRepo.Set(ctx, SettingKeyAdminAPIKeyIPWhitelist, "[]")
	}
	data, err := json.Marshal(ips)
	if err != nil {
		return err
	}
	return s.settingRepo.Set(ctx, SettingKeyAdminAPIKeyIPWhitelist, string(data))
}

// DeleteAdminAPIKeyIPWhitelist 清空管理员 API Key IP 白名单（恢复不限制 IP）。
func (s *SettingService) DeleteAdminAPIKeyIPWhitelist(ctx context.Context) error {
	return s.settingRepo.Delete(ctx, SettingKeyAdminAPIKeyIPWhitelist)
}

// GetAdminAPIKeyCompiledIPWhitelist 获取预编译的 IP 白名单规则，供认证中间件使用。
// 未配置或为空时返回 nil（表示不限制）。
func (s *SettingService) GetAdminAPIKeyCompiledIPWhitelist(ctx context.Context) (*ip.CompiledIPRules, error) {
	list, err := s.GetAdminAPIKeyIPWhitelist(ctx)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	return ip.CompileIPRules(list), nil
}

// joinStrings 将字符串切片拼接为逗号分隔的字符串（避免引入 strings 包依赖）。
func joinStrings(ss []string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += ", "
		}
		result += s
	}
	return result
}
