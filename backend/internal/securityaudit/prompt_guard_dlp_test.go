package securityaudit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// dlpStubScanner 记录 qwen3guard 是否被调用，用于验证 DLP 短路行为。
type dlpStubScanner struct {
	calls  int32
	result *NormalizedResult
	err    error
}

func (s *dlpStubScanner) Scan(
	_ context.Context, _ ActiveEndpoint, _ string, _ []string,
) (*NormalizedResult, error) {
	atomic.AddInt32(&s.calls, 1)
	if s.err != nil {
		return nil, s.err
	}
	if s.result != nil {
		return s.result, nil
	}
	return &NormalizedResult{
		Decision: EventPass, RiskLevel: RiskLow, Action: ActionAllow,
		Categories: []string{}, MatchedScanners: []string{},
		ScannerScores: map[string]float64{}, ScannerEvidence: map[string]string{},
	}, nil
}

// dlpNoopRepo 吞掉所有落库调用，让测试不依赖数据库。
type dlpNoopRepo struct {
	recorded int32
}

func (r *dlpNoopRepo) CreateStagingWithCapacity(
	context.Context, PromptSnapshot, int64, int, int,
) (*Job, error) {
	return nil, nil
}
func (r *dlpNoopRepo) PublishQueued(context.Context, int64) error                     { return nil }
func (r *dlpNoopRepo) MarkStagingFailed(context.Context, int64, string, string) error { return nil }
func (r *dlpNoopRepo) ClaimNextJob(context.Context, time.Time) (*Job, bool, error) {
	return nil, false, nil
}
func (r *dlpNoopRepo) RefreshLease(context.Context, int64, int64, time.Time) error { return nil }
func (r *dlpNoopRepo) Complete(context.Context, *Job, *NormalizedResult, bool) (*Event, error) {
	return nil, nil
}
func (r *dlpNoopRepo) Retry(context.Context, int64, int64, time.Time, string, string) error {
	return nil
}
func (r *dlpNoopRepo) Fail(context.Context, int64, int64, string, string) error { return nil }
func (r *dlpNoopRepo) ReclaimStale(context.Context, time.Time, time.Time, int) (int64, error) {
	return 0, nil
}
func (r *dlpNoopRepo) QueueStats(context.Context) (QueueStats, error) { return QueueStats{}, nil }
func (r *dlpNoopRepo) RecordBlocking(
	context.Context, PromptSnapshot, int64, *NormalizedResult, bool,
) (*Event, error) {
	atomic.AddInt32(&r.recorded, 1)
	return nil, nil
}

// dlpTestConfig 构造一份启用了 DLP 与 qwen3guard 的配置。
func dlpTestConfig(dlpEndpointURL string, blockHigh bool) ActiveConfig {
	dlp := ActiveDLPConfig{
		Enabled: true, ConfirmEnabled: dlpEndpointURL != "",
		BlockOnHighSeverity: blockHigh,
		ConfirmTimeout:      dlpConfirmTimeoutOrDefault(0),
	}
	if dlpEndpointURL != "" {
		dlp.Endpoints = []ActiveEndpoint{{
			ID: "dlp-1", BaseURL: dlpEndpointURL, Model: DefaultDLPConfirmModel,
			TimeoutMS: 5000, Enabled: true,
		}}
	}
	return ActiveConfig{
		RiskControlEnabled: true, Enabled: true, BlockingEnabled: true,
		ConfigVersion: 2, Scanners: AllScannerIDs,
		Endpoints: []ActiveEndpoint{{
			ID: "guard-1", BaseURL: "https://guard.example.test",
			Model: DefaultGuardModel, TimeoutMS: 3000, InputLimit: 4000, Enabled: true,
		}},
		DLP: dlp,
	}
}

func dlpSnapshot(text string) PromptSnapshot {
	return PromptSnapshot{
		RequestID: "req-dlp", UserID: 7, APIKeyID: 3,
		Provider: "openai", Protocol: "openai_chat", Endpoint: "/v1/chat/completions",
		Model: "gpt-test", Stage: "http", ScanText: text, PromptLength: len(text),
	}
}

