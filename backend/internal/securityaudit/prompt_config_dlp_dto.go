// prompt_config_dlp_dto.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 配置的对外 DTO 与转换。
//
// 职责：把 DLP 配置接进管理 API 的读写链路。
//   - PublicDLPConfig：GET /admin/prompt-audit/config 的响应片段。token 只回显
//     "有没有配"与状态，绝不回显明文或密文。
//   - UpdateDLPRequest：PUT 请求片段。token 的三种语义（清空 / 覆盖 / 保留原值）
//     与 upstream 的 UpdateEndpoint 完全一致。
//
// 与 upstream 合并策略：
//   - 转换逻辑全在本文件。upstream 侧只有 PublicConfig / UpdateConfigRequest
//     各加一个字段，以及 buildNextStorage / PublicFromStorage 各 1 行 hook。
//
// =============================================================================
package securityaudit

import (
	"fmt"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// PublicDLPEndpoint 是 DLP 确认节点的对外视图。
type PublicDLPEndpoint struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	BaseURL     string `json:"base_url"`
	Model       string `json:"model"`
	TimeoutMS   int    `json:"timeout_ms"`
	Enabled     bool   `json:"enabled"`
	HasToken    bool   `json:"has_token"`
	TokenStatus string `json:"token_status"`
}

// PublicDLPConfig 是 DLP 配置的对外视图。
type PublicDLPConfig struct {
	Enabled                bool                `json:"enabled"`
	Scanners               []string            `json:"scanners"`
	ConfirmEnabled         bool                `json:"confirm_enabled"`
	ConfirmTimeoutMS       int                 `json:"confirm_timeout_ms"`
	CacheEnabled           bool                `json:"cache_enabled"`
	CacheSensitiveTTLHours int                 `json:"cache_sensitive_ttl_hours"`
	CacheBenignTTLHours    int                 `json:"cache_benign_ttl_hours"`
	BlockOnHighSeverity    bool                `json:"block_on_high_severity"`
	AllGroups              bool                `json:"all_groups"`
	GroupIDs               []int64             `json:"group_ids"`
	Endpoints              []PublicDLPEndpoint `json:"endpoints"`
	// AvailableScanners 让前端不必硬编码检测器清单。
	AvailableScanners []ScannerDefinition `json:"available_scanners"`
	// Rules 是全部检测规则及其生效严重度/启停状态。
	//
	// 规则表住在后端（dlpRules），前端硬编码一份必然随版本漂移，
	// 所以整表下发，界面只负责渲染。
	Rules []DLPRuleCatalogEntry `json:"rules"`
	// AvailableSeverities 是允许管理员设置的严重度取值，供前端渲染选择器。
	AvailableSeverities []RiskLevel `json:"available_severities"`
	// BlockingSeverities 是会触发拦截的严重度（前提是 BlockOnHighSeverity 打开）。
	// 界面用它按草稿实时算「会拦 / 仅记录」，详见 BlockingDLPSeverities 的注释。
	BlockingSeverities []RiskLevel `json:"blocking_severities"`
}

// UpdateDLPEndpoint 是 DLP 确认节点的写入请求。
type UpdateDLPEndpoint struct {
	ID         string `json:"id" binding:"required"`
	Name       string `json:"name" binding:"required"`
	BaseURL    string `json:"base_url" binding:"required"`
	Model      string `json:"model"`
	Token      string `json:"token,omitempty"`
	ClearToken bool   `json:"clear_token"`
	TimeoutMS  int    `json:"timeout_ms"`
	Enabled    bool   `json:"enabled"`
}

// UpdateDLPRequest 是 DLP 配置的写入请求。
type UpdateDLPRequest struct {
	Enabled                bool                `json:"enabled"`
	Scanners               []string            `json:"scanners"`
	ConfirmEnabled         bool                `json:"confirm_enabled"`
	ConfirmTimeoutMS       int                 `json:"confirm_timeout_ms"`
	CacheEnabled           bool                `json:"cache_enabled"`
	CacheSensitiveTTLHours int                 `json:"cache_sensitive_ttl_hours"`
	CacheBenignTTLHours    int                 `json:"cache_benign_ttl_hours"`
	BlockOnHighSeverity    bool                `json:"block_on_high_severity"`
	AllGroups              bool                `json:"all_groups"`
	GroupIDs               []int64             `json:"group_ids"`
	Endpoints              []UpdateDLPEndpoint `json:"endpoints"`
	// Rules 是管理员提交的规则设置。前端提交全量列表，后端只留与内置默认值的偏差
	// （见 normalizeDLPRuleOverrides）。省略该字段时保持原有覆盖不变。
	Rules []UpdateDLPRule `json:"rules,omitempty"`
}

