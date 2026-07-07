// Package service provides business logic and domain services for the application.
// 本文件为私有扩展（不属于 upstream sub2api）
// 功能：TempUnschedulable 自然到期时自动重置 HealthVerdict
// Merge 策略：独立文件，与 upstream 无冲突
package service

import (
	"sync"
	"time"
)

// autoRecoveryTracker 跟踪已处理的账号恢复事件，避免重复触发
type autoRecoveryTracker struct {
	mu         sync.RWMutex
	recovered  map[int64]time.Time // accountID -> 恢复处理时间
	expiration time.Duration       // 记录过期时间
}

func newAutoRecoveryTracker() *autoRecoveryTracker {
	return &autoRecoveryTracker{
		recovered:  make(map[int64]time.Time),
		expiration: 10 * time.Minute, // 10分钟后允许重新处理
	}
}

// shouldTriggerRecovery 判断是否应该触发恢复逻辑
// 返回 true 表示应该触发，同时记录本次触发
func (t *autoRecoveryTracker) shouldTriggerRecovery(accountID int64, tempUnschedUntil time.Time) bool {
	now := time.Now()

	// 检测 TempUnschedulable 是否刚过期（5分钟内）
	if now.Before(tempUnschedUntil) || now.Sub(tempUnschedUntil) > 5*time.Minute {
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// 清理过期记录
	for id, ts := range t.recovered {
		if now.Sub(ts) > t.expiration {
			delete(t.recovered, id)
		}
	}

	// 检查是否已处理过
	if lastRecovered, exists := t.recovered[accountID]; exists {
		// 如果距离上次处理不到 10 分钟，跳过
		if now.Sub(lastRecovered) < t.expiration {
			return false
		}
	}

	// 记录本次处理
	t.recovered[accountID] = now
	return true
}
