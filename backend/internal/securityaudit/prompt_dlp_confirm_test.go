package securityaudit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// dlpTestFindings 构造若干 finding 用于确认测试。
func dlpTestFindings(values ...string) []DLPFinding {
	findings := make([]DLPFinding, 0, len(values))
	for _, value := range values {
		findings = append(findings, DLPFinding{
			RuleID: "pii-phone", Class: DLPClassPII, ScannerID: DLPScannerPII,
			Title: "手机号", Severity: RiskMedium, Score: 0.85,
			Match: value, Value: value,
		})
	}
	return findings
}

// newDLPConfirmServer 起一个假的 chat completions 服务，返回预设的 JSON 内容。
func newDLPConfirmServer(t *testing.T, handler func(requestBody string) (int, string)) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("请求路径 = %s, 期望 /v1/chat/completions", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		status, content := handler(string(raw))
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"upstream failure"}`))
			return
		}
		reply := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": content}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func dlpTestEndpoint(baseURL string) ActiveEndpoint {
	return ActiveEndpoint{
		ID: "dlp-1", Name: "dlp-confirm", BaseURL: baseURL,
		Model: DefaultDLPConfirmModel, TimeoutMS: 5000, Enabled: true,
	}
}

func TestDLPConfirmParsesVerdicts(t *testing.T) {
	server, calls := newDLPConfirmServer(t, func(string) (int, string) {
		return http.StatusOK, `{"results":[
			{"i":1,"sensitive":true,"reason":"真实手机号"},
			{"i":2,"sensitive":false,"reason":"测试号码"}
		]}`
	})
	findings := dlpTestFindings("13912345678", "13800000000")
	verdicts, err := NewDLPConfirmer().Confirm(context.Background(), dlpTestEndpoint(server.URL), findings)
	if err != nil {
		t.Fatalf("Confirm 返回错误: %v", err)
	}
	if len(verdicts) != 2 {
		t.Fatalf("结论数量 = %d, 期望 2", len(verdicts))
	}
	if !verdicts[0].Sensitive || !verdicts[0].Confirmed {
		t.Errorf("第 1 条应为已确认的敏感，实际 %+v", verdicts[0])
	}
	if verdicts[1].Sensitive || !verdicts[1].Confirmed {
		t.Errorf("第 2 条应为已确认的误报，实际 %+v", verdicts[1])
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("HTTP 调用次数 = %d, 期望 1（两条命中应合并为一次请求）", got)
	}
}

func TestDLPConfirmSendsJSONResponseFormat(t *testing.T) {
	var captured string
	server, _ := newDLPConfirmServer(t, func(body string) (int, string) {
		captured = body
		return http.StatusOK, `{"results":[{"i":1,"sensitive":false,"reason":"占位符"}]}`
	})
	_, err := NewDLPConfirmer().Confirm(context.Background(),
		dlpTestEndpoint(server.URL), dlpTestFindings("13912345678"))
	if err != nil {
		t.Fatalf("Confirm 返回错误: %v", err)
	}
	if !strings.Contains(captured, `"response_format"`) {
		t.Error("请求应带 response_format 以强制 JSON 输出")
	}
	if !strings.Contains(captured, `"json_object"`) {
		t.Error("response_format 应为 json_object")
	}
	if !strings.Contains(captured, DefaultDLPConfirmModel) {
		t.Errorf("请求应使用 endpoint 配置的模型 %s", DefaultDLPConfirmModel)
	}
}

func TestDLPConfirmDoesNotLeakFullPrompt(t *testing.T) {
	// 只应外送命中片段，不应把整篇原文发给确认模型。
	var captured string
	server, _ := newDLPConfirmServer(t, func(body string) (int, string) {
		captured = body
		return http.StatusOK, `{"results":[{"i":1,"sensitive":true,"reason":"真实手机号"}]}`
	})
	findings := dlpTestFindings("13912345678")
	_, err := NewDLPConfirmer().Confirm(context.Background(), dlpTestEndpoint(server.URL), findings)
	if err != nil {
		t.Fatalf("Confirm 返回错误: %v", err)
	}
	if !strings.Contains(captured, "13912345678") {
		t.Error("请求应包含命中片段")
	}
	if strings.Contains(captured, "这是不该外送的其余上下文") {
		t.Error("请求不应包含命中片段之外的原文")
	}
}

func TestDLPConfirmBatchesLargeFindingSets(t *testing.T) {
	// 超过单批上限时应拆成多次请求，且每条都拿到结论。
	total := maxDLPConfirmBatchSize + 3
	values := make([]string, 0, total)
	for index := 0; index < total; index++ {
		values = append(values, "1391234567"+string(rune('0'+index%10)))
	}
	server, calls := newDLPConfirmServer(t, func(body string) (int, string) {
		// 按本批实际条数回复，条数从提示词里的声明推断即可。
		count := strings.Count(body, "命中片段")
		var builder strings.Builder
		builder.WriteString(`{"results":[`)
		for index := 0; index < count; index++ {
			if index > 0 {
				builder.WriteString(",")
			}
			builder.WriteString(`{"i":`)
			builder.WriteString(itoa(index + 1))
			builder.WriteString(`,"sensitive":true,"reason":"x"}`)
		}
		builder.WriteString(`]}`)
		return http.StatusOK, builder.String()
	})
	verdicts, err := NewDLPConfirmer().Confirm(context.Background(),
		dlpTestEndpoint(server.URL), dlpTestFindings(values...))
	if err != nil {
		t.Fatalf("Confirm 返回错误: %v", err)
	}
	if len(verdicts) != total {
		t.Fatalf("结论数量 = %d, 期望 %d", len(verdicts), total)
	}
	for index, verdict := range verdicts {
		if !verdict.Confirmed {
			t.Errorf("第 %d 条未拿到确认结论", index+1)
		}
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Errorf("HTTP 调用次数 = %d, 期望 2（%d 条拆两批）", got, total)
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 4)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func TestDLPConfirmStripsMarkdownCodeFence(t *testing.T) {
	server, _ := newDLPConfirmServer(t, func(string) (int, string) {
		return http.StatusOK, "```json\n{\"results\":[{\"i\":1,\"sensitive\":true,\"reason\":\"ok\"}]}\n```"
	})
	verdicts, err := NewDLPConfirmer().Confirm(context.Background(),
		dlpTestEndpoint(server.URL), dlpTestFindings("13912345678"))
	if err != nil {
		t.Fatalf("模型套 markdown 围栏时应能解析: %v", err)
	}
	if !verdicts[0].Sensitive || !verdicts[0].Confirmed {
		t.Errorf("结论 = %+v, 期望已确认的敏感", verdicts[0])
	}
}

