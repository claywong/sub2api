//go:build unit

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// 用户端按模型配额接口的归属边界与展示口径。
//
// 这个接口的核心风险是越权：subscription ID 是可枚举的自增值，一旦忘记校验归属，
// 任何登录用户都能读到他人的用量。因此归属用例是本文件的重点。

type fakeSubByIDRepo struct {
	*stubUserSubscriptionRepo
	sub *service.UserSubscription
	err error
}

func (r fakeSubByIDRepo) GetByID(ctx context.Context, id int64) (*service.UserSubscription, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.sub == nil || r.sub.ID != id {
		return nil, nil
	}
	return r.sub, nil
}

type fakeQuotaGroupRepo struct {
	*stubGroupRepo
	group *service.Group
}

func (r fakeQuotaGroupRepo) GetByIDLite(ctx context.Context, id int64) (*service.Group, error) {
	if r.group == nil || r.group.ID != id {
		return nil, service.ErrGroupNotFound
	}
	return r.group, nil
}

type fakeModelUsageRepo struct {
	records []service.UserGroupModelUsageRecord
}

func (r fakeModelUsageRepo) GetByRule(ctx context.Context, userID, groupID int64, ruleKey string) (*service.UserGroupModelUsageRecord, error) {
	return nil, nil
}

func (r fakeModelUsageRepo) ListByUserGroup(ctx context.Context, userID, groupID int64) ([]service.UserGroupModelUsageRecord, error) {
	return r.records, nil
}

func (r fakeModelUsageRepo) IncrementUsageWithReset(ctx context.Context, userID, groupID int64, ruleKey string, cost float64, now time.Time) error {
	return nil
}

func (r fakeModelUsageRepo) ResetUsageWindows(ctx context.Context, userID, groupID int64, ruleKey string, resetDaily, resetWeekly, resetMonthly bool, now time.Time) error {
	return nil
}

type modelQuotaEndpointResponse struct {
	Data struct {
		Items []service.ModelQuotaUsageProgress `json:"items"`
	} `json:"data"`
}

func float64Ptr(v float64) *float64 { return &v }

// buildModelQuotaEndpoint 组装一个只挂载被测路由的最小 engine，callerUserID 模拟登录身份。
func buildModelQuotaEndpoint(
	callerUserID int64,
	sub *service.UserSubscription,
	subErr error,
	group *service.Group,
	records []service.UserGroupModelUsageRecord,
) *gin.Engine {
	gin.SetMode(gin.TestMode)

	subRepo := fakeSubByIDRepo{stubUserSubscriptionRepo: &stubUserSubscriptionRepo{}, sub: sub, err: subErr}
	groupRepo := fakeQuotaGroupRepo{stubGroupRepo: &stubGroupRepo{}, group: group}
	usageRepo := fakeModelUsageRepo{records: records}

	cfg := &config.Config{RunMode: config.RunModeStandard}
	subscriptionService := service.NewSubscriptionService(groupRepo, subRepo, nil, nil, cfg)
	// billingCache 传 nil：本接口只读用量，不触发缓存失效
	quotaService := service.NewModelQuotaUsageService(usageRepo, groupRepo, nil)
	h := handler.NewSubscriptionHandler(subscriptionService, quotaService)

	r := gin.New()
	v1 := r.Group("/api/v1")
	v1.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: callerUserID, Concurrency: 5})
		c.Set(string(middleware.ContextKeyUserRole), service.RoleUser)
		c.Next()
	})
	v1.GET("/subscriptions/:id/model-quota-usage", h.GetModelQuotaUsage)
	return r
}

func quotaGroupWithRules(rules []service.GroupModelQuotaRule) *service.Group {
	return &service.Group{
		ID:          9,
		Name:        "vip",
		ModelQuotas: service.GroupModelQuotas{Enabled: true, Rules: rules},
	}
}

