// prompt_dlp_evidence_passthrough.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 证据在读写路径上跳过脱敏。
//
// 背景：证据从产出到展示会经过三道 RedactPreview，只改第一道无效。
//
//	① buildDLPEvidence            —— 私有，已按需只脱敏标题与模型理由
//	② insertEvent                 —— upstream，落库前对全部证据脱敏
//	③ BuildIssueSummaries         —— upstream，渲染给前端时再脱敏一次
//
//	②③ 会把 buildDLPEvidence 里刻意保留的命中明文重新吃掉（实测
//	「命中值 13912345678」→「命中值 ***PHONE***」），导致管理员依旧看不到。
//
// 为什么不直接删掉 ②③ 的脱敏：
//
//	那两处是 qwen3guard 与 DLP 共用的路径。qwen3guard 的证据来自模型自由文本，
//	内容不可预期，保留脱敏是合理的默认。DLP 的证据结构由我们自己拼装，明文是
//	刻意的产物，两者需要不同策略。
//
// 为什么按 ScannerBackend 分支而不是加参数：
//
//	②③ 的调用点分散且都在 upstream 文件里，加参数要改多处签名，merge 必冲突。
//	NormalizedResult.ScannerBackend 已经带着 "dlp-regex+llm" 流经这两处，
//	用它做判定只需在每处插一行条件。
//
// 管理员可见性是刻意设计：
//
//	审计页面只对管理员开放，而同一张表的 full_prompt 本来就是明文。证据里的
//	命中值不构成新的暴露面，却是判断误报的唯一依据——脱敏后管理员只能看到
//	「手机号 | 疑似真实手机号」，无法分辨 order_no 误报与真实泄露。
//
// 与 upstream 合并策略：
//   - 本文件纯新增。upstream 侧 prompt_repository.go 与 prompt_issue_summary.go
//     各插一行条件调用，均为单行改动。
//
// =============================================================================
package securityaudit

// DLPEvidenceMaxRunes 是 DLP 证据的长度上限。
//
// 刻意大于 upstream 传入的 160：DLP 证据结构固定但更长——标题、模型理由、
// 命中值（至多 128）、偏移量、上下文窗口（前后各 48）。按 160 截断会把上下文
// 直接砍掉，而上下文正是判断误报的依据。
const DLPEvidenceMaxRunes = 512

// redactEvidenceForBackend 按后端决定证据是否脱敏。
//
// DLP 的证据直接透传：内容由 buildDLPEvidence 结构化拼装，命中明文是刻意保留的，
// 供管理员分诊误报。其余后端（qwen3guard）沿用原有脱敏行为与长度上限，不受影响。
func redactEvidenceForBackend(evidence, scannerBackend string, maxRunes int) string {
	if scannerBackend == DLPScannerBackend {
		return TrimRunes(evidence, DLPEvidenceMaxRunes)
	}
	return RedactPreview(evidence, maxRunes)
}
