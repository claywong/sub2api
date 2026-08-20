// prompt_dlp_cache.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 二次确认结论的缓存。
//
// 作用：同一命中片段在 TTL 内复用上次的模型判定，避免重复请求。配合正则初筛，
// 把确认请求量从"全量请求"压到"新出现的命中"。
//
// 安全约束（重要）：缓存 key 只存 规则ID + 片段的 SHA256，**绝不存敏感明文**。
// 缓存 value 只存 true/false 结论，不存模型给的 reason（reason 可能复述片段内容）。
//
// Redis 不可用时静默降级为"不命中"，即退化成每次都实调模型，不影响正确性。
//
// 与 upstream 合并策略：
//   - 纯新增文件，无 upstream 符号改动，merge 时不会冲突。
//
// =============================================================================
package securityaudit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// DLPCacheKeyPrefix 与 upstream 的 PayloadKeyPrefix 保持同一命名风格。
const DLPCacheKeyPrefix = "sub2api:prompt_audit:dlp:confirm:"

// 缓存值编码。用单字符而非 JSON，减少存储与解析开销。
const (
	dlpCacheValueSensitive = "1"
	dlpCacheValueBenign    = "0"
)

// 默认 TTL。误报（benign）的结论给更长的 TTL：同一份文档示例值会被反复提交，
// 而"确实是真实密钥"的结论保守一些，便于凭证轮换后重新判定。
const (
	DefaultDLPCacheSensitiveTTL = 6 * time.Hour
	DefaultDLPCacheBenignTTL    = 24 * time.Hour
)

// DLPConfirmCache 缓存二次确认结论。redis 为 nil 时所有操作都是安全的空操作。
//
// 刻意不在结构体里存 TTL：缓存实例在服务启动时构造一次，而 DLP 配置是可热更新的
// （管理员在后台改 TTL 后会触发配置 reload）。TTL 由 Store 的调用方按当次生效的
// 配置传入，否则后台改的 TTL 会被静默忽略。
type DLPConfirmCache struct {
	redis *redis.Client
}

// NewDLPConfirmCache 构造缓存。
func NewDLPConfirmCache(client *redis.Client) *DLPConfirmCache {
	return &DLPConfirmCache{redis: client}
}

// dlpCacheTTL 归一化 TTL，非正值回落到默认值。
func dlpCacheTTL(sensitive, benign time.Duration) (time.Duration, time.Duration) {
	if sensitive <= 0 {
		sensitive = DefaultDLPCacheSensitiveTTL
	}
	if benign <= 0 {
		benign = DefaultDLPCacheBenignTTL
	}
	return sensitive, benign
}

// newDLPCacheFor 从 payload store 借用 Redis 客户端构造确认缓存。
//
// 之所以复用 RedisPayloadStore 的客户端而不是新增一个注入参数：改
// NewPromptService 的签名会让 upstream 后续改动必然冲突（CLAUDE.md 的 inline
// 最小化原则）。payload store 已经持有同一个 Redis 客户端，直接借用即可。
//
// store 或其客户端为 nil 时返回的缓存是安全的空操作，DLP 退化为每次实调模型。
func newDLPCacheFor(store *RedisPayloadStore) *DLPConfirmCache {
	if store == nil {
		return NewDLPConfirmCache(nil)
	}
	return NewDLPConfirmCache(store.client)
}

// dlpCacheKey 由规则 ID 与片段哈希构成。
//
// 片段先做规范化（去首尾空白）再哈希，让同一值的不同书写形式命中同一缓存。
// 带上规则 ID 是因为同一段文本在不同规则下的判定可能不同。
func dlpCacheKey(ruleID, value string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return DLPCacheKeyPrefix + ruleID + ":" + hex.EncodeToString(digest[:])
}

// Lookup 批量查缓存，返回与 findings 等长的结论切片。
// 未命中的位置 Confirmed 为 false，调用方需要为这些位置实调模型。
func (c *DLPConfirmCache) Lookup(ctx context.Context, findings []DLPFinding) []DLPConfirmVerdict {
	verdicts := make([]DLPConfirmVerdict, len(findings))
	if c == nil || c.redis == nil || len(findings) == 0 {
		return verdicts
	}
	keys := make([]string, len(findings))
	for index, finding := range findings {
		keys[index] = dlpCacheKey(finding.RuleID, finding.Value)
	}
	values, err := c.redis.MGet(ctx, keys...).Result()
	if err != nil {
		// Redis 故障不应影响检测正确性，退化为全部未命中。
		return verdicts
	}
	for index, raw := range values {
		if index >= len(verdicts) {
			break
		}
		text, ok := raw.(string)
		if !ok {
			continue
		}
		switch text {
		case dlpCacheValueSensitive:
			verdicts[index] = DLPConfirmVerdict{
				Sensitive: true, Confirmed: true, Reason: "命中确认结论缓存",
			}
		case dlpCacheValueBenign:
			verdicts[index] = DLPConfirmVerdict{
				Sensitive: false, Confirmed: true, Reason: "命中确认结论缓存",
			}
		}
	}
	return verdicts
}

// Store 写入确认结论。TTL 由调用方按当次生效的配置传入，传 0 用默认值。
//
// 只写 Confirmed 为 true 的结论——降级放行不代表判定为误报，
// 把它缓存下来会让后续请求错误地跳过确认。
func (c *DLPConfirmCache) Store(
	ctx context.Context, findings []DLPFinding, verdicts []DLPConfirmVerdict,
	sensitiveTTL, benignTTL time.Duration,
) {
	if c == nil || c.redis == nil {
		return
	}
	sensitiveTTL, benignTTL = dlpCacheTTL(sensitiveTTL, benignTTL)
	pipe := c.redis.Pipeline()
	queued := 0
	for index, finding := range findings {
		if index >= len(verdicts) {
			break
		}
		verdict := verdicts[index]
		if !verdict.Confirmed {
			continue
		}
		value, ttl := dlpCacheValueBenign, benignTTL
		if verdict.Sensitive {
			value, ttl = dlpCacheValueSensitive, sensitiveTTL
		}
		pipe.Set(ctx, dlpCacheKey(finding.RuleID, finding.Value), value, ttl)
		queued++
	}
	if queued == 0 {
		return
	}
	// 写缓存失败不影响本次判定结果，忽略错误。
	_, _ = pipe.Exec(ctx)
}
