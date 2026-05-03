package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"
)

// BlockHeader matches chaintracks.BlockHeader + embedded block.Header JSON fields.
type BlockHeader struct {
	// From block.Header
	Version    int32  `json:"version"`
	PrevHash   string `json:"previousHash"`
	MerkleRoot string `json:"merkleRoot"`
	Timestamp  uint32 `json:"time"`
	Bits       uint32 `json:"bits"`
	Nonce      uint32 `json:"nonce"`
	// From chaintracks.BlockHeader
	Height uint32 `json:"height"`
	Hash   string `json:"hash"`
}

// ReorgEvent matches chaintracks.ReorgEvent.
type ReorgEvent struct {
	OrphanedHashes []string     `json:"orphanedHashes"`
	CommonAncestor *BlockHeader `json:"commonAncestor"`
	NewTip         *BlockHeader `json:"newTip"`
	Depth          uint32       `json:"depth"`
}

func startSSESubscribers(ctx context.Context, engine *Engine, cfg *Config, logger *slog.Logger, notifier *Notifier, wg *sync.WaitGroup) {
	if logger == nil {
		logger = slog.Default()
	}
	if wg == nil {
		wg = &sync.WaitGroup{}
	}
	base := arcadeBase
	reconnectMin := time.Second
	reconnectMax := 30 * time.Second
	if cfg != nil {
		base = cfg.ArcadeBaseURL
		reconnectMin = cfg.SSEReconnectInitial
		reconnectMax = cfg.SSEReconnectMax
	}
	if reconnectMin <= 0 {
		reconnectMin = time.Second
	}
	if reconnectMax < reconnectMin {
		reconnectMax = reconnectMin
	}

	client := NewHTTPClient(SSETransport(), 0)
	tipURL := strings.TrimRight(base, "/") + "/chaintracks/v2/tip/stream"
	reorgURL := strings.TrimRight(base, "/") + "/chaintracks/v2/reorg/stream"

	SafeGo(wg, "sse_tip", func() {
		streamLoop(ctx, client, tipURL, "tip", reconnectMin, reconnectMax, logger, func(data []byte) {
			handleTipEvent(engine, logger, data)
		})
	})
	SafeGo(wg, "sse_reorg", func() {
		streamLoop(ctx, client, reorgURL, "reorg", reconnectMin, reconnectMax, logger, func(data []byte) {
			handleReorgEvent(engine, logger, notifier, data)
		})
	})
}

func handleTipEvent(engine *Engine, logger *slog.Logger, data []byte) {
	var h BlockHeader
	if err := json.Unmarshal(data, &h); err != nil {
		logger.Warn("SSE tip parse error", "error", err, "raw", string(data))
		return
	}
	if !callEngineMethod(engine, "RecordTip", int(h.Height), h.Hash, time.Unix(int64(h.Timestamp), 0)) {
		callEngineMethod(engine, "RecordTip", int(h.Height), h.Hash)
	}
	if !callEngineMethod(engine, "UpdateTip", int(h.Height), h.Hash, time.Unix(int64(h.Timestamp), 0)) {
		callEngineMethod(engine, "UpdateTip", int(h.Height), h.Hash)
	}
	logger.Info("SSE tip", "height", h.Height, "hash", h.Hash)
}

func handleReorgEvent(engine *Engine, logger *slog.Logger, notifier *Notifier, data []byte) {
	var r ReorgEvent
	if err := json.Unmarshal(data, &r); err != nil {
		logger.Warn("SSE reorg parse error", "error", err, "raw", string(data))
		return
	}

	height := reorgHeight(r)
	depth := int(r.Depth)
	tipBefore := ""
	if len(r.OrphanedHashes) > 0 {
		tipBefore = r.OrphanedHashes[0]
	}
	tipAfter := ""
	tipAfterHeight := uint32(0)
	if r.NewTip != nil {
		tipAfter = r.NewTip.Hash
		tipAfterHeight = r.NewTip.Height
	}

	callEngineMethod(engine, "RecordReorg", height, depth, tipBefore, tipAfter)
	chainsImpacted := estimateChainsImpacted(engine)
	if notifier != nil && notifier.webhook != "" {
		notifier.ReportReorg(height, depth, chainsImpacted, tipBefore, tipAfter)
	} else {
		IncReorg(depth)
	}
	logger.Info(
		"SSE reorg",
		"height", height,
		"depth", depth,
		"orphaned_hashes", len(r.OrphanedHashes),
		"chains_impacted", chainsImpacted,
		"tip_before", tipBefore,
		"tip_after", tipAfter,
		"tip_after_height", tipAfterHeight,
	)
}

