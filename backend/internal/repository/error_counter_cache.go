package repository

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const errorCounterPrefix = "upstream_error_count:account:"


type errorCounterCache struct {
	rdb *redis.Client
}

// NewErrorCounterCache 创建上游错误计数器缓存实例
func NewErrorCounterCache(rdb *redis.Client) service.ErrorCounterCache {
	return &errorCounterCache{rdb: rdb}
}

// IncrementErrorCount 增加账户的上游错误计数，返回当前计数值
func (c *errorCounterCache) IncrementErrorCount(ctx context.Context, accountID int64, windowMinutes int) (int64, error) {
	key := fmt.Sprintf("%s%d", errorCounterPrefix, accountID)

	ttlSeconds := windowMinutes * 60
	if ttlSeconds < 60 {
		ttlSeconds = 60
	}

	result, err := counterIncrScript.Run(ctx, c.rdb, []string{key}, ttlSeconds).Int64()
	if err != nil {
		return 0, fmt.Errorf("increment error count: %w", err)
	}

	return result, nil
}

// ResetErrorCount 重置账户的上游错误计数
func (c *errorCounterCache) ResetErrorCount(ctx context.Context, accountID int64) error {
	key := fmt.Sprintf("%s%d", errorCounterPrefix, accountID)
	return c.rdb.Del(ctx, key).Err()
}
