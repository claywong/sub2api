// 私有扩展（不属于 upstream sub2api）。
//
// 本文件：「飞书离职自动禁用」的 HTTP 层。
// 所含内容：FeishuOffboardService 接口（handler 侧依赖倒置）、
// FeishuOffboardHandler 及其 6 个接口方法（配置读写 / 连通性测试 / 手动触发 / 执行历史）。
// merge 策略：纯新增文件，与 upstream 无交集，merge 时保留即可。
// 路由注册见 server/routes/admin.go 的 registerFeishuOffboardRoutes。
//
// @author wangzhong
package admin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// FeishuOffboardService 是 handler 需要的服务能力。
//
// 这里声明接口而不是直接依赖 service 里的具体类型，有两个原因：
//  1. HTTP 层与判定/执行逻辑可以并行开发、各自独立编译；
//  2. 单测里可以塞一个假实现，不必起飞书客户端和数据库。
type FeishuOffboardService interface {
	// LoadConfigView 读取配置，密钥只回"是否已配置"。
	LoadConfigView(ctx context.Context) (service.FeishuOffboardConfigView, error)
	// SaveConfig 保存配置，AppSecret 为空表示不修改。
	SaveConfig(ctx context.Context, input service.FeishuOffboardConfigInput) error
	// TestConnection 用当前配置验证飞书凭据可用。
	TestConnection(ctx context.Context) error
	// TriggerManual 立即执行一次，返回本次执行记录（含每人判定依据）。
	// dryRun 为 nil 表示沿用配置里的 dry_run，非 nil 才覆盖；
	// 用指针是为了不把「没指定」误当成「真执行」。
	TriggerManual(ctx context.Context, dryRun *bool) (*service.FeishuOffboardRun, error)
	// ListRuns 按 run_at 倒序分页查询执行历史。
	ListRuns(ctx context.Context, filter service.FeishuOffboardRunListFilter) (*service.FeishuOffboardRunList, error)
	// GetRun 取单次执行详情。
	GetRun(ctx context.Context, id int64) (*service.FeishuOffboardRun, error)
}

// 编译期断言：service 层的具体实现必须满足本文件的接口。
// 少了这行，service 侧改签名时 handler 不会报错，直到运行时注入才发现对不上。
var _ FeishuOffboardService = (*service.FeishuOffboardService)(nil)

// feishuOffboardRunsMaxPageSize 执行历史页大小上限。
// 与 repository 侧的 feishuOffboardRunMaxPageSize 一致：两层各自兜底，
// 避免将来某一侧被绕过时前端能拉走整张表。
const feishuOffboardRunsMaxPageSize = 100

// FeishuOffboardHandler 飞书离职自动禁用的管理接口。
// 全部路由挂在 admin 鉴权组下，变更类操作由全局审计中间件自动留痕。
type FeishuOffboardHandler struct {
	svc FeishuOffboardService
}

// NewFeishuOffboardHandler 构造 handler。
//
// 刻意不在构造函数里收 service：wire 图中判定/执行服务的位置晚于 handler，
// 走 SetService 注入可以不调整既有构造顺序（与 AccountHandler 的
// SetUpstreamBillingProbeService 同一套做法）。未注入时所有接口返回 503。
func NewFeishuOffboardHandler() *FeishuOffboardHandler {
	return &FeishuOffboardHandler{}
}

// SetService 注入服务实现。
func (h *FeishuOffboardHandler) SetService(svc FeishuOffboardService) {
	if h == nil {
		return
	}
	h.svc = svc
}

// ready 校验服务已注入；未注入时直接写 503 并返回 false。
// 功能未接线时返回 503 而不是 500，是为了让前端能区分"没部署好"和"跑挂了"。
func (h *FeishuOffboardHandler) ready(c *gin.Context) bool {
	if h == nil || h.svc == nil {
		response.Error(c, http.StatusServiceUnavailable, "Feishu offboard service not available")
		return false
	}
	return true
}

