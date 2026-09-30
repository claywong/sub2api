package service

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// modelQuotaMatchMaxLen 是规则 match（即 rule_key）的长度上限，
// 与 user_group_model_usage.rule_key 的 VARCHAR(200) 对齐。
const modelQuotaMatchMaxLen = 200

// GroupModelQuotaRule 是 service 层的模型配额规则（字段与 domain 类型一致，
// ent 持久化用 domain 类型，边界处显式转换）。
type GroupModelQuotaRule struct {
	Match   string   `json:"match"`
	Daily   *float64 `json:"daily,omitempty"`
	Weekly  *float64 `json:"weekly,omitempty"`
	Monthly *float64 `json:"monthly,omitempty"`
}

// GroupModelQuotas 是 service 层的分组模型配额配置。
type GroupModelQuotas struct {
	Enabled bool                  `json:"enabled"`
	Rules   []GroupModelQuotaRule `json:"rules,omitempty"`
}

// DomainGroupModelQuotas 把 service 配置转换为 ent 持久化使用的 domain 类型。
func DomainGroupModelQuotas(cfg GroupModelQuotas) domain.GroupModelQuotas {
	out := domain.GroupModelQuotas{Enabled: cfg.Enabled}
	if len(cfg.Rules) == 0 {
		return out
	}
	out.Rules = make([]domain.GroupModelQuotaRule, 0, len(cfg.Rules))
	for _, rule := range cfg.Rules {
		out.Rules = append(out.Rules, domain.GroupModelQuotaRule{
			Match:   rule.Match,
			Daily:   rule.Daily,
			Weekly:  rule.Weekly,
			Monthly: rule.Monthly,
		})
	}
	return out
}

// GroupModelQuotasFromDomain 把 ent 读出的 domain 配置转换为 service 类型。
func GroupModelQuotasFromDomain(cfg domain.GroupModelQuotas) GroupModelQuotas {
	out := GroupModelQuotas{Enabled: cfg.Enabled}
	if len(cfg.Rules) == 0 {
		return out
	}
	out.Rules = make([]GroupModelQuotaRule, 0, len(cfg.Rules))
	for _, rule := range cfg.Rules {
		out.Rules = append(out.Rules, GroupModelQuotaRule{
			Match:   rule.Match,
			Daily:   rule.Daily,
			Weekly:  rule.Weekly,
			Monthly: rule.Monthly,
		})
	}
	return out
}

// ModelQuotaRuleKey 返回规则用于用量记账的键：规则原文归一（TrimSpace + 小写）。
//
// 用规则原文而非具体模型名做键，是「系列共享额度」语义的关键：
// claude-opus-4-5 与 claude-opus-4-6 都命中 "claude-opus*" 时记到同一行。
func ModelQuotaRuleKey(match string) string {
	return strings.ToLower(strings.TrimSpace(match))
}

// ModelQuotasEnabled 报告该分组是否启用了按模型配额。
func (g *Group) ModelQuotasEnabled() bool {
	return g != nil && g.ModelQuotas.Enabled && len(g.ModelQuotas.Rules) > 0
}

// HasLimit 报告该规则是否至少配置了一个窗口上限。
// 三个窗口全为 nil 的规则不产生任何约束，判定时直接跳过。
func (r GroupModelQuotaRule) HasLimit() bool {
	return r.Daily != nil || r.Weekly != nil || r.Monthly != nil
}

// MatchRule 返回请求模型命中的唯一规则，未命中返回 nil。
//
// 命中语义（与 composite 路由 resolver、model_pricing resolver 的既有优先级一致）：
//  1. exact 规则优先于 prefix 规则；
//  2. 多条 prefix 规则同时命中时，前缀最长者优先（"claude-opus-4*" 胜过 "claude-opus*"）；
//  3. 同强度下先声明者优先（保持配置可预测）。
//
// 模型名的候选形式复用白名单的归一规则（Gemini models/ 前缀、-thinking 后缀、
// OpenAI 推理后缀），避免改写模型名前缀即可绕开配额。
//
// 三窗口全空的规则视为无约束，不参与命中——否则它会「吃掉」本应由更宽泛规则
// 承接的请求，导致宽泛规则的额度永远不被计量。
func (q GroupModelQuotas) MatchRule(model string) *GroupModelQuotaRule {
	if !q.Enabled || len(q.Rules) == 0 {
		return nil
	}
	idx := bestModelRuleIndex(model, len(q.Rules), func(i int) string {
		if !q.Rules[i].HasLimit() {
			return ""
		}
		return ModelQuotaRuleKey(q.Rules[i].Match)
	})
	if idx < 0 {
		return nil
	}
	return &q.Rules[idx]
}

