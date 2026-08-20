// prompt_config_dlp.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 检测的配置结构与校验。
//
// 设计要点：
//   - 独立的 DLPConfig 结构，通过 storageConfig / ActiveConfig 各追加一个字段
//     引用，upstream 已有字段零改动。
//   - 全部字段零值可用：upstream 的配置 JSON 里没有 dlp 字段时，DLP 默认关闭，
//     行为与改动前完全一致。
//   - DLP 有自己的 Endpoints 与 Scanners，与 qwen3guard 的同名配置互不干扰。
//     这样换 DLP 确认模型不会影响内容安全审计，反之亦然。
//
// 与 upstream 合并策略：
//   - 纯新增文件 + 两处单字段追加，merge 时冲突面极小。
//
// =============================================================================
package securityaudit

import (
	"sort"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// DLP 配置的取值边界。
const (
	MinDLPConfirmTimeoutMS = 500
	MaxDLPConfirmTimeoutMS = 30000
	MaxDLPCacheTTLHours    = 720 // 30 天
)

// DefaultDLPConfirmModel 是二次确认的默认模型。
// 选它的原因：成本低、延迟 1.4~2.9s 可接受、实测降误报判断 9/9 全对。
const DefaultDLPConfirmModel = "gpt-5.6-luna"

// DLPConfig 是 DLP 检测的持久化配置。
//
// json tag 全部带 omitempty，保证未配置时序列化结果里不出现 dlp 字段，
// 与 upstream 的配置文件保持字节级兼容。
type DLPConfig struct {
	// Enabled 是 DLP 检测的独立开关。关闭时正则完全不跑。
	Enabled bool `json:"enabled,omitempty"`
	// Scanners 指定启用哪些 DLP 检测器。为空表示三类全启用。
	Scanners []string `json:"scanners,omitempty"`
	// ConfirmEnabled 控制是否做 LLM 二次确认。
	// 关闭时正则命中直接按严重度处置，误报率会显著上升。
	ConfirmEnabled bool `json:"confirm_enabled,omitempty"`
	// Endpoints 是二次确认用的模型节点。与 qwen3guard 的节点池分开配置。
	Endpoints []StorageEndpoint `json:"endpoints,omitempty"`
	// ConfirmTimeoutMS 是单次确认的超时。超时按 fail-open 放行。
	ConfirmTimeoutMS int `json:"confirm_timeout_ms,omitempty"`
	// CacheEnabled 控制是否缓存确认结论。
	CacheEnabled bool `json:"cache_enabled,omitempty"`
	// CacheSensitiveTTLHours / CacheBenignTTLHours 分别是"判为敏感"与
	// "判为误报"结论的缓存时长。0 表示用默认值。
	CacheSensitiveTTLHours int `json:"cache_sensitive_ttl_hours,omitempty"`
	CacheBenignTTLHours    int `json:"cache_benign_ttl_hours,omitempty"`
	// BlockOnHighSeverity 控制 high/critical 命中是否拦截请求。
	// medium 命中恒为仅审计，不受此开关影响。哪些规则算 high 可由管理员配置，
	// 见 RuleOverrides。
	BlockOnHighSeverity bool `json:"block_on_high_severity,omitempty"`
	// RuleOverrides 是单条规则的严重度与启停覆盖，只存与内置默认值的偏差。
	// 语义与归一化见 prompt_dlp_rule_overrides.go。
	RuleOverrides DLPRuleOverrides `json:"rule_overrides,omitempty"`
	// AllGroups / GroupIDs 是 DLP 自己的生效范围，与 qwen3guard 的分组设置独立。
	//
	// 必须独立：DLP 与内容安全是两类检测，管理员完全可能只想对部分分组查敏感信息，
	// 却对全部分组做内容安全（或反之）。共用一份分组会让两者互相牵连。
	//
	// 语义与 upstream 的 AllGroups/GroupIDs 一致。注意零值 false 表示"仅指定分组"
	// 且列表为空，即不对任何分组生效——这是 Enabled=false 时的安全默认；一旦
	// Enabled=true，ValidateDLPConfig 会要求必须给出范围。
	AllGroups bool    `json:"all_groups,omitempty"`
	GroupIDs  []int64 `json:"group_ids,omitempty"`
}

// ActiveDLPConfig 是运行时视图，token 已解密、endpoint 已归一化。
type ActiveDLPConfig struct {
	Enabled             bool
	Scanners            []string
	ConfirmEnabled      bool
	Endpoints           []ActiveEndpoint
	ConfirmTimeout      time.Duration
	CacheEnabled        bool
	CacheSensitiveTTL   time.Duration
	CacheBenignTTL      time.Duration
	BlockOnHighSeverity bool
	AllGroups           bool
	GroupIDs            []int64
	// RuleOverrides 是单条规则的严重度与启停覆盖，见 prompt_dlp_rule_overrides.go。
	RuleOverrides DLPRuleOverrides
}

// IncludesGroup 判断某分组是否在 DLP 的生效范围内。
//
// 语义与 upstream 的 ActiveConfig.IncludesGroup 完全一致（含 GroupIDs 已排序的
// 前提，二分查找），但读的是 DLP 自己的分组字段。
func (cfg ActiveDLPConfig) IncludesGroup(groupID *int64) bool {
	if cfg.AllGroups {
		return true
	}
	if groupID == nil {
		return false
	}
	index := sort.Search(len(cfg.GroupIDs), func(i int) bool { return cfg.GroupIDs[i] >= *groupID })
	return index < len(cfg.GroupIDs) && cfg.GroupIDs[index] == *groupID
}

// Clone 深拷贝运行时视图，避免多个配置 snapshot 共享 slice 底层数组。
//
// ActiveDLPConfig 是值类型，但内部两个 slice 在浅拷贝后仍共享底层数组。
// ConfigManager 的 snapshot 会被并发读取，共享会让调用方的修改污染 snapshot。
func (cfg ActiveDLPConfig) Clone() ActiveDLPConfig {
	cfg.Scanners = append([]string(nil), cfg.Scanners...)
	cfg.GroupIDs = append([]int64(nil), cfg.GroupIDs...)
	cfg.Endpoints = append([]ActiveEndpoint(nil), cfg.Endpoints...)
	return cfg
}

// Clone 深拷贝持久化配置。storageConfig 里 DLP 是指针，浅拷贝会让新旧 snapshot
// 共享同一个结构，buildNextStorage 原地改动就会改到已发布的旧配置。
func (cfg *DLPConfig) Clone() *DLPConfig {
	if cfg == nil {
		return nil
	}
	copied := *cfg
	copied.Scanners = append([]string(nil), cfg.Scanners...)
	copied.GroupIDs = append([]int64(nil), cfg.GroupIDs...)
	copied.Endpoints = append([]StorageEndpoint(nil), cfg.Endpoints...)
	return &copied
}

// EnabledEndpoints 返回可用于运行时的确认节点。
// 过滤规则与 upstream 的 ActiveConfig.EnabledEndpoints 保持一致：
// 未启用或 token 无法解密的节点都排除。
func (cfg ActiveDLPConfig) EnabledEndpoints() []ActiveEndpoint {
	result := make([]ActiveEndpoint, 0, len(cfg.Endpoints))
	for _, endpoint := range cfg.Endpoints {
		if !endpoint.Enabled || endpoint.TokenInvalid {
			continue
		}
		if strings.TrimSpace(endpoint.BaseURL) == "" {
			continue
		}
		result = append(result, endpoint)
	}
	return result
}

// InvalidTokenEndpointIDs 列出 token 无法用当前加密密钥解密的确认节点。
// 语义与 upstream 的 ActiveConfig.InvalidTokenEndpointIDs 一致。
func (cfg ActiveDLPConfig) InvalidTokenEndpointIDs() []string {
	ids := make([]string, 0)
	for _, endpoint := range cfg.Endpoints {
		if endpoint.TokenInvalid {
			ids = append(ids, endpoint.ID)
		}
	}
	return ids
}

// ConfirmReady 判断二次确认链路是否可用。
// 不可用时 DLP 仍可只靠正则工作（误报率更高），由调用方决定是否继续。
func (cfg ActiveDLPConfig) ConfirmReady() bool {
	return cfg.ConfirmEnabled && len(cfg.EnabledEndpoints()) > 0
}

// EffectiveScanners 返回实际启用的 DLP 检测器 ID。
func (cfg ActiveDLPConfig) EffectiveScanners() []string {
	if len(cfg.Scanners) == 0 {
		return DLPScannerIDs()
	}
	result := make([]string, 0, len(cfg.Scanners))
	for _, id := range cfg.Scanners {
		if IsDLPScanner(id) {
			result = append(result, id)
		}
	}
	return result
}

// ToActiveDLPConfig 把持久化配置转成运行时视图。
//
// decryptToken 用于解密 endpoint token，与 upstream 的 endpoint 处理保持一致；
// 传 nil 时视为无需解密（token 原样使用），便于单测。
func (cfg DLPConfig) ToActiveDLPConfig(decryptToken func(string) (string, error)) ActiveDLPConfig {
	active := ActiveDLPConfig{
		Enabled:             cfg.Enabled,
		Scanners:            append([]string(nil), cfg.Scanners...),
		ConfirmEnabled:      cfg.ConfirmEnabled,
		CacheEnabled:        cfg.CacheEnabled,
		BlockOnHighSeverity: cfg.BlockOnHighSeverity,
		ConfirmTimeout:      time.Duration(cfg.ConfirmTimeoutMS) * time.Millisecond,
		CacheSensitiveTTL:   time.Duration(cfg.CacheSensitiveTTLHours) * time.Hour,
		CacheBenignTTL:      time.Duration(cfg.CacheBenignTTLHours) * time.Hour,
		AllGroups:           cfg.AllGroups,
		// IncludesGroup 用二分查找，这里必须保证有序。持久化层已排序去重，
		// 这里再排一次是为了容忍手工改过的配置行。
		GroupIDs: sortedUniqueGroupIDs(cfg.GroupIDs),
		// 归一化一次：容忍手工改过的配置行里出现已下线的 rule ID 或非法严重度。
		RuleOverrides: normalizeDLPRuleOverrides(cfg.RuleOverrides),
	}
	// 向后兼容：分组字段是后加的，早先存下的 DLP 配置里没有 all_groups，
	// 反序列化后是 false + 空列表，照新语义会变成"不对任何分组生效"，
	// DLP 静默停摆。这类组合已被 ValidateDLPConfig 拒绝，能出现只可能是旧配置，
	// 因此一律按"全部分组"解释，保住升级前的既有行为。
	if active.Enabled && !active.AllGroups && len(active.GroupIDs) == 0 {
		active.AllGroups = true
	}
	if active.ConfirmTimeout <= 0 {
		active.ConfirmTimeout = DefaultTimeoutMS * time.Millisecond
	}
	for _, stored := range cfg.Endpoints {
		endpoint := ActiveEndpoint{
			ID: stored.ID, Name: stored.Name, Protocol: stored.Protocol,
			BaseURL: stored.BaseURL, Model: stored.Model,
			TimeoutMS: stored.TimeoutMS, InputLimit: stored.InputLimit,
			Enabled: stored.Enabled,
		}
		if endpoint.Model == "" {
			endpoint.Model = DefaultDLPConfirmModel
		}
		if endpoint.TimeoutMS <= 0 {
			endpoint.TimeoutMS = int(active.ConfirmTimeout / time.Millisecond)
		}
		endpoint.Token, endpoint.TokenInvalid = resolveDLPEndpointToken(stored, decryptToken)
		active.Endpoints = append(active.Endpoints, endpoint)
	}
	return active
}

// resolveDLPEndpointToken 解密 endpoint token。
// 解密失败时标记 TokenInvalid，让节点在管理端仍可见但不参与运行时调用
// （与 upstream 处理加密密钥变更的策略一致）。
func resolveDLPEndpointToken(
	stored StorageEndpoint, decryptToken func(string) (string, error),
) (string, bool) {
	cipher := strings.TrimSpace(stored.TokenCiphertext)
	if cipher == "" {
		return "", false
	}
	if decryptToken == nil {
		return cipher, false
	}
	plain, err := decryptToken(cipher)
	if err != nil {
		return "", true
	}
	return plain, false
}

// activeDLPFromStorage 把持久化的 DLP 配置转成运行时视图。
//
// stored 为 nil（upstream 配置里没有 dlp 字段）时返回零值，DLP 保持关闭，
// 行为与改动前完全一致。
func activeDLPFromStorage(stored *DLPConfig, encryptor SecretEncryptor) ActiveDLPConfig {
	if stored == nil {
		return ActiveDLPConfig{}
	}
	var decrypt func(string) (string, error)
	if encryptor != nil {
		decrypt = encryptor.Decrypt
	}
	return stored.ToActiveDLPConfig(decrypt)
}

// sortedUniqueGroupIDs 排序去重分组 ID，并丢掉非法的非正数 ID。
func sortedUniqueGroupIDs(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(ids))
	result := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

// ValidateDLPConfig 校验管理员提交的 DLP 配置。
func ValidateDLPConfig(cfg DLPConfig) error {
	if !cfg.Enabled {
		// 关闭状态下不校验细节，允许保存半成品配置。
		return nil
	}
	for _, scanner := range cfg.Scanners {
		if !IsDLPScanner(scanner) {
			return infraerrors.BadRequest("dlp_invalid_scanner", "DLP 检测器无效")
		}
	}
	if err := validateDLPRuleOverrides(cfg.RuleOverrides); err != nil {
		return err
	}
	// 「启用了 DLP 但每条规则都被关掉」与「启用却没选任何分组」是同一类问题：
	// 配置看着是开的，实际静默不工作。必须在保存时拒掉。
	if enabledDLPRuleCount(cfg.Scanners, cfg.RuleOverrides) == 0 {
		return infraerrors.BadRequest("dlp_rule_scope_required",
			"启用 DLP 检测时至少需要保留一条生效的检测规则")
	}
	// 启用却没有任何生效范围时，DLP 会静默不工作。这类"开了但没效果"的配置
	// 必须在保存时就拒掉，否则管理员只能靠观察日志才发现。
	if !cfg.AllGroups && len(sortedUniqueGroupIDs(cfg.GroupIDs)) == 0 {
		return infraerrors.BadRequest("dlp_group_scope_required",
			"启用 DLP 检测时需选择全部分组或至少一个指定分组")
	}
	if cfg.ConfirmTimeoutMS != 0 &&
		(cfg.ConfirmTimeoutMS < MinDLPConfirmTimeoutMS || cfg.ConfirmTimeoutMS > MaxDLPConfirmTimeoutMS) {
		return infraerrors.BadRequest("dlp_invalid_confirm_timeout", "DLP 二次确认超时超出允许范围")
	}
	if err := validateDLPCacheTTL(cfg); err != nil {
		return err
	}
	if cfg.ConfirmEnabled && countEnabledDLPEndpoints(cfg.Endpoints) == 0 {
		return infraerrors.BadRequest("dlp_confirm_endpoint_required",
			"启用 DLP 二次确认前至少需要启用一个确认节点")
	}
	for _, endpoint := range cfg.Endpoints {
		if endpoint.TimeoutMS != 0 &&
			(endpoint.TimeoutMS < MinTimeoutMS || endpoint.TimeoutMS > MaxTimeoutMS) {
			return infraerrors.BadRequest("dlp_invalid_endpoint_timeout", "DLP 确认节点超时超出允许范围")
		}
		if endpoint.Enabled && strings.TrimSpace(endpoint.BaseURL) == "" {
			return infraerrors.BadRequest("dlp_endpoint_base_url_required", "DLP 确认节点地址不能为空")
		}
		if strings.TrimSpace(endpoint.BaseURL) != "" {
			if _, err := NormalizeBaseURL(endpoint.BaseURL); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateDLPCacheTTL 校验缓存 TTL 取值。
func validateDLPCacheTTL(cfg DLPConfig) error {
	if cfg.CacheSensitiveTTLHours < 0 || cfg.CacheSensitiveTTLHours > MaxDLPCacheTTLHours {
		return infraerrors.BadRequest("dlp_invalid_cache_ttl", "DLP 确认缓存时长超出允许范围")
	}
	if cfg.CacheBenignTTLHours < 0 || cfg.CacheBenignTTLHours > MaxDLPCacheTTLHours {
		return infraerrors.BadRequest("dlp_invalid_cache_ttl", "DLP 确认缓存时长超出允许范围")
	}
	return nil
}

// countEnabledDLPEndpoints 统计启用的确认节点数量。
func countEnabledDLPEndpoints(endpoints []StorageEndpoint) int {
	count := 0
	for _, endpoint := range endpoints {
		if endpoint.Enabled && strings.TrimSpace(endpoint.BaseURL) != "" {
			count++
		}
	}
	return count
}
