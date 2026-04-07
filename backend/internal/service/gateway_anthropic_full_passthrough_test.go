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

func newAnthropicFullPassthroughServiceForTest(upstream *anthropicHTTPUpstreamRecorder) *GatewayService {
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			MaxLineSize: defaultMaxLineSize,
		},
	}
	return &GatewayService{
		cfg:                  cfg,
		responseHeaderFilter: compileResponseHeaderFilter(cfg),
		httpUpstream:         upstream,
		rateLimitService:     &RateLimitService{},
		deferredService:      &DeferredService{},
		claudeTokenProvider:  NewClaudeTokenProvider(nil, nil, nil),
	}
}

func newAnthropicFullPassthroughAccountForTest(accountType string) *Account {
	account := &Account{
		ID:          401,
		Name:        "anthropic-full-pass",
		Platform:    PlatformAnthropic,
		Type:        accountType,
		Concurrency: 1,
		Credentials: map[string]any{},
		Extra: map[string]any{
			"anthropic_passthrough_mode": AnthropicPassthroughModeFull,
		},
		Status:      StatusActive,
		Schedulable: true,
	}

	switch accountType {
	case AccountTypeAPIKey:
		account.Credentials["api_key"] = "upstream-apikey-token"
		account.Credentials["base_url"] = "https://api.anthropic.com"
		account.Credentials["model_mapping"] = map[string]any{
			"claude-sonnet-4-20250514": "claude-3-haiku-20240307",
		}
	case AccountTypeOAuth:
		account.Credentials["access_token"] = "oauth-upstream-token"
	case AccountTypeSetupToken:
		account.Credentials["access_token"] = "setup-upstream-token"
	}

	return account
}

func expectedAnthropicFullPassthroughAuth(accountType string) (headerKey string, headerValue string) {
	switch accountType {
	case AccountTypeAPIKey:
		return "x-api-key", "upstream-apikey-token"
	case AccountTypeOAuth:
		return "authorization", "Bearer oauth-upstream-token"
	case AccountTypeSetupToken:
		return "authorization", "Bearer setup-upstream-token"
	default:
		return "", ""
	}
}

