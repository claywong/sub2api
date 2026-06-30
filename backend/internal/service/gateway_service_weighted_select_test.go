//go:build unit

// gateway_service_weighted_select_test.go
// 私有扩展测试（不属于 upstream sub2api）：性价比确定性选号。
//
// 新方案语义（区别于历史的加权随机）：
//   - 选号是确定性的，不再"按权重分配份额"，而是"score 容差带内按 load 选最空"。
//   - 因此本测试基本都用单次调用断言"谁被选中"，而不是统计份额。
//   - 容差带 CostTolerance 默认 0.15，影响"等价组"宽度；想测纯最高分用 ≈0。
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

func makeAccount(id int64, priority int, rate float64, load int) accountWithLoad {
	return accountWithLoad{
		account:  &Account{ID: id, Priority: priority, RateMultiplier: &rate},
		loadInfo: &AccountLoadInfo{AccountID: id, LoadRate: load},
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

// recordCacheQuality 写入 n 条带缓存命中的样本。
// 命中率 = cacheRead/(cacheRead+cacheCreate+input)；TTFT 2s、OTPS 80 固定，保持质量相近。
func recordCacheQuality(s *GatewayService, id int64, model string, cacheRead, cacheCreate, input, n int) {
	for i := 0; i < n; i++ {
		s.modelQualityCache.Record(id, model, CallSample{
			Success: true, TTFTMs: 2000, DurationMs: 3000, OutputTokens: 81,
			CacheReadTokens: cacheRead, CacheCreationTokens: cacheCreate, InputTokens: input,
		})
	}
}

// ─── 配置默认值 ───────────────────────────────────────────────────────────────

func TestWeightedSelectionConfig_Defaults(t *testing.T) {
	s := newWeightedTestService(true)
	c := s.weightedSelectionConfig()
	assert.Equal(t, 60, c.QualityWindowMinutes)
	assert.InDelta(t, 1.0, c.CostAggressiveness, 1e-9, "成本敏感度默认 1.0")
	assert.InDelta(t, 0.15, c.CostTolerance, 1e-9, "成本容差默认 0.15")
}

func TestWeightedSelectionConfig_ToleranceClampedTo1(t *testing.T) {
	s := newWeightedTestService(true)
	s.cfg.Gateway.Scheduling.WeightedSelection.CostTolerance = 5
	assert.InDelta(t, 1.0, s.weightedSelectionConfig().CostTolerance, 1e-9, "上限 1.0")
}

func TestWeightedSelection_DisabledByDefault(t *testing.T) {
	s := newWeightedTestService(false)
	assert.False(t, s.isWeightedSelectionEnabled())
}

// ─── 质量打分 ─────────────────────────────────────────────────────────────────

func TestComputeQualityScore_ColdStartIsOne(t *testing.T) {
	// 无样本各项取 1 → quality = 0.6 + 0.4 = 1.0
	s := newWeightedTestService(true)
	q, _ := s.computeQualityScore(1, "claude-sonnet")
	assert.InDelta(t, 1.0, q, 1e-9, "无样本 quality 应为 1.0")
}

func TestComputeQualityScore_NoSuccessRateInQuality(t *testing.T) {
	// 关键回归：successRate 不再进 quality（HealthVerdict 门禁前置过滤）。
	// 即使账号在 healthCache 里全失败，quality 仍只受 TTFT/OTPS 影响。
	s := newWeightedTestService(true)
	for i := 0; i < 10; i++ {
		s.healthCache.RecordRealCall(1, CallSample{Success: false})
	}
	recordQuality(s, 1, "m", 1000, 101, 2000, 10) // TTFT 1s, OTPS 100
	q, _ := s.computeQualityScore(1, "m")
	assert.Greater(t, q, 0.9, "TTFT/OTPS 都好，quality 应近满分（与 errRate 无关）：q=%.3f", q)
}

func TestComputeQualityScore_FastBeatsSlow(t *testing.T) {
	s := newWeightedTestService(true)
	// acc1: TTFT 1s、OTPS≈100；acc2: TTFT 9s、OTPS≈20
	recordQuality(s, 1, "m", 1000, 101, 2000, 10)
	recordQuality(s, 2, "m", 9000, 21, 10000, 10)
	q1, _ := s.computeQualityScore(1, "m")
	q2, _ := s.computeQualityScore(2, "m")
	assert.Greater(t, q1, q2, "快账号 quality 应高于慢账号")
	assert.Greater(t, q1-q2, 0.3, "差距应明显（≥ 0.3）")
}

func TestComputeQualityScore_OTPSThreshold(t *testing.T) {
	// 新阈值：OTPS 20→0 分，50→满分。验证边界。
	s := newWeightedTestService(true)
	// 输出 token N+1，duration_ms - ttft_ms = decode_ms → OTPS = N×1000/decode_ms
	// 让 decode_ms=1000, OutputTokens=21 → OTPS=20 → otpsScore=0
	recordQuality(s, 1, "m", 1000, 21, 2000, 10) // OTPS=20
	recordQuality(s, 2, "m", 1000, 51, 2000, 10) // OTPS=50
	_, d1 := s.computeQualityScore(1, "m")
	_, d2 := s.computeQualityScore(2, "m")
	assert.InDelta(t, 0.0, d1.OTPSScore, 0.05, "OTPS=20 应为 0 分附近：%.3f", d1.OTPSScore)
	assert.InDelta(t, 1.0, d2.OTPSScore, 0.05, "OTPS=50 应满分附近：%.3f", d2.OTPSScore)
}

func TestComputeQualityScore_P90PenalizesTail(t *testing.T) {
	s := newWeightedTestService(true)
	// acc1: 稳定 5s；acc2: 均值同为 5s 但 20% 长尾(1s×8 + 21s×2)
	recordQuality(s, 1, "m", 5000, 0, 0, 10)
	recordQuality(s, 2, "m", 1000, 0, 0, 8)
	recordQuality(s, 2, "m", 21000, 0, 0, 2)
	q1, d1 := s.computeQualityScore(1, "m")
	q2, d2 := s.computeQualityScore(2, "m")
	assert.InDelta(t, 5000, d1.TTFTP90Ms, 1.0, "稳定账号 P90 = avg")
	assert.Greater(t, d2.TTFTP90Ms, d1.TTFTP90Ms, "长尾账号 P90 应更差")
	assert.Greater(t, q1, q2, "长尾账号 quality 应更低")
}

// ─── 选号：基本行为 ──────────────────────────────────────────────────────────

func TestSelectByWeightedQuality_EmptyAndSingle(t *testing.T) {
	s := newWeightedTestService(true)
	assert.Nil(t, s.selectByWeightedQuality(nil, "m", nil, ""))
	one := []accountWithLoad{makeAccount(1, 0, 1.0, 0)}
	got := s.selectByWeightedQuality(one, "m", nil, "")
	require.NotNil(t, got)
	assert.Equal(t, int64(1), got.account.ID)
}

func TestSelectByWeightedQuality_PriorityDominates(t *testing.T) {
	s := newWeightedTestService(true)
	// acc2 priority 更高（数值更小）但质量差，仍必选。
	recordQuality(s, 1, "m", 1000, 101, 2000, 10)
	recordQuality(s, 2, "m", 9000, 21, 10000, 10)
	available := []accountWithLoad{
		makeAccount(1, 10, 1.0, 0),
		makeAccount(2, 5, 1.0, 0),
	}
	got := s.selectByWeightedQuality(available, "m", nil, "")
	require.NotNil(t, got)
	assert.Equal(t, int64(2), got.account.ID, "Priority 必须压倒质量分")
}

// 同 load 时优质账号被选（确定性，单次断言）。
func TestSelectByWeightedQuality_GoodQualityWinsAtSameLoad(t *testing.T) {
	s := newWeightedTestService(true)
	recordQuality(s, 1, "m", 1000, 101, 2000, 10) // 优质
	recordQuality(s, 2, "m", 9000, 21, 10000, 10) // 劣质
	available := []accountWithLoad{
		makeAccount(1, 0, 1.0, 0),
		makeAccount(2, 0, 1.0, 0),
	}
	got := s.selectByWeightedQuality(available, "m", nil, "")
	require.NotNil(t, got)
	assert.Equal(t, int64(1), got.account.ID, "同 load 下优质账号应被确定性选中")
}

// 同 load + 质量相近时便宜账号被选。
func TestSelectByWeightedQuality_CheaperWinsAtSimilarQuality(t *testing.T) {
	s := newWeightedTestService(true)
	recordQuality(s, 1, "m", 2000, 81, 3000, 10) // 质量基本相同
	recordQuality(s, 2, "m", 2100, 81, 3000, 10)
	available := []accountWithLoad{
		makeAccount(1, 0, 4.0, 0), // 贵 4 倍
		makeAccount(2, 0, 1.0, 0), // 便宜
	}
	got := s.selectByWeightedQuality(available, "m", nil, "")
	require.NotNil(t, got)
	assert.Equal(t, int64(2), got.account.ID, "质量相近时便宜账号应胜")
}

// ─── 选号：容差带 + 负载均衡 ────────────────────────────────────────────────

// 同 score 时按 load 选最空。
func TestSelectByWeightedQuality_TieBreakByLoad(t *testing.T) {
	s := newWeightedTestService(true)
	recordQuality(s, 1, "m", 2000, 81, 3000, 10)
	recordQuality(s, 2, "m", 2000, 81, 3000, 10)
	available := []accountWithLoad{
		makeAccount(1, 0, 1.0, 80), // 同 quality、同 rate，但负载高
		makeAccount(2, 0, 1.0, 10), // 负载低
	}
	got := s.selectByWeightedQuality(available, "m", nil, "")
	require.NotNil(t, got)
	assert.Equal(t, int64(2), got.account.ID, "score 相等应选 load 最低的")
}

// 容差带内 score 不是最高但 load 更低 → 被选中。
func TestSelectByWeightedQuality_ToleranceBandLoadBalance(t *testing.T) {
	s := newWeightedTestService(true)
	// 两账号 quality 都满分（无样本），倍率 1.0 vs 1.1 → score 相差 ~10%，落在 15% 容差带内
	available := []accountWithLoad{
		makeAccount(1, 0, 1.0, 90),  // score 略高，但 load 高
		makeAccount(2, 0, 1.1, 5),   // score 略低（约 0.91×），但 load 远低
	}
	got := s.selectByWeightedQuality(available, "m", nil, "")
	require.NotNil(t, got)
	assert.Equal(t, int64(2), got.account.ID, "15%% 容差内应按 load 选最空的")
}

// 容差带收紧到接近 0 时，只选 score 最高的那个（即便 load 高）。
func TestSelectByWeightedQuality_TightToleranceSelectsBest(t *testing.T) {
	s := newWeightedTestService(true)
	s.cfg.Gateway.Scheduling.WeightedSelection.CostTolerance = 0.001 // 接近 0
	available := []accountWithLoad{
		makeAccount(1, 0, 1.0, 90), // score 最高
		makeAccount(2, 0, 1.1, 5),  // 便宜不到 15%，被踢出带外
	}
	got := s.selectByWeightedQuality(available, "m", nil, "")
	require.NotNil(t, got)
	assert.Equal(t, int64(1), got.account.ID, "tolerance≈0 时纯选 score 最高")
}

// 容差带放大到 1.0 → 全候选入带 → 退化为纯按 load 选最空。
func TestSelectByWeightedQuality_FullToleranceBalancesLoad(t *testing.T) {
	s := newWeightedTestService(true)
	s.cfg.Gateway.Scheduling.WeightedSelection.CostTolerance = 1.0
	// 哪怕一个贵 10 倍，只要 load 低，也应被选中
	available := []accountWithLoad{
		makeAccount(1, 0, 1.0, 90),
		makeAccount(2, 0, 10.0, 5),
	}
	got := s.selectByWeightedQuality(available, "m", nil, "")
	require.NotNil(t, got)
	assert.Equal(t, int64(2), got.account.ID, "tolerance=1 时纯按 load")
}

// ─── 选号：冷启动 ────────────────────────────────────────────────────────────

// 冷启动账号 load=0 → 即便老账号 score 高，冷账号也会因 load 低被优先（自带预热）。
// 前提：score 落在容差带内（quality 都=1.0，rate 相同，effRate 差异在缓存折扣 → 带内）。
func TestSelectByWeightedQuality_ColdStartGetsPreheatedByLoad(t *testing.T) {
	s := newWeightedTestService(true)
	// 老账号有缓存样本（命中率 0.5），冷账号无样本但 load 低
	recordCacheQuality(s, 1, "m", 50, 0, 50, 30)
	available := []accountWithLoad{
		makeAccount(1, 0, 1.0, 50), // 老账号：高缓存折扣 → score 更高，但 load 高
		makeAccount(2, 0, 1.0, 0),  // 冷账号：乐观估计后 score 接近，load=0
	}
	got := s.selectByWeightedQuality(available, "m", nil, "")
	require.NotNil(t, got)
	// 冷启动乐观估计让冷账号 effRate 也享受到平均折扣 → 进容差带 → 按 load 胜出
	assert.Equal(t, int64(2), got.account.ID, "冷启动 + 乐观估计 + load 优势 → 冷账号被选")
}

// 没有任何账号有缓存样本时，冷启动乐观估计无数据 → 全用裸 rate，按 load 选。
func TestSelectByWeightedQuality_ColdStartWithNoCacheData(t *testing.T) {
	s := newWeightedTestService(true)
	// 两个账号都无缓存样本，rate 一样
	available := []accountWithLoad{
		makeAccount(1, 0, 1.0, 30),
		makeAccount(2, 0, 1.0, 0),
	}
	got := s.selectByWeightedQuality(available, "m", nil, "")
	require.NotNil(t, got)
	assert.Equal(t, int64(2), got.account.ID, "全无缓存数据时按 load 选最空")
}

// ─── 选号：缓存命中率作为成本因子 ────────────────────────────────────────────

func TestSelectByWeightedQuality_CacheHitCheaperWins(t *testing.T) {
	s := newWeightedTestService(true)
	recordCacheQuality(s, 1, "m", 90, 0, 10, 60) // hit 0.9
	recordCacheQuality(s, 2, "m", 0, 0, 100, 60) // hit 0.0
	available := []accountWithLoad{
		makeAccount(1, 0, 1.0, 0),
		makeAccount(2, 0, 1.0, 0),
	}
	got := s.selectByWeightedQuality(available, "m", nil, "")
	require.NotNil(t, got)
	assert.Equal(t, int64(1), got.account.ID, "高命中率账号 effRate 更低应胜")
}

func TestSelectByWeightedQuality_CacheSampleShrinkage(t *testing.T) {
	// 相同高命中率：样本多的账号有效折扣更强 → effRate 更低 → 被选。
	pickWith := func(nSamples int) int64 {
		s := newWeightedTestService(true)
		recordCacheQuality(s, 1, "m", 90, 0, 10, nSamples) // hit 0.9
		recordCacheQuality(s, 2, "m", 90, 0, 10, 5)        // hit 0.9 但样本少
		available := []accountWithLoad{
			makeAccount(1, 0, 1.0, 0),
			makeAccount(2, 0, 1.0, 0),
		}
		got := s.selectByWeightedQuality(available, "m", nil, "")
		require.NotNil(t, got)
		return got.account.ID
	}
	// 收紧 tolerance 排除负载均衡干扰，纯看 effRate
	assert.Equal(t, int64(1), pickWith(200), "样本充足的高命中账号应被选")
}

// ─── 选号：极端值不爆炸 ──────────────────────────────────────────────────────

// 0 倍率账号经 clampRate 保护，不应产生 NaN/Inf。
func TestSelectByWeightedQuality_ZeroRateMultiplierClamped(t *testing.T) {
	s := newWeightedTestService(true)
	available := []accountWithLoad{
		makeAccount(1, 0, 0.0, 0), // 0 倍率被 clamp 到 0.05
		makeAccount(2, 0, 1.0, 0),
	}
	got := s.selectByWeightedQuality(available, "m", nil, "")
	require.NotNil(t, got, "不应 panic")
	// clamp 后倍率 0.05 远便宜，应胜
	assert.Equal(t, int64(1), got.account.ID, "clamp 后仍最便宜账号胜")
}

// 极端：所有候选 score=0 → 阈值=0 → 全部入带 → 退化为按 load 选。
func TestSelectByWeightedQuality_AllZeroScoreFallback(t *testing.T) {
	// 此场景不易构造（quality 最小 = 0，仅当 TTFT P90≥15s 且 OTPS≤20）
	s := newWeightedTestService(true)
	recordQuality(s, 1, "m", 20000, 21, 21000, 10) // TTFT 20s, OTPS≈20
	recordQuality(s, 2, "m", 20000, 21, 21000, 10)
	available := []accountWithLoad{
		makeAccount(1, 0, 1.0, 50),
		makeAccount(2, 0, 1.0, 5),
	}
	got := s.selectByWeightedQuality(available, "m", nil, "")
	require.NotNil(t, got, "全 0 也能选出，不 panic")
	assert.Equal(t, int64(2), got.account.ID, "全 0 退化为按 load")
}

// effRate 下限保护：满命中+小倍率不爆。
func TestSelectByWeightedQuality_CacheEffRateFloorNoExplode(t *testing.T) {
	s := newWeightedTestService(true)
	recordCacheQuality(s, 1, "m", 100, 0, 0, 300) // hit=1, N=300 → effRate 触下限
	recordCacheQuality(s, 2, "m", 0, 0, 100, 300)
	available := []accountWithLoad{
		makeAccount(1, 0, 0.1, 0),
		makeAccount(2, 0, 1.0, 0),
	}
	got := s.selectByWeightedQuality(available, "m", nil, "")
	require.NotNil(t, got, "不 panic")
	assert.Equal(t, int64(1), got.account.ID, "极低 effRate 账号胜")
}

// β≈0：完全不看成本 → quality 相同时只看 load。
func TestSelectByWeightedQuality_CostAggressivenessZero(t *testing.T) {
	s := newWeightedTestService(true)
	s.cfg.Gateway.Scheduling.WeightedSelection.CostAggressiveness = 0.001
	recordQuality(s, 1, "m", 2000, 81, 3000, 10)
	recordQuality(s, 2, "m", 2000, 81, 3000, 10)
	// 即便倍率差 4 倍，β≈0 时 effRate^β ≈ 1 → score 几乎相等 → 按 load 选
	available := []accountWithLoad{
		makeAccount(1, 0, 4.0, 30),
		makeAccount(2, 0, 1.0, 50), // 便宜但 load 高
	}
	got := s.selectByWeightedQuality(available, "m", nil, "")
	require.NotNil(t, got)
	assert.Equal(t, int64(1), got.account.ID, "β≈0 时倍率不影响 score → load 决胜")
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
