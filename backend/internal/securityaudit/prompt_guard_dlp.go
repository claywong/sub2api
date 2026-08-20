// prompt_guard_dlp.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 检测在 GuardEvaluator 上的编排。
//
// 检测链路：
//  1. 正则扫描（μs 级、零网络调用）
//  2. 未命中 → 返回 nil，请求继续走 upstream 流程，行为与改动前完全一致
//  3. 命中 → 查确认缓存 → 未缓存的送 luna 批量确认
//  4. 确认为真敏感 → 按严重度处置（high/critical 拦截，medium 仅审计）
//  5. 确认为误报 / 确认链路故障 → 放行（fail-open）
//
// 调用来源：Coordinator.Check 经 PromptService.EvaluateDLP 进入，跑在 upstream 的
// 审计模式分发之前。刻意不挂在 GuardEvaluator.Evaluate 里面——那样会被 qwen3guard
// 的模式开关绑死，ModeOff/ModeAsync 下 DLP 静默失效。详见 coordinator_dlp.go。
//
// 为什么 fail-open：DLP 依赖第三方中转模型，实测会出现 403/429/401 波动。若沿用
// upstream qwen3guard 的 fail-closed（返回 DecisionUnavailable → HTTP 503），
// 对方一抖动就会拖垮整个网关。正则层零外部依赖仍在工作，降级只是少了降误报那层。
//
// 与 upstream 合并策略：
//   - 本文件是纯增量。upstream 侧仅需 4 处极小改动：GuardEvaluator 加 2 个字段、
//     ActiveConfig 加 1 个字段、Coordinator 加 1 个字段 + Check 开头 4 行 hook。
//
// =============================================================================
package securityaudit

import (
	"context"
	"strings"
	"time"
)

// DLP 相关的错误码与后端标识。
const (
	// ErrorCodeDLPBlocked 是 DLP 拦截的错误码，与 qwen3guard 的拦截区分开，
	// 便于运维在日志里分辨是内容安全拦的还是数据防泄漏拦的。
	ErrorCodeDLPBlocked = "dlp_sensitive_data_blocked"
	// ErrorCodeDLPConfirmDegraded 标记"确认链路故障导致放行"，
	// 与"模型判定为误报导致放行"区分，便于事后追查漏放。
	ErrorCodeDLPConfirmDegraded = "dlp_confirm_degraded"

	// DLPScannerBackend 写入 prompt_audit_events.scanner_backend。
	DLPScannerBackend = "dlp-regex+llm"

	// DLPPolicyID 写入事件的 policy_id，便于按策略来源筛选。
	DLPPolicyID = "dlp-regex"
)

// DLPClientMessage 是 DLP 拦截时返回给客户端的提示。
// 刻意不回显命中内容，避免把敏感片段再吐回响应体。
const DLPClientMessage = "请求包含敏感信息（如凭证、证件号或口令），已被数据防泄漏策略拦截"

// blockedErrorCodeAndMessage 决定拦截决策对外暴露的错误码与客户端文案。
//
// upstream 的 prioritize 原本硬编码 qwen3guard 的错误码与文案。DLP 拦截需要保留
// 自己的标识，否则：
//   - 客户端收到「提示词安全审计拒绝了该请求」，与实际拦截原因（敏感信息泄露）不符；
//   - 运维在 API 边界只看到 prompt_guard_blocked，无法区分两套拦截器。
//
// 非 DLP 的拦截原样沿用 upstream 的取值，行为不变。
func blockedErrorCodeAndMessage(prompt *PromptDecision) (string, string) {
	if prompt != nil && prompt.ErrorCode == ErrorCodeDLPBlocked {
		return ErrorCodeDLPBlocked, DLPClientMessage
	}
	return ErrorCodeBlocked, "提示词安全审计拒绝了该请求，请调整输入后重试"
}

// WithDLP 给 evaluator 装上 DLP 组件。返回 g 便于链式调用。
//
// 之所以用 setter 而不是改 NewGuardEvaluator 的签名：upstream 的构造函数签名
// 一旦改动，未来 merge 必然冲突（CLAUDE.md 的 inline 最小化原则）。
func (g *GuardEvaluator) WithDLP(confirmer *DLPConfirmer, cache *DLPConfirmCache) *GuardEvaluator {
	if g == nil {
		return nil
	}
	g.dlpConfirmer = confirmer
	g.dlpCache = cache
	return g
}

