package service

import (
	"context"
	"time"
)

// RequestLog 请求内容记录，存储原始的请求体和响应体。
type RequestLog struct {
	RequestID    string
	SessionID    string
	UserID       int64
	RequestBody  string
	ResponseBody string
	CreatedAt    time.Time
}

// RequestLogRepository 请求内容记录存储接口。
type RequestLogRepository interface {
	// CreateBestEffort 异步写入，队列满时静默丢弃，不阻塞调用方。
	CreateBestEffort(ctx context.Context, log *RequestLog)
	// GetByRequestIDs 按 request_id 批量查询，返回 map[requestID]*RequestLog。
	GetByRequestIDs(ctx context.Context, requestIDs []string) (map[string]*RequestLog, error)
}
