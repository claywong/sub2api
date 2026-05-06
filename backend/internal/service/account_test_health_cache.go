package service

import (
	"fmt"
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
)

// 滑动窗口（bucketed rolling window）
const (
	healthBucketCount   = 10 // 10 个桶
	healthBucketSeconds = 60 // 每桶 1 分钟，总窗口 10 分钟

	// DefaultHealthWindowSeconds 滑动窗口总长度（秒），供外部引用。
	DefaultHealthWindowSeconds = healthBucketCount * healthBucketSeconds
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
	return r
}

// metricBucket 单个时间桶，记录一段（healthBucketSeconds）内的累计指标。
type metricBucket struct {
	startSec  int64   // 桶起点 unix 秒；0 表示未使用
	reqCount  int     // 总请求数
	errCount  int     // 失败次数
	slowCount int     // 慢请求数（duration ≥ slowThresholdMs）
	ttftSum   int64   // TTFT 累加（ms）
	ttftN     int     // 有 TTFT 样本的次数
	otpsSum   float64 // OTPS 累加（tokens/s）
	otpsN     int     // 有 OTPS 样本的次数
}

func (b *metricBucket) reset(startSec int64) {
	b.startSec = startSec
	b.reqCount = 0
	b.errCount = 0
	b.slowCount = 0
	b.ttftSum = 0
	b.ttftN = 0
	b.otpsSum = 0
	b.otpsN = 0
}

// CallSample 一次调用（主动 test 或真实 gateway 调用）的样本。
// TTFTMs/DurationMs/OutputTokens 为 0 表示该字段无样本（不计入相应指标）。
type CallSample struct {
	Success      bool
	TTFTMs       int
	DurationMs   int
	OutputTokens int
}

// HealthSnapshot 滑动窗口聚合后的健康指标快照。
type HealthSnapshot struct {
	ReqCount        int
	ErrCount        int
	SlowCount       int
	TTFTSampleCount int
	OTPSSampleCount int
	ttftSumMs       int64
	otpsSum         float64
}

// ErrRate 窗口内错误率（0~1）；ReqCount=0 时返回 0。
func (s HealthSnapshot) ErrRate() float64 {
	if s.ReqCount <= 0 {
		return 0
	}
	return float64(s.ErrCount) / float64(s.ReqCount)
}

// SlowRate 窗口内慢请求率（0~1）。
func (s HealthSnapshot) SlowRate() float64 {
	if s.ReqCount <= 0 {
		return 0
	}
	return float64(s.SlowCount) / float64(s.ReqCount)
}

// TTFTAvg 窗口内 TTFT 平均（ms）；无样本时返回 0。
func (s HealthSnapshot) TTFTAvg() float64 {
	if s.TTFTSampleCount <= 0 {
		return 0
	}
	return float64(s.ttftSumMs) / float64(s.TTFTSampleCount)
}

// OTPSAvg 窗口内 OTPS 平均（tokens/s）；无样本时返回 0。
func (s HealthSnapshot) OTPSAvg() float64 {
	if s.OTPSSampleCount <= 0 {
		return 0
	}
	return s.otpsSum / float64(s.OTPSSampleCount)
}

// HasTTFT 窗口内是否有 TTFT 样本。
func (s HealthSnapshot) HasTTFT() bool { return s.TTFTSampleCount > 0 }

// HasOTPS 窗口内是否有 OTPS 样本。
func (s HealthSnapshot) HasOTPS() bool { return s.OTPSSampleCount > 0 }

// AccountTestHealth 存储单个账号的健康状态。
// 滑动窗口（buckets）记录最近 10 分钟的请求/错误/TTFT/OTPS/慢率聚合数据，
// 主动 test 与真实调用共用同一份窗口（融合视图）。
//
// 退避状态机字段（ConsecFails/RetryInterval/NextRetryAt/TempUnschedDuration）
// 是 ScheduledTestRunner 专用的窗口外状态，不参与 Snapshot 聚合。
type AccountTestHealth struct {
	mu sync.Mutex

	// 滑动窗口（环形 buffer）
	buckets [healthBucketCount]metricBucket
	cursor  int

	// test runner 退避状态机（窗口外）
	ConsecFails         int
	RetryInterval       time.Duration
	NextRetryAt         time.Time
	TempUnschedDuration time.Duration
	LastStatus          string
	LastTestedAt        time.Time

	// lastVerdict 记录上一次 HealthVerdict() 返回值，用于状态变化日志去抖。
	lastVerdict HealthVerdict
}

