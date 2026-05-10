// gateway_service_errors.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：自定义错误类型。
//
// 定义了 ClientRequestError，用于区分"客户端请求内容问题"和"账号侧问题"，
// 让健康统计与 ops error_owner='client' 的分类语义对齐。
//
// 与 upstream 合并策略：
//   - 这个类型不依赖任何 upstream 类型，搬到 companion 文件后零冲突。
//   - 实际使用点（return &ClientRequestError{...}）仍留在 gateway_service.go
//     中，因为是包内 inline 修改。
// =============================================================================
package service

import "fmt"

// ClientRequestError 表示由客户端请求内容导致的错误（如 invalid_request_error），
// 不应计入账号健康统计，与 ops error_owner = 'client' 的分类语义对齐。
type ClientRequestError struct {
	StatusCode int
	cause      error
}

func (e *ClientRequestError) Error() string {
	if e.cause != nil {
		return e.cause.Error()
	}
	return fmt.Sprintf("upstream error: %d", e.StatusCode)
}

func (e *ClientRequestError) Unwrap() error { return e.cause }
