// 私有扩展（不属于 upstream sub2api）。
//
// 智谱账号指纹归一化的「目标解析」与「客户端准入」：
//   - 入站客户端识别 + 账号三个归一化开关，决定本次请求归一成哪种客户端形态
//     （改写实现见 openai_gateway_messages_anthropic_native_fingerprint.go）；
//   - 准入开关开启时，只允许 Codex / Claude Code / ZCode 三类客户端调用。
//
// @author wangzhong
package service

import (
	"errors"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
)

// 账号级配置（account.Extra），默认全关（opt-in）：
//   - codex：Codex 客户端（codex_cli_rs / codex-tui）→ codex 形态
//   - claudecode：所有 claude-cli/ 客户端（cli / IDE / SDK）→ claudecode 形态
//   - zcode：ZCode 客户端 → zcode 形态
//   - restrict_clients：禁止其他客户端，只允许上述三类客户端调用（与归一化开关互相独立）
const (
	anthropicFingerprintNormalizeCodexExtraKey      = "anthropic_fingerprint_normalize_codex"
	anthropicFingerprintNormalizeClaudeCodeExtraKey = "anthropic_fingerprint_normalize_claudecode"
	anthropicFingerprintNormalizeZCodeExtraKey      = "anthropic_fingerprint_normalize_zcode"
	anthropicFingerprintRestrictClientsExtraKey     = "anthropic_fingerprint_restrict_clients"
)

// anthropicFingerprintRestrictedClientMessage 准入拒绝时返回给客户端的错误信息。
const anthropicFingerprintRestrictedClientMessage = "This account only allows Codex, Claude Code and ZCode clients"

// errAnthropicFingerprintClientRestricted 准入拒绝时返回给调用方的错误（响应已写出）。
var errAnthropicFingerprintClientRestricted = errors.New("anthropic_fingerprint_restrict_clients: only codex, claude code and zcode clients are allowed")

// anthropicFingerprintClient 入站客户端分类。
type anthropicFingerprintClient int

const (
	anthropicFingerprintClientOther anthropicFingerprintClient = iota
	anthropicFingerprintClientCodex
	anthropicFingerprintClientClaudeCode
	anthropicFingerprintClientZCode
)

const (
	claudeCLIUserAgentPrefix = "claude-cli/"
	zcodeUserAgentPrefix     = "zcode/"
)

// anthropicFingerprintSwitches 账号三个归一化开关的快照。
type anthropicFingerprintSwitches struct {
	codex      bool
	claudeCode bool
	zcode      bool
}

// anthropicFingerprintSwitchesOf 读取账号归一化开关；nil 账号全关。
func anthropicFingerprintSwitchesOf(a *Account) anthropicFingerprintSwitches {
	if a == nil {
		return anthropicFingerprintSwitches{}
	}
	return anthropicFingerprintSwitches{
		codex:      anthropicFingerprintExtraFlag(a.Extra, anthropicFingerprintNormalizeCodexExtraKey),
		claudeCode: anthropicFingerprintExtraFlag(a.Extra, anthropicFingerprintNormalizeClaudeCodeExtraKey),
		zcode:      anthropicFingerprintExtraFlag(a.Extra, anthropicFingerprintNormalizeZCodeExtraKey),
	}
}

// AnthropicFingerprintNormalizeCodexEnabled 报告账号是否开启「Codex 客户端归一化」。
func (a *Account) AnthropicFingerprintNormalizeCodexEnabled() bool {
	return anthropicFingerprintSwitchesOf(a).codex
}

// AnthropicFingerprintNormalizeClaudeCodeEnabled 报告账号是否开启「Claude Code 客户端归一化」。
func (a *Account) AnthropicFingerprintNormalizeClaudeCodeEnabled() bool {
	return anthropicFingerprintSwitchesOf(a).claudeCode
}

