// gateway_service_scheduling.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：账号健康感知调度相关方法。
//
// 这个文件聚合了两类附着在 GatewayService 上的私有逻辑：
//   1. SchedulingHealthConfig 配置默认值的解析；
//   2. 账号健康缓存（healthCache）相关的接口（HealthCache、RecordAnthropicCall、
//      onHealthVerdictChange、isAccountSchedulableForHealth ...）。
//
// 还有一个零碎的 logSchedulerSelected 也在这里，便于后续如果上游引入类似日志我们可以
// 集中替换。
//
// 与 upstream 合并策略：
//   - 把这些方法搬到 companion 文件后，gateway_service.go 与 upstream 的 diff
//     主要剩下：构造函数参数、Service 结构体新增字段、SelectAccountWithLoadAwareness
//     里几处内联调用（健康过滤）。这些是无法外迁的。
//   - 真正定义 healthCache 的类型、CallSample、HealthVerdict 等都在
//     account_test_health_cache.go，跟本文件解耦。
// =============================================================================
package service

import (
	"context"
	"fmt"
	mathrand "math/rand"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// resolvedSchedulingHealth 返回 SchedulingHealthConfig，零值字段回退到内置默认。
func (s *GatewayService) resolvedSchedulingHealth() config.SchedulingHealthConfig {
	c := s.schedulingConfig().Health
	if c.WindowMinutes <= 0 {
		c.WindowMinutes = 10
	}
	if c.MinSamples <= 0 {
		c.MinSamples = 5
	}
	if c.ErrCountSoft <= 0 {
		c.ErrCountSoft = 3
	}
	if c.ErrCountHard <= 0 {
		c.ErrCountHard = 5
	}
	if c.ErrRateSoft <= 0 {
		c.ErrRateSoft = 0.3
	}
	if c.ErrRateHard <= 0 {
		c.ErrRateHard = 0.5
	}
	if c.TTFTStickyOnlyMs <= 0 {
		c.TTFTStickyOnlyMs = 10000
	}
	if c.OTPSStickyOnlyMin <= 0 {
		c.OTPSStickyOnlyMin = 20
	}
	return c
}

// groupContainedInLogList 判断 groupID 是否在 debug.log_groups 白名单内。
func groupContainedInLogList(groupID *int64, list []int64) bool {
	if groupID == nil {
		return false
	}
	target := *groupID
	for _, id := range list {
		if id == target {
			return true
		}
	}
	return false
}

// HealthCache 暴露内部健康缓存指针，仅用于 admin 调试 / 监控接口。
// 调用方不应通过它绕过 gateway 调度路径写入数据。
func (s *GatewayService) HealthCache() *AccountTestHealthCache {
	if s == nil {
		return nil
	}
	return s.healthCache
}

// logSchedulerSelected 统一打"调度选中"日志，覆盖 sticky/weighted/legacy/fallback 各层。
//
// 触发规则（满足其一即打）：
//  1. cfg.Debug.LogDecisions = true（全量）
//  2. groupID 在 cfg.Debug.LogGroups 白名单内（指定 group 强制 100%）
//  3. cfg.Debug.LogSampleRate > 0 且 RNG 命中（采样）
func (s *GatewayService) logSchedulerSelected(layer string, account *Account, sessionHash string, groupID *int64, acquired bool) {
	if s == nil || account == nil {
		return
	}
	cfg := s.schedulingConfig().Debug
	if !cfg.LogDecisions && !groupContainedInLogList(groupID, cfg.LogGroups) {
		// 未启用且不在白名单 → 按采样率
		if cfg.LogSampleRate <= 0 || mathrand.Float64() > cfg.LogSampleRate {
			return
		}
	}
	logger.LegacyPrintf("service.gateway",
		"[SchedulerSelected] layer=%s group=%v session=%s account=%d priority=%d acquired=%v",
		layer, derefGroupID(groupID), shortSessionHash(sessionHash),
		account.ID, account.Priority, acquired)
}

// RecordAnthropicCall 上报一次真实 Anthropic 请求的完整样本。
// healthCache（account 级）供 HealthVerdict 三态判定；
// modelQualityCache（account+model 级）供质量分桶调度使用。
//
// 失败语义参考 docs/anthropic-scheduling-weighted.md §10：
//   - 上游错误（4xx/5xx/429/超时、UpstreamFailoverError 等）→ Success=false
//   - 客户端中断（context.Canceled）→ 调用方应跳过，不上报
//   - 请求内容问题（PromptTooLong/BetaBlocked）→ 调用方应跳过
func (s *GatewayService) RecordAnthropicCall(accountID int64, model string, sample CallSample) {
	if s.healthCache != nil {
		s.healthCache.RecordRealCall(accountID, sample)
	}
	s.modelQualityCache.Record(accountID, model, sample)
}

// ModelQualityCache 暴露内部质量缓存指针，仅供 admin 监控接口使用。
func (s *GatewayService) ModelQualityCache() *AccountModelQualityCache {
	if s == nil {
		return nil
	}
	return s.modelQualityCache
}

// AnthropicHealthSnapshot 返回账号当前 10min 滑动窗口的指标快照，供日志和监控使用。
// healthCache 为 nil 时返回零值快照（不影响调用方）。
func (s *GatewayService) AnthropicHealthSnapshot(accountID int64) HealthSnapshot {
	if s.healthCache == nil {
		return HealthSnapshot{}
	}
	snap, _, _ := s.healthCache.SnapshotAndVerdict(accountID)
	return snap
}

// AnthropicHealthVerdictThresholds 返回当前生效的 HealthVerdict 判定阈值，
// 供 handler 层日志使用，避免硬编码魔法值。
func (s *GatewayService) AnthropicHealthVerdictThresholds() HealthVerdictConfig {
	return s.healthVerdictConfig()
}

// ReportAnthropicAccountResult 将真实请求的成败结果上报到健康缓存（滑动窗口）。
// 与 ReportAnthropicAccountTTFT/Duration 配合使用，让"用户真实调用"和"主动 test"
// 共用同一份滑动窗口，使 HealthVerdict 三态判定能感知真实流量的失败。
//
// 失败语义参考 docs/anthropic-scheduling-weighted.md §10：
//   - 上游错误（4xx/5xx/429/超时）→ success=false
//   - 客户端中断（context.Canceled）→ 调用方应跳过，不上报
//   - 请求内容问题（PromptTooLong 等）→ 调用方应跳过，不上报
func (s *GatewayService) ReportAnthropicAccountResult(accountID int64, success bool) {
	if s.healthCache == nil {
		return
	}
	s.healthCache.ReportRealCall(accountID, success)
}

// healthVerdictConfig 把 SchedulingHealthConfig 转换为
// AccountTestHealthCache.HealthVerdict() 的入参。
// 基于滑动窗口快照的窗口指标判定（errCount/errRate/ttftAvg/otpsAvg）。
func (s *GatewayService) healthVerdictConfig() HealthVerdictConfig {
	c := s.resolvedSchedulingHealth()
	return HealthVerdictConfig{
		WindowSeconds:     int64(c.WindowMinutes) * 60,
		MinSamples:        c.MinSamples,
		ErrCountSoft:      c.ErrCountSoft,
		ErrCountHard:      c.ErrCountHard,
		ErrRateSoft:       c.ErrRateSoft,
		ErrRateHard:       c.ErrRateHard,
		TTFTStickyOnlyMs:  c.TTFTStickyOnlyMs,
		TTFTExcludedMs:    c.TTFTExcludedMs,
		OTPSStickyOnlyMin: c.OTPSStickyOnlyMin,
		OTPSExcludedMin:   c.OTPSExcludedMin,
	}
}

// isAccountHealthEnabled 返回是否启用账号健康感知调度（三态 + 硬过滤），默认 false。
func (s *GatewayService) isAccountHealthEnabled() bool {
	return s.schedulingConfig().AccountHealth.Enabled
}

// onHealthVerdictChange 在账号健康状态切换时由 AccountTestHealthCache 异步回调。
// 当状态切换为 HealthExcluded 时触发 TempUnschedulable，时长由配置决定（默认 30 分钟）。
func (s *GatewayService) onHealthVerdictChange(accountID int64, prev, current HealthVerdict) {
	if current != HealthExcluded {
		return
	}
	minutes := s.resolvedSchedulingHealth().ExcludedTempUnschedMinutes
	if minutes <= 0 {
		minutes = 30
	}
	until := time.Now().Add(time.Duration(minutes) * time.Minute)
	snap, _, reason := s.healthCache.SnapshotAndVerdictWithConfig(accountID, s.healthVerdictConfig())
	msg := fmt.Sprintf("health_excluded(%s) req=%d err=%d err_rate=%.1f%% ttft_avg=%.0fms otps_avg=%.1f",
		reason, snap.ReqCount, snap.ErrCount, snap.ErrRate()*100, snap.TTFTAvg(), snap.OTPSAvg())
	if err := s.rateLimitService.SetTempUnschedulableForScheduledTest(context.Background(), accountID, until, msg); err != nil {
		logger.LegacyPrintf("service.gateway", "[HealthExcluded] SetTempUnschedulable failed account_id=%d err=%v", accountID, err)
	}
}

// isAccountSchedulableForHealth 与 isAccountSchedulableForWindowCost / RPM 同款签名风格。
// 仅作用于 Anthropic 平台账号，沿用 WindowCostStickyOnly 三态语义：
//   - HealthOK         → 始终放行
//   - HealthStickyOnly → 仅 isSticky=true 路径放行（Layer 1.5 sticky 命中时通过；
//     Layer 2 新会话路径拒绝）
//   - HealthExcluded   → 触发 TempUnschedulable（由 OnVerdictChange 回调异步处理），
//     调度层与 StickyOnly 行为相同，后续由 TempUnschedulable 接管
func (s *GatewayService) isAccountSchedulableForHealth(account *Account, isSticky bool) bool {
	if account == nil || account.Platform != PlatformAnthropic {
		return true
	}
	if s == nil || s.healthCache == nil {
		return true
	}
	verdict, prev, changed := s.healthCache.HealthVerdictWithChange(account.ID, s.healthVerdictConfig())
	if changed {
		snap, _, reason := s.healthCache.SnapshotAndVerdictWithConfig(account.ID, s.healthVerdictConfig())
		logger.LegacyPrintf("service.gateway",
			"[WARN] [HealthVerdictChange] account_id=%d account_name=%s %s->%s reason=%q req=%d err=%d err_rate=%.1f%% ttft_avg=%.0fms otps_avg=%.1f slow_rate=%.1f%%",
			account.ID, account.Name, prev.String(), verdict.String(), reason,
			snap.ReqCount, snap.ErrCount, snap.ErrRate()*100,
			snap.TTFTAvg(), snap.OTPSAvg(), snap.SlowRate()*100)
	}
	switch verdict {
	case HealthOK:
		return true
	case HealthStickyOnly:
		return isSticky
	case HealthExcluded:
		// TempUnschedulable 由 OnVerdictChange 异步触发，生效前这里继续同步拦截。
		// TempUnschedulable 生效后 shouldClearStickySession 会清除 sticky 绑定，
		// 后续请求不再走到这里。
		return false
	}
	return true
}
