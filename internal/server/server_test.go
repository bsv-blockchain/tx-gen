package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bsv-blockchain/tx-gen/internal/config"
	"github.com/bsv-blockchain/tx-gen/internal/engine"
	"github.com/bsv-blockchain/tx-gen/internal/metrics"
)

const (
	testToken = "secret"
	testTxID  = "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0"
)

var errUnreachable = errors.New("arcade unreachable")

// fakeEngine is a controllable Engine.
type fakeEngine struct {
	mu        sync.Mutex
	tps       int64
	snapshot  engine.State
	updates   []engine.BootstrapUpdate
	configErr error
}

func (f *fakeEngine) SetTPS(tps int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tps = tps
}

func (f *fakeEngine) Snapshot() engine.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot
}

func (f *fakeEngine) ConfigureBootstrap(u engine.BootstrapUpdate) (engine.BootstrapConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.configErr != nil {
		return engine.BootstrapConfig{}, f.configErr
	}
	f.updates = append(f.updates, u)
	cfg := engine.BootstrapConfig{NumChains: 4, FanoutSize: 2, SustainFee: 7}
	if u.NumChains != nil {
		cfg.NumChains = *u.NumChains
	}
	if u.FanoutSize != nil {
		cfg.FanoutSize = *u.FanoutSize
	}
	if u.SustainFee != nil {
		cfg.SustainFee = *u.SustainFee
	}
	return cfg, nil
}

func (f *fakeEngine) currentTPS() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tps
}

// fakeArcade is a controllable Arcade.
type fakeArcade struct {
	status       int
	body         string
	err          error
	reachableErr error
}

func (f *fakeArcade) TxStatus(context.Context, string) (int, string, error) {
	return f.status, f.body, f.err
}

func (f *fakeArcade) Reachable(context.Context) error { return f.reachableErr }

func newTestServer(t *testing.T, eng Engine, arc Arcade, configure ...func(*config.Config)) *Server {
	t.Helper()
	cfg := &config.Config{
		AdminToken: testToken,
		MaxTPS:     10_000,
		PrivateKey: "private-key-secret",
		InstanceID: "inst01",
	}
	for _, fn := range configure {
		fn(cfg)
	}
	opts := Options{Engine: eng, Config: cfg, Metrics: metrics.New(), Version: "v1.2.3"}
	if arc != nil {
		opts.Arcade = arc
	}
	return New(opts)
}

func do(t *testing.T, s *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func adminJSON() map[string]string {
	return map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json"}
}

func adminOnly() map[string]string {
	return map[string]string{"Authorization": "Bearer " + testToken}
}

func readyState() engine.State {
	return engine.State{Lifecycle: engine.LifecycleRunning, BootstrapStage: engine.StageL2Done, ChainsActive: 4}
}

func TestConfigSetsTPS(t *testing.T) {
	eng := &fakeEngine{}
	s := newTestServer(t, eng, nil)

	rr := do(t, s, http.MethodPost, "/config", `{"tps":16}`, adminJSON())
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.JSONEq(t, `{"tps":16}`, rr.Body.String())
	assert.Equal(t, int64(16), eng.currentTPS())
	assert.Empty(t, eng.updates, "a TPS-only update does not touch bootstrap")
}

func TestConfigUsesConfiguredMaxTPS(t *testing.T) {
	eng := &fakeEngine{}
	s := newTestServer(t, eng, nil, func(c *config.Config) { c.MaxTPS = 5 })

	rr := do(t, s, http.MethodPost, "/config", `{"tps":6}`, adminJSON())
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "tps must be 0-5")
	assert.Zero(t, eng.currentTPS())

	rr = do(t, s, http.MethodPost, "/config", `{"tps":5}`, adminJSON())
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, int64(5), eng.currentTPS())
}

func TestConfigDefaultsMaxTPS(t *testing.T) {
	s := newTestServer(t, &fakeEngine{}, nil, func(c *config.Config) { c.MaxTPS = 0 })
	rr := do(t, s, http.MethodPost, "/config", `{"tps":10001}`, adminJSON())
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "tps must be 0-10000")
}

