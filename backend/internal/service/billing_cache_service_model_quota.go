// 私有扩展（不属于 upstream sub2api）。
//
// 文件作用：受保护模型的 per-user 共享日/周额度检查与异步更新服务。
//   由 GatewayHandler 在请求前调用 CheckProtectedModelQuota，
//   在计费后调用 QueueUpdateModelQuotaUsage 更新缓存（fail-open）。
//
// 包含类型/方法：
//   - ModelQuotaCacheService（结构体与构造）
//   - ModelQuotaCacheService.CheckProtectedModelQuota
//   - ModelQuotaCacheService.QueueUpdateModelQuotaUsage
//
// 额度检查语义：
//   - 冷缓存（首次请求）= 0 用量 → 通过（不拦截）
//   - 计费成功后 UpdateModelQuotaUsage 写入/更新缓存
//   - Redis 故障时 fail-open（不阻塞请求）
//
// merge 策略：全新文件，无 upstream 冲突。

package service

import (
	"context"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

var (
	// ErrModelQuotaDailyExceeded 当天受保护模型额度已耗尽
	ErrModelQuotaDailyExceeded = infraerrors.TooManyRequests(
		"MODEL_DAILY_QUOTA_EXCEEDED", "daily protected-model usage limit exceeded for this subscription")
	// ErrModelQuotaWeeklyExceeded 本周受保护模型额度已耗尽
	ErrModelQuotaWeeklyExceeded = infraerrors.TooManyRequests(
		"MODEL_WEEKLY_QUOTA_EXCEEDED", "weekly protected-model usage limit exceeded for this subscription")
)

// ModelQuotaCacheService 受保护模型共享额度检查与更新服务。
type ModelQuotaCacheService struct {
	cache ModelQuotaCache
}

// NewModelQuotaCacheService 构造服务。
func NewModelQuotaCacheService(cache ModelQuotaCache) *ModelQuotaCacheService {
	return &ModelQuotaCacheService{cache: cache}
}

// CheckProtectedModelQuota 在请求前检查用户对受保护模型集合的共享日/周额度是否已耗尽。
//
// 快速 bypass（任一满足直接返回 nil）：
//   - group == nil 或 ProtectedModelQuota 未配置
//   - 配置的配额均未设置（nil 或 0）
//
// 冷缓存（key 不存在）时，使用量视为 0，请求通过。
// Redis 故障时 fail-open（返回 nil，不阻塞请求）。
func (s *ModelQuotaCacheService) CheckProtectedModelQuota(
	ctx context.Context,
	group *Group,
	userID, groupID int64,
) error {
	if group == nil || group.ProtectedModelQuota == nil {
		return nil
	}
	quota := group.ProtectedModelQuota
	if !quota.HasDailyLimit() && !quota.HasWeeklyLimit() {
		return nil
	}

	usage, err := s.cache.GetModelQuotaUsage(ctx, userID, groupID)
	if err != nil {
		logger.LegacyPrintf("service.model_quota",
			"Warning: model quota check failed (fail-open) user=%d group=%d: %v",
			userID, groupID, err)
		return nil
	}

	if quota.HasDailyLimit() && usage.DailyUsage >= *quota.DailyLimitUSD {
		return ErrModelQuotaDailyExceeded
	}
	if quota.HasWeeklyLimit() && usage.WeeklyUsage >= *quota.WeeklyLimitUSD {
		return ErrModelQuotaWeeklyExceeded
	}
	return nil
}

// QueueUpdateModelQuotaUsage 异步更新共享额度用量缓存（计费成功后调用）。
// 非关键路径；故障仅打日志，不影响主流程。
func (s *ModelQuotaCacheService) QueueUpdateModelQuotaUsage(
	ctx context.Context,
	userID, groupID int64,
	costUSD float64,
) {
	go func() {
		if err := s.cache.UpdateModelQuotaUsage(ctx, userID, groupID, costUSD); err != nil {
			logger.LegacyPrintf("service.model_quota",
				"Warning: update model quota usage failed user=%d group=%d: %v",
				userID, groupID, err)
		}
	}()
}
