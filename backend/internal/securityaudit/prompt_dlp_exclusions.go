// prompt_dlp_exclusions.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 命中的排除链（降误报）。
//
// 对应 detection-rules.md 里各检测器的「不匹配的（主动排除）」小节。排除链在
// 正则命中之后、算法校验之前/之后运行，命中排除即丢弃该 finding。
//
// 为什么排除链必须在正则层做（而不是全丢给 LLM 二次确认）：
//
//	实测 gpt-5.6-luna 会把 {"order_no":"13912345678"} 判成真实手机号。这类
//	结构性误报（ID 字段、文件名、内网地址）LLM 兜不住，必须靠规则拦掉。LLM 只
//	负责它擅长的语义判断（占位符、示例值、上下文用途）。
//
// RE2 限制：
//
//	Go 的 regexp 不支持 lookahead/lookbehind，所以「命中串前后紧邻字母数字」
//	这条只能按匹配下标用 Go 代码判断，见 boundaryTouchesAlphanumeric。
//
// 与 upstream 合并策略：
//   - 纯新增文件，无 upstream 符号改动，merge 时不会冲突。
//
// =============================================================================
package securityaudit

import "strings"

// placeholderPrefixes 是文档/示例占位符的前缀特征。
var placeholderPrefixes = []string{
	"your-", "your_", "yourapi", "placeholder", "example", "sample", "dummy",
	"fake", "redacted", "changeme", "change-me", "xxx", "<", "test_", "test-",
	"foobar", "foo-bar", "todo", "tbd", "insert", "replace-me", "myapi",
}

// fakeValueSubstrings 是假值特征子串。
var fakeValueSubstrings = []string{
	"super-secret", "super_secret", "supersecret", "notarealkey", "not-a-real",
	"aaaaaaaa", "xxxxxxxx", "1234567890abcdef",
}

// knownExampleValues 是各厂商文档里的知名示例值，精确匹配即排除。
var knownExampleValues = map[string]struct{}{
	"akiaiosfodnn7example":                     {},
	"wjalrxutnfemi/k7mdeng/bpxrficyexamplekey": {},
}

// literalSecretWords 是「字面量词」——值本身就是这些词时，不是真实密钥。
var literalSecretWords = map[string]struct{}{
	"password": {}, "passwd": {}, "pwd": {}, "pass": {}, "secret": {},
	"token": {}, "admin": {}, "root": {}, "user": {}, "username": {},
	"test": {}, "demo": {}, "none": {}, "null": {}, "nil": {},
	"undefined": {}, "true": {}, "false": {}, "unknown": {}, "empty": {},
	"apikey": {}, "api_key": {}, "accesskey": {}, "access_key": {},
	"yourpassword": {}, "mypassword": {},
}

// knownTestPasswords 是已知测试口令，精确匹配即排除。
var knownTestPasswords = map[string]struct{}{
	"test": {}, "test@123": {}, "test123": {}, "1234": {}, "12345": {},
	"123456": {}, "1234567": {}, "12345678": {}, "123456789": {}, "1234567890": {},
	"abc123": {}, "admin123": {}, "password123": {}, "qwerty": {}, "111111": {},
}

// emptyValueTokens 是占位/空值 token。
var emptyValueTokens = map[string]struct{}{
	"null": {}, "none": {}, "nil": {}, "undefined": {}, "true": {}, "false": {},
	"unknown": {}, "...": {}, "*": {}, "-": {}, "n/a": {}, "na": {}, "": {},
}

// emptyValuePrefixes 是模板变量/占位符前缀。
var emptyValuePrefixes = []string{
	"...", "***", "xxx", "<", "${", "{{", "%s", "%(", "$(",
}

// idFieldSubstrings 是平台内部 ID 语义的字段名子串。
var idFieldSubstrings = []string{
	"imei", "imsi", "serial", "uuid", "guid", "snowflake", "gps", "device",
	"vehicle", "asset", "entity", "tenant", "order", "trace", "span", "batch",
	"session", "request", "transaction", "invoice", "shipment", "waybill",
}

// idFieldChineseMarkers 是中文 ID 标识。
var idFieldChineseMarkers = []string{
	"编号", "流水", "序列", "设备", "车辆", "资产", "实体", "商户", "运单", "订单",
	"批次", "单号", "工单",
}

