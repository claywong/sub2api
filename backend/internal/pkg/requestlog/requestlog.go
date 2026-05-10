// Package requestlog 提供 request_logs 表的请求/响应精简工具。
//
// 设计目标：
//   - 请求体仅保留 messages 数组的最后一条（去掉历史和大型 system/tools）
//   - 响应体把流式 SSE 聚合为最终 content 数组（text + tool_use），剔除中间增量
//   - 流式与非流式接口对称，由调用方根据上下游协议选择
package requestlog

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// MaxSessionIDLen 与 DB schema 中 session_id varchar(256) 对齐。
const MaxSessionIDLen = 256

// NormalizeSessionID 规范化用于 request_logs.session_id 的字符串：
// 去除首尾空白；超过 MaxSessionIDLen 的 raw ID 直接截断到上限以避免 DB 写入失败。
func NormalizeSessionID(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) > MaxSessionIDLen {
		trimmed = trimmed[:MaxSessionIDLen]
	}
	return trimmed
}

// preserveImageKeys 是 SafeTruncateJSON 不会截断的字段名集合：
// 命中以下任意 parent key 后，整棵子树（含 base64 数据）原样保留。
// 覆盖 Anthropic（content[].source.data）、OpenAI（image_url.url）、
// Responses（input[].image / image_url）等多模态图片字段。
var preserveImageKeys = map[string]struct{}{
	"source":    {}, // Anthropic image content_block 的 source
	"image":     {}, // 通用图片对象
	"image_url": {}, // OpenAI / Responses 风格
	"url":       {}, // image_url.url 内层
	"data":      {}, // base64 数据字段
}

