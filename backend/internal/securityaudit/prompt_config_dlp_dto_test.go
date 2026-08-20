package securityaudit

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// dlpFakeEncryptor 打桩加密器，便于验证 token 的三种写入语义。
type dlpFakeEncryptor struct {
	keyConfigured bool
	failEncrypt   bool
}

func (e dlpFakeEncryptor) Encrypt(value string) (string, error) {
	if e.failEncrypt {
		return "", errors.New("encrypt failed")
	}
	return "cipher(" + value + ")", nil
}

func (e dlpFakeEncryptor) KeyConfigured() bool { return e.keyConfigured }

func dlpUpdateRequest() *UpdateDLPRequest {
	return &UpdateDLPRequest{
		Enabled: true, ConfirmEnabled: true, BlockOnHighSeverity: true,
		AllGroups:        true,
		Scanners:         []string{DLPScannerPII, DLPScannerCredential},
		ConfirmTimeoutMS: 5000,
		Endpoints: []UpdateDLPEndpoint{{
			ID: "e1", Name: "luna", BaseURL: "https://api.example.com",
			Model: DefaultDLPConfirmModel, Token: "secret-token",
			TimeoutMS: 5000, Enabled: true,
		}},
	}
}

func TestDLPStorageFromUpdateEncryptsToken(t *testing.T) {
	stored, err := dlpStorageFromUpdate(nil, dlpUpdateRequest(),
		dlpFakeEncryptor{keyConfigured: true})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if len(stored.Endpoints) != 1 {
		t.Fatalf("节点数量 = %d, 期望 1", len(stored.Endpoints))
	}
	if stored.Endpoints[0].TokenCiphertext != "cipher(secret-token)" {
		t.Errorf("token 应被加密，实际 %q", stored.Endpoints[0].TokenCiphertext)
	}
	if !stored.Enabled || !stored.ConfirmEnabled || !stored.BlockOnHighSeverity {
		t.Error("开关未正确透传")
	}
}

func TestDLPStorageFromUpdateNilRequestKeepsCurrent(t *testing.T) {
	// 旧客户端不带 dlp 字段时，绝不能把已有配置清空。
	current := &DLPConfig{Enabled: true, ConfirmEnabled: true}
	stored, err := dlpStorageFromUpdate(current, nil, dlpFakeEncryptor{keyConfigured: true})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if stored != current {
		t.Error("请求未带 dlp 字段时应原样保留当前配置")
	}
}

func TestDLPStorageFromUpdatePreservesTokenWhenOmitted(t *testing.T) {
	// 只改其他字段、不传 token 时，原密文必须保留，否则编辑一次就丢 token。
	current := &DLPConfig{Endpoints: []StorageEndpoint{
		{ID: "e1", TokenCiphertext: "old-cipher"},
	}}
	req := dlpUpdateRequest()
	req.Endpoints[0].Token = ""
	stored, err := dlpStorageFromUpdate(current, req, dlpFakeEncryptor{keyConfigured: true})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if stored.Endpoints[0].TokenCiphertext != "old-cipher" {
		t.Errorf("未传 token 时应保留原密文，实际 %q", stored.Endpoints[0].TokenCiphertext)
	}
}

func TestDLPStorageFromUpdateClearToken(t *testing.T) {
	current := &DLPConfig{Endpoints: []StorageEndpoint{
		{ID: "e1", TokenCiphertext: "old-cipher"},
	}}
	req := dlpUpdateRequest()
	req.Endpoints[0].Token = ""
	req.Endpoints[0].ClearToken = true
	req.ConfirmEnabled = false // 清空 token 后不强制要求可用节点
	stored, err := dlpStorageFromUpdate(current, req, dlpFakeEncryptor{keyConfigured: true})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if stored.Endpoints[0].TokenCiphertext != "" {
		t.Errorf("ClearToken 应清空密文，实际 %q", stored.Endpoints[0].TokenCiphertext)
	}
}

func TestDLPStorageFromUpdateRequiresEncryptionKey(t *testing.T) {
	_, err := dlpStorageFromUpdate(nil, dlpUpdateRequest(),
		dlpFakeEncryptor{keyConfigured: false})
	if err == nil {
		t.Fatal("未配置固定加密密钥时应拒绝保存 token")
	}
	if !strings.Contains(err.Error(), "TOTP_ENCRYPTION_KEY") {
		t.Errorf("错误提示应指引配置加密密钥，实际 %v", err)
	}
}

func TestDLPStorageFromUpdateNormalizesBaseURL(t *testing.T) {
	req := dlpUpdateRequest()
	req.Endpoints[0].BaseURL = "https://api.example.com/v1/"
	stored, err := dlpStorageFromUpdate(nil, req, dlpFakeEncryptor{keyConfigured: true})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if stored.Endpoints[0].BaseURL != "https://api.example.com" {
		t.Errorf("BaseURL 应被归一化，实际 %q", stored.Endpoints[0].BaseURL)
	}
}

func TestDLPStorageFromUpdateRejectsBadBaseURL(t *testing.T) {
	req := dlpUpdateRequest()
	req.Endpoints[0].BaseURL = "ftp://example.com"
	if _, err := dlpStorageFromUpdate(nil, req, dlpFakeEncryptor{keyConfigured: true}); err == nil {
		t.Fatal("非 HTTP(S) 地址应被拒绝")
	}
}

func TestDLPStorageFromUpdateDefaultsModel(t *testing.T) {
	req := dlpUpdateRequest()
	req.Endpoints[0].Model = ""
	stored, err := dlpStorageFromUpdate(nil, req, dlpFakeEncryptor{keyConfigured: true})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if stored.Endpoints[0].Model != DefaultDLPConfirmModel {
		t.Errorf("未填模型应回落到 %s，实际 %q", DefaultDLPConfirmModel, stored.Endpoints[0].Model)
	}
}

