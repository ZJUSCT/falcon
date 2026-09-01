package zfsagent

import (
	"encoding/json"
	"net/http"
)

// Server serves the read-only zfs-agent HTTP API:
//
//   - GET /v1/zfs   the node's ZFS usage report
//   - GET /healthz  liveness/readiness probe (always 200 "ok")
//
// Error handling mirrors internal/webapi: JSON bodies {"error": ...} with
// two-space indent, non-GET requests answered 405 with Allow: GET before any
// routing, unknown paths 404.
type Server struct {
	Collector *Collector
}

// Handler builds the http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/zfs", s.handleReport)
	mux.HandleFunc("/healthz", handleHealth)
	// No other routes: everything else is a 404 JSON error.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONError(w, http.StatusNotFound, "not found")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read-only listener: reject anything that is not a plain GET.
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed: this endpoint is read-only (GET)")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	report, err := s.Collector.Report(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, "application/json", report)
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func writeJSON(w http.ResponseWriter, statusCode int, contentType string, payload interface{}) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(statusCode)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

func writeJSONError(w http.ResponseWriter, statusCode int, message string) {
	writeJSON(w, statusCode, "application/json", map[string]string{"error": message})
}
