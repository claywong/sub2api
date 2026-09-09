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

// modelQuotaFixture 建好一组 user + group，返回 repo 与两者 ID。
func modelQuotaFixture(t *testing.T) (service.UserGroupModelUsageRepository, int64, int64) {
	t.Helper()
	client := testEntClient(t)
	user := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("model-quota-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
	})
	group := mustCreateGroup(t, client, &service.Group{
		Name: "model-quota-group-" + uuid.NewString(),
	})
	return NewUserGroupModelUsageRepository(client), user.ID, group.ID
}

func TestUserGroupModelUsageRepo_IncrementCreatesAndAccumulates(t *testing.T) {
	ctx := context.Background()
	repo, userID, groupID := modelQuotaFixture(t)
	const ruleKey = "claude-opus*"
	now := time.Now()

	// 首次累加：应建行且三窗口都写入 cost
	require.NoError(t, repo.IncrementUsageWithReset(ctx, userID, groupID, ruleKey, 1.5, now))
	record, err := repo.GetByRule(ctx, userID, groupID, ruleKey)
	require.NoError(t, err)
	require.NotNil(t, record)
	require.InDelta(t, 1.5, record.DailyUsageUSD, 1e-9)
	require.InDelta(t, 1.5, record.WeeklyUsageUSD, 1e-9)
	require.InDelta(t, 1.5, record.MonthlyUsageUSD, 1e-9)
	require.NotNil(t, record.DailyWindowStart)
	require.True(t, record.DailyWindowStart.Equal(timezone.StartOfDay(now)))

	// 同窗口内二次累加：三窗口都应叠加
	require.NoError(t, repo.IncrementUsageWithReset(ctx, userID, groupID, ruleKey, 2.5, now))
	record, err = repo.GetByRule(ctx, userID, groupID, ruleKey)
	require.NoError(t, err)
	require.InDelta(t, 4.0, record.DailyUsageUSD, 1e-9)
	require.InDelta(t, 4.0, record.WeeklyUsageUSD, 1e-9)
	require.InDelta(t, 4.0, record.MonthlyUsageUSD, 1e-9)
}

func TestUserGroupModelUsageRepo_ResetsExpiredDailyWindow(t *testing.T) {
	ctx := context.Background()
	repo, userID, groupID := modelQuotaFixture(t)
	const ruleKey = "claude-opus*"

	yesterday := time.Now().AddDate(0, 0, -1)
	require.NoError(t, repo.IncrementUsageWithReset(ctx, userID, groupID, ruleKey, 10, yesterday))

	// 今天再累加：日窗口过期应重置为本次 cost，周/月仍在窗口内应叠加
	now := time.Now()
	require.NoError(t, repo.IncrementUsageWithReset(ctx, userID, groupID, ruleKey, 3, now))

	record, err := repo.GetByRule(ctx, userID, groupID, ruleKey)
	require.NoError(t, err)
	require.InDelta(t, 3.0, record.DailyUsageUSD, 1e-9, "expired daily window should reset to the new cost")
	require.InDelta(t, 13.0, record.WeeklyUsageUSD, 1e-9, "weekly window should still accumulate")
	require.True(t, record.DailyWindowStart.Equal(timezone.StartOfDay(now)))
}

func TestUserGroupModelUsageRepo_ResetsExpiredMonthlyWindow(t *testing.T) {
	ctx := context.Background()
	repo, userID, groupID := modelQuotaFixture(t)
	const ruleKey = "gpt-6-astra"

	old := time.Now().Add(-31 * 24 * time.Hour)
	require.NoError(t, repo.IncrementUsageWithReset(ctx, userID, groupID, ruleKey, 50, old))

	now := time.Now()
	require.NoError(t, repo.IncrementUsageWithReset(ctx, userID, groupID, ruleKey, 5, now))

	record, err := repo.GetByRule(ctx, userID, groupID, ruleKey)
	require.NoError(t, err)
	require.InDelta(t, 5.0, record.MonthlyUsageUSD, 1e-9, "30-day rolling window should reset once expired")
	require.NotNil(t, record.MonthlyWindowStart)
	require.WithinDuration(t, now, *record.MonthlyWindowStart, 5*time.Second,
		"expired monthly window should re-anchor to now")
}

