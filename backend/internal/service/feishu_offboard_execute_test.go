// 私有扩展（不属于 upstream sub2api）。
//
// 本文件：执行层单测——熔断、dry-run、只改 status、admin 保护。
// merge 策略：纯新增文件。
//
// 这些用例锁住的都是安全保证：一旦被改坏，后果是凌晨批量误禁在职员工，
// 所以断言写得比较严（连"有没有多改一列"都验）。
//
// @author wangzhong
package service

import (
	"context"
	"errors"
	"testing"
)

// fakeUserRepoForOffboard 只实现本测试需要的方法，其余靠嵌入接口占位。
type fakeUserRepoForOffboard struct {
	UserRepository
	users map[int64]*User
	// updates 记录每次 Update 传入的字段掩码，用于验证只动了 status
	updates    []UserUpdateFields
	updateErr  error
	getErr     error
	updateSeen []int64
}

func (f *fakeUserRepoForOffboard) GetByID(_ context.Context, id int64) (*User, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	u, ok := f.users[id]
	if !ok {
		return nil, errors.New("not found")
	}
	// 返回副本，避免测试里被就地改写而看不出问题
	cp := *u
	return &cp, nil
}

func (f *fakeUserRepoForOffboard) Update(
	_ context.Context, user *User, fields UserUpdateFields,
) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updates = append(f.updates, fields)
	f.updateSeen = append(f.updateSeen, user.ID)
	if stored, ok := f.users[user.ID]; ok {
		stored.Status = user.Status
	}
	return nil
}

type fakeCacheInvalidator struct {
	invalidated []int64
}

func (f *fakeCacheInvalidator) InvalidateAuthCacheByKey(context.Context, string)    {}
func (f *fakeCacheInvalidator) InvalidateAuthCacheByGroupID(context.Context, int64) {}
func (f *fakeCacheInvalidator) InvalidateAuthCacheByUserID(_ context.Context, id int64) {
	f.invalidated = append(f.invalidated, id)
}

func TestCircuitBreaker_TripsAboveThreshold(t *testing.T) {
	mk := func(n int) []OffboardDecision {
		out := make([]OffboardDecision, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, OffboardDecision{Verdict: OffboardVerdictResigned})
		}
		return out
	}

	cases := []struct {
		name      string
		hits      int
		threshold int
		wantBroke bool
	}{
		{"低于阈值不熔断", 14, 15, false},
		{"等于阈值不熔断", 15, 15, false},
		{"超过阈值熔断", 16, 15, true},
		{"阈值为0时回落默认15", 16, 0, true},
		{"零命中不熔断", 0, 15, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkCircuitBreaker(mk(tc.hits), tc.threshold)
			if got.Broken != tc.wantBroke {
				t.Fatalf("命中 %d / 阈值 %d：期望 broken=%v，实际 %v",
					tc.hits, tc.threshold, tc.wantBroke, got.Broken)
			}
			if got.HitCount != tc.hits {
				t.Errorf("命中数应为 %d，实际 %d", tc.hits, got.HitCount)
			}
		})
	}
}

// 熔断只统计 resigned，其他结论不该把阈值顶爆。
func TestCircuitBreaker_OnlyCountsResigned(t *testing.T) {
	decisions := []OffboardDecision{
		{Verdict: OffboardVerdictResigned},
		{Verdict: OffboardVerdictUnverifiable},
		{Verdict: OffboardVerdictUnverifiable},
		{Verdict: OffboardVerdictInService},
		{Verdict: OffboardVerdictSkipAdmin},
		{Verdict: OffboardVerdictFrozen},
	}
	got := checkCircuitBreaker(decisions, 2)
	if got.Broken {
		t.Fatalf("只有 1 个 resigned 不应熔断，实际熔断（命中数 %d）", got.HitCount)
	}
	if got.HitCount != 1 {
		t.Errorf("命中数应为 1，实际 %d", got.HitCount)
	}
}

func TestApplyDecisions_OnlyUpdatesStatusColumn(t *testing.T) {
	repo := &fakeUserRepoForOffboard{users: map[int64]*User{
		7: {ID: 7, Email: "a@g7.com.cn", Status: StatusActive, Role: RoleUser},
	}}
	inv := &fakeCacheInvalidator{}
	e := &offboardExecutor{userRepo: repo, authCacheInvalidator: inv}

	decisions := []OffboardDecision{{UserID: 7, Verdict: OffboardVerdictResigned}}
	n := e.applyDecisions(context.Background(), decisions, false)

	if n != 1 || !decisions[0].Disabled {
		t.Fatalf("应成功禁用 1 人，实际 n=%d disabled=%v err=%q",
			n, decisions[0].Disabled, decisions[0].DisableError)
	}
	if repo.users[7].Status != StatusDisabled {
		t.Errorf("状态应为 disabled，实际 %q", repo.users[7].Status)
	}
	if len(repo.updates) != 1 {
		t.Fatalf("应只调用一次 Update，实际 %d 次", len(repo.updates))
	}
	// 关键断言：只有 Status 掩码为 true。多改一列就可能清掉余额或分组权限。
	f := repo.updates[0]
	if !f.Status {
		t.Error("Status 掩码必须为 true")
	}
	if f.Email || f.Username || f.Notes || f.Role || f.Concurrency ||
		f.RPMLimit || f.AllowedGroups || f.PasswordHash ||
		f.BalanceNotifySettings || f.BalanceNotifyExtraEmails {
		t.Errorf("除 Status 外不得声明其他字段，实际掩码 %+v", f)
	}
	// 不失效鉴权缓存的话，已签发的 API Key 在缓存 TTL 内仍可用，禁用就不是即时的。
	if len(inv.invalidated) != 1 || inv.invalidated[0] != 7 {
		t.Errorf("应失效 user 7 的鉴权缓存，实际 %v", inv.invalidated)
	}
}

