// 私有扩展（不属于 upstream sub2api）。
//
// 功能：service.MonitorGroupUsageRepository 的 PostgreSQL 实现。
// 一次查询完成 分组 → 模型 两级聚合，返回给 /monitor 页 分组消耗 section。
//
// 数据源：
//   - usage_logs：成功计费的请求，提供请求数/token/cache/ttft/otps/cost
//   - ops_error_logs：仅 error_source='upstream_http'，作为成功率分母的"上游错误"计数
//   - user_allowed_groups：过滤当前用户可见分组
//   - groups：分组名回填（未在表中 fallback "未分组-<id>"）
//
// merge 策略：新文件，upstream 未来若接入类似聚合可整体删除或改造。
package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// monitorGroupUsageRepository 使用原生 SQL 完成两级聚合。
// 无需 ent 事务上下文，因此不持有 ent client。
type monitorGroupUsageRepository struct {
	db *sql.DB
}

// NewMonitorGroupUsageRepository 构造 repo。
func NewMonitorGroupUsageRepository(db *sql.DB) service.MonitorGroupUsageRepository {
	return &monitorGroupUsageRepository{db: db}
}

// AggregateByUserVisibleGroups 按用户可见分组聚合近 window 内消耗数据。
//
// 数据口径：
//   - 请求数        = 成功请求 + 上游错误
//   - 成功率        = 成功请求 / (成功请求 + 上游错误)
//   - 缓存率        = cache_read / (cache_read + input + cache_creation) * 100
//   - TTFT 均值/p90 = 仅统计 first_token_ms > 0 的记录（加权还原）
//   - OTPS 均值     = 单条 output_tokens/(decode_ms/1000)，加权还原；
//     decode_ms 有首字延迟样本（first_token_ms > 0 且 < duration_ms）时取 duration_ms-first_token_ms（扣除等待时间，只算生成阶段），
//     否则（如非流式请求没有首字延迟采样）退回整段 duration_ms
//   - 次均成本      = 总成本 / 成功请求数（错误请求没成本）
func (r *monitorGroupUsageRepository) AggregateByUserVisibleGroups(
	ctx context.Context,
	userID int64,
	window time.Duration,
) ([]*service.MonitorGroupUsage, error) {
	if userID <= 0 {
		return []*service.MonitorGroupUsage{}, nil
	}
	windowSeconds := int64(window.Seconds())
	if windowSeconds <= 0 {
		windowSeconds = 3600
	}

	// 说明：
	//  1) visible_groups 从 user_allowed_groups 取用户可见 group_id
	//  2) usage_agg 按 (group_id, model) 聚合 usage_logs，保留 ttft/otps 的 sum+count 以便外层加权
	//  3) err_agg  按 (group_id, model) 聚合 ops_error_logs（仅 upstream_http）
	//  4) 用 FULL OUTER JOIN 合并二者，保留双方独有的 (group, model)
	//  5) 外层按 group_id 汇总，并用 json_agg 打包模型数组
	const q = `
WITH visible_groups AS (
    SELECT group_id FROM user_allowed_groups WHERE user_id = $1
),
usage_agg AS (
    SELECT
        ul.group_id,
        ul.model,
        COUNT(*)                                                     AS req_success,
        SUM(ul.cache_read_tokens)::float8                            AS cache_read_sum,
        SUM(ul.cache_read_tokens + ul.input_tokens + ul.cache_creation_tokens)::float8 AS cache_denom_sum,
        SUM(ul.first_token_ms) FILTER (WHERE ul.first_token_ms > 0)  AS ttft_sum,
        COUNT(*) FILTER (WHERE ul.first_token_ms > 0)                AS ttft_count,
        PERCENTILE_CONT(0.9) WITHIN GROUP (ORDER BY ul.first_token_ms)
            FILTER (WHERE ul.first_token_ms > 0)                     AS ttft_p90,
        SUM(ul.output_tokens::float8 / (
                CASE WHEN ul.first_token_ms > 0 AND ul.duration_ms > ul.first_token_ms
                     THEN (ul.duration_ms - ul.first_token_ms)
                     ELSE ul.duration_ms
                END / 1000.0))
            FILTER (WHERE ul.duration_ms > 0 AND ul.output_tokens > 0) AS otps_sum,
        COUNT(*) FILTER (WHERE ul.duration_ms > 0 AND ul.output_tokens > 0) AS otps_count,
        SUM(ul.total_cost * COALESCE(ul.rate_multiplier, 1))::float8 AS total_cost
    FROM usage_logs ul
    WHERE ul.created_at >= NOW() - ($2::bigint || ' seconds')::interval
      AND ul.group_id IN (SELECT group_id FROM visible_groups)
    GROUP BY ul.group_id, ul.model
),
err_agg AS (
    SELECT
        el.group_id,
        el.model,
        COUNT(*) AS req_upstream_err
    FROM ops_error_logs el
    WHERE el.created_at >= NOW() - ($2::bigint || ' seconds')::interval
      AND el.error_source = 'upstream_http'
      AND el.group_id IN (SELECT group_id FROM visible_groups)
      AND el.model IS NOT NULL AND el.model <> ''
    GROUP BY el.group_id, el.model
),
model_stats AS (
    SELECT
        COALESCE(u.group_id, e.group_id)   AS group_id,
        COALESCE(u.model, e.model)         AS model,
        COALESCE(u.req_success, 0)         AS req_success,
        COALESCE(e.req_upstream_err, 0)    AS req_upstream_err,
        u.cache_read_sum, u.cache_denom_sum,
        u.ttft_sum, u.ttft_count, u.ttft_p90,
        u.otps_sum, u.otps_count,
        COALESCE(u.total_cost, 0)          AS total_cost
    FROM usage_agg u
    FULL OUTER JOIN err_agg e USING (group_id, model)
),
model_json AS (
    SELECT
        group_id,
        json_agg(
            json_build_object(
                'model',          model,
                'requests',       req_success + req_upstream_err,
                'success_rate',   CASE WHEN (req_success + req_upstream_err) > 0
                                       THEN ROUND(req_success::numeric * 100.0 / (req_success + req_upstream_err), 2)
                                       ELSE NULL END,
                'cache_hit_rate', CASE WHEN COALESCE(cache_denom_sum, 0) > 0
                                       THEN ROUND((cache_read_sum / cache_denom_sum * 100.0)::numeric, 2)
                                       ELSE NULL END,
                'ttft_avg_ms',    CASE WHEN COALESCE(ttft_count, 0) > 0
                                       THEN ROUND(ttft_sum::numeric / ttft_count)
                                       ELSE NULL END,
                'ttft_p90_ms',    CASE WHEN ttft_p90 IS NOT NULL
                                       THEN ROUND(ttft_p90::numeric)
                                       ELSE NULL END,
                'otps_avg',       CASE WHEN COALESCE(otps_count, 0) > 0
                                       THEN ROUND((otps_sum / otps_count)::numeric, 2)
                                       ELSE NULL END,
                'cost_avg',       CASE WHEN req_success > 0
                                       THEN ROUND((total_cost / req_success)::numeric, 8)
                                       ELSE NULL END,
                'total_cost',     ROUND(total_cost::numeric, 6)
            )
            ORDER BY (req_success + req_upstream_err) DESC
        ) AS models
    FROM model_stats
    GROUP BY group_id
),
group_stats AS (
    SELECT
        group_id,
        SUM(req_success)      AS req_success,
        SUM(req_upstream_err) AS req_upstream_err,
        SUM(cache_read_sum)   AS cache_read_sum,
        SUM(cache_denom_sum)  AS cache_denom_sum,
        SUM(ttft_sum)         AS ttft_sum,
        SUM(ttft_count)       AS ttft_count,
        -- 分组级 TTFT p90 用汇总口径：把所有 first_token_ms 归到一组再算，
        -- 这里通过 sub-query 处理，避免对模型 p90 再平均带来的误差
        SUM(otps_sum)         AS otps_sum,
        SUM(otps_count)       AS otps_count,
        SUM(total_cost)       AS total_cost
    FROM model_stats
    GROUP BY group_id
),
group_ttft_p90 AS (
    SELECT
        ul.group_id,
        PERCENTILE_CONT(0.9) WITHIN GROUP (ORDER BY ul.first_token_ms)
            FILTER (WHERE ul.first_token_ms > 0) AS ttft_p90
    FROM usage_logs ul
    WHERE ul.created_at >= NOW() - ($2::bigint || ' seconds')::interval
      AND ul.group_id IN (SELECT group_id FROM visible_groups)
    GROUP BY ul.group_id
)
SELECT
    gs.group_id,
    COALESCE(g.name, '未分组-' || COALESCE(gs.group_id::text, 'null')) AS group_name,
    (gs.req_success + gs.req_upstream_err)                              AS requests,
    CASE WHEN (gs.req_success + gs.req_upstream_err) > 0
         THEN ROUND(gs.req_success::numeric * 100.0 / (gs.req_success + gs.req_upstream_err), 2)
         ELSE NULL END                                                  AS success_rate,
    CASE WHEN COALESCE(gs.cache_denom_sum, 0) > 0
         THEN ROUND((gs.cache_read_sum / gs.cache_denom_sum * 100.0)::numeric, 2)
         ELSE NULL END                                                  AS cache_hit_rate,
    CASE WHEN COALESCE(gs.ttft_count, 0) > 0
         THEN ROUND(gs.ttft_sum::numeric / gs.ttft_count)
         ELSE NULL END                                                  AS ttft_avg_ms,
    CASE WHEN gp.ttft_p90 IS NOT NULL
         THEN ROUND(gp.ttft_p90::numeric)
         ELSE NULL END                                                  AS ttft_p90_ms,
    CASE WHEN COALESCE(gs.otps_count, 0) > 0
         THEN ROUND((gs.otps_sum / gs.otps_count)::numeric, 2)
         ELSE NULL END                                                  AS otps_avg,
    CASE WHEN gs.req_success > 0
         THEN ROUND((gs.total_cost / gs.req_success)::numeric, 8)
         ELSE NULL END                                                  AS cost_avg,
    ROUND(gs.total_cost::numeric, 6)                                    AS total_cost,
    mj.models
FROM group_stats gs
LEFT JOIN groups g          ON g.id = gs.group_id
LEFT JOIN model_json mj     ON mj.group_id IS NOT DISTINCT FROM gs.group_id
LEFT JOIN group_ttft_p90 gp ON gp.group_id IS NOT DISTINCT FROM gs.group_id
ORDER BY (gs.req_success + gs.req_upstream_err) DESC
`

	rows, err := r.db.QueryContext(ctx, q, userID, windowSeconds)
	if err != nil {
		return nil, fmt.Errorf("query monitor group usage: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]*service.MonitorGroupUsage, 0)
	for rows.Next() {
		var (
			groupID      sql.NullInt64
			groupName    string
			requests     int64
			successRate  sql.NullFloat64
			cacheHitRate sql.NullFloat64
			ttftAvgMs    sql.NullInt64
			ttftP90Ms    sql.NullInt64
			otpsAvg      sql.NullFloat64
			costAvg      sql.NullFloat64
			totalCost    float64
			modelsJSON   sql.NullString
		)
		if err := rows.Scan(
			&groupID, &groupName, &requests,
			&successRate, &cacheHitRate,
			&ttftAvgMs, &ttftP90Ms,
			&otpsAvg, &costAvg, &totalCost,
			&modelsJSON,
		); err != nil {
			return nil, fmt.Errorf("scan monitor group usage row: %w", err)
		}

		g := &service.MonitorGroupUsage{
			GroupName: groupName,
			Requests:  requests,
			TotalCost: totalCost,
			Models:    []*service.MonitorGroupUsageModel{},
		}
		if groupID.Valid {
			id := groupID.Int64
			g.GroupID = &id
		}
		if successRate.Valid {
			v := successRate.Float64
			g.SuccessRate = &v
		}
		if cacheHitRate.Valid {
			v := cacheHitRate.Float64
			g.CacheHitRate = &v
		}
		if ttftAvgMs.Valid {
			v := ttftAvgMs.Int64
			g.TTFTAvgMs = &v
		}
		if ttftP90Ms.Valid {
			v := ttftP90Ms.Int64
			g.TTFTP90Ms = &v
		}
		if otpsAvg.Valid {
			v := otpsAvg.Float64
			g.OTPSAvg = &v
		}
		if costAvg.Valid {
			v := costAvg.Float64
			g.CostAvg = &v
		}
		if modelsJSON.Valid && modelsJSON.String != "" {
			var models []*service.MonitorGroupUsageModel
			if err := json.Unmarshal([]byte(modelsJSON.String), &models); err != nil {
				return nil, fmt.Errorf("decode models json: %w", err)
			}
			if models != nil {
				g.Models = models
			}
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate monitor group usage rows: %w", err)
	}
	return out, nil
}
