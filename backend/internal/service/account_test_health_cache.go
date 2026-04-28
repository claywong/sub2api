package service

import (
	"math"
	"sync"
	"time"
)

// 包级常量：健康感知参数
const (
	HardFilterThreshold       = 2
	TempUnschedThreshold      = 3
	TempUnschedInitDuration   = 10 * time.Minute
	TempUnschedMaxDuration    = 60 * time.Minute
	RetryIntervalStep         = 30 * time.Second
	RetryIntervalMax          = 5 * time.Minute
	TTFTEwmaAlpha             = 0.3
	LatencyBucketFast         = 1000
	LatencyBucketSlow         = 3000
	SlowThreshold             = 60000
	SlowWindowDuration        = 10 * time.Minute
	SlowMinSampleCount        = 5
	SlowBucketMid             = 0.20
	SlowBucketHigh            = 0.50
)

// AccountTestHealth 存储单个账号的健康状态
type AccountTestHealth struct {
	mu                 sync.Mutex
	ConsecFails        int
	RetryInterval      time.Duration
	NextRetryAt        time.Time
	TTFTEwma           float64
	SlowReqCount       int
	TotalReqCount      int
	WindowStart        time.Time
	TempUnschedDuration time.Duration
	LastStatus         string
	LastTestedAt       time.Time
}

// AccountTestHealthCache 使用 sync.Map 存储账号健康状态，key 为 accountID int64
type AccountTestHealthCache struct {
	m sync.Map
}

// NewAccountTestHealthCache 创建一个新的 AccountTestHealthCache
func NewAccountTestHealthCache() *AccountTestHealthCache {
	return &AccountTestHealthCache{}
}

// Get 返回指定账号的健康状态，不存在时返回 nil
func (c *AccountTestHealthCache) Get(accountID int64) *AccountTestHealth {
	v, ok := c.m.Load(accountID)
	if !ok {
		return nil
	}
	return v.(*AccountTestHealth)
}

// GetOrCreate 返回指定账号的健康状态，不存在时创建一个新的
func (c *AccountTestHealthCache) GetOrCreate(accountID int64) *AccountTestHealth {
	h := &AccountTestHealth{
		TTFTEwma: math.NaN(),
	}
	actual, _ := c.m.LoadOrStore(accountID, h)
	return actual.(*AccountTestHealth)
}

// UpdateFromTest 根据测试结果更新健康状态
func (c *AccountTestHealthCache) UpdateFromTest(accountID int64, result *ScheduledTestResult) {
	h := c.GetOrCreate(accountID)
	h.mu.Lock()
	defer h.mu.Unlock()

	h.LastTestedAt = time.Now()
	h.LastStatus = result.Status

	if result.Status == "success" {
		h.ConsecFails = 0
		h.RetryInterval = 0
		if result.FirstTokenMs != nil {
			ewma := h.TTFTEwma
			ttft := float64(*result.FirstTokenMs)
			if math.IsNaN(ewma) {
				h.TTFTEwma = ttft
			} else {
				h.TTFTEwma = TTFTEwmaAlpha*ttft + (1-TTFTEwmaAlpha)*ewma
			}
		}
	} else {
		h.ConsecFails++
		if h.RetryInterval == 0 {
			h.RetryInterval = RetryIntervalStep
		} else {
			h.RetryInterval *= 2
			if h.RetryInterval > RetryIntervalMax {
				h.RetryInterval = RetryIntervalMax
			}
		}
		h.NextRetryAt = time.Now().Add(h.RetryInterval)
	}
}

// UpdateTTFT 使用 EWMA 更新首字时间（alpha=0.3），NaN 时直接赋值
func (c *AccountTestHealthCache) UpdateTTFT(accountID int64, ttftMs int) {
	h := c.GetOrCreate(accountID)
	h.mu.Lock()
	defer h.mu.Unlock()

	if math.IsNaN(h.TTFTEwma) {
		h.TTFTEwma = float64(ttftMs)
	} else {
		h.TTFTEwma = TTFTEwmaAlpha*float64(ttftMs) + (1-TTFTEwmaAlpha)*h.TTFTEwma
	}
}

// UpdateDuration 使用滑动窗口（10min）统计慢请求（阈值 60000ms）
func (c *AccountTestHealthCache) UpdateDuration(accountID int64, durationMs int) {
	h := c.GetOrCreate(accountID)
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()
	// 窗口过期时重置
	if h.WindowStart.IsZero() || now.Sub(h.WindowStart) > SlowWindowDuration {
		h.WindowStart = now
		h.SlowReqCount = 0
		h.TotalReqCount = 0
	}

	h.TotalReqCount++
	if durationMs >= SlowThreshold {
		h.SlowReqCount++
	}
}

// LatencyBucket 返回 TTFT 延时分桶：NaN→1，<1000ms→0，1000-2999ms→1，≥3000ms→2
func (c *AccountTestHealthCache) LatencyBucket(accountID int64) int {
	h := c.Get(accountID)
	if h == nil {
		return 1
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	if math.IsNaN(h.TTFTEwma) {
		return 1
	}
	if h.TTFTEwma < float64(LatencyBucketFast) {
		return 0
	}
	if h.TTFTEwma < float64(LatencyBucketSlow) {
		return 1
	}
	return 2
}

// SlowBucket 返回慢请求分桶：样本<5或无数据→0，慢率<20%→0，20%-50%→1，≥50%→2
func (c *AccountTestHealthCache) SlowBucket(accountID int64) int {
	h := c.Get(accountID)
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.TotalReqCount < SlowMinSampleCount {
		return 0
	}

	slowRate := float64(h.SlowReqCount) / float64(h.TotalReqCount)
	if slowRate < SlowBucketMid {
		return 0
	}
	if slowRate < SlowBucketHigh {
		return 1
	}
	return 2
}

// PassesHardFilter 返回账号是否通过硬过滤：ConsecFails >= 2 返回 false
func (c *AccountTestHealthCache) PassesHardFilter(accountID int64) bool {
	h := c.Get(accountID)
	if h == nil {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ConsecFails < HardFilterThreshold
}