func TestConfigUpdatesBootstrapParameters(t *testing.T) {
	eng := &fakeEngine{}
	s := newTestServer(t, eng, nil)

	rr := do(t, s, http.MethodPost, "/config", `{"tps":2,"numChains":1200,"fanoutSize":100,"sustainFee":7}`, adminJSON())
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.JSONEq(t, `{"tps":2,"numChains":1200,"fanoutSize":100,"sustainFee":7}`, rr.Body.String())
	assert.Equal(t, int64(2), eng.currentTPS())
	require.Len(t, eng.updates, 1)
	assert.Equal(t, 1200, *eng.updates[0].NumChains)

	s.mu.Lock()
	defer s.mu.Unlock()
	assert.Equal(t, 1200, s.cfg.NumChains, "the reported config follows the update")
	assert.Equal(t, 100, s.cfg.FanoutSize)
	assert.Equal(t, uint64(7), s.cfg.SustainFee)
}

func TestConfigRejectsBootstrapParametersAfterFunding(t *testing.T) {
	eng := &fakeEngine{configErr: engine.ErrBootstrapLocked}
	s := newTestServer(t, eng, nil)

	rr := do(t, s, http.MethodPost, "/config", `{"numChains":1200,"tps":3}`, adminJSON())
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Contains(t, rr.Body.String(), "before initial funding")
	assert.Zero(t, eng.currentTPS(), "nothing is applied when bootstrap is rejected")
}

