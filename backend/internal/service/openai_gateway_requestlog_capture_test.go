package service

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestlog"
	"github.com/tidwall/gjson"
)

// 覆盖 OpenAI Responses 入口的响应内容采集（此前该路径从不填充
// CapturedResponseBody，导致 /v1/responses 请求在 request_logs 中缺失正文）。

func newOpenAIGatewayWithCapture(enabled bool) *OpenAIGatewayService {
	cfg := &config.Config{}
	cfg.Gateway.RequestLog.Enabled = enabled
	return &OpenAIGatewayService{cfg: cfg}
}

func TestCaptureResponsesNonStreamBody_JSON(t *testing.T) {
	svc := newOpenAIGatewayWithCapture(true)
	body := []byte(`{"id":"resp_1","status":"completed","usage":{"input_tokens":10},` +
		`"output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`)

	got := svc.captureResponsesNonStreamBody(body)
	if got == "" {
		t.Fatal("expected captured body for JSON response")
	}
	if text := gjson.Get(got, "output.0.content.0.text").String(); text != "hi" {
		t.Errorf("expected output text preserved, got %q (full: %s)", text, got)
	}
	if status := gjson.Get(got, "status").String(); status != "completed" {
		t.Errorf("expected status preserved, got %q", status)
	}
	// usage / id 属于元信息，精简后应被丢弃
	if gjson.Get(got, "usage").Exists() || gjson.Get(got, "id").Exists() {
		t.Errorf("expected usage/id dropped, got %s", got)
	}
}

func TestCaptureResponsesNonStreamBody_SSEFallback(t *testing.T) {
	svc := newOpenAIGatewayWithCapture(true)
	// stream=false 时上游仍可能回 SSE 文本（handleSSEToJSON 分支）。
	sse := "event: response.completed\n" +
		`data: {"type":"response.completed","response":{"status":"completed",` +
		`"output":[{"type":"message","content":[{"type":"output_text","text":"from-sse"}]}]}}` + "\n\n"

	got := svc.captureResponsesNonStreamBody([]byte(sse))
	if got == "" {
		t.Fatal("expected captured body for SSE-framed response")
	}
	if text := gjson.Get(got, "output.0.content.0.text").String(); text != "from-sse" {
		t.Errorf("expected SSE output aggregated, got %q (full: %s)", text, got)
	}
}

func TestCaptureResponsesNonStreamBody_DisabledOrEmpty(t *testing.T) {
	body := []byte(`{"status":"completed","output":[]}`)
	if got := newOpenAIGatewayWithCapture(false).captureResponsesNonStreamBody(body); got != "" {
		t.Errorf("expected empty when switch disabled, got %q", got)
	}
	if got := newOpenAIGatewayWithCapture(true).captureResponsesNonStreamBody(nil); got != "" {
		t.Errorf("expected empty for nil body, got %q", got)
	}
	svc := &OpenAIGatewayService{cfg: nil}
	if got := svc.captureResponsesNonStreamBody(body); got != "" {
		t.Errorf("expected empty when cfg is nil, got %q", got)
	}
}

func TestFinalizeResponsesCollector_NilSafe(t *testing.T) {
	if got := finalizeResponsesCollector(nil); got != "" {
		t.Errorf("expected empty string for nil collector, got %q", got)
	}
}

// alpha/search 返回搜索结果 schema（无 output 字段），必须原样保留而不是被
// Responses 精简逻辑剥成 "{}"。
func TestCaptureAlphaSearchResponseBody(t *testing.T) {
	body := []byte(`{"results":[{"title":"t","url":"https://example.com","snippet":"s"}]}`)

	got := newOpenAIGatewayWithCapture(true).captureAlphaSearchResponseBody(body)
	if got != string(body) {
		t.Errorf("expected raw search body preserved, got %q", got)
	}
	if title := gjson.Get(got, "results.0.title").String(); title != "t" {
		t.Errorf("expected search results readable, got %q", got)
	}

	if got := newOpenAIGatewayWithCapture(false).captureAlphaSearchResponseBody(body); got != "" {
		t.Errorf("expected empty when switch disabled, got %q", got)
	}
	if got := newOpenAIGatewayWithCapture(true).captureAlphaSearchResponseBody(nil); got != "" {
		t.Errorf("expected empty for nil body, got %q", got)
	}
	svc := &OpenAIGatewayService{cfg: nil}
	if got := svc.captureAlphaSearchResponseBody(body); got != "" {
		t.Errorf("expected empty when cfg is nil, got %q", got)
	}
}

// 流式路径按 "data: <payload>" + 空行 的方式喂采集器（不依赖上游是否发送空行分隔）。
// 这里固定该约定，防止后续改动漏掉显式 flush 而使采集结果为空。
func TestResponsesCollectorExplicitFlushConvention(t *testing.T) {
	events := []string{
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","content":[{"type":"output_text","text":"a"}]}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","content":[{"type":"output_text","text":"b"}]}}`,
	}

	withFlush := requestlog.NewResponsesCollector()
	for _, data := range events {
		withFlush.OnLine("data: " + data)
		withFlush.OnLine("")
	}
	got := withFlush.Finalize()
	if n := len(gjson.Get(got, "output").Array()); n != 2 {
		t.Fatalf("expected 2 aggregated items, got %d (full: %s)", n, got)
	}

	// 反例：不喂空行时，只有最后一个事件会在 Finalize 内被 flush，
	// 中间事件全部丢失——这正是必须显式补空行的原因。
	noFlush := requestlog.NewResponsesCollector()
	for _, data := range events {
		noFlush.OnLine("data: " + data)
	}
	if n := len(gjson.Get(noFlush.Finalize(), "output").Array()); n == 2 {
		t.Error("expected concatenated payloads to lose events without explicit blank line")
	}
}

func TestResponsesCollectorTerminalEventWins(t *testing.T) {
	collector := requestlog.NewResponsesCollector()
	feed := func(data string) {
		collector.OnLine("data: " + data)
		collector.OnLine("")
	}
	feed(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","content":[{"type":"output_text","text":"partial"}]}}`)
	feed(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"final"}]}]}}`)

	got := collector.Finalize()
	if text := gjson.Get(got, "output.0.content.0.text").String(); text != "final" {
		t.Errorf("expected terminal event output to win, got %q (full: %s)", text, got)
	}
	if status := gjson.Get(got, "status").String(); status != "completed" {
		t.Errorf("expected status captured, got %q", status)
	}
}

func TestResponsesCollectorIgnoresDoneSentinel(t *testing.T) {
	collector := requestlog.NewResponsesCollector()
	collector.OnLine("data: [DONE]")
	collector.OnLine("")
	if got := collector.Finalize(); got != "" {
		t.Errorf("expected empty result for [DONE]-only stream, got %q", got)
	}
}

// captureResponsesNonStreamBody 依赖 bodyHasSSEFraming 分流，这里锁定分流判断本身：
// JSON 字符串里出现的 "data:" 字面量不得被误判为 SSE。
func TestCaptureResponsesNonStreamBody_JSONContainingDataLiteral(t *testing.T) {
	svc := newOpenAIGatewayWithCapture(true)
	body := []byte(`{"status":"completed","output":[{"type":"message","content":` +
		`[{"type":"output_text","text":"see data: value"}]}]}`)

	got := svc.captureResponsesNonStreamBody(body)
	if !strings.Contains(got, "see data: value") {
		t.Errorf("expected JSON body handled as JSON, got %q", got)
	}
}
