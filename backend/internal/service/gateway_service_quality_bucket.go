// gateway_service_quality_bucket.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：基于 TTFT / OTPS 分桶的调度过滤函数。
//
// 新增方法：
//   - ttftBucket(ttftMs float64, hasSample bool) int
//   - otpsBucket(otps float64, hasSample bool) int
//   - filterByMinTTFTBucket(accounts []accountWithLoad, cache *AccountTestHealthCache) []accountWithLoad
//   - filterByMinOTPSBucket(accounts []accountWithLoad, cache *AccountTestHealthCache) []accountWithLoad
//
// 排序链：Priority → TTFT bucket → OTPS bucket → LoadRate → LRU
//
// 与 upstream 合并策略：
//   - 本文件全部为新增，不修改任何 upstream 函数。
//   - gateway_service.go 只需在三层漏斗处追加 2 行调用（最小 inline）。
// =============================================================================
package service

// qualityBucketCache 返回用于质量分桶的健康缓存。
// account_health 未启用时返回 nil，使两个 filter 函数退化为透传，
// 完全保留 Priority → LoadRate → LRU 的原始行为。
func (s *GatewayService) qualityBucketCache() *AccountTestHealthCache {
	if !s.isAccountHealthEnabled() {
		return nil
	}
	return s.healthCache
}

// ttftBucket 将 TTFT（ms）转换为分桶编号，编号越小越优先。
//
// 分桶规则：
//
//	< 3000ms        → 0（最优）
//	[3000, 5000ms)  → 1
//	[5000, 7000ms)  → 2
//	以此类推，每 2000ms 一组
//
// 无样本时返回 0（冷启动保护，与 HealthOK 的 MinSamples 策略一致）。
func ttftBucket(ttftMs float64, hasSample bool) int {
	if !hasSample || ttftMs <= 0 {
		return 0
	}
	if ttftMs < 3000 {
		return 0
	}
	return int((ttftMs-3000)/2000) + 1
}

// otpsBucket 将 OTPS（tokens/s）转换为分桶编号，编号越小越优先（越快越好）。
//
// 分桶规则：
//
//	>= 60           → 0（最优）
//	[40, 60)        → 1
//	[20, 40)        → 2
//	< 20            → 3（最差，一个桶）
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

// filterByMinTTFTBucket 从候选账号中只保留 TTFT 分桶编号最小（最优）的账号。
//
// 若 cache 为 nil（health 未启用），所有账号分桶均为 0，过滤无效，直接返回原切片。
func filterByMinTTFTBucket(accounts []accountWithLoad, cache *AccountTestHealthCache) []accountWithLoad {
	if len(accounts) == 0 || cache == nil {
		return accounts
	}

	minBucket := int(^uint(0) >> 1) // MaxInt
	snaps := make([]HealthSnapshot, len(accounts))
	for i, acc := range accounts {
		snaps[i] = cache.Snapshot(acc.account.ID)
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
func filterByMinOTPSBucket(accounts []accountWithLoad, cache *AccountTestHealthCache) []accountWithLoad {
	if len(accounts) == 0 || cache == nil {
		return accounts
	}

	minBucket := int(^uint(0) >> 1) // MaxInt
	snaps := make([]HealthSnapshot, len(accounts))
	for i, acc := range accounts {
		snaps[i] = cache.Snapshot(acc.account.ID)
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
