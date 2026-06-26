// 私有扩展（不属于 upstream sub2api）。
//
// 文件作用：声明 SessionModelLockService 依赖的缓存接口。
// 包含类型：SessionModelLockCache（仅 interface）。
// merge 策略：全新文件，无 upstream 冲突。
package service

import (
	"context"
	"time"
)

// SessionModelLockTTL 是单个会话锁状态在缓存中的存活时间。
// 24h 远大于绝大多数 Claude Code 会话的生命周期，避免误锁。
const SessionModelLockTTL = 24 * time.Hour

// SessionModelLockCache 抽象会话级模型锁的状态存储，便于单测中 mock。
type SessionModelLockCache interface {
	// SetIfAbsent 实现"首次写入"语义。
	//
	// 返回值约定：
	//   - isFirst=true:  key 不存在，已写入 (firstModel="")
	//   - isFirst=false: key 已存在，firstModel=已存储的首个 model
	//   - err:           底层错误，调用方应当 fail-open
	SetIfAbsent(ctx context.Context, groupID int64, sessionID, model string, ttl time.Duration) (firstModel string, isFirst bool, err error)
}