// bestModelRuleIndex 返回 model 命中的最具体规则下标，未命中返回 -1。
// 按模型配额与单模型倍率共用这一套命中语义：
//  1. exact 规则优先于 prefix 规则；
//  2. 多条 prefix 同时命中时，前缀最长者优先；
//  3. 同强度下先声明者优先。
//
// entryAt 返回第 i 条规则的归一化 match；返回空串表示该规则不参与命中。
func bestModelRuleIndex(model string, n int, entryAt func(i int) string) int {
	if strings.TrimSpace(model) == "" {
		return -1
	}
	candidates := groupModelAllowlistCandidates(model)
	bestExact, bestPrefix, bestLen := -1, -1, -1
	for i := 0; i < n; i++ {
		entry := entryAt(i)
		if entry == "" {
			continue
		}
		if strings.HasSuffix(entry, "*") {
			prefix := strings.TrimSuffix(entry, "*")
			// 最长前缀优先；等长时保留先声明者
			if matchesAnyCandidatePrefix(candidates, prefix) && len(prefix) > bestLen {
				bestPrefix, bestLen = i, len(prefix)
			}
			continue
		}
		if bestExact < 0 && containsCandidate(candidates, entry) {
			bestExact = i
		}
	}
	if bestExact >= 0 {
		return bestExact
	}
	return bestPrefix
}

func matchesAnyCandidatePrefix(candidates []string, prefix string) bool {
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate, prefix) {
			return true
		}
	}
	return false
}

func containsCandidate(candidates []string, entry string) bool {
	for _, candidate := range candidates {
		if candidate == entry {
			return true
		}
	}
	return false
}

// normalizeGroupModelQuotas 归一化管理端提交的模型配额配置。
//
// 校验规则（与 normalizeGroupModelAllowlist 的风格一致，配置错误返回 400
// 而不是运行时静默放行）：
//   - match 必填，`*` 只允许出现在末尾，裸 "*" 不允许（等价于总配额，应改用分组限额）；
//   - match 按归一键去重，重复声明返回 400（避免两条规则记到同一行导致语义歧义）；
//   - 各窗口上限须 >= 0，负数返回 400；
//   - enabled=true 但规则列表为空返回 400。
func normalizeGroupModelQuotas(cfg GroupModelQuotas) (GroupModelQuotas, error) {
	out := GroupModelQuotas{Enabled: cfg.Enabled}
	if len(cfg.Rules) == 0 {
		if out.Enabled {
			return out, invalidModelQuota("model quotas cannot be enabled with an empty rule list")
		}
		return out, nil
	}

	seen := make(map[string]struct{}, len(cfg.Rules))
	out.Rules = make([]GroupModelQuotaRule, 0, len(cfg.Rules))
	for _, rule := range cfg.Rules {
		normalized, err := normalizeModelQuotaRule(rule)
		if err != nil {
			return out, err
		}
		key := ModelQuotaRuleKey(normalized.Match)
		if _, ok := seen[key]; ok {
			return out, invalidModelQuota("duplicate model quota rule: " + normalized.Match)
		}
		seen[key] = struct{}{}
		out.Rules = append(out.Rules, normalized)
	}
	if len(out.Rules) == 0 {
		if out.Enabled {
			return out, invalidModelQuota("model quotas cannot be enabled with an empty rule list")
		}
		out.Rules = nil
	}
	return out, nil
}

// validateModelRuleMatch 校验按模型规则的 match（已 TrimSpace），合法返回空串。
// subject 用于错误文案（如 "model quota"），bareWildcardHint 提示裸 "*" 的替代做法。
//
// 长度上限：rule_key 落库列为 VARCHAR(200)，超长规则能存进 jsonb 配置，但每次落账
// 都会因 rule_key 溢出让整个计费事务失败，必须在配置入口拦截；单模型倍率沿用同一上限。
func validateModelRuleMatch(match, subject, bareWildcardHint string) string {
	if match == "" {
		return subject + " rule match cannot be empty"
	}
	if strings.Contains(strings.TrimSuffix(match, "*"), "*") {
		return fmt.Sprintf(`wildcard "*" is only allowed at the end of a %s rule`, subject)
	}
	if match == "*" {
		return fmt.Sprintf(`%s rule "*" is not allowed; %s`, subject, bareWildcardHint)
	}
	if len(match) > modelQuotaMatchMaxLen {
		return fmt.Sprintf("%s rule match is longer than %d characters", subject, modelQuotaMatchMaxLen)
	}
	return ""
}

func normalizeModelQuotaRule(rule GroupModelQuotaRule) (GroupModelQuotaRule, error) {
	match := strings.TrimSpace(rule.Match)
	if msg := validateModelRuleMatch(match, "model quota", "use the group-level limits instead"); msg != "" {
		return rule, invalidModelQuota(msg)
	}
	for _, limit := range []*float64{rule.Daily, rule.Weekly, rule.Monthly} {
		if limit != nil && *limit < 0 {
			return rule, invalidModelQuota("model quota limits must be >= 0")
		}
	}
	return GroupModelQuotaRule{
		Match:   match,
		Daily:   rule.Daily,
		Weekly:  rule.Weekly,
		Monthly: rule.Monthly,
	}, nil
}

func invalidModelQuota(message string) error {
	return infraerrors.New(http.StatusBadRequest, "INVALID_MODEL_QUOTAS", message)
}
