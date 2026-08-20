// prompt_dlp_confirm.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 命中的 LLM 二次确认。
//
// 职责：把正则存活的 finding 批量送给一个 chat completions 兼容模型（默认
// gpt-5.6-luna），让它判断每条命中是真实敏感信息泄露还是误报。
//
// 为什么要二次确认：正则只能判断"形状像"，判断不了语境。占位符、厂商文档示例值、
// 变量名引用这类误报需要语义理解。实测 luna 在这类判断上 9/9 全对。
//
// 为什么批量：一次请求确认多条 finding，把请求数从 O(命中数) 压到 O(1)。实测
// 5 条/请求全对，只花 158 output tokens。
//
// 降级策略：fail-open（确认失败即放行 + 记审计）。与 upstream qwen3guard 的
// fail-closed（503）刻意不同——DLP 依赖的是第三方中转，实测会出现 403/429/401
// 波动，fail-closed 会把对方的抖动直接变成本网关的 503。正则层零外部依赖仍在
// 工作，降级只是少了降误报那一层。
//
// 与 upstream 合并策略：
//   - 纯新增文件，复用 upstream 的 ChatCompletionsURL / extractOpenAIContent /
//     NewSecureHTTPClient，不改动 upstream 符号，merge 时不会冲突。
//
// =============================================================================
package securityaudit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// dlpConfirmSystemPrompt 是二次确认的系统提示词。
//
// 措辞刻意收紧到「只判断是否真实泄露」这一个问题上，并显式列出非敏感情形，
// 避免模型自由发挥。要求纯 JSON 输出以便稳定解析。
const dlpConfirmSystemPrompt = `你是敏感信息二次确认器。上游正则已命中若干疑似敏感片段，你的唯一任务是逐条判断它是否为真实的敏感信息泄露。

判定为非敏感（sensitive=false）的情形：
- 占位符或模板变量（your-xxx / placeholder / example / xxx / ${VAR}）
- 厂商文档或教程里的示例值
- 明显的测试数据
- 仅出现变量名、字段名或常量名，没有真实值
- 内网地址、本地地址（localhost / 127.x / 10.x / 192.168.x / 172.16-31.x）
- 值本身就是 password/secret/token 这类字面量词

判定为敏感（sensitive=true）的情形：
- 看起来是真实使用中的密钥、口令、证件号、卡号、连接凭据

严格只输出 JSON，不要解释、不要 markdown 代码块。格式：
{"results":[{"i":1,"sensitive":true,"reason":"15字内理由"}]}
results 的元素顺序与顺序编号必须与输入一致，每条输入都要有对应输出。`

// maxDLPConfirmBatchSize 限制单次请求确认的 finding 条数。
// 过大会拉长单次延迟并增加模型漏项风险；实测 5 条稳定全对。
const maxDLPConfirmBatchSize = 8

// dlpConfirmMaxTokens 是确认响应的 token 上限。
// luna 是推理模型，需要给足预算，否则推理未结束就被截断导致空内容。
const dlpConfirmMaxTokens = 3000

// DLPConfirmVerdict 是单条 finding 的确认结论。
type DLPConfirmVerdict struct {
	Sensitive bool
	Reason    string
	// Confirmed 表示这条结论确实来自模型。false 说明模型没给出对应项，
	// 调用方应按降级策略处理，而不是把它当成"模型判为误报"。
	Confirmed bool
}

// DLPConfirmer 对 finding 批量做二次确认。
type DLPConfirmer struct {
	clients sync.Map
}

// NewDLPConfirmer 构造一个二次确认器。HTTP 客户端按 endpoint 缓存复用。
func NewDLPConfirmer() *DLPConfirmer { return &DLPConfirmer{} }

// dlpConfirmResponse 是模型返回的 JSON 结构。
type dlpConfirmResponse struct {
	Results []struct {
		Index     int    `json:"i"`
		Sensitive bool   `json:"sensitive"`
		Reason    string `json:"reason"`
	} `json:"results"`
}

// Confirm 对 findings 批量做二次确认，返回与输入等长、下标一一对应的结论切片。
//
// 任何错误都以 error 返回，由调用方决定降级行为；本函数不自行放行或拦截。
func (c *DLPConfirmer) Confirm(
	ctx context.Context, endpoint ActiveEndpoint, findings []DLPFinding,
) ([]DLPConfirmVerdict, error) {
	verdicts := make([]DLPConfirmVerdict, len(findings))
	if len(findings) == 0 {
		return verdicts, nil
	}
	for start := 0; start < len(findings); start += maxDLPConfirmBatchSize {
		end := start + maxDLPConfirmBatchSize
		if end > len(findings) {
			end = len(findings)
		}
		batch := findings[start:end]
		batchVerdicts, err := c.confirmBatch(ctx, endpoint, batch)
		if err != nil {
			return verdicts, err
		}
		copy(verdicts[start:end], batchVerdicts)
	}
	return verdicts, nil
}

// confirmBatch 确认一个批次，返回与 batch 等长的结论。
func (c *DLPConfirmer) confirmBatch(
	ctx context.Context, endpoint ActiveEndpoint, batch []DLPFinding,
) ([]DLPConfirmVerdict, error) {
	verdicts := make([]DLPConfirmVerdict, len(batch))
	content, err := c.callModel(ctx, endpoint, buildDLPConfirmPrompt(batch))
	if err != nil {
		return verdicts, err
	}
	parsed, err := parseDLPConfirmResponse(content)
	if err != nil {
		return verdicts, err
	}
	for _, item := range parsed.Results {
		// 模型返回的编号是 1-based。越界项直接忽略，避免脏数据写坏结论。
		index := item.Index - 1
		if index < 0 || index >= len(verdicts) {
			continue
		}
		verdicts[index] = DLPConfirmVerdict{
			Sensitive: item.Sensitive,
			Reason:    strings.TrimSpace(item.Reason),
			Confirmed: true,
		}
	}
	return verdicts, nil
}

