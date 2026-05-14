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

// contextCapturingUpstream 捕获 DoWithTLS 调用时的请求 context，用于验证超时注入
type contextCapturingUpstream struct {
	capturedCtx context.Context
	resp        *http.Response
}

func (u *contextCapturingUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.DoWithTLS(req, "", 0, 0, nil)
}

func (u *contextCapturingUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.capturedCtx = req.Context()
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

// TestForward_AnthropicResponseHeaderTimeout_StreamInjectsDeadline 验证流式请求
// 在配置了 anthropic_response_header_timeout 时，context 带有 deadline
func TestForward_AnthropicResponseHeaderTimeout_StreamInjectsDeadline(t *testing.T) {
	gin.SetMode(gin.TestMode)

	streamBody := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-3-5-sonnet-20241022\",\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	streamResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(streamBody))),
	}
	upstream := &contextCapturingUpstream{resp: streamResp}
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
		Body:   body,
		Model:  "claude-3-5-sonnet-20241022",
		Stream: true,
	}

	before := time.Now()
	_, _ = svc.Forward(context.Background(), c, account, parsed)

	require.NotNil(t, upstream.capturedCtx, "DoWithTLS 应被调用")
	deadline, ok := upstream.capturedCtx.Deadline()
	require.True(t, ok, "流式请求配置了超时后，context 应带有 deadline")
	assert.WithinDuration(t, before.Add(30*time.Second), deadline, 2*time.Second,
		"deadline 应在 30 秒左右")
}

// TestForward_AnthropicResponseHeaderTimeout_NonStreamNoDeadline 验证非流式请求
// 不受 anthropic_response_header_timeout 影响，依赖 Transport 层的 response_header_timeout
func TestForward_AnthropicResponseHeaderTimeout_NonStreamNoDeadline(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &contextCapturingUpstream{resp: makeAnthropicOKResponse()}
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
		Body:   body,
		Model:  "claude-3-5-sonnet-20241022",
		Stream: false,
	}

	_, _ = svc.Forward(context.Background(), c, account, parsed)

	require.NotNil(t, upstream.capturedCtx, "DoWithTLS 应被调用")
	_, ok := upstream.capturedCtx.Deadline()
	assert.False(t, ok, "非流式请求不应因 AnthropicResponseHeaderTimeout 注入 deadline")
}

// TestForward_AnthropicResponseHeaderTimeout_ZeroDisabled 验证流式请求配置为 0 时不注入 deadline
func TestForward_AnthropicResponseHeaderTimeout_ZeroDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	streamBody := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	streamResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(streamBody))),
	}
	upstream := &contextCapturingUpstream{resp: streamResp}
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
		Body:   body,
		Model:  "claude-3-5-sonnet-20241022",
		Stream: true,
	}

	_, _ = svc.Forward(context.Background(), c, account, parsed)

	require.NotNil(t, upstream.capturedCtx, "DoWithTLS 应被调用")
	_, ok := upstream.capturedCtx.Deadline()
	assert.False(t, ok, "AnthropicResponseHeaderTimeout=0 时不应注入 deadline")
}

// TestForward_AnthropicResponseHeaderTimeout_OpenAINotAffected 验证 OpenAI 平台不受影响
func TestForward_AnthropicResponseHeaderTimeout_OpenAINotAffected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &contextCapturingUpstream{}
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
		Body:   body,
		Model:  "gpt-4o",
		Stream: false,
	}

	_, _ = svc.Forward(context.Background(), c, account, parsed)

	if upstream.capturedCtx != nil {
		_, ok := upstream.capturedCtx.Deadline()
		assert.False(t, ok, "OpenAI 平台不应因 AnthropicResponseHeaderTimeout 注入 deadline")
	}
}