func TestGatewayService_AnthropicFullPassthrough_ForwardPreservesOAuthAndSetupTokenRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name        string
		accountType string
	}{
		{name: "oauth", accountType: AccountTypeOAuth},
		{name: "setup-token", accountType: AccountTypeSetupToken},
	}

	body := []byte(`{"model":"claude-sonnet-4-20250514","stream":false,"metadata":{"user_id":"raw-user"},"temperature":0.7,"tool_choice":{"type":"auto"},"tools":[{"name":"bash","description":"run shell","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"text","text":""}]}]}`)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Request.Header.Set("User-Agent", "claude-cli/1.2.3")
			c.Request.Header.Set("X-App", "claude-code")
			c.Request.Header.Set("Anthropic-Version", "2023-06-01")
			c.Request.Header.Set("Anthropic-Beta", "oauth-test-beta")
			c.Request.Header.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")
			c.Request.Header.Set("X-Stainless-Lang", "js")
			c.Request.Header.Set("X-Stainless-Helper-Method", "stream")
			c.Request.Header.Set("Authorization", "Bearer inbound-token")
			c.Request.Header.Set("X-Api-Key", "inbound-api-key")
			c.Request.Header.Set("X-Goog-Api-Key", "inbound-goog-key")
			c.Request.Header.Set("Cookie", "session=secret")

			upstream := &anthropicHTTPUpstreamRecorder{
				resp: &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Type": []string{"application/json"},
						"x-request-id": []string{"rid-anthropic-full-oauth"},
					},
					Body: io.NopCloser(strings.NewReader(`{"id":"msg_full","type":"message","usage":{"input_tokens":9,"output_tokens":4}}`)),
				},
			}

			svc := newAnthropicFullPassthroughServiceForTest(upstream)
			account := newAnthropicFullPassthroughAccountForTest(tt.accountType)
			parsed := &ParsedRequest{
				Body:   body,
				Model:  "claude-sonnet-4-20250514",
				Stream: false,
			}

			result, err := svc.Forward(context.Background(), c, account, parsed)
			require.NoError(t, err)
			require.NotNil(t, result)

			require.Equal(t, string(body), string(upstream.lastBody), "完整透传不应改写请求体")
			require.Equal(t, "raw-user", gjson.GetBytes(upstream.lastBody, "metadata.user_id").String())
			require.Equal(t, 0.7, gjson.GetBytes(upstream.lastBody, "temperature").Float())
			require.Equal(t, "auto", gjson.GetBytes(upstream.lastBody, "tool_choice.type").String())
			require.Equal(t, "bash", gjson.GetBytes(upstream.lastBody, "tools.0.name").String())

			authHeaderKey, authHeaderValue := expectedAnthropicFullPassthroughAuth(tt.accountType)
			require.Equal(t, authHeaderValue, getHeaderRaw(upstream.lastReq.Header, authHeaderKey))
			require.Empty(t, getHeaderRaw(upstream.lastReq.Header, "x-api-key"))
			require.Empty(t, getHeaderRaw(upstream.lastReq.Header, "x-goog-api-key"))
			require.Empty(t, getHeaderRaw(upstream.lastReq.Header, "cookie"))
			require.Equal(t, "claude-cli/1.2.3", getHeaderRaw(upstream.lastReq.Header, "user-agent"))
			require.Equal(t, "claude-code", getHeaderRaw(upstream.lastReq.Header, "x-app"))
			require.Equal(t, "oauth-test-beta", getHeaderRaw(upstream.lastReq.Header, "anthropic-beta"))
			require.Equal(t, "true", getHeaderRaw(upstream.lastReq.Header, "anthropic-dangerous-direct-browser-access"))
			require.Equal(t, "js", getHeaderRaw(upstream.lastReq.Header, "x-stainless-lang"))
			require.Equal(t, "stream", getHeaderRaw(upstream.lastReq.Header, "x-stainless-helper-method"))
			require.Equal(t, "2023-06-01", getHeaderRaw(upstream.lastReq.Header, "anthropic-version"))
			require.Equal(t, http.StatusOK, rec.Code)
		})
	}
}

