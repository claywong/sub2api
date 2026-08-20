// prompt_dlp_scanner.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 正则检测引擎主入口。
//
// 流程（detection-rules.md 第一~四节）：
//
//	扫描全部规则 → 逐命中跑排除链 → 跑算法校验 → 检测器间去重 → 产出 finding
//
// 这一层完全不产生网络调用，是"减少请求量"的关键：只有存活 finding 才会进入
// prompt_dlp_confirm.go 的 LLM 二次确认。
//
// 与 upstream 合并策略：
//   - 纯新增文件，无 upstream 符号改动，merge 时不会冲突。
//
// =============================================================================
package securityaudit

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// DLPFinding 是一条存活的正则命中。
//
// StartRune/EndRune 用 rune 下标（不是字节下标），因为 upstream 的
// IssueSummary.StartRune/EndRune 语义是 rune 偏移，前端高亮也按 rune 算。
type DLPFinding struct {
	RuleID    string
	Class     DLPDetectorClass
	ScannerID string
	Title     string
	Severity  RiskLevel
	Score     float64
	// Match 是命中的完整片段，Value 是其中的敏感值部分（key=value 规则里的值）。
	Match string
	Value string
	// 字节下标，仅用于内部去重计算。
	startByte int
	endByte   int
	StartRune int
	EndRune   int
}

// dlpScanResult 汇总一次扫描的结果，含被排除的命中数量便于观测规则是否过严。
type dlpScanResult struct {
	Findings      []DLPFinding
	ExcludedCount int
	// ExcludedReasons 按原因计数，写进结构化日志用于调参。
	ExcludedReasons map[string]int
}

// ScanDLP 对文本执行正则检测。enabledScanners 为空表示全部 DLP scanner 启用。
func ScanDLP(text string, enabledScanners []string) dlpScanResult {
	return ScanDLPWithOverrides(text, enabledScanners, nil)
}

// ScanDLPWithOverrides 在 ScanDLP 之上应用管理员的单条规则覆盖。
//
// 保留 ScanDLP 的原签名而不是直接加参数：它有大量调用点（含测试）不关心覆盖，
// 让它们继续用两参数形式更清楚，也避免一处签名变更牵动整片改动。
//
// overrides 为 nil 时行为与 ScanDLP 完全一致。
func ScanDLPWithOverrides(
	text string, enabledScanners []string, overrides DLPRuleOverrides,
) dlpScanResult {
	result := dlpScanResult{ExcludedReasons: map[string]int{}}
	if strings.TrimSpace(text) == "" {
		return result
	}
	enabled := dlpEnabledSet(enabledScanners)
	candidates := make([]DLPFinding, 0, 8)

	for _, rule := range dlpRules {
		if _, ok := enabled[rule.ScannerID]; !ok {
			continue
		}
		// 管理员逐条关掉的规则直接跳过：正则都不跑，零开销。
		if overrides.IsRuleDisabled(rule.ID) {
			continue
		}
		// 严重度可能被管理员改过。用生效值构造 finding，让下游的去重优先级、
		// HighestSeverity 与 dlpShouldBlock 自动跟着变——它们都只读 finding.Severity。
		severity := overrides.EffectiveSeverity(rule)
		for _, location := range rule.Pattern.FindAllStringSubmatchIndex(text, -1) {
			match := text[location[0]:location[1]]
			value, valueStart, valueEnd := dlpRuleValue(rule, text, location)
			if value == "" {
				continue
			}
			if verdict := applyExclusions(rule, text, match, value, location[0], location[1]); verdict.Excluded {
				result.ExcludedCount++
				result.ExcludedReasons[verdict.Reason]++
				continue
			}
			// 算法校验针对值部分（身份证/银行卡/手机号规则无捕获组，值即整段）。
			if !runValidator(rule.Validator, value) {
				result.ExcludedCount++
				result.ExcludedReasons["算法校验未通过："+string(rule.Validator)]++
				continue
			}
			candidates = append(candidates, DLPFinding{
				RuleID: rule.ID, Class: rule.Class, ScannerID: rule.ScannerID,
				Title: rule.Title, Severity: severity, Score: rule.Confidence,
				Match: match, Value: value,
				startByte: valueStart, endByte: valueEnd,
			})
		}
	}

	survivors := dedupeDLPFindings(candidates)
	for index := range survivors {
		survivors[index].StartRune = utf8.RuneCountInString(text[:survivors[index].startByte])
		survivors[index].EndRune = utf8.RuneCountInString(text[:survivors[index].endByte])
	}
	result.Findings = survivors
	return result
}

// dlpRuleValue 取出规则声明的值部分及其字节区间。
// ValueGroup 为 0 时值即整个匹配。
func dlpRuleValue(rule DLPRule, text string, location []int) (string, int, int) {
	if rule.ValueGroup <= 0 {
		return text[location[0]:location[1]], location[0], location[1]
	}
	groupStart := rule.ValueGroup * 2
	if groupStart+1 >= len(location) {
		return "", 0, 0
	}
	start, end := location[groupStart], location[groupStart+1]
	if start < 0 || end < 0 || start >= end {
		return "", 0, 0
	}
	return text[start:end], start, end
}

// dlpEnabledSet 把启用列表转成集合。空列表视为全部启用。
func dlpEnabledSet(enabledScanners []string) map[string]struct{} {
	result := map[string]struct{}{}
	if len(enabledScanners) == 0 {
		for _, id := range DLPScannerIDs() {
			result[id] = struct{}{}
		}
		return result
	}
	for _, id := range enabledScanners {
		if IsDLPScanner(id) {
			result[id] = struct{}{}
		}
	}
	return result
}

// HighestSeverity 返回 finding 集合里的最高严重度。空集合返回 RiskLow。
func HighestSeverity(findings []DLPFinding) RiskLevel {
	highest := RiskLow
	for _, finding := range findings {
		if dlpSeverityRank(finding.Severity) > dlpSeverityRank(highest) {
			highest = finding.Severity
		}
	}
	return highest
}

// dlpSeverityRank 给严重度排序，便于比较取最高。
func dlpSeverityRank(level RiskLevel) int {
	switch level {
	case RiskCritical:
		return 4
	case RiskHigh:
		return 3
	case RiskMedium:
		return 2
	default:
		return 1
	}
}

// DLPCategories 返回 finding 涉及的 scanner ID 集合，按 catalog 顺序排列。
func DLPCategories(findings []DLPFinding) []string {
	seen := map[string]struct{}{}
	for _, finding := range findings {
		seen[finding.ScannerID] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for _, id := range DLPScannerIDs() {
		if _, ok := seen[id]; ok {
			result = append(result, id)
		}
	}
	return result
}

// sortDLPFindings 让输出顺序稳定：先按起始位置，再按规则 ID。
func sortDLPFindings(findings []DLPFinding) {
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].startByte != findings[j].startByte {
			return findings[i].startByte < findings[j].startByte
		}
		return findings[i].RuleID < findings[j].RuleID
	})
}
