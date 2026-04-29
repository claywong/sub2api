package service

import (
	"context"
	"hash/fnv"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// SchedulerWeightedMetrics 内存计数器，按需通过 admin endpoint 导出。
// 全局单例，无需注入；调度路径热点频率较低（每请求 1 次），atomic 即可。
type SchedulerWeightedMetrics struct {
	// 算法分流
	WeightedRequests atomic.Int64
	LegacyRequests   atomic.Int64

	// weighted 路径分支
	WeightedSelected         atomic.Int64 // 成功命中
	WeightedFallthroughLegacy atomic.Int64 // 全部抢槽失败 → legacy 兜底

	// 候选过滤（每次 weighted 调用累加）
	CandidatesEvaluated atomic.Int64
	NoHealthyAccount    atomic.Int64

	// 选中账号统计（按 accountID 聚合）
	mu             sync.RWMutex
	selectedByAcct map[int64]int64
}

// schedulerMetrics 全局单例
var schedulerMetrics = &SchedulerWeightedMetrics{
	selectedByAcct: make(map[int64]int64),
}

// SchedulerMetrics 返回全局调度 metrics，用于 admin endpoint。
func SchedulerMetrics() *SchedulerWeightedMetrics { return schedulerMetrics }

// recordSelected 累加选中账号计数
func (m *SchedulerWeightedMetrics) recordSelected(accountID int64) {
	m.mu.Lock()
	m.selectedByAcct[accountID]++
	m.mu.Unlock()
}

// SnapshotSelectedByAccount 返回选中账号的命中分布快照（admin endpoint 使用）。
func (m *SchedulerWeightedMetrics) SnapshotSelectedByAccount() map[int64]int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[int64]int64, len(m.selectedByAcct))
	for k, v := range m.selectedByAcct {
		out[k] = v
	}
	return out
}

// SchedulerMetricsSnapshot 一份只读快照。
type SchedulerMetricsSnapshot struct {
	WeightedRequests          int64           `json:"weighted_requests"`
	LegacyRequests            int64           `json:"legacy_requests"`
	WeightedSelected          int64           `json:"weighted_selected"`
	WeightedFallthroughLegacy int64           `json:"weighted_fallthrough_legacy"`
	CandidatesEvaluated       int64           `json:"candidates_evaluated"`
	NoHealthyAccount          int64           `json:"no_healthy_account"`
	SelectedByAccount         map[int64]int64 `json:"selected_by_account"`
}

// Snapshot 返回当前 metrics 快照。
func (m *SchedulerWeightedMetrics) Snapshot() SchedulerMetricsSnapshot {
	return SchedulerMetricsSnapshot{
		WeightedRequests:          m.WeightedRequests.Load(),
		LegacyRequests:            m.LegacyRequests.Load(),
		WeightedSelected:          m.WeightedSelected.Load(),
		WeightedFallthroughLegacy: m.WeightedFallthroughLegacy.Load(),
		CandidatesEvaluated:       m.CandidatesEvaluated.Load(),
		NoHealthyAccount:          m.NoHealthyAccount.Load(),
		SelectedByAccount:         m.SnapshotSelectedByAccount(),
	}
}

// weightedCandidate 单个加权打分候选。
type weightedCandidate struct {
	account  *Account
	loadInfo *AccountLoadInfo
	snapshot HealthSnapshot

	// 已归一化到 [0,1]，1=好
	priorityFactor float64
	loadFactor     float64
	ttftFactor     float64
	otpsFactor     float64
	errFactor      float64

	score float64
}

