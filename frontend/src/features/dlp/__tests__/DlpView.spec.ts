import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { SCANNER_CATALOG } from '@/features/prompt-audit/viewModel'
import type { DlpConfig, DlpConfigResponse } from '../types'
import DlpView from '../DlpView.vue'

const mocks = vi.hoisted(() => ({
  getConfig: vi.fn(), updateConfig: vi.fn(), listEvents: vi.fn(), getEvent: vi.fn(),
  deleteEvent: vi.fn(), batchDeleteEvents: vi.fn(), previewDelete: vi.fn(),
  deleteEventsByFilter: vi.fn(), listGroups: vi.fn(),
  showSuccess: vi.fn(), showError: vi.fn(),
}))

vi.mock('../api', () => ({ default: mocks }))
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showSuccess: mocks.showSuccess, showError: mocks.showError }),
}))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      locale: { value: 'en' },
      t: (key: string, params?: Record<string, unknown>) =>
        key.replace(/\{(\w+)\}/g, (_, token) => String(params?.[token] ?? `{${token}}`)),
    }),
  }
})

const dlpConfig = (): DlpConfig => ({
  enabled: true, scanners: ['dlp_pii'], confirm_enabled: true, confirm_timeout_ms: 5000,
  cache_enabled: true, cache_sensitive_ttl_hours: 6, cache_benign_ttl_hours: 24,
  block_on_high_severity: true, record_regex_hits: false, all_groups: true, group_ids: [],
  endpoints: [{
    id: 'dlp-1', name: 'Luna', base_url: 'https://api.example.com', model: 'gpt-5.6-luna',
    timeout_ms: 5000, enabled: true, has_token: true, token_status: 'configured',
  }],
  available_scanners: [],
  rules: [
    {
      id: 'credential-aws-access-key', scanner_id: 'dlp_credential', title: 'AWS Access Key',
      default_severity: 'medium', severity: 'medium', disabled: false, broad: false,
    },
    {
      id: 'pii-idcard', scanner_id: 'dlp_pii', title: '身份证号',
      default_severity: 'high', severity: 'high', disabled: false, broad: false,
    },
  ],
  available_severities: ['medium', 'high'],
  blocking_severities: ['high', 'critical'],
})

// 响应里必须带上 qwen3guard 字段：DLP 页面保存时要原样回传，
// 否则后端会用零值覆盖它们。
const baseConfig = (): DlpConfigResponse => ({
  enabled: true, blocking_enabled: true, blocking_latest_turn_only: false, store_pass_events: false,
  strategy: 'priority', worker_count: 4, queue_capacity: 100,
  scanners: SCANNER_CATALOG.map((item) => item.id), all_groups: true, group_ids: [],
  endpoints: [{ id: 'guard-1', name: 'Guard One', base_url: 'http://127.0.0.1:8000', enabled: true }],
  config_version: 7, updated_at: '2026-07-16T00:00:00Z', dlp: dlpConfig(),
})

const AppLayoutStub = { template: '<div><slot /></div>' }
const DlpPanelStub = defineComponent({
  props: ['draft', 'groups'],
  emits: ['update:draft'],
  template: `<div data-test="dlp-panel">
    <button data-test="toggle-block" @click="$emit('update:draft', { ...draft, block_on_high_severity: !draft.block_on_high_severity })">toggle</button>
    <button data-test="raise-severity" @click="$emit('update:draft', { ...draft, rules: draft.rules.map((r) => r.id === 'credential-aws-access-key' ? { ...r, severity: 'high' } : r) })">raise</button>
  </div>`,
})
const EventsStub = defineComponent({
  props: ['events', 'filters', 'selectedIds', 'loading', 'error', 'total', 'page', 'pageSize'],
  emits: ['filters-change', 'search', 'selection', 'page', 'page-size', 'view', 'delete', 'batch-delete', 'preview-delete'],
  template: `<div data-test="events">{{ error }}
    <button data-test="preview" @click="$emit('preview-delete')">preview</button>
    <button data-test="delete-one" @click="$emit('delete', 5)">delete</button>
    <button data-test="select-batch" @click="$emit('selection', [5, 6])">select</button>
    <button data-test="delete-batch" @click="$emit('batch-delete')">batch</button>
  </div>`,
})
const DetailStub = defineComponent({ props: ['show', 'event', 'loading'], emits: ['close'], template: '<div data-test="detail" />' })
const ConfirmStub = defineComponent({
  props: ['show', 'title', 'message'], emits: ['confirm', 'cancel'],
  template: '<div v-if="show" data-test="confirm"><button data-test="confirm-action" @click="$emit(\'confirm\')">confirm</button></div>',
})
const FilterDeleteStub = defineComponent({
  props: ['show', 'initialFilters', 'preview', 'previewing', 'deleting'],
  emits: ['close', 'preview', 'confirm', 'criteria-change'],
  template: `<div v-if="show" data-test="filter-delete-dialog">
    <button data-test="dialog-confirm" @click="$emit('confirm', { ...initialFilters, start_at: '2026-07-15T00:00', end_at: '2026-07-16T00:00' })">confirm</button>
  </div>`,
})

