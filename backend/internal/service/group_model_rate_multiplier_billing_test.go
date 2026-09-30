//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 单模型倍率系数的计费回归：两个计费入口都必须把系数折进使用记录的 rate_multiplier，
// 且 actual_cost = total_cost × rate_multiplier（使用页口径）。

func opusRateGroup(groupID int64, groupRate float64) *Group {
	return &Group{
		ID:             groupID,
		Platform:       PlatformComposite,
		RateMultiplier: groupRate,
		ModelRateMultipliers: GroupModelRateMultipliers{
			Enabled: true,
			Rules:   []GroupModelRateMultiplierRule{{Match: "claude-opus*", Multiplier: 2}},
		},
	}
}

func recordGatewayUsageForRateFactor(t *testing.T, svc *GatewayService, group *Group, requested, forwarded string) *UsageLog {
	t.Helper()
	usageRepo := svc.usageLogRepo.(*openAIRecordUsageLogRepoStub)
	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "rate_factor_" + requested + "_" + forwarded,
			Usage:     ClaudeUsage{InputTokens: 1000, OutputTokens: 500},
			Model:     forwarded,
			Duration:  time.Second,
		},
		APIKey:         &APIKey{ID: 901, GroupID: i64p(group.ID), Group: group},
		User:           &User{ID: 902},
		Account:        &Account{ID: 903, Platform: PlatformAnthropic},
		RequestedModel: requested,
		PricingAt:      time.Date(2026, time.June, 29, 15, 30, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	return usageRepo.lastLog
}

func requireActualIsTotalTimesRate(t *testing.T, log *UsageLog, wantRate float64) {
	t.Helper()
	require.InDelta(t, wantRate, log.RateMultiplier, 1e-12)
	require.Greater(t, log.TotalCost, 0.0, "total_cost 必须保持官方价（>0）")
	require.InDelta(t, log.TotalCost*wantRate, log.ActualCost, 1e-12)
}

func TestGatewayRecordUsage_ModelRateFactor(t *testing.T) {
	newSvc := func(rateRepo UserGroupRateRepository) *GatewayService {
		svc := newGatewayRecordUsageServiceForTest(&openAIRecordUsageLogRepoStub{inserted: true}, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
		svc.userGroupRateResolver = newUserGroupRateResolver(rateRepo, nil, resolveUserGroupRateCacheTTL(svc.cfg), nil, "service.gateway.test")
		return svc
	}

	t.Run("matched model multiplies group rate", func(t *testing.T) {
		log := recordGatewayUsageForRateFactor(t, newSvc(nil), opusRateGroup(910, 1.5), "claude-opus-4-6", "claude-opus-4-6")
		requireActualIsTotalTimesRate(t, log, 3.0)
	})

	t.Run("unmatched model keeps group rate", func(t *testing.T) {
		log := recordGatewayUsageForRateFactor(t, newSvc(nil), opusRateGroup(911, 1.5), "claude-sonnet-4", "claude-sonnet-4")
		requireActualIsTotalTimesRate(t, log, 1.5)
	})

	t.Run("user specific rate multiplies factor", func(t *testing.T) {
		userRate := 1.2
		rateRepo := &openAIUserGroupRateRepoStub{rate: &userRate}
		log := recordGatewayUsageForRateFactor(t, newSvc(rateRepo), opusRateGroup(912, 1.5), "claude-opus-4-6", "claude-opus-4-6")
		require.Equal(t, 1, rateRepo.calls)
		requireActualIsTotalTimesRate(t, log, 2.4)
	})

	t.Run("composite alias falls back to forwarded model", func(t *testing.T) {
		log := recordGatewayUsageForRateFactor(t, newSvc(nil), opusRateGroup(913, 1.5), "claude", "claude-opus-4-6")
		requireActualIsTotalTimesRate(t, log, 3.0)
	})

	t.Run("peak factor stacks on top", func(t *testing.T) {
		group := opusRateGroup(914, 1.5)
		group.SubscriptionType = SubscriptionTypeSubscription
		group.PeakRateEnabled, group.PeakStart, group.PeakEnd, group.PeakRateMultiplier = true, "14:00", "18:00", 1.5
		log := recordGatewayUsageForRateFactor(t, newSvc(nil), group, "claude-opus-4-6", "claude-opus-4-6")
		requireActualIsTotalTimesRate(t, log, 4.5)
	})

	t.Run("disabled config keeps group rate", func(t *testing.T) {
		group := opusRateGroup(915, 1.5)
		group.ModelRateMultipliers.Enabled = false
		log := recordGatewayUsageForRateFactor(t, newSvc(nil), group, "claude-opus-4-6", "claude-opus-4-6")
		requireActualIsTotalTimesRate(t, log, 1.5)
	})
}

func TestOpenAIRecordUsage_ModelRateFactor(t *testing.T) {
	userRate := 1.8
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	rateRepo := &openAIUserGroupRateRepoStub{rate: &userRate}
	svc := newOpenAIRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, rateRepo)
	group := &Group{
		ID:             920,
		RateMultiplier: 1.4,
		ModelRateMultipliers: GroupModelRateMultipliers{
			Enabled: true,
			Rules:   []GroupModelRateMultiplierRule{{Match: "gpt-5.1", Multiplier: 2}},
		},
	}

	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{
			RequestID: "resp_model_rate_factor",
			Usage:     OpenAIUsage{InputTokens: 1000, OutputTokens: 500},
			Model:     "gpt-5.1",
			Duration:  time.Second,
		},
		APIKey:         &APIKey{ID: 921, GroupID: i64p(group.ID), Group: group},
		User:           &User{ID: 922},
		Account:        &Account{ID: 923},
		RequestedModel: "gpt-5.1",
	})

	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	requireActualIsTotalTimesRate(t, usageRepo.lastLog, userRate*2)
}

