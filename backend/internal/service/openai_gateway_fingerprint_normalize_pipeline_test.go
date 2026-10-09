// 私有扩展（不属于 upstream sub2api）。
// 指纹归一化在三条出站路径上的链路测试：从入站请求头到上游实际收到的请求头。
// 纯函数单测见 openai_gateway_messages_anthropic_native_fingerprint_test.go。
package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
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
	return sendCCAndCaptureHeaders(t, account, c, targetURL).Get("User-Agent")
}

// sendCCAndCaptureHeaders 经 sendCCUpstreamRequest 发出请求，返回上游收到的请求头。
func sendCCAndCaptureHeaders(t *testing.T, account *Account, c *gin.Context, targetURL string) http.Header {
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
	return upstream.request.Header
}

// fpPipelineAccountWith 构造开启指定 Extra 开关键的智谱账号。
func fpPipelineAccountWith(keys ...string) *Account {
	account := fpPipelineAccount(PlatformZhipu, false, false)
	for _, key := range keys {
		account.Extra[key] = true
	}
	return account
}

// adaptive 智谱账号的 Codex 流量（/v1/responses 转 chat_completions）走 CC 出站路径。
func TestFingerprintNormalizeCCPipeline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	codexUA := anthropicFingerprintNormalizedUserAgent(anthropicFingerprintCodex, "")
	claudeUA := anthropicFingerprintNormalizedUserAgent(anthropicFingerprintClaudeCode, "")
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

// 生效范围仅智谱：其他国产供应商即使 extra 里写了开关也不归一。
func TestFingerprintNormalizeOnlyZhipu(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, platform := range []string{PlatformKimi, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo, PlatformOpenAI} {
		t.Run(platform, func(t *testing.T) {
			account := fpPipelineAccount(platform, true, true)
			for _, ua := range []string{fpPipelineCodexUA, fpPipelineClaudeUA} {
				c := fpPipelineContext(t, ua, "")
				require.Equal(t, anthropicFingerprintOff, accountAnthropicFingerprintTarget(account, c))
			}
		})
	}

	t.Run("CC 出站：DeepSeek 开关全开 UA 仍原样", func(t *testing.T) {
		c := fpPipelineContext(t, fpPipelineCodexUA, "codex-tui")
		got := sendCCAndCaptureUA(t, fpPipelineAccount(PlatformDeepseek, true, true), c, "https://api.deepseek.com/chat/completions")
		require.Equal(t, fpPipelineCodexUA, got)
	})

	t.Run("原生 Responses 出站：DeepSeek 开关全开 UA 仍原样", func(t *testing.T) {
		account := fpPipelineAccount(PlatformDeepseek, true, true)
		require.True(t, account.UsesNativeCNResponses(), "前置条件：DeepSeek adaptive 账号走原生 Responses")
		c := fpPipelineContext(t, fpPipelineCodexUA, "codex-tui")
		req, err := openCodeSessionTestService().buildUpstreamRequest(
			context.Background(), c, account, []byte(`{"model":"deepseek-v4","input":"hello"}`), "sk-test", false, "", false,
		)
		require.NoError(t, err)
		require.Equal(t, fpPipelineCodexUA, req.Header.Get("User-Agent"))
	})

	t.Run("智谱开关全开才生效", func(t *testing.T) {
		account := fpPipelineAccount(PlatformZhipu, true, true)
		require.Equal(t, anthropicFingerprintCodex, accountAnthropicFingerprintTarget(account, fpPipelineContext(t, fpPipelineCodexUA, "codex-tui")))
		require.Equal(t, anthropicFingerprintClaudeCode, accountAnthropicFingerprintTarget(account, fpPipelineContext(t, fpPipelineClaudeUA, "")))
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
		resetClaudeCodeStainlessLearned(t)
		h := stainless()
		NormalizeNativeAnthropicRequestHeaders(fpNormalizeTestAccount(21), h, anthropicFingerprintClaudeCode, "")
		require.Equal(t, claudeCodeStainlessFallbackPackageVer, getHeaderRaw(h, "x-stainless-package-version"))
		require.Equal(t, claudeCodeStainlessOS, getHeaderRaw(h, "x-stainless-os"))
		require.Equal(t, claudeCodeStainlessFallbackRuntimeVer, getHeaderRaw(h, "x-stainless-runtime-version"))
		require.Equal(t, "", getHeaderRaw(h, "x-stainless-arch"), "请求里没有的身份键不应新增")
		require.Equal(t, "2", getHeaderRaw(h, "x-stainless-retry-count"), "retry-count 是逐请求值，不属于身份")
	})

	t.Run("codex：删除全部 x-stainless-*", func(t *testing.T) {
		h := stainless()
		NormalizeNativeAnthropicRequestHeaders(fpNormalizeTestAccount(22), h, anthropicFingerprintCodex, "")
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
		NormalizeNativeAnthropicRequestHeaders(account, h, anthropicFingerprintCodex, "")
		require.Equal(t, "Windows", getHeaderRaw(h, "x-stainless-os"))
		require.Equal(t, "", getHeaderRaw(h, "x-stainless-package-version"))
	})

	t.Run("UA 总是写入，且不残留不同大小写的旧 UA", func(t *testing.T) {
		want := anthropicFingerprintNormalizedUserAgent(anthropicFingerprintClaudeCode, "")

		missing := http.Header{}
		NormalizeNativeAnthropicRequestHeaders(fpNormalizeTestAccount(24), missing, anthropicFingerprintClaudeCode, "")
		require.Equal(t, want, getHeaderRaw(missing, "user-agent"), "请求原本没带 UA 也要补上")

		dup := http.Header{}
		dup["user-agent"] = []string{"raw-lower/1.0"}
		dup.Set("User-Agent", "canonical/1.0")
		NormalizeNativeAnthropicRequestHeaders(fpNormalizeTestAccount(25), dup, anthropicFingerprintClaudeCode, "")
		var values []string
		for name, vals := range dup {
			if http.CanonicalHeaderKey(name) == "User-Agent" {
				values = append(values, vals...)
			}
		}
		require.Equal(t, []string{want}, values)
	})
}