// SafeTruncateJSON 递归遍历 JSON，将所有超过 perStringMax 字节的 string 值替换为
// "<truncated: N bytes>" 占位符，保证返回值始终是合法 JSON。
// 仅在 perStringMax > 0 时启用截断；解析失败或 perStringMax<=0 时原样返回。
//
// 图片相关字段（preserveImageKeys 命中）会原样保留 base64 内容，不做截断。
// 该函数为 WriteRequestLog 在配置 max_body_bytes 触发兜底时使用，默认链路不会调用。
func SafeTruncateJSON(raw []byte, perStringMax int) []byte {
	if perStringMax <= 0 || len(raw) == 0 {
		return raw
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	truncated := truncateJSONValue(v, perStringMax, "")
	out, err := json.Marshal(truncated)
	if err != nil {
		return raw
	}
	return out
}

func truncateJSONValue(v any, perStringMax int, parentKey string) any {
	if _, preserve := preserveImageKeys[parentKey]; preserve {
		return v
	}
	switch x := v.(type) {
	case string:
		if len(x) > perStringMax {
			return fmt.Sprintf("<truncated: %d bytes>", len(x))
		}
		return x
	case []any:
		for i := range x {
			x[i] = truncateJSONValue(x[i], perStringMax, parentKey)
		}
		return x
	case map[string]any:
		for k, val := range x {
			x[k] = truncateJSONValue(val, perStringMax, k)
		}
		return x
	default:
		return v
	}
}

// SimplifiedRequest 是 request_body 落库的统一精简结构：
// 仅保留「这次推理的最小上下文」—— 用户最近的真实输入、上一轮工具调用、
// 当前请求所携带的工具结果。
// 跨 Anthropic / OpenAI ChatCompletions / OpenAI Responses 三协议共享同一形态。
type SimplifiedRequest struct {
	Model       string       `json:"model,omitempty"`
	UserInput   string       `json:"user_input,omitempty"`
	ToolUses    []ToolUse    `json:"tool_uses,omitempty"`
	ToolResults []ToolResult `json:"tool_results,omitempty"`
}

// ToolUse 描述一次工具调用：id 用于和 ToolResult.ToolUseID 对应；
// input 优先解析为对象（OpenAI 的 arguments 是 string-form JSON 时会还原），失败时退化为字符串。
type ToolUse struct {
	ID    string `json:"id,omitempty"`
	Name  string `json:"name"`
	Input any    `json:"input,omitempty"`
}

// ToolResult 描述工具回灌给模型的结果。
// content 类型不固定（Anthropic 可能是数组或字符串，OpenAI 一般是字符串），用 any 透传。
type ToolResult struct {
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"`
}

// extractTextFromContent 从 content（可能是 string 或 [{type,text}] 数组）抽出所有 text 块。
// 兼容 Anthropic content[].type=="text" 与 Responses input[].content[].type=="input_text"。
// 多个 text 块用 "\n" 拼接；无 text 返回 ""。
func extractTextFromContent(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	if !content.IsArray() {
		return ""
	}
	var sb strings.Builder
	for _, item := range content.Array() {
		t := item.Get("type").String()
		if t != "" && t != "text" && t != "input_text" {
			continue
		}
		txt := item.Get("text").String()
		if txt == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(txt)
	}
	return sb.String()
}

// parseRawAny 把 gjson.Result 还原为 any（map/slice/string/...）；JSON 解析失败时退化为字符串。
func parseRawAny(v gjson.Result) any {
	if !v.Exists() {
		return nil
	}
	var parsed any
	if err := json.Unmarshal([]byte(v.Raw), &parsed); err == nil {
		return parsed
	}
	return v.String()
}

// parseMaybeJSONString 处理 OpenAI 协议中 arguments 为 string-form JSON 的情况：
// 输入是字符串就先 Unmarshal；非字符串走 parseRawAny。
func parseMaybeJSONString(v gjson.Result) any {
	if !v.Exists() {
		return nil
	}
	if v.Type == gjson.String {
		s := v.String()
		var parsed any
		if err := json.Unmarshal([]byte(s), &parsed); err == nil {
			return parsed
		}
		return s
	}
	return parseRawAny(v)
}

// marshalSimplified 序列化 SimplifiedRequest；失败返回空串让上层 fallthrough。
func marshalSimplified(r SimplifiedRequest) string {
	b, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return string(b)
}

// SimplifyAnthropicRequest 提取 Anthropic /v1/messages 请求的最小上下文包：
//   - user_input：倒序回溯最近一条 role==user 且 content 含 text 的项
//   - tool_uses：最后一条 role==assistant 内的所有 type==tool_use 块
//   - tool_results：当前 last role==user 内的所有 type==tool_result 块
//
// 协议特征校验：检测到 ChatCompletions 形态（messages[].role=="tool" 或
// messages[].tool_calls）而无 Anthropic 形态（content[].type=="tool_use|tool_result"）
// 时返回空串，让 SimplifyRequestBody 把请求交给 ChatCompletions 分支。
// 不做长度截断；超长字段的兜底截断由 WriteRequestLog 根据 MaxBodyBytes 配置决定。
func SimplifyAnthropicRequest(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return ""
	}
	arr := messages.Array()
	if len(arr) == 0 {
		return ""
	}

	hasAnthropicFeature := false
	hasChatCompletionsFeature := false
	for _, m := range arr {
		if m.Get("role").String() == "tool" {
			hasChatCompletionsFeature = true
		}
		if m.Get("tool_calls").IsArray() {
			hasChatCompletionsFeature = true
		}
		if c := m.Get("content"); c.IsArray() {
			for _, ci := range c.Array() {
				t := ci.Get("type").String()
				if t == "tool_use" || t == "tool_result" {
					hasAnthropicFeature = true
				}
			}
		}
	}
	if hasChatCompletionsFeature && !hasAnthropicFeature {
		return ""
	}

	out := SimplifiedRequest{Model: gjson.GetBytes(body, "model").String()}

	lastUserIdx := -1
	for i := len(arr) - 1; i >= 0; i-- {
		if arr[i].Get("role").String() == "user" {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx >= 0 {
		if c := arr[lastUserIdx].Get("content"); c.IsArray() {
			for _, ci := range c.Array() {
				if ci.Get("type").String() == "tool_result" {
					out.ToolResults = append(out.ToolResults, ToolResult{
						ToolUseID: ci.Get("tool_use_id").String(),
						Content:   parseRawAny(ci.Get("content")),
					})
				}
			}
		}
	}

	for i := len(arr) - 1; i >= 0; i-- {
		if arr[i].Get("role").String() != "user" {
			continue
		}
		if text := extractTextFromContent(arr[i].Get("content")); text != "" {
			out.UserInput = text
			break
		}
	}

	for i := len(arr) - 1; i >= 0; i-- {
		if arr[i].Get("role").String() != "assistant" {
			continue
		}
		if c := arr[i].Get("content"); c.IsArray() {
			for _, ci := range c.Array() {
				if ci.Get("type").String() == "tool_use" {
					out.ToolUses = append(out.ToolUses, ToolUse{
						ID:    ci.Get("id").String(),
						Name:  ci.Get("name").String(),
						Input: parseRawAny(ci.Get("input")),
					})
				}
			}
		}
		break
	}

	return marshalSimplified(out)
}

// SimplifyRequestBody 综合尝试简化 Anthropic / ChatCompletions / Responses 请求体。
// 调用顺序：Anthropic → ChatCompletions → Responses；全部识别失败则原样返回 body 字符串。
func SimplifyRequestBody(body []byte) string {
	if simplified := SimplifyAnthropicRequest(body); simplified != "" {
		return simplified
	}
	if simplified := SimplifyChatCompletionsRequest(body); simplified != "" {
		return simplified
	}
	if simplified := SimplifyResponsesRequest(body); simplified != "" {
		return simplified
	}
	return string(body)
}

// SimplifyResponsesRequest 提取 OpenAI Responses /v1/responses 请求的最小上下文包。
// input 为字符串时直接作为 user_input；为数组时：
//   - tool_results：input[] 末尾连续 type==function_call_output
//   - tool_uses：tool_results 之前末尾连续 type==function_call
//   - user_input：再之前最近一条 type==message && role==user
//
// 不做长度截断；超长字段的兜底截断由 WriteRequestLog 根据 MaxBodyBytes 配置决定。
func SimplifyResponsesRequest(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return ""
	}
	model := gjson.GetBytes(body, "model").String()

	if input.Type == gjson.String {
		return marshalSimplified(SimplifiedRequest{Model: model, UserInput: input.String()})
	}
	if !input.IsArray() {
		return ""
	}
	arr := input.Array()
	if len(arr) == 0 {
		return ""
	}

	out := SimplifiedRequest{Model: model}

	outputStart := len(arr)
	for i := len(arr) - 1; i >= 0; i-- {
		if arr[i].Get("type").String() == "function_call_output" {
			outputStart = i
		} else {
			break
		}
	}
	for i := outputStart; i < len(arr); i++ {
		m := arr[i]
		out.ToolResults = append(out.ToolResults, ToolResult{
			ToolUseID: m.Get("call_id").String(),
			Content:   parseRawAny(m.Get("output")),
		})
	}

	callStart := outputStart
	for i := outputStart - 1; i >= 0; i-- {
		if arr[i].Get("type").String() == "function_call" {
			callStart = i
		} else {
			break
		}
	}
	for i := callStart; i < outputStart; i++ {
		m := arr[i]
		out.ToolUses = append(out.ToolUses, ToolUse{
			ID:    m.Get("call_id").String(),
			Name:  m.Get("name").String(),
			Input: parseMaybeJSONString(m.Get("arguments")),
		})
	}

	pickUserInput := func(stopIdx int) string {
		for i := stopIdx - 1; i >= 0; i-- {
			m := arr[i]
			if m.Get("type").String() != "message" || m.Get("role").String() != "user" {
				continue
			}
			if text := extractTextFromContent(m.Get("content")); text != "" {
				return text
			}
		}
		return ""
	}
	out.UserInput = pickUserInput(callStart)
	if out.UserInput == "" {
		out.UserInput = pickUserInput(len(arr))
	}

	return marshalSimplified(out)
}

// SimplifyChatCompletionsRequest 提取 OpenAI /v1/chat/completions 请求的最小上下文包。
//   - tool_results：messages[] 末尾连续 role==tool
//   - tool_uses：tool_results 之前最近一条 role==assistant 的 tool_calls
//   - user_input：再之前最近一条 role==user 的 content
//
// 与 Anthropic 共享 messages 顶层结构，由 SimplifyRequestBody 的调用顺序保证不会误吞
// Anthropic 请求（Anthropic 函数会主动 detect ChatCompletions 形态并放行）。
func SimplifyChatCompletionsRequest(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return ""
	}
	arr := messages.Array()
	if len(arr) == 0 {
		return ""
	}

	out := SimplifiedRequest{Model: gjson.GetBytes(body, "model").String()}

	toolStart := len(arr)
	for i := len(arr) - 1; i >= 0; i-- {
		if arr[i].Get("role").String() == "tool" {
			toolStart = i
		} else {
			break
		}
	}
	for i := toolStart; i < len(arr); i++ {
		m := arr[i]
		out.ToolResults = append(out.ToolResults, ToolResult{
			ToolUseID: m.Get("tool_call_id").String(),
			Content:   parseRawAny(m.Get("content")),
		})
	}

	for i := toolStart - 1; i >= 0; i-- {
		if arr[i].Get("role").String() != "assistant" {
			continue
		}
		if tcs := arr[i].Get("tool_calls"); tcs.IsArray() {
			for _, tc := range tcs.Array() {
				out.ToolUses = append(out.ToolUses, ToolUse{
					ID:    tc.Get("id").String(),
					Name:  tc.Get("function.name").String(),
					Input: parseMaybeJSONString(tc.Get("function.arguments")),
				})
			}
		}
		break
	}

	pickUserInput := func(stopIdx int) string {
		for i := stopIdx - 1; i >= 0; i-- {
			if arr[i].Get("role").String() != "user" {
				continue
			}
			if text := extractTextFromContent(arr[i].Get("content")); text != "" {
				return text
			}
		}
		return ""
	}
	out.UserInput = pickUserInput(toolStart)
	if out.UserInput == "" {
		out.UserInput = pickUserInput(len(arr))
	}

	return marshalSimplified(out)
}