// EvaluateDLP 执行 DLP 检测。
//
// 返回 nil 表示"不由 DLP 决定本次请求"，调用方应继续原有流程。
// 只有确认为真实敏感信息且严重度达到拦截门槛时才返回非 nil 的拦截决策。
//
// Enabled 与分组范围已由 PromptService.EvaluateDLP 判过，这里仍再判一次 Enabled，
// 因为本方法是导出的，不能假定调用方一定做了门控。
func (g *GuardEvaluator) EvaluateDLP(
	ctx context.Context, cfg ActiveConfig, snapshot PromptSnapshot,
) *PromptDecision {
	if g == nil {
		return nil
	}
	dlpCfg := cfg.DLP
	if !dlpCfg.Enabled {
		return nil
	}
	start := g.clock.Now()

	// 第一层：正则。零网络调用，未命中直接返回。
	scan := ScanDLPWithOverrides(snapshot.ScanText, dlpCfg.EffectiveScanners(), dlpCfg.RuleOverrides)
	if len(scan.Findings) == 0 {
		if scan.ExcludedCount > 0 {
			LogDLPInfo(EventDLPRegexExcluded, mergeLogFields(snapshotLogFields(snapshot), map[string]any{
				"excluded_count":  scan.ExcludedCount,
				"exclude_reasons": scan.ExcludedReasons,
				"status":          "excluded",
			}))
		}
		return nil
	}

	// 第二层：确认。缓存命中的直接复用，其余送模型。
	findings := scan.Findings
	verdicts, degraded := g.confirmDLPFindings(ctx, dlpCfg, snapshot, findings)

	sensitive := collectSensitiveDLPFindings(findings, verdicts)
	latency := int(g.clock.Now().Sub(start).Milliseconds())

	if len(sensitive) == 0 {
		// 全部判为误报，或确认链路故障导致无法判定。两种情况都放行，但日志要能区分。
		g.logDLPAllowed(snapshot, findings, degraded, latency)
		return nil
	}

	return g.buildDLPDecision(ctx, cfg, snapshot, sensitive, verdicts, findings, latency)
}

// confirmDLPFindings 对 finding 做二次确认，返回结论与是否发生降级。
//
// degraded 为 true 表示确认链路不可用或调用失败，调用方需按 fail-open 处理。
func (g *GuardEvaluator) confirmDLPFindings(
	ctx context.Context, dlpCfg ActiveDLPConfig, snapshot PromptSnapshot, findings []DLPFinding,
) ([]DLPConfirmVerdict, bool) {
	// 未启用二次确认时，正则命中即视为敏感（误报率更高，由管理员自行取舍）。
	if !dlpCfg.ConfirmEnabled {
		verdicts := make([]DLPConfirmVerdict, len(findings))
		for index := range verdicts {
			verdicts[index] = DLPConfirmVerdict{
				Sensitive: true, Confirmed: true, Reason: "未启用二次确认，正则命中即判定",
			}
		}
		return verdicts, false
	}
	if !dlpCfg.ConfirmReady() {
		// 开了确认但没有可用节点：按 fail-open 放行，并明确记录降级原因。
		return make([]DLPConfirmVerdict, len(findings)), true
	}

	verdicts := g.lookupDLPCache(ctx, dlpCfg, findings)
	pending, pendingIndexes := collectPendingDLPFindings(findings, verdicts)
	if len(pending) == 0 {
		return verdicts, false
	}

	endpoint := dlpCfg.EnabledEndpoints()[0]
	confirmCtx, cancel := context.WithTimeout(ctx, dlpCfg.ConfirmTimeout)
	defer cancel()
	fresh, err := g.dlpConfirmer.Confirm(confirmCtx, endpoint, pending)
	if err != nil {
		LogDLPWarn(EventDLPConfirmFailed, mergeLogFields(snapshotLogFields(snapshot), map[string]any{
			"error_code":     ErrorCodeDLPConfirmDegraded,
			"finding_count":  len(pending),
			"guard_endpoint": endpoint.ID,
			"status":         "degraded",
		}))
		return verdicts, true
	}
	for offset, verdict := range fresh {
		if offset >= len(pendingIndexes) {
			break
		}
		verdicts[pendingIndexes[offset]] = verdict
	}
	g.storeDLPCache(ctx, dlpCfg, pending, fresh)

	// 模型漏项也算降级：这些 finding 没有结论，不能当成误报放行。
	for _, verdict := range verdicts {
		if !verdict.Confirmed {
			return verdicts, true
		}
	}
	return verdicts, false
}

