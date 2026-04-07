import { describe, expect, it } from 'vitest'
import {
  ANTHROPIC_PASSTHROUGH_MODE_AUTH_ONLY,
  ANTHROPIC_PASSTHROUGH_MODE_COMPAT,
  ANTHROPIC_PASSTHROUGH_MODE_FULL,
  isAnthropicPassthroughModeSupported,
  normalizeAnthropicPassthroughMode,
  resolveAnthropicPassthroughModeFromExtra,
  writeAnthropicPassthroughModeToExtra
} from '@/utils/anthropicPassthroughMode'

describe('anthropicPassthroughMode utils', () => {
  it('normalizes supported mode values', () => {
    expect(normalizeAnthropicPassthroughMode('compat')).toBe(ANTHROPIC_PASSTHROUGH_MODE_COMPAT)
    expect(normalizeAnthropicPassthroughMode('AUTH_ONLY')).toBe(ANTHROPIC_PASSTHROUGH_MODE_AUTH_ONLY)
    expect(normalizeAnthropicPassthroughMode(' full ')).toBe(ANTHROPIC_PASSTHROUGH_MODE_FULL)
    expect(normalizeAnthropicPassthroughMode('invalid')).toBeNull()
  })

  it('checks mode support by account type', () => {
    expect(isAnthropicPassthroughModeSupported('oauth', ANTHROPIC_PASSTHROUGH_MODE_COMPAT)).toBe(true)
    expect(isAnthropicPassthroughModeSupported('oauth', ANTHROPIC_PASSTHROUGH_MODE_FULL)).toBe(true)
    expect(isAnthropicPassthroughModeSupported('oauth', ANTHROPIC_PASSTHROUGH_MODE_AUTH_ONLY)).toBe(false)
    expect(isAnthropicPassthroughModeSupported('setup-token', ANTHROPIC_PASSTHROUGH_MODE_FULL)).toBe(true)
    expect(isAnthropicPassthroughModeSupported('apikey', ANTHROPIC_PASSTHROUGH_MODE_AUTH_ONLY)).toBe(true)
  })

  it('resolves new mode first and falls back to legacy api key flag', () => {
    expect(
      resolveAnthropicPassthroughModeFromExtra(
        {
          anthropic_passthrough_mode: 'full',
          anthropic_passthrough: true
        },
        'apikey'
      )
    ).toBe(ANTHROPIC_PASSTHROUGH_MODE_FULL)

    expect(
      resolveAnthropicPassthroughModeFromExtra(
        {
          anthropic_passthrough: true
        },
        'apikey'
      )
    ).toBe(ANTHROPIC_PASSTHROUGH_MODE_AUTH_ONLY)

    expect(
      resolveAnthropicPassthroughModeFromExtra(
        {
          anthropic_passthrough_mode: 'auth_only',
          anthropic_passthrough: true
        },
        'oauth'
      )
    ).toBe(ANTHROPIC_PASSTHROUGH_MODE_COMPAT)
  })

  it('writes supported modes and removes legacy keys', () => {
    expect(
      writeAnthropicPassthroughModeToExtra(
        {
          anthropic_passthrough: true
        },
        'apikey',
        ANTHROPIC_PASSTHROUGH_MODE_FULL
      )
    ).toEqual({
      anthropic_passthrough_mode: ANTHROPIC_PASSTHROUGH_MODE_FULL
    })

    expect(
      writeAnthropicPassthroughModeToExtra(
        {
          anthropic_passthrough_mode: 'full',
          anthropic_passthrough: true
        },
        'oauth',
        ANTHROPIC_PASSTHROUGH_MODE_COMPAT
      )
    ).toEqual({})

    expect(
      writeAnthropicPassthroughModeToExtra(
        {},
        'oauth',
        ANTHROPIC_PASSTHROUGH_MODE_AUTH_ONLY
      )
    ).toEqual({})
  })
})