// classifyAnthropicFingerprintClient 按入站 UA / originator 识别客户端。
// Codex 优先（沿用 openai.IsCodexOfficialClientByHeaders，与 OAuth 路径同一判据）。
func classifyAnthropicFingerprintClient(userAgent, originator string) anthropicFingerprintClient {
	if openai.IsCodexOfficialClientByHeaders(userAgent, originator) {
		return anthropicFingerprintClientCodex
	}
	lower := strings.ToLower(strings.TrimSpace(userAgent))
	switch {
	case strings.HasPrefix(lower, claudeCLIUserAgentPrefix):
		return anthropicFingerprintClientClaudeCode
	case strings.HasPrefix(lower, zcodeUserAgentPrefix):
		return anthropicFingerprintClientZCode
	default:
		return anthropicFingerprintClientOther
	}
}

// classifyInboundAnthropicFingerprintClient 识别 gin 请求的入站客户端；c 为空时视为 other。
func classifyInboundAnthropicFingerprintClient(c *gin.Context) anthropicFingerprintClient {
	originator := ""
	if c != nil && c.Request != nil {
		originator = c.Request.Header.Get("originator")
	}
	return classifyAnthropicFingerprintClient(inboundUserAgent(c), originator)
}

// resolveAnthropicFingerprintTarget 按开关 + 客户端解析最终归一化目标。
// 每类客户端只归一成它自己的身份（UA 与请求体天然自洽）；other 客户端不归一。
func resolveAnthropicFingerprintTarget(sw anthropicFingerprintSwitches, client anthropicFingerprintClient) anthropicFingerprintNormalizeMode {
	switch client {
	case anthropicFingerprintClientCodex:
		return anthropicFingerprintModeIf(sw.codex, anthropicFingerprintCodex)
	case anthropicFingerprintClientClaudeCode:
		return anthropicFingerprintModeIf(sw.claudeCode, anthropicFingerprintClaudeCode)
	case anthropicFingerprintClientZCode:
		return anthropicFingerprintModeIf(sw.zcode, anthropicFingerprintZCode)
	default:
		return anthropicFingerprintOff
	}
}

func anthropicFingerprintModeIf(enabled bool, mode anthropicFingerprintNormalizeMode) anthropicFingerprintNormalizeMode {
	if enabled {
		return mode
	}
	return anthropicFingerprintOff
}

// inboundUserAgent 返回入站请求的原始 User-Agent；c 为空时返回空串。
func inboundUserAgent(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	return c.Request.Header.Get("User-Agent")
}

// anthropicFingerprintAccountInScope 报告账号是否在本功能生效范围内。
//
// 仅智谱（zhipu / GLM）账号生效，其他国产供应商与 OpenCode Go 一律不生效：
//   - 本功能按智谱的实际上游行为设计与验证，其他渠道不在范围内；
//   - OpenCode Go 等上游依赖 applyOpenCodeUpstreamUserAgent 写入的规范 UA 通过
//     Cloudflare 前置拦截，被覆盖会触发 CF 1010/403 并计入账号 403 strike。
func anthropicFingerprintAccountInScope(account *Account) bool {
	return account != nil && account.Platform == PlatformZhipu
}

// accountAnthropicFingerprintTarget 读账号开关 + 识别入站客户端，解析本次请求的归一化目标。
// 所有出站 hook 都经本函数判定，生效范围只在 anthropicFingerprintAccountInScope 控制。
func accountAnthropicFingerprintTarget(account *Account, c *gin.Context) anthropicFingerprintNormalizeMode {
	if !anthropicFingerprintAccountInScope(account) {
		return anthropicFingerprintOff
	}
	return resolveAnthropicFingerprintTarget(anthropicFingerprintSwitchesOf(account), classifyInboundAnthropicFingerprintClient(c))
}

// shouldRejectAnthropicFingerprintClient 报告本次请求是否应被准入开关拒绝：
// 智谱账号开启 restrict_clients，且入站客户端不是 Codex / Claude Code / ZCode。
// 调用方负责按入口协议写 403 错误体（与 codex_cli_only 拒绝同样不做 failover）。
func shouldRejectAnthropicFingerprintClient(account *Account, c *gin.Context) bool {
	if !anthropicFingerprintAccountInScope(account) {
		return false
	}
	if !anthropicFingerprintExtraFlag(account.Extra, anthropicFingerprintRestrictClientsExtraKey) {
		return false
	}
	return classifyInboundAnthropicFingerprintClient(c) == anthropicFingerprintClientOther
}
