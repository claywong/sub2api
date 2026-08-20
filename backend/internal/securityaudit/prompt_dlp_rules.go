// prompt_dlp_rules.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 敏感信息检测的正则规则库。
//
// 本文件按 detection-rules.md 的规则矩阵定义三大类检测器：
//   - 凭证泄露（credential）：AWS / GitHub / Google / Stripe / Slack / 私钥 /
//     JWT / 通用 key=value / 数据库连接串
//   - 个人信息（pii）：身份证 / 银行卡 / 手机号（均需配合校验器）
//   - 敏感信息（sensitive）：云密钥 / 密码字段 / JDBC 连接串
//
// 设计约束：
//   - Go 的 regexp 是 RE2，不支持 lookahead/lookbehind。凡"命中串前后紧邻
//     字母数字"这类边界判断，一律交给 prompt_dlp_exclusions.go 按匹配下标用
//     Go 代码实现，不写进正则。
//   - 需要算法校验的规则（身份证校验码、银行卡 Luhn、手机号号段）只在这里放
//     宽正则，真正的判定在 prompt_dlp_validators.go。
//
// 与 upstream 合并策略：
//   - 纯新增文件，不含任何 upstream 符号的改动，merge 时不会冲突。
//
// =============================================================================
package securityaudit

import "regexp"

// DLPDetectorClass 标识检测器大类，用于去重时决定优先级。
type DLPDetectorClass string

const (
	DLPClassCredential DLPDetectorClass = "credential"
	DLPClassPII        DLPDetectorClass = "pii"
	DLPClassSensitive  DLPDetectorClass = "sensitive"
)

// DLPValidatorKind 标识命中后需要执行的算法校验。
type DLPValidatorKind string

const (
	DLPValidatorNone     DLPValidatorKind = ""
	DLPValidatorIDCard   DLPValidatorKind = "idcard"
	DLPValidatorBankCard DLPValidatorKind = "bankcard"
	DLPValidatorPhone    DLPValidatorKind = "phone"
)

// DLPRule 描述一条正则检测规则。
//
// ValueGroup 指定"真正的敏感值"位于第几个捕获组：key=value 形式的规则命中的
// 整段包含字段名，排除链和二次确认都应该只看值部分。0 表示整个匹配即为值。
type DLPRule struct {
	ID         string
	Class      DLPDetectorClass
	ScannerID  string
	Title      string
	Severity   RiskLevel
	Confidence float64
	Pattern    *regexp.Regexp
	ValueGroup int
	Validator  DLPValidatorKind
	// Broad 标记宽泛规则。去重时若宽泛命中区间包含了具体高严重度命中，丢弃宽泛者。
	Broad bool
}

