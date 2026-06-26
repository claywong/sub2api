//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUpdateFromTest_SuccessPath(t *testing.T) {
	c := NewAccountTestHealthCache(nil)

	ttft := int64(500)
	c.UpdateFromTest(1, &ScheduledTestResult{Status: "success", FirstTokenMs: &ttft})

	h := c.Get(1)
	require.NotNil(t, h)
	require.Equal(t, "success", h.LastStatus)
}

func TestUpdateFromTest_WritesIntoBucketWindow(t *testing.T) {
	c := NewAccountTestHealthCache(nil)

	ttft := int64(800)
	c.UpdateFromTest(1, &ScheduledTestResult{Status: "success", FirstTokenMs: &ttft})
	c.UpdateFromTest(1, &ScheduledTestResult{Status: "failed"})

	s := c.Snapshot(1)
	require.Equal(t, 2, s.ReqCount)
	require.Equal(t, 1, s.ErrCount)
	require.True(t, s.HasTTFT())
	require.InDelta(t, 800.0, s.TTFTAvg(), 0.001)
}

func TestUpdateFromTest_FailurePath(t *testing.T) {
	c := NewAccountTestHealthCache(nil)
	failResult := &ScheduledTestResult{Status: "failed"}

	c.UpdateFromTest(1, failResult)
	c.UpdateFromTest(1, failResult)
	c.UpdateFromTest(1, failResult)

	s := c.Snapshot(1)
	require.Equal(t, 3, s.ReqCount)
	require.Equal(t, 3, s.ErrCount)

	c.UpdateFromTest(1, &ScheduledTestResult{Status: "success"})
	require.Equal(t, "success", c.Get(1).LastStatus)
}


func TestRecord_AccumulatesIntoSnapshot(t *testing.T) {
	c := NewAccountTestHealthCache(nil)

	c.Record(1, CallSample{Success: true, TTFTMs: 1000, DurationMs: 5000})
	c.Record(1, CallSample{Success: true, TTFTMs: 2000, DurationMs: 25000})
	c.Record(1, CallSample{Success: false})

	s := c.Snapshot(1)
	require.Equal(t, 3, s.ReqCount)
	require.Equal(t, 1, s.ErrCount)
	require.InDelta(t, 1.0/3.0, s.ErrRate(), 1e-6)
	require.Equal(t, 2, s.TTFTSampleCount)
	require.InDelta(t, 1500.0, s.TTFTAvg(), 0.001)
	// slowThresholdMs 默认 20000，2 次 duration 中 25000 算慢请求
	require.Equal(t, 1, s.SlowCount)
	require.InDelta(t, 1.0/3.0, s.SlowRate(), 1e-6)
}

func TestRecord_OTPSStandardFormula(t *testing.T) {
	c := NewAccountTestHealthCache(nil)

	// output_tokens=21, ttft=1000, duration=2000 → decode=1000ms
	// otps = (21-1) * 1000 / 1000 = 20
	c.Record(1, CallSample{Success: true, TTFTMs: 1000, DurationMs: 2000, OutputTokens: 21})

	s := c.Snapshot(1)
	require.True(t, s.HasOTPS())
	require.InDelta(t, 20.0, s.OTPSAvg(), 0.001)
}

func TestRecord_OTPSDropsShortSamples(t *testing.T) {
	c := NewAccountTestHealthCache(nil)

	// output_tokens=5 < 10 → 不计入 OTPS
	c.Record(1, CallSample{Success: true, TTFTMs: 500, DurationMs: 1500, OutputTokens: 5})

	s := c.Snapshot(1)
	require.False(t, s.HasOTPS())
	// 但 TTFT 仍计入
	require.True(t, s.HasTTFT())
}

func TestRecord_OTPSGuardsAgainstDivisionByZero(t *testing.T) {
	c := NewAccountTestHealthCache(nil)

	// duration <= ttft → 不计入 OTPS（避免除零或负值）
	c.Record(1, CallSample{Success: true, TTFTMs: 1000, DurationMs: 1000, OutputTokens: 50})
	c.Record(1, CallSample{Success: true, TTFTMs: 2000, DurationMs: 1000, OutputTokens: 50})

	s := c.Snapshot(1)
	require.False(t, s.HasOTPS())
}

func TestSnapshot_NoEntryReturnsZero(t *testing.T) {
	c := NewAccountTestHealthCache(nil)
	s := c.Snapshot(999)
	require.Equal(t, 0, s.ReqCount)
	require.Equal(t, 0, s.ErrCount)
	require.False(t, s.HasTTFT())
	require.False(t, s.HasOTPS())
	require.Equal(t, 0.0, s.ErrRate())
	require.Equal(t, 0.0, s.SlowRate())
}

func TestSnapshot_ExcludesExpiredBuckets(t *testing.T) {
	c := NewAccountTestHealthCache(nil)
	h := c.GetOrCreate(1)

	// 手动注入一个 11 分钟前的桶
	now := time.Now()
	h.mu.Lock()
	old := &h.buckets[0]
	old.startSec = now.Unix() - 11*60
	old.reqCount = 100
	old.errCount = 50
	h.cursor = 0
	h.mu.Unlock()

	// 当前桶写入新数据
	c.Record(1, CallSample{Success: true})

	s := c.Snapshot(1)
	// 旧桶不应被算入
	require.Equal(t, 1, s.ReqCount)
	require.Equal(t, 0, s.ErrCount)
}

