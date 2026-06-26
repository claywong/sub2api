// gateway_service_weighted_select.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：Layer 2 性价比加权选号。
//
// 核心公式（一行）：
//
//	weight = quality / effRate^β
//
//	quality = (1 - errRate) × ttftFactor × otpsFactor       // 0~1
//	effRate = rate × (1 - 0.9 × shrunkHit)                  // 期望单 token 成本
//
// 无样本时 ttftFactor/otpsFactor/successRate 各自取 1（不奖不罚，鼓励冷启动探索）。
// 缓存命中率不进 quality（避免自增强污染），只折进 effRate。
//
// 新增方法：
//   - weightedSelectionConfig() / isWeightedSelectionEnabled()
//   - computeQualityScore(accountID, model) (quality, detail)
//   - QualityScoreForAccount(accountID, model) float64  ← admin 观测用
//   - selectByWeightedQuality(available, model, groupID, sessionHash) *accountWithLoad
//
// 与 upstream 合并策略：
//   - 本文件全部为新增，不修改任何 upstream 函数。
//   - gateway_service.go 的 Layer 2 漏斗只加一个 if/else hook（最小 inline）。
//
// =============================================================================
package service

import (
	"fmt"
	"math"
	mathrand "math/rand"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// 写死的内部常量：调参意义不大，藏在代码里减少配置噪音。
const (
	wsTTFTZeroMs        = 15000.0 // TTFT P90 ≥ 15s → ttftFactor = 0
	wsOTPSFullScore     = 80.0    // OTPS ≥ 80 tokens/s → otpsFactor = 1
	wsCacheDiscount     = 0.9     // 缓存命中折扣强度（对应 Anthropic cache_read ≈ 0.1× 价格）
	wsCacheHitShrinkK   = 20.0    // 命中率样本量收缩系数：effHit = hit × N/(N+K)
	wsMinRateMultiplier = 0.05    // 倍率下限，防止 0 倍率账号 effRate → 0
)

// weightedSelectionConfig 返回填充默认值后的加权选号配置。
// 极简方案：实际只用到 QualityWindowMinutes 和 CostAggressiveness。
func (s *GatewayService) weightedSelectionConfig() weightedSelectionRuntimeConfig {
	c := s.schedulingConfig().WeightedSelection
	rt := weightedSelectionRuntimeConfig{
		QualityWindowMinutes: c.QualityWindowMinutes,
		CostAggressiveness:   c.CostAggressiveness,
	}
	if rt.QualityWindowMinutes <= 0 {
		rt.QualityWindowMinutes = defaultQualityWindowMinutes
	}
	if rt.CostAggressiveness <= 0 {
		rt.CostAggressiveness = 1.0
	}
	return rt
}

// weightedSelectionRuntimeConfig 内部使用，与 config 解耦（兼容期里 config struct 仍带废弃字段）。
type weightedSelectionRuntimeConfig struct {
	QualityWindowMinutes int
	CostAggressiveness   float64
}

// isWeightedSelectionEnabled 返回是否启用加权选号，默认 false。
// 启用时同步质量窗口长度（幂等）。
func (s *GatewayService) isWeightedSelectionEnabled() bool {
	c := s.schedulingConfig().WeightedSelection
	if !c.Enabled {
		return false
	}
	if s.modelQualityCache != nil {
		s.modelQualityCache.ConfigureWindowMinutes(s.weightedSelectionConfig().QualityWindowMinutes)
	}
	return true
}

// clamp01 复用 openai_account_scheduler.go 中的声明。

// qualityScoreDetail 单账号打分明细，供调试日志使用。
type qualityScoreDetail struct {
	Quality      float64 // 综合质量分（0~1）
	TTFTP90Ms    float64 // 0 表示无样本
	OTPSAvg      float64 // 0 表示无样本
	ErrRate      float64 // -1 表示无样本
	CacheHitRate float64 // -1 表示无样本
	CacheHitN    int     // 缓存命中率样本数
	Rate         float64 // 账号倍率（clamp 后）
	EffRate      float64 // 有效成本倍率（含缓存折扣）
	Weight       float64 // 最终抽签权重
}

// computeQualityScore 计算账号在指定模型上的综合质量分（0~1）：
//
//	quality = (1 - errRate) × ttftFactor × otpsFactor
//	ttftFactor = clamp01(1 - p90 / 15000)    // 无样本取 1
//	otpsFactor = clamp01(otpsAvg / 80)       // 无样本取 1
//	successRate = 1 - errRate                // 无样本取 1
//
// 无样本各项取 1（不奖不罚），让冷启动账号有充分探索机会。
// 错误率来自账号级健康窗口（10min），TTFT/OTPS 来自 account+model 质量窗口（默认 60min）。
func (s *GatewayService) computeQualityScore(accountID int64, model string) (float64, qualityScoreDetail) {
	d := qualityScoreDetail{ErrRate: -1, CacheHitRate: -1}

	ttftFactor, otpsFactor, successRate := 1.0, 1.0, 1.0

	if s.modelQualityCache != nil {
		snap, p90 := s.modelQualityCache.SnapshotWithP90(accountID, model)
		if snap.HasTTFT() {
			ttftRef := p90
			if ttftRef <= 0 {
				ttftRef = snap.TTFTAvg() // P90 不可得时退化用 avg
			}
			d.TTFTP90Ms = ttftRef
			ttftFactor = clamp01(1 - ttftRef/wsTTFTZeroMs)
		}
		if snap.HasOTPS() {
			d.OTPSAvg = snap.OTPSAvg()
			otpsFactor = clamp01(snap.OTPSAvg() / wsOTPSFullScore)
		}
		if snap.HasCacheHit() {
			d.CacheHitRate = snap.CacheHitRateAvg()
			d.CacheHitN = snap.CacheHitSampleCount
		}
	}

	if s.healthCache != nil {
		hs := s.healthCache.Snapshot(accountID)
		if hs.ReqCount > 0 {
			d.ErrRate = hs.ErrRate()
			successRate = clamp01(1 - hs.ErrRate())
		}
	}

	d.Quality = successRate * ttftFactor * otpsFactor
	return d.Quality, d
}

// QualityScoreForAccount 返回账号在指定模型上的综合质量分（0~1），供 admin 观测使用。
// 加权选号未启用时仍可调用。
func (s *GatewayService) QualityScoreForAccount(accountID int64, model string) float64 {
	q, _ := s.computeQualityScore(accountID, model)
	return q
}

// selectByWeightedQuality 在已通过全部过滤、LoadRate<100 的候选中按性价比加权随机选号。
//
// 流程：
//  1. filterByMinPriority：Priority 人工意图最高，严格取最小值组；
//  2. 每个候选计算 weight = quality / effRate^β；
//  3. 按 weight 加权随机抽一个。
//
// 返回 nil 表示候选为空（调用方按原逻辑 break）。
func (s *GatewayService) selectByWeightedQuality(available []accountWithLoad, model string, groupID *int64, sessionHash string) *accountWithLoad {
	if len(available) == 0 {
		return nil
	}
	candidates := filterByMinPriority(available)
	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) == 1 {
		return &candidates[0]
	}

	cfg := s.weightedSelectionConfig()
	details := make([]qualityScoreDetail, len(candidates))
	total := 0.0
	for i := range candidates {
		_, d := s.computeQualityScore(candidates[i].account.ID, model)
		d.Rate = clampRate(candidates[i].account.BillingRateMultiplier())
		d.EffRate = applyCacheDiscount(d.Rate, d.CacheHitRate, d.CacheHitN)
		d.Weight = d.Quality / math.Pow(d.EffRate, cfg.CostAggressiveness)
		details[i] = d
		total += d.Weight
	}

	// 全 0 兜底：所有候选 quality=0（极端：全部账号错误率 100%）→ 均匀随机给一次机会。
	if total <= 0 {
		return &candidates[mathrand.Intn(len(candidates))]
	}

	r := mathrand.Float64() * total
	selectedIdx := len(candidates) - 1
	for i := range details {
		r -= details[i].Weight
		if r < 0 {
			selectedIdx = i
			break
		}
	}

	s.logWeightedSelection(candidates, details, selectedIdx, model, groupID, sessionHash)
	return &candidates[selectedIdx]
}

