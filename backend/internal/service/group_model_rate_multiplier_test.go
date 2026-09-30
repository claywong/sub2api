//go:build unit

package service

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func rateRules(rules ...GroupModelRateMultiplierRule) GroupModelRateMultipliers {
	return GroupModelRateMultipliers{Enabled: true, Rules: rules}
}

func TestGroupModelRateMultipliersMatchRule(t *testing.T) {
	tests := []struct {
		name  string
		cfg   GroupModelRateMultipliers
		model string
		want  string // 期望命中的规则 match，空串表示未命中
	}{
		{"disabled never matches", GroupModelRateMultipliers{Rules: []GroupModelRateMultiplierRule{{Match: "claude-opus*", Multiplier: 2}}}, "claude-opus-4-6", ""},
		{"empty model", rateRules(GroupModelRateMultiplierRule{Match: "claude-opus*", Multiplier: 2}), " ", ""},
		{"exact", rateRules(GroupModelRateMultiplierRule{Match: "gpt-6-astra", Multiplier: 1.5}), "gpt-6-astra", "gpt-6-astra"},
		{"prefix", rateRules(GroupModelRateMultiplierRule{Match: "claude-opus*", Multiplier: 2}), "claude-opus-4-6", "claude-opus*"},
		{"case insensitive", rateRules(GroupModelRateMultiplierRule{Match: "Claude-Opus*", Multiplier: 2}), "claude-opus-4-6", "Claude-Opus*"},
		{"exact beats prefix", rateRules(
			GroupModelRateMultiplierRule{Match: "claude-opus*", Multiplier: 2},
			GroupModelRateMultiplierRule{Match: "claude-opus-4-6", Multiplier: 3},
		), "claude-opus-4-6", "claude-opus-4-6"},
		{"longest prefix wins", rateRules(
			GroupModelRateMultiplierRule{Match: "claude-opus*", Multiplier: 2},
			GroupModelRateMultiplierRule{Match: "claude-opus-4*", Multiplier: 3},
		), "claude-opus-4-6", "claude-opus-4*"},
		{"no match", rateRules(GroupModelRateMultiplierRule{Match: "claude-opus*", Multiplier: 2}), "claude-sonnet-4", ""},
		{"gemini models/ prefix normalized", rateRules(GroupModelRateMultiplierRule{Match: "gemini-3-pro*", Multiplier: 2}), "models/gemini-3-pro-preview", "gemini-3-pro*"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.MatchRule(tt.model)
			if tt.want == "" {
				require.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			require.Equal(t, tt.want, got.Match)
		})
	}
}

func TestGroupModelRateFactor(t *testing.T) {
	group := &Group{ModelRateMultipliers: rateRules(
		GroupModelRateMultiplierRule{Match: "claude-opus*", Multiplier: 2},
	)}

	require.Equal(t, 2.0, group.ModelRateFactor("claude-opus-4-6"))
	require.Equal(t, defaultModelRateFactor, group.ModelRateFactor("claude-sonnet-4"))
	// 首个候选（composite 公开别名）未命中时回退到实际转发模型
	require.Equal(t, 2.0, group.ModelRateFactor("claude", "", "claude-opus-4-6"))
	// 按候选顺序取第一个命中者
	multi := &Group{ModelRateMultipliers: rateRules(
		GroupModelRateMultiplierRule{Match: "claude", Multiplier: 1.2},
		GroupModelRateMultiplierRule{Match: "claude-opus*", Multiplier: 2},
	)}
	require.Equal(t, 1.2, multi.ModelRateFactor("claude", "claude-opus-4-6"))

	var nilGroup *Group
	require.Equal(t, defaultModelRateFactor, nilGroup.ModelRateFactor("claude-opus-4-6"))
	require.Equal(t, defaultModelRateFactor, modelRateFactorFromAPIKey(nil, "claude-opus-4-6"))
	require.Equal(t, defaultModelRateFactor, modelRateFactorFromAPIKey(&APIKey{}, "claude-opus-4-6"))
}