// tryWeightedAnthropicSelection 在 weighted 算法下尝试一次完整的候选池选择。
//
// 返回:
//   - selection != nil, decided=true：成功返回选择结果（含抢槽 + sticky binding）
//   - selection==nil,  decided=true：明确失败（无候选/抢槽全失败但 weighted 已决定不再回退）
//   - decided=false：weighted 流程未给出明确结果（如所有候选 score 异常），调用方应回退到 legacy
//
// 当前实现：成功一律 decided=true；候选全部抢槽失败时 decided=false（fallthrough 到 legacy 兜底）。
func (s *GatewayService) tryWeightedAnthropicSelection(
	ctx context.Context,
	available []accountWithLoad,
	groupID *int64,
	sessionHash string,
	requestedModel string,
	preferOAuth bool,
) (*AccountSelectionResult, bool) {
	if s == nil || s.healthCache == nil || len(available) == 0 {
		return nil, false
	}
	schedulerMetrics.WeightedRequests.Add(1)

	weights := s.resolvedScoreWeights()
	thresholds := s.resolvedScoreThresholds()
	healthCfg := s.resolvedSchedulingHealth()
	topK := s.resolvedSchedulingTopK()
	debug := s.schedulingConfig().Debug

	// 1. 构造候选并算分
	pool := make([]weightedCandidate, 0, len(available))
	priorityMin, priorityMax := available[0].account.Priority, available[0].account.Priority
	for _, acc := range available[1:] {
		if acc.account.Priority < priorityMin {
			priorityMin = acc.account.Priority
		}
		if acc.account.Priority > priorityMax {
			priorityMax = acc.account.Priority
		}
	}
	for i := range available {
		acc := available[i].account
		loadInfo := available[i].loadInfo
		var snap HealthSnapshot
		if s.healthCache != nil {
			snap = s.healthCache.Snapshot(acc.ID)
		}
		c := weightedCandidate{
			account:  acc,
			loadInfo: loadInfo,
			snapshot: snap,
		}
		c.priorityFactor = computePriorityFactor(acc.Priority, priorityMin, priorityMax)
		c.loadFactor = computeLoadFactor(loadInfo, thresholds)
		// 样本数门槛：< MinSamples 时性能因子取中性值（避免新账号被误判）
		if healthCfg.MinSamples > 0 && snap.ReqCount < healthCfg.MinSamples {
			c.ttftFactor = 0.5
			c.otpsFactor = 0.5
			c.errFactor = 0.7
		} else {
			c.ttftFactor = computeTTFTFactor(snap, thresholds)
			c.otpsFactor = computeOTPSFactor(snap, thresholds)
			c.errFactor = clamp01(1.0 - snap.ErrRate())
		}
		c.score = weights.ErrRate*c.errFactor +
			weights.TTFT*c.ttftFactor +
			weights.OTPS*c.otpsFactor +
			weights.Priority*c.priorityFactor +
			weights.Load*c.loadFactor
		pool = append(pool, c)
	}

	schedulerMetrics.CandidatesEvaluated.Add(int64(len(pool)))

	// 2. Top-K 选取
	ranked := selectTopKWeighted(pool, topK)
	if len(ranked) == 0 {
		schedulerMetrics.NoHealthyAccount.Add(1)
		logger.LegacyPrintf("service.gateway",
			"[NoHealthyAccount] group=%v session=%s candidates=%d (no candidate ranked)",
			derefGroupID(groupID), shortSessionHash(sessionHash), len(pool))
		return nil, false
	}

	// 3. 加权随机生成尝试顺序（同会话稳定命中、无会话锚点时混入时间熵）
	seed := deriveAnthropicSelectionSeed(sessionHash, requestedModel, groupID)
	order := buildWeightedSelectionOrder(ranked, seed, preferOAuth)

	if debug.LogDecisions || groupContainedInLogList(groupID, debug.LogGroups) {
		logger.LegacyPrintf("service.gateway",
			"[SchedulerWeighted] group=%v session=%s candidates=%d topK=%d (top1 acc=%d score=%.3f)",
			derefGroupID(groupID), shortSessionHash(sessionHash),
			len(pool), len(ranked),
			ranked[0].account.ID, ranked[0].score)
	}

	// 候选明细日志（仅在 debug.LogScoreDetails 或 group 白名单内时输出）
	if debug.LogScoreDetails || groupContainedInLogList(groupID, debug.LogGroups) {
		for _, c := range ranked {
			logger.LegacyPrintf("service.gateway",
				"[SchedulerCandidate] group=%v acc=%d priority=%d score=%.3f f_err=%.2f f_ttft=%.2f f_otps=%.2f f_load=%.2f f_priority=%.2f raw_err_rate=%.3f raw_ttft=%.0f raw_otps=%.1f raw_load=%d",
				derefGroupID(groupID), c.account.ID, c.account.Priority, c.score,
				c.errFactor, c.ttftFactor, c.otpsFactor, c.loadFactor, c.priorityFactor,
				c.snapshot.ErrRate(), c.snapshot.TTFTAvg(), c.snapshot.OTPSAvg(), c.loadInfo.LoadRate)
		}
	}

	// 4. 依次尝试抢槽，直到成功或全失败
	for _, c := range order {
		result, err := s.tryAcquireAccountSlot(ctx, c.account.ID, c.account.Concurrency)
		if err != nil || !result.Acquired {
			continue
		}
		if !s.checkAndRegisterSession(ctx, c.account, sessionHash) {
			result.ReleaseFunc()
			continue
		}
		if sessionHash != "" && s.cache != nil {
			_ = s.cache.SetSessionAccountID(ctx, derefGroupID(groupID), sessionHash, c.account.ID, stickySessionTTL)
		}
		selection, sErr := s.newSelectionResult(ctx, c.account, true, result.ReleaseFunc, nil)
		if sErr != nil {
			result.ReleaseFunc()
			continue
		}
		schedulerMetrics.WeightedSelected.Add(1)
		schedulerMetrics.recordSelected(c.account.ID)
		s.logSchedulerSelected("weighted", c.account, sessionHash, groupID, true)
		return selection, true
	}

	// 候选全部抢槽失败：返回 decided=false，让上层 legacy 兜底循环或 Layer 3 排队接管
	schedulerMetrics.WeightedFallthroughLegacy.Add(1)
	return nil, false
}

