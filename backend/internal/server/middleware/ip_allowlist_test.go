package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type ipAllowlistStoreStub struct {
	enabled bool
	ips     []string
}

func (s *ipAllowlistStoreStub) IPAllowlistEnabled(context.Context) bool { return s.enabled }
func (s *ipAllowlistStoreStub) ListEnabledIPAllowlist(context.Context) ([]string, error) {
	return s.ips, nil
}

func TestIPAllowlistDisabledAllowsAnyIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(NewIPAllowlistMiddleware(&ipAllowlistStoreStub{enabled: false}).Handler())
	r.GET("/protected", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.RemoteAddr = "203.0.113.10:1234"
	resp := httptest.NewRecorder()
	r.ServeHTTP(resp, req)

	require.Equal(t, http.StatusNoContent, resp.Code)
}

func TestIPAllowlistEnabledAllowsWhitelistedIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(NewIPAllowlistMiddleware(&ipAllowlistStoreStub{enabled: true, ips: []string{"203.0.113.10/32"}}).Handler())
	r.GET("/protected", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.RemoteAddr = "203.0.113.10:1234"
	resp := httptest.NewRecorder()
	r.ServeHTTP(resp, req)

	require.Equal(t, http.StatusNoContent, resp.Code)
}

func TestIPAllowlistEnabledRejectsNonWhitelistedIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(NewIPAllowlistMiddleware(&ipAllowlistStoreStub{enabled: true, ips: []string{"203.0.113.10/32"}}).Handler())
	r.GET("/protected", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.RemoteAddr = "203.0.113.11:1234"
	resp := httptest.NewRecorder()
	r.ServeHTTP(resp, req)

	require.Equal(t, http.StatusForbidden, resp.Code)
	require.Contains(t, resp.Body.String(), "IP_NOT_ALLOWED")
}
