//go:build unit

package service

import (
	"math"
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
	// TTFTEwma 直接赋值（之前为 NaN）
	require.InDelta(t, float64(ttft), h.TTFTEwma, 0.001)
}

func TestUpdateFromTest_SuccessEWMA(t *testing.T) {
	c := NewAccountTestHealthCache(nil)

	// 首次 success：直接赋值
	ttft1 := int64(800)
	c.UpdateFromTest(1, &ScheduledTestResult{Status: "success", FirstTokenMs: &ttft1})
	h := c.Get(1)
	h.mu.Lock()
	require.InDelta(t, 800.0, h.TTFTEwma, 0.001)
	h.mu.Unlock()

	// 第二次 success：EWMA 更新
	ttft2 := int64(200)
	c.UpdateFromTest(1, &ScheduledTestResult{Status: "success", FirstTokenMs: &ttft2})
	h.mu.Lock()
	expected := TTFTEwmaAlpha*200.0 + (1-TTFTEwmaAlpha)*800.0
	require.InDelta(t, expected, h.TTFTEwma, 0.001)
	h.mu.Unlock()
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

func TestUpdateTTFT_EWMA(t *testing.T) {
	c := NewAccountTestHealthCache(nil)

	// 首次直接赋值
	c.UpdateTTFT(1, 1000)
	h := c.Get(1)
	h.mu.Lock()
	require.InDelta(t, 1000.0, h.TTFTEwma, 0.001)
	h.mu.Unlock()

	// 后续 EWMA: 0.3*sample + 0.7*ewma
	c.UpdateTTFT(1, 400)
	h.mu.Lock()
	expected := 0.3*400.0 + 0.7*1000.0
	require.InDelta(t, expected, h.TTFTEwma, 0.001)
	h.mu.Unlock()
}

func TestLatencyBucket(t *testing.T) {
	tests := []struct {
		name     string
		ttftEwma float64
		want     int
	}{
		{"NaN returns 1", math.NaN(), 1},
		{"<1000ms returns 0", 999.9, 0},
		{"exactly 1000ms returns 1", 1000.0, 1},
		{"1000-2999ms returns 1", 2000.0, 1},
		{"exactly 3000ms returns 2", 3000.0, 2},
		{">3000ms returns 2", 5000.0, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewAccountTestHealthCache(nil)
			h := c.GetOrCreate(1)
			h.mu.Lock()
			h.TTFTEwma = tt.ttftEwma
			h.mu.Unlock()
			require.Equal(t, tt.want, c.LatencyBucket(1))
		})
	}

	t.Run("no data returns 1", func(t *testing.T) {
		c := NewAccountTestHealthCache(nil)
		require.Equal(t, 1, c.LatencyBucket(999))
	})
}

func TestUpdateDuration_WindowExpiry(t *testing.T) {
	c := NewAccountTestHealthCache(nil)

	// 记录几次慢请求
	c.UpdateDuration(1, 70000) // slow
	c.UpdateDuration(1, 70000) // slow
	c.UpdateDuration(1, 10000) // fast

	h := c.Get(1)
	h.mu.Lock()
	require.Equal(t, 3, h.TotalReqCount)
	require.Equal(t, 2, h.SlowReqCount)

	// 手动将 WindowStart 设置为超过 10min 前，模拟窗口过期
	h.WindowStart = time.Now().Add(-(SlowWindowDuration + time.Second))
	h.mu.Unlock()

	// 新请求进来，窗口应重置
	c.UpdateDuration(1, 5000)

	h.mu.Lock()
	defer h.mu.Unlock()
	require.Equal(t, 1, h.TotalReqCount, "窗口过期后应重置为 1")
	require.Equal(t, 0, h.SlowReqCount, "窗口过期后慢请求数应清零")
}

