//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 默认窗口判定配置：min_samples=5, err 软=5/硬=10、err 率软=0.3/硬=0.5、TTFT 8000ms。
// 与生产默认值（resolvedSchedulingHealth 默认）一致。
func testHealthVerdictConfig() HealthVerdictConfig {
	return HealthVerdictConfig{
		MinSamples:        5,
		ErrCountSoft:      5,
		ErrCountHard:      10,
		ErrRateSoft:       0.3,
		ErrRateHard:       0.5,
		TTFTStickyOnlyMs:  8000,
		OTPSStickyOnlyMin: 10,
	}
}

func TestHealthVerdict_StringRepresentation(t *testing.T) {
	require.Equal(t, "OK", HealthOK.String())
	require.Equal(t, "StickyOnly", HealthStickyOnly.String())
	require.Equal(t, "Excluded", HealthExcluded.String())
	require.Equal(t, "Unknown", HealthVerdict(99).String())
}

func TestHealthVerdict_NoEntryReturnsOK(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	require.Equal(t, HealthOK, cache.HealthVerdict(7, testHealthVerdictConfig()))
}

// 样本不足（< MinSamples）时不应触发任何阈值
func TestHealthVerdict_BelowMinSamplesReturnsOK(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	cache.ReportRealCall(7, false)
	cache.ReportRealCall(7, false)
	cache.ReportRealCall(7, false)
	require.Equal(t, HealthOK, cache.HealthVerdict(7, testHealthVerdictConfig()))
}

// errCount=5 但 errRate < 0.5（不触发 Hard）→ 仅 Soft 生效 → StickyOnly
func TestHealthVerdict_ErrCountSoftProducesStickyOnly(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	for i := 0; i < 5; i++ {
		cache.ReportRealCall(7, false)
	}
	for i := 0; i < 6; i++ {
		cache.ReportRealCall(7, true)
	}
	// errCount=5 → 触发 ErrCountSoft；errRate=5/11≈0.45 → 未到 ErrRateHard(0.5)
	require.Equal(t, HealthStickyOnly, cache.HealthVerdict(7, testHealthVerdictConfig()))
}

// 10 次失败 → ErrCountHard 触发 → Excluded
func TestHealthVerdict_ErrCountHardProducesExcluded(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	for i := 0; i < 10; i++ {
		cache.ReportRealCall(7, false)
	}
	require.Equal(t, HealthExcluded, cache.HealthVerdict(7, testHealthVerdictConfig()))
}

// 50% 错误率 → ErrRateHard 触发（≥ 0.5）→ Excluded
func TestHealthVerdict_ErrRateHardProducesExcluded(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	for i := 0; i < 5; i++ {
		cache.ReportRealCall(7, false)
	}
	for i := 0; i < 5; i++ {
		cache.ReportRealCall(7, true)
	}
	// 5/10 = 0.5 → Hard
	require.Equal(t, HealthExcluded, cache.HealthVerdict(7, testHealthVerdictConfig()))
}

// 30%-49% 错误率 → ErrRateSoft 触发 → StickyOnly
func TestHealthVerdict_ErrRateSoftProducesStickyOnly(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	// 让 Soft 在 ErrCountSoft 之前生效：故意构造 errCount=4 但占比 ≥ 0.3
	for i := 0; i < 4; i++ {
		cache.ReportRealCall(7, false)
	}
	for i := 0; i < 6; i++ {
		cache.ReportRealCall(7, true)
	}
	// 4/10 = 0.4：未到 Hard（0.5），但触发 Soft（0.3）；errCount=4 < ErrCountSoft=5 不会触发
	require.Equal(t, HealthStickyOnly, cache.HealthVerdict(7, testHealthVerdictConfig()))
}

// Hard 优先于 Soft：err count 同时满足 Soft 和 Hard 时返回 Excluded
func TestHealthVerdict_HardOverridesSoft(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	for i := 0; i < 15; i++ {
		cache.ReportRealCall(7, false)
	}
	require.Equal(t, HealthExcluded, cache.HealthVerdict(7, testHealthVerdictConfig()))
}

// 高 TTFT（窗口平均 ≥ 8000ms）→ StickyOnly
func TestHealthVerdict_HighTTFTStickyOnly(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	for i := 0; i < 5; i++ {
		cache.Record(7, CallSample{Success: true, TTFTMs: 9000})
	}
	require.Equal(t, HealthStickyOnly, cache.HealthVerdict(7, testHealthVerdictConfig()))
}

