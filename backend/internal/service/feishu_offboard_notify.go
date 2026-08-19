// 私有扩展（不属于 upstream sub2api）。
//
// 本文件：「飞书离职自动禁用」的结果邮件通知。
// 所含内容：FeishuOffboardEmailNotifier 及其 NotifyOffboardResult。
// merge 策略：纯新增文件，与 upstream 无交集，merge 时保留即可。
//
// 只通知管理员，不通知被禁用的当事人：人已离职，通知没有意义还可能造成困扰。
//
// @author wangzhong
package service

import (
	"context"
	"fmt"
	"html"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// feishuOffboardMaxListedRows 邮件里最多逐条列出多少人。
// 超出部分只给数量，避免一封邮件几百行没人看。
const feishuOffboardMaxListedRows = 30

// FeishuOffboardEmailNotifier 通过 SMTP 发送执行结果。
type FeishuOffboardEmailNotifier struct {
	emailService *EmailService
	settingRepo  SettingRepository
}

func NewFeishuOffboardEmailNotifier(
	emailService *EmailService, settingRepo SettingRepository,
) *FeishuOffboardEmailNotifier {
	return &FeishuOffboardEmailNotifier{
		emailService: emailService,
		settingRepo:  settingRepo,
	}
}

// NotifyOffboardResult 给管理员发一封执行结果邮件。
//
// to 为空时回落到系统的账号配额通知邮箱，这样管理员不必为这个功能
// 单独再配一遍收件人。
func (n *FeishuOffboardEmailNotifier) NotifyOffboardResult(
	ctx context.Context, run *FeishuOffboardRun, to []string,
) {
	if n == nil || n.emailService == nil || run == nil {
		return
	}
	recipients := n.resolveRecipients(ctx, to)
	if len(recipients) == 0 {
		logger.LegacyPrintf("service.feishu_offboard",
			"[FeishuOffboard] no notify recipients configured, skip email")
		return
	}

	subject := n.buildSubject(ctx, run)
	body := n.buildBody(run)
	for _, addr := range recipients {
		if err := n.emailService.SendEmail(ctx, addr, subject, body); err != nil {
			logger.LegacyPrintf("service.feishu_offboard",
				"[FeishuOffboard] send result email to %s failed: %v", addr, err)
		}
	}
}

func (n *FeishuOffboardEmailNotifier) resolveRecipients(
	ctx context.Context, to []string,
) []string {
	out := make([]string, 0, len(to))
	for _, addr := range to {
		if a := strings.TrimSpace(addr); a != "" {
			out = append(out, a)
		}
	}
	if len(out) > 0 || n.settingRepo == nil {
		return out
	}
	raw, err := n.settingRepo.GetValue(ctx, SettingKeyAccountQuotaNotifyEmails)
	if err != nil || strings.TrimSpace(raw) == "" || raw == "[]" {
		return nil
	}
	return filterVerifiedEmails(ParseNotifyEmails(raw))
}

// buildSubject 让标题一眼能看出严重程度：
// 熔断和真实禁用是需要立刻看的，空跑和纯错误则不必惊动。
func (n *FeishuOffboardEmailNotifier) buildSubject(
	ctx context.Context, run *FeishuOffboardRun,
) string {
	site := "Sub2API"
	if n.settingRepo != nil {
		if name, err := n.settingRepo.GetValue(ctx, SettingKeySiteName); err == nil {
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				site = trimmed
			}
		}
	}

	switch {
	case run.CircuitBroken:
		return fmt.Sprintf("[%s] 离职核查已熔断，命中 %d 人未执行禁用",
			site, run.ResignedCount)
	case run.ErrorMessage != "":
		return fmt.Sprintf("[%s] 离职核查执行失败", site)
	case run.DryRun:
		return fmt.Sprintf("[%s] 离职核查（空跑）命中 %d 人，未实际禁用",
			site, run.ResignedCount)
	default:
		return fmt.Sprintf("[%s] 离职核查已禁用 %d 个账号",
			site, run.DisabledCount)
	}
}

