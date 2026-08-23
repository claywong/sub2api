// openai_gateway_service_requestlog.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：OpenAI 网关的请求内容日志（request_logs）
// 相关方法。
//
// 与 GatewayService.WriteRequestLog 行为对齐，但服务于 OpenAI 协议（Chat
// Completions / Responses）。截断与精简规则同步沿用 requestlog 包的工具函数。
//
// 与 upstream 合并策略：
//   - OpenAIGatewayService 上新增的字段 requestLogRepo、构造参数与 ForwardResult
//     上新增的 CapturedResponseBody 字段仍留在 openai_gateway_service.go（Go
//     struct 必须整块定义）。
//   - 这两个方法是纯增量，搬到 companion 文件后 openai_gateway_service.go 与
//     upstream 的 diff 可以缩到几行 inline 修改。
//
// =============================================================================
package service

import (
	"context"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestlog"
)

// ResolveRequestID 解析请求级 request_id，与 RecordUsage 内部使用的逻辑一致。
// 用于在 handler 同步路径上提前固定 ID，确保 usage_logs 与 request_logs 使用同一 ID。
func (s *OpenAIGatewayService) ResolveRequestID(ctx context.Context, upstreamRequestID string) string {
	return resolveUsageBillingRequestID(ctx, upstreamRequestID)
}

// finalizeResponsesCollector 安全地从可空 collector 取出聚合结果。
// 与 finalizeRespCollector（Anthropic）对应，服务于 OpenAI Responses 协议。
func finalizeResponsesCollector(c *requestlog.ResponsesCollector) string {
	if c == nil {
		return ""
	}
	return c.Finalize()
}

// captureAlphaSearchResponseBody 采集 /v1/alpha/search 响应体，供写入 request_logs。
// alpha/search 返回的是搜索结果 schema（无 Responses 的 output 字段），套用
// SimplifyResponsesNonStream 会把内容剥成 "{}"，因此原样保留，超长交由
// WriteRequestLog 按 MaxBodyBytes 截断。开关关闭时返回空串。
func (s *OpenAIGatewayService) captureAlphaSearchResponseBody(body []byte) string {
	if s.cfg == nil || !s.cfg.Gateway.RequestLog.Enabled || len(body) == 0 {
		return ""
	}
	return string(body)
}

// captureResponsesNonStreamBody 采集非流式 Responses 响应体，供写入 request_logs。
// 上游在 stream=false 时仍可能回 SSE 文本（handleSSEToJSON 分支），因此这里按形态分流：
// 带 SSE 帧的走 ResponsesCollector 逐行聚合，纯 JSON 走 SimplifyResponsesNonStream。
// 开关关闭时返回空串，不做任何解析开销。
func (s *OpenAIGatewayService) captureResponsesNonStreamBody(body []byte) string {
	if s.cfg == nil || !s.cfg.Gateway.RequestLog.Enabled || len(body) == 0 {
		return ""
	}
	if !bodyHasSSEFraming(body) {
		return requestlog.SimplifyResponsesNonStream(body)
	}
	collector := requestlog.NewResponsesCollector()
	for _, line := range strings.Split(string(body), "\n") {
		collector.OnLine(line)
	}
	return collector.Finalize()
}

// WriteRequestLog 异步写入请求内容日志，仅当 gateway.request_log.enabled=true 时生效。
// 截断规则与 GatewayService.WriteRequestLog 保持一致，详见后者注释。
func (s *OpenAIGatewayService) WriteRequestLog(ctx context.Context, requestID, sessionID string, userID int64, reqBody, respBody string) {
	if s.requestLogRepo == nil || s.cfg == nil || !s.cfg.Gateway.RequestLog.Enabled {
		return
	}
	if requestID == "" {
		requestID = "generated:" + generateRequestID()
	}
	sessionID = requestlog.NormalizeSessionID(sessionID)
	reqBody = requestlog.SimplifyRequestBody([]byte(reqBody))
	maxBytes := s.cfg.Gateway.RequestLog.MaxBodyBytes
	if maxBytes > 0 && len(reqBody) > maxBytes {
		safe := requestlog.SafeTruncateJSON([]byte(reqBody), maxBytes/4)
		if len(safe) <= maxBytes {
			reqBody = string(safe)
		} else {
			reqBody = reqBody[:maxBytes]
		}
	}
	if maxBytes > 0 && len(respBody) > maxBytes {
		safe := requestlog.SafeTruncateJSON([]byte(respBody), maxBytes/4)
		if len(safe) <= maxBytes {
			respBody = string(safe)
		} else {
			respBody = respBody[:maxBytes]
		}
	}
	s.requestLogRepo.CreateBestEffort(ctx, &RequestLog{
		RequestID:    requestID,
		SessionID:    sessionID,
		UserID:       userID,
		RequestBody:  reqBody,
		ResponseBody: respBody,
		CreatedAt:    time.Now(),
	})
}
