package admin

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// SchedulerAdminHandler 暴露调度器实时观测接口。
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

// QualityItem 单账号+模型的质量指标视图。
type QualityItem struct {
	AccountID           int64   `json:"account_id"`
	Model               string  `json:"model"`
	Score               float64 `json:"score"`
	TTFTSampleCount     int     `json:"ttft_sample_count"`
	TTFTAvgMs           float64 `json:"ttft_avg_ms"`
	TTFTP90Ms           float64 `json:"ttft_p90_ms"`
	TTFTEffMs           float64 `json:"ttft_eff_ms"` // 0.5*avg+0.5*p90，加权选号实际使用值
	OTPSSampleCount     int     `json:"otps_sample_count"`
	OTPSAvg             float64 `json:"otps_avg"`
	CacheHitSampleCount int     `json:"cache_hit_sample_count"`
	CacheHitRateAvg     float64 `json:"cache_hit_rate_avg"`
	TTFTBucket          int     `json:"ttft_bucket"`
	OTPSBucket          int     `json:"otps_bucket"`
	CacheHitBucket      int     `json:"cache_hit_bucket"`
}

// GetQuality 返回 account+model 维度的质量指标快照。
// GET /api/v1/admin/scheduler/quality
// 可选 query: account_id, model
func (h *SchedulerAdminHandler) GetQuality(c *gin.Context) {
	if h.gatewayService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Gateway service not available")
		return
	}
	cache := h.gatewayService.ModelQualityCache()
	if cache == nil {
		response.Success(c, []QualityItem{})
		return
	}

	filterAccountID := int64(0)
	if s := strings.TrimSpace(c.Query("account_id")); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "invalid account_id")
			return
		}
		filterAccountID = id
	}
	filterModel := strings.TrimSpace(c.Query("model"))

	keys := cache.ListTrackedKeys()
	out := make([]QualityItem, 0, len(keys))
	for _, key := range keys {
		if filterAccountID > 0 && key.AccountID() != filterAccountID {
			continue
		}
		if filterModel != "" && key.Model() != filterModel {
			continue
		}
		out = append(out, buildQualityItem(cache, key, h.gatewayService))
	}
	response.Success(c, out)
}

func buildQualityItem(cache *service.AccountModelQualityCache, key service.AccountModelCacheKey, gw *service.GatewayService) QualityItem {
	snap, p90 := cache.SnapshotWithP90(key.AccountID(), key.Model())
	ttftEff := 0.0
	if snap.HasTTFT() {
		ttftEff = snap.TTFTAvg()
		if p90 > 0 {
			ttftEff = 0.5*snap.TTFTAvg() + 0.5*p90
		}
	}
	score := 0.0
	if gw != nil {
		score = gw.QualityScoreForAccount(key.AccountID(), key.Model())
	}
	return QualityItem{
		AccountID:           key.AccountID(),
		Model:               key.Model(),
		Score:               score,
		TTFTSampleCount:     snap.TTFTSampleCount,
		TTFTAvgMs:           snap.TTFTAvg(),
		TTFTP90Ms:           p90,
		TTFTEffMs:           ttftEff,
		OTPSSampleCount:     snap.OTPSSampleCount,
		OTPSAvg:             snap.OTPSAvg(),
		CacheHitSampleCount: snap.CacheHitSampleCount,
		CacheHitRateAvg:     snap.CacheHitRateAvg(),
		TTFTBucket:          service.TTFTBucket(snap.TTFTAvg(), snap.HasTTFT()),
		OTPSBucket:          service.OTPSBucket(snap.OTPSAvg(), snap.HasOTPS()),
		CacheHitBucket:      service.CacheHitBucket(snap.CacheHitRateAvg(), snap.HasCacheHit()),
	}
}