// GetConfig 读取配置。
// GET /api/v1/admin/feishu-offboard/config
//
// 响应用 service.FeishuOffboardConfigView，其中只有 app_secret_configured 布尔，
// 没有 app_secret 字段——密钥一旦下发就等于泄露，前端也不需要回显它。
func (h *FeishuOffboardHandler) GetConfig(c *gin.Context) {
	if !h.ready(c) {
		return
	}

	cfg, err := h.svc.LoadConfigView(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, cfg)
}

// UpdateConfig 保存配置。
// PUT /api/v1/admin/feishu-offboard/config
//
// app_secret 传空字符串表示"保持原值不变"，因此前端可以在不回显密钥的情况下
// 只改开关或 cron。真正的语义校验（cron 是否合法、阈值区间）由 service 负责，
// 这里只做入参清洗和明显非法值的拦截。
func (h *FeishuOffboardHandler) UpdateConfig(c *gin.Context) {
	if !h.ready(c) {
		return
	}

	var input service.FeishuOffboardConfigInput
	if err := c.ShouldBindJSON(&input); err != nil {
		response.BadRequest(c, "Invalid request body")
		return
	}

	// 去空白：AppID / Schedule 前后带空格是复制粘贴的常见后果，
	// 直接存进去会让 cron 解析和飞书鉴权莫名失败。
	input.AppID = strings.TrimSpace(input.AppID)
	input.AppSecret = strings.TrimSpace(input.AppSecret)
	input.Schedule = strings.TrimSpace(input.Schedule)
	input.NotifyTo = normalizeFeishuNotifyTo(input.NotifyTo)

	// 负阈值会让熔断逻辑恒为真（等于功能永不执行），属于配置错误而非合法诉求。
	if input.Threshold < 0 {
		response.BadRequest(c, "circuit_breaker_threshold must not be negative")
		return
	}

	if err := h.svc.SaveConfig(c.Request.Context(), input); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// 回读一次再返回，前端就不必自己拼"保存后的状态"（尤其是 app_secret_configured
	// 会因为本次是否传了密钥而变化）。回读失败不算保存失败，降级成空响应。
	cfg, err := h.svc.LoadConfigView(c.Request.Context())
	if err != nil {
		response.Success(c, nil)
		return
	}
	response.Success(c, cfg)
}

// normalizeFeishuNotifyTo 清洗收件人列表：去空白、丢空项、去重且保持原顺序。
// 重复收件人会导致同一封结果邮件被投递多次。
func normalizeFeishuNotifyTo(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(list))
	out := make([]string, 0, len(list))
	for _, item := range list {
		addr := strings.TrimSpace(item)
		if addr == "" {
			continue
		}
		key := strings.ToLower(addr)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, addr)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// TestConnection 测试飞书连通性。
// POST /api/v1/admin/feishu-offboard/test
//
// 只验证凭据能换到 token、通讯录接口可读，不做任何判定、不禁用任何人。
func (h *FeishuOffboardHandler) TestConnection(c *gin.Context) {
	if !h.ready(c) {
		return
	}

	if err := h.svc.TestConnection(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"ok": true})
}

// feishuOffboardRunRequest 手动触发的请求体。
//
// dry_run 用指针是为了区分"没传"和"显式传 false"：前者沿用系统配置，
// 后者是管理员明确要求真跑。这个区别直接决定会不会真的禁用账号。
type feishuOffboardRunRequest struct {
	DryRun *bool `json:"dry_run"`
}

// feishuOffboardRunResponse 手动触发的响应。
//
// 额外带一个 summary 是刻意的：手动触发是破坏性操作，前端必须能一眼看到
// "这次是不是演练""到底禁了几个人"，而不是从 run 的十几个字段里自己推断。
type feishuOffboardRunResponse struct {
	Run     *service.FeishuOffboardRun `json:"run"`
	Summary feishuOffboardRunSummary   `json:"summary"`
}

