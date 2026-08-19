// 私有扩展（不属于 upstream sub2api）。
//
// 本文件：飞书通讯录 API 的最小封装，供「离职自动禁用」功能使用。
// 所含内容：FeishuContactClient 及其 BatchGetUsersByEmails / GetUserDetail。
// merge 策略：纯新增文件，与 upstream 无交集，merge 时保留即可。
//
// 为什么不用 lark-cli 那套「搜索 + 逐个查详情」：
// contact/v3 的 search-user 只支持 user_access_token（需交互式 OAuth），
// 服务端拿不到，所以改用 batch_get_id + 单用户详情，两者都支持
// tenant_access_token（bot 身份），token 由 SDK 自动获取与续期。
//
// @author wangzhong
package service

import (
	"context"
	"fmt"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcontact "github.com/larksuite/oapi-sdk-go/v3/service/contact/v3"
)

// feishuBatchGetIDMaxEmails 是 batch_get_id 单次可传的邮箱上限。
// 飞书文档写明最多 50 条，实测传 51 条直接返回
// 99992402 field validation failed，所以必须由调用方分批。
const feishuBatchGetIDMaxEmails = 50

// FeishuUserStatus 是飞书用户状态的扁平化表示。
//
// SDK 里这些字段都是 *bool，nil 与 false 语义不同（前者是"没返回"）。
// 判定逻辑里反复取值，每次都判空会淹没主干逻辑，所以这里一次性拍平成 bool。
type FeishuUserStatus struct {
	IsFrozen    bool `json:"is_frozen"`
	IsResigned  bool `json:"is_resigned"`
	IsActivated bool `json:"is_activated"`
	IsExited    bool `json:"is_exited"`
	IsUnjoin    bool `json:"is_unjoin"`
}

// FeishuUserCandidate 是 batch_get_id 返回的一个候选账号。
//
// 之所以叫"候选"而不是"用户"：一个邮箱可能返回多条记录，且分属不同的人
// （邮箱被回收再分配给新人，历史账号仍与该邮箱关联）。
// 实测 zhaoxinxin@g7.com.cn 返回 3 条，其中 2 条是已离职的「赵新鑫」、
// 1 条是在职的「赵鑫鑫」。所以这一步的产物只能当候选，
// 必须再用 GetUserDetail 的 enterprise_email 精确比对才能确定谁是当事人。
type FeishuUserCandidate struct {
	OpenID string
	Email  string
	// Status 为 nil 表示飞书没返回状态：该邮箱在通讯录里查不到
	// （外部合作方、邮箱不存在、或不在机器人可见范围）。
	// 这种情况绝不能当作离职，两者是完全不同的事。
	Status *FeishuUserStatus
}

// FeishuUserDetail 是单用户详情的关键字段。
type FeishuUserDetail struct {
	OpenID          string
	Name            string
	EnterpriseEmail string
	Email           string
	EmployeeNo      string
	JobTitle        string
	Status          *FeishuUserStatus
}

// MatchesEmail 判断该详情是否确实对应给定邮箱。
//
// 身份确认只认邮箱精确匹配，不认姓名：同名真实存在，
// 而且邮箱回收后历史账号的 enterprise_email 会变成空，
// 正好可以据此把「曾用过这个邮箱的离职者」与「当前持有者」区分开。
func (d *FeishuUserDetail) MatchesEmail(email string) bool {
	if d == nil {
		return false
	}
	target := strings.ToLower(strings.TrimSpace(email))
	if target == "" {
		return false
	}
	for _, candidate := range []string{d.EnterpriseEmail, d.Email} {
		if strings.ToLower(strings.TrimSpace(candidate)) == target {
			return true
		}
	}
	return false
}

// FeishuContactClient 是通讯录查询的接口抽象，便于单测注入假实现。
type FeishuContactClient interface {
	// BatchGetUsersByEmails 按邮箱批量查候选账号。emails 超过 50 条时由实现分批。
	BatchGetUsersByEmails(ctx context.Context, emails []string) ([]FeishuUserCandidate, error)
	// GetUserDetail 查单个用户详情，用于邮箱精确比对。
	GetUserDetail(ctx context.Context, openID string) (*FeishuUserDetail, error)
}

type feishuContactClient struct {
	cli *lark.Client
}

// NewFeishuContactClient 用 App ID / App Secret 构造客户端。
// SDK 内部管理 tenant_access_token 的获取与续期，无需自己缓存。
func NewFeishuContactClient(appID, appSecret string) (FeishuContactClient, error) {
	appID = strings.TrimSpace(appID)
	appSecret = strings.TrimSpace(appSecret)
	if appID == "" || appSecret == "" {
		return nil, fmt.Errorf("feishu app_id/app_secret not configured")
	}
	return &feishuContactClient{cli: lark.NewClient(appID, appSecret)}, nil
}

