// 私有扩展（不属于 upstream sub2api）。
// openai_gateway_messages_anthropic_native_fingerprint.go 的单元测试。
package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func fpNormalizeTestAccount(id int64) *Account {
	return &Account{ID: id, Name: "glm-test", Platform: PlatformZhipu}
}

func TestNormalizeNativeAnthropicRequestBodyJSONUserID(t *testing.T) {
	account := fpNormalizeTestAccount(42)
	body := []byte(`{"model":"glm-4.7","metadata":{"user_id":"{\"device_id\":\"client-aabbcc\",\"account_uuid\":\"11111111-2222-3333-4444-555555555555\",\"session_id\":\"sess-abc\"}"},"system":[{"type":"text","text":"You are Claude Code"}],"messages":[{"role":"user","content":"hi"}]}`)

	out := NormalizeNativeAnthropicRequestBody(account, body)
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
	out2 := NormalizeNativeAnthropicRequestBody(account, body)
	if string(extractJSONString(t, out2, "metadata.user_id")) != raw {
		t.Fatal("same account should produce identical canonical identity")
	}

	// 不同账号派生不同身份
	out3 := NormalizeNativeAnthropicRequestBody(fpNormalizeTestAccount(43), body)
	if extractJSONString(t, out3, "metadata.user_id") == raw {
		t.Fatal("different accounts should derive different identities")
	}
}

func TestNormalizeNativeAnthropicRequestBodyLegacyUserID(t *testing.T) {
	account := fpNormalizeTestAccount(7)
	legacy := "user_0000000000000000000000000000000000000000000000000000000000000000_account_11111111-2222-3333-4444-555555555555_session_99999999-8888-7777-6666-555555555555"
	body, _ := json.Marshal(map[string]any{"metadata": map[string]any{"user_id": legacy}})

	out := NormalizeNativeAnthropicRequestBody(account, body)
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
		out := NormalizeNativeAnthropicRequestBody(account, body)
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
		out := NormalizeNativeAnthropicRequestBody(account, body)
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
		out := NormalizeNativeAnthropicRequestBody(account, body)
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
		out := NormalizeNativeAnthropicRequestBody(account, body)
		if !bytes.Equal(extractRaw(t, out, "system"), []byte(`[{"type":"text","text":"keep me"}]`)) {
			t.Fatalf("system bytes changed: %s", extractRaw(t, out, "system"))
		}
	})
}

func TestNormalizeNativeAnthropicRequestBodyPassthrough(t *testing.T) {
	account := fpNormalizeTestAccount(9)

	t.Run("nil account", func(t *testing.T) {
		body := []byte(`{"metadata":{"user_id":"{\"device_id\":\"x\",\"session_id\":\"y\"}"}}`)
		if got := NormalizeNativeAnthropicRequestBody(nil, body); !bytes.Equal(got, body) {
			t.Fatal("nil account must not modify body")
		}
	})

	t.Run("unparseable user_id stays", func(t *testing.T) {
		body := []byte(`{"metadata":{"user_id":"garbage-not-a-format"}}`)
		out := NormalizeNativeAnthropicRequestBody(account, body)
		if extractJSONString(t, out, "metadata.user_id") != "garbage-not-a-format" {
			t.Fatal("unparseable user_id must stay unchanged")
		}
	})

	t.Run("no metadata at all", func(t *testing.T) {
		body := []byte(`{"model":"glm-4.7","messages":[{"role":"user","content":"hi"}]}`)
		if got := NormalizeNativeAnthropicRequestBody(account, body); !bytes.Equal(got, body) {
			t.Fatal("body without identity fields must stay unchanged")
		}
	})
}

func TestNormalizeNativeAnthropicRequestHeaders(t *testing.T) {
	account := fpNormalizeTestAccount(3)
	h := http.Header{}
	h.Set("User-Agent", "some-sdk/0.1.2")
	h.Set("x-anthropic-billing-header", "cc_version=2.1.81.a1b")

	NormalizeNativeAnthropicRequestHeaders(account, h)

	if got := h.Get("User-Agent"); got != anthropicFingerprintNormalizedUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, anthropicFingerprintNormalizedUserAgent)
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
	NormalizeNativeAnthropicRequestHeaders(overridden, h2)
	if got := h2.Get("User-Agent"); got != "claude-cli/9.9.9" {
		t.Fatalf("account-level UA override must win, got %q", got)
	}
	if h2.Get("x-anthropic-billing-header") != "" {
		t.Fatal("x-anthropic-billing-header must still be stripped with UA override")
	}

	// nil 安全
	NormalizeNativeAnthropicRequestHeaders(nil, nil)
	NormalizeNativeAnthropicRequestHeaders(nil, h2)
}

func TestAnthropicFingerprintNormalizeContextSwitch(t *testing.T) {
	if anthropicFingerprintNormalizeEnabled(nil) {
		t.Fatal("nil context must read as disabled")
	}

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	if anthropicFingerprintNormalizeEnabled(c) {
		t.Fatal("unset key must read as disabled")
	}

	SetAnthropicFingerprintNormalize(c, true)
	if !anthropicFingerprintNormalizeEnabled(c) {
		t.Fatal("enabled flag was lost")
	}
	SetAnthropicFingerprintNormalize(c, false)
	if anthropicFingerprintNormalizeEnabled(c) {
		t.Fatal("disabled flag was lost")
	}
	SetAnthropicFingerprintNormalize(nil, true) // 不应 panic
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
