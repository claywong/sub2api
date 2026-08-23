package securityaudit

import (
	"strings"
	"testing"
)

// dlpScopeSecret 是各类片段里用来标记「这段进没进扫描范围」的哨兵值。
// 用真实形态的身份证号，这样既能验证片段是否进入 ScanText，也能验证 DLP 是否命中。
const dlpScopeSecret = "110101199003072316"

// dlpScopeScanText 走 DLP 专用提取链路，返回最终用于扫描的文本。
func dlpScopeScanText(t *testing.T, protocol, body string) string {
	t.Helper()
	req := Request{Protocol: protocol, Body: []byte(body), Stage: "http"}
	snapshot, err := ExtractDLPSnapshot(req)
	if err != nil {
		t.Fatalf("提取 DLP 快照失败: %v", err)
	}
	return snapshot.ScanText
}

// TestDLPScopeIncludesUserAndToolOutput 断言用户输入与工具输出进入扫描范围。
// 这两类是「本地敏感数据流出」的唯一两个入口，漏掉任一条都会造成真实漏检。
func TestDLPScopeIncludesUserAndToolOutput(t *testing.T) {
	cases := []struct {
		name     string
		protocol string
		body     string
	}{
		{
			name:     "用户输入",
			protocol: "openai_chat",
			body: `{"messages":[
				{"role":"user","content":"我的身份证号是 ` + dlpScopeSecret + `"}
			]}`,
		},
		{
			name:     "OpenAI 工具输出",
			protocol: "openai_chat",
			body: `{"messages":[
				{"role":"user","content":"读一下配置"},
				{"role":"tool","content":"idcard=` + dlpScopeSecret + `"}
			]}`,
		},
		{
			name:     "Anthropic 工具输出",
			protocol: "anthropic_messages",
			body: `{"messages":[
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"t1","content":"文件内容 ` + dlpScopeSecret + `"}
				]}
			]}`,
		},
		{
			name:     "Gemini 工具输出",
			protocol: "gemini",
			body: `{"contents":[
				{"role":"user","parts":[
					{"functionResponse":{"name":"read","response":{"idcard":"` + dlpScopeSecret + `"}}}
				]}
			]}`,
		},
		{
			name:     "Responses 工具输出",
			protocol: "openai_responses",
			body: `{"input":[
				{"type":"function_call_output","call_id":"c1","output":"读到 ` + dlpScopeSecret + `"}
			]}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			scanText := dlpScopeScanText(t, testCase.protocol, testCase.body)
			if !strings.Contains(scanText, dlpScopeSecret) {
				t.Fatalf("%s 未进入扫描范围，实际=%q", testCase.name, scanText)
			}
			if result := ScanDLP(scanText, nil); len(result.Findings) == 0 {
				t.Errorf("%s 进入了扫描范围但 DLP 未命中，排除原因=%v",
					testCase.name, result.ExcludedReasons)
			}
		})
	}
}

// TestDLPScopeExcludesNonDataSources 断言非本地数据源不进入扫描范围。
//
// 这是收窄的核心收益：system prompt 来自上游服务商、assistant 文本由模型生成、
// 工具入参也是模型生成的，三者都不是本地数据源。把它们排除掉才能让单请求从
// 190 万 rune 降到正常量级，也避免上游 system prompt 明文落进审计表。
func TestDLPScopeExcludesNonDataSources(t *testing.T) {
	cases := []struct {
		name     string
		protocol string
		body     string
	}{
		{
			name:     "system 提示词",
			protocol: "openai_chat",
			body: `{"messages":[
				{"role":"system","content":"内部凭据 ` + dlpScopeSecret + `"},
				{"role":"user","content":"你好"}
			]}`,
		},
		{
			name:     "Anthropic system 字段",
			protocol: "anthropic_messages",
			body: `{"system":"内部凭据 ` + dlpScopeSecret + `",
				"messages":[{"role":"user","content":"你好"}]}`,
		},
		{
			name:     "developer 提示词",
			protocol: "openai_chat",
			body: `{"messages":[
				{"role":"developer","content":"内部凭据 ` + dlpScopeSecret + `"},
				{"role":"user","content":"你好"}
			]}`,
		},
		{
			name:     "assistant 历史回复",
			protocol: "openai_chat",
			body: `{"messages":[
				{"role":"user","content":"你好"},
				{"role":"assistant","content":"记得那个号 ` + dlpScopeSecret + `"},
				{"role":"user","content":"继续"}
			]}`,
		},
		{
			name:     "OpenAI 工具入参",
			protocol: "openai_chat",
			body: `{"messages":[
				{"role":"user","content":"写文件"},
				{"role":"assistant","tool_calls":[
					{"id":"c1","type":"function","function":{"name":"write",
					 "arguments":"{\"content\":\"` + dlpScopeSecret + `\"}"}}
				]}
			]}`,
		},
		{
			name:     "Gemini 工具入参",
			protocol: "gemini",
			body: `{"contents":[
				{"role":"user","parts":[{"text":"写文件"}]},
				{"role":"model","parts":[
					{"functionCall":{"name":"write","args":{"content":"` + dlpScopeSecret + `"}}}
				]}
			]}`,
		},
		{
			name:     "Responses 工具入参",
			protocol: "openai_responses",
			body: `{"input":[
				{"role":"user","content":"写文件"},
				{"type":"function_call","call_id":"c1","name":"write",
				 "arguments":"{\"content\":\"` + dlpScopeSecret + `\"}"}
			]}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			scanText := dlpScopeScanText(t, testCase.protocol, testCase.body)
			if strings.Contains(scanText, dlpScopeSecret) {
				t.Errorf("%s 不应进入 DLP 扫描范围，实际=%q", testCase.name, scanText)
			}
		})
	}
}

