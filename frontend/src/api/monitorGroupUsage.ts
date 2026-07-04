/**
 * Private extension (not upstream sub2api).
 *
 * /monitor page — 分组消耗 (last 1 hour) section API.
 *
 * Returns per-user visible groups aggregated over the last hour, with 分组 → 模型
 * two-level nesting. Metrics: requests, success_rate, cache_hit_rate, ttft_avg_ms,
 * ttft_p90_ms, otps_avg, cost_avg, total_cost.
 */
import { apiClient } from './client'

export interface MonitorGroupUsageModel {
  model: string
  requests: number
  success_rate: number | null
  cache_hit_rate: number | null
  ttft_avg_ms: number | null
  ttft_p90_ms: number | null
  otps_avg: number | null
  cost_avg: number | null
  total_cost: number
}

export interface MonitorGroupUsageGroup {
  group_id: number | null
  group_name: string
  requests: number
  success_rate: number | null
  cache_hit_rate: number | null
  ttft_avg_ms: number | null
  ttft_p90_ms: number | null
  otps_avg: number | null
  cost_avg: number | null
  total_cost: number
  models: MonitorGroupUsageModel[]
}

export interface MonitorGroupUsageResponse {
  window_seconds: number
  generated_at: string
  groups: MonitorGroupUsageGroup[]
}

export async function fetchMonitorGroupUsage(options?: {
  signal?: AbortSignal
}): Promise<MonitorGroupUsageResponse> {
  const { data } = await apiClient.get<MonitorGroupUsageResponse>('/monitor/group-usage', {
    signal: options?.signal,
  })
  return data
}

export default { fetchMonitorGroupUsage }
