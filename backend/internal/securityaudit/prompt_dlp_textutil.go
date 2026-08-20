// prompt_dlp_textutil.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 排除链依赖的文本位置判断工具。
//
// 这些函数都是纯函数，按字节下标在原文上做上下文判断。之所以不写成正则，是因为
// Go 的 regexp 是 RE2，不支持 lookahead/lookbehind，"命中串前后紧邻什么字符"
// 这类判断只能在代码里按下标做。
//
// 与 upstream 合并策略：
//   - 纯新增文件，无 upstream 符号改动，merge 时不会冲突。
//
// =============================================================================
package securityaudit

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// boundaryTouchesAlphanumeric 判断命中区间 [start,end) 的前后是否紧邻 ASCII 字母或数字。
// 紧邻则说明命中串只是更长 token 的一部分（如 UUID 片段、编号），应排除。
//
// 只看 ASCII 是刻意的：这条规则的目的是识别「命中串嵌在更长的 ASCII token 里」。
// 若把 CJK 也算进来，"我的手机号是13912345678" 会因为前一个字符 '是' 是
// unicode.IsLetter 而被误排除——而这恰恰是中文语境下最典型的真实泄露场景。
func boundaryTouchesAlphanumeric(text string, start, end int) bool {
	if start > 0 {
		if isASCIIAlphanumeric(text[start-1]) {
			return true
		}
	}
	if end < len(text) {
		if isASCIIAlphanumeric(text[end]) {
			return true
		}
	}
	return false
}

// isASCIIAlphanumeric 判断单字节是否为 ASCII 字母或数字。
func isASCIIAlphanumeric(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	default:
		return false
	}
}

// precededByDot 判断命中串前一个字符是否为小数点，用于排除浮点数的小数部分。
func precededByDot(text string, start int) bool {
	if start <= 0 {
		return false
	}
	return text[start-1] == '.'
}

// userPathPrefixes 是形似手机号的系统用户名路径前缀。
var userPathPrefixes = []string{`C:\Users\`, `c:\users\`, "/c/Users/", "/Users/", "/home/"}

// withinUserPath 判断命中串是否位于用户目录路径中。
// 只回看命中前的一小段窗口，避免整篇原文里出现过路径就误排除。
func withinUserPath(text string, start int) bool {
	windowStart := start - 64
	if windowStart < 0 {
		windowStart = 0
	}
	window := text[windowStart:start]
	for _, prefix := range userPathPrefixes {
		if strings.Contains(window, prefix) {
			return true
		}
		if strings.Contains(strings.ToLower(window), strings.ToLower(prefix)) {
			return true
		}
	}
	return false
}

// withinFilenameFragment 判断命中串是否为文件名中的时间戳/ID 片段，
// 特征是前面紧跟 _ 或 -，后面紧跟 .扩展名。
func withinFilenameFragment(text string, start, end int) bool {
	if start == 0 {
		return false
	}
	prev := text[start-1]
	if prev != '_' && prev != '-' {
		return false
	}
	rest := text[end:]
	if !strings.HasPrefix(rest, ".") || len(rest) < 2 {
		return false
	}
	for index := 1; index < len(rest); index++ {
		c := rest[index]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			continue
		}
		// 扩展名至少要有一个字母才算文件名
		return index > 1
	}
	return len(rest) > 1
}

// precedingFieldName 提取命中串左侧的字段名，形如 `"phone": 139...`
// 或 `order_no=139...`。返回归一化后的字段名与是否找到。
func precedingFieldName(text string, start int) (string, bool) {
	windowStart := start - 96
	if windowStart < 0 {
		windowStart = 0
	}
	window := text[windowStart:start]
	// 从右往左找最近的键值分隔符。除 ASCII 的 : = 外还要认全角冒号：，
	// 否则「设备编号：13912345678」这类中文字段名无法识别。
	separator := -1
	for index := len(window) - 1; index >= 0; index-- {
		c := window[index]
		if c == ':' || c == '=' {
			separator = index
			break
		}
		// 全角冒号 U+FF1A 的 UTF-8 编码为 EF BC 9A
		if c == 0x9A && index >= 2 && window[index-2] == 0xEF && window[index-1] == 0xBC {
			separator = index - 2
			break
		}
		// 遇到明显的分隔符就停，说明命中串不是某个字段的值
		if c == ',' || c == ';' || c == '{' || c == '\n' || c == '[' {
			return "", false
		}
	}
	if separator < 0 {
		return "", false
	}
	// 分隔符左侧提取标识符字符
	nameEnd := separator
	for nameEnd > 0 && isFieldNameNoise(window[nameEnd-1]) {
		nameEnd--
	}
	nameStart := nameEnd
	for nameStart > 0 && isFieldNameChar(window[nameStart-1]) {
		nameStart--
	}
	if nameStart >= nameEnd {
		return "", false
	}
	return normalizeFieldName(window[nameStart:nameEnd]), true
}

func isFieldNameChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_' || c == '-' || c == '.':
		return true
	default:
		// 允许中文字段名
		return c >= 0x80
	}
}

func isFieldNameNoise(c byte) bool {
	return c == ' ' || c == '"' || c == '\'' || c == '\t'
}

// normalizeFieldName 归一化字段名：转小写并去掉连字符/空格/点。
func normalizeFieldName(value string) string {
	replacer := strings.NewReplacer("-", "_", " ", "_", ".", "_", `"`, "", "'", "")
	return strings.ToLower(strings.TrimSpace(replacer.Replace(value)))
}

