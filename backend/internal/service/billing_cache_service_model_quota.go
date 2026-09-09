package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

// modelQuotaCacheTTL 是按模型配额用量缓存的 TTL。
// 与 billing 其它缓存同量级；DB 是权威值，缓存过期只会多一次回源查询。
const modelQuotaCacheTTL = 5 * time.Minute

// checkGroupModelQuotaEligibility 校验请求模型命中的配额规则是否已超限。
//
// 返回 nil = 允许；返回 ErrModel{Daily/Weekly/Monthly}QuotaExhausted = 拒绝
// （带 window_resets_at metadata，供网关换算 Retry-After）。
//
// 与分组总限额（daily/weekly/monthly_limit_usd）是两层独立约束，两层都需满足。
// 与计费模式无关：订阅模式和余额模式都生效。
//
// fail-open 场景（一律放行，不阻断计费链路）：
//   - 分组未启用按模型配额，或请求模型未命中任何带限额的规则
//   - 用量 repo 未注入
//   - Redis 与 DB 都读取失败
func (s *BillingCacheService) checkGroupModelQuotaEligibility(
	ctx context.Context,
	userID int64,
	group *Group,
	requestedModel string,
) error {
	if !group.ModelQuotasEnabled() || requestedModel == "" {
		return nil
	}
	rule := group.ModelQuotas.MatchRule(requestedModel)
	if rule == nil {
		return nil
	}
	if s.modelQuotaUsageRepo == nil {
		return nil
	}

	ruleKey := ModelQuotaRuleKey(rule.Match)
	entry, err := s.loadModelQuotaUsage(ctx, userID, group.ID, ruleKey)
	if err != nil {
		// 读取失败一律 fail-open：配额是限流性质的约束，不应因存储故障拒绝请求。
		slog.Warn("model_quota_usage_load_failed",
			"user_id", userID,
			"group_id", group.ID,
			"rule_key", ruleKey,
			"error", err,
		)
		return nil
	}
	if entry == nil {
		// 该规则还没有任何用量：只要限额 > 0 就放行；限额为 0 表示显式禁用。
		return checkModelQuotaLimits(rule, ModelQuotaUsageCacheEntry{}, time.Now())
	}
	return checkModelQuotaLimits(rule, *entry, time.Now())
}

// checkModelQuotaLimits 用给定用量快照逐窗口比对限额。
//
// 窗口已过期时把对应用量视为 0（DB 层 IncrementUsageWithReset 有窗口自愈能力，
// 持久化数据始终正确；此处只影响本次判定），与 checkUserPlatformQuotaEligibility
// 的处理方式一致。
func checkModelQuotaLimits(rule *GroupModelQuotaRule, entry ModelQuotaUsageCacheEntry, now time.Time) error {
	daily, weekly, monthly := effectiveModelQuotaUsage(entry, now)

	if rule.Daily != nil && daily >= *rule.Daily {
		return withWindowResetsMetadata(ErrModelDailyQuotaExhausted, nextDailyReset(now))
	}
	if rule.Weekly != nil && weekly >= *rule.Weekly {
		return withWindowResetsMetadata(ErrModelWeeklyQuotaExhausted, nextWeeklyReset(now))
	}
	if rule.Monthly != nil && monthly >= *rule.Monthly {
		return withWindowResetsMetadata(ErrModelMonthlyQuotaExhausted, nextMonthlyResetFrom(entry.MonthlyWindowStart, now))
	}
	return nil
}

// effectiveModelQuotaUsage 返回三窗口在 now 时刻的有效用量（过期窗口归零）。
func effectiveModelQuotaUsage(entry ModelQuotaUsageCacheEntry, now time.Time) (daily, weekly, monthly float64) {
	daily, weekly, monthly = entry.DailyUsageUSD, entry.WeeklyUsageUSD, entry.MonthlyUsageUSD
	if quotaWindowExpired(entry.DailyWindowStart, timezone.StartOfDay(now)) {
		daily = 0
	}
	if quotaWindowExpired(entry.WeeklyWindowStart, timezone.StartOfWeek(now)) {
		weekly = 0
	}
	if monthlyModelQuotaWindowExpired(entry.MonthlyWindowStart, now) {
		monthly = 0
	}
	return daily, weekly, monthly
}

// monthlyModelQuotaWindowExpired 判断 30 天滚动月度窗口是否已过期。
// 与 repository.monthlyMaybeReset 和 nextMonthlyResetFrom 同口径。
func monthlyModelQuotaWindowExpired(start *time.Time, now time.Time) bool {
	if start == nil {
		return true
	}
	return now.Sub(*start) >= 30*24*time.Hour
}

// loadModelQuotaUsage 读取用量快照：Redis 优先，MISS 或故障回源 DB 并回填缓存。
// 返回 (nil, nil) 表示该规则尚无用量记录。
func (s *BillingCacheService) loadModelQuotaUsage(
	ctx context.Context,
	userID, groupID int64,
	ruleKey string,
) (*ModelQuotaUsageCacheEntry, error) {
	if s.cache != nil {
		entry, ok, err := s.cache.GetModelQuotaUsageCache(ctx, userID, groupID, ruleKey)
		if err == nil && ok {
			return entry, nil
		}
		// err != nil（Redis 故障）与 !ok（MISS）都走 DB 回源
	}

	record, err := s.modelQuotaUsageRepo.GetByRule(ctx, userID, groupID, ruleKey)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, nil
	}
	entry := &ModelQuotaUsageCacheEntry{
		DailyUsageUSD:      record.DailyUsageUSD,
		WeeklyUsageUSD:     record.WeeklyUsageUSD,
		MonthlyUsageUSD:    record.MonthlyUsageUSD,
		DailyWindowStart:   record.DailyWindowStart,
		WeeklyWindowStart:  record.WeeklyWindowStart,
		MonthlyWindowStart: record.MonthlyWindowStart,
	}
	if s.cache != nil {
		if err := s.cache.SetModelQuotaUsageCache(ctx, userID, groupID, ruleKey, entry, modelQuotaCacheTTL); err != nil {
			slog.Warn("model_quota_usage_cache_set_failed",
				"user_id", userID,
				"group_id", groupID,
				"rule_key", ruleKey,
				"error", err,
			)
		}
	}
	return entry, nil
}

// UpdateModelQuotaUsageCache 在落账后累加用量缓存。
//
// DB 已在计费事务内权威累加，此处只维护缓存热度；key 不存在时是 no-op，
// 下次读取 MISS 会回源到已经正确的 DB 值。
func (s *BillingCacheService) UpdateModelQuotaUsageCache(ctx context.Context, userID, groupID int64, ruleKey string, cost float64) {
	if s == nil || s.cache == nil || ruleKey == "" || cost <= 0 {
		return
	}
	if err := s.cache.IncrModelQuotaUsageCache(ctx, userID, groupID, ruleKey, cost, modelQuotaCacheTTL); err != nil {
		slog.Warn("model_quota_usage_cache_incr_failed",
			"user_id", userID,
			"group_id", groupID,
			"rule_key", ruleKey,
			"error", err,
		)
	}
}

// InvalidateModelQuotaUsage 失效指定规则的用量缓存（管理端重置用量后调用）。
func (s *BillingCacheService) InvalidateModelQuotaUsage(ctx context.Context, userID, groupID int64, ruleKey string) error {
	if s == nil || s.cache == nil {
		return nil
	}
	return s.cache.InvalidateModelQuotaUsageCache(ctx, userID, groupID, ruleKey)
}
