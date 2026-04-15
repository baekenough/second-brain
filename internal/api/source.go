package api

import "net/http"

func (s *Server) listSourcesHandler(w http.ResponseWriter, r *http.Request) {
	counts, err := s.docs.CountBySource(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list sources")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"sources": counts})
}
