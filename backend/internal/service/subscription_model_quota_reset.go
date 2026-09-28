package service

import (
	"context"
	"fmt"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
)

// 私有扩展：订阅"重置配额"同步重置按模型配额用量。
//
// 订阅用量（user_subscriptions）与按模型用量（user_group_model_usage）是两层独立约束，
// 只重置前者时用户仍会被模型规则 429 拦截，因此重置订阅时按相同窗口一并归零后者。
//
// @author wangzhong

// subscriptionModelQuotaResetter 是 SubscriptionService 对按模型配额的最小依赖。
type subscriptionModelQuotaResetter interface {
	ResetUsage(ctx context.Context, userID, groupID int64, ruleKey string, resetDaily, resetWeekly, resetMonthly bool) error
	InvalidateUsageCache(ctx context.Context, userID, groupID int64, ruleKey string) error
}

// ProvideSubscriptionService 创建订阅服务并注入按模型配额重置能力。
func ProvideSubscriptionService(
	groupRepo GroupRepository,
	userSubRepo UserSubscriptionRepository,
	billingCacheService *BillingCacheService,
	entClient *dbent.Client,
	cfg *config.Config,
	modelQuotaUsage *ModelQuotaUsageService,
) *SubscriptionService {
	svc := NewSubscriptionService(groupRepo, userSubRepo, billingCacheService, entClient, cfg)
	if modelQuotaUsage != nil {
		svc.modelQuotaResetter = modelQuotaUsage
	}
	return svc
}

// resetModelQuotaUsage 按订阅重置的窗口归零该用户在该分组下所有规则的按模型用量。
func (s *SubscriptionService) resetModelQuotaUsage(ctx context.Context, sub *UserSubscription, resetDaily, resetWeekly, resetMonthly bool) error {
	if s.modelQuotaResetter == nil || sub == nil {
		return nil
	}
	if err := s.modelQuotaResetter.ResetUsage(ctx, sub.UserID, sub.GroupID, "", resetDaily, resetWeekly, resetMonthly); err != nil {
		return fmt.Errorf("reset model quota usage: %w", err)
	}
	return nil
}

// invalidateModelQuotaUsageCache 在事务提交后再次失效按模型用量缓存。
func (s *SubscriptionService) invalidateModelQuotaUsageCache(ctx context.Context, userID, groupID int64) error {
	if s.modelQuotaResetter == nil {
		return nil
	}
	return s.modelQuotaResetter.InvalidateUsageCache(ctx, userID, groupID, "")
}
