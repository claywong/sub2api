// prompt_dlp_rule_overrides.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 单条规则的严重度与启停覆盖。
//
// 为什么需要：
//
//	dlpRules 里的 Severity 原本是编译期写死的，而严重度直接决定处置——
//	dlpShouldBlock 只拦 >= high，medium 恒为仅记录。这带来一个反直觉的后果：
//	「凭证泄露」9 条规则里 8 条是 medium（AWS Access Key、GitHub Token、私钥块
//	等），管理员开了「高危命中时拦截请求」，这些凭证泄露照样放行，只留一条事件。
//	不同部署对「什么算高危」判断不同，所以把严重度交给管理员配。
//
//	逐条启停解决另一个问题：Broad 规则（通用 API Key、密码字段、云密钥字段）
//	误报相对高，原先只能关掉整个检测器（连带 8 条精确规则一起失效），
//	现在可以只关噪声那条。
//
// 严重度只允许 medium / high：
//
//	low 与 medium 在拦截行为上完全一致（都不拦），critical 与 high 也一致，
//	多给两个级别只会让人以为有行为差异。事件分诊靠规则标题已经足够。
//
// 存储策略：只存与内置默认值的偏差
//
//	全量存会让「升级后规则默认严重度调整」无法生效——管理员没改过的规则也会被
//	旧值钉住。normalizeDLPRuleOverrides 负责把等于默认值且未禁用的条目丢掉。
//
// 未知 rule ID 一律丢弃而不报错：
//
//	版本间规则会增删，旧配置里残留一个已下线的 rule ID 不该让整个配置加载失败。
//
// 与 upstream 合并策略：
//   - 本文件纯增量。私有文件 prompt_config_dlp.go / prompt_dlp_scanner.go /
//     prompt_config_dlp_dto.go 各加少量 hook，均不涉及 upstream 文件。
//
// =============================================================================
package securityaudit

