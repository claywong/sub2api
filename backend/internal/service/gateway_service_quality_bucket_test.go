//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testModel = "claude-3-5-sonnet-20241022"

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
		{3999, 0},
		{4000, 1},
		{6999, 1},
		{7000, 2},
		{9999, 2},
		{10000, 3},
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

// ─── cacheHitBucket ───────────────────────────────────────────────────────────

func TestCacheHitBucket_NoSample(t *testing.T) {
	assert.Equal(t, 0, cacheHitBucket(0, false), "无样本 → bucket 0（冷启动保护）")
	assert.Equal(t, 0, cacheHitBucket(0.5, false), "无样本时忽略 rate 值")
}

func TestCacheHitBucket_Boundaries(t *testing.T) {
	cases := []struct {
		rate float64
		want int
	}{
		{1.0, 0},
		{0.80, 0},
		{0.79, 1},
		{0.60, 1},
		{0.59, 2},
		{0.40, 2},
		{0.39, 3},
		{0.20, 3},
		{0.19, 4},
		{0.0, 4},
	}
	for _, c := range cases {
		got := cacheHitBucket(c.rate, true)
		assert.Equal(t, c.want, got, "rate=%.2f", c.rate)
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func makeAccountWithLoad(id int64, loadRate int) accountWithLoad {
	return accountWithLoad{
		account:  &Account{ID: id},
		loadInfo: &AccountLoadInfo{AccountID: id, LoadRate: loadRate},
	}
}

func newQualityCache() *AccountModelQualityCache {
	return NewAccountModelQualityCache()
}

// recordTTFT 向 cache 写入 n 条 TTFT 样本。
func recordTTFT(cache *AccountModelQualityCache, id int64, ttftMs int, n int) {
	for i := 0; i < n; i++ {
		cache.Record(id, testModel, CallSample{
			Success:      true,
			TTFTMs:       ttftMs,
			DurationMs:   ttftMs + 500,
			OutputTokens: 20,
		})
	}
}

// recordOTPS 向 cache 写入 n 条 OTPS 样本。
func recordOTPS(cache *AccountModelQualityCache, id int64, otpsTarget float64, ttftMs int, n int) {
	outputTokens := 101
	durationMs := ttftMs + int(float64(outputTokens-1)*1000/otpsTarget)
	for i := 0; i < n; i++ {
		cache.Record(id, testModel, CallSample{
			Success:      true,
			TTFTMs:       ttftMs,
			DurationMs:   durationMs,
			OutputTokens: outputTokens,
		})
	}
}

// recordCacheHit 向 cache 写入 n 条缓存命中率样本。
func recordCacheHit(cache *AccountModelQualityCache, id int64, hitRate float64, n int) {
	total := 1000
	readTokens := int(float64(total) * hitRate)
	inputTokens := total - readTokens
	for i := 0; i < n; i++ {
		cache.Record(id, testModel, CallSample{
			Success:             true,
			CacheReadTokens:     readTokens,
			CacheCreationTokens: 0,
			InputTokens:         inputTokens,
		})
	}
}

// ─── filterByMinTTFTBucket ────────────────────────────────────────────────────

func TestFilterByMinTTFTBucket_NilCache(t *testing.T) {
	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 0),
		makeAccountWithLoad(2, 0),
	}
	got := filterByMinTTFTBucket(accounts, nil, testModel)
	assert.Equal(t, accounts, got, "cache=nil 时应原样返回")
}

func TestFilterByMinTTFTBucket_Empty(t *testing.T) {
	cache := newQualityCache()
	got := filterByMinTTFTBucket(nil, cache, testModel)
	assert.Empty(t, got)
}

func TestFilterByMinTTFTBucket_NoSampleAllBucket0(t *testing.T) {
	cache := newQualityCache()
	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 0),
		makeAccountWithLoad(2, 0),
	}
	got := filterByMinTTFTBucket(accounts, cache, testModel)
	assert.Equal(t, accounts, got, "无样本时全部 bucket 0，原样返回")
}