// newDLPConfirmStub 起一个假的确认服务。
func newDLPConfirmStub(t *testing.T, sensitive bool, status int) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		reply := map[string]any{"choices": []map[string]any{{"message": map[string]string{
			"content": `{"results":[{"i":1,"sensitive":` + boolText(sensitive) + `,"reason":"r"}]}`,
		}}}}
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func newDLPTestEvaluator(scanner PromptScanner, repo JobRepository) *GuardEvaluator {
	return NewGuardEvaluator(scanner, repo, NewAtomicMetrics()).
		WithDLP(NewDLPConfirmer(), NewDLPConfirmCache(nil))
}

// ---------- 核心行为：正则未命中不产生任何网络调用 ----------

func TestDLPGuardCleanTextSkipsConfirmNetwork(t *testing.T) {
	confirmServer, confirmCalls := newDLPConfirmStub(t, true, http.StatusOK)
	scanner := &dlpStubScanner{}
	evaluator := newDLPTestEvaluator(scanner, &dlpNoopRepo{})

	decision := evaluator.EvaluateDLP(context.Background(),
		dlpTestConfig(confirmServer.URL, true),
		dlpSnapshot("帮我写一个快速排序的 Python 实现"))

	if decision != nil {
		t.Fatalf("正则未命中时应返回 nil 让流程继续，实际 %+v", decision)
	}
	if got := atomic.LoadInt32(confirmCalls); got != 0 {
		t.Errorf("确认服务调用次数 = %d, 期望 0（正则未命中不应产生网络调用）", got)
	}
}

func TestDLPGuardRegexExcludedSkipsConfirmNetwork(t *testing.T) {
	// 正则命中但被排除链丢弃时，同样不应产生网络调用。
	confirmServer, confirmCalls := newDLPConfirmStub(t, true, http.StatusOK)
	evaluator := newDLPTestEvaluator(&dlpStubScanner{}, &dlpNoopRepo{})

	decision := evaluator.EvaluateDLP(context.Background(),
		dlpTestConfig(confirmServer.URL, true),
		dlpSnapshot(`api_key = "your-api-key-here"`))

	if decision != nil {
		t.Fatalf("命中被排除后应返回 nil，实际 %+v", decision)
	}
	if got := atomic.LoadInt32(confirmCalls); got != 0 {
		t.Errorf("确认服务调用次数 = %d, 期望 0（被排除的命中不应送确认）", got)
	}
}

func TestDLPGuardDisabledSkipsEverything(t *testing.T) {
	confirmServer, confirmCalls := newDLPConfirmStub(t, true, http.StatusOK)
	evaluator := newDLPTestEvaluator(&dlpStubScanner{}, &dlpNoopRepo{})
	cfg := dlpTestConfig(confirmServer.URL, true)
	cfg.DLP.Enabled = false

	decision := evaluator.EvaluateDLP(context.Background(), cfg,
		dlpSnapshot("身份证 110101199003072316"))

	if decision != nil {
		t.Fatalf("DLP 关闭时应返回 nil，实际 %+v", decision)
	}
	if got := atomic.LoadInt32(confirmCalls); got != 0 {
		t.Errorf("DLP 关闭时不应调用确认服务，实际 %d 次", got)
	}
}

// ---------- 拦截与分级 ----------

