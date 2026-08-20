package securityaudit

import (
	"encoding/json"
	"strings"
	"testing"
)

// 一个能被 DLP 正则命中的真实形态身份证号，用作"藏在工具结果里的敏感信息"。
const toolResultSecret = "110101199003072316"

// scanTextFor 走完整的快照提取链路，返回最终用于扫描的文本。
func scanTextFor(t *testing.T, protocol, body string, latestTurnOnly bool) string {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		t.Fatalf("测试用 body 不是合法 JSON: %v", err)
	}
	req := Request{Protocol: protocol, Body: []byte(body), Stage: "http"}
	snapshot, err := ExtractBlockingPromptSnapshot(req, latestTurnOnly)
	if err != nil {
		t.Fatalf("提取快照失败: %v", err)
	}
	return snapshot.ScanText
}

// assertToolSecretScanned 断言工具结果里的敏感信息进入了扫描文本，并能被 DLP 命中。
func assertToolSecretScanned(t *testing.T, protocol, body string) {
	t.Helper()
	for _, latestTurnOnly := range []bool{false, true} {
		scanText := scanTextFor(t, protocol, body, latestTurnOnly)
		mode := "全量扫描"
		if latestTurnOnly {
			mode = "blocking 收窄"
		}
		if !strings.Contains(scanText, toolResultSecret) {
			t.Errorf("[%s/%s] 工具结果里的敏感信息未进入扫描文本，实际=%q",
				protocol, mode, scanText)
			continue
		}
		result := ScanDLP(scanText, nil)
		if len(result.Findings) == 0 {
			t.Errorf("[%s/%s] 扫描文本含敏感信息但 DLP 未命中，排除原因=%v",
				protocol, mode, result.ExcludedReasons)
		}
	}
}

func TestToolResultOpenAIChatRoleTool(t *testing.T) {
	body := `{"model":"gpt-test","messages":[
		{"role":"user","content":"查一下这个用户"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"c1","type":"function","function":{"name":"get_user","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"{\"idcard\":\"` + toolResultSecret + `\"}"}
	]}`
	assertToolSecretScanned(t, "openai_chat", body)
}

func TestToolResultAnthropicToolResultBlock(t *testing.T) {
	// tool_result 嵌在 role="user" 消息的 content 数组里，content 为字符串。
	body := `{"model":"claude-test","messages":[
		{"role":"user","content":"查一下这个用户"},
		{"role":"assistant","content":[
			{"type":"tool_use","id":"t1","name":"get_user","input":{}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"t1","content":"身份证 ` + toolResultSecret + `"}]}
	]}`
	assertToolSecretScanned(t, "anthropic_messages", body)
}

func TestToolResultAnthropicToolResultBlockArray(t *testing.T) {
	// tool_result 的 content 是 block 数组的形态。
	body := `{"model":"claude-test","messages":[
		{"role":"user","content":"查一下"},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"t1","content":[
				{"type":"text","text":"身份证 ` + toolResultSecret + `"}]}]}
	]}`
	assertToolSecretScanned(t, "anthropic_messages", body)
}

func TestToolResultAnthropicToolResultAmongTextBlocks(t *testing.T) {
	// tool_result 与普通 text block 混排时，两者都应被提取。
	body := `{"model":"claude-test","messages":[
		{"role":"user","content":[
			{"type":"text","text":"帮我核对"},
			{"type":"tool_result","tool_use_id":"t1","content":"身份证 ` + toolResultSecret + `"}]}
	]}`
	scanText := scanTextFor(t, "anthropic_messages", body, true)
	if !strings.Contains(scanText, "帮我核对") {
		t.Errorf("普通 text block 应保留，实际=%q", scanText)
	}
	if !strings.Contains(scanText, toolResultSecret) {
		t.Errorf("tool_result 内容应被提取，实际=%q", scanText)
	}
}

func TestToolResultGeminiFunctionResponse(t *testing.T) {
	body := `{"contents":[
		{"role":"user","parts":[{"text":"查一下这个用户"}]},
		{"role":"user","parts":[{"functionResponse":{"name":"get_user",
			"response":{"idcard":"` + toolResultSecret + `"}}}]}
	]}`
	assertToolSecretScanned(t, "gemini", body)
}

func TestToolResultGeminiFunctionResponseSnakeCase(t *testing.T) {
	// 部分 SDK 用下划线写法。
	body := `{"contents":[
		{"role":"user","parts":[{"function_response":{"name":"get_user",
			"response":{"idcard":"` + toolResultSecret + `"}}}]}
	]}`
	assertToolSecretScanned(t, "gemini", body)
}

