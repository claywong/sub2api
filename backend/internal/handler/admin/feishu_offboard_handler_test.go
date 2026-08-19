// 私有扩展（不属于 upstream sub2api）。
//
// 本文件：FeishuOffboardHandler 的 HTTP 层单测。
// 覆盖重点是"错了会出事"的几条：密钥不外泄、列表不吐明细、
// dry_run 语义、page_size 上限。
// merge 策略：纯新增文件，与 upstream 无交集，merge 时保留即可。
//
// @author wangzhong
package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// stubFeishuOffboardService 记录入参并回放预设结果。
type stubFeishuOffboardService struct {
	view       service.FeishuOffboardConfigView
	savedInput *service.FeishuOffboardConfigInput
	run        *service.FeishuOffboardRun
	gotDryRun     *bool
	triggerCalled bool
	list       *service.FeishuOffboardRunList
	gotFilter  *service.FeishuOffboardRunListFilter
	runByID    *service.FeishuOffboardRun
	gotID      int64
}

func (s *stubFeishuOffboardService) LoadConfigView(context.Context) (service.FeishuOffboardConfigView, error) {
	return s.view, nil
}

func (s *stubFeishuOffboardService) SaveConfig(_ context.Context, input service.FeishuOffboardConfigInput) error {
	s.savedInput = &input
	return nil
}

func (s *stubFeishuOffboardService) TestConnection(context.Context) error { return nil }

func (s *stubFeishuOffboardService) TriggerManual(_ context.Context, dryRun *bool) (*service.FeishuOffboardRun, error) {
	s.triggerCalled = true
	s.gotDryRun = dryRun
	return s.run, nil
}

func (s *stubFeishuOffboardService) ListRuns(
	_ context.Context, filter service.FeishuOffboardRunListFilter,
) (*service.FeishuOffboardRunList, error) {
	s.gotFilter = &filter
	return s.list, nil
}

func (s *stubFeishuOffboardService) GetRun(_ context.Context, id int64) (*service.FeishuOffboardRun, error) {
	s.gotID = id
	return s.runByID, nil
}

func newFeishuOffboardTestRouter(svc FeishuOffboardService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewFeishuOffboardHandler()
	h.SetService(svc)

	r := gin.New()
	g := r.Group("/api/v1/admin/feishu-offboard")
	g.GET("/config", h.GetConfig)
	g.PUT("/config", h.UpdateConfig)
	g.POST("/test", h.TestConnection)
	g.POST("/run", h.TriggerRun)
	g.GET("/runs", h.ListRuns)
	g.GET("/runs/:id", h.GetRun)
	return r
}

func doFeishuRequest(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	r.ServeHTTP(rec, req)
	return rec
}

// GET /config 只能回 app_secret_configured，绝不能出现明文密钥字段。
func TestFeishuOffboardGetConfigNeverLeaksSecret(t *testing.T) {
	svc := &stubFeishuOffboardService{view: service.FeishuOffboardConfigView{
		Enabled:             true,
		Schedule:            "0 1 * * *",
		AppID:               "cli_demo",
		AppSecretConfigured: true,
		Threshold:           15,
	}}
	rec := doFeishuRequest(newFeishuOffboardTestRouter(svc), http.MethodGet,
		"/api/v1/admin/feishu-offboard/config", "")

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), "app_secret\"")
	require.Contains(t, rec.Body.String(), "\"app_secret_configured\":true")
}

// PUT /config 需要去空白、去重收件人，并且不把空 app_secret 当成"清空"。
func TestFeishuOffboardUpdateConfigNormalizesInput(t *testing.T) {
	svc := &stubFeishuOffboardService{}
	rec := doFeishuRequest(newFeishuOffboardTestRouter(svc), http.MethodPut,
		"/api/v1/admin/feishu-offboard/config",
		`{"enabled":true,"schedule":"  0 1 * * *  ","app_id":" cli_demo ","app_secret":"",
		  "circuit_breaker_threshold":15,"notify_to":[" a@x.com ","A@x.com","","b@x.com"]}`)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, svc.savedInput)
	require.Equal(t, "0 1 * * *", svc.savedInput.Schedule)
	require.Equal(t, "cli_demo", svc.savedInput.AppID)
	require.Empty(t, svc.savedInput.AppSecret, "空 app_secret 应原样透传，由 service 解释为不修改")
	require.Equal(t, []string{"a@x.com", "b@x.com"}, svc.savedInput.NotifyTo)
}

// 负阈值会让熔断恒为真，等于功能永不执行，属于配置错误。
func TestFeishuOffboardUpdateConfigRejectsNegativeThreshold(t *testing.T) {
	svc := &stubFeishuOffboardService{}
	rec := doFeishuRequest(newFeishuOffboardTestRouter(svc), http.MethodPut,
		"/api/v1/admin/feishu-offboard/config",
		`{"enabled":true,"circuit_breaker_threshold":-1}`)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Nil(t, svc.savedInput, "非法配置不应落库")
}

