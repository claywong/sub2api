// 私有扩展（不属于 upstream sub2api）。
//
// 本文件为智谱（zhipu / GLM）账号的出站请求提供指纹归一化能力，让同一上游账号的
// 拼车用户在供应商侧按客户端类型各自收敛为「同一个客户端」。
//
// 配置：账号级三个归一化开关（codex / claudecode / zcode）+ 一个客户端准入开关
// （restrict_clients），默认关闭。目标解析与准入判定见
// openai_gateway_messages_anthropic_native_fingerprint_target.go，生效范围仅智谱。
//
// 出站 hook（均经 accountAnthropicFingerprintTarget 判定）：
//   - Anthropic 直通（openai_gateway_messages_anthropic_native.go）：body 改写
//     metadata.user_id 身份字段、剥 billing header 块；header 归一 UA、x-stainless-*、originator
//   - chat_completions（openai_gateway_cc_pipeline.go::sendCCUpstreamRequest）：归一 UA、originator
//
// 旧的分组级开关 groups.fingerprint_normalize_enabled 已不再读写，列暂保留以保证回滚安全。
//
// merge 策略：upstream 不含本文件；上述两个文件中的调用 hook，merge 时保留即可。
//
// @author wangzhong
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
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// anthropicFingerprintNormalizeMode 是一次请求解析后的**最终归一化目标形态**
// （不是账号静态配置值），由 resolveAnthropicFingerprintTarget 解析。
type anthropicFingerprintNormalizeMode string

const (
	// anthropicFingerprintOff 不做归一化，原样透传客户端指纹。
	anthropicFingerprintOff anthropicFingerprintNormalizeMode = "off"
	// anthropicFingerprintClaudeCode 归一成 Claude Code（claude-cli）形态：
	// UA 版本段统一为 EffectiveCLIVersion（入口段保留），身份字段账号级恒定，删 billing header。
	anthropicFingerprintClaudeCode anthropicFingerprintNormalizeMode = "claudecode"
	// anthropicFingerprintCodex 归一成 Codex 形态：UA + originator 取 OAuth 同源的
	// 规范身份（resolveCodexOutboundIdentity），身份字段账号级恒定。
	anthropicFingerprintCodex anthropicFingerprintNormalizeMode = "codex"
	// anthropicFingerprintZCode 归一成 ZCode 形态：UA 的 ZCode 版本与 node 运行时统一为
	// zcodeCanonicalVersion / zcodeCanonicalNodeRuntime，其余段保留。
	anthropicFingerprintZCode anthropicFingerprintNormalizeMode = "zcode"
)

const (
	// zcodeCanonicalVersion ZCode 规范版本。ZCode 无公开版本源，无法自动同步，
	// 取线上智谱流量最新观测版本（2026-09），升级时改此常量。
	zcodeCanonicalVersion = "3.14.3"
	// zcodeCanonicalNodeRuntime ZCode 规范 node 运行时主版本。
	zcodeCanonicalNodeRuntime = "24"
)

