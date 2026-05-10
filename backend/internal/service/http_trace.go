package service

import (
	"context"
	"net/http/httptrace"
	"time"
)

// HTTPTraceMetrics 收集单次 HTTP 请求的连接阶段性能指标。
//
// ConnectDone-ConnectStart 对应的含义依 transport 类型而异：
//   - 标准 transport：仅 TCP 三次握手时间
//   - utls（TLS 指纹）transport：TCP 三次握手 + TLS 握手合计时间
//     （因为 DialTLSContext 把两个阶段合并处理）
//
// 连接复用时 ConnectStart/Done 不触发，tcpConnMs 保持 0，
// 调用方可以据此判断是否建立了新连接。
type HTTPTraceMetrics struct {
	startTime    time.Time
	connectStart time.Time
	tcpConnMs    int // TCP 连接时间（ms）；连接复用时为 0
	ttfbMs       int // TTFB 首字节时间（ms），从 startTime 计算
}

// newHTTPTraceMetrics 创建一个新的指标收集器，startTime 用于计算 TTFB。
func newHTTPTraceMetrics(startTime time.Time) *HTTPTraceMetrics {
	return &HTTPTraceMetrics{startTime: startTime}
}

// clientTrace 返回绑定到本收集器的 httptrace.ClientTrace。
// 回调在单个请求的 transport 内顺序调用，无需加锁。
func (m *HTTPTraceMetrics) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		ConnectStart: func(network, addr string) {
			m.connectStart = time.Now()
		},
		ConnectDone: func(network, addr string, err error) {
			if err == nil && !m.connectStart.IsZero() {
				m.tcpConnMs = int(time.Since(m.connectStart).Milliseconds())
			}
		},
		GotFirstResponseByte: func() {
			if !m.startTime.IsZero() {
				m.ttfbMs = int(time.Since(m.startTime).Milliseconds())
			}
		},
	}
}

// TCPConnMs 返回连接建立时间（ms）；连接复用时为 0。
func (m *HTTPTraceMetrics) TCPConnMs() int {
	if m == nil {
		return 0
	}
	return m.tcpConnMs
}

// TTFBMs 返回首字节时间（ms）；未触发时为 0。
func (m *HTTPTraceMetrics) TTFBMs() int {
	if m == nil {
		return 0
	}
	return m.ttfbMs
}

// withHTTPTrace 把 httptrace 注入 req 的 context，返回新的 request 和指标收集器。
// startTime 应与 ForwardResult.Duration 所用的 startTime 一致，以保证 TTFB 口径统一。
func withHTTPTrace(ctx context.Context, startTime time.Time) (context.Context, *HTTPTraceMetrics) {
	m := newHTTPTraceMetrics(startTime)
	return httptrace.WithClientTrace(ctx, m.clientTrace()), m
}