// lookupDLPCache 查确认缓存，未启用缓存时返回全未命中。
func (g *GuardEvaluator) lookupDLPCache(
	ctx context.Context, dlpCfg ActiveDLPConfig, findings []DLPFinding,
) []DLPConfirmVerdict {
	if !dlpCfg.CacheEnabled || g.dlpCache == nil {
		return make([]DLPConfirmVerdict, len(findings))
	}
	return g.dlpCache.Lookup(ctx, findings)
}

// storeDLPCache 写入确认结论。
func (g *GuardEvaluator) storeDLPCache(
	ctx context.Context, dlpCfg ActiveDLPConfig, findings []DLPFinding, verdicts []DLPConfirmVerdict,
) {
	if !dlpCfg.CacheEnabled || g.dlpCache == nil {
		return
	}
	g.dlpCache.Store(ctx, findings, verdicts, dlpCfg.CacheSensitiveTTL, dlpCfg.CacheBenignTTL)
}

// collectPendingDLPFindings 挑出还没有结论、需要实调模型的 finding。
func collectPendingDLPFindings(
	findings []DLPFinding, verdicts []DLPConfirmVerdict,
) ([]DLPFinding, []int) {
	pending := make([]DLPFinding, 0, len(findings))
	indexes := make([]int, 0, len(findings))
	for index, finding := range findings {
		if index < len(verdicts) && verdicts[index].Confirmed {
			continue
		}
		pending = append(pending, finding)
		indexes = append(indexes, index)
	}
	return pending, indexes
}

// collectSensitiveDLPFindings 挑出被确认为真实敏感的 finding。
//
// 只认 Confirmed 且 Sensitive 的结论：未确认（模型漏项或调用失败）不算敏感，
// 走 fail-open 放行。
func collectSensitiveDLPFindings(
	findings []DLPFinding, verdicts []DLPConfirmVerdict,
) []DLPFinding {
	result := make([]DLPFinding, 0, len(findings))
	for index, finding := range findings {
		if index >= len(verdicts) {
			break
		}
		if verdicts[index].Confirmed && verdicts[index].Sensitive {
			result = append(result, finding)
		}
	}
	return result
}

// buildDLPDecision 根据确认为敏感的 finding 构造决策并落审计事件。
func (g *GuardEvaluator) buildDLPDecision(
	ctx context.Context, cfg ActiveConfig, snapshot PromptSnapshot,
	sensitive []DLPFinding, verdicts []DLPConfirmVerdict, allFindings []DLPFinding, latency int,
) *PromptDecision {
	severity := HighestSeverity(sensitive)
	shouldBlock := dlpShouldBlock(cfg.DLP, severity)

	result := buildDLPNormalizedResult(sensitive, verdicts, allFindings, severity, shouldBlock, latency)
	g.recordDLPEvent(ctx, cfg, snapshot, result)

	fields := mergeLogFields(snapshotLogFields(snapshot), map[string]any{
		"risk_level":     string(severity),
		"categories":     result.Categories,
		"finding_count":  len(sensitive),
		"latency_ms":     latency,
		"config_version": cfg.ConfigVersion,
	})
	if !shouldBlock {
		fields["status"] = "flagged"
		LogDLPWarn(EventDLPFlagged, fields)
		return &PromptDecision{Kind: DecisionFlag, Result: result, AllowNextStage: true}
	}
	fields["status"] = "blocked"
	fields["error_code"] = ErrorCodeDLPBlocked
	LogDLPWarn(EventDLPBlocked, fields)
	return &PromptDecision{Kind: DecisionBlock, ErrorCode: ErrorCodeDLPBlocked, Result: result}
}

// dlpShouldBlock 按严重度决定是否拦截。
//
// 遵循 detection-rules.md 第五节处置矩阵：high/critical 拦截并告警，
// medium（JWT、手机号）恒为仅审计不拦截，不受 BlockOnHighSeverity 开关影响。
func dlpShouldBlock(dlpCfg ActiveDLPConfig, severity RiskLevel) bool {
	if !dlpCfg.BlockOnHighSeverity {
		return false
	}
	return dlpSeverityRank(severity) >= dlpSeverityRank(RiskHigh)
}

