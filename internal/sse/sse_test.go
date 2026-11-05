package sse

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bsv-blockchain/tx-gen/internal/metrics"
)

// fakeSink records the events it receives.
type fakeSink struct {
	mu     sync.Mutex
	tips   []int
	reorgs []string
}

func (f *fakeSink) RecordTip(height int, _ string, _ time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tips = append(f.tips, height)
}

func (f *fakeSink) RecordReorg(height, depth int, before, after string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reorgs = append(f.reorgs, fmt.Sprintf("%d/%d %s->%s", height, depth, before, after))
	return 42
}

func (f *fakeSink) tipCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tips)
}

func (f *fakeSink) reorgList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reorgs...)
}

// fakeReporter records reported reorgs.
type fakeReporter struct {
	calls atomic.Int32
	last  atomic.Int64
}

func (f *fakeReporter) ReportReorg(_ context.Context, _, _, chainsImpacted int, _, _ string) {
	f.calls.Add(1)
	f.last.Store(int64(chainsImpacted))
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newTestSubscriber(sink EventSink, logs *syncBuffer, m *metrics.Metrics) *subscriber {
	return newSubscriber(Options{
		Sink:         sink,
		Metrics:      m,
		ReconnectMin: time.Millisecond,
		ReconnectMax: 5 * time.Millisecond,
		IdleTimeout:  time.Second,
		Logger:       slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
}

// streamServer serves each SSE path with a fixed body, then closes the stream.
func streamServer(t *testing.T, bodies map[string]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestStartDeliversTipAndReorgEvents(t *testing.T) {
	server := streamServer(t, map[string]string{
		"/chaintracks/v2/tip/stream": "data: {\"height\":900000,\"hash\":\"tip\",\"time\":1700000000}\n\n",
		"/chaintracks/v2/reorg/stream": "data: {\"orphanedHashes\":[\"old\"],\"commonAncestor\":{\"height\":899998},\n" +
			"data: \"newTip\":{\"height\":900001,\"hash\":\"new\"},\"depth\":2}\n\n",
	})
	sink := &fakeSink{}
	reporter := &fakeReporter{}
	m := metrics.New()

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	Start(ctx, &wg, Options{
		BaseURL:      server.URL + "/",
		Sink:         sink,
		Reporter:     reporter,
		Metrics:      m,
		ReconnectMin: time.Millisecond,
		ReconnectMax: 5 * time.Millisecond,
	})

	require.Eventually(t, func() bool { return sink.tipCount() > 0 && reporter.calls.Load() > 0 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	wg.Wait()

	assert.Equal(t, 900_000, sink.tips[0])
	assert.Equal(t, "899999/2 old->new", sink.reorgList()[0], "multi-line data is joined into one event")
	assert.Equal(t, int64(42), reporter.last.Load())
	count, err := testutil.GatherAndCount(m.Registry(), "txgen_reorg_total")
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestStartSubscribesToTxEventsWithCallbackToken(t *testing.T) {
	var gotToken atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" {
			gotToken.Store(r.URL.Query().Get("callbackToken"))
			_, _ = fmt.Fprint(w, "data: {\"txid\":\"abc\",\"txStatus\":\"MINED\"}\n\n")
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	Start(ctx, &wg, Options{
		BaseURL:       server.URL,
		CallbackToken: "secret token",
		Sink:          &fakeSink{},
		ReconnectMin:  time.Millisecond,
		ReconnectMax:  5 * time.Millisecond,
	})
	require.Eventually(t, func() bool { return gotToken.Load() != nil }, 5*time.Second, 5*time.Millisecond)
	cancel()
	wg.Wait()
	assert.Equal(t, "secret token", gotToken.Load())
}

func TestStartWithoutCallbackTokenSkipsTxEvents(t *testing.T) {
	var eventsHit atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" {
			eventsHit.Store(true)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	Start(ctx, &wg, Options{
		BaseURL:      server.URL,
		Sink:         &fakeSink{},
		ReconnectMin: time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(logs, nil)),
	})
	time.Sleep(30 * time.Millisecond)
	cancel()
	wg.Wait()
	assert.False(t, eventsHit.Load())
	assert.Contains(t, logs.String(), "Arcade tx SSE disabled")
	assert.Contains(t, logs.String(), "SSE disconnected")
}

func TestConnectReportsStatusAndEOF(t *testing.T) {
	server := streamServer(t, map[string]string{"/ok": "data: one\n\ndata: two\n"})
	logs := &syncBuffer{}
	s := newTestSubscriber(&fakeSink{}, logs, nil)

	var events []string
	uptime, err := s.connect(t.Context(), server.URL+"/ok", "test", func(_ context.Context, data []byte) {
		events = append(events, string(data))
	})
	assert.Positive(t, uptime)
	require.ErrorIs(t, err, io.EOF)
	assert.Equal(t, []string{"one", "two"}, events, "a trailing event without a blank line is still delivered")

	uptime, err = s.connect(t.Context(), server.URL+"/missing", "test", nil)
	assert.Zero(t, uptime)
	require.ErrorIs(t, err, ErrUnexpectedStatus)

	uptime, err = s.connect(t.Context(), "http://127.0.0.1:1/unreachable", "test", nil)
	assert.Zero(t, uptime)
	require.Error(t, err)
}

func TestConnectDropsIdleStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // never send an event
	}))
	defer server.Close()

	s := newTestSubscriber(&fakeSink{}, &syncBuffer{}, nil)
	s.idleTimeout = 20 * time.Millisecond
	done := make(chan struct{})
	go func() {
		_, _ = s.connect(t.Context(), server.URL, "idle", func(context.Context, []byte) {})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("idle stream was not dropped")
	}
}

func TestHandleTipRejectsMalformedJSON(t *testing.T) {
	logs := &syncBuffer{}
	sink := &fakeSink{}
	s := newTestSubscriber(sink, logs, nil)
	s.handleTip(t.Context(), []byte("{bad"))
	assert.Zero(t, sink.tipCount())
	assert.Contains(t, logs.String(), "SSE tip parse error")
}

func TestHandleReorgCountsWithoutReporter(t *testing.T) {
	m := metrics.New()
	sink := &fakeSink{}
	s := newTestSubscriber(sink, &syncBuffer{}, m)
	s.handleReorg(t.Context(), []byte(`{"orphanedHashes":[],"newTip":{"height":100,"hash":"n"},"depth":3}`))
	assert.Equal(t, []string{"98/3 ->n"}, sink.reorgList())

	count, err := testutil.GatherAndCount(m.Registry(), "txgen_reorg_total")
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	logs := &syncBuffer{}
	s = newTestSubscriber(sink, logs, m)
	s.handleReorg(t.Context(), []byte("nope"))
	assert.Contains(t, logs.String(), "SSE reorg parse error")
}

func TestHandleArcadeTx(t *testing.T) {
	logs := &syncBuffer{}
	s := newTestSubscriber(&fakeSink{}, logs, nil)

	s.handleArcadeTx(t.Context(), []byte(`{"type":"status","data":{"txId":"abc","status":"REJECTED","reason":"double spend"}}`))
	out := logs.String()
	assert.Contains(t, out, "level=WARN")
	assert.Contains(t, out, "txid=abc")
	assert.Contains(t, out, "tx_status=REJECTED")
	assert.Contains(t, out, "event_type=status")
	assert.Contains(t, out, `extra_info="double spend"`)

	s.handleArcadeTx(t.Context(), []byte(`{"txid":"def","txStatus":"MINED"}`))
	assert.Contains(t, logs.String(), "level=DEBUG")

	s.handleArcadeTx(t.Context(), []byte("not json"))
	assert.Contains(t, logs.String(), "Arcade tx SSE parse error")
}

func TestReorgHeight(t *testing.T) {
	assert.Equal(t, 11, reorgHeight(ReorgEvent{CommonAncestor: &BlockHeader{Height: 10}}))
	assert.Equal(t, 98, reorgHeight(ReorgEvent{NewTip: &BlockHeader{Height: 100}, Depth: 3}))
	assert.Zero(t, reorgHeight(ReorgEvent{NewTip: &BlockHeader{Height: 2}, Depth: 3}))
	assert.Zero(t, reorgHeight(ReorgEvent{}))
}

func TestJSONString(t *testing.T) {
	payload := map[string]any{"a": 1, "b": "two"}
	assert.Equal(t, "two", jsonString(payload, "a", "b"))
	assert.Empty(t, jsonString(payload, "c"))
	assert.Empty(t, jsonString(nil, "a"))
}

func TestNewSubscriberClampsReconnectMax(t *testing.T) {
	s := newSubscriber(Options{ReconnectMin: time.Minute, ReconnectMax: time.Second})
	assert.Equal(t, time.Minute, s.reconnectMin)
	assert.Equal(t, time.Minute, s.reconnectMax, "max is raised to min")

	s = newSubscriber(Options{})
	assert.Equal(t, defaultReconnectInitial, s.reconnectMin)
	assert.Equal(t, defaultReconnectMax, s.reconnectMax)
	assert.Equal(t, defaultIdleTimeout, s.idleTimeout)
}

// TestStreamLoopBacksOffOnInstantClose verifies an endpoint that accepts and
// closes at once does not reset the backoff on every connection.
func TestStreamLoopBacksOffOnInstantClose(t *testing.T) {
	var connects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		connects.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	s := newSubscriber(Options{
		Sink:         &fakeSink{},
		ReconnectMin: 10 * time.Millisecond,
		ReconnectMax: 200 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ctx, cancel := context.WithTimeout(t.Context(), 600*time.Millisecond)
	defer cancel()
	s.streamLoop(ctx, server.URL, "instant", func(context.Context, []byte) {})

	// Without backoff this would reconnect about 40 times (every ~15ms);
	// backing off 10, 20, 40, 80, 160, 200ms... allows only a handful.
	assert.Less(t, connects.Load(), int32(12))
	assert.GreaterOrEqual(t, connects.Load(), int32(3))
}

func TestStreamLoopStopsOnCancel(t *testing.T) {
	server := streamServer(t, map[string]string{"/s": "data: x\n\n"})
	s := newTestSubscriber(&fakeSink{}, &syncBuffer{}, nil)
	var count atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		s.streamLoop(ctx, server.URL+"/s", "s", func(context.Context, []byte) { count.Add(1) })
		close(done)
	}()
	require.Eventually(t, func() bool { return count.Load() >= 2 }, 5*time.Second, time.Millisecond, "the stream reconnects")
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("streamLoop did not stop")
	}
}
