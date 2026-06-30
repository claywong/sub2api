package service

// 私有扩展（不属于 upstream sub2api）
//
// 所含方法：
//   - (*Account).SupportsSessionLimit
//
// 背景：upstream 的「会话数量控制」（最大并发会话数 + 空闲超时）仅对 Anthropic
// OAuth / SetupToken 账号生效（见 IsAnthropicOAuthOrSetupToken）。本 fork 需要让该
// 能力对 Anthropic API Key 账号也生效，因此抽出独立的 SupportsSessionLimit 谓词，
// 在所有「会话限制」判定点替换原 IsAnthropicOAuthOrSetupToken 调用。
//
// merge 策略：纯增量，永不与 upstream 冲突。若未来 upstream 自行支持 API Key 会话
// 限制，删除本文件并把调用点改回 upstream 的判定即可。
//
// @author wangzhong

// SupportsSessionLimit 判断账号是否启用「会话数量控制」能力。
//
// 与 upstream 的 IsAnthropicOAuthOrSetupToken 的唯一区别：额外放开 Anthropic API Key
// 账号。其余底层设施（GetMaxSessions / GetSessionIdleTimeoutMinutes / sessionLimitCache /
// GenerateSessionHash）本就与账号类型无关，无需改动。
//
// 注意：窗口费用控制（5h window cost）与 RPM 限制仍只对 OAuth/SetupToken 生效，
// 不在本谓词放开范围内。
func (a *Account) SupportsSessionLimit() bool {
	if a == nil {
		return false
	}
	return a.Platform == PlatformAnthropic &&
		(a.Type == AccountTypeOAuth || a.Type == AccountTypeSetupToken || a.Type == AccountTypeAPIKey)
}
