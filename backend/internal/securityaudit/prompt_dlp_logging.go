// prompt_dlp_logging.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 的结构化日志。
//
// 为什么不复用 upstream 的 LogInfo/LogWarn：
//
//	upstream 对事件名做白名单校验（prompt_logging.go:62 未注册的事件直接 return），
//	而 knownLogEvents 的条目数被 upstream 测试硬断言
//	（prompt_logging_test.go:36 require.Len(knownLogEvents, 28)），用来守住
//	「事件字典是稳定且经过评审的」这一契约。往里注入 DLP 事件会破坏该契约，
//	且每次 upstream 调整事件数都会再撞一次车。
//
//	所以 DLP 自带一套日志出口：沿用 upstream 完全相同的脱敏纪律
//	（字段白名单 + 字符串截断 + error_code 归一化），但维护自己的字典。
//
// 脱敏纪律（与 upstream 一致，且更严）：
//   - 字段白名单：未列出的字段一律丢弃，避免有人顺手把命中明文塞进日志。
//   - 白名单里刻意不含任何承载敏感明文的字段（命中片段、原文、模型给的理由）。
//     日志会长期留存并可能外送到日志平台，写明文等于扩大泄露面。
//     命中详情走审计事件表（已脱敏），日志只保留可聚合的计数与分类。
//
// 与 upstream 合并策略：
//   - 纯新增文件，不改动 upstream 的任何 var/const，merge 时不会冲突。
//
// =============================================================================
package securityaudit

import (
	"context"
	"log/slog"
	"strings"
)

// DLP 的日志事件名。沿用 upstream 的 "模块.动作" 命名风格，
// 前缀统一为 prompt_dlp 便于按前缀检索与告警配置。
const (
	EventDLPBlocked         = "prompt_dlp.blocked"
	EventDLPFlagged         = "prompt_dlp.flagged"
	EventDLPFalsePositive   = "prompt_dlp.false_positive"
	EventDLPConfirmFailed   = "prompt_dlp.confirm_failed"
	EventDLPConfirmDegraded = "prompt_dlp.confirm_degraded"
	EventDLPRegexExcluded   = "prompt_dlp.regex_excluded"
	EventDLPRecordFailed    = "prompt_dlp.record_failed"
)

// knownDLPLogEvents 是 DLP 的事件字典。新增事件必须在此登记，
// 与 upstream 同样采用"未登记即不输出"的策略，防止日志事件名野生扩散。
var knownDLPLogEvents = map[string]struct{}{
	EventDLPBlocked: {}, EventDLPFlagged: {}, EventDLPFalsePositive: {},
	EventDLPConfirmFailed: {}, EventDLPConfirmDegraded: {},
	EventDLPRegexExcluded: {}, EventDLPRecordFailed: {},
}

// allowedDLPLogFields 是 DLP 日志允许输出的字段。
//
// 前半部分与 upstream 的 allowedLogFields 对齐（请求上下文），
// 后半部分是 DLP 自己的可聚合指标。任何可能含敏感明文的字段都不在此列。
var allowedDLPLogFields = map[string]struct{}{
	// 请求上下文（来自 snapshotLogFields）
	"request_id": {}, "user_id": {}, "api_key_id": {}, "group_id": {},
	"provider": {}, "protocol": {}, "endpoint": {}, "model": {}, "stage": {},
	"config_version": {}, "status": {}, "error_code": {}, "latency_ms": {},
	"risk_level": {}, "action": {}, "decision": {}, "guard_endpoint_id": {},
	// DLP 指标
	"finding_count": {}, "excluded_count": {}, "categories": {},
	"exclude_reasons": {}, "confirmed_count": {}, "cache_hit_count": {},
}

// LogDLPInfo 输出 INFO 级 DLP 日志。
func LogDLPInfo(event string, fields map[string]any) {
	logDLP(slog.LevelInfo, event, fields)
}

// LogDLPWarn 输出 WARN 级 DLP 日志。
func LogDLPWarn(event string, fields map[string]any) {
	logDLP(slog.LevelWarn, event, fields)
}

// logDLP 是 DLP 日志的统一出口，负责事件校验与字段脱敏。
func logDLP(level slog.Level, event string, fields map[string]any) {
	if _, ok := knownDLPLogEvents[event]; !ok {
		return
	}
	slog.LogAttrs(context.Background(), level, event, safeDLPAttrs(fields)...)
}

// safeDLPAttrs 按白名单过滤字段并做脱敏，逻辑与 upstream 的 safeAttrs 保持一致。
func safeDLPAttrs(fields map[string]any) []slog.Attr {
	attrs := make([]slog.Attr, 0, len(fields))
	for key, value := range fields {
		key = strings.TrimSpace(key)
		if _, allowed := allowedDLPLogFields[key]; !allowed {
			continue
		}
		if text, ok := value.(string); ok {
			if key == "error_code" {
				value = stableErrorCode(text)
			} else {
				value = TrimRunes(strings.TrimSpace(text), 256)
			}
		}
		attrs = append(attrs, slog.Any(key, value))
	}
	return attrs
}
