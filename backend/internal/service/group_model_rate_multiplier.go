package service

import (
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// defaultModelRateFactor 是未命中任何规则时的模型倍率系数（不改变既有倍率）。
const defaultModelRateFactor = 1.0

// GroupModelRateMultiplierRule 是 service 层的单模型倍率系数规则（字段与 domain 类型一致，
// ent 持久化用 domain 类型，边界处显式转换）。
type GroupModelRateMultiplierRule struct {
	Match      string  `json:"match"`
	Multiplier float64 `json:"multiplier"`
}

// GroupModelRateMultipliers 是 service 层的分组单模型倍率系数配置（私有扩展）。
//
// 计费时 rate_multiplier = (用户专属倍率 ?? 分组倍率) × 命中规则的系数 × 高峰因子，
// 系数只折进最终倍率写入使用记录，不单独落库。详见 docs/group-model-rate-multiplier.md。
type GroupModelRateMultipliers struct {
	Enabled bool                           `json:"enabled"`
	Rules   []GroupModelRateMultiplierRule `json:"rules,omitempty"`
}

// DomainGroupModelRateMultipliers 把 service 配置转换为 ent 持久化使用的 domain 类型。
func DomainGroupModelRateMultipliers(cfg GroupModelRateMultipliers) domain.GroupModelRateMultipliers {
	out := domain.GroupModelRateMultipliers{Enabled: cfg.Enabled}
	if len(cfg.Rules) == 0 {
		return out
	}
	out.Rules = make([]domain.GroupModelRateMultiplierRule, 0, len(cfg.Rules))
	for _, rule := range cfg.Rules {
		out.Rules = append(out.Rules, domain.GroupModelRateMultiplierRule{Match: rule.Match, Multiplier: rule.Multiplier})
	}
	return out
}

// GroupModelRateMultipliersFromDomain 把 ent 读出的 domain 配置转换为 service 类型。
func GroupModelRateMultipliersFromDomain(cfg domain.GroupModelRateMultipliers) GroupModelRateMultipliers {
	out := GroupModelRateMultipliers{Enabled: cfg.Enabled}
	if len(cfg.Rules) == 0 {
		return out
	}
	out.Rules = make([]GroupModelRateMultiplierRule, 0, len(cfg.Rules))
	for _, rule := range cfg.Rules {
		out.Rules = append(out.Rules, GroupModelRateMultiplierRule{Match: rule.Match, Multiplier: rule.Multiplier})
	}
	return out
}

// cloneGroupModelRateMultipliers 复制配置；规则是值类型，复制切片即可与源分组隔离。
func cloneGroupModelRateMultipliers(value GroupModelRateMultipliers) GroupModelRateMultipliers {
	return GroupModelRateMultipliers{
		Enabled: value.Enabled,
		Rules:   append([]GroupModelRateMultiplierRule(nil), value.Rules...),
	}
}

// MatchRule 返回模型命中的唯一规则，未命中返回 nil。
// 命中语义与 GroupModelQuotas.MatchRule 完全一致（见 bestModelRuleIndex）。
func (m GroupModelRateMultipliers) MatchRule(model string) *GroupModelRateMultiplierRule {
	if !m.Enabled || len(m.Rules) == 0 {
		return nil
	}
	idx := bestModelRuleIndex(model, len(m.Rules), func(i int) string {
		return ModelQuotaRuleKey(m.Rules[i].Match)
	})
	if idx < 0 {
		return nil
	}
	return &m.Rules[idx]
}

// ModelRateFactor 返回本次请求的模型倍率系数，未启用或未命中时为 defaultModelRateFactor。
//
// models 按优先级传入候选模型名（请求模型 → 渠道原始模型 → 实际转发模型），
// 取第一个命中规则的候选：composite 公开别名（如 claude）未配规则时，
// 仍能按实际转发的 claude-opus-4-6 命中 "claude-opus*"。
func (g *Group) ModelRateFactor(models ...string) float64 {
	if g == nil {
		return defaultModelRateFactor
	}
	for _, model := range models {
		if rule := g.ModelRateMultipliers.MatchRule(model); rule != nil {
			return rule.Multiplier
		}
	}
	return defaultModelRateFactor
}

// modelRateFactorFromAPIKey 是计费入口的便捷封装：从认证快照里的分组读取系数。
func modelRateFactorFromAPIKey(apiKey *APIKey, models ...string) float64 {
	if apiKey == nil {
		return defaultModelRateFactor
	}
	return apiKey.Group.ModelRateFactor(models...)
}

// normalizeGroupModelRateMultipliers 归一化管理端提交的单模型倍率系数配置。
//
// 校验规则（配置错误返回 400，而不是运行时静默按 1 倍计费）：
//   - match 规则与按模型配额一致（见 validateModelRuleMatch），按归一键去重；
//   - multiplier 必须是有限数且 > 0：0 会让模型免费，禁用模型应改用模型白名单或按模型配额；
//   - enabled=true 但规则列表为空返回 400。
func normalizeGroupModelRateMultipliers(cfg GroupModelRateMultipliers) (GroupModelRateMultipliers, error) {
	out := GroupModelRateMultipliers{Enabled: cfg.Enabled}
	if len(cfg.Rules) == 0 {
		if out.Enabled {
			return out, invalidModelRateMultiplier("model rate multipliers cannot be enabled with an empty rule list")
		}
		return out, nil
	}

	seen := make(map[string]struct{}, len(cfg.Rules))
	out.Rules = make([]GroupModelRateMultiplierRule, 0, len(cfg.Rules))
	for _, rule := range cfg.Rules {
		normalized, err := normalizeModelRateMultiplierRule(rule)
		if err != nil {
			return out, err
		}
		key := ModelQuotaRuleKey(normalized.Match)
		if _, ok := seen[key]; ok {
			return out, invalidModelRateMultiplier("duplicate model rate multiplier rule: " + normalized.Match)
		}
		seen[key] = struct{}{}
		out.Rules = append(out.Rules, normalized)
	}
	return out, nil
}

func normalizeModelRateMultiplierRule(rule GroupModelRateMultiplierRule) (GroupModelRateMultiplierRule, error) {
	match := strings.TrimSpace(rule.Match)
	if msg := validateModelRuleMatch(match, "model rate multiplier", "adjust the group rate multiplier instead"); msg != "" {
		return rule, invalidModelRateMultiplier(msg)
	}
	if math.IsNaN(rule.Multiplier) || math.IsInf(rule.Multiplier, 0) || rule.Multiplier <= 0 {
		return rule, invalidModelRateMultiplier(fmt.Sprintf("model rate multiplier for %q must be a finite number > 0", match))
	}
	return GroupModelRateMultiplierRule{Match: match, Multiplier: rule.Multiplier}, nil
}

// normalizeGroupModelRules 归一化创建分组时提交的按模型规则（配额 + 倍率系数）。
// 私有扩展集中在此处，避免 upstream 的 CreateGroup 继续膨胀、merge 时冲突。
func normalizeGroupModelRules(quotas GroupModelQuotas, rates GroupModelRateMultipliers) (GroupModelQuotas, GroupModelRateMultipliers, error) {
	normalizedQuotas, err := normalizeGroupModelQuotas(quotas)
	if err != nil {
		return GroupModelQuotas{}, GroupModelRateMultipliers{}, err
	}
	normalizedRates, err := normalizeGroupModelRateMultipliers(rates)
	if err != nil {
		return GroupModelQuotas{}, GroupModelRateMultipliers{}, err
	}
	return normalizedQuotas, normalizedRates, nil
}

// applyGroupModelRuleUpdates 把更新请求里的按模型规则写入分组；nil 表示该项不修改。
func applyGroupModelRuleUpdates(group *Group, quotas *GroupModelQuotas, rates *GroupModelRateMultipliers) error {
	if quotas != nil {
		normalized, err := normalizeGroupModelQuotas(*quotas)
		if err != nil {
			return err
		}
		group.ModelQuotas = normalized
	}
	if rates != nil {
		normalized, err := normalizeGroupModelRateMultipliers(*rates)
		if err != nil {
			return err
		}
		group.ModelRateMultipliers = normalized
	}
	return nil
}

func invalidModelRateMultiplier(message string) error {
	return infraerrors.New(http.StatusBadRequest, "INVALID_MODEL_RATE_MULTIPLIERS", message)
}
