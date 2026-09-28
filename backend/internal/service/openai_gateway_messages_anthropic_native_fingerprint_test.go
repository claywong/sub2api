// 私有扩展（不属于 upstream sub2api）。
// openai_gateway_messages_anthropic_native_fingerprint.go 的单元测试。
package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/tidwall/gjson"
)

func fpNormalizeTestAccount(id int64) *Account {
	return &Account{ID: id, Name: "glm-test", Platform: PlatformZhipu}
}

func TestNormalizeNativeAnthropicRequestBodyJSONUserID(t *testing.T) {
	account := fpNormalizeTestAccount(42)
	body := []byte(`{"model":"glm-4.7","metadata":{"user_id":"{\"device_id\":\"client-aabbcc\",\"account_uuid\":\"11111111-2222-3333-4444-555555555555\",\"session_id\":\"sess-abc\"}"},"system":[{"type":"text","text":"You are Claude Code"}],"messages":[{"role":"user","content":"hi"}]}`)

	out := NormalizeNativeAnthropicRequestBody(account, body, anthropicFingerprintClaudeCode)
	raw := extractJSONString(t, out, "metadata.user_id")

	var j jsonUserID
	if err := json.Unmarshal([]byte(raw), &j); err != nil {
		t.Fatalf("rewritten user_id is not valid JSON: %v", err)
	}
	if j.DeviceID == "client-aabbcc" {
		t.Fatal("device_id was not rewritten")
	}
	if len(j.DeviceID) != 64 {
		t.Fatalf("device_id should be 64 hex chars, got %d", len(j.DeviceID))
	}
	if j.AccountUUID == "11111111-2222-3333-4444-555555555555" {
		t.Fatal("account_uuid was not rewritten")
	}
	if j.SessionID != "sess-abc" {
		t.Fatalf("session_id must be preserved, got %q", j.SessionID)
	}

	// 确定性：同一账号两次归一化结果一致
	out2 := NormalizeNativeAnthropicRequestBody(account, body, anthropicFingerprintClaudeCode)
	if string(extractJSONString(t, out2, "metadata.user_id")) != raw {
		t.Fatal("same account should produce identical canonical identity")
	}

	// 不同账号派生不同身份
	out3 := NormalizeNativeAnthropicRequestBody(fpNormalizeTestAccount(43), body, anthropicFingerprintClaudeCode)
	if extractJSONString(t, out3, "metadata.user_id") == raw {
		t.Fatal("different accounts should derive different identities")
	}
}

func TestNormalizeNativeAnthropicRequestBodyLegacyUserID(t *testing.T) {
	account := fpNormalizeTestAccount(7)
	legacy := "user_0000000000000000000000000000000000000000000000000000000000000000_account_11111111-2222-3333-4444-555555555555_session_99999999-8888-7777-6666-555555555555"
	body, _ := json.Marshal(map[string]any{"metadata": map[string]any{"user_id": legacy}})

	out := NormalizeNativeAnthropicRequestBody(account, body, anthropicFingerprintClaudeCode)
	raw := extractJSONString(t, out, "metadata.user_id")

	wantPrefix := fmt.Sprintf("user_%s_account_%s_session_", anthropicFingerprintCanonicalDeviceID(account), anthropicFingerprintCanonicalAccountUUID(account))
	if raw != wantPrefix+"99999999-8888-7777-6666-555555555555" {
		t.Fatalf("legacy rewrite mismatch: %q", raw)
	}
}

