// 私有扩展（不属于 upstream sub2api）。
//
// 文件作用：定义受保护模型共享额度所需的接口、数据类型。
//   具体实现在 billing_cache_service_model_quota.go（服务层）和
//   repository/billing_cache_model_quota.go（缓存层）。
//
// 包含类型：
//   - ModelQuotaUsage       — 日/周用量快照
//   - ModelQuotaCache       — Redis 操作接口
//
// merge 策略：全新文件，无 upstream 冲突。

package service

import "context"

// ModelQuotaUsage 日/周用量快照（来自 Redis 缓存）。
type ModelQuotaUsage struct {
	DailyUsage  float64
	WeeklyUsage float64
}

// ModelQuotaCache 受保护模型共享额度的 Redis 缓存接口（按 user+group 维度聚合）。
type ModelQuotaCache interface {
	// GetModelQuotaUsage 读取当前窗口内的用量（窗口过期自动归零）。
	GetModelQuotaUsage(ctx context.Context, userID, groupID int64) (ModelQuotaUsage, error)
	// SetModelQuotaUsage 写入初始用量（从 DB 预热时使用）。
	SetModelQuotaUsage(ctx context.Context, userID, groupID int64, data ModelQuotaUsage) error
	// UpdateModelQuotaUsage 增加用量；key 不存在时返回 nil（fail-open）。
	UpdateModelQuotaUsage(ctx context.Context, userID, groupID int64, cost float64) error
}