// isPIIFieldName 判断字段名是否在 PII 白名单里（即便含 id 也必须上报）。
func isPIIFieldName(fieldName string) bool {
	compact := strings.ReplaceAll(fieldName, "_", "")
	if _, ok := piiFieldNameWhitelist[fieldName]; ok {
		return true
	}
	_, ok := piiFieldNameWhitelist[compact]
	return ok
}

// looksLikeIDField 判断字段名是否为内部 ID 语义。
func looksLikeIDField(fieldName string) bool {
	compact := strings.ReplaceAll(fieldName, "_", "")
	if strings.HasSuffix(fieldName, "id") || strings.HasSuffix(compact, "id") {
		return true
	}
	if strings.HasSuffix(fieldName, "_no") || strings.HasSuffix(compact, "no") {
		return true
	}
	for _, fragment := range idFieldSubstrings {
		if strings.Contains(compact, fragment) {
			return true
		}
	}
	for _, marker := range idFieldChineseMarkers {
		if strings.Contains(fieldName, marker) {
			return true
		}
	}
	return false
}

// appearsAsIDFieldValue 预扫描：同一数字串在原文别处以 ID 字段值出现过。
// 用于抑制"同值裸出现"的误报（detection-rules.md 的 ID 字段值预扫描）。
func appearsAsIDFieldValue(text, match string) bool {
	if match == "" {
		return false
	}
	searchFrom := 0
	for {
		index := strings.Index(text[searchFrom:], match)
		if index < 0 {
			return false
		}
		absolute := searchFrom + index
		if fieldName, ok := precedingFieldName(text, absolute); ok {
			if !isPIIFieldName(fieldName) && looksLikeIDField(fieldName) {
				return true
			}
		}
		searchFrom = absolute + len(match)
		if searchFrom >= len(text) {
			return false
		}
	}
}

// precededByJDBCScheme 判断命中串是否紧跟在 jdbc: 之后，
// 即它其实是 JDBC 连接串的一部分。
func precededByJDBCScheme(text string, start int) bool {
	const scheme = "jdbc:"
	if start < len(scheme) {
		return false
	}
	return strings.EqualFold(text[start-len(scheme):start], scheme)
}

// jwtInURLParam 判断 JWT 是否作为 URL 查询参数出现（?jwt= 或 &jwt=）。
func jwtInURLParam(text string, start int) bool {
	windowStart := start - 16
	if windowStart < 0 {
		windowStart = 0
	}
	window := strings.ToLower(text[windowStart:start])
	return strings.Contains(window, "?jwt=") || strings.Contains(window, "&jwt=") ||
		strings.Contains(window, "?token=") || strings.Contains(window, "&token=")
}

// isSingleWordLabel 判断值是否为「无数字的单词标签」，
// 如 pick_finish / cargo / org_name —— 是字段含义名而非密钥值。
func isSingleWordLabel(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || hasDigit(trimmed) {
		return false
	}
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r == '_' || r == '-' || r == '.':
		default:
			return false
		}
	}
	return true
}

// isAllChinese 判断值是否仅含中文字符（可含空白）。
func isAllChinese(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}
	for _, r := range trimmed {
		if unicode.IsSpace(r) {
			continue
		}
		if !unicode.Is(unicode.Han, r) {
			return false
		}
	}
	return true
}

// containsCodeNoise 判断原文是否含代码/路径噪声特征。
func containsCodeNoise(text string) bool {
	lower := strings.ToLower(text)
	for _, pattern := range codeNoisePatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// keywordIsStandaloneComponent 校验密码关键词是否为独立组件，
// 排除 donkey / accessory 这类把 pass/key 粘在更大单词里的子串误匹配。
func keywordIsStandaloneComponent(text string, start int) bool {
	if start == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(text[:start])
	if r == utf8.RuneError {
		return true
	}
	// 前一个字符是字母则说明关键词粘在更大单词里
	return !unicode.IsLetter(r)
}

// hostIsLocalOrPrivate 判断连接串里的主机是否为本地或内网地址。
func hostIsLocalOrPrivate(connection string) bool {
	host := extractConnectionHost(connection)
	if host == "" {
		return false
	}
	host = strings.ToLower(host)
	if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "0.0.0.0" {
		return true
	}
	if strings.HasPrefix(host, "127.") || strings.HasPrefix(host, "10.") ||
		strings.HasPrefix(host, "192.168.") {
		return true
	}
	if strings.HasPrefix(host, "172.") {
		return isPrivate172(host)
	}
	if strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") ||
		strings.HasSuffix(host, ".localdomain") {
		return true
	}
	return false
}

// isPrivate172 判断 172.16.0.0/12 私网段。
func isPrivate172(host string) bool {
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return false
	}
	second := 0
	for index := 0; index < len(parts[1]); index++ {
		c := parts[1][index]
		if c < '0' || c > '9' {
			return false
		}
		second = second*10 + int(c-'0')
	}
	return second >= 16 && second <= 31
}

// extractConnectionHost 从 scheme://user:pass@host:port/db 里取出 host。
func extractConnectionHost(connection string) string {
	at := strings.LastIndex(connection, "@")
	if at < 0 || at+1 >= len(connection) {
		return ""
	}
	rest := connection[at+1:]
	if index := strings.IndexAny(rest, ":/?"); index >= 0 {
		rest = rest[:index]
	}
	return rest
}
