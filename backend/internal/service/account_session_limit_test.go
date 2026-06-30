package service

import "testing"

// 私有扩展单测（不属于 upstream sub2api）
// 验证 SupportsSessionLimit 在 upstream 的 OAuth/SetupToken 基础上额外放开 Anthropic API Key。
// @author wangzhong

func TestSupportsSessionLimit(t *testing.T) {
	cases := []struct {
		name     string
		platform string
		typ      string
		want     bool
	}{
		{"anthropic_oauth", PlatformAnthropic, AccountTypeOAuth, true},
		{"anthropic_setup_token", PlatformAnthropic, AccountTypeSetupToken, true},
		{"anthropic_apikey", PlatformAnthropic, AccountTypeAPIKey, true},
		{"anthropic_bedrock", PlatformAnthropic, AccountTypeBedrock, false},
		{"openai_apikey", PlatformOpenAI, AccountTypeAPIKey, false},
		{"openai_oauth", PlatformOpenAI, AccountTypeOAuth, false},
		{"gemini_oauth", PlatformGemini, AccountTypeOAuth, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Account{Platform: tc.platform, Type: tc.typ}
			if got := a.SupportsSessionLimit(); got != tc.want {
				t.Fatalf("SupportsSessionLimit(platform=%s, type=%s) = %v, want %v", tc.platform, tc.typ, got, tc.want)
			}
		})
	}

	// nil 安全
	var nilAcc *Account
	if nilAcc.SupportsSessionLimit() {
		t.Fatal("nil account should not support session limit")
	}
}
