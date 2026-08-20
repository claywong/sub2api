// prompt_guard_dlp_passevent.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：把"正则命中但最终放行"也写成审计事件。
//
// 覆盖 EvaluateDLP 里 len(sensitive) == 0 的两条出口：
//   - 模型判为误报：已知无害，落 risk_level=low / action=Allow
//   - 确认链路降级放行：未知、可能真漏了，落 risk_level=medium / action=Warn
//
// 为什么要区分这两类：事件表没有专门字段承载"为什么放行"，EventFilter 也只能按
// decision / risk_level 筛（没有 action 筛选）。把降级放在更高的 risk_level 上，
// 管理员筛 decision=pass + risk_level=medium 就能单独复查降级期间的漏放；
// 语义上也站得住——确认为误报与"没能确认"的风险本就不同。
//
// 两个刻意的实现选择：
//   - 开关关闭时在本文件里直接短路，不走 RecordBlocking。因为 RecordBlocking 里
//     insertJob 是无条件执行的，只有 insertEvent 受 storePassEvents 控制；靠仓储层
//     跳过会让 jobs 表在开关关闭时照样每次命中都长一行。
//   - 证据复用 buildDLPEvidence，它已 RedactPreview 且只写规则名/理由/位置，
//     绝不写命中的敏感明文——这类事件量大且长期留存，写明文等于把敏感数据又落一份盘。
//
// 与 upstream 合并策略：
//   - 纯新增文件。prompt_guard_dlp.go 里只有一行 hook 调用本文件的 recordDLPPassEvent。
//
// =============================================================================
package securityaudit

import "context"

// recordDLPPassEvent 在开关打开时，把"命中但放行"写成 decision=pass 的审计事件。
//
// degraded 区分两类放行原因，语义见文件头注释。
func (g *GuardEvaluator) recordDLPPassEvent(
	ctx context.Context, cfg ActiveConfig, snapshot PromptSnapshot,
	findings []DLPFinding, verdicts []DLPConfirmVerdict, degraded bool, latency int,
) {
	// 短路必须在这里：见文件头关于 insertJob 的说明。
	if !cfg.DLP.RecordRegexHits || g.repo == nil || len(findings) == 0 {
		return
	}

	result := buildDLPPassResult(findings, verdicts, degraded, latency)
	// storePassEvents 传 true：开关已经在上面判过，这里不该再被 qwen3guard 的
	// store_pass_events 二次否决——那是内容安全的开关，与 DLP 无关。
	if _, err := g.repo.RecordBlocking(ctx, snapshot.Redacted(), cfg.ConfigVersion, result, true); err != nil {
		LogDLPWarn(EventDLPRecordFailed, mergeLogFields(snapshotLogFields(snapshot), map[string]any{
			"error_code": "dlp_record_failed",
			"status":     "failed",
			"degraded":   degraded,
		}))
	}
}

// buildDLPPassResult 构造"命中但放行"事件的 NormalizedResult。
//
// 与 buildDLPNormalizedResult 的区别：那个处理确认为敏感的命中（flag/critical），
// 这个处理放行的命中（pass），两者的 decision/risk/action 取值完全不同，
// 合成一个函数会让参数里塞满互斥的分支标志。
func buildDLPPassResult(
	findings []DLPFinding, verdicts []DLPConfirmVerdict, degraded bool, latency int,
) *NormalizedResult {
	// 误报＝已知无害；降级＝没能确认，可能真漏了，所以给更高的 risk_level，
	// 让管理员能用现有的 risk_level 筛选把它单独捞出来复查。
	risk, action := RiskLow, ActionAllow
	if degraded {
		risk, action = RiskMedium, ActionWarn
	}

	categories := DLPCategories(findings)
	result := &NormalizedResult{
		Decision: EventPass, RiskLevel: risk, Action: action,
		// Safety 用 "Safe"：这条事件记录的是放行结果，不是风险判定。
		Safety:          "Safe",
		Categories:      categories,
		MatchedScanners: categories,
		ScannerScores:   map[string]float64{},
		ScannerEvidence: map[string]string{},
		ScannerBackend:  DLPScannerBackend,
		ScannerVersion:  DLPPolicyID,
		PolicyID:        DLPPolicyID,
		PolicyVersion:   1,
		ChunkTotal:      1,
		LatencyMS:       latency,
	}
	for _, finding := range findings {
		if finding.Score > result.ScannerScores[finding.ScannerID] {
			result.ScannerScores[finding.ScannerID] = finding.Score
		}
		if _, exists := result.ScannerEvidence[finding.ScannerID]; !exists {
			result.ScannerEvidence[finding.ScannerID] = buildDLPPassEvidence(finding, verdicts, findings, degraded)
		}
	}
	return result
}

// buildDLPPassEvidence 生成放行事件的证据文本。
//
// 降级时模型没有给出理由（verdicts 是零值），直接写死原因，否则证据里只剩规则名
// 与位置，管理员看不出这条是"确认过没事"还是"没能确认"。
func buildDLPPassEvidence(
	finding DLPFinding, verdicts []DLPConfirmVerdict, allFindings []DLPFinding, degraded bool,
) string {
	if degraded {
		return RedactPreview(finding.Title+" | 确认服务不可用，按放行处理 | 位置 "+
			itoaDLP(finding.StartRune)+"-"+itoaDLP(finding.EndRune), 160)
	}
	// 非降级路径复用既有实现：它会带上模型给出的误报理由。
	return buildDLPEvidence(finding, verdicts, allFindings)
}