func TestToolResultGeminiNestedResponse(t *testing.T) {
	// 敏感信息藏在 response 的深层嵌套里。
	body := `{"contents":[
		{"role":"user","parts":[{"functionResponse":{"name":"q",
			"response":{"data":{"profile":{"cert":{"no":"` + toolResultSecret + `"}}}}}}]}
	]}`
	assertToolSecretScanned(t, "gemini", body)
}

func TestToolResultResponsesFunctionCallOutput(t *testing.T) {
	body := `{"model":"gpt-test","input":[
		{"role":"user","content":[{"type":"input_text","text":"查一下"}]},
		{"type":"function_call","call_id":"c1","name":"get_user","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1",
			"output":"{\"idcard\":\"` + toolResultSecret + `\"}"}
	]}`
	assertToolSecretScanned(t, "openai_responses", body)
}

func TestToolResultResponsesOtherOutputTypes(t *testing.T) {
	for _, outputType := range []string{
		"tool_call_output", "computer_call_output", "local_shell_call_output",
		"custom_tool_call_output",
	} {
		body := `{"model":"gpt-test","input":[
			{"type":"` + outputType + `","call_id":"c1",
				"output":"身份证 ` + toolResultSecret + `"}
		]}`
		scanText := scanTextFor(t, "openai_responses", body, true)
		if !strings.Contains(scanText, toolResultSecret) {
			t.Errorf("[%s] 工具输出未被提取，实际=%q", outputType, scanText)
		}
	}
}

// ---------- 工具入参（与工具结果同为客户端可控内容，且会转发给上游）----------

func TestToolArgumentsOpenAIChatToolCalls(t *testing.T) {
	body := `{"model":"gpt-test","messages":[
		{"role":"user","content":"帮我登记"},
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function",
			"function":{"name":"reg","arguments":"{\"idcard\":\"` + toolResultSecret + `\"}"}}]}
	]}`
	assertToolSecretScanned(t, "openai_chat", body)
}

func TestToolArgumentsAnthropicToolUse(t *testing.T) {
	body := `{"model":"claude-test","messages":[
		{"role":"user","content":"帮我登记"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"reg",
			"input":{"idcard":"` + toolResultSecret + `"}}]}
	]}`
	assertToolSecretScanned(t, "anthropic_messages", body)
}

func TestToolArgumentsGeminiFunctionCall(t *testing.T) {
	body := `{"contents":[
		{"role":"user","parts":[{"text":"帮我登记"}]},
		{"role":"user","parts":[{"functionCall":{"name":"reg",
			"args":{"idcard":"` + toolResultSecret + `"}}}]}
	]}`
	assertToolSecretScanned(t, "gemini", body)
}

func TestToolArgumentsGeminiFunctionCallSnakeCase(t *testing.T) {
	body := `{"contents":[
		{"role":"user","parts":[{"function_call":{"name":"reg",
			"args":{"idcard":"` + toolResultSecret + `"}}}]}
	]}`
	assertToolSecretScanned(t, "gemini", body)
}

func TestToolArgumentsResponsesFunctionCall(t *testing.T) {
	body := `{"model":"gpt-test","input":[
		{"role":"user","content":[{"type":"input_text","text":"帮我登记"}]},
		{"type":"function_call","call_id":"c1","name":"reg",
			"arguments":"{\"idcard\":\"` + toolResultSecret + `\"}"}
	]}`
	assertToolSecretScanned(t, "openai_responses", body)
}

func TestToolArgumentsPreserveFieldNameContext(t *testing.T) {
	// 入参里的 JSON 串必须整串保留，不能解析后只取叶子值——
	// 密码字段这类规则依赖 "key":"value" 的字段名上下文才能命中。
	body := `{"model":"gpt-test","messages":[
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function",
			"function":{"name":"conn","arguments":"{\"db_password\":\"Xk9#mQ2vL8nPz\"}"}}]}
	]}`
	scanText := scanTextFor(t, "openai_chat", body, true)
	if !strings.Contains(scanText, "db_password") {
		t.Errorf("入参应保留字段名上下文，实际扫描文本=%q", scanText)
	}
	result := ScanDLP(scanText, nil)
	if len(result.Findings) == 0 {
		t.Errorf("入参里的口令应被命中，排除原因=%v", result.ExcludedReasons)
	}
}

func TestToolArgumentsHelpersReturnNilForNonToolItems(t *testing.T) {
	if texts := anthropicToolUseTexts([]any{map[string]any{"type": "text", "text": "hi"}}); texts != nil {
		t.Errorf("普通 text block 不应被当成 tool_use，实际=%v", texts)
	}
	if texts := openAIToolCallTexts(map[string]any{"role": "user", "content": "hi"}); texts != nil {
		t.Errorf("无 tool_calls 的消息不应产出入参，实际=%v", texts)
	}
	if texts := geminiFunctionCallTexts(map[string]any{"text": "hi"}); texts != nil {
		t.Errorf("普通 text part 不应被当成 functionCall，实际=%v", texts)
	}
	if texts := responsesFunctionCallTexts(map[string]any{"type": "message"}); texts != nil {
		t.Errorf("普通 message item 不应被当成 function_call，实际=%v", texts)
	}
}

