// 私有扩展（不属于 upstream sub2api）。
//
// 文件作用：为受保护模型配置的 per-user 共享日/周额度提供 Redis 缓存支持。
//   通过在 billingCache 上新增方法实现，不修改 billing_cache.go。
//
// 包含类型/方法：
//   - billingCache.GetModelQuotaUsage
//   - billingCache.UpdateModelQuotaUsage
//   - NewModelQuotaCache（暴露 service.ModelQuotaCache 接口）
//
// Redis key 格式：billing:mquota:{userID}:{groupID}
// Hash 字段：daily_usage, weekly_usage, daily_win_start, weekly_win_start
//
// merge 策略：全新文件，无 upstream 冲突。

package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const (
	modelQuotaKeyPrefix = "billing:mquota:"

	mqFieldDailyUsage     = "daily_usage"
	mqFieldWeeklyUsage    = "weekly_usage"
	mqFieldDailyWinStart  = "daily_win_start"
	mqFieldWeeklyWinStart = "weekly_win_start"

	modelQuotaCacheTTL = 8 * 24 * time.Hour // 8天，覆盖完整一周窗口
)

// updateModelQuotaUsageScript 原子地更新日/周用量窗口计数器。
// key 不存在时自动创建（首次计费后需要建立记录）。
// 窗口到期时自动重置。
// ARGV: [1]=cost, [2]=ttl_sec, [3]=now_unix, [4]=day_sec, [5]=week_sec
var updateModelQuotaUsageScript = redis.NewScript(`
	local cost    = tonumber(ARGV[1])
	local now     = tonumber(ARGV[3])
	local day_s   = tonumber(ARGV[4])
	local week_s  = tonumber(ARGV[5])

	local function update_window(usage_field, win_field, window_duration)
		local w = tonumber(redis.call('HGET', KEYS[1], win_field) or 0)
		if w == 0 or (now - w) >= window_duration then
			redis.call('HSET', KEYS[1], usage_field, tostring(cost))
			redis.call('HSET', KEYS[1], win_field, tostring(now))
		else
			redis.call('HINCRBYFLOAT', KEYS[1], usage_field, cost)
		end
	end

	update_window('daily_usage',  'daily_win_start',  day_s)
	update_window('weekly_usage', 'weekly_win_start', week_s)
	redis.call('EXPIRE', KEYS[1], ARGV[2])
	return 1
`)

// modelQuotaKey 生成 Redis key（按 user+group 聚合，所有受保护模型共享）。
func modelQuotaKey(userID, groupID int64) string {
	return fmt.Sprintf("%s%d:%d", modelQuotaKeyPrefix, userID, groupID)
}

// GetModelQuotaUsage 读取当前 daily/weekly 使用量（自动感知窗口是否过期）。
// 若 key 不存在返回零值和 nil。
func (c *billingCache) GetModelQuotaUsage(ctx context.Context, userID, groupID int64) (service.ModelQuotaUsage, error) {
	key := modelQuotaKey(userID, groupID)
	data, err := c.rdb.HGetAll(ctx, key).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return service.ModelQuotaUsage{}, err
	}

	now := time.Now().Unix()
	const daySec int64 = 86400
	const weekSec int64 = 7 * 86400

	var out service.ModelQuotaUsage

	if v, ok := data[mqFieldDailyUsage]; ok {
		if ws, ok2 := data[mqFieldDailyWinStart]; ok2 {
			winStart, _ := strconv.ParseInt(ws, 10, 64)
			if now-winStart < daySec {
				out.DailyUsage, _ = strconv.ParseFloat(v, 64)
			}
		}
	}
	if v, ok := data[mqFieldWeeklyUsage]; ok {
		if ws, ok2 := data[mqFieldWeeklyWinStart]; ok2 {
			winStart, _ := strconv.ParseInt(ws, 10, 64)
			if now-winStart < weekSec {
				out.WeeklyUsage, _ = strconv.ParseFloat(v, 64)
			}
		}
	}
	return out, nil
}

// SetModelQuotaUsage 初始化缓存条目（用于从 DB 同步初始值）。
func (c *billingCache) SetModelQuotaUsage(ctx context.Context, userID, groupID int64, data service.ModelQuotaUsage) error {
	key := modelQuotaKey(userID, groupID)
	now := time.Now().Unix()
	fields := map[string]any{
		mqFieldDailyUsage:     data.DailyUsage,
		mqFieldWeeklyUsage:    data.WeeklyUsage,
		mqFieldDailyWinStart:  now,
		mqFieldWeeklyWinStart: now,
	}
	pipe := c.rdb.Pipeline()
	pipe.HSet(ctx, key, fields)
	pipe.Expire(ctx, key, modelQuotaCacheTTL)
	_, err := pipe.Exec(ctx)
	return err
}

// UpdateModelQuotaUsage 异步安全地增加日/周用量计数（窗口过期自动重置）。
// key 不存在时返回 nil（fail-open：缓存未热身不阻塞请求）。
func (c *billingCache) UpdateModelQuotaUsage(ctx context.Context, userID, groupID int64, cost float64) error {
	key := modelQuotaKey(userID, groupID)
	now := time.Now().Unix()
	const daySec = 86400
	const weekSec = 7 * 86400
	ttlSec := int(modelQuotaCacheTTL.Seconds())

	_, err := updateModelQuotaUsageScript.Run(
		ctx, c.rdb, []string{key},
		cost, ttlSec, now, daySec, weekSec,
	).Result()
	if err != nil {
		return err
	}
	return nil
}

// NewModelQuotaCache 将同一 billingCache 实例暴露为 ModelQuotaCache 接口。
// 在 wire_gen.go 中用同一个 rdb 创建即可与 BillingCache 共享连接。
func NewModelQuotaCache(rdb *redis.Client) service.ModelQuotaCache {
	return &billingCache{rdb: rdb}
}
