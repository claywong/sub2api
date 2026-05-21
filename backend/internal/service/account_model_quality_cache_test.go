//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── AccountModelQualityCache 基础 ────────────────────────────────────────────

func TestModelQualityCache_NoEntryReturnsZeroSnapshot(t *testing.T) {
	c := NewAccountModelQualityCache()
	s := c.Snapshot(1, "claude-3-5-sonnet-20241022")
	assert.False(t, s.HasTTFT())
	assert.False(t, s.HasOTPS())
	assert.False(t, s.HasCacheHit())
}

func TestModelQualityCache_RecordAndSnapshot(t *testing.T) {
	c := NewAccountModelQualityCache()
	c.Record(1, "claude-3-5-sonnet-20241022", CallSample{
		Success:      true,
		TTFTMs:       1200,
		DurationMs:   3000,
		OutputTokens: 50,
	})

	s := c.Snapshot(1, "claude-3-5-sonnet-20241022")
	require.True(t, s.HasTTFT())
	assert.InDelta(t, 1200.0, s.TTFTAvg(), 0.001)
}

func TestModelQualityCache_ModelIsolation(t *testing.T) {
	c := NewAccountModelQualityCache()

	// 同一账号，两个模型写入不同的 TTFT
	c.Record(1, "claude-3-5-sonnet-20241022", CallSample{Success: true, TTFTMs: 1000, DurationMs: 1500, OutputTokens: 20})
	c.Record(1, "claude-opus-4-5", CallSample{Success: true, TTFTMs: 8000, DurationMs: 9000, OutputTokens: 20})

	sonnetSnap := c.Snapshot(1, "claude-3-5-sonnet-20241022")
	opusSnap := c.Snapshot(1, "claude-opus-4-5")

	require.True(t, sonnetSnap.HasTTFT())
	require.True(t, opusSnap.HasTTFT())
	assert.InDelta(t, 1000.0, sonnetSnap.TTFTAvg(), 0.001, "sonnet TTFT 不应被 opus 污染")
	assert.InDelta(t, 8000.0, opusSnap.TTFTAvg(), 0.001, "opus TTFT 不应被 sonnet 污染")
}

// ─── 渠道映射场景：reqModel 与 mappedModel 隔离 ───────────────────────────────
//
// 模拟：调度用 reqModel="claude-3-5-sonnet-20241022"，
//       渠道映射后 Forward 使用 mappedModel="claude-3-5-sonnet-20241020"，
//       上报应使用 reqModel，确保分桶读写 key 一致。

func TestModelQualityCache_ChannelMapping_ReqModelKeyConsistency(t *testing.T) {
	c := NewAccountModelQualityCache()

	reqModel := "claude-3-5-sonnet-20241022"
	mappedModel := "claude-3-5-sonnet-20241020"

	// 模拟正确行为：用 reqModel 写入（handler 传 reqModel，不用 result.Model）
	c.Record(1, reqModel, CallSample{
		Success:         true,
		TTFTMs:          800,
		DurationMs:      2000,
		OutputTokens:    30,
		CacheReadTokens: 500,
		InputTokens:     500,
	})

	// 用 reqModel 查：应有数据
	snapReq := c.Snapshot(1, reqModel)
	require.True(t, snapReq.HasTTFT(), "用 reqModel 查应有 TTFT 样本")
	assert.InDelta(t, 800.0, snapReq.TTFTAvg(), 0.001)
	require.True(t, snapReq.HasCacheHit(), "用 reqModel 查应有 CacheHit 样本")
	assert.InDelta(t, 0.5, snapReq.CacheHitRateAvg(), 0.001)

	// 用 mappedModel 查：应为空（数据没有写在 mapped model 下）
	snapMapped := c.Snapshot(1, mappedModel)
	assert.False(t, snapMapped.HasTTFT(), "用 mappedModel 查不应有数据（key 不同）")
	assert.False(t, snapMapped.HasCacheHit(), "用 mappedModel 查不应有数据（key 不同）")
}

func TestModelQualityCache_ChannelMapping_BucketFilterUsesReqModel(t *testing.T) {
	c := NewAccountModelQualityCache()

	reqModel := "claude-3-5-sonnet-20241022"
	mappedModel := "claude-3-5-sonnet-20241020"

	// acc1 用 reqModel 写了好的 TTFT（bucket 0）
	c.Record(1, reqModel, CallSample{Success: true, TTFTMs: 500, DurationMs: 1000, OutputTokens: 20})
	// acc2 用 reqModel 写了差的 TTFT（bucket 2）
	c.Record(2, reqModel, CallSample{Success: true, TTFTMs: 7500, DurationMs: 8000, OutputTokens: 20})

	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 0),
		makeAccountWithLoad(2, 0),
	}

	// 用 reqModel 过滤：acc1 胜出
	got := filterByMinTTFTBucket(accounts, c, reqModel)
	require.Len(t, got, 1)
	assert.Equal(t, int64(1), got[0].account.ID, "用 reqModel 过滤应选出 acc1")

	// 用 mappedModel 过滤：无样本，全部 bucket 0，两个都返回
	gotMapped := filterByMinTTFTBucket(accounts, c, mappedModel)
	assert.Len(t, gotMapped, 2, "用 mappedModel 过滤因无样本应返回全部（冷启动保护）")
}

// ─── OTPSAvg 和 CacheHitRateAvg ──────────────────────────────────────────────

func TestModelQualityCache_OTPSAvg(t *testing.T) {
	c := NewAccountModelQualityCache()
	// output=101, ttft=500, duration=500+1000=1500 → otps=(100)*1000/1000=100
	c.Record(1, "claude-3-5-sonnet-20241022", CallSample{
		Success:      true,
		TTFTMs:       500,
		DurationMs:   1500,
		OutputTokens: 101,
	})
	s := c.Snapshot(1, "claude-3-5-sonnet-20241022")
	require.True(t, s.HasOTPS())
	assert.InDelta(t, 100.0, s.OTPSAvg(), 0.001)
}

func TestModelQualityCache_CacheHitRateAvg(t *testing.T) {
	c := NewAccountModelQualityCache()
	// cache_read=300, input=700 → rate=0.3
	c.Record(1, "claude-3-5-sonnet-20241022", CallSample{
		Success:         true,
		CacheReadTokens: 300,
		InputTokens:     700,
	})
	s := c.Snapshot(1, "claude-3-5-sonnet-20241022")
	require.True(t, s.HasCacheHit())
	assert.InDelta(t, 0.3, s.CacheHitRateAvg(), 0.001)
}

func TestModelQualityCache_ZeroIDOrNilSafe(t *testing.T) {
	c := NewAccountModelQualityCache()
	// accountID=0 不应 panic
	c.Record(0, "claude-3-5-sonnet-20241022", CallSample{Success: true, TTFTMs: 1000})
	s := c.Snapshot(0, "claude-3-5-sonnet-20241022")
	assert.False(t, s.HasTTFT(), "accountID=0 应被忽略")

	// nil cache 不应 panic
	var nilCache *AccountModelQualityCache
	nilCache.Record(1, "m", CallSample{})
	s2 := nilCache.Snapshot(1, "m")
	assert.False(t, s2.HasTTFT())
}