func (c *feishuContactClient) BatchGetUsersByEmails(
	ctx context.Context, emails []string,
) ([]FeishuUserCandidate, error) {
	if c == nil || c.cli == nil {
		return nil, fmt.Errorf("feishu client not initialized")
	}
	cleaned := normalizeFeishuEmails(emails)
	if len(cleaned) == 0 {
		return nil, nil
	}

	out := make([]FeishuUserCandidate, 0, len(cleaned))
	for start := 0; start < len(cleaned); start += feishuBatchGetIDMaxEmails {
		end := start + feishuBatchGetIDMaxEmails
		if end > len(cleaned) {
			end = len(cleaned)
		}
		batch, err := c.batchGetOnce(ctx, cleaned[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
	}
	return out, nil
}

func (c *feishuContactClient) batchGetOnce(
	ctx context.Context, emails []string,
) ([]FeishuUserCandidate, error) {
	// IncludeResigned(true) 是这个功能的成立前提：不带这个参数时，
	// 已离职的人只返回 email、不返回 user_id 和 status，
	// 与"外部人员""邮箱不存在"完全无法区分，也就永远判不出离职。
	body := larkcontact.NewBatchGetIdUserReqBodyBuilder().
		Emails(emails).
		IncludeResigned(true).
		Build()

	req := larkcontact.NewBatchGetIdUserReqBuilder().
		UserIdType("open_id").
		Body(body).
		Build()

	resp, err := c.cli.Contact.User.BatchGetId(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("feishu batch_get_id request failed: %w", err)
	}
	if !resp.Success() {
		return nil, fmt.Errorf("feishu batch_get_id failed: code=%d msg=%s log_id=%s",
			resp.Code, resp.Msg, resp.RequestId())
	}
	if resp.Data == nil {
		return nil, nil
	}

	out := make([]FeishuUserCandidate, 0, len(resp.Data.UserList))
	for _, item := range resp.Data.UserList {
		if item == nil {
			continue
		}
		out = append(out, FeishuUserCandidate{
			OpenID: derefString(item.UserId),
			Email:  strings.ToLower(strings.TrimSpace(derefString(item.Email))),
			Status: convertFeishuStatus(item.Status),
		})
	}
	return out, nil
}

func (c *feishuContactClient) GetUserDetail(
	ctx context.Context, openID string,
) (*FeishuUserDetail, error) {
	if c == nil || c.cli == nil {
		return nil, fmt.Errorf("feishu client not initialized")
	}
	openID = strings.TrimSpace(openID)
	if openID == "" {
		return nil, fmt.Errorf("empty open_id")
	}

	req := larkcontact.NewGetUserReqBuilder().
		UserId(openID).
		UserIdType("open_id").
		DepartmentIdType("open_department_id").
		Build()

	resp, err := c.cli.Contact.User.Get(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("feishu get user request failed: %w", err)
	}
	if !resp.Success() {
		return nil, fmt.Errorf("feishu get user failed: code=%d msg=%s log_id=%s",
			resp.Code, resp.Msg, resp.RequestId())
	}
	if resp.Data == nil || resp.Data.User == nil {
		return nil, fmt.Errorf("feishu get user returned empty user")
	}

	u := resp.Data.User
	return &FeishuUserDetail{
		OpenID:          openID,
		Name:            derefString(u.Name),
		EnterpriseEmail: strings.TrimSpace(derefString(u.EnterpriseEmail)),
		Email:           strings.TrimSpace(derefString(u.Email)),
		EmployeeNo:      derefString(u.EmployeeNo),
		JobTitle:        derefString(u.JobTitle),
		Status:          convertFeishuStatus(u.Status),
	}, nil
}

// normalizeFeishuEmails 去空、转小写并去重，保持首次出现顺序。
// 去重是为了不浪费 50 条/批的配额。
func normalizeFeishuEmails(emails []string) []string {
	seen := make(map[string]struct{}, len(emails))
	out := make([]string, 0, len(emails))
	for _, raw := range emails {
		email := strings.ToLower(strings.TrimSpace(raw))
		if email == "" {
			continue
		}
		if _, dup := seen[email]; dup {
			continue
		}
		seen[email] = struct{}{}
		out = append(out, email)
	}
	return out
}

func convertFeishuStatus(s *larkcontact.UserStatus) *FeishuUserStatus {
	if s == nil {
		return nil
	}
	return &FeishuUserStatus{
		IsFrozen:    derefBool(s.IsFrozen),
		IsResigned:  derefBool(s.IsResigned),
		IsActivated: derefBool(s.IsActivated),
		IsExited:    derefBool(s.IsExited),
		IsUnjoin:    derefBool(s.IsUnjoin),
	}
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefBool(p *bool) bool {
	return p != nil && *p
}