// computePriorityFactor 价格归一化（候选池相对值，越便宜越高）。
// priority 数字越小=越便宜=越优。返回 [0,1]，1=池内最便宜。
func computePriorityFactor(p, minP, maxP int) float64 {
	if maxP <= minP {
		return 1.0
	}
	return clamp01(1.0 - float64(p-minP)/float64(maxP-minP))
}

// computeLoadFactor 非线性 loadFactor：
//   - LoadRate < threshold（默认 70）→ 1.0（不参与排序）
//   - threshold ~ 100 之间线性下降到 0
//
// 这样设计的原因：在并发数严格管控的系统里（LoadRate < 100% = 健康），
// 把 load 当连续"越低越好"的软排序信号意义不大，但接近满载时仍需推流量到余量大的账号。
func computeLoadFactor(loadInfo *AccountLoadInfo, t config.ScoreThresholdsConfig) float64 {
	if loadInfo == nil {
		return 1.0
	}
	rate := float64(loadInfo.LoadRate)
	threshold := t.LoadThresholdPct
	if threshold <= 0 {
		threshold = 70
	}
	if rate < threshold {
		return 1.0
	}
	if rate >= 100 {
		return 0.0
	}
	return (100.0 - rate) / (100.0 - threshold)
}

// computeTTFTFactor TTFT 绝对阈值归一化：≤ best=1，≥ worst=0，中间线性。
// 无样本（HasTTFT=false）时返回 0.5（中性，不偏袒）。
func computeTTFTFactor(s HealthSnapshot, t config.ScoreThresholdsConfig) float64 {
	if !s.HasTTFT() {
		return 0.5
	}
	best := float64(t.TTFTBestMs)
	worst := float64(t.TTFTWorstMs)
	if best <= 0 {
		best = 1500
	}
	if worst <= best {
		worst = best + 1
	}
	avg := s.TTFTAvg()
	if avg <= best {
		return 1.0
	}
	if avg >= worst {
		return 0.0
	}
	return (worst - avg) / (worst - best)
}

// computeOTPSFactor OTPS 绝对阈值归一化：≥ best=1、≤ worst=0、中间线性。
// 无样本时返回 0.5。
func computeOTPSFactor(s HealthSnapshot, t config.ScoreThresholdsConfig) float64 {
	if !s.HasOTPS() {
		return 0.5
	}
	best := t.OTPSBest
	worst := t.OTPSWorst
	if best <= 0 {
		best = 80
	}
	if worst < 0 {
		worst = 0
	}
	if best <= worst {
		best = worst + 1
	}
	avg := s.OTPSAvg()
	if avg >= best {
		return 1.0
	}
	if avg <= worst {
		return 0.0
	}
	return (avg - worst) / (best - worst)
}

// selectTopKWeighted 选出 score 最高的 K 个候选（K 超过 len 时返回全部）。
// 使用部分排序（O(n log k) 小堆）以应对大候选池。
func selectTopKWeighted(pool []weightedCandidate, k int) []weightedCandidate {
	if len(pool) == 0 || k <= 0 {
		return nil
	}
	if k >= len(pool) {
		out := append([]weightedCandidate(nil), pool...)
		sort.Slice(out, func(i, j int) bool {
			return weightedCandidateBetter(out[i], out[j])
		})
		return out
	}
	// 简化：直接全排序后裁剪。N 通常 < 100，复杂度可接受。
	out := append([]weightedCandidate(nil), pool...)
	sort.Slice(out, func(i, j int) bool {
		return weightedCandidateBetter(out[i], out[j])
	})
	return out[:k]
}

