import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { SCANNER_CATALOG } from '@/features/prompt-audit/viewModel'
import zhAdmin from '@/i18n/locales/zh/admin/dlp'
import enAdmin from '@/i18n/locales/en/admin/dlp'
import DlpPanel from '../components/DlpPanel.vue'
import type { DlpConfig, DlpConfigResponse } from '../types'
import {
  buildConfigUpdateRequest,
  buildDlpUpdateRequest,
  buildGuardPassthrough,
  createDefaultDlpEndpoint,
  DEFAULT_DLP_CONFIRM_MODEL,
  DLP_SCANNER_CATALOG,
  dlpConfigToDraft,
  dlpDraftFingerprint,
  responseToPageDraft,
} from '../viewModel'

const dlpConfig = (): DlpConfig => ({
  enabled: true,
  scanners: ['dlp_pii'],
  confirm_enabled: true,
  confirm_timeout_ms: 5000,
  cache_enabled: true,
  cache_sensitive_ttl_hours: 6,
  cache_benign_ttl_hours: 24,
  block_on_high_severity: true,
  all_groups: true,
  group_ids: [],
  endpoints: [{
    id: 'dlp-1', name: 'Luna', base_url: 'https://api.example.com',
    model: DEFAULT_DLP_CONFIRM_MODEL, timeout_ms: 5000, enabled: true,
    has_token: true, token_status: 'configured',
  }],
  available_scanners: [],
})

const dlpGroups = () => [
  { id: 3, name: '默认分组', platform: 'openai', status: 'active' as const },
  { id: 9, name: 'AWS直连（订阅）', platform: 'anthropic', status: 'active' as const },
]

// configResponse 模拟 GET /admin/prompt-audit/config：既有 dlp 子树，
// 也有必须原样回传的 qwen3guard 字段。
const configResponse = (dlp?: DlpConfig): DlpConfigResponse => ({
  enabled: true,
  blocking_enabled: true,
  blocking_latest_turn_only: false,
  store_pass_events: false,
  strategy: 'priority',
  worker_count: 4,
  queue_capacity: 100,
  scanners: SCANNER_CATALOG.map((item) => item.id),
  all_groups: true,
  group_ids: [],
  endpoints: [{ id: 'guard-1', name: 'Guard', base_url: 'http://127.0.0.1:8000', enabled: true }],
  config_version: 7,
  updated_at: '2026-07-16T00:00:00Z',
  dlp: dlp ?? dlpConfig(),
})

// 与 prompt-audit 的测试保持一致：mock vue-i18n 让 t() 返回原始 key，
// 断言 key 而非译文——文案调整不会弄坏组件测试。
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

function mountPanel(config: DlpConfig) {
  return mount(DlpPanel, { props: { draft: dlpConfigToDraft(config), groups: dlpGroups() } })
}

