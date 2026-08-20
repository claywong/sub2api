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
	// medium 命中（JWT、手机号）按 detection-rules.md 恒为仅审计，不受此开关影响。
	BlockOnHighSeverity bool `json:"block_on_high_severity,omitempty"`
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
