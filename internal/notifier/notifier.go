// Package notifier posts notable events, such as mainnet reorgs, to a Slack
// incoming webhook.
package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/bsv-blockchain/tx-gen/internal/httpclient"
)

const (
	// dedupeWindow suppresses repeats of the same message key.
	dedupeWindow = 5 * time.Minute

	// postTimeout bounds each webhook request.
	postTimeout = 10 * time.Second

	// deepReorg is the depth above which a reorg is flagged as severe.
	deepReorg = 3

	// reorgUsername is the Slack display name for reorg posts.
	reorgUsername = "txgen-reorg"
)

// ErrWebhookFailed is returned when Slack rejects a webhook post.
var ErrWebhookFailed = errors.New("slack webhook rejected the post")

// Options configures a Notifier.
type Options struct {
	// WebhookURL is the Slack incoming webhook. It is required.
	WebhookURL string

	// Channel overrides the webhook's default channel when set.
	Channel string

	// Environment and InstanceID tag every message.
	Environment string

	// InstanceID identifies the deployment in every message.
	InstanceID string

	// HTTPClient overrides the default client (10s timeout).
	HTTPClient *http.Client

	// Logger receives delivery failures; it defaults to slog.Default.
	Logger *slog.Logger
}

// Notifier posts deduplicated messages to Slack. It is safe for concurrent use.
type Notifier struct {
	webhook  string
	channel  string
	env      string
	instance string
	http     *http.Client
	logger   *slog.Logger

	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time
}

// New returns a notifier for the webhook in opts.
func New(opts Options) *Notifier {
	client := opts.HTTPClient
	if client == nil {
		client = httpclient.New(httpclient.NewPooledTransport(), postTimeout)
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Notifier{
		webhook:  opts.WebhookURL,
		channel:  opts.Channel,
		env:      opts.Environment,
		instance: opts.InstanceID,
		http:     client,
		logger:   logger,
		last:     make(map[string]time.Time),
		now:      time.Now,
	}
}

// ReportReorg posts a reorg to Slack, at most once per height every five
// minutes. Reorgs deeper than three blocks are flagged as severe. Delivery
// failures are logged, never returned: a Slack outage must not affect the
// chains.
func (n *Notifier) ReportReorg(ctx context.Context, height, depth, chainsImpacted int, tipBefore, tipAfter string) {
	if !n.shouldSend("reorg-" + strconv.Itoa(height)) {
		return
	}
	emoji := ":warning:"
	if depth > deepReorg {
		emoji = ":rotating_light:"
	}
	msg := fmt.Sprintf("%s Reorg detected: height=%d depth=%d chains=%d tip %s->%s [%s/%s]",
		emoji, height, depth, chainsImpacted, tipBefore, tipAfter, n.env, n.instance)
	if err := n.post(ctx, msg, reorgUsername); err != nil {
		n.logger.Warn("slack reorg notification failed", "height", height, "depth", depth, "error", err)
		return
	}
	n.logger.Debug("slack reorg notification sent", "height", height, "depth", depth)
}

// shouldSend reports whether key has not been sent within the dedupe
// window, and records it as sent.
func (n *Notifier) shouldSend(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.now()
	if last, ok := n.last[key]; ok && now.Sub(last) < dedupeWindow {
		return false
	}
	n.last[key] = now
	for k, at := range n.last { // keep the map bounded
		if now.Sub(at) >= dedupeWindow {
			delete(n.last, k)
		}
	}
	return true
}

// webhookPayload is the Slack incoming-webhook request body.
type webhookPayload struct {
	Text     string `json:"text"`
	Channel  string `json:"channel,omitempty"`
	Username string `json:"username"`
}

// post sends msg to the webhook.
func (n *Notifier) post(ctx context.Context, msg, username string) error {
	body, err := json.Marshal(webhookPayload{Text: msg, Channel: n.channel, Username: username})
	if err != nil {
		return fmt.Errorf("encode slack payload: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, postTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhook, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build slack request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.http.Do(req)
	if err != nil {
		return fmt.Errorf("post to slack: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%w: HTTP %d", ErrWebhookFailed, resp.StatusCode)
	}
	return nil
}
