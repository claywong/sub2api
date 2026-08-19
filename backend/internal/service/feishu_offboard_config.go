// 私有扩展（不属于 upstream sub2api）。
//
// 本文件：「飞书离职自动禁用」配置的读写与校验。
// 所含内容：FeishuOffboardConfigStore 及其 LoadConfig / LoadView /
// SaveConfig / ValidateInput，以及配套的解析、归一化、存储编码辅助函数。
// merge 策略：纯新增文件，与 upstream 无交集，merge 时保留即可。
//
// 契约见 feishu_offboard_types.go，本文件不定义任何对外类型。
//
// 两条贯穿全文的设计原则：
//
//  1. 零值可用。settings 表里一个 key 都没有时，LoadConfig 必须返回
//     Enabled=false 的合法配置而不是报错——否则「装了但没配」的部署
//     会在启动路径上炸掉，而这个功能本身是可选的。
//  2. 坏值不阻断。单个 key 存了非法内容（手工改库、老版本遗留格式）时
//     回落默认值并打日志，不让一个坏值导致整个功能起不来。
//     容错风格参照 ops_cleanup_service.go 的 computeEffectiveLocked。
//
// @author wangzhong
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/robfig/cron/v3"
)

// feishuOffboardLogComponent 日志组件名，与 settings key 前缀保持一致，便于捞日志。
const feishuOffboardLogComponent = "service.feishu_offboard"

// feishuOffboardCronParser 5 段 cron 解析器（分 时 日 月 周）。
//
// 必须显式指定字段集：cron 库的默认 parser 接受可选的秒字段，
// 那会让 "0 1 * * *" 被解释成「每分钟的第 1 秒」这类完全错误的时间。
// 与 opsCleanupCronParser 的构造方式保持一致。
var feishuOffboardCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// feishuOffboardSettingKeys 是本功能占用的全部 settings key。
// 集中在一处，避免 Load 少读一个 key 却没人发现。
var feishuOffboardSettingKeys = []string{
	SettingKeyFeishuOffboardEnabled,
	SettingKeyFeishuOffboardSchedule,
	SettingKeyFeishuOffboardAppID,
	SettingKeyFeishuOffboardAppSecret,
	SettingKeyFeishuOffboardDryRun,
	SettingKeyFeishuOffboardThreshold,
	SettingKeyFeishuOffboardNotifyTo,
}

// FeishuOffboardConfigStore 负责本功能配置在 settings 表上的读写。
//
// 只依赖 SettingRepository：配置本身不需要 cfg 文件参与
// （与 ops cleanup 不同，这个功能没有 upstream 的 yaml 配置来源），
// 所以「生效配置」就是 settings 表的内容，没有优先级合并逻辑。
type FeishuOffboardConfigStore struct {
	settingRepo SettingRepository
}

// NewFeishuOffboardConfigStore 构造配置存储。
// settingRepo 允许为 nil：此时所有读操作返回默认配置，写操作报错。
// 这样即便 wire 装配顺序出问题也不会 panic。
func NewFeishuOffboardConfigStore(settingRepo SettingRepository) *FeishuOffboardConfigStore {
	return &FeishuOffboardConfigStore{settingRepo: settingRepo}
}

// FeishuOffboardDefaultConfig 返回一份「什么都没配」的合法配置。
//
// Enabled=false 是关键：这保证了未配置的部署上功能静默不跑。
func FeishuOffboardDefaultConfig() FeishuOffboardConfig {
	return FeishuOffboardConfig{
		Enabled:   false,
		Schedule:  FeishuOffboardDefaultSchedule,
		DryRun:    false,
		Threshold: FeishuOffboardDefaultThreshold,
		NotifyTo:  nil,
	}
}

// ── 读 ────────────────────────────────────────────────────────────────

