//go:build integration

package repository

// 投影漏列回归：认证专用查询 GetByKeyForAuth 的分组显式投影必须携带
// model_rate_multipliers。两个计费入口都直接读 apiKey.Group.ModelRateMultipliers，
// 投影漏列会让系数在真实流量上静默按 1 倍计费，且不报错、无日志。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGetByKeyForAuthCarriesModelRateMultipliersProjection(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	group := mustCreateGroup(t, integrationEntClient, &service.Group{
		Name:     fmt.Sprintf("model-rate-proj-group-%d", suffix),
		Platform: service.PlatformComposite,
		ModelRateMultipliers: service.GroupModelRateMultipliers{
			Enabled: true,
			Rules: []service.GroupModelRateMultiplierRule{
				{Match: "claude-opus*", Multiplier: 2},
				{Match: "gpt-6-astra", Multiplier: 1.25},
			},
		},
	})
	user := mustCreateUser(t, integrationEntClient, &service.User{
		Email: fmt.Sprintf("model-rate-proj-%d@example.com", suffix), Concurrency: 5,
	})
	groupID := group.ID
	keyValue := fmt.Sprintf("sk-model-rate-proj-%d", suffix)
	apiKeyRepo := NewAPIKeyRepository(integrationEntClient, integrationDB)
	key := &service.APIKey{
		UserID: user.ID, GroupID: &groupID, Key: keyValue,
		Name: "model-rate-proj", Status: service.StatusActive,
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
	require.Equal(t, service.GroupModelRateMultipliers{
		Enabled: true,
		Rules: []service.GroupModelRateMultiplierRule{
			{Match: "claude-opus*", Multiplier: 2},
			{Match: "gpt-6-astra", Multiplier: 1.25},
		},
	}, got.Group.ModelRateMultipliers, "model_rate_multipliers 必须进入认证投影（漏列会让系数静默按 1 倍计费）")

	// 端到端确认：投影带出的配置能直接驱动计费系数
	require.Equal(t, 2.0, got.Group.ModelRateFactor("claude-opus-4-6"))
	require.Equal(t, 1.0, got.Group.ModelRateFactor("claude-sonnet-4"))
}
