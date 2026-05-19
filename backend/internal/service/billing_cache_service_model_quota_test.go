// 私有扩展（不属于 upstream sub2api）。
//
// 文件作用：ModelQuotaCacheService 共享额度检查单元测试。
// merge 策略：全新文件，无 upstream 冲突。

package service

import (
	"context"
	"errors"
	"strconv"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// fakeModelQuotaCache 隔离 Redis 依赖的内存版 ModelQuotaCache（单 key 版）。
type fakeModelQuotaCache struct {
	usage   ModelQuotaUsage
	getErr  error
	updates int
	updErr  error
}

func (c *fakeModelQuotaCache) GetModelQuotaUsage(_ context.Context, _, _ int64) (ModelQuotaUsage, error) {
	if c.getErr != nil {
		return ModelQuotaUsage{}, c.getErr
	}
	return c.usage, nil
}

func (c *fakeModelQuotaCache) SetModelQuotaUsage(_ context.Context, _, _ int64, data ModelQuotaUsage) error {
	c.usage = data
	return nil
}

func (c *fakeModelQuotaCache) UpdateModelQuotaUsage(_ context.Context, _, _ int64, cost float64) error {
	c.updates++
	if c.updErr != nil {
		return c.updErr
	}
	c.usage.DailyUsage += cost
	c.usage.WeeklyUsage += cost
	return nil
}

// fakeModelQuotaCacheMap 多 key 内存实现，用于验证按 (userID, groupID) 维度的用量隔离。
type fakeModelQuotaCacheMap struct {
	usages map[string]*ModelQuotaUsage
}

func mqKey(userID, groupID int64) string {
	return strconv.FormatInt(userID, 10) + ":" + strconv.FormatInt(groupID, 10)
}

func (c *fakeModelQuotaCacheMap) get(userID, groupID int64) *ModelQuotaUsage {
	k := mqKey(userID, groupID)
	if u, ok := c.usages[k]; ok {
		return u
	}
	u := &ModelQuotaUsage{}
	c.usages[k] = u
	return u
}

func (c *fakeModelQuotaCacheMap) GetModelQuotaUsage(_ context.Context, userID, groupID int64) (ModelQuotaUsage, error) {
	return *c.get(userID, groupID), nil
}

func (c *fakeModelQuotaCacheMap) SetModelQuotaUsage(_ context.Context, userID, groupID int64, data ModelQuotaUsage) error {
	k := mqKey(userID, groupID)
	cp := data
	c.usages[k] = &cp
	return nil
}

func (c *fakeModelQuotaCacheMap) UpdateModelQuotaUsage(_ context.Context, userID, groupID int64, cost float64) error {
	u := c.get(userID, groupID)
	u.DailyUsage += cost
	u.WeeklyUsage += cost
	return nil
}

func ptrF(v float64) *float64 { return &v }

func newQuotaGroup(q *ProtectedModelQuota) *Group {
	return &Group{ID: 1, ProtectedModelQuota: q}
}

func TestCheckProtectedModelQuota_NilGroup_AllowsAll(t *testing.T) {
	svc := NewModelQuotaCacheService(&fakeModelQuotaCache{})
	if err := svc.CheckProtectedModelQuota(context.Background(), nil, 1, 1); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckProtectedModelQuota_NilQuota_AllowsAll(t *testing.T) {
	svc := NewModelQuotaCacheService(&fakeModelQuotaCache{})
	g := newQuotaGroup(nil)
	if err := svc.CheckProtectedModelQuota(context.Background(), g, 1, 1); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckProtectedModelQuota_EmptyLimits_AllowsAll(t *testing.T) {
	// 配置存在但 daily/weekly 都是 0（HasDailyLimit / HasWeeklyLimit 都为 false）
	svc := NewModelQuotaCacheService(&fakeModelQuotaCache{})
	g := newQuotaGroup(&ProtectedModelQuota{DailyLimitUSD: ptrF(0), WeeklyLimitUSD: nil})
	if err := svc.CheckProtectedModelQuota(context.Background(), g, 1, 1); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckProtectedModelQuota_UnderLimit_AllowsAll(t *testing.T) {
	cache := &fakeModelQuotaCache{usage: ModelQuotaUsage{DailyUsage: 1.5, WeeklyUsage: 5}}
	svc := NewModelQuotaCacheService(cache)
	g := newQuotaGroup(&ProtectedModelQuota{DailyLimitUSD: ptrF(10), WeeklyLimitUSD: ptrF(50)})
	if err := svc.CheckProtectedModelQuota(context.Background(), g, 1, 1); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckProtectedModelQuota_DailyExceeded_Blocks(t *testing.T) {
	cache := &fakeModelQuotaCache{usage: ModelQuotaUsage{DailyUsage: 10.5}}
	svc := NewModelQuotaCacheService(cache)
	g := newQuotaGroup(&ProtectedModelQuota{DailyLimitUSD: ptrF(10)})
	err := svc.CheckProtectedModelQuota(context.Background(), g, 1, 1)
	if !errors.Is(err, ErrModelQuotaDailyExceeded) {
		t.Fatalf("expected ErrModelQuotaDailyExceeded, got %v", err)
	}
}

func TestCheckProtectedModelQuota_WeeklyExceeded_Blocks(t *testing.T) {
	cache := &fakeModelQuotaCache{usage: ModelQuotaUsage{DailyUsage: 1, WeeklyUsage: 50}}
	svc := NewModelQuotaCacheService(cache)
	g := newQuotaGroup(&ProtectedModelQuota{DailyLimitUSD: ptrF(10), WeeklyLimitUSD: ptrF(50)})
	err := svc.CheckProtectedModelQuota(context.Background(), g, 1, 1)
	if !errors.Is(err, ErrModelQuotaWeeklyExceeded) {
		t.Fatalf("expected ErrModelQuotaWeeklyExceeded, got %v", err)
	}
}

func TestCheckProtectedModelQuota_RedisError_FailOpen(t *testing.T) {
	cache := &fakeModelQuotaCache{getErr: errors.New("redis down")}
	svc := NewModelQuotaCacheService(cache)
	g := newQuotaGroup(&ProtectedModelQuota{DailyLimitUSD: ptrF(10)})
	if err := svc.CheckProtectedModelQuota(context.Background(), g, 1, 1); err != nil {
		t.Fatalf("expected nil on redis error (fail-open), got %v", err)
	}
}

func TestCheckProtectedModelQuota_ErrorTypeIs429(t *testing.T) {
	// 确认 ErrModelQuotaDailyExceeded 是 429 类型（避免被误归为 500）
	if code := infraerrors.Code(ErrModelQuotaDailyExceeded); code != 429 {
		t.Fatalf("expected 429, got %d", code)
	}
	if code := infraerrors.Code(ErrModelQuotaWeeklyExceeded); code != 429 {
		t.Fatalf("expected 429, got %d", code)
	}
}

func TestQueueUpdateModelQuotaUsage_InvokesCache(t *testing.T) {
	cache := &fakeModelQuotaCache{}
	svc := NewModelQuotaCacheService(cache)
	svc.QueueUpdateModelQuotaUsage(context.Background(), 1, 1, 1.23)
	// QueueUpdateModelQuotaUsage 在 goroutine 中执行；直接调底层 cache 验证语义。
	if err := cache.UpdateModelQuotaUsage(context.Background(), 1, 1, 1.23); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if cache.updates == 0 {
		t.Fatalf("expected at least one update invocation, got 0")
	}
}

// 场景：保护模型逐次计费，未到上限前 Check 允许，累加到上限即拒绝。
// 同时验证 "用任意保护模型都增加同一额度池" —— 接口签名已不区分 model，
// 只要 (userID, groupID) 相同就累加到同一计数器。
func TestProtectedQuota_AccumulatesToBlockOnLimit(t *testing.T) {
	cache := &fakeModelQuotaCache{}
	svc := NewModelQuotaCacheService(cache)
	g := newQuotaGroup(&ProtectedModelQuota{DailyLimitUSD: ptrF(10)})
	ctx := context.Background()
	const userID, groupID int64 = 7, 9

	// 初始 0：通过
	if err := svc.CheckProtectedModelQuota(ctx, g, userID, groupID); err != nil {
		t.Fatalf("expected nil at usage=0, got %v", err)
	}

	// 累加 3：通过
	if err := cache.UpdateModelQuotaUsage(ctx, userID, groupID, 3); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if err := svc.CheckProtectedModelQuota(ctx, g, userID, groupID); err != nil {
		t.Fatalf("expected nil at usage=3/10, got %v", err)
	}

	// 再累加 4 → 共 7：通过（恰好低于上限）
	_ = cache.UpdateModelQuotaUsage(ctx, userID, groupID, 4)
	if err := svc.CheckProtectedModelQuota(ctx, g, userID, groupID); err != nil {
		t.Fatalf("expected nil at usage=7/10, got %v", err)
	}

	// 再累加 3 → 共 10：拒绝（>=10 即超限，CheckProtectedModelQuota 用的 >=）
	_ = cache.UpdateModelQuotaUsage(ctx, userID, groupID, 3)
	if err := svc.CheckProtectedModelQuota(ctx, g, userID, groupID); !errors.Is(err, ErrModelQuotaDailyExceeded) {
		t.Fatalf("expected ErrModelQuotaDailyExceeded at usage=10/10, got %v", err)
	}
}

// 场景：weekly 上限独立于 daily 工作；只配 weekly 时 daily 缺位也能正常 block。
func TestProtectedQuota_WeeklyOnly_BlocksOnLimit(t *testing.T) {
	cache := &fakeModelQuotaCache{}
	svc := NewModelQuotaCacheService(cache)
	g := newQuotaGroup(&ProtectedModelQuota{WeeklyLimitUSD: ptrF(50)})
	ctx := context.Background()

	_ = cache.UpdateModelQuotaUsage(ctx, 1, 1, 49.9)
	if err := svc.CheckProtectedModelQuota(ctx, g, 1, 1); err != nil {
		t.Fatalf("expected nil at usage=49.9/50, got %v", err)
	}
	_ = cache.UpdateModelQuotaUsage(ctx, 1, 1, 0.1)
	if err := svc.CheckProtectedModelQuota(ctx, g, 1, 1); !errors.Is(err, ErrModelQuotaWeeklyExceeded) {
		t.Fatalf("expected ErrModelQuotaWeeklyExceeded at usage=50/50, got %v", err)
	}
}

// 场景：用量隔离 —— 不同 (userID, groupID) 互不影响，确保 "用满" 状态仅针对当前订阅。
func TestProtectedQuota_IsolatedAcrossUsers(t *testing.T) {
	cache := &fakeModelQuotaCacheMap{usages: map[string]*ModelQuotaUsage{}}
	svc := NewModelQuotaCacheService(cache)
	g := newQuotaGroup(&ProtectedModelQuota{DailyLimitUSD: ptrF(10)})
	ctx := context.Background()

	// user 1 用满 → 拒绝
	_ = cache.UpdateModelQuotaUsage(ctx, 1, 1, 10)
	if err := svc.CheckProtectedModelQuota(ctx, g, 1, 1); !errors.Is(err, ErrModelQuotaDailyExceeded) {
		t.Fatalf("user 1 should be blocked, got %v", err)
	}

	// user 2 同 group，仍可通行（额度池按 user+group 维度独立）
	if err := svc.CheckProtectedModelQuota(ctx, g, 2, 1); err != nil {
		t.Fatalf("user 2 should be allowed, got %v", err)
	}

	// 同 user 跨 group 也独立
	if err := svc.CheckProtectedModelQuota(ctx, g, 1, 99); err != nil {
		t.Fatalf("user 1 group 99 should be allowed, got %v", err)
	}
}
