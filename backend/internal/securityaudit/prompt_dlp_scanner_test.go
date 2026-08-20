package securityaudit

import (
	"strings"
	"testing"
)

// hitRule 断言扫描结果中命中了指定规则。
func hitRule(t *testing.T, text, ruleID string) {
	t.Helper()
	result := ScanDLP(text, nil)
	for _, finding := range result.Findings {
		if finding.RuleID == ruleID {
			return
		}
	}
	t.Errorf("原文 %q 期望命中规则 %s，实际命中 %v（排除 %d 条：%v）",
		text, ruleID, findingRuleIDs(result.Findings), result.ExcludedCount, result.ExcludedReasons)
}

// missRule 断言扫描结果中没有命中指定规则。
func missRule(t *testing.T, text, ruleID, reason string) {
	t.Helper()
	result := ScanDLP(text, nil)
	for _, finding := range result.Findings {
		if finding.RuleID == ruleID {
			t.Errorf("原文 %q 不应命中规则 %s（%s），但命中了，片段=%q",
				text, ruleID, reason, finding.Match)
			return
		}
	}
}

// missAll 断言完全没有任何命中。
func missAll(t *testing.T, text, reason string) {
	t.Helper()
	result := ScanDLP(text, nil)
	if len(result.Findings) > 0 {
		t.Errorf("原文 %q 不应有任何命中（%s），实际命中 %v",
			text, reason, findingRuleIDs(result.Findings))
	}
}

func findingRuleIDs(findings []DLPFinding) []string {
	result := make([]string, 0, len(findings))
	for _, finding := range findings {
		result = append(result, finding.RuleID)
	}
	return result
}

// ---------- 凭证泄露检测器 ----------

func TestDLPScanCredentialMatches(t *testing.T) {
	hitRule(t, "aws_key = AKIAZXCVBNMQWERTYUI7", "credential-aws-access-key")
	hitRule(t, "token: ghp_aB3dE5fG7hJ9kL1mN3pQ5rS7tU9vW1xY3zA5", "credential-github-token")
	hitRule(t, "key=AIzaSyC8Xk9mQ2vL8nPz7bR4tY6uI0oP3aS5dF7", "credential-google-api-key")
	// 用拼接构造，避免仓库里出现完整的 sk_live_ 字面量被平台密钥扫描误判为真实凭证。
	hitRule(t, "stripe: "+"sk_"+"live_"+"9zQm4Vt7Kb2Rn8Xc5Wp3Ld6H", "credential-stripe-secret-key")
	hitRule(t, "slack xoxb-2401-59087-abcdefghijk", "credential-slack-token")
	hitRule(t, "-----BEGIN RSA PRIVATE KEY-----", "credential-private-key-block")
	hitRule(t, "-----BEGIN PRIVATE KEY-----", "credential-private-key-block")
	hitRule(t, "DATABASE_URL=postgres://admin:Pr0dPass9@db.corp.com:5432/main",
		"credential-db-connection-string")
	hitRule(t, "mongodb://svcuser:S3cr3tPw@cluster0.prod.net:27017/app",
		"credential-db-connection-string")
}

func TestDLPScanCredentialJWT(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiI5OTg4NzciLCJuYW1lIjoiWmhhbmcifQ.aB3dE5fG7hJ9kL1mN3pQ5rS7tU9v"
	hitRule(t, "Authorization: Bearer "+jwt, "credential-jwt")
	missRule(t, "https://cdn.example.net/img.png?jwt="+jwt, "credential-jwt",
		"URL 查询参数里的 JWT 多为图片签名临时凭证")
}

func TestDLPScanCredentialExclusions(t *testing.T) {
	missRule(t, "aws_access_key_id = AKIAIOSFODNN7EXAMPLE", "credential-aws-access-key",
		"AWS 官方文档示例值")
	missRule(t, `api_key = "your-api-key-here"`, "credential-generic-api-key",
		"占位符前缀 your-")
	missRule(t, `api_key = "placeholder-value-here"`, "credential-generic-api-key",
		"占位符 placeholder")
	missRule(t, `client_secret: "super-secret-value-xx"`, "credential-generic-api-key",
		"假值特征子串 super-secret")
	missRule(t, `access_token = "abcdefghijklmnopqrst"`, "credential-generic-api-key",
		"值不含任何数字")
	missRule(t, "DATABASE_URL=postgres://user:pass@localhost:5432/dev",
		"credential-db-connection-string", "本地地址非外泄")
	missRule(t, "DATABASE_URL=mysql://root:rootpw@127.0.0.1:3306/test",
		"credential-db-connection-string", "回环地址非外泄")
	missRule(t, "REDIS_URL=redis://default:pw123456@10.0.3.12:6379/0",
		"credential-db-connection-string", "内网 10.x 地址非外泄")
	missRule(t, "DSN=postgres://svc:pw999@172.20.5.8:5432/db",
		"credential-db-connection-string", "内网 172.16-31.x 地址非外泄")
	missRule(t, "DSN=postgres://svc:pw999@192.168.1.20:5432/db",
		"credential-db-connection-string", "内网 192.168.x 地址非外泄")
}