func TestDLPConfirmMissingItemStaysUnconfirmed(t *testing.T) {
	// 模型漏返回第 2 条时，第 2 条必须保持 Confirmed=false，
	// 绝不能被当成"模型判为误报"而放行。
	server, _ := newDLPConfirmServer(t, func(string) (int, string) {
		return http.StatusOK, `{"results":[{"i":1,"sensitive":true,"reason":"真实"}]}`
	})
	verdicts, err := NewDLPConfirmer().Confirm(context.Background(),
		dlpTestEndpoint(server.URL), dlpTestFindings("13912345678", "13712345678"))
	if err != nil {
		t.Fatalf("Confirm 返回错误: %v", err)
	}
	if !verdicts[0].Confirmed {
		t.Error("第 1 条应已确认")
	}
	if verdicts[1].Confirmed {
		t.Error("模型漏返回的第 2 条必须保持未确认状态")
	}
	if verdicts[1].Sensitive {
		t.Error("未确认的结论不应带 Sensitive=true")
	}
}

func TestDLPConfirmOutOfRangeIndexIgnored(t *testing.T) {
	server, _ := newDLPConfirmServer(t, func(string) (int, string) {
		return http.StatusOK, `{"results":[
			{"i":1,"sensitive":true,"reason":"ok"},
			{"i":99,"sensitive":true,"reason":"越界"},
			{"i":0,"sensitive":true,"reason":"越界"}
		]}`
	})
	verdicts, err := NewDLPConfirmer().Confirm(context.Background(),
		dlpTestEndpoint(server.URL), dlpTestFindings("13912345678"))
	if err != nil {
		t.Fatalf("越界编号不应导致失败: %v", err)
	}
	if len(verdicts) != 1 || !verdicts[0].Confirmed {
		t.Errorf("应只接受合法编号的结论，实际 %+v", verdicts)
	}
}

