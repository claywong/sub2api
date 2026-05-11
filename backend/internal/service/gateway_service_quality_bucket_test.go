//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── ttftBucket ───────────────────────────────────────────────────────────────

func TestTTFTBucket_NoSample(t *testing.T) {
	assert.Equal(t, 0, ttftBucket(0, false), "无样本 → bucket 0（冷启动保护）")
	assert.Equal(t, 0, ttftBucket(9999, false), "无样本时忽略 ttftMs 值")
}

func TestTTFTBucket_Boundaries(t *testing.T) {
	cases := []struct {
		ttftMs float64
		want   int
	}{
		{0, 0},
		{1000, 0},
		{2999, 0},
		{3000, 1},
		{4999, 1},
		{5000, 2},
		{6999, 2},
		{7000, 3},
		{9000, 4},
	}
	for _, c := range cases {
		got := ttftBucket(c.ttftMs, true)
		assert.Equal(t, c.want, got, "ttftMs=%.0f", c.ttftMs)
	}
}

// ─── otpsBucket ───────────────────────────────────────────────────────────────

func TestOTPSBucket_NoSample(t *testing.T) {
	assert.Equal(t, 0, otpsBucket(0, false), "无样本 → bucket 0（冷启动保护）")
	assert.Equal(t, 0, otpsBucket(5, false), "无样本时忽略 otps 值")
}

func TestOTPSBucket_Boundaries(t *testing.T) {
	cases := []struct {
		otps float64
		want int
	}{
		{100, 0},
		{60, 0},
		{59.9, 1},
		{40, 1},
		{39.9, 2},
		{20, 2},
		{19.9, 3},
		{0, 3},
	}
	for _, c := range cases {
		got := otpsBucket(c.otps, true)
		assert.Equal(t, c.want, got, "otps=%.1f", c.otps)
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func makeAccountWithLoad(id int64, loadRate int) accountWithLoad {
	return accountWithLoad{
		account:  &Account{ID: id},
		loadInfo: &AccountLoadInfo{AccountID: id, LoadRate: loadRate},
	}
}

// recordTTFT 向 cache 写入 n 条 TTFT 样本（均为 ttftMs，成功请求）。
func recordTTFT(cache *AccountTestHealthCache, id int64, ttftMs int, n int) {
	for i := 0; i < n; i++ {
		cache.RecordRealCall(id, CallSample{
			Success:      true,
			TTFTMs:       ttftMs,
			DurationMs:   ttftMs + 500,
			OutputTokens: 20,
		})
	}
}

// recordOTPS 向 cache 写入 n 条 OTPS 样本（固定 OTPS ≈ otpsTarget）。
// ttftMs 同时作为该样本的 TTFT 值写入窗口，调用方应与 recordTTFT 保持一致，避免拉偏均值。
func recordOTPS(cache *AccountTestHealthCache, id int64, otpsTarget float64, ttftMs int, n int) {
	// OTPS = (outputTokens-1)*1000 / (durationMs-ttftMs)
	outputTokens := 101
	durationMs := ttftMs + int(float64(outputTokens-1)*1000/otpsTarget)
	for i := 0; i < n; i++ {
		cache.RecordRealCall(id, CallSample{
			Success:      true,
			TTFTMs:       ttftMs,
			DurationMs:   durationMs,
			OutputTokens: outputTokens,
		})
	}
}

// ─── filterByMinTTFTBucket ────────────────────────────────────────────────────

func TestFilterByMinTTFTBucket_NilCache(t *testing.T) {
	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 0),
		makeAccountWithLoad(2, 0),
	}
	got := filterByMinTTFTBucket(accounts, nil)
	assert.Equal(t, accounts, got, "cache=nil 时应原样返回")
}

func TestFilterByMinTTFTBucket_Empty(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	got := filterByMinTTFTBucket(nil, cache)
	assert.Empty(t, got)
}

func TestFilterByMinTTFTBucket_NoSampleAllBucket0(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 0),
		makeAccountWithLoad(2, 0),
	}
	// 无任何样本 → 全部 bucket 0，直接返回原切片
	got := filterByMinTTFTBucket(accounts, cache)
	assert.Equal(t, accounts, got)
}

func TestFilterByMinTTFTBucket_SelectsFastest(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	// acc1: TTFT 2000ms → bucket 0
	// acc2: TTFT 4000ms → bucket 1
	// acc3: TTFT 6000ms → bucket 2
	recordTTFT(cache, 1, 2000, 5)
	recordTTFT(cache, 2, 4000, 5)
	recordTTFT(cache, 3, 6000, 5)

	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 50),
		makeAccountWithLoad(2, 10),
		makeAccountWithLoad(3, 0),
	}
	got := filterByMinTTFTBucket(accounts, cache)
	require.Len(t, got, 1)
	assert.Equal(t, int64(1), got[0].account.ID)
}

