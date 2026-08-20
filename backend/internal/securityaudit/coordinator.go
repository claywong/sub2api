package securityaudit

import (
	"context"
	"errors"
	"net/http"
	"sync"
)

type LegacyEngine interface {
	Check(ctx context.Context, req Request) (*LegacyDecision, error)
}

type PromptEngine interface {
	EffectiveMode() Mode
	Enqueue(ctx context.Context, req Request) error
	Evaluate(ctx context.Context, req Request) (*PromptDecision, error)
}

type Coordinator struct {
	legacy LegacyEngine
	prompt PromptEngine

	// 私有扩展：DLP 引擎，通过 WithDLP 注入，实现见 coordinator_dlp.go。
	dlp DLPEngine
}

func NewCoordinator(legacy LegacyEngine, prompt PromptEngine) *Coordinator {
	return &Coordinator{legacy: legacy, prompt: prompt}
}

func (c *Coordinator) Check(ctx context.Context, req Request) Decision {
	if c == nil {
		return allowDecision(nil, nil)
	}
	// 私有扩展：DLP 检测独立于审计模式，必须跑在 mode 分发之前，否则 ModeOff 与
	// ModeAsync 下 DLP 会静默不生效。实现见 coordinator_dlp.go。
	if decision := c.checkDLP(ctx, req); decision != nil {
		return *decision
	}
	mode := ModeOff
	if c.prompt != nil {
		mode = c.prompt.EffectiveMode()
	}
	switch mode {
	case ModeAsync:
		// Enqueue is deliberately best-effort. The implementation owns a bounded
		// context and copies request memory before it can outlive the Handler.
		_ = c.prompt.Enqueue(ctx, req.Clone())
		legacy, _ := c.checkLegacy(ctx, req)
		return prioritize(legacy, nil)
	case ModeBlocking:
		return c.checkBlocking(ctx, req)
	default:
		legacy, _ := c.checkLegacy(ctx, req)
		return prioritize(legacy, nil)
	}
}

func (c *Coordinator) checkBlocking(ctx context.Context, req Request) Decision {
	var wg sync.WaitGroup
	wg.Add(2)
	var legacy *LegacyDecision
	var prompt *PromptDecision
	go func() {
		defer wg.Done()
		legacy, _ = c.checkLegacy(ctx, req)
	}()
	go func() {
		defer wg.Done()
		if c.prompt == nil {
			prompt = unavailablePromptDecision(ErrorCodeUnavailable)
			return
		}
		result, err := c.prompt.Evaluate(ctx, req.Clone())
		if err != nil {
			var guardErr *GuardError
			if errors.As(err, &guardErr) && guardErr.Code == ErrorCodeInvalidResponse {
				prompt = unavailablePromptDecision(ErrorCodeInvalidResponse)
				return
			}
			prompt = unavailablePromptDecision(ErrorCodeUnavailable)
			return
		}
		if result == nil {
			prompt = unavailablePromptDecision(ErrorCodeUnavailable)
			return
		}
		prompt = result
	}()
	wg.Wait()
	return prioritize(legacy, prompt)
}

func (c *Coordinator) checkLegacy(ctx context.Context, req Request) (*LegacyDecision, error) {
	if c.legacy == nil {
		return nil, nil
	}
	return c.legacy.Check(ctx, req)
}

func prioritize(legacy *LegacyDecision, prompt *PromptDecision) Decision {
	if legacy != nil && legacy.Blocked {
		status := legacy.StatusCode
		if status < 400 || status > 599 {
			status = http.StatusForbidden
		}
		code := legacy.ErrorCode
		if code == "" {
			code = "content_policy_violation"
		}
		return Decision{
			Kind: DecisionBlock, HTTPStatus: status, ErrorCode: code, ClientMessage: legacy.Message,
			Legacy: legacy, Prompt: prompt, AllowNextStage: false,
		}
	}
	if prompt == nil {
		return allowDecision(legacy, nil)
	}
	switch prompt.Kind {
	case DecisionBlock:
		// 私有扩展：DLP 拦截要保留自己的错误码与文案，否则运维在 API 边界上无法
		// 区分是内容安全拦的还是数据防泄漏拦的。实现见 prompt_guard_dlp.go。
		errorCode, clientMessage := blockedErrorCodeAndMessage(prompt)
		return Decision{Kind: DecisionBlock, HTTPStatus: http.StatusForbidden, ErrorCode: errorCode,
			ClientMessage: clientMessage, Legacy: legacy, Prompt: prompt}
	case DecisionInvalid:
		return Decision{Kind: DecisionInvalid, HTTPStatus: http.StatusServiceUnavailable, ErrorCode: ErrorCodeInvalidResponse,
			ClientMessage: "提示词安全审计暂时不可用，请稍后重试", Legacy: legacy, Prompt: prompt}
	case DecisionUnavailable:
		return Decision{Kind: DecisionUnavailable, HTTPStatus: http.StatusServiceUnavailable, ErrorCode: ErrorCodeUnavailable,
			ClientMessage: "提示词安全审计暂时不可用，请稍后重试", Legacy: legacy, Prompt: prompt}
	case DecisionFlag:
		return Decision{Kind: DecisionFlag, HTTPStatus: http.StatusOK, Legacy: legacy, Prompt: prompt, AllowNextStage: true}
	default:
		return allowDecision(legacy, prompt)
	}
}

func allowDecision(legacy *LegacyDecision, prompt *PromptDecision) Decision {
	return Decision{Kind: DecisionAllow, HTTPStatus: http.StatusOK, Legacy: legacy, Prompt: prompt, AllowNextStage: true}
}

func unavailablePromptDecision(code string) *PromptDecision {
	kind := DecisionUnavailable
	if code == ErrorCodeInvalidResponse {
		kind = DecisionInvalid
	}
	return &PromptDecision{Kind: kind, ErrorCode: code, AllowNextStage: false}
}
