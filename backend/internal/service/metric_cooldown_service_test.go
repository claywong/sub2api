//go:build unit

package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func defaultMetricCooldownConfig() config.MetricCooldownConfig {
	return config.MetricCooldownConfig{
		Enabled:        true,
		Cron:           "*/5 * * * *",
		WindowMinutes:  30,
		MinSampleCount: 20,
		CooldownHours:  8,
		Rules: config.MetricCooldownRules{
			TTFTMs:       config.MetricCooldownRule{Enabled: true, Op: ">", Threshold: 5000},
			OTPS:         config.MetricCooldownRule{Enabled: true, Op: "<", Threshold: 10},
			CacheHitRate: config.MetricCooldownRule{Enabled: true, Op: "<", Threshold: 30},
			CostPerReq:   config.MetricCooldownRule{Enabled: true, Op: ">", Threshold: 0.05},
		},
	}
}

func TestResolveEffectiveMetricCooldown_NoOverride(t *testing.T) {
	g := defaultMetricCooldownConfig()
	got := resolveEffectiveMetricCooldown(g, nil)
	require.Equal(t, g, got)
	require.Equal(t, g, resolveEffectiveMetricCooldown(g, map[string]any{}))
	require.Equal(t, g, resolveEffectiveMetricCooldown(g, map[string]any{"other": 1}))
}

func TestResolveEffectiveMetricCooldown_PartialOverride(t *testing.T) {
	g := defaultMetricCooldownConfig()
	extra := map[string]any{
		metricCooldownExtraKey: map[string]any{
			"min_sample_count": 50,
			"rules": map[string]any{
				"ttft_ms": map[string]any{"threshold": 10000.0},
				"otps":    map[string]any{"enabled": false},
			},
		},
	}
	got := resolveEffectiveMetricCooldown(g, extra)
	require.Equal(t, 50, got.MinSampleCount)
	require.Equal(t, 30, got.WindowMinutes, "未覆盖字段应继承全局")
	require.InDelta(t, 10000.0, got.Rules.TTFTMs.Threshold, 0.0001)
	require.Equal(t, ">", got.Rules.TTFTMs.Op, "rule.op 未覆盖应继承")
	require.True(t, got.Rules.TTFTMs.Enabled, "rule.enabled 未覆盖应继承")
	require.False(t, got.Rules.OTPS.Enabled, "rule.enabled 应被覆盖为 false")
	require.InDelta(t, 10.0, got.Rules.OTPS.Threshold, 0.0001, "OTPS threshold 应继承")
}

func TestResolveEffectiveMetricCooldown_DisablePerAccount(t *testing.T) {
	g := defaultMetricCooldownConfig()
	extra := map[string]any{metricCooldownExtraKey: map[string]any{"enabled": false}}
	got := resolveEffectiveMetricCooldown(g, extra)
	require.False(t, got.Enabled)
}

func ptrFMetric(v float64) *float64 { return &v }

func TestEvaluateMetricCooldown_NoHits(t *testing.T) {
	eff := defaultMetricCooldownConfig()
	snap := AccountMetricSnapshot{
		AccountID: 1, Samples: 100,
		TTFTMs: ptrFMetric(1000), OTPS: ptrFMetric(50), CacheHitRate: ptrFMetric(80), CostPerReq: ptrFMetric(0.001),
	}
	reason, hit := evaluateMetricCooldown(eff, snap)
	require.False(t, hit)
	require.Empty(t, reason)
}

func TestEvaluateMetricCooldown_TTFTHigh(t *testing.T) {
	eff := defaultMetricCooldownConfig()
	snap := AccountMetricSnapshot{
		AccountID: 1, Samples: 100,
		TTFTMs: ptrFMetric(8000), OTPS: ptrFMetric(50), CacheHitRate: ptrFMetric(80), CostPerReq: ptrFMetric(0.001),
	}
	reason, hit := evaluateMetricCooldown(eff, snap)
	require.True(t, hit)
	require.Contains(t, reason, "ttft_ms=8000>5000")
	require.Contains(t, reason, "samples=100")
	require.Contains(t, reason, "window=30m")
}

func TestEvaluateMetricCooldown_OTPSLow(t *testing.T) {
	eff := defaultMetricCooldownConfig()
	snap := AccountMetricSnapshot{
		AccountID: 1, Samples: 100,
		TTFTMs: ptrFMetric(1000), OTPS: ptrFMetric(5), CacheHitRate: ptrFMetric(80), CostPerReq: ptrFMetric(0.001),
	}
	reason, hit := evaluateMetricCooldown(eff, snap)
	require.True(t, hit)
	require.Contains(t, reason, "otps=5<10")
}