// 空 body 等价于 {}：此时必须把 nil 传给 service，表示「沿用配置里的 dry_run」。
//
// 这条断言锁的是一个真实缺陷：如果 handler 把「没指定」兜成 false，
// 管理员配置的空跑模式会在手动触发时被静默改成真禁用。
// summary.dry_run 则必须反映服务端真实生效的模式，而不是请求里传的值。
func TestFeishuOffboardTriggerRunAcceptsEmptyBodyAndReportsEffectiveDryRun(t *testing.T) {
	svc := &stubFeishuOffboardService{run: &service.FeishuOffboardRun{
		ID: 7, DryRun: true, CheckedCount: 285, ResignedCount: 3, DisabledCount: 0,
	}}
	rec := doFeishuRequest(newFeishuOffboardTestRouter(svc), http.MethodPost,
		"/api/v1/admin/feishu-offboard/run", "")

	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, svc.triggerCalled, "应调用 TriggerManual")
	require.Nil(t, svc.gotDryRun,
		"未传 dry_run 时必须传 nil（沿用配置），绝不能兜成 false 变成真禁用")

	var resp struct {
		Data struct {
			Summary struct {
				DryRun        bool `json:"dry_run"`
				CheckedCount  int  `json:"checked_count"`
				DisabledCount int  `json:"disabled_count"`
			} `json:"summary"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.True(t, resp.Data.Summary.DryRun, "summary 应回放服务端实际生效的 dry_run")
	require.Equal(t, 285, resp.Data.Summary.CheckedCount)
	require.Equal(t, 0, resp.Data.Summary.DisabledCount)
}

// 显式 dry_run:true 必须原样传到 service，否则会真禁人。
func TestFeishuOffboardTriggerRunHonorsExplicitDryRun(t *testing.T) {
	svc := &stubFeishuOffboardService{run: &service.FeishuOffboardRun{ID: 8, DryRun: true}}
	rec := doFeishuRequest(newFeishuOffboardTestRouter(svc), http.MethodPost,
		"/api/v1/admin/feishu-offboard/run", `{"dry_run":true}`)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, svc.gotDryRun)
	require.True(t, *svc.gotDryRun)
}

// 列表接口不能吐出每人的判定明细（一次执行可能几百条）。
func TestFeishuOffboardListRunsStripsDecisions(t *testing.T) {
	svc := &stubFeishuOffboardService{list: &service.FeishuOffboardRunList{
		Items: []service.FeishuOffboardRun{{
			ID: 1,
			Decisions: []service.OffboardDecision{
				{UserID: 2, Email: "x@y.com", Verdict: service.OffboardVerdictResigned},
			},
		}},
		Total: 1, Page: 1, PageSize: 20,
	}}
	rec := doFeishuRequest(newFeishuOffboardTestRouter(svc), http.MethodGet,
		"/api/v1/admin/feishu-offboard/runs", "")

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), "x@y.com")
	require.NotContains(t, rec.Body.String(), "decisions")
}

// page_size 上限 100，防止前端一次拉走整张表。
func TestFeishuOffboardListRunsClampsPageSize(t *testing.T) {
	svc := &stubFeishuOffboardService{list: &service.FeishuOffboardRunList{
		Items: []service.FeishuOffboardRun{}, Page: 1, PageSize: 100,
	}}
	rec := doFeishuRequest(newFeishuOffboardTestRouter(svc), http.MethodGet,
		"/api/v1/admin/feishu-offboard/runs?page=3&page_size=999", "")

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, svc.gotFilter)
	require.Equal(t, 3, svc.gotFilter.Page)
	require.Equal(t, feishuOffboardRunsMaxPageSize, svc.gotFilter.PageSize)
}

// 详情接口保留明细（这是它存在的理由），非法 id 走 400。
func TestFeishuOffboardGetRunReturnsDecisions(t *testing.T) {
	svc := &stubFeishuOffboardService{runByID: &service.FeishuOffboardRun{
		ID: 9,
		Decisions: []service.OffboardDecision{
			{UserID: 2, Email: "x@y.com", Verdict: service.OffboardVerdictResigned, Reason: "已离职"},
		},
	}}
	r := newFeishuOffboardTestRouter(svc)

	rec := doFeishuRequest(r, http.MethodGet, "/api/v1/admin/feishu-offboard/runs/9", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, int64(9), svc.gotID)
	require.Contains(t, rec.Body.String(), "x@y.com")

	bad := doFeishuRequest(r, http.MethodGet, "/api/v1/admin/feishu-offboard/runs/abc", "")
	require.Equal(t, http.StatusBadRequest, bad.Code)
}

// service 未注入时全部返回 503，而不是 panic 或 500。
func TestFeishuOffboardReturns503WhenServiceMissing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewFeishuOffboardHandler()
	r := gin.New()
	r.GET("/config", h.GetConfig)

	rec := doFeishuRequest(r, http.MethodGet, "/config", "")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