func TestConfigValidation(t *testing.T) {
	tests := map[string]string{
		"empty object":      `{}`,
		"negative tps":      `{"tps":-1}`,
		"zero chains":       `{"numChains":0}`,
		"zero fanout":       `{"fanoutSize":0}`,
		"zero fee":          `{"sustainFee":0}`,
		"malformed JSON":    `{"tps":`,
		"wrong field type":  `{"tps":"fast"}`,
		"body over the cap": `{"tps":1,"pad":"` + strings.Repeat("x", maxConfigBodyBytes) + `"}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			eng := &fakeEngine{}
			rr := do(t, newTestServer(t, eng, nil), http.MethodPost, "/config", body, adminJSON())
			assert.Equal(t, http.StatusBadRequest, rr.Code)
			assert.Zero(t, eng.currentTPS())
			assert.Empty(t, eng.updates)
		})
	}
}

func TestAdminEndpointsRequireAuth(t *testing.T) {
	s := newTestServer(t, &fakeEngine{}, &fakeArcade{})
	requests := []struct{ method, path string }{
		{http.MethodPost, "/config"},
		{http.MethodPost, "/stop"},
		{http.MethodGet, "/status"},
		{http.MethodGet, "/arcade/tx/" + testTxID},
	}
	for _, r := range requests {
		for _, auth := range []string{"", "Bearer", "Bearer ", "Bearer wrong", "Basic " + testToken, testToken} {
			rr := do(t, s, r.method, r.path, `{"tps":1}`, map[string]string{
				"Authorization": auth, "Content-Type": "application/json",
			})
			assert.Equal(t, http.StatusUnauthorized, rr.Code, "%s %s with %q", r.method, r.path, auth)
		}
	}
}

func TestAuthFailsWithoutAdminToken(t *testing.T) {
	s := newTestServer(t, &fakeEngine{}, nil, func(c *config.Config) { c.AdminToken = "" })
	rr := do(t, s, http.MethodGet, "/status", "", map[string]string{"Authorization": "Bearer "})
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestConfigRequiresJSONContentType(t *testing.T) {
	s := newTestServer(t, &fakeEngine{}, nil)
	for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded", ";;"} {
		rr := do(t, s, http.MethodPost, "/config", `{"tps":1}`, map[string]string{
			"Authorization": "Bearer " + testToken, "Content-Type": contentType,
		})
		assert.Equal(t, http.StatusUnsupportedMediaType, rr.Code, "content type %q", contentType)
	}
	rr := do(t, s, http.MethodPost, "/config", `{"tps":1}`, map[string]string{
		"Authorization": "Bearer " + testToken, "Content-Type": "application/json; charset=utf-8",
	})
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestStopPausesEngine(t *testing.T) {
	eng := &fakeEngine{tps: 16}
	rr := do(t, newTestServer(t, eng, nil), http.MethodPost, "/stop", "", adminJSON())
	require.Equal(t, http.StatusOK, rr.Code)
	assert.JSONEq(t, `{"tps":0}`, rr.Body.String())
	assert.Zero(t, eng.currentTPS())
}

func TestHealthz(t *testing.T) {
	rr := do(t, newTestServer(t, &fakeEngine{}, nil), http.MethodGet, "/healthz", "", nil)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.JSONEq(t, `{"status":"ok"}`, rr.Body.String())
}

func TestReadyz(t *testing.T) {
	notReady := func(mutate func(*engine.State)) engine.State {
		s := readyState()
		mutate(&s)
		return s
	}
	tests := []struct {
		name       string
		state      engine.State
		running    bool
		stopped    bool
		arcade     Arcade
		wantStatus int
		wantCheck  map[string]any
	}{
		{
			name: "ready", state: readyState(), running: true, arcade: &fakeArcade{}, wantStatus: http.StatusOK,
			wantCheck: map[string]any{"engine": "running", "arcade": "reachable", "usableChains": 4.0},
		},
		{
			name: "engine starting", state: readyState(), arcade: &fakeArcade{}, wantStatus: http.StatusServiceUnavailable,
			wantCheck: map[string]any{"engine": "starting"},
		},
		{
			name: "engine stopped unexpectedly", state: readyState(), stopped: true, arcade: &fakeArcade{}, wantStatus: http.StatusServiceUnavailable,
			wantCheck: map[string]any{"engine": "stopped"},
		},
		{
			name: "bootstrap failed", state: notReady(func(s *engine.State) { s.Lifecycle = engine.LifecycleFailed }), running: true,
			arcade: &fakeArcade{}, wantStatus: http.StatusServiceUnavailable, wantCheck: map[string]any{"lifecycle": "failed"},
		},
		{
			name: "bootstrap partial", state: notReady(func(s *engine.State) { s.BootstrapStage = engine.StageL2Partial }), running: true,
			arcade: &fakeArcade{}, wantStatus: http.StatusServiceUnavailable, wantCheck: map[string]any{"bootstrapStage": "l2_partial"},
		},
		{
			name: "no chains", state: notReady(func(s *engine.State) { s.ChainsActive = 0 }), running: true,
			arcade: &fakeArcade{}, wantStatus: http.StatusServiceUnavailable, wantCheck: map[string]any{"usableChains": 0.0},
		},
		{
			name: "broadcast failures", state: notReady(func(s *engine.State) { s.ConsecutiveArcadeFailures = 2 }), running: true,
			arcade: &fakeArcade{}, wantStatus: http.StatusServiceUnavailable, wantCheck: map[string]any{"consecutiveFailures": 2.0},
		},
		{
			name: "arcade unreachable", state: readyState(), running: true, arcade: &fakeArcade{reachableErr: errUnreachable},
			wantStatus: http.StatusServiceUnavailable, wantCheck: map[string]any{"arcade": "arcade unreachable"},
		},
		{
			name: "no arcade client", state: readyState(), running: true, wantStatus: http.StatusServiceUnavailable,
			wantCheck: map[string]any{"arcade": ErrArcadeUnavailable.Error()},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, &fakeEngine{snapshot: tc.state}, tc.arcade)
			if tc.running {
				s.MarkEngineRunning()
			}
			if tc.stopped {
				s.MarkEngineRunning()
				s.MarkEngineStopped(true)
			}

			rr := do(t, s, http.MethodGet, "/readyz", "", nil)
			assert.Equal(t, tc.wantStatus, rr.Code)
			var body struct {
				Ready  bool           `json:"ready"`
				Checks map[string]any `json:"checks"`
			}
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
			assert.Equal(t, tc.wantStatus == http.StatusOK, body.Ready)
			for key, want := range tc.wantCheck {
				assert.Equal(t, want, body.Checks[key], "check %s", key)
			}
			assert.Contains(t, body.Checks, "snapshot")
		})
	}
}

func TestMarkEngineStoppedExpected(t *testing.T) {
	s := newTestServer(t, &fakeEngine{snapshot: readyState()}, &fakeArcade{})
	s.MarkEngineRunning()
	s.MarkEngineStopped(false)
	rr := do(t, s, http.MethodGet, "/readyz", "", nil)
	assert.Contains(t, rr.Body.String(), `"engine":"starting"`, "a clean stop is not reported as a crash")
}

func TestStatusRedactsSecrets(t *testing.T) {
	eng := &fakeEngine{snapshot: readyState()}
	rr := do(t, newTestServer(t, eng, nil), http.MethodGet, "/status", "", adminOnly())
	require.Equal(t, http.StatusOK, rr.Code)

	var body struct {
		Version string         `json:"version"`
		Uptime  string         `json:"uptime"`
		Config  map[string]any `json:"config"`
		Engine  engine.State   `json:"engine"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "v1.2.3", body.Version)
	assert.NotEmpty(t, body.Uptime)
	assert.Equal(t, "***", body.Config["adminToken"])
	assert.Equal(t, "***", body.Config["privateKey"])
	assert.Equal(t, "inst01", body.Config["instanceId"])
	assert.Equal(t, readyState(), body.Engine)
	assert.NotContains(t, rr.Body.String(), "private-key-secret")
	assert.NotContains(t, rr.Body.String(), `"`+testToken+`"`)
}

