// prompt_snapshot_toolresult.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：工具调用结果的提取。
//
// 背景：upstream 的提取器只放行 text / input_text / output_text 这几种 content
// block，导致以下工具结果被静默跳过，攻击者把敏感内容塞进工具结果即可绕过检测：
//
//	协议            工具结果位置                                      upstream 状态
//	OpenAI Chat     role="tool" 消息的 content（纯字符串）              已覆盖
//	Anthropic       role="user" 消息内 type="tool_result" block         跳过
//	Gemini          parts[].functionResponse.response                  跳过
//	Responses       type="function_call_output" item 的 output          跳过
//
// 本文件补齐后三种，并解决一个连带问题：blocking 模式下
// blockingSegmentsLatestUserAndPreviousOutput 会把扫描范围收窄到「最近的 user 轮
// + 前一轮 assistant 输出」，而 isUserSegment 不认 role="tool"，导致 OpenAI Chat
// 的工具结果即便提取出来也会被排除在同步扫描之外。trailingToolSegments 负责把它
// 补回当前轮。
//
// 与 upstream 合并策略：
//   - 提取逻辑全部放本文件，upstream 侧仅 4 处 2~4 行的 hook。
//
// =============================================================================
package securityaudit

import "strings"

// maxToolResultWalkDepth 限制在工具结果 JSON 里递归的深度。
// 工具返回的 response 是任意结构，没有深度上限会被恶意深层嵌套拖慢热路径。
const maxToolResultWalkDepth = 6

// anthropicToolResultTexts 提取 Anthropic 的 tool_result block 文本。
//
// tool_result 的 content 既可能是字符串，也可能是 block 数组：
//
//	{"type":"tool_result","tool_use_id":"x","content":"纯文本"}
//	{"type":"tool_result","tool_use_id":"x","content":[{"type":"text","text":"..."}]}
//
// 返回空切片表示该 block 不是 tool_result，调用方应继续原有逻辑。
func anthropicToolResultTexts(typeName string, object map[string]any) []string {
	if typeName != "tool_result" {
		return nil
	}
	return collectToolPayloadTexts(object["content"], 0)
}

// geminiFunctionResponseTexts 提取 Gemini 的 functionResponse 文本。
//
//	{"functionResponse":{"name":"get_user","response":{"idcard":"..."}}}
//
// response 是任意 JSON，这里递归收集其中所有字符串值。
func geminiFunctionResponseTexts(part map[string]any) []string {
	raw, exists := part["functionResponse"]
	if !exists {
		// 兼容下划线写法：不同 SDK 对 Gemini 字段命名不一致。
		raw, exists = part["function_response"]
	}
	if !exists {
		return nil
	}
	response, ok := raw.(map[string]any)
	if !ok {
		return collectToolPayloadTexts(raw, 0)
	}
	if payload, exists := response["response"]; exists {
		return collectToolPayloadTexts(payload, 0)
	}
	return collectToolPayloadTexts(response, 0)
}

// responsesFunctionOutputTexts 提取 Responses API 的 function_call_output 文本。
//
//	{"type":"function_call_output","call_id":"x","output":"文本或 JSON 字符串"}
//
// 返回空切片表示该 item 不是工具输出，调用方应继续原有逻辑。
func responsesFunctionOutputTexts(entry map[string]any) []string {
	typeName := strings.ToLower(strings.TrimSpace(stringValue(entry["type"])))
	switch typeName {
	case "function_call_output", "tool_call_output", "computer_call_output",
		"local_shell_call_output", "custom_tool_call_output":
	default:
		return nil
	}
	if payload, exists := entry["output"]; exists {
		return collectToolPayloadTexts(payload, 0)
	}
	return nil
}

// collectToolPayloadTexts 递归收集任意 JSON 结构里的字符串值。
//
// 之所以要递归而不是只取固定字段：工具返回的结构完全由被调用方决定，敏感信息可能
// 出现在任意层级的任意键上。递归深度受 maxToolResultWalkDepth 限制。
func collectToolPayloadTexts(value any, depth int) []string {
	if depth > maxToolResultWalkDepth {
		return nil
	}
	switch typed := value.(type) {
	case string:
		if trimmed := strings.TrimSpace(typed); trimmed != "" {
			return []string{typed}
		}
		return nil
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			result = append(result, collectToolPayloadTexts(item, depth+1)...)
		}
		return result
	case map[string]any:
		return collectToolPayloadMapTexts(typed, depth)
	default:
		// 数字、布尔、nil 不可能承载本方案关心的敏感信息形态，忽略。
		return nil
	}
}

// collectToolPayloadMapTexts 处理 map 分支，按 key 排序保证提取顺序稳定可测。
func collectToolPayloadMapTexts(object map[string]any, depth int) []string {
	// 嵌套的 text/content 优先，让常见结构的提取结果更贴近原文顺序。
	result := make([]string, 0, len(object))
	for _, key := range sortedMapKeys(object) {
		if isToolPayloadNoiseKey(key) {
			continue
		}
		result = append(result, collectToolPayloadTexts(object[key], depth+1)...)
	}
	return result
}

