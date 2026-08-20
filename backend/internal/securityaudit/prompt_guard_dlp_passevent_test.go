// prompt_guard_dlp_passevent_test.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：「正则命中但放行」写事件开关的测试。
//
// 覆盖三件事：
//   - 开关默认关闭时不写事件、也不调 RecordBlocking（后者会无条件插 jobs 行，
//     是这个功能最容易踩的坑）
//   - 打开后误报与降级各自落成可区分的事件
//   - 事件证据不含命中的敏感明文
//
// 与 upstream 合并策略：纯新增文件。
// =============================================================================
package securityaudit

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// dlpCapturingRepo 在 noop 之外记下传给 RecordBlocking 的结果，供断言 decision 等字段。
type dlpCapturingRepo struct {
	dlpNoopRepo
	mu       sync.Mutex
	captured []*NormalizedResult
}

func (r *dlpCapturingRepo) RecordBlocking(
	_ context.Context, _ PromptSnapshot, _ int64, result *NormalizedResult, _ bool,
) (*Event, error) {
	atomic.AddInt32(&r.recorded, 1)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.captured = append(r.captured, result)
	return nil, nil
}

func (r *dlpCapturingRepo) last() *NormalizedResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.captured) == 0 {
		return nil
	}
	return r.captured[len(r.captured)-1]
}

// dlpRecordHitsConfig 构造一份打开了「命中即记事件」的配置。
func dlpRecordHitsConfig(confirmURL string, record bool) ActiveConfig {
	cfg := dlpTestConfig(confirmURL, true)
	cfg.DLP.RecordRegexHits = record
	return cfg
}

const dlpPassEventText = "身份证号 110101199003072316 已核验"

// ---------- 开关关闭：保持原有行为 ----------

func TestDLPPassEventDisabledByDefault(t *testing.T) {
	// 默认关闭，误报不写事件——也不能调 RecordBlocking：它里面 insertJob 是
	// 无条件的，调一次 jobs 表就长一行，等于开关只挡住了一半。
	confirmServer, _ := newDLPConfirmStub(t, false, http.StatusOK)
	repo := &dlpCapturingRepo{}
	evaluator := newDLPTestEvaluator(&dlpStubScanner{}, repo)

	decision := evaluator.EvaluateDLP(context.Background(),
		dlpTestConfig(confirmServer.URL, true), dlpSnapshot(dlpPassEventText))

	if decision != nil {
		t.Fatalf("误报应放行，实际 %+v", decision)
	}
	if got := atomic.LoadInt32(&repo.recorded); got != 0 {
		t.Errorf("开关关闭时不应调用 RecordBlocking（会插 jobs 行），实际调用 %d 次", got)
	}
}

func TestDLPPassEventDegradedDisabledWritesNothing(t *testing.T) {
	// 降级路径同样受开关控制，关闭时只留 WARN 日志。
	confirmServer, _ := newDLPConfirmStub(t, true, http.StatusInternalServerError)
	repo := &dlpCapturingRepo{}
	evaluator := newDLPTestEvaluator(&dlpStubScanner{}, repo)

	evaluator.EvaluateDLP(context.Background(),
		dlpTestConfig(confirmServer.URL, true), dlpSnapshot(dlpPassEventText))

	if got := atomic.LoadInt32(&repo.recorded); got != 0 {
		t.Errorf("开关关闭时降级放行不应写事件，实际调用 %d 次", got)
	}
}

// ---------- 开关打开：两类放行各自落事件 ----------

func TestDLPPassEventRecordsFalsePositive(t *testing.T) {
	confirmServer, _ := newDLPConfirmStub(t, false, http.StatusOK)
	repo := &dlpCapturingRepo{}
	evaluator := newDLPTestEvaluator(&dlpStubScanner{}, repo)

	decision := evaluator.EvaluateDLP(context.Background(),
		dlpRecordHitsConfig(confirmServer.URL, true), dlpSnapshot(dlpPassEventText))

	// 写事件不改变放行判定。
	if decision != nil {
		t.Fatalf("记录事件不应改变放行结果，实际 %+v", decision)
	}
	result := repo.last()
	if result == nil {
		t.Fatal("开关打开时误报应写入事件")
	}
	if result.Decision != EventPass {
		t.Errorf("误报事件的 decision 应为 pass，实际 %q", result.Decision)
	}
	// low + Allow 表示"已确认无害"，与降级的 medium + Warn 区分开。
	if result.RiskLevel != RiskLow {
		t.Errorf("误报事件的 risk_level 应为 low，实际 %q", result.RiskLevel)
	}
	if result.Action != ActionAllow {
		t.Errorf("误报事件的 action 应为 Allow，实际 %q", result.Action)
	}
	if result.ScannerBackend != DLPScannerBackend {
		t.Errorf("事件必须带 DLP 的 scanner_backend 才能被 DLP 页面筛出来，实际 %q", result.ScannerBackend)
	}
	if len(result.Categories) == 0 {
		t.Error("事件缺少检测器分类，界面上看不出是哪类规则命中")
	}
}