// UpdateDLPRule 是单条规则的写入请求。
type UpdateDLPRule struct {
	ID string `json:"id" binding:"required"`
	// Severity 为空时沿用内置默认严重度。
	Severity RiskLevel `json:"severity,omitempty"`
	// Enabled 为 false 表示逐条关掉该规则。
	// 用 Enabled 而非 Disabled：与界面上的勾选框同向，避免前端反转语义时出错。
	Enabled bool `json:"enabled"`
}

// dlpRuleOverridesFromUpdate 把写入请求里的规则列表转成覆盖表。
//
// req 为 nil 表示本次请求没带 rules 字段，返回 current 保持原值不变——
// 与 dlpStorageFromUpdate 对 dlp 字段的处理保持一致的语义。
func dlpRuleOverridesFromUpdate(current DLPRuleOverrides, rules []UpdateDLPRule) DLPRuleOverrides {
	if rules == nil {
		return normalizeDLPRuleOverrides(current)
	}
	overrides := make(DLPRuleOverrides, len(rules))
	for _, rule := range rules {
		overrides[rule.ID] = DLPRuleOverride{Severity: rule.Severity, Disabled: !rule.Enabled}
	}
	return normalizeDLPRuleOverrides(overrides)
}

// publicDLPFromStorage 把持久化配置转成对外视图。
//
// stored 为 nil 时返回"关闭且检测器全可选"的默认视图，让前端能正常渲染表单。
// invalidTokenIDs 沿用 upstream 传给 PublicFromStorage 的同一份集合。
func publicDLPFromStorage(stored *DLPConfig, invalidTokenIDs map[string]struct{}) PublicDLPConfig {
	public := PublicDLPConfig{
		Scanners:            []string{},
		GroupIDs:            []int64{},
		Endpoints:           []PublicDLPEndpoint{},
		AvailableScanners:   dlpScannerDefinitionList(),
		AvailableSeverities: ConfigurableDLPSeverities(),
		BlockingSeverities:  BlockingDLPSeverities(),
	}
	if stored == nil {
		// 未配置过 DLP 时给「全部分组」作为表单默认，与 upstream 新建配置的默认一致。
		public.AllGroups = true
		// 规则表按内置默认值下发，让表单在首次配置时也能正常渲染。
		public.Rules = DLPRuleCatalog(nil)
		return public
	}
	public.Rules = DLPRuleCatalog(stored.RuleOverrides)
	public.AllGroups = stored.AllGroups
	if len(stored.GroupIDs) > 0 {
		public.GroupIDs = append(public.GroupIDs, stored.GroupIDs...)
	}
	public.Enabled = stored.Enabled
	public.ConfirmEnabled = stored.ConfirmEnabled
	public.ConfirmTimeoutMS = stored.ConfirmTimeoutMS
	public.CacheEnabled = stored.CacheEnabled
	public.CacheSensitiveTTLHours = stored.CacheSensitiveTTLHours
	public.CacheBenignTTLHours = stored.CacheBenignTTLHours
	public.BlockOnHighSeverity = stored.BlockOnHighSeverity
	if len(stored.Scanners) > 0 {
		public.Scanners = append(public.Scanners, stored.Scanners...)
	}
	for _, endpoint := range stored.Endpoints {
		hasToken := strings.TrimSpace(endpoint.TokenCiphertext) != ""
		public.Endpoints = append(public.Endpoints, PublicDLPEndpoint{
			ID: endpoint.ID, Name: endpoint.Name, BaseURL: endpoint.BaseURL,
			Model: endpoint.Model, TimeoutMS: endpoint.TimeoutMS, Enabled: endpoint.Enabled,
			HasToken:    hasToken,
			TokenStatus: dlpTokenStatus(endpoint.ID, hasToken, invalidTokenIDs),
		})
	}
	return public
}

// dlpTokenStatus 返回 token 的可用状态。
// 状态词沿用 upstream PublicEndpoint 的取值（missing / configured / invalid），
// 让前端可以复用同一套状态渲染逻辑。
func dlpTokenStatus(endpointID string, hasToken bool, invalidTokenIDs map[string]struct{}) string {
	if !hasToken {
		return "missing"
	}
	if _, invalid := invalidTokenIDs[endpointID]; invalid {
		// 加密密钥变更导致无法解密：节点在管理端仍可见，但运行时不参与调用。
		return "invalid"
	}
	return "configured"
}

// dlpScannerDefinitionList 返回全部 DLP 检测器定义。
func dlpScannerDefinitionList() []ScannerDefinition {
	result := make([]ScannerDefinition, len(dlpScannerDefinitions))
	copy(result, dlpScannerDefinitions)
	return result
}

