//go:build unit

package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// ===== 因子归一化 =====

func TestComputePriorityFactor(t *testing.T) {
	// 单值池：归一化为 1.0
	require.InDelta(t, 1.0, computePriorityFactor(10, 10, 10), 1e-9)
	// 最小值
	require.InDelta(t, 1.0, computePriorityFactor(10, 10, 45), 1e-9)
	// 最大值
	require.InDelta(t, 0.0, computePriorityFactor(45, 10, 45), 1e-9)
	// 中间
	require.InDelta(t, 1.0-15.0/35.0, computePriorityFactor(25, 10, 45), 1e-9)
}

func TestComputeLoadFactor_NonLinear(t *testing.T) {
	th := config.ScoreThresholdsConfig{LoadThresholdPct: 70}
	// < threshold → 1.0
	require.Equal(t, 1.0, computeLoadFactor(&AccountLoadInfo{LoadRate: 30}, th))
	require.Equal(t, 1.0, computeLoadFactor(&AccountLoadInfo{LoadRate: 69}, th))
	// = threshold → 1.0
	require.Equal(t, 1.0, computeLoadFactor(&AccountLoadInfo{LoadRate: 70}, th))
	// 70~100 之间线性
	require.InDelta(t, 0.5, computeLoadFactor(&AccountLoadInfo{LoadRate: 85}, th), 1e-9)
	// >= 100 → 0
	require.Equal(t, 0.0, computeLoadFactor(&AccountLoadInfo{LoadRate: 100}, th))
}

func TestComputeTTFTFactor_AbsoluteThreshold(t *testing.T) {
	th := config.ScoreThresholdsConfig{TTFTBestMs: 1500, TTFTWorstMs: 6000}

	// 无样本 → 中性
	require.Equal(t, 0.5, computeTTFTFactor(HealthSnapshot{}, th))

	// 满分（≤ best）
	s := HealthSnapshot{TTFTSampleCount: 1, ttftSumMs: 1000}
	require.Equal(t, 1.0, computeTTFTFactor(s, th))

	// 最差（≥ worst）
	s = HealthSnapshot{TTFTSampleCount: 1, ttftSumMs: 6000}
	require.Equal(t, 0.0, computeTTFTFactor(s, th))

	// 中间线性
	s = HealthSnapshot{TTFTSampleCount: 1, ttftSumMs: 3750} // (6000-3750)/(6000-1500)=0.5
	require.InDelta(t, 0.5, computeTTFTFactor(s, th), 1e-6)
}

func TestComputeOTPSFactor_AbsoluteThreshold(t *testing.T) {
	th := config.ScoreThresholdsConfig{OTPSBest: 80, OTPSWorst: 10}

	// 无样本
	require.Equal(t, 0.5, computeOTPSFactor(HealthSnapshot{}, th))

	// 满分（≥ best）
	s := HealthSnapshot{OTPSSampleCount: 1, otpsSum: 100}
	require.Equal(t, 1.0, computeOTPSFactor(s, th))

	// 最差（≤ worst）
	s = HealthSnapshot{OTPSSampleCount: 1, otpsSum: 5}
	require.Equal(t, 0.0, computeOTPSFactor(s, th))

	// 中间线性
	s = HealthSnapshot{OTPSSampleCount: 1, otpsSum: 45} // (45-10)/(80-10)=0.5
	require.InDelta(t, 0.5, computeOTPSFactor(s, th), 1e-6)
}

// ===== Top-K 选择 =====

func TestSelectTopKWeighted_FullSortWhenSmaller(t *testing.T) {
	pool := []weightedCandidate{
		{account: &Account{ID: 1, Priority: 10}, score: 1.0},
		{account: &Account{ID: 2, Priority: 10}, score: 3.0},
		{account: &Account{ID: 3, Priority: 10}, score: 2.0},
	}
	out := selectTopKWeighted(pool, 5)
	require.Len(t, out, 3)
	require.Equal(t, int64(2), out[0].account.ID, "score 高排第一")
	require.Equal(t, int64(3), out[1].account.ID)
	require.Equal(t, int64(1), out[2].account.ID)
}

