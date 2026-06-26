// ratelimit_service_upstream_error.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：上游错误阈值（500/502/520）相关方法。
//
// 本文件聚合 RateLimitService 上"上游错误阈值"和"健康封禁"相关的私有逻辑：
//   - SetErrorCounterCache：注入错误计数缓存（可选依赖）。
//   - HandleUpstreamErrorThreshold：核心入口，复用流超时阈值配置进行计数。
//   - triggerUpstreamErrorTempUnsched / triggerUpstreamErrorAccountError：
//     达阈触发临时不可调度 / 账号错误状态。
//   - SetTempUnschedulableForScheduledTest：供定时测试退火逻辑写入 TempUnsched。
//
// 与 upstream 合并策略：
//   - RateLimitService 上新增的字段 errorCounterCache 仍留在 ratelimit_service.go
//     （Go struct 必须整块定义）。
//   - 这些方法是纯增量，搬到 companion 文件后 ratelimit_service.go 与 upstream
//     的 diff 可以缩到 10 行内。
// =============================================================================
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// SetErrorCounterCache 设置上游错误计数器缓存（可选依赖）
func (s *RateLimitService) SetErrorCounterCache(cache ErrorCounterCache) {
	s.errorCounterCache = cache
}

// HandleUpstreamErrorThreshold 处理上游 500/502/520 错误的阈值检查。
// 复用流超时处理的阈值配置（窗口、次数、封禁时长），使用独立计数器。
// 非池模式：每次错误调用；池模式：同账号重试耗尽后调用。
// 返回是否已触发临时不可调度。
func (s *RateLimitService) HandleUpstreamErrorThreshold(ctx context.Context, account *Account, statusCode int) bool {
	if account == nil {
		return false
	}
	if s.settingService == nil {
		return false
	}

	settings, err := s.settingService.GetStreamTimeoutSettings(ctx)
	if err != nil {
		slog.Warn("upstream_error_threshold_get_settings_failed", "account_id", account.ID, "error", err)
		return false
	}
	if !settings.Enabled || settings.Action == StreamTimeoutActionNone {
		return false
	}

	var count int64 = 1
	if s.errorCounterCache != nil {
		count, err = s.errorCounterCache.IncrementErrorCount(ctx, account.ID, settings.ThresholdWindowMinutes)
		if err != nil {
			slog.Warn("upstream_error_threshold_increment_failed", "account_id", account.ID, "error", err)
			count = 1
		}
	}

	slog.Info("upstream_error_threshold_count",
		"account_id", account.ID,
		"status_code", statusCode,
		"count", count,
		"threshold", settings.ThresholdCount,
		"window_minutes", settings.ThresholdWindowMinutes,
	)

	if count < int64(settings.ThresholdCount) {
		return false
	}

	switch settings.Action {
	case StreamTimeoutActionTempUnsched:
		return s.triggerUpstreamErrorTempUnsched(ctx, account, settings, statusCode)
	case StreamTimeoutActionError:
		return s.triggerUpstreamErrorAccountError(ctx, account, statusCode)
	default:
		return false
	}
}

// triggerUpstreamErrorTempUnsched 上游错误达到阈值后触发临时不可调度
func (s *RateLimitService) triggerUpstreamErrorTempUnsched(ctx context.Context, account *Account, settings *StreamTimeoutSettings, statusCode int) bool {
	now := time.Now()
	until := now.Add(time.Duration(settings.TempUnschedMinutes) * time.Minute)

	state := &TempUnschedState{
		UntilUnix:       until.Unix(),
		TriggeredAtUnix: now.Unix(),
		StatusCode:      statusCode,
		MatchedKeyword:  "upstream_error",
		RuleIndex:       -1,
		ErrorMessage:    fmt.Sprintf("Upstream error %d exceeded threshold (%d times in %d min)", statusCode, settings.ThresholdCount, settings.ThresholdWindowMinutes),
	}

	reason := ""
	if raw, err := json.Marshal(state); err == nil {
		reason = string(raw)
	}
	if reason == "" {
		reason = state.ErrorMessage
	}

	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, reason); err != nil {
		slog.Warn("upstream_error_threshold_set_temp_unsched_failed", "account_id", account.ID, "error", err)
		return false
	}

	if s.tempUnschedCache != nil {
		if err := s.tempUnschedCache.SetTempUnsched(ctx, account.ID, state); err != nil {
			slog.Warn("upstream_error_threshold_set_temp_unsched_cache_failed", "account_id", account.ID, "error", err)
		}
	}

	if s.errorCounterCache != nil {
		if err := s.errorCounterCache.ResetErrorCount(ctx, account.ID); err != nil {
			slog.Warn("upstream_error_threshold_reset_count_failed", "account_id", account.ID, "error", err)
		}
	}

	slog.Info("upstream_error_threshold_temp_unschedulable",
		"account_id", account.ID,
		"status_code", statusCode,
		"until", until,
	)
	return true
}

// triggerUpstreamErrorAccountError 上游错误达到阈值后标记账号错误状态
func (s *RateLimitService) triggerUpstreamErrorAccountError(ctx context.Context, account *Account, statusCode int) bool {
	errorMsg := fmt.Sprintf("Upstream error %d exceeded threshold (repeated failures)", statusCode)

	if err := s.accountRepo.SetError(ctx, account.ID, errorMsg); err != nil {
		slog.Warn("upstream_error_threshold_set_error_failed", "account_id", account.ID, "error", err)
		return false
	}

	if s.errorCounterCache != nil {
		if err := s.errorCounterCache.ResetErrorCount(ctx, account.ID); err != nil {
			slog.Warn("upstream_error_threshold_reset_count_failed", "account_id", account.ID, "error", err)
		}
	}

	slog.Warn("upstream_error_threshold_account_error", "account_id", account.ID, "status_code", statusCode)
	return true
}

// SetTempUnschedulableForScheduledTest 供定时测试退火逻辑调用，持久化 TempUnschedulable 状态。
func (s *RateLimitService) SetTempUnschedulableForScheduledTest(ctx context.Context, accountID int64, until time.Time, errorMessage string) error {
	const reason = "scheduled_test_consecutive_failures"
	if err := s.accountRepo.SetTempUnschedulable(ctx, accountID, until, reason); err != nil {
		return err
	}
	if s.tempUnschedCache != nil {
		state := &TempUnschedState{
			UntilUnix:       until.Unix(),
			TriggeredAtUnix: time.Now().Unix(),
			ErrorMessage:    errorMessage,
		}
		if err := s.tempUnschedCache.SetTempUnsched(ctx, accountID, state); err != nil {
			slog.Warn("temp_unsched_cache_set_failed", "account_id", accountID, "error", err)
		}
	}
	slog.Info("account_temp_unschedulable_by_scheduled_test", "account_id", accountID, "until", until)
	return nil
}
