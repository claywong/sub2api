package repository

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// incrModelQuotaUsageScript 在 key 存在时累加三窗口用量并续期。
// key 不存在返回 0（no-op）：DB 是权威值，下次读取 MISS 会回源到正确数值。
var incrModelQuotaUsageScript = redis.NewScript(`
	local exists = redis.call('EXISTS', KEYS[1])
	if exists == 0 then
		return 0
	end
	local cost = tonumber(ARGV[1])
	redis.call('HINCRBYFLOAT', KEYS[1], 'daily_usage', cost)
	redis.call('HINCRBYFLOAT', KEYS[1], 'weekly_usage', cost)
	redis.call('HINCRBYFLOAT', KEYS[1], 'monthly_usage', cost)
	redis.call('EXPIRE', KEYS[1], ARGV[2])
	return 1
`)

// modelQuotaUsageCacheKey 构造 Redis key。
// rule_key 已在 service 层归一为小写，可直接进 key。
func modelQuotaUsageCacheKey(userID, groupID int64, ruleKey string) string {
	return fmt.Sprintf("billing:model_quota_usage:%d:%d:%s", userID, groupID, ruleKey)
}

func parseModelQuotaUsageHash(m map[string]string) *service.ModelQuotaUsageCacheEntry {
	if len(m) == 0 {
		return nil
	}
	parseFloat := func(s string) float64 {
		if s == "" {
			return 0
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			log.Printf("billing_cache: corrupt model quota usage field %q (using 0): %v", s, err)
			return 0
		}
		return f
	}
	parseTimePtr := func(s string) *time.Time {
		if s == "" {
			return nil
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil
		}
		t := time.Unix(n, 0)
		return &t
	}
	return &service.ModelQuotaUsageCacheEntry{
		DailyUsageUSD:      parseFloat(m["daily_usage"]),
		WeeklyUsageUSD:     parseFloat(m["weekly_usage"]),
		MonthlyUsageUSD:    parseFloat(m["monthly_usage"]),
		DailyWindowStart:   parseTimePtr(m["daily_window_start"]),
		WeeklyWindowStart:  parseTimePtr(m["weekly_window_start"]),
		MonthlyWindowStart: parseTimePtr(m["monthly_window_start"]),
	}
}

func (c *billingCache) GetModelQuotaUsageCache(ctx context.Context, userID, groupID int64, ruleKey string) (*service.ModelQuotaUsageCacheEntry, bool, error) {
	key := modelQuotaUsageCacheKey(userID, groupID, ruleKey)
	m, err := c.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, false, err
	}
	entry := parseModelQuotaUsageHash(m)
	if entry == nil {
		return nil, false, nil
	}
	return entry, true, nil
}

func (c *billingCache) SetModelQuotaUsageCache(ctx context.Context, userID, groupID int64, ruleKey string, entry *service.ModelQuotaUsageCacheEntry, ttl time.Duration) error {
	if entry == nil {
		return nil
	}
	key := modelQuotaUsageCacheKey(userID, groupID, ruleKey)

	// 窗口起点为 nil（未初始化）时写空串，读取时 parseTimePtr 返回 nil，
	// 判定侧据此把该窗口视为已过期、用量按 0 计。
	timeField := func(t *time.Time) any {
		if t == nil {
			return ""
		}
		return t.Unix()
	}
	fields := map[string]any{
		"daily_usage":          entry.DailyUsageUSD,
		"weekly_usage":         entry.WeeklyUsageUSD,
		"monthly_usage":        entry.MonthlyUsageUSD,
		"daily_window_start":   timeField(entry.DailyWindowStart),
		"weekly_window_start":  timeField(entry.WeeklyWindowStart),
		"monthly_window_start": timeField(entry.MonthlyWindowStart),
	}

	pipe := c.rdb.TxPipeline()
	pipe.HSet(ctx, key, fields)
	pipe.Expire(ctx, key, ttl)
	_, err := pipe.Exec(ctx)
	return err
}

func (c *billingCache) IncrModelQuotaUsageCache(ctx context.Context, userID, groupID int64, ruleKey string, cost float64, ttl time.Duration) error {
	key := modelQuotaUsageCacheKey(userID, groupID, ruleKey)
	_, err := incrModelQuotaUsageScript.Run(ctx, c.rdb, []string{key}, cost, int(ttl.Seconds())).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	return nil
}

func (c *billingCache) InvalidateModelQuotaUsageCache(ctx context.Context, userID, groupID int64, ruleKey string) error {
	key := modelQuotaUsageCacheKey(userID, groupID, ruleKey)
	return c.rdb.Del(ctx, key).Err()
}