func TestUserGroupModelUsageRepo_KeepsMonthlyAnchorWithinWindow(t *testing.T) {
	ctx := context.Background()
	repo, userID, groupID := modelQuotaFixture(t)
	const ruleKey = "gpt-6-astra"

	start := time.Now().Add(-10 * 24 * time.Hour)
	require.NoError(t, repo.IncrementUsageWithReset(ctx, userID, groupID, ruleKey, 4, start))
	first, err := repo.GetByRule(ctx, userID, groupID, ruleKey)
	require.NoError(t, err)
	require.NotNil(t, first.MonthlyWindowStart)
	anchor := *first.MonthlyWindowStart

	require.NoError(t, repo.IncrementUsageWithReset(ctx, userID, groupID, ruleKey, 6, time.Now()))
	second, err := repo.GetByRule(ctx, userID, groupID, ruleKey)
	require.NoError(t, err)
	require.InDelta(t, 10.0, second.MonthlyUsageUSD, 1e-9)
	require.True(t, second.MonthlyWindowStart.Equal(anchor),
		"monthly anchor must not drift while inside the window")
}

func TestUserGroupModelUsageRepo_RulesAreIsolated(t *testing.T) {
	ctx := context.Background()
	repo, userID, groupID := modelQuotaFixture(t)
	now := time.Now()

	require.NoError(t, repo.IncrementUsageWithReset(ctx, userID, groupID, "claude-opus*", 7, now))
	require.NoError(t, repo.IncrementUsageWithReset(ctx, userID, groupID, "gpt-6-astra", 3, now))

	opus, err := repo.GetByRule(ctx, userID, groupID, "claude-opus*")
	require.NoError(t, err)
	require.InDelta(t, 7.0, opus.DailyUsageUSD, 1e-9)

	astra, err := repo.GetByRule(ctx, userID, groupID, "gpt-6-astra")
	require.NoError(t, err)
	require.InDelta(t, 3.0, astra.DailyUsageUSD, 1e-9)

	all, err := repo.ListByUserGroup(ctx, userID, groupID)
	require.NoError(t, err)
	require.Len(t, all, 2)
}

func TestUserGroupModelUsageRepo_GetByRuleMissingReturnsNil(t *testing.T) {
	ctx := context.Background()
	repo, userID, groupID := modelQuotaFixture(t)

	record, err := repo.GetByRule(ctx, userID, groupID, "never-used*")
	require.NoError(t, err)
	require.Nil(t, record)
}

func TestUserGroupModelUsageRepo_ResetUsageWindows(t *testing.T) {
	ctx := context.Background()
	repo, userID, groupID := modelQuotaFixture(t)
	now := time.Now()
	require.NoError(t, repo.IncrementUsageWithReset(ctx, userID, groupID, "claude-opus*", 9, now))
	require.NoError(t, repo.IncrementUsageWithReset(ctx, userID, groupID, "gpt-6-astra", 4, now))

	// 只重置一条规则的日窗口
	require.NoError(t, repo.ResetUsageWindows(ctx, userID, groupID, "claude-opus*", true, false, false, now))

	opus, err := repo.GetByRule(ctx, userID, groupID, "claude-opus*")
	require.NoError(t, err)
	require.Zero(t, opus.DailyUsageUSD)
	require.InDelta(t, 9.0, opus.WeeklyUsageUSD, 1e-9, "weekly must be untouched when only daily is reset")

	astra, err := repo.GetByRule(ctx, userID, groupID, "gpt-6-astra")
	require.NoError(t, err)
	require.InDelta(t, 4.0, astra.DailyUsageUSD, 1e-9, "other rules must be untouched")

	// ruleKey 为空 → 重置该 (user, group) 下所有规则的全部窗口
	require.NoError(t, repo.ResetUsageWindows(ctx, userID, groupID, "", true, true, true, now))
	for _, key := range []string{"claude-opus*", "gpt-6-astra"} {
		record, err := repo.GetByRule(ctx, userID, groupID, key)
		require.NoError(t, err)
		require.Zero(t, record.DailyUsageUSD, key)
		require.Zero(t, record.WeeklyUsageUSD, key)
		require.Zero(t, record.MonthlyUsageUSD, key)
	}
}
