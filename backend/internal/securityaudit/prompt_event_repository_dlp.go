// prompt_event_repository_dlp.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：按检测来源筛选审计事件。
//
// 背景：DLP 与 qwen3guard 共用 prompt_audit_events 表，只靠 scanner_backend 列
// 区分（DLP 写 "dlp-regex+llm"，qwen3guard 写 "qwen3guard-openai"）。两者拆成
// 独立管理页面后，各自的事件列表必须只看自己的事件，否则管理员在 DLP 页面会看到
// 内容安全的拦截记录，无法判断到底是哪套策略生效。
//
// 为什么对外暴露 source 而不是 scanner_backend：
//   - scanner_backend 是实现细节（含模型名/协议），换确认模型就会变值；
//     前端固定传 source=dlp / source=guard，映射关系收敛在本文件一处。
//   - guard 侧用「排除 DLP」而非「等于 qwen3guard-openai」来匹配：upstream 未来
//     新增别的内容安全后端时，无需再改这里就能落到 guard 分组。
//
// 所含内容：
//   - EventSource* 常量与 ResolveEventScannerBackends：source → SQL 匹配条件
//   - canonicalScannerBackends：归一化，保证 FilterHash 稳定
//   - scannerBackendClause：生成 WHERE 片段
//
// 与 upstream 合并策略：
//   - 本文件纯增量。upstream 侧仅 3 处极小改动：
//     EventFilter 加 1 个字段、canonicalEventFilter 加 1 行、buildEventWhere 加 1 行。
//
// =============================================================================
package securityaudit

import (
	"fmt"
	"sort"
	"strings"
)

// 事件来源标识，对应管理 API 的 ?source= 取值。
const (
	// EventSourceDLP 只看数据防泄漏事件。
	EventSourceDLP = "dlp"
	// EventSourceGuard 只看内容安全（qwen3guard）事件。
	EventSourceGuard = "guard"
)

// ResolveEventScannerBackends 把 source 转成 scanner_backend 匹配条件。
//
// 返回值语义：
//   - backends 非空、negate=false → scanner_backend IN (backends)
//   - backends 非空、negate=true  → scanner_backend NOT IN (backends)
//   - backends 为空               → 不加条件（source 为空或取值无法识别）
//
// 无法识别的 source 刻意不报错而是退化成「不过滤」：这是个筛选维度而非权限边界，
// 拼错参数返回全量比返回 400 更不容易把管理页面搞白屏。
func ResolveEventScannerBackends(source string) (backends []string, negate bool) {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case EventSourceDLP:
		return []string{DLPScannerBackend}, false
	case EventSourceGuard:
		// 排除法：DLP 之外的都算内容安全，upstream 新增后端时无需改这里。
		return []string{DLPScannerBackend}, true
	default:
		return nil, false
	}
}

// canonicalScannerBackends 归一化 backend 列表：去空、去重、排序。
//
// 必须排序去重：FilterHash 直接对 EventFilter 做 JSON 序列化，顺序不稳定会让
// 同一筛选条件算出不同的 hash，删除确认 token 就会校验失败。
func canonicalScannerBackends(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	if len(result) == 0 {
		return nil
	}
	sort.Strings(result)
	return result
}

// scannerBackendClause 生成 scanner_backend 的 WHERE 片段与绑定参数。
//
// firstIndex 是下一个可用的占位符序号（$N 从 1 开始）。返回空串表示无需加条件。
// 用逐个占位符而不是 pq.Array：与 buildEventWhere 里其余条件的写法保持一致，
// 且列表长度固定为 1，不存在参数膨胀问题。
func scannerBackendClause(backends []string, negate bool, firstIndex int) (string, []any) {
	canonical := canonicalScannerBackends(backends)
	if len(canonical) == 0 {
		return "", nil
	}
	placeholders := make([]string, 0, len(canonical))
	args := make([]any, 0, len(canonical))
	for index, backend := range canonical {
		placeholders = append(placeholders, fmt.Sprintf("$%d", firstIndex+index))
		args = append(args, backend)
	}
	operator := "IN"
	if negate {
		operator = "NOT IN"
	}
	// NOT IN 对 NULL 会整体判 NULL 从而漏掉行，历史数据里 scanner_backend 可能为空，
	// 因此显式补一个 IS NULL 分支，保证「排除 DLP」能把旧事件也算进 guard。
	if negate {
		return fmt.Sprintf(" AND (e.scanner_backend %s (%s) OR e.scanner_backend IS NULL)",
			operator, strings.Join(placeholders, ",")), args
	}
	return fmt.Sprintf(" AND e.scanner_backend %s (%s)", operator, strings.Join(placeholders, ",")), args
}
