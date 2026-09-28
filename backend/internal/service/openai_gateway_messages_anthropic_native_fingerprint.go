// 私有扩展（不属于 upstream sub2api）。
//
// 本文件为 CN 供应商 Anthropic 协议直通路径（openai_gateway_messages_anthropic_native.go）
// 提供出站指纹归一化能力，对应账号级配置 account.Extra["anthropic_fingerprint_normalize"]
// （off / claudecode / codex，migration 910 移除了旧的分组级开关）。开启后对出站
// 请求做归一，让同一上游账号的所有拼车用户在供应商侧呈现为「同一个客户端」：
//  1. metadata.user_id 的 device_id/account_uuid 改写为账号级恒定值（session_id 保留）
//  2. 删除 body.system 中 Claude Code 注入的 x-anthropic-billing-header 块
//  3. User-Agent 归一：claudecode → claude-cli/<EffectiveCLIVersion>；
//     codex → 规范 codex-tui UA（版本均走面板/自动同步，不再写死）
//
// 所含符号：
//   - anthropicFingerprintNormalizeMode / anthropicFingerprintNormalizeModeFromExtra
//   - Account.GetAnthropicFingerprintNormalizeMode
//   - NormalizeNativeAnthropicRequestBody / NormalizeNativeAnthropicRequestHeaders
//
// merge 策略：upstream 不含本文件；openai_gateway_messages_anthropic_native.go
// 中仅有 2 处调用 hook，merge 时保留即可。
package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// anthropicFingerprintNormalizeMode 是一次请求解析后的**最终归一化目标形态**
// （不是账号静态配置值）。账号侧配置是两个独立开关（见下方 Extra key），
// 由 resolveAnthropicFingerprintTarget 结合入站客户端类型解析成本类型。
type anthropicFingerprintNormalizeMode string

const (
	// anthropicFingerprintOff 不做归一化，原样透传客户端指纹。
	anthropicFingerprintOff anthropicFingerprintNormalizeMode = "off"
	// anthropicFingerprintClaudeCode 归一成 Claude Code（claude-cli）形态：
	// UA = claude-cli/<EffectiveCLIVersion>，身份字段账号级恒定，删 billing header。
	anthropicFingerprintClaudeCode anthropicFingerprintNormalizeMode = "claudecode"
	// anthropicFingerprintCodex 归一成 Codex（codex-tui）形态：
	// UA = 规范 codex-tui UA（版本走 Codex 自动同步），身份字段账号级恒定。
	anthropicFingerprintCodex anthropicFingerprintNormalizeMode = "codex"
)

// 账号级归一化配置：两个独立开关（account.Extra），可任意组合，互不产生错配。
//   - codex 开关：只把 Codex 客户端的出站请求归一成 codex-tui 形态
//   - claudecode 开关：只把非 Codex 客户端（Claude Code 等）归一成 claude-cli 形态
//
// 让同一上游账号的拼车用户在供应商侧按客户端类型各自收敛成一个稳定客户端。
// adaptive 协议账号同时服务两类客户端时，两个开关都开即可分别归一。
// 两个开关都关 = 不归一（默认，opt-in）。
const (
	anthropicFingerprintNormalizeCodexExtraKey      = "anthropic_fingerprint_normalize_codex"
	anthropicFingerprintNormalizeClaudeCodeExtraKey = "anthropic_fingerprint_normalize_claudecode"
)

// anthropicBillingHeaderBlockRe 匹配 system prompt 中 Claude Code 注入的
// billing header 块（块内容以 x-anthropic-billing-header: 开头）。
var anthropicBillingHeaderBlockRe = regexp.MustCompile(`^\s*x-anthropic-billing-header:`)

// anthropicBillingHeaderLineRe 匹配字符串形态 system 中内联的 billing header 行。
var anthropicBillingHeaderLineRe = regexp.MustCompile(`(?m)^x-anthropic-billing-header:[^\n]*\n?`)

// anthropicFingerprintExtraFlag 读取账号 Extra 中的布尔开关，兼容 JSON 反序列化
// 后的 bool 与字符串 "true" 两种形态；缺失或非法一律 false（opt-in）。
func anthropicFingerprintExtraFlag(extra map[string]any, key string) bool {
	if extra == nil {
		return false
	}
	switch v := extra[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	default:
		return false
	}
}

// AnthropicFingerprintNormalizeCodexEnabled 报告账号是否开启「Codex 客户端归一化」。
func (a *Account) AnthropicFingerprintNormalizeCodexEnabled() bool {
	if a == nil {
		return false
	}
	return anthropicFingerprintExtraFlag(a.Extra, anthropicFingerprintNormalizeCodexExtraKey)
}