// ZCode 开关、IDE 入口保留、Codex originator 配套。
func TestFingerprintNormalizeClientScopes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	codexIdentity := resolveCodexOutboundIdentity("")
	const vscodeUA = "claude-cli/2.1.100 (external, claude-vscode, agent-sdk/0.2.1)"
	const zcodeUA = "ZCode/3.11.2 ai-sdk/provider-utils/4.0.27 runtime/node.js/22"

	t.Run("codex：UA 与 originator 成对写入（OAuth 同源）", func(t *testing.T) {
		c := fpPipelineContext(t, fpPipelineCodexUA, "codex_cli_rs")
		h := sendCCAndCaptureHeaders(t, fpPipelineAccountWith(anthropicFingerprintNormalizeCodexExtraKey), c, fpPipelineZhipuCC)
		require.Equal(t, codexIdentity.userAgent, h.Get("User-Agent"))
		require.Equal(t, codexIdentity.originator, h.Get("originator"))
	})

	t.Run("claudecode：IDE 入口段保留，只统一版本", func(t *testing.T) {
		account := fpPipelineAccountWith(anthropicFingerprintNormalizeClaudeCodeExtraKey)
		h := http.Header{}
		h.Set("User-Agent", vscodeUA)
		NormalizeNativeAnthropicRequestHeaders(account, h, accountAnthropicFingerprintTarget(account, fpPipelineContext(t, vscodeUA, "")), vscodeUA)
		require.Equal(t, "claude-cli/"+claude.EffectiveCLIVersion()+" (external, claude-vscode, agent-sdk/0.2.1)", getHeaderRaw(h, "user-agent"))
		require.Empty(t, getHeaderRaw(h, "originator"), "非 codex 形态不写 originator")
	})

	t.Run("claudecode 开关不覆盖 other 客户端", func(t *testing.T) {
		c := fpPipelineContext(t, "litellm/1.70.0", "")
		got := sendCCAndCaptureUA(t, fpPipelineAccountWith(anthropicFingerprintNormalizeClaudeCodeExtraKey), c, fpPipelineZhipuCC)
		require.Equal(t, "litellm/1.70.0", got)
	})

	t.Run("zcode：版本与 node 运行时统一", func(t *testing.T) {
		c := fpPipelineContext(t, zcodeUA, "")
		got := sendCCAndCaptureUA(t, fpPipelineAccountWith(anthropicFingerprintNormalizeZCodeExtraKey), c, fpPipelineZhipuCC)
		require.Equal(t, "ZCode/"+zcodeCanonicalVersion+" ai-sdk/provider-utils/4.0.27 runtime/node.js/"+zcodeCanonicalNodeRuntime, got)
	})

	t.Run("账号级 originator 覆写优先", func(t *testing.T) {
		account := fpPipelineAccountWith(anthropicFingerprintNormalizeCodexExtraKey)
		account.Credentials[credKeyHeaderOverrideEnabled] = true
		account.Credentials[credKeyHeaderOverrides] = map[string]any{"originator": "admin-pinned"}
		c := fpPipelineContext(t, fpPipelineCodexUA, "codex_cli_rs")
		require.Equal(t, "admin-pinned", getHeaderRaw(sendCCAndCaptureHeaders(t, account, c, fpPipelineZhipuCC), "originator"))
	})
}

