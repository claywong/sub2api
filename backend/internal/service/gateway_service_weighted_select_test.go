//go:build unit

// gateway_service_weighted_select_test.go
// 私有扩展测试（不属于 upstream sub2api）：性价比加权选号。
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

// ─── 配置默认值 ───────────────────────────────────────────────────────────────

func TestWeightedSelectionConfig_Defaults(t *testing.T) {
	s := newWeightedTestService(true)
	c := s.weightedSelectionConfig()
	assert.Equal(t, 60, c.QualityWindowMinutes)
	assert.InDelta(t, 1.0, c.CostAggressiveness, 1e-9, "成本敏感度默认 1.0")
}

func TestWeightedSelection_DisabledByDefault(t *testing.T) {
	s := newWeightedTestService(false)
	assert.False(t, s.isWeightedSelectionEnabled())
}

// ─── 质量打分 ─────────────────────────────────────────────────────────────────

func TestComputeQualityScore_ColdStartIsOne(t *testing.T) {
	// 极简方案：无样本各项取 1（不奖不罚），quality = 1.0
	// —— 让冷启动账号在性价比上和"中等好账号"竞争，鼓励探索。
	s := newWeightedTestService(true)
	q, _ := s.computeQualityScore(1, "claude-sonnet")
	assert.InDelta(t, 1.0, q, 1e-9, "无样本 quality 应为 1.0")
}

