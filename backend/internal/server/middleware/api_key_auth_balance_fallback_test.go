//go:build unit

// 私有扩展（不属于 upstream sub2api）
// 测试订阅超限后的余额兜底逻辑（AllowBalanceFallback）
// merge 策略：upstream 不含此文件，merge 时保留。

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBalanceFallbackOnSubscriptionQuotaExceeded(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dailyLimit := 1.0
	baseGroup := service.Group{
		ID:               42,
		Name:             "sub-group",
		Status:           service.StatusActive,
		Hydrated:         true,
		Platform:         service.PlatformAnthropic,
		SubscriptionType: service.SubscriptionTypeSubscription,
		DailyLimitUSD:    &dailyLimit,
	}

	makeAPIKey := func(balance float64, group *service.Group) *service.APIKey {
		user := &service.User{
			ID:          7,
			Role:        service.RoleUser,
			Status:      service.StatusActive,
			Balance:     balance,
			Concurrency: 3,
		}
		ak := &service.APIKey{
			ID:     100,
			UserID: user.ID,
			Key:    "test-key",
			Status: service.StatusActive,
			User:   user,
			Group:  group,
		}
		ak.GroupID = &group.ID
		return ak
	}

	// 超限订阅（DailyUsageUSD 已超过 DailyLimitUSD）
	makeExceededSub := func(userID, groupID int64) *service.UserSubscription {
		now := time.Now()
		return &service.UserSubscription{
			ID:               55,
			UserID:           userID,
			GroupID:          groupID,
			Status:           service.SubscriptionStatusActive,
			ExpiresAt:        now.Add(24 * time.Hour),
			DailyWindowStart: &now,
			DailyUsageUSD:    999, // 远超 dailyLimit=1.0
		}
	}

	makeSubscriptionRepo := func(sub *service.UserSubscription) *stubUserSubscriptionRepo {
		return &stubUserSubscriptionRepo{
			getActive: func(ctx context.Context, userID, groupID int64) (*service.UserSubscription, error) {
				clone := *sub
				return &clone, nil
			},
			updateStatus:   func(ctx context.Context, id int64, status string) error { return nil },
			activateWindow: func(ctx context.Context, id int64, start time.Time) error { return nil },
			resetDaily:     func(ctx context.Context, id int64, start time.Time) error { return nil },
			resetWeekly:    func(ctx context.Context, id int64, start time.Time) error { return nil },
			resetMonthly:   func(ctx context.Context, id int64, start time.Time) error { return nil },
		}
	}

	cfg := &config.Config{RunMode: config.RunModeStandard}

	t.Run("fallback_enabled_balance_sufficient_passes_as_balance_mode", func(t *testing.T) {
		group := baseGroup
		group.AllowBalanceFallback = true

		apiKey := makeAPIKey(50.0, &group)
		sub := makeExceededSub(apiKey.User.ID, group.ID)

		apiKeyRepo := &stubApiKeyRepo{
			getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
				clone := *apiKey
				return &clone, nil
			},
		}
		apiKeyService := service.NewAPIKeyService(apiKeyRepo, nil, nil, nil, nil, nil, cfg)
		subscriptionService := service.NewSubscriptionService(nil, makeSubscriptionRepo(sub), nil, nil, cfg)

		var capturedSub *service.UserSubscription
		router := gin.New()
		router.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(apiKeyService, subscriptionService, nil, cfg)))
		router.GET("/t", func(c *gin.Context) {
			capturedSub, _ = GetSubscriptionFromContext(c)
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/t", nil)
		req.Header.Set("Authorization", "bearer "+apiKey.Key)
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "quota exceeded with fallback+balance should pass")
		require.Nil(t, capturedSub, "subscription should be nil in context (balance mode)")
	})

	t.Run("fallback_enabled_balance_zero_returns_insufficient_balance", func(t *testing.T) {
		group := baseGroup
		group.AllowBalanceFallback = true

		apiKey := makeAPIKey(0, &group) // 余额为 0
		sub := makeExceededSub(apiKey.User.ID, group.ID)

		apiKeyRepo := &stubApiKeyRepo{
			getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
				clone := *apiKey
				return &clone, nil
			},
		}
		apiKeyService := service.NewAPIKeyService(apiKeyRepo, nil, nil, nil, nil, nil, cfg)
		subscriptionService := service.NewSubscriptionService(nil, makeSubscriptionRepo(sub), nil, nil, cfg)
		router := newAuthTestRouter(apiKeyService, subscriptionService, cfg)

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/t", nil)
		req.Header.Set("Authorization", "bearer "+apiKey.Key)
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusForbidden, w.Code)
		require.Contains(t, w.Body.String(), "INSUFFICIENT_BALANCE")
	})

	t.Run("fallback_disabled_returns_usage_limit_exceeded", func(t *testing.T) {
		group := baseGroup
		group.AllowBalanceFallback = false // 默认行为

		apiKey := makeAPIKey(50.0, &group)
		sub := makeExceededSub(apiKey.User.ID, group.ID)

		apiKeyRepo := &stubApiKeyRepo{
			getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
				clone := *apiKey
				return &clone, nil
			},
		}
		apiKeyService := service.NewAPIKeyService(apiKeyRepo, nil, nil, nil, nil, nil, cfg)
		subscriptionService := service.NewSubscriptionService(nil, makeSubscriptionRepo(sub), nil, nil, cfg)
		router := newAuthTestRouter(apiKeyService, subscriptionService, cfg)

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/t", nil)
		req.Header.Set("Authorization", "bearer "+apiKey.Key)
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusTooManyRequests, w.Code)
		require.Contains(t, w.Body.String(), "USAGE_LIMIT_EXCEEDED")
	})
}