// LoadConfig 从 settings 读出生效配置（含 AppSecret 明文，仅供服务端执行使用）。
//
// 缺失的 key 一律用默认值填充，不报错。只有 repository 本身失败
// （库连不上等）才返回 error，且此时第一个返回值仍是合法的默认配置，
// 调用方即便忽略 error 也不会拿到半成品。
func (s *FeishuOffboardConfigStore) LoadConfig(ctx context.Context) (FeishuOffboardConfig, error) {
	raw, err := s.loadRaw(ctx)
	if err != nil {
		return FeishuOffboardDefaultConfig(), err
	}
	return buildFeishuOffboardConfig(raw), nil
}

// LoadView 返回给前端的配置视图，密钥只回「是否已配置」布尔。
// 与 SMTPPasswordConfigured 的处理一致：明文密钥绝不出现在任何响应里。
func (s *FeishuOffboardConfigStore) LoadView(ctx context.Context) (FeishuOffboardConfigView, error) {
	cfg, err := s.LoadConfig(ctx)
	view := FeishuOffboardConfigView{
		Enabled:             cfg.Enabled,
		Schedule:            cfg.Schedule,
		AppID:               cfg.AppID,
		AppSecretConfigured: cfg.AppSecret != "",
		DryRun:              cfg.DryRun,
		Threshold:           cfg.Threshold,
		NotifyTo:            cfg.NotifyTo,
	}
	if view.NotifyTo == nil {
		// 前端拿到 null 时 v-for 会报错，统一给空数组。
		view.NotifyTo = []string{}
	}
	return view, err
}

// loadRaw 取出本功能的全部 key。
// GetMultiple 只返回库里存在的 key，所以返回的 map 可能不全，
// 缺失判断一律交给下游的 pick* 辅助函数。
func (s *FeishuOffboardConfigStore) loadRaw(ctx context.Context) (map[string]string, error) {
	if s == nil || s.settingRepo == nil {
		return map[string]string{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	raw, err := s.settingRepo.GetMultiple(ctx, feishuOffboardSettingKeys)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			// 理论上 GetMultiple 不会返回这个错，兜一手：一条都没有等价于全部缺失。
			return map[string]string{}, nil
		}
		logger.LegacyPrintf(feishuOffboardLogComponent,
			"[FeishuOffboard] 读取配置失败，回落默认配置: %v", err)
		return nil, fmt.Errorf("读取飞书离职配置失败: %w", err)
	}
	if raw == nil {
		raw = map[string]string{}
	}
	return raw, nil
}

// buildFeishuOffboardConfig 把 settings 原始 map 解析成生效配置。
// 每个字段独立容错：任一字段解析失败只影响它自己。
func buildFeishuOffboardConfig(raw map[string]string) FeishuOffboardConfig {
	cfg := FeishuOffboardDefaultConfig()

	cfg.Enabled = pickFeishuOffboardBool(raw, SettingKeyFeishuOffboardEnabled, cfg.Enabled)
	cfg.DryRun = pickFeishuOffboardBool(raw, SettingKeyFeishuOffboardDryRun, cfg.DryRun)
	cfg.AppID = strings.TrimSpace(raw[SettingKeyFeishuOffboardAppID])
	cfg.AppSecret = strings.TrimSpace(raw[SettingKeyFeishuOffboardAppSecret])
	cfg.Schedule = pickFeishuOffboardSchedule(raw[SettingKeyFeishuOffboardSchedule])
	cfg.Threshold = pickFeishuOffboardThreshold(raw[SettingKeyFeishuOffboardThreshold])
	cfg.NotifyTo = parseFeishuOffboardNotifyTo(raw[SettingKeyFeishuOffboardNotifyTo])

	// 库里可能出现「Enabled=true 但凭证被清空」的组合（手工改库、
	// 或先删了 app_id 再重启）。这种配置每天都会失败刷日志，
	// 读取时直接降级为关闭：宁可不跑，也不要留一个注定失败的定时任务。
	if cfg.Enabled && (cfg.AppID == "" || cfg.AppSecret == "") {
		logger.LegacyPrintf(feishuOffboardLogComponent,
			"[FeishuOffboard] 已开启但凭证不完整（app_id_set=%t app_secret_set=%t），本次视为关闭",
			cfg.AppID != "", cfg.AppSecret != "")
		cfg.Enabled = false
	}
	return cfg
}

