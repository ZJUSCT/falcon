package webapi

import (
	"net/http"
)

// handleVersion answers GET /api/version with the controller build version.
// The UI displays it in the sidebar; the value comes from the build, not the
// config, so it always describes the running binary.
func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": s.Version})
}
