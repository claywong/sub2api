//go:build integration

package repository

// 投影漏列回归：认证专用查询 GetByKeyForAuth 的分组显式投影必须携带
// model_quotas。该查询是认证快照的唯一数据来源，而按模型配额的判定
// （checkGroupModelQuotaEligibility）与记账（resolveModelQuotaRuleKey）都直接读
// apiKey.Group.ModelQuotas——投影漏列会让配额在真实流量上静默放行，且不报错、
// 无日志，是最难发现的失效模式。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGetByKeyForAuthCarriesModelQuotasProjection(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	daily := 50.0
	weekly := 200.0
	exactDaily := 10.0
	group := mustCreateGroup(t, integrationEntClient, &service.Group{
		Name:     fmt.Sprintf("model-quota-proj-group-%d", suffix),
		Platform: service.PlatformOpenAI,
		ModelQuotas: service.GroupModelQuotas{
			Enabled: true,
			Rules: []service.GroupModelQuotaRule{
				{Match: "claude-opus*", Daily: &daily, Weekly: &weekly},
				{Match: "gpt-6-astra", Daily: &exactDaily},
			},
		},
	})
	user := mustCreateUser(t, integrationEntClient, &service.User{
		Email: fmt.Sprintf("model-quota-proj-%d@example.com", suffix), Concurrency: 5,
	})
	groupID := group.ID
	keyValue := fmt.Sprintf("sk-model-quota-proj-%d", suffix)
	apiKeyRepo := NewAPIKeyRepository(integrationEntClient, integrationDB)
	key := &service.APIKey{
		UserID: user.ID, GroupID: &groupID, Key: keyValue,
		Name: "model-quota-proj", Status: service.StatusActive,
	}
	require.NoError(t, apiKeyRepo.Create(ctx, key))
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(ctx, "DELETE FROM auth_cache_invalidation_outbox WHERE cache_key = encode(sha256(convert_to($1, 'UTF8')), 'hex')", keyValue)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM api_keys WHERE id = $1", key.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id = $1", user.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM groups WHERE id = $1", group.ID)
		require.NoError(t, err)
	})

	got, err := apiKeyRepo.GetByKeyForAuth(ctx, keyValue)
	require.NoError(t, err)
	require.NotNil(t, got.Group, "认证查询必须带出分组")

	require.True(t, got.Group.ModelQuotas.Enabled,
		"model_quotas 必须进入认证投影（投影漏列会让按模型配额静默放行）")
	require.Len(t, got.Group.ModelQuotas.Rules, 2)

	// 上限是 *float64，必须逐个确认没有在 JSON 往返中丢成 nil——
	// nil 的语义是"该窗口不限制"，丢失后配额同样静默失效。
	prefixRule := got.Group.ModelQuotas.Rules[0]
	require.Equal(t, "claude-opus*", prefixRule.Match)
	require.NotNil(t, prefixRule.Daily)
	require.InDelta(t, 50.0, *prefixRule.Daily, 1e-9)
	require.NotNil(t, prefixRule.Weekly)
	require.InDelta(t, 200.0, *prefixRule.Weekly, 1e-9)
	require.Nil(t, prefixRule.Monthly, "未配置的窗口必须保持 nil（表示不限制）")

	exactRule := got.Group.ModelQuotas.Rules[1]
	require.Equal(t, "gpt-6-astra", exactRule.Match)
	require.NotNil(t, exactRule.Daily)
	require.InDelta(t, 10.0, *exactRule.Daily, 1e-9)

	// 端到端确认：投影带出的配置能直接驱动规则匹配
	require.True(t, got.Group.ModelQuotasEnabled())
	matched := got.Group.ModelQuotas.MatchRule("claude-opus-4-6")
	require.NotNil(t, matched, "投影出的配置必须能匹配到规则")
	require.Equal(t, "claude-opus*", matched.Match)
}

// TestGetByKeyForAuthKeepsZeroModelQuotaLimit 单独覆盖"限额为 0"这一语义：
// 0 表示显式禁用该模型，若 JSON 往返把 0 丢成 nil 就会变成"不限制"，
// 是最危险的一种失真（禁用变放行）。
func TestGetByKeyForAuthKeepsZeroModelQuotaLimit(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	zero := 0.0
	group := mustCreateGroup(t, integrationEntClient, &service.Group{
		Name:     fmt.Sprintf("model-quota-zero-group-%d", suffix),
		Platform: service.PlatformOpenAI,
		ModelQuotas: service.GroupModelQuotas{
			Enabled: true,
			Rules:   []service.GroupModelQuotaRule{{Match: "gpt-6-astra", Daily: &zero}},
		},
	})
	user := mustCreateUser(t, integrationEntClient, &service.User{
		Email: fmt.Sprintf("model-quota-zero-%d@example.com", suffix), Concurrency: 5,
	})
	groupID := group.ID
	keyValue := fmt.Sprintf("sk-model-quota-zero-%d", suffix)
	apiKeyRepo := NewAPIKeyRepository(integrationEntClient, integrationDB)
	key := &service.APIKey{
		UserID: user.ID, GroupID: &groupID, Key: keyValue,
		Name: "model-quota-zero", Status: service.StatusActive,
	}
	require.NoError(t, apiKeyRepo.Create(ctx, key))
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(ctx, "DELETE FROM auth_cache_invalidation_outbox WHERE cache_key = encode(sha256(convert_to($1, 'UTF8')), 'hex')", keyValue)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM api_keys WHERE id = $1", key.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id = $1", user.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM groups WHERE id = $1", group.ID)
		require.NoError(t, err)
	})

	got, err := apiKeyRepo.GetByKeyForAuth(ctx, keyValue)
	require.NoError(t, err)
	require.NotNil(t, got.Group)
	require.Len(t, got.Group.ModelQuotas.Rules, 1)

	rule := got.Group.ModelQuotas.Rules[0]
	require.NotNil(t, rule.Daily, "限额 0 不能在往返中变成 nil，否则禁用会变成不限制")
	require.Zero(t, *rule.Daily)
}
