//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// modelQuotaBillingFixture 建好 user / group / apiKey / account，返回落账 repo、
// 用量读取 repo 以及各方 ID。
type modelQuotaBillingFixture struct {
	billingRepo service.UsageBillingRepository
	usageRepo   service.UserGroupModelUsageRepository
	userID      int64
	groupID     int64
	apiKeyID    int64
	accountID   int64
}

func newModelQuotaBillingFixture(t *testing.T) modelQuotaBillingFixture {
	t.Helper()
	client := testEntClient(t)
	user := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("mq-billing-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Balance:      1000,
	})
	group := mustCreateGroup(t, client, &service.Group{
		Name: "mq-billing-group-" + uuid.NewString(),
	})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{
		UserID:  user.ID,
		Key:     "sk-mq-billing-" + uuid.NewString(),
		Name:    "billing",
		GroupID: &group.ID,
	})
	account := mustCreateAccount(t, client, &service.Account{
		Name: "mq-billing-account-" + uuid.NewString(),
		Type: service.AccountTypeAPIKey,
	})
	return modelQuotaBillingFixture{
		billingRepo: NewUsageBillingRepository(client, integrationDB),
		usageRepo:   NewUserGroupModelUsageRepository(client),
		userID:      user.ID,
		groupID:     group.ID,
		apiKeyID:    apiKey.ID,
		accountID:   account.ID,
	}
}

func (f modelQuotaBillingFixture) command(ruleKey string, cost float64) *service.UsageBillingCommand {
	return &service.UsageBillingCommand{
		RequestID:         uuid.NewString(),
		APIKeyID:          f.apiKeyID,
		UserID:            f.userID,
		AccountID:         f.accountID,
		GroupID:           f.groupID,
		AccountType:       service.AccountTypeAPIKey,
		Model:             "claude-opus-4-6",
		BalanceCost:       cost,
		ModelQuotaRuleKey: ruleKey,
		ModelQuotaCost:    cost,
	}
}

func TestUsageBillingApply_AccumulatesModelQuotaInTransaction(t *testing.T) {
	ctx := context.Background()
	f := newModelQuotaBillingFixture(t)
	const ruleKey = "claude-opus*"

	result, err := f.billingRepo.Apply(ctx, f.command(ruleKey, 2.5))
	require.NoError(t, err)
	require.True(t, result.Applied)

	record, err := f.usageRepo.GetByRule(ctx, f.userID, f.groupID, ruleKey)
	require.NoError(t, err)
	require.NotNil(t, record, "billing transaction must create the usage row")
	require.InDelta(t, 2.5, record.DailyUsageUSD, 1e-9)
	require.InDelta(t, 2.5, record.WeeklyUsageUSD, 1e-9)
	require.InDelta(t, 2.5, record.MonthlyUsageUSD, 1e-9)
	require.NotNil(t, record.DailyWindowStart)
	require.True(t, record.DailyWindowStart.Equal(timezone.StartOfDay(time.Now())))

	// 第二笔：同窗口内应叠加
	result, err = f.billingRepo.Apply(ctx, f.command(ruleKey, 1.25))
	require.NoError(t, err)
	require.True(t, result.Applied)

	record, err = f.usageRepo.GetByRule(ctx, f.userID, f.groupID, ruleKey)
	require.NoError(t, err)
	require.InDelta(t, 3.75, record.DailyUsageUSD, 1e-9)
}

// TestUsageBillingApply_ModelQuotaIsIdempotentPerRequest 验证按模型配额复用了
// usage_billing_dedup 的请求级幂等：同一 request_id 重放不会重复累加用量。
func TestUsageBillingApply_ModelQuotaIsIdempotentPerRequest(t *testing.T) {
	ctx := context.Background()
	f := newModelQuotaBillingFixture(t)
	const ruleKey = "claude-opus*"

	cmd := f.command(ruleKey, 4)
	first, err := f.billingRepo.Apply(ctx, cmd)
	require.NoError(t, err)
	require.True(t, first.Applied)

	// 同一 request_id + 同一指纹重放
	replay, err := f.billingRepo.Apply(ctx, cmd)
	require.NoError(t, err)
	require.False(t, replay.Applied, "replay must be deduplicated")

	record, err := f.usageRepo.GetByRule(ctx, f.userID, f.groupID, ruleKey)
	require.NoError(t, err)
	require.InDelta(t, 4.0, record.DailyUsageUSD, 1e-9, "replay must not double-count model quota usage")
}