func (n *FeishuOffboardEmailNotifier) buildBody(run *FeishuOffboardRun) string {
	var b strings.Builder
	b.WriteString("<div style=\"font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;font-size:14px;line-height:1.7;color:#1f2937\">")

	if run.CircuitBroken {
		// 熔断是最需要人立刻介入的情况，放在最前面并且给出原因。
		b.WriteString("<p style=\"padding:12px;background:#fef3c7;border-left:4px solid #d97706;margin:0 0 16px\">")
		b.WriteString("<strong>已触发安全熔断，本次未禁用任何账号。</strong><br>")
		b.WriteString(html.EscapeString(run.ErrorMessage))
		b.WriteString("<br>一次命中过多通常意味着飞书数据异常，而非真的集体离职，请人工核对后再手动处理。")
		b.WriteString("</p>")
	} else if run.DryRun {
		b.WriteString("<p style=\"padding:12px;background:#e0f2fe;border-left:4px solid #0284c7;margin:0 0 16px\">")
		b.WriteString("<strong>空跑模式：下列账号被判定为已离职，但本次未实际禁用。</strong>")
		b.WriteString("</p>")
	}

	if run.ErrorMessage != "" && !run.CircuitBroken {
		b.WriteString("<p style=\"padding:12px;background:#fee2e2;border-left:4px solid #dc2626;margin:0 0 16px\">")
		b.WriteString("<strong>执行出错：</strong>")
		b.WriteString(html.EscapeString(run.ErrorMessage))
		b.WriteString("</p>")
	}

	b.WriteString("<h3 style=\"margin:16px 0 8px\">执行概况</h3><ul style=\"margin:0;padding-left:20px\">")
	fmt.Fprintf(&b, "<li>触发方式：%s</li>", triggerLabel(run.TriggerSource))
	fmt.Fprintf(&b, "<li>检查人数：%d</li>", run.CheckedCount)
	fmt.Fprintf(&b, "<li>判定已离职：%d</li>", run.ResignedCount)
	fmt.Fprintf(&b, "<li>实际禁用：%d</li>", run.DisabledCount)
	fmt.Fprintf(&b, "<li>无法核实：%d（飞书查不到或邮箱对不上，未做任何处理）</li>",
		run.UnverifiableCount)
	fmt.Fprintf(&b, "<li>跳过管理员：%d</li>", run.SkippedCount)
	fmt.Fprintf(&b, "<li>耗时：%.1f 秒</li>", float64(run.DurationMs)/1000)
	b.WriteString("</ul>")

	n.writeDecisionTable(&b, run)

	b.WriteString("<p style=\"margin-top:16px;color:#6b7280;font-size:13px\">")
	b.WriteString("被禁用的账号仅修改了状态，余额、分组权限与订阅记录均保留；")
	b.WriteString("人员回归时将状态改回 active 即可恢复，无需重新配置。")
	b.WriteString("</p></div>")
	return b.String()
}

// writeDecisionTable 列出被判定离职的人及其依据。
// 依据要写清楚，这样收到邮件的人能自己判断这次禁用是否合理，
// 而不用再去翻库。
func (n *FeishuOffboardEmailNotifier) writeDecisionTable(
	b *strings.Builder, run *FeishuOffboardRun,
) {
	rows := make([]OffboardDecision, 0, run.ResignedCount)
	for _, d := range run.Decisions {
		if d.Verdict == OffboardVerdictResigned {
			rows = append(rows, d)
		}
	}
	if len(rows) == 0 {
		return
	}

	b.WriteString("<h3 style=\"margin:16px 0 8px\">判定为已离职的账号</h3>")
	b.WriteString("<table style=\"border-collapse:collapse;width:100%;font-size:13px\">")
	b.WriteString("<tr style=\"background:#f3f4f6\">" +
		"<th style=\"padding:6px;border:1px solid #e5e7eb;text-align:left\">用户</th>" +
		"<th style=\"padding:6px;border:1px solid #e5e7eb;text-align:left\">邮箱</th>" +
		"<th style=\"padding:6px;border:1px solid #e5e7eb;text-align:left\">飞书姓名/工号</th>" +
		"<th style=\"padding:6px;border:1px solid #e5e7eb;text-align:left\">依据</th>" +
		"<th style=\"padding:6px;border:1px solid #e5e7eb;text-align:left\">结果</th></tr>")

	limit := len(rows)
	if limit > feishuOffboardMaxListedRows {
		limit = feishuOffboardMaxListedRows
	}
	for _, d := range rows[:limit] {
		result := "已禁用"
		if d.DisableError != "" {
			result = "禁用失败：" + d.DisableError
		} else if !d.Disabled {
			result = "未执行"
		}
		who := d.FeishuName
		if d.EmployeeNo != "" {
			who = fmt.Sprintf("%s / %s", d.FeishuName, d.EmployeeNo)
		}
		reason := d.Reason
		if d.CandidateCount > 1 {
			// 多候选说明该邮箱有历史账号，值得让人知道判定是做过邮箱比对的。
			reason = fmt.Sprintf("%s（飞书 %d 条候选，已按邮箱精确匹配确认当事人）",
				reason, d.CandidateCount)
		}
		fmt.Fprintf(b, "<tr>"+
			"<td style=\"padding:6px;border:1px solid #e5e7eb\">%s</td>"+
			"<td style=\"padding:6px;border:1px solid #e5e7eb\">%s</td>"+
			"<td style=\"padding:6px;border:1px solid #e5e7eb\">%s</td>"+
			"<td style=\"padding:6px;border:1px solid #e5e7eb\">%s</td>"+
			"<td style=\"padding:6px;border:1px solid #e5e7eb\">%s</td></tr>",
			html.EscapeString(d.Username), html.EscapeString(d.Email),
			html.EscapeString(who), html.EscapeString(reason),
			html.EscapeString(result))
	}
	b.WriteString("</table>")
	if len(rows) > limit {
		fmt.Fprintf(b, "<p style=\"color:#6b7280;font-size:13px\">另有 %d 人未在此列出，"+
			"请到管理后台的执行记录中查看完整明细。</p>", len(rows)-limit)
	}
}

func triggerLabel(source string) string {
	if source == OffboardTriggerManual {
		return "管理员手动触发"
	}
	return "定时任务"
}