// ---------- PII 检测器 ----------

func TestDLPScanPIIMatches(t *testing.T) {
	hitRule(t, "身份证号 110101199003072316 已核验", "pii-idcard")
	hitRule(t, "客户联系电话 13912345678 请尽快回电", "pii-phone")
	hitRule(t, "银行卡 4539128473610583 已绑定", "pii-bankcard")
}

func TestDLPScanPIIValidatorRejects(t *testing.T) {
	missRule(t, "编码 110101199003072311 待确认", "pii-idcard", "身份证校验码错误")
	missRule(t, "号码 15412345678 无效", "pii-phone", "154 为数据卡号段")
	missRule(t, "卡号 1234567890123456 测试", "pii-bankcard", "BIN 前缀不可信")
	missRule(t, "卡号 4111111111111112 测试", "pii-bankcard", "Luhn 校验失败")
}

func TestDLPScanPIIExclusions(t *testing.T) {
	// 这是实测中 luna 会误判的 case，必须在正则层拦掉。
	missRule(t, `{"order_no":"13912345678"}`, "pii-phone",
		"order_no 是内部 ID 语义字段")
	missRule(t, `{"device_id":"13912345678"}`, "pii-phone", "device_id 为内部 ID")
	missRule(t, `{"trace_id": "13912345678"}`, "pii-phone", "trace_id 为内部 ID")
	missRule(t, "设备编号：13912345678", "pii-phone", "中文 ID 标识")
	missRule(t, "IMG_15012345678.jpg", "pii-phone", "文件名中的时间戳片段")
	missRule(t, "测试用号码 13800000000", "pii-phone", "含 3 位以上连续相同数字")
	missRule(t, `C:\Users\13912345678\Desktop`, "pii-phone", "Windows 用户名路径")
	missRule(t, "/Users/13912345678/project", "pii-phone", "Unix 用户目录路径")
	missRule(t, "价格 12.13912345678 元", "pii-phone", "小数的小数部分")
	missRule(t, "asset_id=65116464360759296", "pii-bankcard", "已知内部编号白名单")
	missRule(t, "token=abc13912345678def", "pii-phone", "命中串前后紧邻字母")
}

func TestDLPScanPIIFieldNameWhitelistForcesReport(t *testing.T) {
	// 字段名含 id 但属于 PII 白名单时必须上报，不能被 ID 排除规则吃掉。
	hitRule(t, `{"idcard":"110101199003072316"}`, "pii-idcard")
	hitRule(t, `{"idcard_no":"110101199003072316"}`, "pii-idcard")
	hitRule(t, `{"phone":"13912345678"}`, "pii-phone")
	hitRule(t, `{"mobile":"13912345678"}`, "pii-phone")
	hitRule(t, `{"bankcard":"4539128473610583"}`, "pii-bankcard")
	hitRule(t, `{"bankcardno":"4539128473610583"}`, "pii-bankcard")
}

func TestDLPScanPIIIDFieldPrescan(t *testing.T) {
	// 同一数字串在别处以 ID 字段值出现过，裸出现也应视为 ID。
	text := `{"vehicle_id":"13912345678"} 另外还有 13912345678 这个值`
	missRule(t, text, "pii-phone", "同值预扫描：该串在原文中以 ID 字段值出现")
}

// ---------- 敏感信息检测器 ----------

func TestDLPScanSensitiveMatches(t *testing.T) {
	hitRule(t, "db_password: Xk9#mQ2vL8nPz", "sensitive-password-field")
	hitRule(t, "aliyun key LTAIabcdefgh12345678", "sensitive-cloud-key-prefix")
	hitRule(t, "tencent AKIDxyz9876543210abc", "sensitive-cloud-key-prefix")
	hitRule(t, "jdbc:mysql://appuser:Pr0dPw88@db.corp.com:3306/orders",
		"sensitive-jdbc-connection")
}

