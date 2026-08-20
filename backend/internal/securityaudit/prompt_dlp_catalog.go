// prompt_dlp_catalog.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP scanner 的 ID 常量与 catalog 注册。
//
// 为什么要有这个文件：
//   - ScannerCatalog / AllScannerIDs 定义在 upstream 的 prompt_qwen3guard.go 里。
//     直接往那两个字面量里加条目会与 upstream 后续改动撞车，所以这里改用 init()
//     在运行时注入，upstream 文件保持零改动。
//   - prompt_issue_summary.go:15 对不在 ScannerCatalog 里的 category 会静默
//     丢弃（前端也不显示），因此注册是必须的，不是可选优化。
//   - prompt_config.go:322 用 ScannerCatalog 校验管理员提交的 scanner 白名单，
//     不注册会导致 DLP scanner ID 被配置校验拒掉。
//
// 与 upstream 合并策略：
//   - 纯新增文件 + init() 注入，merge 时不会冲突。
//
// =============================================================================
package securityaudit

// DLP scanner ID。与 qwen3guard 的 9 个模型分类并列，通过 enabledScanners 独立开关。
const (
	DLPScannerCredential = "dlp_credential"
	DLPScannerPII        = "dlp_pii"
	DLPScannerSensitive  = "dlp_sensitive"
)

// dlpScannerDefinitions 是三个 DLP scanner 的展示元数据。
var dlpScannerDefinitions = []ScannerDefinition{
	{
		ID: DLPScannerCredential, Label: "Credential Leak", LabelZH: "凭证泄露",
		Description: "API key, token, private key or database connection string leakage",
	},
	{
		ID: DLPScannerPII, Label: "Personal Information", LabelZH: "个人信息",
		Description: "Chinese ID card, bank card or mobile phone number",
	},
	{
		ID: DLPScannerSensitive, Label: "Sensitive Field", LabelZH: "敏感字段",
		Description: "Cloud access key, password field or JDBC connection string",
	},
}

// DLPScannerIDs 返回全部 DLP scanner ID。
func DLPScannerIDs() []string {
	result := make([]string, 0, len(dlpScannerDefinitions))
	for _, definition := range dlpScannerDefinitions {
		result = append(result, definition.ID)
	}
	return result
}

// IsDLPScanner 判断一个 scanner ID 是否属于 DLP 检测器。
func IsDLPScanner(id string) bool {
	switch id {
	case DLPScannerCredential, DLPScannerPII, DLPScannerSensitive:
		return true
	default:
		return false
	}
}

// init 把 DLP scanner 注册进 upstream 的 ScannerCatalog。
//
// 只注册 catalog，刻意不往 AllScannerIDs 追加：
//   - AllScannerIDs 在 upstream 的语义是「qwen3guard 的模型分类列表」，会被原样
//     传给 ParseQwen3Guard 当作启用分类，也被 upstream 测试当作规范列表断言。
//     往里塞 DLP ID 属于语义污染，会让 qwen3guard 的解析结果凭空多出永不命中的
//     分类，并直接弄坏 upstream 测试。
//   - DLP 检测器的启停走自己的配置（见 prompt_config_dlp.go 的 Scanners 字段），
//     与 qwen3guard 的 Scanners 列表互不干扰。
//
// 注册 ScannerCatalog 则是必须的：
//   - prompt_issue_summary.go:15 会丢弃不在 catalog 里的 category，前端不显示；
//   - prompt_config.go:322 用 catalog 校验管理员提交的 scanner 白名单。
//
// 幂等：重复注册同一 ID 时跳过。
func init() {
	for _, definition := range dlpScannerDefinitions {
		if _, exists := ScannerCatalog[definition.ID]; exists {
			continue
		}
		ScannerCatalog[definition.ID] = definition
	}
}
