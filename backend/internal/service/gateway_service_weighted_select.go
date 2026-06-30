// gateway_service_weighted_select.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：Layer 2 性价比选号。
//
// 核心公式：
//
//	score   = quality / effRate^β
//	quality = 0.6 × ttftScore + 0.4 × otpsScore               // 0~1，体验加权和
//	effRate = rate × (1 - 0.9 × shrunkHit)                    // 期望单 token 成本
//
//	ttftScore = clamp01(1 - p90 / 15000)                       // 无样本取 1
//	otpsScore = clamp01((otps - 20) / (50 - 20))               // 20→0 分，50→满分；无样本取 1
//
// 设计要点：
//   - 成功率不进 quality：账号可靠性由 HealthVerdict 门禁（10min 窗口）前置硬过滤，
//     评分只负责"活着的账号之间按性价比优选"，职责不重叠。
//   - 缓存命中率不进 quality（避免自增强污染），只折进 effRate。
//   - 冷启动账号无缓存样本时，用候选集平均缓存率乐观估计 effRate，
//     避免"无缓存显贵"导致新账号一直选不上 → 一直没缓存的死循环。
//   - 选号不再加权随机，而是确定性："score 最高带（容差 CostTolerance）内选当前最空的账号"，
//     既可解释，又自带负载均衡与冷启动预热（冷账号 load 最低，优先被选）。
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
	wsTTFTZeroMs        = 15000.0 // TTFT P90 ≥ 15s → ttftScore = 0
	wsOTPSZeroScore     = 20.0    // OTPS ≤ 20 tokens/s → otpsScore = 0
	wsOTPSFullScore     = 50.0    // OTPS ≥ 50 tokens/s → otpsScore = 1
	wsQualityTTFTWeight = 0.6     // quality 中 TTFT 占比（用户对首字节更敏感）
	wsQualityOTPSWeight = 0.4     // quality 中 OTPS 占比
	wsCacheDiscount     = 0.9     // 缓存命中折扣强度（对应 Anthropic cache_read ≈ 0.1× 价格）
	wsCacheHitShrinkK   = 20.0    // 命中率样本量收缩系数：effHit = hit × N/(N+K)
	wsColdStartCacheN   = 20      // 冷启动乐观估计的等效样本量（配合 ShrinkK 给半折，温和乐观）
	wsMinRateMultiplier = 0.05    // 倍率下限，防止 0 倍率账号 effRate → 0
	wsCostToleranceDef  = 0.15    // 成本容差带默认宽度
)

// weightedSelectionConfig 返回填充默认值后的选号配置。
func (s *GatewayService) weightedSelectionConfig() weightedSelectionRuntimeConfig {
	c := s.schedulingConfig().WeightedSelection
	rt := weightedSelectionRuntimeConfig{
		QualityWindowMinutes: c.QualityWindowMinutes,
		CostAggressiveness:   c.CostAggressiveness,
		CostTolerance:        c.CostTolerance,
	}
	if rt.QualityWindowMinutes <= 0 {
		rt.QualityWindowMinutes = defaultQualityWindowMinutes
	}
	if rt.CostAggressiveness <= 0 {
		rt.CostAggressiveness = 1.0
	}
	if rt.CostTolerance <= 0 {
		rt.CostTolerance = wsCostToleranceDef
	}
	if rt.CostTolerance > 1 {
		rt.CostTolerance = 1
	}
	return rt
}

// weightedSelectionRuntimeConfig 内部使用，与 config 解耦（兼容期里 config struct 仍带废弃字段）。
type weightedSelectionRuntimeConfig struct {
	QualityWindowMinutes int
	CostAggressiveness   float64
	CostTolerance        float64
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
	TTFTScore    float64 // TTFT 子分（0~1）
	OTPSScore    float64 // OTPS 子分（0~1）
	TTFTP90Ms    float64 // 0 表示无样本
	OTPSAvg      float64 // 0 表示无样本
	CacheHitRate float64 // -1 表示无样本
	CacheHitN    int     // 缓存命中率样本数
	Rate         float64 // 账号倍率（clamp 后）
	EffRate      float64 // 有效成本倍率（含缓存折扣）
	Score        float64 // 最终性价比分 = quality / effRate^β
	Load         int     // 当前负载率（0~100+）
}