func TestNormalizeNativeAnthropicRequestBodyBillingBlocks(t *testing.T) {
	account := fpNormalizeTestAccount(1)

	t.Run("array with object blocks", func(t *testing.T) {
		body := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.81.a1b; cc_entrypoint=cli;"},{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],"messages":[]}`)
		out := NormalizeNativeAnthropicRequestBody(account, body, anthropicFingerprintClaudeCode)
		sys := extractRaw(t, out, "system")
		if bytes.Contains(sys, []byte("billing-header")) {
			t.Fatalf("billing header block not removed: %s", sys)
		}
		if !bytes.Contains(sys, []byte("You are Claude Code")) {
			t.Fatal("normal system block was removed unexpectedly")
		}
	})

	t.Run("array with string blocks", func(t *testing.T) {
		body := []byte(`{"system":["x-anthropic-billing-header: cc_version=2.1.81.a1b","real prompt"],"messages":[]}`)
		out := NormalizeNativeAnthropicRequestBody(account, body, anthropicFingerprintClaudeCode)
		sys := extractRaw(t, out, "system")
		if bytes.Contains(sys, []byte("billing-header")) {
			t.Fatalf("billing header block not removed: %s", sys)
		}
		if !bytes.Contains(sys, []byte("real prompt")) {
			t.Fatal("normal system block was removed unexpectedly")
		}
	})

	t.Run("inline string system", func(t *testing.T) {
		body := []byte(`{"system":"base prompt\nx-anthropic-billing-header: cc_version=2.1.81.a1b; cc_entrypoint=cli;\ntail","messages":[]}`)
		out := NormalizeNativeAnthropicRequestBody(account, body, anthropicFingerprintClaudeCode)
		sys := extractRaw(t, out, "system")
		if bytes.Contains(sys, []byte("billing-header")) {
			t.Fatalf("inline billing line not removed: %s", sys)
		}
		if !bytes.Contains(sys, []byte("base prompt")) || !bytes.Contains(sys, []byte("tail")) {
			t.Fatal("non-billing lines were removed unexpectedly")
		}
	})

	t.Run("no billing block keeps bytes untouched", func(t *testing.T) {
		body := []byte(`{"system":[{"type":"text","text":"keep me"}],"messages":[]}`)
		out := NormalizeNativeAnthropicRequestBody(account, body, anthropicFingerprintClaudeCode)
		if !bytes.Equal(extractRaw(t, out, "system"), []byte(`[{"type":"text","text":"keep me"}]`)) {
			t.Fatalf("system bytes changed: %s", extractRaw(t, out, "system"))
		}
	})
}

func TestNormalizeNativeAnthropicRequestBodyPassthrough(t *testing.T) {
	account := fpNormalizeTestAccount(9)

	t.Run("nil account", func(t *testing.T) {
		body := []byte(`{"metadata":{"user_id":"{\"device_id\":\"x\",\"session_id\":\"y\"}"}}`)
		if got := NormalizeNativeAnthropicRequestBody(nil, body, anthropicFingerprintClaudeCode); !bytes.Equal(got, body) {
			t.Fatal("nil account must not modify body")
		}
	})

	t.Run("unparseable user_id stays", func(t *testing.T) {
		body := []byte(`{"metadata":{"user_id":"garbage-not-a-format"}}`)
		out := NormalizeNativeAnthropicRequestBody(account, body, anthropicFingerprintClaudeCode)
		if extractJSONString(t, out, "metadata.user_id") != "garbage-not-a-format" {
			t.Fatal("unparseable user_id must stay unchanged")
		}
	})

	t.Run("no metadata at all", func(t *testing.T) {
		body := []byte(`{"model":"glm-4.7","messages":[{"role":"user","content":"hi"}]}`)
		if got := NormalizeNativeAnthropicRequestBody(account, body, anthropicFingerprintClaudeCode); !bytes.Equal(got, body) {
			t.Fatal("body without identity fields must stay unchanged")
		}
	})
}