func TestEvaluateMetricCooldown_MultipleHits(t *testing.T) {
	eff := defaultMetricCooldownConfig()
	snap := AccountMetricSnapshot{
		AccountID: 1, Samples: 100,
		TTFTMs: ptrFMetric(8000), OTPS: ptrFMetric(5), CacheHitRate: ptrFMetric(10), CostPerReq: ptrFMetric(0.1),
	}
	reason, hit := evaluateMetricCooldown(eff, snap)
	require.True(t, hit)
	require.Contains(t, reason, "ttft_ms")
	require.Contains(t, reason, "otps")
	require.Contains(t, reason, "cache_hit_rate")
	require.Contains(t, reason, "cost_per_req")
}

func TestEvaluateMetricCooldown_NilMetricSkippedNotHit(t *testing.T) {
	eff := defaultMetricCooldownConfig()
	snap := AccountMetricSnapshot{
		AccountID: 1, Samples: 100,
		TTFTMs: nil, OTPS: ptrFMetric(50), CacheHitRate: ptrFMetric(80), CostPerReq: ptrFMetric(0.001),
	}
	reason, hit := evaluateMetricCooldown(eff, snap)
	require.False(t, hit, "nil 指标即便高也不触发")
	require.Empty(t, reason)
}

func TestEvaluateMetricCooldown_DisabledRuleNotHit(t *testing.T) {
	eff := defaultMetricCooldownConfig()
	eff.Rules.TTFTMs.Enabled = false
	snap := AccountMetricSnapshot{
		AccountID: 1, Samples: 100,
		TTFTMs: ptrFMetric(99999), OTPS: ptrFMetric(50), CacheHitRate: ptrFMetric(80), CostPerReq: ptrFMetric(0.001),
	}
	_, hit := evaluateMetricCooldown(eff, snap)
	require.False(t, hit)
}

// ---------- runScan 集成 ----------

type stubMetricCooldownRepo struct {
	calls     int
	since     time.Time
	end       time.Time
	min       int
	snapshots []AccountMetricSnapshot
	err       error
}

func (r *stubMetricCooldownRepo) AggregateAccountMetrics(_ context.Context, since, end time.Time, minSamples int) ([]AccountMetricSnapshot, error) {
	r.calls++
	r.since, r.end, r.min = since, end, minSamples
	return r.snapshots, r.err
}

type stubAccountRepoForCooldown struct {
	mockAccountRepoForGemini
	accounts             map[int64]*Account
	setTempCalls         []setTempUnschedRecord
	setTempUnschedulable error
}

type setTempUnschedRecord struct {
	accountID int64
	until     time.Time
	reason    string
}

func (r *stubAccountRepoForCooldown) GetByID(_ context.Context, id int64) (*Account, error) {
	if a, ok := r.accounts[id]; ok {
		return a, nil
	}
	return nil, nil
}

func (r *stubAccountRepoForCooldown) SetTempUnschedulable(_ context.Context, id int64, until time.Time, reason string) error {
	r.setTempCalls = append(r.setTempCalls, setTempUnschedRecord{id, until, reason})
	return r.setTempUnschedulable
}

func TestMetricCooldownService_RunScan_TriggersCooldown(t *testing.T) {
	cfg := &config.Config{MetricCooldown: defaultMetricCooldownConfig()}
	repo := &stubMetricCooldownRepo{
		snapshots: []AccountMetricSnapshot{
			{AccountID: 11, Samples: 50, TTFTMs: ptrFMetric(8000), OTPS: ptrFMetric(50), CacheHitRate: ptrFMetric(80), CostPerReq: ptrFMetric(0.001)},
		},
	}
	accountRepo := &stubAccountRepoForCooldown{
		accounts: map[int64]*Account{11: {ID: 11, Status: StatusActive}},
	}
	rate := NewRateLimitService(accountRepo, nil, cfg, nil, nil)
	svc := NewMetricCooldownService(cfg, repo, accountRepo, rate)

	svc.runScan(context.Background())

	require.Equal(t, 1, repo.calls)
	require.Len(t, accountRepo.setTempCalls, 1)
	require.Equal(t, int64(11), accountRepo.setTempCalls[0].accountID)
	require.True(t, strings.HasPrefix(accountRepo.setTempCalls[0].reason, ManualCooldownReasonPrefix))
	require.Contains(t, accountRepo.setTempCalls[0].reason, "ttft_ms")
	// 8 小时窗口
	delta := accountRepo.setTempCalls[0].until.Sub(time.Now())
	require.InDelta(t, (8 * time.Hour).Seconds(), delta.Seconds(), 60.0)
}

