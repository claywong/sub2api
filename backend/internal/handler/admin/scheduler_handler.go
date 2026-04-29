package admin

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// SchedulerAdminHandler 暴露调度器实时观测接口（weighted 算法）。
// 仅作只读 dump，不改变运行时状态。
type SchedulerAdminHandler struct {
	gatewayService *service.GatewayService
}

// NewSchedulerAdminHandler 创建 admin scheduler handler。
func NewSchedulerAdminHandler(gatewayService *service.GatewayService) *SchedulerAdminHandler {
	return &SchedulerAdminHandler{
		gatewayService: gatewayService,
	}
}

// GetMetrics 返回 weighted 调度器的内存计数器快照。
// GET /api/v1/admin/scheduler/metrics
func (h *SchedulerAdminHandler) GetMetrics(c *gin.Context) {
	response.Success(c, service.SchedulerMetrics().Snapshot())
}

// SchedulerSnapshotItem 单账号的调度视图。
type SchedulerSnapshotItem struct {
	AccountID     int64                  `json:"account_id"`
	Verdict       string                 `json:"verdict"`
	VerdictReason string                 `json:"verdict_reason,omitempty"`
	Window        SchedulerSnapshotStats `json:"window"`
}

// SchedulerSnapshotStats 滑动窗口聚合数据。
type SchedulerSnapshotStats struct {
	ReqCount        int     `json:"req_count"`
	ErrCount        int     `json:"err_count"`
	SlowCount       int     `json:"slow_count"`
	ErrRate         float64 `json:"err_rate"`
	SlowRate        float64 `json:"slow_rate"`
	TTFTSampleCount int     `json:"ttft_sample_count"`
	TTFTAvgMs       float64 `json:"ttft_avg_ms"`
	OTPSSampleCount int     `json:"otps_sample_count"`
	OTPSAvg         float64 `json:"otps_avg"`
}

// GetSnapshot 返回当前所有账号的健康窗口快照（按需可加 group_id 过滤）。
// GET /api/v1/admin/scheduler/snapshot
// 可选 query: account_id（指定单个账号）
func (h *SchedulerAdminHandler) GetSnapshot(c *gin.Context) {
	if h.gatewayService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Gateway service not available")
		return
	}
	cache := h.gatewayService.HealthCache()
	if cache == nil {
		response.Success(c, []SchedulerSnapshotItem{})
		return
	}

	// 单账号查询
	if accountIDStr := strings.TrimSpace(c.Query("account_id")); accountIDStr != "" {
		id, err := strconv.ParseInt(accountIDStr, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "invalid account_id")
			return
		}
		item := buildSnapshotItem(cache, id)
		response.Success(c, item)
		return
	}

	// 全量：枚举 healthCache 中所有有过记录的账号
	ids := cache.ListTrackedAccountIDs()
	out := make([]SchedulerSnapshotItem, 0, len(ids))
	for _, id := range ids {
		out = append(out, buildSnapshotItem(cache, id))
	}
	response.Success(c, out)
}

// buildSnapshotItem 把 cache.Snapshot(id) + verdict 拼装成 admin 视图。
func buildSnapshotItem(cache *service.AccountTestHealthCache, accountID int64) SchedulerSnapshotItem {
	s := cache.Snapshot(accountID)
	v, reason := cache.HealthVerdictWithReason(accountID)
	return SchedulerSnapshotItem{
		AccountID:     accountID,
		Verdict:       v.String(),
		VerdictReason: reason,
		Window: SchedulerSnapshotStats{
			ReqCount:        s.ReqCount,
			ErrCount:        s.ErrCount,
			SlowCount:       s.SlowCount,
			ErrRate:         s.ErrRate(),
			SlowRate:        s.SlowRate(),
			TTFTSampleCount: s.TTFTSampleCount,
			TTFTAvgMs:       s.TTFTAvg(),
			OTPSSampleCount: s.OTPSSampleCount,
			OTPSAvg:         s.OTPSAvg(),
		},
	}
}
