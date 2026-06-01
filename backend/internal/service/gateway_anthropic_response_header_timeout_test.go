package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// responseHeaderTimeoutCapturingUpstream 捕获 DoWithTLS 调用时传入的 responseHeaderTimeout 参数
type responseHeaderTimeoutCapturingUpstream struct {
	capturedTimeout time.Duration
	called          bool
	resp            *http.Response
}

func (u *responseHeaderTimeoutCapturingUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.DoWithTLS(req, "", 0, 0, nil, 0)
}

func (u *responseHeaderTimeoutCapturingUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile, responseHeaderTimeout time.Duration) (*http.Response, error) {
	u.called = true
	u.capturedTimeout = responseHeaderTimeout
	if u.resp != nil {
		return u.resp, nil
	}
	body := io.NopCloser(bytes.NewReader([]byte(`{"type":"message","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)))
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body}, nil
}

func makeAnthropicOKResponse() *http.Response {
	body := `{"type":"message","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
	}
}

func makeForwardGatewayService(cfg *config.Config, upstream HTTPUpstream) *GatewayService {
	return &GatewayService{
		cfg:                 cfg,
		httpUpstream:        upstream,
		rateLimitService:    &RateLimitService{},
		tlsFPProfileService: nil, // Account 未启用 TLS 指纹，ResolveTLSProfile 会在 nil receiver 前提前返回
	}
}

func anthropicTestAccount() *Account {
	return &Account{
		ID:          1,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra:       map[string]any{},
	}
}

func openaiTestAccount() *Account {
	return &Account{
		ID:          2,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "Bearer test-token"},
		Extra:       map[string]any{},
	}
}

// TestForward_AnthropicResponseHeaderTimeout_StreamPassesTimeout 验证流式请求
// 在配置了 anthropic_response_header_timeout 时，DoWithTLS 收到正确的超时参数
func TestForward_AnthropicResponseHeaderTimeout_StreamPassesTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)

	streamBody := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-3-5-sonnet-20241022\",\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	streamResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(streamBody))),
	}
	upstream := &responseHeaderTimeoutCapturingUpstream{resp: streamResp}
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			ResponseHeaderTimeout:          600,
			AnthropicResponseHeaderTimeout: 30,
			MaxLineSize:                    defaultMaxLineSize,
		},
	}
	svc := makeForwardGatewayService(cfg, upstream)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"claude-3-5-sonnet-20241022","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	account := anthropicTestAccount()
	parsed := &ParsedRequest{
		Body:   NewRequestBodyRef(body),
		Model:  "claude-3-5-sonnet-20241022",
		Stream: true,
	}

	_, _ = svc.Forward(context.Background(), c, account, parsed)

	require.True(t, upstream.called, "DoWithTLS 应被调用")
	assert.Equal(t, 30*time.Second, upstream.capturedTimeout,
		"流式 Anthropic 请求应向 DoWithTLS 传入 30s responseHeaderTimeout")
}

// TestForward_AnthropicResponseHeaderTimeout_NonStreamZeroTimeout 验证非流式请求
// 不受 anthropic_response_header_timeout 影响，DoWithTLS 收到 0（使用全局默认）
func TestForward_AnthropicResponseHeaderTimeout_NonStreamZeroTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &responseHeaderTimeoutCapturingUpstream{resp: makeAnthropicOKResponse()}
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			ResponseHeaderTimeout:          600,
			AnthropicResponseHeaderTimeout: 30,
			MaxLineSize:                    defaultMaxLineSize,
		},
	}
	svc := makeForwardGatewayService(cfg, upstream)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"claude-3-5-sonnet-20241022","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	account := anthropicTestAccount()
	parsed := &ParsedRequest{
		Body:   NewRequestBodyRef(body),
		Model:  "claude-3-5-sonnet-20241022",
		Stream: false,
	}

	_, _ = svc.Forward(context.Background(), c, account, parsed)

	require.True(t, upstream.called, "DoWithTLS 应被调用")
	assert.Equal(t, time.Duration(0), upstream.capturedTimeout,
		"非流式请求不应因 AnthropicResponseHeaderTimeout 传入非零 responseHeaderTimeout")
}

// TestForward_AnthropicResponseHeaderTimeout_ZeroDisabled 验证流式请求配置为 0 时传入 0
func TestForward_AnthropicResponseHeaderTimeout_ZeroDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	streamBody := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	streamResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(streamBody))),
	}
	upstream := &responseHeaderTimeoutCapturingUpstream{resp: streamResp}
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			ResponseHeaderTimeout:          600,
			AnthropicResponseHeaderTimeout: 0, // 0 = 不启用
			MaxLineSize:                    defaultMaxLineSize,
		},
	}
	svc := makeForwardGatewayService(cfg, upstream)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"claude-3-5-sonnet-20241022","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	account := anthropicTestAccount()
	parsed := &ParsedRequest{
		Body:   NewRequestBodyRef(body),
		Model:  "claude-3-5-sonnet-20241022",
		Stream: true,
	}

	_, _ = svc.Forward(context.Background(), c, account, parsed)

	require.True(t, upstream.called, "DoWithTLS 应被调用")
	assert.Equal(t, time.Duration(0), upstream.capturedTimeout,
		"AnthropicResponseHeaderTimeout=0 时应传入 0")
}

// TestForward_AnthropicResponseHeaderTimeout_OpenAINotAffected 验证 OpenAI 平台不受影响
func TestForward_AnthropicResponseHeaderTimeout_OpenAINotAffected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &responseHeaderTimeoutCapturingUpstream{}
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			ResponseHeaderTimeout:          600,
			AnthropicResponseHeaderTimeout: 30, // 只对 Anthropic 生效
			MaxLineSize:                    defaultMaxLineSize,
		},
	}
	svc := makeForwardGatewayService(cfg, upstream)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-4o","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	account := openaiTestAccount()
	parsed := &ParsedRequest{
		Body:   NewRequestBodyRef(body),
		Model:  "gpt-4o",
		Stream: false,
	}

	_, _ = svc.Forward(context.Background(), c, account, parsed)

	if upstream.called {
		assert.Equal(t, time.Duration(0), upstream.capturedTimeout,
			"OpenAI 平台不应因 AnthropicResponseHeaderTimeout 传入非零 responseHeaderTimeout")
	}
}