// buildDLPNormalizedResult 把 DLP 结果映射成 upstream 的 NormalizedResult，
// 这样它能直接复用既有的持久化、IssueSummary 渲染与前端展示。
func buildDLPNormalizedResult(
	sensitive []DLPFinding, verdicts []DLPConfirmVerdict, allFindings []DLPFinding,
	severity RiskLevel, shouldBlock bool, latency int,
) *NormalizedResult {
	decision, action := EventFlag, ActionWarn
	if shouldBlock {
		decision, action = EventCritical, ActionBlock
	}
	categories := DLPCategories(sensitive)
	result := &NormalizedResult{
		Decision: decision, RiskLevel: severity, Action: action,
		Safety:          "Unsafe",
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
	for _, finding := range sensitive {
		// 取同一 scanner 下置信度最高的分数。
		if finding.Score > result.ScannerScores[finding.ScannerID] {
			result.ScannerScores[finding.ScannerID] = finding.Score
		}
		// 证据只写规则标题与理由，绝不写命中的敏感明文。
		if _, exists := result.ScannerEvidence[finding.ScannerID]; !exists {
			result.ScannerEvidence[finding.ScannerID] = buildDLPEvidence(finding, verdicts, allFindings)
		}
	}
	return result
}

// buildDLPEvidence 生成脱敏后的证据文本。
//
// 刻意不包含命中的原始值：审计事件会长期留存，写入明文等于把敏感数据又落一份盘。
func buildDLPEvidence(
	finding DLPFinding, verdicts []DLPConfirmVerdict, allFindings []DLPFinding,
) string {
	parts := []string{finding.Title}
	if reason := findDLPVerdictReason(finding, verdicts, allFindings); reason != "" {
		parts = append(parts, reason)
	}
	parts = append(parts, "位置 "+itoaDLP(finding.StartRune)+"-"+itoaDLP(finding.EndRune))
	return RedactPreview(strings.Join(parts, " | "), 160)
}

// findDLPVerdictReason 找出某个 finding 对应的确认理由。
func findDLPVerdictReason(
	finding DLPFinding, verdicts []DLPConfirmVerdict, allFindings []DLPFinding,
) string {
	for index, candidate := range allFindings {
		if index >= len(verdicts) {
			break
		}
		if candidate.RuleID == finding.RuleID && candidate.startByte == finding.startByte {
			return verdicts[index].Reason
		}
	}
	return ""
}

// logDLPAllowed 记录"命中但放行"的情况，区分误报与降级。
func (g *GuardEvaluator) logDLPAllowed(
	snapshot PromptSnapshot, findings []DLPFinding, degraded bool, latency int,
) {
	fields := mergeLogFields(snapshotLogFields(snapshot), map[string]any{
		"finding_count": len(findings),
		"latency_ms":    latency,
	})
	if degraded {
		// 这条日志是漏放风险的唯一线索，级别用 WARN 保证不被过滤掉。
		fields["error_code"] = ErrorCodeDLPConfirmDegraded
		fields["status"] = "degraded_allow"
		LogDLPWarn(EventDLPConfirmDegraded, fields)
		return
	}
	fields["status"] = "false_positive"
	LogDLPInfo(EventDLPFalsePositive, fields)
}

// recordDLPEvent 把 DLP 结果写入审计事件表，复用 upstream 的 RecordBlocking。
//
// 两个要点：
//   - 传 snapshot.Redacted()：与 upstream 同步路径一致，落库前清掉 ScanText，
//     避免把用户原文（含敏感明文）写进审计库。
//   - storePassEvents 传 true：DLP 事件只在确认为敏感时才会走到这里，都是需要
//     留档的命中，不受"是否记录未命中"开关影响。
//   - 写失败只记日志不改判定：拦截与否已定，落库失败不应改变请求走向。
func (g *GuardEvaluator) recordDLPEvent(
	ctx context.Context, cfg ActiveConfig, snapshot PromptSnapshot, result *NormalizedResult,
) {
	if g.repo == nil || result == nil {
		return
	}
	if _, err := g.repo.RecordBlocking(ctx, snapshot.Redacted(), cfg.ConfigVersion, result, true); err != nil {
		LogDLPWarn(EventDLPRecordFailed, mergeLogFields(snapshotLogFields(snapshot), map[string]any{
			"error_code": "dlp_record_failed",
			"status":     "failed",
		}))
	}
}

// itoaDLP 是最小化的整数转字符串，避免为此引入 strconv 依赖。
func itoaDLP(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	digits := make([]byte, 0, 8)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}

// dlpConfirmTimeoutOrDefault 兜底确认超时，供测试与配置缺省场景使用。
func dlpConfirmTimeoutOrDefault(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return DefaultTimeoutMS * time.Millisecond
	}
	return timeout
}
