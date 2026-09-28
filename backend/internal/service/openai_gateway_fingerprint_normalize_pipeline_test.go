// 私有扩展（不属于 upstream sub2api）。
// 指纹归一化在三条出站路径上的链路测试：从入站请求头到上游实际收到的请求头。
// 纯函数单测见 openai_gateway_messages_anthropic_native_fingerprint_test.go。
package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const (
	fpPipelineCodexUA  = "codex_cli_rs/0.125.0 (Ubuntu 22.4.0; x86_64) xterm-256color"
	fpPipelineClaudeUA = "claude-cli/2.1.200 (external, cli)"
	fpPipelineZhipuCC  = "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions"
)

// fpPipelineAccount 构造一个开启指定归一化开关的 api_key 账号。
func fpPipelineAccount(platform string, codexOn, claudeOn bool) *Account {
	extra := map[string]any{}
	if codexOn {
		extra[anthropicFingerprintNormalizeCodexExtraKey] = true
	}
	if claudeOn {
		extra[anthropicFingerprintNormalizeClaudeCodeExtraKey] = true
	}
	return &Account{
		ID:          101,
		Name:        "fp-pipeline",
		Platform:    platform,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test", "api_protocol": APIProtocolAdaptive},
		Extra:       extra,
	}
}

// fpPipelineContext 构造带入站客户端身份头的请求上下文。
func fpPipelineContext(t *testing.T, userAgent, originator string) *gin.Context {
	t.Helper()
	c := newOpenCodeSessionTestContext(t, "")
	c.Request.Header.Set("User-Agent", userAgent)
	if originator != "" {
		c.Request.Header.Set("originator", originator)
	}
	return c
}

// sendCCAndCaptureUA 经 sendCCUpstreamRequest 发出请求，返回上游收到的 User-Agent。
func sendCCAndCaptureUA(t *testing.T, account *Account, c *gin.Context, targetURL string) string {
	t.Helper()
	upstream := &openCodeSessionHTTPUpstream{}
	svc := openCodeSessionTestService()
	svc.httpUpstream = upstream
	resp, err := svc.sendCCUpstreamRequest(
		context.Background(), c, account, targetURL, []byte(`{"model":"glm-5.1"}`),
		false, "sk-test", "", "",
	)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.NotNil(t, upstream.request)
	return upstream.request.Header.Get("User-Agent")
}

// adaptive 智谱账号的 Codex 流量（/v1/responses 转 chat_completions）走 CC 出站路径。
func TestFingerprintNormalizeCCPipeline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	codexUA := anthropicFingerprintNormalizedUserAgent(anthropicFingerprintCodex)
	claudeUA := anthropicFingerprintNormalizedUserAgent(anthropicFingerprintClaudeCode)
	require.NotEmpty(t, codexUA)
	require.NotEmpty(t, claudeUA)

	t.Run("codex 开关 + Codex 客户端 → codex-tui", func(t *testing.T) {
		c := fpPipelineContext(t, fpPipelineCodexUA, "codex-tui")
		got := sendCCAndCaptureUA(t, fpPipelineAccount(PlatformZhipu, true, false), c, fpPipelineZhipuCC)
		require.Equal(t, codexUA, got)
	})

	t.Run("只开 codex + Claude Code 客户端 → UA 原样", func(t *testing.T) {
		c := fpPipelineContext(t, fpPipelineClaudeUA, "")
		got := sendCCAndCaptureUA(t, fpPipelineAccount(PlatformZhipu, true, false), c, fpPipelineZhipuCC)
		require.Equal(t, fpPipelineClaudeUA, got)
	})

	t.Run("两个都开 + Claude Code 客户端 → claude-cli", func(t *testing.T) {
		c := fpPipelineContext(t, fpPipelineClaudeUA, "")
		got := sendCCAndCaptureUA(t, fpPipelineAccount(PlatformZhipu, true, true), c, fpPipelineZhipuCC)
		require.Equal(t, claudeUA, got)
	})

	t.Run("两个都关 → UA 原样", func(t *testing.T) {
		c := fpPipelineContext(t, fpPipelineCodexUA, "codex-tui")
		got := sendCCAndCaptureUA(t, fpPipelineAccount(PlatformZhipu, false, false), c, fpPipelineZhipuCC)
		require.Equal(t, fpPipelineCodexUA, got)
	})

	t.Run("账号级 UA 覆写优先于归一化", func(t *testing.T) {
		account := fpPipelineAccount(PlatformZhipu, true, true)
		account.Credentials[credKeyHeaderOverrideEnabled] = true
		account.Credentials[credKeyHeaderOverrides] = map[string]any{"user-agent": "admin-pinned/1.0"}
		c := fpPipelineContext(t, fpPipelineCodexUA, "codex-tui")
		got := sendCCAndCaptureUA(t, account, c, fpPipelineZhipuCC)
		require.Equal(t, "admin-pinned/1.0", got)
	})
}

// OpenCode Go 依赖规范 UA 过 Cloudflare，开关即使被写入也不能改变其出站 UA。
func TestFingerprintNormalizeSkipsOpenCodeGo(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const target = "https://opencode.ai/zen/go/v1/chat/completions"

	baseline := fpPipelineAccount(PlatformOpenCodeGo, false, false)
	baseline.Credentials["base_url"] = "https://opencode.ai/zen/go/v1"
	enabled := fpPipelineAccount(PlatformOpenCodeGo, true, true)
	enabled.Credentials["base_url"] = "https://opencode.ai/zen/go/v1"

	wantUA := sendCCAndCaptureUA(t, baseline, fpPipelineContext(t, fpPipelineCodexUA, "codex-tui"), target)
	gotUA := sendCCAndCaptureUA(t, enabled, fpPipelineContext(t, fpPipelineCodexUA, "codex-tui"), target)
	require.Equal(t, wantUA, gotUA, "OpenCode Go 出站 UA 不应受指纹归一化开关影响")
	require.Equal(t, anthropicFingerprintOff, accountAnthropicFingerprintTarget(enabled, fpPipelineContext(t, fpPipelineCodexUA, "codex-tui")))
}

