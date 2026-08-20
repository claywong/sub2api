package securityaudit

import "testing"

func TestDLPValidateIDCard(t *testing.T) {
	valid := []string{
		"110101199003072316", // 北京，1990-03-07
		"440301198512015611", // 深圳，1985-12-01
		"310101199208151234", // 上海，1992-08-15
	}
	for _, value := range valid {
		if !validateIDCard(value) {
			t.Errorf("validateIDCard(%q) = false, 期望 true", value)
		}
	}

	invalid := map[string]string{
		"11010119900307231 ": "长度含空格",
		"110101199003072311": "校验码错误",
		"110101199002307231": "2 月 30 日不存在",
		"110101189012017231": "年份早于 1900",
		"11010129990307231X": "年份晚于当前年",
		"1101011990030723":   "长度不足",
		"11010119900307231Y": "校验位非法字符",
	}
	for value, reason := range invalid {
		if validateIDCard(value) {
			t.Errorf("validateIDCard(%q) = true, 期望 false（%s）", value, reason)
		}
	}
}

// makeIDCard 给 17 位前缀补上正确校验位，返回完整 18 位身份证号。
// 测试内自行计算，避免硬编码的样本数据本身算错校验位。
func makeIDCard(prefix17 string) string {
	if len(prefix17) != 17 {
		return ""
	}
	sum := 0
	for index := 0; index < 17; index++ {
		sum += int(prefix17[index]-'0') * idCardWeights[index]
	}
	return prefix17 + string(idCardCheckCodes[sum%11])
}

// findIDCardWithCheckCode 枚举 3 位顺序码，找出校验位等于 want 的身份证号。
func findIDCardWithCheckCode(t *testing.T, areaBirth string, want byte) string {
	t.Helper()
	for seq := 0; seq < 1000; seq++ {
		candidate := makeIDCard(areaBirth + padSeq(seq))
		if candidate != "" && candidate[17] == want {
			return candidate
		}
	}
	t.Fatalf("找不到校验位为 %c 的身份证号（前缀 %s）", want, areaBirth)
	return ""
}

func padSeq(seq int) string {
	digits := []byte{byte('0' + seq/100%10), byte('0' + seq/10%10), byte('0' + seq%10)}
	return string(digits)
}

func TestDLPValidateIDCardCheckCodeX(t *testing.T) {
	// 校验位为 X 的号码必须被接受，且大小写 x 等价。
	card := findIDCardWithCheckCode(t, "11010119900307", 'X')
	if !validateIDCard(card) {
		t.Errorf("validateIDCard(%q) = false, 校验位 X 应被接受", card)
	}
	lower := card[:17] + "x"
	if !validateIDCard(lower) {
		t.Errorf("validateIDCard(%q) = false, 小写 x 校验位应被接受", lower)
	}
}

func TestDLPValidateIDCardLeapYear(t *testing.T) {
	// 2000-02-29 是合法闰日，2001-02-29 不是。仅验证生日部分逻辑。
	if !validIDCardBirthday("20000229") {
		t.Error("2000-02-29 应为合法生日")
	}
	if validIDCardBirthday("20010229") {
		t.Error("2001-02-29 应为非法生日")
	}
}

func TestDLPValidateBankCard(t *testing.T) {
	valid := []string{
		"4111111111111111", // Visa 测试卡号，Luhn 通过
		"5500005555555559", // MasterCard
		"6212345678901232", // 银联，首位 6
	}
	for _, value := range valid {
		if !validateBankCard(value) {
			t.Errorf("validateBankCard(%q) = false, 期望 true", value)
		}
	}

	invalid := map[string]string{
		"4111111111111112":     "Luhn 校验失败",
		"1234567890123456":     "BIN 前缀不可信",
		"411111111111111":      "长度不足 16",
		"41111111111111111111": "长度超过 19",
		"411a111111111111":     "含非数字",
	}
	for value, reason := range invalid {
		if validateBankCard(value) {
			t.Errorf("validateBankCard(%q) = true, 期望 false（%s）", value, reason)
		}
	}
}

func TestDLPValidateBankCardLegacyUnionPayBIN(t *testing.T) {
	// 老式银联 BIN 首位是 9，不在 4/5/6 白名单里，必须靠 BIN 白名单放行。
	// 构造一个 955880 开头且 Luhn 通过的卡号。
	card := makeLuhnValid("955880123456789")
	if card == "" {
		t.Fatal("构造 Luhn 合法卡号失败")
	}
	if !validateBankCard(card) {
		t.Errorf("validateBankCard(%q) = false, 老式银联 BIN 应放行", card)
	}
}

// makeLuhnValid 给 prefix 追加一位校验位使其 Luhn 通过，返回完整卡号。
func makeLuhnValid(prefix string) string {
	for digit := byte('0'); digit <= '9'; digit++ {
		candidate := prefix + string(digit)
		if luhnValid(candidate) {
			return candidate
		}
	}
	return ""
}

func TestDLPValidatePhone(t *testing.T) {
	valid := []string{
		"13912345678", "14512345678", "15012345678", "15512345678",
		"16212345678", "16512345678", "17012345678", "18812345678",
		"19012345678", "19912345678",
	}
	for _, value := range valid {
		if !validatePhone(value) {
			t.Errorf("validatePhone(%q) = false, 期望 true", value)
		}
	}

	invalid := map[string]string{
		"15412345678":  "154 为数据卡号段，已排除",
		"16112345678":  "161 非真实号段",
		"16312345678":  "163 非真实号段",
		"19412345678":  "194 非真实号段",
		"12912345678":  "129 非真实号段",
		"1391234567":   "长度不足 11",
		"139123456789": "长度超过 11",
		"1391234567a":  "含非数字",
	}
	for value, reason := range invalid {
		if validatePhone(value) {
			t.Errorf("validatePhone(%q) = true, 期望 false（%s）", value, reason)
		}
	}
}

func TestDLPHasRepeatedRun(t *testing.T) {
	cases := []struct {
		value string
		n     int
		want  bool
	}{
		{"13800000000", 3, true},
		{"13912345678", 3, false},
		{"11123456789", 3, true},
		{"11223344556", 3, false},
		{"abc", 1, false},
		{"", 3, false},
	}
	for _, item := range cases {
		if got := hasRepeatedRun(item.value, item.n); got != item.want {
			t.Errorf("hasRepeatedRun(%q, %d) = %v, 期望 %v", item.value, item.n, got, item.want)
		}
	}
}

func TestDLPHasDigit(t *testing.T) {
	if hasDigit("super-secret-value") {
		t.Error("纯字母与连字符不应判定为含数字")
	}
	if !hasDigit("secret123") {
		t.Error("含数字应判定为 true")
	}
}

func TestDLPRunValidatorNoneAlwaysPasses(t *testing.T) {
	if !runValidator(DLPValidatorNone, "anything") {
		t.Error("无校验器的规则应直接通过")
	}
}
