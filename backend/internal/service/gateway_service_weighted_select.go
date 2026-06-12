// gateway_service_weighted_select.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：Layer 2 质量加权选号。
//
// 设计文档：docs/anthropic-scheduling-weighted-selection.md
//
// 排序逻辑：Priority（人工意图）→ 质量相近带（体验优先，TTFT 含 P90 长尾惩罚）
//         → 带内按账号倍率倒数加权 → 加权随机；带外保留探索 floor。
//
// 新增方法：
//   - weightedSelectionConfig() / isWeightedSelectionEnabled()
//   - computeQualityScore(accountID, model) (score, detail)
//   - selectByWeightedQuality(available, model, groupID, sessionHash) *accountWithLoad
//
// 与 upstream 合并策略：
//   - 本文件全部为新增，不修改任何 upstream 函数。
//   - gateway_service.go 的 Layer 2 漏斗只加一个 if/else hook（最小 inline）。
// =============================================================================
package service

import (
	"fmt"
	"math"
	mathrand "math/rand"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// 内置默认值（配置为 0 时生效），与设计文档 §6 一致。
const (
	defaultWSBandEpsilon    = 0.10
	defaultWSCostBeta       = 1.0
	defaultWSExploreFloor   = 0.05
	defaultWSTTFTFullMs     = 1000.0
	defaultWSTTFTZeroMs     = 10000.0
	defaultWSOTPSFull       = 80.0
	defaultWSWTTFT          = 0.40
	defaultWSWOTPS          = 0.35
	defaultWSWErr           = 0.25
	wsNeutralScore          = 0.5  // 无样本中性分
	wsMinRateMultiplier     = 0.05 // 倍率下限，防止 0 倍率账号权重无穷大
)

// weightedSelectionConfig 返回填充默认值后的加权选号配置。
func (s *GatewayService) weightedSelectionConfig() config.WeightedSelectionConfig {
	c := s.schedulingConfig().WeightedSelection
	if c.QualityWindowMinutes <= 0 {
		c.QualityWindowMinutes = defaultQualityWindowMinutes
	}
	if c.BandEpsilon <= 0 {
		c.BandEpsilon = defaultWSBandEpsilon
	}
	if c.CostBeta <= 0 {
		c.CostBeta = defaultWSCostBeta
	}
	if c.ExploreFloor <= 0 {
		c.ExploreFloor = defaultWSExploreFloor
	}
	if c.TTFTFullScoreMs <= 0 {
		c.TTFTFullScoreMs = int(defaultWSTTFTFullMs)
	}
	if c.TTFTZeroScoreMs <= c.TTFTFullScoreMs {
		c.TTFTZeroScoreMs = int(defaultWSTTFTZeroMs)
	}
	if c.OTPSFullScore <= 0 {
		c.OTPSFullScore = defaultWSOTPSFull
	}
	if c.WTTFT <= 0 && c.WOTPS <= 0 && c.WErr <= 0 {
		c.WTTFT, c.WOTPS, c.WErr = defaultWSWTTFT, defaultWSWOTPS, defaultWSWErr
	}
	return c
}

// isWeightedSelectionEnabled 返回是否启用质量加权选号，默认 false。
// 启用时顺带同步质量窗口长度（幂等，原子比较）。
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
	Score     float64
	TTFTEffMs float64 // 0 表示无样本
	OTPSAvg   float64 // 0 表示无样本
	ErrRate   float64 // -1 表示无样本
	Rate      float64 // 账号倍率（clamp 后）
	Weight    float64 // 最终抽签权重
	InBand    bool
}

// computeQualityScore 计算账号在指定模型上的连续质量分（0~1）。
//
//	score = wTTFT·ttftScore + wOTPS·otpsScore + wErr·errScore（权重归一化）
//	ttftEff = 0.5·avg + 0.5·P90（无 P90 样本时退化为 avg）
//	各分量无样本时取 0.5 中性分（冷启动不奖不罚）。
//
// 错误率来自账号级健康窗口（10min，HealthVerdict 同源），TTFT/OTPS 来自
// account+model 质量窗口（默认 60min）。定时测试数据不进质量窗口（见设计文档 §4）。
func (s *GatewayService) computeQualityScore(accountID int64, model string, cfg config.WeightedSelectionConfig) (float64, qualityScoreDetail) {
	d := qualityScoreDetail{ErrRate: -1}

	ttftScore, otpsScore, errScore := wsNeutralScore, wsNeutralScore, wsNeutralScore

	if s.modelQualityCache != nil {
		snap, p90 := s.modelQualityCache.SnapshotWithP90(accountID, model)
		if snap.HasTTFT() {
			ttftEff := snap.TTFTAvg()
			if p90 > 0 {
				ttftEff = 0.5*snap.TTFTAvg() + 0.5*p90
			}
			d.TTFTEffMs = ttftEff
			full, zero := float64(cfg.TTFTFullScoreMs), float64(cfg.TTFTZeroScoreMs)
			ttftScore = clamp01(1 - (ttftEff-full)/(zero-full))
		}
		if snap.HasOTPS() {
			d.OTPSAvg = snap.OTPSAvg()
			otpsScore = clamp01(snap.OTPSAvg() / cfg.OTPSFullScore)
		}
	}

	if s.healthCache != nil {
		hs := s.healthCache.Snapshot(accountID)
		if hs.ReqCount > 0 {
			d.ErrRate = hs.ErrRate()
			errScore = clamp01(1 - hs.ErrRate())
		}
	}

	wSum := cfg.WTTFT + cfg.WOTPS + cfg.WErr
	d.Score = (cfg.WTTFT*ttftScore + cfg.WOTPS*otpsScore + cfg.WErr*errScore) / wSum
	return d.Score, d
}

