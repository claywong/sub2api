// 私有扩展（不属于 upstream sub2api）。
//
// 智谱账号 restrict_clients 的严格准入：按入站客户端类型复用现有判据，不改动 upstream 逻辑。
//   - Codex：黑名单 → 严格 UA / originator / 白名单 → 版本门 → 引擎指纹门，
//     判据与 codex_cli_only 一致（Detect 的编排因绑定 OpenAI OAuth 账号开关，此处按相同顺序独立编排，
//     只调用 openai 包的公开函数与 CodexRestrictionPolicy 全局策略）；
//   - Claude Code：复用 ClaudeCodeValidator（UA 版本号、system prompt、必需头、metadata.user_id）；
//   - ZCode：无 upstream 判据，保持 UA 前缀识别。
//
// @author wangzhong
package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// 准入拒绝原因（仅用于日志，不返回给客户端，避免向伪装客户端泄露门控细节）。
const (
	fingerprintRejectClaudeCodeValidation = "claude_code_validation_failed"
)

// fingerprintClaudeCodeValidator 复用 upstream 的 Claude Code 校验器（无状态，可共享）。
var fingerprintClaudeCodeValidator = NewClaudeCodeValidator()

// shouldRejectAnthropicFingerprintClient 报告本次请求是否应被准入开关拒绝：
// 智谱账号开启 restrict_clients，且入站客户端未通过对应类型的严格校验。
// body 为入站请求体（Claude Code 的 messages 校验与 Codex 引擎指纹门需要）。
// 调用方负责按入口协议写 403 错误体（与 codex_cli_only 拒绝同样不做 failover）。
func (s *OpenAIGatewayService) shouldRejectAnthropicFingerprintClient(account *Account, c *gin.Context, body []byte) bool {
	if !anthropicFingerprintAccountInScope(account) {
		return false
	}
	if !anthropicFingerprintExtraFlag(account.Extra, anthropicFingerprintRestrictClientsExtraKey) {
		return false
	}
	reason := s.rejectReasonForFingerprintClient(c, body)
	if reason == "" {
		return false
	}
	logger.L().Info("anthropic_fingerprint.client_restricted",
		zap.Int64("account_id", account.ID),
		zap.String("reason", reason),
		zap.String("user_agent", inboundUserAgent(c)),
	)
	return true
}

// rejectReasonForFingerprintClient 按入站客户端类型分派校验；返回空串表示放行。
func (s *OpenAIGatewayService) rejectReasonForFingerprintClient(c *gin.Context, body []byte) string {
	switch classifyInboundAnthropicFingerprintClient(c) {
	case anthropicFingerprintClientZCode:
		return ""
	case anthropicFingerprintClientClaudeCode:
		if validateFingerprintClaudeCode(c, body) {
			return ""
		}
		return fingerprintRejectClaudeCodeValidation
	default:
		// Codex 与 other 都过 Codex 门：other 仍可能被全局白名单 / App Server 开关放行。
		return s.codexGateRejectReason(c, body)
	}
}

// validateFingerprintClaudeCode 复用 ClaudeCodeValidator.Validate。
// 非 messages 路径 Validate 只看 UA，因此仅在 messages 路径才解析请求体。
func validateFingerprintClaudeCode(c *gin.Context, body []byte) bool {
	if c == nil || c.Request == nil {
		return false
	}
	var bodyMap map[string]any
	if strings.Contains(c.Request.URL.Path, "messages") && len(body) > 0 {
		_ = json.Unmarshal(body, &bodyMap)
	}
	return fingerprintClaudeCodeValidator.Validate(c.Request, bodyMap)
}