// AnthropicFingerprintNormalizeClaudeCodeEnabled 报告账号是否开启「Claude Code 客户端归一化」。
func (a *Account) AnthropicFingerprintNormalizeClaudeCodeEnabled() bool {
	if a == nil {
		return false
	}
	return anthropicFingerprintExtraFlag(a.Extra, anthropicFingerprintNormalizeClaudeCodeExtraKey)
}

// resolveAnthropicFingerprintTarget 按两个独立开关 + 入站客户端类型解析本次请求的
// 最终归一化目标。每个开关只把对应客户端归一成它自己的身份，天然不产生错配：
//   - Codex 客户端：codexEnabled 开则归一成 codex，否则 off
//   - 非 Codex 客户端：claudeCodeEnabled 开则归一成 claudecode，否则 off
func resolveAnthropicFingerprintTarget(codexEnabled, claudeCodeEnabled, isCodexClient bool) anthropicFingerprintNormalizeMode {
	if isCodexClient {
		if codexEnabled {
			return anthropicFingerprintCodex
		}
		return anthropicFingerprintOff
	}
	if claudeCodeEnabled {
		return anthropicFingerprintClaudeCode
	}
	return anthropicFingerprintOff
}

// isCodexInboundClient 判断入站请求是否来自 Codex 官方客户端（按 UA / originator
// 头识别）。c 为空时返回 false。
func isCodexInboundClient(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	return openai.IsCodexOfficialClientByHeaders(
		c.Request.Header.Get("User-Agent"),
		c.Request.Header.Get("originator"),
	)
}

// accountAnthropicFingerprintTarget 便捷组合：读账号两个开关 + 判入站客户端类型，
// 解析出本次请求的最终归一化目标。
//
// 仅国产供应商（kimi/zhipu/deepseek/minimax）生效：OpenCode Go 等上游依赖
// applyOpenCodeUpstreamUserAgent 写入的规范 UA 通过 Cloudflare 前置拦截，被覆盖会
// 触发 CF 1010/403 并计入账号 403 strike。所有出站 hook 都经本函数判定，限制只写这一处。
func accountAnthropicFingerprintTarget(account *Account, c *gin.Context) anthropicFingerprintNormalizeMode {
	if account == nil || !account.IsCNProvider() {
		return anthropicFingerprintOff
	}
	return resolveAnthropicFingerprintTarget(
		account.AnthropicFingerprintNormalizeCodexEnabled(),
		account.AnthropicFingerprintNormalizeClaudeCodeEnabled(),
		isCodexInboundClient(c),
	)
}

// anthropicFingerprintCanonicalDeviceID 返回账号级恒定的 device_id（64 位 hex，
// 与 Claude Code 客户端 device_id 形态一致）。从 account.ID 确定性派生，
// 同一账号永远得到同一值，不同账号互不相同。
func anthropicFingerprintCanonicalDeviceID(account *Account) string {
	if account == nil || account.ID == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte("sub2api:anthropic-fp-device:v1:" + fmt.Sprintf("%d", account.ID)))
	return hex.EncodeToString(sum[:])
}

// anthropicFingerprintCanonicalAccountUUID 返回账号级恒定的 account_uuid（UUIDv4 形态）。
// 复用 Codex 指纹收敛的稳定 UUID 派生（openai_codex_fingerprint.go）。
func anthropicFingerprintCanonicalAccountUUID(account *Account) string {
	if account == nil || account.ID == 0 {
		return ""
	}
	return deriveStableUUIDv4("sub2api:anthropic-fp-account:v1:" + fmt.Sprintf("%d", account.ID))
}

// NormalizeNativeAnthropicRequestBody 对直通出站 body 做指纹归一化：
// 改写 metadata.user_id 身份字段、删除 system 中的 billing header 块。
// claudecode / codex 两种模式的 body 归一化一致（身份字段账号级恒定、剥离
// billing header 块——codex 形态本无该块，删除是防御性无副作用）。
// 任何一步失败都原样返回，绝不阻断转发。
func NormalizeNativeAnthropicRequestBody(account *Account, body []byte, mode anthropicFingerprintNormalizeMode) []byte {
	if account == nil || len(body) == 0 || mode == anthropicFingerprintOff {
		return body
	}
	body = rewriteAnthropicMetadataUserID(account, body)
	body = stripAnthropicBillingHeaderBlocks(body)
	return body
}

