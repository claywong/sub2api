// 私有扩展（不属于 upstream sub2api）。
//
// 文件作用：会话级模型锁定（Session Model Lock）核心业务逻辑。
//   配合分组的 ProtectedModels 列表，对每个 Claude Code 会话首次出现的 model 做 SETNX 记录；
//   后续若 model 切换到保护列表中的另一模型，则返回 *SessionModelLockError，由 Handler 翻译为 403。
//
// 包含类型/方法：
//   - SessionModelLockService（核心结构体与构造）
//   - SessionModelLockError（业务专属错误类型）
//   - SessionModelLockService.Check
//
// merge 策略：全新文件，无 upstream 冲突。
package service

import (
	"context"
	"fmt"
)

// SessionModelLockError 表示当前请求的 model 在该会话中被锁定，不允许使用。
// Handler 层用 errors.As 取出后翻译为 HTTP 403 + permission_error。
type SessionModelLockError struct {
	SessionID    string
	FirstModel   string
	CurrentModel string
}

func (e *SessionModelLockError) Error() string {
	return fmt.Sprintf("session %q locked to first model %q, cannot switch to %q", e.SessionID, e.FirstModel, e.CurrentModel)
}

// SessionModelLockService 实现会话级模型锁的检查。
//
// 调用入口：GatewayHandler.Messages（仅 Anthropic 协议）。
// 失败语义：仅对"违反锁定语义"的请求返回 *SessionModelLockError；
//          底层 Redis 故障或 session_id 缺失一律 fail-open（返回 nil 或 非业务 error）。
type SessionModelLockService struct {
	cache SessionModelLockCache
}

// NewSessionModelLockService 构造服务。
func NewSessionModelLockService(cache SessionModelLockCache) *SessionModelLockService {
	return &SessionModelLockService{cache: cache}
}

// Check 执行会话级模型锁检查。
//
// 返回值：
//   - nil:                       允许通过
//   - *SessionModelLockError:    业务拒绝，应返回 403
//   - 其他 error:                内部错误（Redis 故障等），Handler 应 fail-open
//
// 快速 bypass 条件（任一满足直接返回 nil）：
//   - group == nil 或 ProtectedModels 为空
//   - parsed 为 nil 或 parsed.Model 为空
//   - metadata.user_id 缺失或无法解析出 session_id
//   - 当前请求 model 不在保护列表内（即使切换，也不是 "切到保护模型" 的非法路径）
//
// 锁定语义：
//   - 会话首次出现的 model = firstModel
//   - 当前请求 model = currentModel
//   - 若 currentModel == firstModel：允许（保持原状）
//   - 若 currentModel != firstModel 且 currentModel 命中保护列表：拒绝
//   - 若 currentModel != firstModel 且 currentModel 不在保护列表：允许（切到非保护模型）
func (s *SessionModelLockService) Check(ctx context.Context, group *Group, parsed *ParsedRequest) error {
	// 快速 bypass：分组未配置保护列表
	if group == nil || len(group.ProtectedModels) == 0 {
		return nil
	}
	if parsed == nil || parsed.Model == "" {
		return nil
	}

	// 提取 session_id（仅依赖 Claude Code metadata.user_id）
	uid := ParseMetadataUserID(parsed.MetadataUserID)
	if uid == nil || uid.SessionID == "" {
		return nil
	}

	// 优化：如果当前 model 不在保护列表，无论历史如何都不可能违反锁
	// （锁的语义是"不允许中途切入保护模型"，所以只有 currentModel ∈ 保护列表才需要进一步检查）
	if !matchesAnyProtectedModel(parsed.Model, group.ProtectedModels) {
		// 此分支不需要写入 Redis：首次模型若是非保护模型，后续即使再来非保护模型也不违反；
		// 若后续切到保护模型，那一刻的 SETNX 会覆盖空白记录——所以这里 fall-through 走 SetIfAbsent 也行。
		// 但更优做法是：依然写入首次模型，作为后续可能的对比依据。
		_, _, err := s.cache.SetIfAbsent(ctx, group.ID, uid.SessionID, parsed.Model, SessionModelLockTTL)
		if err != nil {
			return err
		}
		return nil
	}

	// 当前 model 命中保护列表，必须确认它是会话的首个模型
	firstModel, isFirst, err := s.cache.SetIfAbsent(ctx, group.ID, uid.SessionID, parsed.Model, SessionModelLockTTL)
	if err != nil {
		return err
	}
	if isFirst {
		// 首次写入即当前 model，会话从保护模型起手，允许
		return nil
	}
	if firstModel == parsed.Model {
		// 同一会话保持同一保护模型，允许
		return nil
	}
	// 当前模型 ≠ 首个模型 且 当前模型 ∈ 保护列表：拒绝
	return &SessionModelLockError{
		SessionID:    uid.SessionID,
		FirstModel:   firstModel,
		CurrentModel: parsed.Model,
	}
}

// matchesAnyProtectedModel 判断 model 是否匹配任一保护模式。
// 复用 group.go 中现有的 matchModelPattern（已支持末尾 * 通配符）。
func matchesAnyProtectedModel(model string, patterns []string) bool {
	for _, p := range patterns {
		if matchModelPattern(p, model) {
			return true
		}
	}
	return false
}