// toolPayloadNoiseKeys 是无需扫描的元数据键。
//
// 跳过它们既省 CPU，也避免 tool_use_id 这类随机串触发正则误报。
var toolPayloadNoiseKeys = map[string]struct{}{
	"type": {}, "tool_use_id": {}, "tool_call_id": {}, "call_id": {}, "id": {},
	"name": {}, "role": {}, "is_error": {}, "index": {}, "status": {},
	"cache_control": {}, "mime_type": {}, "media_type": {},
}

func isToolPayloadNoiseKey(key string) bool {
	_, ok := toolPayloadNoiseKeys[strings.ToLower(strings.TrimSpace(key))]
	return ok
}

// sortedMapKeys 返回排序后的键，保证遍历顺序确定。
func sortedMapKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	// 键数量很小，插入排序足够且避免引入 sort 依赖带来的分配。
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// isAnthropicToolUseBlock 判断 content block 是否为 Anthropic 的工具调用。
// 该 block 的入参在 message 层由 messageToolArgumentTexts 提取。
func isAnthropicToolUseBlock(typeName string) bool {
	return typeName == "tool_use"
}

// messageToolArgumentTexts 从一条消息里提取全部工具入参。
//
// 统一在 message 层做而不是在 content block 层，是为了让入参片段能被标记成 tool
// 角色：blocking 收窄模式只保留「最近 user 轮 + 前一轮 assistant 输出」，排在最后
// 的工具调用若沿用 assistant 角色会被丢弃，标记成 tool 才能被 trailingToolSegments
// 补回当前轮。
//
// 覆盖两种形态：
//   - OpenAI Chat：message.tool_calls[].function.arguments
//   - Anthropic：message.content[].(type=tool_use).input
func messageToolArgumentTexts(message map[string]any) []string {
	result := openAIToolCallTexts(message)
	return append(result, anthropicToolUseTexts(message["content"])...)
}

// anthropicToolUseTexts 提取 Anthropic content 数组里全部 tool_use 的入参。
//
//	{"type":"tool_use","id":"t1","name":"reg","input":{"idcard":"..."}}
//
// 入参与工具结果同样是客户端可控内容，且会被原样转发给上游模型，因此也在 DLP
// 的检测范围内。
func anthropicToolUseTexts(content any) []string {
	blocks, ok := content.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(blocks))
	for _, item := range blocks {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if !isAnthropicToolUseBlock(strings.ToLower(stringValue(object["type"]))) {
			continue
		}
		result = append(result, collectToolPayloadTexts(object["input"], 0)...)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// openAIToolCallTexts 提取 OpenAI Chat 的 tool_calls 入参。
//
//	{"role":"assistant","tool_calls":[{"function":{"name":"reg","arguments":"{...}"}}]}
//
// arguments 是 JSON 字符串，刻意整串返回而不解析后取叶子值：保留 {"key":"value"}
// 的字段名上下文，key=value 类规则（密码字段、通用 Key）才能命中。
func openAIToolCallTexts(message map[string]any) []string {
	raw, exists := message["tool_calls"]
	if !exists {
		return nil
	}
	calls, ok := raw.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(calls))
	for _, item := range calls {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		function, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		result = append(result, collectToolPayloadTexts(function["arguments"], 0)...)
	}
	return result
}

// geminiFunctionCallTexts 提取 Gemini 的 functionCall 入参。
//
//	{"functionCall":{"name":"reg","args":{"idcard":"..."}}}
func geminiFunctionCallTexts(part map[string]any) []string {
	raw, exists := part["functionCall"]
	if !exists {
		raw, exists = part["function_call"]
	}
	if !exists {
		return nil
	}
	call, ok := raw.(map[string]any)
	if !ok {
		return collectToolPayloadTexts(raw, 0)
	}
	if args, exists := call["args"]; exists {
		return collectToolPayloadTexts(args, 0)
	}
	return nil
}

// responsesFunctionCallTexts 提取 Responses API 的 function_call 入参。
//
//	{"type":"function_call","call_id":"c1","name":"reg","arguments":"{...}"}
func responsesFunctionCallTexts(entry map[string]any) []string {
	typeName := strings.ToLower(strings.TrimSpace(stringValue(entry["type"])))
	switch typeName {
	case "function_call", "custom_tool_call", "local_shell_call", "computer_call":
	default:
		return nil
	}
	if arguments, exists := entry["arguments"]; exists {
		return collectToolPayloadTexts(arguments, 0)
	}
	if action, exists := entry["action"]; exists {
		// computer_call / local_shell_call 的入参在 action 下。
		return collectToolPayloadTexts(action, 0)
	}
	return nil
}

// isToolResultSegment 判断片段是否来自工具调用结果。
func isToolResultSegment(segment promptSegment) bool {
	role := strings.ToLower(strings.TrimSpace(segment.role))
	return role == "tool" || role == "function"
}

// trailingToolSegments 取出 from 位置之后的工具结果片段。
//
// 用于 blocking 模式：OpenAI Chat 的工具结果是独立的 role="tool" 消息，排在最近
// 的 user 消息之后，而 isUserSegment 不认这个角色。若不单独补回来，同步拦截模式下
// 工具结果永远扫不到。
func trailingToolSegments(segments []promptSegment, from int) []promptSegment {
	if from < 0 || from >= len(segments) {
		return nil
	}
	result := make([]promptSegment, 0, len(segments)-from)
	for _, segment := range segments[from:] {
		if isToolResultSegment(segment) {
			result = append(result, segment)
		}
	}
	return result
}