// anthropicFingerprintNormalizedUserAgent 返回指定模式的出站归一化 User-Agent。
//   - claudecode：claude-cli/<EffectiveCLIVersion>，版本走面板/自动同步（不再写死）。
//   - codex：规范 codex-tui UA，版本走 Codex 面板/自动同步（codexCanonicalUserAgent）。
//
// 无法解析时返回空串，调用方保持原 UA 不动。
func anthropicFingerprintNormalizedUserAgent(mode anthropicFingerprintNormalizeMode) string {
	switch mode {
	case anthropicFingerprintClaudeCode:
		return claude.DefaultUserAgent()
	case anthropicFingerprintCodex:
		return codexCanonicalUserAgent()
	default:
		return ""
	}
}

// NormalizeNativeAnthropicRequestHeaders 对直通出站 headers 做指纹归一化：
// 按 mode 归一 User-Agent（claudecode / codex 形态）+ 兜底剥离 billing header
// 头（该头不在 allowedHeaders 白名单，正常路径本就不会透传，此处防御账号级
// HeaderOverride 显式注入的情况）。
// 账号级显式配置的 user-agent 覆写优先于归一化默认值（管理员意图优先）。
func NormalizeNativeAnthropicRequestHeaders(account *Account, h http.Header, mode anthropicFingerprintNormalizeMode) {
	if h == nil || mode == anthropicFingerprintOff {
		return
	}
	if account == nil {
		deleteHeaderAllForms(h, "x-anthropic-billing-header")
		return
	}
	// UA 先删全部大小写形态再写入：直通构造器按客户端原始大小写透传，
	// 不清理会残留旧值，出站同时带两个 UA。
	if _, overridden := account.HeaderOverrideValue("user-agent"); !overridden {
		if ua := anthropicFingerprintNormalizedUserAgent(mode); ua != "" {
			deleteHeaderAllForms(h, "user-agent")
			setHeaderRaw(h, resolveWireCasing("user-agent"), ua)
		}
	}
	normalizeStainlessHeaders(account, h, mode)
	deleteHeaderAllForms(h, "x-anthropic-billing-header")
}

// anthropicFingerprintStainlessIdentityKeys 是 x-stainless-* 中携带客户端身份的键
// （SDK 语言/版本、系统、架构、运行时）。retry-count / timeout 是逐请求值，不属于身份。
var anthropicFingerprintStainlessIdentityKeys = []string{
	"x-stainless-lang",
	"x-stainless-package-version",
	"x-stainless-os",
	"x-stainless-arch",
	"x-stainless-runtime",
	"x-stainless-runtime-version",
}

// normalizeStainlessHeaders 统一直通出站的 x-stainless-* 身份头：
//   - claudecode：请求中已有的身份键改写为 claude.DefaultHeaders() 规范值（只改不增，
//     与 IdentityService.ApplyFingerprint 语义一致），消除不同客户端版本的 SDK 差异；
//   - codex：删除全部 x-stainless-*，真实 Codex 客户端不发送这组头，留着会暴露非 Codex。
//
// 账号级显式覆写的键保留管理员值。anthropic-beta 不在此处理：它与请求体能力字段联动
// （sanitizeAnthropicBodyForBetaTokens），统一会导致上游 400。
func normalizeStainlessHeaders(account *Account, h http.Header, mode anthropicFingerprintNormalizeMode) {
	switch mode {
	case anthropicFingerprintClaudeCode:
		canonical := claude.DefaultHeaders()
		for _, key := range anthropicFingerprintStainlessIdentityKeys {
			if _, overridden := account.HeaderOverrideValue(key); overridden || getHeaderRaw(h, key) == "" {
				continue
			}
			value := canonicalHeaderValue(canonical, key)
			if value == "" {
				continue
			}
			deleteHeaderAllForms(h, key)
			setHeaderRaw(h, resolveWireCasing(key), value)
		}
	case anthropicFingerprintCodex:
		for name := range h {
			lower := strings.ToLower(name)
			if !strings.HasPrefix(lower, "x-stainless-") {
				continue
			}
			if _, overridden := account.HeaderOverrideValue(lower); overridden {
				continue
			}
			delete(h, name)
		}
	}
}

// canonicalHeaderValue 按不区分大小写的键名从 claude.DefaultHeaders() 取值。
func canonicalHeaderValue(canonical map[string]string, key string) string {
	for name, value := range canonical {
		if strings.EqualFold(name, key) {
			return value
		}
	}
	return ""
}