// weightedCandidateBetter 比较两个候选的优先级（用于 Top-K 排序的稳定 tiebreaker）。
func weightedCandidateBetter(a, b weightedCandidate) bool {
	if a.score != b.score {
		return a.score > b.score
	}
	if a.account.Priority != b.account.Priority {
		return a.account.Priority < b.account.Priority
	}
	if a.loadInfo != nil && b.loadInfo != nil && a.loadInfo.LoadRate != b.loadInfo.LoadRate {
		return a.loadInfo.LoadRate < b.loadInfo.LoadRate
	}
	return a.account.ID < b.account.ID
}

// deriveAnthropicSelectionSeed 根据会话锚点生成稳定/随机 RNG 种子。
//
// 策略：
//   - sessionHash 非空 → 同会话每次返回相同种子（避免抖动）
//   - sessionHash 空（新会话/无会话）→ 混入时间熵，避免长期固定命中同一账号
func deriveAnthropicSelectionSeed(sessionHash, requestedModel string, groupID *int64) uint64 {
	hasher := fnv.New64a()
	write := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		_, _ = hasher.Write([]byte(s))
		_, _ = hasher.Write([]byte{0})
	}
	write(sessionHash)
	write(requestedModel)
	if groupID != nil {
		_, _ = hasher.Write([]byte(strconv.FormatInt(*groupID, 10)))
	}
	seed := hasher.Sum64()
	if strings.TrimSpace(sessionHash) == "" {
		seed ^= uint64(time.Now().UnixNano())
	}
	if seed == 0 {
		seed = uint64(time.Now().UnixNano()) ^ 0x9e3779b97f4a7c15
	}
	return seed
}

// xorshift64 简单的快速 RNG，无外部依赖、无加锁。
type xorshift64 struct {
	state uint64
}

func (r *xorshift64) next() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 2685821657736338717
}

func (r *xorshift64) float() float64 {
	return float64(r.next()>>11) / (1 << 53)
}

// buildWeightedSelectionOrder 将 Top-K 候选按 (score-minScore)+0.5 加权随机重排，
// 同分时拉开 ~2-5 倍命中概率差距，既不让最高分独占，也不让倒数被频繁选中。
//
// preferOAuth 在两个候选权重相等时，让 OAuth 类型优先。
func buildWeightedSelectionOrder(top []weightedCandidate, seed uint64, preferOAuth bool) []weightedCandidate {
	if len(top) <= 1 {
		out := append([]weightedCandidate(nil), top...)
		return out
	}
	rng := &xorshift64{state: seed}
	if rng.state == 0 {
		rng.state = 0x9e3779b97f4a7c15
	}

	pool := append([]weightedCandidate(nil), top...)
	// 计算 (score - minScore) + 0.5 作为权重，base 0.5 给低分留兜底概率
	minScore := pool[0].score
	for _, c := range pool[1:] {
		if c.score < minScore {
			minScore = c.score
		}
	}
	weights := make([]float64, len(pool))
	for i, c := range pool {
		w := (c.score - minScore) + 0.5
		if math.IsNaN(w) || math.IsInf(w, 0) || w <= 0 {
			w = 0.5
		}
		// preferOAuth：OAuth 账号权重 +5%
		if preferOAuth && c.account.Type == AccountTypeOAuth {
			w *= 1.05
		}
		weights[i] = w
	}

	// 加权随机抽取（不放回），生成尝试顺序
	out := make([]weightedCandidate, 0, len(pool))
	for len(pool) > 0 {
		var sum float64
		for _, w := range weights {
			sum += w
		}
		r := rng.float() * sum
		idx := len(pool) - 1
		for i, w := range weights {
			r -= w
			if r <= 0 {
				idx = i
				break
			}
		}
		out = append(out, pool[idx])
		// 剔除已选
		pool = append(pool[:idx], pool[idx+1:]...)
		weights = append(weights[:idx], weights[idx+1:]...)
	}
	return out
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