func TestFilterByMinTTFTBucket_SelectsFastest(t *testing.T) {
	cache := newQualityCache()
	// acc1: 2000ms → bucket 0；acc2: 5000ms → bucket 1；acc3: 8000ms → bucket 2
	recordTTFT(cache, 1, 2000, 5)
	recordTTFT(cache, 2, 5000, 5)
	recordTTFT(cache, 3, 8000, 5)

	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 50),
		makeAccountWithLoad(2, 10),
		makeAccountWithLoad(3, 0),
	}
	got := filterByMinTTFTBucket(accounts, cache, testModel)
	require.Len(t, got, 1)
	assert.Equal(t, int64(1), got[0].account.ID)
}

func TestFilterByMinTTFTBucket_TieKeepsBoth(t *testing.T) {
	cache := newQualityCache()
	// acc1: 1000ms，acc2: 3500ms，都在 bucket 0（< 4000ms）
	recordTTFT(cache, 1, 1000, 5)
	recordTTFT(cache, 2, 3500, 5)

	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 0),
		makeAccountWithLoad(2, 0),
	}
	got := filterByMinTTFTBucket(accounts, cache, testModel)
	assert.Len(t, got, 2)
}

func TestFilterByMinTTFTBucket_NoSampleCompetesWithBucket0(t *testing.T) {
	cache := newQualityCache()
	// acc1 无样本 → bucket 0；acc2: 5000ms → bucket 1
	recordTTFT(cache, 2, 5000, 5)

	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 0),
		makeAccountWithLoad(2, 0),
	}
	got := filterByMinTTFTBucket(accounts, cache, testModel)
	require.Len(t, got, 1)
	assert.Equal(t, int64(1), got[0].account.ID)
}

func TestFilterByMinTTFTBucket_ModelIsolation(t *testing.T) {
	cache := newQualityCache()
	// acc1: sonnet TTFT=2000ms(b0)，opus TTFT=8000ms(b2)
	// 查 sonnet 时 acc1 应在 bucket 0；查 opus 时 acc1 应在 bucket 2
	cache.Record(1, "claude-3-5-sonnet-20241022", CallSample{Success: true, TTFTMs: 2000, DurationMs: 2500, OutputTokens: 20})
	cache.Record(1, "claude-opus-4-5", CallSample{Success: true, TTFTMs: 8000, DurationMs: 8500, OutputTokens: 20})
	cache.Record(2, "claude-3-5-sonnet-20241022", CallSample{Success: true, TTFTMs: 6000, DurationMs: 6500, OutputTokens: 20})
	cache.Record(2, "claude-opus-4-5", CallSample{Success: true, TTFTMs: 2000, DurationMs: 2500, OutputTokens: 20})

	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 0),
		makeAccountWithLoad(2, 0),
	}

	// sonnet：acc1 bucket 0 胜出
	gotSonnet := filterByMinTTFTBucket(accounts, cache, "claude-3-5-sonnet-20241022")
	require.Len(t, gotSonnet, 1)
	assert.Equal(t, int64(1), gotSonnet[0].account.ID, "sonnet 应选 acc1")

	// opus：acc2 bucket 0 胜出
	gotOpus := filterByMinTTFTBucket(accounts, cache, "claude-opus-4-5")
	require.Len(t, gotOpus, 1)
	assert.Equal(t, int64(2), gotOpus[0].account.ID, "opus 应选 acc2")
}

// ─── filterByMinOTPSBucket ────────────────────────────────────────────────────

func TestFilterByMinOTPSBucket_NilCache(t *testing.T) {
	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 0),
		makeAccountWithLoad(2, 0),
	}
	got := filterByMinOTPSBucket(accounts, nil, testModel)
	assert.Equal(t, accounts, got, "cache=nil 时应原样返回")
}