// dlpRules 是全部规则的注册表。顺序即为报告顺序，便于测试稳定比对。
var dlpRules = []DLPRule{
	{
		ID: "credential-aws-access-key", Class: DLPClassCredential,
		ScannerID: DLPScannerCredential, Title: "AWS Access Key",
		Severity: RiskMedium, Confidence: 0.9,
		Pattern: regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA)[0-9A-Z]{16}\b`),
	},
	{
		ID: "credential-github-token", Class: DLPClassCredential,
		ScannerID: DLPScannerCredential, Title: "GitHub Token",
		Severity: RiskMedium, Confidence: 0.9,
		Pattern: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`),
	},
	{
		ID: "credential-google-api-key", Class: DLPClassCredential,
		ScannerID: DLPScannerCredential, Title: "Google API Key",
		Severity: RiskMedium, Confidence: 0.9,
		Pattern: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
	},
	{
		ID: "credential-stripe-secret-key", Class: DLPClassCredential,
		ScannerID: DLPScannerCredential, Title: "Stripe Secret Key",
		Severity: RiskMedium, Confidence: 0.9,
		Pattern: regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[0-9a-zA-Z]{16,}\b`),
	},
	{
		ID: "credential-slack-token", Class: DLPClassCredential,
		ScannerID: DLPScannerCredential, Title: "Slack Token",
		Severity: RiskMedium, Confidence: 0.9,
		Pattern: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`),
	},
	{
		ID: "credential-private-key-block", Class: DLPClassCredential,
		ScannerID: DLPScannerCredential, Title: "私钥块",
		Severity: RiskMedium, Confidence: 0.95,
		Pattern: regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP )?PRIVATE KEY-----`),
	},
	{
		ID: "credential-jwt", Class: DLPClassCredential,
		ScannerID: DLPScannerCredential, Title: "JWT",
		Severity: RiskMedium, Confidence: 0.8,
		Pattern: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
	},
	{
		ID: "credential-generic-api-key", Class: DLPClassCredential,
		ScannerID: DLPScannerCredential, Title: "通用 API Key",
		Severity: RiskMedium, Confidence: 0.7, Broad: true,
		// ['"]?\s*[:=] 中的引号是必需的：JSON 形态是 "api_key":"value"，关键词与
		// 冒号之间隔着一个引号。少了它会漏掉 API 网关流量里最主流的 JSON 格式。
		Pattern: regexp.MustCompile(
			`(?i)(?:api[_-]?key|secret|access[_-]?token|auth[_-]?token|password|passwd|client[_-]?secret)` +
				`['"]?\s*[:=]\s*['"]?([A-Za-z0-9+/=_-]{16,})`),
		ValueGroup: 1,
	},
	{
		ID: "credential-db-connection-string", Class: DLPClassCredential,
		ScannerID: DLPScannerCredential, Title: "数据库连接串",
		Severity: RiskHigh, Confidence: 0.95,
		Pattern: regexp.MustCompile(`\b(?:mongodb\+srv|mongodb|postgresql|postgres|mysql|redis)://[^:/\s]+:[^@\s]+@[^\s/?]+`),
	},

	{
		ID: "pii-idcard", Class: DLPClassPII,
		ScannerID: DLPScannerPII, Title: "身份证号",
		Severity: RiskHigh, Confidence: 0.95,
		Pattern:   regexp.MustCompile(`\d{17}[\dXx]`),
		Validator: DLPValidatorIDCard,
	},
	{
		ID: "pii-bankcard", Class: DLPClassPII,
		ScannerID: DLPScannerPII, Title: "银行卡号",
		Severity: RiskHigh, Confidence: 0.9,
		Pattern:   regexp.MustCompile(`\d{16,19}`),
		Validator: DLPValidatorBankCard,
	},
	{
		ID: "pii-phone", Class: DLPClassPII,
		ScannerID: DLPScannerPII, Title: "手机号",
		Severity: RiskMedium, Confidence: 0.85,
		Pattern:   regexp.MustCompile(`1[3-9]\d{9}`),
		Validator: DLPValidatorPhone,
	},

	{
		// 刻意要求必须带值：detection-rules.md 规定裸字段名（无值）不算泄露，
		// 用正则强制值存在比事后排除更直接。同时让命中区间覆盖整个 key=value，
		// 这样当值本身是 LTAI/AKID 时，去重逻辑能识别出"宽泛命中包含具体命中"
		// 并只保留更具体的那条，避免同一个密钥被报两遍。
		ID: "sensitive-cloud-key-field", Class: DLPClassSensitive,
		ScannerID: DLPScannerSensitive, Title: "云密钥字段",
		Severity: RiskHigh, Confidence: 0.8, Broad: true,
		Pattern: regexp.MustCompile(
			`(?i)access[_\- ]?key[_\- ]?(?:id|secret)['"]?\s*[:=]\s*['"]?([^\s'",;}\]]+)`),
		ValueGroup: 1,
	},
	{
		ID: "sensitive-cloud-key-prefix", Class: DLPClassSensitive,
		ScannerID: DLPScannerSensitive, Title: "云密钥",
		Severity: RiskHigh, Confidence: 0.95,
		Pattern: regexp.MustCompile(`\b(?:LTAI|AKID)[A-Za-z0-9]{12,40}`),
	},
	{
		ID: "sensitive-password-field", Class: DLPClassSensitive,
		ScannerID: DLPScannerSensitive, Title: "密码字段",
		Severity: RiskHigh, Confidence: 0.75, Broad: true,
		Pattern:    regexp.MustCompile(`(?i)(?:passwd|password|pwd|pass)['"]?\s*[:=]\s*['"]?([^\s'",;}\]]+)`),
		ValueGroup: 1,
	},
	{
		ID: "sensitive-jdbc-connection", Class: DLPClassSensitive,
		ScannerID: DLPScannerSensitive, Title: "JDBC 连接串",
		Severity: RiskHigh, Confidence: 0.95,
		Pattern: regexp.MustCompile(`jdbc:[a-z0-9]+://[^:/\s]+:[^@\s]+@[^\s/?]+`),
	},
}

// DLPRules 返回规则注册表的只读视图。
func DLPRules() []DLPRule {
	result := make([]DLPRule, len(dlpRules))
	copy(result, dlpRules)
	return result
}

// dlpRuleByID 便于测试与审计日志按 ID 反查规则。
func dlpRuleByID(id string) (DLPRule, bool) {
	for _, rule := range dlpRules {
		if rule.ID == id {
			return rule, true
		}
	}
	return DLPRule{}, false
}
