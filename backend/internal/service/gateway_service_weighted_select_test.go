//go:build unit

// gateway_service_weighted_select_test.go
// 私有扩展测试（不属于 upstream sub2api）：质量加权选号。
package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newWeightedTestService(enabled bool) *GatewayService {
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.WeightedSelection.Enabled = enabled
	return &GatewayService{
		cfg:               cfg,
		modelQualityCache: NewAccountModelQualityCache(),
		healthCache:       NewAccountTestHealthCache(nil),
	}
}

func makeWeightedAccount(id int64, priority int, rate float64) accountWithLoad {
	return accountWithLoad{
		account:  &Account{ID: id, Priority: priority, RateMultiplier: &rate},
		loadInfo: &AccountLoadInfo{AccountID: id},
	}
}

// 写入 n 条指定 TTFT/OTPS 的样本。
func recordQuality(s *GatewayService, id int64, model string, ttftMs, outputTokens, durationMs, n int) {
	for i := 0; i < n; i++ {
		s.modelQualityCache.Record(id, model, CallSample{
			Success: true, TTFTMs: ttftMs, DurationMs: durationMs, OutputTokens: outputTokens,
		})
	}
}

// ─── 配置默认值 ───────────────────────────────────────────────────────────────

func TestWeightedSelectionConfig_Defaults(t *testing.T) {
	s := newWeightedTestService(true)
	c := s.weightedSelectionConfig()
	assert.Equal(t, 60, c.QualityWindowMinutes)
	assert.InDelta(t, 0.10, c.BandEpsilon, 1e-9)
	assert.InDelta(t, 1.0, c.CostBeta, 1e-9)
	assert.InDelta(t, 0.05, c.ExploreFloor, 1e-9)
	assert.Equal(t, 1000, c.TTFTFullScoreMs)
	assert.Equal(t, 10000, c.TTFTZeroScoreMs)
	assert.InDelta(t, 80.0, c.OTPSFullScore, 1e-9)
	assert.InDelta(t, 0.40, c.WTTFT, 1e-9)
}

func TestWeightedSelection_DisabledByDefault(t *testing.T) {
	s := newWeightedTestService(false)
	assert.False(t, s.isWeightedSelectionEnabled())
}

// ─── 质量打分 ─────────────────────────────────────────────────────────────────

func TestComputeQualityScore_ColdStartNeutral(t *testing.T) {
	s := newWeightedTestService(true)
	score, _ := s.computeQualityScore(1, "claude-sonnet", s.weightedSelectionConfig())
	assert.InDelta(t, 0.5, score, 1e-9, "无样本应为中性分 0.5")
}

func TestComputeQualityScore_FastBeatsSlow(t *testing.T) {
	s := newWeightedTestService(true)
	// acc1: TTFT 1s、OTPS 高；acc2: TTFT 9s、OTPS 低
	recordQuality(s, 1, "m", 1000, 101, 2000, 10) // otps=(100)*1000/1000=100
	recordQuality(s, 2, "m", 9000, 21, 10000, 10) // otps=20*1000/1000=20
	cfg := s.weightedSelectionConfig()
	s1, _ := s.computeQualityScore(1, "m", cfg)
	s2, _ := s.computeQualityScore(2, "m", cfg)
	assert.Greater(t, s1, s2)
	assert.Greater(t, s1-s2, cfg.BandEpsilon, "差距应足以掉出相近带")
}

func TestComputeQualityScore_P90PenalizesTail(t *testing.T) {
	s := newWeightedTestService(true)
	// acc1: 稳定 5s；acc2: 均值同为 5s 但 20% 长尾(1s×8 + 21s×2)
	recordQuality(s, 1, "m", 5000, 0, 0, 10)
	recordQuality(s, 2, "m", 1000, 0, 0, 8)
	recordQuality(s, 2, "m", 21000, 0, 0, 2)
	cfg := s.weightedSelectionConfig()
	s1, d1 := s.computeQualityScore(1, "m", cfg)
	s2, d2 := s.computeQualityScore(2, "m", cfg)
	assert.InDelta(t, 5000, d1.TTFTEffMs, 1.0)
	assert.Greater(t, d2.TTFTEffMs, d1.TTFTEffMs, "长尾账号 ttftEff 应更差")
	assert.Greater(t, s1, s2, "均值相同时长尾账号分数应更低")
}

// ─── 加权选号 ─────────────────────────────────────────────────────────────────