describe('DLP view model', () => {
  it('models the three DLP detectors', () => {
    expect(DLP_SCANNER_CATALOG).toHaveLength(3)
    expect(DLP_SCANNER_CATALOG.map((item) => item.id)).toEqual([
      'dlp_credential', 'dlp_pii', 'dlp_sensitive',
    ])
  })

  it('keeps DLP detectors out of the qwen3guard catalog', () => {
    // AllScannerIDs 会被原样发给审计模型，混入 DLP ID 会产出永不命中的分类。
    const guardIds = SCANNER_CATALOG.map((item) => item.id) as string[]
    for (const scanner of DLP_SCANNER_CATALOG) {
      expect(guardIds).not.toContain(scanner.id)
    }
  })

  it('falls back to renderable defaults when the backend omits dlp', () => {
    const draft = dlpConfigToDraft(undefined)
    expect(draft.enabled).toBe(false)
    expect(draft.scanners).toEqual([])
    expect(draft.endpoints).toEqual([])
    expect(draft.confirm_timeout_ms).toBeGreaterThan(0)
  })

  it('tolerates a config payload without the dlp field', () => {
    const legacy = { ...configResponse(), dlp: undefined } as unknown as DlpConfigResponse
    const draft = responseToPageDraft(legacy)
    expect(draft.dlp.enabled).toBe(false)
    expect(draft.dlp.endpoints).toEqual([])
  })

  it('normalizes null collections inside dlp', () => {
    const broken = {
      ...dlpConfig(), scanners: null, endpoints: null, available_scanners: null,
    } as unknown as DlpConfig
    const draft = dlpConfigToDraft(broken)
    expect(draft.scanners).toEqual([])
    expect(draft.endpoints).toEqual([])
    expect(draft.available_scanners).toEqual([])
  })

  it('strips tokens when converting the config into a draft', () => {
    const draft = dlpConfigToDraft(dlpConfig())
    expect(draft.endpoints[0].token).toBe('')
    expect(draft.endpoints[0].clear_token).toBe(false)
    // has_token 仍需保留，界面靠它提示"留空保留原 Key"
    expect(draft.endpoints[0].has_token).toBe(true)
  })

  it('defaults the DLP scope to all groups when the backend omits dlp', () => {
    // 后端未配置过 DLP 时也给 all_groups=true，避免表单一打开就是"不对任何分组生效"。
    expect(dlpConfigToDraft(undefined).all_groups).toBe(true)
    expect(dlpConfigToDraft(undefined).group_ids).toEqual([])
  })

  it('clears DLP group ids when switching back to all groups', () => {
    // 残留的旧选择若不清掉，用户切回"指定分组"时会突然生效。
    const draft = dlpConfigToDraft({ ...dlpConfig(), all_groups: true, group_ids: [3, 9] })
    const built = buildDlpUpdateRequest(draft)
    expect(built.all_groups).toBe(true)
    expect(built.group_ids).toEqual([])
  })

  it('sorts DLP group ids so the backend binary search stays valid', () => {
    const draft = dlpConfigToDraft({ ...dlpConfig(), all_groups: false, group_ids: [9, 3] })
    expect(buildDlpUpdateRequest(draft).group_ids).toEqual([3, 9])
  })

  it('omits blank tokens so the backend keeps the stored key', () => {
    const request = buildConfigUpdateRequest(responseToPageDraft(configResponse()))
    expect(request.dlp.endpoints[0].token).toBeUndefined()
  })

  it('disables confirmation when DLP itself is off', () => {
    // 否则后端会因为"启用确认必须有可用节点"而拒绝保存一个已关闭的功能。
    const config = { ...dlpConfig(), enabled: false, confirm_enabled: true, endpoints: [] }
    const request = buildConfigUpdateRequest(responseToPageDraft(configResponse(config)))
    expect(request.dlp.confirm_enabled).toBe(false)
  })

  it('defaults the confirmation model when left blank', () => {
    const draft = responseToPageDraft(configResponse())
    draft.dlp.endpoints[0].model = '   '
    expect(buildConfigUpdateRequest(draft).dlp.endpoints[0].model).toBe(DEFAULT_DLP_CONFIRM_MODEL)
  })

  it('coerces numeric fields submitted as strings', () => {
    const draft = responseToPageDraft(configResponse())
    draft.dlp.confirm_timeout_ms = '8000' as unknown as number
    draft.dlp.cache_benign_ttl_hours = '12' as unknown as number
    const request = buildConfigUpdateRequest(draft)
    expect(request.dlp.confirm_timeout_ms).toBe(8000)
    expect(request.dlp.cache_benign_ttl_hours).toBe(12)
  })

  it('creates confirmation endpoints with the luna default', () => {
    const endpoint = createDefaultDlpEndpoint(1)
    expect(endpoint.model).toBe(DEFAULT_DLP_CONFIRM_MODEL)
    expect(endpoint.enabled).toBe(true)
    expect(endpoint.token).toBe('')
    expect(endpoint.has_token).toBe(false)
  })
})
// 这组测试守的是拆页面引入的最大风险：DLP 页面与 qwen3guard 共用
// PUT /admin/prompt-audit/config，而该接口除 expected_config_version 外都是
// 非指针字段。少带一个字段，后端就会用零值覆盖 qwen3guard 的配置，
// 且因为 enabled=false 能通过校验，不会有任何报错。
describe('qwen3guard passthrough', () => {
  it('carries every qwen3guard field back untouched', () => {
    const response = configResponse()
    const request = buildConfigUpdateRequest(responseToPageDraft(response))

    expect(request.enabled).toBe(response.enabled)
    expect(request.blocking_enabled).toBe(response.blocking_enabled)
    expect(request.blocking_latest_turn_only).toBe(response.blocking_latest_turn_only)
    expect(request.store_pass_events).toBe(response.store_pass_events)
    expect(request.worker_count).toBe(response.worker_count)
    expect(request.queue_capacity).toBe(response.queue_capacity)
    expect(request.scanners).toEqual(response.scanners)
    expect(request.all_groups).toBe(response.all_groups)
    expect(request.group_ids).toEqual(response.group_ids)
    expect(request.endpoints).toEqual(response.endpoints)
  })

  it('never drops the qwen3guard endpoint pool', () => {
    // 这是最危险的一条：节点池被清空后 qwen3guard 会静默失效，
    // 且 API Key 已加密落库，清掉就找不回来。
    const request = buildConfigUpdateRequest(responseToPageDraft(configResponse()))
    expect(request.endpoints).toHaveLength(1)
    expect(request.endpoints[0].id).toBe('guard-1')
  })

  it('keeps the qwen3guard scope separate from the DLP scope', () => {
    const response = configResponse({ ...dlpConfig(), all_groups: false, group_ids: [9] })
    // qwen3guard 覆盖全部分组，DLP 只覆盖 9。两者不得互相污染。
    const request = buildConfigUpdateRequest(responseToPageDraft(response))

    expect(request.all_groups).toBe(true)
    expect(request.group_ids).toEqual([])
    expect(request.dlp.all_groups).toBe(false)
    expect(request.dlp.group_ids).toEqual([9])
  })

  it('submits the version it loaded so the optimistic lock still applies', () => {
    const request = buildConfigUpdateRequest(responseToPageDraft(configResponse()))
    expect(request.expected_config_version).toBe(7)
  })

  it('fills renderable defaults when guard fields are missing', () => {
    // 接口降级时字段可能缺失。回传 undefined 会被 JSON 丢弃，
    // 从而触发后端的零值覆盖路径，所以必须补成显式值。
    const guard = buildGuardPassthrough(undefined)
    expect(guard.strategy).toBe('priority')
    expect(guard.scanners).toEqual([])
    expect(guard.endpoints).toEqual([])
    expect(guard.enabled).toBe(false)
    for (const value of Object.values(guard)) {
      expect(value).not.toBeUndefined()
    }
  })

  it('ignores qwen3guard changes when deciding whether the page is dirty', () => {
    // guard 字段在本页面不可编辑。若纳入指纹，别处改了 qwen3guard 就会让
    // DLP 页面误显示"有未保存修改"。
    const draft = responseToPageDraft(configResponse())
    const server = responseToPageDraft(configResponse())
    draft.guard.blocking_enabled = !draft.guard.blocking_enabled
    draft.guard.worker_count = 99

    expect(dlpDraftFingerprint(draft)).toBe(dlpDraftFingerprint(server))

    // 但 dlp 子树的改动必须被认出来。
    draft.dlp.block_on_high_severity = !draft.dlp.block_on_high_severity
    expect(dlpDraftFingerprint(draft)).not.toBe(dlpDraftFingerprint(server))
  })
})