func TestDLPGuardBlocksConfirmedHighSeverity(t *testing.T) {
	confirmServer, _ := newDLPConfirmStub(t, true, http.StatusOK)
	repo := &dlpNoopRepo{}
	evaluator := newDLPTestEvaluator(&dlpStubScanner{}, repo)

	decision := evaluator.EvaluateDLP(context.Background(),
		dlpTestConfig(confirmServer.URL, true),
		dlpSnapshot("身份证号 110101199003072316 已核验"))

	if decision == nil {
		t.Fatal("确认为真实高危敏感信息时应拦截")
	}
	if decision.Kind != DecisionBlock {
		t.Errorf("决策 = %s, 期望 %s", decision.Kind, DecisionBlock)
	}
	if decision.ErrorCode != ErrorCodeDLPBlocked {
		t.Errorf("错误码 = %s, 期望 %s", decision.ErrorCode, ErrorCodeDLPBlocked)
	}
	if decision.Result == nil {
		t.Fatal("拦截决策应带 NormalizedResult 以便落库与前端展示")
	}
	if decision.Result.Action != ActionBlock {
		t.Errorf("Action = %s, 期望 Block", decision.Result.Action)
	}
	if decision.Result.ScannerBackend != DLPScannerBackend {
		t.Errorf("ScannerBackend = %s, 期望 %s", decision.Result.ScannerBackend, DLPScannerBackend)
	}
	if atomic.LoadInt32(&repo.recorded) == 0 {
		t.Error("拦截应写入审计事件")
	}
}

func TestDLPGuardMediumSeverityFlagsOnly(t *testing.T) {
	// 按 detection-rules.md 处置矩阵，手机号是 medium：仅审计不拦截。
	confirmServer, _ := newDLPConfirmStub(t, true, http.StatusOK)
	evaluator := newDLPTestEvaluator(&dlpStubScanner{}, &dlpNoopRepo{})

	decision := evaluator.EvaluateDLP(context.Background(),
		dlpTestConfig(confirmServer.URL, true),
		dlpSnapshot("联系电话 13704251983 请回电"))

	if decision == nil {
		t.Fatal("medium 命中应返回 flag 决策以便留档")
	}
	if decision.Kind != DecisionFlag {
		t.Errorf("决策 = %s, 期望 %s（medium 不拦截）", decision.Kind, DecisionFlag)
	}
	if !decision.AllowNextStage {
		t.Error("flag 决策应允许请求继续")
	}
}

func TestDLPGuardBlockSwitchOff(t *testing.T) {
	confirmServer, _ := newDLPConfirmStub(t, true, http.StatusOK)
	evaluator := newDLPTestEvaluator(&dlpStubScanner{}, &dlpNoopRepo{})

	decision := evaluator.EvaluateDLP(context.Background(),
		dlpTestConfig(confirmServer.URL, false), // BlockOnHighSeverity=false
		dlpSnapshot("身份证号 110101199003072316 已核验"))

	if decision == nil {
		t.Fatal("应返回 flag 决策")
	}
	if decision.Kind != DecisionFlag {
		t.Errorf("关闭拦截开关时决策 = %s, 期望 %s", decision.Kind, DecisionFlag)
	}
}

func TestDLPGuardFalsePositiveAllows(t *testing.T) {
	// 模型判为误报时应放行，且不写审计事件。
	// 「不写事件」是 RecordRegexHits 关闭（默认）时的行为；打开后的行为见
	// prompt_guard_dlp_passevent_test.go。
	confirmServer, confirmCalls := newDLPConfirmStub(t, false, http.StatusOK)
	repo := &dlpNoopRepo{}
	evaluator := newDLPTestEvaluator(&dlpStubScanner{}, repo)

	decision := evaluator.EvaluateDLP(context.Background(),
		dlpTestConfig(confirmServer.URL, true),
		dlpSnapshot("身份证号 110101199003072316 已核验"))

	if decision != nil {
		t.Fatalf("模型判为误报时应放行，实际 %+v", decision)
	}
	if got := atomic.LoadInt32(confirmCalls); got != 1 {
		t.Errorf("应实际调用确认服务 1 次，实际 %d 次", got)
	}
	if atomic.LoadInt32(&repo.recorded) != 0 {
		t.Error("误报放行不应写入审计事件")
	}
}

// ---------- fail-open 降级 ----------