func reorgHeight(r ReorgEvent) int {
	if r.CommonAncestor != nil {
		return int(r.CommonAncestor.Height) + 1
	}
	if r.NewTip != nil && r.Depth > 0 && r.NewTip.Height >= r.Depth {
		return int(r.NewTip.Height-r.Depth) + 1
	}
	return 0
}

func estimateChainsImpacted(engine *Engine) int {
	snapshot, ok := callSnapshot(engine)
	if !ok {
		return 0
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return 0
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return 0
	}
	if chains, ok := firstNumber(m, "usableChains", "activeChainCount", "chainsActive", "ActiveChains"); ok {
		return int(chains)
	}
	return 0
}

// streamLoop connects to an SSE endpoint and dispatches each data payload to handler.
// Reconnects automatically on disconnect.
func streamLoop(ctx context.Context, client *http.Client, url, name string, reconnectMin, reconnectMax time.Duration, logger *slog.Logger, handler func([]byte)) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		if err := connectStream(ctx, client, url, name, logger, handler); err != nil && ctx.Err() == nil {
			attempt++
			IncSSEDisconnect(name)
			SetSSEConnected(false, name)
			delay := reconnectDelay(reconnectMin, reconnectMax, attempt)
			logger.Warn("SSE disconnected", "stream", name, "error", err, "reconnect_in", delay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			continue
		}
		attempt = 0
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func connectStream(ctx context.Context, client *http.Client, url, name string, logger *slog.Logger, handler func([]byte)) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}

	IncSSEEvent(name)
	SetSSEConnected(true, name)
	defer SetSSEConnected(false, name)
	logger.Info("SSE connected", "stream", name)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	activity, stopIdle := watchIdle(resp.Body, 60*time.Second)
	defer stopIdle()
	signalActivity(activity)

	var data strings.Builder
	for scanner.Scan() {
		signalActivity(activity)
		line := scanner.Text()
		if line == "" {
			if data.Len() > 0 {
				handler([]byte(strings.TrimSuffix(data.String(), "\n")))
				data.Reset()
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data.WriteString(sseData(line))
			data.WriteByte('\n')
			continue
		}
	}
	if data.Len() > 0 {
		handler([]byte(strings.TrimSuffix(data.String(), "\n")))
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.EOF
}

func sseData(line string) string {
	value := strings.TrimPrefix(line, "data:")
	return strings.TrimPrefix(value, " ")
}

func watchIdle(body io.Closer, timeout time.Duration) (chan<- struct{}, func()) {
	activity := make(chan struct{}, 1)
	done := make(chan struct{})
	var wg sync.WaitGroup
	SafeGo(&wg, "sse_idle_watch", func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				_ = body.Close()
				return
			case <-activity:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(timeout)
			case <-done:
				return
			}
		}
	})
	return activity, func() { close(done) }
}

func signalActivity(activity chan<- struct{}) {
	select {
	case activity <- struct{}{}:
	default:
	}
}

func reconnectDelay(minDelay, maxDelay time.Duration, attempt int) time.Duration {
	if attempt <= 0 {
		attempt = 1
	}
	delay := minDelay << (attempt - 1)
	if delay <= 0 || delay > maxDelay {
		delay = maxDelay
	}
	if delay <= 0 {
		return minDelay
	}
	return delay + time.Duration(rand.Int63n(int64(delay/2+1)))
}

func callEngineMethod(engine *Engine, name string, args ...any) bool {
	if engine == nil {
		return false
	}
	method := reflect.ValueOf(engine).MethodByName(name)
	if !method.IsValid() || method.Type().NumIn() != len(args) {
		return false
	}
	values := make([]reflect.Value, len(args))
	for i, arg := range args {
		param := method.Type().In(i)
		value := reflect.ValueOf(arg)
		if !value.IsValid() {
			return false
		}
		if value.Type().AssignableTo(param) {
			values[i] = value
			continue
		}
		if value.Type().ConvertibleTo(param) {
			values[i] = value.Convert(param)
			continue
		}
		return false
	}
	method.Call(values)
	return true
}
