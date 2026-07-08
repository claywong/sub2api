// gateway_anthropic_full_passthrough.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：Anthropic 账号「完整透传」模式
// （accounts.extra.anthropic_passthrough_mode=full）。
//
// 与「API Key 透传」（gateway_anthropic_passthrough.go）的区别：
//   - 完整透传允许 OAuth/Setup Token/API Key 任意账号类型直连上游，鉴权方式
//     由 tokenType 决定（oauth 用 Bearer，其余用 x-api-key）；
//   - 不做 StripEmptyTextBlocks / FilterWebSearchHistoryBlocks 等 body 预处理，
//     真正做到「原样透传」。
//
// merge 策略：全新文件，无 upstream 冲突（upstream 没有此功能）。
// =============================================================================
package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"

	"github.com/gin-gonic/gin"
)

// deleteHeaderRaw 删除 header，同时清理原始大小写残留的 key。
func deleteHeaderRaw(h http.Header, key string) {
	if h == nil {
		return
	}
	h.Del(key)
	if wireKey := resolveWireCasing(key); wireKey != key {
		delete(h, wireKey)
	}
	delete(h, key)
}

// copyAllowedPassthroughHeaders 只复制白名单内的请求头，避免把入站鉴权/cookie 等敏感头透传给上游。
func copyAllowedPassthroughHeaders(dst http.Header, src http.Header) {
	if dst == nil || src == nil {
		return
	}
	for key, values := range src {
		lowerKey := strings.ToLower(strings.TrimSpace(key))
		if !allowedHeaders[lowerKey] {
			continue
		}
		wireKey := resolveWireCasing(key)
		for _, v := range values {
			addHeaderRaw(dst, wireKey, v)
		}
	}
}

// buildAnthropicPassthroughTargetURL 计算上游目标 URL。
// 支持账号自带 base_url、custom_base_url 启用、以及默认 Anthropic 官方端点。
func (s *GatewayService) buildAnthropicPassthroughTargetURL(account *Account, path string) (string, error) {
	var targetURL string
	switch path {
	case "/v1/messages":
		targetURL = claudeAPIURL
	case "/v1/messages/count_tokens":
		targetURL = claudeAPICountTokensURL
	default:
		return "", fmt.Errorf("unsupported anthropic passthrough path: %s", path)
	}

	if account == nil {
		return targetURL, nil
	}

	if account.Type == AccountTypeAPIKey {
		baseURL := account.GetBaseURL()
		if baseURL == "" {
			return targetURL, nil
		}
		validatedURL, err := s.validateUpstreamBaseURL(baseURL)
		if err != nil {
			return "", err
		}
		return validatedURL + path + "?beta=true", nil
	}

	if account.IsCustomBaseURLEnabled() {
		customURL := account.GetCustomBaseURL()
		if customURL == "" {
			return "", fmt.Errorf("custom_base_url is enabled but not configured for account %d", account.ID)
		}
		validatedURL, err := s.validateUpstreamBaseURL(customURL)
		if err != nil {
			return "", err
		}
		return s.buildCustomRelayURL(validatedURL, path, account), nil
	}

	return targetURL, nil
}

// resolveAnthropicPassthroughProxyURL 计算应使用的出站代理 URL。
// 当账号启用 custom_base_url 且配置了非空地址时，跳过代理（直连用户自定义中转站）。
func resolveAnthropicPassthroughProxyURL(account *Account) string {
	if account == nil || account.ProxyID == nil || account.Proxy == nil {
		return ""
	}
	if account.IsCustomBaseURLEnabled() && account.GetCustomBaseURL() != "" {
		return ""
	}
	return account.Proxy.URL()
}

