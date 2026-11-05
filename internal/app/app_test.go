package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bsv-blockchain/tx-gen/internal/config"
	"github.com/bsv-blockchain/tx-gen/internal/safe"
	"github.com/bsv-blockchain/tx-gen/internal/store"
	"github.com/bsv-blockchain/tx-gen/internal/txbuilder"
)

const (
	testToken = "secret"
	testWIF   = "cNGwGSc7KRrTmdLUZ54fiSXWbhLNDc2Eg5zNucgQxyQCzuQ5YRDq"
	seedTxID  = "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0"
)

// fakeArcade accepts every broadcast and serves empty SSE streams.
type fakeArcade struct {
	broadcasts atomic.Int32
}

func (f *fakeArcade) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/tx":
		_, _ = io.Copy(io.Discard, r.Body)
		f.broadcasts.Add(1)
		w.WriteHeader(http.StatusAccepted)
	case r.URL.Path == "/health":
		w.WriteHeader(http.StatusOK)
	case strings.HasSuffix(r.URL.Path, "/stream"):
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	return strconv.Itoa(port)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func taggedMode(t *testing.T) *txbuilder.Mode {
	t.Helper()
	mode, err := txbuilder.NewTaggedDropMode("inst01", 0)
	require.NoError(t, err)
	return mode
}

