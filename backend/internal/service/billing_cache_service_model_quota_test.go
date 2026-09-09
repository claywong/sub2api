//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

func asApplicationError(t *testing.T, err error) *infraerrors.ApplicationError {
	t.Helper()
	appErr, ok := err.(*infraerrors.ApplicationError)
	if !ok || appErr == nil {
		t.Fatalf("expected *ApplicationError, got %T (%v)", err, err)
	}
	return appErr
}

// modelQuotaSubscriptionCacheStub 让订阅检查通过（活跃、未过期、无用量）。
type modelQuotaSubscriptionCacheStub struct {
	BillingCache
}

func (m *modelQuotaSubscriptionCacheStub) GetSubscriptionCache(_ context.Context, _, _ int64) (*SubscriptionCacheData, error) {
	return &SubscriptionCacheData{
		Status:    SubscriptionStatusActive,
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}, nil
}

func (m *modelQuotaSubscriptionCacheStub) GetModelQuotaUsageCache(context.Context, int64, int64, string) (*ModelQuotaUsageCacheEntry, bool, error) {
	// 强制回源 repo，由 fakeModelQuotaUsageRepo 提供用量
	return nil, false, nil
}

func (m *modelQuotaSubscriptionCacheStub) SetModelQuotaUsageCache(context.Context, int64, int64, string, *ModelQuotaUsageCacheEntry, time.Duration) error {
	return nil
}

// modelQuotaBalanceCacheStub 让余额检查通过。
type modelQuotaBalanceCacheStub struct {
	BillingCache
	balance float64
}

func (m *modelQuotaBalanceCacheStub) GetUserBalance(_ context.Context, _ int64) (float64, error) {
	return m.balance, nil
}

func (m *modelQuotaBalanceCacheStub) GetModelQuotaUsageCache(context.Context, int64, int64, string) (*ModelQuotaUsageCacheEntry, bool, error) {
	return nil, false, nil
}

func (m *modelQuotaBalanceCacheStub) SetModelQuotaUsageCache(context.Context, int64, int64, string, *ModelQuotaUsageCacheEntry, time.Duration) error {
	return nil
}

// fakeModelQuotaUsageRepo 返回预置的用量记录。
type fakeModelQuotaUsageRepo struct {
	record  *UserGroupModelUsageRecord
	err     error
	gotRule string
}

func (f *fakeModelQuotaUsageRepo) GetByRule(_ context.Context, _, _ int64, ruleKey string) (*UserGroupModelUsageRecord, error) {
	f.gotRule = ruleKey
	if f.err != nil {
		return nil, f.err
	}
	return f.record, nil
}

func (f *fakeModelQuotaUsageRepo) ListByUserGroup(_ context.Context, _, _ int64) ([]UserGroupModelUsageRecord, error) {
	return nil, nil
}

func (f *fakeModelQuotaUsageRepo) IncrementUsageWithReset(_ context.Context, _, _ int64, _ string, _ float64, _ time.Time) error {
	return nil
}

func (f *fakeModelQuotaUsageRepo) ResetUsageWindows(_ context.Context, _, _ int64, _ string, _, _, _ bool, _ time.Time) error {
	return nil
}

// newModelQuotaTestService 构造只装配按模型配额所需依赖的服务实例。
// cache 留 nil：判定会直接回源 repo，正好覆盖 Redis 未配置的部署形态。
func newModelQuotaTestService(repo UserGroupModelUsageRepository) *BillingCacheService {
	return &BillingCacheService{
		cfg:                 &config.Config{},
		modelQuotaUsageRepo: repo,
	}
}

func modelQuotaGroup(rules ...GroupModelQuotaRule) *Group {
	return &Group{
		ID:               10,
		SubscriptionType: SubscriptionTypeStandard,
		Status:           StatusActive,
		ModelQuotas:      GroupModelQuotas{Enabled: true, Rules: rules},
	}
}

// usageNow 构造窗口起点均为当前窗口的用量记录（即用量都在有效期内）。
func usageNow(daily, weekly, monthly float64) *UserGroupModelUsageRecord {
	now := time.Now()
	dayStart := timezone.StartOfDay(now)
	weekStart := timezone.StartOfWeek(now)
	return &UserGroupModelUsageRecord{
		DailyUsageUSD:      daily,
		WeeklyUsageUSD:     weekly,
		MonthlyUsageUSD:    monthly,
		DailyWindowStart:   &dayStart,
		WeeklyWindowStart:  &weekStart,
		MonthlyWindowStart: &now,
	}
}