describe('DlpPanel', () => {
  it('hides the configuration body while DLP is disabled', () => {
    const wrapper = mountPanel({ ...dlpConfig(), enabled: false })
    expect(wrapper.find('[data-test="dlp-enabled"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="dlp-confirm-enabled"]').exists()).toBe(false)
  })

  it('renders every detector plus disposition and cache controls', () => {
    const wrapper = mountPanel(dlpConfig())
    for (const scanner of DLP_SCANNER_CATALOG) {
      expect(wrapper.text()).toContain(`admin.dlp.detectorLabels.${scanner.id}`)
    }
    expect(wrapper.find('[data-test="dlp-block-high"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="dlp-cache-enabled"]').exists()).toBe(true)
  })

  it('treats an empty detector list as all enabled', () => {
    // 后端语义：scanners 为空表示全启用。界面必须如实反映，否则会让人误以为全关。
    const wrapper = mountPanel({ ...dlpConfig(), scanners: [] })
    const boxes = wrapper.findAll('input[type="checkbox"]')
    const checked = boxes.filter((box) => (box.element as HTMLInputElement).checked)
    expect(checked.length).toBeGreaterThanOrEqual(DLP_SCANNER_CATALOG.length)
  })

  it('warns when confirmation is enabled without an endpoint', () => {
    const wrapper = mountPanel({ ...dlpConfig(), endpoints: [] })
    expect(wrapper.text()).toContain('admin.dlp.endpointRequired')
  })

  it('renders its own group scope selector', () => {
    const wrapper = mountPanel(dlpConfig())
    expect(wrapper.find('[data-test="dlp-scope-all"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="dlp-scope-selected"]').exists()).toBe(true)
    // 独立于提示词审计这件事必须写在界面上，否则没人知道两者不联动。
    expect(wrapper.text()).toContain('admin.dlp.scopeHint')
  })

  it('lists selectable groups only in specified-groups mode', () => {
    const allGroups = mountPanel(dlpConfig())
    expect(allGroups.text()).not.toContain('默认分组')

    const scoped = mountPanel({ ...dlpConfig(), all_groups: false, group_ids: [3] })
    expect(scoped.text()).toContain('默认分组')
    expect(scoped.text()).toContain('AWS直连（订阅）')
  })

  it('warns when specified-groups mode has no group selected', () => {
    // 这种配置会让 DLP 静默不工作，界面必须提示。
    const wrapper = mountPanel({ ...dlpConfig(), all_groups: false, group_ids: [] })
    expect(wrapper.find('[data-test="dlp-scope-empty-warning"]').exists()).toBe(true)
  })

  it('flags group ids that no longer exist', () => {
    const wrapper = mountPanel({ ...dlpConfig(), all_groups: false, group_ids: [3, 404] })
    expect(wrapper.text()).toContain('admin.dlp.missingGroups')
    expect(wrapper.text()).toContain('404')
  })

  it('emits an updated draft when a group is toggled', async () => {
    const wrapper = mountPanel({ ...dlpConfig(), all_groups: false, group_ids: [] })
    const boxes = wrapper.findAll('input[type="checkbox"]')
    const groupBox = boxes.find((box) => !box.attributes('data-test'))
    await groupBox?.setValue(true)
    expect(wrapper.emitted('update:draft')).toBeTruthy()
  })

  it('surfaces the fail-open behaviour to the operator', () => {
    // 降级放行意味着可能漏放，界面必须明确告知而不是静默处理。
    const wrapper = mountPanel(dlpConfig())
    expect(wrapper.text()).toContain('admin.dlp.failOpenNotice')
  })

  it('flags endpoints whose stored key cannot be decrypted', () => {
    const config = dlpConfig()
    config.endpoints[0].token_status = 'invalid'
    const wrapper = mountPanel(config)
    expect(wrapper.text()).toContain('admin.dlp.tokenStatus.invalid')
  })

  it('never renders a stored token value', () => {
    const wrapper = mountPanel(dlpConfig())
    const tokenInput = wrapper.findAll('input[type="password"]')[0]
    expect((tokenInput.element as HTMLInputElement).value).toBe('')
  })

  it('emits an updated draft when toggling the master switch', async () => {
    const wrapper = mountPanel(dlpConfig())
    await wrapper.find('[data-test="dlp-enabled"]').setValue(false)
    const emitted = wrapper.emitted('update:draft')
    expect(emitted).toBeTruthy()
    expect((emitted?.[0]?.[0] as { enabled: boolean }).enabled).toBe(false)
  })

  it('adds a confirmation endpoint on demand', async () => {
    const wrapper = mountPanel({ ...dlpConfig(), endpoints: [] })
    await wrapper.find('[data-test="dlp-add-endpoint"]').trigger('click')
    const next = wrapper.emitted('update:draft')?.[0]?.[0] as { endpoints: unknown[] }
    expect(next.endpoints).toHaveLength(1)
  })
})

