package service

// 投影漏列回归（service 半程）：认证快照 build → L2 JSON 序列化 → 反序列化
// → 还原 apiKey.Group → 按模型配额判定/记账，全链路保真。
// repository 半程（真实 GetByKeyForAuth 投影）见
// internal/repository/api_key_repo_model_quota_projection_integration_test.go。

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func modelQuotaAuthTestAPIKey() *APIKey {
	groupID := int64(51)
	daily := 50.0
	weekly := 200.0
	zero := 0.0
	return &APIKey{
		ID:      83,
		UserID:  41,
		GroupID: &groupID,
		Key:     "sk-model-quota-roundtrip",
		Name:    "model-quota-auth-roundtrip",
		Status:  StatusActive,
		User: &User{
			ID:          41,
			Email:       "model-quota@test.local",
			Status:      StatusActive,
			Concurrency: 5,
		},
		Group: &Group{
			ID:               groupID,
			Name:             "quota-roundtrip",
			Platform:         PlatformComposite,
			Status:           StatusActive,
			Hydrated:         true,
			RateMultiplier:   1,
			SubscriptionType: SubscriptionTypeStandard,
			ModelQuotas: GroupModelQuotas{
				Enabled: true,
				Rules: []GroupModelQuotaRule{
					{Match: "claude-opus*", Daily: &daily, Weekly: &weekly},
					{Match: "gpt-6-astra", Daily: &zero},
				},
			},
		},
	}
}

// 快照构建 → L2 JSON 往返 → 还原：配额配置必须全程保真。
func TestAPIKeyAuthSnapshotModelQuotasRoundtrip(t *testing.T) {
	svc := &APIKeyService{}
	apiKey := modelQuotaAuthTestAPIKey()

	snapshot := svc.snapshotFromAPIKey(context.Background(), apiKey)
	require.NotNil(t, snapshot)
	require.Equal(t, apiKeyAuthSnapshotVersion, snapshot.Version)

	payload, err := json.Marshal(&APIKeyAuthCacheEntry{Snapshot: snapshot})
	require.NoError(t, err)
	var restored APIKeyAuthCacheEntry
	require.NoError(t, json.Unmarshal(payload, &restored))

	materialized, used, err := svc.applyAuthCacheEntry(apiKey.Key, &restored)
	require.NoError(t, err)
	require.True(t, used)
	require.NotNil(t, materialized.Group)

	quotas := materialized.Group.ModelQuotas
	require.True(t, quotas.Enabled, "model_quotas 必须在快照往返后保真（漏放会让配额静默放行）")
	require.Len(t, quotas.Rules, 2)

	require.Equal(t, "claude-opus*", quotas.Rules[0].Match)
	require.NotNil(t, quotas.Rules[0].Daily)
	require.InDelta(t, 50.0, *quotas.Rules[0].Daily, 1e-12)
	require.NotNil(t, quotas.Rules[0].Weekly)
	require.InDelta(t, 200.0, *quotas.Rules[0].Weekly, 1e-12)
	require.Nil(t, quotas.Rules[0].Monthly, "未配置窗口须保持 nil（表示不限制）")

	// 限额 0 是"显式禁用"，丢成 nil 会反转为"不限制"——最危险的失真。
	require.NotNil(t, quotas.Rules[1].Daily, "限额 0 不能在往返中丢成 nil")
	require.Zero(t, *quotas.Rules[1].Daily)

	// 端到端：还原后的分组能直接驱动判定与记账，且两侧算出同一规则键
	require.True(t, materialized.Group.ModelQuotasEnabled())
	matched := quotas.MatchRule("claude-opus-4-6")
	require.NotNil(t, matched, "还原后的配置必须能匹配规则（漏放时本断言最先失败）")
	require.Equal(t, "claude-opus*", matched.Match)
	require.Equal(t, "claude-opus*", resolveModelQuotaRuleKey(materialized, "claude-opus-4-6"))
}

// 旧版本快照（v24 及更早，无 model_quotas 字段）必须被淘汰回源，
// 否则配置了配额的分组会在缓存过期前静默放行。
func TestAPIKeyAuthSnapshotModelQuotasOldVersionEvicted(t *testing.T) {
	svc := &APIKeyService{}
	snapshot := svc.snapshotFromAPIKey(context.Background(), modelQuotaAuthTestAPIKey())
	require.NotNil(t, snapshot)
	snapshot.Version = 24

	materialized, used, err := svc.applyAuthCacheEntry("sk-old-quota", &APIKeyAuthCacheEntry{Snapshot: snapshot})
	require.NoError(t, err)
	require.False(t, used, "v24 快照没有 model_quotas，必须淘汰并回源重建")
	require.Nil(t, materialized)
}
