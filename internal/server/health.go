package server

import (
	"encoding/json"
	"net/http"
)

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	v, ready, _ := s.versionString()
	body := map[string]any{
		"status":         "ok",
		"claude_version": v,
		"claude_ready":   ready,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	v, ready, errMsg := s.versionString()
	w.Header().Set("Content-Type", "application/json")
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "not_ready",
			"error":  errMsg,
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":         "ready",
		"claude_version": v,
	})
}