func TestDLPScanKeyValueRulesCoverJSONForm(t *testing.T) {
	// API 网关流量里 JSON 是主流格式，而 JSON 的键形态是 "key":"value" ——
	// 关键词与冒号之间隔着一个引号。正则若只写 keyword\s*[:=] 会漏掉全部 JSON
	// 载荷，这是高影响漏报，必须逐形态守住。
	cases := map[string]string{
		`{"db_password":"Xk9#mQ2vL8nPz"}`:            "sensitive-password-field",
		`{"db_password": "Xk9#mQ2vL8nPz"}`:           "sensitive-password-field",
		`{'db_password': 'Xk9#mQ2vL8nPz'}`:           "sensitive-password-field",
		`{"api_key":"sk_abcdefgh12345678xyz"}`:       "credential-generic-api-key",
		`{"client_secret":"aB3dE5fG7hJ9kL1mN3pQ"}`:   "credential-generic-api-key",
		`{"access_token": "aB3dE5fG7hJ9kL1mN3pQ5r"}`: "credential-generic-api-key",
	}
	for text, ruleID := range cases {
		hitRule(t, text, ruleID)
	}
}

func TestDLPScanKeyValueRulesKeepExclusionsInJSONForm(t *testing.T) {
	// 放宽引号后排除链仍须生效，否则会把占位符全报成真实密钥。
	cases := map[string]string{
		`{"api_key":"your-api-key-here-xx"}`:   "占位符前缀",
		`{"password":"123456"}`:                "已知测试口令",
		`{"password":"null"}`:                  "占位空值",
		`{"client_secret":"super-secret-val"}`: "假值特征子串",
	}
	for text, reason := range cases {
		result := ScanDLP(text, nil)
		if len(result.Findings) > 0 {
			t.Errorf("原文 %q 不应命中（%s），实际 %v", text, reason, findingRuleIDs(result.Findings))
		}
	}
}

func TestDLPScanSensitivePasswordExclusions(t *testing.T) {
	cases := map[string]string{
		"password = null":                       "占位空值",
		"password: undefined":                   "占位空值",
		"password = ...":                        "占位符",
		"password: ${DB_PASSWORD}":              "模板变量",
		"password = your-password":              "占位符前缀",
		"pwd: test@123":                         "已知测试口令",
		"password = 123456":                     "已知测试口令",
		"password: password":                    "字面量词",
		"password = pick_finish":                "无数字的单词标签",
		"pwd: cargo":                            "无数字的单词标签",
		"password = 100062":                     "12 位以下纯数字",
		"password: 密码":                          "纯中文",
		`password = os.getenv("DB_PASSWORD")`:   "代码引用",
		"config in /src/main/resources pwd=abc": "Maven 路径噪声",
	}
	for text, reason := range cases {
		missRule(t, text, "sensitive-password-field", reason)
	}
}

func TestDLPScanSensitivePasswordKeywordBoundary(t *testing.T) {
	// pass/pwd 粘在更大单词里不算密码字段。
	missRule(t, "donkey = brownAnimal99", "sensitive-password-field", "donkey 含 key 子串")
	missRule(t, "accessory: leatherBag12", "sensitive-password-field", "accessory 含 access 子串")
}

func TestDLPScanSensitiveCloudKeyBareFieldName(t *testing.T) {
	missRule(t, "配置项包含 access_key_id 与 access_key_secret 两个字段",
		"sensitive-cloud-key-field", "命中仅为裸字段名，未附带值")
	missRule(t, "文档说明 accesskeyid 的含义", "sensitive-cloud-key-field",
		"命中仅为裸字段名")
}

func TestDLPScanSensitiveJDBCExclusions(t *testing.T) {
	missRule(t, "jdbc:mysql://user:pass@host:port/db", "sensitive-jdbc-connection",
		"host:port 字面占位")
	missRule(t, "jdbc:mysql://root:rootpw@localhost:3306/dev", "sensitive-jdbc-connection",
		"本地地址")
	missRule(t, "jdbc:postgresql://svc:pw1@10.2.3.4:5432/db", "sensitive-jdbc-connection",
		"内网地址")
}

// ---------- 去重 ----------

