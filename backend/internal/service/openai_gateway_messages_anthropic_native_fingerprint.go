// 私有扩展（不属于 upstream sub2api）。
//
// 本文件为 CN 供应商 Anthropic 协议直通路径（openai_gateway_messages_anthropic_native.go）
// 提供出站指纹归一化能力，对应分组开关 groups.anthropic_fingerprint_normalize_enabled
// （migration 907）。开启后对出站请求做三类归一，让同一上游账号的所有
// 拼车用户在供应商侧呈现为「同一个人的同一台机器」：
//  1. metadata.user_id 的 device_id/account_uuid 改写为账号级恒定值（session_id 保留）
//  2. 删除 body.system 中 Claude Code 注入的 x-anthropic-billing-header 块
//  3. User-Agent 归一为规范 claude-cli 值，并兜底剥离 billing header 头
//
// 所含符号：
//   - SetAnthropicFingerprintNormalize / anthropicFingerprintNormalizeEnabled
//   - NormalizeNativeAnthropicRequestBody / NormalizeNativeAnthropicRequestHeaders
//
// merge 策略：upstream 不含本文件；openai_gateway_messages_anthropic_native.go
// 与 openai_gateway_handler.go 中仅有 2 处各 2 行的调用 hook，merge 时保留即可。
package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// anthropicFingerprintNormalizeCtxKey 是 gin context 中传递分组开关的 key。
// service 转发层拿不到 Group 实体（分组-账号多对多），由 handler 层在调用
// ForwardAsAnthropic 前注入。
const anthropicFingerprintNormalizeCtxKey = "sub2api_anthropic_fingerprint_normalize"

// anthropicFingerprintNormalizedUserAgent 归一后的出站 User-Agent。
// 版本号取 @anthropic-ai/claude-code npm 最新版（2026-08 为 2.1.237），
// 所有客户端统一为同一形态，消除不同客户端/SDK 的 UA 差异。
// 跟新版本：npm view @anthropic-ai/claude-code version
const anthropicFingerprintNormalizedUserAgent = "claude-cli/2.1.237"

// anthropicBillingHeaderBlockRe 匹配 system prompt 中 Claude Code 注入的
// billing header 块（块内容以 x-anthropic-billing-header: 开头）。
var anthropicBillingHeaderBlockRe = regexp.MustCompile(`^\s*x-anthropic-billing-header:`)

// anthropicBillingHeaderLineRe 匹配字符串形态 system 中内联的 billing header 行。
var anthropicBillingHeaderLineRe = regexp.MustCompile(`(?m)^x-anthropic-billing-header:[^\n]*\n?`)

// SetAnthropicFingerprintNormalize 由 handler 层调用，注入分组开关状态。
func SetAnthropicFingerprintNormalize(c *gin.Context, enabled bool) {
	if c == nil {
		return
	}
	c.Set(anthropicFingerprintNormalizeCtxKey, enabled)
}

// anthropicFingerprintNormalizeEnabled 读取分组开关；c 为空或未注入时视为关闭。
func anthropicFingerprintNormalizeEnabled(c *gin.Context) bool {
	if c == nil {
		return false
	}
	v, ok := c.Get(anthropicFingerprintNormalizeCtxKey)
	if !ok {
		return false
	}
	enabled, _ := v.(bool)
	return enabled
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
// 任何一步失败都原样返回，绝不阻断转发。
func NormalizeNativeAnthropicRequestBody(account *Account, body []byte) []byte {
	if account == nil || len(body) == 0 {
		return body
	}
	body = rewriteAnthropicMetadataUserID(account, body)
	body = stripAnthropicBillingHeaderBlocks(body)
	return body
}

// NormalizeNativeAnthropicRequestHeaders 对直通出站 headers 做指纹归一化：
// User-Agent 归一 + 兜底剥离 billing header 头（该头不在 allowedHeaders 白名单，
// 正常路径本就不会透传，此处防御账号级 HeaderOverride 显式注入的情况）。
// 账号级显式配置的 user-agent 覆写优先于归一化默认值（管理员意图优先）。
func NormalizeNativeAnthropicRequestHeaders(account *Account, h http.Header) {
	if h == nil {
		return
	}
	if account == nil {
		h.Del("x-anthropic-billing-header")
		return
	}
	if _, overridden := account.HeaderOverrideValue("user-agent"); !overridden {
		if ua := h.Get("User-Agent"); ua != "" && ua != anthropicFingerprintNormalizedUserAgent {
			h.Set("User-Agent", anthropicFingerprintNormalizedUserAgent)
		}
	}
	h.Del("x-anthropic-billing-header")
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
