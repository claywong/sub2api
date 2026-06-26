// gateway_service_requestlog.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：请求内容日志（request_logs）相关方法。
//
// 这些方法附着在 GatewayService 上，作用是：
//   - 在 handler 同步路径上提前固定 request_id，让 usage_logs 与 request_logs
//     使用同一 ID。
//   - 提取客户端会话标识，用于跨协议聚合 request_logs.session_id。
//   - 异步写入 request_logs，受 gateway.request_log.enabled 开关控制。
//
// 与 upstream 合并策略：
//   - 这些方法是纯增量，move 到 companion 文件可以让 gateway_service.go
//     与 upstream 的 diff 大幅缩减，降低 merge 冲突率。
//   - GatewayService 上新增的字段（requestLogRepo）和构造函数参数仍留在
//     gateway_service.go，因为 Go 的 struct 定义和 NewGatewayService 必须放
//     在一起。
// =============================================================================
package service

import (
	"context"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestlog"

	"github.com/gin-gonic/gin"
)

// ResolveRequestID 解析请求级 request_id（client:/local:/upstream/generated 优先级），
// 用于在 handler 同步路径上提前固定 ID，确保 usage_logs 与 request_logs 使用同一 ID。
func (s *GatewayService) ResolveRequestID(ctx context.Context, upstreamRequestID string) string {
	return resolveUsageBillingRequestID(ctx, upstreamRequestID)
}

// ExtractClientSessionID 提取客户端原始会话标识，用于写入 request_logs.session_id。
// 优先级：HTTP header session_id / conversation_id → metadata.user_id 中的 SessionID
// → header anthropic-session-id（部分 SDK 自定义）→ 空串。
//
// 该值不做 hash，只规范化（trim + 截断到 256 字节）。这样跨协议同一会话的多次请求
// 在 request_logs 中拥有相同 session_id，便于按会话聚合查询。
func (s *GatewayService) ExtractClientSessionID(c *gin.Context, parsed *ParsedRequest) string {
	if c != nil {
		if v := strings.TrimSpace(c.GetHeader("session_id")); v != "" {
			return requestlog.NormalizeSessionID(v)
		}
		if v := strings.TrimSpace(c.GetHeader("conversation_id")); v != "" {
			return requestlog.NormalizeSessionID(v)
		}
	}
	if parsed != nil && parsed.MetadataUserID != "" {
		if uid := ParseMetadataUserID(parsed.MetadataUserID); uid != nil && uid.SessionID != "" {
			return requestlog.NormalizeSessionID(uid.SessionID)
		}
	}
	return ""
}

// finalizeRespCollector 安全地从可空 collector 取出聚合结果。
func finalizeRespCollector(c *requestlog.AnthropicCollector) string {
	if c == nil {
		return ""
	}
	return c.Finalize()
}

// WriteRequestLog 异步写入请求内容日志，仅当 gateway.request_log.enabled=true 时生效。
//
// 入参约定：
//   - sessionID：原始客户端会话标识（不 hash），由 NormalizeSessionID 截断到 256；
//     上层应通过 ExtractClientSessionID 获取，保证跨协议含义一致。
//   - reqBody：原始请求体；函数会调用 SimplifyRequestBody 只保留 messages/input 最后一条，
//     无法识别协议时原样保留，最后用 MaxBodyBytes 兜底（仅在已经无法精简时）。
//   - respBody：调用方应已经经过 collector / Simplify*Response 精简；本函数也兜底 MaxBodyBytes。
func (s *GatewayService) WriteRequestLog(ctx context.Context, requestID, sessionID string, userID int64, reqBody, respBody string) {
	if s.requestLogRepo == nil || s.cfg == nil || !s.cfg.Gateway.RequestLog.Enabled {
		return
	}
	if requestID == "" {
		requestID = resolveUsageBillingRequestID(ctx, "")
	}
	sessionID = requestlog.NormalizeSessionID(sessionID)
	reqBody = requestlog.SimplifyRequestBody([]byte(reqBody))
	maxBytes := s.cfg.Gateway.RequestLog.MaxBodyBytes
	if maxBytes > 0 && len(reqBody) > maxBytes {
		// 若精简后仍超大（极少发生：messages[-1] 自身就很大），尝试 SafeTruncate；
		// 解析失败再退回 byte 截断，最坏是非法 JSON 但不阻塞写入。
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
