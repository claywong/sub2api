import { beforeEach, describe, expect, it, vi } from 'vitest'
import { emptyEventFilters } from '@/features/prompt-audit/viewModel'

const client = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn(), post: vi.fn(), delete: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: client }))

import dlpAPI, { DLP_EVENT_SOURCE, DLP_SCANNER_BACKEND } from '../api'

// 有明确时间范围的筛选条件：按条件删除要求显式区间。
function rangedFilters() {
  const filters = emptyEventFilters()
  filters.start_at = '2026-07-15T00:00'
  filters.end_at = '2026-07-16T00:00'
  return filters
}

describe('DLP API', () => {
  beforeEach(() => Object.values(client).forEach((mock) => mock.mockReset()))

  it('reuses the prompt-audit config endpoint', async () => {
    // 刻意复用而不新开 /admin/dlp/config：DLP 配置就存在 PromptAuditConfig 的
    // dlp 子树里，另开接口要把后端存储也拆开。
    client.get.mockResolvedValue({ data: { config_version: 1, dlp: {} } })
    await dlpAPI.getConfig()
    expect(client.get).toHaveBeenCalledWith('/admin/prompt-audit/config')
  })

  it('scopes the event list to DLP events', async () => {
    // 不带 source 的话列表会混进 qwen3guard 的拦截记录，
    // 管理员无法判断到底是哪套策略生效。
    client.get.mockResolvedValue({ data: { items: [], total: 0, page: 1, page_size: 20, pages: 0 } })
    await dlpAPI.listEvents(emptyEventFilters(), 2, 50)

    expect(client.get).toHaveBeenCalledWith('/admin/prompt-audit/events', {
      params: expect.objectContaining({ page: 2, page_size: 50, source: DLP_EVENT_SOURCE }),
    })
  })

  it('keeps the source scope when other filters are applied', async () => {
    client.get.mockResolvedValue({ data: { items: [], total: 0, page: 1, page_size: 20, pages: 0 } })
    const filters = emptyEventFilters()
    filters.decision = 'critical'
    await dlpAPI.listEvents(filters, 1, 20)

    const params = client.get.mock.calls[0][1].params
    expect(params.source).toBe(DLP_EVENT_SOURCE)
    expect(params.decision).toBe('critical')
  })

  it('limits the delete preview to DLP events', async () => {
    // 这条最关键：DLP 页面点「按条件删除」若不限定来源，
    // 会把 qwen3guard 的事件一起删掉，且不可恢复。
    client.post.mockResolvedValue({ data: { matched_count: 3 } })
    await dlpAPI.previewDelete(rangedFilters())

    expect(client.post).toHaveBeenCalledWith(
      '/admin/prompt-audit/events/delete-preview',
      expect.objectContaining({ scanner_backends: [DLP_SCANNER_BACKEND] }),
    )
  })

  it('limits the confirmed filter delete to DLP events', async () => {
    client.post.mockResolvedValue({ data: { deleted_events: 3, deleted_jobs: 3 } })
    await dlpAPI.deleteEventsByFilter(rangedFilters(), {
      matched_count: 3, filter_summary: {}, snapshot_max_id: 10,
      filter_hash: 'a'.repeat(64), confirmation_token: 'opaque-token',
      expires_at: '2026-07-16T00:05:00Z',
    })

    const [, payload] = client.post.mock.calls[0]
    // filter 必须与预览时一致，否则后端的 filter_hash 校验会拒绝。
    expect(payload.filter).toEqual(expect.objectContaining({ scanner_backends: [DLP_SCANNER_BACKEND] }))
    expect(payload.confirmation_token).toBe('opaque-token')
    expect(payload.confirm).toBe(true)
  })

  it('sends the same filter shape to preview and confirmed delete', async () => {
    // 两次请求的 filter 必须逐字段一致：后端用 filter_hash 校验，
    // 差一个字段就会报「删除确认无效或已过期」。
    const filters = rangedFilters()
    client.post.mockResolvedValue({ data: { matched_count: 1 } })
    await dlpAPI.previewDelete(filters)
    const previewFilter = client.post.mock.calls[0][1]

    client.post.mockResolvedValue({ data: { deleted_events: 1, deleted_jobs: 1 } })
    await dlpAPI.deleteEventsByFilter(filters, {
      matched_count: 1, filter_summary: {}, snapshot_max_id: 5,
      filter_hash: 'b'.repeat(64), confirmation_token: 'token', expires_at: '2026-07-16T00:05:00Z',
    })
    const deleteFilter = client.post.mock.calls[1][1].filter

    expect(deleteFilter).toEqual(previewFilter)
  })

  it('does not scope single and batch deletes by source', async () => {
    // 按 ID 删除本就精确到具体事件，不需要来源限定；
    // 多传字段反而与 upstream 的接口约定不符。
    client.delete.mockResolvedValue({ data: { deleted_events: 1, deleted_jobs: 1 } })
    await dlpAPI.deleteEvent(42)
    expect(client.delete).toHaveBeenCalledWith('/admin/prompt-audit/events/42')

    client.post.mockResolvedValue({ data: { deleted_events: 2, deleted_jobs: 2 } })
    await dlpAPI.batchDeleteEvents([1, 2])
    expect(client.post).toHaveBeenCalledWith('/admin/prompt-audit/events/batch-delete', { ids: [1, 2] })
  })

  it('matches the backend scanner_backend value', () => {
    // 与 backend/internal/securityaudit/prompt_guard_dlp.go 的 DLPScannerBackend
    // 必须字面一致，否则按条件删除会匹配不到任何事件。
    expect(DLP_SCANNER_BACKEND).toBe('dlp-regex+llm')
    expect(DLP_EVENT_SOURCE).toBe('dlp')
  })
})