func TestNormalizeGroupModelRateMultipliers(t *testing.T) {
	invalid := []struct {
		name string
		cfg  GroupModelRateMultipliers
	}{
		{"enabled with empty rules", GroupModelRateMultipliers{Enabled: true}},
		{"empty match", rateRules(GroupModelRateMultiplierRule{Match: "  ", Multiplier: 2})},
		{"bare wildcard", rateRules(GroupModelRateMultiplierRule{Match: "*", Multiplier: 2})},
		{"wildcard in middle", rateRules(GroupModelRateMultiplierRule{Match: "claude-*-opus", Multiplier: 2})},
		{"match too long", rateRules(GroupModelRateMultiplierRule{Match: strings.Repeat("a", modelQuotaMatchMaxLen+1), Multiplier: 2})},
		{"zero multiplier", rateRules(GroupModelRateMultiplierRule{Match: "claude-opus*", Multiplier: 0})},
		{"negative multiplier", rateRules(GroupModelRateMultiplierRule{Match: "claude-opus*", Multiplier: -1})},
		{"NaN multiplier", rateRules(GroupModelRateMultiplierRule{Match: "claude-opus*", Multiplier: math.NaN()})},
		{"Inf multiplier", rateRules(GroupModelRateMultiplierRule{Match: "claude-opus*", Multiplier: math.Inf(1)})},
		{"duplicate after normalization", rateRules(
			GroupModelRateMultiplierRule{Match: "claude-opus*", Multiplier: 2},
			GroupModelRateMultiplierRule{Match: " Claude-Opus* ", Multiplier: 3},
		)},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			_, err := normalizeGroupModelRateMultipliers(tt.cfg)
			require.Error(t, err)
			require.Contains(t, err.Error(), "INVALID_MODEL_RATE_MULTIPLIERS")
		})
	}

	t.Run("empty disabled config passes", func(t *testing.T) {
		out, err := normalizeGroupModelRateMultipliers(GroupModelRateMultipliers{})
		require.NoError(t, err)
		require.False(t, out.Enabled)
		require.Empty(t, out.Rules)
	})

	t.Run("trims match and keeps multiplier", func(t *testing.T) {
		out, err := normalizeGroupModelRateMultipliers(rateRules(
			GroupModelRateMultiplierRule{Match: "  claude-opus*  ", Multiplier: 2.5},
		))
		require.NoError(t, err)
		require.Equal(t, []GroupModelRateMultiplierRule{{Match: "claude-opus*", Multiplier: 2.5}}, out.Rules)
	})

	t.Run("disabled config with rules is kept for later re-enable", func(t *testing.T) {
		out, err := normalizeGroupModelRateMultipliers(GroupModelRateMultipliers{
			Rules: []GroupModelRateMultiplierRule{{Match: "claude-opus*", Multiplier: 2}},
		})
		require.NoError(t, err)
		require.False(t, out.Enabled)
		require.Len(t, out.Rules, 1)
	})
}

func TestApplyGroupModelRuleUpdates(t *testing.T) {
	group := &Group{
		ModelQuotas:          GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{{Match: "gpt-6-astra", Daily: f64(10)}}},
		ModelRateMultipliers: rateRules(GroupModelRateMultiplierRule{Match: "claude-opus*", Multiplier: 2}),
	}

	// nil 表示不修改
	require.NoError(t, applyGroupModelRuleUpdates(group, nil, nil))
	require.Len(t, group.ModelQuotas.Rules, 1)
	require.Len(t, group.ModelRateMultipliers.Rules, 1)

	updated := rateRules(GroupModelRateMultiplierRule{Match: "gpt-6*", Multiplier: 1.5})
	require.NoError(t, applyGroupModelRuleUpdates(group, nil, &updated))
	require.Equal(t, "gpt-6*", group.ModelRateMultipliers.Rules[0].Match)
	require.Len(t, group.ModelQuotas.Rules, 1, "只更新倍率时配额保持不变")

	bad := rateRules(GroupModelRateMultiplierRule{Match: "gpt-6*", Multiplier: 0})
	require.Error(t, applyGroupModelRuleUpdates(group, nil, &bad))
	require.Equal(t, 1.5, group.ModelRateMultipliers.Rules[0].Multiplier, "校验失败不得写入分组")
}

func TestGroupModelRateMultipliersDomainRoundTripAndClone(t *testing.T) {
	src := rateRules(
		GroupModelRateMultiplierRule{Match: "claude-opus*", Multiplier: 2},
		GroupModelRateMultiplierRule{Match: "gpt-6-astra", Multiplier: 1.5},
	)
	require.Equal(t, src, GroupModelRateMultipliersFromDomain(DomainGroupModelRateMultipliers(src)))
	require.Equal(t, GroupModelRateMultipliers{}, GroupModelRateMultipliersFromDomain(DomainGroupModelRateMultipliers(GroupModelRateMultipliers{})))

	cloned := cloneGroupModelRateMultipliers(src)
	require.Equal(t, src, cloned)
	cloned.Rules[0].Multiplier = 9
	require.Equal(t, 2.0, src.Rules[0].Multiplier, "复制出的分组不得与源分组共享规则切片")
}
