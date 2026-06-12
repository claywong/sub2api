// account_model_quality_cache.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：按 account+model 维度存储质量指标。
//
// 与 AccountTestHealthCache 的区别：
//   - key 为 accountModelKey{accountID, model}，同一账号不同模型独立统计
//   - 只存 TTFT / OTPS / CacheHit 三个质量指标，不做 HealthVerdict 三态判定
//   - 不涉及 TempUnschedulable / OnVerdictChange 回调
//   - 窗口与健康缓存解耦：固定 12 桶，桶长由窗口分钟数推导（默认 60min → 5min/桶），
//     可经 ConfigureWindowMinutes 调整；HealthVerdict 的 10min 窗口不受影响
//   - 每桶保留有限 TTFT 样本，供加权选号估计 P90（惩罚长尾延迟）
//
// 供 gateway_service_quality_bucket.go 的分桶过滤函数
// 与 gateway_service_weighted_select.go 的加权选号使用。
// =============================================================================
package service

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// qualityBucketCount 质量窗口桶数（固定），桶长 = 窗口长度 / 桶数。
	qualityBucketCount = 12
	// defaultQualityWindowMinutes 默认质量窗口长度（分钟）。
	defaultQualityWindowMinutes = 60
	// qualityBucketMaxTTFTSamples 单桶保留的 TTFT 样本上限（P90 估计用），超出丢弃。
	qualityBucketMaxTTFTSamples = 64
)

// accountModelKey sync.Map 的 key，按账号+模型维度隔离质量指标。
type accountModelKey struct {
	accountID int64
	model     string
}

// qualityBucket 质量窗口的单个时间桶：复用 metricBucket 的累计字段，
// 额外保留有限 TTFT 原始样本用于 P90 估计。
type qualityBucket struct {
	metricBucket
	ttftSamples []int32
}

func (b *qualityBucket) reset(startSec int64) {
	b.metricBucket.reset(startSec)
	b.ttftSamples = b.ttftSamples[:0]
}

// modelQualityWindow 单个 account+model 的滑动窗口。
type modelQualityWindow struct {
	mu      sync.Mutex
	buckets [qualityBucketCount]qualityBucket
	cursor  int
}

// AccountModelQualityCache 按 account+model 维度缓存质量指标（TTFT/OTPS/CacheHit）。
// 进程内单例，不持久化，重启后冷启动保护（无样本 → 最优桶 / 中性分）。
type AccountModelQualityCache struct {
	m sync.Map // key: accountModelKey, value: *modelQualityWindow
	// bucketSeconds 单桶长度（秒），默认 defaultQualityWindowMinutes*60/qualityBucketCount。
	bucketSeconds atomic.Int64
}

// NewAccountModelQualityCache 创建缓存实例。
func NewAccountModelQualityCache() *AccountModelQualityCache {
	c := &AccountModelQualityCache{}
	c.bucketSeconds.Store(int64(defaultQualityWindowMinutes * 60 / qualityBucketCount))
	return c
}

// ConfigureWindowMinutes 设置质量窗口总长度（分钟），幂等，可在运行期调用。
// minutes <= 0 时保持当前值不变。
func (c *AccountModelQualityCache) ConfigureWindowMinutes(minutes int) {
	if c == nil || minutes <= 0 {
		return
	}
	sec := int64(minutes * 60 / qualityBucketCount)
	if sec < 1 {
		sec = 1
	}
	if c.bucketSeconds.Load() != sec {
		c.bucketSeconds.Store(sec)
	}
}

func (c *AccountModelQualityCache) bucketSec() int64 {
	if v := c.bucketSeconds.Load(); v > 0 {
		return v
	}
	return int64(defaultQualityWindowMinutes * 60 / qualityBucketCount)
}

func (c *AccountModelQualityCache) getOrCreate(key accountModelKey) *modelQualityWindow {
	if v, ok := c.m.Load(key); ok {
		return v.(*modelQualityWindow)
	}
	w := &modelQualityWindow{}
	actual, _ := c.m.LoadOrStore(key, w)
	return actual.(*modelQualityWindow)
}

func (w *modelQualityWindow) advanceLocked(now time.Time, bucketSec int64) *qualityBucket {
	nowSec := now.Unix()
	bucketStart := nowSec - (nowSec % bucketSec)
	cur := &w.buckets[w.cursor]
	if cur.startSec == bucketStart {
		return cur
	}
	w.cursor = (w.cursor + 1) % qualityBucketCount
	next := &w.buckets[w.cursor]
	next.reset(bucketStart)
	return next
}

func (w *modelQualityWindow) recordLocked(sample CallSample, now time.Time, bucketSec int64) {
	b := w.advanceLocked(now, bucketSec)
	if sample.TTFTMs > 0 {
		b.ttftSum += int64(sample.TTFTMs)
		b.ttftN++
		if len(b.ttftSamples) < qualityBucketMaxTTFTSamples {
			b.ttftSamples = append(b.ttftSamples, int32(sample.TTFTMs))
		}
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

func (w *modelQualityWindow) snapshotLocked(now time.Time, bucketSec int64) HealthSnapshot {
	cutoff := now.Unix() - qualityBucketCount*bucketSec
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

// ttftP90Locked 基于窗口内保留的 TTFT 样本估计 P90（ms）。
// 无样本返回 0。样本上限 12 桶 × 64 = 768，排序开销可忽略。
func (w *modelQualityWindow) ttftP90Locked(now time.Time, bucketSec int64) float64 {
	cutoff := now.Unix() - qualityBucketCount*bucketSec
	var samples []int32
	for i := range w.buckets {
		b := &w.buckets[i]
		if b.startSec == 0 || b.startSec < cutoff {
			continue
		}
		samples = append(samples, b.ttftSamples...)
	}
	if len(samples) == 0 {
		return 0
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	idx := (len(samples)*9+9)/10 - 1 // ceil(0.9*n) 的 0-based 下标
	if idx < 0 {
		idx = 0
	}
	if idx >= len(samples) {
		idx = len(samples) - 1
	}
	return float64(samples[idx])
}

// Record 上报一次真实请求的质量样本。
func (c *AccountModelQualityCache) Record(accountID int64, model string, sample CallSample) {
	if c == nil || accountID <= 0 {
		return
	}
	key := accountModelKey{accountID: accountID, model: model}
	w := c.getOrCreate(key)
	sec := c.bucketSec()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.recordLocked(sample, time.Now(), sec)
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
	sec := c.bucketSec()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snapshotLocked(time.Now(), sec)
}

// SnapshotWithP90 返回窗口聚合快照与 TTFT P90 估计（ms，0 表示无样本）。
// 供加权选号打分使用（ttftEff = 0.5*avg + 0.5*p90，惩罚长尾）。
func (c *AccountModelQualityCache) SnapshotWithP90(accountID int64, model string) (HealthSnapshot, float64) {
	if c == nil || accountID <= 0 {
		return HealthSnapshot{}, 0
	}
	key := accountModelKey{accountID: accountID, model: model}
	v, ok := c.m.Load(key)
	if !ok {
		return HealthSnapshot{}, 0
	}
	w := v.(*modelQualityWindow)
	sec := c.bucketSec()
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snapshotLocked(now, sec), w.ttftP90Locked(now, sec)
}
