package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

// ModelQuotaUsageWindow 是单个窗口的用量进度。
type ModelQuotaUsageWindow struct {
	LimitUSD        float64   `json:"limit_usd"`
	UsedUSD         float64   `json:"used_usd"`
	RemainingUSD    float64   `json:"remaining_usd"`
	Percentage      float64   `json:"percentage"`
	ResetsAt        time.Time `json:"resets_at"`
	ResetsInSeconds int64     `json:"resets_in_seconds"`
}

// ModelQuotaUsageProgress 是某条规则在某用户下的用量进度。
// 未配置上限的窗口为 nil。
type ModelQuotaUsageProgress struct {
	Match   string                 `json:"match"`
	RuleKey string                 `json:"rule_key"`
	Daily   *ModelQuotaUsageWindow `json:"daily,omitempty"`
	Weekly  *ModelQuotaUsageWindow `json:"weekly,omitempty"`
	Monthly *ModelQuotaUsageWindow `json:"monthly,omitempty"`
}

// AdminModelQuotaService 提供管理端查看/重置按模型配额用量的能力。
type AdminModelQuotaService struct {
	usageRepo    UserGroupModelUsageRepository
	groupRepo    GroupRepository
	billingCache *BillingCacheService
}

// NewAdminModelQuotaService 创建 AdminModelQuotaService。
func NewAdminModelQuotaService(
	usageRepo UserGroupModelUsageRepository,
	groupRepo GroupRepository,
	billingCache *BillingCacheService,
) *AdminModelQuotaService {
	return &AdminModelQuotaService{
		usageRepo:    usageRepo,
		groupRepo:    groupRepo,
		billingCache: billingCache,
	}
}

// GetUsage 返回某用户在某分组下所有已配置规则的用量进度。
//
// 以分组当前配置的规则为准来组织输出：配置里已删除的规则即使还有历史用量行
// 也不返回（与判定侧"匹配不到规则即忽略"的处理保持一致）。
func (s *AdminModelQuotaService) GetUsage(ctx context.Context, userID, groupID int64) ([]ModelQuotaUsageProgress, error) {
	group, err := s.groupRepo.GetByIDLite(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if !group.ModelQuotasEnabled() {
		return []ModelQuotaUsageProgress{}, nil
	}

	records, err := s.usageRepo.ListByUserGroup(ctx, userID, groupID)
	if err != nil {
		return nil, err
	}
	byRule := make(map[string]UserGroupModelUsageRecord, len(records))
	for _, record := range records {
		byRule[record.RuleKey] = record
	}

	now := time.Now()
	out := make([]ModelQuotaUsageProgress, 0, len(group.ModelQuotas.Rules))
	for i := range group.ModelQuotas.Rules {
		rule := &group.ModelQuotas.Rules[i]
		if !rule.HasLimit() {
			continue
		}
		ruleKey := ModelQuotaRuleKey(rule.Match)
		record := byRule[ruleKey] // 缺省零值：无用量记录时按 0 展示
		out = append(out, buildModelQuotaProgress(rule, ruleKey, record, now))
	}
	return out, nil
}

func buildModelQuotaProgress(
	rule *GroupModelQuotaRule,
	ruleKey string,
	record UserGroupModelUsageRecord,
	now time.Time,
) ModelQuotaUsageProgress {
	entry := ModelQuotaUsageCacheEntry{
		DailyUsageUSD:      record.DailyUsageUSD,
		WeeklyUsageUSD:     record.WeeklyUsageUSD,
		MonthlyUsageUSD:    record.MonthlyUsageUSD,
		DailyWindowStart:   record.DailyWindowStart,
		WeeklyWindowStart:  record.WeeklyWindowStart,
		MonthlyWindowStart: record.MonthlyWindowStart,
	}
	// 过期窗口按 0 展示，与判定口径一致，避免界面显示"已满"但实际可用
	daily, weekly, monthly := effectiveModelQuotaUsage(entry, now)

	progress := ModelQuotaUsageProgress{Match: rule.Match, RuleKey: ruleKey}
	if rule.Daily != nil {
		progress.Daily = buildModelQuotaWindow(*rule.Daily, daily, nextDailyReset(now), now)
	}
	if rule.Weekly != nil {
		progress.Weekly = buildModelQuotaWindow(*rule.Weekly, weekly, nextWeeklyReset(now), now)
	}
	if rule.Monthly != nil {
		progress.Monthly = buildModelQuotaWindow(*rule.Monthly, monthly, nextMonthlyResetFrom(record.MonthlyWindowStart, now), now)
	}
	return progress
}

func buildModelQuotaWindow(limit, used float64, resetsAt, now time.Time) *ModelQuotaUsageWindow {
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	percentage := 0.0
	if limit > 0 {
		percentage = (used / limit) * 100
	} else {
		// limit == 0 表示显式禁用：展示为 100% 已用，避免界面显示 0% 误导
		percentage = 100
	}
	return &ModelQuotaUsageWindow{
		LimitUSD:        limit,
		UsedUSD:         used,
		RemainingUSD:    remaining,
		Percentage:      percentage,
		ResetsAt:        resetsAt,
		ResetsInSeconds: int64(resetsAt.Sub(now).Seconds()),
	}
}

// ResetUsage 强制把指定规则（ruleKey 为空则该分组下全部规则）的窗口用量归零。
//
// 归零后必须失效 Redis 用量缓存，否则下次 preflight 仍会读到旧用量。
// ruleKey 为空时逐条失效，不用 KEYS/SCAN 扫描（生产 Redis 上是危险操作）。
func (s *AdminModelQuotaService) ResetUsage(
	ctx context.Context,
	userID, groupID int64,
	ruleKey string,
	resetDaily, resetWeekly, resetMonthly bool,
) error {
	if !resetDaily && !resetWeekly && !resetMonthly {
		return ErrInvalidInput
	}
	if err := s.usageRepo.ResetUsageWindows(ctx, userID, groupID, ruleKey, resetDaily, resetWeekly, resetMonthly, timezone.Now()); err != nil {
		return err
	}
	if s.billingCache == nil {
		return nil
	}
	if ruleKey != "" {
		return s.billingCache.InvalidateModelQuotaUsage(ctx, userID, groupID, ruleKey)
	}
	group, err := s.groupRepo.GetByIDLite(ctx, groupID)
	if err != nil {
		// 用量已归零，仅缓存未失效：不阻断本次操作，缓存最长在 TTL 后自愈
		return nil
	}
	for i := range group.ModelQuotas.Rules {
		key := ModelQuotaRuleKey(group.ModelQuotas.Rules[i].Match)
		if key == "" {
			continue
		}
		_ = s.billingCache.InvalidateModelQuotaUsage(ctx, userID, groupID, key)
	}
	return nil
}
