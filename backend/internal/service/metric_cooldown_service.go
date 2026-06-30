package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/robfig/cron/v3"
)

const metricCooldownExtraKey = "metric_cooldown_override"

// MetricCooldownService 周期扫描账号近 N 分钟指标，命中阈值的账号打 manual-cooldown。
type MetricCooldownService struct {
	cfg          *config.Config
	repo         MetricCooldownRepository
	accountRepo  AccountRepository
	rateLimitSvc *RateLimitService

	cron      *cron.Cron
	startOnce sync.Once
	stopOnce  sync.Once
}

func NewMetricCooldownService(
	cfg *config.Config,
	repo MetricCooldownRepository,
	accountRepo AccountRepository,
	rateLimitSvc *RateLimitService,
) *MetricCooldownService {
	return &MetricCooldownService{
		cfg:          cfg,
		repo:         repo,
		accountRepo:  accountRepo,
		rateLimitSvc: rateLimitSvc,
	}
}

func (s *MetricCooldownService) Start() {
	if s == nil || s.cfg == nil || !s.cfg.MetricCooldown.Enabled {
		return
	}
	s.startOnce.Do(func() {
		loc := time.Local
		if parsed, err := time.LoadLocation(s.cfg.Timezone); err == nil && parsed != nil {
			loc = parsed
		}
		c := cron.New(cron.WithParser(scheduledTestCronParser), cron.WithLocation(loc))
		if _, err := c.AddFunc(s.cfg.MetricCooldown.Cron, func() { s.runScan(context.Background()) }); err != nil {
			logger.LegacyPrintf("service.metric_cooldown", "[MetricCooldown] not started (invalid cron %q): %v", s.cfg.MetricCooldown.Cron, err)
			return
		}
		s.cron = c
		s.cron.Start()
		logger.LegacyPrintf("service.metric_cooldown", "[MetricCooldown] started (cron=%s window=%dm cooldown=%.1fh)",
			s.cfg.MetricCooldown.Cron, s.cfg.MetricCooldown.WindowMinutes, s.cfg.MetricCooldown.CooldownHours)
	})
}

func (s *MetricCooldownService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.cron == nil {
			return
		}
		ctx := s.cron.Stop()
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
			logger.LegacyPrintf("service.metric_cooldown", "[MetricCooldown] cron stop timed out")
		}
	})
}

// runScan 跑一次扫描。导出供单测调用。
func (s *MetricCooldownService) runScan(ctx context.Context) {
	if s == nil || s.cfg == nil {
		return
	}
	global := s.cfg.MetricCooldown
	if !global.Enabled {
		return
	}
	now := time.Now()
	since := now.Add(-time.Duration(global.WindowMinutes) * time.Minute)
	scanStart := now

	snapshots, err := s.repo.AggregateAccountMetrics(ctx, since, now, global.MinSampleCount)
	if err != nil {
		slog.Warn("metric_cooldown_aggregate_failed",
			"window_minutes", global.WindowMinutes,
			"min_samples", global.MinSampleCount,
			"error", err)
		return
	}

	hits := 0
	for _, snap := range snapshots {
		acc, err := s.accountRepo.GetByID(ctx, snap.AccountID)
		if err != nil || acc == nil {
			continue
		}
		eff := resolveEffectiveMetricCooldown(global, acc.Extra)
		if !eff.Enabled {
			continue
		}
		if snap.Samples < eff.MinSampleCount {
			continue
		}
		reason, hit := evaluateMetricCooldown(eff, snap)
		if !hit {
			continue
		}
		duration := time.Duration(eff.CooldownHours * float64(time.Hour))
		if err := s.rateLimitSvc.SetManualCooldown(ctx, acc, duration, reason); err != nil {
			slog.Warn("metric_cooldown_set_failed",
				"account_id", acc.ID,
				"reason", reason,
				"error", err)
			continue
		}
		hits++
		slog.Warn("metric_cooldown_triggered",
			"account_id", acc.ID,
			"account_name", acc.Name,
			"platform", acc.Platform,
			"duration", duration.String(),
			"samples", snap.Samples,
			"window_minutes", eff.WindowMinutes,
			"ttft_ms", metricPtrValue(snap.TTFTMs),
			"otps", metricPtrValue(snap.OTPS),
			"cache_hit_rate", metricPtrValue(snap.CacheHitRate),
			"cost_per_req", metricPtrValue(snap.CostPerReq),
			"reason", reason)
	}
	slog.Info("metric_cooldown_scan_done",
		"accounts_evaluated", len(snapshots),
		"accounts_cooled", hits,
		"window_minutes", global.WindowMinutes,
		"elapsed_ms", time.Since(scanStart).Milliseconds())
}

