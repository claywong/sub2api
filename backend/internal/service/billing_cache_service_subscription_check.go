// 私有扩展（不属于 upstream sub2api）
//
// 提供基于 Redis 缓存的订阅用量上限检查导出方法，供中间件层调用，
// 使中间件 fallback 决策与 handler 层 CheckBillingEligibility 共享同一份数据源，
// 避免出现"中间件依据 DB 快照判定未超 → handler 依据 Redis 判定已超 → 直接 429"
// 的窗口期，让首请求即可命中余额兜底。
//
// merge 策略：纯增量方法，永不与 upstream 冲突。
package service

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// CheckSubscriptionUsageLimit 仅基于 Redis 缓存校验订阅用量是否超限。
// 不做 RPM、不做 platform quota、不做 API Key rate limit；这些已分别在
// CheckBillingEligibility 与中间件其他位置覆盖。
//
// 返回值与 checkSubscriptionEligibility 完全一致：
//   - nil                     : 未超限
//   - ErrDailyLimitExceeded   : 日额超限
//   - ErrWeeklyLimitExceeded  : 周额超限
//   - ErrMonthlyLimitExceeded : 月额超限
//   - ErrSubscriptionInvalid  : 订阅状态异常或已过期（缓存视角）
//   - ErrBillingServiceUnavailable / 熔断错误：缓存不可用
func (s *BillingCacheService) CheckSubscriptionUsageLimit(
	ctx context.Context,
	userID int64,
	group *Group,
	subscription *UserSubscription,
) error {
	if s == nil || group == nil || subscription == nil {
		return nil
	}
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		return nil
	}
	return s.checkSubscriptionEligibility(ctx, userID, group, subscription)
}
