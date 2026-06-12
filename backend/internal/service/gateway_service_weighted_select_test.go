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