func TestComputeQualityScore_FastBeatsSlow(t *testing.T) {
	s := newWeightedTestService(true)
	// acc1: TTFT 1s、OTPS 高；acc2: TTFT 9s、OTPS 低
	recordQuality(s, 1, "m", 1000, 101, 2000, 10) // otps≈100
	recordQuality(s, 2, "m", 9000, 21, 10000, 10) // otps≈20
	q1, _ := s.computeQualityScore(1, "m")
	q2, _ := s.computeQualityScore(2, "m")
	assert.Greater(t, q1, q2, "快账号 quality 应高于慢账号")
	assert.Greater(t, q1-q2, 0.3, "差距应明显（≥ 0.3）")
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

// ─── 加权选号 ─────────────────────────────────────────────────────────────────

func TestSelectByWeightedQuality_PriorityDominates(t *testing.T) {
	s := newWeightedTestService(true)
	// acc2 priority 更高（数值更小）但质量差，仍必选。
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
	// acc1 优质（quality 高），acc2 劣质（quality 低）
	recordQuality(s, 1, "m", 1000, 101, 2000, 10)
	recordQuality(s, 2, "m", 9000, 21, 10000, 10)
	available := []accountWithLoad{
		makeWeightedAccount(1, 0, 1.0),
		makeWeightedAccount(2, 0, 1.0),
	}
	counts := countWeightedPicks(s, available, 2000)
	assert.Greater(t, counts[1], counts[2]*4, "优质账号应明显多拿：%v", counts)
}

func TestSelectByWeightedQuality_SimilarQualityCheaperWins(t *testing.T) {
	s := newWeightedTestService(true)
	// 两账号质量几乎相同，倍率 1.0 vs 4.0 → β=1 时权重比 ≈ 4:1
	recordQuality(s, 1, "m", 2000, 81, 3000, 10)
	recordQuality(s, 2, "m", 2100, 81, 3000, 10)
	available := []accountWithLoad{
		makeWeightedAccount(1, 0, 4.0),
		makeWeightedAccount(2, 0, 1.0),
	}
	counts := countWeightedPicks(s, available, 2000)
	assert.Greater(t, counts[2], counts[1]*2, "质量相近时便宜账号应明显多拿：%v", counts)
	assert.Greater(t, counts[1], 200, "贵账号仍应有可观份额：%v", counts)
}

func TestSelectByWeightedQuality_ColdStartGetsShare(t *testing.T) {
	s := newWeightedTestService(true)
	// acc1 有数据且优质（quality < 1），acc2 冷启动（quality = 1）
	// 同倍率下冷启动权重 = 1.0，老账号 ≈ 0.9 → 冷启动甚至略多拿一点（设计如此）
	recordQuality(s, 1, "m", 1000, 101, 2000, 10)
	available := []accountWithLoad{
		makeWeightedAccount(1, 0, 1.0),
		makeWeightedAccount(2, 0, 1.0),
	}
	counts := countWeightedPicks(s, available, 2000)
	assert.Greater(t, counts[2], 500, "冷启动账号应拿到充分探索份额：%v", counts)
	assert.Greater(t, counts[1], 500, "老优质账号仍应有可观份额：%v", counts)
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
	counts := countWeightedPicks(s, available, 500)
	assert.Greater(t, counts[2], 0, "0 倍率账号不应吃掉全部流量：%v", counts)
}

// 极端：所有候选 quality=0（全部错误率 100%）应均匀兜底，不 panic。
func TestSelectByWeightedQuality_AllZeroQualityFallback(t *testing.T) {
	s := newWeightedTestService(true)
	// 注入 10 条失败样本 → ErrRate = 1.0 → successRate = 0 → quality = 0
	for _, id := range []int64{1, 2} {
		for i := 0; i < 10; i++ {
			s.healthCache.RecordRealCall(id, CallSample{Success: false})
		}
	}
	available := []accountWithLoad{
		makeWeightedAccount(1, 0, 1.0),
		makeWeightedAccount(2, 0, 1.0),
	}
	counts := countWeightedPicks(s, available, 500)
	assert.Equal(t, 500, counts[1]+counts[2], "兜底应保证总和不变：%v", counts)
	assert.Greater(t, counts[1], 100, "兜底应近似均匀：%v", counts)
	assert.Greater(t, counts[2], 100, "兜底应近似均匀：%v", counts)
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

// ─── 缓存命中率作为有效成本因子（已常开，写死 discount=0.9, K=20）─────────────

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

// 同质量、同倍率，命中率高的账号 effRate 更低 → 多拿。
func TestSelectByWeightedQuality_CacheHitCheaperWins(t *testing.T) {
	s := newWeightedTestService(true)
	recordCacheQuality(s, 1, "m", 90, 0, 10, 60) // hit 0.9
	recordCacheQuality(s, 2, "m", 0, 0, 100, 60) // hit 0.0
	available := []accountWithLoad{makeWeightedAccount(1, 0, 1.0), makeWeightedAccount(2, 0, 1.0)}
	counts := countWeightedPicks(s, available, 2000)
	assert.Greater(t, counts[1], counts[2], "高命中率账号应多拿：%v", counts)
	assert.Greater(t, counts[2], 200, "低命中率账号仍应有可观份额：%v", counts)
}

// 无缓存样本：effRate=rate，回退到均匀。
func TestSelectByWeightedQuality_CacheColdStartDegradesToRate(t *testing.T) {
	s := newWeightedTestService(true)
	// 只写 TTFT/OTPS，不带 cache token → CacheHitN=0
	recordQuality(s, 1, "m", 2000, 81, 3000, 10)
	recordQuality(s, 2, "m", 2000, 81, 3000, 10)
	available := []accountWithLoad{makeWeightedAccount(1, 0, 1.0), makeWeightedAccount(2, 0, 1.0)}
	counts := countWeightedPicks(s, available, 2000)
	assert.Greater(t, counts[1], 800, "无缓存样本应回退均匀：%v", counts)
	assert.Greater(t, counts[2], 800, "无缓存样本应回退均匀：%v", counts)
}

// 样本量收缩：相同命中率下，样本多的账号有效折扣更强 → 份额更高。
func TestSelectByWeightedQuality_CacheSampleShrinkage(t *testing.T) {
	share := func(nSamples int) int {
		s := newWeightedTestService(true)
		recordCacheQuality(s, 1, "m", 90, 0, 10, nSamples) // hit 0.9, N=nSamples
		recordCacheQuality(s, 2, "m", 0, 0, 100, 60)       // 基准 hit 0
		available := []accountWithLoad{makeWeightedAccount(1, 0, 1.0), makeWeightedAccount(2, 0, 1.0)}
		return countWeightedPicks(s, available, 2000)[1]
	}
	few := share(2)
	many := share(200)
	assert.Greater(t, many, few, "样本多的高命中账号折扣更强：few=%d many=%d", few, many)
}

// effRate 下限保护：极端参数（小倍率+满命中）不产生 NaN/Inf，对手仍有份额。
func TestSelectByWeightedQuality_CacheEffRateFloorNoExplode(t *testing.T) {
	s := newWeightedTestService(true)
	recordCacheQuality(s, 1, "m", 100, 0, 0, 300) // hit 1.0, N=300 → effRate 触下限
	recordCacheQuality(s, 2, "m", 0, 0, 100, 300) // hit 0
	available := []accountWithLoad{makeWeightedAccount(1, 0, 0.1), makeWeightedAccount(2, 0, 1.0)}
	counts := countWeightedPicks(s, available, 2000)
	assert.Equal(t, 2000, counts[1]+counts[2], "无丢失/无 panic：%v", counts)
	assert.Greater(t, counts[1], counts[2], "极低有效成本账号应多拿：%v", counts)
	assert.Greater(t, counts[2], 0, "对手账号仍应保留份额（无除零爆权重）：%v", counts)
}

// CostAggressiveness=0：完全不看成本，仅按 quality 抽签。
func TestSelectByWeightedQuality_CostAggressivenessZero(t *testing.T) {
	s := newWeightedTestService(true)
	s.cfg.Gateway.Scheduling.WeightedSelection.CostAggressiveness = 0.001 // ≈0，effRate^0 ≈ 1
	// 同 quality，倍率 1 vs 4 —— β≈0 时应近似均匀
	recordQuality(s, 1, "m", 2000, 81, 3000, 10)
	recordQuality(s, 2, "m", 2000, 81, 3000, 10)
	available := []accountWithLoad{makeWeightedAccount(1, 0, 4.0), makeWeightedAccount(2, 0, 1.0)}
	counts := countWeightedPicks(s, available, 2000)
	assert.Greater(t, counts[1], 800, "β≈0 时倍率不影响分布：%v", counts)
	assert.Greater(t, counts[2], 800, "β≈0 时倍率不影响分布：%v", counts)
}
