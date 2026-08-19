// 私有扩展（不属于 upstream sub2api）。
//
// 本文件：「飞书离职自动禁用」的判定逻辑（只读，不产生副作用）。
// 所含内容：DecideOffboard、decideOne、classifyDetail。
// merge 策略：纯新增文件，与 upstream 无交集，merge 时保留即可。
//
// 判定与执行刻意分在两个文件：本文件只产出结论，不碰数据库、不禁用任何人，
// 因此可以被单测穷举各种飞书返回而没有任何风险。
// 真正的禁用动作在 feishu_offboard_execute.go。
//
// @author wangzhong
package service

import (
	"context"
	"fmt"
	"strings"
)

// offboardDecider 是判定所需的最小依赖集合。
type offboardDecider struct {
	client FeishuContactClient
}

// DecideOffboard 对给定用户批量判定在职状态。
//
// 只返回结论，不做任何写操作。users 应当是 sub2api 侧 status=active 的用户。
//
// 两阶段设计的原因见 decideOne 的注释。这里先做一次批量查询把绝大多数
// 在职的人筛掉，避免对 285 个人都去查详情（那需要 570 次调用且会触发限流）。
func (d *offboardDecider) DecideOffboard(
	ctx context.Context, users []User,
) ([]OffboardDecision, error) {
	if d == nil || d.client == nil {
		return nil, fmt.Errorf("feishu client not configured")
	}
	if len(users) == 0 {
		return nil, nil
	}

	// admin 账号服务端本就禁止禁用（admin_user.go 的 "cannot disable admin user"），
	// 提前分流，省掉无意义的飞书调用。
	targets := make([]User, 0, len(users))
	decisions := make([]OffboardDecision, 0, len(users))
	for _, u := range users {
		if u.IsAdmin() {
			decisions = append(decisions, OffboardDecision{
				UserID:   u.ID,
				Email:    u.Email,
				Username: u.Username,
				Verdict:  OffboardVerdictSkipAdmin,
				Reason:   "管理员账号，服务端禁止禁用",
			})
			continue
		}
		if strings.TrimSpace(u.Email) == "" {
			// 没邮箱就无法与飞书比对身份，只能交人工。
			decisions = append(decisions, OffboardDecision{
				UserID:   u.ID,
				Username: u.Username,
				Verdict:  OffboardVerdictUnverifiable,
				Reason:   "sub2api 侧无邮箱，无法比对飞书身份",
			})
			continue
		}
		targets = append(targets, u)
	}
	if len(targets) == 0 {
		return decisions, nil
	}

	emails := make([]string, 0, len(targets))
	for _, u := range targets {
		emails = append(emails, u.Email)
	}

	candidates, err := d.client.BatchGetUsersByEmails(ctx, emails)
	if err != nil {
		return nil, fmt.Errorf("batch query feishu failed: %w", err)
	}

	byEmail := make(map[string][]FeishuUserCandidate, len(candidates))
	for _, c := range candidates {
		key := strings.ToLower(strings.TrimSpace(c.Email))
		if key == "" {
			continue
		}
		byEmail[key] = append(byEmail[key], c)
	}

	for _, u := range targets {
		key := strings.ToLower(strings.TrimSpace(u.Email))
		decisions = append(decisions, d.decideOne(ctx, u, byEmail[key]))
	}
	return decisions, nil
}

