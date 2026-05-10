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

// DefaultPerStringTruncate 是单个 JSON 字符串字段的默认截断阈值（字节）。
// 仅作用于精简后 JSON 中的 string 值（如 message 文本、tool input、image base64），
// 防止单条消息中嵌入超大 base64 等内容把行膨胀到 MB 级。
const DefaultPerStringTruncate = 4096

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
// 仅在 perStringMax > 0 时启用截断；解析失败时原样返回。
//
// 图片相关字段（preserveImageKeys 命中）会原样保留 base64 内容，不做截断。
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

// SimplifyAnthropicRequest 仅保留 messages 数组最后一条 + 顶层 model 字段。
// 其余字段（system/tools/历史 messages）全部丢弃。如未能识别为 Anthropic 协议则返回空串，
// 由调用方决定回退策略（原样保留 / 尝试其他协议）。
// 输出会经过 SafeTruncateJSON，单字段超过 DefaultPerStringTruncate 字节时替换为占位符，
// 防止 base64 图片 / 巨大文本占满日志列。
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
	last := arr[len(arr)-1].Raw
	model := gjson.GetBytes(body, "model").String()

	out := []byte("{}")
	if model != "" {
		out, _ = sjson.SetBytes(out, "model", model)
	}
	out, _ = sjson.SetRawBytes(out, "messages", []byte("["+last+"]"))
	out = SafeTruncateJSON(out, DefaultPerStringTruncate)
	return string(out)
}

// SimplifyRequestBody 综合尝试简化 Anthropic / Responses / ChatCompletions 请求体。
// 如全部协议都识别失败，则返回原始 body 字符串。
func SimplifyRequestBody(body []byte) string {
	if simplified := SimplifyAnthropicRequest(body); simplified != "" {
		return simplified
	}
	if simplified := SimplifyResponsesRequest(body); simplified != "" {
		return simplified
	}
	return string(body)
}

// SimplifyResponsesRequest 仅保留 input 数组最后一条 + 顶层 model 字段。
// 适用于 OpenAI Responses API 协议。如未能识别为 Responses 协议则返回空串。
// 输出经过 SafeTruncateJSON 截断超大 string 字段。
func SimplifyResponsesRequest(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	input := gjson.GetBytes(body, "input")
	model := gjson.GetBytes(body, "model").String()
	if input.IsArray() {
		arr := input.Array()
		if len(arr) == 0 {
			return ""
		}
		last := arr[len(arr)-1].Raw
		out := []byte("{}")
		if model != "" {
			out, _ = sjson.SetBytes(out, "model", model)
		}
		out, _ = sjson.SetRawBytes(out, "input", []byte("["+last+"]"))
		out = SafeTruncateJSON(out, DefaultPerStringTruncate)
		return string(out)
	}
	if input.Type == gjson.String {
		out := []byte("{}")
		if model != "" {
			out, _ = sjson.SetBytes(out, "model", model)
		}
		// input 是 string 时直接 set 字符串字段，由 sjson 处理转义；
		// 截断单字段长度由 SafeTruncateJSON 兜底。
		out, _ = sjson.SetBytes(out, "input", input.String())
		out = SafeTruncateJSON(out, DefaultPerStringTruncate)
		return string(out)
	}
	return ""
}

// SimplifyChatCompletionsRequest 仅保留 messages 数组最后一条 + 顶层 model。
// 适用于 OpenAI /v1/chat/completions 协议。
func SimplifyChatCompletionsRequest(body []byte) string {
	// 与 Anthropic 同结构（messages 数组），可以直接复用
	return SimplifyAnthropicRequest(body)
}

// SimplifyAnthropicResponse 从 Anthropic 非流式响应 JSON 中提取
// content 数组与 stop_reason，丢弃 usage / id / role 等元信息。
// 输出经过 SafeTruncateJSON 防止 tool 调用 input 中嵌入超大文本。
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
	out = SafeTruncateJSON(out, DefaultPerStringTruncate)
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
	return string(SafeTruncateJSON(b, DefaultPerStringTruncate))
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
	return string(SafeTruncateJSON(b, DefaultPerStringTruncate))
}

// SimplifyResponsesNonStream 从 OpenAI Responses 非流式 JSON 提取 output + status.
// 输出经过 SafeTruncateJSON 截断超大 string 字段。
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
	out = SafeTruncateJSON(out, DefaultPerStringTruncate)
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
	return string(SafeTruncateJSON(b, DefaultPerStringTruncate))
}

// SimplifyChatCompletionsNonStream 从 chat completions 非流式 JSON 提取 choices.
// 输出经过 SafeTruncateJSON 截断超大 string 字段。
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
	out = SafeTruncateJSON(out, DefaultPerStringTruncate)
	return string(out)
}
