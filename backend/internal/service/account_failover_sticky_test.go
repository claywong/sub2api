//go:build unit

// 私有扩展测试（不属于 upstream sub2api）
//
// 覆盖 account_failover_sticky.go 的「救火号」开关：
//   - 开关读取的各种 Credentials 形态
//   - ctx failover 标记的读写
//   - skipStickyBindForFailover 的二维真值表
//
// @author wangzhong
package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSkipsStickyBindOnFailover(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		want    bool
	}{
		{name: "nil account", account: nil, want: false},
		{name: "nil credentials", account: &Account{}, want: false},
		{name: "key absent", account: &Account{Credentials: map[string]any{"other": true}}, want: false},
		{name: "enabled", account: &Account{Credentials: map[string]any{"failover_no_sticky": true}}, want: true},
		{name: "explicit false", account: &Account{Credentials: map[string]any{"failover_no_sticky": false}}, want: false},
		{name: "nil value", account: &Account{Credentials: map[string]any{"failover_no_sticky": nil}}, want: false},
		// JSON 往返或管理端误填可能产生非 bool 值，一律按未开启处理，不做字符串宽容解析：
		// 开关语义必须显式，避免 "false" 被当成真。
		{name: "string true not accepted", account: &Account{Credentials: map[string]any{"failover_no_sticky": "true"}}, want: false},
		{name: "number not accepted", account: &Account{Credentials: map[string]any{"failover_no_sticky": 1}}, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.account.SkipsStickyBindOnFailover())
		})
	}
}

func TestFailoverAttemptContext(t *testing.T) {
	t.Run("unmarked context is first attempt", func(t *testing.T) {
		require.False(t, FailoverAttemptFromContext(context.Background()))
	})

	t.Run("nil context", func(t *testing.T) {
		require.False(t, FailoverAttemptFromContext(nil))
		require.Nil(t, WithFailoverAttempt(nil, true))
	})

	t.Run("marked true", func(t *testing.T) {
		ctx := WithFailoverAttempt(context.Background(), true)
		require.True(t, FailoverAttemptFromContext(ctx))
	})

	t.Run("marked false", func(t *testing.T) {
		ctx := WithFailoverAttempt(context.Background(), false)
		require.False(t, FailoverAttemptFromContext(ctx))
	})

	// 重试循环每轮都会重写标记，后写的必须覆盖先写的。
	t.Run("rewrite overrides", func(t *testing.T) {
		ctx := WithFailoverAttempt(context.Background(), false)
		ctx = WithFailoverAttempt(ctx, true)
		require.True(t, FailoverAttemptFromContext(ctx))

		ctx = WithFailoverAttempt(ctx, false)
		require.False(t, FailoverAttemptFromContext(ctx))
	})
}

// failoverStickyBindRecorderCache 记录 SetSessionAccountID 的实际写入，
// 用于验证「绑定真的被跳过」而非仅验证谓词返回值。
type failoverStickyBindRecorderCache struct {
	GatewayCache

	boundAccountIDs []int64
}

func (c *failoverStickyBindRecorderCache) SetSessionAccountID(_ context.Context, _ int64, _ string, accountID int64, _ time.Duration) error {
	c.boundAccountIDs = append(c.boundAccountIDs, accountID)
	return nil
}

// TestBindGatewayStickySessionDuringSelectionSkipsFailoverAccount 覆盖接线本身：
// 调度层调用 bindGatewayStickySessionDuringSelection 时，救火号在 failover attempt
// 上不得产生任何 Redis 写入。
func TestBindGatewayStickySessionDuringSelectionSkipsFailoverAccount(t *testing.T) {
	const sessionHash = "session-abc"
	groupID := int64(7)

	rescue := &Account{ID: 101, Credentials: map[string]any{"failover_no_sticky": true}}
	normal := &Account{ID: 202, Credentials: map[string]any{}}

	tests := []struct {
		name     string
		account  *Account
		failover bool
		wantBind []int64
	}{
		{
			name:    "rescue account on failover attempt is not bound",
			account: rescue, failover: true, wantBind: nil,
		},
		{
			name:    "rescue account on first attempt is bound",
			account: rescue, failover: false, wantBind: []int64{101},
		},
		{
			name:    "normal account on failover attempt is bound",
			account: normal, failover: true, wantBind: []int64{202},
		},
		{
			name:    "nil account is a no-op",
			account: nil, failover: true, wantBind: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cache := &failoverStickyBindRecorderCache{}
			svc := &GatewayService{cache: cache}

			ctx := WithFailoverAttempt(context.Background(), tc.failover)
			require.NoError(t, svc.bindGatewayStickySessionDuringSelection(ctx, &groupID, sessionHash, tc.account))
			require.Equal(t, tc.wantBind, cache.boundAccountIDs)
		})
	}
}

func TestSkipStickyBindForFailover(t *testing.T) {
	switchOn := &Account{ID: 1, Credentials: map[string]any{"failover_no_sticky": true}}
	switchOff := &Account{ID: 2, Credentials: map[string]any{}}

	tests := []struct {
		name     string
		account  *Account
		failover bool
		want     bool
		reason   string
	}{
		{
			name: "switch on + failover attempt", account: switchOn, failover: true, want: true,
			reason: "救火号被 failover 选中：唯一应跳过绑定的组合",
		},
		{
			name: "switch on + first attempt", account: switchOn, failover: false, want: false,
			reason: "首次 attempt 选中救火号仍接管会话（仅 failover 不粘语义）",
		},
		{
			name: "switch off + failover attempt", account: switchOff, failover: true, want: false,
			reason: "普通账号 failover 命中后照常接管，保持 upstream 行为",
		},
		{
			name: "switch off + first attempt", account: switchOff, failover: false, want: false,
		},
		{
			name: "nil account", account: nil, failover: true, want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := WithFailoverAttempt(context.Background(), tc.failover)
			require.Equal(t, tc.want, skipStickyBindForFailover(ctx, tc.account), tc.reason)
		})
	}
}