func TestDLPGuardConfirmFailureFailsOpen(t *testing.T) {
	// 确认服务 500 时必须放行（fail-open），绝不能变成 503 拖垮网关。
	confirmServer, _ := newDLPConfirmStub(t, true, http.StatusInternalServerError)
	evaluator := newDLPTestEvaluator(&dlpStubScanner{}, &dlpNoopRepo{})

	decision := evaluator.EvaluateDLP(context.Background(),
		dlpTestConfig(confirmServer.URL, true),
		dlpSnapshot("身份证号 110101199003072316 已核验"))

	if decision != nil {
		t.Fatalf("确认失败应 fail-open 放行，实际 %+v（这会导致网关 5xx）", decision)
	}
}

func TestDLPGuardNoConfirmEndpointFailsOpen(t *testing.T) {
	// 开了二次确认但没有可用节点：放行，不能拦也不能 503。
	evaluator := newDLPTestEvaluator(&dlpStubScanner{}, &dlpNoopRepo{})
	cfg := dlpTestConfig("", true)
	cfg.DLP.ConfirmEnabled = true // 无 endpoint

	decision := evaluator.EvaluateDLP(context.Background(), cfg,
		dlpSnapshot("身份证号 110101199003072316 已核验"))

	if decision != nil {
		t.Fatalf("无可用确认节点时应 fail-open 放行，实际 %+v", decision)
	}
}

func TestDLPGuardConfirmDisabledTrustsRegex(t *testing.T) {
	// 关闭二次确认时，正则命中即判定敏感（管理员自行承担误报）。
	evaluator := newDLPTestEvaluator(&dlpStubScanner{}, &dlpNoopRepo{})
	cfg := dlpTestConfig("", true)
	cfg.DLP.ConfirmEnabled = false

	decision := evaluator.EvaluateDLP(context.Background(), cfg,
		dlpSnapshot("身份证号 110101199003072316 已核验"))

	if decision == nil {
		t.Fatal("关闭二次确认时正则命中应直接判定")
	}
	if decision.Kind != DecisionBlock {
		t.Errorf("决策 = %s, 期望 %s", decision.Kind, DecisionBlock)
	}
}

// ---------- 与 qwen3guard 的关系 ----------

func TestDLPGuardBlocksOnConfirmedSensitiveData(t *testing.T) {
	// EvaluateDLP 自身的职责：确认为敏感的高危命中要给出拦截决策。
	// 「拦截后不再调 qwen3guard」的短路语义已上移到 Coordinator 层，
	// 由 TestCoordinatorDLPBlockShortCircuitsPromptEngine 覆盖。
	confirmServer, _ := newDLPConfirmStub(t, true, http.StatusOK)
	evaluator := newDLPTestEvaluator(&dlpStubScanner{}, &dlpNoopRepo{})

	decision := evaluator.EvaluateDLP(context.Background(),
		dlpTestConfig(confirmServer.URL, true),
		dlpSnapshot("身份证号 110101199003072316 已核验"))

	if decision == nil || decision.Kind != DecisionBlock {
		t.Fatalf("应由 DLP 拦截，实际 %+v", decision)
	}
	if decision.ErrorCode != ErrorCodeDLPBlocked {
		t.Errorf("错误码 = %q, 期望 %q", decision.ErrorCode, ErrorCodeDLPBlocked)
	}
}

func TestDLPGuardCleanTextReturnsNilSoUpstreamContinues(t *testing.T) {
	// DLP 未命中必须返回 nil，让请求继续走 upstream 的内容安全流程，
	// 不能吞掉 qwen3guard。
	evaluator := newDLPTestEvaluator(&dlpStubScanner{}, &dlpNoopRepo{})

	decision := evaluator.EvaluateDLP(context.Background(),
		dlpTestConfig("", true),
		dlpSnapshot("帮我写一个快速排序"))

	if decision != nil {
		t.Errorf("正则未命中应返回 nil，实际 %+v", decision)
	}
}