// 客户端准入：开启后只允许 Codex / Claude Code / ZCode，其他客户端 403。
func TestAnthropicFingerprintRestrictClients(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const zcodeUA = "ZCode/3.14.3 ai-sdk/provider-utils/4.0.27 runtime/node.js/24"
	restricted := fpPipelineAccountWith(anthropicFingerprintRestrictClientsExtraKey)

	svc := openCodeSessionTestService()

	t.Run("判定：ZCode 放行，其他拒绝", func(t *testing.T) {
		require.False(t, svc.shouldRejectAnthropicFingerprintClient(restricted, fpPipelineContext(t, zcodeUA, ""), nil))
		for _, ua := range []string{"litellm/1.70.0", "curl/8.0", ""} {
			require.True(t, svc.shouldRejectAnthropicFingerprintClient(restricted, fpPipelineContext(t, ua, ""), nil), ua)
		}
	})

	t.Run("判定：Codex 走 codex_cli_only 同款门（版本 + 引擎指纹）", func(t *testing.T) {
		withFingerprint := fpPipelineContext(t, fpPipelineCodexUA, "")
		withFingerprint.Request.Header.Set("x-codex-window-id", "w1")
		require.False(t, svc.shouldRejectAnthropicFingerprintClient(restricted, withFingerprint, nil), "官方 UA + x-codex- 头放行")

		require.True(t, svc.shouldRejectAnthropicFingerprintClient(restricted, fpPipelineContext(t, fpPipelineCodexUA, ""), nil), "缺引擎指纹头拒绝")

		originatorOnly := fpPipelineContext(t, "some-sdk/1.0", "codex_cli_rs")
		originatorOnly.Request.Header.Set("x-codex-window-id", "w1")
		require.True(t, svc.shouldRejectAnthropicFingerprintClient(restricted, originatorOnly, nil), "官方 originator 但 UA 无可解析引擎版本，被版本门拒绝")
	})

	t.Run("判定：Claude Code 复用 ClaudeCodeValidator", func(t *testing.T) {
		// 非 messages 路径（/v1/responses）：UA 匹配即通过。
		require.False(t, svc.shouldRejectAnthropicFingerprintClient(restricted, fpPipelineContext(t, fpPipelineClaudeUA, ""), nil))

		// messages 路径：只有 UA 不够，缺 system / 必需头 / metadata.user_id 被拒。
		bare := fpPipelineContext(t, fpPipelineClaudeUA, "")
		bare.Request.URL.Path = "/v1/messages"
		require.True(t, svc.shouldRejectAnthropicFingerprintClient(restricted, bare, []byte(`{"model":"glm-5.1","messages":[]}`)))

		// messages 路径：完整的 Claude Code 请求放行。
		full := fpPipelineContext(t, fpPipelineClaudeUA, "")
		full.Request.URL.Path = "/v1/messages"
		full.Request.Header.Set("X-App", "cli")
		full.Request.Header.Set("anthropic-beta", "claude-code-20250219")
		full.Request.Header.Set("anthropic-version", "2023-06-01")
		userID := `{"device_id":"` + strings.Repeat("a", 64) + `","account_uuid":"","session_id":"11111111-1111-1111-1111-111111111111"}`
		userIDJSON, err := json.Marshal(userID)
		require.NoError(t, err)
		body := []byte(`{"model":"glm-5.1","system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],` +
			`"metadata":{"user_id":` + string(userIDJSON) + `},"messages":[]}`)
		require.False(t, svc.shouldRejectAnthropicFingerprintClient(restricted, full, body))
	})

	t.Run("默认关闭不拦截", func(t *testing.T) {
		require.False(t, svc.shouldRejectAnthropicFingerprintClient(fpPipelineAccountWith(), fpPipelineContext(t, "litellm/1.70.0", ""), nil))
	})

	t.Run("非智谱账号即使写了开关也不拦截", func(t *testing.T) {
		account := fpPipelineAccount(PlatformDeepseek, false, false)
		account.Extra[anthropicFingerprintRestrictClientsExtraKey] = true
		require.False(t, svc.shouldRejectAnthropicFingerprintClient(account, fpPipelineContext(t, "litellm/1.70.0", ""), nil))
	})

	t.Run("/v1/messages 入口：403 且不请求上游", func(t *testing.T) {
		c := fpPipelineContext(t, "litellm/1.70.0", "")
		upstream := &openCodeSessionHTTPUpstream{}
		svc := openCodeSessionTestService()
		svc.httpUpstream = upstream
		body := []byte(`{"model":"glm-5.1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
		_, err := svc.ForwardAsAnthropic(context.Background(), c, restricted, body, "", "")
		require.ErrorIs(t, err, errAnthropicFingerprintClientRestricted)
		require.Equal(t, http.StatusForbidden, c.Writer.Status())
		require.Nil(t, upstream.request, "被拒绝的请求不应发往上游")
	})

	t.Run("/v1/chat/completions 入口：403 且不请求上游", func(t *testing.T) {
		c := fpPipelineContext(t, "litellm/1.70.0", "")
		upstream := &openCodeSessionHTTPUpstream{}
		svc := openCodeSessionTestService()
		svc.httpUpstream = upstream
		body := []byte(`{"model":"glm-5.1","messages":[{"role":"user","content":"hi"}]}`)
		_, err := svc.ForwardAsChatCompletions(context.Background(), c, restricted, body, "", "")
		require.ErrorIs(t, err, errAnthropicFingerprintClientRestricted)
		require.Equal(t, http.StatusForbidden, c.Writer.Status())
		require.Nil(t, upstream.request)
	})

	t.Run("/v1/responses 入口：403 且不请求上游", func(t *testing.T) {
		c := fpPipelineContext(t, "litellm/1.70.0", "")
		upstream := &openCodeSessionHTTPUpstream{}
		svc := openCodeSessionTestService()
		svc.httpUpstream = upstream
		_, err := svc.Forward(context.Background(), c, restricted, []byte(`{"model":"glm-5.1","input":"hi"}`))
		require.ErrorIs(t, err, errAnthropicFingerprintClientRestricted)
		require.Equal(t, http.StatusForbidden, c.Writer.Status())
		require.Nil(t, upstream.request)
	})
}

// resetClaudeCodeStainlessLearned 清空学习到的 stainless 版本，测试结束后恢复。
func resetClaudeCodeStainlessLearned(t *testing.T) {
	t.Helper()
	claudeCodeStainlessLearnedMu.Lock()
	saved := claudeCodeStainlessLearned
	claudeCodeStainlessLearned = claudeCodeStainlessVersions{}
	claudeCodeStainlessLearnedMu.Unlock()
	t.Cleanup(func() {
		claudeCodeStainlessLearnedMu.Lock()
		claudeCodeStainlessLearned = saved
		claudeCodeStainlessLearnedMu.Unlock()
	})
}

// x-stainless 版本类规范值跟随生效 CLI 版本：从同版本真实客户端学习，否则回退兜底值。
func TestClaudeCodeStainlessVersionLearning(t *testing.T) {
	resetClaudeCodeStainlessLearned(t)
	effective := claude.EffectiveCLIVersion()
	inbound := func(pkg, runtime string) http.Header {
		h := http.Header{}
		setHeaderRaw(h, resolveWireCasing("x-stainless-package-version"), pkg)
		setHeaderRaw(h, resolveWireCasing("x-stainless-runtime-version"), runtime)
		return h
	}
	canonical := func() (string, string) {
		c := claudeCodeCanonicalStainlessHeaders()
		return c[claudeCodeStainlessPackageVersionHeader], c[claudeCodeStainlessRuntimeVersionHeader]
	}

	pkg, runtime := canonical()
	require.Equal(t, claudeCodeStainlessFallbackPackageVer, pkg, "未学习时回退兜底值")
	require.Equal(t, claudeCodeStainlessFallbackRuntimeVer, runtime)

	learnClaudeCodeStainlessVersions("claude-cli/0.0.1 (external, cli)", inbound("9.9.9", "v99.0.0"))
	pkg, _ = canonical()
	require.Equal(t, claudeCodeStainlessFallbackPackageVer, pkg, "非生效版本的客户端不参与学习")

	learnClaudeCodeStainlessVersions("claude-cli/"+effective+" (external, cli)", inbound("bad", "v1"))
	pkg, _ = canonical()
	require.Equal(t, claudeCodeStainlessFallbackPackageVer, pkg, "畸形值不参与学习")

	learnClaudeCodeStainlessVersions("claude-cli/"+effective+" (external, claude-vscode, agent-sdk/0.2.1)", inbound("0.120.0", "v26.5.0"))
	pkg, runtime = canonical()
	require.Equal(t, "0.120.0", pkg, "同生效版本的真实客户端被学习")
	require.Equal(t, "v26.5.0", runtime)

	claudeCodeStainlessLearnedMu.Lock()
	claudeCodeStainlessLearned.cliVersion = "0.0.1"
	claudeCodeStainlessLearnedMu.Unlock()
	pkg, _ = canonical()
	require.Equal(t, claudeCodeStainlessFallbackPackageVer, pkg, "生效版本切换后学习值失效，回退兜底值")
}

// 开 ZCode 开关后补齐 X-ZCode-* 身份头，并删除 x-stainless-*。
func TestZCodeIdentityHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const zcodeUA = "ZCode/3.14.1 ai-sdk/provider-utils/4.0.27 runtime/node.js/24"
	account := fpPipelineAccountWith(anthropicFingerprintNormalizeZCodeExtraKey)

	t.Run("CC 出站：规范身份头 + 入站会话 / 请求 ID 沿用", func(t *testing.T) {
		c := fpPipelineContext(t, zcodeUA, "")
		c.Request.Header.Set("X-Title", zcodeTitleCLI)
		c.Request.Header.Set("X-Session-Id", "sess-inbound")
		c.Request.Header.Set("X-Request-Id", "req-inbound")
		c.Request.Header.Set("X-Platform", "win32-x64")
		h := sendCCAndCaptureHeaders(t, account, c, fpPipelineZhipuCC)
		require.Equal(t, zcodeCanonicalVersion, h.Get("X-ZCode-App-Version"))
		require.Equal(t, zcodeTitleCLI, h.Get("X-Title"), "保留入站的 CLI 身份")
		require.Equal(t, zcodeReferer, h.Get("HTTP-Referer"))
		require.Equal(t, zcodeCanonicalPlatform, h.Get("X-Platform"), "设备属性统一为账号级值")
		require.Equal(t, zcodeCanonicalTimezone, h.Get("X-Client-Timezone"))
		require.Equal(t, "sess-inbound", h.Get("X-Session-Id"))
		require.Equal(t, "req-inbound", h.Get("X-Request-Id"))
		require.NotEmpty(t, h.Get("X-Query-Id"), "入站缺失的请求级 ID 由网关生成")
		require.NotEmpty(t, h.Get("X-ZCode-Trace-Id"))
	})

	t.Run("入站缺会话 ID：按账号确定性派生", func(t *testing.T) {
		first := sendCCAndCaptureHeaders(t, account, fpPipelineContext(t, zcodeUA, ""), fpPipelineZhipuCC)
		second := sendCCAndCaptureHeaders(t, account, fpPipelineContext(t, zcodeUA, ""), fpPipelineZhipuCC)
		require.NotEmpty(t, first.Get("X-Session-Id"))
		require.Equal(t, first.Get("X-Session-Id"), second.Get("X-Session-Id"))
		require.Equal(t, zcodeTitleElectron, first.Get("X-Title"), "缺失时取桌面身份")
	})

	t.Run("Anthropic 直通：删除 x-stainless-*", func(t *testing.T) {
		h := http.Header{}
		setHeaderRaw(h, resolveWireCasing("x-stainless-os"), "MacOS")
		NormalizeNativeAnthropicRequestHeaders(account, h, anthropicFingerprintZCode, zcodeUA)
		applyZCodeIdentityHeaders(account, h, anthropicFingerprintZCode, nil)
		require.Empty(t, getHeaderRaw(h, "x-stainless-os"))
		require.Equal(t, zcodeCanonicalVersion, h.Get("X-ZCode-App-Version"))
	})

	t.Run("非 zcode 目标不注入", func(t *testing.T) {
		h := sendCCAndCaptureHeaders(t, account, fpPipelineContext(t, fpPipelineClaudeUA, ""), fpPipelineZhipuCC)
		require.Empty(t, h.Get("X-ZCode-App-Version"))
	})

	t.Run("账号级覆写优先", func(t *testing.T) {
		overridden := fpPipelineAccountWith(anthropicFingerprintNormalizeZCodeExtraKey)
		overridden.Credentials[credKeyHeaderOverrideEnabled] = true
		overridden.Credentials[credKeyHeaderOverrides] = map[string]any{"x-platform": "linux-x64"}
		h := sendCCAndCaptureHeaders(t, overridden, fpPipelineContext(t, zcodeUA, ""), fpPipelineZhipuCC)
		require.Equal(t, "linux-x64", getHeaderRaw(h, "x-platform"))
	})
}