// decideOne 判定单个用户。
//
// 为什么需要第二阶段的详情查询而不能只看批量结果：
//
// 一个邮箱在飞书可能返回多条记录，且分属不同的人。邮箱被回收后重新分配给新人，
// 而历史账号仍与该邮箱关联。实测 zhaoxinxin@g7.com.cn 返回 3 条：
// 2 条是已离职的「赵新鑫」（enterprise_email 为空），1 条是在职的「赵鑫鑫」
// （enterprise_email 精确等于该邮箱）。281 个活跃用户里有 15 个（5.3%）是这种情况。
//
// 如果只看"候选里有没有 is_resigned=true"就判离职，会把在职的赵鑫鑫禁掉。
// 所以必须逐个查详情，用 enterprise_email 精确比对，认定唯一的当事人，
// 只依据那一条的状态做判定。
//
// 单条候选且明确在职时可以跳过详情查询——这是纯粹的性能优化，
// 覆盖了绝大多数用户，且不影响判定正确性（没有第二条记录可混淆）。
func (d *offboardDecider) decideOne(
	ctx context.Context, u User, candidates []FeishuUserCandidate,
) OffboardDecision {
	base := OffboardDecision{
		UserID:         u.ID,
		Email:          u.Email,
		Username:       u.Username,
		CandidateCount: len(candidates),
	}

	if len(candidates) == 0 {
		// 飞书查不到：外部合作方、邮箱不存在、或不在机器人可见范围。
		// 这与"已离职"是完全不同的事，绝不能禁用。
		base.Verdict = OffboardVerdictUnverifiable
		base.Reason = "飞书通讯录查不到该邮箱（可能是外部人员或邮箱不在通讯录）"
		return base
	}

	// 快路径：只有一条候选且明确在职，无歧义可能，不必再查详情。
	if len(candidates) == 1 && candidates[0].Status != nil &&
		!candidates[0].Status.IsResigned && !candidates[0].Status.IsExited &&
		!candidates[0].Status.IsFrozen {
		base.Verdict = OffboardVerdictInService
		base.Reason = "在职"
		base.FeishuOpenID = candidates[0].OpenID
		base.FeishuFlags = candidates[0].Status
		return base
	}

	// 慢路径：逐个查详情，用 enterprise_email 找出真正的当事人。
	var lastErr error
	for _, c := range candidates {
		if strings.TrimSpace(c.OpenID) == "" {
			continue
		}
		detail, err := d.client.GetUserDetail(ctx, c.OpenID)
		if err != nil {
			// 单条查不到不代表结论，继续看其他候选；
			// 全部失败才归为无法核实。
			lastErr = err
			continue
		}
		if !detail.MatchesEmail(u.Email) {
			continue
		}
		return applyDetail(base, detail)
	}

	if lastErr != nil {
		base.Verdict = OffboardVerdictUnverifiable
		base.Reason = fmt.Sprintf("查询飞书详情失败，无法核实：%v", lastErr)
		return base
	}

	// 有候选但没有一条的 enterprise_email 能对上：
	// 说明这些记录属于曾用过该邮箱的其他人（通常是离职后邮箱被回收）。
	// 拿别人的状态判当事人是错的，只能交人工。
	base.Verdict = OffboardVerdictUnverifiable
	base.Reason = fmt.Sprintf(
		"飞书返回 %d 条候选，但无一条邮箱等于 %s（疑为邮箱回收后的历史账号）",
		len(candidates), u.Email)
	return base
}

// applyDetail 依据已确认身份的详情记录得出最终结论。
//
// 判定离职只认 is_resigned / is_exited，不看 is_activated。
// 实测已离职员工普遍是 is_resigned=true 而 is_activated 仍为 true，
// 按 is_activated 判会漏掉几乎所有离职的人；反过来把 is_activated=false
// 当离职则会误伤尚未激活的新人。
func applyDetail(base OffboardDecision, detail *FeishuUserDetail) OffboardDecision {
	base.FeishuOpenID = detail.OpenID
	base.FeishuName = detail.Name
	base.EmployeeNo = detail.EmployeeNo
	base.FeishuFlags = detail.Status

	if detail.Status == nil {
		base.Verdict = OffboardVerdictUnverifiable
		base.Reason = "飞书未返回该用户状态，无法核实"
		return base
	}

	st := detail.Status
	switch {
	case st.IsResigned || st.IsExited:
		base.Verdict = OffboardVerdictResigned
		parts := []string{}
		if st.IsResigned {
			parts = append(parts, "已离职")
		}
		if st.IsExited {
			parts = append(parts, "已退出租户")
		}
		if st.IsActivated {
			// 值得记下来：离职但飞书账号仍激活是常态，
			// 说明不能靠 is_activated 判断。
			parts = append(parts, "飞书账号仍激活")
		}
		base.Reason = strings.Join(parts, "，")
	case st.IsFrozen:
		// 冻结但未标记离职，原因不明（可能停职、可能异常），不擅自禁用。
		base.Verdict = OffboardVerdictFrozen
		base.Reason = "飞书账号被冻结但未标记离职，需人工判断"
	case st.IsUnjoin || !st.IsActivated:
		base.Verdict = OffboardVerdictFrozen
		base.Reason = "飞书账号尚未激活（新人或待入职），不作离职处理"
	default:
		base.Verdict = OffboardVerdictInService
		base.Reason = "在职"
	}
	return base
}

// SummarizeDecisions 汇总各结论的数量，供落库与告警使用。
func SummarizeDecisions(decisions []OffboardDecision) (resigned, unverifiable, skipped, inService int) {
	for _, d := range decisions {
		switch d.Verdict {
		case OffboardVerdictResigned:
			resigned++
		case OffboardVerdictUnverifiable:
			unverifiable++
		case OffboardVerdictSkipAdmin:
			skipped++
		case OffboardVerdictInService, OffboardVerdictFrozen:
			inService++
		}
	}
	return
}
