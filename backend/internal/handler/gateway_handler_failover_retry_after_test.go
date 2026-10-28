package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// retryAfterPassthroughRuleRepo 仅提供 List，供 ErrorPassthroughService 启动时加载规则。
type retryAfterPassthroughRuleRepo struct {
	service.ErrorPassthroughRepository
	rules []*model.ErrorPassthroughRule
}

func (r *retryAfterPassthroughRuleRepo) List(context.Context) ([]*model.ErrorPassthroughRule, error) {
	return r.rules, nil
}

func newRetryAfterFailoverContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c, rec
}

func newUpstream429FailoverErr(retryAfter string) *service.UpstreamFailoverError {
	headers := http.Header{}
	if retryAfter != "" {
		headers.Set("Retry-After", retryAfter)
	}
	return &service.UpstreamFailoverError{
		StatusCode:      http.StatusTooManyRequests,
		ResponseBody:    []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"throttled"}}`),
		ResponseHeaders: headers,
	}
}

func TestGatewayHandleFailoverExhaustedCopiesUpstreamRetryAfter(t *testing.T) {
	httpDate := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	tests := []struct {
		name       string
		retryAfter string
		want       string
	}{
		{name: "delta seconds", retryAfter: "1800", want: "1800"},
		{name: "http date", retryAfter: httpDate, want: httpDate},
		{name: "invalid value dropped", retryAfter: "soon", want: ""},
		{name: "beyond seven days dropped", retryAfter: "604801", want: ""},
		{name: "absent", retryAfter: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, rec := newRetryAfterFailoverContext()
			h := &GatewayHandler{}

			h.handleFailoverExhausted(c, newUpstream429FailoverErr(tt.retryAfter), service.PlatformAnthropic, false)

			require.Equal(t, http.StatusTooManyRequests, rec.Code)
			require.Equal(t, tt.want, rec.Header().Get("Retry-After"))
			require.JSONEq(t, `{"type":"error","error":{"type":"rate_limit_error","message":"Upstream rate limit exceeded, please retry later"}}`, rec.Body.String())
		})
	}
}

func TestGatewayHandleFailoverExhaustedKeepsRetryAfterWhenPassthroughRuleMatches(t *testing.T) {
	responseCode := http.StatusServiceUnavailable
	customMessage := "busy, retry later"
	repo := &retryAfterPassthroughRuleRepo{rules: []*model.ErrorPassthroughRule{{
		ID:            1,
		Name:          "remap 429",
		Enabled:       true,
		ErrorCodes:    []int{http.StatusTooManyRequests},
		MatchMode:     model.MatchModeAny,
		ResponseCode:  &responseCode,
		CustomMessage: &customMessage,
	}}}
	c, rec := newRetryAfterFailoverContext()
	h := &GatewayHandler{errorPassthroughService: service.NewErrorPassthroughService(repo, nil)}

	h.handleFailoverExhausted(c, newUpstream429FailoverErr("120"), service.PlatformAnthropic, false)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "120", rec.Header().Get("Retry-After"))
	require.Contains(t, rec.Body.String(), customMessage)
}