// ---------- 元数据键不应引入噪声 ----------

func TestToolResultSkipsMetadataKeys(t *testing.T) {
	// tool_use_id 这类随机串不应进入扫描文本，避免触发正则误报。
	body := `{"model":"claude-test","messages":[
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"toolu_01A09q90qw90lq917835lq9",
				"content":"结果正常"}]}
	]}`
	scanText := scanTextFor(t, "anthropic_messages", body, true)
	if strings.Contains(scanText, "toolu_01A09q90qw90lq917835lq9") {
		t.Errorf("tool_use_id 不应进入扫描文本，实际=%q", scanText)
	}
	if !strings.Contains(scanText, "结果正常") {
		t.Errorf("工具结果正文应被提取，实际=%q", scanText)
	}
}

func TestToolResultDepthLimit(t *testing.T) {
	// 超过深度上限的嵌套不再递归，防止恶意深层结构拖慢热路径。
	deep := map[string]any{"leaf": toolResultSecret}
	for index := 0; index < maxToolResultWalkDepth+4; index++ {
		deep = map[string]any{"nest": deep}
	}
	texts := collectToolPayloadTexts(deep, 0)
	for _, text := range texts {
		if strings.Contains(text, toolResultSecret) {
			t.Error("超出深度上限的内容不应被提取")
		}
	}
}

func TestToolResultIgnoresNonStringScalars(t *testing.T) {
	payload := map[string]any{"count": 42, "ok": true, "nothing": nil, "text": "有效内容"}
	texts := collectToolPayloadTexts(payload, 0)
	if len(texts) != 1 || texts[0] != "有效内容" {
		t.Errorf("只应提取字符串值，实际=%v", texts)
	}
}

func TestToolResultDeterministicOrder(t *testing.T) {
	payload := map[string]any{"b": "second", "a": "first", "c": "third"}
	for attempt := 0; attempt < 8; attempt++ {
		texts := collectToolPayloadTexts(payload, 0)
		if len(texts) != 3 || texts[0] != "first" || texts[1] != "second" || texts[2] != "third" {
			t.Fatalf("提取顺序应稳定（按键排序），实际=%v", texts)
		}
	}
}

// ---------- 非工具结果不受影响 ----------

func TestToolResultHelpersReturnNilForNonToolBlocks(t *testing.T) {
	if texts := anthropicToolResultTexts("text", map[string]any{"text": "hi"}); texts != nil {
		t.Errorf("普通 text block 不应被当成 tool_result，实际=%v", texts)
	}
	if texts := geminiFunctionResponseTexts(map[string]any{"text": "hi"}); texts != nil {
		t.Errorf("普通 text part 不应被当成 functionResponse，实际=%v", texts)
	}
	if texts := responsesFunctionOutputTexts(map[string]any{"type": "message"}); texts != nil {
		t.Errorf("普通 message item 不应被当成工具输出，实际=%v", texts)
	}
}

func TestToolResultTrailingSegments(t *testing.T) {
	segments := []promptSegment{
		{text: "user1", user: true, role: "user"},
		{text: "assistant1", role: "assistant"},
		{text: "tool1", role: "tool"},
		{text: "tool2", role: "function"},
	}
	trailing := trailingToolSegments(segments, 1)
	if len(trailing) != 2 {
		t.Fatalf("应取出 2 条工具片段，实际 %d 条", len(trailing))
	}
	if trailingToolSegments(segments, 99) != nil {
		t.Error("越界起点应返回 nil")
	}
	if len(trailingToolSegments(segments, 0)) != 2 {
		t.Error("从头开始也应只取工具片段")
	}
}

// ---------- 端到端：工具结果里的敏感信息触发拦截 ----------

func TestToolResultTriggersDLPBlockEndToEnd(t *testing.T) {
	body := `{"model":"gpt-test","messages":[
		{"role":"user","content":"查一下这个用户"},
		{"role":"tool","tool_call_id":"c1","content":"{\"idcard\":\"` + toolResultSecret + `\"}"}
	]}`
	scanText := scanTextFor(t, "openai_chat", body, true)
	result := ScanDLP(scanText, nil)
	if len(result.Findings) == 0 {
		t.Fatalf("工具结果里的身份证应被 DLP 命中，扫描文本=%q", scanText)
	}
	if got := HighestSeverity(result.Findings); got != RiskHigh {
		t.Errorf("身份证命中的严重度 = %s, 期望 high（应触发拦截）", got)
	}
}
