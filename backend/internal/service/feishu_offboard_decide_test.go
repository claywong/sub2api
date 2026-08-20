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
	"strings"
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

// 候选里只有一条邮箱能对上、其余属于别人时，只依据匹配那条判定，
// 不能被其他候选的离职状态带偏。
//
// 真实数据里更常见的形态是多条记录都能匹配上邮箱
// （见 TestDecide_SamePersonTwoAccounts_*，实测 15/281 个用户是那种），
// 单条匹配这一路径同样需要守住。
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

// 生产实测：hejiacheng@g7.com.cn 返回 2 条，同名、同工号 2781、
// 两条 enterprise_email 都精确匹配，但一条 is_resigned=true、另一条 false
// （离职后回归，旧账号未清理）。飞书把离职那条排在前面。
//
// 这条用例锁的是一个真实缺陷：早先的实现"第一条邮箱匹配就返回"，
// 会把这个当天仍在正常使用、余额 900+ 的在职员工禁掉。
// 规则改为「任一条在职即判在职」后才修复。
func TestDecide_SamePersonTwoAccounts_AnyActiveMeansInService(t *testing.T) {
	email := "hejiacheng@g7.com.cn"
	cli := &fakeFeishuClient{
		byEmail: map[string][]FeishuUserCandidate{
			// 顺序照抄飞书实际返回：离职那条在前
			email: {
				{OpenID: "ou_resigned", Email: email, Status: statusOf(true, true, false)},
				{OpenID: "ou_active", Email: email, Status: statusOf(false, true, false)},
			},
		},
		byOpenID: map[string]*FeishuUserDetail{
			"ou_resigned": {OpenID: "ou_resigned", Name: "何佳诚", EmployeeNo: "2781",
				EnterpriseEmail: email, Status: statusOf(true, true, false)},
			"ou_active": {OpenID: "ou_active", Name: "何佳诚", EmployeeNo: "2781",
				JobTitle:        "软件交付工程师",
				EnterpriseEmail: email, Status: statusOf(false, true, false)},
		},
	}

	got := decideSingle(t, cli, User{ID: 12, Email: email, Username: "何佳诚", Role: RoleUser})

	if got.Verdict != OffboardVerdictInService {
		t.Fatalf("存在在职账号时必须判 in_service，实际 %q（原因：%s）",
			got.Verdict, got.Reason)
	}
	if got.FeishuOpenID != "ou_active" {
		t.Errorf("应采纳在职那条 ou_active 作为依据，实际 %q", got.FeishuOpenID)
	}
	if got.MatchedCount != 2 {
		t.Errorf("应记录 2 条邮箱匹配记录以便追溯，实际 %d", got.MatchedCount)
	}
	// 报告里必须说明"有离职记录但没禁"，否则复核的人会以为系统漏了。
	if !strings.Contains(got.Reason, "离职") {
		t.Errorf("Reason 应说明存在离职记录却未禁用的原因，实际 %q", got.Reason)
	}
}

// 顺序反过来（在职那条在前）结论必须一致——不能依赖飞书返回顺序。
func TestDecide_SamePersonTwoAccounts_OrderIndependent(t *testing.T) {
	email := "rehire-order@g7.com.cn"
	mk := func(first, second FeishuUserCandidate) OffboardVerdict {
		cli := &fakeFeishuClient{
			byEmail: map[string][]FeishuUserCandidate{email: {first, second}},
			byOpenID: map[string]*FeishuUserDetail{
				"ou_r": {OpenID: "ou_r", EnterpriseEmail: email,
					Status: statusOf(true, true, false)},
				"ou_a": {OpenID: "ou_a", EnterpriseEmail: email,
					Status: statusOf(false, true, false)},
			},
		}
		return decideSingle(t, cli,
			User{ID: 1, Email: email, Role: RoleUser}).Verdict
	}
	r := FeishuUserCandidate{OpenID: "ou_r", Email: email, Status: statusOf(true, true, false)}
	a := FeishuUserCandidate{OpenID: "ou_a", Email: email, Status: statusOf(false, true, false)}

	if v1, v2 := mk(r, a), mk(a, r); v1 != v2 {
		t.Fatalf("结论不能依赖飞书返回顺序：离职在前=%q 在职在前=%q", v1, v2)
	} else if v1 != OffboardVerdictInService {
		t.Fatalf("两种顺序都应判 in_service，实际 %q", v1)
	}
}

// 全部匹配记录都离职 → 才判离职。
func TestDecide_AllMatchedResigned_IsResigned(t *testing.T) {
	email := "allgone@g7.com.cn"
	cli := &fakeFeishuClient{
		byEmail: map[string][]FeishuUserCandidate{
			email: {
				{OpenID: "ou_1", Email: email, Status: statusOf(true, false, false)},
				{OpenID: "ou_2", Email: email, Status: statusOf(true, true, false)},
			},
		},
		byOpenID: map[string]*FeishuUserDetail{
			"ou_1": {OpenID: "ou_1", EnterpriseEmail: email, Status: statusOf(true, false, false)},
			"ou_2": {OpenID: "ou_2", EnterpriseEmail: email, Status: statusOf(true, true, false)},
		},
	}

	got := decideSingle(t, cli, User{ID: 2, Email: email, Role: RoleUser})

	if got.Verdict != OffboardVerdictResigned {
		t.Fatalf("全部匹配记录均离职时应判 resigned，实际 %q", got.Verdict)
	}
	if got.MatchedCount != 2 {
		t.Errorf("MatchedCount 应为 2，实际 %d", got.MatchedCount)
	}
}

// 离职 + 冻结、无在职记录 → 交人工，不禁用。
func TestDecide_ResignedPlusFrozen_NoActive_IsNotDisabled(t *testing.T) {
	email := "mixed@g7.com.cn"
	cli := &fakeFeishuClient{
		byEmail: map[string][]FeishuUserCandidate{
			email: {
				{OpenID: "ou_r", Email: email, Status: statusOf(true, false, false)},
				{OpenID: "ou_f", Email: email, Status: statusOf(false, true, true)},
			},
		},
		byOpenID: map[string]*FeishuUserDetail{
			"ou_r": {OpenID: "ou_r", EnterpriseEmail: email, Status: statusOf(true, false, false)},
			"ou_f": {OpenID: "ou_f", EnterpriseEmail: email, Status: statusOf(false, true, true)},
		},
	}

	got := decideSingle(t, cli, User{ID: 3, Email: email, Role: RoleUser})

	if got.Verdict == OffboardVerdictResigned {
		t.Fatalf("状态不一致（离职+冻结）时不应禁用，实际 %q", got.Verdict)
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
