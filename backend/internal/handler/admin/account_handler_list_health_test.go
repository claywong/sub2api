package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountHandlerListIncludesHealthForAnthropicAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	adminSvc := newStubAdminService()
	adminSvc.accounts = []service.Account{
		{
			ID:          58,
			Name:        "zp_lyy",
			Platform:    service.PlatformAnthropic,
			Type:        service.AccountTypeAPIKey,
			Status:      service.StatusActive,
			Schedulable: true,
		},
	}

	healthCache := service.NewAccountTestHealthCache(nil)
	healthCache.RecordRealCall(58, service.CallSample{Success: false, TTFTMs: 12000, DurationMs: 25000, OutputTokens: 20})
	for i := 0; i < 4; i++ {
		healthCache.RecordRealCall(58, service.CallSample{Success: true, TTFTMs: 11000, DurationMs: 22000, OutputTokens: 20})
	}

	handler := NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, healthCache)
	router := gin.New()
	router.GET("/api/v1/admin/accounts", handler.List)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Code int `json:"code"`
		Data struct {
			Items []struct {
				ID                  int64                 `json:"id"`
				AccountHealth       *AccountHealthRuntime `json:"account_health"`
				HealthVerdict       *string               `json:"health_verdict"`
				HealthVerdictReason *string               `json:"health_verdict_reason"`
			} `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 0, resp.Code)
	require.Len(t, resp.Data.Items, 1)
	item := resp.Data.Items[0]
	require.Equal(t, int64(58), item.ID)
	require.NotNil(t, item.AccountHealth)
	require.True(t, item.AccountHealth.Available)
	require.Equal(t, int64(service.DefaultHealthWindowSeconds), item.AccountHealth.WindowSeconds)
	require.Equal(t, 5, item.AccountHealth.ReqCount)
	require.Equal(t, 1, item.AccountHealth.ErrCount)
	require.Equal(t, "StickyOnly", item.AccountHealth.Verdict)
	require.NotNil(t, item.HealthVerdict)
	require.Equal(t, "StickyOnly", *item.HealthVerdict)
	require.NotNil(t, item.HealthVerdictReason)
	require.NotEmpty(t, *item.HealthVerdictReason)
}
