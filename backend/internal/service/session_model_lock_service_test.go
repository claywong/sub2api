// 私有扩展（不属于 upstream sub2api）。
//
// 文件作用：SessionModelLockService 单元测试。
// merge 策略：全新文件，无 upstream 冲突。
package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeSessionModelLockCache 一个手写的 in-memory SessionModelLockCache 实现，
// 用于隔离 Redis 依赖。
type fakeSessionModelLockCache struct {
	data    map[string]string
	errOnce error // 下次 SetIfAbsent 调用返回的错误，用完即清
}

func newFakeCache() *fakeSessionModelLockCache {
	return &fakeSessionModelLockCache{data: make(map[string]string)}
}

func fakeKey(groupID int64, sessionID string) string {
	return string(rune(groupID)) + ":" + sessionID
}

func (c *fakeSessionModelLockCache) SetIfAbsent(_ context.Context, groupID int64, sessionID, model string, _ time.Duration) (string, bool, error) {
	if c.errOnce != nil {
		err := c.errOnce
		c.errOnce = nil
		return "", false, err
	}
	k := fakeKey(groupID, sessionID)
	if v, ok := c.data[k]; ok {
		return v, false, nil
	}
	c.data[k] = model
	return "", true, nil
}

// metaUserID 构造一个 JSON 格式的 metadata.user_id，符合 Claude Code 新格式。
func metaUserID(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	return `{"device_id":"d1","account_uuid":"a1","session_id":"` + sessionID + `"}`
}

func newTestParsed(model, sessionID string) *ParsedRequest {
	return &ParsedRequest{
		Model:          model,
		MetadataUserID: metaUserID(sessionID),
	}
}

func newTestGroup(protectedModels []string) *Group {
	return &Group{ID: 7, ProtectedModels: protectedModels}
}