// pickFeishuOffboardBool 解析 bool，缺失或非法时回落 def。
func pickFeishuOffboardBool(raw map[string]string, key string, def bool) bool {
	value := strings.TrimSpace(raw[key])
	if value == "" {
		return def
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		logger.LegacyPrintf(feishuOffboardLogComponent,
			"[FeishuOffboard] 配置 %s=%q 不是合法布尔值，回落 %t", key, value, def)
		return def
	}
	return parsed
}

// pickFeishuOffboardSchedule 归一化 cron 表达式。
// 空值或非法值回落 FeishuOffboardDefaultSchedule——一个坏的 cron
// 不应该让功能起不来，每天 01:00 跑一次总比不跑合理。
func pickFeishuOffboardSchedule(value string) string {
	schedule := strings.TrimSpace(value)
	if schedule == "" {
		return FeishuOffboardDefaultSchedule
	}
	if err := validateFeishuOffboardSchedule(schedule); err != nil {
		logger.LegacyPrintf(feishuOffboardLogComponent,
			"[FeishuOffboard] 配置 schedule=%q 非法（%v），回落 %q",
			schedule, err, FeishuOffboardDefaultSchedule)
		return FeishuOffboardDefaultSchedule
	}
	return schedule
}

// pickFeishuOffboardThreshold 解析熔断阈值。
//
// <1 一律回落默认值：0 在这里不能表示「无上限」，
// 那等于关掉了防批量误禁的护栏（见 types 里对 Threshold 的说明）。
func pickFeishuOffboardThreshold(value string) int {
	text := strings.TrimSpace(value)
	if text == "" {
		return FeishuOffboardDefaultThreshold
	}
	parsed, err := strconv.Atoi(text)
	if err != nil {
		logger.LegacyPrintf(feishuOffboardLogComponent,
			"[FeishuOffboard] 配置 threshold=%q 不是整数，回落 %d", text, FeishuOffboardDefaultThreshold)
		return FeishuOffboardDefaultThreshold
	}
	if parsed < 1 {
		logger.LegacyPrintf(feishuOffboardLogComponent,
			"[FeishuOffboard] 配置 threshold=%d 小于 1（熔断护栏不可关闭），回落 %d",
			parsed, FeishuOffboardDefaultThreshold)
		return FeishuOffboardDefaultThreshold
	}
	return parsed
}

// parseFeishuOffboardNotifyTo 解析收件人列表。
//
// 标准存储格式是 JSON 数组；同时兼容逗号/换行分隔的裸字符串，
// 因为这个 key 很可能被手工写进库（运维直接 UPDATE settings 加个收件人）。
// 解析失败返回 nil：收不到邮件比整个功能起不来好。
func parseFeishuOffboardNotifyTo(value string) []string {
	text := strings.TrimSpace(value)
	if text == "" {
		return nil
	}
	// 以 [ 或 { 开头说明这个值本意是 JSON。解析不了就是坏值，
	// 绝不能退到分隔符解析——那会把 `{oops` 整段当成一个收件人地址。
	if strings.HasPrefix(text, "[") || strings.HasPrefix(text, "{") {
		var list []string
		if err := json.Unmarshal([]byte(text), &list); err != nil {
			logger.LegacyPrintf(feishuOffboardLogComponent,
				"[FeishuOffboard] 配置 notify_to 不是合法 JSON 数组（%v），本次不发通知", err)
			return nil
		}
		return normalizeFeishuOffboardNotifyTo(list)
	}
	return normalizeFeishuOffboardNotifyTo(strings.FieldsFunc(text, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r'
	}))
}