// dlpStorageFromUpdate 把写入请求转成持久化配置。
//
// req 为 nil 表示本次请求没带 dlp 字段（旧客户端），返回 current 保持原值不变——
// 绝不能因此把已有的 DLP 配置清空。
func dlpStorageFromUpdate(
	current *DLPConfig, req *UpdateDLPRequest, encryptor dlpTokenEncryptor,
) (*DLPConfig, error) {
	if req == nil {
		return current, nil
	}
	next := &DLPConfig{
		Enabled:                req.Enabled,
		ConfirmEnabled:         req.ConfirmEnabled,
		ConfirmTimeoutMS:       req.ConfirmTimeoutMS,
		CacheEnabled:           req.CacheEnabled,
		CacheSensitiveTTLHours: req.CacheSensitiveTTLHours,
		CacheBenignTTLHours:    req.CacheBenignTTLHours,
		BlockOnHighSeverity:    req.BlockOnHighSeverity,
		AllGroups:              req.AllGroups,
		// 落库即排序去重，让 ActiveDLPConfig.IncludesGroup 的二分查找有序前提成立。
		GroupIDs:  sortedUniqueGroupIDs(req.GroupIDs),
		Scanners:  normalizeDLPScanners(req.Scanners),
		Endpoints: make([]StorageEndpoint, 0, len(req.Endpoints)),
	}
	currentByID := map[string]StorageEndpoint{}
	var currentOverrides DLPRuleOverrides
	if current != nil {
		currentOverrides = current.RuleOverrides
		for _, endpoint := range current.Endpoints {
			currentByID[endpoint.ID] = endpoint
		}
	}
	next.RuleOverrides = dlpRuleOverridesFromUpdate(currentOverrides, req.Rules)
	for _, endpoint := range req.Endpoints {
		stored, err := dlpStoredEndpoint(endpoint, currentByID, encryptor)
		if err != nil {
			return nil, err
		}
		next.Endpoints = append(next.Endpoints, stored)
	}
	if err := ValidateDLPConfig(*next); err != nil {
		return nil, err
	}
	return next, nil
}

// dlpTokenEncryptor 抽出加密所需的最小能力，便于单测打桩。
type dlpTokenEncryptor interface {
	Encrypt(value string) (string, error)
	KeyConfigured() bool
}

// KeyConfigured 暴露 ConfigManager 是否配置了固定加密密钥。
//
// 方法定义放在 companion 文件里（Go 允许同包任意文件为类型定义方法），
// 这样 upstream 的 prompt_config_store.go 无需任何改动。
func (m *ConfigManager) KeyConfigured() bool {
	if m == nil {
		return false
	}
	return m.encryptionKeyConfigured
}

// dlpStoredEndpoint 转换单个确认节点，处理 token 的三种语义。
func dlpStoredEndpoint(
	endpoint UpdateDLPEndpoint, currentByID map[string]StorageEndpoint, encryptor dlpTokenEncryptor,
) (StorageEndpoint, error) {
	baseURL, err := NormalizeBaseURL(endpoint.BaseURL)
	if err != nil {
		return StorageEndpoint{}, err
	}
	stored := StorageEndpoint{
		ID: strings.TrimSpace(endpoint.ID), Name: strings.TrimSpace(endpoint.Name),
		BaseURL: baseURL, Model: strings.TrimSpace(endpoint.Model),
		TimeoutMS: endpoint.TimeoutMS, Enabled: endpoint.Enabled,
	}
	if stored.Model == "" {
		stored.Model = DefaultDLPConfirmModel
	}
	old, hadOld := currentByID[stored.ID]
	switch {
	case endpoint.ClearToken:
		stored.TokenCiphertext = ""
	case strings.TrimSpace(endpoint.Token) != "":
		if encryptor == nil {
			return StorageEndpoint{}, infraerrors.BadRequest(ErrorCodeEncryptionKeyRequired,
				"加密组件不可用，无法保存 DLP 确认节点 Token")
		}
		if !encryptor.KeyConfigured() {
			return StorageEndpoint{}, infraerrors.BadRequest(ErrorCodeEncryptionKeyRequired,
				"未配置固定加密密钥，DLP 确认节点 Token 将在服务重启后失效。"+
					"请先设置 TOTP_ENCRYPTION_KEY 环境变量（64 位十六进制）并重启服务")
		}
		ciphertext, err := encryptor.Encrypt(strings.TrimSpace(endpoint.Token))
		if err != nil {
			return StorageEndpoint{}, fmt.Errorf("encrypt dlp endpoint token: %w", err)
		}
		stored.TokenCiphertext = ciphertext
	case hadOld:
		// 未传 token 且未要求清空：保留原密文，避免编辑其他字段时把 token 弄丢。
		stored.TokenCiphertext = old.TokenCiphertext
	}
	return stored, nil
}

// normalizeDLPScanners 过滤非法的检测器 ID 并去重，保持 catalog 顺序。
func normalizeDLPScanners(scanners []string) []string {
	if len(scanners) == 0 {
		return nil
	}
	selected := map[string]struct{}{}
	for _, id := range scanners {
		trimmed := strings.TrimSpace(id)
		if IsDLPScanner(trimmed) {
			selected[trimmed] = struct{}{}
		}
	}
	result := make([]string, 0, len(selected))
	for _, id := range DLPScannerIDs() {
		if _, ok := selected[id]; ok {
			result = append(result, id)
		}
	}
	return result
}