func get(t *testing.T, url string, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestRunEndToEnd boots the whole service against fake Arcade and
// WhatsOnChain servers: it bootstraps 4 chains from one funding UTXO, becomes
// ready, generates on demand, and shuts down cleanly.
func TestRunEndToEnd(t *testing.T) {
	arcadeFake := &fakeArcade{}
	arcadeServer := httptest.NewServer(arcadeFake)
	defer arcadeServer.Close()
	wocServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"result":[{"height":900000,"tx_hash":%q,"tx_pos":0,"value":1000000}]}`, seedTxID)
	}))
	defer wocServer.Close()

	port := freePort(t)
	cfg := &config.Config{
		AdminToken:           testToken,
		Port:                 port,
		StatePath:            filepath.Join(t.TempDir(), "state.db"),
		ArcadeBaseURL:        arcadeServer.URL,
		WOCBaseURL:           wocServer.URL,
		InstanceID:           "inst01",
		NumChains:            4,
		FanoutSize:           2,
		SustainFee:           txbuilder.DefaultSustainFee,
		MaxTPS:               10_000,
		BroadcastConcurrency: 8,
		BroadcastRetryMax:    1,
		HTTPTimeout:          5 * time.Second,
		SSEReconnectMin:      10 * time.Millisecond,
		SSEReconnectMax:      50 * time.Millisecond,
		SlackWebhookURL:      "http://127.0.0.1:1/unused",
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, discardLogger(), BuildInfo{Version: "test"}) }()

	base := "http://127.0.0.1:" + port
	require.Eventually(t, func() bool {
		status, _ := get(t, base+"/readyz", nil)
		return status == http.StatusOK
	}, 10*time.Second, 20*time.Millisecond, "the service never became ready")
	assert.Equal(t, int32(3), arcadeFake.broadcasts.Load(), "1 L1 + 2 L2 fanouts")

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/config", strings.NewReader(`{"tps":1000}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Eventually(t, func() bool { return arcadeFake.broadcasts.Load() > 10 }, 10*time.Second, 10*time.Millisecond)

	status, body := get(t, base+"/status", map[string]string{"Authorization": "Bearer " + testToken})
	require.Equal(t, http.StatusOK, status)
	var payload struct {
		Version string `json:"version"`
		Engine  struct {
			Lifecycle      string `json:"lifecycle"`
			BootstrapStage string `json:"bootstrapStage"`
			ChainsActive   int    `json:"chainsActive"`
		} `json:"engine"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &payload))
	assert.Equal(t, "test", payload.Version)
	assert.Equal(t, "running", payload.Engine.Lifecycle)
	assert.Equal(t, "l2_done", payload.Engine.BootstrapStage)
	assert.Equal(t, 4, payload.Engine.ChainsActive)

	status, body = get(t, base+"/metrics", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `txgen_bootstrap_stage{stage="l2_done"} 1`)
	assert.Contains(t, body, "txgen_chains_active 4")

	cancel()
	select {
	case err = <-done:
		require.NoError(t, err)
	case <-time.After(40 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	// The state file resumes the same chains without a new bootstrap.
	st, err := store.Open(cfg.StatePath)
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()
	utxos, err := st.LoadAll()
	require.NoError(t, err)
	assert.Len(t, utxos, 4)
	stage, err := st.GetMeta(store.MetaBootstrapStage)
	require.NoError(t, err)
	assert.Equal(t, "l2_done", stage)
}

func TestRunRejectsInvalidMode(t *testing.T) {
	cfg := &config.Config{PrivateKey: "not-a-key", StatePath: filepath.Join(t.TempDir(), "state.db")}
	err := Run(t.Context(), cfg, discardLogger(), BuildInfo{})
	require.ErrorIs(t, err, txbuilder.ErrInvalidPrivateKey)
}

func TestRunReportsStoreErrors(t *testing.T) {
	cfg := &config.Config{InstanceID: "inst01", StatePath: t.TempDir()} // a directory, not a file
	require.Error(t, Run(t.Context(), cfg, discardLogger(), BuildInfo{}))
}

func TestRunReportsFetchErrors(t *testing.T) {
	wocServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer wocServer.Close()
	cfg := &config.Config{
		InstanceID: "inst01",
		StatePath:  filepath.Join(t.TempDir(), "state.db"),
		WOCBaseURL: wocServer.URL,
	}
	err := Run(t.Context(), cfg, discardLogger(), BuildInfo{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fetch UTXOs")
}

func TestRunReportsListenErrors(t *testing.T) {
	wocServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"result":[]}`)
	}))
	defer wocServer.Close()
	cfg := &config.Config{
		AdminToken:      testToken,
		Port:            "not-a-port",
		InstanceID:      "inst01",
		StatePath:       filepath.Join(t.TempDir(), "state.db"),
		WOCBaseURL:      wocServer.URL,
		ArcadeBaseURL:   "http://127.0.0.1:1",
		SSEReconnectMin: time.Millisecond,
		SSEReconnectMax: time.Millisecond,
	}
	err := Run(t.Context(), cfg, discardLogger(), BuildInfo{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listen")
}

func TestEnsureStateMatchesModeRecordsNewState(t *testing.T) {
	st := openStore(t)
	mode := taggedMode(t)
	require.NoError(t, ensureStateMatchesMode(st, mode, false))

	hash, err := st.GetMeta(store.MetaLockScriptHash)
	require.NoError(t, err)
	assert.Equal(t, mode.ScriptHash(), hash)
	name, err := st.GetMeta(store.MetaTxMode)
	require.NoError(t, err)
	assert.Equal(t, txbuilder.ModeTaggedDrop, name)

	require.NoError(t, ensureStateMatchesMode(st, mode, true), "the same mode resumes")
}

func TestEnsureStateMatchesModeRejectsMismatchedState(t *testing.T) {
	st := openStore(t)
	require.NoError(t, st.SetMeta(store.MetaLockScriptHash, taggedMode(t).ScriptHash()))
	p2pkh, err := txbuilder.NewP2PKHMode(testWIF, 0)
	require.NoError(t, err)

	err = ensureStateMatchesMode(st, p2pkh, true)
	require.ErrorIs(t, err, ErrStateMismatch)
	assert.Contains(t, err.Error(), "STATE_PATH")
}

func TestEnsureStateMatchesModeRejectsLegacyState(t *testing.T) {
	st := openStore(t)
	require.NoError(t, st.SaveUTXO(txbuilder.UTXO{TxHash: seedTxID, Value: 100}))
	err := ensureStateMatchesMode(st, taggedMode(t), true)
	require.ErrorIs(t, err, ErrLegacyState)
}

func TestInitialUTXOsPrefersPersistedState(t *testing.T) {
	st := openStore(t)
	mode := taggedMode(t)
	require.NoError(t, ensureStateMatchesMode(st, mode, false))
	persisted := txbuilder.UTXO{TxHash: seedTxID, Value: 100}
	require.NoError(t, st.SaveUTXO(persisted))

	source := &countingSource{}
	utxos, err := initialUTXOs(t.Context(), st, mode, source, discardLogger())
	require.NoError(t, err)
	assert.Equal(t, []txbuilder.UTXO{persisted}, utxos)
	assert.Zero(t, source.calls.Load(), "WhatsOnChain is not queried when state exists")
}

func TestInitialUTXOsFetchesForFreshState(t *testing.T) {
	st := openStore(t)
	source := &countingSource{utxos: []txbuilder.UTXO{{TxHash: seedTxID, Value: 5}}}
	utxos, err := initialUTXOs(t.Context(), st, taggedMode(t), source, discardLogger())
	require.NoError(t, err)
	assert.Equal(t, source.utxos, utxos)
	assert.Equal(t, int32(1), source.calls.Load())
}

type countingSource struct {
	utxos []txbuilder.UTXO
	calls atomic.Int32
}

func (c *countingSource) FetchUTXOs(context.Context, string) ([]txbuilder.UTXO, error) {
	c.calls.Add(1)
	return c.utxos, nil
}

func TestNewLogger(t *testing.T) {
	var buf bytes.Buffer
	NewLogger(&buf, "debug", "json").Debug("hello", "k", "v")
	assert.Contains(t, buf.String(), `"msg":"hello"`)

	buf.Reset()
	NewLogger(&buf, "WARN", "text").Info("hidden")
	assert.Empty(t, buf.String(), "info is below warn")

	buf.Reset()
	NewLogger(&buf, "nonsense", "anything").Info("shown")
	assert.Contains(t, buf.String(), `"msg":"shown"`, "unknown values fall back to info and json")
}

func TestWaitTimeout(t *testing.T) {
	var wg sync.WaitGroup
	assert.True(t, waitTimeout(safe.Runner{}, &wg, time.Second))

	wg.Add(1)
	assert.False(t, waitTimeout(safe.Runner{}, &wg, 10*time.Millisecond))
	wg.Done()
}
