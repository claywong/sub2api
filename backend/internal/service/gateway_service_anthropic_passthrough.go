// gateway_service_anthropic_passthrough.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：Anthropic 平台 count_tokens 与 messages
// 透传相关的辅助方法。
//
// 这里聚合 4 类纯新增（upstream 不存在）的逻辑：
//   - 目标 URL/代理 URL 解析：buildAnthropicPassthroughTargetURL、
//     resolveAnthropicPassthroughProxyURL；
//   - 通用请求构建：buildAnthropicSafePassthroughRequest（OAuth/APIKey 都可用）；
//   - count_tokens 通用 forward 框架：forwardCountTokensAnthropicPassthrough，
//     使用回调模式接受不同模式（API Key / Full Passthrough）的 build 函数；
//   - "full passthrough" 模式专用方法：forwardCountTokensAnthropicFullPassthrough、
//     buildCountTokensRequestAnthropicFullPassthrough、
//     buildCountTokensRequestAnthropicAPIKeyPassthroughForMode（适配器）。
//
// 与 upstream 合并策略：
//   - 这些函数 upstream 完全没有，搬到 companion 文件后，gateway_service.go
//     与 upstream 之间的"纯新增"diff 行数会进一步缩小约 150 行。
//   - upstream 现有的 forwardCountTokensAnthropicAPIKeyPassthrough 与
//     buildCountTokensRequestAnthropicAPIKeyPassthrough 在我们这边被改造成
//     thin wrapper（委托到 forwardCountTokensAnthropicPassthrough），这部分
//     仍留在 gateway_service.go，无法 move（它们的方法名上游有，是 inline 修改）。
// =============================================================================
package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"

	"github.com/gin-gonic/gin"
)

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

// buildAnthropicSafePassthroughRequest 构造一次"安全透传"的 HTTP 请求：
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
) (*http.Request, error) {
	targetURL, err := s.buildAnthropicPassthroughTargetURL(account, path)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
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

	return req, nil
}

// forwardCountTokensAnthropicPassthrough 是 count_tokens 透传的通用框架。
// 调用方（API Key / Full Passthrough 模式）只需提供对应的 buildRequest 回调，
// 即可复用同一套上游错误处理、ops 上报、404 fallback 与响应头转写流程。
func (s *GatewayService) forwardCountTokensAnthropicPassthrough(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	errorLabel string,
	allowOAuth bool,
	buildRequest anthropicPassthroughRequestBuilder,
) error {
	token, tokenType, err := s.GetAccessToken(ctx, account)
	if err != nil {
		s.countTokensError(c, http.StatusBadGateway, "upstream_error", "Failed to get access token")
		return err
	}
	if tokenType != "apikey" && (!allowOAuth || tokenType != "oauth") {
		s.countTokensError(c, http.StatusBadGateway, "upstream_error", "Invalid account token type")
		expected := "apikey"
		if allowOAuth {
			expected = "apikey or oauth"
		}
		return fmt.Errorf("%s requires %s token, got: %s", errorLabel, expected, tokenType)
	}

	upstreamReq, err := buildRequest(ctx, c, account, body, token, tokenType)
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

// buildCountTokensRequestAnthropicAPIKeyPassthroughForMode 把现有的
// buildCountTokensRequestAnthropicAPIKeyPassthrough（不需要 tokenType 参数）适配成
// anthropicPassthroughRequestBuilder 签名（带 tokenType）。
func (s *GatewayService) buildCountTokensRequestAnthropicAPIKeyPassthroughForMode(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	token string,
	_ string,
) (*http.Request, error) {
	return s.buildCountTokensRequestAnthropicAPIKeyPassthrough(ctx, c, account, body, token)
}

// forwardCountTokensAnthropicFullPassthrough 走"完整透传"模式的 count_tokens 入口：
// 同时支持 OAuth 与 API Key 鉴权，使用 buildAnthropicSafePassthroughRequest 构造请求。
func (s *GatewayService) forwardCountTokensAnthropicFullPassthrough(ctx context.Context, c *gin.Context, account *Account, body []byte) error {
	return s.forwardCountTokensAnthropicPassthrough(
		ctx,
		c,
		account,
		body,
		"anthropic full passthrough",
		true,
		s.buildCountTokensRequestAnthropicFullPassthrough,
	)
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
) (*http.Request, error) {
	return s.buildAnthropicSafePassthroughRequest(ctx, c, account, body, token, tokenType, "/v1/messages/count_tokens")
}