// SimplifyAnthropicResponse 从 Anthropic 非流式响应 JSON 中提取
// content 数组与 stop_reason，丢弃 usage / id / role 等元信息。
// 不做长度截断；超长字段的兜底截断由 WriteRequestLog 根据 MaxBodyBytes 配置决定。
func SimplifyAnthropicResponse(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	content := gjson.GetBytes(body, "content")
	stopReason := gjson.GetBytes(body, "stop_reason").String()

	out := []byte("{}")
	if content.Exists() {
		out, _ = sjson.SetRawBytes(out, "content", []byte(content.Raw))
	}
	if stopReason != "" {
		out, _ = sjson.SetBytes(out, "stop_reason", stopReason)
	}
	return string(out)
}

// AnthropicCollector 边解析 Anthropic SSE 流，边累积 content blocks。
// 调用方在每一行（或每一个 SSE 事件）触发 OnLine，最终通过 Finalize 取出聚合结果。
//
// 支持的事件：
//   - content_block_start: 新建一个 block (text/tool_use/thinking)
//   - content_block_delta: text_delta / input_json_delta / thinking_delta
//   - content_block_stop:  把累积的 input_json 字符串解析为 input 对象
//   - message_delta:       提取 stop_reason
type AnthropicCollector struct {
	blocks     map[int]*anthropicBlock
	order      []int
	stopReason string

	pendingEvent string
	pendingData  string
}

