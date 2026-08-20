// prompt_dlp_dedupe.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 检测器间去重。
//
// 对应 detection-rules.md 第四节：
//   - regex 命中与 PII/凭证命中区间重叠 → 丢弃 regex 命中（后者带校验，质量更高）
//   - 宽泛 regex 命中包含具体高严重度命中 → 丢弃宽泛者
//     （如 Sensitive Field 整体命中包含了 Cloud Key 的 AKID 值）
//   - 完全相同区间的同规则命中去重
//
// 与 upstream 合并策略：
//   - 纯新增文件，无 upstream 符号改动，merge 时不会冲突。
//
// =============================================================================
package securityaudit

import "sort"

// dedupeDLPFindings 执行检测器间去重，返回存活的命中。
//
// 采用「排序 + 贪心保留」而非两两淘汰：先按质量优先级给候选建立全序，再从优到劣
// 逐个保留，丢弃与已保留命中区间重叠的低质量候选。
//
// 之所以不能两两淘汰：两条规则命中同一区间时（如 credential-generic-api-key 与
// sensitive-cloud-key-prefix 都命中 `access_key_secret=AKIDxxx` 的值部分），
// 各自都会判定对方胜出，导致双双被丢弃、该敏感信息完全漏报。
func dedupeDLPFindings(findings []DLPFinding) []DLPFinding {
	if len(findings) <= 1 {
		return append([]DLPFinding(nil), findings...)
	}
	ranked := dropIdenticalSpans(findings)
	sortByDLPQuality(ranked)
	survivors := make([]DLPFinding, 0, len(ranked))
	for _, candidate := range ranked {
		if overlapsAnyKept(candidate, survivors) {
			continue
		}
		survivors = append(survivors, candidate)
	}
	sortDLPFindings(survivors)
	return survivors
}

// sortByDLPQuality 按质量从优到劣排序，是去重结果确定性的来源。
//
// 优先级依次为：
//  1. 具体规则优于宽泛规则（detection-rules.md：宽泛命中包含具体命中时丢弃宽泛者）
//  2. 带校验/强特征的 PII、凭证优于宽泛的敏感信息 regex
//  3. 严重度高者优先
//  4. 置信度高者优先
//  5. 规则 ID 字典序（仅为保证结果稳定可测）
func sortByDLPQuality(findings []DLPFinding) {
	sort.SliceStable(findings, func(i, j int) bool {
		left, right := findings[i], findings[j]
		if left.Broad() != right.Broad() {
			return !left.Broad()
		}
		if leftRank, rightRank := dlpClassRank(left.Class), dlpClassRank(right.Class); leftRank != rightRank {
			return leftRank < rightRank
		}
		if leftRank, rightRank := dlpSeverityRank(left.Severity), dlpSeverityRank(right.Severity); leftRank != rightRank {
			return leftRank > rightRank
		}
		if left.Score != right.Score {
			return left.Score > right.Score
		}
		return left.RuleID < right.RuleID
	})
}

// dlpClassRank 给检测器大类排优先级，数值越小质量越高。
// PII 与凭证都带算法校验或强前缀特征，质量高于宽泛的敏感信息正则。
func dlpClassRank(class DLPDetectorClass) int {
	switch class {
	case DLPClassPII:
		return 0
	case DLPClassCredential:
		return 1
	default:
		return 2
	}
}

// overlapsAnyKept 判断候选是否与任一已保留命中的区间重叠。
func overlapsAnyKept(candidate DLPFinding, kept []DLPFinding) bool {
	for _, existing := range kept {
		if spansOverlap(candidate, existing) {
			return true
		}
	}
	return false
}

// dropIdenticalSpans 丢弃「同规则 + 同区间」的重复命中。
func dropIdenticalSpans(findings []DLPFinding) []DLPFinding {
	type spanKey struct {
		ruleID string
		start  int
		end    int
	}
	seen := make(map[spanKey]struct{}, len(findings))
	result := make([]DLPFinding, 0, len(findings))
	for _, finding := range findings {
		key := spanKey{finding.RuleID, finding.startByte, finding.endByte}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, finding)
	}
	return result
}

// Broad 反查规则是否为宽泛规则。
func (f DLPFinding) Broad() bool {
	rule, ok := dlpRuleByID(f.RuleID)
	return ok && rule.Broad
}

// spansOverlap 判断两个命中的字节区间是否重叠。
func spansOverlap(a, b DLPFinding) bool {
	return a.startByte < b.endByte && b.startByte < a.endByte
}
