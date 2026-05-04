package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

const arcadeBase = "https://arcade-v2-us-1.bsvblockchain.tech"

type arcadeClient struct {
	base          string
	http          *http.Client
	concurrency   chan struct{}
	retryMax      int
	logger        *slog.Logger
	store         *Store
	callbackToken string
}

func newArcadeClientWithConfig(cfg *Config, store *Store, logger *slog.Logger) *arcadeClient {
	if logger == nil {
		logger = slog.Default()
	}
	base := arcadeBase
	timeout := 30 * time.Second
	concurrency := 64
	retryMax := 3
	callbackToken := ""
	if cfg != nil {
		base = cfg.ArcadeBaseURL
		timeout = cfg.HTTPTimeout
		concurrency = cfg.BroadcastConcurrency
		retryMax = cfg.BroadcastRetryMax
		callbackToken = cfg.ArcadeCallbackToken
	}
	if concurrency <= 0 {
		concurrency = 64
	}
	if retryMax <= 0 {
		retryMax = 1
	}
	return &arcadeClient{
		base:          strings.TrimRight(base, "/"),
		http:          NewHTTPClient(ArcadeTransport(), timeout),
		concurrency:   make(chan struct{}, concurrency),
		retryMax:      retryMax,
		logger:        logger,
		store:         store,
		callbackToken: callbackToken,
	}
}

func (c *arcadeClient) Broadcast(ctx context.Context, efBytes []byte) error {
	return c.broadcastWithKind(ctx, "tx", efBytes)
}

func (c *arcadeClient) BroadcastKind(ctx context.Context, kind string, efBytes []byte) error {
	if kind == "" {
		kind = "tx"
	}
	return c.broadcastWithKindTxID(ctx, kind, "", efBytes)
}

func (c *arcadeClient) BroadcastKnownTx(ctx context.Context, kind, txid string, efBytes []byte) error {
	if kind == "" {
		kind = "tx"
	}
	return c.broadcastWithKindTxID(ctx, kind, txid, efBytes)
}

func (c *arcadeClient) BroadcastTx(ctx context.Context, txid string, efBytes []byte) error {
	return c.BroadcastKindTx(ctx, "tx", txid, efBytes)
}

func (c *arcadeClient) BroadcastKindTx(ctx context.Context, kind, txid string, efBytes []byte) error {
	if txid != "" && c.store != nil {
		if err := c.store.SavePending(txid, efBytes); err != nil {
			return fmt.Errorf("save pending broadcast: %w", err)
		}
	}
	if err := c.BroadcastKnownTx(ctx, kind, txid, efBytes); err != nil {
		return err
	}
	if txid != "" && c.store != nil {
		if err := c.store.ClearPending(txid); err != nil {
			return fmt.Errorf("clear pending broadcast: %w", err)
		}
	}
	return nil
}

func (c *arcadeClient) broadcastWithKind(ctx context.Context, kind string, efBytes []byte) error {
	return c.broadcastWithKindTxID(ctx, kind, "", efBytes)
}

func (c *arcadeClient) broadcastWithKindTxID(ctx context.Context, kind, txid string, efBytes []byte) error {
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()

	var lastErr error
	for attempt := 1; attempt <= c.retryMax; attempt++ {
		start := time.Now()
		statusCode, body, err := c.postEF(ctx, efBytes)
		latency := time.Since(start)
		ObserveBroadcastLatency(kind, latency)

		if err == nil && statusCode == http.StatusAccepted {
			IncBroadcast(kind, "ok")
			c.logBroadcastResponse(ctx, slog.LevelDebug, "arcade broadcast accepted", kind, txid, attempt, statusCode, body, latency, nil)
			return nil
		}

		retryable := false
		if err != nil {
			lastErr = err
			retryable = ctx.Err() == nil
		} else {
			lastErr = fmt.Errorf("arcade %d: %s", statusCode, body)
			retryable = statusCode == http.StatusTooManyRequests || statusCode >= 500
		}

		if !retryable || attempt == c.retryMax {
			IncBroadcast(kind, "error")
			c.logBroadcastResponse(ctx, slog.LevelWarn, "arcade broadcast rejected", kind, txid, attempt, statusCode, body, latency, lastErr)
			return lastErr
		}

		IncBroadcast(kind, "retry")
		backoff := retryBackoff(attempt)
		c.logBroadcastResponse(ctx, slog.LevelWarn, "arcade broadcast retry", kind, txid, attempt, statusCode, body, latency, lastErr, "backoff", backoff)
		if err := sleepContext(ctx, backoff); err != nil {
			return err
		}
	}
	return lastErr
}

func (c *arcadeClient) logBroadcastResponse(ctx context.Context, level slog.Level, msg, kind, txid string, attempt, statusCode int, body string, latency time.Duration, err error, extra ...any) {
	if c.logger == nil || !c.logger.Enabled(ctx, level) {
		return
	}
	attrs := []any{
		"kind", kind,
		"attempt", attempt,
		"http_status", statusCode,
		"latency", latency,
	}
	if txid != "" {
		attrs = append(attrs, "txid", txid)
	}
	if txStatus := arcadeTxStatus(body); txStatus != "" {
		attrs = append(attrs, "tx_status", txStatus)
	}
	if body != "" {
		attrs = append(attrs, "response", truncateString(body, 4096))
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	attrs = append(attrs, extra...)
	c.logger.Log(ctx, level, msg, attrs...)
}

func arcadeTxStatus(body string) string {
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

func truncateString(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return s[:limit] + "...[truncated]"
}

func (c *arcadeClient) postEF(ctx context.Context, efBytes []byte) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/tx", bytes.NewReader(efBytes))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if c.callbackToken != "" {
		req.Header.Set("X-CallbackToken", c.callbackToken)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, string(body), nil
}

func (c *arcadeClient) TxStatus(ctx context.Context, txid string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/tx/"+txid, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, string(body), nil
}

func (c *arcadeClient) Reachable(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("arcade status %d", resp.StatusCode)
	}
	return nil
}

func (c *arcadeClient) acquire(ctx context.Context) error {
	select {
	case c.concurrency <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *arcadeClient) release() {
	<-c.concurrency
}

func retryBackoff(attempt int) time.Duration {
	base := 100 * time.Millisecond
	backoff := base << (attempt - 1)
	if backoff > 5*time.Second {
		backoff = 5 * time.Second
	}
	jitter := time.Duration(rand.Int63n(int64(backoff / 2)))
	return backoff + jitter
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
