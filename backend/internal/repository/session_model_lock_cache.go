// Package repository — 私有扩展（不属于 upstream sub2api）。
//
// 文件作用：Redis 层会话级模型锁定缓存。
// 包含类型：sessionModelLockCache 及其构造函数。
// merge 策略：全新文件，无 upstream 冲突。
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const sessionModelLockPrefix = "session_model_lock:"

type sessionModelLockCache struct {
	rdb *redis.Client
}

// NewSessionModelLockCache 构造私有扩展的会话模型锁缓存。
func NewSessionModelLockCache(rdb *redis.Client) service.SessionModelLockCache {
	return &sessionModelLockCache{rdb: rdb}
}

// buildSessionModelLockKey 构造 Redis key。
// 格式: session_model_lock:{groupID}:{sessionID}
// 含 groupID 实现分组隔离，避免跨分组共享会话状态。
func buildSessionModelLockKey(groupID int64, sessionID string) string {
	return fmt.Sprintf("%s%d:%s", sessionModelLockPrefix, groupID, sessionID)
}

// SetIfAbsent 实现"首次写入"语义。
//
// 返回值语义：
//   - isFirst=true:   key 不存在时成功写入，firstModel=""（无历史）
//   - isFirst=false:  key 已存在，firstModel=已存储的首个 model
//   - err:            底层 Redis 错误（调用方应 fail-open）
func (c *sessionModelLockCache) SetIfAbsent(ctx context.Context, groupID int64, sessionID, model string, ttl time.Duration) (string, bool, error) {
	if sessionID == "" {
		return "", false, errors.New("session_id is empty")
	}
	key := buildSessionModelLockKey(groupID, sessionID)

	ok, err := c.rdb.SetNX(ctx, key, model, ttl).Result()
	if err != nil {
		return "", false, err
	}
	if ok {
		return "", true, nil
	}

	// key 已存在，读取当前值返回
	firstModel, err := c.rdb.Get(ctx, key).Result()
	if err != nil {
		// 极小概率竞态：SETNX 失败但 GET 时刚好被 TTL 清掉。视为不存在历史。
		if errors.Is(err, redis.Nil) {
			return "", false, nil
		}
		return "", false, err
	}
	return firstModel, false, nil
}
