// Package webapi serves public catalog reads and authenticated mirror administration.
package webapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SiteConfig carries the identity of this mirror site used by /mirrorz.json.
type SiteConfig struct {
	// URL is the fallback site base URL without a trailing slash, e.g.
	// https://mirrors.zjusct.io. It backs the mirrorz site section and entry
	// URLs whenever the request Host is not one of the publish hostnames.
	URL string
	// Abbr is the short site name, e.g. ZJU.
	Abbr string
	// Name is the full site name, e.g. "Zhejiang University Mirror".
	Name                                                                        string
	Logo, LogoDarkmode, Homepage, Issue, Request, Email, Group, Disk, Note, Big string
	Disable                                                                     bool
}

// Server serves the HTTP API on top of controller-runtime clients.
type Server struct {
	// Client reads Mirror and ProxyMirror objects. It is expected to be the
	// manager's cached client so the handlers never hit the API server
	// directly (and, with the namespace-scoped cache, only see objects from
	// the controller's own namespace).
	Client client.Reader
	// Writes use live reads and a fixed namespace, independent of the read cache.
	Writer    client.Client
	APIReader client.Reader
	Namespace string
	LogStream func(context.Context, string, string, *corev1.PodLogOptions) (io.ReadCloser, error)
	Site      SiteConfig
	// PublishHostnames is the whitelist of publish hostnames (config
	// publish.hostnames). A request whose Host (port stripped, matched
	// case-insensitively) is on the list gets its mirrorz document reflected
	// with that host; anything else falls back to Site.URL. May be empty.
	PublishHostnames []string
	// MirrorzEnabled gates GET /mirrorz.json (config mirrorz.enabled).
	MirrorzEnabled bool
	// Version is the controller build version reported by GET /api/version.
	// Release builds stamp the git tag via ldflags (see the Dockerfile and
	// the release workflow); unstamped builds report "dev".
	Version string
	// Usage aggregates zfs-agent reports for GET /api/usage. It is nil when
	// the ZFS_AGENT_SERVICE environment variable is unset, which disables
	// the endpoint (404) — there is no config field for it.
	Usage      *UsageAggregator
	Auth       *Authenticator
	UIUpstream string
}

// MirrorzHandler serves the public catalog port: /mirrorz.json only. The
// port structurally cannot expose anything else — no admin route, no /api,
// no OAuth — regardless of routing mistakes upstream; wiring this handler
// into an admin-facing route yields 404s, not leaks.
func (s *Server) MirrorzHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mirrorz.json", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed: this endpoint is read-only (GET)")
			return
		}
		s.handleMirrorZ(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONError(w, http.StatusNotFound, "not found")
	})
	return mux
}

// AdminHandler serves the admin port: /api endpoints, OAuth, the UI
// reverse proxy, and the /api/mirrors mutations. When OAuth is configured
// the ENTIRE surface requires a session (no anonymous reads); without it
// everything answers unauthenticated — debug/local mode. /mirrorz.json
// deliberately lives on the mirrorz port only.
func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/jobs", s.handleJobs)
	mux.HandleFunc("/api/repos", s.handleRepoRoot)
	mux.HandleFunc("/api/repos/", s.handleRepo)
	mux.HandleFunc("/api/usage", s.handleUsage)
	mux.HandleFunc("/api/storage", s.handleStorage)
	mux.HandleFunc("/api/version", s.handleVersion)
	if s.Auth != nil {
		mux.HandleFunc("/oauth/login", s.Auth.login)
		mux.HandleFunc("/oauth/callback", s.Auth.callback)
		mux.HandleFunc("/oauth/session", s.Auth.session)
		mux.HandleFunc("/oauth/logout", s.Auth.logout)
	}
	// No UI is embedded; everything else is a 404 JSON error.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONError(w, http.StatusNotFound, "not found")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The login flow itself must stay reachable pre-session.
		oauth := strings.HasPrefix(r.URL.Path, "/oauth/")
		if s.Auth != nil && !oauth {
			if !s.Auth.require(w, r) {
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/mirrors/") {
			if strings.HasSuffix(r.URL.Path, "/logs") || strings.HasSuffix(r.URL.Path, "/logs/stream") {
				if r.Method != http.MethodGet {
					w.Header().Set("Allow", http.MethodGet)
					writeJSONError(w, 405, "method not allowed")
					return
				}
				s.handleMirrorLogs(w, r)
				return
			}
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", http.MethodPost)
				writeJSONError(w, 405, "method not allowed")
				return
			}
			s.handleMirrorAction(w, r)
			return
		}
		// Non-API paths serve the admin UI (static export) when deployed.
		if !oauth && !strings.HasPrefix(r.URL.Path, "/api/") && s.UIUpstream != "" {
			u, err := url.Parse(s.UIUpstream)
			if err != nil {
				http.Error(w, "invalid UI upstream", http.StatusInternalServerError)
				return
			}
			httputil.NewSingleHostReverseProxy(u).ServeHTTP(w, r)
			return
		}
		// Everything else is read-only: only plain GET passes.
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed: this endpoint is read-only (GET)")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, statusCode int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

func writeJSONError(w http.ResponseWriter, statusCode int, message string) {
	writeJSON(w, statusCode, map[string]string{"error": message})
}

// splitNameExtension splits a repo name requested as /api/repos/<name> into
// the object name and the requested serialization format. The format follows
// the request extension; the default (no extension) is YAML — a deliberate
// change from the legacy /api/repos endpoint which always answered JSON.
func splitNameExtension(path string) (name string, yamlFormat bool) {
	trimmed := strings.TrimPrefix(path, "/api/repos/")
	switch {
	case strings.HasSuffix(trimmed, ".json"):
		return strings.TrimSuffix(trimmed, ".json"), false
	case strings.HasSuffix(trimmed, ".yaml"), strings.HasSuffix(trimmed, ".yml"):
		return strings.TrimSuffix(strings.TrimSuffix(trimmed, ".yaml"), ".yml"), true
	default:
		return trimmed, true
	}
}
