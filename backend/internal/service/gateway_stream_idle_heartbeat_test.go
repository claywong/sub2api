//go:build unit

package service

// 数据间隔超时（gateway.stream_data_interval_timeout）的计时基准回归测试。
//
// 背景：该超时原先以「scanner 读到任意一行」为基准，导致第三方中转网关在排队等
// 上游时发的空心跳（空行 / `:` 注释 / 裸 event 行）会不断刷新计时器，超时永不触
// 发。线上实测出现过 TTFT 154s 的请求——上游秒回响应头（绕过
// anthropic_response_header_timeout），随后靠空心跳续命，客户端干等两分半。
//
// 修复后基准改为「最近一次携带负载的 data 行」，因此：
//   - 只有空心跳、没有 data 行 → 应按 interval 超时并 failover
//   - 有真实 data 行（含 Anthropic 官方 ping）→ 应正常续命，不误杀

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newIdleTimeoutGatewayService(intervalSec int) *GatewayService {
	return &GatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				StreamDataIntervalTimeout: intervalSec,
				MaxLineSize:               defaultMaxLineSize,
			},
		},
		rateLimitService: &RateLimitService{},
	}
}

// pumpLines 以固定节奏向管道写入若干行，间隔刻意小于超时阈值，
// 模拟中转网关"连接活着但没有实质进展"的心跳行为。
func pumpLines(pw *io.PipeWriter, line string, every time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if _, err := pw.Write([]byte(line)); err != nil {
				return
			}
		}
	}
}

func TestHandleStreamingResponse_BlankLineHeartbeatDoesNotDeferIdleTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newIdleTimeoutGatewayService(1)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       pr,
	}

	stop := make(chan struct{})
	// 每 200ms 一个空行，远快于 1s 阈值：修复前会无限续命
	go pumpLines(pw, "\n", 200*time.Millisecond, stop)

	result, err := svc.handleStreamingResponse(context.Background(), resp, c, &Account{ID: 1}, time.Now(), "model", "model", false)
	close(stop)
	_ = pw.Close()
	_ = pr.Close()

	require.Error(t, err)
	require.Contains(t, err.Error(), "stream data interval timeout",
		"纯空行心跳不算上游进展，应触发数据间隔超时")
	require.NotNil(t, result)
	require.Nil(t, result.firstTokenMs, "从未收到 data 行，firstTokenMs 应为空")
}

func TestHandleStreamingResponse_CommentHeartbeatDoesNotDeferIdleTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newIdleTimeoutGatewayService(1)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       pr,
	}

	stop := make(chan struct{})
	// SSE 注释行心跳，同样不携带负载
	go pumpLines(pw, ": keepalive\n", 200*time.Millisecond, stop)

	result, err := svc.handleStreamingResponse(context.Background(), resp, c, &Account{ID: 2}, time.Now(), "model", "model", false)
	close(stop)
	_ = pw.Close()
	_ = pr.Close()

	require.Error(t, err)
	require.Contains(t, err.Error(), "stream data interval timeout",
		"`:` 注释心跳不算上游进展，应触发数据间隔超时")
	require.NotNil(t, result)
}

func TestHandleStreamingResponse_PingDataEventDefersIdleTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newIdleTimeoutGatewayService(1)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       pr,
	}

	go func() {
		defer func() { _ = pw.Close() }()
		// Anthropic 官方 ping 携带 data 负载：表示上游活着且在工作（如 extended
		// thinking 期间），必须续命，否则会误杀正常长思考请求。
		// 持续 1.6s > 1s 阈值，若 ping 不计入基准则此处必然超时。
		for i := 0; i < 8; i++ {
			if _, err := pw.Write([]byte("event: ping\ndata: {\"type\": \"ping\"}\n\n")); err != nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		_, _ = pw.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7}}}\n\n"))
		_, _ = pw.Write([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":4}}\n\n"))
		_, _ = pw.Write([]byte("data: [DONE]\n\n"))
	}()

	result, err := svc.handleStreamingResponse(context.Background(), resp, c, &Account{ID: 3}, time.Now(), "model", "model", false)
	_ = pr.Close()

	require.NoError(t, err, "带负载的 ping 应续命，不应误判为空闲超时")
	require.NotNil(t, result)
	require.NotNil(t, result.usage)
	require.Equal(t, 7, result.usage.InputTokens)
	require.Equal(t, 4, result.usage.OutputTokens)
}

func TestGatewayService_AnthropicPassthrough_BlankLineHeartbeatDoesNotDeferIdleTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newIdleTimeoutGatewayService(1)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       pr,
	}

	stop := make(chan struct{})
	go pumpLines(pw, "\n", 200*time.Millisecond, stop)

	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
		context.Background(), resp, c, &Account{ID: 4}, time.Now(), "claude-opus-4-8")
	close(stop)
	_ = pw.Close()
	_ = pr.Close()

	require.Error(t, err)
	require.Contains(t, err.Error(), "stream data interval timeout",
		"passthrough 路径同样不应被空行心跳续命")
	require.NotNil(t, result)
	require.False(t, result.clientDisconnect)
}

func TestSSELineCarriesData(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"空字符串", "", false},
		{"纯空白", "   ", false},
		{"SSE 注释心跳", ": keepalive", false},
		{"裸 event 行", "event: ping", false},
		{"data 前缀但内容为空", "data:", false},
		{"data 前缀仅空白", "data:   ", false},
		{"正常 data 行", `data: {"type":"content_block_delta"}`, true},
		{"官方 ping 的 data 行", `data: {"type": "ping"}`, true},
		{"DONE 终止标记", "data: [DONE]", true},
		{"data 无空格分隔", `data:{"type":"message_stop"}`, true},
		{"带前导空白的 data 行", `  data: {"type":"ping"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, sseLineCarriesData(tc.line))
		})
	}
}
