//go:build unit

// 私有扩展测试（不属于 upstream sub2api）
// 覆盖：管理员 API Key IP 白名单中间件行为
package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const testAdminKey = "admin-test-key-1234567890"

// adminIPTestUserRepo 扩展 stubUserRepo，支持 GetFirstAdmin。
type adminIPTestUserRepo struct {
	stubUserRepo
}

func (r *adminIPTestUserRepo) GetFirstAdmin(ctx context.Context) (*service.User, error) {
	return &service.User{
		ID:          1,
		Role:        service.RoleAdmin,
		Status:      service.StatusActive,
		Concurrency: 1,
	}, nil
}

// buildAdminIPRouter 构建一个使用 admin API key 认证、带 IP 白名单配置的路由。
func buildAdminIPRouter(t *testing.T, whitelist []string, trustForwardedIP bool) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	whitelistJSON := "[]"
	if len(whitelist) > 0 {
		b, _ := json.Marshal(whitelist)
		whitelistJSON = string(b)
	}

	settingRepo := fakeSettingRepo{
		values: map[string]string{
			service.SettingKeyAdminAPIKey:           testAdminKey,
			service.SettingKeyAdminAPIKeyIPWhitelist: whitelistJSON,
		},
	}
	settingService := service.NewSettingService(settingRepo, &config.Config{})

	userService := service.NewUserService(&adminIPTestUserRepo{}, nil, nil, nil)

	cfg := &config.Config{}
	cfg.SetTrustForwardedIPForAPIKeyACL(trustForwardedIP)

	router := gin.New()
	require.NoError(t, router.SetTrustedProxies(nil))
	router.Use(gin.HandlerFunc(NewAdminAuthMiddleware(nil, userService, settingService, cfg)))
	router.GET("/t", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return router
}

func TestAdminAPIKeyIPWhitelistAllowsWhenNotConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settingRepo := fakeSettingRepo{
		values: map[string]string{
			service.SettingKeyAdminAPIKey: testAdminKey,
			// 未配置白名单
		},
	}
	settingService := service.NewSettingService(settingRepo, &config.Config{})
	userService := service.NewUserService(&adminIPTestUserRepo{}, nil, nil, nil)

	router := gin.New()
	require.NoError(t, router.SetTrustedProxies(nil))
	router.Use(gin.HandlerFunc(NewAdminAuthMiddleware(nil, userService, settingService, &config.Config{})))
	router.GET("/t", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/t", nil)
	req.RemoteAddr = "9.9.9.9:1234"
	req.Header.Set("x-api-key", testAdminKey)
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
}

func TestAdminAPIKeyIPWhitelistAllowsMatchingIP(t *testing.T) {
	router := buildAdminIPRouter(t, []string{"1.2.3.4"}, false)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/t", nil)
	req.RemoteAddr = "1.2.3.4:5678"
	req.Header.Set("x-api-key", testAdminKey)
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
}

func TestAdminAPIKeyIPWhitelistBlocksNonMatchingIP(t *testing.T) {
	router := buildAdminIPRouter(t, []string{"1.2.3.4"}, false)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/t", nil)
	req.RemoteAddr = "9.9.9.9:5678"
	req.Header.Set("x-api-key", testAdminKey)
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "ACCESS_DENIED")
}

func TestAdminAPIKeyIPWhitelistAllowsCIDRRange(t *testing.T) {
	router := buildAdminIPRouter(t, []string{"10.0.0.0/8"}, false)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/t", nil)
	req.RemoteAddr = "10.1.2.3:5678"
	req.Header.Set("x-api-key", testAdminKey)
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
}

func TestAdminAPIKeyIPWhitelistBlocksOutsideCIDR(t *testing.T) {
	router := buildAdminIPRouter(t, []string{"10.0.0.0/8"}, false)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/t", nil)
	req.RemoteAddr = "11.0.0.1:5678"
	req.Header.Set("x-api-key", testAdminKey)
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestAdminAPIKeyIPWhitelistDoesNotTrustForwardedIPByDefault(t *testing.T) {
	// 白名单配置转发来的 IP，但未开启 trust_forwarded，应拦截
	router := buildAdminIPRouter(t, []string{"1.2.3.4"}, false)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/t", nil)
	req.RemoteAddr = "9.9.9.9:5678" // 真实连接 IP 不在白名单
	req.Header.Set("x-api-key", testAdminKey)
	req.Header.Set("X-Forwarded-For", "1.2.3.4") // 转发 IP 在白名单，但不信任
	req.Header.Set("CF-Connecting-IP", "1.2.3.4")
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestAdminAPIKeyIPWhitelistTrustsForwardedIPWhenEnabled(t *testing.T) {
	// 开启 trust_forwarded，转发 IP 在白名单 → 放行
	router := buildAdminIPRouter(t, []string{"1.2.3.4"}, true)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/t", nil)
	req.RemoteAddr = "9.9.9.9:5678"
	req.Header.Set("x-api-key", testAdminKey)
	req.Header.Set("CF-Connecting-IP", "1.2.3.4")
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
}

func TestAdminAPIKeyIPWhitelistDoesNotAffectJWTAuth(t *testing.T) {
	// JWT 登录方式不受 IP 白名单约束
	cfg := &config.Config{JWT: config.JWTConfig{Secret: "test-secret", ExpireHour: 1}}
	authService := service.NewAuthService(nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil)

	admin := &service.User{
		ID:           1,
		Email:        "admin@example.com",
		Role:         service.RoleAdmin,
		Status:       service.StatusActive,
		TokenVersion: 1,
		Concurrency:  1,
	}
	userRepo := &stubUserRepo{
		getByID: func(ctx context.Context, id int64) (*service.User, error) {
			if id != admin.ID {
				return nil, service.ErrUserNotFound
			}
			clone := *admin
			return &clone, nil
		},
	}
	userService := service.NewUserService(userRepo, nil, nil, nil)

	// 白名单里只有一个不可能匹配的 IP
	settingRepo := fakeSettingRepo{
		values: map[string]string{
			service.SettingKeyAdminAPIKeyIPWhitelist: `["255.255.255.255"]`,
		},
	}
	settingService := service.NewSettingService(settingRepo, &config.Config{})

	router := gin.New()
	require.NoError(t, router.SetTrustedProxies(nil))
	router.Use(gin.HandlerFunc(NewAdminAuthMiddleware(authService, userService, settingService, cfg)))
	router.GET("/t", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	token, err := authService.GenerateToken(admin)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/t", nil)
	req.RemoteAddr = "9.9.9.9:5678" // 不在白名单
	req.Header.Set("Authorization", "Bearer "+token)
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
}