// codexGateRejectReason 按 codex_cli_only 同一顺序判定；返回空串表示放行。
func (s *OpenAIGatewayService) codexGateRejectReason(c *gin.Context, body []byte) string {
	userAgent, originator, header := inboundCodexIdentity(c)
	policy := s.fingerprintCodexPolicy(c)

	// 黑名单优先（OR：任一已声明字段命中即拒）。
	if openai.MatchDenyEntries(userAgent, originator, policy.Blacklist) {
		return CodexClientRestrictionReasonBlacklisted
	}

	reason, skipFingerprint := resolveFingerprintCodexCandidate(userAgent, originator, policy)
	if reason == "" {
		return CodexClientRestrictionReasonNotMatchedUA
	}

	// 版本门仅对官方候选；白名单 / App Server 候选可能不带可解析引擎版本。
	if reason == CodexClientRestrictionReasonMatchedUA || reason == CodexClientRestrictionReasonMatchedOriginator {
		if denied := fingerprintCodexVersionReject(userAgent, policy); denied != "" {
			return denied
		}
	}

	// 引擎指纹 AND 硬门；命中的白名单条目可显式 skip。
	if !skipFingerprint && !openai.EvaluateEngineFingerprint(header, body, policy.EngineFingerprintSignals) {
		return CodexClientRestrictionReasonMissingEngineFingerprint
	}
	return ""
}

// inboundCodexIdentity 取入站 UA / originator / 全部请求头；c 为空时全部为零值。
func inboundCodexIdentity(c *gin.Context) (userAgent, originator string, header http.Header) {
	if c == nil {
		return "", "", nil
	}
	userAgent = c.GetHeader("User-Agent")
	originator = c.GetHeader("originator")
	if c.Request != nil {
		header = c.Request.Header
	}
	return userAgent, originator, header
}

// fingerprintCodexPolicy 读取 codex_cli_only 全局策略（黑白名单、版本区间、指纹信号）。
// 缺 settingService（仅测试 / 误配可达）时退回默认种子指纹信号，避免指纹门失败开放。
func (s *OpenAIGatewayService) fingerprintCodexPolicy(c *gin.Context) CodexRestrictionPolicy {
	if s == nil || s.settingService == nil {
		return CodexRestrictionPolicy{EngineFingerprintSignals: openai.DefaultEngineFingerprintSignals}
	}
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	return s.settingService.GetCodexRestrictionPolicy(ctx)
}

// resolveFingerprintCodexCandidate 解析身份候选（官方 UA > 官方 originator > 全局白名单 > 全局 App Server 开关）。
// 返回空 reason 表示无候选命中；skipFingerprint 仅白名单条目显式声明时为 true。
func resolveFingerprintCodexCandidate(userAgent, originator string, policy CodexRestrictionPolicy) (reason string, skipFingerprint bool) {
	switch {
	case openai.IsCodexOfficialClientRequestStrict(userAgent):
		return CodexClientRestrictionReasonMatchedUA, false
	case openai.IsCodexOfficialClientOriginator(originator):
		return CodexClientRestrictionReasonMatchedOriginator, false
	}
	if entry, ok := openai.MatchClientEntry(userAgent, originator, policy.Whitelist); ok {
		return CodexClientRestrictionReasonMatchedWhitelistClient, entry.SkipEngineFingerprint
	}
	if policy.AllowAppServerClients {
		return CodexClientRestrictionReasonMatchedAppServerClient, false
	}
	return "", false
}

// fingerprintCodexVersionReject 校验 Codex 引擎版本：必须可解析，且落在 [min, max]。返回空串表示通过。
func fingerprintCodexVersionReject(userAgent string, policy CodexRestrictionPolicy) string {
	ver, ok := openai.ParseCodexEngineVersion(userAgent)
	if !ok {
		return CodexClientRestrictionReasonVersionUndetectable
	}
	if policy.MinCodexVersion != "" && CompareVersions(ver, policy.MinCodexVersion) < 0 {
		return CodexClientRestrictionReasonVersionTooLow
	}
	if policy.MaxCodexVersion != "" && CompareVersions(ver, policy.MaxCodexVersion) > 0 {
		return CodexClientRestrictionReasonVersionTooHigh
	}
	return ""
}