func TestSelectTopKWeighted_LimitedK(t *testing.T) {
	pool := []weightedCandidate{
		{account: &Account{ID: 1, Priority: 10}, score: 1.0},
		{account: &Account{ID: 2, Priority: 10}, score: 3.0},
		{account: &Account{ID: 3, Priority: 10}, score: 2.0},
		{account: &Account{ID: 4, Priority: 10}, score: 4.0},
		{account: &Account{ID: 5, Priority: 10}, score: 0.5},
	}
	out := selectTopKWeighted(pool, 2)
	require.Len(t, out, 2)
	require.Equal(t, int64(4), out[0].account.ID)
	require.Equal(t, int64(2), out[1].account.ID)
}

func TestSelectTopKWeighted_TieBreakerByPriority(t *testing.T) {
	pool := []weightedCandidate{
		{account: &Account{ID: 1, Priority: 20}, loadInfo: &AccountLoadInfo{LoadRate: 50}, score: 2.0},
		{account: &Account{ID: 2, Priority: 10}, loadInfo: &AccountLoadInfo{LoadRate: 50}, score: 2.0},
	}
	out := selectTopKWeighted(pool, 2)
	require.Equal(t, int64(2), out[0].account.ID, "score 相同时 priority 小的排前")
}

// ===== 种子稳定性 =====

func TestDeriveAnthropicSelectionSeed_Stable(t *testing.T) {
	gid := int64(1)
	seed1 := deriveAnthropicSelectionSeed("session-abc", "claude-sonnet", &gid)
	seed2 := deriveAnthropicSelectionSeed("session-abc", "claude-sonnet", &gid)
	require.Equal(t, seed1, seed2, "同会话/模型/group 应得到相同种子")
}

func TestDeriveAnthropicSelectionSeed_DifferentSession(t *testing.T) {
	gid := int64(1)
	a := deriveAnthropicSelectionSeed("session-a", "claude-sonnet", &gid)
	b := deriveAnthropicSelectionSeed("session-b", "claude-sonnet", &gid)
	require.NotEqual(t, a, b)
}

func TestDeriveAnthropicSelectionSeed_NoSessionMixesTime(t *testing.T) {
	// 无 sessionHash 时混入 UnixNano 时间熵；连续多次采样，至少应有两次不同。
	seen := make(map[uint64]struct{})
	for i := 0; i < 50; i++ {
		seen[deriveAnthropicSelectionSeed("", "claude-sonnet", nil)] = struct{}{}
		if len(seen) > 1 {
			return
		}
	}
	require.Greater(t, len(seen), 1, "无 sessionHash 时应混入时间熵，多次采样应至少出现 2 个不同的 seed")
}

// ===== 加权随机顺序 =====

func TestBuildWeightedSelectionOrder_SingleCandidate(t *testing.T) {
	in := []weightedCandidate{
		{account: &Account{ID: 1}, score: 1.0},
	}
	out := buildWeightedSelectionOrder(in, 12345, false)
	require.Len(t, out, 1)
	require.Equal(t, int64(1), out[0].account.ID)
}

func TestBuildWeightedSelectionOrder_DeterministicWithSameSeed(t *testing.T) {
	in := []weightedCandidate{
		{account: &Account{ID: 1}, score: 1.0},
		{account: &Account{ID: 2}, score: 2.0},
		{account: &Account{ID: 3}, score: 3.0},
	}
	out1 := buildWeightedSelectionOrder(in, 0xdeadbeef, false)
	out2 := buildWeightedSelectionOrder(in, 0xdeadbeef, false)
	require.Equal(t, len(out1), len(out2))
	for i := range out1 {
		require.Equal(t, out1[i].account.ID, out2[i].account.ID, "相同种子应产生相同顺序")
	}
}

func TestBuildWeightedSelectionOrder_HighScoreFavored(t *testing.T) {
	in := []weightedCandidate{
		{account: &Account{ID: 1}, score: 0.1},
		{account: &Account{ID: 2}, score: 4.0},
	}
	hits := map[int64]int{}
	for seed := uint64(1); seed <= 200; seed++ {
		out := buildWeightedSelectionOrder(in, seed, false)
		hits[out[0].account.ID]++
	}
	// 高分账号被排第一的次数应明显多于低分（≥ 3 倍）
	require.Greater(t, hits[2], hits[1]*3,
		"高分账号被选概率应明显高于低分（实际 hits: %v）", hits)
}