// buildAnthropicSafePassthroughRequest 构造一次「安全透传」的 HTTP 请求：
// 同时支持 OAuth Bearer 与 API Key（x-api-key）两种鉴权方式，
// 并清理客户端请求中可能泄漏的 cookie 与第三方 token，
// 自动补齐 content-type / anthropic-version 默认值。
func (s *GatewayService) buildAnthropicSafePassthroughRequest(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	token string,
	tokenType string,
	path string,
) (*http.Request, []byte, error) {
	targetURL, err := s.buildAnthropicPassthroughTargetURL(account, path)
	if err != nil {
		return nil, nil, err
	}

	// 能力维度 body sanitize：透传路径上 anthropic-beta header 原样透传客户端值，
	// 依此决定是否保留 body 中的 context_management。避免"客户端 body 带字段但
	// header 忘记带 beta token"的客户端 bug 在透传场景下让上游 400。
	clientBeta := ""
	if c != nil && c.Request != nil {
		clientBeta = getHeaderRaw(c.Request.Header, "anthropic-beta")
	}
	if sanitized, changed := sanitizeAnthropicBodyForBetaTokens(body, clientBeta); changed {
		body = sanitized
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}

	if c != nil && c.Request != nil {
		copyAllowedPassthroughHeaders(req.Header, c.Request.Header)
	}

	deleteHeaderRaw(req.Header, "authorization")
	deleteHeaderRaw(req.Header, "x-api-key")
	deleteHeaderRaw(req.Header, "x-goog-api-key")
	deleteHeaderRaw(req.Header, "cookie")

	if tokenType == "oauth" {
		setHeaderRaw(req.Header, "authorization", "Bearer "+token)
	} else {
		setHeaderRaw(req.Header, "x-api-key", token)
	}

	if getHeaderRaw(req.Header, "content-type") == "" {
		setHeaderRaw(req.Header, "content-type", "application/json")
	}
	if getHeaderRaw(req.Header, "anthropic-version") == "" {
		setHeaderRaw(req.Header, "anthropic-version", "2023-06-01")
	}

	// 账号级请求头覆写（最终生效，覆盖上面所有来源的同名头）
	account.ApplyHeaderOverrides(req.Header)

	return req, body, nil
}

func (s *GatewayService) buildUpstreamRequestAnthropicFullPassthrough(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	token string,
	tokenType string,
) (*http.Request, []byte, error) {
	return s.buildAnthropicSafePassthroughRequest(ctx, c, account, body, token, tokenType, "/v1/messages")
}

