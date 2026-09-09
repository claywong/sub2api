package domain

// GroupModelQuotaRule 是一条「按模型/模型前缀」的配额规则。
//
// Match 是客户端书写的模型名或模型前缀（末尾 `*` 表示前缀匹配），例如：
//   - "gpt-6-astra"   精确匹配单个模型
//   - "claude-opus*"  匹配整个 claude-opus 系列，系列内所有模型共享同一份额度
//
// Daily/Weekly/Monthly 为该规则的 USD 上限，语义与 user_platform_quotas 一致：
//   - nil → 该窗口不限制
//   - 0   → 该窗口完全禁用（usage >= 0 恒成立，任何请求都会被拒绝）
//   - >0  → USD 上限
type GroupModelQuotaRule struct {
	Match   string   `json:"match"`
	Daily   *float64 `json:"daily,omitempty"`
	Weekly  *float64 `json:"weekly,omitempty"`
	Monthly *float64 `json:"monthly,omitempty"`
}

// GroupModelQuotas 是分组级「按模型/模型前缀」配额配置。
//
// 与分组原有 daily/weekly/monthly_limit_usd（整组共享的总配额）是两层独立约束：
// 总配额是外层，本配置是内层，两层都需满足才放行。
//
// 一次请求只命中最具体的一条规则（exact 优先于 prefix，prefix 间最长优先），
// 命中规则的用量按规则原文记账，见 user_group_model_usage 表。
type GroupModelQuotas struct {
	Enabled bool                  `json:"enabled"`
	Rules   []GroupModelQuotaRule `json:"rules,omitempty"`
}