// clampRate 倍率下限保护，防止 0 倍率账号 effRate → 0 引发权重无穷大。
func clampRate(r float64) float64 {
	if r < wsMinRateMultiplier {
		return wsMinRateMultiplier
	}
	return r
}

// applyCacheDiscount 把缓存命中率折进 effRate：
//
//	effRate = rate × (1 - 0.9 × shrunkHit)
//	shrunkHit = hit × N/(N+20)   // 样本量收缩，防止少样本虚高
//
// 无样本时返回 rate（命中率信息缺失，不做折扣）。
func applyCacheDiscount(rate, hit float64, n int) float64 {
	if n <= 0 || hit < 0 {
		return rate
	}
	h := clamp01(hit)
	effHit := h * float64(n) / (float64(n) + wsCacheHitShrinkK)
	eff := rate * (1 - wsCacheDiscount*effHit)
	if eff < wsMinRateMultiplier {
		return wsMinRateMultiplier
	}
	return eff
}

// logWeightedSelection 打加权选号决策日志，复用 scheduling debug 触发规则：
// LogDecisions 全量 / LogGroups 白名单 / LogSampleRate 采样；
// LogScoreDetails=true 时展开每候选明细。
func (s *GatewayService) logWeightedSelection(candidates []accountWithLoad, details []qualityScoreDetail, selectedIdx int, model string, groupID *int64, sessionHash string) {
	cfg := s.schedulingConfig().Debug
	if !cfg.LogDecisions && !groupContainedInLogList(groupID, cfg.LogGroups) {
		if cfg.LogSampleRate <= 0 || mathrand.Float64() > cfg.LogSampleRate {
			return
		}
	}

	sel := candidates[selectedIdx]
	logger.LegacyPrintf("service.gateway",
		"[WeightedSelect] group=%v session=%s model=%s candidates=%d selected=%d quality=%.3f rate=%.2f eff_rate=%.2f weight=%.3f",
		derefGroupID(groupID), shortSessionHash(sessionHash), model, len(candidates),
		sel.account.ID, details[selectedIdx].Quality, details[selectedIdx].Rate,
		details[selectedIdx].EffRate, details[selectedIdx].Weight)

	if !cfg.LogScoreDetails {
		return
	}
	var b strings.Builder
	for i := range candidates {
		d := details[i]
		b.WriteString(fmt.Sprintf(" [%d quality=%.3f ttft_p90=%.0fms otps=%.1f err=%.2f cache_hit=%.2f rate=%.2f eff_rate=%.2f w=%.3f]",
			candidates[i].account.ID, d.Quality, d.TTFTP90Ms, d.OTPSAvg, d.ErrRate, d.CacheHitRate, d.Rate, d.EffRate, d.Weight))
	}
	logger.LegacyPrintf("service.gateway", "[WeightedSelectDetail]%s", b.String())
}
