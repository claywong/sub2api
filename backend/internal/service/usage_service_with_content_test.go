package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// withContentUsageLogRepo 嵌入接口，只覆盖 ListWithFilters。
type withContentUsageLogRepo struct {
	UsageLogRepository
	logs []UsageLog
}

func (r *withContentUsageLogRepo) ListWithFilters(_ context.Context, _ pagination.PaginationParams, _ usagestats.UsageLogFilters) ([]UsageLog, *pagination.PaginationResult, error) {
	return r.logs, &pagination.PaginationResult{Total: int64(len(r.logs))}, nil
}

// withContentRequestLogRepo 返回预置内容。
type withContentRequestLogRepo struct {
	data map[string]*RequestLog
}

func (r *withContentRequestLogRepo) CreateBestEffort(_ context.Context, _ *RequestLog) {}
func (r *withContentRequestLogRepo) GetByRequestIDs(_ context.Context, ids []string) (map[string]*RequestLog, error) {
	result := make(map[string]*RequestLog)
	for _, id := range ids {
		if rl, ok := r.data[id]; ok {
			result[id] = rl
		}
	}
	return result, nil
}

func newUsageSvcForContentTest(logs []UsageLog, content map[string]*RequestLog) *UsageService {
	return &UsageService{
		usageRepo:      &withContentUsageLogRepo{logs: logs},
		requestLogRepo: &withContentRequestLogRepo{data: content},
	}
}

func TestListWithFilters_WithContentFalse_NoContentFetched(t *testing.T) {
	logs := []UsageLog{{RequestID: "req-1", Model: "gpt-4"}}
	svc := newUsageSvcForContentTest(logs, map[string]*RequestLog{
		"req-1": {RequestID: "req-1", RequestBody: "hello", ResponseBody: "world", SessionID: "sess-1"},
	})

	results, _, err := svc.ListWithFilters(context.Background(), pagination.PaginationParams{}, usagestats.UsageLogFilters{}, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if results[0].RequestBody != "" || results[0].ResponseBody != "" || results[0].SessionID != "" {
		t.Error("expected no content fields when withContent=false")
	}
}

func TestListWithFilters_WithContentTrue_MergesContent(t *testing.T) {
	logs := []UsageLog{
		{RequestID: "req-1", Model: "gpt-4"},
		{RequestID: "req-2", Model: "gpt-4"},
	}
	svc := newUsageSvcForContentTest(logs, map[string]*RequestLog{
		"req-1": {RequestID: "req-1", RequestBody: `{"prompt":"hi"}`, ResponseBody: `{"text":"hello"}`, SessionID: "sess-abc"},
	})

	results, _, err := svc.ListWithFilters(context.Background(), pagination.PaginationParams{}, usagestats.UsageLogFilters{}, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if results[0].RequestBody != `{"prompt":"hi"}` {
		t.Errorf("req-1 request_body: got %q", results[0].RequestBody)
	}
	if results[0].ResponseBody != `{"text":"hello"}` {
		t.Errorf("req-1 response_body: got %q", results[0].ResponseBody)
	}
	if results[0].SessionID != "sess-abc" {
		t.Errorf("req-1 session_id: got %q", results[0].SessionID)
	}
	// req-2 无记录，字段应为空
	if results[1].RequestBody != "" || results[1].SessionID != "" {
		t.Error("req-2 should have empty content fields")
	}
}

func TestListWithFilters_WithContentTrue_NilRepo_NoError(t *testing.T) {
	logs := []UsageLog{{RequestID: "req-1"}}
	svc := &UsageService{
		usageRepo:      &withContentUsageLogRepo{logs: logs},
		requestLogRepo: nil,
	}

	results, _, err := svc.ListWithFilters(context.Background(), pagination.PaginationParams{}, usagestats.UsageLogFilters{}, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if results[0].RequestBody != "" {
		t.Error("expected empty content when requestLogRepo is nil")
	}
}

func TestListWithFilters_WithContentTrue_EmptyLogs(t *testing.T) {
	svc := newUsageSvcForContentTest(nil, nil)
	results, _, err := svc.ListWithFilters(context.Background(), pagination.PaginationParams{}, usagestats.UsageLogFilters{}, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty result, got %d", len(results))
	}
}

func TestListWithFilters_WithContentTrue_NoMatchInRequestLog(t *testing.T) {
	logs := []UsageLog{{RequestID: "req-999"}}
	svc := newUsageSvcForContentTest(logs, map[string]*RequestLog{})

	results, _, err := svc.ListWithFilters(context.Background(), pagination.PaginationParams{}, usagestats.UsageLogFilters{}, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if results[0].RequestBody != "" || results[0].SessionID != "" {
		t.Error("expected empty content when no matching request_log entry")
	}
}
