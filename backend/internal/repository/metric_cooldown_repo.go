package repository

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"time"
)

type metricCooldownRepository struct {
	db *sql.DB
}

// NewMetricCooldownRepository 返回基于 *sql.DB 的实现，直接聚合 usage_logs。
func NewMetricCooldownRepository(db *sql.DB) service.MetricCooldownRepository {
	return &metricCooldownRepository{db: db}
}

const metricCooldownAggregateSQL = `
SELECT
    account_id,
    COUNT(*) AS samples,
    AVG(first_token_ms) FILTER (WHERE first_token_ms IS NOT NULL AND first_token_ms > 0) AS ttft_avg,
    AVG(
        CASE WHEN duration_ms IS NOT NULL AND duration_ms > 0
        THEN output_tokens::numeric / (duration_ms / 1000.0) END
    ) AS otps_avg,
    CASE
        WHEN SUM(cache_read_tokens) + SUM(input_tokens) + SUM(cache_creation_tokens) > 0
        THEN SUM(cache_read_tokens)::numeric
             / (SUM(cache_read_tokens) + SUM(input_tokens) + SUM(cache_creation_tokens)) * 100
        ELSE NULL
    END AS cache_hit_rate,
    CASE WHEN COUNT(*) > 0
        THEN SUM(total_cost * COALESCE(account_rate_multiplier, 1.0))::numeric / COUNT(*)
        ELSE NULL
    END AS cost_per_req
FROM usage_logs
WHERE created_at >= $1 AND created_at < $2
GROUP BY account_id
HAVING COUNT(*) >= $3
`

func (r *metricCooldownRepository) AggregateAccountMetrics(ctx context.Context, since, end time.Time, minSamples int) ([]service.AccountMetricSnapshot, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("metric cooldown repository: nil db")
	}
	rows, err := r.db.QueryContext(ctx, metricCooldownAggregateSQL, since, end, minSamples)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []service.AccountMetricSnapshot
	for rows.Next() {
		var (
			snap         service.AccountMetricSnapshot
			ttft         sql.NullFloat64
			otps         sql.NullFloat64
			cacheHitRate sql.NullFloat64
			costPerReq   sql.NullFloat64
		)
		if err := rows.Scan(&snap.AccountID, &snap.Samples, &ttft, &otps, &cacheHitRate, &costPerReq); err != nil {
			return nil, err
		}
		if ttft.Valid {
			v := ttft.Float64
			snap.TTFTMs = &v
		}
		if otps.Valid {
			v := otps.Float64
			snap.OTPS = &v
		}
		if cacheHitRate.Valid {
			v := cacheHitRate.Float64
			snap.CacheHitRate = &v
		}
		if costPerReq.Valid {
			v := costPerReq.Float64
			snap.CostPerReq = &v
		}
		out = append(out, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