// computeQualityScore 计算账号在指定模型上的综合质量分（0~1）：
//
//	quality   = 0.6 × ttftScore + 0.4 × otpsScore
//	ttftScore = clamp01(1 - p90 / 15000)     // 无样本取 1
//	otpsScore = clamp01((otps - 20) / 30)    // 无样本取 1
//
// 无样本各项取 1（乐观），让冷启动账号有充分探索机会。
// 成功率不在此计算——账号可靠性由 HealthVerdict 门禁前置过滤，评分不再重复判定。
// TTFT/OTPS 来自 account+model 质量窗口（默认 60min）。
func (s *GatewayService) computeQualityScore(accountID int64, model string) (float64, qualityScoreDetail) {
	d := qualityScoreDetail{CacheHitRate: -1}

	ttftScore, otpsScore := 1.0, 1.0

	if s.modelQualityCache != nil {
		snap, p90 := s.modelQualityCache.SnapshotWithP90(accountID, model)
		if snap.HasTTFT() {
			ttftRef := p90
			if ttftRef <= 0 {
				ttftRef = snap.TTFTAvg() // P90 不可得时退化用 avg
			}
			d.TTFTP90Ms = ttftRef
			ttftScore = clamp01(1 - ttftRef/wsTTFTZeroMs)
		}
		if snap.HasOTPS() {
			d.OTPSAvg = snap.OTPSAvg()
			otpsScore = clamp01((snap.OTPSAvg() - wsOTPSZeroScore) / (wsOTPSFullScore - wsOTPSZeroScore))
		}
		if snap.HasCacheHit() {
			d.CacheHitRate = snap.CacheHitRateAvg()
			d.CacheHitN = snap.CacheHitSampleCount
		}
	}

	d.TTFTScore = ttftScore
	d.OTPSScore = otpsScore
	d.Quality = wsQualityTTFTWeight*ttftScore + wsQualityOTPSWeight*otpsScore
	return d.Quality, d
}

// QualityScoreForAccount 返回账号在指定模型上的综合质量分（0~1），供 admin 观测使用。
// 加权选号未启用时仍可调用。
func (s *GatewayService) QualityScoreForAccount(accountID int64, model string) float64 {
	q, _ := s.computeQualityScore(accountID, model)
	return q
}

// selectByWeightedQuality 在已通过全部过滤、LoadRate<100 的候选中按性价比确定性选号。
//
// 流程：
//  1. filterByMinPriority：Priority 人工意图最高，严格取最小值组；
//  2. 每个候选计算 score = quality / effRate^β（冷账号缓存用候选集平均乐观估计）；
//  3. 取 score 最高带（score ≥ maxScore×(1-CostTolerance)）内当前负载最低的账号。
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

	// 第一遍：算 quality + 倍率，并收集有样本账号的缓存率用于冷启动乐观估计。
	sumHit, cntHit := 0.0, 0
	for i := range candidates {
		_, d := s.computeQualityScore(candidates[i].account.ID, model)
		d.Rate = clampRate(candidates[i].account.BillingRateMultiplier())
		details[i] = d
		if d.CacheHitN > 0 && d.CacheHitRate >= 0 {
			sumHit += d.CacheHitRate
			cntHit++
		}
	}
	avgHit := -1.0
	if cntHit > 0 {
		avgHit = sumHit / float64(cntHit)
	}

	// 第二遍：算 effRate（冷账号乐观）+ score，记录最高分。
	maxScore := 0.0
	for i := range details {
		hit, n := details[i].CacheHitRate, details[i].CacheHitN
		if (n <= 0 || hit < 0) && avgHit >= 0 {
			// 冷启动乐观估计：用候选集平均缓存率，避免"无缓存显贵"饿死新账号。
			hit, n = avgHit, wsColdStartCacheN
		}
		details[i].EffRate = applyCacheDiscount(details[i].Rate, hit, n)
		details[i].Score = details[i].Quality / math.Pow(details[i].EffRate, cfg.CostAggressiveness)
		if details[i].Score > maxScore {
			maxScore = details[i].Score
		}
	}

	// 第三遍：score 容差带内按负载选最空（冷账号 load=0 天然优先，自带预热）。
	// maxScore=0（极端：全候选 quality=0）时 threshold=0，全部入带，退化为纯选最空。
	threshold := maxScore * (1 - cfg.CostTolerance)
	selectedIdx, bestLoad := 0, int(^uint(0)>>1)
	for i := range details {
		load := 0
		if candidates[i].loadInfo != nil {
			load = candidates[i].loadInfo.LoadRate
		}
		details[i].Load = load
		if details[i].Score < threshold {
			continue
		}
		if load < bestLoad {
			bestLoad = load
			selectedIdx = i
		}
	}

	s.logWeightedSelection(candidates, details, selectedIdx, model, groupID, sessionHash)
	return &candidates[selectedIdx]
}

// clampRate 倍率下限保护，防止 0 倍率账号 effRate → 0 引发 score 无穷大。
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

// logWeightedSelection 打选号决策日志，复用 scheduling debug 触发规则：
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
	d := details[selectedIdx]
	logger.LegacyPrintf("service.gateway",
		"[WeightedSelect] group=%v session=%s model=%s candidates=%d selected=%d quality=%.3f rate=%.2f eff_rate=%.2f score=%.3f load=%d",
		derefGroupID(groupID), shortSessionHash(sessionHash), model, len(candidates),
		sel.account.ID, d.Quality, d.Rate, d.EffRate, d.Score, d.Load)

	if !cfg.LogScoreDetails {
		return
	}
	var b strings.Builder
	for i := range candidates {
		d := details[i]
		b.WriteString(fmt.Sprintf(" [%d quality=%.3f ttft_p90=%.0fms otps=%.1f cache_hit=%.2f rate=%.2f eff_rate=%.2f score=%.3f load=%d]",
			candidates[i].account.ID, d.Quality, d.TTFTP90Ms, d.OTPSAvg, d.CacheHitRate, d.Rate, d.EffRate, d.Score, d.Load))
	}
	logger.LegacyPrintf("service.gateway", "[WeightedSelectDetail]%s", b.String())
}
