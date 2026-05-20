package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const (
	webhookDefaultTimeout    = 5 * time.Second
	webhookDefaultBufferSize = 2048
	webhookDefaultMaxRetries = 2
	webhookRetryBaseDelay    = 300 * time.Millisecond
	webhookDropLogInterval   = 10 * time.Second
	webhookSignatureHeader   = "X-Sub2Api-Signature"
	webhookTimestampHeader   = "X-Sub2Api-Timestamp"
)

// OpsWebhookPayload is the body sent to the configured endpoint for each error.
type OpsWebhookPayload struct {
	Event     string         `json:"event"`
	Timestamp string         `json:"timestamp"`
	Error     *OpsWebhookError `json:"error"`
}

// OpsWebhookError is the error detail in the payload.
// A safe subset of OpsInsertErrorLogInput — no request body or PII headers.
type OpsWebhookError struct {
	Phase      string `json:"phase"`
	Type       string `json:"type"`
	Severity   string `json:"severity"`
	StatusCode int    `json:"status_code"`

	Platform string `json:"platform,omitempty"`
	Model    string `json:"model,omitempty"`
	Message  string `json:"message,omitempty"`

	UserID    *int64 `json:"user_id,omitempty"`
	GroupID   *int64 `json:"group_id,omitempty"`
	AccountID *int64 `json:"account_id,omitempty"`

	RequestID       string `json:"request_id,omitempty"`
	ClientRequestID string `json:"client_request_id,omitempty"`
	RequestPath     string `json:"request_path,omitempty"`

	UpstreamStatusCode   *int    `json:"upstream_status_code,omitempty"`
	UpstreamErrorMessage *string `json:"upstream_error_message,omitempty"`

	UpstreamLatencyMs *int64 `json:"upstream_latency_ms,omitempty"`
	ResponseLatencyMs *int64 `json:"response_latency_ms,omitempty"`

	CreatedAt string `json:"created_at"`
}

// OpsErrorWebhookDispatcher forwards each error event to an external HTTP endpoint.
// One HTTP POST per error event. Non-blocking enqueue: events are silently dropped
// when the buffer is full to never impact the request path.
type OpsErrorWebhookDispatcher struct {
	cfg        config.OpsWebhookConfig
	httpClient *http.Client

	ch        chan *OpsInsertErrorLogInput
	stop      chan struct{}
	wg        sync.WaitGroup
	startOnce sync.Once
	stopOnce  sync.Once

	lastDropLogMu sync.Mutex
	lastDropLog   time.Time
}

// NewOpsErrorWebhookDispatcher creates a dispatcher from cfg.
// Returns nil when cfg.URL is empty (feature disabled).
func NewOpsErrorWebhookDispatcher(cfg config.OpsWebhookConfig) *OpsErrorWebhookDispatcher {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil
	}
	bufSize := cfg.BufferSize
	if bufSize <= 0 {
		bufSize = webhookDefaultBufferSize
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = webhookDefaultTimeout
	}
	return &OpsErrorWebhookDispatcher{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: timeout},
		ch:         make(chan *OpsInsertErrorLogInput, bufSize),
		stop:       make(chan struct{}),
	}
}

// Start launches the background dispatch goroutine.
func (d *OpsErrorWebhookDispatcher) Start() {
	if d == nil {
		return
	}
	d.startOnce.Do(func() {
		d.wg.Add(1)
		go d.run()
	})
}

// Stop drains remaining events and shuts down the goroutine.
func (d *OpsErrorWebhookDispatcher) Stop() {
	if d == nil {
		return
	}
	d.stopOnce.Do(func() { close(d.stop) })
	d.wg.Wait()
}

// Enqueue adds an error to the dispatch buffer without blocking.
func (d *OpsErrorWebhookDispatcher) Enqueue(entry *OpsInsertErrorLogInput) {
	if d == nil || entry == nil {
		return
	}
	select {
	case d.ch <- entry:
	default:
		d.logDrop()
	}
}

func (d *OpsErrorWebhookDispatcher) logDrop() {
	d.lastDropLogMu.Lock()
	defer d.lastDropLogMu.Unlock()
	now := time.Now()
	if now.Sub(d.lastDropLog) < webhookDropLogInterval {
		return
	}
	d.lastDropLog = now
	logger.LegacyPrintf("service.ops_webhook", "[OpsWebhook] buffer full, dropping event (url=%s)", d.cfg.URL)
}

func (d *OpsErrorWebhookDispatcher) run() {
	defer d.wg.Done()
	for {
		select {
		case entry := <-d.ch:
			d.send(entry)
		case <-d.stop:
			// Drain remaining buffered events before exit.
			for {
				select {
				case entry := <-d.ch:
					d.send(entry)
				default:
					return
				}
			}
		}
	}
}

func (d *OpsErrorWebhookDispatcher) send(entry *OpsInsertErrorLogInput) {
	payload := &OpsWebhookPayload{
		Event:     "error",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Error:     toWebhookError(entry),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		logger.LegacyPrintf("service.ops_webhook", "[OpsWebhook] marshal failed: %v", err)
		return
	}

	maxRetries := d.cfg.MaxRetries
	if maxRetries < 0 {
		maxRetries = webhookDefaultMaxRetries
	}
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(webhookRetryBaseDelay * time.Duration(1<<uint(attempt-1)))
		}
		if d.doPost(body) {
			return
		}
	}
	logger.LegacyPrintf("service.ops_webhook", "[OpsWebhook] all retries exhausted (url=%s)", d.cfg.URL)
}

func (d *OpsErrorWebhookDispatcher) doPost(body []byte) bool {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, d.cfg.URL, bytes.NewReader(body))
	if err != nil {
		logger.LegacyPrintf("service.ops_webhook", "[OpsWebhook] build request failed: %v", err)
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if secret := strings.TrimSpace(d.cfg.Secret); secret != "" {
		ts := fmt.Sprintf("%d", time.Now().Unix())
		req.Header.Set(webhookTimestampHeader, ts)
		req.Header.Set(webhookSignatureHeader, computeWebhookSignature(secret, ts, body))
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		logger.LegacyPrintf("service.ops_webhook", "[OpsWebhook] request failed: %v", err)
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 500 {
		logger.LegacyPrintf("service.ops_webhook", "[OpsWebhook] server error: status=%d", resp.StatusCode)
		return false
	}
	return true
}

// computeWebhookSignature returns sha256=<hex(hmac-sha256(secret, timestamp+"."+body))>.
func computeWebhookSignature(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func toWebhookError(e *OpsInsertErrorLogInput) *OpsWebhookError {
	if e == nil {
		return nil
	}
	ts := e.CreatedAt
	if ts.IsZero() {
		ts = time.Now()
	}
	return &OpsWebhookError{
		Phase:      e.ErrorPhase,
		Type:       e.ErrorType,
		Severity:   e.Severity,
		StatusCode: e.StatusCode,

		Platform:    e.Platform,
		Model:       e.Model,
		Message:     e.ErrorMessage,
		RequestPath: e.RequestPath,

		UserID:    e.UserID,
		GroupID:   e.GroupID,
		AccountID: e.AccountID,

		RequestID:       e.RequestID,
		ClientRequestID: e.ClientRequestID,

		UpstreamStatusCode:   e.UpstreamStatusCode,
		UpstreamErrorMessage: e.UpstreamErrorMessage,

		UpstreamLatencyMs: e.UpstreamLatencyMs,
		ResponseLatencyMs: e.ResponseLatencyMs,

		CreatedAt: ts.UTC().Format(time.RFC3339),
	}
}