// feishuOffboardRunSummary 本次执行的判定汇总。
type feishuOffboardRunSummary struct {
	// DryRun 是本次实际生效的模式，来自服务端返回的执行记录，
	// 而不是请求里传的值——系统配置可能强制 dry-run，此时请求传 false 也不会真禁。
	DryRun bool `json:"dry_run"`
	// CircuitBroken 命中熔断阈值：判定照做，禁用全部跳过。
	CircuitBroken     bool   `json:"circuit_broken"`
	CheckedCount      int    `json:"checked_count"`
	ResignedCount     int    `json:"resigned_count"`
	DisabledCount     int    `json:"disabled_count"`
	UnverifiableCount int    `json:"unverifiable_count"`
	SkippedCount      int    `json:"skipped_count"`
	DurationMs        int64  `json:"duration_ms"`
	ErrorMessage      string `json:"error_message,omitempty"`
}

// TriggerRun 手动触发一次执行。
// POST /api/v1/admin/feishu-offboard/run
//
// 破坏性操作：非 dry-run 模式下会真的把判定为离职的账号禁用掉。
// 请求体可选，形如 {"dry_run": true}；不传则沿用系统配置里的 dry_run 开关。
func (h *FeishuOffboardHandler) TriggerRun(c *gin.Context) {
	if !h.ready(c) {
		return
	}

	// 请求体允许为空（等价于 {}），所以 EOF 不当错误处理。
	var req feishuOffboardRunRequest
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		response.BadRequest(c, "Invalid request body")
		return
	}

	// req.DryRun 为 nil 时原样传 nil，让服务层沿用配置里的 dry_run。
	// 这里绝不能兜成 false：那会把管理员配置的空跑模式静默变成真禁用。
	run, err := h.svc.TriggerManual(c.Request.Context(), req.DryRun)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if run == nil {
		response.Error(c, http.StatusInternalServerError, "Feishu offboard run produced no result")
		return
	}

	response.Success(c, feishuOffboardRunResponse{
		Run: run,
		Summary: feishuOffboardRunSummary{
			DryRun:            run.DryRun,
			CircuitBroken:     run.CircuitBroken,
			CheckedCount:      run.CheckedCount,
			ResignedCount:     run.ResignedCount,
			DisabledCount:     run.DisabledCount,
			UnverifiableCount: run.UnverifiableCount,
			SkippedCount:      run.SkippedCount,
			DurationMs:        run.DurationMs,
			ErrorMessage:      run.ErrorMessage,
		},
	})
}

// ListRuns 执行历史分页。
// GET /api/v1/admin/feishu-offboard/runs
//
// 刻意不返回每人的判定明细：一次执行可能有几百条 decisions，
// 全塞进列表会让响应体膨胀到没法用。明细只在 /runs/:id 提供。
func (h *FeishuOffboardHandler) ListRuns(c *gin.Context) {
	if !h.ready(c) {
		return
	}

	page, pageSize := response.ParsePagination(c)
	if pageSize > feishuOffboardRunsMaxPageSize {
		pageSize = feishuOffboardRunsMaxPageSize
	}

	result, err := h.svc.ListRuns(c.Request.Context(), service.FeishuOffboardRunListFilter{
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if result == nil {
		response.Paginated(c, []service.FeishuOffboardRun{}, 0, page, pageSize)
		return
	}

	// 在 HTTP 边界再抹一次 decisions：即便将来某个实现忘了裁剪，
	// 列表接口也不会突然吐出几百条明细。Items 是值切片，改的是副本。
	items := result.Items
	for i := range items {
		items[i].Decisions = nil
	}
	response.Paginated(c, items, result.Total, result.Page, result.PageSize)
}

// GetRun 单次执行详情（含每人判定依据）。
// GET /api/v1/admin/feishu-offboard/runs/:id
//
// decisions 里保留了当时飞书返回的原始状态位，用来回答"凭什么禁了这个人"。
func (h *FeishuOffboardHandler) GetRun(c *gin.Context) {
	if !h.ready(c) {
		return
	}

	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid run id")
		return
	}

	run, err := h.svc.GetRun(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if run == nil {
		response.NotFound(c, "Feishu offboard run not found")
		return
	}
	response.Success(c, run)
}
