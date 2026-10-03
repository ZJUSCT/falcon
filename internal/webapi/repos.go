package webapi

import (
	"net/http"
	"strings"

	"sigs.k8s.io/yaml"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

// handleRepo serves GET /api/repos/<name> (optionally /api/repos/<name>.json).
//
// The legacy Docker scheduler exposed repo configurations as JSON via
// GET /api/repos (whole list, shared.Repo shape). This controller instead
// answers a single repo per request with the CR's **spec only** — status is
// intentionally never exposed here — and defaults to YAML, with the extension
// of the request selecting the serialization (see splitNameExtension).
//
//   - 200: exactly one Mirror matches <name> across all namespaces; the body
//     is its spec.
//   - 404: no object with that name exists.
//   - 409: the name is ambiguous — more than one Mirror matches in different
//     namespaces.
func (s *Server) handleRepo(w http.ResponseWriter, r *http.Request) {
	name, asYAML := splitNameExtension(r.URL.Path)
	name = strings.TrimSpace(name)
	if name == "" {
		writeJSONError(w, http.StatusNotFound, "missing repo name: use /api/repos/<name>")
		return
	}

	matches, err := s.lookupSpecs(r, name)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	switch len(matches) {
	case 0:
		writeJSONError(w, http.StatusNotFound, "repo not found: "+name)
	case 1:
		s.writeSpec(w, asYAML, matches[0])
	default:
		writeJSONError(w, http.StatusConflict, "ambiguous repo name: "+name)
	}
}

// handleRepoRoot answers GET /api/repos without a name; the legacy endpoint
// listed all repos, this one only documents the per-name lookup.
func (s *Server) handleRepoRoot(w http.ResponseWriter, _ *http.Request) {
	writeJSONError(w, http.StatusNotFound, "missing repo name: use /api/repos/<name>")
}

func (s *Server) lookupSpecs(r *http.Request, name string) ([]interface{}, error) {
	var mirrors mirrorv1alpha1.MirrorList
	if err := s.Client.List(r.Context(), &mirrors); err != nil {
		return nil, err
	}

	var specs []interface{}
	for i := range mirrors.Items {
		if mirrors.Items[i].Name == name {
			specs = append(specs, &mirrors.Items[i].Spec)
		}
	}

	return specs, nil
}

func (s *Server) writeSpec(w http.ResponseWriter, asYAML bool, spec interface{}) {
	if asYAML {
		data, err := yaml.Marshal(spec)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/x-yaml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}
	writeJSON(w, http.StatusOK, spec)
}
