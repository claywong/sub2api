package service

import (
	"context"
	"time"
)

// AccountMetricSnapshot 单个账号在一段窗口内的聚合指标。
// 任一指标为 nil 表示样本不足（如所有 first_token_ms <= 0）。
type AccountMetricSnapshot struct {
	AccountID    int64
	Samples      int
	TTFTMs       *float64 // 平均首 token 延迟 (ms)
	OTPS         *float64 // 平均输出 tokens/秒
	CacheHitRate *float64 // 缓存命中率，单位百分比 0-100
	CostPerReq   *float64 // 均次成本 (与 usage_logs.total_cost 同单位)
}

// MetricCooldownRepository 聚合 usage_logs 出指标快照。
type MetricCooldownRepository interface {
	// AggregateAccountMetrics 返回所有 created_at >= since 且 created_at < end 的 usage_logs
	// 按 account_id 聚合的指标，仅包含 samples >= minSamples 的账号。
	AggregateAccountMetrics(ctx context.Context, since, end time.Time, minSamples int) ([]AccountMetricSnapshot, error)
}
