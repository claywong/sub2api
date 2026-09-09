package repository

import (
	"context"
	"fmt"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/usergroupmodelusage"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type userGroupModelUsageRepository struct {
	client *dbent.Client
}

// NewUserGroupModelUsageRepository 创建 UserGroupModelUsageRepository 实现。
func NewUserGroupModelUsageRepository(client *dbent.Client) service.UserGroupModelUsageRepository {
	return &userGroupModelUsageRepository{client: client}
}

func (r *userGroupModelUsageRepository) withTx(ctx context.Context, fn func(txCtx context.Context, txClient *dbent.Client) error) error {
	if tx := dbent.TxFromContext(ctx); tx != nil {
		return fn(ctx, tx.Client())
	}

	tx, err := r.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin user_group_model_usage transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	txCtx := dbent.NewTxContext(ctx, tx)
	if err := fn(txCtx, tx.Client()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit user_group_model_usage transaction: %w", err)
	}
	return nil
}

func (r *userGroupModelUsageRepository) GetByRule(ctx context.Context, userID, groupID int64, ruleKey string) (*service.UserGroupModelUsageRecord, error) {
	row, err := r.client.UserGroupModelUsage.Query().
		Where(
			usergroupmodelusage.UserIDEQ(userID),
			usergroupmodelusage.GroupIDEQ(groupID),
			usergroupmodelusage.RuleKeyEQ(ruleKey),
			usergroupmodelusage.DeletedAtIsNil(),
		).
		Only(ctx)
	if dbent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return usageEntityToRecord(row), nil
}

func (r *userGroupModelUsageRepository) ListByUserGroup(ctx context.Context, userID, groupID int64) ([]service.UserGroupModelUsageRecord, error) {
	rows, err := r.client.UserGroupModelUsage.Query().
		Where(
			usergroupmodelusage.UserIDEQ(userID),
			usergroupmodelusage.GroupIDEQ(groupID),
			usergroupmodelusage.DeletedAtIsNil(),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]service.UserGroupModelUsageRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, *usageEntityToRecord(row))
	}
	return out, nil
}

func usageEntityToRecord(row *dbent.UserGroupModelUsage) *service.UserGroupModelUsageRecord {
	if row == nil {
		return nil
	}
	return &service.UserGroupModelUsageRecord{
		UserID:             row.UserID,
		GroupID:            row.GroupID,
		RuleKey:            row.RuleKey,
		DailyUsageUSD:      row.DailyUsageUsd,
		WeeklyUsageUSD:     row.WeeklyUsageUsd,
		MonthlyUsageUSD:    row.MonthlyUsageUsd,
		DailyWindowStart:   row.DailyWindowStart,
		WeeklyWindowStart:  row.WeeklyWindowStart,
		MonthlyWindowStart: row.MonthlyWindowStart,
	}
}

