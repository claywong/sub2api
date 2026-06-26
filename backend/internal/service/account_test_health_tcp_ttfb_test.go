//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCallSample_TCPConnAndTTFB(t *testing.T) {
	c := NewAccountTestHealthCache(nil)

	// 记录包含 TCP 连接时间和 TTFB 的样本
	sample1 := CallSample{
		Success:      true,
		TCPConnMs:    50,  // TCP 连接 50ms
		TTFBMs:       200, // 首字节 200ms
		TTFTMs:       800, // 首 token 800ms
		DurationMs:   5000,
		OutputTokens: 100,
	}
	c.Record(1, sample1)

	sample2 := CallSample{
		Success:      true,
		TCPConnMs:    100, // TCP 连接 100ms
		TTFBMs:       300, // 首字节 300ms
		TTFTMs:       1000,
		DurationMs:   6000,
		OutputTokens: 150,
	}
	c.Record(1, sample2)

	// 验证快照聚合
	s := c.Snapshot(1)
	require.Equal(t, 2, s.ReqCount)
	require.Equal(t, 0, s.ErrCount)

	// 验证 TCP 连接时间
	require.True(t, s.HasTCPConn())
	require.Equal(t, 2, s.TCPConnSampleCount)
	require.InDelta(t, 75.0, s.TCPConnAvg(), 0.001) // (50+100)/2 = 75

	// 验证 TTFB
	require.True(t, s.HasTTFB())
	require.Equal(t, 2, s.TTFBSampleCount)
	require.InDelta(t, 250.0, s.TTFBAvg(), 0.001) // (200+300)/2 = 250

	// 验证 TTFT
	require.True(t, s.HasTTFT())
	require.InDelta(t, 900.0, s.TTFTAvg(), 0.001) // (800+1000)/2 = 900
}

func TestCallSample_PartialMetrics(t *testing.T) {
	c := NewAccountTestHealthCache(nil)

	// 只有 TCP 连接时间，没有 TTFB
	sample1 := CallSample{
		Success:   true,
		TCPConnMs: 50,
		TTFTMs:    800,
	}
	c.Record(1, sample1)

	// 只有 TTFB，没有 TCP 连接时间
	sample2 := CallSample{
		Success: true,
		TTFBMs:  200,
		TTFTMs:  1000,
	}
	c.Record(1, sample2)

	s := c.Snapshot(1)
	require.Equal(t, 2, s.ReqCount)

	// TCP 连接时间只有 1 个样本
	require.True(t, s.HasTCPConn())
	require.Equal(t, 1, s.TCPConnSampleCount)
	require.InDelta(t, 50.0, s.TCPConnAvg(), 0.001)

	// TTFB 只有 1 个样本
	require.True(t, s.HasTTFB())
	require.Equal(t, 1, s.TTFBSampleCount)
	require.InDelta(t, 200.0, s.TTFBAvg(), 0.001)
}

func TestCallSample_NoTCPConnOrTTFB(t *testing.T) {
	c := NewAccountTestHealthCache(nil)

	// 不包含 TCP 连接时间和 TTFB 的样本（旧版本兼容）
	sample := CallSample{
		Success:      true,
		TTFTMs:       800,
		DurationMs:   5000,
		OutputTokens: 100,
	}
	c.Record(1, sample)

	s := c.Snapshot(1)
	require.Equal(t, 1, s.ReqCount)

	// 没有 TCP 连接时间样本
	require.False(t, s.HasTCPConn())
	require.Equal(t, 0, s.TCPConnSampleCount)
	require.Equal(t, 0.0, s.TCPConnAvg())

	// 没有 TTFB 样本
	require.False(t, s.HasTTFB())
	require.Equal(t, 0, s.TTFBSampleCount)
	require.Equal(t, 0.0, s.TTFBAvg())

	// 但有 TTFT 样本
	require.True(t, s.HasTTFT())
	require.InDelta(t, 800.0, s.TTFTAvg(), 0.001)
}

func TestCallSample_RealCallWithTCPConnAndTTFB(t *testing.T) {
	c := NewAccountTestHealthCache(nil)

	// 模拟真实调用上报 TCP 连接时间和 TTFB
	sample := CallSample{
		Success:      true,
		TCPConnMs:    80,
		TTFBMs:       250,
		TTFTMs:       900,
		DurationMs:   5000,
		OutputTokens: 120,
	}
	c.RecordRealCall(1, sample)

	s := c.Snapshot(1)
	require.Equal(t, 1, s.ReqCount)
	require.Equal(t, 0, s.ErrCount)

	// 验证所有指标都被记录
	require.True(t, s.HasTCPConn())
	require.InDelta(t, 80.0, s.TCPConnAvg(), 0.001)

	require.True(t, s.HasTTFB())
	require.InDelta(t, 250.0, s.TTFBAvg(), 0.001)

	require.True(t, s.HasTTFT())
	require.InDelta(t, 900.0, s.TTFTAvg(), 0.001)
}

func TestCallSample_PerformanceAnalysis(t *testing.T) {
	c := NewAccountTestHealthCache(nil)

	// 场景：网络慢（TCP 连接时间高）
	slowNetwork := CallSample{
		Success:   true,
		TCPConnMs: 500, // TCP 连接很慢
		TTFBMs:    600, // TTFB 也慢
		TTFTMs:    1200,
	}
	c.Record(1, slowNetwork)

	// 场景：网络快但模型推理慢
	slowModel := CallSample{
		Success:   true,
		TCPConnMs: 50,   // TCP 连接快
		TTFBMs:    100,  // TTFB 快
		TTFTMs:    5000, // 但 TTFT 很慢（模型推理慢）
	}
	c.Record(1, slowModel)

	s := c.Snapshot(1)
	require.Equal(t, 2, s.ReqCount)

	// 平均 TCP 连接时间
	avgTCPConn := s.TCPConnAvg()
	require.InDelta(t, 275.0, avgTCPConn, 0.001) // (500+50)/2

	// 平均 TTFB
	avgTTFB := s.TTFBAvg()
	require.InDelta(t, 350.0, avgTTFB, 0.001) // (600+100)/2

	// 平均 TTFT
	avgTTFT := s.TTFTAvg()
	require.InDelta(t, 3100.0, avgTTFT, 0.001) // (1200+5000)/2

	// 分析：可以通过 TTFT - TTFB 计算模型推理时间
	// slowNetwork: 1200 - 600 = 600ms 模型推理
	// slowModel: 5000 - 100 = 4900ms 模型推理
	// 平均模型推理时间: (600+4900)/2 = 2750ms
	avgModelInference := avgTTFT - avgTTFB
	require.InDelta(t, 2750.0, avgModelInference, 0.001)
}