func TestCheckGroupModelQuotaEligibility(t *testing.T) {
	ctx := context.Background()

	t.Run("disabled group passes", func(t *testing.T) {
		repo := &fakeModelQuotaUsageRepo{record: usageNow(999, 999, 999)}
		svc := newModelQuotaTestService(repo)
		g := &Group{ID: 10, ModelQuotas: GroupModelQuotas{Enabled: false}}
		if err := svc.checkGroupModelQuotaEligibility(ctx, 1, g, "claude-opus-4-6"); err != nil {
			t.Fatalf("expected pass, got %v", err)
		}
		if repo.gotRule != "" {
			t.Error("disabled config should not query usage repo")
		}
	})

	t.Run("empty model passes", func(t *testing.T) {
		svc := newModelQuotaTestService(&fakeModelQuotaUsageRepo{})
		g := modelQuotaGroup(GroupModelQuotaRule{Match: "claude-opus*", Daily: f64(10)})
		if err := svc.checkGroupModelQuotaEligibility(ctx, 1, g, ""); err != nil {
			t.Fatalf("expected pass, got %v", err)
		}
	})

	t.Run("unmatched model passes", func(t *testing.T) {
		repo := &fakeModelQuotaUsageRepo{record: usageNow(999, 0, 0)}
		svc := newModelQuotaTestService(repo)
		g := modelQuotaGroup(GroupModelQuotaRule{Match: "claude-opus*", Daily: f64(10)})
		if err := svc.checkGroupModelQuotaEligibility(ctx, 1, g, "gpt-6-astra"); err != nil {
			t.Fatalf("expected pass, got %v", err)
		}
		if repo.gotRule != "" {
			t.Error("unmatched model should not query usage repo")
		}
	})

	t.Run("no usage record passes", func(t *testing.T) {
		svc := newModelQuotaTestService(&fakeModelQuotaUsageRepo{record: nil})
		g := modelQuotaGroup(GroupModelQuotaRule{Match: "claude-opus*", Daily: f64(10)})
		if err := svc.checkGroupModelQuotaEligibility(ctx, 1, g, "claude-opus-4-6"); err != nil {
			t.Fatalf("expected pass, got %v", err)
		}
	})

	t.Run("usage under limit passes", func(t *testing.T) {
		svc := newModelQuotaTestService(&fakeModelQuotaUsageRepo{record: usageNow(9.99, 0, 0)})
		g := modelQuotaGroup(GroupModelQuotaRule{Match: "claude-opus*", Daily: f64(10)})
		if err := svc.checkGroupModelQuotaEligibility(ctx, 1, g, "claude-opus-4-6"); err != nil {
			t.Fatalf("expected pass, got %v", err)
		}
	})

	t.Run("daily limit reached is rejected", func(t *testing.T) {
		svc := newModelQuotaTestService(&fakeModelQuotaUsageRepo{record: usageNow(10, 0, 0)})
		g := modelQuotaGroup(GroupModelQuotaRule{Match: "claude-opus*", Daily: f64(10)})
		err := svc.checkGroupModelQuotaEligibility(ctx, 1, g, "claude-opus-4-6")
		if !errors.Is(err, ErrModelDailyQuotaExhausted) {
			t.Fatalf("expected daily quota error, got %v", err)
		}
	})

	t.Run("weekly limit reached is rejected", func(t *testing.T) {
		svc := newModelQuotaTestService(&fakeModelQuotaUsageRepo{record: usageNow(0, 50, 0)})
		g := modelQuotaGroup(GroupModelQuotaRule{Match: "claude-opus*", Weekly: f64(50)})
		err := svc.checkGroupModelQuotaEligibility(ctx, 1, g, "claude-opus-4-6")
		if !errors.Is(err, ErrModelWeeklyQuotaExhausted) {
			t.Fatalf("expected weekly quota error, got %v", err)
		}
	})

	t.Run("monthly limit reached is rejected", func(t *testing.T) {
		svc := newModelQuotaTestService(&fakeModelQuotaUsageRepo{record: usageNow(0, 0, 200)})
		g := modelQuotaGroup(GroupModelQuotaRule{Match: "claude-opus*", Monthly: f64(200)})
		err := svc.checkGroupModelQuotaEligibility(ctx, 1, g, "claude-opus-4-6")
		if !errors.Is(err, ErrModelMonthlyQuotaExhausted) {
			t.Fatalf("expected monthly quota error, got %v", err)
		}
	})

	t.Run("zero limit denies immediately", func(t *testing.T) {
		svc := newModelQuotaTestService(&fakeModelQuotaUsageRepo{record: nil})
		g := modelQuotaGroup(GroupModelQuotaRule{Match: "gpt-6-astra", Daily: f64(0)})
		err := svc.checkGroupModelQuotaEligibility(ctx, 1, g, "gpt-6-astra")
		if !errors.Is(err, ErrModelDailyQuotaExhausted) {
			t.Fatalf("expected zero limit to deny, got %v", err)
		}
	})

	t.Run("expired daily window resets usage for the check", func(t *testing.T) {
		// 窗口起点在昨天：日用量应按 0 计，放行
		yesterday := timezone.StartOfDay(time.Now().AddDate(0, 0, -1))
		now := time.Now()
		record := &UserGroupModelUsageRecord{
			DailyUsageUSD:      999,
			DailyWindowStart:   &yesterday,
			MonthlyWindowStart: &now,
		}
		svc := newModelQuotaTestService(&fakeModelQuotaUsageRepo{record: record})
		g := modelQuotaGroup(GroupModelQuotaRule{Match: "claude-opus*", Daily: f64(10)})
		if err := svc.checkGroupModelQuotaEligibility(ctx, 1, g, "claude-opus-4-6"); err != nil {
			t.Fatalf("expired window should reset usage, got %v", err)
		}
	})

	t.Run("expired monthly window resets usage for the check", func(t *testing.T) {
		old := time.Now().Add(-31 * 24 * time.Hour)
		record := &UserGroupModelUsageRecord{
			MonthlyUsageUSD:    999,
			MonthlyWindowStart: &old,
		}
		svc := newModelQuotaTestService(&fakeModelQuotaUsageRepo{record: record})
		g := modelQuotaGroup(GroupModelQuotaRule{Match: "claude-opus*", Monthly: f64(10)})
		if err := svc.checkGroupModelQuotaEligibility(ctx, 1, g, "claude-opus-4-6"); err != nil {
			t.Fatalf("expired monthly window should reset usage, got %v", err)
		}
	})

	t.Run("nil repo fails open", func(t *testing.T) {
		svc := newModelQuotaTestService(nil)
		g := modelQuotaGroup(GroupModelQuotaRule{Match: "claude-opus*", Daily: f64(0)})
		if err := svc.checkGroupModelQuotaEligibility(ctx, 1, g, "claude-opus-4-6"); err != nil {
			t.Fatalf("nil repo should fail open, got %v", err)
		}
	})

	t.Run("repo error fails open", func(t *testing.T) {
		svc := newModelQuotaTestService(&fakeModelQuotaUsageRepo{err: errors.New("db down")})
		g := modelQuotaGroup(GroupModelQuotaRule{Match: "claude-opus*", Daily: f64(0)})
		if err := svc.checkGroupModelQuotaEligibility(ctx, 1, g, "claude-opus-4-6"); err != nil {
			t.Fatalf("repo error should fail open, got %v", err)
		}
	})

	t.Run("longest prefix rule owns the quota", func(t *testing.T) {
		repo := &fakeModelQuotaUsageRepo{record: usageNow(5, 0, 0)}
		svc := newModelQuotaTestService(repo)
		g := modelQuotaGroup(
			GroupModelQuotaRule{Match: "claude-opus*", Daily: f64(100)},
			GroupModelQuotaRule{Match: "claude-opus-4*", Daily: f64(5)},
		)
		err := svc.checkGroupModelQuotaEligibility(ctx, 1, g, "claude-opus-4-6")
		if !errors.Is(err, ErrModelDailyQuotaExhausted) {
			t.Fatalf("expected the longest prefix rule (limit 5) to apply, got %v", err)
		}
		if repo.gotRule != "claude-opus-4*" {
			t.Errorf("expected usage keyed by longest prefix rule, got %q", repo.gotRule)
		}
	})

	t.Run("error carries window_resets_at metadata", func(t *testing.T) {
		svc := newModelQuotaTestService(&fakeModelQuotaUsageRepo{record: usageNow(10, 0, 0)})
		g := modelQuotaGroup(GroupModelQuotaRule{Match: "claude-opus*", Daily: f64(10)})
		err := svc.checkGroupModelQuotaEligibility(ctx, 1, g, "claude-opus-4-6")
		appErr := asApplicationError(t, err)
		raw, ok := appErr.Metadata["window_resets_at"]
		if !ok || raw == "" {
			t.Fatalf("expected window_resets_at metadata, got %+v", appErr.Metadata)
		}
		resetAt, parseErr := time.Parse(time.RFC3339, raw)
		if parseErr != nil {
			t.Fatalf("metadata is not RFC3339: %v", parseErr)
		}
		if !resetAt.After(time.Now()) {
			t.Errorf("reset time should be in the future, got %v", resetAt)
		}
	})
}

