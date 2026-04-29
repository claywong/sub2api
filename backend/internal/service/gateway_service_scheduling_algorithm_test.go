//go:build unit

package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// helper：构造仅包含 Scheduling 配置的 GatewayService
func newSchedulingTestService(sched config.GatewaySchedulingConfig) *GatewayService {
	return &GatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				Scheduling: sched,
			},
		},
	}
}

func TestChooseSchedulingAlgorithm_Default(t *testing.T) {
	svc := newSchedulingTestService(config.GatewaySchedulingConfig{})

	gid := int64(1)
	require.Equal(t, schedulingAlgorithmLegacy, svc.chooseSchedulingAlgorithm(&gid))
	require.Equal(t, schedulingAlgorithmLegacy, svc.chooseSchedulingAlgorithm(nil))
}

func TestChooseSchedulingAlgorithm_GlobalWeighted(t *testing.T) {
	svc := newSchedulingTestService(config.GatewaySchedulingConfig{
		Algorithm: schedulingAlgorithmWeighted,
	})

	gid := int64(1)
	require.Equal(t, schedulingAlgorithmWeighted, svc.chooseSchedulingAlgorithm(&gid))
}

func TestChooseSchedulingAlgorithm_WeightedGroupWhitelist(t *testing.T) {
	svc := newSchedulingTestService(config.GatewaySchedulingConfig{
		Algorithm:      schedulingAlgorithmLegacy,
		WeightedGroups: []int64{7, 8},
	})

	gid7 := int64(7)
	gid9 := int64(9)
	require.Equal(t, schedulingAlgorithmWeighted, svc.chooseSchedulingAlgorithm(&gid7))
	require.Equal(t, schedulingAlgorithmLegacy, svc.chooseSchedulingAlgorithm(&gid9))
}

// LegacyGroups 优先级最高，即使全局是 weighted 且在 WeightedGroups 中也强制 legacy。
func TestChooseSchedulingAlgorithm_LegacyGroupOverridesAll(t *testing.T) {
	svc := newSchedulingTestService(config.GatewaySchedulingConfig{
		Algorithm:      schedulingAlgorithmWeighted,
		WeightedGroups: []int64{7},
		LegacyGroups:   []int64{7},
	})

	gid := int64(7)
	require.Equal(t, schedulingAlgorithmLegacy, svc.chooseSchedulingAlgorithm(&gid))
}

func TestResolvedSchedulingHealth_Defaults(t *testing.T) {
	svc := newSchedulingTestService(config.GatewaySchedulingConfig{})
	c := svc.resolvedSchedulingHealth()
	require.Equal(t, 10, c.WindowMinutes)
	require.Equal(t, 5, c.MinSamples)
	require.Equal(t, 5, c.ErrCountSoft)
	require.Equal(t, 10, c.ErrCountHard)
	require.InDelta(t, 0.3, c.ErrRateSoft, 1e-9)
	require.InDelta(t, 0.5, c.ErrRateHard, 1e-9)
	require.Equal(t, 8000, c.TTFTStickyOnlyMs)
	require.InDelta(t, 10.0, c.OTPSStickyOnlyMin, 1e-9)
}

func TestResolvedSchedulingHealth_OverridesPreserved(t *testing.T) {
	svc := newSchedulingTestService(config.GatewaySchedulingConfig{
		Health: config.SchedulingHealthConfig{
			WindowMinutes:     5,
			MinSamples:        3,
			ErrRateSoft:       0.2,
			TTFTStickyOnlyMs:  6000,
			OTPSStickyOnlyMin: 20,
		},
	})
	c := svc.resolvedSchedulingHealth()
	require.Equal(t, 5, c.WindowMinutes)
	require.Equal(t, 3, c.MinSamples)
	require.InDelta(t, 0.2, c.ErrRateSoft, 1e-9)
	require.Equal(t, 6000, c.TTFTStickyOnlyMs)
	require.InDelta(t, 20.0, c.OTPSStickyOnlyMin, 1e-9)
	// 未指定的字段仍走默认
	require.Equal(t, 5, c.ErrCountSoft)
	require.Equal(t, 10, c.ErrCountHard)
	require.InDelta(t, 0.5, c.ErrRateHard, 1e-9)
}

func TestResolvedScoreWeights_Defaults(t *testing.T) {
	svc := newSchedulingTestService(config.GatewaySchedulingConfig{})
	w := svc.resolvedScoreWeights()
	require.InDelta(t, 1.5, w.ErrRate, 1e-9)
	require.InDelta(t, 1.2, w.TTFT, 1e-9)
	require.InDelta(t, 1.0, w.OTPS, 1e-9)
	require.InDelta(t, 0.3, w.Load, 1e-9)
	require.InDelta(t, 0.5, w.Priority, 1e-9)
}

func TestResolvedScoreThresholds_Defaults(t *testing.T) {
	svc := newSchedulingTestService(config.GatewaySchedulingConfig{})
	c := svc.resolvedScoreThresholds()
	require.Equal(t, 1500, c.TTFTBestMs)
	require.Equal(t, 6000, c.TTFTWorstMs)
	require.InDelta(t, 80.0, c.OTPSBest, 1e-9)
	require.InDelta(t, 10.0, c.OTPSWorst, 1e-9)
	require.InDelta(t, 70.0, c.LoadThresholdPct, 1e-9)
}

func TestResolvedSchedulingTopK_Default(t *testing.T) {
	svc := newSchedulingTestService(config.GatewaySchedulingConfig{})
	require.Equal(t, 5, svc.resolvedSchedulingTopK())

	svc = newSchedulingTestService(config.GatewaySchedulingConfig{TopK: 12})
	require.Equal(t, 12, svc.resolvedSchedulingTopK())
}