type anthropicBlock struct {
	blockType string
	text      strings.Builder
	thinking  strings.Builder
	inputBuf  strings.Builder
	toolID    string
	toolName  string
	rawStart  json.RawMessage
}

// NewAnthropicCollector 创建一个 Anthropic SSE 收集器。
func NewAnthropicCollector() *AnthropicCollector {
	return &AnthropicCollector{blocks: make(map[int]*anthropicBlock)}
}

// OnLine 接收一行 SSE 文本（可能是 "event: xxx" / "data: {...}" / 空行）。
// 空行表示一个事件结束，会触发实际处理。
func (c *AnthropicCollector) OnLine(line string) {
	trimmed := strings.TrimRight(line, "\r")
	if trimmed == "" {
		c.flushEvent()
		return
	}
	if strings.HasPrefix(trimmed, "event:") {
		c.pendingEvent = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		return
	}
	if strings.HasPrefix(trimmed, "data:") {
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if c.pendingData == "" {
			c.pendingData = data
		} else {
			c.pendingData += data
		}
	}
}

func (c *AnthropicCollector) flushEvent() {
	defer func() {
		c.pendingEvent = ""
		c.pendingData = ""
	}()
	if c.pendingData == "" || c.pendingData == "[DONE]" {
		return
	}

	parsed := gjson.Parse(c.pendingData)
	if !parsed.IsObject() {
		return
	}
	eventName := c.pendingEvent
	if eventName == "" {
		eventName = parsed.Get("type").String()
	}

	switch eventName {
	case "content_block_start":
		idx := int(parsed.Get("index").Int())
		blockType := parsed.Get("content_block.type").String()
		blk := &anthropicBlock{blockType: blockType}
		if blockType == "tool_use" {
			blk.toolID = parsed.Get("content_block.id").String()
			blk.toolName = parsed.Get("content_block.name").String()
		} else if blockType == "server_tool_use" || blockType == "redacted_thinking" {
			blk.rawStart = json.RawMessage(parsed.Get("content_block").Raw)
		}
		if _, exists := c.blocks[idx]; !exists {
			c.order = append(c.order, idx)
		}
		c.blocks[idx] = blk
	case "content_block_delta":
		idx := int(parsed.Get("index").Int())
		blk := c.blocks[idx]
		if blk == nil {
			blk = &anthropicBlock{}
			c.blocks[idx] = blk
			c.order = append(c.order, idx)
		}
		deltaType := parsed.Get("delta.type").String()
		switch deltaType {
		case "text_delta":
			blk.text.WriteString(parsed.Get("delta.text").String())
		case "input_json_delta":
			blk.inputBuf.WriteString(parsed.Get("delta.partial_json").String())
		case "thinking_delta":
			blk.thinking.WriteString(parsed.Get("delta.thinking").String())
		}
	case "message_delta":
		if reason := parsed.Get("delta.stop_reason").String(); reason != "" {
			c.stopReason = reason
		}
	}
}

