package middleware

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	ippkg "github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/gin-gonic/gin"
)

type IPAllowlistStore interface {
	ListEnabledIPAllowlist(ctx context.Context) ([]string, error)
	IPAllowlistEnabled(ctx context.Context) bool
}

type IPAllowlistMiddleware struct {
	store    IPAllowlistStore
	mu       sync.RWMutex
	rules    *ippkg.CompiledIPRules
	loadedAt time.Time
}

func ClientIPForAccess(c *gin.Context) string { return ippkg.GetSecurityClientIP(c, false) }

func NewIPAllowlistMiddleware(store IPAllowlistStore) *IPAllowlistMiddleware {
	return &IPAllowlistMiddleware{store: store}
}

func (m *IPAllowlistMiddleware) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if isIPAllowlistException(c.Request.URL.Path) || m.allowed(c) {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"code": "IP_NOT_ALLOWED", "message": "当前 IP 不允许访问"})
	}
}

func (m *IPAllowlistMiddleware) allowed(c *gin.Context) bool {
	if m == nil || m.store == nil {
		return true
	}
	if !m.store.IPAllowlistEnabled(c.Request.Context()) {
		return true
	}
	m.mu.RLock()
	rules, loadedAt := m.rules, m.loadedAt
	m.mu.RUnlock()
	if loadedAt.IsZero() || time.Since(loadedAt) > 30*time.Second {
		if patterns, err := m.store.ListEnabledIPAllowlist(c.Request.Context()); err == nil {
			rules = ippkg.CompileIPRules(patterns)
			m.mu.Lock()
			m.rules, m.loadedAt = rules, time.Now()
			m.mu.Unlock()
		}
	}
	if rules == nil || rules.PatternCount == 0 {
		return false
	}
	ip := ippkg.GetSecurityClientIP(c, false)
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return false
	}
	for _, candidate := range rules.IPs {
		if candidate.Equal(parsed) {
			return true
		}
	}
	for _, cidr := range rules.CIDRs {
		if cidr.Contains(parsed) {
			return true
		}
	}
	return false
}

func isIPAllowlistException(path string) bool {
	return path == "/health" || path == "/ip-access-request" || strings.HasPrefix(path, "/api/v1/ip-access-requests") || strings.HasPrefix(path, "/assets/") || path == "/favicon.ico"
}