func TestDLPPassEventRecordsDegradedDistinctly(t *testing.T) {
	// 降级放行是漏放风险，必须能和"确认过没事"区分开：事件表没有专门字段，
	// EventFilter 也只能按 decision/risk_level 筛，所以用更高的 risk_level 承载。
	confirmServer, _ := newDLPConfirmStub(t, true, http.StatusInternalServerError)
	repo := &dlpCapturingRepo{}
	evaluator := newDLPTestEvaluator(&dlpStubScanner{}, repo)

	evaluator.EvaluateDLP(context.Background(),
		dlpRecordHitsConfig(confirmServer.URL, true), dlpSnapshot(dlpPassEventText))

	result := repo.last()
	if result == nil {
		t.Fatal("开关打开时降级放行应写入事件")
	}
	if result.Decision != EventPass {
		t.Errorf("降级事件的 decision 应为 pass，实际 %q", result.Decision)
	}
	if result.RiskLevel != RiskMedium {
		t.Errorf("降级事件的 risk_level 应为 medium（便于单独筛出复查），实际 %q", result.RiskLevel)
	}
	if result.Action != ActionWarn {
		t.Errorf("降级事件的 action 应为 Warn，实际 %q", result.Action)
	}
	// 证据必须说明是"没能确认"，否则和误报事件在界面上分不出来。
	joined := strings.Join(evidenceValues(result), " ")
	if !strings.Contains(joined, "确认服务不可用") {
		t.Errorf("降级事件的证据应说明原因，实际 %q", joined)
	}
}

func TestDLPPassEventKeepsSensitiveHitsUnchanged(t *testing.T) {
	// 打开开关不应影响真命中的判定：仍是 flag/critical，不能被降成 pass。
	confirmServer, _ := newDLPConfirmStub(t, true, http.StatusOK)
	repo := &dlpCapturingRepo{}
	evaluator := newDLPTestEvaluator(&dlpStubScanner{}, repo)

	decision := evaluator.EvaluateDLP(context.Background(),
		dlpRecordHitsConfig(confirmServer.URL, true), dlpSnapshot(dlpPassEventText))

	if decision == nil {
		t.Fatal("确认为敏感时应产生决策")
	}
	result := repo.last()
	if result == nil {
		t.Fatal("确认为敏感应写入事件")
	}
	if result.Decision == EventPass {
		t.Error("真命中不应落成 pass 事件")
	}
}

// ---------- 明文不落库 ----------

func TestDLPPassEventEvidenceExcludesPlaintext(t *testing.T) {
	// 这类事件量大且长期留存，写明文等于把敏感数据又落一份盘。
	const idCard = "110101199003072316"
	confirmServer, _ := newDLPConfirmStub(t, false, http.StatusOK)
	repo := &dlpCapturingRepo{}
	evaluator := newDLPTestEvaluator(&dlpStubScanner{}, repo)

	evaluator.EvaluateDLP(context.Background(),
		dlpRecordHitsConfig(confirmServer.URL, true), dlpSnapshot("身份证号 "+idCard+" 已核验"))

	result := repo.last()
	if result == nil {
		t.Fatal("应写入事件")
	}
	for scannerID, evidence := range result.ScannerEvidence {
		if strings.Contains(evidence, idCard) {
			t.Errorf("检测器 %s 的证据里出现了命中明文: %q", scannerID, evidence)
		}
	}
}

// evidenceValues 取出全部证据文本，便于整体断言。
func evidenceValues(result *NormalizedResult) []string {
	values := make([]string, 0, len(result.ScannerEvidence))
	for _, evidence := range result.ScannerEvidence {
		values = append(values, evidence)
	}
	return values
}