func TestUsageBillingApply_SkipsModelQuotaWithoutRuleKey(t *testing.T) {
	ctx := context.Background()
	f := newModelQuotaBillingFixture(t)

	cmd := f.command("", 3)
	result, err := f.billingRepo.Apply(ctx, cmd)
	require.NoError(t, err)
	require.True(t, result.Applied)

	rows, err := f.usageRepo.ListByUserGroup(ctx, f.userID, f.groupID)
	require.NoError(t, err)
	require.Empty(t, rows, "no rule key means no model quota row")
}

// TestUsageBillingApply_ResetsExpiredDailyWindow 覆盖落账 SQL 里的窗口自愈分支：
// 该逻辑用纯 SQL 的 CASE 表达式实现（不走 repo 的 ent 路径），必须独立验证。
func TestUsageBillingApply_ResetsExpiredDailyWindow(t *testing.T) {
	ctx := context.Background()
	f := newModelQuotaBillingFixture(t)
	const ruleKey = "claude-opus*"

	// 先用 repo 造一条昨天的用量行
	yesterday := time.Now().AddDate(0, 0, -1)
	require.NoError(t, f.usageRepo.IncrementUsageWithReset(ctx, f.userID, f.groupID, ruleKey, 100, yesterday))

	// 再走落账：日窗口已过期，应重置为本次 cost；周窗口未过期，应叠加
	result, err := f.billingRepo.Apply(ctx, f.command(ruleKey, 7))
	require.NoError(t, err)
	require.True(t, result.Applied)

	record, err := f.usageRepo.GetByRule(ctx, f.userID, f.groupID, ruleKey)
	require.NoError(t, err)
	require.InDelta(t, 7.0, record.DailyUsageUSD, 1e-9,
		"billing SQL must reset an expired daily window instead of accumulating")
	require.InDelta(t, 107.0, record.WeeklyUsageUSD, 1e-9,
		"weekly window is still open and must accumulate")
}

func TestUsageBillingApply_ResetsExpiredMonthlyWindow(t *testing.T) {
	ctx := context.Background()
	f := newModelQuotaBillingFixture(t)
	const ruleKey = "gpt-6-astra"

	old := time.Now().Add(-31 * 24 * time.Hour)
	require.NoError(t, f.usageRepo.IncrementUsageWithReset(ctx, f.userID, f.groupID, ruleKey, 80, old))

	result, err := f.billingRepo.Apply(ctx, f.command(ruleKey, 6))
	require.NoError(t, err)
	require.True(t, result.Applied)

	record, err := f.usageRepo.GetByRule(ctx, f.userID, f.groupID, ruleKey)
	require.NoError(t, err)
	require.InDelta(t, 6.0, record.MonthlyUsageUSD, 1e-9,
		"billing SQL must reset a 30-day rolling window once expired")
	require.WithinDuration(t, time.Now(), *record.MonthlyWindowStart, 10*time.Second)
}

func TestUsageBillingApply_KeepsMonthlyAnchorWithinWindow(t *testing.T) {
	ctx := context.Background()
	f := newModelQuotaBillingFixture(t)
	const ruleKey = "gpt-6-astra"

	start := time.Now().Add(-5 * 24 * time.Hour)
	require.NoError(t, f.usageRepo.IncrementUsageWithReset(ctx, f.userID, f.groupID, ruleKey, 20, start))
	before, err := f.usageRepo.GetByRule(ctx, f.userID, f.groupID, ruleKey)
	require.NoError(t, err)
	anchor := *before.MonthlyWindowStart

	result, err := f.billingRepo.Apply(ctx, f.command(ruleKey, 5))
	require.NoError(t, err)
	require.True(t, result.Applied)

	after, err := f.usageRepo.GetByRule(ctx, f.userID, f.groupID, ruleKey)
	require.NoError(t, err)
	require.InDelta(t, 25.0, after.MonthlyUsageUSD, 1e-9)
	require.True(t, after.MonthlyWindowStart.Equal(anchor),
		"billing SQL must not drift the monthly anchor while inside the window")
}
