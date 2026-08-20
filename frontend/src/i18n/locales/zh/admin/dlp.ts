// dlp.ts
// ============================================================================
// 私有扩展（不属于 upstream sub2api）：数据防泄漏（DLP）页面的中文文案。
//
// 独立成文件而不是塞进 promptAudit.ts：后者是 upstream 文件，
// 在里面加 60 行会让每次 merge upstream 都要处理冲突。
// ============================================================================

export default {
  dlp: {
    title: '数据防泄漏（DLP）',
    description:
      '用本地正则检测用户输入与工具结果里的凭证、证件号、口令等敏感信息；命中后可再用模型二次确认以降低误报。DLP 独立于提示词审计，两者的开关互不影响。',
    configVersion: '配置版本 v{version}',
    tabs: { config: '配置', events: '事件' },
    actions: { retry: '重试' },
    saveBar: { dirty: '有未保存的修改', synced: '已同步' },
    messages: {
      saved: 'DLP 配置已保存',
      deleted: '已删除 {count} 条事件',
    },
    events: {
      deleteConfirmTitle: '删除 DLP 事件',
      deleteConfirmMessage: '确定要删除选中的 {count} 条事件吗？此操作不可撤销。',
    },
    errors: {
      loadConfig: '加载 DLP 配置失败',
      saveConfig: '保存 DLP 配置失败',
      loadGroups: '加载分组列表失败',
      loadEvents: '加载 DLP 事件失败',
      loadDetail: '加载事件详情失败',
      delete: '删除事件失败',
      previewDelete: '生成删除预览失败',
      deleteConfirmation: '删除确认无效或已过期，请重新预览',
      conflict: '配置已被其他管理员修改，请刷新后重试',
    },

    enabled: '启用 DLP 检测',
    detectors: '检测器',
    detectorsHint:
      '未勾选任何检测器时视为全部启用。展开后可逐条调整规则的严重度，或单独关掉误报多的规则。',
    detectorLabels: {
      dlp_credential: '凭证泄露',
      dlp_pii: '个人信息',
      dlp_sensitive: '敏感字段',
    },
    disposition: '命中处置',
    blockOnHigh: '高危命中时拦截请求',
    blockOnHighHint:
      '只有标为「高危」的规则命中才会拦截，中危命中一律只记录事件。哪条规则算高危可在下方逐条调整——默认只有 6 条是高危，AWS Access Key、GitHub Token、私钥块等凭证类默认是中危，也就是不拦。',
    rules: {
      enabledCount: '{enabled}/{total} 条规则已启用',
      severity: { medium: '中危', high: '高危' },
      severityFor: '{rule} 的严重度',
      effectBlock: '会拦截',
      effectAudit: '仅记录',
      effectOff: '已关闭',
      changed: '已改',
      changedHint: '默认为{severity}',
      broad: '宽泛',
      broadHint: '这条规则匹配范围较宽，误报相对多；可单独关掉而不影响同类的精确规则。',
      allDisabledWarning: '该检测器下所有规则都已关闭，等同于关掉整个检测器。',
    },
    scope: '适用范围',
    scopeHint: 'DLP 有自己独立的生效范围，与提示词审计的审计模式、分组设置无关。',
    allGroups: '全部分组',
    selectedGroups: '指定分组',
    searchGroups: '搜索分组',
    noGroups: '没有匹配的分组',
    missingGroups: '配置中存在已删除的分组 ID',
    selectedCount: '已选 {count} 个分组',
    scopeEmptyWarning: '未选择任何分组，DLP 将不会检测任何请求。保存前请至少选择一个分组。',
    confirm: '二次确认',
    confirmEnabled: '正则命中后用模型二次确认',
    confirmEnabledHint:
      '只有正则命中的片段才会送模型，未命中的请求零额外开销。关闭后正则命中即判定，误报会明显增多。',
    confirmTimeout: '确认超时（毫秒）',
    failOpenNotice:
      '确认服务不可用或超时时按放行处理，并记录降级告警——避免第三方模型抖动导致网关整体不可用。',
    cache: '确认结果缓存',
    cacheEnabled: '缓存确认结论',
    cacheEnabledHint: '相同片段在有效期内复用上次判定，不再重复请求模型。仅缓存结论，不存储命中内容。',
    cacheSensitiveTtl: '判为敏感的缓存时长（小时）',
    cacheBenignTtl: '判为误报的缓存时长（小时）',
    endpoints: '确认节点',
    endpointsHint: '与提示词审计的节点池分开配置，互不影响。按顺序使用第一个可用节点。',
    endpointRequired: '已启用二次确认，请至少添加并启用一个确认节点。',
    addEndpoint: '添加节点',
    removeEndpoint: '移除',
    endpointEnabled: '启用',
    endpointName: '名称',
    endpointBaseUrl: '接口地址',
    endpointModel: '模型',
    endpointTimeout: '超时（毫秒）',
    endpointToken: 'API Key',
    clearToken: '清除已保存的 Key',
    tokenKeepPlaceholder: '留空则保留已保存的 Key',
    tokenEmptyPlaceholder: '请输入 API Key',
    tokenStatus: {
      configured: 'Key 已配置',
      missing: '未配置 Key',
      invalid: 'Key 无法解密，该节点已排除在运行时之外',
    },
  },
}
