//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUpdateFromTest_SuccessPath(t *testing.T) {
	c := NewAccountTestHealthCache(nil)

	// 先制造两次失败
	failResult := &ScheduledTestResult{Status: "failed"}
	c.UpdateFromTest(1, failResult)
	c.UpdateFromTest(1, failResult)

	h := c.Get(1)
	require.NotNil(t, h)
	require.Equal(t, 2, h.ConsecFails)
	require.True(t, h.RetryInterval > 0)

	// 成功后 ConsecFails 归零，RetryInterval 重置
	ttft := int64(500)
	successResult := &ScheduledTestResult{Status: "success", FirstTokenMs: &ttft}
	c.UpdateFromTest(1, successResult)

	h.mu.Lock()
	defer h.mu.Unlock()
	require.Equal(t, 0, h.ConsecFails)
	require.Equal(t, time.Duration(0), h.RetryInterval)
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

	// 第 1 次失败
	c.UpdateFromTest(1, failResult)
	h := c.Get(1)
	h.mu.Lock()
	require.Equal(t, 1, h.ConsecFails)
	require.Equal(t, RetryIntervalStep, h.RetryInterval)
	h.mu.Unlock()

	// 第 2 次失败：RetryInterval 翻倍
	c.UpdateFromTest(1, failResult)
	h.mu.Lock()
	require.Equal(t, 2, h.ConsecFails)
	require.Equal(t, RetryIntervalStep*2, h.RetryInterval)
	h.mu.Unlock()

	// 第 3 次失败：ConsecFails=3，RetryInterval 翻倍
	c.UpdateFromTest(1, failResult)
	h.mu.Lock()
	require.Equal(t, 3, h.ConsecFails)
	require.Equal(t, RetryIntervalStep*4, h.RetryInterval)
	h.mu.Unlock()

	// 多次失败后 RetryInterval 不超过上限
	for i := 0; i < 10; i++ {
		c.UpdateFromTest(1, failResult)
	}
	h.mu.Lock()
	require.LessOrEqual(t, h.RetryInterval, RetryIntervalMax)
	h.mu.Unlock()

	// 手动模拟触发过隔离后 TempUnschedDuration 被设置
	h.mu.Lock()
	h.TempUnschedDuration = TempUnschedInitDuration * 4
	h.mu.Unlock()

	// 成功后三个退避字段全部归零
	c.UpdateFromTest(1, &ScheduledTestResult{Status: "success"})
	h.mu.Lock()
	require.Equal(t, 0, h.ConsecFails)
	require.Equal(t, time.Duration(0), h.RetryInterval)
	require.Equal(t, time.Duration(0), h.TempUnschedDuration)
	h.mu.Unlock()
}

func TestPassesHardFilter(t *testing.T) {
	c := NewAccountTestHealthCache(nil)

	// 无数据：通过
	require.True(t, c.PassesHardFilter(1))

	// ConsecFails=1：通过
	c.UpdateFromTest(1, &ScheduledTestResult{Status: "failed"})
	require.True(t, c.PassesHardFilter(1))

	// ConsecFails=2：不通过
	c.UpdateFromTest(1, &ScheduledTestResult{Status: "failed"})
	require.False(t, c.PassesHardFilter(1))

	// ConsecFails=3：不通过
	c.UpdateFromTest(1, &ScheduledTestResult{Status: "failed"})
	require.False(t, c.PassesHardFilter(1))
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

func TestRecordRealCall_TracksConsecFails(t *testing.T) {
	c := NewAccountTestHealthCache(nil)

	c.RecordRealCall(1, CallSample{Success: false})
	c.RecordRealCall(1, CallSample{Success: false})
	require.Equal(t, 2, c.Get(1).ConsecFails)

	c.RecordRealCall(1, CallSample{Success: true})
	require.Equal(t, 0, c.Get(1).ConsecFails)
}

func TestReportRealCall_BackwardsCompatible(t *testing.T) {
	c := NewAccountTestHealthCache(nil)

	c.ReportRealCall(1, false)
	c.ReportRealCall(1, false)
	c.ReportRealCall(1, false)
	require.Equal(t, 3, c.Get(1).ConsecFails)

	c.ReportRealCall(1, true)
	require.Equal(t, 0, c.Get(1).ConsecFails)

	// ReportRealCall 也应该写入窗口（每次都计为 reqCount）
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