func TestFilterByMinTTFTBucket_TieKeepsBoth(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	// acc1 和 acc2 都在 bucket 0（< 3000ms），应同时保留
	recordTTFT(cache, 1, 1000, 5)
	recordTTFT(cache, 2, 2500, 5)

	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 0),
		makeAccountWithLoad(2, 0),
	}
	got := filterByMinTTFTBucket(accounts, cache)
	assert.Len(t, got, 2)
}

func TestFilterByMinTTFTBucket_NoSampleCompetesWithBucket0(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	// acc1 无样本 → bucket 0；acc2 TTFT=5000ms → bucket 1
	// 结果：acc1 留下，acc2 被过滤
	recordTTFT(cache, 2, 5000, 5)

	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 0),
		makeAccountWithLoad(2, 0),
	}
	got := filterByMinTTFTBucket(accounts, cache)
	require.Len(t, got, 1)
	assert.Equal(t, int64(1), got[0].account.ID)
}

// ─── filterByMinOTPSBucket ────────────────────────────────────────────────────

func TestFilterByMinOTPSBucket_NilCache(t *testing.T) {
	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 0),
		makeAccountWithLoad(2, 0),
	}
	got := filterByMinOTPSBucket(accounts, nil)
	assert.Equal(t, accounts, got, "cache=nil 时应原样返回")
}

func TestFilterByMinOTPSBucket_SelectsFastest(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	// acc1: OTPS ≈ 80 → bucket 0
	// acc2: OTPS ≈ 30 → bucket 2
	recordOTPS(cache, 1, 80, 500, 5)
	recordOTPS(cache, 2, 30, 500, 5)

	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 50),
		makeAccountWithLoad(2, 0),
	}
	got := filterByMinOTPSBucket(accounts, cache)
	require.Len(t, got, 1)
	assert.Equal(t, int64(1), got[0].account.ID)
}

func TestFilterByMinOTPSBucket_LowOTPSGroupedTogether(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	// acc1: OTPS ≈ 10 → bucket 3；acc2: OTPS ≈ 15 → bucket 3
	// 两者同桶，都应保留
	recordOTPS(cache, 1, 10, 500, 5)
	recordOTPS(cache, 2, 15, 500, 5)

	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 0),
		makeAccountWithLoad(2, 0),
	}
	got := filterByMinOTPSBucket(accounts, cache)
	assert.Len(t, got, 2)
}

func TestFilterByMinOTPSBucket_NoSampleCompetesWithBucket0(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)
	// acc1 无样本 → bucket 0；acc2 OTPS ≈ 10 → bucket 3
	recordOTPS(cache, 2, 10, 500, 5)

	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 0),
		makeAccountWithLoad(2, 0),
	}
	got := filterByMinOTPSBucket(accounts, cache)
	require.Len(t, got, 1)
	assert.Equal(t, int64(1), got[0].account.ID)
}

// ─── 集成：TTFT → OTPS → LoadRate 联合过滤 ────────────────────────────────────

func TestQualityBucket_FullChain(t *testing.T) {
	cache := NewAccountTestHealthCache(nil)

	// acc1: TTFT=2000ms(b0)，OTPS=80(b0)，load=50
	// acc2: TTFT=2000ms(b0)，OTPS=30(b2)，load=0
	// acc3: TTFT=4000ms(b1)，OTPS=80(b0)，load=0
	// 期望：TTFT 过滤只剩 acc1/acc2，OTPS 过滤只剩 acc1，acc3 应被 TTFT 步淘汰
	recordTTFT(cache, 1, 2000, 5)
	recordOTPS(cache, 1, 80, 2000, 5)
	recordTTFT(cache, 2, 2000, 5)
	recordOTPS(cache, 2, 30, 2000, 5)
	recordTTFT(cache, 3, 4000, 5)
	recordOTPS(cache, 3, 80, 4000, 5)

	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 50),
		makeAccountWithLoad(2, 0),
		makeAccountWithLoad(3, 0),
	}

	after := filterByMinTTFTBucket(accounts, cache)
	assert.Len(t, after, 2, "TTFT 过滤后剩 acc1/acc2")

	after = filterByMinOTPSBucket(after, cache)
	require.Len(t, after, 1, "OTPS 过滤后只剩 acc1")
	assert.Equal(t, int64(1), after[0].account.ID)
}