func TestNormalizeNativeAnthropicRequestHeaders(t *testing.T) {
	account := fpNormalizeTestAccount(3)
	h := http.Header{}
	h.Set("User-Agent", "some-sdk/0.1.2")
	h.Set("x-anthropic-billing-header", "cc_version=2.1.81.a1b")

	NormalizeNativeAnthropicRequestHeaders(account, h, anthropicFingerprintClaudeCode)

	wantUA := anthropicFingerprintNormalizedUserAgent(anthropicFingerprintClaudeCode)
	if got := h.Get("User-Agent"); got != wantUA {
		t.Fatalf("User-Agent = %q, want %q", got, wantUA)
	}
	if h.Get("x-anthropic-billing-header") != "" {
		t.Fatal("x-anthropic-billing-header must be stripped")
	}

	// 账号级显式 UA 覆写优先于归一化默认值（header_overrides 仅对 api_key 账号开放）
	overridden := fpNormalizeTestAccount(4)
	overridden.Type = AccountTypeAPIKey
	overridden.Credentials = map[string]any{
		"header_override_enabled": true,
		"header_overrides":        map[string]any{"user-agent": "claude-cli/9.9.9"},
	}
	h2 := http.Header{}
	h2.Set("User-Agent", "some-sdk/0.1.2")
	h2.Set("x-anthropic-billing-header", "cc_version=2.1.81.a1b")
	// 真实链路顺序：先应用账号级覆写，再做归一化
	overridden.ApplyHeaderOverrides(h2)
	NormalizeNativeAnthropicRequestHeaders(overridden, h2, anthropicFingerprintClaudeCode)
	if got := h2.Get("User-Agent"); got != "claude-cli/9.9.9" {
		t.Fatalf("account-level UA override must win, got %q", got)
	}
	if h2.Get("x-anthropic-billing-header") != "" {
		t.Fatal("x-anthropic-billing-header must still be stripped with UA override")
	}

	// off 模式不改动
	h3 := http.Header{}
	h3.Set("User-Agent", "some-sdk/0.1.2")
	NormalizeNativeAnthropicRequestHeaders(account, h3, anthropicFingerprintOff)
	if got := h3.Get("User-Agent"); got != "some-sdk/0.1.2" {
		t.Fatalf("off mode must not touch UA, got %q", got)
	}

	// codex 模式：UA 归一为 codex-tui 形态（非 claude-cli）
	h4 := http.Header{}
	h4.Set("User-Agent", "some-sdk/0.1.2")
	NormalizeNativeAnthropicRequestHeaders(account, h4, anthropicFingerprintCodex)
	codexUA := anthropicFingerprintNormalizedUserAgent(anthropicFingerprintCodex)
	if got := h4.Get("User-Agent"); got != codexUA {
		t.Fatalf("codex UA = %q, want %q", got, codexUA)
	}

	// nil 安全
	NormalizeNativeAnthropicRequestHeaders(nil, nil, anthropicFingerprintClaudeCode)
	NormalizeNativeAnthropicRequestHeaders(nil, h2, anthropicFingerprintClaudeCode)
}

func TestResolveAnthropicFingerprintTarget(t *testing.T) {
	cases := []struct {
		name     string
		codexOn  bool
		claudeOn bool
		isCodex  bool
		want     anthropicFingerprintNormalizeMode
	}{
		{"both off, codex client", false, false, true, anthropicFingerprintOff},
		{"both off, cc client", false, false, false, anthropicFingerprintOff},
		{"codex on, codex client -> codex", true, false, true, anthropicFingerprintCodex},
		{"codex on, cc client -> off (cc switch off)", true, false, false, anthropicFingerprintOff},
		{"cc on, cc client -> claudecode", false, true, false, anthropicFingerprintClaudeCode},
		{"cc on, codex client -> off (codex switch off)", false, true, true, anthropicFingerprintOff},
		{"both on, codex client -> codex", true, true, true, anthropicFingerprintCodex},
		{"both on, cc client -> claudecode", true, true, false, anthropicFingerprintClaudeCode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveAnthropicFingerprintTarget(tc.codexOn, tc.claudeOn, tc.isCodex); got != tc.want {
				t.Fatalf("resolve(codex=%v, cc=%v, isCodex=%v) = %q, want %q", tc.codexOn, tc.claudeOn, tc.isCodex, got, tc.want)
			}
		})
	}
}