// HealthVerdict 健康判定三态。复用 WindowCostStickyOnly 的设计语义。
type HealthVerdict int

const (
	// HealthOK 正常调度
	HealthOK HealthVerdict = iota
	// HealthStickyOnly 仅允许粘性会话（已粘上的 session 继续，新会话避开）
	HealthStickyOnly
	// HealthExcluded 完全排除（含 sticky 也踢出）
	HealthExcluded
)

// String 返回 verdict 的可读名称。
func (v HealthVerdict) String() string {
	switch v {
	case HealthOK:
		return "OK"
	case HealthStickyOnly:
		return "StickyOnly"
	case HealthExcluded:
		return "Excluded"
	default:
		return "Unknown"
	}
}

// HealthVerdictConfig 调用 HealthVerdict() 时由 gateway service 传入的阈值。
// 基于滑动窗口快照（HealthSnapshot）做三态判定。
type HealthVerdictConfig struct {
	WindowSeconds     int64   // 评估窗口长度，<=0 时使用默认 healthBucketCount*healthBucketSeconds
	MinSamples        int     // 触发判定的最小样本数；窗口内 reqCount 不足时返回 OK
	ErrCountSoft      int     // 错误数 ≥ 此值 → StickyOnly
	ErrCountHard      int     // 错误数 ≥ 此值 → Excluded
	ErrRateSoft       float64 // 错误率 ≥ 此值 → StickyOnly
	ErrRateHard       float64 // 错误率 ≥ 此值 → Excluded
	TTFTStickyOnlyMs  int     // TTFTAvg ≥ 此值 → StickyOnly
	OTPSStickyOnlyMin float64 // OTPSAvg < 此值（且有样本）→ StickyOnly
}

// AccountTestHealthCache 使用 sync.Map 存储账号健康状态，key 为 accountID int64
type AccountTestHealthCache struct {
	m              sync.Map
	cfg            healthCfg
	verdictCfg     HealthVerdictConfig
	// OnVerdictChange 在账号健康状态发生切换时调用（异步，不阻塞调度路径）。
	// 参数：accountID、切换前状态、切换后状态。
	OnVerdictChange func(accountID int64, prev, current HealthVerdict)
}

// NewAccountTestHealthCache 创建一个新的 AccountTestHealthCache。
// cfg 为 nil 时所有阈值使用内置默认值。
func NewAccountTestHealthCache(cfg *config.AccountHealthConfig) *AccountTestHealthCache {
	return &AccountTestHealthCache{
		cfg:        resolveHealthCfg(cfg),
		verdictCfg: defaultHealthVerdictConfig(),
	}
}

// SetVerdictConfig 更新三态判定阈值，由 GatewayService 在初始化后调用以注入运行时配置。
// 配置为静态加载，只需在启动时设置一次。
func (c *AccountTestHealthCache) SetVerdictConfig(cfg HealthVerdictConfig) {
	c.verdictCfg = cfg
}

// Get 返回指定账号的健康状态，不存在时返回 nil
func (c *AccountTestHealthCache) Get(accountID int64) *AccountTestHealth {
	v, ok := c.m.Load(accountID)
	if !ok {
		return nil
	}
	return v.(*AccountTestHealth)
}

// GetOrCreate 返回指定账号的健康状态，不存在时创建一个新的。
// 先 Load 再 LoadOrStore，避免在热路径上每次都分配临时对象。
func (c *AccountTestHealthCache) GetOrCreate(accountID int64) *AccountTestHealth {
	if v, ok := c.m.Load(accountID); ok {
		return v.(*AccountTestHealth)
	}
	h := &AccountTestHealth{}
	actual, _ := c.m.LoadOrStore(accountID, h)
	return actual.(*AccountTestHealth)
}

