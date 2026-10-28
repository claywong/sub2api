package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 上游网关（如池模式账号背后的 kiro.rs）对 429 明确给出 Retry-After 时，
// failover 错误必须携带上游响应头，handler 才能在账号耗尽后告知客户端等待时长。
func TestGatewayForward429FailoverCarriesUpstreamRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, passthrough := range []bool{false, true} {
		name := "standard"
		if passthrough {
			name = "anthropic_apikey_passthrough"
		}
		t.Run(name, func(t *testing.T) {
			upstreamHeader := http.Header{
				"Retry-After":  []string{"1800"},
				"Content-Type": []string{"application/json"},
			}
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{{
				StatusCode: http.StatusTooManyRequests,
				Header:     upstreamHeader,
				Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"throttled"}}`)),
			}}}
			cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
			svc := &GatewayService{
				cfg:                  cfg,
				responseHeaderFilter: compileResponseHeaderFilter(cfg),
				httpUpstream:         upstream,
				rateLimitService:     &RateLimitService{},
				deferredService:      &DeferredService{},
				tlsFPProfileService:  &TLSFingerprintProfileService{},
			}
			account := &Account{
				ID:          1,
				Name:        "pool-gateway",
				Platform:    PlatformAnthropic,
				Type:        AccountTypeAPIKey,
				Concurrency: 1,
				Status:      StatusActive,
				Schedulable: true,
				Credentials: map[string]any{
					"api_key":                      "upstream-key",
					"base_url":                     "https://upstream.example.com",
					"pool_mode":                    true,
					"pool_mode_retry_status_codes": []any{float64(500), float64(502)},
				},
				Extra: map[string]any{"anthropic_passthrough": passthrough},
			}
			body := []byte(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
			require.NoError(t, err)

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

			result, err := svc.Forward(context.Background(), c, account, parsed)
			require.Nil(t, result)
			var failoverErr *UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr)
			require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
			require.False(t, failoverErr.RetryableOnSameAccount)
			require.Equal(t, "1800", failoverErr.ResponseHeaders.Get("Retry-After"))

			// 必须是副本：后续修改上游响应头不能影响已返回的 failover 错误。
			upstreamHeader.Set("Retry-After", "1")
			require.Equal(t, "1800", failoverErr.ResponseHeaders.Get("Retry-After"))
			require.Zero(t, rec.Body.Len(), "failover 前不应写出客户端响应")
		})
	}
}