func TestSelectByWeightedQuality_PriorityDominates(t *testing.T) {
	s := newWeightedTestService(true)
	// acc2 priority 更高(数值更小)但质量差,仍必选
	recordQuality(s, 1, "m", 1000, 101, 2000, 10)
	recordQuality(s, 2, "m", 9000, 21, 10000, 10)
	available := []accountWithLoad{
		makeWeightedAccount(1, 10, 1.0),
		makeWeightedAccount(2, 5, 1.0),
	}
	for i := 0; i < 20; i++ {
		got := s.selectByWeightedQuality(available, "m", nil, "")
		require.NotNil(t, got)
		assert.Equal(t, int64(2), got.account.ID, "Priority 必须压倒质量分")
	}
}

func TestSelectByWeightedQuality_GoodQualityGetsMoreTraffic(t *testing.T) {
	s := newWeightedTestService(true)
	// acc1 优质,acc2 劣质(掉出相近带,只剩 floor)
	recordQuality(s, 1, "m", 1000, 101, 2000, 10)
	recordQuality(s, 2, "m", 9000, 21, 10000, 10)
	available := []accountWithLoad{
		makeWeightedAccount(1, 0, 1.0),
		makeWeightedAccount(2, 0, 1.0),
	}
	counts := map[int64]int{}
	for i := 0; i < 2000; i++ {
		got := s.selectByWeightedQuality(available, "m", nil, "")
		require.NotNil(t, got)
		counts[got.account.ID]++
	}
	// 期望约 95% / 5%(floor=0.05),给宽容差
	assert.Greater(t, counts[1], 1700, "优质账号应拿绝大多数选择: %v", counts)
	assert.Greater(t, counts[2], 0, "劣质账号应保留探索 floor: %v", counts)
	assert.Less(t, counts[2], 300, "劣质账号只应有 floor 份额: %v", counts)
}

func TestSelectByWeightedQuality_SimilarQualityCheaperWins(t *testing.T) {
	s := newWeightedTestService(true)
	// 两账号质量几乎相同(同带),倍率 1.0 vs 4.0 → 权重 4:1
	recordQuality(s, 1, "m", 2000, 81, 3000, 10)
	recordQuality(s, 2, "m", 2100, 81, 3000, 10)
	available := []accountWithLoad{
		makeWeightedAccount(1, 0, 4.0),
		makeWeightedAccount(2, 0, 1.0),
	}
	counts := map[int64]int{}
	for i := 0; i < 2000; i++ {
		got := s.selectByWeightedQuality(available, "m", nil, "")
		require.NotNil(t, got)
		counts[got.account.ID]++
	}
	// 期望 acc2 : acc1 ≈ 4 : 1(80%/20%),给宽容差
	assert.Greater(t, counts[2], counts[1]*2, "体验相近时便宜账号应明显多拿: %v", counts)
	assert.Greater(t, counts[1], 200, "贵账号在带内仍应有可观份额: %v", counts)
}

func TestSelectByWeightedQuality_ColdStartGetsShare(t *testing.T) {
	s := newWeightedTestService(true)
	// acc1 有数据且优质,acc2 无样本(中性 0.5)→ 带外 floor
	recordQuality(s, 1, "m", 1000, 101, 2000, 10)
	available := []accountWithLoad{
		makeWeightedAccount(1, 0, 1.0),
		makeWeightedAccount(2, 0, 1.0),
	}
	counts := map[int64]int{}
	for i := 0; i < 2000; i++ {
		got := s.selectByWeightedQuality(available, "m", nil, "")
		require.NotNil(t, got)
		counts[got.account.ID]++
	}
	assert.Greater(t, counts[2], 0, "冷启动账号应有探索流量: %v", counts)
}

func TestSelectByWeightedQuality_EmptyAndSingle(t *testing.T) {
	s := newWeightedTestService(true)
	assert.Nil(t, s.selectByWeightedQuality(nil, "m", nil, ""))
	one := []accountWithLoad{makeWeightedAccount(1, 0, 1.0)}
	got := s.selectByWeightedQuality(one, "m", nil, "")
	require.NotNil(t, got)
	assert.Equal(t, int64(1), got.account.ID)
}

func TestSelectByWeightedQuality_ZeroRateMultiplierClamped(t *testing.T) {
	s := newWeightedTestService(true)
	// 倍率 0 的账号不应权重无穷大
	available := []accountWithLoad{
		makeWeightedAccount(1, 0, 0.0),
		makeWeightedAccount(2, 0, 1.0),
	}
	counts := map[int64]int{}
	for i := 0; i < 500; i++ {
		got := s.selectByWeightedQuality(available, "m", nil, "")
		require.NotNil(t, got)
		counts[got.account.ID]++
	}
	assert.Greater(t, counts[2], 0, "0 倍率账号不应吃掉全部流量: %v", counts)
}