// 低 OTPS 触发 StickyOnly
func TestHealthVerdict_LowOTPSStickyOnly(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	// otps = (50-1)*1000 / (10000-1000) = 49000/9000 ≈ 5.4 < 10
	for i := 0; i < 5; i++ {
		cache.Record(7, CallSample{Success: true, TTFTMs: 1000, DurationMs: 10000, OutputTokens: 50})
	}
	require.Equal(t, HealthStickyOnly, cache.HealthVerdict(7, testHealthVerdictConfig()))
}

// 错误数 5 后即使大量成功稀释错误率，errCount 不归零，账号仍处 StickyOnly，
// 直到窗口过期（自然滚出）或人为干预。这是滑动窗口的预期语义。
func TestHealthVerdict_StaysStickyUntilWindowExpires(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	// 起步：5 次失败 + 6 次成功 → StickyOnly（errCount=5 触发 Soft，errRate ≈ 0.45 未到 Hard）
	for i := 0; i < 5; i++ {
		cache.ReportRealCall(7, false)
	}
	for i := 0; i < 6; i++ {
		cache.ReportRealCall(7, true)
	}
	require.Equal(t, HealthStickyOnly, cache.HealthVerdict(7, testHealthVerdictConfig()))

	// 即便注入 30 次成功，errCount 仍 = 5（绝对计数）→ 仍 StickyOnly
	for i := 0; i < 30; i++ {
		cache.ReportRealCall(7, true)
	}
	require.Equal(t, HealthStickyOnly, cache.HealthVerdict(7, testHealthVerdictConfig()),
		"窗口未过期且 errCount=5 持续 ≥ ErrCountSoft，账号应保持 StickyOnly")

	// 模拟窗口过期：把所有桶手动拨到 11 分钟前 → Snapshot 应为空 → 回到 OK
	h := cache.Get(7)
	h.mu.Lock()
	now := time.Now().Unix()
	for i := range h.buckets {
		if h.buckets[i].startSec > 0 {
			h.buckets[i].startSec = now - 11*60
		}
	}
	h.mu.Unlock()
	require.Equal(t, HealthOK, cache.HealthVerdict(7, testHealthVerdictConfig()))
}

func TestHealthVerdictWithChange_EmitsTransitionOnce(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	cfg := testHealthVerdictConfig()

	// 初始无记录
	v, prev, changed := cache.HealthVerdictWithChange(7, cfg)
	require.Equal(t, HealthOK, v)
	require.False(t, changed, "无 entry 时不视为切换")
	require.Equal(t, HealthOK, prev)

	// 触发 → StickyOnly：errCount=5、errRate=5/11≈0.45（< ErrRateHard 0.5）
	for i := 0; i < 5; i++ {
		cache.ReportRealCall(7, false)
	}
	for i := 0; i < 6; i++ {
		cache.ReportRealCall(7, true)
	}
	v, prev, changed = cache.HealthVerdictWithChange(7, cfg)
	require.Equal(t, HealthStickyOnly, v)
	require.True(t, changed)
	require.Equal(t, HealthOK, prev)

	// 再次查询不应重复报告 changed
	v, prev, changed = cache.HealthVerdictWithChange(7, cfg)
	require.Equal(t, HealthStickyOnly, v)
	require.False(t, changed)
	require.Equal(t, HealthStickyOnly, prev)

	// 再 5 次失败：errCount=10 → ErrCountHard 触发 → Excluded
	for i := 0; i < 5; i++ {
		cache.ReportRealCall(7, false)
	}
	v, prev, changed = cache.HealthVerdictWithChange(7, cfg)
	require.Equal(t, HealthExcluded, v)
	require.True(t, changed)
	require.Equal(t, HealthStickyOnly, prev)
}

func TestHealthVerdict_DisabledThresholdsNoOp(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	for i := 0; i < 50; i++ {
		cache.ReportRealCall(7, false)
	}
	// 全部阈值 0 → 永远 OK
	cfg := HealthVerdictConfig{}
	require.Equal(t, HealthOK, cache.HealthVerdict(7, cfg))
}

// MinSamples=0 时所有窗口指标立即生效（不要求最少样本）
func TestHealthVerdict_MinSamplesZeroAlwaysApplies(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	cache.ReportRealCall(7, false)
	cache.ReportRealCall(7, false)
	cfg := HealthVerdictConfig{
		MinSamples:   0,
		ErrCountHard: 2,
	}
	require.Equal(t, HealthExcluded, cache.HealthVerdict(7, cfg))
}