// ===== 综合打分（端到端） =====

func TestWeightedScoreEndToEnd_PerformanceBeatsCheap(t *testing.T) {
	// 模拟方案文档 §3.4.2 的两账号场景：
	// A：便宜（priority=10）但慢、错多
	// B：贵（priority=45）但快、稳
	healthCache := NewAccountTestHealthCache(nil)
	// A 账号窗口数据：errCount 高、ttft 慢、otps 低
	for i := 0; i < 6; i++ {
		healthCache.Record(1, CallSample{Success: false})
	}
	healthCache.Record(1, CallSample{Success: true, TTFTMs: 4500, DurationMs: 30000, OutputTokens: 50})
	healthCache.Record(1, CallSample{Success: true, TTFTMs: 4500, DurationMs: 30000, OutputTokens: 50})

	// B 账号窗口数据：成功率高、ttft 快、otps 高
	for i := 0; i < 9; i++ {
		healthCache.Record(2, CallSample{Success: true, TTFTMs: 1800, DurationMs: 5000, OutputTokens: 200})
	}
	healthCache.Record(2, CallSample{Success: false})

	svc := &GatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				Scheduling: config.GatewaySchedulingConfig{
					Health: config.SchedulingHealthConfig{MinSamples: 5},
				},
			},
		},
		healthCache: healthCache,
	}

	weights := svc.resolvedScoreWeights()
	thresholds := svc.resolvedScoreThresholds()
	healthCfg := svc.resolvedSchedulingHealth()

	pool := []weightedCandidate{
		{account: &Account{ID: 1, Priority: 10}, loadInfo: &AccountLoadInfo{LoadRate: 30}, snapshot: healthCache.Snapshot(1)},
		{account: &Account{ID: 2, Priority: 45}, loadInfo: &AccountLoadInfo{LoadRate: 50}, snapshot: healthCache.Snapshot(2)},
	}
	priorityMin, priorityMax := 10, 45
	for i := range pool {
		c := &pool[i]
		c.priorityFactor = computePriorityFactor(c.account.Priority, priorityMin, priorityMax)
		c.loadFactor = computeLoadFactor(c.loadInfo, thresholds)
		if healthCfg.MinSamples > 0 && c.snapshot.ReqCount < healthCfg.MinSamples {
			c.ttftFactor, c.otpsFactor, c.errFactor = 0.5, 0.5, 0.7
		} else {
			c.ttftFactor = computeTTFTFactor(c.snapshot, thresholds)
			c.otpsFactor = computeOTPSFactor(c.snapshot, thresholds)
			c.errFactor = clamp01(1.0 - c.snapshot.ErrRate())
		}
		c.score = weights.ErrRate*c.errFactor +
			weights.TTFT*c.ttftFactor +
			weights.OTPS*c.otpsFactor +
			weights.Priority*c.priorityFactor +
			weights.Load*c.loadFactor
	}
	t.Logf("A score=%.3f (err=%.2f ttft=%.2f otps=%.2f prio=%.2f load=%.2f)",
		pool[0].score, pool[0].errFactor, pool[0].ttftFactor, pool[0].otpsFactor, pool[0].priorityFactor, pool[0].loadFactor)
	t.Logf("B score=%.3f (err=%.2f ttft=%.2f otps=%.2f prio=%.2f load=%.2f)",
		pool[1].score, pool[1].errFactor, pool[1].ttftFactor, pool[1].otpsFactor, pool[1].priorityFactor, pool[1].loadFactor)

	// 性能差距应足以让贵但稳的 B 胜出便宜但烂的 A
	require.Greater(t, pool[1].score, pool[0].score,
		"B（贵+快+稳）应胜过 A（便宜+慢+错多）")
}

// ===== Group 日志白名单 =====

func TestGroupContainedInLogList(t *testing.T) {
	require.False(t, groupContainedInLogList(nil, []int64{1}))
	gid := int64(7)
	require.False(t, groupContainedInLogList(&gid, nil))
	require.False(t, groupContainedInLogList(&gid, []int64{1, 2, 3}))
	require.True(t, groupContainedInLogList(&gid, []int64{1, 7, 3}))
}