func TestArcadeTxStatusProxy(t *testing.T) {
	arc := &fakeArcade{status: http.StatusOK, body: `{"txid":"` + testTxID + `","txStatus":"SEEN_ON_NETWORK"}`}
	rr := do(t, newTestServer(t, &fakeEngine{}, arc), http.MethodGet, "/arcade/tx/"+testTxID, "", adminOnly())
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, "application/json", rr.Header().Get("Content-Type"))
	assert.Equal(t, "nosniff", rr.Header().Get("X-Content-Type-Options"))
	assert.JSONEq(t, arc.body, rr.Body.String())
}

func TestArcadeTxStatusPassesThroughPlainText(t *testing.T) {
	arc := &fakeArcade{status: http.StatusNotFound, body: "not found"}
	rr := do(t, newTestServer(t, &fakeEngine{}, arc), http.MethodGet, "/arcade/tx/"+testTxID, "", adminOnly())
	assert.Equal(t, http.StatusNotFound, rr.Code)
	assert.Equal(t, "text/plain; charset=utf-8", rr.Header().Get("Content-Type"))
	assert.Equal(t, "not found", rr.Body.String())
}

func TestArcadeTxStatusErrors(t *testing.T) {
	s := newTestServer(t, &fakeEngine{}, &fakeArcade{err: errUnreachable})
	assert.Equal(t, http.StatusBadRequest, do(t, s, http.MethodGet, "/arcade/tx/not-hex", "", adminOnly()).Code)
	assert.Equal(t, http.StatusBadRequest, do(t, s, http.MethodGet, "/arcade/tx/"+strings.Repeat("z", 64), "", adminOnly()).Code)
	assert.Equal(t, http.StatusBadGateway, do(t, s, http.MethodGet, "/arcade/tx/"+testTxID, "", adminOnly()).Code)

	noArcade := newTestServer(t, &fakeEngine{}, nil)
	assert.Equal(t, http.StatusServiceUnavailable, do(t, noArcade, http.MethodGet, "/arcade/tx/"+testTxID, "", adminOnly()).Code)
}

func TestMetricsEndpoint(t *testing.T) {
	s := newTestServer(t, &fakeEngine{}, nil)
	do(t, s, http.MethodGet, "/status", "", nil) // one auth failure
	rr := do(t, s, http.MethodGet, "/metrics", "", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), "txgen_auth_fail_total 1")
}

func TestListenAndServeAndShutdown(t *testing.T) {
	s := newTestServer(t, &fakeEngine{}, nil)
	require.NoError(t, s.Shutdown(t.Context()), "shutdown before start is a no-op")

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(addr) }()

	require.Eventually(t, func() bool {
		req, reqErr := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/healthz", nil)
		if reqErr != nil {
			return false
		}
		resp, getErr := http.DefaultClient.Do(req)
		if getErr != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 10*time.Millisecond)

	require.NoError(t, s.Shutdown(t.Context()))
	select {
	case err = <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after Shutdown")
	}
}

func TestListenAndServeReportsBindErrors(t *testing.T) {
	err := newTestServer(t, &fakeEngine{}, nil).ListenAndServe("256.0.0.1:bad")
	require.Error(t, err)
}

func TestValidTxID(t *testing.T) {
	assert.True(t, validTxID(testTxID))
	assert.False(t, validTxID(testTxID[:63]))
	assert.False(t, validTxID(strings.Repeat("g", 64)))
	assert.False(t, validTxID(""))
}