func TestDLPNormalizeScannersFiltersAndDedupes(t *testing.T) {
	got := normalizeDLPScanners([]string{
		DLPScannerPII, "violent", DLPScannerPII, " " + DLPScannerCredential + " ", "bogus",
	})
	if len(got) != 2 {
		t.Fatalf("规范化后数量 = %d, 期望 2，实际 %v", len(got), got)
	}
	// 顺序应跟随 catalog，保证结果稳定
	if got[0] != DLPScannerCredential || got[1] != DLPScannerPII {
		t.Errorf("顺序应跟随 catalog，实际 %v", got)
	}
}

func TestDLPPublicFromStorageHidesToken(t *testing.T) {
	stored := &DLPConfig{
		Enabled: true, ConfirmEnabled: true,
		Endpoints: []StorageEndpoint{{
			ID: "e1", Name: "luna", BaseURL: "https://api.example.com",
			Model: DefaultDLPConfirmModel, TokenCiphertext: "cipher(secret-token)", Enabled: true,
		}},
	}
	public := publicDLPFromStorage(stored, map[string]struct{}{})
	raw, err := json.Marshal(public)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(raw), "secret-token") || strings.Contains(string(raw), "cipher(") {
		t.Errorf("对外视图不得回显 token 明文或密文，实际 %s", raw)
	}
	if !public.Endpoints[0].HasToken {
		t.Error("应告知前端 token 已配置")
	}
	if public.Endpoints[0].TokenStatus != "configured" {
		t.Errorf("TokenStatus = %q, 期望 configured", public.Endpoints[0].TokenStatus)
	}
}

func TestDLPPublicFromStorageTokenStatuses(t *testing.T) {
	stored := &DLPConfig{Endpoints: []StorageEndpoint{
		{ID: "no-token"},
		{ID: "ok-token", TokenCiphertext: "c1"},
		{ID: "bad-token", TokenCiphertext: "c2"},
	}}
	public := publicDLPFromStorage(stored, map[string]struct{}{"bad-token": {}})
	want := map[string]string{"no-token": "missing", "ok-token": "configured", "bad-token": "invalid"}
	for _, endpoint := range public.Endpoints {
		if got := endpoint.TokenStatus; got != want[endpoint.ID] {
			t.Errorf("节点 %s 的 TokenStatus = %q, 期望 %q", endpoint.ID, got, want[endpoint.ID])
		}
	}
}

func TestDLPPublicFromStorageNilReturnsRenderableDefault(t *testing.T) {
	public := publicDLPFromStorage(nil, nil)
	if public.Enabled {
		t.Error("未配置时应为关闭")
	}
	if public.Scanners == nil || public.Endpoints == nil {
		t.Error("切片应为非 nil，避免前端渲染时报错")
	}
	if len(public.AvailableScanners) != len(DLPScannerIDs()) {
		t.Errorf("应下发全部可选检测器，实际 %d 个", len(public.AvailableScanners))
	}
}

func TestDLPActiveInvalidTokenEndpointIDs(t *testing.T) {
	cfg := ActiveDLPConfig{Endpoints: []ActiveEndpoint{
		{ID: "ok"}, {ID: "bad", TokenInvalid: true},
	}}
	ids := cfg.InvalidTokenEndpointIDs()
	if len(ids) != 1 || ids[0] != "bad" {
		t.Errorf("应只列出 token 失效的节点，实际 %v", ids)
	}
}

func TestDLPInvalidTokenSurfacedThroughActiveConfig(t *testing.T) {
	// DLP 节点的 token 失效必须能透出到管理端，否则节点被静默排除而无任何提示。
	cfg := ActiveConfig{
		Endpoints: []ActiveEndpoint{{ID: "guard-bad", TokenInvalid: true}},
		DLP: ActiveDLPConfig{Endpoints: []ActiveEndpoint{
			{ID: "dlp-bad", TokenInvalid: true},
		}},
	}
	ids := cfg.InvalidTokenEndpointIDs()
	found := map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if !found["guard-bad"] || !found["dlp-bad"] {
		t.Errorf("应同时包含 qwen3guard 与 DLP 的失效节点，实际 %v", ids)
	}
}

func TestDLPUpdateRequestOmittedInJSON(t *testing.T) {
	// UpdateConfigRequest 不带 dlp 时序列化不应出现该字段，保持与旧客户端兼容。
	raw, err := json.Marshal(UpdateConfigRequest{ExpectedConfigVersion: 1})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(raw), `"dlp"`) {
		t.Errorf("未设置 DLP 时不应出现 dlp 字段，实际 %s", raw)
	}
}

func TestDLPRoundTripThroughDTO(t *testing.T) {
	// 写入 → 持久化 → 运行时 的完整往返。
	stored, err := dlpStorageFromUpdate(nil, dlpUpdateRequest(),
		dlpFakeEncryptor{keyConfigured: true})
	if err != nil {
		t.Fatalf("写入转换失败: %v", err)
	}
	active := stored.ToActiveDLPConfig(func(cipher string) (string, error) {
		return strings.TrimSuffix(strings.TrimPrefix(cipher, "cipher("), ")"), nil
	})
	if !active.Enabled || !active.ConfirmReady() {
		t.Fatal("往返后 DLP 应处于启用且确认链路可用状态")
	}
	if active.Endpoints[0].Token != "secret-token" {
		t.Errorf("解密后的 token = %q, 期望 secret-token", active.Endpoints[0].Token)
	}
	scanners := active.EffectiveScanners()
	if len(scanners) != 2 {
		t.Errorf("启用的检测器数量 = %d, 期望 2，实际 %v", len(scanners), scanners)
	}
}
