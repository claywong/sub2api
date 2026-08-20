// coordinator_dlp.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 在 Coordinator 层的独立入口。
//
// 为什么必须放在 Coordinator 层：
//
//	upstream 的 Coordinator.Check 按 EffectiveMode 分发——ModeOff 完全不碰
//	prompt 引擎，ModeAsync 只调 Enqueue，只有 ModeBlocking 才调 Evaluate。
//	DLP 原先挂在 GuardEvaluator.Evaluate 里面，导致它被 qwen3guard 的模式开关
//	绑死：管理员把审计模式设成「关闭」或「异步只审计」时，DLP 即便 Enabled=true
//	也一次都不会执行，且没有任何日志——静默失效。
//
//	DLP 与内容安全是两类独立检测，只想用 DLP 的部署是常态。所以 DLP 的入口上移到
//	模式分发之前，由它自己的 Enabled + 分组范围决定是否执行。
//
// 处置优先级：
//
//	DLP 跑在最前面，判定拦截就立即返回，legacy 与 qwen3guard 都不再执行。取舍是
//	当多个检测器都会拦同一条请求时，客户端看到的是 DLP 的错误码而非 legacy 的。
//	这可以接受——三者的结论都是 403 拦截，差别只在错误码与文案，而先跑 DLP 能省掉
//	一次内容安全的模型调用。
//
//	DLP 未拦截时（未命中 / 判为误报 / 仅审计 / 链路降级）原样走 upstream 的模式
//	分发，行为与改动前完全一致。
//
// 与 upstream 合并策略：
//   - 本文件纯增量。upstream 的 coordinator.go 只加 4 行 hook。
//
// =============================================================================
package securityaudit

import (
	"context"
	"net/http"
)

// DLPEngine 是 Coordinator 依赖的 DLP 能力。
//
// 单独定义接口而不复用 PromptEngine：DLP 的执行条件与审计模式无关，塞进
// PromptEngine 会让 upstream 的接口语义变形，未来 merge 更容易撞车。
type DLPEngine interface {
	// EvaluateDLP 执行 DLP 检测。返回 nil 表示不由 DLP 决定本次请求。
	EvaluateDLP(ctx context.Context, req Request) *PromptDecision
}

// WithDLP 给 Coordinator 装上 DLP 引擎。返回 c 便于链式调用。
//
// 用 setter 而不改 NewCoordinator 签名：upstream 构造函数一旦改签名，
// 未来 merge 必然冲突（CLAUDE.md 的 inline 最小化原则）。
func (c *Coordinator) WithDLP(engine DLPEngine) *Coordinator {
	if c == nil {
		return nil
	}
	c.dlp = engine
	return c
}

// ProvideCoordinator 是给 wire 用的构造函数，替代 ProviderSet 里的 NewCoordinator。
//
// 之所以不直接在 wire_gen.go 里手写 .WithDLP(...)：wire_gen.go 是生成产物，
// 任何人重跑 wire 都会把手改冲掉，DLP 就静默失去装配。放在 provider 里能保证
// 重新生成后依然正确。
func ProvideCoordinator(legacy LegacyEngine, prompt PromptEngine, dlp DLPEngine) *Coordinator {
	return NewCoordinator(legacy, prompt).WithDLP(dlp)
}

// checkDLP 执行 DLP 前置检测，返回非 nil 表示应当立即以该决策作答。
//
// 只在 DLP 判定为拦截时返回决策；flag（仅审计）和放行都返回 nil，让请求继续
// 走 upstream 的模式分发，这样 DLP 的审计不会挡住内容安全检测。
func (c *Coordinator) checkDLP(ctx context.Context, req Request) *Decision {
	if c == nil || c.dlp == nil {
		return nil
	}
	prompt := c.dlp.EvaluateDLP(ctx, req)
	if prompt == nil || prompt.Kind != DecisionBlock {
		return nil
	}
	errorCode, clientMessage := blockedErrorCodeAndMessage(prompt)
	return &Decision{
		Kind: DecisionBlock, HTTPStatus: http.StatusForbidden, ErrorCode: errorCode,
		ClientMessage: clientMessage, Prompt: prompt, AllowNextStage: false,
	}
}