func TestGatewayService_AnthropicFullPassthrough_ForwardPreservesAPIKeyRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"claude-sonnet-4-20250514","stream":false,"metadata":{"user_id":"raw-user"},"temperature":0.2,"tool_choice":{"type":"auto"},"tools":[{"name":"editor","description":"open file","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"text","text":""}]}]}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("User-Agent", "claude-cli/1.2.3")
	c.Request.Header.Set("X-App", "claude-code")
	c.Request.Header.Set("Anthropic-Beta", "apikey-full-beta")
	c.Request.Header.Set("Authorization", "Bearer inbound-token")
	c.Request.Header.Set("X-Api-Key", "inbound-api-key")
	c.Request.Header.Set("X-Goog-Api-Key", "inbound-goog-key")
	c.Request.Header.Set("Cookie", "session=secret")

	upstream := &anthropicHTTPUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
				"x-request-id": []string{"rid-anthropic-full-apikey"},
			},
			Body: io.NopCloser(strings.NewReader(`{"id":"msg_full","type":"message","usage":{"input_tokens":5,"output_tokens":2}}`)),
		},
	}

	svc := newAnthropicFullPassthroughServiceForTest(upstream)
	account := newAnthropicFullPassthroughAccountForTest(AccountTypeAPIKey)
	parsed := &ParsedRequest{
		Body:   body,
		Model:  "claude-sonnet-4-20250514",
		Stream: false,
	}

	result, err := svc.Forward(context.Background(), c, account, parsed)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, string(body), string(upstream.lastBody), "API Key 完整透传不应改写 body 或做模型映射")
	require.Equal(t, "claude-sonnet-4-20250514", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, "upstream-apikey-token", getHeaderRaw(upstream.lastReq.Header, "x-api-key"))
	require.Empty(t, getHeaderRaw(upstream.lastReq.Header, "authorization"))
	require.Empty(t, getHeaderRaw(upstream.lastReq.Header, "x-goog-api-key"))
	require.Empty(t, getHeaderRaw(upstream.lastReq.Header, "cookie"))
	require.Equal(t, "application/json", getHeaderRaw(upstream.lastReq.Header, "content-type"), "缺失时应补 content-type")
	require.Equal(t, "2023-06-01", getHeaderRaw(upstream.lastReq.Header, "anthropic-version"), "缺失时应补 anthropic-version")
	require.Equal(t, "apikey-full-beta", getHeaderRaw(upstream.lastReq.Header, "anthropic-beta"))
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestGatewayService_AnthropicFullPassthrough_CountTokensPreservesRequestsAcrossAccountTypes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name        string
		accountType string
	}{
		{name: "oauth", accountType: AccountTypeOAuth},
		{name: "setup-token", accountType: AccountTypeSetupToken},
		{name: "apikey", accountType: AccountTypeAPIKey},
	}

	body := []byte(`{"model":"claude-sonnet-4-20250514","metadata":{"user_id":"raw-user"},"temperature":0.6,"tool_choice":{"type":"auto"},"tools":[{"name":"bash","description":"run shell","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(body))
			c.Request.Header.Set("User-Agent", "claude-cli/1.2.3")
			c.Request.Header.Set("X-App", "claude-code")
			c.Request.Header.Set("Anthropic-Beta", "count-tokens-beta")
			c.Request.Header.Set("X-Stainless-Timeout", "600")
			c.Request.Header.Set("Authorization", "Bearer inbound-token")
			c.Request.Header.Set("X-Api-Key", "inbound-api-key")
			c.Request.Header.Set("Cookie", "session=secret")

			upstreamRespBody := `{"input_tokens":42}`
			upstream := &anthropicHTTPUpstreamRecorder{
				resp: &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Type": []string{"application/json"},
						"x-request-id": []string{"rid-anthropic-full-count"},
					},
					Body: io.NopCloser(strings.NewReader(upstreamRespBody)),
				},
			}

			svc := newAnthropicFullPassthroughServiceForTest(upstream)
			account := newAnthropicFullPassthroughAccountForTest(tt.accountType)
			parsed := &ParsedRequest{
				Body:  body,
				Model: "claude-sonnet-4-20250514",
			}

			err := svc.ForwardCountTokens(context.Background(), c, account, parsed)
			require.NoError(t, err)

			authHeaderKey, authHeaderValue := expectedAnthropicFullPassthroughAuth(tt.accountType)
			require.Equal(t, string(body), string(upstream.lastBody), "count_tokens 完整透传不应改写 body")
			require.Equal(t, "claude-sonnet-4-20250514", gjson.GetBytes(upstream.lastBody, "model").String())
			require.Equal(t, authHeaderValue, getHeaderRaw(upstream.lastReq.Header, authHeaderKey))
			require.Empty(t, getHeaderRaw(upstream.lastReq.Header, "x-goog-api-key"))
			require.Empty(t, getHeaderRaw(upstream.lastReq.Header, "cookie"))
			require.Equal(t, "claude-cli/1.2.3", getHeaderRaw(upstream.lastReq.Header, "user-agent"))
			require.Equal(t, "claude-code", getHeaderRaw(upstream.lastReq.Header, "x-app"))
			require.Equal(t, "count-tokens-beta", getHeaderRaw(upstream.lastReq.Header, "anthropic-beta"))
			require.Equal(t, "600", getHeaderRaw(upstream.lastReq.Header, "x-stainless-timeout"))
			require.Equal(t, "application/json", getHeaderRaw(upstream.lastReq.Header, "content-type"))
			require.Equal(t, "2023-06-01", getHeaderRaw(upstream.lastReq.Header, "anthropic-version"))
			require.Equal(t, http.StatusOK, rec.Code)
			require.JSONEq(t, upstreamRespBody, rec.Body.String())
		})
	}
}