func TestDLPDedupeSensitiveYieldsToCloudKey(t *testing.T) {
	// Sensitive Field 的宽泛命中包含 Cloud Key 的 AKID 值时，应只保留具体的那条。
	result := ScanDLP("access_key_secret=AKIDxyz9876543210abc", nil)
	ids := findingRuleIDs(result.Findings)
	if containsString(ids, "sensitive-cloud-key-field") &&
		containsString(ids, "sensitive-cloud-key-prefix") {
		t.Errorf("宽泛的云密钥字段命中应被具体的云密钥值命中取代，实际=%v", ids)
	}
	if !containsString(ids, "sensitive-cloud-key-prefix") {
		t.Errorf("应保留具体的云密钥值命中，实际=%v", ids)
	}
}

func TestDLPDedupeGenericKeyYieldsToSpecific(t *testing.T) {
	// 通用 key=value 宽泛命中与具体的 AWS Key 命中重叠时，保留 AWS。
	result := ScanDLP("api_key=AKIAZXCVBNMQWERTYUI7", nil)
	ids := findingRuleIDs(result.Findings)
	if !containsString(ids, "credential-aws-access-key") {
		t.Errorf("应保留具体的 AWS Key 命中，实际=%v", ids)
	}
	if containsString(ids, "credential-generic-api-key") {
		t.Errorf("宽泛的通用 Key 命中应被具体命中取代，实际=%v", ids)
	}
}

