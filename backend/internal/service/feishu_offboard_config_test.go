// 私有扩展（不属于 upstream sub2api）。
//
// 本文件：feishu_offboard_config.go 的单元测试。
// merge 策略：纯新增文件，merge 时保留即可。
//
// @author wangzhong
package service

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// feishuOffboardSettingRepoStub 是内存版 SettingRepository。
// 只实现本功能用到的 GetMultiple / SetMultiple 语义，其余方法按接口补齐。
type feishuOffboardSettingRepoStub struct {
	values map[string]string
	// getErr 非 nil 时模拟库读失败，用于验证读失败不阻断保存。
	getErr error
	// setCalls 记录每次 SetMultiple 的入参，用于断言"哪些 key 被写了"。
	setCalls []map[string]string
}

func newFeishuOffboardRepoStub(values map[string]string) *feishuOffboardSettingRepoStub {
	if values == nil {
		values = map[string]string{}
	}
	return &feishuOffboardSettingRepoStub{values: values}
}

func (r *feishuOffboardSettingRepoStub) Get(ctx context.Context, key string) (*Setting, error) {
	if value, ok := r.values[key]; ok {
		return &Setting{Key: key, Value: value}, nil
	}
	return nil, ErrSettingNotFound
}

func (r *feishuOffboardSettingRepoStub) GetValue(ctx context.Context, key string) (string, error) {
	s, err := r.Get(ctx, key)
	if err != nil {
		return "", err
	}
	return s.Value, nil
}

func (r *feishuOffboardSettingRepoStub) Set(ctx context.Context, key, value string) error {
	r.values[key] = value
	return nil
}

// GetMultiple 复刻真实实现的关键语义：只返回库里存在的 key。
func (r *feishuOffboardSettingRepoStub) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	out := make(map[string]string)
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func (r *feishuOffboardSettingRepoStub) SetMultiple(ctx context.Context, settings map[string]string) error {
	snapshot := make(map[string]string, len(settings))
	for k, v := range settings {
		snapshot[k] = v
		r.values[k] = v
	}
	r.setCalls = append(r.setCalls, snapshot)
	return nil
}

func (r *feishuOffboardSettingRepoStub) GetAll(ctx context.Context) (map[string]string, error) {
	out := make(map[string]string, len(r.values))
	for k, v := range r.values {
		out[k] = v
	}
	return out, nil
}

func (r *feishuOffboardSettingRepoStub) Delete(ctx context.Context, key string) error {
	delete(r.values, key)
	return nil
}

// TestFeishuOffboardConfigEmptySettingsReturnsUsableDefaults
// settings 表完全为空时必须拿到 Enabled=false 的合法配置，且不报错。
func TestFeishuOffboardConfigEmptySettingsReturnsUsableDefaults(t *testing.T) {
	store := NewFeishuOffboardConfigStore(newFeishuOffboardRepoStub(nil))

	cfg, err := store.LoadConfig(context.Background())
	require.NoError(t, err, "空 settings 不应报错")
	require.False(t, cfg.Enabled, "未配置时功能必须静默关闭")
	require.Equal(t, FeishuOffboardDefaultSchedule, cfg.Schedule)
	require.Equal(t, FeishuOffboardDefaultThreshold, cfg.Threshold)
	require.Empty(t, cfg.AppID)
	require.Empty(t, cfg.AppSecret)
	require.Nil(t, cfg.NotifyTo)

	view, err := store.LoadView(context.Background())
	require.NoError(t, err)
	require.False(t, view.AppSecretConfigured, "空库不应报告已配置密钥")
	require.NotNil(t, view.NotifyTo, "视图里的收件人必须是空数组而不是 null")
	require.Empty(t, view.NotifyTo)
}

// TestFeishuOffboardConfigViewNeverLeaksSecret 视图只回布尔，不回明文。
func TestFeishuOffboardConfigViewNeverLeaksSecret(t *testing.T) {
	repo := newFeishuOffboardRepoStub(map[string]string{
		SettingKeyFeishuOffboardEnabled:   "true",
		SettingKeyFeishuOffboardAppID:     "cli_abc",
		SettingKeyFeishuOffboardAppSecret: "top-secret",
		SettingKeyFeishuOffboardNotifyTo:  `["a@x.com","a@x.com"," b@x.com ",""]`,
	})
	store := NewFeishuOffboardConfigStore(repo)

	view, err := store.LoadView(context.Background())
	require.NoError(t, err)
	require.True(t, view.Enabled)
	require.True(t, view.AppSecretConfigured)
	require.Equal(t, []string{"a@x.com", "b@x.com"}, view.NotifyTo, "收件人应去空去重并 trim")
}

