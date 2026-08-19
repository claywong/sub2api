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
// 一个邮箱在飞书可能返回多条记录，而 batch_get_id 不给出这些记录归属于谁。
// 实测 281 个活跃用户里有 15 个（5.3%）命中此形态，且每一例都是
// "恰好一条在职、其余若干条已离职"，成因有两类：
//   - 同一个人有多个账号（工号相同，如 hejiacheng 的工号均为 2781），
//     典型是离职后回归而旧账号未清理；
//   - 不同的人共用/继承同一邮箱（工号不同，如 zhaoxinxin 下的
//     赵新鑫 3600 与赵鑫鑫 10733）。
//
// 两类都不能只看"候选里有没有 is_resigned=true"，否则会禁掉在职的人。
// 所以必须逐个查详情，用 enterprise_email 精确比对筛出与本人邮箱相符的记录，
// 再由 applyMatchedDetails 按"任一在职即不禁用"裁决。
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

	// 慢路径：逐个查详情，收集**所有** enterprise_email 能对上的记录。
	//
	// 这里必须收集全部而不是"第一条匹配就返回"：同一个人可能有多个飞书账号，
	// 邮箱全都对得上但状态冲突。实测 hejiacheng@g7.com.cn 返回 2 条，
	// 同名、同工号 2781、两条 enterprise_email 都精确匹配，
	// 但一条 is_resigned=true、另一条 false（离职后回归，旧账号未清理）。
	// 按顺序取第一条的话，飞书恰好把离职那条排在前面，
	// 就会禁掉一个当天还在正常使用的在职员工。
	var lastErr error
	matched := make([]*FeishuUserDetail, 0, len(candidates))
	for _, c := range candidates {
		if strings.TrimSpace(c.OpenID) == "" {
			continue
		}
		detail, err := d.client.GetUserDetail(ctx, c.OpenID)
		if err != nil {
			// 单条查不到不代表结论，继续看其他候选；
			// 一条都没匹配上且有报错时才归为无法核实。
			lastErr = err
			continue
		}
		if !detail.MatchesEmail(u.Email) {
			continue
		}
		matched = append(matched, detail)
	}

	if len(matched) > 0 {
		return applyMatchedDetails(base, matched)
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

// applyMatchedDetails 依据所有邮箱匹配成功的记录做最终裁决。
//
// 裁决规则：**任一条显示在职就判在职，只有全部记录都显示离职才判离职。**
//
// 这个不对称是有意的。漏判的代价是一个已离职账号多留一天，下次核查还能抓到；
// 误判的代价是打断一个在职员工的工作，而 sub2api 侧没有操作审计可以一键回滚。
// 两者量级差得远，所以宁可保守。
//
// 同一个人有多个飞书账号是真实存在的（离职后回归、账号迁移未清理），
// 这种情况下"有一个活跃账号"就足以说明人还在职。
func applyMatchedDetails(
	base OffboardDecision, matched []*FeishuUserDetail,
) OffboardDecision {
	base.MatchedCount = len(matched)

	var (
		inService  *FeishuUserDetail
		resigned   *FeishuUserDetail
		restricted *FeishuUserDetail // 冻结 / 未激活
		noStatus   *FeishuUserDetail
	)
	for _, d := range matched {
		if d.Status == nil {
			if noStatus == nil {
				noStatus = d
			}
			continue
		}
		switch {
		case d.Status.IsResigned || d.Status.IsExited:
			if resigned == nil {
				resigned = d
			}
		case d.Status.IsFrozen || d.Status.IsUnjoin || !d.Status.IsActivated:
			if restricted == nil {
				restricted = d
			}
		default:
			if inService == nil {
				inService = d
			}
		}
	}

	// 有任何一条在职 → 判在职。这是本函数存在的核心理由。
	if inService != nil {
		out := applyDetail(base, inService)
		if resigned != nil {
			// 冲突要在报告里说清楚，否则复核的人看到"这人飞书有离职记录却没被禁"
			// 会以为系统漏了。
			out.Reason = fmt.Sprintf(
				"在职（该邮箱在飞书另有 %d 条已离职记录，"+
					"但存在仍在职的账号，按保守规则不禁用；常见于离职后回归未清理旧账号）",
				countResigned(matched))
		}
		return out
	}

	// 无在职记录，但有受限记录（冻结/未激活）→ 交人工，不禁用。
	if restricted != nil {
		out := applyDetail(base, restricted)
		if resigned != nil {
			out.Verdict = OffboardVerdictFrozen
			out.Reason = "该邮箱在飞书同时存在已离职与冻结/未激活的账号，状态不一致，需人工判断"
		}
		return out
	}

	// 全部匹配记录都显示离职 → 判离职。
	if resigned != nil {
		out := applyDetail(base, resigned)
		if len(matched) > 1 {
			out.Reason = fmt.Sprintf("%s（该邮箱在飞书的 %d 条匹配记录均已离职）",
				out.Reason, len(matched))
		}
		return out
	}

	// 只剩没有状态的记录，无法判定。
	if noStatus != nil {
		return applyDetail(base, noStatus)
	}

	base.Verdict = OffboardVerdictUnverifiable
	base.Reason = "飞书匹配记录均无状态信息，无法核实"
	return base
}

func countResigned(matched []*FeishuUserDetail) int {
	n := 0
	for _, d := range matched {
		if d.Status != nil && (d.Status.IsResigned || d.Status.IsExited) {
			n++
		}
	}
	return n
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