func TestDLPDedupeIdenticalSpans(t *testing.T) {
	result := ScanDLP("身份证 110101199003072316", nil)
	seen := map[string]int{}
	for _, finding := range result.Findings {
		seen[finding.RuleID]++
	}
	for ruleID, count := range seen {
		if count > 1 {
			t.Errorf("规则 %s 在同一位置产出了 %d 条重复命中", ruleID, count)
		}
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// ---------- 引擎行为 ----------

func TestDLPScanEmptyText(t *testing.T) {
	if result := ScanDLP("", nil); len(result.Findings) != 0 {
		t.Error("空文本不应有命中")
	}
	if result := ScanDLP("   \n\t ", nil); len(result.Findings) != 0 {
		t.Error("纯空白文本不应有命中")
	}
}

func TestDLPScanCleanTextProducesNoFindings(t *testing.T) {
	missAll(t, "帮我写一个快速排序的 Python 实现，要求带注释", "普通请求无敏感信息")
	missAll(t, "What is the capital of France?", "普通英文请求")
}

func TestDLPScanRespectsEnabledScanners(t *testing.T) {
	text := "身份证 110101199003072316 手机 13912345678"
	// 只启用凭证检测器时，PII 命中不应出现。
	result := ScanDLP(text, []string{DLPScannerCredential})
	for _, finding := range result.Findings {
		if finding.ScannerID == DLPScannerPII {
			t.Errorf("未启用 PII scanner 时不应产出 PII 命中：%s", finding.RuleID)
		}
	}
	// 启用 PII 时应能命中。
	result = ScanDLP(text, []string{DLPScannerPII})
	if len(result.Findings) == 0 {
		t.Error("启用 PII scanner 后应有命中")
	}
}

func TestDLPScanRuneOffsets(t *testing.T) {
	// 含中文前缀时，rune 下标必须按字符算而非字节。
	text := "我的手机号是13912345678"
	result := ScanDLP(text, []string{DLPScannerPII})
	if len(result.Findings) != 1 {
		t.Fatalf("期望 1 条命中，实际 %d 条", len(result.Findings))
	}
	finding := result.Findings[0]
	if finding.StartRune != 6 {
		t.Errorf("StartRune = %d, 期望 6（中文按字符计数）", finding.StartRune)
	}
	if finding.EndRune != 17 {
		t.Errorf("EndRune = %d, 期望 17", finding.EndRune)
	}
}

func TestDLPScanSeverityAndCategories(t *testing.T) {
	result := ScanDLP("身份证 110101199003072316", nil)
	if got := HighestSeverity(result.Findings); got != RiskHigh {
		t.Errorf("身份证命中的最高严重度 = %s, 期望 high", got)
	}
	categories := DLPCategories(result.Findings)
	if !containsString(categories, DLPScannerPII) {
		t.Errorf("categories = %v, 期望含 %s", categories, DLPScannerPII)
	}
}

func TestDLPScanPhoneSeverityIsMedium(t *testing.T) {
	// 按 detection-rules.md 处置矩阵，手机号是 medium（仅审计不拦截）。
	result := ScanDLP("联系电话 13912345678", nil)
	if len(result.Findings) == 0 {
		t.Fatal("应命中手机号")
	}
	if got := HighestSeverity(result.Findings); got != RiskMedium {
		t.Errorf("手机号命中的严重度 = %s, 期望 medium", got)
	}
}

func TestDLPScanJWTSeverityIsMedium(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiI5OTg4NzciLCJuYW1lIjoiTGkifQ.aB3dE5fG7hJ9kL1mN3pQ5rS7tU9v"
	result := ScanDLP("Authorization: Bearer "+jwt, []string{DLPScannerCredential})
	if len(result.Findings) == 0 {
		t.Fatal("应命中 JWT")
	}
	if got := HighestSeverity(result.Findings); got != RiskMedium {
		t.Errorf("JWT 命中的严重度 = %s, 期望 medium", got)
	}
}

func TestDLPScanMultipleFindingsSorted(t *testing.T) {
	text := "手机 13912345678 身份证 110101199003072316"
	result := ScanDLP(text, nil)
	if len(result.Findings) < 2 {
		t.Fatalf("期望至少 2 条命中，实际 %d 条：%v",
			len(result.Findings), findingRuleIDs(result.Findings))
	}
	for index := 1; index < len(result.Findings); index++ {
		if result.Findings[index].startByte < result.Findings[index-1].startByte {
			t.Error("命中未按起始位置排序")
		}
	}
}

func TestDLPScanExcludedReasonsRecorded(t *testing.T) {
	result := ScanDLP("测试号码 13800000000", nil)
	if result.ExcludedCount == 0 {
		t.Error("应记录被排除的命中数量")
	}
	if len(result.ExcludedReasons) == 0 {
		t.Error("应记录排除原因便于调参")
	}
}

func TestDLPScanLongTextPerformance(t *testing.T) {
	// 确认长文本不会因正则回溯而卡死（RE2 无回溯，此处只做冒烟）。
	text := strings.Repeat("这是一段普通的中文描述文本，不含任何敏感信息。", 2000)
	result := ScanDLP(text, nil)
	if len(result.Findings) != 0 {
		t.Errorf("长普通文本不应有命中，实际 %v", findingRuleIDs(result.Findings))
	}
}

func TestDLPRulesAllCompile(t *testing.T) {
	// 规则表里每条规则的必填字段都要完整，避免新增规则时漏填。
	for _, rule := range DLPRules() {
		if rule.ID == "" || rule.Pattern == nil {
			t.Errorf("规则 %+v 缺少 ID 或 Pattern", rule)
		}
		if rule.ScannerID == "" || !IsDLPScanner(rule.ScannerID) {
			t.Errorf("规则 %s 的 ScannerID %q 非法", rule.ID, rule.ScannerID)
		}
		if rule.Severity == "" {
			t.Errorf("规则 %s 缺少 Severity", rule.ID)
		}
	}
}

func TestDLPCatalogRegistered(t *testing.T) {
	// 未注册到 ScannerCatalog 的 category 会被 BuildIssueSummaries 静默丢弃。
	for _, id := range DLPScannerIDs() {
		if _, ok := ScannerCatalog[id]; !ok {
			t.Errorf("scanner %s 未注册到 ScannerCatalog，命中将不会显示在前端", id)
		}
	}
}

func TestDLPDoesNotPolluteQwen3GuardScannerList(t *testing.T) {
	// AllScannerIDs 是 qwen3guard 的模型分类列表，会被原样传给 ParseQwen3Guard。
	// DLP ID 混进去会让解析结果多出永不命中的分类，并弄坏 upstream 测试。
	for _, id := range DLPScannerIDs() {
		if containsString(AllScannerIDs, id) {
			t.Errorf("DLP scanner %s 不应出现在 AllScannerIDs 中（qwen3guard 分类列表）", id)
		}
	}
}

func TestDLPBuildIssueSummariesRendersDLPCategories(t *testing.T) {
	// 端到端确认 DLP 分类能被 upstream 的 IssueSummary 构造函数渲染出来。
	result := NormalizedResult{
		Decision: EventCritical, RiskLevel: RiskHigh, Action: ActionBlock,
		Categories:      []string{DLPScannerPII},
		ScannerScores:   map[string]float64{DLPScannerPII: 0.95},
		ScannerEvidence: map[string]string{DLPScannerPII: "身份证号"},
	}
	summaries := BuildIssueSummaries(result)
	if len(summaries) != 1 {
		t.Fatalf("期望 1 条 IssueSummary，实际 %d 条", len(summaries))
	}
	if summaries[0].ScannerID != DLPScannerPII {
		t.Errorf("ScannerID = %s, 期望 %s", summaries[0].ScannerID, DLPScannerPII)
	}
	if summaries[0].Title == "" {
		t.Error("Title 不应为空，说明 catalog 元数据缺失")
	}
}