// TestWindowResetBypassesRedisFallbackCheck 验证 needsMaintenance=true（窗口过期需重置）时，
// 即使 Redis 缓存里仍有旧窗口超限数据，也不触发余额兜底，请求应走订阅模式。
//
// 修复背景：e629e8f3 在中间件加了 Redis 二次校验，但 DoWindowMaintenance 是异步的，
// Redis 里的旧值在第一个请求时尚未被清掉，导致误判超限触发兜底。
// 修复方案：needsMaintenance=true 时跳过 Redis 检查。
func TestWindowResetBypassesRedisFallbackCheck(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dailyLimit := 1.0
	cfg := &config.Config{RunMode: config.RunModeStandard}

	makeGroup := func(allowFallback bool) service.Group {
		return service.Group{
			ID:                   42,
			Name:                 "sub-group",
			Status:               service.StatusActive,
			Hydrated:             true,
			Platform:             service.PlatformAnthropic,
			SubscriptionType:     service.SubscriptionTypeSubscription,
			DailyLimitUSD:        &dailyLimit,
			AllowBalanceFallback: allowFallback,
		}
	}

	makeAPIKey := func(group *service.Group) *service.APIKey {
		user := &service.User{ID: 7, Role: service.RoleUser, Status: service.StatusActive, Balance: 50, Concurrency: 3}
		ak := &service.APIKey{ID: 100, UserID: user.ID, Key: "test-key", Status: service.StatusActive, User: user, Group: group}
		ak.GroupID = &group.ID
		return ak
	}

	// 窗口在 25 小时前启动：NeedsDailyReset()=true，模拟"昨天超限"状态。
	makeExpiredWindowSub := func(userID, groupID int64) *service.UserSubscription {
		windowStart := time.Now().Add(-25 * time.Hour)
		return &service.UserSubscription{
			ID: 55, UserID: userID, GroupID: groupID,
			Status:           service.SubscriptionStatusActive,
			ExpiresAt:        time.Now().Add(24 * time.Hour),
			DailyWindowStart: &windowStart,
			DailyUsageUSD:    999, // 旧窗口超限值
		}
	}

	// 窗口在 1 小时前启动：NeedsDailyReset()=false，DB 快照看起来未超。
	makeActiveWindowSub := func(userID, groupID int64) *service.UserSubscription {
		windowStart := time.Now().Add(-1 * time.Hour)
		return &service.UserSubscription{
			ID: 55, UserID: userID, GroupID: groupID,
			Status:           service.SubscriptionStatusActive,
			ExpiresAt:        time.Now().Add(23 * time.Hour),
			DailyWindowStart: &windowStart,
			DailyUsageUSD:    0.5, // DB 快照未超（< dailyLimit=1.0）
		}
	}

	makeSubRepo := func(sub *service.UserSubscription) *stubUserSubscriptionRepo {
		return &stubUserSubscriptionRepo{
			getActive:      func(_ context.Context, _, _ int64) (*service.UserSubscription, error) { clone := *sub; return &clone, nil },
			updateStatus:   func(_ context.Context, _ int64, _ string) error { return nil },
			activateWindow: func(_ context.Context, _ int64, _ time.Time) error { return nil },
			resetDaily:     func(_ context.Context, _ int64, _ time.Time) error { return nil },
			resetWeekly:    func(_ context.Context, _ int64, _ time.Time) error { return nil },
			resetMonthly:   func(_ context.Context, _ int64, _ time.Time) error { return nil },
		}
	}

	// Redis stub：GetSubscriptionCache 始终返回超限数据（模拟窗口重置后 Redis 尚未被清掉的状态）
	redisExceededCache := &stubBillingCacheAlwaysExceeded{
		dailyLimit: dailyLimit,
		dailyUsage: 999,
	}

	buildRouter := func(group *service.Group, sub *service.UserSubscription, billingCache *service.BillingCacheService) (*gin.Engine, *(*service.UserSubscription)) {
		apiKeyRepo := &stubApiKeyRepo{
			getByKey: func(_ context.Context, _ string) (*service.APIKey, error) {
				clone := makeAPIKey(group)
				return clone, nil
			},
		}
		apiKeySvc := service.NewAPIKeyService(apiKeyRepo, nil, nil, nil, nil, nil, cfg)
		subSvc := service.NewSubscriptionService(nil, makeSubRepo(sub), nil, nil, cfg)

		captured := (*service.UserSubscription)(nil)
		r := gin.New()
		r.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(apiKeySvc, subSvc, billingCache, cfg)))
		r.GET("/t", func(c *gin.Context) {
			captured, _ = GetSubscriptionFromContext(c)
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})
		return r, &captured
	}

	t.Run("expired_window_skips_redis_check_stays_in_subscription_mode", func(t *testing.T) {
		// needsMaintenance=true → Redis 检查跳过 → 走订阅模式
		group := makeGroup(true) // AllowBalanceFallback=true，若误触发兜底则 capturedSub 为 nil
		sub := makeExpiredWindowSub(7, group.ID)

		billingCacheSvc := service.NewBillingCacheService(redisExceededCache, nil, nil, nil, nil, nil, cfg, nil)
		defer billingCacheSvc.Stop()

		router, captured := buildRouter(&group, sub, billingCacheSvc)

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/t", nil)
		req.Header.Set("Authorization", "bearer test-key")
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.NotNil(t, *captured, "窗口过期时应跳过 Redis 检查，保持订阅模式，不触发余额兜底")
	})

	t.Run("active_window_redis_check_triggers_balance_fallback", func(t *testing.T) {
		// needsMaintenance=false + Redis 超限 → 触发余额兜底 → capturedSub 为 nil
		group := makeGroup(true)
		sub := makeActiveWindowSub(7, group.ID)

		billingCacheSvc := service.NewBillingCacheService(redisExceededCache, nil, nil, nil, nil, nil, cfg, nil)
		defer billingCacheSvc.Stop()

		router, captured := buildRouter(&group, sub, billingCacheSvc)

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/t", nil)
		req.Header.Set("Authorization", "bearer test-key")
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.Nil(t, *captured, "窗口未过期时 Redis 超限应触发余额兜底，subscription 应为 nil")
	})
}