// IncrementUsageWithReset 原子累加 cost 到 (user, group, rule) 三个窗口的用量。
//
// 窗口语义与 user_platform_quotas 完全一致（复用 maybeReset / monthlyMaybeReset）：
// 日按配置时区当日 0 点、周按本周起点、月为 30 天滚动。
//
// 记录不存在时插入新行——与 user_platform_quotas 的 fail-open 建行同理：
// 计费链路不能因用量行缺失而阻断请求。此处限额来自分组配置而非本表，
// 所以新建行不涉及"限额为 NULL 导致永久无限额"的问题。
//
// 并发下用 ON CONFLICT DO UPDATE 累加而非裸 INSERT：另一请求可能在本事务
// SELECT FOR UPDATE 之后、INSERT 之前刚建行，裸 INSERT 会撞部分唯一索引致
// 事务回滚、本次 cost 丢失。
func (r *userGroupModelUsageRepository) IncrementUsageWithReset(
	ctx context.Context,
	userID, groupID int64,
	ruleKey string,
	cost float64,
	now time.Time,
) error {
	return r.withTx(ctx, func(txCtx context.Context, txClient *dbent.Client) error {
		existing, err := txClient.UserGroupModelUsage.Query().
			Where(
				usergroupmodelusage.UserIDEQ(userID),
				usergroupmodelusage.GroupIDEQ(groupID),
				usergroupmodelusage.RuleKeyEQ(ruleKey),
				usergroupmodelusage.DeletedAtIsNil(),
			).
			ForUpdate().
			Only(txCtx)
		if dbent.IsNotFound(err) {
			const insertSQL = `INSERT INTO user_group_model_usage
				(user_id, group_id, rule_key, daily_usage_usd, weekly_usage_usd, monthly_usage_usd,
				 daily_window_start, weekly_window_start, monthly_window_start, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $4, $4, $5, $6, $7, $8, $8)
				ON CONFLICT (user_id, group_id, rule_key) WHERE deleted_at IS NULL DO UPDATE SET
					daily_usage_usd   = user_group_model_usage.daily_usage_usd   + EXCLUDED.daily_usage_usd,
					weekly_usage_usd  = user_group_model_usage.weekly_usage_usd  + EXCLUDED.weekly_usage_usd,
					monthly_usage_usd = user_group_model_usage.monthly_usage_usd + EXCLUDED.monthly_usage_usd,
					updated_at        = EXCLUDED.updated_at`
			// $7 = now：30 天滚动月度窗口以当前时刻为起始
			_, e := txClient.ExecContext(txCtx, insertSQL,
				userID, groupID, ruleKey, cost,
				timezone.StartOfDay(now), timezone.StartOfWeek(now), now, now)
			return e
		}
		if err != nil {
			return err
		}

		newDaily := maybeReset(existing.DailyUsageUsd, existing.DailyWindowStart, timezone.StartOfDay(now), cost)
		newWeekly := maybeReset(existing.WeeklyUsageUsd, existing.WeeklyWindowStart, timezone.StartOfWeek(now), cost)
		newMonthly, newMonthlyStart := monthlyMaybeReset(existing.MonthlyUsageUsd, existing.MonthlyWindowStart, cost, now)

		_, e := existing.Update().
			SetDailyUsageUsd(newDaily).
			SetWeeklyUsageUsd(newWeekly).
			SetMonthlyUsageUsd(newMonthly).
			SetDailyWindowStart(timezone.StartOfDay(now)).
			SetWeeklyWindowStart(timezone.StartOfWeek(now)).
			SetMonthlyWindowStart(newMonthlyStart). // 30 天滚动：仅过期时更新起始
			Save(txCtx)
		return e
	})
}

// ResetUsageWindows 无条件把指定窗口归零并重写窗口起点。
//
// ⚠️ 与 user_platform_quotas 的 ResetExpiredWindow 同样**不校验窗口是否过期**，
// 仅供管理端强制重置使用。自动过期重置由 IncrementUsageWithReset 内部完成。
//
// 日窗口锚点取当天 0 点（手动重置只清用量，不改变"每天 0 点刷新"的节奏），
// 周/月锚定重置时刻，与 SubscriptionService.AdminResetQuota 的语义一致。
func (r *userGroupModelUsageRepository) ResetUsageWindows(
	ctx context.Context,
	userID, groupID int64,
	ruleKey string,
	resetDaily, resetWeekly, resetMonthly bool,
	now time.Time,
) error {
	if !resetDaily && !resetWeekly && !resetMonthly {
		return nil
	}
	update := r.client.UserGroupModelUsage.Update().
		Where(
			usergroupmodelusage.UserIDEQ(userID),
			usergroupmodelusage.GroupIDEQ(groupID),
			usergroupmodelusage.DeletedAtIsNil(),
		)
	if ruleKey != "" {
		update = update.Where(usergroupmodelusage.RuleKeyEQ(ruleKey))
	}
	if resetDaily {
		update = update.SetDailyUsageUsd(0).SetDailyWindowStart(timezone.StartOfDay(now))
	}
	if resetWeekly {
		update = update.SetWeeklyUsageUsd(0).SetWeeklyWindowStart(now)
	}
	if resetMonthly {
		update = update.SetMonthlyUsageUsd(0).SetMonthlyWindowStart(now)
	}
	_, err := update.Save(ctx)
	return err
}
