// 私有扩展（不属于 upstream sub2api）。
//
// 本文件：判定逻辑单测。用假客户端穷举飞书可能的返回形态。
// merge 策略：纯新增文件。
//
// 这些用例不是凑覆盖率，每一条都对应一种会导致误禁在职员工的真实情形，
// 其中 TestDecide_EmailReuse_* 两条来自生产数据实测。
//
// @author wangzhong
package service

import (
	"context"
	"errors"
	"testing"
)

// fakeFeishuClient 按邮箱/open_id 返回预设数据。
type fakeFeishuClient struct {
	byEmail   map[string][]FeishuUserCandidate
	byOpenID  map[string]*FeishuUserDetail
	detailErr map[string]error
	batchErr  error
	// detailCalls 记录详情接口被调了几次，用于验证快路径确实省掉了调用。
	detailCalls int
}

func (f *fakeFeishuClient) BatchGetUsersByEmails(
	_ context.Context, emails []string,
) ([]FeishuUserCandidate, error) {
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	out := []FeishuUserCandidate{}
	for _, e := range emails {
		out = append(out, f.byEmail[e]...)
	}
	return out, nil
}

func (f *fakeFeishuClient) GetUserDetail(
	_ context.Context, openID string,
) (*FeishuUserDetail, error) {
	f.detailCalls++
	if err, ok := f.detailErr[openID]; ok {
		return nil, err
	}
	if d, ok := f.byOpenID[openID]; ok {
		return d, nil
	}
	return nil, errors.New("not found")
}

func statusOf(resigned, activated, frozen bool) *FeishuUserStatus {
	return &FeishuUserStatus{
		IsResigned: resigned, IsActivated: activated, IsFrozen: frozen,
	}
}