// Finalize 输出聚合后的 JSON：{"content":[...], "stop_reason":"..."}.
// 若没有任何 content，返回空字符串。
func (c *AnthropicCollector) Finalize() string {
	c.flushEvent()
	if len(c.order) == 0 && c.stopReason == "" {
		return ""
	}
	contentItems := make([]map[string]any, 0, len(c.order))
	for _, idx := range c.order {
		blk := c.blocks[idx]
		if blk == nil {
			continue
		}
		switch blk.blockType {
		case "text":
			contentItems = append(contentItems, map[string]any{
				"type": "text",
				"text": blk.text.String(),
			})
		case "thinking":
			contentItems = append(contentItems, map[string]any{
				"type":     "thinking",
				"thinking": blk.thinking.String(),
			})
		case "tool_use":
			item := map[string]any{
				"type": "tool_use",
				"id":   blk.toolID,
				"name": blk.toolName,
			}
			input := blk.inputBuf.String()
			if input != "" {
				var parsed any
				if err := json.Unmarshal([]byte(input), &parsed); err == nil {
					item["input"] = parsed
				} else {
					item["input_raw"] = input
				}
			} else {
				item["input"] = map[string]any{}
			}
			contentItems = append(contentItems, item)
		default:
			item := map[string]any{"type": blk.blockType}
			if blk.text.Len() > 0 {
				item["text"] = blk.text.String()
			}
			contentItems = append(contentItems, item)
		}
	}

	out := map[string]any{}
	if len(contentItems) > 0 {
		out["content"] = contentItems
	}
	if c.stopReason != "" {
		out["stop_reason"] = c.stopReason
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

// ResponsesCollector 边解析 OpenAI Responses 流，边累积输出。
// Responses 流式协议在 response.completed 事件携带完整 output，因此实现策略：
//   - 优先取 response.completed.response.output / response.failed / response.incomplete
//   - 若没有终止事件，按 output_item.done 累积
type ResponsesCollector struct {
	finalOutput  string // 已序列化的 output JSON
	stopReason   string
	pendingEvent string
	pendingData  string
	itemsByIndex map[int]string
	order        []int
}

func NewResponsesCollector() *ResponsesCollector {
	return &ResponsesCollector{itemsByIndex: make(map[int]string)}
}

func (c *ResponsesCollector) OnLine(line string) {
	trimmed := strings.TrimRight(line, "\r")
	if trimmed == "" {
		c.flushEvent()
		return
	}
	if strings.HasPrefix(trimmed, "event:") {
		c.pendingEvent = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		return
	}
	if strings.HasPrefix(trimmed, "data:") {
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if c.pendingData == "" {
			c.pendingData = data
		} else {
			c.pendingData += data
		}
	}
}

func (c *ResponsesCollector) flushEvent() {
	defer func() {
		c.pendingEvent = ""
		c.pendingData = ""
	}()
	if c.pendingData == "" || c.pendingData == "[DONE]" {
		return
	}
	parsed := gjson.Parse(c.pendingData)
	if !parsed.IsObject() {
		return
	}
	eventType := parsed.Get("type").String()
	if c.pendingEvent != "" {
		eventType = c.pendingEvent
	}

	switch eventType {
	case "response.completed", "response.done", "response.incomplete", "response.failed":
		if output := parsed.Get("response.output"); output.Exists() && output.IsArray() && len(output.Array()) > 0 {
			c.finalOutput = output.Raw
		}
		if status := parsed.Get("response.status").String(); status != "" {
			c.stopReason = status
		}
	case "response.output_item.done":
		idx := int(parsed.Get("output_index").Int())
		item := parsed.Get("item").Raw
		if item != "" {
			if _, exists := c.itemsByIndex[idx]; !exists {
				c.order = append(c.order, idx)
			}
			c.itemsByIndex[idx] = item
		}
	}
}

func (c *ResponsesCollector) Finalize() string {
	c.flushEvent()
	out := map[string]any{}
	if c.finalOutput != "" {
		out["output"] = json.RawMessage(c.finalOutput)
	} else if len(c.order) > 0 {
		items := make([]json.RawMessage, 0, len(c.order))
		for _, idx := range c.order {
			items = append(items, json.RawMessage(c.itemsByIndex[idx]))
		}
		out["output"] = items
	}
	if c.stopReason != "" {
		out["status"] = c.stopReason
	}
	if len(out) == 0 {
		return ""
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

// SimplifyResponsesNonStream 从 OpenAI Responses 非流式 JSON 提取 output + status.
// 不做长度截断；超长字段的兜底截断由 WriteRequestLog 根据 MaxBodyBytes 配置决定。
func SimplifyResponsesNonStream(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	output := gjson.GetBytes(body, "output")
	status := gjson.GetBytes(body, "status").String()
	out := []byte("{}")
	if output.Exists() {
		out, _ = sjson.SetRawBytes(out, "output", []byte(output.Raw))
	}
	if status != "" {
		out, _ = sjson.SetBytes(out, "status", status)
	}
	return string(out)
}

// ChatCompletionsCollector 处理 OpenAI /v1/chat/completions 流式响应。
// 它聚合 choices[*].delta.content / tool_calls，最终输出 choices 数组。
type ChatCompletionsCollector struct {
	choices      map[int]*chatChoice
	order        []int
	finishReason string

	pendingData string
}

type chatChoice struct {
	role         string
	content      strings.Builder
	toolCalls    map[int]*chatToolCall
	toolOrder    []int
	finishReason string
}

type chatToolCall struct {
	id       string
	name     string
	argsBuf  strings.Builder
	function bool
}

func NewChatCompletionsCollector() *ChatCompletionsCollector {
	return &ChatCompletionsCollector{choices: make(map[int]*chatChoice)}
}

// OnLine 接受 SSE 行（chat completions 没有 event 名，只有 data）。
func (c *ChatCompletionsCollector) OnLine(line string) {
	trimmed := strings.TrimRight(line, "\r")
	if trimmed == "" {
		c.flushEvent()
		return
	}
	if strings.HasPrefix(trimmed, "data:") {
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if c.pendingData == "" {
			c.pendingData = data
		} else {
			c.pendingData += data
		}
	}
}

func (c *ChatCompletionsCollector) flushEvent() {
	defer func() { c.pendingData = "" }()
	if c.pendingData == "" || c.pendingData == "[DONE]" {
		return
	}
	parsed := gjson.Parse(c.pendingData)
	if !parsed.IsObject() {
		return
	}
	choices := parsed.Get("choices")
	if !choices.IsArray() {
		return
	}
	for _, choice := range choices.Array() {
		idx := int(choice.Get("index").Int())
		ch := c.choices[idx]
		if ch == nil {
			ch = &chatChoice{toolCalls: make(map[int]*chatToolCall)}
			c.choices[idx] = ch
			c.order = append(c.order, idx)
		}
		delta := choice.Get("delta")
		if delta.Exists() {
			if role := delta.Get("role").String(); role != "" && ch.role == "" {
				ch.role = role
			}
			if content := delta.Get("content"); content.Exists() && content.Type == gjson.String {
				ch.content.WriteString(content.String())
			}
			if tools := delta.Get("tool_calls"); tools.IsArray() {
				for _, t := range tools.Array() {
					tIdx := int(t.Get("index").Int())
					tc := ch.toolCalls[tIdx]
					if tc == nil {
						tc = &chatToolCall{}
						ch.toolCalls[tIdx] = tc
						ch.toolOrder = append(ch.toolOrder, tIdx)
					}
					if id := t.Get("id").String(); id != "" {
						tc.id = id
					}
					if name := t.Get("function.name").String(); name != "" {
						tc.name = name
						tc.function = true
					}
					if args := t.Get("function.arguments").String(); args != "" {
						tc.argsBuf.WriteString(args)
					}
				}
			}
		}
		if reason := choice.Get("finish_reason").String(); reason != "" {
			ch.finishReason = reason
			c.finishReason = reason
		}
	}
}

func (c *ChatCompletionsCollector) Finalize() string {
	c.flushEvent()
	if len(c.order) == 0 {
		return ""
	}
	choices := make([]map[string]any, 0, len(c.order))
	for _, idx := range c.order {
		ch := c.choices[idx]
		msg := map[string]any{}
		if ch.role != "" {
			msg["role"] = ch.role
		}
		if ch.content.Len() > 0 {
			msg["content"] = ch.content.String()
		}
		if len(ch.toolOrder) > 0 {
			toolList := make([]map[string]any, 0, len(ch.toolOrder))
			for _, tIdx := range ch.toolOrder {
				t := ch.toolCalls[tIdx]
				entry := map[string]any{"id": t.id}
				if t.function {
					fn := map[string]any{"name": t.name}
					argsStr := t.argsBuf.String()
					if argsStr != "" {
						var parsed any
						if err := json.Unmarshal([]byte(argsStr), &parsed); err == nil {
							fn["arguments"] = parsed
						} else {
							fn["arguments_raw"] = argsStr
						}
					}
					entry["type"] = "function"
					entry["function"] = fn
				}
				toolList = append(toolList, entry)
			}
			msg["tool_calls"] = toolList
		}
		entry := map[string]any{"index": idx, "message": msg}
		if ch.finishReason != "" {
			entry["finish_reason"] = ch.finishReason
		}
		choices = append(choices, entry)
	}
	out := map[string]any{"choices": choices}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

// SimplifyChatCompletionsNonStream 从 chat completions 非流式 JSON 提取 choices.
// 不做长度截断；超长字段的兜底截断由 WriteRequestLog 根据 MaxBodyBytes 配置决定。
func SimplifyChatCompletionsNonStream(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	choices := gjson.GetBytes(body, "choices")
	if !choices.Exists() {
		return ""
	}
	out := []byte("{}")
	out, _ = sjson.SetRawBytes(out, "choices", []byte(choices.Raw))
	return string(out)
}