// ─── 质量窗口 P90 ────────────────────────────────────────────────────────────

func TestQualityCache_SnapshotWithP90(t *testing.T) {
	c := NewAccountModelQualityCache()
	for i := 1; i <= 10; i++ {
		c.Record(1, "m", CallSample{Success: true, TTFTMs: i * 1000})
	}
	snap, p90 := c.SnapshotWithP90(1, "m")
	require.True(t, snap.HasTTFT())
	assert.InDelta(t, 5500.0, snap.TTFTAvg(), 0.001)
	assert.InDelta(t, 9000.0, p90, 0.001, "10 个样本 1s..10s 的 P90 应为 9s")
}

func TestQualityCache_ConfigureWindowMinutes(t *testing.T) {
	c := NewAccountModelQualityCache()
	assert.Equal(t, int64(300), c.bucketSec(), "默认 60min/12 桶 = 300s")
	c.ConfigureWindowMinutes(12)
	assert.Equal(t, int64(60), c.bucketSec())
	c.ConfigureWindowMinutes(0) // 非法值不变
	assert.Equal(t, int64(60), c.bucketSec())
}

// ─── 缓存命中率作为带内成本因子（私有扩展）──────────────────────────────────

// recordCacheQuality 写入 n 条带缓存命中的样本。
// 命中率 = cacheRead/(cacheRead+cacheCreate+input)；TTFT 2s、OTPS 80 固定，
// 保证账号落在同一"体验相近带"，仅命中率/倍率不同，便于隔离成本因子的影响。
func recordCacheQuality(s *GatewayService, id int64, model string, cacheRead, cacheCreate, input, n int) {
	for i := 0; i < n; i++ {
		s.modelQualityCache.Record(id, model, CallSample{
			Success: true, TTFTMs: 2000, DurationMs: 3000, OutputTokens: 81,
			CacheReadTokens: cacheRead, CacheCreationTokens: cacheCreate, InputTokens: input,
		})
	}
}

func countWeightedPicks(s *GatewayService, available []accountWithLoad, iters int) map[int64]int {
	counts := map[int64]int{}
	for i := 0; i < iters; i++ {
		got := s.selectByWeightedQuality(available, "m", nil, "")
		if got != nil {
			counts[got.account.ID]++
		}
	}
	return counts
}

func TestWeightedSelectionConfig_CacheDefaults(t *testing.T) {
	s := newWeightedTestService(true)
	c := s.weightedSelectionConfig()
	assert.InDelta(t, 0.0, c.CacheDiscount, 1e-9, "缓存折扣默认 0 即关闭")
	assert.Equal(t, 20, c.CacheHitShrinkK)
	// discount 越界封顶到 0.9
	s.cfg.Gateway.Scheduling.WeightedSelection.CacheDiscount = 0.99
	assert.InDelta(t, 0.9, s.weightedSelectionConfig().CacheDiscount, 1e-9, "discount 应封顶到 0.9")
}

// 关闭时(discount=0)：高命中率账号不应获得任何成本优势（与原行为等价）。
func TestSelectByWeightedQuality_CacheDiscountZero_NoEffect(t *testing.T) {
	s := newWeightedTestService(true) // CacheDiscount 默认 0
	recordCacheQuality(s, 1, "m", 90, 0, 10, 60) // hit 0.9
	recordCacheQuality(s, 2, "m", 0, 0, 100, 60) // hit 0.0
	available := []accountWithLoad{makeWeightedAccount(1, 0, 1.0), makeWeightedAccount(2, 0, 1.0)}
	counts := countWeightedPicks(s, available, 2000)
	assert.Greater(t, counts[1], 800, "关闭时分布应接近均匀: %v", counts)
	assert.Greater(t, counts[2], 800, "关闭时分布应接近均匀: %v", counts)
}

// 开启时：体验相近、同倍率，命中率高的账号有效成本更低 → 多拿。
func TestSelectByWeightedQuality_CacheHitCheaperWins(t *testing.T) {
	s := newWeightedTestService(true)
	s.cfg.Gateway.Scheduling.WeightedSelection.CacheDiscount = 0.5
	recordCacheQuality(s, 1, "m", 90, 0, 10, 60) // hit 0.9
	recordCacheQuality(s, 2, "m", 0, 0, 100, 60) // hit 0.0
	available := []accountWithLoad{makeWeightedAccount(1, 0, 1.0), makeWeightedAccount(2, 0, 1.0)}
	counts := countWeightedPicks(s, available, 2000)
	assert.Greater(t, counts[1], counts[2], "高命中率账号应多拿: %v", counts)
	assert.Greater(t, counts[2], 200, "低命中率账号在带内仍应有可观份额: %v", counts)
}