func decideSingle(t *testing.T, cli FeishuContactClient, u User) OffboardDecision {
	t.Helper()
	d := &offboardDecider{client: cli}
	got, err := d.DecideOffboard(context.Background(), []User{u})
	if err != nil {
		t.Fatalf("DecideOffboard 返回错误: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("期望 1 条判定，实际 %d 条", len(got))
	}
	return got[0]
}

// 生产实测：zhaoxinxin@g7.com.cn 返回 3 条候选，
// 2 条是已离职的「赵新鑫」（enterprise_email 为空），
// 1 条是在职的「赵鑫鑫」（邮箱精确匹配）。
// 只看"有没有 resigned=true"会禁掉在职的赵鑫鑫。
func TestDecide_EmailReuse_InServiceHolderMustNotBeDisabled(t *testing.T) {
	email := "zhaoxinxin@g7.com.cn"
	cli := &fakeFeishuClient{
		byEmail: map[string][]FeishuUserCandidate{
			email: {
				{OpenID: "ou_old1", Email: email, Status: statusOf(true, false, false)},
				{OpenID: "ou_old2", Email: email, Status: statusOf(true, false, false)},
				{OpenID: "ou_cur", Email: email, Status: statusOf(false, true, false)},
			},
		},
		byOpenID: map[string]*FeishuUserDetail{
			// 历史账号：邮箱已被回收，enterprise_email 为空
			"ou_old1": {OpenID: "ou_old1", Name: "赵新鑫", Status: statusOf(true, false, false)},
			"ou_old2": {OpenID: "ou_old2", Name: "赵新鑫", Status: statusOf(true, false, false)},
			// 当前持有者：邮箱精确匹配
			"ou_cur": {OpenID: "ou_cur", Name: "赵鑫鑫",
				EnterpriseEmail: email, Status: statusOf(false, true, false)},
		},
	}

	got := decideSingle(t, cli, User{ID: 1, Email: email, Username: "赵鑫鑫", Role: RoleUser})

	if got.Verdict != OffboardVerdictInService {
		t.Fatalf("邮箱复用场景下当前持有者在职，必须判 in_service，实际 %q（原因：%s）",
			got.Verdict, got.Reason)
	}
	if got.FeishuOpenID != "ou_cur" {
		t.Errorf("应采纳邮箱匹配的那条记录 ou_cur，实际采纳 %q", got.FeishuOpenID)
	}
	if got.CandidateCount != 3 {
		t.Errorf("应记录候选数 3 以便追溯，实际 %d", got.CandidateCount)
	}
}

// 生产实测：wangke@g7.com.cn 返回「王科」（已离职、无邮箱）和
// 「王珂」（已离职、邮箱匹配、工号 9223）两条，当事人确实离职。
func TestDecide_EmailReuse_ResignedHolderIsDetected(t *testing.T) {
	email := "wangke@g7.com.cn"
	cli := &fakeFeishuClient{
		byEmail: map[string][]FeishuUserCandidate{
			email: {
				{OpenID: "ou_wangke_old", Email: email, Status: statusOf(true, false, false)},
				{OpenID: "ou_wangke_cur", Email: email, Status: statusOf(true, true, false)},
			},
		},
		byOpenID: map[string]*FeishuUserDetail{
			"ou_wangke_old": {OpenID: "ou_wangke_old", Name: "王科",
				Status: statusOf(true, false, false)},
			"ou_wangke_cur": {OpenID: "ou_wangke_cur", Name: "王珂", EmployeeNo: "9223",
				EnterpriseEmail: email, Status: statusOf(true, true, false)},
		},
	}

	got := decideSingle(t, cli, User{ID: 2, Email: email, Username: "王珂", Role: RoleUser})

	if got.Verdict != OffboardVerdictResigned {
		t.Fatalf("当事人已离职，应判 resigned，实际 %q（原因：%s）", got.Verdict, got.Reason)
	}
	if got.EmployeeNo != "9223" {
		t.Errorf("应采纳邮箱匹配那条的工号 9223，实际 %q", got.EmployeeNo)
	}
}

// 离职但 is_activated 仍为 true 是生产常态（徐典阳实测）。
// 这条用例锁死"不能靠 is_activated 判断"这个结论。
func TestDecide_ResignedButStillActivated(t *testing.T) {
	email := "xudianyang@g7.com.cn"
	cli := &fakeFeishuClient{
		byEmail: map[string][]FeishuUserCandidate{
			email: {{OpenID: "ou_xdy", Email: email, Status: statusOf(true, true, false)}},
		},
		byOpenID: map[string]*FeishuUserDetail{
			"ou_xdy": {OpenID: "ou_xdy", Name: "徐典阳", EmployeeNo: "3564",
				EnterpriseEmail: email, Status: statusOf(true, true, false)},
		},
	}

	got := decideSingle(t, cli, User{ID: 3, Email: email, Role: RoleUser})

	if got.Verdict != OffboardVerdictResigned {
		t.Fatalf("is_resigned=true 应判离职（无论 is_activated），实际 %q", got.Verdict)
	}
}

// 飞书查不到（外部合作方、邮箱不存在）绝不能当离职。
func TestDecide_NoCandidate_IsUnverifiableNotResigned(t *testing.T) {
	email := "liuqingrui_wb@mail.g7e6.com.cn"
	cli := &fakeFeishuClient{byEmail: map[string][]FeishuUserCandidate{}}

	got := decideSingle(t, cli, User{ID: 4, Email: email, Role: RoleUser})

	if got.Verdict != OffboardVerdictUnverifiable {
		t.Fatalf("查不到的人必须判 unverifiable，绝不能判离职，实际 %q", got.Verdict)
	}
}

// 有候选但没有一条邮箱能对上：这些记录属于别人，不能拿来判当事人。
func TestDecide_CandidatesButNoEmailMatch_IsUnverifiable(t *testing.T) {
	email := "someone@g7.com.cn"
	cli := &fakeFeishuClient{
		byEmail: map[string][]FeishuUserCandidate{
			email: {{OpenID: "ou_other", Email: email, Status: statusOf(true, false, false)}},
		},
		byOpenID: map[string]*FeishuUserDetail{
			// enterprise_email 是别人的
			"ou_other": {OpenID: "ou_other", Name: "另一个人",
				EnterpriseEmail: "other@g7.com.cn", Status: statusOf(true, false, false)},
		},
	}

	got := decideSingle(t, cli, User{ID: 5, Email: email, Role: RoleUser})

	if got.Verdict != OffboardVerdictUnverifiable {
		t.Fatalf("无邮箱匹配项时必须判 unverifiable，实际 %q（原因：%s）",
			got.Verdict, got.Reason)
	}
}

// 详情接口失败不能退化成"离职"。
func TestDecide_DetailError_IsUnverifiable(t *testing.T) {
	email := "err@g7.com.cn"
	cli := &fakeFeishuClient{
		byEmail: map[string][]FeishuUserCandidate{
			email: {{OpenID: "ou_err", Email: email, Status: statusOf(true, true, false)}},
		},
		detailErr: map[string]error{"ou_err": errors.New("41050 no permission")},
	}

	got := decideSingle(t, cli, User{ID: 6, Email: email, Role: RoleUser})

	if got.Verdict != OffboardVerdictUnverifiable {
		t.Fatalf("详情失败必须判 unverifiable，实际 %q", got.Verdict)
	}
}

// 冻结但未离职：原因不明，不擅自禁用。
func TestDecide_FrozenNotResigned_IsNotDisabled(t *testing.T) {
	email := "frozen@g7.com.cn"
	cli := &fakeFeishuClient{
		byEmail: map[string][]FeishuUserCandidate{
			email: {{OpenID: "ou_f", Email: email, Status: statusOf(false, true, true)}},
		},
		byOpenID: map[string]*FeishuUserDetail{
			"ou_f": {OpenID: "ou_f", EnterpriseEmail: email, Status: statusOf(false, true, true)},
		},
	}

	got := decideSingle(t, cli, User{ID: 7, Email: email, Role: RoleUser})

	if got.Verdict != OffboardVerdictFrozen {
		t.Fatalf("冻结未离职应判 frozen，实际 %q", got.Verdict)
	}
}

// 未激活的新人不能当离职。
func TestDecide_NotActivatedNewbie_IsNotResigned(t *testing.T) {
	email := "newbie@g7.com.cn"
	cli := &fakeFeishuClient{
		byEmail: map[string][]FeishuUserCandidate{
			email: {{OpenID: "ou_n", Email: email,
				Status: &FeishuUserStatus{IsUnjoin: true, IsActivated: false}}},
		},
		byOpenID: map[string]*FeishuUserDetail{
			"ou_n": {OpenID: "ou_n", EnterpriseEmail: email,
				Status: &FeishuUserStatus{IsUnjoin: true, IsActivated: false}},
		},
	}

	got := decideSingle(t, cli, User{ID: 8, Email: email, Role: RoleUser})

	if got.Verdict == OffboardVerdictResigned {
		t.Fatalf("未激活的新人不能判离职，实际 %q", got.Verdict)
	}
}

// admin 不查飞书直接跳过。
func TestDecide_AdminIsSkippedWithoutFeishuCall(t *testing.T) {
	cli := &fakeFeishuClient{byEmail: map[string][]FeishuUserCandidate{}}
	got := decideSingle(t, cli, User{ID: 9, Email: "admin@g7.com.cn", Role: RoleAdmin})

	if got.Verdict != OffboardVerdictSkipAdmin {
		t.Fatalf("admin 应判 skip_admin，实际 %q", got.Verdict)
	}
	if cli.detailCalls != 0 {
		t.Errorf("admin 不该触发详情查询，实际调了 %d 次", cli.detailCalls)
	}
}

// 无邮箱用户无法比对身份。
func TestDecide_EmptyEmail_IsUnverifiable(t *testing.T) {
	cli := &fakeFeishuClient{}
	got := decideSingle(t, cli, User{ID: 10, Email: "", Role: RoleUser})

	if got.Verdict != OffboardVerdictUnverifiable {
		t.Fatalf("无邮箱应判 unverifiable，实际 %q", got.Verdict)
	}
}

// 单条候选且明确在职时走快路径，不查详情（性能优化，不影响正确性）。
func TestDecide_SingleActiveCandidate_SkipsDetailCall(t *testing.T) {
	email := "active@g7.com.cn"
	cli := &fakeFeishuClient{
		byEmail: map[string][]FeishuUserCandidate{
			email: {{OpenID: "ou_a", Email: email, Status: statusOf(false, true, false)}},
		},
	}

	got := decideSingle(t, cli, User{ID: 11, Email: email, Role: RoleUser})

	if got.Verdict != OffboardVerdictInService {
		t.Fatalf("应判 in_service，实际 %q", got.Verdict)
	}
	if cli.detailCalls != 0 {
		t.Errorf("单条在职候选应走快路径不查详情，实际调了 %d 次", cli.detailCalls)
	}
}

// 批量接口整体失败应返回错误，而不是把所有人判成某种结论。
func TestDecide_BatchError_ReturnsError(t *testing.T) {
	cli := &fakeFeishuClient{batchErr: errors.New("network down")}
	d := &offboardDecider{client: cli}
	_, err := d.DecideOffboard(context.Background(),
		[]User{{ID: 12, Email: "a@g7.com.cn", Role: RoleUser}})

	if err == nil {
		t.Fatal("批量查询失败时应返回错误，不能静默产出判定")
	}
}

func TestSummarizeDecisions(t *testing.T) {
	got := []OffboardDecision{
		{Verdict: OffboardVerdictResigned},
		{Verdict: OffboardVerdictResigned},
		{Verdict: OffboardVerdictUnverifiable},
		{Verdict: OffboardVerdictSkipAdmin},
		{Verdict: OffboardVerdictInService},
		{Verdict: OffboardVerdictFrozen},
	}
	resigned, unverifiable, skipped, inService := SummarizeDecisions(got)
	if resigned != 2 || unverifiable != 1 || skipped != 1 || inService != 2 {
		t.Fatalf("汇总不符：resigned=%d unverifiable=%d skipped=%d inService=%d",
			resigned, unverifiable, skipped, inService)
	}
}
