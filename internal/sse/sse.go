// Package sse subscribes to Arcade's server-sent event streams: the
// chaintracks tip and reorg streams, and (with a callback token) the
// per-transaction status events.
//
// Each stream reconnects with exponential backoff and is dropped and
// reopened if it stays silent for longer than the idle timeout.
package sse

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/tx-gen/internal/arcade"
	"github.com/bsv-blockchain/tx-gen/internal/backoff"
	"github.com/bsv-blockchain/tx-gen/internal/httpclient"
	"github.com/bsv-blockchain/tx-gen/internal/metrics"
	"github.com/bsv-blockchain/tx-gen/internal/safe"
)

// Stream names, used in logs and metric labels.
const (
	StreamTip      = "tip"
	StreamReorg    = "reorg"
	StreamArcadeTx = "arcade_tx"
)

const (
	// defaultIdleTimeout drops a stream that has sent nothing for this long.
	defaultIdleTimeout = 60 * time.Second

	// defaultReconnectInitial is the first reconnect delay.
	defaultReconnectInitial = time.Second

	// defaultReconnectMax caps the reconnect delay.
	defaultReconnectMax = 30 * time.Second

	// maxEventBytes caps the size of one event.
	maxEventBytes = 1 << 20

	// maxLoggedBytes caps how much of a raw event is logged.
	maxLoggedBytes = 4096
)

// ErrUnexpectedStatus is returned when a stream answers without 200 OK.
var ErrUnexpectedStatus = errors.New("unexpected SSE response status")

// EventSink receives chain events.
type EventSink interface {
	// RecordTip records a new chain tip.
	RecordTip(height int, hash string, at time.Time)

	// RecordReorg records a reorg and returns the number of chains active.
	RecordReorg(height, depth int, tipBefore, tipAfter string) int
}

// ReorgReporter is notified of each reorg, for example to post it to Slack.
type ReorgReporter interface {
	// ReportReorg reports one reorg.
	ReportReorg(ctx context.Context, height, depth, chainsImpacted int, tipBefore, tipAfter string)
}

// Options configures the subscribers. Sink is required.
type Options struct {
	// BaseURL is the Arcade node; it defaults to arcade.DefaultBaseURL.
	BaseURL string

	// CallbackToken enables the transaction-event stream when set.
	CallbackToken string

	// ReconnectMin is the first reconnect delay; it defaults to 1s.
	ReconnectMin time.Duration

	// ReconnectMax caps the reconnect delay; it defaults to 30s.
	ReconnectMax time.Duration

	// IdleTimeout reopens a silent stream; it defaults to 60s.
	IdleTimeout time.Duration

	// HTTPClient overrides the streaming client.
	HTTPClient *http.Client

	// Sink receives tip and reorg events.
	Sink EventSink

	// Reporter, when set, is notified of every reorg.
	Reporter ReorgReporter

	// Metrics receives stream metrics. It may be nil.
	Metrics *metrics.Metrics

	// Logger receives stream logs; it defaults to slog.Default.
	Logger *slog.Logger

	// Runner starts the stream goroutines.
	Runner safe.Runner
}

// BlockHeader is a chaintracks block header as sent on the tip stream.
type BlockHeader struct {
	Version    int32  `json:"version"`
	PrevHash   string `json:"previousHash"`
	MerkleRoot string `json:"merkleRoot"`
	Timestamp  uint32 `json:"time"`
	Bits       uint32 `json:"bits"`
	Nonce      uint32 `json:"nonce"`
	Height     uint32 `json:"height"`
	Hash       string `json:"hash"`
}

// ReorgEvent is a chaintracks reorg as sent on the reorg stream.
type ReorgEvent struct {
	OrphanedHashes []string     `json:"orphanedHashes"`
	CommonAncestor *BlockHeader `json:"commonAncestor"`
	NewTip         *BlockHeader `json:"newTip"`
	Depth          uint32       `json:"depth"`
}

// subscriber holds the shared state of every stream.
type subscriber struct {
	client       *http.Client
	reconnectMin time.Duration
	reconnectMax time.Duration
	idleTimeout  time.Duration
	sink         EventSink
	reporter     ReorgReporter
	metrics      *metrics.Metrics
	logger       *slog.Logger
	runner       safe.Runner
}