// piiFieldNameWhitelist 是 PII 字段名白名单：即便字段名含 id，其值也必须上报。
var piiFieldNameWhitelist = map[string]struct{}{
	"idcard": {}, "identitycard": {}, "idcardno": {}, "idnumber": {},
	"id_no": {}, "idno": {}, "cardno": {}, "cardnumber": {}, "cardnum": {},
	"bankcard": {}, "bankcardno": {}, "bankcardnumber": {},
	"phone": {}, "mobile": {}, "tel": {}, "telephone": {}, "phonenumber": {},
	"mobileno": {}, "mobilenumber": {}, "shenfenzheng": {},
}

// knownInternalNumbers 是已知内部编号白名单（Luhn 恰好通过的资产/车辆 ID）。
var knownInternalNumbers = map[string]struct{}{
	"65116464360759296": {},
}

// bareCloudKeyFieldNames 是 Cloud Key 规则的裸字段名（无值时排除）。
var bareCloudKeyFieldNames = map[string]struct{}{
	"access_key_id": {}, "accesskeyid": {}, "access_key_secret": {},
	"accesskeysecret": {}, "access_key": {}, "accesskey": {},
}

// codeNoisePatterns 是代码/路径噪声特征。
var codeNoisePatterns = []string{
	"/src/main/resources", "line[len(", "os.getenv", "os.environ",
	"process.env", "system.getenv", "configuration.get", "viper.get",
	"config.get", "settings.",
}

// dlpExclusionResult 描述排除判定结果。Excluded 为 true 时 Reason 说明原因，
// 会写进审计日志便于事后核对规则是否过严。
type dlpExclusionResult struct {
	Excluded bool
	Reason   string
}

func excluded(reason string) dlpExclusionResult {
	return dlpExclusionResult{Excluded: true, Reason: reason}
}

var notExcluded = dlpExclusionResult{}

// applyExclusions 对一条命中执行完整排除链。
//
// text 是被扫描的完整原文，match 是命中片段，value 是规则声明的值部分
// （key=value 规则里的值；无捕获组时等于 match），start/end 是 match 在 text 中
// 的字节下标。
func applyExclusions(rule DLPRule, text, match, value string, start, end int) dlpExclusionResult {
	switch rule.Class {
	case DLPClassPII:
		return applyPIIExclusions(rule, text, match, start, end)
	case DLPClassCredential:
		return applyCredentialExclusions(rule, text, match, value, start)
	case DLPClassSensitive:
		return applySensitiveExclusions(rule, text, match, value, start)
	}
	return notExcluded
}

// applyPIIExclusions 实现 detection-rules.md「PII 通用排除」。
func applyPIIExclusions(rule DLPRule, text, match string, start, end int) dlpExclusionResult {
	if _, ok := knownInternalNumbers[match]; ok {
		return excluded("已知内部编号白名单")
	}
	// 边界紧邻字母数字：多为 token/编号片段。RE2 无 lookaround，按下标判断。
	if boundaryTouchesAlphanumeric(text, start, end) {
		return excluded("命中串前后紧邻字母或数字")
	}
	if precededByDot(text, start) {
		return excluded("命中串位于小数的小数部分")
	}
	if withinUserPath(text, start) {
		return excluded("命中串位于用户目录路径中")
	}
	if withinFilenameFragment(text, start, end) {
		return excluded("命中串为文件名中的时间戳或 ID 片段")
	}
	if hasRepeatedRun(match, 3) {
		return excluded("含 3 位以上连续相同数字，测试数据特征")
	}
	if rule.Validator == DLPValidatorPhone && countOccurrences(match, "123") >= 2 {
		return excluded("含多个 123 子串，测试号码特征")
	}
	// ID 字段判定：字段名语义为内部 ID 时排除；但 PII 字段名白名单强制上报。
	fieldName, hasField := precedingFieldName(text, start)
	if hasField && !isPIIFieldName(fieldName) && looksLikeIDField(fieldName) {
		return excluded("命中值挂在内部 ID 语义字段上：" + fieldName)
	}
	// 同值预扫描：同一数字串在别处以 ID 字段值出现过，则裸出现也视为 ID。
	if appearsAsIDFieldValue(text, match) {
		return excluded("同值在原文中以内部 ID 字段值出现")
	}
	return notExcluded
}