// TestCheckBillingEligibilityAppliesModelQuotaInBothModes 验证按模型配额在订阅模式
// 与余额模式都生效——这是它与 user×platform quota（仅余额模式）的关键区别。
func TestCheckBillingEligibilityAppliesModelQuotaInBothModes(t *testing.T) {
	ctx := context.Background()
	rule := GroupModelQuotaRule{Match: "claude-opus*", Daily: f64(10)}

	t.Run("subscription mode", func(t *testing.T) {
		svc := &BillingCacheService{
			cfg:                 &config.Config{},
			modelQuotaUsageRepo: &fakeModelQuotaUsageRepo{record: usageNow(10, 0, 0)},
			cache:               &modelQuotaSubscriptionCacheStub{},
		}
		group := &Group{
			ID:               10,
			SubscriptionType: SubscriptionTypeSubscription,
			Status:           StatusActive,
			ModelQuotas:      GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{rule}},
		}
		sub := &UserSubscription{Status: SubscriptionStatusActive, ExpiresAt: time.Now().Add(24 * time.Hour)}
		err := svc.CheckBillingEligibility(ctx, &User{ID: 1}, nil, group, sub, "anthropic", "claude-opus-4-6")
		if !errors.Is(err, ErrModelDailyQuotaExhausted) {
			t.Fatalf("subscription mode should enforce model quota, got %v", err)
		}
	})

	t.Run("balance mode", func(t *testing.T) {
		svc := &BillingCacheService{
			cfg:                 &config.Config{},
			modelQuotaUsageRepo: &fakeModelQuotaUsageRepo{record: usageNow(10, 0, 0)},
			cache:               &modelQuotaBalanceCacheStub{balance: 1000},
		}
		group := &Group{
			ID:               10,
			SubscriptionType: SubscriptionTypeStandard,
			Status:           StatusActive,
			ModelQuotas:      GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{rule}},
		}
		err := svc.CheckBillingEligibility(ctx, &User{ID: 1}, nil, group, nil, "", "claude-opus-4-6")
		if !errors.Is(err, ErrModelDailyQuotaExhausted) {
			t.Fatalf("balance mode should enforce model quota, got %v", err)
		}
	})
}