func TestSnapshot_BucketRollOver(t *testing.T) {
	c := NewAccountTestHealthCache(nil)
	h := c.GetOrCreate(1)

	// 注入桶 0 = 现在；桶 1 模拟 1 分钟后
	now := time.Now()
	bucketStart := now.Unix() - (now.Unix() % healthBucketSeconds)
	h.mu.Lock()
	h.buckets[0].startSec = bucketStart
	h.buckets[0].reqCount = 5
	h.cursor = 0
	h.mu.Unlock()

	// Record 应该感知到 cursor 已对齐当前桶，不滚动
	c.Record(1, CallSample{Success: true})

	h.mu.Lock()
	require.Equal(t, 6, h.buckets[0].reqCount, "同一桶内不应滚动")
	h.mu.Unlock()
}

func TestRecordRealCall_WritesWindow(t *testing.T) {
	c := NewAccountTestHealthCache(nil)

	c.RecordRealCall(1, CallSample{Success: false})
	c.RecordRealCall(1, CallSample{Success: false})
	c.RecordRealCall(1, CallSample{Success: true})

	s := c.Snapshot(1)
	require.Equal(t, 3, s.ReqCount)
	require.Equal(t, 2, s.ErrCount)
}

func TestReportRealCall_WritesWindow(t *testing.T) {
	c := NewAccountTestHealthCache(nil)

	c.ReportRealCall(1, false)
	c.ReportRealCall(1, false)
	c.ReportRealCall(1, false)
	c.ReportRealCall(1, true)

	s := c.Snapshot(1)
	require.Equal(t, 4, s.ReqCount)
	require.Equal(t, 3, s.ErrCount)
}

func TestSnapshotPercentageMethods(t *testing.T) {
	s := HealthSnapshot{ReqCount: 0}
	require.Equal(t, 0.0, s.ErrRate())
	require.Equal(t, 0.0, s.SlowRate())

	s = HealthSnapshot{ReqCount: 100, ErrCount: 25, SlowCount: 10}
	require.InDelta(t, 0.25, s.ErrRate(), 1e-9)
	require.InDelta(t, 0.10, s.SlowRate(), 1e-9)
}

func TestCacheHitRate_NoSample(t *testing.T) {
	c := NewAccountTestHealthCache(nil)
	s := c.Snapshot(1)
	require.False(t, s.HasCacheHit())
	require.Equal(t, 0.0, s.CacheHitRateAvg())
}

func TestCacheHitRate_AllTokensZero_NotRecorded(t *testing.T) {
	c := NewAccountTestHealthCache(nil)
	// 三个 token 字段均为 0 → total=0 → 不纳入样本
	c.Record(1, CallSample{Success: true, CacheReadTokens: 0, CacheCreationTokens: 0, InputTokens: 0})
	s := c.Snapshot(1)
	require.Equal(t, 1, s.ReqCount)
	require.False(t, s.HasCacheHit())
}

func TestCacheHitRate_FullHit(t *testing.T) {
	c := NewAccountTestHealthCache(nil)
	// cache_read=200, creation=0, input=0 → total=200, rate=1.0
	c.Record(1, CallSample{Success: true, CacheReadTokens: 200, CacheCreationTokens: 0, InputTokens: 0})
	s := c.Snapshot(1)
	require.True(t, s.HasCacheHit())
	require.InDelta(t, 1.0, s.CacheHitRateAvg(), 1e-9)
}

func TestCacheHitRate_NoHit(t *testing.T) {
	c := NewAccountTestHealthCache(nil)
	// cache_read=0, creation=100, input=100 → total=200, rate=0.0（有样本）
	c.Record(1, CallSample{Success: true, CacheReadTokens: 0, CacheCreationTokens: 100, InputTokens: 100})
	s := c.Snapshot(1)
	require.True(t, s.HasCacheHit())
	require.InDelta(t, 0.0, s.CacheHitRateAvg(), 1e-9)
}

func TestCacheHitRate_TypicalMixed(t *testing.T) {
	c := NewAccountTestHealthCache(nil)
	// cache_read=100, creation=50, input=50 → total=200, rate=0.5
	c.Record(1, CallSample{Success: true, CacheReadTokens: 100, CacheCreationTokens: 50, InputTokens: 50})
	s := c.Snapshot(1)
	require.True(t, s.HasCacheHit())
	require.InDelta(t, 0.5, s.CacheHitRateAvg(), 1e-9)
}

func TestCacheHitRate_AverageMultipleSamples(t *testing.T) {
	c := NewAccountTestHealthCache(nil)
	// 样本1: rate=1.0（全命中）
	c.Record(1, CallSample{Success: true, CacheReadTokens: 200, InputTokens: 0})
	// 样本2: rate=0.0（无命中）
	c.Record(1, CallSample{Success: true, CacheReadTokens: 0, InputTokens: 200})
	s := c.Snapshot(1)
	require.Equal(t, 2, s.CacheHitSampleCount)
	require.InDelta(t, 0.5, s.CacheHitRateAvg(), 1e-9)
}