function mountView() {
  return mount(DlpView, {
    global: {
      stubs: {
        AppLayout: AppLayoutStub, DlpPanel: DlpPanelStub, EventWorkspace: EventsStub,
        EventDetailDialog: DetailStub, FilterDeleteDialog: FilterDeleteStub, ConfirmDialog: ConfirmStub,
      },
    },
  })
}

describe('DlpView', () => {
  beforeEach(() => {
    Object.values(mocks).forEach((mock) => mock.mockReset())
    mocks.getConfig.mockResolvedValue(baseConfig())
    mocks.listGroups.mockResolvedValue([])
    mocks.listEvents.mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20, pages: 0 })
    mocks.updateConfig.mockImplementation(async () => ({ ...baseConfig(), config_version: 8 }))
    mocks.previewDelete.mockResolvedValue({
      matched_count: 2, filter_summary: {}, snapshot_max_id: 10,
      filter_hash: 'a'.repeat(64), confirmation_token: 'opaque-confirmation',
      expires_at: '2026-07-16T00:05:00Z',
    })
    mocks.deleteEventsByFilter.mockResolvedValue({ deleted_events: 2, deleted_jobs: 2 })
    mocks.deleteEvent.mockResolvedValue({ deleted_events: 1, deleted_jobs: 1 })
    mocks.batchDeleteEvents.mockResolvedValue({ deleted_events: 2, deleted_jobs: 2 })
  })

  it('loads config, groups, and events independently', async () => {
    const wrapper = mountView()
    expect(mocks.getConfig).toHaveBeenCalledOnce()
    expect(mocks.listGroups).toHaveBeenCalledOnce()
    expect(mocks.listEvents).toHaveBeenCalledOnce()
    await flushPromises()
    expect(wrapper.find('[data-test="events"]').exists()).toBe(true)
  })

  it('does not render the qwen3guard runtime panel', async () => {
    // DLP 在 Coordinator 里同步执行，没有 worker/队列，
    // 运行态指标对它没有意义。
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="tab-config"]').trigger('click')
    expect(wrapper.find('[data-test="runtime"]').exists()).toBe(false)
    expect(wrapper.html()).not.toContain('RuntimeOverview')
  })

  it('keeps a failed config load from blocking the event list', async () => {
    mocks.getConfig.mockRejectedValue(new Error('config offline'))
    const wrapper = mountView()
    await flushPromises()
    // 配置加载失败时展示重试入口，但事件列表仍然可用。
    expect(wrapper.text()).toContain('config offline')
    expect(mocks.listEvents).toHaveBeenCalledOnce()
  })

  it('separates configuration and events into page tabs', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-test="tab-events"]').attributes('aria-selected')).toBe('true')
    expect(wrapper.get('[data-test="tab-panel-config"]').attributes('style') || '').toContain('display: none')
    expect(wrapper.find('[data-test="save-config"]').exists()).toBe(false)
    expect(wrapper.get('[data-test="tab-events"]').text()).toContain('admin.dlp.tabs.events')

    await wrapper.get('[data-test="tab-config"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-test="tab-panel-config"]').attributes('style') || '').not.toContain('display: none')
    expect(wrapper.find('[data-test="save-config"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="dlp-panel"]').exists()).toBe(true)
  })

  it('does not expose the qwen3guard toggles', async () => {
    // 拆页面的初衷：这些开关属于提示词审计，摆在 DLP 页面会被误当成 DLP 的开关。
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="tab-config"]').trigger('click')

    expect(wrapper.find('[data-test="enabled-toggle"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="blocking-toggle"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="blocking-latest-turn-only-toggle"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="store-pass-toggle"]').exists()).toBe(false)
  })

  it('puts the four DLP switches in the save bar', async () => {
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="tab-config"]').trigger('click')

    for (const id of ['dlp-enabled-toggle', 'dlp-block-high-toggle', 'dlp-confirm-toggle', 'dlp-cache-toggle']) {
      expect(wrapper.get(`[data-test="${id}"]`).attributes('role')).toBe('switch')
    }
  })

  it('greys out the dependent switches when DLP is off', async () => {
    // 关掉 DLP 后另外三个开关都不起作用，可点会让人以为改了有效。
    mocks.getConfig.mockResolvedValue({ ...baseConfig(), dlp: { ...dlpConfig(), enabled: false } })
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="tab-config"]').trigger('click')

    expect(wrapper.get('[data-test="dlp-enabled-toggle"]').attributes()).not.toHaveProperty('disabled')
    for (const id of ['dlp-block-high-toggle', 'dlp-confirm-toggle', 'dlp-cache-toggle']) {
      expect(wrapper.get(`[data-test="${id}"]`).attributes()).toHaveProperty('disabled')
    }
  })

  it('greys out the cache switch when confirmation is off', async () => {
    // 缓存的是二次确认的结论，确认关着就没有结论可缓存。
    mocks.getConfig.mockResolvedValue({ ...baseConfig(), dlp: { ...dlpConfig(), confirm_enabled: false } })
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="tab-config"]').trigger('click')

    expect(wrapper.get('[data-test="dlp-confirm-toggle"]').attributes()).not.toHaveProperty('disabled')
    expect(wrapper.get('[data-test="dlp-cache-toggle"]').attributes()).toHaveProperty('disabled')
  })

  it('saves a switch flipped in the save bar', async () => {
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="tab-config"]').trigger('click')
    await wrapper.get('[data-test="dlp-enabled-toggle"]').trigger('click')

    expect(wrapper.text()).toContain('admin.dlp.saveBar.dirty')
    await wrapper.get('[data-test="save-config"]').trigger('click')
    await flushPromises()

    expect(mocks.updateConfig).toHaveBeenCalledWith(expect.objectContaining({
      // guard 的 enabled 必须保持原样：这里改的是 dlp.enabled。
      enabled: true,
      dlp: expect.objectContaining({ enabled: false }),
    }))
  })

  it('leaves the other DLP fields untouched when one switch is flipped', async () => {
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="tab-config"]').trigger('click')
    await wrapper.get('[data-test="dlp-confirm-toggle"]').trigger('click')
    await wrapper.get('[data-test="save-config"]').trigger('click')
    await flushPromises()

    const payload = mocks.updateConfig.mock.calls[0][0]
    expect(payload.dlp.confirm_enabled).toBe(false)
    expect(payload.dlp.block_on_high_severity).toBe(true)
    expect(payload.dlp.scanners).toEqual(['dlp_pii'])
    expect(payload.dlp.rules).toHaveLength(2)
    expect(payload.dlp.endpoints).toHaveLength(1)
  })

  it('carries the qwen3guard config back untouched when saving', async () => {
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="tab-config"]').trigger('click')
    await wrapper.get('[data-test="toggle-block"]').trigger('click')

    expect(wrapper.text()).toContain('admin.dlp.saveBar.dirty')
    await wrapper.get('[data-test="save-config"]').trigger('click')
    await flushPromises()

    // 少带任何一个 guard 字段，后端都会把它解成零值写库。
    expect(mocks.updateConfig).toHaveBeenCalledWith(expect.objectContaining({
      expected_config_version: 7,
      blocking_enabled: true,
      worker_count: 4,
      queue_capacity: 100,
      endpoints: [expect.objectContaining({ id: 'guard-1' })],
      scanners: SCANNER_CATALOG.map((item) => item.id),
      dlp: expect.objectContaining({ block_on_high_severity: false }),
    }))
    expect(mocks.showSuccess).toHaveBeenCalled()
  })

  it('submits rule severity changes', async () => {
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="tab-config"]').trigger('click')
    await wrapper.get('[data-test="raise-severity"]').trigger('click')

    expect(wrapper.text()).toContain('admin.dlp.saveBar.dirty')
    await wrapper.get('[data-test="save-config"]').trigger('click')
    await flushPromises()

    const payload = mocks.updateConfig.mock.calls[0][0]
    const aws = payload.dlp.rules.find(
      (rule: { id: string }) => rule.id === 'credential-aws-access-key',
    )
    expect(aws.severity).toBe('high')
    expect(aws.enabled).toBe(true)
    // 必须提交全量规则：少提交的会被后端当成「没带 rules」而保持原有覆盖。
    expect(payload.dlp.rules).toHaveLength(2)
  })

  it('resyncs the version after a save so the next save is not rejected', async () => {
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="tab-config"]').trigger('click')
    await wrapper.get('[data-test="toggle-block"]').trigger('click')
    await wrapper.get('[data-test="save-config"]').trigger('click')
    await flushPromises()

    // 保存成功后页面应显示已同步，并采用后端返回的新版本号。
    expect(wrapper.text()).toContain('admin.dlp.saveBar.synced')
    await wrapper.get('[data-test="toggle-block"]').trigger('click')
    await wrapper.get('[data-test="save-config"]').trigger('click')
    await flushPromises()
    expect(mocks.updateConfig).toHaveBeenLastCalledWith(
      expect.objectContaining({ expected_config_version: 8 }),
    )
  })

  it('reports a save failure without silently dropping the edit', async () => {
    mocks.updateConfig.mockRejectedValue(new Error('save failed'))
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="tab-config"]').trigger('click')
    await wrapper.get('[data-test="toggle-block"]').trigger('click')
    await wrapper.get('[data-test="save-config"]').trigger('click')
    await flushPromises()

    expect(mocks.showError).toHaveBeenCalled()
    // 失败后草稿仍是脏的，用户可以重试而不必重新编辑。
    expect(wrapper.text()).toContain('admin.dlp.saveBar.dirty')
  })

  it('discards local edits on reset', async () => {
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="tab-config"]').trigger('click')
    await wrapper.get('[data-test="toggle-block"]').trigger('click')
    expect(wrapper.text()).toContain('admin.dlp.saveBar.dirty')

    await wrapper.findAll('button').find((button) => button.text() === 'common.reset')?.trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('admin.dlp.saveBar.synced')
  })

  it('executes single, batch, and filter deletion flows', async () => {
    const wrapper = mountView()
    await flushPromises()

    await wrapper.get('[data-test="delete-one"]').trigger('click')
    await wrapper.get('[data-test="confirm-action"]').trigger('click')
    await flushPromises()
    expect(mocks.deleteEvent).toHaveBeenCalledWith(5)

    await wrapper.get('[data-test="select-batch"]').trigger('click')
    await wrapper.get('[data-test="delete-batch"]').trigger('click')
    await wrapper.get('[data-test="confirm-action"]').trigger('click')
    await flushPromises()
    expect(mocks.batchDeleteEvents).toHaveBeenCalledWith([5, 6])

    // 一键路径：没有手动预览时即时换一个确认 token 再删。
    await wrapper.get('[data-test="preview"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-test="dialog-confirm"]').trigger('click')
    await flushPromises()
    expect(mocks.previewDelete).toHaveBeenCalledOnce()
    expect(mocks.deleteEventsByFilter).toHaveBeenCalledWith(
      expect.objectContaining({ start_at: '2026-07-15T00:00' }),
      expect.objectContaining({ confirmation_token: 'opaque-confirmation' }),
    )
    expect(wrapper.find('[data-test="filter-delete-dialog"]').exists()).toBe(false)
  })

  it('reloads events after a deletion', async () => {
    const wrapper = mountView()
    await flushPromises()
    expect(mocks.listEvents).toHaveBeenCalledTimes(1)

    await wrapper.get('[data-test="delete-one"]').trigger('click')
    await wrapper.get('[data-test="confirm-action"]').trigger('click')
    await flushPromises()
    expect(mocks.listEvents).toHaveBeenCalledTimes(2)
  })
})
