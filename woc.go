package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/script"
)

type wocClient struct {
	base     string
	http     *http.Client
	retryMax int
	logger   *slog.Logger
}

func newWOCClient() *wocClient {
	return newWOCClientWithConfig(nil, slog.Default())
}

func newWOCClientWithConfig(cfg *Config, logger *slog.Logger) *wocClient {
	if logger == nil {
		logger = slog.Default()
	}
	base := "https://api.whatsonchain.com/v1/bsv/main"
	timeout := 30 * time.Second
	retryMax := 3
	if cfg != nil {
		base = cfg.WOCBaseURL
		timeout = cfg.HTTPTimeout
		retryMax = cfg.BroadcastRetryMax
	}
	if retryMax <= 0 {
		retryMax = 1
	}
	return &wocClient{
		base:     strings.TrimRight(base, "/"),
		http:     NewHTTPClient(ArcadeTransport(), timeout),
		retryMax: retryMax,
		logger:   logger,
	}
}

type wocUTXO struct {
	Height int64  `json:"height"`
	TxHash string `json:"tx_hash"`
	TxPos  uint32 `json:"tx_pos"`
	Value  uint64 `json:"value"`
}

func scriptToHash(s *script.Script) string {
	h := sha256.Sum256(*s)
	for i, j := 0, len(h)-1; i < j; i, j = i+1, j-1 {
		h[i], h[j] = h[j], h[i]
	}
	return hex.EncodeToString(h[:])
}

func (c *wocClient) fetchUnspent(scriptHash string) ([]UTXO, error) {
	return c.FetchUTXOs(context.Background(), scriptHash)
}

func (c *wocClient) FetchUTXOs(ctx context.Context, scriptHash string) ([]UTXO, error) {
	url := fmt.Sprintf("%s/script/%s/unspent/all", c.base, scriptHash)
	var lastErr error
	for attempt := 1; attempt <= c.retryMax; attempt++ {
		utxos, retryable, err := c.fetchUTXOsOnce(ctx, url)
		if err == nil {
			return utxos, nil
		}
		lastErr = err
		if !retryable || attempt == c.retryMax {
			break
		}
		backoff := retryBackoff(attempt)
		c.logger.Warn("WOC fetch retry", "attempt", attempt, "backoff", backoff, "error", err)
		if err := sleepContext(ctx, backoff); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *wocClient) fetchUTXOsOnce(ctx context.Context, url string) ([]UTXO, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ctx.Err() == nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, true, err
	}

	if resp.StatusCode != http.StatusOK {
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		if len(body) > 4096 {
			body = body[:4096]
		}
		return nil, retryable, fmt.Errorf("WOC %d: %s", resp.StatusCode, body)
	}

	var envelope struct {
		Result []wocUTXO `json:"result"`
		Error  string    `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, false, fmt.Errorf("decode: %w", err)
	}
	if envelope.Error != "" {
		return nil, false, fmt.Errorf("WOC: %s", envelope.Error)
	}
	raw := envelope.Result

	utxos := make([]UTXO, len(raw))
	for i, r := range raw {
		utxos[i] = UTXO{
			TxHash: r.TxHash,
			TxPos:  r.TxPos,
			Value:  r.Value,
			Height: r.Height,
		}
	}
	return utxos, false, nil
}
