// Package server is the tx-gen HTTP API: health and readiness probes, the
// engine status, Prometheus metrics, and the authenticated admin endpoints
// that set the target TPS and the bootstrap parameters.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/tx-gen/internal/config"
	"github.com/bsv-blockchain/tx-gen/internal/engine"
	"github.com/bsv-blockchain/tx-gen/internal/metrics"
)

const (
	// maxConfigBodyBytes caps the size of an admin request body.
	maxConfigBodyBytes = 1024

	// readHeaderTimeout bounds reading request headers.
	readHeaderTimeout = 5 * time.Second

	// idleTimeout closes idle keep-alive connections.
	idleTimeout = 120 * time.Second

	// txidHexLength is the length of a hex transaction id.
	txidHexLength = 64
)

// ErrArcadeUnavailable is reported by /readyz when no Arcade client is set.
var ErrArcadeUnavailable = errors.New("arcade client unavailable")

// Engine is the part of the engine the API controls.
type Engine interface {
	// SetTPS sets the target transactions per second.
	SetTPS(tps int64)

	// Snapshot returns the current engine state.
	Snapshot() engine.State

	// ConfigureBootstrap changes bootstrap parameters before funding.
	ConfigureBootstrap(update engine.BootstrapUpdate) (engine.BootstrapConfig, error)
}

// Arcade is the part of the Arcade client the API uses.
type Arcade interface {
	// TxStatus returns Arcade's status code and body for txid.
	TxStatus(ctx context.Context, txid string) (int, string, error)

	// Reachable reports whether Arcade is healthy.
	Reachable(ctx context.Context) error
}

// Options configures a Server. Engine and Config are required.
type Options struct {
	// Engine is the engine the admin endpoints control.
	Engine Engine

	// Arcade is used by /readyz and /arcade/tx/{txid}. It may be nil, in
	// which case the server is never ready.
	Arcade Arcade

	// Config is the runtime configuration. The server keeps a copy, updated
	// when POST /config changes the bootstrap parameters.
	Config *config.Config

	// Metrics is served on /metrics and receives auth failures. It may be nil.
	Metrics *metrics.Metrics

	// Logger receives request logs; it defaults to slog.Default.
	Logger *slog.Logger

	// Version is reported by /status.
	Version string
}

// Server serves the tx-gen HTTP API.
type Server struct {
	engine    Engine
	arcade    Arcade
	metrics   *metrics.Metrics
	logger    *slog.Logger
	version   string
	startedAt time.Time
	mux       *http.ServeMux

	mu         sync.Mutex
	cfg        config.Config
	httpServer *http.Server

	engineRunning           atomic.Bool
	engineStoppedUnexpected atomic.Bool
}

// New returns a server for opts with every route registered.
func New(opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		engine:    opts.Engine,
		arcade:    opts.Arcade,
		metrics:   opts.Metrics,
		logger:    logger,
		version:   opts.Version,
		startedAt: time.Now(),
		mux:       http.NewServeMux(),
		cfg:       *opts.Config,
	}
	s.mux.HandleFunc("POST /config", s.handleConfig)
	s.mux.HandleFunc("POST /stop", s.handleStop)
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.HandleFunc("GET /status", s.handleStatus)
	s.mux.HandleFunc("GET /arcade/tx/{txid}", s.handleArcadeTxStatus)
	s.mux.Handle("GET /metrics", opts.Metrics.Handler())
	return s
}

