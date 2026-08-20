// 私有扩展（不属于 upstream sub2api）。
//
// 本文件：「飞书离职自动禁用」功能的共享类型与接口契约。
// 所含内容：配置结构、settings key、判定结论枚举、执行结果、repository 接口。
// merge 策略：纯新增文件，与 upstream 无交集，merge 时保留即可。
//
// 这个文件只放类型和签名、不放实现，是为了让 config / repository /
// 判定逻辑 / handler / 前端几块能各自独立开发而不互相踩。
//
// @author wangzhong
package service

import (
	"context"
	"time"
)

// ── settings key ──────────────────────────────────────────────────────
//
// 全部走 settings 表。注意 settings 表是明文存储
// （setting_repo.go 的 Set 直接写 value，没有加密层），
// AppSecret 与既有的 smtp_password 是同等待遇，不要对外宣称它被加密了。
const (
	SettingKeyFeishuOffboardEnabled   = "feishu_offboard_enabled"    // 总开关
	SettingKeyFeishuOffboardSchedule  = "feishu_offboard_schedule"   // 5 段 cron
	SettingKeyFeishuOffboardAppID     = "feishu_offboard_app_id"     // 飞书 App ID
	SettingKeyFeishuOffboardAppSecret = "feishu_offboard_app_secret" // 飞书 App Secret（留空即不改）
	SettingKeyFeishuOffboardDryRun    = "feishu_offboard_dry_run"    // 只判定不禁用
	SettingKeyFeishuOffboardThreshold = "feishu_offboard_threshold"  // 熔断阈值
	SettingKeyFeishuOffboardNotifyTo  = "feishu_offboard_notify_to"  // 结果邮件收件人（JSON 数组）
)

const (
	// FeishuOffboardDefaultSchedule 每天 01:00。
	FeishuOffboardDefaultSchedule = "0 1 * * *"
	// FeishuOffboardDefaultThreshold 单次命中超过这个数就只告警不执行。
	// 一天离职十几人以上，更可能是飞书数据异常或接口语义变了，
	// 而不是真的集体离职；宁可漏一天也不要批量误禁在职员工。
	FeishuOffboardDefaultThreshold = 15
	// FeishuOffboardJobName 用于 ops job heartbeat。
	FeishuOffboardJobName = "feishu_offboard"
)

// ── 配置 ──────────────────────────────────────────────────────────────

// FeishuOffboardConfig 是「生效配置」。全部字段零值可用：
// Enabled 默认 false，所以不配置时整个功能静默不跑。
type FeishuOffboardConfig struct {
	Enabled   bool     `json:"enabled"`
	Schedule  string   `json:"schedule"`
	AppID     string   `json:"app_id"`
	AppSecret string   `json:"-"` // 绝不出现在任何响应里
	DryRun    bool     `json:"dry_run"`
	Threshold int      `json:"circuit_breaker_threshold"`
	NotifyTo  []string `json:"notify_to"`
}

// FeishuOffboardConfigView 是给前端看的配置，密钥只回"是否已配置"。
// 与 SMTPPasswordConfigured 的处理保持一致。
type FeishuOffboardConfigView struct {
	Enabled             bool     `json:"enabled"`
	Schedule            string   `json:"schedule"`
	AppID               string   `json:"app_id"`
	AppSecretConfigured bool     `json:"app_secret_configured"`
	DryRun              bool     `json:"dry_run"`
	Threshold           int      `json:"circuit_breaker_threshold"`
	NotifyTo            []string `json:"notify_to"`
}

// FeishuOffboardConfigInput 是前端提交的配置。
// AppSecret 为空字符串表示"不修改"，而不是"清空"——
// 这样前端不必回显密钥也能保存其他字段。
type FeishuOffboardConfigInput struct {
	Enabled   bool     `json:"enabled"`
	Schedule  string   `json:"schedule"`
	AppID     string   `json:"app_id"`
	AppSecret string   `json:"app_secret"`
	DryRun    bool     `json:"dry_run"`
	Threshold int      `json:"circuit_breaker_threshold"`
	NotifyTo  []string `json:"notify_to"`
}

// ── 判定结论 ──────────────────────────────────────────────────────────

// OffboardVerdict 是对单个用户的判定结论。
// 只有 OffboardVerdictResigned 会触发禁用，其余一律放过。
type OffboardVerdict string

