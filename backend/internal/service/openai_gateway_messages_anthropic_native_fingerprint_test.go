// 私有扩展（不属于 upstream sub2api）。
// openai_gateway_messages_anthropic_native_fingerprint.go 的单元测试。
package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
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

	NormalizeNativeAnthropicRequestHeaders(account, h, anthropicFingerprintClaudeCode, "")

	wantUA := anthropicFingerprintNormalizedUserAgent(anthropicFingerprintClaudeCode, "")
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
	NormalizeNativeAnthropicRequestHeaders(overridden, h2, anthropicFingerprintClaudeCode, "")
	if got := h2.Get("User-Agent"); got != "claude-cli/9.9.9" {
		t.Fatalf("account-level UA override must win, got %q", got)
	}
	if h2.Get("x-anthropic-billing-header") != "" {
		t.Fatal("x-anthropic-billing-header must still be stripped with UA override")
	}

	// off 模式不改动
	h3 := http.Header{}
	h3.Set("User-Agent", "some-sdk/0.1.2")
	NormalizeNativeAnthropicRequestHeaders(account, h3, anthropicFingerprintOff, "")
	if got := h3.Get("User-Agent"); got != "some-sdk/0.1.2" {
		t.Fatalf("off mode must not touch UA, got %q", got)
	}

	// codex 模式：UA 归一为 codex-tui 形态（非 claude-cli）
	h4 := http.Header{}
	h4.Set("User-Agent", "some-sdk/0.1.2")
	NormalizeNativeAnthropicRequestHeaders(account, h4, anthropicFingerprintCodex, "")
	codexUA := anthropicFingerprintNormalizedUserAgent(anthropicFingerprintCodex, "")
	if got := h4.Get("User-Agent"); got != codexUA {
		t.Fatalf("codex UA = %q, want %q", got, codexUA)
	}

	// nil 安全
	NormalizeNativeAnthropicRequestHeaders(nil, nil, anthropicFingerprintClaudeCode, "")
	NormalizeNativeAnthropicRequestHeaders(nil, h2, anthropicFingerprintClaudeCode, "")
}