// Handler returns the API's HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// ListenAndServe serves the API on addr until Shutdown is called, after
// which it returns nil.
func (s *Server) ListenAndServe(addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.mux,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
	s.mu.Lock()
	s.httpServer = srv
	s.mu.Unlock()

	s.logger.Info("server listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	return nil
}

// Shutdown gracefully stops the server, waiting for active requests until
// ctx ends.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	srv := s.httpServer
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// MarkEngineRunning records that the engine goroutine has started.
func (s *Server) MarkEngineRunning() {
	s.engineRunning.Store(true)
}

// MarkEngineStopped records that the engine goroutine has returned;
// unexpected means it returned before shutdown was requested.
func (s *Server) MarkEngineStopped(unexpected bool) {
	s.engineRunning.Store(false)
	if unexpected {
		s.engineStoppedUnexpected.Store(true)
	}
}

// configRequest is the POST /config body. Every field is optional, but at
// least one must be set.
type configRequest struct {
	TPS        *int64  `json:"tps"`
	NumChains  *int    `json:"numChains"`
	FanoutSize *int    `json:"fanoutSize"`
	SustainFee *uint64 `json:"sustainFee"`
}

// handleConfig sets the target TPS and, before funding, the bootstrap
// parameters.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if !s.prepareAuthenticatedPost(w, r) {
		return
	}

	var req configRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if msg := s.validate(req); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	response := map[string]any{}
	if req.NumChains != nil || req.FanoutSize != nil || req.SustainFee != nil {
		cfg, err := s.engine.ConfigureBootstrap(engine.BootstrapUpdate{
			NumChains:  req.NumChains,
			FanoutSize: req.FanoutSize,
			SustainFee: req.SustainFee,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		s.mu.Lock()
		s.cfg.NumChains, s.cfg.FanoutSize, s.cfg.SustainFee = cfg.NumChains, cfg.FanoutSize, cfg.SustainFee
		s.mu.Unlock()
		response["numChains"] = cfg.NumChains
		response["fanoutSize"] = cfg.FanoutSize
		response["sustainFee"] = cfg.SustainFee
		s.logger.Info("bootstrap config updated",
			"numChains", cfg.NumChains, "fanoutSize", cfg.FanoutSize, "sustainFee", cfg.SustainFee)
	}
	if req.TPS != nil {
		s.engine.SetTPS(*req.TPS)
		response["tps"] = *req.TPS
		s.logger.Info("TPS updated", "tps", *req.TPS)
	}
	s.writeJSON(w, http.StatusOK, response)
}

// validate returns a client error message for req, or "" when it is valid.
func (s *Server) validate(req configRequest) string {
	s.mu.Lock()
	maxTPS := int64(s.cfg.MaxTPS)
	s.mu.Unlock()
	if maxTPS <= 0 {
		maxTPS = config.DefaultMaxTPS
	}
	switch {
	case req.TPS == nil && req.NumChains == nil && req.FanoutSize == nil && req.SustainFee == nil:
		return "no config fields supplied"
	case req.TPS != nil && (*req.TPS < 0 || *req.TPS > maxTPS):
		return fmt.Sprintf("tps must be 0-%d", maxTPS)
	case req.NumChains != nil && *req.NumChains <= 0:
		return "numChains must be positive"
	case req.FanoutSize != nil && *req.FanoutSize <= 0:
		return "fanoutSize must be positive"
	case req.SustainFee != nil && *req.SustainFee == 0:
		return "sustainFee must be positive"
	default:
		return ""
	}
}

// handleStop pauses every chain; it is shorthand for POST /config {"tps":0}.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if !s.prepareAuthenticatedPost(w, r) {
		return
	}
	s.engine.SetTPS(0)
	s.logger.Info("TPS stopped")
	s.writeJSON(w, http.StatusOK, map[string]int64{"tps": 0})
}

// handleHealthz reports liveness: the process is up and serving.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz reports readiness: 200 only while chains are running
// healthily, 503 otherwise, with the individual checks in the body.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ready, checks := s.readiness(r.Context())
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	s.writeJSON(w, status, map[string]any{"ready": ready, "checks": checks})
}

// handleStatus returns the engine snapshot and the redacted configuration.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	s.mu.Lock()
	cfg := s.cfg.Redacted()
	s.mu.Unlock()
	s.writeJSON(w, http.StatusOK, map[string]any{
		"version": s.version,
		"uptime":  time.Since(s.startedAt).String(),
		"config":  &cfg,
		"engine":  s.engine.Snapshot(),
	})
}