// forwardAnthropicFullPassthroughWithInput 是「完整透传」模式的转发实现。
// 与 forwardAnthropicAPIKeyPassthroughWithInput 结构一致（重试/failover/响应处理复用同一套
// handleStreamingResponseAnthropicAPIKeyPassthrough / handleNonStreamingResponseAnthropicAPIKeyPassthrough），
// 差异仅在于：允许 OAuth/Setup Token 账号、不做 body 预处理、请求构建走 buildAnthropicSafePassthroughRequest。
func (s *GatewayService) forwardAnthropicFullPassthroughWithInput(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	input anthropicPassthroughForwardInput,
) (*ForwardResult, error) {
	token, tokenType, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	if tokenType != "apikey" && tokenType != "oauth" {
		return nil, fmt.Errorf("anthropic full passthrough requires apikey or oauth token, got: %s", tokenType)
	}

	proxyURL := resolveAnthropicPassthroughProxyURL(account)

	logger.LegacyPrintf("service.gateway", "[Anthropic 完整透传] 命中 Full 透传分支: account=%d name=%s model=%s stream=%v",
		account.ID, account.Name, input.RequestModel, input.RequestStream)

	if c != nil {
		c.Set("anthropic_passthrough", true)
	}

	setOpsUpstreamRequestBody(c, input.Body)

	var resp *http.Response
	retryStart := time.Now()
	for attempt := 1; attempt <= maxRetryAttempts; attempt++ {
		upstreamCtx, releaseUpstreamCtx := detachStreamUpstreamContext(ctx, input.RequestStream)
		upstreamReq, wireBody, err := s.buildUpstreamRequestAnthropicFullPassthrough(upstreamCtx, c, account, input.Body, token, tokenType)
		releaseUpstreamCtx()
		if err != nil {
			return nil, err
		}
		if input.Parsed != nil && !bytes.Equal(wireBody, input.Body) {
			// build 阶段会按 beta 能力清理 body，发送前同步到 ParsedRequest 当前视图。
			if err := input.Parsed.ReplaceBody(wireBody); err != nil {
				return nil, err
			}
			input.Body = input.Parsed.Body.Bytes()
		}

		resp, err = s.httpUpstream.DoWithTLS(upstreamReq, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account), 0)
		if err != nil {
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			safeErr := sanitizeUpstreamErrorMessage(err.Error())
			setOpsUpstreamError(c, 0, safeErr, "")
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: 0,
				UpstreamURL:        safeUpstreamURL(upstreamReq.URL.String()),
				Passthrough:        true,
				Kind:               "request_error",
				Message:            safeErr,
			})
			return nil, &UpstreamFailoverError{
				StatusCode:             http.StatusBadGateway,
				ResponseBody:           []byte(fmt.Sprintf(`{"type":"error","error":{"type":"upstream_error","message":%q}}`, safeErr)),
				RetryableOnSameAccount: true,
			}
		}

		// 透传分支禁止 400 请求体降级重试（该重试会改写请求体）
		if resp.StatusCode >= 400 && resp.StatusCode != 400 && s.shouldRetryUpstreamError(account, resp.StatusCode) {
			if attempt < maxRetryAttempts {
				elapsed := time.Since(retryStart)
				if elapsed >= maxRetryElapsed {
					break
				}

				delay := retryBackoffDelay(attempt)
				remaining := maxRetryElapsed - elapsed
				if delay > remaining {
					delay = remaining
				}
				if delay <= 0 {
					break
				}

				respBody, _ := s.readUpstreamErrorBody(resp)
				_ = resp.Body.Close()
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: resp.StatusCode,
					UpstreamRequestID:  resp.Header.Get("x-request-id"),
					UpstreamURL:        safeUpstreamURL(upstreamReq.URL.String()),
					Passthrough:        true,
					Kind:               "retry",
					Message:            extractUpstreamErrorMessage(respBody),
					Detail: func() string {
						if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
							return truncateString(string(respBody), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
						}
						return ""
					}(),
				})
				logger.LegacyPrintf("service.gateway", "Anthropic full passthrough account %d: upstream error %d, retry %d/%d after %v (elapsed=%v/%v)",
					account.ID, resp.StatusCode, attempt, maxRetryAttempts, delay, elapsed, maxRetryElapsed)
				if err := sleepWithContext(ctx, delay); err != nil {
					return nil, err
				}
				continue
			}
			break
		}

		break
	}
	if resp == nil || resp.Body == nil {
		return nil, errors.New("upstream request failed: empty response")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 && s.shouldRetryUpstreamError(account, resp.StatusCode) {
		if s.shouldFailoverUpstreamError(resp.StatusCode) {
			respBody, _ := s.readUpstreamErrorBody(resp)
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(respBody))

			logger.LegacyPrintf("service.gateway", "[Anthropic Full Passthrough] Upstream error (retry exhausted, failover): Account=%d(%s) Status=%d RequestID=%s Body=%s",
				account.ID, account.Name, resp.StatusCode, resp.Header.Get("x-request-id"), truncateString(string(respBody), 1000))

			s.handleRetryExhaustedSideEffects(ctx, resp, account)
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				UpstreamRequestID:  resp.Header.Get("x-request-id"),
				Passthrough:        true,
				Kind:               "retry_exhausted_failover",
				Message:            extractUpstreamErrorMessage(respBody),
				Detail: func() string {
					if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
						return truncateString(string(respBody), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
					}
					return ""
				}(),
			})
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}
		return s.handleRetryExhaustedError(ctx, resp, c, account)
	}

	if resp.StatusCode >= 400 && s.shouldFailoverUpstreamError(resp.StatusCode) {
		respBody, _ := s.readUpstreamErrorBody(resp)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBody))

		logger.LegacyPrintf("service.gateway", "[Anthropic Full Passthrough] Upstream error (failover): Account=%d(%s) Status=%d RequestID=%s Body=%s",
			account.ID, account.Name, resp.StatusCode, resp.Header.Get("x-request-id"), truncateString(string(respBody), 1000))

		s.handleFailoverSideEffects(ctx, resp, account, input.RequestModel)
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Passthrough:        true,
			Kind:               "failover",
			Message:            extractUpstreamErrorMessage(respBody),
			Detail: func() string {
				if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
					return truncateString(string(respBody), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
				}
				return ""
			}(),
		})
		return nil, &UpstreamFailoverError{
			StatusCode:             resp.StatusCode,
			ResponseBody:           respBody,
			RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
		}
	}

	if resp.StatusCode >= 400 {
		return s.handleErrorResponse(ctx, resp, c, account, input.RequestModel)
	}

	var usage *ClaudeUsage
	var firstTokenMs *int
	var clientDisconnect bool
	var capturedBody string
	if input.RequestStream {
		streamResult, err := s.handleStreamingResponseAnthropicAPIKeyPassthrough(ctx, resp, c, account, input.StartTime, input.RequestModel)
		if err != nil {
			return nil, err
		}
		usage = streamResult.usage
		firstTokenMs = streamResult.firstTokenMs
		clientDisconnect = streamResult.clientDisconnect
		capturedBody = streamResult.capturedBody
	} else {
		usage, capturedBody, err = s.handleNonStreamingResponseAnthropicAPIKeyPassthrough(ctx, resp, c, account)
		if err != nil {
			return nil, err
		}
	}
	if usage == nil {
		usage = &ClaudeUsage{}
	}

	return &ForwardResult{
		RequestID:            resp.Header.Get("x-request-id"),
		Usage:                *usage,
		Model:                input.OriginalModel,
		UpstreamModel:        input.RequestModel,
		Stream:               input.RequestStream,
		Duration:             time.Since(input.StartTime),
		FirstTokenMs:         firstTokenMs,
		CapturedResponseBody: capturedBody,
		ClientDisconnect:     clientDisconnect,
	}, nil
}

