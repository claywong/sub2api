package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func newTestDispatcher(serverURL string, extra ...func(*config.OpsWebhookConfig)) *OpsErrorWebhookDispatcher {
	cfg := config.OpsWebhookConfig{
		URL:            serverURL,
		TimeoutSeconds: 2,
		MaxRetries:     0,
		BufferSize:     64,
	}
	for _, fn := range extra {
		fn(&cfg)
	}
	d := NewOpsErrorWebhookDispatcher(cfg)
	d.Start()
	return d
}

func makeEntry(phase, errType, platform string) *OpsInsertErrorLogInput {
	return &OpsInsertErrorLogInput{
		ErrorPhase:   phase,
		ErrorType:    errType,
		Severity:     "P1",
		StatusCode:   502,
		Platform:     platform,
		Model:        "gpt-4o",
		ErrorMessage: "bad gateway",
		CreatedAt:    time.Now(),
	}
}

func waitForCount(counter *atomic.Int64, n int64, deadline time.Duration) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if counter.Load() >= n {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestNewOpsErrorWebhookDispatcher_DisabledWhenURLEmpty verifies nil returned for empty URL.
func TestNewOpsErrorWebhookDispatcher_DisabledWhenURLEmpty(t *testing.T) {
	d := NewOpsErrorWebhookDispatcher(config.OpsWebhookConfig{URL: ""})
	if d != nil {
		t.Fatal("expected nil dispatcher when URL is empty")
	}
}

// TestDispatcher_EnqueueDoesNotBlock verifies Enqueue returns immediately even when buffer is full.
func TestDispatcher_EnqueueDoesNotBlock(t *testing.T) {
	// Slow server: each request sleeps 200ms, so the dispatcher's single goroutine
	// will be busy and the tiny buffer fills up quickly.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDispatcher(srv.URL, func(c *config.OpsWebhookConfig) {
		c.BufferSize = 1 // tiny buffer to provoke drops fast
	})
	defer d.Stop()

	// All 50 Enqueue calls must complete well within 1s even though server is slow.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			d.Enqueue(makeEntry("upstream", "upstream_error", "openai"))
		}
	}()

	select {
	case <-done:
		// pass
	case <-time.After(time.Second):
		t.Fatal("Enqueue blocked")
	}
}

// TestDispatcher_SendsOnePostPerError verifies each Enqueue results in one HTTP POST.
func TestDispatcher_SendsOnePostPerError(t *testing.T) {
	var received atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDispatcher(srv.URL)
	defer d.Stop()

	const n = 3
	for i := 0; i < n; i++ {
		d.Enqueue(makeEntry("upstream", "upstream_error", "openai"))
	}

	if !waitForCount(&received, n, 3*time.Second) {
		t.Fatalf("expected %d requests, got %d", n, received.Load())
	}
}