// TestFeishuOffboardConfigSaveKeepsExistingSecretWhenBlank
// 这是本功能最关键的语义：AppSecret 留空 = 不修改，库里原值必须保留。
func TestFeishuOffboardConfigSaveKeepsExistingSecretWhenBlank(t *testing.T) {
	repo := newFeishuOffboardRepoStub(map[string]string{
		SettingKeyFeishuOffboardAppID:     "cli_old",
		SettingKeyFeishuOffboardAppSecret: "old-secret",
	})
	store := NewFeishuOffboardConfigStore(repo)

	err := store.SaveConfig(context.Background(), FeishuOffboardConfigInput{
		Enabled:   true,
		Schedule:  "30 2 * * *",
		AppID:     "  cli_new  ", // 前后空白必须被 trim
		AppSecret: "",            // 留空：不修改
		Threshold: 20,
	})
	require.NoError(t, err)

	require.Len(t, repo.setCalls, 1)
	require.NotContains(t, repo.setCalls[0], SettingKeyFeishuOffboardAppSecret,
		"留空时绝不能写 app_secret key")
	require.Equal(t, "old-secret", repo.values[SettingKeyFeishuOffboardAppSecret], "原密钥必须保留")

	cfg, err := store.LoadConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, "cli_new", cfg.AppID, "AppID 应已 trim")
	require.Equal(t, "old-secret", cfg.AppSecret)
	require.Equal(t, "30 2 * * *", cfg.Schedule)
	require.Equal(t, 20, cfg.Threshold)
	require.True(t, cfg.Enabled)
}

// TestFeishuOffboardConfigSaveUpdatesSecretWhenProvided 提交新密钥时正常覆盖，且 trim。
func TestFeishuOffboardConfigSaveUpdatesSecretWhenProvided(t *testing.T) {
	repo := newFeishuOffboardRepoStub(map[string]string{
		SettingKeyFeishuOffboardAppSecret: "old-secret",
	})
	store := NewFeishuOffboardConfigStore(repo)

	require.NoError(t, store.SaveConfig(context.Background(), FeishuOffboardConfigInput{
		Enabled:   true,
		AppID:     "cli_abc",
		AppSecret: "  new-secret\n",
		Threshold: 5,
	}))
	require.Equal(t, "new-secret", repo.values[SettingKeyFeishuOffboardAppSecret])
}

// TestFeishuOffboardConfigThresholdFallsBackTo15 阈值 0 / 负数 / 坏值一律回落默认，
// 不接受"无上限"——这是防批量误禁的护栏。
func TestFeishuOffboardConfigThresholdFallsBackTo15(t *testing.T) {
	repo := newFeishuOffboardRepoStub(nil)
	store := NewFeishuOffboardConfigStore(repo)

	// 入口：提交 0
	require.NoError(t, store.SaveConfig(context.Background(), FeishuOffboardConfigInput{
		Schedule:  FeishuOffboardDefaultSchedule,
		Threshold: 0,
	}))
	require.Equal(t, strconv.Itoa(FeishuOffboardDefaultThreshold),
		repo.values[SettingKeyFeishuOffboardThreshold], "0 必须落库为默认阈值")

	// 出口：库里存了坏值也要能读出默认值
	for _, bad := range []string{"0", "-3", "abc", ""} {
		repo.values[SettingKeyFeishuOffboardThreshold] = bad
		cfg, err := store.LoadConfig(context.Background())
		require.NoError(t, err, "threshold=%q", bad)
		require.Equal(t, FeishuOffboardDefaultThreshold, cfg.Threshold, "threshold=%q", bad)
	}
}

// TestFeishuOffboardConfigRejectsInvalidCron 非法 cron 必须被拒；
// 空 cron 是"用默认"而不是错误。
func TestFeishuOffboardConfigRejectsInvalidCron(t *testing.T) {
	store := NewFeishuOffboardConfigStore(newFeishuOffboardRepoStub(nil))

	for _, bad := range []string{"not-a-cron", "0 1 * *", "99 1 * * *", "0 1 * * * *"} {
		require.Error(t, store.ValidateInput(FeishuOffboardConfigInput{Schedule: bad, Threshold: 1}),
			"schedule=%q 应被拒绝", bad)
		require.Error(t, store.SaveConfig(context.Background(), FeishuOffboardConfigInput{
			Schedule: bad, Threshold: 1,
		}), "schedule=%q 不应落库", bad)
	}

	// 空表达式走默认值，不报错
	require.NoError(t, store.ValidateInput(FeishuOffboardConfigInput{Schedule: "   ", Threshold: 1}))
	require.NoError(t, store.ValidateInput(FeishuOffboardConfigInput{Schedule: "*/5 * * * *", Threshold: 1}))
}

