// prompt_service_dlp.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：PromptService 上的独立 DLP 评估入口。
//
// 与 upstream Evaluate 的门控差异（这是本文件存在的全部理由）：
//
//	                       upstream Evaluate        本文件 EvaluateDLP
//	审计模式               必须 ModeBlocking        不看，DLP 有自己的开关
//	prompt 审计总开关      必须 Enabled             不看
//	分组范围               cfg.IncludesGroup        cfg.DLP.IncludesGroup（独立）
//	扫描范围               受 BlockingLatestTurnOnly 恒全量，不受收窄影响
//	配置不可用             ModeBlocking 时报错      恒 fail-open 放行
//
// 为什么扫描范围恒全量：BlockingLatestTurnOnly 是 qwen3guard 为了省 token 才收窄到
// 最后一轮的。敏感信息可能出现在任何一轮的输入、工具结果或工具入参里，收窄会直接
// 造成漏检；而 DLP 的正则是本地执行，扫全量的成本是 μs 级，没有省的必要。
//
// 为什么恒 fail-open：与 prompt_guard_dlp.go 的降级策略一致。配置读不到时放行而
// 不是报 503，避免 DLP 把网关整体拖挂。
//
// 与 upstream 合并策略：
//   - 纯新增文件，不改 upstream 的 Evaluate，merge 时不会冲突。
//
// =============================================================================
package securityaudit

import (
	"context"
	"errors"
)

// EvaluateDLP 执行 DLP 检测，实现 coordinator_dlp.go 的 DLPEngine 接口。
//
// 返回 nil 表示不由 DLP 决定本次请求（未启用 / 不在范围 / 未命中 / 判为误报 /
// 仅审计 / 链路降级），调用方应继续原有流程。
//
// 不返回 error：DLP 全程 fail-open，任何异常都放行并由 prompt_guard_dlp.go 记日志，
// 给调用方 error 只会诱导它做出 fail-closed 的处置。
func (s *PromptService) EvaluateDLP(ctx context.Context, req Request) *PromptDecision {
	if s == nil || s.config == nil || s.evaluator == nil {
		return nil
	}
	cfg, ok := s.config.Active()
	if !ok {
		// 配置尚未加载或加载失败：放行。DLP 宁可漏检也不能挡住网关。
		return nil
	}
	if !cfg.DLP.Enabled {
		return nil
	}
	if !cfg.DLP.IncludesGroup(req.GroupID) {
		return nil
	}
	// 恒传 false：DLP 必须看全量对话，理由见文件头。
	snapshot, err := ExtractBlockingPromptSnapshot(req, false)
	if errors.Is(err, ErrNoPromptText) {
		return nil
	}
	if err != nil {
		// 请求体不是合法 JSON 之类的提取失败：放行，交给 upstream 流程去报错。
		return nil
	}
	return s.evaluator.EvaluateDLP(ctx, cfg, snapshot)
}