func TestModelRateFactorKeepsIndependentImageRate(t *testing.T) {
	group := opusRateGroup(930, 1.5)
	group.ImageRateIndependent = true
	group.ImageRateMultiplier = 0.5
	apiKey := &APIKey{Group: group}

	base := group.RateMultiplier * modelRateFactorFromAPIKey(apiKey, "claude-opus-4-6")
	text, image := computePeakAwareMultipliers(apiKey, base, time.Now())
	require.InDelta(t, 3.0, text, 1e-12)
	require.InDelta(t, 0.5, image, 1e-12, "开启独立图片倍率时不得乘模型系数")

	group.ImageRateIndependent = false
	_, image = computePeakAwareMultipliers(apiKey, base, time.Now())
	require.InDelta(t, 3.0, image, 1e-12, "未开启独立图片倍率时继承基础倍率 × 系数")
}

// 模型广场按模型下发系数：命中规则的模型带系数，未命中的保持零值（展示按 1）。
func TestListPlazaGroups_ModelRateFactor(t *testing.T) {
	ch := plazaPricedChannel(1, "ch", []int64{10}, PlatformAnthropic, "claude-opus-4-6", "claude-sonnet-4")
	group := *opusRateGroup(10, 1.5)
	group.Platform = PlatformAnthropic

	out, err := newPlazaService([]Channel{ch}, []Group{group}, nil).ListGroups(context.Background())

	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Len(t, out[0].Models, 2)
	require.Equal(t, "claude-opus-4-6", out[0].Models[0].Name)
	require.Equal(t, 2.0, out[0].Models[0].RateFactor)
	require.Zero(t, out[0].Models[1].RateFactor, "未命中规则的模型不下发系数")
}

// 快照构建 → L2 JSON 往返 → 还原：系数配置必须全程保真，旧版本快照必须淘汰。
func TestAPIKeyAuthSnapshotModelRateMultipliersRoundtrip(t *testing.T) {
	svc := &APIKeyService{}
	apiKey := modelQuotaAuthTestAPIKey()
	apiKey.Group.ModelRateMultipliers = GroupModelRateMultipliers{
		Enabled: true,
		Rules:   []GroupModelRateMultiplierRule{{Match: "claude-opus*", Multiplier: 2}},
	}

	snapshot := svc.snapshotFromAPIKey(context.Background(), apiKey)
	require.NotNil(t, snapshot)
	payload, err := json.Marshal(&APIKeyAuthCacheEntry{Snapshot: snapshot})
	require.NoError(t, err)
	var restored APIKeyAuthCacheEntry
	require.NoError(t, json.Unmarshal(payload, &restored))

	materialized, used, err := svc.applyAuthCacheEntry(apiKey.Key, &restored)
	require.NoError(t, err)
	require.True(t, used)
	require.Equal(t, apiKey.Group.ModelRateMultipliers, materialized.Group.ModelRateMultipliers)
	require.Equal(t, 2.0, materialized.Group.ModelRateFactor("claude-opus-4-6"))

	snapshot.Version = 25
	stale, used, err := svc.applyAuthCacheEntry("sk-old-rate", &APIKeyAuthCacheEntry{Snapshot: snapshot})
	require.NoError(t, err)
	require.False(t, used, "v25 快照没有 model_rate_multipliers，必须淘汰并回源重建")
	require.Nil(t, stale)
}