// handleArcadeTxStatus proxies Arcade's status for one transaction.
func (s *Server) handleArcadeTxStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	txid := r.PathValue("txid")
	if !validTxID(txid) {
		http.Error(w, "invalid txid", http.StatusBadRequest)
		return
	}
	if s.arcade == nil {
		http.Error(w, ErrArcadeUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	status, body, err := s.arcade.TxStatus(r.Context(), txid)
	if err != nil {
		s.logger.Warn("arcade tx status", "txid", txid, "error", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// Arcade's body is served as JSON or plain text, never sniffed as HTML.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if json.Valid([]byte(body)) {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.WriteHeader(status)
	if _, err = w.Write([]byte(body)); err != nil { //nolint:gosec // G705: non-HTML content type with nosniff
		s.logger.Warn("write arcade tx status response", "error", err)
	}
}

// readiness evaluates every readiness check.
func (s *Server) readiness(ctx context.Context) (bool, map[string]any) {
	checks := map[string]any{}
	ready := true

	switch {
	case s.engineStoppedUnexpected.Load():
		checks["engine"] = "stopped"
		ready = false
	case !s.engineRunning.Load():
		checks["engine"] = "starting"
		ready = false
	default:
		checks["engine"] = "running"
	}

	snapshot := s.engine.Snapshot()
	checks["snapshot"] = snapshot
	checks["lifecycle"] = snapshot.Lifecycle
	checks["bootstrapStage"] = snapshot.BootstrapStage
	checks["usableChains"] = snapshot.ChainsActive
	checks["consecutiveFailures"] = snapshot.ConsecutiveArcadeFailures
	stopped := snapshot.Lifecycle == engine.LifecycleFailed ||
		snapshot.Lifecycle == engine.LifecycleStopping ||
		snapshot.Lifecycle == engine.LifecycleStopped
	if stopped ||
		snapshot.BootstrapStage != engine.StageL2Done ||
		snapshot.ChainsActive <= 0 ||
		snapshot.ConsecutiveArcadeFailures > 0 {
		ready = false
	}

	if s.arcade == nil {
		checks["arcade"] = ErrArcadeUnavailable.Error()
		ready = false
	} else if err := s.arcade.Reachable(ctx); err != nil {
		checks["arcade"] = err.Error()
		ready = false
	} else {
		checks["arcade"] = "reachable"
	}
	return ready, checks
}

// prepareAuthenticatedPost authorizes r, caps its body, and requires a JSON
// content type. It writes the error response and returns false on failure.
func (s *Server) prepareAuthenticatedPost(w http.ResponseWriter, r *http.Request) bool {
	if !s.authorize(w, r) {
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxConfigBodyBytes)
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "content-type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	return true
}

// authorize checks the bearer token, writing 401 Unauthorized on failure.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	if s.authorized(r) {
		return true
	}
	s.metrics.IncAuthFail()
	s.logger.Warn("auth failed", "remote", r.RemoteAddr, "path", r.URL.Path, "method", r.Method)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

// authorized compares the bearer token with ADMIN_TOKEN in constant time.
func (s *Server) authorized(r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	adminToken := s.cfg.AdminToken
	s.mu.Unlock()
	if !ok || token == "" || adminToken == "" {
		return false
	}
	got := sha256.Sum256([]byte(token))
	want := sha256.Sum256([]byte(adminToken))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// writeJSON writes v as a JSON response with status.
func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.logger.Warn("write JSON response", "error", err)
	}
}

// validTxID reports whether txid is 64 hex characters.
func validTxID(txid string) bool {
	if len(txid) != txidHexLength {
		return false
	}
	_, err := hex.DecodeString(txid)
	return err == nil
}
