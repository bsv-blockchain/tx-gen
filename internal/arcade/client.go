// Package arcade is a client for the Arcade v2 transaction processor: it
// broadcasts EF transactions and looks up their status.
package arcade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bsv-blockchain/tx-gen/internal/backoff"
	"github.com/bsv-blockchain/tx-gen/internal/httpclient"
	"github.com/bsv-blockchain/tx-gen/internal/metrics"
)

const (
	// DefaultBaseURL is the public Arcade v2 node.
	DefaultBaseURL = "https://arcade-v2-us-1.bsvblockchain.tech"

	// defaultTimeout bounds each request when Options.Timeout is 0.
	defaultTimeout = 30 * time.Second

	// defaultConcurrency caps in-flight broadcasts when Options.Concurrency is 0.
	defaultConcurrency = 64

	// maxResponseBytes caps how much of a response body is read.
	maxResponseBytes = 4096

	// maxLoggedBytes caps how much of a response body is logged.
	maxLoggedBytes = 4096

	// reachableTimeout bounds the health check behind /readyz.
	reachableTimeout = 2 * time.Second

	// broadcastKind is the metrics label for transaction broadcasts.
	broadcastKind = "tx"
)

var (
	// ErrRejected is returned when Arcade answers a broadcast with a
	// non-retryable status, or when every retry is exhausted.
	ErrRejected = errors.New("arcade rejected the broadcast")

	// ErrUnhealthy is returned by Reachable when Arcade answers with a 5xx.
	ErrUnhealthy = errors.New("arcade is unhealthy")
)

// Options configures a Client. Every field is optional.
type Options struct {
	// BaseURL is the Arcade node; it defaults to DefaultBaseURL.
	BaseURL string

	// CallbackToken is sent as X-CallbackToken with every broadcast, which
	// subscribes the transaction to Arcade's /events stream.
	CallbackToken string

	// Timeout bounds each request; it defaults to 30s.
	Timeout time.Duration

	// Concurrency caps in-flight broadcasts; it defaults to 64.
	Concurrency int

	// RetryMax is the number of attempts per broadcast; it defaults to 1.
	RetryMax int

	// HTTPClient overrides the pooled client built from Timeout.
	HTTPClient *http.Client

	// Logger receives broadcast logs; it defaults to slog.Default.
	Logger *slog.Logger

	// Metrics receives broadcast counts and latencies. It may be nil.
	Metrics *metrics.Metrics
}

// Client talks to one Arcade node. It is safe for concurrent use.
type Client struct {
	baseURL       string
	callbackToken string
	http          *http.Client
	slots         chan struct{}
	retryMax      int
	logger        *slog.Logger
	metrics       *metrics.Metrics
}

// New returns a client for the node in opts.
func New(opts Options) *Client {
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = httpclient.New(httpclient.NewPooledTransport(), timeout)
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		baseURL:       strings.TrimRight(baseURL, "/"),
		callbackToken: opts.CallbackToken,
		http:          httpClient,
		slots:         make(chan struct{}, concurrency),
		retryMax:      max(opts.RetryMax, 1),
		logger:        logger,
		metrics:       opts.Metrics,
	}
}

