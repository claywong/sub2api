//go:build unit

package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// helper：构造接好 healthCache 的 svc
func newHealthGateService() *GatewayService {
	return &GatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				Scheduling: config.GatewaySchedulingConfig{
					Health: config.SchedulingHealthConfig{
						MinSamples:        5,
						ErrCountSoft:      5,
						ErrCountHard:      10,
						ErrRateSoft:       0.3,
						ErrRateHard:       0.5,
						TTFTStickyOnlyMs:  8000,
						OTPSStickyOnlyMin: 10,
					},
				},
			},
		},
		healthCache: NewAccountTestHealthCache(nil),
	}
}

func TestIsAccountSchedulableForHealth_NonAnthropicAlwaysAllowed(t *testing.T) {
	svc := newHealthGateService()
	openaiAcc := &Account{ID: 1, Platform: PlatformOpenAI}
	require.True(t, svc.isAccountSchedulableForHealth(openaiAcc, true))
	require.True(t, svc.isAccountSchedulableForHealth(openaiAcc, false))
}

func TestIsAccountSchedulableForHealth_NoCacheAlwaysAllowed(t *testing.T) {
	svc := &GatewayService{cfg: &config.Config{}}
	acc := &Account{ID: 1, Platform: PlatformAnthropic}
	require.True(t, svc.isAccountSchedulableForHealth(acc, true))
	require.True(t, svc.isAccountSchedulableForHealth(acc, false))
}

func TestIsAccountSchedulableForHealth_OKBothPathsAllowed(t *testing.T) {
	svc := newHealthGateService()
	acc := &Account{ID: 1, Platform: PlatformAnthropic}
	require.True(t, svc.isAccountSchedulableForHealth(acc, true))
	require.True(t, svc.isAccountSchedulableForHealth(acc, false))
}

func TestIsAccountSchedulableForHealth_StickyOnlyOnlyAllowsSticky(t *testing.T) {
	svc := newHealthGateService()
	acc := &Account{ID: 1, Platform: PlatformAnthropic}
	// 制造 StickyOnly：errCount=5（触发 ErrCountSoft），但 errRate=5/11≈0.45（未到 ErrRateHard 0.5）
	for i := 0; i < 5; i++ {
		svc.healthCache.ReportRealCall(acc.ID, false)
	}
	for i := 0; i < 6; i++ {
		svc.healthCache.ReportRealCall(acc.ID, true)
	}
	require.True(t, svc.isAccountSchedulableForHealth(acc, true), "sticky 路径应放行 StickyOnly")
	require.False(t, svc.isAccountSchedulableForHealth(acc, false), "新会话路径应拒绝 StickyOnly")
}

func TestIsAccountSchedulableForHealth_ExcludedBlocksAll(t *testing.T) {
	svc := newHealthGateService()
	acc := &Account{ID: 1, Platform: PlatformAnthropic}
	for i := 0; i < 10; i++ {
		svc.healthCache.ReportRealCall(acc.ID, false)
	}
	require.False(t, svc.isAccountSchedulableForHealth(acc, true), "Excluded 应拦截 sticky 路径")
	require.False(t, svc.isAccountSchedulableForHealth(acc, false), "Excluded 应拦截新会话路径")
}

func TestReportAnthropicAccountResult_FlowsThroughHealthCache(t *testing.T) {
	svc := newHealthGateService()
	svc.ReportAnthropicAccountResult(7, false)
	svc.ReportAnthropicAccountResult(7, false)
	h := svc.healthCache.Get(7)
	require.NotNil(t, h)
	require.Equal(t, 2, h.ConsecFails)

	svc.ReportAnthropicAccountResult(7, true)
	require.Equal(t, 0, svc.healthCache.Get(7).ConsecFails)
}

func TestReportAnthropicAccountResult_NoCacheNoPanic(t *testing.T) {
	svc := &GatewayService{cfg: &config.Config{}}
	require.NotPanics(t, func() {
		svc.ReportAnthropicAccountResult(7, true)
		svc.ReportAnthropicAccountResult(7, false)
	})
}

// logSchedulerSelected 主要是 IO 副作用，这里只验证开关逻辑不 panic 且
// 在 nil 输入下安全返回。
func TestLogSchedulerSelected_GuardsAgainstNil(t *testing.T) {
	require.NotPanics(t, func() {
		var svc *GatewayService
		svc.logSchedulerSelected("legacy", nil, "", nil, false)
	})

	svc := &GatewayService{cfg: &config.Config{
		Gateway: config.GatewayConfig{
			Scheduling: config.GatewaySchedulingConfig{
				Debug: config.SchedulingDebugConfig{LogDecisions: true},
			},
		},
	}}
	require.NotPanics(t, func() {
		svc.logSchedulerSelected("legacy", &Account{ID: 7, Priority: 10}, "abc", nil, true)
	})
}

// 采样率 0 + 不在白名单 + 未启用 LogDecisions → 不应实际打日志（无副作用，仅作回归）。
func TestLogSchedulerSelected_SamplingDisabled(t *testing.T) {
	svc := &GatewayService{cfg: &config.Config{
		Gateway: config.GatewayConfig{
			Scheduling: config.GatewaySchedulingConfig{
				Debug: config.SchedulingDebugConfig{
					LogDecisions:  false,
					LogGroups:     nil,
					LogSampleRate: 0,
				},
			},
		},
	}}
	gid := int64(99)
	require.NotPanics(t, func() {
		for i := 0; i < 50; i++ {
			svc.logSchedulerSelected("weighted", &Account{ID: 7, Priority: 10}, "session", &gid, true)
		}
	})
}