// selectByWeightedQuality 在已通过全部过滤、LoadRate<100 的候选中加权随机选号。
//
// 流程（设计文档 §3）：
//  1. filterByMinPriority：Priority 人工意图最高，严格取最小值组；
//  2. 组内算连续质量分，划"体验相近带"（score >= best - ε）；
//  3. 带内 weight = (1/倍率)^β，带外 weight = floor × 带内最大权重；
//  4. 按 weight 加权随机抽一个返回。
//
// 返回 nil 表示候选为空（调用方按原逻辑 break）。
func (s *GatewayService) selectByWeightedQuality(available []accountWithLoad, model string, groupID *int64, sessionHash string) *accountWithLoad {
	if len(available) == 0 {
		return nil
	}
	cfg := s.weightedSelectionConfig()

	candidates := filterByMinPriority(available)
	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) == 1 {
		return &candidates[0]
	}

	details := make([]qualityScoreDetail, len(candidates))
	bestScore := 0.0
	for i := range candidates {
		score, d := s.computeQualityScore(candidates[i].account.ID, model, cfg)
		details[i] = d
		if score > bestScore {
			bestScore = score
		}
	}

	// 划带 + 算权重：带内按倍率倒数，带外稍后统一给探索 floor。
	maxInBand := 0.0
	for i := range candidates {
		rate := candidates[i].account.BillingRateMultiplier()
		if rate < wsMinRateMultiplier {
			rate = wsMinRateMultiplier
		}
		details[i].Rate = rate
		if details[i].Score >= bestScore-cfg.BandEpsilon {
			details[i].InBand = true
			details[i].Weight = math.Pow(1/rate, cfg.CostBeta)
			if details[i].Weight > maxInBand {
				maxInBand = details[i].Weight
			}
		}
	}
	if maxInBand <= 0 {
		// 理论上不可达（bestScore 所在账号必然入带），防御性兜底为均匀随机。
		return &candidates[mathrand.Intn(len(candidates))]
	}
	floorWeight := cfg.ExploreFloor * maxInBand
	total := 0.0
	for i := range details {
		if !details[i].InBand {
			details[i].Weight = floorWeight
		}
		total += details[i].Weight
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

	s.logWeightedSelection(candidates, details, selectedIdx, bestScore, model, groupID, sessionHash)
	return &candidates[selectedIdx]
}

// logWeightedSelection 打加权选号决策日志，复用 scheduling debug 的触发规则：
// LogDecisions 全量 / LogGroups 白名单 / LogSampleRate 采样；
// LogScoreDetails=true 时展开每候选明细。
func (s *GatewayService) logWeightedSelection(candidates []accountWithLoad, details []qualityScoreDetail, selectedIdx int, bestScore float64, model string, groupID *int64, sessionHash string) {
	cfg := s.schedulingConfig().Debug
	if !cfg.LogDecisions && !groupContainedInLogList(groupID, cfg.LogGroups) {
		if cfg.LogSampleRate <= 0 || mathrand.Float64() > cfg.LogSampleRate {
			return
		}
	}

	sel := candidates[selectedIdx]
	logger.LegacyPrintf("service.gateway",
		"[WeightedSelect] group=%v session=%s model=%s candidates=%d best_score=%.3f selected=%d score=%.3f rate=%.2f weight=%.3f in_band=%v",
		derefGroupID(groupID), shortSessionHash(sessionHash), model, len(candidates), bestScore,
		sel.account.ID, details[selectedIdx].Score, details[selectedIdx].Rate,
		details[selectedIdx].Weight, details[selectedIdx].InBand)

	if !cfg.LogScoreDetails {
		return
	}
	var b strings.Builder
	for i := range candidates {
		d := details[i]
		b.WriteString(fmt.Sprintf(" [%d score=%.3f ttft_eff=%.0fms otps=%.1f err=%.2f rate=%.2f w=%.3f band=%v]",
			candidates[i].account.ID, d.Score, d.TTFTEffMs, d.OTPSAvg, d.ErrRate, d.Rate, d.Weight, d.InBand))
	}
	logger.LegacyPrintf("service.gateway", "[WeightedSelectDetail]%s", b.String())
}