func doModelQuotaRequest(t *testing.T, r *gin.Engine, subscriptionID int64) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/subscriptions/"+strconv.FormatInt(subscriptionID, 10)+"/model-quota-usage", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestModelQuotaEndpointRejectsOtherUsersSubscription(t *testing.T) {
	// 订阅属于 user 2，调用方是 user 1
	sub := &service.UserSubscription{ID: 42, UserID: 2, GroupID: 9}
	group := quotaGroupWithRules([]service.GroupModelQuotaRule{
		{Match: "claude-opus*", Daily: float64Ptr(50)},
	})
	r := buildModelQuotaEndpoint(1, sub, nil, group, []service.UserGroupModelUsageRecord{
		{UserID: 2, GroupID: 9, RuleKey: "claude-opus*", DailyUsageUSD: 33},
	})

	w := doModelQuotaRequest(t, r, 42)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for another user's subscription, got %d: %s", w.Code, w.Body.String())
	}
	// 不得泄漏任何用量数字
	if body := w.Body.String(); strings.Contains(body, "33") {
		t.Fatalf("response leaked other user's usage: %s", body)
	}
}

func TestModelQuotaEndpointReturnsNotFoundForMissingSubscription(t *testing.T) {
	r := buildModelQuotaEndpoint(1, nil, nil, quotaGroupWithRules(nil), nil)

	w := doModelQuotaRequest(t, r, 404)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing subscription, got %d: %s", w.Code, w.Body.String())
	}
}

func TestModelQuotaEndpointRejectsInvalidID(t *testing.T) {
	r := buildModelQuotaEndpoint(1, nil, nil, quotaGroupWithRules(nil), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/subscriptions/abc/model-quota-usage", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-numeric id, got %d: %s", w.Code, w.Body.String())
	}
}

func TestModelQuotaEndpointReturnsOwnUsage(t *testing.T) {
	sub := &service.UserSubscription{ID: 42, UserID: 1, GroupID: 9}
	group := quotaGroupWithRules([]service.GroupModelQuotaRule{
		{Match: "claude-opus*", Daily: float64Ptr(50)},
		{Match: "gpt-6-astra", Monthly: float64Ptr(200)},
	})
	now := time.Now()
	r := buildModelQuotaEndpoint(1, sub, nil, group, []service.UserGroupModelUsageRecord{
		{
			UserID: 1, GroupID: 9, RuleKey: "claude-opus*",
			DailyUsageUSD: 12.5, DailyWindowStart: &now,
		},
	})

	w := doModelQuotaRequest(t, r, 42)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var got modelQuotaEndpointResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v (%s)", err, w.Body.String())
	}
	if len(got.Data.Items) != 2 {
		t.Fatalf("expected 2 rules, got %d: %s", len(got.Data.Items), w.Body.String())
	}

	opus := got.Data.Items[0]
	if opus.Match != "claude-opus*" {
		t.Fatalf("unexpected first rule: %+v", opus)
	}
	if opus.Daily == nil {
		t.Fatal("expected daily window for configured daily limit")
	}
	if opus.Daily.UsedUSD != 12.5 || opus.Daily.LimitUSD != 50 {
		t.Fatalf("unexpected daily window: %+v", opus.Daily)
	}
	if opus.Weekly != nil || opus.Monthly != nil {
		t.Fatalf("未配置的窗口不应返回: %+v", opus)
	}

	astra := got.Data.Items[1]
	if astra.Monthly == nil || astra.Monthly.LimitUSD != 200 {
		t.Fatalf("unexpected monthly window: %+v", astra.Monthly)
	}
	// 无用量记录的规则按 0 展示，而不是缺项
	if astra.Monthly.UsedUSD != 0 {
		t.Fatalf("expected zero usage for rule without record, got %v", astra.Monthly.UsedUSD)
	}
}

func TestModelQuotaEndpointReturnsEmptyWhenGroupHasNoQuotas(t *testing.T) {
	sub := &service.UserSubscription{ID: 42, UserID: 1, GroupID: 9}
	group := &service.Group{ID: 9, Name: "plain"}
	r := buildModelQuotaEndpoint(1, sub, nil, group, nil)

	w := doModelQuotaRequest(t, r, 42)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got modelQuotaEndpointResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(got.Data.Items) != 0 {
		t.Fatalf("expected no items, got %+v", got.Data.Items)
	}
}
