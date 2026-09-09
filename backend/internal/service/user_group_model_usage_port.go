package service

import (
	"context"
	"time"
)

// UserGroupModelUsageRecord 是 (user, group, rule_key) 维度的用量快照。
//
// 只含用量与窗口起点，不含限额——限额存在 groups.model_quotas 配置里，
// 判定时从已加载的 Group 读取，无需再查库。
type UserGroupModelUsageRecord struct {
	UserID             int64
	GroupID            int64
	RuleKey            string
	DailyUsageUSD      float64
	WeeklyUsageUSD     float64
	MonthlyUsageUSD    float64
	DailyWindowStart   *time.Time
	WeeklyWindowStart  *time.Time
	MonthlyWindowStart *time.Time
}

// UserGroupModelUsageRepository 定义按模型配额用量的数据访问接口。
type UserGroupModelUsageRepository interface {
	// GetByRule 查询单条用量记录，未找到时返回 (nil, nil)。
	GetByRule(ctx context.Context, userID, groupID int64, ruleKey string) (*UserGroupModelUsageRecord, error)
	// ListByUserGroup 查询该用户在该分组下所有规则的用量记录（排除软删除）。
	ListByUserGroup(ctx context.Context, userID, groupID int64) ([]UserGroupModelUsageRecord, error)
	// IncrementUsageWithReset 原子累加 cost，窗口已过期则先重置再累加。
	IncrementUsageWithReset(ctx context.Context, userID, groupID int64, ruleKey string, cost float64, now time.Time) error
	// ResetUsageWindows 无条件把指定窗口的用量归零并重写窗口起点（管理端强制重置）。
	// ruleKey 为空表示重置该 (user, group) 下的所有规则。
	ResetUsageWindows(ctx context.Context, userID, groupID int64, ruleKey string, resetDaily, resetWeekly, resetMonthly bool, now time.Time) error
}
