package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/guryn/ccproxy/internal/openai"
)

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().Unix()
	data := make([]openai.Model, 0, len(s.rt.Cfg.Models)+3)
	seen := map[string]bool{}
	for _, m := range s.rt.Cfg.Models {
		data = append(data, openai.Model{
			ID:      m.ID,
			Object:  "model",
			Created: now,
			OwnedBy: "ccproxy",
		})
		seen[m.ID] = true
	}
	if s.rt.Cfg.PassthroughEnabled() {
		// Surface the family aliases so OpenAI clients with model pickers can
		// discover them. Full ids (claude-sonnet-4-6 etc.) are not enumerated
		// since they evolve; they still work when sent in the request body.
		for _, family := range []string{"opus", "sonnet", "haiku"} {
			if seen[family] {
				continue
			}
			ownedBy := "claude-passthrough"
			if v, ok := s.rt.Cfg.ModelVersions[family]; ok && v != "" {
				ownedBy = "claude-passthrough → " + v
			}
			data = append(data, openai.Model{
				ID:      family,
				Object:  "model",
				Created: now,
				OwnedBy: ownedBy,
			})
		}
	}
	body := openai.ModelList{Object: "list", Data: data}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