func TestDLPConfirmUpstreamErrorReturnsError(t *testing.T) {
	server, _ := newDLPConfirmServer(t, func(string) (int, string) {
		return http.StatusTooManyRequests, ""
	})
	_, err := NewDLPConfirmer().Confirm(context.Background(),
		dlpTestEndpoint(server.URL), dlpTestFindings("13912345678"))
	if err == nil {
		t.Fatal("上游返回 429 时应返回错误，交由调用方按 fail-open 降级")
	}
	var guardErr *GuardError
	if !asGuardError(err, &guardErr) {
		t.Fatalf("错误类型 = %T, 期望 *GuardError", err)
	}
	if guardErr.HTTPStatus != http.StatusTooManyRequests {
		t.Errorf("HTTPStatus = %d, 期望 429", guardErr.HTTPStatus)
	}
	if !guardErr.Retryable {
		t.Error("429 应标记为可重试")
	}
}

func TestDLPConfirmInvalidJSONReturnsError(t *testing.T) {
	server, _ := newDLPConfirmServer(t, func(string) (int, string) {
		return http.StatusOK, "这不是 JSON"
	})
	_, err := NewDLPConfirmer().Confirm(context.Background(),
		dlpTestEndpoint(server.URL), dlpTestFindings("13912345678"))
	if err == nil {
		t.Fatal("模型返回非 JSON 时应返回错误")
	}
}

func TestDLPConfirmEmptyResultsReturnsError(t *testing.T) {
	server, _ := newDLPConfirmServer(t, func(string) (int, string) {
		return http.StatusOK, `{"results":[]}`
	})
	_, err := NewDLPConfirmer().Confirm(context.Background(),
		dlpTestEndpoint(server.URL), dlpTestFindings("13912345678"))
	if err == nil {
		t.Fatal("模型返回空 results 时应返回错误，而不是静默放行")
	}
}

func TestDLPConfirmNoFindingsSkipsNetwork(t *testing.T) {
	server, calls := newDLPConfirmServer(t, func(string) (int, string) {
		return http.StatusOK, `{"results":[]}`
	})
	verdicts, err := NewDLPConfirmer().Confirm(context.Background(), dlpTestEndpoint(server.URL), nil)
	if err != nil {
		t.Fatalf("空 finding 不应报错: %v", err)
	}
	if len(verdicts) != 0 {
		t.Errorf("结论数量 = %d, 期望 0", len(verdicts))
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Errorf("HTTP 调用次数 = %d, 期望 0（无命中不应产生网络调用）", got)
	}
}

