// Package woc is a WhatsOnChain client that looks up the unspent outputs of a
// locking script.
package woc

import (
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
	"github.com/bsv-blockchain/tx-gen/internal/txbuilder"
)

const (
	// DefaultBaseURL is the WhatsOnChain mainnet API.
	DefaultBaseURL = "https://api.whatsonchain.com/v1/bsv/main"

	// defaultTimeout bounds each request when Options.Timeout is 0.
	defaultTimeout = 30 * time.Second

	// maxResponseBytes caps the size of a UTXO listing.
	maxResponseBytes = 32 << 20

	// maxErrorBytes caps how much of an error body is returned.
	maxErrorBytes = 4096
)

// ErrRequestFailed is returned when WhatsOnChain answers with an error.
var ErrRequestFailed = errors.New("whatsonchain request failed")

// Options configures a Client. Every field is optional.
type Options struct {
	// BaseURL is the API root; it defaults to DefaultBaseURL.
	BaseURL string

	// Timeout bounds each request; it defaults to 30s.
	Timeout time.Duration

	// RetryMax is the number of attempts per lookup; it defaults to 1.
	RetryMax int

	// HTTPClient overrides the pooled client built from Timeout.
	HTTPClient *http.Client

	// Logger receives retry logs; it defaults to slog.Default.
	Logger *slog.Logger
}

// Client queries one WhatsOnChain API. It is safe for concurrent use.
type Client struct {
	baseURL  string
	http     *http.Client
	retryMax int
	logger   *slog.Logger
}

// New returns a client for the API in opts.
func New(opts Options) *Client {
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
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
		baseURL:  strings.TrimRight(baseURL, "/"),
		http:     httpClient,
		retryMax: max(opts.RetryMax, 1),
		logger:   logger,
	}
}

// FetchUTXOs returns every unspent output locked by the script whose
// WhatsOnChain script hash is scriptHash. 429 and 5xx answers and transport
// errors are retried with backoff, up to RetryMax attempts.
func (c *Client) FetchUTXOs(ctx context.Context, scriptHash string) ([]txbuilder.UTXO, error) {
	url := fmt.Sprintf("%s/script/%s/unspent/all", c.baseURL, scriptHash)
	var lastErr error
	for attempt := 1; attempt <= c.retryMax; attempt++ {
		utxos, retryable, err := c.fetchOnce(ctx, url)
		if err == nil {
			return utxos, nil
		}
		lastErr = err
		if !retryable || attempt == c.retryMax {
			break
		}
		delay := backoff.Retry(attempt)
		c.logger.Warn("WhatsOnChain lookup retry", "attempt", attempt, "backoff", delay, "error", err)
		if err = backoff.Sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// wocUTXO is one entry of a WhatsOnChain unspent listing.
type wocUTXO struct {
	Height int64  `json:"height"`
	TxHash string `json:"tx_hash"`
	TxPos  uint32 `json:"tx_pos"`
	Value  uint64 `json:"value"`
}

// unspentResponse is the WhatsOnChain unspent/all response envelope.
type unspentResponse struct {
	Result []wocUTXO `json:"result"`
	Error  string    `json:"error"`
}

// fetchOnce performs one lookup and reports whether a failure is retryable.
func (c *Client) fetchOnce(ctx context.Context, url string) ([]txbuilder.UTXO, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ctx.Err() == nil, fmt.Errorf("whatsonchain request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, true, fmt.Errorf("read whatsonchain response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError
		if len(body) > maxErrorBytes {
			body = body[:maxErrorBytes]
		}
		return nil, retryable, fmt.Errorf("%w: HTTP %d: %s", ErrRequestFailed, resp.StatusCode, body)
	}

	var envelope unspentResponse
	if err = json.Unmarshal(body, &envelope); err != nil {
		return nil, false, fmt.Errorf("decode whatsonchain response: %w", err)
	}
	if envelope.Error != "" {
		return nil, false, fmt.Errorf("%w: %s", ErrRequestFailed, envelope.Error)
	}

	utxos := make([]txbuilder.UTXO, len(envelope.Result))
	for i, r := range envelope.Result {
		utxos[i] = txbuilder.UTXO{TxHash: r.TxHash, TxPos: r.TxPos, Value: r.Value, Height: r.Height}
	}
	return utxos, false, nil
}