// kimi / deepseek / minimax 的 adaptive 账号，Codex 流量走原生 Responses 端点（buildUpstreamRequest）。
func TestFingerprintNormalizeNativeCNResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	codexUA := anthropicFingerprintNormalizedUserAgent(anthropicFingerprintCodex)
	svc := openCodeSessionTestService()
	body := []byte(`{"model":"deepseek-v4","input":"hello"}`)

	for _, platform := range []string{PlatformDeepseek, PlatformKimi, PlatformMiniMax} {
		t.Run(platform, func(t *testing.T) {
			account := fpPipelineAccount(platform, true, false)
			require.True(t, account.UsesNativeCNResponses(), "前置条件：该平台 adaptive 账号应走原生 Responses")

			c := fpPipelineContext(t, fpPipelineCodexUA, "codex-tui")
			req, err := svc.buildUpstreamRequest(context.Background(), c, account, body, "sk-test", false, "", false)
			require.NoError(t, err)
			require.Equal(t, codexUA, req.Header.Get("User-Agent"))
		})
	}

	t.Run("开关关闭时 UA 原样", func(t *testing.T) {
		account := fpPipelineAccount(PlatformDeepseek, false, false)
		c := fpPipelineContext(t, fpPipelineCodexUA, "codex-tui")
		req, err := svc.buildUpstreamRequest(context.Background(), c, account, body, "sk-test", false, "", false)
		require.NoError(t, err)
		require.Equal(t, fpPipelineCodexUA, req.Header.Get("User-Agent"))
	})
}

// Anthropic 直通出站头：UA 总是写入、x-stainless-* 身份头按目标统一。
func TestNormalizeNativeAnthropicStainlessHeaders(t *testing.T) {
	stainless := func() http.Header {
		h := http.Header{}
		setHeaderRaw(h, resolveWireCasing("x-stainless-package-version"), "0.50.0")
		setHeaderRaw(h, resolveWireCasing("x-stainless-os"), "MacOS")
		setHeaderRaw(h, resolveWireCasing("x-stainless-runtime-version"), "v20.1.0")
		setHeaderRaw(h, resolveWireCasing("x-stainless-retry-count"), "2")
		return h
	}

	t.Run("claudecode：已有身份键改写为规范值，只改不增，逐请求键不动", func(t *testing.T) {
		h := stainless()
		NormalizeNativeAnthropicRequestHeaders(fpNormalizeTestAccount(21), h, anthropicFingerprintClaudeCode)
		require.Equal(t, "0.94.0", getHeaderRaw(h, "x-stainless-package-version"))
		require.Equal(t, "Linux", getHeaderRaw(h, "x-stainless-os"))
		require.Equal(t, "v24.3.0", getHeaderRaw(h, "x-stainless-runtime-version"))
		require.Equal(t, "", getHeaderRaw(h, "x-stainless-arch"), "请求里没有的身份键不应新增")
		require.Equal(t, "2", getHeaderRaw(h, "x-stainless-retry-count"), "retry-count 是逐请求值，不属于身份")
	})

	t.Run("codex：删除全部 x-stainless-*", func(t *testing.T) {
		h := stainless()
		NormalizeNativeAnthropicRequestHeaders(fpNormalizeTestAccount(22), h, anthropicFingerprintCodex)
		for name := range h {
			require.NotContains(t, name, "tainless", "codex 目标下不应残留 x-stainless 头：%s", name)
		}
	})

	t.Run("账号级覆写的 stainless 键保留管理员值", func(t *testing.T) {
		account := fpNormalizeTestAccount(23)
		account.Type = AccountTypeAPIKey
		account.Credentials = map[string]any{
			credKeyHeaderOverrideEnabled: true,
			credKeyHeaderOverrides:       map[string]any{"x-stainless-os": "Windows"},
		}
		h := stainless()
		account.ApplyHeaderOverrides(h)
		NormalizeNativeAnthropicRequestHeaders(account, h, anthropicFingerprintCodex)
		require.Equal(t, "Windows", getHeaderRaw(h, "x-stainless-os"))
		require.Equal(t, "", getHeaderRaw(h, "x-stainless-package-version"))
	})

	t.Run("UA 总是写入，且不残留不同大小写的旧 UA", func(t *testing.T) {
		want := anthropicFingerprintNormalizedUserAgent(anthropicFingerprintClaudeCode)

		missing := http.Header{}
		NormalizeNativeAnthropicRequestHeaders(fpNormalizeTestAccount(24), missing, anthropicFingerprintClaudeCode)
		require.Equal(t, want, getHeaderRaw(missing, "user-agent"), "请求原本没带 UA 也要补上")

		dup := http.Header{}
		dup["user-agent"] = []string{"raw-lower/1.0"}
		dup.Set("User-Agent", "canonical/1.0")
		NormalizeNativeAnthropicRequestHeaders(fpNormalizeTestAccount(25), dup, anthropicFingerprintClaudeCode)
		var values []string
		for name, vals := range dup {
			if http.CanonicalHeaderKey(name) == "User-Agent" {
				values = append(values, vals...)
			}
		}
		require.Equal(t, []string{want}, values)
	})
}
