import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { SCANNER_CATALOG } from '@/features/prompt-audit/viewModel'
import zhAdmin from '@/i18n/locales/zh/admin/dlp'
import enAdmin from '@/i18n/locales/en/admin/dlp'
import DlpPanel from '../components/DlpPanel.vue'
import type { DlpConfig, DlpConfigResponse, DlpRule } from '../types'
import {
  buildConfigUpdateRequest,
  buildDlpUpdateRequest,
  buildGuardPassthrough,
  countEnabledRules,
  createDefaultDlpEndpoint,
  DEFAULT_DLP_CONFIRM_MODEL,
  DLP_SCANNER_CATALOG,
  dlpConfigToDraft,
  dlpDraftFingerprint,
  responseToPageDraft,
  ruleBlocks,
  ruleChangedFromDefault,
  rulesByScanner,
} from '../viewModel'

// 规则样本取自后端 dlpRules 的真实条目与默认严重度。
// AWS Access Key 默认 medium 是关键样本：它是「开了拦截开关却不拦凭证泄露」
// 这个反直觉行为的来源，也是严重度可配的动因。
const AWS_RULE = 'credential-aws-access-key'
const GENERIC_KEY_RULE = 'credential-generic-api-key'
const IDCARD_RULE = 'pii-idcard'

const dlpRules = (): DlpRule[] => [
  {
    id: AWS_RULE, scanner_id: 'dlp_credential', title: 'AWS Access Key',
    default_severity: 'medium', severity: 'medium', disabled: false, broad: false,
  },
  {
    id: GENERIC_KEY_RULE, scanner_id: 'dlp_credential', title: '通用 API Key',
    default_severity: 'medium', severity: 'medium', disabled: false, broad: true,
  },
  {
    id: IDCARD_RULE, scanner_id: 'dlp_pii', title: '身份证号',
    default_severity: 'high', severity: 'high', disabled: false, broad: false,
  },
]

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
  rules: dlpRules(),
  available_severities: ['medium', 'high'],
  blocking_severities: ['high', 'critical'],
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