// applyCredentialExclusions 实现凭证泄露检测器的排除项。
func applyCredentialExclusions(rule DLPRule, text, match, value string, start int) dlpExclusionResult {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if _, ok := knownExampleValues[normalized]; ok {
		return excluded("命中值为厂商文档示例值")
	}
	if result := commonPlaceholderExclusions(normalized); result.Excluded {
		return result
	}
	switch rule.ID {
	case "credential-jwt":
		// URL 参数里的 JWT 多为图片签名 URL 的临时凭证，非用户泄露。
		if jwtInURLParam(text, start) {
			return excluded("JWT 出现在 URL 查询参数中")
		}
	case "credential-generic-api-key":
		// 真实密钥几乎都含数字。
		if !hasDigit(value) {
			return excluded("命中值不含任何数字")
		}
		if _, ok := literalSecretWords[normalized]; ok {
			return excluded("命中值为字面量词而非真实密钥")
		}
		if isSingleWordLabel(value) {
			return excluded("命中值为无数字的单词标签")
		}
		if containsCodeNoise(text) && !hasDigit(value) {
			return excluded("命中位于代码引用上下文")
		}
	case "credential-db-connection-string":
		// jdbc:mysql://... 里内嵌的 mysql://user:pass@host 会同时命中本规则，
		// 但 JDBC 有自己的专属排除（host:port 字面占位）。让更具体的 JDBC 规则
		// 接管，否则 `jdbc:mysql://user:pass@host:port/db` 这种文档占位串会从
		// 本规则漏报出去。
		if precededByJDBCScheme(text, start) {
			return excluded("交由更具体的 JDBC 连接串规则处理")
		}
		if hostIsLocalOrPrivate(match) {
			return excluded("连接串指向本地或内网地址")
		}
	}
	return notExcluded
}

// applySensitiveExclusions 实现敏感信息检测器（云密钥/密码字段/JDBC）的排除项。
func applySensitiveExclusions(rule DLPRule, text, match, value string, start int) dlpExclusionResult {
	switch rule.ID {
	case "sensitive-cloud-key-field":
		// 裸字段名（无值）已由规则正则要求必须带值来排除，这里只过滤占位符值。
		normalized := strings.ToLower(strings.TrimSpace(value))
		if _, ok := emptyValueTokens[normalized]; ok {
			return excluded("云密钥字段值为占位或空值")
		}
		if _, ok := literalSecretWords[normalized]; ok {
			return excluded("云密钥字段值为字面量词")
		}
		if result := commonPlaceholderExclusions(normalized); result.Excluded {
			return result
		}
	case "sensitive-cloud-key-prefix":
		normalized := strings.ToLower(strings.TrimSpace(match))
		if result := commonPlaceholderExclusions(normalized); result.Excluded {
			return result
		}
	case "sensitive-password-field":
		normalized := strings.ToLower(strings.TrimSpace(value))
		if _, ok := emptyValueTokens[normalized]; ok {
			return excluded("密码字段值为占位或空值")
		}
		for _, prefix := range emptyValuePrefixes {
			if strings.HasPrefix(normalized, prefix) {
				return excluded("密码字段值为模板变量或占位符")
			}
		}
		if _, ok := knownTestPasswords[normalized]; ok {
			return excluded("命中值为已知测试口令")
		}
		if _, ok := literalSecretWords[normalized]; ok {
			return excluded("命中值为字面量词而非真实口令")
		}
		if isSingleWordLabel(value) {
			return excluded("命中值为无数字的单词标签")
		}
		if isAllDigits(value) && len(value) < 12 {
			return excluded("命中值为 12 位以下纯数字，多为 ID 或编码")
		}
		if isAllChinese(value) {
			return excluded("命中值仅含中文，非口令")
		}
		if containsCodeNoise(text) {
			return excluded("命中位于代码或路径噪声上下文")
		}
		if !keywordIsStandaloneComponent(text, start) {
			return excluded("关键词粘连在更大单词中（如 donkey/accessory）")
		}
		if result := commonPlaceholderExclusions(normalized); result.Excluded {
			return result
		}
	case "sensitive-jdbc-connection":
		if strings.Contains(strings.ToLower(match), "host:port") {
			return excluded("JDBC 连接串含 host:port 字面占位")
		}
		if hostIsLocalOrPrivate(match) {
			return excluded("JDBC 连接串指向本地或内网地址")
		}
	}
	return notExcluded
}

// commonPlaceholderExclusions 是占位符/假值的共用判定。
func commonPlaceholderExclusions(normalizedValue string) dlpExclusionResult {
	for _, prefix := range placeholderPrefixes {
		if strings.HasPrefix(normalizedValue, prefix) {
			return excluded("命中值带占位符前缀：" + prefix)
		}
	}
	for _, fragment := range fakeValueSubstrings {
		if strings.Contains(normalizedValue, fragment) {
			return excluded("命中值含假值特征子串：" + fragment)
		}
	}
	if strings.Contains(normalizedValue, "example") {
		return excluded("命中值含 example 示例特征")
	}
	return notExcluded
}