// applyFingerprintNormalizeUserAgent 对出站请求做 UA 指纹归一化，三条出站路径共用：
// Anthropic 直通（NormalizeNativeAnthropicRequestHeaders）、chat_completions
// （sendCCUpstreamRequest）、国产供应商原生 Responses（buildUpstreamRequest）。
// target 已由 resolveAnthropicFingerprintTarget 解析。
// 语义：总是写入归一化 UA（请求原本没带 UA 也补上），保证出站 UA 恒定；
// 账号级显式 user-agent 覆写优先（管理员意图优先）。
// 注：只归一 UA，不主动注入 originator（第三方 CN 上游对 originator 无要求，避免污染）。
func applyFingerprintNormalizeUserAgent(account *Account, h http.Header, target anthropicFingerprintNormalizeMode) {
	if h == nil || account == nil || target == anthropicFingerprintOff {
		return
	}
	if _, overridden := account.HeaderOverrideValue("user-agent"); overridden {
		return
	}
	if ua := anthropicFingerprintNormalizedUserAgent(target); ua != "" {
		h.Set("user-agent", ua)
	}
}

// rewriteAnthropicMetadataUserID 把 metadata.user_id 的身份字段改写为账号级
// 恒定值。session_id 保留（会话是自然行为，收敛成常量反而异常）。
// 兼容 JSON 新格式与 legacy 下划线格式（见 metadata_userid.go），解析失败原样返回。
func rewriteAnthropicMetadataUserID(account *Account, body []byte) []byte {
	raw := strings.TrimSpace(gjson.GetBytes(body, "metadata.user_id").String())
	if raw == "" {
		return body
	}
	parsed := ParseMetadataUserID(raw)
	if parsed == nil {
		return body
	}

	deviceID := anthropicFingerprintCanonicalDeviceID(account)
	accountUUID := anthropicFingerprintCanonicalAccountUUID(account)
	if deviceID == "" || accountUUID == "" {
		return body
	}

	var rewritten string
	if parsed.IsNewFormat {
		j := jsonUserID{
			DeviceID:    deviceID,
			AccountUUID: accountUUID,
			SessionID:   parsed.SessionID,
		}
		out, err := json.Marshal(j)
		if err != nil {
			return body
		}
		rewritten = string(out)
	} else {
		// legacy：user_{64hex}_account_{uuid}_session_{uuid}
		rewritten = fmt.Sprintf("user_%s_account_%s_session_%s", deviceID, accountUUID, parsed.SessionID)
	}

	updated, err := sjson.SetBytes(body, "metadata.user_id", rewritten)
	if err != nil {
		return body
	}
	return updated
}

// stripAnthropicBillingHeaderBlocks 删除 system 中纯粹的 billing header 块。
// system 为块数组时，整块删除 text 以 x-anthropic-billing-header: 开头的元素
// （块字节原样保留，不重新序列化）；system 为纯字符串时按行删除。
func stripAnthropicBillingHeaderBlocks(body []byte) []byte {
	sys := gjson.GetBytes(body, "system")
	if !sys.Exists() {
		return body
	}

	if sys.IsArray() {
		var blocks []json.RawMessage
		if err := json.Unmarshal([]byte(sys.Raw), &blocks); err != nil {
			return body
		}
		kept := make([]json.RawMessage, 0, len(blocks))
		for _, b := range blocks {
			if anthropicBlockIsBillingHeader(b) {
				continue
			}
			kept = append(kept, b)
		}
		if len(kept) == len(blocks) {
			return body
		}
		updated, err := sjson.SetBytes(body, "system", kept)
		if err != nil {
			return body
		}
		return updated
	}

	if sys.Type == gjson.String {
		trimmed := anthropicBillingHeaderLineRe.ReplaceAllString(sys.String(), "")
		if trimmed == sys.String() {
			return body
		}
		updated, err := sjson.SetBytes(body, "system", trimmed)
		if err != nil {
			return body
		}
		return updated
	}
	return body
}

// anthropicBlockIsBillingHeader 判断一个 system 块是否纯粹是 billing header。
// 块有两种形态：纯字符串，或 {"type":"text","text":"..."} 对象。
func anthropicBlockIsBillingHeader(block json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(block))
	if trimmed == "" {
		return false
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(block, &s); err != nil {
			return false
		}
		return anthropicBillingHeaderBlockRe.MatchString(s)
	}
	text := gjson.GetBytes(block, "text").String()
	return text != "" && anthropicBillingHeaderBlockRe.MatchString(text)
}
