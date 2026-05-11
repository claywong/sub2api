//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 端到端验证：真实调用上报样本 → buckets 累计 → Snapshot 派生 OTPS → HealthVerdict 感知。

// 1. 业内 OTPS 公式：(output-1)*1000 / (duration-ttft)，仅在 output >= 10 时生效
func TestOTPSEndToEnd_StandardFormula(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)

	// 5 次正常调用：output=100, ttft=1000, duration=3000 → decode=2000ms
	// otps = (100-1)*1000 / 2000 = 49.5
	for i := 0; i < 5; i++ {
		cache.RecordRealCall(7, CallSample{
			Success:      true,
			TTFTMs:       1000,
			DurationMs:   3000,
			OutputTokens: 100,
		})
	}

	s := cache.Snapshot(7)
	require.True(t, s.HasOTPS())
	require.Equal(t, 5, s.OTPSSampleCount)
	require.InDelta(t, 49.5, s.OTPSAvg(), 0.001)
}

// 2. OTPS 信号触发 HealthVerdict StickyOnly
func TestOTPSEndToEnd_LowOTPSTriggersStickyOnly(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)

	// 慢账号：output=50, ttft=1000, duration=15000 → decode=14000
	// otps = 49*1000/14000 ≈ 3.5（远低于 OTPSStickyOnlyMin=10）
	for i := 0; i < 6; i++ {
		cache.RecordRealCall(7, CallSample{
			Success:      true,
			TTFTMs:       1000,
			DurationMs:   15000,
			OutputTokens: 50,
		})
	}

	cfg := HealthVerdictConfig{
		MinSamples:        5,
		OTPSStickyOnlyMin: 10,
	}
	require.Equal(t, HealthStickyOnly, cache.HealthVerdict(7, cfg))
}

// 3. OTPS 因子在 weighted 打分中正确反映优劣
// 4. 短输出（< 10 tokens）不进 OTPS 样本，但 TTFT 仍计入
func TestOTPSEndToEnd_ShortOutputsExcluded(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	for i := 0; i < 5; i++ {
		cache.RecordRealCall(7, CallSample{
			Success:      true,
			TTFTMs:       500,
			DurationMs:   1500,
			OutputTokens: 5, // < 10
		})
	}
	s := cache.Snapshot(7)
	require.False(t, s.HasOTPS(), "短输出不计入 OTPS")
	require.True(t, s.HasTTFT(), "TTFT 仍正常累计")
	require.Equal(t, 5, s.ReqCount)
}

// 5. 无 OTPS 样本时 OTPSStickyOnlyMin 不应被错误触发
func TestOTPSEndToEnd_NoSampleNoStickyOnly(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	// 全部短输出 → 无 OTPS 样本
	for i := 0; i < 6; i++ {
		cache.RecordRealCall(7, CallSample{Success: true, TTFTMs: 500, DurationMs: 1500, OutputTokens: 5})
	}
	cfg := HealthVerdictConfig{
		MinSamples:        5,
		OTPSStickyOnlyMin: 50,
	}
	require.Equal(t, HealthOK, cache.HealthVerdict(7, cfg),
		"无 OTPS 样本时不应被 OTPSStickyOnlyMin 误判")
}
