package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Server struct {
	engine     *Engine
	adminToken string
	mux        *http.ServeMux
	cfg        *Config
	store      *Store
	logger     *slog.Logger
	startedAt  time.Time

	mu         sync.Mutex
	httpServer *http.Server

	engineRunning           atomic.Bool
	engineStoppedUnexpected atomic.Bool
}

func newServerWithConfig(engine *Engine, cfg *Config, store *Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		engine:     engine,
		adminToken: cfg.AdminToken,
		mux:        http.NewServeMux(),
		cfg:        cfg,
		store:      store,
		logger:     logger,
		startedAt:  time.Now(),
	}
	s.mux.HandleFunc("POST /config", s.handleConfig)
	s.mux.HandleFunc("POST /stop", s.handleStop)
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.HandleFunc("GET /status", s.handleStatus)
	s.mux.HandleFunc("GET /arcade/tx/{txid}", s.handleArcadeTxStatus)
	RegisterMetrics(s.mux)
	return s
}

func (s *Server) start(addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	s.mu.Lock()
	s.httpServer = srv
	s.mu.Unlock()

	s.logger.Info("server listening", "addr", addr)
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	srv := s.httpServer
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if !s.prepareAuthenticatedPost(w, r) {
		return
	}

	var req struct {
		TPS        *int64  `json:"tps"`
		NumChains  *int    `json:"numChains"`
		FanoutSize *int    `json:"fanoutSize"`
		SustainFee *uint64 `json:"sustainFee"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.TPS == nil && req.NumChains == nil && req.FanoutSize == nil && req.SustainFee == nil {
		http.Error(w, "no config fields supplied", http.StatusBadRequest)
		return
	}

	maxTPS := int64(10000)
	if s.cfg != nil && s.cfg.MaxTPS > 0 {
		maxTPS = int64(s.cfg.MaxTPS)
	}
	if req.TPS != nil && (*req.TPS < 0 || *req.TPS > maxTPS) {
		http.Error(w, "tps must be 0-"+strconv.FormatInt(maxTPS, 10), http.StatusBadRequest)
		return
	}
	if req.NumChains != nil && *req.NumChains <= 0 {
		http.Error(w, "numChains must be positive", http.StatusBadRequest)
		return
	}
	if req.FanoutSize != nil && *req.FanoutSize <= 0 {
		http.Error(w, "fanoutSize must be positive", http.StatusBadRequest)
		return
	}
	if req.SustainFee != nil && *req.SustainFee == 0 {
		http.Error(w, "sustainFee must be positive", http.StatusBadRequest)
		return
	}

	response := map[string]any{}
	if req.NumChains != nil || req.FanoutSize != nil || req.SustainFee != nil {
		cfg, err := s.engine.ConfigureBootstrap(req.NumChains, req.FanoutSize, req.SustainFee)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if s.cfg != nil {
			s.cfg.NumChains = cfg.NumChains
			s.cfg.FanoutSize = cfg.FanoutSize
			s.cfg.SustainFee = cfg.SustainFee
		}
		response["numChains"] = cfg.NumChains
		response["fanoutSize"] = cfg.FanoutSize
		response["sustainFee"] = cfg.SustainFee
		s.logger.Info("bootstrap config updated", "numChains", cfg.NumChains, "fanoutSize", cfg.FanoutSize, "sustainFee", cfg.SustainFee)
	}

	if req.TPS != nil {
		s.engine.SetTPS(*req.TPS)
		SetTPSTarget(int(*req.TPS))
		response["tps"] = *req.TPS
		s.logger.Info("TPS updated", "tps", *req.TPS)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.logger.Warn("write config response", "error", err)
	}
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if !s.prepareAuthenticatedPost(w, r) {
		return
	}
	s.engine.SetTPS(0)
	SetTPSTarget(0)
	s.logger.Info("TPS stopped")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]int64{"tps": 0}); err != nil {
		s.logger.Warn("write stop response", "error", err)
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ready, checks := s.readiness(r.Context())
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"ready":  ready,
		"checks": checks,
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.authFailed(r)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": version,
		"uptime":  time.Since(s.startedAt).String(),
		"config":  redactedConfig(s.cfg),
		"engine":  s.engineStatus(),
	})
}

func (s *Server) handleArcadeTxStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.authFailed(r)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	txid := r.PathValue("txid")
	if !validTxID(txid) {
		http.Error(w, "invalid txid", http.StatusBadRequest)
		return
	}
	if s.engine == nil || s.engine.arcade == nil {
		http.Error(w, "arcade client unavailable", http.StatusServiceUnavailable)
		return
	}
	statusCode, body, err := s.engine.arcade.TxStatus(r.Context(), txid)
	if err != nil {
		s.logger.Warn("arcade tx status", "txid", txid, "error", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if json.Valid([]byte(body)) {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.WriteHeader(statusCode)
	if _, err := w.Write([]byte(body)); err != nil {
		s.logger.Warn("write arcade tx status response", "error", err)
	}
}

func (s *Server) prepareAuthenticatedPost(w http.ResponseWriter, r *http.Request) bool {
	if !s.authorized(r) {
		s.authFailed(r)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "content-type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	return true
}

func (s *Server) authorized(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	if token == "" || s.adminToken == "" {
		return false
	}
	got := sha256.Sum256([]byte(token))
	want := sha256.Sum256([]byte(s.adminToken))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

func validTxID(txid string) bool {
	if len(txid) != 64 {
		return false
	}
	_, err := hex.DecodeString(txid)
	return err == nil
}

func (s *Server) authFailed(r *http.Request) {
	IncAuthFail()
	s.logger.Warn("auth failed", "remote", r.RemoteAddr, "path", r.URL.Path, "method", r.Method)
}

func (s *Server) readiness(ctx context.Context) (bool, map[string]any) {
	checks := map[string]any{}
	ready := true

	if s.engineStoppedUnexpected.Load() {
		checks["engine"] = "stopped"
		ready = false
	} else if !s.engineRunning.Load() {
		checks["engine"] = "starting"
		ready = false
	} else {
		checks["engine"] = "running"
	}

	if snapshot, ok := callSnapshot(s.engine); ok {
		checks["snapshot"] = snapshot
		if !snapshotReady(snapshot, checks) {
			ready = false
		}
	} else if s.engine != nil && s.engine.queue != nil {
		checks["queueDepth"] = s.engine.queue.Len()
	}

	if err := s.arcadeReachable(ctx); err != nil {
		checks["arcade"] = err.Error()
		ready = false
	} else {
		checks["arcade"] = "reachable"
	}

	return ready, checks
}

func (s *Server) engineStatus() any {
	if snapshot, ok := callSnapshot(s.engine); ok {
		return snapshot
	}
	status := map[string]any{
		"running":                s.engineRunning.Load(),
		"stoppedUnexpectedly":    s.engineStoppedUnexpected.Load(),
		"stateSnapshotAvailable": false,
	}
	if s.engine != nil {
		status["tps"] = s.engine.TPS()
		if s.engine.queue != nil {
			status["queueDepth"] = s.engine.queue.Len()
		}
	}
	if stage := s.bootstrapStage(); stage != "" {
		status["bootstrapStage"] = stage
	}
	return status
}

func (s *Server) bootstrapStage() string {
	if s.store == nil {
		return ""
	}
	stage, err := s.store.GetMeta("bootstrap_stage")
	if err != nil {
		s.logger.Warn("read bootstrap stage", "error", err)
		return ""
	}
	return stage
}

func (s *Server) arcadeReachable(ctx context.Context) error {
	if s.engine == nil || s.engine.arcade == nil {
		return errors.New("arcade client unavailable")
	}
	return s.engine.arcade.Reachable(ctx)
}

func (s *Server) markEngineRunning() {
	s.engineRunning.Store(true)
}

func (s *Server) markEngineStopped(unexpected bool) {
	s.engineRunning.Store(false)
	if unexpected {
		s.engineStoppedUnexpected.Store(true)
	}
}

func callSnapshot(engine *Engine) (any, bool) {
	if engine == nil {
		return nil, false
	}
	method := reflect.ValueOf(engine).MethodByName("Snapshot")
	if !method.IsValid() || method.Type().NumIn() != 0 || method.Type().NumOut() != 1 {
		return nil, false
	}
	out := method.Call(nil)
	return out[0].Interface(), true
}

func snapshotReady(snapshot any, checks map[string]any) bool {
	payload, err := json.Marshal(snapshot)
	if err != nil {
		checks["snapshotReady"] = "unreadable"
		return true
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		checks["snapshotReady"] = "unreadable"
		return true
	}

	ready := true
	if lifecycle := firstString(m, "lifecycle", "Lifecycle", "state", "State"); lifecycle != "" {
		checks["lifecycle"] = lifecycle
		switch strings.ToLower(lifecycle) {
		case "failed", "stopping", "stopped":
			ready = false
		}
	}
	if stage := firstString(m, "bootstrapStage", "BootstrapStage", "bootstrap_stage"); stage != "" {
		checks["bootstrapStage"] = stage
		if stage != "l2_done" {
			ready = false
		}
	}
	if chains, ok := firstNumber(m, "usableChains", "activeChainCount", "chainsActive", "ActiveChains"); ok {
		checks["usableChains"] = chains
		if chains <= 0 {
			ready = false
		}
	}
	if failures, ok := firstNumber(m, "consecutiveFailures", "consecutiveArcadeFailures", "ConsecutiveArcadeFailures"); ok {
		checks["consecutiveFailures"] = failures
		if failures > 0 {
			ready = false
		}
	}
	return ready
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok {
			return v
		}
	}
	return ""
}

func firstNumber(m map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		switch v := m[key].(type) {
		case float64:
			return v, true
		case int:
			return float64(v), true
		case int64:
			return float64(v), true
		}
	}
	return 0, false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Default().Warn("write JSON response", "error", err)
	}
}

type configView Config

func redactedConfig(cfg *Config) any {
	if cfg == nil {
		return nil
	}
	redacted := configView(*cfg)
	if redacted.AdminToken != "" {
		redacted.AdminToken = "***"
	}
	if redacted.PrivateKey != "" {
		redacted.PrivateKey = "***"
	}
	if redacted.ArcadeCallbackToken != "" {
		redacted.ArcadeCallbackToken = "***"
	}
	if redacted.SlackWebhookURL != "" {
		redacted.SlackWebhookURL = "***"
	}
	return redacted
}