// TestFeishuOffboardConfigLoadFallsBackOnBadCron 库里存了坏 cron 时回落默认，
// 不让一个坏值导致整个功能起不来。
func TestFeishuOffboardConfigLoadFallsBackOnBadCron(t *testing.T) {
	repo := newFeishuOffboardRepoStub(map[string]string{
		SettingKeyFeishuOffboardSchedule: "every monday",
		SettingKeyFeishuOffboardEnabled:  "yes-please", // 也不是合法布尔
		SettingKeyFeishuOffboardNotifyTo: "{oops",      // 也不是合法 JSON
	})
	cfg, err := NewFeishuOffboardConfigStore(repo).LoadConfig(context.Background())
	require.NoError(t, err, "坏值不应导致读取失败")
	require.Equal(t, FeishuOffboardDefaultSchedule, cfg.Schedule)
	require.False(t, cfg.Enabled)
	require.Nil(t, cfg.NotifyTo)
}

// TestFeishuOffboardConfigRejectsEnabledWithoutAppID
// 开了开关却没凭证会让任务每天失败刷日志，必须在保存时拒绝。
func TestFeishuOffboardConfigRejectsEnabledWithoutAppID(t *testing.T) {
	repo := newFeishuOffboardRepoStub(nil)
	store := NewFeishuOffboardConfigStore(repo)

	err := store.SaveConfig(context.Background(), FeishuOffboardConfigInput{
		Enabled:   true,
		AppID:     "   ", // 全空白，trim 后等于没填
		AppSecret: "some-secret",
		Threshold: 10,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "App ID")
	require.Empty(t, repo.setCalls, "校验失败不应落库")

	require.Error(t, store.ValidateInput(FeishuOffboardConfigInput{Enabled: true, Threshold: 1}))
}

// TestFeishuOffboardConfigRejectsEnabledWithoutSecret
// 库里没密钥且本次也没提交时必须拒绝；库里已有密钥则允许留空。
func TestFeishuOffboardConfigRejectsEnabledWithoutSecret(t *testing.T) {
	empty := newFeishuOffboardRepoStub(nil)
	err := NewFeishuOffboardConfigStore(empty).SaveConfig(context.Background(), FeishuOffboardConfigInput{
		Enabled:   true,
		AppID:     "cli_abc",
		Threshold: 10,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "App Secret")
	require.Empty(t, empty.setCalls)

	withSecret := newFeishuOffboardRepoStub(map[string]string{
		SettingKeyFeishuOffboardAppSecret: "existing",
	})
	require.NoError(t, NewFeishuOffboardConfigStore(withSecret).SaveConfig(context.Background(),
		FeishuOffboardConfigInput{Enabled: true, AppID: "cli_abc", Threshold: 10}),
		"库里已有密钥时留空应当允许")

	// ValidateInput 无法读库，所以不做密钥存在性校验（否则会误报）。
	require.NoError(t, NewFeishuOffboardConfigStore(empty).ValidateInput(FeishuOffboardConfigInput{
		Enabled: true, AppID: "cli_abc", Threshold: 10,
	}))
}

// TestFeishuOffboardConfigEnabledWithoutCredentialsDegradesOnLoad
// 手工改库造出"开着但没凭证"的组合时，读取阶段降级为关闭。
func TestFeishuOffboardConfigEnabledWithoutCredentialsDegradesOnLoad(t *testing.T) {
	repo := newFeishuOffboardRepoStub(map[string]string{
		SettingKeyFeishuOffboardEnabled: "true",
		SettingKeyFeishuOffboardAppID:   "cli_abc",
		// app_secret 缺失
	})
	cfg, err := NewFeishuOffboardConfigStore(repo).LoadConfig(context.Background())
	require.NoError(t, err)
	require.False(t, cfg.Enabled, "凭证不全时不应真的开启")
}

// TestFeishuOffboardConfigLoadPropagatesRepoError
// 库读失败要返回 error，但第一个返回值仍是合法默认配置（调用方忽略 error 也不会拿到半成品）。
func TestFeishuOffboardConfigLoadPropagatesRepoError(t *testing.T) {
	repo := newFeishuOffboardRepoStub(nil)
	repo.getErr = errors.New("db down")
	cfg, err := NewFeishuOffboardConfigStore(repo).LoadConfig(context.Background())
	require.Error(t, err)
	require.Equal(t, FeishuOffboardDefaultSchedule, cfg.Schedule)
	require.Equal(t, FeishuOffboardDefaultThreshold, cfg.Threshold)
	require.False(t, cfg.Enabled)
}

// TestFeishuOffboardConfigNilRepoIsSafe repo 未注入时读操作返回默认值、写操作报错，不 panic。
func TestFeishuOffboardConfigNilRepoIsSafe(t *testing.T) {
	store := NewFeishuOffboardConfigStore(nil)
	cfg, err := store.LoadConfig(context.Background())
	require.NoError(t, err)
	require.False(t, cfg.Enabled)
	require.Error(t, store.SaveConfig(context.Background(), FeishuOffboardConfigInput{}))
}
