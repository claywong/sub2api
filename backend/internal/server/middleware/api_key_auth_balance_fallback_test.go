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