// stubBillingCacheAlwaysExceeded 模拟 Redis 里有超限的订阅缓存数据（其他操作全部 no-op）。
type stubBillingCacheAlwaysExceeded struct {
	dailyLimit float64
	dailyUsage float64
}

func (s *stubBillingCacheAlwaysExceeded) GetSubscriptionCache(_ context.Context, _, _ int64) (*service.SubscriptionCacheData, error) {
	return &service.SubscriptionCacheData{
		Status:     service.SubscriptionStatusActive,
		ExpiresAt:  time.Now().Add(24 * time.Hour),
		DailyUsage: s.dailyUsage,
	}, nil
}

func (s *stubBillingCacheAlwaysExceeded) GetUserBalance(context.Context, int64) (float64, error) {
	return 0, nil
}
func (s *stubBillingCacheAlwaysExceeded) SetUserBalance(context.Context, int64, float64) error {
	return nil
}
func (s *stubBillingCacheAlwaysExceeded) DeductUserBalance(context.Context, int64, float64) error {
	return nil
}
func (s *stubBillingCacheAlwaysExceeded) InvalidateUserBalance(context.Context, int64) error {
	return nil
}
func (s *stubBillingCacheAlwaysExceeded) SetSubscriptionCache(context.Context, int64, int64, *service.SubscriptionCacheData) error {
	return nil
}
func (s *stubBillingCacheAlwaysExceeded) UpdateSubscriptionUsage(context.Context, int64, int64, float64) error {
	return nil
}
func (s *stubBillingCacheAlwaysExceeded) InvalidateSubscriptionCache(context.Context, int64, int64) error {
	return nil
}
func (s *stubBillingCacheAlwaysExceeded) GetAPIKeyRateLimit(context.Context, int64) (*service.APIKeyRateLimitCacheData, error) {
	return nil, nil
}
func (s *stubBillingCacheAlwaysExceeded) SetAPIKeyRateLimit(context.Context, int64, *service.APIKeyRateLimitCacheData) error {
	return nil
}
func (s *stubBillingCacheAlwaysExceeded) UpdateAPIKeyRateLimitUsage(context.Context, int64, float64) error {
	return nil
}
func (s *stubBillingCacheAlwaysExceeded) InvalidateAPIKeyRateLimit(context.Context, int64) error {
	return nil
}
func (s *stubBillingCacheAlwaysExceeded) GetUserPlatformQuotaCache(context.Context, int64, string) (*service.UserPlatformQuotaCacheEntry, bool, error) {
	return nil, false, nil
}
func (s *stubBillingCacheAlwaysExceeded) SetUserPlatformQuotaCache(context.Context, int64, string, *service.UserPlatformQuotaCacheEntry, time.Duration) error {
	return nil
}
func (s *stubBillingCacheAlwaysExceeded) DeleteUserPlatformQuotaCache(context.Context, int64, string) error {
	return nil
}
func (s *stubBillingCacheAlwaysExceeded) IncrUserPlatformQuotaUsageCache(context.Context, int64, string, float64, time.Duration, bool) error {
	return nil
}
func (s *stubBillingCacheAlwaysExceeded) PopDirtyUserPlatformQuotaKeys(context.Context, int) ([]service.UserPlatformQuotaKey, error) {
	return nil, nil
}
func (s *stubBillingCacheAlwaysExceeded) ReaddDirtyUserPlatformQuotaKeys(context.Context, []service.UserPlatformQuotaKey) error {
	return nil
}
func (s *stubBillingCacheAlwaysExceeded) BatchGetUserPlatformQuotaCache(context.Context, []service.UserPlatformQuotaKey) ([]*service.UserPlatformQuotaCacheEntry, error) {
	return nil, nil
}