// Start subscribes to the tip and reorg streams, and to the transaction-event
// stream when a callback token is set. The goroutines are tracked by wg and
// run until ctx is canceled.
func Start(ctx context.Context, wg *sync.WaitGroup, opts Options) {
	s := newSubscriber(opts)
	base := strings.TrimRight(opts.BaseURL, "/")
	if base == "" {
		base = arcade.DefaultBaseURL
	}

	s.runner.Go(wg, "sse_tip", func() {
		s.streamLoop(ctx, base+"/chaintracks/v2/tip/stream", StreamTip, s.handleTip)
	})
	s.runner.Go(wg, "sse_reorg", func() {
		s.streamLoop(ctx, base+"/chaintracks/v2/reorg/stream", StreamReorg, s.handleReorg)
	})
	if opts.CallbackToken == "" {
		s.logger.Info("Arcade tx SSE disabled", "reason", "ARCADE_CALLBACK_TOKEN not set")
		return
	}
	eventsURL := base + "/events?callbackToken=" + url.QueryEscape(opts.CallbackToken)
	s.runner.Go(wg, "sse_arcade_tx", func() {
		s.streamLoop(ctx, eventsURL, StreamArcadeTx, s.handleArcadeTx)
	})
}

// newSubscriber applies the defaults to opts.
func newSubscriber(opts Options) *subscriber {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	client := opts.HTTPClient
	if client == nil {
		client = httpclient.New(httpclient.NewStreamTransport(), 0)
	}
	reconnectMin := opts.ReconnectMin
	if reconnectMin <= 0 {
		reconnectMin = defaultReconnectInitial
	}
	reconnectMax := opts.ReconnectMax
	if reconnectMax <= 0 {
		reconnectMax = defaultReconnectMax
	}
	idle := opts.IdleTimeout
	if idle <= 0 {
		idle = defaultIdleTimeout
	}
	runner := opts.Runner
	if runner.Logger == nil {
		runner.Logger = logger
	}
	return &subscriber{
		client:       client,
		reconnectMin: reconnectMin,
		reconnectMax: max(reconnectMax, reconnectMin),
		idleTimeout:  idle,
		sink:         opts.Sink,
		reporter:     opts.Reporter,
		metrics:      opts.Metrics,
		logger:       logger,
		runner:       runner,
	}
}

// handleTip records a tip event.
func (s *subscriber) handleTip(_ context.Context, data []byte) {
	var h BlockHeader
	if err := json.Unmarshal(data, &h); err != nil {
		s.logger.Warn("SSE tip parse error", "error", err, "raw", arcade.Truncate(string(data), maxLoggedBytes))
		return
	}
	s.sink.RecordTip(int(h.Height), h.Hash, time.Unix(int64(h.Timestamp), 0))
	s.logger.Info("SSE tip", "height", h.Height, "hash", h.Hash)
}

// handleReorg records a reorg event, counts it, and reports it.
func (s *subscriber) handleReorg(ctx context.Context, data []byte) {
	var r ReorgEvent
	if err := json.Unmarshal(data, &r); err != nil {
		s.logger.Warn("SSE reorg parse error", "error", err, "raw", arcade.Truncate(string(data), maxLoggedBytes))
		return
	}

	height := reorgHeight(r)
	depth := int(r.Depth)
	var tipBefore, tipAfter string
	var tipAfterHeight uint32
	if len(r.OrphanedHashes) > 0 {
		tipBefore = r.OrphanedHashes[0]
	}
	if r.NewTip != nil {
		tipAfter = r.NewTip.Hash
		tipAfterHeight = r.NewTip.Height
	}

	chainsImpacted := s.sink.RecordReorg(height, depth, tipBefore, tipAfter)
	s.metrics.IncReorg(depth)
	if s.reporter != nil {
		s.reporter.ReportReorg(ctx, height, depth, chainsImpacted, tipBefore, tipAfter)
	}
	s.logger.Info("SSE reorg",
		"height", height,
		"depth", depth,
		"orphaned_hashes", len(r.OrphanedHashes),
		"chains_impacted", chainsImpacted,
		"tip_before", tipBefore,
		"tip_after", tipAfter,
		"tip_after_height", tipAfterHeight,
	)
}

// handleArcadeTx logs a transaction status event. Rejections log at warn.
func (s *subscriber) handleArcadeTx(ctx context.Context, data []byte) {
	raw := arcade.Truncate(string(data), maxLoggedBytes)
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		s.logger.Warn("Arcade tx SSE parse error", "error", err, "raw", raw)
		return
	}
	nested, _ := payload["data"].(map[string]any)
	field := func(keys ...string) string {
		if v := jsonString(payload, keys...); v != "" {
			return v
		}
		return jsonString(nested, keys...)
	}
	txid := field("txid", "txId", "transactionId", "transactionID")
	status := field("txStatus", "tx_status", "status")
	eventType := field("type", "event", "eventType")
	extraInfo := field("extraInfo", "extra_info", "reason", "message")

	level := slog.LevelDebug
	if strings.EqualFold(status, "rejected") {
		level = slog.LevelWarn
	}
	attrs := []any{"raw", raw}
	for _, kv := range [][2]string{{"txid", txid}, {"tx_status", status}, {"event_type", eventType}, {"extra_info", extraInfo}} {
		if kv[1] != "" {
			attrs = append(attrs, kv[0], kv[1])
		}
	}
	s.logger.Log(ctx, level, "Arcade tx SSE", attrs...)
	s.metrics.IncSSEEvent(StreamArcadeTx)
}