// forwardCountTokensAnthropicFullPassthrough 走"完整透传"模式的 count_tokens 入口：
// 同时支持 OAuth 与 API Key 鉴权，使用 buildAnthropicSafePassthroughRequest 构造请求。
func (s *GatewayService) forwardCountTokensAnthropicFullPassthrough(ctx context.Context, c *gin.Context, account *Account, body []byte) error {
	token, tokenType, err := s.GetAccessToken(ctx, account)
	if err != nil {
		s.countTokensError(c, http.StatusBadGateway, "upstream_error", "Failed to get access token")
		return err
	}
	if tokenType != "apikey" && tokenType != "oauth" {
		s.countTokensError(c, http.StatusBadGateway, "upstream_error", "Invalid account token type")
		return fmt.Errorf("anthropic full passthrough requires apikey or oauth token, got: %s", tokenType)
	}

	upstreamReq, _, err := s.buildCountTokensRequestAnthropicFullPassthrough(ctx, c, account, body, token, tokenType)
	if err != nil {
		s.countTokensError(c, http.StatusInternalServerError, "api_error", "Failed to build request")
		return err
	}

	proxyURL := resolveAnthropicPassthroughProxyURL(account)

	resp, err := s.httpUpstream.DoWithTLS(upstreamReq, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account), 0)
	if err != nil {
		setOpsUpstreamError(c, 0, sanitizeUpstreamErrorMessage(err.Error()), "")
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: 0,
			UpstreamURL:        safeUpstreamURL(upstreamReq.URL.String()),
			Passthrough:        true,
			Kind:               "request_error",
			Message:            sanitizeUpstreamErrorMessage(err.Error()),
		})
		s.countTokensError(c, http.StatusBadGateway, "upstream_error", "Request failed")
		return fmt.Errorf("upstream request failed: %w", err)
	}

	countTokensTooLarge := func(c *gin.Context) {
		s.countTokensError(c, http.StatusBadGateway, "upstream_error", "Upstream response too large")
	}
	respBody, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, countTokensTooLarge)
	_ = resp.Body.Close()
	if err != nil {
		if !errors.Is(err, ErrUpstreamResponseBodyTooLarge) {
			s.countTokensError(c, http.StatusBadGateway, "upstream_error", "Failed to read response")
		}
		return err
	}

	if resp.StatusCode >= 400 {
		if s.rateLimitService != nil {
			s.rateLimitService.HandleUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
		}

		upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
		upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)

		// 中转站不支持 count_tokens 端点时（404），返回 404 让客户端 fallback 到本地估算。
		// 仅在错误消息明确指向 count_tokens endpoint 不存在时生效，避免误吞其他 404（如错误 base_url）。
		// 返回 nil 避免 handler 层记录为错误，也不设置 ops 上游错误上下文。
		if isCountTokensUnsupported404(resp.StatusCode, respBody) {
			logger.LegacyPrintf("service.gateway",
				"[count_tokens] Upstream does not support count_tokens (404), returning 404: account=%d name=%s msg=%s",
				account.ID, account.Name, truncateString(upstreamMsg, 512))
			s.countTokensError(c, http.StatusNotFound, "not_found_error", "count_tokens endpoint is not supported by upstream")
			return nil
		}

		upstreamDetail := ""
		if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
			maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
			if maxBytes <= 0 {
				maxBytes = 2048
			}
			upstreamDetail = truncateString(string(respBody), maxBytes)
		}
		setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, upstreamDetail)
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			UpstreamURL:        safeUpstreamURL(upstreamReq.URL.String()),
			Passthrough:        true,
			Kind:               "http_error",
			Message:            upstreamMsg,
			Detail:             upstreamDetail,
		})

		errMsg := "Upstream request failed"
		switch resp.StatusCode {
		case 429:
			errMsg = "Rate limit exceeded"
		case 529:
			errMsg = "Service overloaded"
		}
		s.countTokensError(c, resp.StatusCode, "upstream_error", errMsg)
		if upstreamMsg == "" {
			return fmt.Errorf("upstream error: %d", resp.StatusCode)
		}
		return fmt.Errorf("upstream error: %d message=%s", resp.StatusCode, upstreamMsg)
	}

	writeAnthropicPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/json"
	}
	c.Data(resp.StatusCode, contentType, respBody)
	return nil
}

// buildCountTokensRequestAnthropicFullPassthrough 是 full passthrough 模式下
// 的请求构造函数。
func (s *GatewayService) buildCountTokensRequestAnthropicFullPassthrough(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	token string,
	tokenType string,
) (*http.Request, []byte, error) {
	return s.buildAnthropicSafePassthroughRequest(ctx, c, account, body, token, tokenType, "/v1/messages/count_tokens")
}