// buildDLPConfirmPrompt 拼装批量确认的用户消息。
//
// 只发送命中片段与一小段上下文，不发送整篇原文——既省 token，也避免把无关的
// 用户数据外送给确认模型。
func buildDLPConfirmPrompt(batch []DLPFinding) string {
	var builder strings.Builder
	builder.WriteString("请逐条判断以下命中：\n\n")
	for index, finding := range batch {
		fmt.Fprintf(&builder, "[%d] 类型: %s\n", index+1, finding.Title)
		fmt.Fprintf(&builder, "    命中片段: %s\n", finding.Value)
		if finding.Match != finding.Value {
			fmt.Fprintf(&builder, "    所在表达: %s\n", finding.Match)
		}
		builder.WriteString("\n")
	}
	fmt.Fprintf(&builder, "共 %d 条，results 必须返回 %d 个元素。", len(batch), len(batch))
	return builder.String()
}

// parseDLPConfirmResponse 解析模型输出。容忍模型套了 markdown 代码块的情况。
func parseDLPConfirmResponse(content string) (dlpConfirmResponse, error) {
	var parsed dlpConfirmResponse
	cleaned := stripJSONCodeFence(content)
	if cleaned == "" {
		return parsed, &GuardError{Code: ErrorCodeInvalidResponse,
			Cause: errors.New("dlp confirm returned empty content")}
	}
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		return parsed, &GuardError{Code: ErrorCodeInvalidResponse, Cause: err}
	}
	if len(parsed.Results) == 0 {
		return parsed, &GuardError{Code: ErrorCodeInvalidResponse,
			Cause: errors.New("dlp confirm returned no results")}
	}
	return parsed, nil
}

// stripJSONCodeFence 去掉模型可能套上的 ```json 围栏，并截取到最外层 JSON 对象。
func stripJSONCodeFence(content string) string {
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "```") {
		if newline := strings.IndexByte(trimmed, '\n'); newline >= 0 {
			trimmed = trimmed[newline+1:]
		}
		if fence := strings.LastIndex(trimmed, "```"); fence >= 0 {
			trimmed = trimmed[:fence]
		}
		trimmed = strings.TrimSpace(trimmed)
	}
	start := strings.IndexByte(trimmed, '{')
	end := strings.LastIndexByte(trimmed, '}')
	if start < 0 || end <= start {
		return ""
	}
	return trimmed[start : end+1]
}

// callModel 调用 chat completions 接口并返回消息正文。
func (c *DLPConfirmer) callModel(
	ctx context.Context, endpoint ActiveEndpoint, userPrompt string,
) (string, error) {
	client, err := c.clientFor(endpoint)
	if err != nil {
		return "", &GuardError{Code: ErrorCodeUnavailable, Cause: err}
	}
	requestURL, err := ChatCompletionsURL(endpoint.BaseURL)
	if err != nil {
		return "", &GuardError{Code: ErrorCodeUnavailable, Cause: err}
	}
	payload := map[string]any{
		"model": endpoint.Model,
		"messages": []map[string]string{
			{"role": "system", "content": dlpConfirmSystemPrompt},
			{"role": "user", "content": userPrompt},
		},
		// 强制 JSON 输出，避免模型加解释导致解析失败。
		"response_format":       map[string]string{"type": "json_object"},
		"max_completion_tokens": dlpConfirmMaxTokens,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", &GuardError{Code: ErrorCodeInvalidResponse, Cause: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return "", &GuardError{Code: ErrorCodeUnavailable, Cause: err}
	}
	req.Header.Set("Content-Type", "application/json")
	if endpoint.Token != "" {
		req.Header.Set("Authorization", "Bearer "+endpoint.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		timeout := errors.Is(err, context.DeadlineExceeded)
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			timeout = true
		}
		return "", &GuardError{Code: ErrorCodeUnavailable, Retryable: true, Timeout: timeout, Cause: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return "", &GuardError{Code: ErrorCodeUnavailable, HTTPStatus: resp.StatusCode, Retryable: retryable}
	}
	limited := io.LimitReader(resp.Body, maxGuardResponseBytes+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil {
		return "", &GuardError{Code: ErrorCodeUnavailable, Retryable: true, Cause: err}
	}
	if int64(len(responseBody)) > maxGuardResponseBytes {
		return "", &GuardError{Code: ErrorCodeInvalidResponse}
	}
	content, err := extractOpenAIContent(responseBody)
	if err != nil {
		return "", &GuardError{Code: ErrorCodeInvalidResponse, Cause: err}
	}
	return content, nil
}

// clientFor 按 endpoint 复用 HTTP 客户端，沿用 upstream 的安全客户端构造。
func (c *DLPConfirmer) clientFor(endpoint ActiveEndpoint) (*http.Client, error) {
	key := fmt.Sprintf("dlp|%s|%s|%d", endpoint.ID, endpoint.BaseURL, endpoint.TimeoutMS)
	if cached, ok := c.clients.Load(key); ok {
		if client, ok := cached.(*http.Client); ok {
			return client, nil
		}
	}
	client, err := NewSecureHTTPClient(endpoint)
	if err != nil {
		return nil, err
	}
	actual, _ := c.clients.LoadOrStore(key, client)
	if stored, ok := actual.(*http.Client); ok {
		return stored, nil
	}
	return client, nil
}