func metricPtrValue(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

// resolveEffectiveMetricCooldown 把全局默认与 account.extra 中的 override 深合并。
// override 中存在的字段覆盖全局；缺失字段继承。
func resolveEffectiveMetricCooldown(global config.MetricCooldownConfig, extra map[string]any) config.MetricCooldownConfig {
	eff := global
	if extra == nil {
		return eff
	}
	rawOverride, ok := extra[metricCooldownExtraKey]
	if !ok {
		return eff
	}
	override, ok := rawOverride.(map[string]any)
	if !ok {
		return eff
	}
	if v, ok := boolValue(override, "enabled"); ok {
		eff.Enabled = v
	}
	if v, ok := intValue(override, "window_minutes"); ok && v > 0 {
		eff.WindowMinutes = v
	}
	if v, ok := intValue(override, "min_sample_count"); ok && v > 0 {
		eff.MinSampleCount = v
	}
	if v, ok := floatValue(override, "cooldown_hours"); ok && v > 0 {
		eff.CooldownHours = v
	}
	if rawRules, ok := override["rules"].(map[string]any); ok {
		eff.Rules.TTFTMs = mergeRule(eff.Rules.TTFTMs, rawRules["ttft_ms"])
		eff.Rules.OTPS = mergeRule(eff.Rules.OTPS, rawRules["otps"])
		eff.Rules.CacheHitRate = mergeRule(eff.Rules.CacheHitRate, rawRules["cache_hit_rate"])
		eff.Rules.CostPerReq = mergeRule(eff.Rules.CostPerReq, rawRules["cost_per_req"])
	}
	return eff
}

func mergeRule(base config.MetricCooldownRule, raw any) config.MetricCooldownRule {
	m, ok := raw.(map[string]any)
	if !ok {
		return base
	}
	if v, ok := boolValue(m, "enabled"); ok {
		base.Enabled = v
	}
	if v, ok := stringValue(m, "op"); ok && (v == ">" || v == "<") {
		base.Op = v
	}
	if v, ok := floatValue(m, "threshold"); ok {
		base.Threshold = v
	}
	return base
}

func boolValue(m map[string]any, key string) (bool, bool) {
	v, ok := m[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func intValue(m map[string]any, key string) (int, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		return int(t), true
	}
	return 0, false
}

func floatValue(m map[string]any, key string) (float64, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	}
	return 0, false
}

func stringValue(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// evaluateMetricCooldown 按生效规则评估指标快照。
// 任一启用规则命中即返回 reason，包含全部命中项与方向、阈值、实际值。
func evaluateMetricCooldown(eff config.MetricCooldownConfig, snap AccountMetricSnapshot) (string, bool) {
	type hit struct {
		name      string
		actual    float64
		op        string
		threshold float64
	}
	var hits []hit
	check := func(name string, rule config.MetricCooldownRule, actual *float64) {
		if !rule.Enabled || actual == nil {
			return
		}
		if compareMetricCooldown(*actual, rule.Op, rule.Threshold) {
			hits = append(hits, hit{name, *actual, rule.Op, rule.Threshold})
		}
	}
	check("ttft_ms", eff.Rules.TTFTMs, snap.TTFTMs)
	check("otps", eff.Rules.OTPS, snap.OTPS)
	check("cache_hit_rate", eff.Rules.CacheHitRate, snap.CacheHitRate)
	check("cost_per_req", eff.Rules.CostPerReq, snap.CostPerReq)
	if len(hits) == 0 {
		return "", false
	}
	parts := make([]string, 0, len(hits))
	for _, h := range hits {
		parts = append(parts, fmt.Sprintf("%s=%s%s%s", h.name, formatMetric(h.actual), h.op, formatMetric(h.threshold)))
	}
	reason := fmt.Sprintf("%s (window=%dm samples=%d)", strings.Join(parts, ", "), eff.WindowMinutes, snap.Samples)
	return reason, true
}

func compareMetricCooldown(actual float64, op string, threshold float64) bool {
	switch op {
	case ">":
		return actual > threshold
	case "<":
		return actual < threshold
	}
	return false
}

func formatMetric(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", v), "0"), ".")
}
