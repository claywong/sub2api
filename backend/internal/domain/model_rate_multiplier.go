package domain

// GroupModelRateMultiplierRule 是一条「按模型/模型前缀」的倍率系数规则。
//
// Match 语义与 GroupModelQuotaRule 一致：精确模型名，或末尾 `*` 的前缀（如 "claude-opus*"）。
// Multiplier 是叠加在分组倍率（或用户专属倍率）之上的系数，必须 > 0。
type GroupModelRateMultiplierRule struct {
	Match      string  `json:"match"`
	Multiplier float64 `json:"multiplier"`
}

// GroupModelRateMultipliers 是分组级「按模型/模型前缀」倍率系数配置。
//
// 计费时 rate_multiplier = (用户专属倍率 ?? 分组倍率) × 命中规则的系数 × 高峰因子；
// 未命中任何规则时系数为 1。一次请求只命中最具体的一条规则。
type GroupModelRateMultipliers struct {
	Enabled bool                           `json:"enabled"`
	Rules   []GroupModelRateMultiplierRule `json:"rules,omitempty"`
}
