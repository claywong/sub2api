export const ANTHROPIC_PASSTHROUGH_MODE_COMPAT = 'compat'
export const ANTHROPIC_PASSTHROUGH_MODE_AUTH_ONLY = 'auth_only'
export const ANTHROPIC_PASSTHROUGH_MODE_FULL = 'full'

export type AnthropicPassthroughMode =
  | typeof ANTHROPIC_PASSTHROUGH_MODE_COMPAT
  | typeof ANTHROPIC_PASSTHROUGH_MODE_AUTH_ONLY
  | typeof ANTHROPIC_PASSTHROUGH_MODE_FULL

export type AnthropicPassthroughAccountType = 'oauth' | 'setup-token' | 'apikey'

const ANTHROPIC_PASSTHROUGH_MODES = new Set<AnthropicPassthroughMode>([
  ANTHROPIC_PASSTHROUGH_MODE_COMPAT,
  ANTHROPIC_PASSTHROUGH_MODE_AUTH_ONLY,
  ANTHROPIC_PASSTHROUGH_MODE_FULL
])

const isAnthropicPassthroughAccountType = (
  type: unknown
): type is AnthropicPassthroughAccountType => {
  return type === 'oauth' || type === 'setup-token' || type === 'apikey'
}

export const normalizeAnthropicPassthroughMode = (
  mode: unknown
): AnthropicPassthroughMode | null => {
  if (typeof mode !== 'string') return null
  const normalized = mode.trim().toLowerCase()
  if (ANTHROPIC_PASSTHROUGH_MODES.has(normalized as AnthropicPassthroughMode)) {
    return normalized as AnthropicPassthroughMode
  }
  return null
}

export const isAnthropicPassthroughModeSupported = (
  type: unknown,
  mode: AnthropicPassthroughMode
): boolean => {
  if (!isAnthropicPassthroughAccountType(type)) return false
  if (type === 'apikey') return true
  return mode !== ANTHROPIC_PASSTHROUGH_MODE_AUTH_ONLY
}

export const resolveAnthropicPassthroughModeFromExtra = (
  extra: Record<string, unknown> | null | undefined,
  type: unknown,
  defaultMode: AnthropicPassthroughMode = ANTHROPIC_PASSTHROUGH_MODE_COMPAT
): AnthropicPassthroughMode => {
  if (!isAnthropicPassthroughAccountType(type)) return defaultMode
  if (!extra) return defaultMode

  const mode = normalizeAnthropicPassthroughMode(extra.anthropic_passthrough_mode)
  if (mode && isAnthropicPassthroughModeSupported(type, mode)) {
    return mode
  }

  if (type === 'apikey' && extra.anthropic_passthrough === true) {
    return ANTHROPIC_PASSTHROUGH_MODE_AUTH_ONLY
  }

  return defaultMode
}

export const writeAnthropicPassthroughModeToExtra = (
  extra: Record<string, unknown> | null | undefined,
  type: unknown,
  mode: AnthropicPassthroughMode
): Record<string, unknown> => {
  const next: Record<string, unknown> = { ...(extra || {}) }

  delete next.anthropic_passthrough

  if (
    !isAnthropicPassthroughAccountType(type) ||
    !isAnthropicPassthroughModeSupported(type, mode) ||
    mode === ANTHROPIC_PASSTHROUGH_MODE_COMPAT
  ) {
    delete next.anthropic_passthrough_mode
    return next
  }

  next.anthropic_passthrough_mode = mode
  return next
}