func TestSlowBucket(t *testing.T) {
	t.Run("no data returns 0", func(t *testing.T) {
		c := NewAccountTestHealthCache(nil)
		require.Equal(t, 0, c.SlowBucket(999))
	})

	t.Run("sample < 5 returns 0", func(t *testing.T) {
		c := NewAccountTestHealthCache(nil)
		for i := 0; i < 4; i++ {
			c.UpdateDuration(1, 70000) // all slow
		}
		require.Equal(t, 0, c.SlowBucket(1))
	})

	t.Run("slow rate < 20% returns 0", func(t *testing.T) {
		c := NewAccountTestHealthCache(nil)
		c.UpdateDuration(1, 70000) // slow
		for i := 0; i < 9; i++ {
			c.UpdateDuration(1, 1000) // fast
		}
		// 1/10 = 10% < 20%
		require.Equal(t, 0, c.SlowBucket(1))
	})

	t.Run("slow rate 20%-49% returns 1", func(t *testing.T) {
		c := NewAccountTestHealthCache(nil)
		for i := 0; i < 3; i++ {
			c.UpdateDuration(1, 70000) // slow
		}
		for i := 0; i < 7; i++ {
			c.UpdateDuration(1, 1000) // fast
		}
		// 3/10 = 30%
		require.Equal(t, 1, c.SlowBucket(1))
	})

	t.Run("slow rate >= 50% returns 2", func(t *testing.T) {
		c := NewAccountTestHealthCache(nil)
		for i := 0; i < 5; i++ {
			c.UpdateDuration(1, 70000) // slow
		}
		for i := 0; i < 5; i++ {
			c.UpdateDuration(1, 1000) // fast
		}
		// 5/10 = 50%
		require.Equal(t, 2, c.SlowBucket(1))
	})

	t.Run("expired window returns 0 and resets counters", func(t *testing.T) {
		c := NewAccountTestHealthCache(nil)
		// 注入足够样本使 SlowBucket 返回 2
		for i := 0; i < 5; i++ {
			c.UpdateDuration(1, 70000)
		}
		require.Equal(t, 2, c.SlowBucket(1))

		// 手动将 WindowStart 拨到窗口之前
		h := c.GetOrCreate(1)
		h.mu.Lock()
		h.WindowStart = time.Now().Add(-(SlowWindowDuration + time.Second))
		h.mu.Unlock()

		// 窗口已过期：应返回 0，并清空计数
		require.Equal(t, 0, c.SlowBucket(1))
		h.mu.Lock()
		require.Equal(t, 0, h.TotalReqCount)
		require.Equal(t, 0, h.SlowReqCount)
		require.True(t, h.WindowStart.IsZero())
		h.mu.Unlock()
	})
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

func TestFilterByMinLatencyBucket(t *testing.T) {
	t.Run("empty slice", func(t *testing.T) {
		c := NewAccountTestHealthCache(nil)
		result := filterByMinLatencyBucket(nil, c)
		require.Empty(t, result)
	})

	t.Run("all same bucket returns all", func(t *testing.T) {
		c := NewAccountTestHealthCache(nil)
		accounts := []accountWithLoad{
			{account: &Account{ID: 1}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 2}, loadInfo: &AccountLoadInfo{}},
		}
		// 无数据时 LatencyBucket 均为 1
		result := filterByMinLatencyBucket(accounts, c)
		require.Len(t, result, 2)
	})

	t.Run("filters to min latency bucket", func(t *testing.T) {
		c := NewAccountTestHealthCache(nil)
		// account 1: fast bucket=0
		c.UpdateTTFT(1, 500)
		// account 2: mid bucket=1 (NaN)
		// account 3: slow bucket=2
		h3 := c.GetOrCreate(3)
		h3.mu.Lock()
		h3.TTFTEwma = 4000.0
		h3.mu.Unlock()

		accounts := []accountWithLoad{
			{account: &Account{ID: 1}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 2}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 3}, loadInfo: &AccountLoadInfo{}},
		}
		result := filterByMinLatencyBucket(accounts, c)
		require.Len(t, result, 1)
		require.Equal(t, int64(1), result[0].account.ID)
	})

	t.Run("empty result falls back to original", func(t *testing.T) {
		c := NewAccountTestHealthCache(nil)
		accounts := []accountWithLoad{
			{account: &Account{ID: 1}, loadInfo: &AccountLoadInfo{}},
		}
		result := filterByMinLatencyBucket(accounts, c)
		require.Len(t, result, 1)
	})
}

func TestFilterByMinSlowBucket(t *testing.T) {
	t.Run("empty slice", func(t *testing.T) {
		c := NewAccountTestHealthCache(nil)
		result := filterByMinSlowBucket(nil, c)
		require.Empty(t, result)
	})

	t.Run("all same slow bucket returns all", func(t *testing.T) {
		c := NewAccountTestHealthCache(nil)
		accounts := []accountWithLoad{
			{account: &Account{ID: 1}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 2}, loadInfo: &AccountLoadInfo{}},
		}
		// 无数据时 SlowBucket 均为 0
		result := filterByMinSlowBucket(accounts, c)
		require.Len(t, result, 2)
	})

	t.Run("filters to min slow bucket", func(t *testing.T) {
		c := NewAccountTestHealthCache(nil)
		// account 1: slow bucket=0（无数据）
		// account 2: slow bucket=2（50% 慢请求）
		for i := 0; i < 5; i++ {
			c.UpdateDuration(2, 70000)
		}
		for i := 0; i < 5; i++ {
			c.UpdateDuration(2, 1000)
		}

		accounts := []accountWithLoad{
			{account: &Account{ID: 1}, loadInfo: &AccountLoadInfo{}},
			{account: &Account{ID: 2}, loadInfo: &AccountLoadInfo{}},
		}
		result := filterByMinSlowBucket(accounts, c)
		require.Len(t, result, 1)
		require.Equal(t, int64(1), result[0].account.ID)
	})
}
