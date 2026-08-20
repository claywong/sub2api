// prompt_dlp_validators.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 的算法校验器。
//
// 正则只能定位"形状像"的数字串，真正判定靠这里的算法校验，这是把 PII 误报压下来
// 的关键一层（detection-rules.md 第二节）：
//   - 身份证：末位校验码（加权求和 + 模 11 映射）+ 第 7-14 位生日合法性
//   - 银行卡：Luhn 校验 + BIN 前缀（4/5/6 或老式银联 6 位 BIN 白名单）
//   - 手机号：前 3 位真实号段白名单
//
// 与 upstream 合并策略：
//   - 纯新增文件，无 upstream 符号改动，merge 时不会冲突。
//
// =============================================================================
package securityaudit

import (
	"strconv"
	"strings"
	"time"
)

// idCardWeights 是身份证前 17 位的加权因子。
var idCardWeights = [17]int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}

// idCardCheckCodes 是模 11 结果到校验码的映射。
var idCardCheckCodes = [11]byte{'1', '0', 'X', '9', '8', '7', '6', '5', '4', '3', '2'}

// unionPayLegacyBINs 是老式银联卡的 6 位 BIN 白名单。这些卡首位不是 4/5/6，
// 但确实是真实银行卡，需要单独放行。
var unionPayLegacyBINs = map[string]struct{}{
	"955880": {}, "955881": {}, "955882": {},
}

// validateIDCard 校验 18 位身份证号：先验生日合法性，再验末位校验码。
func validateIDCard(value string) bool {
	if len(value) != 18 {
		return false
	}
	for index := 0; index < 17; index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	if !validIDCardBirthday(value[6:14]) {
		return false
	}
	sum := 0
	for index := 0; index < 17; index++ {
		sum += int(value[index]-'0') * idCardWeights[index]
	}
	expected := idCardCheckCodes[sum%11]
	actual := value[17]
	if actual == 'x' {
		actual = 'X'
	}
	return actual == expected
}

// validIDCardBirthday 校验 YYYYMMDD 是否为合法日期，年份限定 1900~当前年。
// 用 time.Parse 天然覆盖闰年与各月天数。
func validIDCardBirthday(value string) bool {
	if len(value) != 8 {
		return false
	}
	parsed, err := time.Parse("20060102", value)
	if err != nil {
		return false
	}
	year := parsed.Year()
	if year < 1900 || year > time.Now().UTC().Year() {
		return false
	}
	return true
}

// validateBankCard 校验 16-19 位银行卡号：Luhn 通过且 BIN 前缀可信。
func validateBankCard(value string) bool {
	if len(value) < 16 || len(value) > 19 {
		return false
	}
	if !isAllDigits(value) {
		return false
	}
	if !luhnValid(value) {
		return false
	}
	if len(value) >= 6 {
		if _, ok := unionPayLegacyBINs[value[:6]]; ok {
			return true
		}
	}
	switch value[0] {
	case '4', '5', '6':
		return true
	default:
		return false
	}
}

// luhnValid 执行标准 Luhn 校验。
func luhnValid(value string) bool {
	sum := 0
	double := false
	for index := len(value) - 1; index >= 0; index-- {
		digit := int(value[index] - '0')
		if double {
			digit *= 2
			if digit > 9 {
				digit -= 9
			}
		}
		sum += digit
		double = !double
	}
	return sum%10 == 0
}

// phoneSegmentRanges 描述真实手机号段。每项为闭区间 [low, high]，
// 对应 detection-rules.md 的号段白名单（154 数据卡已排除）。
var phoneSegmentRanges = [][2]int{
	{130, 139},
	{145, 149},
	{150, 153}, {155, 159},
	{162, 162}, {165, 167},
	{170, 178},
	{180, 189},
	{190, 193}, {195, 199},
}

// validatePhone 校验 11 位手机号的前 3 位是否落在真实号段白名单内。
func validatePhone(value string) bool {
	if len(value) != 11 || !isAllDigits(value) {
		return false
	}
	prefix, err := strconv.Atoi(value[:3])
	if err != nil {
		return false
	}
	for _, span := range phoneSegmentRanges {
		if prefix >= span[0] && prefix <= span[1] {
			return true
		}
	}
	return false
}

// runValidator 按规则声明的校验器类型执行校验。无校验器的规则直接通过。
func runValidator(kind DLPValidatorKind, value string) bool {
	switch kind {
	case DLPValidatorIDCard:
		return validateIDCard(value)
	case DLPValidatorBankCard:
		return validateBankCard(value)
	case DLPValidatorPhone:
		return validatePhone(value)
	default:
		return true
	}
}

// isAllDigits 判断字符串是否全为 ASCII 数字。
func isAllDigits(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

// hasDigit 判断字符串是否含至少一个 ASCII 数字。
// 排除链用它实现"真实密钥几乎都含数字"这条规则。
func hasDigit(value string) bool {
	for index := 0; index < len(value); index++ {
		if value[index] >= '0' && value[index] <= '9' {
			return true
		}
	}
	return false
}

// hasRepeatedRun 判断字符串是否含长度 >= n 的连续相同字符。
// 用于识别 13800000000 这类测试数据。
func hasRepeatedRun(value string, n int) bool {
	if n <= 1 || len(value) < n {
		return false
	}
	run := 1
	for index := 1; index < len(value); index++ {
		if value[index] == value[index-1] {
			run++
			if run >= n {
				return true
			}
			continue
		}
		run = 1
	}
	return false
}

// countOccurrences 统计 substr 在 value 中的出现次数（含重叠计数不需要，故用标准计数）。
func countOccurrences(value, substr string) int {
	return strings.Count(value, substr)
}
