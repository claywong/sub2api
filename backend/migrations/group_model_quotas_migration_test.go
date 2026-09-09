package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGroupModelQuotasMigration 校验按模型配额迁移的关键约束。
//
// 迁移一旦应用就不能再改（有 checksum 校验），所以这些断言是防止后续
// 误编辑的护栏，而不只是重述内容。
func TestGroupModelQuotasMigration(t *testing.T) {
	content, err := FS.ReadFile("909_group_model_quotas.sql")
	require.NoError(t, err)

	raw := string(content)
	sql := strings.Join(strings.Fields(raw), " ")

	// 配置列：幂等新增，NOT NULL + 默认空对象（读侧按"未配置"处理）
	require.Contains(t, sql, "ALTER TABLE groups ADD COLUMN IF NOT EXISTS model_quotas JSONB NOT NULL DEFAULT '{}'::jsonb")

	// 用量表：幂等建表
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS user_group_model_usage")

	// 三窗口用量 + 窗口起点齐全，且用量为 NOT NULL DEFAULT 0（落账做加法，NULL 会传染）
	for _, column := range []string{"daily_usage_usd", "weekly_usage_usd", "monthly_usage_usd"} {
		require.Contains(t, sql, column+" DECIMAL(20,10) NOT NULL DEFAULT 0",
			"usage column %s must be NOT NULL DEFAULT 0", column)
	}
	for _, column := range []string{"daily_window_start", "weekly_window_start", "monthly_window_start"} {
		require.Contains(t, sql, column+" TIMESTAMPTZ", "window column %s missing", column)
	}

	// 金额精度必须与 user_platform_quotas 对齐，避免两套配额四舍五入口径不一致
	require.Contains(t, sql, "DECIMAL(20,10)")

	// 软删除友好的部分唯一索引：落账的 ON CONFLICT ... WHERE deleted_at IS NULL 依赖它，
	// 索引缺失或漏掉 WHERE 子句都会让并发落账撞唯一约束而回滚，丢掉本次用量。
	require.Contains(t, sql, "CREATE UNIQUE INDEX IF NOT EXISTS usergroupmodelusage_user_group_rule_uq")
	require.Contains(t, sql, "ON user_group_model_usage (user_id, group_id, rule_key)")
	require.Contains(t, sql, "WHERE deleted_at IS NULL")

	// 级联删除：用户/分组删除后不留孤儿用量行
	require.Contains(t, sql, "REFERENCES users(id) ON DELETE CASCADE")
	require.Contains(t, sql, "REFERENCES groups(id) ON DELETE CASCADE")

	// 按 (user, group) 批量读取用量的索引（判定与进度展示都走这个组合）
	require.Contains(t, sql, "usergroupmodelusage_user_group")

	// DDL 必须带超时保护，避免线上长时间持锁
	require.Contains(t, sql, "SET LOCAL lock_timeout")
	require.Contains(t, sql, "SET LOCAL statement_timeout")
}