import (
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// DLPRuleOverride 是单条规则的管理员覆盖项。
type DLPRuleOverride struct {
	// Severity 覆盖规则的内置严重度。空字符串表示沿用内置默认值。
	Severity RiskLevel `json:"severity,omitempty"`
	// Disabled 为 true 时该规则完全不参与扫描。
	Disabled bool `json:"disabled,omitempty"`
}

// DLPRuleOverrides 是 ruleID → 覆盖项。
type DLPRuleOverrides map[string]DLPRuleOverride

// IsRuleDisabled 判断规则是否被管理员关掉。
func (overrides DLPRuleOverrides) IsRuleDisabled(ruleID string) bool {
	if len(overrides) == 0 {
		return false
	}
	return overrides[ruleID].Disabled
}

// EffectiveSeverity 返回规则的生效严重度。
//
// 覆盖值非法或缺失时回落到规则内置默认值，绝不返回空——空严重度会让
// dlpSeverityRank 落到 default 分支（等同 low），把高危规则悄悄降级。
func (overrides DLPRuleOverrides) EffectiveSeverity(rule DLPRule) RiskLevel {
	if len(overrides) == 0 {
		return rule.Severity
	}
	override, exists := overrides[rule.ID]
	if !exists || !isConfigurableDLPSeverity(override.Severity) {
		return rule.Severity
	}
	return override.Severity
}

// isConfigurableDLPSeverity 判断严重度是否属于允许管理员设置的取值。
func isConfigurableDLPSeverity(level RiskLevel) bool {
	return level == RiskMedium || level == RiskHigh
}

// ConfigurableDLPSeverities 返回允许管理员设置的严重度，供前端渲染选择器。
func ConfigurableDLPSeverities() []RiskLevel {
	return []RiskLevel{RiskMedium, RiskHigh}
}

// DLPRuleCatalogEntry 是单条规则的对外视图。
type DLPRuleCatalogEntry struct {
	ID        string `json:"id"`
	ScannerID string `json:"scanner_id"`
	Title     string `json:"title"`
	// DefaultSeverity 是代码内置的严重度，界面用它标出「已改过默认值」。
	DefaultSeverity RiskLevel `json:"default_severity"`
	// Severity 是当前生效的严重度。
	Severity RiskLevel `json:"severity"`
	Disabled bool      `json:"disabled"`
	// Broad 标记宽泛规则，这类误报相对高，界面上提示管理员。
	Broad bool `json:"broad"`
}

// DLPRuleCatalog 返回全部规则的对外视图，已应用管理员覆盖。
func DLPRuleCatalog(overrides DLPRuleOverrides) []DLPRuleCatalogEntry {
	result := make([]DLPRuleCatalogEntry, 0, len(dlpRules))
	for _, rule := range dlpRules {
		result = append(result, DLPRuleCatalogEntry{
			ID: rule.ID, ScannerID: rule.ScannerID, Title: rule.Title,
			DefaultSeverity: rule.Severity,
			Severity:        overrides.EffectiveSeverity(rule),
			Disabled:        overrides.IsRuleDisabled(rule.ID),
			Broad:           rule.Broad,
		})
	}
	return result
}

// BlockingDLPSeverities 返回会触发拦截的严重度（前提是拦截总开关已打开）。
//
// 为什么下发这个而不是逐规则的「是否会拦」布尔值：
//
//	管理员在界面上改严重度时，草稿状态与已保存状态不一致。后端按已保存配置算出的
//	布尔值当场就过期了，界面必须按草稿实时算。但「哪些严重度会拦」这个阈值应当由
//	后端说了算——它和 dlpShouldBlock 是同一个事实来源，前端只负责把它和草稿组合。
func BlockingDLPSeverities() []RiskLevel {
	result := make([]RiskLevel, 0, 2)
	for _, level := range []RiskLevel{RiskLow, RiskMedium, RiskHigh, RiskCritical} {
		if dlpSeverityRank(level) >= dlpSeverityRank(RiskHigh) {
			result = append(result, level)
		}
	}
	return result
}

// normalizeDLPRuleOverrides 归一化覆盖表：丢弃未知规则、非法严重度与无意义条目。
//
// 「无意义条目」指严重度等于内置默认值且未禁用——存下来只会让日后调整内置默认值
// 对老配置失效。返回 nil 而非空 map，让 omitempty 生效，配置行保持干净。
func normalizeDLPRuleOverrides(overrides DLPRuleOverrides) DLPRuleOverrides {
	if len(overrides) == 0 {
		return nil
	}
	result := make(DLPRuleOverrides, len(overrides))
	for ruleID, override := range overrides {
		trimmed := strings.TrimSpace(ruleID)
		rule, exists := dlpRuleByID(trimmed)
		if !exists {
			// 版本间规则会增删，残留的旧 ID 静默丢弃而不是让配置加载失败。
			continue
		}
		normalized := DLPRuleOverride{Disabled: override.Disabled}
		if isConfigurableDLPSeverity(override.Severity) && override.Severity != rule.Severity {
			normalized.Severity = override.Severity
		}
		if normalized.Severity == "" && !normalized.Disabled {
			continue
		}
		result[trimmed] = normalized
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// validateDLPRuleOverrides 校验覆盖表。
//
// 只拒非法严重度：未知 rule ID 由 normalize 静默丢弃（见文件头说明），
// 而管理员从界面提交一个非法严重度属于请求有误，应当明确报错。
func validateDLPRuleOverrides(overrides DLPRuleOverrides) error {
	for ruleID, override := range overrides {
		if override.Severity == "" {
			continue
		}
		if !isConfigurableDLPSeverity(override.Severity) {
			return infraerrors.BadRequest("dlp_invalid_rule_severity",
				"DLP 规则严重度只能是 medium 或 high："+ruleID)
		}
	}
	return nil
}

// enabledDLPRuleCount 统计在给定检测器范围内、未被逐条关掉的规则数量。
//
// 用于拒绝「DLP 已启用但一条规则都不生效」的配置：这类配置会让 DLP 静默停摆，
// 与既有的「启用却没选任何分组」属同一类问题，都必须在保存时就拦下来。
func enabledDLPRuleCount(enabledScanners []string, overrides DLPRuleOverrides) int {
	enabled := dlpEnabledSet(enabledScanners)
	count := 0
	for _, rule := range dlpRules {
		if _, ok := enabled[rule.ScannerID]; !ok {
			continue
		}
		if overrides.IsRuleDisabled(rule.ID) {
			continue
		}
		count++
	}
	return count
}

// DLPRuleIDsByScanner 返回某检测器下的全部规则 ID，按注册顺序。
func DLPRuleIDsByScanner(scannerID string) []string {
	result := make([]string, 0, 4)
	for _, rule := range dlpRules {
		if rule.ScannerID == scannerID {
			result = append(result, rule.ID)
		}
	}
	return result
}