// normalizeFeishuOffboardNotifyTo trim、去空、去重（忽略大小写），保持原始顺序。
// 返回 nil 而非空 slice，便于 LoadView 统一处理。
func normalizeFeishuOffboardNotifyTo(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(list))
	result := make([]string, 0, len(list))
	for _, item := range list {
		addr := strings.TrimSpace(item)
		if addr == "" {
			continue
		}
		// 邮箱大小写不敏感，用小写做去重键但保留用户输入的原始写法。
		key := strings.ToLower(addr)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, addr)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// ── 写 ────────────────────────────────────────────────────────────────

// SaveConfig 把前端提交的配置写回 settings。
//
// AppSecret 语义：input.AppSecret 为空字符串表示「不修改」，不是「清空」。
// 此时不写这个 key，库里原值原样保留——前端因此不必回显密钥
// 也能保存其他字段（与 setting_update.go 里 SMTP 密码的处理一致）。
// 需要真正更换密钥时，前端提交新值即可；本接口不提供清空密钥的入口。
func (s *FeishuOffboardConfigStore) SaveConfig(ctx context.Context, input FeishuOffboardConfigInput) error {
	if s == nil || s.settingRepo == nil {
		return errors.New("飞书离职配置存储未初始化")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	normalized := normalizeFeishuOffboardInput(input)

	// 先取库里已有的密钥状态：Enabled=true 的校验需要知道
	// 「本次没提交密钥」到底是因为已经存过了，还是压根没配过。
	secretState := feishuOffboardSecretUnknown
	if existing, err := s.loadRaw(ctx); err == nil {
		if strings.TrimSpace(existing[SettingKeyFeishuOffboardAppSecret]) != "" {
			secretState = feishuOffboardSecretPresent
		} else {
			secretState = feishuOffboardSecretAbsent
		}
	} else {
		// 读失败时不阻断保存（否则用户永远改不了配置），
		// 只把密钥状态标为未知，跳过那一项校验。
		logger.LegacyPrintf(feishuOffboardLogComponent,
			"[FeishuOffboard] 保存前读取旧配置失败，跳过密钥存在性校验: %v", err)
	}

	if err := validateFeishuOffboardInput(normalized, secretState); err != nil {
		return err
	}

	notifyToJSON, err := json.Marshal(feishuOffboardNotifyToForStorage(normalized.NotifyTo))
	if err != nil {
		return fmt.Errorf("序列化通知收件人失败: %w", err)
	}

	updates := map[string]string{
		SettingKeyFeishuOffboardEnabled:   strconv.FormatBool(normalized.Enabled),
		SettingKeyFeishuOffboardSchedule:  normalized.Schedule,
		SettingKeyFeishuOffboardAppID:     normalized.AppID,
		SettingKeyFeishuOffboardDryRun:    strconv.FormatBool(normalized.DryRun),
		SettingKeyFeishuOffboardThreshold: strconv.Itoa(normalized.Threshold),
		SettingKeyFeishuOffboardNotifyTo:  string(notifyToJSON),
	}
	// 留空即不改：只在本次确实提交了新密钥时才写这个 key。
	if normalized.AppSecret != "" {
		updates[SettingKeyFeishuOffboardAppSecret] = normalized.AppSecret
	}

	if err := s.settingRepo.SetMultiple(ctx, updates); err != nil {
		return fmt.Errorf("保存飞书离职配置失败: %w", err)
	}
	return nil
}

// feishuOffboardNotifyToForStorage 保证 json.Marshal 出 [] 而不是 null，
// 便于直接读库排查时看得懂。
func feishuOffboardNotifyToForStorage(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// ── 校验 ──────────────────────────────────────────────────────────────

// feishuOffboardSecretState 表示「库里是否已有 AppSecret」的三态。
//
// 需要三态而不是 bool：ValidateInput 是无 ctx 的纯函数，拿不到库里的状态，
// 此时必须跳过这一项校验而不是当成「没配」误报。
type feishuOffboardSecretState int

const (
	// feishuOffboardSecretUnknown 未查询库（纯校验场景），跳过密钥存在性检查。
	feishuOffboardSecretUnknown feishuOffboardSecretState = iota
	// feishuOffboardSecretPresent 库里已有密钥。
	feishuOffboardSecretPresent
	// feishuOffboardSecretAbsent 库里没有密钥。
	feishuOffboardSecretAbsent
)

// ValidateInput 对前端提交的配置做无副作用校验，供 handler 在入口处快速拒绝。
//
// 注意：这里**不**校验「Enabled=true 时密钥是否已配置」——
// 那需要读库才能区分「留空是因为库里已有」还是「压根没配」，
// 而本方法按契约不接收 ctx。该项由 SaveConfig 在读到旧值后补校验，
// 所以 SaveConfig 不依赖调用方先调本方法也能保证安全。
func (s *FeishuOffboardConfigStore) ValidateInput(input FeishuOffboardConfigInput) error {
	return validateFeishuOffboardInput(normalizeFeishuOffboardInput(input), feishuOffboardSecretUnknown)
}

// normalizeFeishuOffboardInput 归一化前端输入，让校验和落库看到同一份数据。
//
// AppID / AppSecret 必须 trim：用户从飞书开放平台后台复制凭证时
// 极易带上首尾空格或换行，带空格的 app_id 会让 API 直接返回
// 「应用不存在」，排查起来毫无线索。
func normalizeFeishuOffboardInput(input FeishuOffboardConfigInput) FeishuOffboardConfigInput {
	out := input
	out.AppID = strings.TrimSpace(input.AppID)
	out.AppSecret = strings.TrimSpace(input.AppSecret)
	out.Schedule = strings.TrimSpace(input.Schedule)
	if out.Schedule == "" {
		// 空 cron 是「用默认」而不是错误：前端不填就是每天 01:00。
		out.Schedule = FeishuOffboardDefaultSchedule
	}
	if out.Threshold < 1 {
		// 0 / 负数一律回落默认阈值，不接受「无上限」。
		out.Threshold = FeishuOffboardDefaultThreshold
	}
	out.NotifyTo = normalizeFeishuOffboardNotifyTo(input.NotifyTo)
	return out
}

// validateFeishuOffboardInput 校验已归一化的输入。
// 入参必须先过 normalizeFeishuOffboardInput，否则空 schedule 会被误判为非法。
func validateFeishuOffboardInput(input FeishuOffboardConfigInput, secret feishuOffboardSecretState) error {
	if err := validateFeishuOffboardSchedule(input.Schedule); err != nil {
		return fmt.Errorf("定时表达式非法: %w", err)
	}
	if input.Threshold < 1 {
		// 归一化之后不该走到这里，留作防御：调用方直接调本函数时也不能放过。
		return errors.New("熔断阈值必须大于等于 1")
	}
	if !input.Enabled {
		// 关闭状态下允许保存半成品配置（先填 app_id 下次再填密钥）。
		return nil
	}
	// 以下是「开了开关」才生效的校验：凭证不全会让定时任务每天失败刷日志，
	// 与其等到凌晨报错，不如在保存时就拒绝。
	if input.AppID == "" {
		return errors.New("启用飞书离职检查前必须填写 App ID")
	}
	if secret == feishuOffboardSecretAbsent && input.AppSecret == "" {
		return errors.New("启用飞书离职检查前必须填写 App Secret")
	}
	return nil
}

// validateFeishuOffboardSchedule 校验 5 段 cron。空字符串视为非法，
// 「空表示用默认」的语义由归一化阶段负责，本函数只做纯语法判断。
func validateFeishuOffboardSchedule(schedule string) error {
	trimmed := strings.TrimSpace(schedule)
	if trimmed == "" {
		return errors.New("表达式为空")
	}
	if _, err := feishuOffboardCronParser.Parse(trimmed); err != nil {
		return fmt.Errorf("%q 不是合法的 5 段 cron（分 时 日 月 周）: %w", trimmed, err)
	}
	return nil
}
