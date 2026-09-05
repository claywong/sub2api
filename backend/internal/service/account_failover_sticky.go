package service

// 私有扩展（不属于 upstream sub2api）
//
// 所含内容：
//   - 常量 credentialKeyFailoverNoSticky
//   - 方法 (*Account).SkipsStickyBindOnFailover
//   - 函数 WithFailoverAttempt / FailoverAttemptFromContext
//   - 函数 skipStickyBindForFailover
//
// 背景：「救火账号」语义。某些账号（高价号、低配额号、共享号）只希望在 failover
// 重试时临时顶一下，不希望它接管粘性会话长期占住 session —— 否则一次偶发故障就把
// 会话永久迁移到救火号上，后续请求全部落在不想长期使用的账号。
//
// 语义：账号开启 credentials.failover_no_sticky 后，当它是被 failover 重试选中的
// （而非首次 attempt 选中、也非同账号重试）时，即使请求成功也不写粘性绑定。
// 原账号的绑定原样保留，下次请求若原账号已恢复则继续命中它（prompt cache 不丢），
// 若原账号仍不可调度则由既有的 shouldClearStickySession 清除并重新调度。
//
// 「是否 failover 重试」的信号选取（三个候选，前两个不可用）：
//   - len(excludedIDs) > 0 —— 不可用：HandleSelectionExhausted 的 503 退避分支会
//     清空 FailedAccountIDs，清空后的重试看起来像首次 attempt。
//   - ctxkey.AccountSwitchCount —— 不可用：写入时机在选号「之后」，调度层读不到。
//   - fs.SwitchCount > 0 且在选号「之前」写 ctx —— 采用：503 退避只清
//     FailedAccountIDs，不重置 SwitchCount，判定始终准确。
//
// 同账号重试（FailoverState.IsSameAccountRetry）不递增 SwitchCount，因此不算
// failover，粘性照常保持 —— 同账号重试的账号仍是原账号，语义上本就该粘。
//
// merge 策略：本文件纯增量，且刻意不改 RequestMetadata struct（用独立 ctx key
// 而非新增字段），使 upstream 改动 request_metadata.go 时零冲突。
//
// 调用点清单（改动 upstream 文件的位置，merge 后需确认仍在）：
//   - gateway_service.go      bindGatewayStickySessionDuringSelection 内部判定
//   - gateway_handler.go      成功路径 BindStickySession 前判定
//   - gateway_handler.go              ×2  选号前 WithFailoverAttempt（Anthropic + Gemini）
//   - gateway_handler_responses.go        选号前 WithFailoverAttempt
//   - gateway_handler_chat_completions.go 选号前 WithFailoverAttempt
//
// 刻意不做的事：Layer 1.5 命中存量绑定时「不」跳过 RefreshSessionTTL。若救火号在
// 开关打开前已是某会话的 sticky owner，那条绑定是正常选号产生的，不属于本开关的
// 管辖范围（本开关只否决 failover 路径的接管）。存量绑定按原逻辑续期，直到账号
// 不可调度时由 shouldClearStickySession 清除。
//
// 注：bindGatewayStickySessionDuringSelection 的签名由 accountID int64 改为
// account *Account，使 7 处调用点由编译器强制同步。这是刻意的设计：上一个私有
// 扩展 stickySlotConcurrency 是可选包装，d754be0d8 拆分 service 文件时调用点被
// 还原成原始参数，编译与测试均通过，功能静默失效。改签名把语义保护从「靠记性」
// 变成「靠类型」。
//
// @author wangzhong

import "context"

// credentialKeyFailoverNoSticky 是账号「救火号」开关在 Credentials 中的键名。
// 缺省（键不存在）即保持 upstream 行为：failover 命中并成功后照常接管粘性。

// failoverAttemptContextKey 是「当前 attempt 由 failover 重试触发」的 ctx key。
// 用独立的私有 struct key 而非 ctxkey.Key 常量，避免碰撞，也避免改动 upstream 的
// ctxkey 包与 RequestMetadata struct。
type failoverAttemptContextKey struct{}

// SkipsStickyBindOnFailover 返回该账号是否开启了「救火号」开关：
// 被 failover 重试选中时不接管粘性会话。

// WithFailoverAttempt 标记当前 attempt 是否由 failover 重试触发。
// 必须在 SelectAccountWithLoadAwareness 之前调用，调度层才能读到。
func WithFailoverAttempt(ctx context.Context, value bool) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, failoverAttemptContextKey{}, value)
}

// FailoverAttemptFromContext 返回当前 attempt 是否由 failover 重试触发。
// 未标记时返回 false（首次 attempt 语义）。
func FailoverAttemptFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	value, _ := ctx.Value(failoverAttemptContextKey{}).(bool)
	return value
}

// skipStickyBindForFailover 判定是否应跳过对该账号的粘性绑定写入。
// 仅当「账号开启救火号开关」且「当前 attempt 由 failover 重试触发」时为 true。
// skipStickyBindForFailoverByID 是 skipStickyBindForFailover 的按 ID 回查版本，
// 用于只拿到 accountID 的绑定点（利润门终检路径）。
//
// 调用方必须先自行确认 FailoverAttemptFromContext(ctx) 为真，避免在正常路径上
// 引入一次多余的账号查询 —— 本函数不重复该判断。
//
// 回查失败时返回 false（保持 upstream 绑定行为），不因一次读库失败改变调度语义。