func TestResolveModelQuotaRuleKey(t *testing.T) {
	groupWithRules := &Group{ModelQuotas: GroupModelQuotas{
		Enabled: true,
		Rules: []GroupModelQuotaRule{
			{Match: "Claude-Opus*", Daily: f64(10)},
			{Match: "gpt-6-astra", Daily: f64(5)},
		},
	}}

	tests := []struct {
		name   string
		apiKey *APIKey
		model  string
		want   string
	}{
		{"nil api key", nil, "claude-opus-4-6", ""},
		{"nil group", &APIKey{}, "claude-opus-4-6", ""},
		{"empty model", &APIKey{Group: groupWithRules}, "", ""},
		{"quotas disabled", &APIKey{Group: &Group{ModelQuotas: GroupModelQuotas{Enabled: false}}}, "claude-opus-4-6", ""},
		{"unmatched model", &APIKey{Group: groupWithRules}, "gemini-3-pro", ""},
		{"prefix rule normalized to lowercase", &APIKey{Group: groupWithRules}, "claude-opus-4-6", "claude-opus*"},
		{"exact rule", &APIKey{Group: groupWithRules}, "gpt-6-astra", "gpt-6-astra"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveModelQuotaRuleKey(tt.apiKey, tt.model); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestModelQuotaJudgementMatchesAccounting 是本功能最关键的一致性保证：
// 判定侧（checkGroupModelQuotaEligibility）与记账侧（resolveModelQuotaRuleKey）
// 必须对同一请求算出同一个规则键，否则会出现"判定用 A 的额度、用量记到 B"的错配。
func TestModelQuotaJudgementMatchesAccounting(t *testing.T) {
	group := modelQuotaGroup(
		GroupModelQuotaRule{Match: "claude*", Daily: f64(100)},
		GroupModelQuotaRule{Match: "claude-opus*", Daily: f64(50)},
		GroupModelQuotaRule{Match: "claude-opus-4-6", Daily: f64(10)},
		GroupModelQuotaRule{Match: "gpt-6-astra", Daily: f64(20)},
	)
	apiKey := &APIKey{Group: group}

	for _, model := range []string{
		"claude-opus-4-6",
		"claude-opus-4-5",
		"claude-sonnet-4-5",
		"gpt-6-astra",
		"models/gemini-3-pro",
		"CLAUDE-OPUS-4-6",
	} {
		t.Run(model, func(t *testing.T) {
			repo := &fakeModelQuotaUsageRepo{record: nil}
			svc := newModelQuotaTestService(repo)
			if err := svc.checkGroupModelQuotaEligibility(context.Background(), 1, group, model); err != nil {
				t.Fatalf("unexpected rejection: %v", err)
			}
			accountingKey := resolveModelQuotaRuleKey(apiKey, model)
			// repo.gotRule 为判定侧实际查询的键；未命中任何规则时两侧都应为空
			if repo.gotRule != accountingKey {
				t.Errorf("judgement key %q != accounting key %q", repo.gotRule, accountingKey)
			}
		})
	}
}