// TestDLPScopeCoversAllHistoryTurns 收窄的是「角色」不是「轮次」。
// 早期实现刻意扫全量轮次以防漏检，这一点必须保留：敏感数据可能出现在任何一轮。
func TestDLPScopeCoversAllHistoryTurns(t *testing.T) {
	body := `{"messages":[
		{"role":"user","content":"第一轮 ` + dlpScopeSecret + `"},
		{"role":"assistant","content":"好的"},
		{"role":"user","content":"第二轮"},
		{"role":"assistant","content":"明白"},
		{"role":"user","content":"第三轮"}
	]}`
	scanText := dlpScopeScanText(t, "openai_chat", body)
	if !strings.Contains(scanText, dlpScopeSecret) {
		t.Errorf("历史轮次的用户输入应进入扫描范围，实际=%q", scanText)
	}
}

// TestDLPScopeFullPromptMatchesScanText 落库文本与扫描文本必须同源。
//
// 这是「具体风险看不到」的根因回归：早期 ScanText 不截断而 full_prompt 截到
// 65536，命中落在截断区外时，管理员在界面上看到的文本压根不含出问题的片段。
func TestDLPScopeFullPromptMatchesScanText(t *testing.T) {
	body := `{"messages":[
		{"role":"system","content":"系统提示不该出现"},
		{"role":"user","content":"我的身份证号是 ` + dlpScopeSecret + `"}
	]}`
	req := Request{Protocol: "openai_chat", Body: []byte(body), Stage: "http"}
	snapshot, err := ExtractDLPSnapshot(req)
	if err != nil {
		t.Fatalf("提取 DLP 快照失败: %v", err)
	}
	if !strings.Contains(snapshot.FullPrompt, dlpScopeSecret) {
		t.Errorf("落库文本应含命中片段，实际=%q", snapshot.FullPrompt)
	}
	if strings.Contains(snapshot.FullPrompt, "系统提示不该出现") {
		t.Errorf("落库文本不应含 system 提示词，实际=%q", snapshot.FullPrompt)
	}
}

// TestTrimRunesLeftKeepsTail TrimRunesLeft 保留尾部，方向与 upstream TrimRunes 相反。
func TestTrimRunesLeftKeepsTail(t *testing.T) {
	if got := TrimRunesLeft("中文测试内容", 3); got != "…试内容" {
		t.Errorf("应保留尾部 3 个 rune，实际=%q", got)
	}
	if got := TrimRunesLeft("短", 5); got != "短" {
		t.Errorf("未超长时应原样返回，实际=%q", got)
	}
	if got := TrimRunesLeft("内容", 0); got != "" {
		t.Errorf("limit 为 0 应返回空串，实际=%q", got)
	}
}