// Broadcast posts ef to /tx and returns nil once Arcade answers 202 Accepted.
//
// 429 and 5xx answers and transport errors are retried with backoff, up to
// RetryMax attempts; any other answer fails at once with ErrRejected. txid is
// used only for logging and may be empty.
func (c *Client) Broadcast(ctx context.Context, txid string, ef []byte) error {
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()

	var lastErr error
	for attempt := 1; attempt <= c.retryMax; attempt++ {
		start := time.Now()
		status, body, err := c.post(ctx, ef)
		latency := time.Since(start)
		c.metrics.ObserveBroadcastLatency(broadcastKind, latency)

		attempted := attemptLog{txid: txid, attempt: attempt, status: status, body: body, latency: latency}
		if err == nil && status == http.StatusAccepted {
			c.metrics.IncBroadcast(broadcastKind, "ok")
			c.logAttempt(ctx, slog.LevelDebug, "arcade broadcast accepted", attempted, nil)
			return nil
		}

		var retryable bool
		if err != nil {
			lastErr = err
			retryable = ctx.Err() == nil
		} else {
			lastErr = fmt.Errorf("%w: HTTP %d: %s", ErrRejected, status, body)
			retryable = status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
		}
		if !retryable || attempt == c.retryMax {
			c.metrics.IncBroadcast(broadcastKind, "error")
			c.logAttempt(ctx, slog.LevelWarn, "arcade broadcast rejected", attempted, lastErr)
			return lastErr
		}

		delay := backoff.Retry(attempt)
		c.metrics.IncBroadcast(broadcastKind, "retry")
		c.logAttempt(ctx, slog.LevelWarn, "arcade broadcast retry", attempted, lastErr, "backoff", delay)
		if err = backoff.Sleep(ctx, delay); err != nil {
			return err
		}
	}
	return lastErr
}

// TxStatus fetches /tx/{txid} and returns Arcade's status code and body.
func (c *Client) TxStatus(ctx context.Context, txid string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/tx/"+txid, nil)
	if err != nil {
		return 0, "", fmt.Errorf("build status request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	return c.do(req)
}

// Reachable reports whether Arcade's /health endpoint answers without a 5xx
// within two seconds.
func (c *Client) Reachable(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, reachableTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("build health request: %w", err)
	}
	status, _, err := c.do(req)
	if err != nil {
		return err
	}
	if status >= http.StatusInternalServerError {
		return fmt.Errorf("%w: HTTP %d", ErrUnhealthy, status)
	}
	return nil
}

// post sends one broadcast request.
func (c *Client) post(ctx context.Context, ef []byte) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/tx", bytes.NewReader(ef))
	if err != nil {
		return 0, "", fmt.Errorf("build broadcast request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if c.callbackToken != "" {
		req.Header.Set("X-CallbackToken", c.callbackToken)
	}
	return c.do(req)
}

// do sends req and returns the status code and up to 4 KiB of the body.
//
// The status code alone decides the outcome: a body that cannot be read in
// full is returned as far as it was read, so a 202 whose body is cut short
// still counts as accepted rather than being broadcast again.
func (c *Client) do(req *http.Request) (int, string, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("arcade %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	return resp.StatusCode, string(body), nil
}

// acquire takes a broadcast slot, waiting while all are in use.
func (c *Client) acquire(ctx context.Context) error {
	select {
	case c.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release returns a broadcast slot.
func (c *Client) release() {
	<-c.slots
}

// attemptLog describes one broadcast attempt for logging.
type attemptLog struct {
	txid    string
	attempt int
	status  int
	body    string
	latency time.Duration
}

// logAttempt logs one broadcast attempt at level, if enabled.
func (c *Client) logAttempt(ctx context.Context, level slog.Level, msg string, a attemptLog, err error, extra ...any) {
	if !c.logger.Enabled(ctx, level) {
		return
	}
	attrs := []any{"kind", broadcastKind, "attempt", a.attempt, "http_status", a.status, "latency", a.latency}
	if a.txid != "" {
		attrs = append(attrs, "txid", a.txid)
	}
	if status := TxStatusFromBody(a.body); status != "" {
		attrs = append(attrs, "tx_status", status)
	}
	if a.body != "" {
		attrs = append(attrs, "response", Truncate(a.body, maxLoggedBytes))
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	attrs = append(attrs, extra...)
	c.logger.Log(ctx, level, msg, attrs...)
}

// TxStatusFromBody returns the txStatus, tx_status, or status field of a JSON
// response body, or "" when there is none.
func TxStatusFromBody(body string) string {
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return ""
	}
	for _, key := range []string{"txStatus", "tx_status", "status"} {
		if status, ok := payload[key].(string); ok {
			return status
		}
	}
	return ""
}

// Truncate returns s cut to limit bytes, marked "...[truncated]" when cut.
func Truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return s[:limit] + "...[truncated]"
}
