// 私有扩展（不属于 upstream sub2api）。
//
// 功能：/monitor 页 "分组消耗（近 1h）" section 的聚合服务。
// 按当前登录用户可见分组，聚合近 1 小时 usage_logs 与 ops_error_logs，
// 得到 分组 → 模型 两级的：请求数 / 成功率 / 缓存率 / TTFT均值 / TTFT p90
// / OTPS均值 / 次均成本 / 总成本。
//
// merge 策略：新文件，不动 upstream。若 upstream 未来提供类似能力，可整体迁移或删除。
package service

import (
	"context"
	"time"
)

// MonitorGroupUsageWindow 聚合窗口，固定 1 小时。
// 独立成常量以便测试与后续调整。
const MonitorGroupUsageWindow = time.Hour

// MonitorGroupUsageRepository 是 monitor 分组消耗聚合的仓储接口。
// repo 具体实现在 repository/monitor_group_usage_repo.go。
type MonitorGroupUsageRepository interface {
	// AggregateByVisibleGroups 按传入的可见分组 ID 集合聚合近 window 内的消耗数据。
	// 可见分组由 service 层（复用 GetAvailableGroups 口径）算好后传入，
	// repo 只负责按 group_id 集合过滤，不再自行查 user_allowed_groups。
	AggregateByVisibleGroups(
		ctx context.Context,
		groupIDs []int64,
		window time.Duration,
	) ([]*MonitorGroupUsage, error)
}

// UserVisibleGroupsProvider 提供"某用户可见分组"的权威口径。
// 由 *APIKeyService.GetAvailableGroups 实现：公开分组 ∪ 用户专属分组 ∪ 有效订阅分组。
type UserVisibleGroupsProvider interface {
	GetAvailableGroups(ctx context.Context, userID int64) ([]Group, error)
}

// MonitorGroupUsage 单个分组的聚合结果，含分组下所有模型明细。
type MonitorGroupUsage struct {
	GroupID      *int64                    `json:"group_id"`
	GroupName    string                    `json:"group_name"`
	Requests     int64                     `json:"requests"` // success + upstream_err
	SuccessRate  *float64                  `json:"success_rate"`
	CacheHitRate *float64                  `json:"cache_hit_rate"`
	TTFTAvgMs    *int64                    `json:"ttft_avg_ms"`
	TTFTP90Ms    *int64                    `json:"ttft_p90_ms"`
	OTPSAvg      *float64                  `json:"otps_avg"`
	CostAvg      *float64                  `json:"cost_avg"`
	TotalCost    float64                   `json:"total_cost"`
	Models       []*MonitorGroupUsageModel `json:"models"`
}

// MonitorGroupUsageModel 分组内单个模型的聚合结果。
type MonitorGroupUsageModel struct {
	Model        string   `json:"model"`
	Requests     int64    `json:"requests"`
	SuccessRate  *float64 `json:"success_rate"`
	CacheHitRate *float64 `json:"cache_hit_rate"`
	TTFTAvgMs    *int64   `json:"ttft_avg_ms"`
	TTFTP90Ms    *int64   `json:"ttft_p90_ms"`
	OTPSAvg      *float64 `json:"otps_avg"`
	CostAvg      *float64 `json:"cost_avg"`
	TotalCost    float64  `json:"total_cost"`
}

// MonitorGroupUsageService 提供分组消耗聚合能力。
type MonitorGroupUsageService struct {
	repo     MonitorGroupUsageRepository
	visibleG UserVisibleGroupsProvider
}

// NewMonitorGroupUsageService 构造服务实例。
// visibleG 提供"用户可见分组"权威口径（*APIKeyService.GetAvailableGroups）。
func NewMonitorGroupUsageService(
	repo MonitorGroupUsageRepository,
	visibleG UserVisibleGroupsProvider,
) *MonitorGroupUsageService {
	return &MonitorGroupUsageService{repo: repo, visibleG: visibleG}
}

// ListForUser 返回登录用户可见分组在近 1h 的聚合结果。
// 可见分组口径 = 公开分组 ∪ 用户专属分组 ∪ 有效订阅分组（复用 GetAvailableGroups）。
// 未找到任何数据时返回空切片（不是 nil）。
func (s *MonitorGroupUsageService) ListForUser(
	ctx context.Context,
	userID int64,
) ([]*MonitorGroupUsage, error) {
	if userID <= 0 {
		return []*MonitorGroupUsage{}, nil
	}

	visible, err := s.visibleG.GetAvailableGroups(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(visible) == 0 {
		return []*MonitorGroupUsage{}, nil
	}
	groupIDs := make([]int64, 0, len(visible))
	for _, g := range visible {
		groupIDs = append(groupIDs, g.ID)
	}

	groups, err := s.repo.AggregateByVisibleGroups(ctx, groupIDs, MonitorGroupUsageWindow)
	if err != nil {
		return nil, err
	}
	if groups == nil {
		return []*MonitorGroupUsage{}, nil
	}
	return groups, nil
}