// 严重度可配是为了解决一个反直觉行为：AWS Access Key 等凭证类规则内置是中危，
// 管理员开了「高危命中时拦截」也不会拦。这组测试守住可配后的语义。
describe('rule severity and toggles', () => {
  it('marks a rule as blocking only when severity is in the blocking set', () => {
    const draft = dlpConfigToDraft(dlpConfig())
    const aws = draft.rules.find((rule) => rule.id === AWS_RULE)!
    const idCard = draft.rules.find((rule) => rule.id === IDCARD_RULE)!

    // 默认 medium：开了拦截开关也不拦，这正是问题所在。
    expect(ruleBlocks(draft, aws)).toBe(false)
    expect(ruleBlocks(draft, idCard)).toBe(true)

    // 提到 high 后才拦。
    expect(ruleBlocks(draft, { ...aws, severity: 'high' })).toBe(true)
  })

  it('never blocks when the master switch is off', () => {
    const draft = dlpConfigToDraft({ ...dlpConfig(), block_on_high_severity: false })
    for (const rule of draft.rules) {
      expect(ruleBlocks(draft, { ...rule, severity: 'high' })).toBe(false)
    }
  })

  it('never blocks a disabled rule', () => {
    // 关掉的规则根本不参与扫描，不可能拦截。
    const draft = dlpConfigToDraft(dlpConfig())
    const idCard = draft.rules.find((rule) => rule.id === IDCARD_RULE)!
    expect(ruleBlocks(draft, { ...idCard, disabled: true })).toBe(false)
  })

  it('reads the blocking threshold from the backend instead of hardcoding high', () => {
    // 后端调整阈值时界面要自动跟上，不能把 'high' 写死在前端。
    const draft = dlpConfigToDraft({ ...dlpConfig(), blocking_severities: ['medium', 'high'] })
    const aws = draft.rules.find((rule) => rule.id === AWS_RULE)!
    expect(ruleBlocks(draft, aws)).toBe(true)
  })

  it('flags rules that deviate from their built-in default', () => {
    const draft = dlpConfigToDraft(dlpConfig())
    const aws = draft.rules.find((rule) => rule.id === AWS_RULE)!

    expect(ruleChangedFromDefault(aws)).toBe(false)
    expect(ruleChangedFromDefault({ ...aws, severity: 'high' })).toBe(true)
    // 关掉也算改动过。
    expect(ruleChangedFromDefault({ ...aws, disabled: true })).toBe(true)
  })

  it('groups and counts rules per detector', () => {
    const draft = dlpConfigToDraft(dlpConfig())
    expect(rulesByScanner(draft.rules, 'dlp_credential')).toHaveLength(2)
    expect(rulesByScanner(draft.rules, 'dlp_pii')).toHaveLength(1)
    expect(countEnabledRules(draft.rules, 'dlp_credential')).toBe(2)

    const withDisabled = draft.rules.map((rule) =>
      rule.id === AWS_RULE ? { ...rule, disabled: true } : rule,
    )
    expect(countEnabledRules(withDisabled, 'dlp_credential')).toBe(1)
  })

  it('submits the full rule list with enabled flags', () => {
    // 后端只留与默认值的偏差，但前端必须提交全量——少提交的规则会被
    // 当成「没带 rules 字段」而保持原有覆盖，管理员的取消操作就丢了。
    const draft = responseToPageDraft(configResponse())
    draft.dlp.rules = draft.dlp.rules.map((rule) =>
      rule.id === AWS_RULE ? { ...rule, severity: 'high' } : rule,
    )
    const request = buildConfigUpdateRequest(draft)

    expect(request.dlp.rules).toHaveLength(3)
    const aws = request.dlp.rules.find((rule) => rule.id === AWS_RULE)!
    expect(aws.severity).toBe('high')
    expect(aws.enabled).toBe(true)
  })

  it('converts disabled into enabled=false for the backend', () => {
    const draft = responseToPageDraft(configResponse())
    draft.dlp.rules = draft.dlp.rules.map((rule) =>
      rule.id === GENERIC_KEY_RULE ? { ...rule, disabled: true } : rule,
    )
    const request = buildConfigUpdateRequest(draft)
    const generic = request.dlp.rules.find((rule) => rule.id === GENERIC_KEY_RULE)!
    expect(generic.enabled).toBe(false)
  })

  it('counts a rule change as a dirty draft', () => {
    // 否则改了严重度但保存按钮不亮，改动会被静默丢弃。
    const draft = responseToPageDraft(configResponse())
    const server = responseToPageDraft(configResponse())
    draft.dlp.rules = draft.dlp.rules.map((rule) =>
      rule.id === AWS_RULE ? { ...rule, severity: 'high' } : rule,
    )
    expect(dlpDraftFingerprint(draft)).not.toBe(dlpDraftFingerprint(server))
  })

  it('falls back to renderable defaults when the backend omits rule metadata', () => {
    const draft = dlpConfigToDraft(undefined)
    expect(draft.rules).toEqual([])
    // 没有可选严重度就渲染不出选择器，没有阈值就算不出「会拦 / 仅记录」。
    expect(draft.available_severities.length).toBeGreaterThan(0)
    expect(draft.blocking_severities.length).toBeGreaterThan(0)
  })
})

