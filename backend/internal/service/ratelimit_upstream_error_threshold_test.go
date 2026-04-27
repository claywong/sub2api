//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// --- fakes（复用同包已有的 tempUnschedCall / setErrorCall / internal500AccountRepoStub）---

// fakeErrorCounterCache 控制 IncrementErrorCount 返回值并记录 ResetErrorCount 调用
type fakeErrorCounterCache struct {
	count      int64
	resetCalls int
}

func (f *fakeErrorCounterCache) IncrementErrorCount(_ context.Context, _ int64, _ int) (int64, error) {
	f.count++
	return f.count, nil
}

func (f *fakeErrorCounterCache) ResetErrorCount(_ context.Context, _ int64) error {
	f.resetCalls++
	return nil
}

// fakeTempUnschedCacheForError 记录 SetTempUnsched 调用
type fakeTempUnschedCacheForError struct {
	setCalls []*TempUnschedState
}

func (f *fakeTempUnschedCacheForError) SetTempUnsched(_ context.Context, _ int64, state *TempUnschedState) error {
	f.setCalls = append(f.setCalls, state)
	return nil
}

func (f *fakeTempUnschedCacheForError) GetTempUnsched(_ context.Context, _ int64) (*TempUnschedState, error) {
	return nil, nil
}

func (f *fakeTempUnschedCacheForError) DeleteTempUnsched(_ context.Context, _ int64) error {
	return nil
}

// --- helper：构造带配置的 RateLimitService ---

// newUpstreamErrorThresholdSvc 创建测试用 RateLimitService。
// initCount 是 errorCounterCache 的起始计数，调用一次 IncrementErrorCount 后返回 initCount+1。
func newUpstreamErrorThresholdSvc(t *testing.T, settings *StreamTimeoutSettings, initCount int64) (
	*RateLimitService, *internal500AccountRepoStub, *fakeErrorCounterCache, *fakeTempUnschedCacheForError,
) {
	t.Helper()
	repo := &internal500AccountRepoStub{}
	cache := &fakeErrorCounterCache{count: initCount} // IncrementErrorCount 先 +1，首次返回 initCount+1
	unschedCache := &fakeTempUnschedCacheForError{}

	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	settingSvc := NewSettingService(&settingRepoStub{
		values: map[string]string{SettingKeyStreamTimeoutSettings: string(raw)},
	}, &config.Config{})

	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, unschedCache)
	svc.SetErrorCounterCache(cache)
	svc.SetSettingService(settingSvc)
	return svc, repo, cache, unschedCache
}

// --- 测试 ---

func TestHandleUpstreamErrorThreshold_NilAccount(t *testing.T) {
	svc := &RateLimitService{}
	require.False(t, svc.HandleUpstreamErrorThreshold(context.Background(), nil, 500))
}

func TestHandleUpstreamErrorThreshold_NilSettingService(t *testing.T) {
	svc := &RateLimitService{}
	require.False(t, svc.HandleUpstreamErrorThreshold(context.Background(), &Account{ID: 1}, 500))
}

func TestHandleUpstreamErrorThreshold_Disabled(t *testing.T) {
	settings := &StreamTimeoutSettings{
		Enabled: false, Action: StreamTimeoutActionTempUnsched,
		ThresholdCount: 1, ThresholdWindowMinutes: 10, TempUnschedMinutes: 5,
	}
	svc, repo, cache, _ := newUpstreamErrorThresholdSvc(t, settings, 1)

	require.False(t, svc.HandleUpstreamErrorThreshold(context.Background(), &Account{ID: 10}, 500))
	require.Empty(t, repo.tempUnschedCalls)
	require.Zero(t, cache.resetCalls)
}

func TestHandleUpstreamErrorThreshold_ActionNone(t *testing.T) {
	settings := &StreamTimeoutSettings{
		Enabled: true, Action: StreamTimeoutActionNone,
		ThresholdCount: 1, ThresholdWindowMinutes: 10, TempUnschedMinutes: 5,
	}
	svc, repo, cache, _ := newUpstreamErrorThresholdSvc(t, settings, 1)

	require.False(t, svc.HandleUpstreamErrorThreshold(context.Background(), &Account{ID: 10}, 502))
	require.Empty(t, repo.tempUnschedCalls)
	require.Zero(t, cache.resetCalls)
}

func TestHandleUpstreamErrorThreshold_BelowThreshold(t *testing.T) {
	settings := &StreamTimeoutSettings{
		Enabled: true, Action: StreamTimeoutActionTempUnsched,
		ThresholdCount: 3, ThresholdWindowMinutes: 10, TempUnschedMinutes: 5,
	}
	// initCount=0 → IncrementErrorCount 返回 1，阈值 3 → 不触发
	svc, repo, cache, _ := newUpstreamErrorThresholdSvc(t, settings, 0)

	require.False(t, svc.HandleUpstreamErrorThreshold(context.Background(), &Account{ID: 10}, 500))
	require.Empty(t, repo.tempUnschedCalls)
	require.Zero(t, cache.resetCalls)
}