// 开启但无缓存样本：effRate=rate，退化为原行为（约 50/50）。
func TestSelectByWeightedQuality_CacheColdStartDegradesToRate(t *testing.T) {
	s := newWeightedTestService(true)
	s.cfg.Gateway.Scheduling.WeightedSelection.CacheDiscount = 0.5
	// 只写 TTFT/OTPS，不带 cache token → CacheHitN=0
	recordQuality(s, 1, "m", 2000, 81, 3000, 10)
	recordQuality(s, 2, "m", 2000, 81, 3000, 10)
	available := []accountWithLoad{makeWeightedAccount(1, 0, 1.0), makeWeightedAccount(2, 0, 1.0)}
	counts := countWeightedPicks(s, available, 2000)
	assert.Greater(t, counts[1], 800, "无缓存样本应退化为均匀: %v", counts)
	assert.Greater(t, counts[2], 800, "无缓存样本应退化为均匀: %v", counts)
}

// 样本量收缩：同命中率下，样本多的账号有效折扣更强 → 份额更高。
func TestSelectByWeightedQuality_CacheSampleShrinkage(t *testing.T) {
	share := func(nSamples int) int {
		s := newWeightedTestService(true)
		s.cfg.Gateway.Scheduling.WeightedSelection.CacheDiscount = 0.5
		recordCacheQuality(s, 1, "m", 90, 0, 10, nSamples) // hit 0.9, N=nSamples
		recordCacheQuality(s, 2, "m", 0, 0, 100, 60)       // 基准 hit 0
		available := []accountWithLoad{makeWeightedAccount(1, 0, 1.0), makeWeightedAccount(2, 0, 1.0)}
		return countWeightedPicks(s, available, 2000)[1]
	}
	few := share(2)
	many := share(200)
	assert.Greater(t, many, few, "样本多的高命中账号折扣更强、份额更高: few=%d many=%d", few, many)
}

// effRate 下限保护：极端参数（小倍率+满命中+高折扣）不产生 NaN/Inf，对手仍有份额。
func TestSelectByWeightedQuality_CacheEffRateFloorNoExplode(t *testing.T) {
	s := newWeightedTestService(true)
	s.cfg.Gateway.Scheduling.WeightedSelection.CacheDiscount = 0.9
	recordCacheQuality(s, 1, "m", 100, 0, 0, 300) // hit 1.0, N=300 → effRate 触下限 0.05
	recordCacheQuality(s, 2, "m", 0, 0, 100, 300) // hit 0
	available := []accountWithLoad{makeWeightedAccount(1, 0, 0.1), makeWeightedAccount(2, 0, 1.0)}
	counts := countWeightedPicks(s, available, 2000)
	assert.Equal(t, 2000, counts[1]+counts[2], "无丢失/无 panic: %v", counts)
	assert.Greater(t, counts[1], counts[2], "极低有效成本账号应多拿: %v", counts)
	assert.Greater(t, counts[2], 0, "对手账号仍应保留份额（无除零爆权重）: %v", counts)
}

// 缓存命中率不影响"体验相近带"划分：体验差但命中率高的账号仍只拿带外 floor。
func TestSelectByWeightedQuality_CacheDoesNotAffectBand(t *testing.T) {
	s := newWeightedTestService(true)
	s.cfg.Gateway.Scheduling.WeightedSelection.CacheDiscount = 0.5
	// acc1：体验好(TTFT 1s, OTPS 100)、命中率 0
	for i := 0; i < 60; i++ {
		s.modelQualityCache.Record(1, "m", CallSample{Success: true, TTFTMs: 1000, DurationMs: 2000, OutputTokens: 101, InputTokens: 100})
	}
	// acc2：体验差(TTFT 9s, OTPS 20)、命中率 0.9（高）
	for i := 0; i < 60; i++ {
		s.modelQualityCache.Record(2, "m", CallSample{Success: true, TTFTMs: 9000, DurationMs: 10000, OutputTokens: 21, CacheReadTokens: 90, InputTokens: 10})
	}
	available := []accountWithLoad{makeWeightedAccount(1, 0, 1.0), makeWeightedAccount(2, 0, 1.0)}
	counts := countWeightedPicks(s, available, 2000)
	assert.Greater(t, counts[1], 1700, "体验好的账号仍应拿绝大多数: %v", counts)
	assert.Less(t, counts[2], 300, "体验差账号即便高命中率也只拿 floor: %v", counts)
}