var (
	// claudeCLIUAVersionRe 匹配 claude-cli UA 首段版本号（含预发布后缀）。
	claudeCLIUAVersionRe = regexp.MustCompile(`(?i)^claude-cli/[0-9][^\s]*`)
	// zcodeUAVersionRe 匹配 ZCode UA 首段版本号。
	zcodeUAVersionRe = regexp.MustCompile(`(?i)^zcode/[0-9][^\s]*`)
	// zcodeUANodeRuntimeRe 匹配 ZCode UA 中的 node 运行时段。
	zcodeUANodeRuntimeRe = regexp.MustCompile(`runtime/node\.js/[0-9][^\s]*`)
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

// anthropicFingerprintNormalizedUserAgent 按目标形态由入站 UA 计算出站 UA：
//   - claudecode：入站是 claude-cli/ 时只替换版本段为 EffectiveCLIVersion，保留入口段
//     （cli / claude-vscode / sdk-ts ...，与各入口不同的请求体保持自洽）；
//     入站 UA 缺失或无法解析时兜底为规范 claude.DefaultUserAgent()。
//   - codex：OAuth 同源的规范 Codex UA（面板 → 自动同步 → 内置）。
//   - zcode：替换 ZCode 版本段与 node 运行时段，其余段保留；入站非 ZCode 时返回空。
//
// 无法解析时返回空串，调用方保持原 UA 不动。
func anthropicFingerprintNormalizedUserAgent(mode anthropicFingerprintNormalizeMode, inboundUA string) string {
	inboundUA = strings.TrimSpace(inboundUA)
	switch mode {
	case anthropicFingerprintClaudeCode:
		if claudeCLIUAVersionRe.MatchString(inboundUA) {
			return claudeCLIUAVersionRe.ReplaceAllLiteralString(inboundUA, claudeCLIUserAgentProduct+"/"+claude.EffectiveCLIVersion())
		}
		return claude.DefaultUserAgent()
	case anthropicFingerprintCodex:
		return resolveCodexOutboundIdentity("").userAgent
	case anthropicFingerprintZCode:
		if !zcodeUAVersionRe.MatchString(inboundUA) {
			return ""
		}
		ua := zcodeUAVersionRe.ReplaceAllLiteralString(inboundUA, "ZCode/"+zcodeCanonicalVersion)
		return zcodeUANodeRuntimeRe.ReplaceAllLiteralString(ua, "runtime/node.js/"+zcodeCanonicalNodeRuntime)
	default:
		return ""
	}
}

// applyFingerprintNormalizeIdentityHeaders 写入归一化 UA；codex 形态同时写入配套
// originator（与 OAuth 出站同源，UA 首段与 originator 必须成对）。
// 账号级显式覆写的 user-agent / originator 保留管理员值。
// 先删全部大小写形态再写入：直通构造器按客户端原始大小写透传，不清理会残留旧值。
func applyFingerprintNormalizeIdentityHeaders(account *Account, h http.Header, mode anthropicFingerprintNormalizeMode, inboundUA string) {
	if _, overridden := account.HeaderOverrideValue("user-agent"); !overridden {
		if ua := anthropicFingerprintNormalizedUserAgent(mode, inboundUA); ua != "" {
			deleteHeaderAllForms(h, "user-agent")
			setHeaderRaw(h, resolveWireCasing("user-agent"), ua)
		}
	}
	if mode != anthropicFingerprintCodex {
		return
	}
	if _, overridden := account.HeaderOverrideValue("originator"); overridden {
		return
	}
	if originator := resolveCodexOutboundIdentity("").originator; originator != "" {
		deleteHeaderAllForms(h, "originator")
		setHeaderRaw(h, resolveWireCasing("originator"), originator)
	}
}

// NormalizeNativeAnthropicRequestHeaders 对直通出站 headers 做指纹归一化：
// 按 mode 归一 UA / originator / x-stainless-*，并兜底剥离 billing header 头（该头不在
// allowedHeaders 白名单，正常路径本就不会透传，此处防御账号级 HeaderOverride 显式注入）。
// inboundUA 为客户端原始 UA，用于保留入口段的版本替换。
func NormalizeNativeAnthropicRequestHeaders(account *Account, h http.Header, mode anthropicFingerprintNormalizeMode, inboundUA string) {
	if h == nil || mode == anthropicFingerprintOff {
		return
	}
	if account == nil {
		deleteHeaderAllForms(h, "x-anthropic-billing-header")
		return
	}
	applyFingerprintNormalizeIdentityHeaders(account, h, mode, inboundUA)
	normalizeStainlessHeaders(account, h, mode, inboundUA)
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
//   - claudecode：请求中已有的身份键改写为 claudeCodeCanonicalStainlessHeaders() 规范值
//     （只改不增，与 IdentityService.ApplyFingerprint 语义一致）；改写前先从当前请求学习
//     与生效 CLI 版本配对的 SDK / 运行时版本，使规范值跟随版本同步；
//   - codex / zcode：删除全部 x-stainless-*，这两类真实客户端不发送这组头，留着会暴露身份。
//
// 账号级显式覆写的键保留管理员值。anthropic-beta 不在此处理：它与请求体能力字段联动
// （sanitizeAnthropicBodyForBetaTokens），统一会导致上游 400。
func normalizeStainlessHeaders(account *Account, h http.Header, mode anthropicFingerprintNormalizeMode, inboundUA string) {
	switch mode {
	case anthropicFingerprintClaudeCode:
		learnClaudeCodeStainlessVersions(inboundUA, h)
		canonical := claudeCodeCanonicalStainlessHeaders()
		for _, key := range anthropicFingerprintStainlessIdentityKeys {
			if _, overridden := account.HeaderOverrideValue(key); overridden || getHeaderRaw(h, key) == "" {
				continue
			}
			value := canonical[key]
			if value == "" {
				continue
			}
			deleteHeaderAllForms(h, key)
			setHeaderRaw(h, resolveWireCasing(key), value)
		}
	case anthropicFingerprintCodex, anthropicFingerprintZCode:
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

// applyFingerprintNormalizeUserAgent 对 chat_completions 出站请求做身份头归一化
// （sendCCUpstreamRequest 调用；Anthropic 直通在 NormalizeNativeAnthropicRequestHeaders 处理）。
// 语义：总是写入归一化 UA（请求原本没带 UA 也补上），codex 形态配套写 originator；
// 账号级显式覆写优先（管理员意图优先）。
//
// 设备标识无需额外收敛：CC 请求体由结构体重建（不含 client_metadata / prompt_cache_key），
// 出站头只透传 openaiCCRawAllowedHeaders 白名单，installation / session / thread 等
// Codex 设备标识在转换时已全部丢弃，上游不可见。
func applyFingerprintNormalizeUserAgent(account *Account, h http.Header, target anthropicFingerprintNormalizeMode, inboundUA string) {
	if h == nil || account == nil || target == anthropicFingerprintOff {
		return
	}
	// CC 构造器使用 Go 规范大小写（h.Set），与其后的 ApplyHeaderOverrides 一致。
	if _, overridden := account.HeaderOverrideValue("user-agent"); !overridden {
		if ua := anthropicFingerprintNormalizedUserAgent(target, inboundUA); ua != "" {
			h.Set("user-agent", ua)
		}
	}
	if target != anthropicFingerprintCodex {
		return
	}
	if _, overridden := account.HeaderOverrideValue("originator"); !overridden {
		if originator := resolveCodexOutboundIdentity("").originator; originator != "" {
			h.Set("originator", originator)
		}
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