describe('DlpPanel', () => {
  it('stays editable while DLP is disabled, with a notice explaining why', () => {
    // 关掉时藏起整个面板的话，管理员就没法在启用前先配好节点和规则。
    const wrapper = mountPanel({ ...dlpConfig(), enabled: false })
    expect(wrapper.find('[data-test="dlp-disabled-notice"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="dlp-scanner-dlp_pii"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="dlp-add-endpoint"]').exists()).toBe(true)
  })

  it('does not show the disabled notice while DLP is on', () => {
    const wrapper = mountPanel(dlpConfig())
    expect(wrapper.find('[data-test="dlp-disabled-notice"]').exists()).toBe(false)
  })

  it('renders every detector plus the disposition explanation', () => {
    const wrapper = mountPanel(dlpConfig())
    for (const scanner of DLP_SCANNER_CATALOG) {
      expect(wrapper.text()).toContain(`admin.dlp.detectorLabels.${scanner.id}`)
    }
    // 开关本身在保存栏，面板只负责解释「哪条算高危」这件事。
    expect(wrapper.find('[data-test="dlp-block-high-hint"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('admin.dlp.cacheSensitiveTtl')
  })

  it('leaves the four top-level switches to the save bar', () => {
    // 与 qwen3guard 页面对齐：顶层开关只在底部保存栏出现一次，
    // 面板里再放一份会出现两个真值来源。
    const wrapper = mountPanel(dlpConfig())
    for (const id of ['dlp-enabled', 'dlp-block-high', 'dlp-confirm-enabled', 'dlp-cache-enabled']) {
      expect(wrapper.find(`[data-test="${id}"]`).exists()).toBe(false)
    }
  })

  it('hides the cache TTLs when confirmation is off', () => {
    // 缓存存的是二次确认的结论，确认关掉后这两个时长没有意义。
    const wrapper = mountPanel({ ...dlpConfig(), confirm_enabled: false })
    expect(wrapper.text()).not.toContain('admin.dlp.cacheSensitiveTtl')
  })

  it('lists each rule with its severity selector', () => {
    const wrapper = mountPanel({ ...dlpConfig(), scanners: [] })
    for (const rule of dlpRules()) {
      expect(wrapper.find(`[data-test="dlp-rule-${rule.id}"]`).exists()).toBe(true)
      expect(wrapper.find(`[data-test="dlp-rule-severity-${rule.id}"]`).exists()).toBe(true)
    }
    // 规则标题来自后端，不经 i18n——前端硬编码一份必然漂移。
    expect(wrapper.text()).toContain('AWS Access Key')
    expect(wrapper.text()).toContain('身份证号')
  })

  it('shows the real disposition per rule rather than making the admin infer it', () => {
    const wrapper = mountPanel({ ...dlpConfig(), scanners: [] })
    // AWS Access Key 默认中危：开了拦截开关也只记录。
    expect(wrapper.get(`[data-test="dlp-rule-effect-${AWS_RULE}"]`).text())
      .toContain('admin.dlp.rules.effectAudit')
    // 身份证号是高危：会拦。
    expect(wrapper.get(`[data-test="dlp-rule-effect-${IDCARD_RULE}"]`).text())
      .toContain('admin.dlp.rules.effectBlock')
  })

  it('shows every rule as records-only when the master switch is off', () => {
    const wrapper = mountPanel({ ...dlpConfig(), scanners: [], block_on_high_severity: false })
    expect(wrapper.get(`[data-test="dlp-rule-effect-${IDCARD_RULE}"]`).text())
      .toContain('admin.dlp.rules.effectAudit')
  })

  it('emits an updated severity when the selector changes', async () => {
    const wrapper = mountPanel({ ...dlpConfig(), scanners: [] })
    await wrapper.get(`[data-test="dlp-rule-severity-${AWS_RULE}"]`).setValue('high')

    const next = wrapper.emitted('update:draft')?.[0]?.[0] as { rules: DlpRule[] }
    const aws = next.rules.find((rule) => rule.id === AWS_RULE)!
    expect(aws.severity).toBe('high')
    // 只动目标规则，其余保持原样。
    expect(next.rules.find((rule) => rule.id === IDCARD_RULE)!.severity).toBe('high')
    expect(next.rules.find((rule) => rule.id === GENERIC_KEY_RULE)!.severity).toBe('medium')
  })

  it('emits a disabled flag when a rule is switched off', async () => {
    const wrapper = mountPanel({ ...dlpConfig(), scanners: [] })
    await wrapper.get(`[data-test="dlp-rule-enabled-${GENERIC_KEY_RULE}"]`).setValue(false)

    const next = wrapper.emitted('update:draft')?.[0]?.[0] as { rules: DlpRule[] }
    expect(next.rules.find((rule) => rule.id === GENERIC_KEY_RULE)!.disabled).toBe(true)
    expect(next.rules.find((rule) => rule.id === AWS_RULE)!.disabled).toBe(false)
  })

  it('disables the severity selector for a switched-off rule', () => {
    const config = dlpConfig()
    config.scanners = []
    config.rules = config.rules.map((rule) =>
      rule.id === AWS_RULE ? { ...rule, disabled: true } : rule,
    )
    const wrapper = mountPanel(config)
    const selector = wrapper.get(`[data-test="dlp-rule-severity-${AWS_RULE}"]`)
    expect(selector.attributes()).toHaveProperty('disabled')
    expect(wrapper.get(`[data-test="dlp-rule-effect-${AWS_RULE}"]`).text())
      .toContain('admin.dlp.rules.effectOff')
  })

  it('marks rules that were changed from their default', () => {
    const config = dlpConfig()
    config.scanners = []
    config.rules = config.rules.map((rule) =>
      rule.id === AWS_RULE ? { ...rule, severity: 'high' } : rule,
    )
    const wrapper = mountPanel(config)
    expect(wrapper.find(`[data-test="dlp-rule-changed-${AWS_RULE}"]`).exists()).toBe(true)
    expect(wrapper.find(`[data-test="dlp-rule-changed-${IDCARD_RULE}"]`).exists()).toBe(false)
  })

  it('warns when every rule in a detector is switched off', () => {
    // 逐条关光等于关掉整个检测器，但勾选框还是选中的，不提示就看不出来。
    const config = dlpConfig()
    config.scanners = []
    config.rules = config.rules.map((rule) =>
      rule.scanner_id === 'dlp_credential' ? { ...rule, disabled: true } : rule,
    )
    const wrapper = mountPanel(config)
    expect(wrapper.find('[data-test="dlp-scanner-all-disabled-dlp_credential"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="dlp-scanner-all-disabled-dlp_pii"]').exists()).toBe(false)
  })

  it('does not warn about empty detectors when the backend sent no rules', () => {
    // 接口降级时规则表为空，不该显示「全部已关闭」这种误导性提示。
    const wrapper = mountPanel({ ...dlpConfig(), scanners: [], rules: [] })
    expect(wrapper.find('[data-test="dlp-scanner-all-disabled-dlp_credential"]').exists()).toBe(false)
  })

  it('flags broad rules so the admin knows which ones are noisy', () => {
    const wrapper = mountPanel({ ...dlpConfig(), scanners: [] })
    expect(wrapper.text()).toContain('admin.dlp.rules.broad')
  })

  it('hides rule details for a detector that is switched off entirely', () => {
    // 检测器整体关掉时规则明细无意义，展示出来会让人以为还在生效。
    const wrapper = mountPanel({ ...dlpConfig(), scanners: ['dlp_pii'] })
    expect(wrapper.find(`[data-test="dlp-rule-${IDCARD_RULE}"]`).exists()).toBe(true)
    expect(wrapper.find(`[data-test="dlp-rule-${AWS_RULE}"]`).exists()).toBe(false)
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
    // 范围说明必须在场：两个单选项本身看不出选的是「对谁生效」。
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
      'title', 'description', 'enabled', 'disabledNotice', 'detectors', 'detectorsHint',
      'disposition', 'blockOnHigh', 'blockOnHighHint',
      'scope', 'scopeHint', 'allGroups', 'selectedGroups', 'searchGroups', 'noGroups',
      'missingGroups', 'selectedCount', 'scopeEmptyWarning',
      'confirm', 'confirmEnabled', 'confirmEnabledHint', 'confirmTimeout', 'failOpenNotice',
      'cache', 'cacheEnabled', 'cacheEnabledHint', 'cacheSensitiveTtl', 'cacheBenignTtl',
      'endpoints', 'endpointsHint', 'endpointRequired', 'addEndpoint', 'removeEndpoint',
      'endpointEnabled', 'endpointName', 'endpointBaseUrl', 'endpointModel', 'endpointTimeout',
      'endpointToken', 'clearToken', 'tokenKeepPlaceholder', 'tokenEmptyPlaceholder',
      'rules.enabledCount', 'rules.severityFor', 'rules.effectBlock', 'rules.effectAudit',
      'rules.effectOff', 'rules.changed', 'rules.changedHint', 'rules.broad', 'rules.broadHint',
    ]
    for (const key of required) {
      expect(zhKeys).toContain(key)
    }
  })

  it('labels every configurable severity in both languages', () => {
    // 漏一个会让选择器显示原始值（medium/high），管理员看不懂。
    for (const level of ['medium', 'high']) {
      expect(zhKeys).toContain(`rules.severity.${level}`)
      expect(enKeys).toContain(`rules.severity.${level}`)
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
