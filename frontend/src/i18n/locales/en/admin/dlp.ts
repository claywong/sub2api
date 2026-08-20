// dlp.ts
// ============================================================================
// Private extension (not part of upstream sub2api): English copy for the
// Data Loss Prevention (DLP) page.
//
// Kept in its own file rather than inside promptAudit.ts: that file belongs to
// upstream, and adding 60 lines there would make every upstream merge conflict.
// ============================================================================

export default {
  dlp: {
    title: 'Data Loss Prevention (DLP)',
    description:
      'Detect credentials, ID numbers and passwords in user input and tool results with local regex rules; confirmed hits can be double-checked by a model to cut false positives. DLP is independent of prompt audit — their switches do not affect each other.',
    configVersion: 'Config version v{version}',
    tabs: { config: 'Configuration', events: 'Events' },
    actions: { retry: 'Retry' },
    saveBar: { dirty: 'Unsaved changes', synced: 'Synced' },
    messages: {
      saved: 'DLP configuration saved',
      deleted: 'Deleted {count} event(s)',
    },
    events: {
      deleteConfirmTitle: 'Delete DLP events',
      deleteConfirmMessage: 'Delete the {count} selected event(s)? This cannot be undone.',
    },
    errors: {
      loadConfig: 'Failed to load the DLP configuration',
      saveConfig: 'Failed to save the DLP configuration',
      loadGroups: 'Failed to load the group list',
      loadEvents: 'Failed to load DLP events',
      loadDetail: 'Failed to load the event detail',
      delete: 'Failed to delete events',
      previewDelete: 'Failed to build the delete preview',
      deleteConfirmation: 'The delete confirmation is invalid or expired. Preview again.',
      conflict: 'Another administrator changed the configuration. Refresh and retry.',
    },

    enabled: 'Enable DLP detection',
    disabledNotice:
      'DLP is currently off. You can set everything up here first, but nothing takes effect until "Enable DLP detection" in the bottom bar is switched on.',
    detectors: 'Detectors',
    detectorsHint:
      'Leaving every detector unchecked means all of them are enabled. Expand a detector to adjust each rule\'s severity, or switch off a single noisy rule.',
    detectorLabels: {
      dlp_credential: 'Credential leak',
      dlp_pii: 'Personal information',
      dlp_sensitive: 'Sensitive field',
    },
    disposition: 'Disposition',
    blockOnHigh: 'Block requests on high-severity hits',
    blockOnHighHint:
      'Only rules marked high-severity cause a block; medium-severity hits are always recorded without blocking. Which rules count as high is adjustable per rule below — by default only 6 are high, while credential rules such as AWS Access Key, GitHub Token and private key blocks default to medium, meaning they are not blocked.',
    rules: {
      enabledCount: '{enabled}/{total} rules enabled',
      severity: { medium: 'Medium', high: 'High' },
      severityFor: 'Severity for {rule}',
      effectBlock: 'Blocks',
      effectAudit: 'Records',
      effectOff: 'Off',
      changed: 'Changed',
      changedHint: 'Default is {severity}',
      broad: 'Broad',
      broadHint:
        'This rule matches broadly and produces relatively more false positives. It can be switched off on its own without affecting the precise rules in the same detector.',
      allDisabledWarning:
        'Every rule in this detector is switched off, which is equivalent to disabling the detector itself.',
    },
    scope: 'Scope',
    scopeHint:
      'DLP has its own scope, independent of the prompt audit mode and its group settings.',
    allGroups: 'All groups',
    selectedGroups: 'Selected groups',
    searchGroups: 'Search groups',
    noGroups: 'No matching groups',
    missingGroups: 'Configuration references deleted group IDs',
    selectedCount: '{count} group(s) selected',
    scopeEmptyWarning:
      'No groups selected. DLP will not inspect any request. Select at least one group before saving.',
    confirm: 'Second-pass confirmation',
    confirmEnabled: 'Confirm regex hits with a model',
    confirmEnabledHint:
      'Only matched snippets are sent to the model, so requests without hits cost nothing extra. Disabling this treats every regex hit as a real leak and raises false positives.',
    confirmTimeout: 'Confirmation timeout (ms)',
    failOpenNotice:
      'When the confirmation service is unavailable or times out the request is allowed through and a degradation warning is logged, so a flaky third-party model cannot take the gateway down.',
    cache: 'Confirmation cache',
    cacheEnabled: 'Cache confirmation verdicts',
    cacheEnabledHint:
      'Identical snippets reuse the previous verdict while it is valid. Only the verdict is stored, never the matched content.',
    cacheSensitiveTtl: 'TTL for sensitive verdicts (hours)',
    cacheBenignTtl: 'TTL for false-positive verdicts (hours)',
    endpoints: 'Confirmation endpoints',
    endpointsHint:
      'Configured separately from the prompt audit pool. The first available endpoint is used.',
    endpointRequired:
      'Second-pass confirmation is enabled, so add and enable at least one confirmation endpoint.',
    addEndpoint: 'Add endpoint',
    removeEndpoint: 'Remove',
    endpointEnabled: 'Enabled',
    endpointName: 'Name',
    endpointBaseUrl: 'Base URL',
    endpointModel: 'Model',
    endpointTimeout: 'Timeout (ms)',
    endpointToken: 'API key',
    clearToken: 'Clear the stored key',
    tokenKeepPlaceholder: 'Leave blank to keep the stored key',
    tokenEmptyPlaceholder: 'Enter an API key',
    tokenStatus: {
      configured: 'Key configured',
      missing: 'No key configured',
      invalid: 'Key cannot be decrypted; this endpoint is excluded at runtime',
    },
  },
}
