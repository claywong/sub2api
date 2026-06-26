package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// requestLogRepoStub 记录所有写入，用于断言。
type requestLogRepoStub struct {
	mu   sync.Mutex
	logs []*RequestLog
}

func (r *requestLogRepoStub) CreateBestEffort(_ context.Context, log *RequestLog) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, log)
}

func (r *requestLogRepoStub) GetByRequestIDs(_ context.Context, ids []string) (map[string]*RequestLog, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idSet := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		idSet[id] = struct{}{}
	}
	result := make(map[string]*RequestLog)
	for _, log := range r.logs {
		if _, ok := idSet[log.RequestID]; ok {
			result[log.RequestID] = log
		}
	}
	return result, nil
}

func (r *requestLogRepoStub) last() *RequestLog {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.logs) == 0 {
		return nil
	}
	return r.logs[len(r.logs)-1]
}

func newGatewayWithRequestLog(enabled bool, maxBodyBytes int) (*OpenAIGatewayService, *requestLogRepoStub) {
	repo := &requestLogRepoStub{}
	cfg := &config.Config{}
	cfg.Gateway.RequestLog.Enabled = enabled
	cfg.Gateway.RequestLog.MaxBodyBytes = maxBodyBytes
	svc := &OpenAIGatewayService{cfg: cfg, requestLogRepo: repo}
	return svc, repo
}

func TestWriteRequestLog_DisabledByDefault(t *testing.T) {
	svc, repo := newGatewayWithRequestLog(false, 0)
	svc.WriteRequestLog(context.Background(), "req-1", "sess-1", 42, "input", "output")
	if repo.last() != nil {
		t.Fatal("expected no write when disabled")
	}
}

func TestWriteRequestLog_Enabled(t *testing.T) {
	svc, repo := newGatewayWithRequestLog(true, 0)
	svc.WriteRequestLog(context.Background(), "req-1", "sess-1", 42, "input", "output")

	// CreateBestEffort 是同步调用 stub，无需等待
	got := repo.last()
	if got == nil {
		t.Fatal("expected log to be written")
	}
	if got.RequestID != "req-1" || got.SessionID != "sess-1" || got.UserID != 42 {
		t.Errorf("unexpected fields: %+v", got)
	}
	if got.RequestBody != "input" || got.ResponseBody != "output" {
		t.Errorf("unexpected body: req=%q resp=%q", got.RequestBody, got.ResponseBody)
	}
}

func TestWriteRequestLog_TruncatesRequestBody(t *testing.T) {
	svc, repo := newGatewayWithRequestLog(true, 5)
	svc.WriteRequestLog(context.Background(), "req-2", "", 1, "123456789", "ok")

	got := repo.last()
	if got == nil {
		t.Fatal("expected log to be written")
	}
	if got.RequestBody != "12345" {
		t.Errorf("expected truncated to 5 bytes, got %q", got.RequestBody)
	}
}

func TestWriteRequestLog_ZeroMaxBodyBytesNoTruncation(t *testing.T) {
	svc, repo := newGatewayWithRequestLog(true, 0)
	longBody := string(make([]byte, 200*1024)) // 200 KB
	svc.WriteRequestLog(context.Background(), "req-3", "", 1, longBody, "ok")

	got := repo.last()
	if got == nil {
		t.Fatal("expected log to be written")
	}
	if len(got.RequestBody) != len(longBody) {
		t.Errorf("expected no truncation: got %d bytes, want %d", len(got.RequestBody), len(longBody))
	}
}

func TestWriteRequestLog_NilRepo(t *testing.T) {
	svc := &OpenAIGatewayService{
		cfg:            &config.Config{},
		requestLogRepo: nil,
	}
	svc.cfg.Gateway.RequestLog.Enabled = true
	// 不应 panic
	svc.WriteRequestLog(context.Background(), "req-4", "", 1, "body", "resp")
}

func TestWriteRequestLog_CreatedAtSet(t *testing.T) {
	svc, repo := newGatewayWithRequestLog(true, 0)
	before := time.Now()
	svc.WriteRequestLog(context.Background(), "req-5", "", 1, "a", "b")
	after := time.Now()

	got := repo.last()
	if got == nil {
		t.Fatal("expected log")
	}
	if got.CreatedAt.Before(before) || got.CreatedAt.After(after) {
		t.Errorf("CreatedAt %v out of range [%v, %v]", got.CreatedAt, before, after)
	}
}
