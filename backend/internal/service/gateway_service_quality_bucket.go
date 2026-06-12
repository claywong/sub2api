// gateway_service_quality_bucket.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：基于 TTFT / OTPS / CacheHit 分桶的调度过滤函数。
//
// 新增方法：
//   - ttftBucket(ttftMs float64, hasSample bool) int
//   - otpsBucket(otps float64, hasSample bool) int
//   - cacheHitBucket(rate float64, hasSample bool) int
//   - filterByMinTTFTBucket(accounts, cache, model) []accountWithLoad
//   - filterByMinOTPSBucket(accounts, cache, model) []accountWithLoad
//   - filterByMinCacheHitBucket(accounts, cache, model) []accountWithLoad
//
// 排序链：Priority → TTFT bucket → OTPS bucket → CacheHit bucket → LoadRate → LRU
//
// 与 upstream 合并策略：
//   - 本文件全部为新增，不修改任何 upstream 函数。
//   - gateway_service.go 只需在漏斗处追加调用（最小 inline）。
// =============================================================================
package service

// TTFTBucket / OTPSBucket / CacheHitBucket 供 admin handler 展示分桶编号。
var TTFTBucket = ttftBucket
var OTPSBucket = otpsBucket
var CacheHitBucket = cacheHitBucket

// qualityBucketCache 返回用于质量分桶的模型质量缓存。
// account_health 未启用时返回 nil，使三个 filter 函数退化为透传，
// 完全保留 Priority → LoadRate → LRU 的原始行为。
func (s *GatewayService) qualityBucketCache() *AccountModelQualityCache {
	if !s.isAccountHealthEnabled() {
		return nil
	}
	return s.modelQualityCache
}

// ttftBucket 将 TTFT（ms）转换为分桶编号，编号越小越优先。
//
// 分桶规则：
//
//	< 4000ms        → 0（最优）
//	[4000, 7000ms)  → 1
//	[7000, 10000ms) → 2
//	以此类推，每 3000ms 一组
//
// 无样本时返回 0（冷启动保护）。
func ttftBucket(ttftMs float64, hasSample bool) int {
	if !hasSample || ttftMs <= 0 {
		return 0
	}
	if ttftMs < 4000 {
		return 0
	}
	return int((ttftMs-4000)/3000) + 1
}

// otpsBucket 将 OTPS（tokens/s）转换为分桶编号，编号越小越优先（越快越好）。
//
// 分桶规则：
//
//	>= 60           → 0（最优）
//	[40, 60)        → 1
//	[20, 40)        → 2
//	< 20            → 3（最差）
//
// 无样本时返回 0（冷启动保护）。
func otpsBucket(otps float64, hasSample bool) int {
	if !hasSample {
		return 0
	}
	switch {
	case otps >= 60:
		return 0
	case otps >= 40:
		return 1
	case otps >= 20:
		return 2
	default:
		return 3
	}
}

// cacheHitBucket 将缓存命中率（0~1）转换为分桶编号，编号越小越优先（命中率越高越好）。
//
// 分桶规则：
//
//	>= 0.80         → 0（最优）
//	[0.60, 0.80)    → 1
//	[0.40, 0.60)    → 2
//	[0.20, 0.40)    → 3
//	< 0.20          → 4（最差）
//
// 无样本时返回 0（冷启动保护）。
func cacheHitBucket(rate float64, hasSample bool) int {
	if !hasSample {
		return 0
	}
	switch {
	case rate >= 0.80:
		return 0
	case rate >= 0.60:
		return 1
	case rate >= 0.40:
		return 2
	case rate >= 0.20:
		return 3
	default:
		return 4
	}
}

// filterByMinTTFTBucket 从候选账号中只保留 TTFT 分桶编号最小（最优）的账号。
//
// 若 cache 为 nil（health 未启用），所有账号分桶均为 0，过滤无效，直接返回原切片。
func filterByMinTTFTBucket(accounts []accountWithLoad, cache *AccountModelQualityCache, model string) []accountWithLoad {
	if len(accounts) == 0 || cache == nil {
		return accounts
	}

	minBucket := int(^uint(0) >> 1) // MaxInt
	snaps := make([]HealthSnapshot, len(accounts))
	for i, acc := range accounts {
		snaps[i] = cache.Snapshot(acc.account.ID, model)
		if b := ttftBucket(snaps[i].TTFTAvg(), snaps[i].HasTTFT()); b < minBucket {
			minBucket = b
		}
	}

	result := make([]accountWithLoad, 0, len(accounts))
	for i, acc := range accounts {
		if ttftBucket(snaps[i].TTFTAvg(), snaps[i].HasTTFT()) == minBucket {
			result = append(result, acc)
		}
	}
	return result
}

// filterByMinOTPSBucket 从候选账号中只保留 OTPS 分桶编号最小（最优）的账号。
//
// 若 cache 为 nil（health 未启用），所有账号分桶均为 0，过滤无效，直接返回原切片。
func filterByMinOTPSBucket(accounts []accountWithLoad, cache *AccountModelQualityCache, model string) []accountWithLoad {
	if len(accounts) == 0 || cache == nil {
		return accounts
	}

	minBucket := int(^uint(0) >> 1) // MaxInt
	snaps := make([]HealthSnapshot, len(accounts))
	for i, acc := range accounts {
		snaps[i] = cache.Snapshot(acc.account.ID, model)
		if b := otpsBucket(snaps[i].OTPSAvg(), snaps[i].HasOTPS()); b < minBucket {
			minBucket = b
		}
	}

	result := make([]accountWithLoad, 0, len(accounts))
	for i, acc := range accounts {
		if otpsBucket(snaps[i].OTPSAvg(), snaps[i].HasOTPS()) == minBucket {
			result = append(result, acc)
		}
	}
	return result
}

// filterByMinCacheHitBucket 从候选账号中只保留缓存命中率分桶编号最小（命中率最高）的账号。
//
// 若 cache 为 nil（health 未启用），所有账号分桶均为 0，过滤无效，直接返回原切片。
func filterByMinCacheHitBucket(accounts []accountWithLoad, cache *AccountModelQualityCache, model string) []accountWithLoad {
	if len(accounts) == 0 || cache == nil {
		return accounts
	}

	minBucket := int(^uint(0) >> 1) // MaxInt
	snaps := make([]HealthSnapshot, len(accounts))
	for i, acc := range accounts {
		snaps[i] = cache.Snapshot(acc.account.ID, model)
		if b := cacheHitBucket(snaps[i].CacheHitRateAvg(), snaps[i].HasCacheHit()); b < minBucket {
			minBucket = b
		}
	}

	result := make([]accountWithLoad, 0, len(accounts))
	for i, acc := range accounts {
		if cacheHitBucket(snaps[i].CacheHitRateAvg(), snaps[i].HasCacheHit()) == minBucket {
			result = append(result, acc)
		}
	}
	return result
}
