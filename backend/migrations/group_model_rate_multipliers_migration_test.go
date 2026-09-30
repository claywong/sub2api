package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGroupModelRateMultipliersMigration 校验分组单模型倍率迁移的关键约束。
//
// 迁移一旦应用就不能再改（有 checksum 校验），这些断言是防止后续误编辑的护栏。
func TestGroupModelRateMultipliersMigration(t *testing.T) {
	content, err := FS.ReadFile("910_group_model_rate_multipliers.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")

	// 配置列：幂等新增，NOT NULL + 默认空对象（读侧按"未配置"处理，系数恒为 1）
	require.Contains(t, sql, "ALTER TABLE groups ADD COLUMN IF NOT EXISTS model_rate_multipliers JSONB NOT NULL DEFAULT '{}'::jsonb")

	// DDL 必须带超时保护，避免线上长时间持锁
	require.Contains(t, sql, "SET LOCAL lock_timeout")
	require.Contains(t, sql, "SET LOCAL statement_timeout")
}