func TestFilterByMinOTPSBucket_SelectsFastest(t *testing.T) {
	cache := newQualityCache()
	// acc1: OTPS≈80 → bucket 0；acc2: OTPS≈30 → bucket 2
	recordOTPS(cache, 1, 80, 500, 5)
	recordOTPS(cache, 2, 30, 500, 5)

	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 50),
		makeAccountWithLoad(2, 0),
	}
	got := filterByMinOTPSBucket(accounts, cache, testModel)
	require.Len(t, got, 1)
	assert.Equal(t, int64(1), got[0].account.ID)
}

func TestFilterByMinOTPSBucket_LowOTPSGroupedTogether(t *testing.T) {
	cache := newQualityCache()
	// acc1≈10，acc2≈15，都在 bucket 3
	recordOTPS(cache, 1, 10, 500, 5)
	recordOTPS(cache, 2, 15, 500, 5)

	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 0),
		makeAccountWithLoad(2, 0),
	}
	got := filterByMinOTPSBucket(accounts, cache, testModel)
	assert.Len(t, got, 2)
}

func TestFilterByMinOTPSBucket_NoSampleCompetesWithBucket0(t *testing.T) {
	cache := newQualityCache()
	// acc1 无样本 → bucket 0；acc2 OTPS≈10 → bucket 3
	recordOTPS(cache, 2, 10, 500, 5)

	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 0),
		makeAccountWithLoad(2, 0),
	}
	got := filterByMinOTPSBucket(accounts, cache, testModel)
	require.Len(t, got, 1)
	assert.Equal(t, int64(1), got[0].account.ID)
}

// ─── filterByMinCacheHitBucket ────────────────────────────────────────────────

func TestFilterByMinCacheHitBucket_NilCache(t *testing.T) {
	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 0),
		makeAccountWithLoad(2, 0),
	}
	got := filterByMinCacheHitBucket(accounts, nil, testModel)
	assert.Equal(t, accounts, got, "cache=nil 时应原样返回")
}

func TestFilterByMinCacheHitBucket_Empty(t *testing.T) {
	cache := newQualityCache()
	got := filterByMinCacheHitBucket(nil, cache, testModel)
	assert.Empty(t, got)
}

func TestFilterByMinCacheHitBucket_NoSampleAllBucket0(t *testing.T) {
	cache := newQualityCache()
	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 0),
		makeAccountWithLoad(2, 0),
	}
	got := filterByMinCacheHitBucket(accounts, cache, testModel)
	assert.Equal(t, accounts, got, "无样本时全部 bucket 0，原样返回")
}

func TestFilterByMinCacheHitBucket_SelectsHighestHitRate(t *testing.T) {
	cache := newQualityCache()
	// acc1: 90% → bucket 0；acc2: 50% → bucket 2；acc3: 10% → bucket 4
	recordCacheHit(cache, 1, 0.90, 5)
	recordCacheHit(cache, 2, 0.50, 5)
	recordCacheHit(cache, 3, 0.10, 5)

	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 50),
		makeAccountWithLoad(2, 0),
		makeAccountWithLoad(3, 0),
	}
	got := filterByMinCacheHitBucket(accounts, cache, testModel)
	require.Len(t, got, 1)
	assert.Equal(t, int64(1), got[0].account.ID)
}

func TestFilterByMinCacheHitBucket_TieKeepsBoth(t *testing.T) {
	cache := newQualityCache()
	// acc1: 85%，acc2: 82%，都在 bucket 0（≥ 80%）
	recordCacheHit(cache, 1, 0.85, 5)
	recordCacheHit(cache, 2, 0.82, 5)

	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 0),
		makeAccountWithLoad(2, 0),
	}
	got := filterByMinCacheHitBucket(accounts, cache, testModel)
	assert.Len(t, got, 2)
}