func TestResolveAnthropicFingerprintTarget(t *testing.T) {
	all := anthropicFingerprintSwitches{codex: true, claudeCode: true, zcode: true}
	cases := []struct {
		name   string
		sw     anthropicFingerprintSwitches
		client anthropicFingerprintClient
		want   anthropicFingerprintNormalizeMode
	}{
		{"全关", anthropicFingerprintSwitches{}, anthropicFingerprintClientClaudeCode, anthropicFingerprintOff},
		{"codex 开 + codex 客户端", anthropicFingerprintSwitches{codex: true}, anthropicFingerprintClientCodex, anthropicFingerprintCodex},
		{"codex 开 + claude 客户端 → off", anthropicFingerprintSwitches{codex: true}, anthropicFingerprintClientClaudeCode, anthropicFingerprintOff},
		{"claudecode 开 + claude 客户端", anthropicFingerprintSwitches{claudeCode: true}, anthropicFingerprintClientClaudeCode, anthropicFingerprintClaudeCode},
		{"claudecode 开 + zcode 客户端 → off", anthropicFingerprintSwitches{claudeCode: true}, anthropicFingerprintClientZCode, anthropicFingerprintOff},
		{"zcode 开 + zcode 客户端", anthropicFingerprintSwitches{zcode: true}, anthropicFingerprintClientZCode, anthropicFingerprintZCode},
		{"全开 + codex", all, anthropicFingerprintClientCodex, anthropicFingerprintCodex},
		{"全开 + claude", all, anthropicFingerprintClientClaudeCode, anthropicFingerprintClaudeCode},
		{"全开 + other 客户端 → off（不归一）", all, anthropicFingerprintClientOther, anthropicFingerprintOff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveAnthropicFingerprintTarget(tc.sw, tc.client); got != tc.want {
				t.Fatalf("resolve = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyAnthropicFingerprintClient(t *testing.T) {
	cases := []struct {
		ua, originator string
		want           anthropicFingerprintClient
	}{
		{"claude-cli/2.1.200 (external, cli)", "", anthropicFingerprintClientClaudeCode},
		{"claude-cli/2.1.200 (external, claude-vscode, agent-sdk/0.2.1)", "", anthropicFingerprintClientClaudeCode},
		{"claude-cli/2.1.200 (external, sdk-ts, agent-sdk/0.2.1)", "", anthropicFingerprintClientClaudeCode},
		{"ZCode/3.14.1 ai-sdk/provider-utils/4.0.27 runtime/node.js/24", "", anthropicFingerprintClientZCode},
		{"codex_cli_rs/0.125.0 (Mac OS X arm64) dumb (codex_cli_rs; 0.125.0)", "", anthropicFingerprintClientCodex},
		{"codex-tui/0.125.0 (Mac OS X arm64) ghostty/1.0 (codex-tui; 0.125.0)", "", anthropicFingerprintClientCodex},
		{"some-sdk/1.0", "codex_cli_rs", anthropicFingerprintClientCodex},
		{"litellm/1.70.0", "", anthropicFingerprintClientOther},
		{"curl/8.0", "", anthropicFingerprintClientOther},
		{"", "", anthropicFingerprintClientOther},
	}
	for _, tc := range cases {
		if got := classifyAnthropicFingerprintClient(tc.ua, tc.originator); got != tc.want {
			t.Fatalf("classify(%q, %q) = %d, want %d", tc.ua, tc.originator, got, tc.want)
		}
	}
}

func TestAnthropicFingerprintNormalizedUserAgentRewrite(t *testing.T) {
	v := claude.EffectiveCLIVersion()

	// claudecode：只替换版本段，入口段保留
	for _, tc := range []struct{ in, want string }{
		{"claude-cli/2.1.100 (external, cli)", "claude-cli/" + v + " (external, cli)"},
		{"claude-cli/2.1.100 (external, claude-vscode, agent-sdk/0.2.1)", "claude-cli/" + v + " (external, claude-vscode, agent-sdk/0.2.1)"},
		{"claude-cli/2.1.100-beta.1 (external, sdk-ts, agent-sdk/0.2.1)", "claude-cli/" + v + " (external, sdk-ts, agent-sdk/0.2.1)"},
		{"litellm/1.70.0", claude.DefaultUserAgent()},
		{"", claude.DefaultUserAgent()},
	} {
		if got := anthropicFingerprintNormalizedUserAgent(anthropicFingerprintClaudeCode, tc.in); got != tc.want {
			t.Fatalf("claudecode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// zcode：替换 ZCode 版本与 node 运行时，ai-sdk 段保留；非 ZCode 入站返回空
	for _, tc := range []struct{ in, want string }{
		{"ZCode/3.11.2 ai-sdk/provider-utils/4.0.27 runtime/node.js/22", "ZCode/" + zcodeCanonicalVersion + " ai-sdk/provider-utils/4.0.27 runtime/node.js/" + zcodeCanonicalNodeRuntime},
		{"ZCode/3.14.1 ai/6.0.193 ai-sdk/provider-utils/4.0.27 runtime/node.js/24", "ZCode/" + zcodeCanonicalVersion + " ai/6.0.193 ai-sdk/provider-utils/4.0.27 runtime/node.js/" + zcodeCanonicalNodeRuntime},
		{"litellm/1.70.0", ""},
	} {
		if got := anthropicFingerprintNormalizedUserAgent(anthropicFingerprintZCode, tc.in); got != tc.want {
			t.Fatalf("zcode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// codex：与 OAuth 同源的规范身份
	if got, want := anthropicFingerprintNormalizedUserAgent(anthropicFingerprintCodex, "litellm/1.0"), resolveCodexOutboundIdentity("").userAgent; got != want {
		t.Fatalf("codex UA = %q, want %q", got, want)
	}
}

func TestApplyFingerprintNormalizeUserAgent(t *testing.T) {
	account := fpNormalizeTestAccount(11)

	// claudecode 目标 → claude-cli UA
	h := http.Header{}
	h.Set("user-agent", "python-requests/2.31")
	applyFingerprintNormalizeUserAgent(account, h, anthropicFingerprintClaudeCode, "")
	if got, want := h.Get("user-agent"), anthropicFingerprintNormalizedUserAgent(anthropicFingerprintClaudeCode, ""); got != want {
		t.Fatalf("claudecode UA = %q, want %q", got, want)
	}

	// codex 目标 → codex-tui UA（与 claudecode 不同）
	h2 := http.Header{}
	h2.Set("user-agent", "python-requests/2.31")
	applyFingerprintNormalizeUserAgent(account, h2, anthropicFingerprintCodex, "")
	codexUA := anthropicFingerprintNormalizedUserAgent(anthropicFingerprintCodex, "")
	if got := h2.Get("user-agent"); got != codexUA {
		t.Fatalf("codex UA = %q, want %q", got, codexUA)
	}

	// off 目标 → 不改
	h3 := http.Header{}
	h3.Set("user-agent", "python-requests/2.31")
	applyFingerprintNormalizeUserAgent(account, h3, anthropicFingerprintOff, "")
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
	applyFingerprintNormalizeUserAgent(overridden, h4, anthropicFingerprintCodex, "")
	if got := h4.Get("user-agent"); got != "python-requests/2.31" {
		t.Fatalf("override account must skip normalize, got %q", got)
	}

	// nil 安全
	applyFingerprintNormalizeUserAgent(nil, h4, anthropicFingerprintCodex, "")
	applyFingerprintNormalizeUserAgent(account, nil, anthropicFingerprintCodex, "")
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