// TestDispatcher_PayloadShape verifies the JSON body structure.
func TestDispatcher_PayloadShape(t *testing.T) {
	ready := make(chan OpsWebhookPayload, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p OpsWebhookPayload
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &p)
		select {
		case ready <- p:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDispatcher(srv.URL)
	defer d.Stop()

	entry := makeEntry("upstream", "upstream_error", "openai")
	entry.RequestID = "req-abc"
	d.Enqueue(entry)

	select {
	case p := <-ready:
		if p.Event != "error" {
			t.Errorf("event=%q, want 'error'", p.Event)
		}
		if p.Error == nil {
			t.Fatal("payload.error is nil")
		}
		if p.Error.Phase != "upstream" {
			t.Errorf("phase=%q, want 'upstream'", p.Error.Phase)
		}
		if p.Error.RequestID != "req-abc" {
			t.Errorf("request_id=%q, want 'req-abc'", p.Error.RequestID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for webhook payload")
	}
}

// TestDispatcher_HMACSignatureHeader verifies HMAC header is sent and verifiable.
func TestDispatcher_HMACSignatureHeader(t *testing.T) {
	type headers struct {
		sig  string
		ts   string
		body []byte
	}
	got := make(chan headers, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		select {
		case got <- headers{
			sig:  r.Header.Get(webhookSignatureHeader),
			ts:   r.Header.Get(webhookTimestampHeader),
			body: b,
		}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	const secret = "test-secret"
	d := newTestDispatcher(srv.URL, func(c *config.OpsWebhookConfig) { c.Secret = secret })
	defer d.Stop()

	d.Enqueue(makeEntry("upstream", "upstream_error", "openai"))

	select {
	case h := <-got:
		if !strings.HasPrefix(h.sig, "sha256=") {
			t.Fatalf("expected sha256= prefix, got %q", h.sig)
		}
		if h.ts == "" {
			t.Fatal("timestamp header missing")
		}
		want := computeWebhookSignature(secret, h.ts, h.body)
		if h.sig != want {
			t.Errorf("signature mismatch: got %q, want %q", h.sig, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for webhook call")
	}
}

// TestDispatcher_RetriesOn5xx verifies retries happen on server errors.
func TestDispatcher_RetriesOn5xx(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDispatcher(srv.URL, func(c *config.OpsWebhookConfig) {
		c.MaxRetries = 3
	})
	defer d.Stop()

	d.Enqueue(makeEntry("upstream", "upstream_error", "openai"))

	if !waitForCount(&calls, 3, 5*time.Second) {
		t.Fatalf("expected at least 3 calls (2 failures + 1 success), got %d", calls.Load())
	}
}

// TestDispatcher_StopDrainsPendingEvents verifies Stop waits for buffered events.
func TestDispatcher_StopDrainsPendingEvents(t *testing.T) {
	var received atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDispatcher(srv.URL)

	const n = 5
	for i := 0; i < n; i++ {
		d.Enqueue(makeEntry("upstream", "upstream_error", "openai"))
	}
	d.Stop() // must drain before returning

	if received.Load() < n {
		t.Errorf("stop did not drain: received %d/%d", received.Load(), n)
	}
}

// TestToWebhookError_NilInput verifies nil-safe behaviour.
func TestToWebhookError_NilInput(t *testing.T) {
	if got := toWebhookError(nil); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

// TestToWebhookError_Fields verifies all mapped fields.
func TestToWebhookError_Fields(t *testing.T) {
	uid := int64(10)
	gid := int64(20)
	upCode := 429
	upMsg := "rate limited"
	latency := int64(300)

	e := &OpsInsertErrorLogInput{
		ErrorPhase:           "upstream",
		ErrorType:            "rate_limit_error",
		Severity:             "P1",
		StatusCode:           429,
		Platform:             "anthropic",
		Model:                "claude-3-5-sonnet",
		ErrorMessage:         "too many requests",
		UserID:               &uid,
		GroupID:              &gid,
		RequestID:            "rid-1",
		ClientRequestID:      "crid-1",
		UpstreamStatusCode:   &upCode,
		UpstreamErrorMessage: &upMsg,
		UpstreamLatencyMs:    &latency,
		CreatedAt:            time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC),
	}

	w := toWebhookError(e)
	if w.Phase != "upstream" {
		t.Errorf("phase: got %q", w.Phase)
	}
	if w.Platform != "anthropic" {
		t.Errorf("platform: got %q", w.Platform)
	}
	if *w.UserID != uid {
		t.Errorf("user_id: got %d", *w.UserID)
	}
	if *w.UpstreamStatusCode != upCode {
		t.Errorf("upstream_status_code: got %d", *w.UpstreamStatusCode)
	}
	if w.CreatedAt != "2026-05-09T00:00:00Z" {
		t.Errorf("created_at: got %q", w.CreatedAt)
	}
}

// TestComputeWebhookSignature_Deterministic verifies same inputs produce same output.
func TestComputeWebhookSignature_Deterministic(t *testing.T) {
	body := []byte(`{"event":"error"}`)
	s1 := computeWebhookSignature("secret", "1000", body)
	s2 := computeWebhookSignature("secret", "1000", body)
	if s1 != s2 {
		t.Error("signature not deterministic")
	}
	if !strings.HasPrefix(s1, "sha256=") {
		t.Errorf("missing sha256= prefix: %q", s1)
	}
}

// TestComputeWebhookSignature_DifferentSecrets verifies different secrets produce different results.
func TestComputeWebhookSignature_DifferentSecrets(t *testing.T) {
	body := []byte(`{"event":"error"}`)
	s1 := computeWebhookSignature("secret-a", "1000", body)
	s2 := computeWebhookSignature("secret-b", "1000", body)
	if s1 == s2 {
		t.Error("different secrets must produce different signatures")
	}
}
