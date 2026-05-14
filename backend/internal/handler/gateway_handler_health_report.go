// gateway_handler_health_report.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：账号健康样本上报相关方法。
//
// 主体方法 reportAnthropicForwardResult 是一个~95 行的纯增量方法，承担：
//   - 把单次 Forward 结果（成败 + TTFT + Duration + OutputTokens + TCP/TTFB）
//     转成 service.CallSample 写入健康缓存；
//   - 失败样本输出 warn 日志，方便排查账号进入 StickyOnly/Excluded 的原因；
//   - 成功但窗口指标退化（TTFT/OTPS 跨越阈值）时输出告警。
//
// 与 upstream 合并策略：
//   - 业务调用 reportAnthropicForwardResult 的若干处分散在 gateway_handler.go
//     的 forward 主路径里，**这些 inline 调用只能留在原文件**，无法迁移。
//   - 但函数本体是独立的，搬到 companion 文件后 gateway_handler.go 与 upstream
//     的 diff 主要剩下几处单行 inline 调用（reportAnthropicForwardResult、
//     ResolveRequestID、WriteRequestLog、ExtractClientSessionID）。
// =============================================================================
package handler

import (
	"context"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"go.uber.org/zap"
)

// reportAnthropicForwardResult 把单次 Forward 调用的样本（成败 + TTFT + Duration + OutputTokens）
// 上报给健康缓存，进入 10 分钟滑动窗口。让"主动 test"和"用户真实调用"在窗口指标上融合，
// 供 HealthVerdict 三态判定与 weighted 算法的加权打分使用。
//
// 失败语义与 ops error_owner 分类对齐——只有 error_owner='provider' 的情况才计为失败：
//   - UpstreamFailoverError（上游 4xx/5xx/429/超时触发 failover）→ 计为失败
//   - 网络/传输层错误 → 计为失败
//   - context.DeadlineExceeded（响应头超时等）→ 计为失败
//   - 客户端中断（context.Canceled）→ 跳过（不是账号问题）
//   - 请求内容问题（BetaBlockedError / ClientRequestError）→ 跳过（error_owner='client'）
//   - 流正常但客户端中途断开（result.ClientDisconnect）→ 跳过
func (h *GatewayHandler) reportAnthropicForwardResult(account *service.Account, err error, result *service.ForwardResult) {
	if h == nil || h.gatewayService == nil {
		return
	}
	if account == nil || account.Platform != service.PlatformAnthropic {
		return
	}

	// 不上报的几种情况（对应 error_owner='client' 或非账号问题）
	if err != nil {
		var betaBlocked *service.BetaBlockedError
		if errors.As(err, &betaBlocked) {
			return
		}
		// 客户端请求内容问题（400 invalid_request_error 等），error_owner='client'
		var clientReqErr *service.ClientRequestError
		if errors.As(err, &clientReqErr) {
			return
		}
		if errors.Is(err, context.Canceled) {
			return
		}
	} else if result != nil && result.ClientDisconnect {
		return
	}

	sample := service.CallSample{Success: err == nil}
	if result != nil {
		if result.FirstTokenMs != nil && *result.FirstTokenMs > 0 {
			sample.TTFTMs = *result.FirstTokenMs
		}
		if result.Duration > 0 {
			sample.DurationMs = int(result.Duration.Milliseconds())
		}
		if result.Usage.OutputTokens > 0 {
			sample.OutputTokens = result.Usage.OutputTokens
		}
		if result.TCPConnMs > 0 {
			sample.TCPConnMs = result.TCPConnMs
		}
		if result.TTFBMs > 0 {
			sample.TTFBMs = result.TTFBMs
		}
		sample.CacheReadTokens = result.Usage.CacheReadInputTokens
		sample.CacheCreationTokens = result.Usage.CacheCreationInputTokens
		sample.InputTokens = result.Usage.InputTokens
	}

	// 失败样本写入健康缓存时打 warn，便于排查账号进入 StickyOnly/Excluded 的原因。
	// 注意：failover 路径下每次切换账号都会调用此函数，所以一次用户请求可能产生多条 warn。
	if !sample.Success {
		fields := []zap.Field{
			zap.Int64("account_id", account.ID),
			zap.String("account_name", account.Name),
			zap.Error(err),
		}
		if sample.TTFTMs > 0 {
			fields = append(fields, zap.Int("ttft_ms", sample.TTFTMs))
		}
		if sample.DurationMs > 0 {
			fields = append(fields, zap.Int("duration_ms", sample.DurationMs))
		}
		logger.L().Warn("gateway.anthropic_health_failure_recorded", fields...)
	} else if sample.TTFTMs > 0 || sample.DurationMs > 0 {
		// 成功但窗口指标异常（TTFT 或 OTPS 可能触发 StickyOnly 判定）时记录，便于关联分析。
		// 阈值直接取 HealthVerdict 配置，与判定逻辑保持一致，避免魔法值。
		snap := h.gatewayService.AnthropicHealthSnapshot(account.ID)
		thresholds := h.gatewayService.AnthropicHealthVerdictThresholds()
		ttfgDegraded := snap.TTFTSampleCount > 0 && snap.TTFTAvg() >= float64(thresholds.TTFTStickyOnlyMs)
		otpsDegraded := snap.OTPSSampleCount > 0 && snap.OTPSAvg() < thresholds.OTPSStickyOnlyMin
		if ttfgDegraded || otpsDegraded {
			logger.L().Warn("gateway.anthropic_health_degraded",
				zap.Int64("account_id", account.ID),
				zap.String("account_name", account.Name),
				zap.Int("ttft_ms", sample.TTFTMs),
				zap.Int("duration_ms", sample.DurationMs),
				zap.Int("output_tokens", sample.OutputTokens),
				zap.Float64("window_ttft_avg_ms", snap.TTFTAvg()),
				zap.Float64("window_otps_avg", snap.OTPSAvg()),
				zap.Float64("window_err_rate", snap.ErrRate()),
				zap.Int("window_req_count", snap.ReqCount),
			)
		}
	}

	h.gatewayService.RecordAnthropicCall(account.ID, sample)
}
