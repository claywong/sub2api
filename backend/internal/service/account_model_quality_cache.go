// account_model_quality_cache.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：按 account+model 维度存储质量指标。
//
// 与 AccountTestHealthCache 的区别：
//   - key 为 accountModelKey{accountID, model}，同一账号不同模型独立统计
//   - 只存 TTFT / OTPS / CacheHit 三个质量指标，不做 HealthVerdict 三态判定
//   - 不涉及 TempUnschedulable / OnVerdictChange 回调
//
// 供 gateway_service_quality_bucket.go 的分桶过滤函数使用。
// =============================================================================
package service

import (
	"math"
	"sync"
	"time"
)

// accountModelKey sync.Map 的 key，按账号+模型维度隔离质量指标。
type accountModelKey struct {
	accountID int64
	model     string
}

// modelQualityWindow 单个 account+model 的滑动窗口（复用 metricBucket）。
type modelQualityWindow struct {
	mu      sync.Mutex
	buckets [healthBucketCount]metricBucket
	cursor  int
}

// AccountModelQualityCache 按 account+model 维度缓存质量指标（TTFT/OTPS/CacheHit）。
// 进程内单例，不持久化，重启后冷启动保护（无样本 → 最优桶）。
type AccountModelQualityCache struct {
	m sync.Map // key: accountModelKey, value: *modelQualityWindow
}

// NewAccountModelQualityCache 创建缓存实例。
func NewAccountModelQualityCache() *AccountModelQualityCache {
	return &AccountModelQualityCache{}
}

func (c *AccountModelQualityCache) getOrCreate(key accountModelKey) *modelQualityWindow {
	if v, ok := c.m.Load(key); ok {
		return v.(*modelQualityWindow)
	}
	w := &modelQualityWindow{}
	actual, _ := c.m.LoadOrStore(key, w)
	return actual.(*modelQualityWindow)
}

func (w *modelQualityWindow) advanceLocked(now time.Time) *metricBucket {
	nowSec := now.Unix()
	bucketStart := nowSec - (nowSec % healthBucketSeconds)
	cur := &w.buckets[w.cursor]
	if cur.startSec == bucketStart {
		return cur
	}
	w.cursor = (w.cursor + 1) % healthBucketCount
	next := &w.buckets[w.cursor]
	next.reset(bucketStart)
	return next
}

func (w *modelQualityWindow) recordLocked(sample CallSample, now time.Time) {
	b := w.advanceLocked(now)
	if sample.TTFTMs > 0 {
		b.ttftSum += int64(sample.TTFTMs)
		b.ttftN++
	}
	if sample.OutputTokens >= 10 && sample.TTFTMs > 0 && sample.DurationMs > sample.TTFTMs {
		decodeMs := sample.DurationMs - sample.TTFTMs
		otps := float64(sample.OutputTokens-1) * 1000.0 / float64(decodeMs)
		if otps > 0 && !math.IsNaN(otps) && !math.IsInf(otps, 0) {
			b.otpsSum += otps
			b.otpsN++
		}
	}
	if total := sample.CacheReadTokens + sample.CacheCreationTokens + sample.InputTokens; total > 0 {
		b.cacheHitRateSum += float64(sample.CacheReadTokens) / float64(total)
		b.cacheHitN++
	}
}

func (w *modelQualityWindow) snapshotLocked(now time.Time) HealthSnapshot {
	cutoff := now.Unix() - healthBucketCount*healthBucketSeconds
	var s HealthSnapshot
	for i := range w.buckets {
		b := &w.buckets[i]
		if b.startSec == 0 || b.startSec < cutoff {
			continue
		}
		s.ttftSumMs += b.ttftSum
		s.TTFTSampleCount += b.ttftN
		s.otpsSum += b.otpsSum
		s.OTPSSampleCount += b.otpsN
		s.cacheHitRateSum += b.cacheHitRateSum
		s.CacheHitSampleCount += b.cacheHitN
	}
	return s
}

// Record 上报一次真实请求的质量样本。
func (c *AccountModelQualityCache) Record(accountID int64, model string, sample CallSample) {
	if c == nil || accountID <= 0 {
		return
	}
	key := accountModelKey{accountID: accountID, model: model}
	w := c.getOrCreate(key)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.recordLocked(sample, time.Now())
}

// Snapshot 返回指定 account+model 的窗口聚合快照。
func (c *AccountModelQualityCache) Snapshot(accountID int64, model string) HealthSnapshot {
	if c == nil || accountID <= 0 {
		return HealthSnapshot{}
	}
	key := accountModelKey{accountID: accountID, model: model}
	v, ok := c.m.Load(key)
	if !ok {
		return HealthSnapshot{}
	}
	w := v.(*modelQualityWindow)
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snapshotLocked(time.Now())
}