func TestDLPGuardZeroConfigBehavesLikeUpstream(t *testing.T) {
	// DLP 配置为零值时（upstream 配置文件里没有 dlp 字段），
	// Evaluate 的行为必须与改动前完全一致。
	scanner := &dlpStubScanner{}
	evaluator := newDLPTestEvaluator(scanner, &dlpNoopRepo{})
	cfg := dlpTestConfig("", false)
	cfg.DLP = ActiveDLPConfig{}

	_, err := evaluator.Evaluate(context.Background(), cfg,
		dlpSnapshot("身份证号 110101199003072316 已核验"))

	if err != nil {
		t.Fatalf("Evaluate 返回错误: %v", err)
	}
	if got := atomic.LoadInt32(&scanner.calls); got == 0 {
		t.Error("DLP 零值配置下应照常走 qwen3guard")
	}
}

// ---------- 对外错误码与文案 ----------

func TestDLPBlockSurfacesOwnErrorCodeAndMessage(t *testing.T) {
	// upstream 的 prioritize 原本硬编码 qwen3guard 的错误码与文案，会把 DLP 拦截
	// 伪装成「提示词安全审计拒绝」，运维在 API 边界无法区分两套拦截器。
	// merge upstream 后若该 hook 被覆盖，本测试会失败。
	prompt := &PromptDecision{
		Kind: DecisionBlock, ErrorCode: ErrorCodeDLPBlocked,
		Result: &NormalizedResult{Decision: EventCritical, RiskLevel: RiskHigh, Action: ActionBlock},
	}
	decision := prioritize(nil, prompt)
	if decision.ErrorCode != ErrorCodeDLPBlocked {
		t.Errorf("对外 ErrorCode = %q, 期望 %q", decision.ErrorCode, ErrorCodeDLPBlocked)
	}
	if decision.ClientMessage != DLPClientMessage {
		t.Errorf("对外 ClientMessage = %q, 期望 DLP 专属文案", decision.ClientMessage)
	}
	if decision.Kind != DecisionBlock {
		t.Errorf("决策 = %s, 期望 %s", decision.Kind, DecisionBlock)
	}
}

func TestQwen3GuardBlockKeepsUpstreamErrorCode(t *testing.T) {
	// DLP 的改动不能影响 qwen3guard 拦截的对外表现。
	prompt := &PromptDecision{
		Kind: DecisionBlock, ErrorCode: ErrorCodeBlocked,
		Result: &NormalizedResult{Decision: EventCritical, RiskLevel: RiskHigh, Action: ActionBlock},
	}
	decision := prioritize(nil, prompt)
	if decision.ErrorCode != ErrorCodeBlocked {
		t.Errorf("qwen3guard 拦截的 ErrorCode = %q, 期望 %q", decision.ErrorCode, ErrorCodeBlocked)
	}
	if decision.ClientMessage == DLPClientMessage {
		t.Error("qwen3guard 拦截不应使用 DLP 的客户端文案")
	}
}

func TestDLPClientMessageExcludesMatchedContent(t *testing.T) {
	// 客户端文案不能回显命中内容，否则等于把敏感片段又吐回响应体。
	for _, fragment := range []string{"110101", "13704251983", "AKIA", "password"} {
		if contains(DLPClientMessage, fragment) {
			t.Errorf("DLP 客户端文案不应包含命中内容样例：%s", fragment)
		}
	}
}

// ---------- 证据脱敏 ----------

func TestDLPGuardEvidenceExcludesPlaintext(t *testing.T) {
	const secret = "110101199003072316"
	confirmServer, _ := newDLPConfirmStub(t, true, http.StatusOK)
	evaluator := newDLPTestEvaluator(&dlpStubScanner{}, &dlpNoopRepo{})

	decision := evaluator.EvaluateDLP(context.Background(),
		dlpTestConfig(confirmServer.URL, true),
		dlpSnapshot("身份证号 "+secret+" 已核验"))

	if decision == nil || decision.Result == nil {
		t.Fatal("应产出拦截决策")
	}
	for scannerID, evidence := range decision.Result.ScannerEvidence {
		if contains(evidence, secret) {
			t.Errorf("scanner %s 的证据包含敏感明文：%q", scannerID, evidence)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		func() bool {
			for index := 0; index+len(needle) <= len(haystack); index++ {
				if haystack[index:index+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}
