package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

type Server struct {
	engine     *Engine
	adminToken string
	mux        *http.ServeMux
}

func newServer(engine *Engine, adminToken string) *Server {
	s := &Server{
		engine:     engine,
		adminToken: adminToken,
		mux:        http.NewServeMux(),
	}
	s.mux.HandleFunc("POST /config", s.handleConfig)
	s.mux.HandleFunc("POST /arc-callback", s.handleArcCallback)
	return s
}

func (s *Server) start(addr string) error {
	fmt.Printf("listening on %s\n", addr)
	return http.ListenAndServe(addr, s.mux)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	token := strings.TrimPrefix(auth, "Bearer ")
	if token == "" || token == auth || token != s.adminToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		TPS int64 `json:"tps"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if req.TPS < 0 || req.TPS > 10000 {
		http.Error(w, "tps must be 0–10000", http.StatusBadRequest)
		return
	}

	s.engine.SetTPS(req.TPS)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int64{"tps": req.TPS})
}

// handleArcCallback receives ARC status update callbacks and logs them.
func (s *Server) handleArcCallback(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	log.Printf("ARC callback: %s", body)
	w.WriteHeader(http.StatusOK)
}
