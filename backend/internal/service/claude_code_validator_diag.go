// claude_code_validator_diag.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：Claude Code 客户端判定失败诊断。
//
// 背景：
//   分组开启 claude_code_only 后，非 Claude Code 客户端会在路由阶段被
//   resolveGatewayGroup 拒绝并返回 ErrClaudeCodeOnly，但 upstream 的判定
//   逻辑（ClaudeCodeValidator.Validate）只返回 bool，不暴露"为什么不通过"。
//   运维拿到 "this group only allows Claude Code clients" 时无法定位根因
//   （UA 不对？metadata 格式不对？缺 header？）。
//
// 本文件提供：
//   - DiagnoseClaudeCodeReject：只读复刻 Validate 的检查顺序，返回失败原因码。
//     仅在判定为 false 的冷路径上调用，不影响正常请求性能。
//   - 一个 service 包内私有 ctx key + set/get，用于把诊断结论从 handler
//     （SetClaudeCodeClientContext）携带到 service（resolveGatewayGroup）。
//   - LogClaudeCodeReject：在真正返回 ErrClaudeCodeOnly 时打一条 warn 日志。
//
// 与 upstream 合并策略：
//   - 纯增量：新函数、新类型、新常量，不改动 upstream 已有方法。
//   - handler/gateway_helper.go 与 gateway_service.go 各加 1 行 hook 调用本文件。
// =============================================================================
package service

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
)

// Claude Code 判定失败原因码（用于日志排障，稳定不随提示词改版漂移）。
const (
	CCRejectUAMismatch       = "ua_mismatch"        // User-Agent 不是 claude-cli/x.y.z
	CCRejectSystemPrompt     = "system_prompt_miss" // messages 路径 system 未命中 Claude Code 提示词/计费块
	CCRejectMissingXApp      = "missing_x_app"      // 缺 X-App header
	CCRejectMissingBeta      = "missing_anthropic_beta"
	CCRejectMissingVersion   = "missing_anthropic_version"
	CCRejectMetadataMissing  = "metadata_user_id_missing" // body.metadata.user_id 缺失/空
	CCRejectMetadataBadShape = "metadata_user_id_bad_format"
	CCRejectUnknown          = "unknown"
)

// ccRejectCtxKey 是本文件私有的 ctx key 类型，避免污染 ctxkey 包（减少与 upstream 冲突）。
type ccRejectCtxKey struct{}

// ccRejectInfo 携带诊断结论，从 handler 流转到 service 拒绝点。
type ccRejectInfo struct {
	Reason    string
	UserAgent string
	Path      string
}

// WithClaudeCodeRejectInfo 把判定失败诊断写入 ctx，供后续拒绝点取出打日志。
func WithClaudeCodeRejectInfo(ctx context.Context, reason, userAgent, path string) context.Context {
	return context.WithValue(ctx, ccRejectCtxKey{}, ccRejectInfo{
		Reason:    reason,
		UserAgent: userAgent,
		Path:      path,
	})
}

// claudeCodeRejectInfo 从 ctx 取出诊断结论。
func claudeCodeRejectInfo(ctx context.Context) (ccRejectInfo, bool) {
	v, ok := ctx.Value(ccRejectCtxKey{}).(ccRejectInfo)
	return v, ok
}

// DiagnoseClaudeCodeReject 只读复刻 ClaudeCodeValidator.Validate 的检查顺序，
// 返回首个未通过的检查对应的原因码。仅在已判定为非 Claude Code 客户端时调用。
//
// body 可为 nil（fast-path：UA 都没过就不会解析 body）。
func DiagnoseClaudeCodeReject(r *http.Request, body map[string]any) string {
	if r == nil {
		return CCRejectUnknown
	}
	ua := r.Header.Get("User-Agent")
	if !claudeCodeUAPattern.MatchString(ua) {
		return CCRejectUAMismatch
	}

	path := r.URL.Path
	// 非 messages 路径 / count_tokens：UA 命中即通过，不会走到拒绝路径。
	if !strings.Contains(path, "messages") || isMessagesCountTokensPath(path) {
		return CCRejectUnknown
	}
	// haiku 探测请求绕过：同样不会被拒。
	if isHaiku, ok := IsMaxTokensOneHaikuRequestFromContext(r.Context()); ok && isHaiku {
		return CCRejectUnknown
	}

	// messages 路径严格校验，逐项复刻 Validate 的判定顺序。
	v := ClaudeCodeValidator{}
	if !v.hasClaudeCodeSystemPrompt(body) {
		return CCRejectSystemPrompt
	}
	if r.Header.Get("X-App") == "" {
		return CCRejectMissingXApp
	}
	if r.Header.Get("anthropic-beta") == "" {
		return CCRejectMissingBeta
	}
	if r.Header.Get("anthropic-version") == "" {
		return CCRejectMissingVersion
	}
	metadata, ok := body["metadata"].(map[string]any)
	if !ok {
		return CCRejectMetadataMissing
	}
	userID, ok := metadata["user_id"].(string)
	if !ok || userID == "" {
		return CCRejectMetadataMissing
	}
	if ParseMetadataUserID(userID) == nil {
		return CCRejectMetadataBadShape
	}
	return CCRejectUnknown
}

// LogClaudeCodeReject 在分组因 claude_code_only 真正拒绝请求时打一条 warn 日志，
// 带上诊断原因码。只在无降级分组、最终返回 ErrClaudeCodeOnly 时调用，无噪音。
func LogClaudeCodeReject(ctx context.Context, group *Group) {
	info, ok := claudeCodeRejectInfo(ctx)
	reason := CCRejectUnknown
	ua, path := "", ""
	if ok {
		reason, ua, path = info.Reason, info.UserAgent, info.Path
	}
	attrs := []any{
		slog.String("reason", reason),
		slog.String("user_agent", ua),
		slog.String("path", path),
	}
	if group != nil {
		attrs = append(attrs, slog.Int64("group_id", group.ID), slog.String("group_name", group.Name))
	}
	slog.Warn("gateway.claude_code_only_rejected", attrs...)
}
