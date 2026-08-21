package securityaudit

import (
	"strings"
	"testing"
)

// evidencePlaintextSample 是一条典型的 DLP 证据，含命中明文与上下文窗口。
const evidencePlaintextSample = "手机号 | 疑似真实手机号 | 命中值 13912345678 | " +
	"位置 10-21 | 上下文 联系电话 ⟦命中⟧ 已核验"

// TestDLPEvidencePassthroughKeepsPlaintext DLP 证据在读写路径上必须原样透传。
//
// 这条测试守的是一个真实踩过的坑：只改 buildDLPEvidence 是无效的，upstream 的
// insertEvent 与 BuildIssueSummaries 会各自再脱敏一次，把命中明文重新吃掉。
func TestDLPEvidencePassthroughKeepsPlaintext(t *testing.T) {
	got := redactEvidenceForBackend(evidencePlaintextSample, DLPScannerBackend, 160)
	if got != evidencePlaintextSample {
		t.Errorf("DLP 证据应原样透传\n期望=%q\n实际=%q", evidencePlaintextSample, got)
	}
	if strings.Contains(got, "***PHONE***") {
		t.Errorf("命中明文被脱敏：%q", got)
	}
	// 模拟读写两道叠加，明文仍须完好。
	twice := redactEvidenceForBackend(
		redactEvidenceForBackend(evidencePlaintextSample, DLPScannerBackend, 160),
		DLPScannerBackend, 160)
	if !strings.Contains(twice, "13912345678") {
		t.Errorf("两道透传后明文丢失：%q", twice)
	}
}

// TestNonDLPEvidenceStillRedacted qwen3guard 的证据必须保持原有脱敏行为。
// 它的证据来自模型自由文本，内容不可预期，脱敏是合理默认。
func TestNonDLPEvidenceStillRedacted(t *testing.T) {
	const backend = "qwen3guard-openai"
	got := redactEvidenceForBackend("联系电话 13912345678", backend, 160)
	if strings.Contains(got, "13912345678") {
		t.Errorf("非 DLP 后端的证据应继续脱敏，实际=%q", got)
	}
	if !strings.Contains(got, "***PHONE***") {
		t.Errorf("非 DLP 后端应命中 phonePattern 脱敏，实际=%q", got)
	}
}

// TestDLPEvidenceLengthAllowsContext 长度上限必须容得下上下文窗口。
// upstream 传的 160 会把上下文直接截掉，而上下文是判断误报的依据。
func TestDLPEvidenceLengthAllowsContext(t *testing.T) {
	long := "通用 API Key | 疑似真实API密钥 | 命中值 " + strings.Repeat("k", 128) +
		" | 位置 123456-123590 | 上下文 " + strings.Repeat("上", 48) + "⟦命中⟧" +
		strings.Repeat("下", 48)
	got := redactEvidenceForBackend(long, DLPScannerBackend, 160)
	if !strings.Contains(got, "⟦命中⟧") {
		t.Errorf("上下文标记被截断，说明长度上限过紧：%q", got)
	}
	if !strings.Contains(got, "位置 123456-123590") {
		t.Errorf("偏移量被截断：%q", got)
	}
}

// TestDLPIssueSummaryKeepsPlaintext 前端渲染用的 IssueSummary 也要保留明文。
// BuildIssueSummaries 是证据到达前端的最后一道，这里再脱敏一次等于前面全白做。
func TestDLPIssueSummaryKeepsPlaintext(t *testing.T) {
	result := NormalizedResult{
		Categories:      []string{DLPScannerPII},
		MatchedScanners: []string{DLPScannerPII},
		ScannerEvidence: map[string]string{DLPScannerPII: evidencePlaintextSample},
		ScannerScores:   map[string]float64{DLPScannerPII: 0.85},
		ScannerBackend:  DLPScannerBackend,
		RiskLevel:       RiskMedium,
		Action:          ActionWarn,
	}
	summaries := BuildIssueSummaries(result)
	if len(summaries) == 0 {
		t.Fatal("应产出 IssueSummary")
	}
	for _, summary := range summaries {
		if !strings.Contains(summary.Evidence, "13912345678") {
			t.Errorf("IssueSummary 里命中明文丢失：%q", summary.Evidence)
		}
		if strings.Contains(summary.Evidence, "***PHONE***") {
			t.Errorf("IssueSummary 里命中明文被脱敏：%q", summary.Evidence)
		}
	}
}

// TestQwen3GuardIssueSummaryStillRedacted 同一入口下 qwen3guard 仍须脱敏。
func TestQwen3GuardIssueSummaryStillRedacted(t *testing.T) {
	result := NormalizedResult{
		Categories:      []string{DLPScannerPII},
		MatchedScanners: []string{DLPScannerPII},
		ScannerEvidence: map[string]string{DLPScannerPII: "联系电话 13912345678"},
		ScannerScores:   map[string]float64{DLPScannerPII: 0.85},
		ScannerBackend:  "qwen3guard-openai",
		RiskLevel:       RiskMedium,
		Action:          ActionWarn,
	}
	for _, summary := range BuildIssueSummaries(result) {
		if strings.Contains(summary.Evidence, "13912345678") {
			t.Errorf("qwen3guard 的 IssueSummary 不应含明文：%q", summary.Evidence)
		}
	}
}