func TestApplyDecisions_DryRunWritesNothing(t *testing.T) {
	repo := &fakeUserRepoForOffboard{users: map[int64]*User{
		7: {ID: 7, Status: StatusActive, Role: RoleUser},
	}}
	inv := &fakeCacheInvalidator{}
	e := &offboardExecutor{userRepo: repo, authCacheInvalidator: inv}

	decisions := []OffboardDecision{{UserID: 7, Verdict: OffboardVerdictResigned}}
	n := e.applyDecisions(context.Background(), decisions, true)

	if n != 0 {
		t.Fatalf("dry-run 不应禁用任何人，实际 %d", n)
	}
	if decisions[0].Disabled {
		t.Error("dry-run 下 Disabled 必须保持 false")
	}
	if len(repo.updates) != 0 {
		t.Errorf("dry-run 不应调用 Update，实际 %d 次", len(repo.updates))
	}
	if repo.users[7].Status != StatusActive {
		t.Errorf("dry-run 后状态应保持 active，实际 %q", repo.users[7].Status)
	}
	if len(inv.invalidated) != 0 {
		t.Errorf("dry-run 不应失效缓存，实际 %v", inv.invalidated)
	}
}

// 只有 resigned 会被禁用，其他结论一律放过。
func TestApplyDecisions_SkipsNonResignedVerdicts(t *testing.T) {
	repo := &fakeUserRepoForOffboard{users: map[int64]*User{
		1: {ID: 1, Status: StatusActive, Role: RoleUser},
		2: {ID: 2, Status: StatusActive, Role: RoleUser},
		3: {ID: 3, Status: StatusActive, Role: RoleUser},
		4: {ID: 4, Status: StatusActive, Role: RoleUser},
	}}
	e := &offboardExecutor{userRepo: repo, authCacheInvalidator: &fakeCacheInvalidator{}}

	decisions := []OffboardDecision{
		{UserID: 1, Verdict: OffboardVerdictUnverifiable},
		{UserID: 2, Verdict: OffboardVerdictInService},
		{UserID: 3, Verdict: OffboardVerdictFrozen},
		{UserID: 4, Verdict: OffboardVerdictSkipAdmin},
	}
	if n := e.applyDecisions(context.Background(), decisions, false); n != 0 {
		t.Fatalf("非 resigned 结论不应禁用任何人，实际 %d", n)
	}
	if len(repo.updateSeen) != 0 {
		t.Errorf("不应有任何 Update 调用，实际动了 %v", repo.updateSeen)
	}
}

// admin 即使被判定为离职也不能禁（防判定与执行之间的提权、或绕过判定直接传 decision）。
func TestDisableOne_RefusesAdmin(t *testing.T) {
	repo := &fakeUserRepoForOffboard{users: map[int64]*User{
		9: {ID: 9, Status: StatusActive, Role: RoleAdmin},
	}}
	e := &offboardExecutor{userRepo: repo, authCacheInvalidator: &fakeCacheInvalidator{}}

	decisions := []OffboardDecision{{UserID: 9, Verdict: OffboardVerdictResigned}}
	n := e.applyDecisions(context.Background(), decisions, false)

	if n != 0 {
		t.Fatalf("不应禁用 admin，实际禁用 %d 人", n)
	}
	if decisions[0].DisableError == "" {
		t.Error("应记录拒绝原因以便追溯")
	}
	if repo.users[9].Status != StatusActive {
		t.Errorf("admin 状态不应被改动，实际 %q", repo.users[9].Status)
	}
}

// 已禁用的账号重复执行是幂等的。
func TestDisableOne_IdempotentOnAlreadyDisabled(t *testing.T) {
	repo := &fakeUserRepoForOffboard{users: map[int64]*User{
		5: {ID: 5, Status: StatusDisabled, Role: RoleUser},
	}}
	e := &offboardExecutor{userRepo: repo, authCacheInvalidator: &fakeCacheInvalidator{}}

	decisions := []OffboardDecision{{UserID: 5, Verdict: OffboardVerdictResigned}}
	n := e.applyDecisions(context.Background(), decisions, false)

	if n != 1 || !decisions[0].Disabled {
		t.Fatalf("已禁用账号应幂等成功，实际 n=%d err=%q", n, decisions[0].DisableError)
	}
	if len(repo.updates) != 0 {
		t.Errorf("已是 disabled 不应再写库，实际 %d 次 Update", len(repo.updates))
	}
}

// 单人禁用失败不能中断整批，且要记录原因。
func TestApplyDecisions_PartialFailureDoesNotAbortBatch(t *testing.T) {
	repo := &fakeUserRepoForOffboard{
		users: map[int64]*User{
			1: {ID: 1, Status: StatusActive, Role: RoleUser},
		},
		// user 2 不存在 → GetByID 失败
	}
	e := &offboardExecutor{userRepo: repo, authCacheInvalidator: &fakeCacheInvalidator{}}

	decisions := []OffboardDecision{
		{UserID: 2, Verdict: OffboardVerdictResigned}, // 先失败
		{UserID: 1, Verdict: OffboardVerdictResigned}, // 后成功
	}
	n := e.applyDecisions(context.Background(), decisions, false)

	if n != 1 {
		t.Fatalf("应有 1 人成功，实际 %d", n)
	}
	if decisions[0].DisableError == "" {
		t.Error("失败项应记录 DisableError")
	}
	if !decisions[1].Disabled {
		t.Error("前一项失败不应影响后一项")
	}
}