const (
	// OffboardVerdictResigned 飞书确认已离职（is_resigned 或 is_exited），
	// 且该记录的 enterprise_email 与 sub2api 邮箱精确匹配。唯一会被禁用的结论。
	OffboardVerdictResigned OffboardVerdict = "resigned"
	// OffboardVerdictInService 在职，保留。
	OffboardVerdictInService OffboardVerdict = "in_service"
	// OffboardVerdictFrozen 飞书账号被冻结但未标记离职，原因不明，交人工。
	OffboardVerdictFrozen OffboardVerdict = "frozen"
	// OffboardVerdictUnverifiable 飞书查不到，或候选记录里没有一条邮箱能对上。
	//
	// 这个结论必须与"离职"严格区分：查不到人和人离职了是两件完全不同的事。
	// 外部合作方（如 xxx_wb@mail.g7e6.com.cn）恒定落在这一类，
	// 把它当离职处理会把合作方账号全部误禁。
	OffboardVerdictUnverifiable OffboardVerdict = "unverifiable"
	// OffboardVerdictSkipAdmin 管理员账号，服务端本就禁止禁用，提前跳过。
	OffboardVerdictSkipAdmin OffboardVerdict = "skip_admin"
)

// OffboardDecision 是单个用户的判定结果及其依据。
//
// 依据字段（FeishuOpenID / FeishuName / FeishuFlags / Reason）会整体落库到
// feishu_offboard_runs.details，目的是事后能回答"凭什么禁了这个人"。
// 出现误禁时可以直接看到当时飞书返回了什么，而不是只能猜。
type OffboardDecision struct {
	UserID   int64           `json:"user_id"`
	Email    string          `json:"email"`
	Username string          `json:"username"`
	Verdict  OffboardVerdict `json:"verdict"`
	// Reason 人类可读的判定说明，会进报告和邮件。
	Reason string `json:"reason"`
	// FeishuOpenID 最终采纳的那条飞书记录（邮箱匹配成功的那条）。
	FeishuOpenID string `json:"feishu_open_id,omitempty"`
	FeishuName   string `json:"feishu_name,omitempty"`
	EmployeeNo   string `json:"employee_no,omitempty"`
	// FeishuFlags 采纳记录的原始状态位，保留用于追溯。
	FeishuFlags *FeishuUserStatus `json:"feishu_flags,omitempty"`
	// CandidateCount 该邮箱在飞书返回了几条候选。>1 说明该邮箱关联了多个账号，
	// 是最容易误判的场景，值得在报告里显式呈现。
	CandidateCount int `json:"candidate_count"`
	// MatchedCount 其中 enterprise_email 精确匹配的有几条。
	//
	// 区分 CandidateCount 与 MatchedCount 才能分清两种截然不同的情形：
	//   - candidate>1 而 matched==1：邮箱被回收，其余记录属于别人；
	//   - matched>1：同一个人有多个飞书账号（离职后回归等），
	//     此时若各记录状态冲突，按「任一在职即不禁用」的保守规则裁决。
	// 报告里呈现这个数字，复核的人才能判断系统用的是哪条依据。
	MatchedCount int `json:"matched_count"`
	// Disabled 是否真的执行了禁用（dry-run 或熔断时为 false）。
	Disabled bool `json:"disabled"`
	// DisableError 禁用失败的原因（若有）。
	DisableError string `json:"disable_error,omitempty"`
}

// ── 执行结果 ──────────────────────────────────────────────────────────

const (
	OffboardTriggerCron   = "cron"
	OffboardTriggerManual = "manual"
)

// FeishuOffboardRun 是一次执行的完整记录，对应 feishu_offboard_runs 表。
type FeishuOffboardRun struct {
	ID                int64              `json:"id"`
	RunAt             time.Time          `json:"run_at"`
	TriggerSource     string             `json:"trigger_source"`
	DryRun            bool               `json:"dry_run"`
	CheckedCount      int                `json:"checked_count"`
	ResignedCount     int                `json:"resigned_count"`
	DisabledCount     int                `json:"disabled_count"`
	UnverifiableCount int                `json:"unverifiable_count"`
	SkippedCount      int                `json:"skipped_count"`
	CircuitBroken     bool               `json:"circuit_broken"`
	DurationMs        int64              `json:"duration_ms"`
	ErrorMessage      string             `json:"error_message,omitempty"`
	Decisions         []OffboardDecision `json:"decisions,omitempty"`
	CreatedAt         time.Time          `json:"created_at"`
}

// FeishuOffboardRunListFilter 执行历史查询条件。
type FeishuOffboardRunListFilter struct {
	Page     int
	PageSize int
}

// FeishuOffboardRunList 分页结果。
type FeishuOffboardRunList struct {
	Items    []FeishuOffboardRun `json:"items"`
	Total    int64               `json:"total"`
	Page     int                 `json:"page"`
	PageSize int                 `json:"page_size"`
}

// FeishuOffboardRepository 执行记录的持久化。
type FeishuOffboardRepository interface {
	// Insert 写入一次执行记录，回填 ID。
	Insert(ctx context.Context, run *FeishuOffboardRun) error
	// List 按 run_at 倒序分页查询。
	List(ctx context.Context, filter FeishuOffboardRunListFilter) (*FeishuOffboardRunList, error)
	// GetByID 取单次执行详情（含 decisions）。
	GetByID(ctx context.Context, id int64) (*FeishuOffboardRun, error)
}
