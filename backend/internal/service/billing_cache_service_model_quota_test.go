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
	"time"

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

// 测试用常量：protectedModel 必匹配 newQuotaGroup 默认 ProtectedModels；
// regularModel 不匹配（用于"普通模型不消耗共享额度"场景）。
const (
	protectedModel = "claude-opus-4.7"
	regularModel   = "gpt-5"
)

func newQuotaGroup(q *ProtectedModelQuota) *Group {
	return &Group{
		ID:                  1,
		ProtectedModels:     []string{"claude-opus-*"},
		ProtectedModelQuota: q,
	}
}

func TestCheckProtectedModelQuota_NilGroup_AllowsAll(t *testing.T) {
	svc := NewModelQuotaCacheService(&fakeModelQuotaCache{})
	if err := svc.CheckProtectedModelQuota(context.Background(), nil, protectedModel, 1, 1); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckProtectedModelQuota_NilQuota_AllowsAll(t *testing.T) {
	svc := NewModelQuotaCacheService(&fakeModelQuotaCache{})
	g := newQuotaGroup(nil)
	if err := svc.CheckProtectedModelQuota(context.Background(), g, protectedModel, 1, 1); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckProtectedModelQuota_EmptyLimits_AllowsAll(t *testing.T) {
	// 配置存在但 daily/weekly 都是 0（HasDailyLimit / HasWeeklyLimit 都为 false）
	svc := NewModelQuotaCacheService(&fakeModelQuotaCache{})
	g := newQuotaGroup(&ProtectedModelQuota{DailyLimitUSD: ptrF(0), WeeklyLimitUSD: nil})
	if err := svc.CheckProtectedModelQuota(context.Background(), g, protectedModel, 1, 1); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckProtectedModelQuota_UnderLimit_AllowsAll(t *testing.T) {
	cache := &fakeModelQuotaCache{usage: ModelQuotaUsage{DailyUsage: 1.5, WeeklyUsage: 5}}
	svc := NewModelQuotaCacheService(cache)
	g := newQuotaGroup(&ProtectedModelQuota{DailyLimitUSD: ptrF(10), WeeklyLimitUSD: ptrF(50)})
	if err := svc.CheckProtectedModelQuota(context.Background(), g, protectedModel, 1, 1); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckProtectedModelQuota_DailyExceeded_Blocks(t *testing.T) {
	cache := &fakeModelQuotaCache{usage: ModelQuotaUsage{DailyUsage: 10.5}}
	svc := NewModelQuotaCacheService(cache)
	g := newQuotaGroup(&ProtectedModelQuota{DailyLimitUSD: ptrF(10)})
	err := svc.CheckProtectedModelQuota(context.Background(), g, protectedModel, 1, 1)
	if !errors.Is(err, ErrModelQuotaDailyExceeded) {
		t.Fatalf("expected ErrModelQuotaDailyExceeded, got %v", err)
	}
}

func TestCheckProtectedModelQuota_WeeklyExceeded_Blocks(t *testing.T) {
	cache := &fakeModelQuotaCache{usage: ModelQuotaUsage{DailyUsage: 1, WeeklyUsage: 50}}
	svc := NewModelQuotaCacheService(cache)
	g := newQuotaGroup(&ProtectedModelQuota{DailyLimitUSD: ptrF(10), WeeklyLimitUSD: ptrF(50)})
	err := svc.CheckProtectedModelQuota(context.Background(), g, protectedModel, 1, 1)
	if !errors.Is(err, ErrModelQuotaWeeklyExceeded) {
		t.Fatalf("expected ErrModelQuotaWeeklyExceeded, got %v", err)
	}
}

func TestCheckProtectedModelQuota_RedisError_FailOpen(t *testing.T) {
	cache := &fakeModelQuotaCache{getErr: errors.New("redis down")}
	svc := NewModelQuotaCacheService(cache)
	g := newQuotaGroup(&ProtectedModelQuota{DailyLimitUSD: ptrF(10)})
	if err := svc.CheckProtectedModelQuota(context.Background(), g, protectedModel, 1, 1); err != nil {
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

func TestQueueUpdateProtectedModelUsage_InvokesCacheForProtectedModel(t *testing.T) {
	cache := &fakeModelQuotaCache{}
	svc := NewModelQuotaCacheService(cache)
	g := newQuotaGroup(&ProtectedModelQuota{DailyLimitUSD: ptrF(10)})

	svc.QueueUpdateProtectedModelUsage(context.Background(), g, protectedModel, 1, 1, 1.23)
	// Queue 在 goroutine 中执行；waitFor 轮询 cache.updates 等待 goroutine 落地。
	waitFor(t, func() bool { return cache.updates >= 1 }, "expected cache.updates >= 1")
}

// 场景：普通模型计费不消耗保护额度池 —— Queue 应直接 return 不触达 cache。
func TestQueueUpdateProtectedModelUsage_SkipsRegularModel(t *testing.T) {
	cache := &fakeModelQuotaCache{}
	svc := NewModelQuotaCacheService(cache)
	g := newQuotaGroup(&ProtectedModelQuota{DailyLimitUSD: ptrF(10)})

	svc.QueueUpdateProtectedModelUsage(context.Background(), g, regularModel, 1, 1, 1.23)
	// 给 goroutine 一点窗口期；如果错误地启动了 goroutine 也能被检出。
	waitForFalse(t, func() bool { return cache.updates >= 1 }, "regular model should not touch cache")
}

// 场景：保护模型用满后，普通模型仍可通行（核心需求）。
// 验证 CheckProtectedModelQuota 仅约束保护列表内 model；普通模型即便共享额度池
// 已耗尽也不应被拦截，否则会出现"保护模型超限连带普通模型停摆"的事故。
func TestProtectedQuota_NonProtectedModelStillAllowedAfterExhausted(t *testing.T) {
	cache := &fakeModelQuotaCache{}
	svc := NewModelQuotaCacheService(cache)
	g := newQuotaGroup(&ProtectedModelQuota{DailyLimitUSD: ptrF(10), WeeklyLimitUSD: ptrF(50)})
	ctx := context.Background()

	// 把额度池用到爆（保护模型计费到 daily 限额）
	_ = cache.UpdateModelQuotaUsage(ctx, 1, 1, 10)

	// 保护模型：被拦
	if err := svc.CheckProtectedModelQuota(ctx, g, protectedModel, 1, 1); !errors.Is(err, ErrModelQuotaDailyExceeded) {
		t.Fatalf("protected model should be blocked after quota exhausted, got %v", err)
	}

	// 普通模型：仍放行（关键断言）
	if err := svc.CheckProtectedModelQuota(ctx, g, regularModel, 1, 1); err != nil {
		t.Fatalf("regular model should NOT be blocked by exhausted protected quota, got %v", err)
	}

	// 即便配置了 weekly 限额且也耗尽，普通模型同样不受影响
	_ = cache.UpdateModelQuotaUsage(ctx, 1, 1, 50)
	if err := svc.CheckProtectedModelQuota(ctx, g, regularModel, 1, 1); err != nil {
		t.Fatalf("regular model should bypass weekly exhaustion too, got %v", err)
	}
}

// 辅助：在最多 ~500ms 内轮询条件，避开 goroutine 不确定时序。
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waitFor timeout: %s", msg)
}

// 辅助：~50ms 内 cond 持续为 false 即视为成功（用于断言"不会发生"）。
func waitForFalse(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("waitForFalse fired: %s", msg)
		}
		time.Sleep(5 * time.Millisecond)
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
	if err := svc.CheckProtectedModelQuota(ctx, g, protectedModel, userID, groupID); err != nil {
		t.Fatalf("expected nil at usage=0, got %v", err)
	}

	// 累加 3：通过
	if err := cache.UpdateModelQuotaUsage(ctx, userID, groupID, 3); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if err := svc.CheckProtectedModelQuota(ctx, g, protectedModel, userID, groupID); err != nil {
		t.Fatalf("expected nil at usage=3/10, got %v", err)
	}

	// 再累加 4 → 共 7：通过（恰好低于上限）
	_ = cache.UpdateModelQuotaUsage(ctx, userID, groupID, 4)
	if err := svc.CheckProtectedModelQuota(ctx, g, protectedModel, userID, groupID); err != nil {
		t.Fatalf("expected nil at usage=7/10, got %v", err)
	}

	// 再累加 3 → 共 10：拒绝（>=10 即超限，CheckProtectedModelQuota 用的 >=）
	_ = cache.UpdateModelQuotaUsage(ctx, userID, groupID, 3)
	if err := svc.CheckProtectedModelQuota(ctx, g, protectedModel, userID, groupID); !errors.Is(err, ErrModelQuotaDailyExceeded) {
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
	if err := svc.CheckProtectedModelQuota(ctx, g, protectedModel, 1, 1); err != nil {
		t.Fatalf("expected nil at usage=49.9/50, got %v", err)
	}
	_ = cache.UpdateModelQuotaUsage(ctx, 1, 1, 0.1)
	if err := svc.CheckProtectedModelQuota(ctx, g, protectedModel, 1, 1); !errors.Is(err, ErrModelQuotaWeeklyExceeded) {
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
	if err := svc.CheckProtectedModelQuota(ctx, g, protectedModel, 1, 1); !errors.Is(err, ErrModelQuotaDailyExceeded) {
		t.Fatalf("user 1 should be blocked, got %v", err)
	}

	// user 2 同 group，仍可通行（额度池按 user+group 维度独立）
	if err := svc.CheckProtectedModelQuota(ctx, g, protectedModel, 2, 1); err != nil {
		t.Fatalf("user 2 should be allowed, got %v", err)
	}

	// 同 user 跨 group 也独立
	if err := svc.CheckProtectedModelQuota(ctx, g, protectedModel, 1, 99); err != nil {
		t.Fatalf("user 1 group 99 should be allowed, got %v", err)
	}
}