func TestApplyCCFingerprintNormalizeUserAgent(t *testing.T) {
	account := fpNormalizeTestAccount(11)

	// claudecode 目标 → claude-cli UA
	h := http.Header{}
	h.Set("user-agent", "python-requests/2.31")
	applyCCFingerprintNormalizeUserAgent(account, h, anthropicFingerprintClaudeCode)
	if got, want := h.Get("user-agent"), anthropicFingerprintNormalizedUserAgent(anthropicFingerprintClaudeCode); got != want {
		t.Fatalf("claudecode UA = %q, want %q", got, want)
	}

	// codex 目标 → codex-tui UA（与 claudecode 不同）
	h2 := http.Header{}
	h2.Set("user-agent", "python-requests/2.31")
	applyCCFingerprintNormalizeUserAgent(account, h2, anthropicFingerprintCodex)
	codexUA := anthropicFingerprintNormalizedUserAgent(anthropicFingerprintCodex)
	if got := h2.Get("user-agent"); got != codexUA {
		t.Fatalf("codex UA = %q, want %q", got, codexUA)
	}

	// off 目标 → 不改
	h3 := http.Header{}
	h3.Set("user-agent", "python-requests/2.31")
	applyCCFingerprintNormalizeUserAgent(account, h3, anthropicFingerprintOff)
	if got := h3.Get("user-agent"); got != "python-requests/2.31" {
		t.Fatalf("off must not touch UA, got %q", got)
	}

	// 账号级 UA 覆写优先
	overridden := fpNormalizeTestAccount(12)
	overridden.Type = AccountTypeAPIKey
	overridden.Credentials = map[string]any{
		"header_override_enabled": true,
		"header_overrides":        map[string]any{"user-agent": "my-custom/1.0"},
	}
	h4 := http.Header{}
	h4.Set("user-agent", "python-requests/2.31")
	applyCCFingerprintNormalizeUserAgent(overridden, h4, anthropicFingerprintCodex)
	if got := h4.Get("user-agent"); got != "python-requests/2.31" {
		t.Fatalf("override account must skip normalize, got %q", got)
	}

	// nil 安全
	applyCCFingerprintNormalizeUserAgent(nil, h4, anthropicFingerprintCodex)
	applyCCFingerprintNormalizeUserAgent(account, nil, anthropicFingerprintCodex)
}

func TestAnthropicFingerprintNormalizeSwitchesFromExtra(t *testing.T) {
	codexKey := anthropicFingerprintNormalizeCodexExtraKey
	ccKey := anthropicFingerprintNormalizeClaudeCodeExtraKey

	cases := []struct {
		name       string
		extra      map[string]any
		wantCodex  bool
		wantClaude bool
	}{
		{"nil extra", nil, false, false},
		{"missing keys", map[string]any{"x": 1}, false, false},
		{"codex bool true", map[string]any{codexKey: true}, true, false},
		{"claudecode bool true", map[string]any{ccKey: true}, false, true},
		{"both bool true", map[string]any{codexKey: true, ccKey: true}, true, true},
		{"string true accepted", map[string]any{codexKey: "true", ccKey: "TRUE"}, true, true},
		{"bool false", map[string]any{codexKey: false, ccKey: false}, false, false},
		{"string false ignored", map[string]any{codexKey: "false"}, false, false},
		{"non-bool non-string ignored", map[string]any{codexKey: 1}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := &Account{Extra: tc.extra}
			if got := acc.AnthropicFingerprintNormalizeCodexEnabled(); got != tc.wantCodex {
				t.Fatalf("codex enabled = %v, want %v", got, tc.wantCodex)
			}
			if got := acc.AnthropicFingerprintNormalizeClaudeCodeEnabled(); got != tc.wantClaude {
				t.Fatalf("claudecode enabled = %v, want %v", got, tc.wantClaude)
			}
		})
	}

	// nil 账号安全
	var nilAcc *Account
	if nilAcc.AnthropicFingerprintNormalizeCodexEnabled() || nilAcc.AnthropicFingerprintNormalizeClaudeCodeEnabled() {
		t.Fatal("nil account must default to false")
	}
}

func extractJSONString(t *testing.T, body []byte, path string) string {
	t.Helper()
	val := gjson.GetBytes(body, path).String()
	if val == "" {
		t.Fatalf("path %s missing in body: %s", path, body)
	}
	return val
}

func extractRaw(t *testing.T, body []byte, path string) []byte {
	t.Helper()
	raw := gjson.GetBytes(body, path).Raw
	if len(raw) == 0 {
		t.Fatalf("path %s missing in body: %s", path, body)
	}
	return []byte(raw)
}