func TestDLPConfirmContextCancellation(t *testing.T) {
	server, _ := newDLPConfirmServer(t, func(string) (int, string) {
		time.Sleep(200 * time.Millisecond)
		return http.StatusOK, `{"results":[{"i":1,"sensitive":true,"reason":"ok"}]}`
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := NewDLPConfirmer().Confirm(ctx, dlpTestEndpoint(server.URL), dlpTestFindings("13912345678"))
	if err == nil {
		t.Fatal("上下文超时应返回错误")
	}
}

// asGuardError 是 errors.As 的薄封装，避免测试文件重复 import。
func asGuardError(err error, target **GuardError) bool {
	for err != nil {
		if guardErr, ok := err.(*GuardError); ok {
			*target = guardErr
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

// ---------- 配置 ----------

func TestDLPConfigZeroValueIsDisabled(t *testing.T) {
	// upstream 配置里没有 dlp 字段时，行为必须与改动前完全一致。
	var cfg DLPConfig
	if cfg.Enabled {
		t.Error("零值配置应为关闭状态")
	}
	if err := ValidateDLPConfig(cfg); err != nil {
		t.Errorf("零值配置应校验通过，实际 %v", err)
	}
	active := cfg.ToActiveDLPConfig(nil)
	if active.Enabled || active.ConfirmReady() {
		t.Error("零值配置转出的运行时视图应为关闭且确认链路不可用")
	}
}

func TestDLPConfigOmitsEmptyFieldsInJSON(t *testing.T) {
	// 序列化后不应出现 dlp 相关字段，保证与 upstream 配置字节兼容。
	raw, err := json.Marshal(DLPConfig{})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if string(raw) != "{}" {
		t.Errorf("零值 DLPConfig 序列化 = %s, 期望 {}", raw)
	}
}

func TestDLPConfigEffectiveScanners(t *testing.T) {
	empty := ActiveDLPConfig{}
	if len(empty.EffectiveScanners()) != len(DLPScannerIDs()) {
		t.Error("未指定 Scanners 时应视为全部启用")
	}
	partial := ActiveDLPConfig{Scanners: []string{DLPScannerPII, "not-a-dlp-scanner"}}
	got := partial.EffectiveScanners()
	if len(got) != 1 || got[0] != DLPScannerPII {
		t.Errorf("EffectiveScanners = %v, 期望仅保留合法的 DLP ID", got)
	}
}

func TestDLPConfigValidation(t *testing.T) {
	base := func() DLPConfig {
		return DLPConfig{
			Enabled: true, ConfirmEnabled: true, AllGroups: true,
			Endpoints: []StorageEndpoint{
				{ID: "e1", BaseURL: "https://api.example.com", Model: DefaultDLPConfirmModel, Enabled: true},
			},
		}
	}
	if err := ValidateDLPConfig(base()); err != nil {
		t.Fatalf("合法配置应校验通过: %v", err)
	}

	// 启用却没有任何生效范围时 DLP 会静默不工作，必须在保存时就拒掉。
	noScope := base()
	noScope.AllGroups = false
	if err := ValidateDLPConfig(noScope); err == nil {
		t.Error("启用 DLP 但未指定任何分组范围时应报错")
	}

	scopedToGroups := base()
	scopedToGroups.AllGroups = false
	scopedToGroups.GroupIDs = []int64{7}
	if err := ValidateDLPConfig(scopedToGroups); err != nil {
		t.Errorf("指定分组的配置应校验通过: %v", err)
	}

	noEndpoint := base()
	noEndpoint.Endpoints = nil
	if err := ValidateDLPConfig(noEndpoint); err == nil {
		t.Error("启用二次确认但无确认节点时应报错")
	}

	badScanner := base()
	badScanner.Scanners = []string{"violent"}
	if err := ValidateDLPConfig(badScanner); err == nil {
		t.Error("非 DLP 的 scanner ID 应被拒绝")
	}

	badTimeout := base()
	badTimeout.ConfirmTimeoutMS = 10
	if err := ValidateDLPConfig(badTimeout); err == nil {
		t.Error("过小的确认超时应被拒绝")
	}

	badTTL := base()
	badTTL.CacheBenignTTLHours = MaxDLPCacheTTLHours + 1
	if err := ValidateDLPConfig(badTTL); err == nil {
		t.Error("超限的缓存 TTL 应被拒绝")
	}

	badURL := base()
	badURL.Endpoints[0].BaseURL = "ftp://example.com"
	if err := ValidateDLPConfig(badURL); err == nil {
		t.Error("非 HTTP(S) 的节点地址应被拒绝")
	}

	// 关闭状态允许保存半成品配置。
	disabled := base()
	disabled.Enabled = false
	disabled.Endpoints = nil
	if err := ValidateDLPConfig(disabled); err != nil {
		t.Errorf("关闭状态应允许保存半成品配置，实际 %v", err)
	}
}

func TestDLPConfigDefaultsApplied(t *testing.T) {
	cfg := DLPConfig{
		Enabled: true, ConfirmEnabled: true,
		Endpoints: []StorageEndpoint{{ID: "e1", BaseURL: "https://api.example.com", Enabled: true}},
	}
	active := cfg.ToActiveDLPConfig(nil)
	if len(active.Endpoints) != 1 {
		t.Fatalf("节点数量 = %d, 期望 1", len(active.Endpoints))
	}
	if active.Endpoints[0].Model != DefaultDLPConfirmModel {
		t.Errorf("未配置模型时应回落到 %s, 实际 %s",
			DefaultDLPConfirmModel, active.Endpoints[0].Model)
	}
	if active.ConfirmTimeout <= 0 {
		t.Error("确认超时应有默认值")
	}
	if !active.ConfirmReady() {
		t.Error("配置完整时确认链路应可用")
	}
}

func TestDLPConfigTokenDecryptFailureMarksInvalid(t *testing.T) {
	cfg := DLPConfig{
		Enabled: true, ConfirmEnabled: true,
		Endpoints: []StorageEndpoint{
			{ID: "e1", BaseURL: "https://api.example.com", TokenCiphertext: "broken", Enabled: true},
		},
	}
	active := cfg.ToActiveDLPConfig(func(string) (string, error) {
		return "", io.ErrUnexpectedEOF
	})
	if !active.Endpoints[0].TokenInvalid {
		t.Error("token 解密失败应标记 TokenInvalid")
	}
	if len(active.EnabledEndpoints()) != 0 {
		t.Error("TokenInvalid 的节点不应参与运行时调用")
	}
	if active.ConfirmReady() {
		t.Error("无可用节点时确认链路应判定为不可用")
	}
}

func TestDLPConfigDecryptsToken(t *testing.T) {
	cfg := DLPConfig{
		Enabled: true, ConfirmEnabled: true,
		Endpoints: []StorageEndpoint{
			{ID: "e1", BaseURL: "https://api.example.com", TokenCiphertext: "cipher", Enabled: true},
		},
	}
	active := cfg.ToActiveDLPConfig(func(cipher string) (string, error) {
		return "plain-" + cipher, nil
	})
	if active.Endpoints[0].Token != "plain-cipher" {
		t.Errorf("Token = %q, 期望解密后的明文", active.Endpoints[0].Token)
	}
	if active.Endpoints[0].TokenInvalid {
		t.Error("解密成功不应标记 TokenInvalid")
	}
}

// ---------- 缓存 ----------

func TestDLPCacheNilClientIsSafeNoop(t *testing.T) {
	cache := NewDLPConfirmCache(nil)
	findings := dlpTestFindings("13912345678")
	verdicts := cache.Lookup(context.Background(), findings)
	if len(verdicts) != 1 {
		t.Fatalf("结论数量 = %d, 期望 1", len(verdicts))
	}
	if verdicts[0].Confirmed {
		t.Error("无 Redis 时应全部未命中")
	}
	// 不应 panic
	cache.Store(context.Background(), findings, verdicts, 0, 0)
}

func TestDLPCacheKeyExcludesPlaintext(t *testing.T) {
	const secret = "13912345678"
	key := dlpCacheKey("pii-phone", secret)
	if strings.Contains(key, secret) {
		t.Errorf("缓存 key 不得包含敏感明文，实际 %q", key)
	}
	if !strings.HasPrefix(key, DLPCacheKeyPrefix) {
		t.Errorf("缓存 key 应带统一前缀，实际 %q", key)
	}
}

func TestDLPCacheKeyNormalizesWhitespace(t *testing.T) {
	if dlpCacheKey("pii-phone", " 13912345678 ") != dlpCacheKey("pii-phone", "13912345678") {
		t.Error("首尾空白不应影响缓存 key")
	}
}

func TestDLPCacheKeyIsRuleScoped(t *testing.T) {
	if dlpCacheKey("pii-phone", "13912345678") == dlpCacheKey("pii-bankcard", "13912345678") {
		t.Error("不同规则下的同一片段应使用不同缓存 key")
	}
}

func TestDLPCacheTTLDefaults(t *testing.T) {
	// TTL 刻意不存在结构体里，改由 Store 调用方按当次生效的配置传入，
	// 否则管理员在后台改的 TTL 会被静默忽略。这里验证归一化逻辑。
	sensitive, benign := dlpCacheTTL(0, 0)
	if sensitive != DefaultDLPCacheSensitiveTTL {
		t.Errorf("sensitiveTTL = %v, 期望默认值", sensitive)
	}
	if benign != DefaultDLPCacheBenignTTL {
		t.Errorf("benignTTL = %v, 期望默认值", benign)
	}
	sensitive, benign = dlpCacheTTL(time.Hour, 2*time.Hour)
	if sensitive != time.Hour || benign != 2*time.Hour {
		t.Error("显式传入的 TTL 应生效")
	}
	sensitive, benign = dlpCacheTTL(-time.Hour, -time.Hour)
	if sensitive != DefaultDLPCacheSensitiveTTL || benign != DefaultDLPCacheBenignTTL {
		t.Error("负值 TTL 应回落到默认值")
	}
}