func TestSessionModelLockService_EmptyProtectedList_AllowsAll(t *testing.T) {
	svc := NewSessionModelLockService(newFakeCache())
	err := svc.Check(context.Background(), newTestGroup(nil), newTestParsed("claude-opus-4.7", "s1"))
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestSessionModelLockService_NilGroup_AllowsAll(t *testing.T) {
	svc := NewSessionModelLockService(newFakeCache())
	if err := svc.Check(context.Background(), nil, newTestParsed("claude-opus-4.7", "s1")); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestSessionModelLockService_MissingMetadata_AllowsAll(t *testing.T) {
	svc := NewSessionModelLockService(newFakeCache())
	parsed := &ParsedRequest{Model: "claude-opus-4.7", MetadataUserID: ""}
	if err := svc.Check(context.Background(), newTestGroup([]string{"claude-opus-*"}), parsed); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestSessionModelLockService_FirstRequest_RecordsAndPasses(t *testing.T) {
	cache := newFakeCache()
	svc := NewSessionModelLockService(cache)
	parsed := newTestParsed("claude-sonnet-4-5", "s1")
	if err := svc.Check(context.Background(), newTestGroup([]string{"claude-opus-*"}), parsed); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if v := cache.data[fakeKey(7, "s1")]; v != "claude-sonnet-4-5" {
		t.Fatalf("first model not recorded, got %q", v)
	}
}

func TestSessionModelLockService_SameModelRepeated_Allows(t *testing.T) {
	cache := newFakeCache()
	svc := NewSessionModelLockService(cache)
	group := newTestGroup([]string{"claude-opus-*"})
	for i := 0; i < 3; i++ {
		if err := svc.Check(context.Background(), group, newTestParsed("claude-sonnet-4-5", "s1")); err != nil {
			t.Fatalf("iter %d: expected nil, got %v", i, err)
		}
	}
}

func TestSessionModelLockService_SwitchToNonProtected_Allows(t *testing.T) {
	cache := newFakeCache()
	svc := NewSessionModelLockService(cache)
	group := newTestGroup([]string{"claude-opus-*"})
	// 首次 sonnet（非保护）
	_ = svc.Check(context.Background(), group, newTestParsed("claude-sonnet-4-5", "s1"))
	// 切到另一个非保护模型
	if err := svc.Check(context.Background(), group, newTestParsed("claude-haiku-3.5", "s1")); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestSessionModelLockService_SwitchToProtected_FromNonProtected_Rejects(t *testing.T) {
	cache := newFakeCache()
	svc := NewSessionModelLockService(cache)
	group := newTestGroup([]string{"claude-opus-*"})
	// 首次 sonnet（非保护）
	_ = svc.Check(context.Background(), group, newTestParsed("claude-sonnet-4-5", "s1"))
	// 切到 opus（保护模型）：应被拒绝
	err := svc.Check(context.Background(), group, newTestParsed("claude-opus-4.7", "s1"))
	var lockErr *SessionModelLockError
	if !errors.As(err, &lockErr) {
		t.Fatalf("expected *SessionModelLockError, got %v", err)
	}
	if lockErr.FirstModel != "claude-sonnet-4-5" || lockErr.CurrentModel != "claude-opus-4.7" {
		t.Fatalf("error fields wrong: %+v", lockErr)
	}
}

func TestSessionModelLockService_FirstIsProtected_AllowsSame(t *testing.T) {
	cache := newFakeCache()
	svc := NewSessionModelLockService(cache)
	group := newTestGroup([]string{"claude-opus-*"})
	// 首次就是 opus（保护）
	if err := svc.Check(context.Background(), group, newTestParsed("claude-opus-4.7", "s1")); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	// 同 session 同 model：允许
	if err := svc.Check(context.Background(), group, newTestParsed("claude-opus-4.7", "s1")); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestSessionModelLockService_SwitchBetweenProtected_Rejects(t *testing.T) {
	cache := newFakeCache()
	svc := NewSessionModelLockService(cache)
	group := newTestGroup([]string{"claude-opus-*"})
	// 首次 opus-4.7
	_ = svc.Check(context.Background(), group, newTestParsed("claude-opus-4.7", "s1"))
	// 切到 opus-4.6（同为保护模型）：应被拒绝
	err := svc.Check(context.Background(), group, newTestParsed("claude-opus-4.6", "s1"))
	var lockErr *SessionModelLockError
	if !errors.As(err, &lockErr) {
		t.Fatalf("expected *SessionModelLockError, got %v", err)
	}
}

func TestSessionModelLockService_FirstProtected_SwitchToNonProtected_Allows(t *testing.T) {
	cache := newFakeCache()
	svc := NewSessionModelLockService(cache)
	group := newTestGroup([]string{"claude-opus-*"})
	// 首次 opus（保护模型）
	_ = svc.Check(context.Background(), group, newTestParsed("claude-opus-4.7", "s1"))
	// 切到 sonnet（非保护）：允许
	if err := svc.Check(context.Background(), group, newTestParsed("claude-sonnet-4-5", "s1")); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestSessionModelLockService_WildcardMatches(t *testing.T) {
	cache := newFakeCache()
	svc := NewSessionModelLockService(cache)
	group := newTestGroup([]string{"claude-opus-*", "gpt-5-pro"})

	// claude-opus-4.7 命中 claude-opus-*
	_ = svc.Check(context.Background(), group, newTestParsed("claude-sonnet-4-5", "s1"))
	err := svc.Check(context.Background(), group, newTestParsed("claude-opus-4.7", "s1"))
	if !errors.As(err, new(*SessionModelLockError)) {
		t.Fatalf("opus-4.7 should match claude-opus-*; got %v", err)
	}

	// gpt-5-pro 精确匹配
	_ = svc.Check(context.Background(), group, newTestParsed("gpt-4o-mini", "s2"))
	err = svc.Check(context.Background(), group, newTestParsed("gpt-5-pro", "s2"))
	if !errors.As(err, new(*SessionModelLockError)) {
		t.Fatalf("gpt-5-pro exact match should reject; got %v", err)
	}
}

func TestSessionModelLockService_CacheError_FailsOpen(t *testing.T) {
	cache := newFakeCache()
	cache.errOnce = errors.New("redis down")
	svc := NewSessionModelLockService(cache)
	group := newTestGroup([]string{"claude-opus-*"})
	err := svc.Check(context.Background(), group, newTestParsed("claude-opus-4.7", "s1"))
	if err == nil {
		t.Fatalf("expected non-nil error, got nil")
	}
	var lockErr *SessionModelLockError
	if errors.As(err, &lockErr) {
		t.Fatalf("redis error should NOT be SessionModelLockError, got %v", err)
	}
}

func TestSessionModelLockService_DifferentSessions_Isolated(t *testing.T) {
	cache := newFakeCache()
	svc := NewSessionModelLockService(cache)
	group := newTestGroup([]string{"claude-opus-*"})

	// session s1 首次 sonnet
	_ = svc.Check(context.Background(), group, newTestParsed("claude-sonnet-4-5", "s1"))
	// session s2 首次 opus：应允许（独立会话）
	if err := svc.Check(context.Background(), group, newTestParsed("claude-opus-4.7", "s2")); err != nil {
		t.Fatalf("expected nil for s2 first opus, got %v", err)
	}
}
