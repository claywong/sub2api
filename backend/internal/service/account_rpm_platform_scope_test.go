package service

import (
	"context"
	"testing"
)

// accountRPMScopeCacheStub 返回固定的分钟计数，用于驱动三区判定。
type accountRPMScopeCacheStub struct {
	RPMCache
	counts map[int64]int
}

func (s *accountRPMScopeCacheStub) GetRPM(_ context.Context, accountID int64) (int, error) {
	return s.counts[accountID], nil
}

func (s *accountRPMScopeCacheStub) GetRPMBatch(_ context.Context, accountIDs []int64) (map[int64]int, error) {
	out := make(map[int64]int, len(accountIDs))
	for _, id := range accountIDs {
		out[id] = s.counts[id]
	}
	return out, nil
}

func rpmScopeAccount(id int64, platform, accType string, baseRPM int) *Account {
	return &Account{
		ID:          id,
		Platform:    platform,
		Type:        accType,
		Concurrency: 0,
		Extra:       map[string]any{"base_rpm": baseRPM, "rpm_sticky_buffer": 2},
	}
}

// TestAccountRPMAppliesToAllOAuthPlatforms 固定 base_rpm 限流的适用范围：
// 所有平台的 OAuth/SetupToken 账号都受限，apikey / bedrock 不受限。
// 历史行为只对 Anthropic OAuth/SetupToken 生效，这里防止回退。
func TestAccountRPMAppliesToAllOAuthPlatforms(t *testing.T) {
	const baseRPM = 10
	cases := []struct {
		name     string
		platform string
		accType  string
		limited  bool
	}{
		{"anthropic oauth", PlatformAnthropic, AccountTypeOAuth, true},
		{"anthropic setup token", PlatformAnthropic, AccountTypeSetupToken, true},
		{"openai oauth", PlatformOpenAI, AccountTypeOAuth, true},
		{"openai setup token", PlatformOpenAI, AccountTypeSetupToken, true},
		{"gemini oauth", PlatformGemini, AccountTypeOAuth, true},
		{"grok oauth", PlatformGrok, AccountTypeOAuth, true},
		{"antigravity oauth", PlatformAntigravity, AccountTypeOAuth, true},
		{"anthropic apikey not limited", PlatformAnthropic, AccountTypeAPIKey, false},
		{"openai apikey not limited", PlatformOpenAI, AccountTypeAPIKey, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := rpmScopeAccount(1, tc.platform, tc.accType, baseRPM)
			// 计数远超 base_rpm + buffer，落在红区
			cache := &accountRPMScopeCacheStub{counts: map[int64]int{1: baseRPM * 10}}

			gw := &GatewayService{rpmCache: cache}
			gotGateway := gw.isAccountSchedulableForRPM(context.Background(), acc, false)

			oa := &OpenAIGatewayService{rpmCache: cache}
			gotOpenAI := oa.isOpenAIAccountSchedulableForRPM(context.Background(), acc, false)

			wantSchedulable := !tc.limited
			if gotGateway != wantSchedulable {
				t.Errorf("GatewayService schedulable = %v, want %v", gotGateway, wantSchedulable)
			}
			if gotOpenAI != wantSchedulable {
				t.Errorf("OpenAIGatewayService schedulable = %v, want %v", gotOpenAI, wantSchedulable)
			}
		})
	}
}

// TestAccountRPMZeroMeansUnlimited 固定默认值语义：base_rpm=0 不限制。
func TestAccountRPMZeroMeansUnlimited(t *testing.T) {
	acc := rpmScopeAccount(1, PlatformOpenAI, AccountTypeOAuth, 0)
	cache := &accountRPMScopeCacheStub{counts: map[int64]int{1: 9999}}

	gw := &GatewayService{rpmCache: cache}
	if !gw.isAccountSchedulableForRPM(context.Background(), acc, false) {
		t.Error("base_rpm=0 应不限制（GatewayService）")
	}
	oa := &OpenAIGatewayService{rpmCache: cache}
	if !oa.isOpenAIAccountSchedulableForRPM(context.Background(), acc, false) {
		t.Error("base_rpm=0 应不限制（OpenAIGatewayService）")
	}
}

// TestAccountRPMTieredZonesForOpenAI 固定 OpenAI 侧三区语义：
// 绿区都可调度；黄区仅粘性；红区都不可调度。
func TestAccountRPMTieredZonesForOpenAI(t *testing.T) {
	const baseRPM = 10
	// buffer 显式设为 2 → 黄区为 [10, 12)
	tests := []struct {
		name          string
		current       int
		wantNonSticky bool
		wantSticky    bool
	}{
		{"green", baseRPM - 1, true, true},
		{"yellow lower bound", baseRPM, false, true},
		{"yellow upper bound", baseRPM + 1, false, true},
		{"red", baseRPM + 2, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acc := rpmScopeAccount(1, PlatformOpenAI, AccountTypeOAuth, baseRPM)
			cache := &accountRPMScopeCacheStub{counts: map[int64]int{1: tt.current}}
			oa := &OpenAIGatewayService{rpmCache: cache}

			if got := oa.isOpenAIAccountSchedulableForRPM(context.Background(), acc, false); got != tt.wantNonSticky {
				t.Errorf("non-sticky = %v, want %v (current=%d)", got, tt.wantNonSticky, tt.current)
			}
			if got := oa.isOpenAIAccountSchedulableForRPM(context.Background(), acc, true); got != tt.wantSticky {
				t.Errorf("sticky = %v, want %v (current=%d)", got, tt.wantSticky, tt.current)
			}
		})
	}
}

// TestAccountRPMFailOpenWithoutCache 固定 fail-open：无 Redis 时不阻塞调度。
func TestAccountRPMFailOpenWithoutCache(t *testing.T) {
	acc := rpmScopeAccount(1, PlatformOpenAI, AccountTypeOAuth, 1)

	gw := &GatewayService{}
	if !gw.isAccountSchedulableForRPM(context.Background(), acc, false) {
		t.Error("rpmCache 为 nil 时应 fail-open（GatewayService）")
	}
	oa := &OpenAIGatewayService{}
	if !oa.isOpenAIAccountSchedulableForRPM(context.Background(), acc, false) {
		t.Error("rpmCache 为 nil 时应 fail-open（OpenAIGatewayService）")
	}
}