// streamLoop keeps one stream connected until ctx is canceled, reconnecting
// with backoff after each disconnect.
//
// The backoff restarts only after a connection that stayed up for at least
// the maximum reconnect delay: a long-lived stream that drops once
// reconnects quickly, while an endpoint that accepts and then closes at once
// still backs off.
func (s *subscriber) streamLoop(ctx context.Context, streamURL, name string, handle func(context.Context, []byte)) {
	attempt := 0
	for ctx.Err() == nil {
		uptime, err := s.connect(ctx, streamURL, name, handle)
		if ctx.Err() != nil {
			return
		}
		if uptime >= s.reconnectMax {
			attempt = 0
		}
		attempt++
		s.metrics.IncSSEDisconnect(name)
		s.metrics.SetSSEConnected(false, name)
		delay := backoff.Reconnect(s.reconnectMin, s.reconnectMax, attempt)
		s.logger.Warn("SSE disconnected", "stream", name, "error", err, "reconnect_in", delay)
		if backoff.Sleep(ctx, delay) != nil {
			return
		}
	}
}

// connect reads one stream connection until it ends, dispatching each event
// to handle. It returns how long the stream stayed established (zero when it
// never answered 200 OK); a stream that ends cleanly returns io.EOF.
func (s *subscriber) connect(ctx context.Context, streamURL, name string, handle func(context.Context, []byte)) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return 0, fmt.Errorf("build SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("connect SSE stream %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%w: %d", ErrUnexpectedStatus, resp.StatusCode)
	}
	established := time.Now()

	s.metrics.IncSSEEvent(name)
	s.metrics.SetSSEConnected(true, name)
	defer s.metrics.SetSSEConnected(false, name)
	s.logger.Info("SSE connected", "stream", name)

	activity, stopIdle := s.watchIdle(resp.Body)
	defer stopIdle()
	signalActivity(activity)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxEventBytes)
	var event strings.Builder
	dispatch := func() {
		if event.Len() > 0 {
			handle(ctx, []byte(strings.TrimSuffix(event.String(), "\n")))
			event.Reset()
		}
	}
	for scanner.Scan() {
		signalActivity(activity)
		line := scanner.Text()
		if line == "" {
			dispatch()
			continue
		}
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			event.WriteString(strings.TrimPrefix(data, " "))
			event.WriteByte('\n')
		}
	}
	dispatch()
	uptime := max(time.Since(established), time.Nanosecond)
	if err = scanner.Err(); err != nil {
		return uptime, fmt.Errorf("read SSE stream %s: %w", name, err)
	}
	return uptime, io.EOF
}

// watchIdle closes body when no activity is signaled for the idle timeout,
// which unblocks the reader so the stream reconnects. Call the returned
// function to stop watching.
func (s *subscriber) watchIdle(body io.Closer) (chan<- struct{}, func()) {
	activity := make(chan struct{}, 1)
	done := make(chan struct{})
	var wg sync.WaitGroup
	s.runner.Go(&wg, "sse_idle_watch", func() {
		timer := time.NewTimer(s.idleTimeout)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				_ = body.Close()
				return
			case <-activity:
				timer.Reset(s.idleTimeout)
			case <-done:
				return
			}
		}
	})
	return activity, func() {
		close(done)
		wg.Wait()
	}
}

// signalActivity records stream activity without blocking.
func signalActivity(activity chan<- struct{}) {
	select {
	case activity <- struct{}{}:
	default:
	}
}

// reorgHeight returns the first height replaced by r, or 0 when unknown.
func reorgHeight(r ReorgEvent) int {
	if r.CommonAncestor != nil {
		return int(r.CommonAncestor.Height) + 1
	}
	if r.NewTip != nil && r.Depth > 0 && r.NewTip.Height >= r.Depth {
		return int(r.NewTip.Height-r.Depth) + 1
	}
	return 0
}

// jsonString returns the first string value among keys in payload.
func jsonString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := payload[key].(string); ok {
			return v
		}
	}
	return ""
}
