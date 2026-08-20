package securityaudit

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeDLPEngine 记录 DLP 是否被调用，并按需返回拦截决策。
type fakeDLPEngine struct {
	decision *PromptDecision
	calls    atomic.Int64
}

func (f *fakeDLPEngine) EvaluateDLP(context.Context, Request) *PromptDecision {
	f.calls.Add(1)
	return f.decision
}

func dlpBlockDecision() *PromptDecision {
	return &PromptDecision{
		Kind: DecisionBlock, ErrorCode: ErrorCodeDLPBlocked,
		Result: &NormalizedResult{Decision: EventCritical, RiskLevel: RiskHigh, Action: ActionBlock},
	}
}

// 这是本次改动要防的回归：DLP 曾挂在 GuardEvaluator.Evaluate 内部，而 upstream 的
// Coordinator 只在 ModeBlocking 下才调 Evaluate，导致「关闭」与「异步只审计」两种
// 模式下 DLP 即便启用也一次都不执行，且没有任何日志。
func TestCoordinatorDLPRunsInEveryAuditMode(t *testing.T) {
	for _, mode := range []Mode{ModeOff, ModeAsync, ModeBlocking} {
		t.Run(string(mode), func(t *testing.T) {
			legacy := &fakeLegacyEngine{}
			prompt := &fakePromptEngine{mode: mode}
			dlp := &fakeDLPEngine{decision: dlpBlockDecision()}

			decision := ProvideCoordinator(legacy, prompt, dlp).
				Check(context.Background(), Request{Body: []byte(`{}`)})

			require.Equal(t, int64(1), dlp.calls.Load(), "DLP 必须在每种审计模式下都执行")
			require.Equal(t, DecisionBlock, decision.Kind)
			require.Equal(t, ErrorCodeDLPBlocked, decision.ErrorCode)
			require.Equal(t, DLPClientMessage, decision.ClientMessage)
			require.False(t, decision.AllowNextStage)
		})
	}
}

func TestCoordinatorDLPBlockShortCircuitsPromptEngine(t *testing.T) {
	// DLP 拦截后不该再跑内容安全，省一次模型调用。
	legacy := &fakeLegacyEngine{}
	prompt := &fakePromptEngine{mode: ModeBlocking}
	dlp := &fakeDLPEngine{decision: dlpBlockDecision()}

	decision := ProvideCoordinator(legacy, prompt, dlp).
		Check(context.Background(), Request{Body: []byte(`{}`)})

	require.Equal(t, DecisionBlock, decision.Kind)
	require.Equal(t, int64(0), prompt.evaluates.Load(), "DLP 拦截应短路 qwen3guard")
	require.Equal(t, int64(0), prompt.enqueues.Load())
	require.Equal(t, int64(0), legacy.calls.Load(), "DLP 拦截应短路 legacy 引擎")
}

func TestCoordinatorDLPAllowFallsThroughToUpstreamFlow(t *testing.T) {
	// DLP 未拦截（未命中/误报/仅审计/降级）时，upstream 流程必须原样执行。
	// blocking 模式下 prompt 引擎必须给出明确结论：upstream 把 nil 视为不可用。
	promptAllow := &PromptDecision{Kind: DecisionAllow, AllowNextStage: true}
	tests := []struct {
		name          string
		dlpDecision   *PromptDecision
		promptDecison *PromptDecision
		mode          Mode
		wantEnqueue   int64
		wantEvaluate  int64
	}{
		{name: "nil decision blocking", mode: ModeBlocking, promptDecison: promptAllow, wantEvaluate: 1},
		{name: "nil decision async", mode: ModeAsync, wantEnqueue: 1},
		{
			name: "flag only does not block", mode: ModeBlocking,
			dlpDecision:   &PromptDecision{Kind: DecisionFlag, AllowNextStage: true},
			promptDecison: promptAllow,
			wantEvaluate:  1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			legacy := &fakeLegacyEngine{}
			prompt := &fakePromptEngine{mode: tt.mode, decision: tt.promptDecison}
			dlp := &fakeDLPEngine{decision: tt.dlpDecision}

			decision := ProvideCoordinator(legacy, prompt, dlp).
				Check(context.Background(), Request{Body: []byte(`{}`)})

			require.Equal(t, DecisionAllow, decision.Kind)
			require.Equal(t, int64(1), legacy.calls.Load())
			require.Equal(t, tt.wantEnqueue, prompt.enqueues.Load())
			require.Equal(t, tt.wantEvaluate, prompt.evaluates.Load())
		})
	}
}

func TestCoordinatorWithoutDLPBehavesLikeUpstream(t *testing.T) {
	// 没装 DLP 引擎时行为必须与 upstream 完全一致。
	legacy := &fakeLegacyEngine{}
	prompt := &fakePromptEngine{mode: ModeBlocking, decision: &PromptDecision{Kind: DecisionBlock}}

	decision := NewCoordinator(legacy, prompt).Check(context.Background(), Request{Body: []byte(`{}`)})

	require.Equal(t, DecisionBlock, decision.Kind)
	require.Equal(t, ErrorCodeBlocked, decision.ErrorCode, "非 DLP 拦截应沿用 upstream 错误码")
	require.Equal(t, int64(1), prompt.evaluates.Load())
}

func TestCoordinatorLegacyBlockStillWinsWhenDLPAllows(t *testing.T) {
	// DLP 放行时 legacy 的既有阻断行为不能被削弱。
	legacy := &fakeLegacyEngine{decision: &LegacyDecision{
		Blocked: true, StatusCode: http.StatusForbidden,
		ErrorCode: "content_policy_violation", Message: "legacy",
	}}
	prompt := &fakePromptEngine{mode: ModeBlocking}
	dlp := &fakeDLPEngine{}

	decision := ProvideCoordinator(legacy, prompt, dlp).
		Check(context.Background(), Request{Body: []byte(`{}`)})

	require.Equal(t, DecisionBlock, decision.Kind)
	require.Equal(t, "content_policy_violation", decision.ErrorCode)
}