func TestMetricCooldownService_RunScan_BelowMinSampleSkipped(t *testing.T) {
	cfg := &config.Config{MetricCooldown: defaultMetricCooldownConfig()}
	// 全局 min=20，账号实际 10 → 跳过
	repo := &stubMetricCooldownRepo{
		snapshots: []AccountMetricSnapshot{
			{AccountID: 12, Samples: 10, TTFTMs: ptrFMetric(99999)},
		},
	}
	accountRepo := &stubAccountRepoForCooldown{
		accounts: map[int64]*Account{12: {ID: 12}},
	}
	rate := NewRateLimitService(accountRepo, nil, cfg, nil, nil)
	NewMetricCooldownService(cfg, repo, accountRepo, rate).runScan(context.Background())
	require.Empty(t, accountRepo.setTempCalls)
}

func TestMetricCooldownService_RunScan_NoHitNoCall(t *testing.T) {
	cfg := &config.Config{MetricCooldown: defaultMetricCooldownConfig()}
	repo := &stubMetricCooldownRepo{
		snapshots: []AccountMetricSnapshot{
			{AccountID: 13, Samples: 100, TTFTMs: ptrFMetric(100), OTPS: ptrFMetric(100), CacheHitRate: ptrFMetric(99), CostPerReq: ptrFMetric(0.0001)},
		},
	}
	accountRepo := &stubAccountRepoForCooldown{accounts: map[int64]*Account{13: {ID: 13}}}
	rate := NewRateLimitService(accountRepo, nil, cfg, nil, nil)
	NewMetricCooldownService(cfg, repo, accountRepo, rate).runScan(context.Background())
	require.Empty(t, accountRepo.setTempCalls)
}

func TestMetricCooldownService_RunScan_AccountOverrideDisabled(t *testing.T) {
	cfg := &config.Config{MetricCooldown: defaultMetricCooldownConfig()}
	repo := &stubMetricCooldownRepo{
		snapshots: []AccountMetricSnapshot{
			{AccountID: 14, Samples: 100, TTFTMs: ptrFMetric(99999)},
		},
	}
	accountRepo := &stubAccountRepoForCooldown{
		accounts: map[int64]*Account{14: {
			ID: 14,
			Extra: map[string]any{
				metricCooldownExtraKey: map[string]any{"enabled": false},
			},
		}},
	}
	rate := NewRateLimitService(accountRepo, nil, cfg, nil, nil)
	NewMetricCooldownService(cfg, repo, accountRepo, rate).runScan(context.Background())
	require.Empty(t, accountRepo.setTempCalls, "account override 关掉后不应触发")
}

func TestMetricCooldownService_RunScan_AccountOverrideHigherThreshold(t *testing.T) {
	cfg := &config.Config{MetricCooldown: defaultMetricCooldownConfig()}
	repo := &stubMetricCooldownRepo{
		snapshots: []AccountMetricSnapshot{
			{AccountID: 15, Samples: 100, TTFTMs: ptrFMetric(8000), OTPS: ptrFMetric(50), CacheHitRate: ptrFMetric(80), CostPerReq: ptrFMetric(0.001)},
		},
	}
	accountRepo := &stubAccountRepoForCooldown{
		accounts: map[int64]*Account{15: {
			ID: 15,
			Extra: map[string]any{
				metricCooldownExtraKey: map[string]any{
					"rules": map[string]any{
						"ttft_ms": map[string]any{"threshold": 10000.0},
					},
				},
			},
		}},
	}
	rate := NewRateLimitService(accountRepo, nil, cfg, nil, nil)
	NewMetricCooldownService(cfg, repo, accountRepo, rate).runScan(context.Background())
	require.Empty(t, accountRepo.setTempCalls, "覆盖后 8000<10000 应不触发")
}

func TestMetricCooldownService_RunScan_GloballyDisabled(t *testing.T) {
	g := defaultMetricCooldownConfig()
	g.Enabled = false
	cfg := &config.Config{MetricCooldown: g}
	repo := &stubMetricCooldownRepo{}
	accountRepo := &stubAccountRepoForCooldown{}
	rate := NewRateLimitService(accountRepo, nil, cfg, nil, nil)
	NewMetricCooldownService(cfg, repo, accountRepo, rate).runScan(context.Background())
	require.Equal(t, 0, repo.calls, "全局关闭则不查 DB")
}
