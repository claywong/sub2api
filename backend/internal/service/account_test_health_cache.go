package service

import (
	"math"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// 内置默认值，config 字段为零时回退到这里
const (
	defaultHardFilterThreshold     = 2
	defaultTempUnschedThreshold    = 4
	defaultTempUnschedInitDuration = 30 * time.Minute
	defaultTempUnschedMaxDuration  = 60 * time.Minute
	defaultRetryIntervalStep       = 15 * time.Second
	defaultRetryIntervalMax        = 5 * time.Minute
	defaultSlowThresholdMs         = 20000
	defaultSlowWindowDuration      = 10 * time.Minute
	defaultSlowMinSampleCount      = 5
	defaultSlowBucketMidPct        = 20
	defaultSlowBucketHighPct       = 50

	// 算法常量，不对外暴露配置（调整影响面太大）
	TTFTEwmaAlpha     = 0.3
	LatencyBucketFast = 4000
	LatencyBucketSlow = 8000
)

// 向后兼容的包级别常量，供外部直接引用的代码过渡期使用
const (
	HardFilterThreshold     = defaultHardFilterThreshold
	TempUnschedThreshold    = defaultTempUnschedThreshold    // 4
	TempUnschedInitDuration = defaultTempUnschedInitDuration // 30min
	TempUnschedMaxDuration  = defaultTempUnschedMaxDuration
	RetryIntervalStep       = defaultRetryIntervalStep // 15s
	RetryIntervalMax        = defaultRetryIntervalMax
	SlowThreshold           = defaultSlowThresholdMs
	SlowWindowDuration      = defaultSlowWindowDuration
	SlowMinSampleCount      = defaultSlowMinSampleCount
	SlowBucketMid           = 0.20
	SlowBucketHigh          = 0.50
)

// healthCfg 持有从 config.AccountHealthConfig 解析后的有效值（已完成零值回退）
type healthCfg struct {
	hardFilterThreshold  int
	tempUnschedThreshold int
	tempUnschedInit      time.Duration
	tempUnschedMax       time.Duration
	retryIntervalStep    time.Duration
	retryIntervalMax     time.Duration
	slowThresholdMs      int
	slowWindow           time.Duration
	slowMinSampleCount   int
	slowBucketMid        float64
	slowBucketHigh       float64
	latencyBucketFastMs  int
	latencyBucketSlowMs  int
}

func resolveHealthCfg(c *config.AccountHealthConfig) healthCfg {
	r := healthCfg{
		hardFilterThreshold:  defaultHardFilterThreshold,
		tempUnschedThreshold: defaultTempUnschedThreshold,
		tempUnschedInit:      defaultTempUnschedInitDuration,
		tempUnschedMax:       defaultTempUnschedMaxDuration,
		retryIntervalStep:    defaultRetryIntervalStep,
		retryIntervalMax:     defaultRetryIntervalMax,
		slowThresholdMs:      defaultSlowThresholdMs,
		slowWindow:           defaultSlowWindowDuration,
		slowMinSampleCount:   defaultSlowMinSampleCount,
		slowBucketMid:        SlowBucketMid,
		slowBucketHigh:       SlowBucketHigh,
		latencyBucketFastMs:  LatencyBucketFast,
		latencyBucketSlowMs:  LatencyBucketSlow,
	}
	if c == nil {
		return r
	}
	if c.HardFilterThreshold > 0 {
		r.hardFilterThreshold = c.HardFilterThreshold
	}
	if c.TempUnschedThreshold > 0 {
		r.tempUnschedThreshold = c.TempUnschedThreshold
	}
	if c.TempUnschedInitMinutes > 0 {
		r.tempUnschedInit = time.Duration(c.TempUnschedInitMinutes) * time.Minute
	}
	if c.TempUnschedMaxMinutes > 0 {
		r.tempUnschedMax = time.Duration(c.TempUnschedMaxMinutes) * time.Minute
	}
	if c.RetryIntervalStepSeconds > 0 {
		r.retryIntervalStep = time.Duration(c.RetryIntervalStepSeconds) * time.Second
	}
	if c.RetryIntervalMaxSeconds > 0 {
		r.retryIntervalMax = time.Duration(c.RetryIntervalMaxSeconds) * time.Second
	}
	if c.SlowThresholdMs > 0 {
		r.slowThresholdMs = c.SlowThresholdMs
	}
	if c.SlowWindowMinutes > 0 {
		r.slowWindow = time.Duration(c.SlowWindowMinutes) * time.Minute
	}
	if c.SlowMinSampleCount > 0 {
		r.slowMinSampleCount = c.SlowMinSampleCount
	}
	if c.SlowBucketMidPct > 0 {
		r.slowBucketMid = float64(c.SlowBucketMidPct) / 100.0
	}
	if c.SlowBucketHighPct > 0 {
		r.slowBucketHigh = float64(c.SlowBucketHighPct) / 100.0
	}
	if c.LatencyBucketFastMs > 0 {
		r.latencyBucketFastMs = c.LatencyBucketFastMs
	}
	if c.LatencyBucketSlowMs > 0 {
		r.latencyBucketSlowMs = c.LatencyBucketSlowMs
	}
	return r
}

// AccountTestHealth 存储单个账号的健康状态
type AccountTestHealth struct {
	mu                  sync.Mutex
	ConsecFails         int
	RetryInterval       time.Duration
	NextRetryAt         time.Time
	TTFTEwma            float64
	SlowReqCount        int
	TotalReqCount       int
	WindowStart         time.Time
	TempUnschedDuration time.Duration
	LastStatus          string
	LastTestedAt        time.Time
}

// AccountTestHealthCache 使用 sync.Map 存储账号健康状态，key 为 accountID int64
type AccountTestHealthCache struct {
	m   sync.Map
	cfg healthCfg
}

// NewAccountTestHealthCache 创建一个新的 AccountTestHealthCache。
// cfg 为 nil 时所有阈值使用内置默认值。
func NewAccountTestHealthCache(cfg *config.AccountHealthConfig) *AccountTestHealthCache {
	return &AccountTestHealthCache{cfg: resolveHealthCfg(cfg)}
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
		h.TempUnschedDuration = 0
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
			h.RetryInterval = c.cfg.retryIntervalStep
		} else {
			h.RetryInterval *= 2
			if h.RetryInterval > c.cfg.retryIntervalMax {
				h.RetryInterval = c.cfg.retryIntervalMax
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

// UpdateDuration 使用滑动窗口统计慢请求
func (c *AccountTestHealthCache) UpdateDuration(accountID int64, durationMs int) {
	h := c.GetOrCreate(accountID)
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()
	if h.WindowStart.IsZero() || now.Sub(h.WindowStart) > c.cfg.slowWindow {
		h.WindowStart = now
		h.SlowReqCount = 0
		h.TotalReqCount = 0
	}

	h.TotalReqCount++
	if durationMs >= c.cfg.slowThresholdMs {
		h.SlowReqCount++
	}
}

// LatencyBucket 返回 TTFT 延时分桶：NaN→1，<4000ms→0，4000-7999ms→1，≥8000ms→2
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
	if h.TTFTEwma < float64(c.cfg.latencyBucketFastMs) {
		return 0
	}
	if h.TTFTEwma < float64(c.cfg.latencyBucketSlowMs) {
		return 1
	}
	return 2
}

// SlowBucket 返回慢请求分桶：样本不足或无数据→0，慢率低→0，中→1，高→2
// 若当前时间已超出窗口，重置统计并返回 0（窗口内无有效样本）。
func (c *AccountTestHealthCache) SlowBucket(accountID int64) int {
	h := c.Get(accountID)
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	// 窗口已过期：清空旧数据，不惩罚账号
	if !h.WindowStart.IsZero() && time.Since(h.WindowStart) > c.cfg.slowWindow {
		h.WindowStart = time.Time{}
		h.SlowReqCount = 0
		h.TotalReqCount = 0
		return 0
	}

	if h.TotalReqCount < c.cfg.slowMinSampleCount {
		return 0
	}

	slowRate := float64(h.SlowReqCount) / float64(h.TotalReqCount)
	if slowRate < c.cfg.slowBucketMid {
		return 0
	}
	if slowRate < c.cfg.slowBucketHigh {
		return 1
	}
	return 2
}

// PassesHardFilter 返回账号是否通过硬过滤
func (c *AccountTestHealthCache) PassesHardFilter(accountID int64) bool {
	h := c.Get(accountID)
	if h == nil {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ConsecFails < c.cfg.hardFilterThreshold
}

// Cfg 暴露已解析的有效配置，供 ScheduledTestRunnerService 等外部组件读取
func (c *AccountTestHealthCache) Cfg() healthCfg {
	return c.cfg
}
