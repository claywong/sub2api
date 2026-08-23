package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// OpenAI 原生 /v1/responses 流式路径的响应采集接线回归。
// 此前该路径从不构建 ResponsesCollector，CapturedResponseBody 恒为空，
// 导致 handler 侧跳过写库、request_logs 中 GPT 请求正文缺失。

func responsesSuccessSSE() string {
	return strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_1"},"sequence_number":0}`,
		"",
		"event: response.output_item.done",
		`data: {"type":"response.output_item.done","output_index":0,"sequence_number":1,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","sequence_number":2,"response":{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":12,"output_tokens":5}}}`,
		"",
	}, "\n")
}

func newStreamCaptureFixture(t *testing.T, requestLogEnabled bool) (*OpenAIGatewayService, *gin.Context, *http.Response) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize},
	}
	cfg.Gateway.RequestLog.Enabled = requestLogEnabled
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(responsesSuccessSSE())),
		Header:     http.Header{"X-Request-Id": []string{"rid-capture"}},
	}
	return svc, c, resp
}

func TestHandleStreamingResponseCapturesResponseBody(t *testing.T) {
	svc, c, resp := newStreamCaptureFixture(t, true)

	result, err := svc.handleStreamingResponse(
		c.Request.Context(), resp, c,
		&Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Name: "acc"},
		time.Now(), "gpt-test", "gpt-test",
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotEmpty(t, result.capturedBody, "streaming path must populate capturedBody when request_log is enabled")

	text := gjson.Get(result.capturedBody, "output.0.content.0.text").String()
	require.Equal(t, "hello", text, "captured body should carry the assistant output (full: %s)", result.capturedBody)
	require.Equal(t, "completed", gjson.Get(result.capturedBody, "status").String())
	// usage 由 usage_logs 单独记录，正文采集不应重复保留
	require.False(t, gjson.Get(result.capturedBody, "usage").Exists())
	// 计费仍须正常解析
	require.Equal(t, 12, result.usage.InputTokens)
	require.Equal(t, 5, result.usage.OutputTokens)
}

func TestHandleStreamingResponseSkipsCaptureWhenDisabled(t *testing.T) {
	svc, c, resp := newStreamCaptureFixture(t, false)

	result, err := svc.handleStreamingResponse(
		c.Request.Context(), resp, c,
		&Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Name: "acc"},
		time.Now(), "gpt-test", "gpt-test",
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Empty(t, result.capturedBody, "capture must stay off when gateway.request_log.enabled=false")
	require.Equal(t, 12, result.usage.InputTokens)
}

func TestHandleStreamingResponsePassthroughCapturesResponseBody(t *testing.T) {
	svc, c, resp := newStreamCaptureFixture(t, true)

	result, err := svc.handleStreamingResponsePassthrough(
		c.Request.Context(), resp, c,
		&Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Name: "acc"},
		time.Now(), "gpt-test", "gpt-test",
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotEmpty(t, result.capturedBody, "passthrough path must populate capturedBody when request_log is enabled")
	require.Equal(t, "hello", gjson.Get(result.capturedBody, "output.0.content.0.text").String())
	require.Equal(t, 12, result.usage.InputTokens)
}

func TestHandleStreamingResponsePassthroughSkipsCaptureWhenDisabled(t *testing.T) {
	svc, c, resp := newStreamCaptureFixture(t, false)

	result, err := svc.handleStreamingResponsePassthrough(
		c.Request.Context(), resp, c,
		&Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Name: "acc"},
		time.Now(), "gpt-test", "gpt-test",
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Empty(t, result.capturedBody)
}
