// api.ts
// ============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 独立页面的接口层。
//
// 复用 upstream 的 /admin/prompt-audit 端点，而不新开一套 /admin/dlp 接口：
//   - DLP 配置本就存在 PromptAuditConfig 的 dlp 子树里，另开接口要把存储也拆开，
//     改动面从「新增一个前端目录」放大到「后端新增 handler + 路由 + 存储迁移」；
//   - 事件也共用 prompt_audit_events 表，只靠 scanner_backend 列区分来源。
//
// 两处 DLP 专属处理：
//   1. 事件查询固定带 source=dlp，让列表只返回 DLP 事件（后端映射见
//      backend/internal/securityaudit/prompt_event_repository_dlp.go）；
//   2. 按筛选删除的 body 里带上 scanner_backends，避免在 DLP 页面点「按条件删除」
//      把 qwen3guard 的事件一起删掉。
// ============================================================================

import { apiClient } from '@/api/client'
import type {
  PromptAuditEvent,
  PromptAuditGroup,
  PromptDeletePreview,
  PromptDeleteResult,
  PromptEventFilters,
  PromptEventPage,
} from '@/features/prompt-audit/types'
import { eventFilterPayload, eventQueryParams } from '@/features/prompt-audit/viewModel'
import type { DlpConfigResponse, DlpConfigUpdateRequest } from './types'

const basePath = '/admin/prompt-audit'

// DLP 事件的来源标识，与后端 EventSourceDLP 对应。
export const DLP_EVENT_SOURCE = 'dlp'

// DLP 事件的 scanner_backend 取值，与后端 DLPScannerBackend 对应。
// 删除链路的 body 直接绑定 EventFilter，需要显式给出 backend 列表。
export const DLP_SCANNER_BACKEND = 'dlp-regex+llm'

// dlpFilterPayload 在 upstream 的筛选载荷上追加来源限定。
//
// 删除接口的 body 直接反序列化成后端 EventFilter，所以这里的字段名必须与
// EventFilter 的 JSON tag 一致（scanner_backends / exclude_scanner_backend）。
function dlpFilterPayload(filters: PromptEventFilters): Record<string, unknown> {
  return {
    ...eventFilterPayload(filters),
    scanner_backends: [DLP_SCANNER_BACKEND],
  }
}

export async function getConfig(): Promise<DlpConfigResponse> {
  const { data } = await apiClient.get<DlpConfigResponse>(`${basePath}/config`)
  return data
}

export async function updateConfig(payload: DlpConfigUpdateRequest): Promise<DlpConfigResponse> {
  const { data } = await apiClient.put<DlpConfigResponse>(`${basePath}/config`, payload)
  return data
}

export async function listEvents(
  filters: PromptEventFilters,
  page: number,
  pageSize: number,
): Promise<PromptEventPage> {
  const { data } = await apiClient.get<PromptEventPage>(`${basePath}/events`, {
    params: { page, page_size: pageSize, source: DLP_EVENT_SOURCE, ...eventQueryParams(filters) },
  })
  return data
}

export async function getEvent(id: number): Promise<PromptAuditEvent> {
  const { data } = await apiClient.get<PromptAuditEvent>(`${basePath}/events/${id}`)
  return data
}

export async function deleteEvent(id: number): Promise<PromptDeleteResult> {
  const { data } = await apiClient.delete<PromptDeleteResult>(`${basePath}/events/${id}`)
  return data
}

export async function batchDeleteEvents(ids: number[]): Promise<PromptDeleteResult> {
  const { data } = await apiClient.post<PromptDeleteResult>(`${basePath}/events/batch-delete`, { ids })
  return data
}

export async function previewDelete(filters: PromptEventFilters): Promise<PromptDeletePreview> {
  const { data } = await apiClient.post<PromptDeletePreview>(
    `${basePath}/events/delete-preview`,
    dlpFilterPayload(filters),
  )
  return data
}

export async function deleteEventsByFilter(
  filters: PromptEventFilters,
  preview: PromptDeletePreview,
): Promise<PromptDeleteResult> {
  const { data } = await apiClient.post<PromptDeleteResult>(`${basePath}/events/delete-by-filter`, {
    filter: dlpFilterPayload(filters),
    snapshot_max_id: preview.snapshot_max_id,
    filter_hash: preview.filter_hash,
    confirmation_token: preview.confirmation_token,
    confirm: true,
  })
  return data
}

export async function listGroups(): Promise<PromptAuditGroup[]> {
  const { data } = await apiClient.get<PromptAuditGroup[]>('/admin/groups/all', {
    params: { include_inactive: true },
  })
  return data
}

export const dlpAPI = {
  getConfig,
  updateConfig,
  listEvents,
  getEvent,
  deleteEvent,
  batchDeleteEvents,
  previewDelete,
  deleteEventsByFilter,
  listGroups,
}

export default dlpAPI
