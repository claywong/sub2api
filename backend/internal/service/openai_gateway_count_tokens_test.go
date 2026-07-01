package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestOpenAIGatewayService_ForwardCountTokensAsAnthropic_OAuthUnsupportedDoesNotCooldown
// 回归测试 issue #3576 / #3574：OAuth 账号缺少 api.responses.write scope 时，
// Platform Responses 的 input_tokens 端点返回 401，这是能力不支持，不是账号异常。
// 必须在调用 HandleUpstreamError 之前短路，否则会被误判为账号级 401，
// 触发 token 缓存失效 + SetTempUnschedulable 冷却，导致账号被无差别打入临时不可调度状态。
func TestOpenAIGatewayService_ForwardCountTokensAsAnthropic_OAuthUnsupportedDoesNotCooldown(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"claude-opus-4-1","messages":[{"role":"user","content":"hello"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"Missing scope api.responses.write"}}`)),
	}}

	repo := &errorPolicyRepoStub{}
	rateLimitSvc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)

	svc := &OpenAIGatewayService{
		cfg:              &config.Config{},
		httpUpstream:     upstream,
		rateLimitService: rateLimitSvc,
	}
	account := &Account{
		ID:          303,
		Name:        "openai-oauth-noscope",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":  "oauth-token",
			"refresh_token": "refresh-token",
		},
		Status:      StatusActive,
		Schedulable: true,
	}

	err := svc.ForwardCountTokensAsAnthropic(context.Background(), c, account, body, "gpt-5.4")
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "Token counting is not supported for this OpenAI account type")

	require.Equal(t, 0, repo.tempCalls, "OAuth 账号缺 scope 不应触发 SetTempUnschedulable 冷却")
	require.Equal(t, 0, repo.setErrCalls, "OAuth 账号缺 scope 不应触发 SetError 永久禁用")
}

func TestOpenAIGatewayService_ForwardCountTokensAsAnthropic_APIKeyUsesResponsesInputTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"claude-sonnet-4-5","system":"You are helpful.","messages":[{"role":"user","content":"hello"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"object":"response.input_tokens","input_tokens":42}`)),
	}}

	svc := &OpenAIGatewayService{
		cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{
			Enabled:           false,
			AllowInsecureHTTP: true,
		}}},
		httpUpstream: upstream,
	}
	account := &Account{
		ID:          101,
		Name:        "openai-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "http://upstream.example",
		},
		Status:      StatusActive,
		Schedulable: true,
	}

	err := svc.ForwardCountTokensAsAnthropic(context.Background(), c, account, body, "gpt-5.3-codex")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"input_tokens":42}`, rec.Body.String())
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "http://upstream.example/v1/responses/input_tokens", upstream.lastReq.URL.String())
	require.Equal(t, "Bearer sk-test", upstream.lastReq.Header.Get("authorization"))
	require.Equal(t, "gpt-5.3-codex", gjson.GetBytes(upstream.lastBody, "model").String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "input").Exists())
	require.False(t, gjson.GetBytes(upstream.lastBody, "messages").Exists())
}

func TestOpenAIGatewayService_ForwardCountTokensAsAnthropic_OAuthFallsBackWhenPlatformEndpointUnsupported(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"claude-opus-4-1","messages":[{"role":"user","content":"hello"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "Claude-Code/1.0")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"unauthorized"}}`)),
	}}

	svc := &OpenAIGatewayService{
		cfg:          &config.Config{},
		httpUpstream: upstream,
	}
	account := &Account{
		ID:          202,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token": "oauth-token",
		},
		Status:      StatusActive,
		Schedulable: true,
	}

	err := svc.ForwardCountTokensAsAnthropic(context.Background(), c, account, body, "gpt-5.4")
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "Token counting is not supported for this OpenAI account type")
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "https://api.openai.com/v1/responses/input_tokens", upstream.lastReq.URL.String())
	require.Equal(t, "Bearer oauth-token", upstream.lastReq.Header.Get("authorization"))
	require.Empty(t, upstream.lastReq.Header.Get("Chatgpt-Account-Id"))
}
