package api

import (
	"context"
	"net/http"
)

// sourceStatusEntry is the JSON representation of a single collector.
type sourceStatusEntry struct {
	Name    string `json:"name"`
	Source  string `json:"source"`
	Enabled bool   `json:"enabled"`
}

// listSourcesHandler handles GET /api/v1/sources.
func (s *Server) listSourcesHandler(w http.ResponseWriter, r *http.Request) {
	var entries []sourceStatusEntry
	for _, c := range s.scheduler.Collectors() {
		entries = append(entries, sourceStatusEntry{
			Name:    c.Name(),
			Source:  string(c.Source()),
			Enabled: c.Enabled(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"sources": entries})
}

// triggerCollectHandler handles POST /api/v1/collect/trigger.
// It triggers all enabled collectors asynchronously.
func (s *Server) triggerCollectHandler(w http.ResponseWriter, r *http.Request) {
	s.scheduler.TriggerAll(context.Background())
	writeJSON(w, http.StatusAccepted, map[string]string{
		"status": "collection triggered",
	})
}
