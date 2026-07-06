import type { UsageLog } from '@/types'

type OtpsRow = Pick<UsageLog, 'output_tokens' | 'duration_ms' | 'first_token_ms'>

/**
 * OTPS（Output Tokens Per Second，生成阶段速度）。
 * 用 (duration_ms - first_token_ms) 作为生成耗时分母，扣除首字延迟对速度的影响。
 * 与 monitor 页面分组用量按请求整段耗时统计的 otps_avg 口径不同，仅用于用量明细单条展示。
 */
export function calculateOtps(row: OtpsRow | null | undefined): number | null {
  const outputTokens = row?.output_tokens ?? 0
  const durationMs = row?.duration_ms
  const firstTokenMs = row?.first_token_ms
  if (outputTokens <= 0 || durationMs == null || firstTokenMs == null) return null
  const generationMs = durationMs - firstTokenMs
  if (generationMs <= 0) return null
  return outputTokens / (generationMs / 1000)
}

export function formatOtps(row: OtpsRow | null | undefined): string | null {
  const otps = calculateOtps(row)
  if (otps == null) return null
  return `${otps.toFixed(1)} t/s`
}