// advanceBucketLocked 将 cursor 推进到 now 所属的桶。需在 h.mu.Lock 内调用。
// 返回当前活动桶的指针。
func (h *AccountTestHealth) advanceBucketLocked(now time.Time) *metricBucket {
	nowSec := now.Unix()
	bucketStart := nowSec - (nowSec % healthBucketSeconds)
	cur := &h.buckets[h.cursor]
	if cur.startSec == bucketStart {
		return cur
	}
	// 推进 cursor 一格，无论 idle 多久，环形 buffer 自然滚动
	h.cursor = (h.cursor + 1) % healthBucketCount
	next := &h.buckets[h.cursor]
	next.reset(bucketStart)
	return next
}

// snapshotLocked 聚合窗口内所有桶。需在 h.mu.Lock 内调用。
func (h *AccountTestHealth) snapshotLocked(now time.Time, windowSec int64) HealthSnapshot {
	if windowSec <= 0 {
		windowSec = healthBucketCount * healthBucketSeconds
	}
	cutoff := now.Unix() - windowSec
	var s HealthSnapshot
	for i := range h.buckets {
		b := &h.buckets[i]
		if b.startSec == 0 || b.startSec < cutoff {
			continue
		}
		s.ReqCount += b.reqCount
		s.ErrCount += b.errCount
		s.SlowCount += b.slowCount
		s.ttftSumMs += b.ttftSum
		s.TTFTSampleCount += b.ttftN
		s.otpsSum += b.otpsSum
		s.OTPSSampleCount += b.otpsN
	}
	return s
}

// recordSampleLocked 把一次 sample 写入活动桶。需在 h.mu.Lock 内调用。
// 计算 OTPS 时遵循业内标准：(output_tokens-1) * 1000 / (duration_ms - ttft_ms)，
// 仅在 OutputTokens >= 10 且 DurationMs > TTFTMs > 0 时纳入样本。
func (h *AccountTestHealth) recordSampleLocked(sample CallSample, now time.Time, slowThresholdMs int) {
	b := h.advanceBucketLocked(now)
	b.reqCount++
	if !sample.Success {
		b.errCount++
	}
	if sample.TTFTMs > 0 {
		b.ttftSum += int64(sample.TTFTMs)
		b.ttftN++
	}
	if sample.DurationMs > 0 && slowThresholdMs > 0 && sample.DurationMs >= slowThresholdMs {
		b.slowCount++
	}
	// OTPS：业内标准 (output_tokens - 1) * 1000 / (duration - ttft)
	// 过滤条件：output ≥ 10、duration > ttft（避免除零或负值）。
	if sample.OutputTokens >= 10 && sample.TTFTMs > 0 && sample.DurationMs > sample.TTFTMs {
		decodeMs := sample.DurationMs - sample.TTFTMs
		otps := float64(sample.OutputTokens-1) * 1000.0 / float64(decodeMs)
		if otps > 0 && !math.IsNaN(otps) && !math.IsInf(otps, 0) {
			b.otpsSum += otps
			b.otpsN++
		}
	}
}

// Record 上报一次调用样本（主动 test 或真实 gateway 调用）。
// 这是窗口数据的统一入口；ConsecFails 由 UpdateFromTest/ReportRealCall 各自维护。
func (c *AccountTestHealthCache) Record(accountID int64, sample CallSample) {
	if c == nil || accountID <= 0 {
		return
	}
	h := c.GetOrCreate(accountID)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recordSampleLocked(sample, time.Now(), c.cfg.slowThresholdMs)
}