describe('DLP i18n coverage', () => {
  // 组件测试断言的是 i18n key，所以还需要单独验证 key 真的存在于两种语言里，
  // 否则漏加文案时界面会显示原始 key 而测试全绿。
  function collectKeys(node: unknown, prefix = ''): string[] {
    if (node === null || typeof node !== 'object') return [prefix]
    return Object.entries(node as Record<string, unknown>).flatMap(([key, value]) =>
      collectKeys(value, prefix ? `${prefix}.${key}` : key),
    )
  }

  const zhKeys = collectKeys(zhAdmin.dlp).sort()
  const enKeys = collectKeys(enAdmin.dlp).sort()

  it('defines the same DLP keys in Chinese and English', () => {
    expect(zhKeys).toEqual(enKeys)
  })

  it('covers every key referenced by the panel', () => {
    const required = [
      'title', 'description', 'enabled', 'detectors', 'detectorsHint',
      'disposition', 'blockOnHigh', 'blockOnHighHint',
      'scope', 'scopeHint', 'allGroups', 'selectedGroups', 'searchGroups', 'noGroups',
      'missingGroups', 'selectedCount', 'scopeEmptyWarning',
      'confirm', 'confirmEnabled', 'confirmEnabledHint', 'confirmTimeout', 'failOpenNotice',
      'cache', 'cacheEnabled', 'cacheEnabledHint', 'cacheSensitiveTtl', 'cacheBenignTtl',
      'endpoints', 'endpointsHint', 'endpointRequired', 'addEndpoint', 'removeEndpoint',
      'endpointEnabled', 'endpointName', 'endpointBaseUrl', 'endpointModel', 'endpointTimeout',
      'endpointToken', 'clearToken', 'tokenKeepPlaceholder', 'tokenEmptyPlaceholder',
    ]
    for (const key of required) {
      expect(zhKeys).toContain(key)
    }
  })

  it('covers every key referenced by the page shell', () => {
    // 页面级文案与面板文案在同一个命名空间下，漏加会让标题栏显示原始 key。
    const required = [
      'configVersion', 'tabs.config', 'tabs.events', 'actions.retry',
      'saveBar.dirty', 'saveBar.synced', 'messages.saved', 'messages.deleted',
      'events.deleteConfirmTitle', 'events.deleteConfirmMessage',
      'errors.loadConfig', 'errors.saveConfig', 'errors.loadGroups', 'errors.loadEvents',
      'errors.loadDetail', 'errors.delete', 'errors.previewDelete',
      'errors.deleteConfirmation', 'errors.conflict',
    ]
    for (const key of required) {
      expect(zhKeys).toContain(key)
    }
  })

  it('labels every detector in both languages', () => {
    for (const scanner of DLP_SCANNER_CATALOG) {
      expect(zhKeys).toContain(`detectorLabels.${scanner.id}`)
      expect(enKeys).toContain(`detectorLabels.${scanner.id}`)
    }
  })

  it('labels every token status the backend can return', () => {
    for (const status of ['configured', 'missing', 'invalid']) {
      expect(zhKeys).toContain(`tokenStatus.${status}`)
      expect(enKeys).toContain(`tokenStatus.${status}`)
    }
  })
})