func TestHandleUpstreamErrorThreshold_TempUnsched_Triggered(t *testing.T) {
	settings := &StreamTimeoutSettings{
		Enabled: true, Action: StreamTimeoutActionTempUnsched,
		ThresholdCount: 3, ThresholdWindowMinutes: 10, TempUnschedMinutes: 5,
	}
	// initCount=2 → IncrementErrorCount 返回 3，达到阈值 → 触发
	svc, repo, cache, unschedCache := newUpstreamErrorThresholdSvc(t, settings, 2)
	account := &Account{ID: 42}

	before := time.Now()
	result := svc.HandleUpstreamErrorThreshold(context.Background(), account, 502)
	after := time.Now()

	require.True(t, result)

	// SetTempUnschedulable 被调用一次
	require.Len(t, repo.tempUnschedCalls, 1)
	call := repo.tempUnschedCalls[0]
	require.Equal(t, int64(42), call.accountID)
	require.WithinDuration(t, before.Add(5*time.Minute), call.until, after.Sub(before)+time.Second)
	require.Contains(t, call.reason, "502")

	// TempUnschedCache 也写入
	require.Len(t, unschedCache.setCalls, 1)
	state := unschedCache.setCalls[0]
	require.Equal(t, 502, state.StatusCode)
	require.Equal(t, "upstream_error", state.MatchedKeyword)

	// 计数器被重置
	require.Equal(t, 1, cache.resetCalls)
}

func TestHandleUpstreamErrorThreshold_ErrorAction_Triggered(t *testing.T) {
	settings := &StreamTimeoutSettings{
		Enabled: true, Action: StreamTimeoutActionError,
		ThresholdCount: 2, ThresholdWindowMinutes: 10, TempUnschedMinutes: 5,
	}
	// initCount=1 → IncrementErrorCount 返回 2，达到阈值 2 → 触发
	svc, repo, cache, _ := newUpstreamErrorThresholdSvc(t, settings, 1)

	result := svc.HandleUpstreamErrorThreshold(context.Background(), &Account{ID: 99}, 520)

	require.True(t, result)
	require.Len(t, repo.setErrorCalls, 1)
	require.Equal(t, int64(99), repo.setErrorCalls[0].accountID)
	require.Contains(t, repo.setErrorCalls[0].reason, "520")
	require.Equal(t, 1, cache.resetCalls)
}

func TestHandleUpstreamErrorThreshold_NilErrorCounterCache_FallsBackToOne(t *testing.T) {
	// 无 errorCounterCache → count 固定 1；阈值=1 时触发
	settings := &StreamTimeoutSettings{
		Enabled: true, Action: StreamTimeoutActionTempUnsched,
		ThresholdCount: 1, ThresholdWindowMinutes: 10, TempUnschedMinutes: 5,
	}
	repo := &internal500AccountRepoStub{}
	unschedCache := &fakeTempUnschedCacheForError{}

	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	settingSvc := NewSettingService(&settingRepoStub{
		values: map[string]string{SettingKeyStreamTimeoutSettings: string(raw)},
	}, &config.Config{})

	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, unschedCache)
	svc.SetSettingService(settingSvc)
	// 不设置 errorCounterCache

	require.True(t, svc.HandleUpstreamErrorThreshold(context.Background(), &Account{ID: 7}, 500))
	require.Len(t, repo.tempUnschedCalls, 1)
}

func TestHandleUpstreamErrorThreshold_MultiCall_ThirdTriggers(t *testing.T) {
	settings := &StreamTimeoutSettings{
		Enabled: true, Action: StreamTimeoutActionTempUnsched,
		ThresholdCount: 3, ThresholdWindowMinutes: 10, TempUnschedMinutes: 5,
	}
	// initCount=0 → 第 1 次返回 1，第 2 次返回 2，第 3 次返回 3
	svc, repo, _, _ := newUpstreamErrorThresholdSvc(t, settings, 0)
	account := &Account{ID: 55}
	ctx := context.Background()

	require.False(t, svc.HandleUpstreamErrorThreshold(ctx, account, 500), "第 1 次：未达阈值")
	require.False(t, svc.HandleUpstreamErrorThreshold(ctx, account, 500), "第 2 次：未达阈值")
	require.True(t, svc.HandleUpstreamErrorThreshold(ctx, account, 500), "第 3 次：触发")
	require.Len(t, repo.tempUnschedCalls, 1)
}

func TestIsUpstreamErrorThresholdStatus(t *testing.T) {
	cases := []struct {
		code    int
		matched bool
	}{
		{500, true},
		{502, true},
		{520, true},
		{400, false},
		{401, false},
		{429, false},
		{503, false},
	}
	for _, c := range cases {
		require.Equal(t, c.matched, isUpstreamErrorThresholdStatus(c.code), "status=%d", c.code)
	}
}

func TestIsPoolModeRetryableStatus_Includes520(t *testing.T) {
	require.True(t, isPoolModeRetryableStatus(520))
	require.True(t, isPoolModeRetryableStatus(500))
	require.True(t, isPoolModeRetryableStatus(502))
}