// Snapshot 返回窗口聚合的健康指标快照。
// windowSeconds <= 0 时使用默认 600s。
func (c *AccountTestHealthCache) Snapshot(accountID int64) HealthSnapshot {
	if c == nil || accountID <= 0 {
		return HealthSnapshot{}
	}
	h := c.Get(accountID)
	if h == nil {
		return HealthSnapshot{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshotLocked(time.Now(), healthBucketCount*healthBucketSeconds)
}

// UpdateFromTest 根据测试结果更新健康状态。
// 同时维护：(a) test 退避状态机（ConsecFails/RetryInterval/NextRetryAt）；
// (b) 滑动窗口（通过 Record-语义写入活动桶，与真实调用融合）。
func (c *AccountTestHealthCache) UpdateFromTest(accountID int64, result *ScheduledTestResult) {
	if result == nil {
		return
	}
	h := c.GetOrCreate(accountID)
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()
	h.LastTestedAt = now
	h.LastStatus = result.Status

	sample := CallSample{Success: result.Status == "success"}
	if result.FirstTokenMs != nil && *result.FirstTokenMs > 0 {
		sample.TTFTMs = int(*result.FirstTokenMs)
	}
	// ScheduledTestResult 当前未携带 duration/output tokens；OTPS 待 test runner 后续扩展。
	h.recordSampleLocked(sample, now, c.cfg.slowThresholdMs)

	if sample.Success {
		h.ConsecFails = 0
		h.RetryInterval = 0
		h.TempUnschedDuration = 0
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
		h.NextRetryAt = now.Add(h.RetryInterval)
	}
	c.checkVerdictChangeLocked(accountID, h)
}

// checkVerdictChangeLocked 在写入数据后检查 verdict 是否变化，变化则异步触发 OnVerdictChange。
// 必须在 h.mu.Lock() 内调用。
func (c *AccountTestHealthCache) checkVerdictChangeLocked(accountID int64, h *AccountTestHealth) {
	if c.OnVerdictChange == nil {
		return
	}
	cfg := defaultHealthVerdictConfig()
	s := h.snapshotLocked(time.Now(), cfg.WindowSeconds)
	current := verdictFromSnapshot(s, cfg)
	prev := h.lastVerdict
	if prev == current {
		return
	}
	h.lastVerdict = current
	go c.OnVerdictChange(accountID, prev, current)
}

// verdictFromSnapshot 根据快照和配置计算三态判定，避免在锁内重复调用带锁的 HealthVerdict。
func verdictFromSnapshot(s HealthSnapshot, cfg HealthVerdictConfig) HealthVerdict {
	if cfg.MinSamples > 0 && s.ReqCount < cfg.MinSamples {
		return HealthOK
	}
	if cfg.ErrCountHard > 0 && s.ErrCount >= cfg.ErrCountHard {
		return HealthExcluded
	}
	if cfg.ErrRateHard > 0 && s.ErrRate() >= cfg.ErrRateHard {
		return HealthExcluded
	}
	if cfg.ErrCountSoft > 0 && s.ErrCount >= cfg.ErrCountSoft {
		return HealthStickyOnly
	}
	if cfg.ErrRateSoft > 0 && s.ErrRate() >= cfg.ErrRateSoft {
		return HealthStickyOnly
	}
	if cfg.TTFTStickyOnlyMs > 0 && s.HasTTFT() && s.TTFTAvg() >= float64(cfg.TTFTStickyOnlyMs) {
		return HealthStickyOnly
	}
	if cfg.OTPSStickyOnlyMin > 0 && s.HasOTPS() && s.OTPSAvg() < cfg.OTPSStickyOnlyMin {
		return HealthStickyOnly
	}
	return HealthOK
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

// ReportRealCall 记录一次真实业务调用结果（gateway 层，区别于主动 test）。
// 内部调用 Record 写入窗口桶，并维护 ConsecFails（成功清零、失败累加）。
//
// 行为：
//   - success=true 时，如 ConsecFails > 0 则清零（真实流量验证账号可用，重置 test 累计的失败）
//   - success=false 时 ConsecFails++
//
// 调用方应在上报前过滤 context.Canceled / 客户端中断等"非账号问题"，避免误报。
func (c *AccountTestHealthCache) ReportRealCall(accountID int64, success bool) {
	c.RecordRealCall(accountID, CallSample{Success: success})
}

// RecordRealCall 真实调用上报的扩展版本，可同时携带 TTFT/Duration/OutputTokens。
// 与 ReportRealCall 行为一致：写入窗口桶 + 维护 ConsecFails。
func (c *AccountTestHealthCache) RecordRealCall(accountID int64, sample CallSample) {
	if c == nil || accountID <= 0 {
		return
	}
	h := c.GetOrCreate(accountID)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recordSampleLocked(sample, time.Now(), c.cfg.slowThresholdMs)
	if sample.Success {
		if h.ConsecFails > 0 {
			h.ConsecFails = 0
		}
	} else {
		h.ConsecFails++
	}
}

// HealthVerdict 基于滑动窗口快照返回三态判定结果。
//
// 优先级：Hard > Soft > OK。窗口样本数不足 minSamples 时返回 OK（避免新账号被误判）。
//
// 配套的 gateway 层封装 isAccountSchedulableForHealth(account, isSticky) 应根据
// 三态返回值决定是否放行：
//   - HealthOK         → 任何路径放行
//   - HealthStickyOnly → 仅 isSticky=true 路径放行
//   - HealthExcluded   → 全部拦截
func (c *AccountTestHealthCache) HealthVerdict(accountID int64, cfg HealthVerdictConfig) HealthVerdict {
	if c == nil || accountID <= 0 {
		return HealthOK
	}
	h := c.Get(accountID)
	if h == nil {
		return HealthOK
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	windowSec := cfg.WindowSeconds
	if windowSec <= 0 {
		windowSec = healthBucketCount * healthBucketSeconds
	}
	s := h.snapshotLocked(time.Now(), windowSec)

	// 样本数不足：除了 ConsecFails 外不做"窗口"判定
	if cfg.MinSamples > 0 && s.ReqCount < cfg.MinSamples {
		return HealthOK
	}

	// Hard 阈值：彻底排除
	if cfg.ErrCountHard > 0 && s.ErrCount >= cfg.ErrCountHard {
		return HealthExcluded
	}
	if cfg.ErrRateHard > 0 && s.ErrRate() >= cfg.ErrRateHard {
		return HealthExcluded
	}

	// Soft 阈值：仅粘性
	if cfg.ErrCountSoft > 0 && s.ErrCount >= cfg.ErrCountSoft {
		return HealthStickyOnly
	}
	if cfg.ErrRateSoft > 0 && s.ErrRate() >= cfg.ErrRateSoft {
		return HealthStickyOnly
	}
	if cfg.TTFTStickyOnlyMs > 0 && s.HasTTFT() && s.TTFTAvg() >= float64(cfg.TTFTStickyOnlyMs) {
		return HealthStickyOnly
	}
	if cfg.OTPSStickyOnlyMin > 0 && s.HasOTPS() && s.OTPSAvg() < cfg.OTPSStickyOnlyMin {
		return HealthStickyOnly
	}

	return HealthOK
}

func defaultHealthVerdictConfig() HealthVerdictConfig {
	return HealthVerdictConfig{
		WindowSeconds:     10 * 60,
		MinSamples:        5,
		ErrCountSoft:      3,
		ErrCountHard:      5,
		ErrRateSoft:       0.3,
		ErrRateHard:       0.5,
		TTFTStickyOnlyMs:  10000,
		OTPSStickyOnlyMin: 20,
	}
}

// HealthVerdictWithDefaults 使用内置默认阈值计算健康三态，等价于 gateway 调度时的判断结果。
// 供 admin handler 等无法获取调度配置的调用方使用。
func (c *AccountTestHealthCache) HealthVerdictWithDefaults(accountID int64) HealthVerdict {
	return c.HealthVerdict(accountID, defaultHealthVerdictConfig())
}

// HealthVerdictWithReason 在 HealthVerdictWithDefaults 基础上同时返回触发原因描述。
// reason 格式示例："err_rate=45.0%(≥30%)" / "err_count=12(≥10)" / "ttft_avg=11200ms(≥10000ms)"
// verdict 为 HealthOK 时 reason 为空字符串。
func (c *AccountTestHealthCache) HealthVerdictWithReason(accountID int64) (verdict HealthVerdict, reason string) {
	if c == nil || accountID <= 0 {
		return HealthOK, ""
	}
	cfg := c.verdictCfg
	h := c.Get(accountID)
	if h == nil {
		return HealthOK, ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	windowSec := cfg.WindowSeconds
	if windowSec <= 0 {
		windowSec = healthBucketCount * healthBucketSeconds
	}
	s := h.snapshotLocked(time.Now(), windowSec)

	if cfg.MinSamples > 0 && s.ReqCount < cfg.MinSamples {
		return HealthOK, ""
	}

	if cfg.ErrCountHard > 0 && s.ErrCount >= cfg.ErrCountHard {
		return HealthExcluded, fmt.Sprintf("err_count=%d(≥%d)", s.ErrCount, cfg.ErrCountHard)
	}
	if cfg.ErrRateHard > 0 && s.ErrRate() >= cfg.ErrRateHard {
		return HealthExcluded, fmt.Sprintf("err_rate=%.1f%%(≥%.0f%%)", s.ErrRate()*100, cfg.ErrRateHard*100)
	}
	if cfg.ErrCountSoft > 0 && s.ErrCount >= cfg.ErrCountSoft {
		return HealthStickyOnly, fmt.Sprintf("err_count=%d(≥%d)", s.ErrCount, cfg.ErrCountSoft)
	}
	if cfg.ErrRateSoft > 0 && s.ErrRate() >= cfg.ErrRateSoft {
		return HealthStickyOnly, fmt.Sprintf("err_rate=%.1f%%(≥%.0f%%)", s.ErrRate()*100, cfg.ErrRateSoft*100)
	}
	if cfg.TTFTStickyOnlyMs > 0 && s.HasTTFT() && s.TTFTAvg() >= float64(cfg.TTFTStickyOnlyMs) {
		return HealthStickyOnly, fmt.Sprintf("ttft_avg=%.0fms(≥%dms)", s.TTFTAvg(), cfg.TTFTStickyOnlyMs)
	}
	if cfg.OTPSStickyOnlyMin > 0 && s.HasOTPS() && s.OTPSAvg() < cfg.OTPSStickyOnlyMin {
		return HealthStickyOnly, fmt.Sprintf("otps_avg=%.1f(<%g)", s.OTPSAvg(), cfg.OTPSStickyOnlyMin)
	}
	return HealthOK, ""
}

// SnapshotAndVerdict 一次加锁同时返回滑动窗口快照和健康三态判定，避免两次独立调用的重复计算。
// 使用运行时配置（由 SetVerdictConfig 注入），与实际调度行为一致。
func (c *AccountTestHealthCache) SnapshotAndVerdict(accountID int64) (snap HealthSnapshot, verdict HealthVerdict, reason string) {
	return c.SnapshotAndVerdictWithConfig(accountID, c.verdictCfg)
}

// SnapshotAndVerdictWithConfig 与 SnapshotAndVerdict 相同，但使用调用方传入的配置。
// gateway 层在打状态切换日志时应使用此方法，确保 reason 与 HealthVerdictWithChange 的判定一致。
func (c *AccountTestHealthCache) SnapshotAndVerdictWithConfig(accountID int64, cfg HealthVerdictConfig) (snap HealthSnapshot, verdict HealthVerdict, reason string) {
	if c == nil || accountID <= 0 {
		return HealthSnapshot{}, HealthOK, ""
	}
	h := c.Get(accountID)
	if h == nil {
		return HealthSnapshot{}, HealthOK, ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	windowSec := cfg.WindowSeconds
	if windowSec <= 0 {
		windowSec = healthBucketCount * healthBucketSeconds
	}
	snap = h.snapshotLocked(time.Now(), windowSec)

	if cfg.MinSamples > 0 && snap.ReqCount < cfg.MinSamples {
		return snap, HealthOK, ""
	}
	if cfg.ErrCountHard > 0 && snap.ErrCount >= cfg.ErrCountHard {
		return snap, HealthExcluded, fmt.Sprintf("err_count=%d(≥%d)", snap.ErrCount, cfg.ErrCountHard)
	}
	if cfg.ErrRateHard > 0 && snap.ErrRate() >= cfg.ErrRateHard {
		return snap, HealthExcluded, fmt.Sprintf("err_rate=%.1f%%(≥%.0f%%)", snap.ErrRate()*100, cfg.ErrRateHard*100)
	}
	if cfg.ErrCountSoft > 0 && snap.ErrCount >= cfg.ErrCountSoft {
		return snap, HealthStickyOnly, fmt.Sprintf("err_count=%d(≥%d)", snap.ErrCount, cfg.ErrCountSoft)
	}
	if cfg.ErrRateSoft > 0 && snap.ErrRate() >= cfg.ErrRateSoft {
		return snap, HealthStickyOnly, fmt.Sprintf("err_rate=%.1f%%(≥%.0f%%)", snap.ErrRate()*100, cfg.ErrRateSoft*100)
	}
	if cfg.TTFTStickyOnlyMs > 0 && snap.HasTTFT() && snap.TTFTAvg() >= float64(cfg.TTFTStickyOnlyMs) {
		return snap, HealthStickyOnly, fmt.Sprintf("ttft_avg=%.0fms(≥%dms)", snap.TTFTAvg(), cfg.TTFTStickyOnlyMs)
	}
	if cfg.OTPSStickyOnlyMin > 0 && snap.HasOTPS() && snap.OTPSAvg() < cfg.OTPSStickyOnlyMin {
		return snap, HealthStickyOnly, fmt.Sprintf("otps_avg=%.1f(<%g)", snap.OTPSAvg(), cfg.OTPSStickyOnlyMin)
	}
	return snap, HealthOK, ""
}

// HealthVerdictWithChange 在 HealthVerdict() 基础上返回是否发生状态切换。
// gateway 层可据此打"状态变化"日志，避免重复刷屏。
// 第二个返回值 prev 是切换前的旧 verdict（仅在 changed=true 时有意义）。
func (c *AccountTestHealthCache) HealthVerdictWithChange(accountID int64, cfg HealthVerdictConfig) (current HealthVerdict, prev HealthVerdict, changed bool) {
	current = c.HealthVerdict(accountID, cfg)
	if c == nil || accountID <= 0 {
		return current, HealthOK, false
	}
	h := c.Get(accountID)
	if h == nil {
		return current, HealthOK, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	prev = h.lastVerdict
	if prev != current {
		h.lastVerdict = current
		if c.OnVerdictChange != nil {
			go c.OnVerdictChange(accountID, prev, current)
		}
		return current, prev, true
	}
	return current, prev, false
}

// Cfg 暴露已解析的有效配置，供 ScheduledTestRunnerService 等外部组件读取
func (c *AccountTestHealthCache) Cfg() healthCfg {
	return c.cfg
}

// Reset 清除指定账号的健康缓存（滑动窗口 + ConsecFails），使 HealthVerdict 立即返回 OK。
// 供 admin 手动恢复 Excluded/StickyOnly 状态时调用。
func (c *AccountTestHealthCache) Reset(accountID int64) {
	if c == nil || accountID <= 0 {
		return
	}
	c.m.Delete(accountID)
}

// ListTrackedAccountIDs 返回当前有记录的所有账号 ID（仅供 admin 监控接口使用）。
// 不保证顺序；返回快照之后新写入的不影响结果。
func (c *AccountTestHealthCache) ListTrackedAccountIDs() []int64 {
	if c == nil {
		return nil
	}
	out := make([]int64, 0, 16)
	c.m.Range(func(key, _ any) bool {
		if id, ok := key.(int64); ok {
			out = append(out, id)
		}
		return true
	})
	return out
}