func TestFilterByMinCacheHitBucket_NoSampleCompetesWithBucket0(t *testing.T) {
	cache := newQualityCache()
	// acc1 无样本 → bucket 0；acc2: 10% → bucket 4
	recordCacheHit(cache, 2, 0.10, 5)

	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 0),
		makeAccountWithLoad(2, 0),
	}
	got := filterByMinCacheHitBucket(accounts, cache, testModel)
	require.Len(t, got, 1)
	assert.Equal(t, int64(1), got[0].account.ID)
}

func TestFilterByMinCacheHitBucket_ModelIsolation(t *testing.T) {
	cache := newQualityCache()
	// acc1: sonnet 命中率 90%，opus 命中率 10%
	// acc2: sonnet 命中率 10%，opus 命中率 90%
	for i := 0; i < 5; i++ {
		cache.Record(1, "claude-3-5-sonnet-20241022", CallSample{Success: true, CacheReadTokens: 900, InputTokens: 100})
		cache.Record(1, "claude-opus-4-5", CallSample{Success: true, CacheReadTokens: 100, InputTokens: 900})
		cache.Record(2, "claude-3-5-sonnet-20241022", CallSample{Success: true, CacheReadTokens: 100, InputTokens: 900})
		cache.Record(2, "claude-opus-4-5", CallSample{Success: true, CacheReadTokens: 900, InputTokens: 100})
	}

	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 0),
		makeAccountWithLoad(2, 0),
	}

	gotSonnet := filterByMinCacheHitBucket(accounts, cache, "claude-3-5-sonnet-20241022")
	require.Len(t, gotSonnet, 1)
	assert.Equal(t, int64(1), gotSonnet[0].account.ID, "sonnet 应选 acc1（命中率 90%）")

	gotOpus := filterByMinCacheHitBucket(accounts, cache, "claude-opus-4-5")
	require.Len(t, gotOpus, 1)
	assert.Equal(t, int64(2), gotOpus[0].account.ID, "opus 应选 acc2（命中率 90%）")
}

// ─── 集成：TTFT → OTPS → CacheHit → LoadRate 联合过滤 ─────────────────────────

func TestQualityBucket_FullChain(t *testing.T) {
	cache := newQualityCache()

	// acc1: TTFT=2000ms(b0), OTPS=80(b0), CacheHit=85%(b0), load=50
	// acc2: TTFT=2000ms(b0), OTPS=80(b0), CacheHit=10%(b4), load=0
	// acc3: TTFT=5000ms(b1), OTPS=80(b0), CacheHit=85%(b0), load=0
	// 期望：TTFT 过滤剩 acc1/acc2，OTPS 不变，CacheHit 过滤剩 acc1
	recordTTFT(cache, 1, 2000, 5)
	recordOTPS(cache, 1, 80, 2000, 5)
	recordCacheHit(cache, 1, 0.85, 5)

	recordTTFT(cache, 2, 2000, 5)
	recordOTPS(cache, 2, 80, 2000, 5)
	recordCacheHit(cache, 2, 0.10, 5)

	recordTTFT(cache, 3, 5000, 5)
	recordOTPS(cache, 3, 80, 5000, 5)
	recordCacheHit(cache, 3, 0.85, 5)

	accounts := []accountWithLoad{
		makeAccountWithLoad(1, 50),
		makeAccountWithLoad(2, 0),
		makeAccountWithLoad(3, 0),
	}

	after := filterByMinTTFTBucket(accounts, cache, testModel)
	assert.Len(t, after, 2, "TTFT 过滤后剩 acc1/acc2")

	after = filterByMinOTPSBucket(after, cache, testModel)
	assert.Len(t, after, 2, "OTPS 同桶，不过滤")

	after = filterByMinCacheHitBucket(after, cache, testModel)
	require.Len(t, after, 1, "CacheHit 过滤后只剩 acc1")
	assert.Equal(t, int64(1), after[0].account.ID)
}
