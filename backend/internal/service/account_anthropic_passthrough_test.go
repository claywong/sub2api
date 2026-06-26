package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccount_ResolveAnthropicPassthroughMode(t *testing.T) {
	tests := []struct {
		name         string
		account      *Account
		expectedMode string
	}{
		{
			name: "apikey full mode from new field",
			account: &Account{
				Platform: PlatformAnthropic,
				Type:     AccountTypeAPIKey,
				Extra: map[string]any{
					"anthropic_passthrough_mode": "full",
				},
			},
			expectedMode: AnthropicPassthroughModeFull,
		},
		{
			name: "apikey auth_only mode from new field",
			account: &Account{
				Platform: PlatformAnthropic,
				Type:     AccountTypeAPIKey,
				Extra: map[string]any{
					"anthropic_passthrough_mode": "auth_only",
				},
			},
			expectedMode: AnthropicPassthroughModeAuthOnly,
		},
		{
			name: "oauth full mode from new field",
			account: &Account{
				Platform: PlatformAnthropic,
				Type:     AccountTypeOAuth,
				Extra: map[string]any{
					"anthropic_passthrough_mode": "full",
				},
			},
			expectedMode: AnthropicPassthroughModeFull,
		},
		{
			name: "setup-token compat mode from new field",
			account: &Account{
				Platform: PlatformAnthropic,
				Type:     AccountTypeSetupToken,
				Extra: map[string]any{
					"anthropic_passthrough_mode": "compat",
				},
			},
			expectedMode: AnthropicPassthroughModeCompat,
		},
		{
			name: "legacy bool on apikey falls back to auth_only",
			account: &Account{
				Platform: PlatformAnthropic,
				Type:     AccountTypeAPIKey,
				Extra: map[string]any{
					"anthropic_passthrough": true,
				},
			},
			expectedMode: AnthropicPassthroughModeAuthOnly,
		},
		{
			name: "legacy bool on oauth is ignored",
			account: &Account{
				Platform: PlatformAnthropic,
				Type:     AccountTypeOAuth,
				Extra: map[string]any{
					"anthropic_passthrough": true,
				},
			},
			expectedMode: AnthropicPassthroughModeCompat,
		},
		{
			name: "invalid mode falls back to compat",
			account: &Account{
				Platform: PlatformAnthropic,
				Type:     AccountTypeAPIKey,
				Extra: map[string]any{
					"anthropic_passthrough_mode": "broken",
				},
			},
			expectedMode: AnthropicPassthroughModeCompat,
		},
		{
			name: "oauth auth_only mode is rejected and falls back to compat",
			account: &Account{
				Platform: PlatformAnthropic,
				Type:     AccountTypeOAuth,
				Extra: map[string]any{
					"anthropic_passthrough_mode": "auth_only",
				},
			},
			expectedMode: AnthropicPassthroughModeCompat,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expectedMode, tt.account.ResolveAnthropicPassthroughMode())
		})
	}
}

func TestAccount_IsAnthropicAPIKeyPassthroughEnabled(t *testing.T) {
	t.Run("new auth_only field enables auth-only passthrough", func(t *testing.T) {
		account := &Account{
			Platform: PlatformAnthropic,
			Type:     AccountTypeAPIKey,
			Extra: map[string]any{
				"anthropic_passthrough_mode": "auth_only",
			},
		}
		require.True(t, account.IsAnthropicAPIKeyPassthroughEnabled())
		require.False(t, account.IsAnthropicFullPassthroughEnabled())
	})

	t.Run("legacy bool remains backward compatible for apikey", func(t *testing.T) {
		account := &Account{
			Platform: PlatformAnthropic,
			Type:     AccountTypeAPIKey,
			Extra: map[string]any{
				"anthropic_passthrough": true,
			},
		}
		require.True(t, account.IsAnthropicAPIKeyPassthroughEnabled())
		require.False(t, account.IsAnthropicFullPassthroughEnabled())
	})

	t.Run("full mode disables auth-only passthrough helper", func(t *testing.T) {
		account := &Account{
			Platform: PlatformAnthropic,
			Type:     AccountTypeAPIKey,
			Extra: map[string]any{
				"anthropic_passthrough_mode": "full",
			},
		}
		require.False(t, account.IsAnthropicAPIKeyPassthroughEnabled())
		require.True(t, account.IsAnthropicFullPassthroughEnabled())
	})

	t.Run("oauth and setup-token can enable full mode", func(t *testing.T) {
		oauth := &Account{
			Platform: PlatformAnthropic,
			Type:     AccountTypeOAuth,
			Extra: map[string]any{
				"anthropic_passthrough_mode": "full",
			},
		}
		require.True(t, oauth.IsAnthropicFullPassthroughEnabled())
		require.False(t, oauth.IsAnthropicAPIKeyPassthroughEnabled())

		setupToken := &Account{
			Platform: PlatformAnthropic,
			Type:     AccountTypeSetupToken,
			Extra: map[string]any{
				"anthropic_passthrough_mode": "full",
			},
		}
		require.True(t, setupToken.IsAnthropicFullPassthroughEnabled())
		require.False(t, setupToken.IsAnthropicAPIKeyPassthroughEnabled())
	})
}
