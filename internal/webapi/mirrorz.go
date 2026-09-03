package webapi

import (
	"context"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

// The MirrorZ data format implemented here is v1.7 (documented as "Data
// Format v1.7" in github.com/mirrorz-org/mirrorz):
//
//	{version?: number, site: Site, info: Info[], mirrors: Mirror[]}
//	Site  {url, abbr, name?, ...}
//	Mirror{cname, url, status, desc?, help?, upstream?, size?, ...}
//
// mirrors[].status is a concat of `[A-Z](\d+)?` tokens: one main status
// (S successful, D pending, Y syncing, F failed, P paused, C reverse proxy
// with cache, R reverse proxy without cache, U unknown) plus any number of
// auxiliary tokens (X next sync, N new mirror, O old success), each carrying
// a unix timestamp where the spec allows one.
const mirrorzVersion = 1.7

type mirrorzSite struct {
	URL  string `json:"url"`
	Abbr string `json:"abbr"`
	Name string `json:"name,omitempty"`
}

type mirrorzMirror struct {
	CName    string `json:"cname"`
	URL      string `json:"url"`
	Status   string `json:"status"`
	Desc     string `json:"desc,omitempty"`
	Upstream string `json:"upstream,omitempty"`
	// Size renders Mirror.status.sizeBytes the way the MirrorZ format defines
	// mirrors[].size: a human-readable string ("596.00G") — the spec (and its
	// frontend schema, src/schema/index.ts) types the field as string, not a
	// byte count. Empty (sizeBytes unknown) omits the field.
	Size string `json:"size,omitempty"`
}

type mirrorzDocument struct {
	Version float64         `json:"version"`
	Site    mirrorzSite     `json:"site"`
	Info    []struct{}      `json:"info"`
	Mirrors []mirrorzMirror `json:"mirrors"`
}

// MirrorZ status letters (main status; see the spec notes above).
const (
	mirrorzSuccess      = "S"
	mirrorzSyncing      = "Y"
	mirrorzFailed       = "F"
	mirrorzPaused       = "P"
	mirrorzProxyCache   = "C"
	mirrorzProxyNoCache = "R"
	mirrorzUnknown      = "U"
)

// tsSuffix renders a unix-seconds suffix for a status token ("" when the
// timestamp is unset — the spec makes the number optional).
func tsSuffix(t *metav1.Time) string {
	if t == nil || t.Unix() <= 0 {
		return ""
	}
	return strconv.FormatInt(t.Unix(), 10)
}

func appendToken(status, token string, timestamp *metav1.Time) string {
	if suffix := tsSuffix(timestamp); suffix != "" {
		return status + token + suffix
	}
	return status
}

func appendCreationToken(status string, created metav1.Time) string {
	if created.IsZero() || created.Unix() <= 0 {
		return status
	}
	return status + "N" + strconv.FormatInt(created.Unix(), 10)
}

// mirrorzStatusForMirror projects synchronization freshness independently of
// endpoint health. buildMirrorZ has already established Ready=True before it
// calls this function. Token order is main, O, X, N.
func mirrorzStatusForMirror(m *mirrorv1alpha1.Mirror) string {
	st := &m.Status
	if st.CurrentSync != nil {
		status := mirrorzSyncing + tsSuffix(st.CurrentSync.StartedAt)
		status = appendToken(status, "O", st.LastPublishedAt)
		return appendCreationToken(status, m.CreationTimestamp)
	}
	if m.Spec.Paused {
		// mirrorz-monitor uses the P timestamp as a freshness fallback. Report
		// the immutable publication's activation time so a paused but still
		// usable endpoint is judged by the content it actually serves.
		status := mirrorzPaused + tsSuffix(st.LastPublishedAt)
		return appendCreationToken(status, m.CreationTimestamp)
	}
	if st.LastSync != nil && st.LastSync.Phase == mirrorv1alpha1.SyncPhaseSucceeded {
		status := mirrorzSuccess + tsSuffix(st.LastPublishedAt)
		status = appendToken(status, "X", st.NextSyncAt)
		return appendCreationToken(status, m.CreationTimestamp)
	}
	if st.LastSync != nil && st.LastSync.Phase == mirrorv1alpha1.SyncPhaseFailed {
		status := mirrorzFailed + tsSuffix(st.LastSync.FinishedAt)
		status = appendToken(status, "O", st.LastPublishedAt)
		status = appendToken(status, "X", st.NextSyncAt)
		return appendCreationToken(status, m.CreationTimestamp)
	}
	return appendCreationToken(mirrorzUnknown, m.CreationTimestamp)
}

// mirrorzStatusForProxyMirror builds the status field of an included proxy.
// Non-ready proxies are filtered out before this projection.
func mirrorzStatusForProxyMirror(p *mirrorv1alpha1.ProxyMirror) string {
	status := mirrorzProxyNoCache
	if p.Spec.Proxy.Cache.Enabled != nil && *p.Spec.Proxy.Cache.Enabled {
		status = mirrorzProxyCache
	}
	return appendCreationToken(status, p.CreationTimestamp)
}

func readyForCurrentGeneration(conditions []metav1.Condition, generation int64) bool {
	condition := meta.FindStatusCondition(conditions, "Ready")
	return condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == generation
}

// pickDescription picks the display description from a LocalizedString:
// zh first, en as fallback, everything else ignored.
func pickDescription(desc map[string]string) string {
	if v, ok := desc["zh"]; ok && v != "" {
		return v
	}
	return desc["en"]
}

// hostOnly strips the port from a Host header value ("mirrors.zjusct.io:8443"
// -> "mirrors.zjusct.io"). A value without a port is returned unchanged.
func hostOnly(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return host
	}
	return hostport
}

// reflectedHost matches the request Host (port stripped, case-insensitive)
// against the publish hostname whitelist. It returns the matched (canonical,
// lowercased request) host and true on a hit.
func (s *Server) reflectedHost(requestHost string) (string, bool) {
	host := strings.ToLower(hostOnly(requestHost))
	if host == "" {
		return "", false
	}
	for _, allowed := range s.PublishHostnames {
		if strings.ToLower(allowed) == host {
			return host, true
		}
	}
	return "", false
}

// siteURLForRequest picks the site base URL for the request: the reflected
// request host on a publish-hostname hit, the configured site URL otherwise.
func (s *Server) siteURLForRequest(requestHost string) string {
	if host, ok := s.reflectedHost(requestHost); ok {
		// Preserve the scheme of the configured site URL; the host comes from
		// the request.
		scheme := "https"
		if i := strings.Index(s.Site.URL, "://"); i > 0 {
			scheme = s.Site.URL[:i]
		}
		return scheme + "://" + host
	}
	return strings.TrimRight(s.Site.URL, "/")
}

// entryURL builds a mirror entry URL: base URL + the CR name. The public path
// of every mirror is its CR name, so the URL is always <base>/<name> without
// a trailing slash (as the MirrorZ spec wants).
func entryURL(baseURL, name string) string {
	return strings.TrimRight(baseURL, "/") + "/" + name
}

// mirrorzSize renders a byte count as the human-readable string the MirrorZ
// format expects for mirrors[].size (the spec shows "size": "596G" and its
// frontend schema types the field as string — it is not a byte integer).
// Units are binary (1024-based) with two decimals; a zero (unknown) size
// renders as "" so the field is omitted from the JSON entry.
func mirrorzSize(sizeBytes int64) string {
	if sizeBytes <= 0 {
		return ""
	}
	units := []string{"B", "K", "M", "G", "T", "P", "E"}
	value := float64(sizeBytes)
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	return strconv.FormatFloat(value, 'f', 2, 64) + units[unit]
}

func (s *Server) handleMirrorZ(w http.ResponseWriter, r *http.Request) {
	if !s.CatalogEnabled {
		writeJSONError(w, http.StatusNotFound, "mirrorz catalog is disabled")
		return
	}
	doc, err := s.buildMirrorZ(r.Context(), r.Host)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, "application/json", doc)
}

// listAll lists every Mirror and ProxyMirror across all namespaces.
func (s *Server) listAll(ctx context.Context) (*mirrorv1alpha1.MirrorList, *mirrorv1alpha1.ProxyMirrorList, error) {
	var mirrors mirrorv1alpha1.MirrorList
	if err := s.Client.List(ctx, &mirrors); err != nil {
		return nil, nil, err
	}
	var proxies mirrorv1alpha1.ProxyMirrorList
	if err := s.Client.List(ctx, &proxies); err != nil {
		return nil, nil, err
	}
	return &mirrors, &proxies, nil
}

// buildMirrorZ assembles the catalog. requestHost is the raw Host header of
// the HTTP request (may be empty in tests): when it matches one of the
// publish hostnames, the site section and every entry URL are reflected with
// that host, otherwise the configured site URL is used.
func (s *Server) buildMirrorZ(ctx context.Context, requestHost string) (*mirrorzDocument, error) {
	mirrors, proxies, err := s.listAll(ctx)
	if err != nil {
		return nil, err
	}
	baseURL := s.siteURLForRequest(requestHost)

	entries := make([]mirrorzMirror, 0, len(mirrors.Items)+len(proxies.Items))
	for i := range mirrors.Items {
		m := &mirrors.Items[i]
		// The catalog contains only an explicitly requested HTTP endpoint
		// that the controller has declared Ready for this exact CR
		// generation. Sync-only, disabled, stale and unhealthy endpoints are
		// omitted so MirrorZ consumers are never directed to them.
		if m.Spec.Services.HTTP == nil || !readyForCurrentGeneration(m.Status.Conditions, m.Generation) {
			continue
		}
		entries = append(entries, mirrorzMirror{
			CName:    m.Name,
			URL:      entryURL(baseURL, m.Name),
			Status:   mirrorzStatusForMirror(m),
			Desc:     pickDescription(m.Spec.Info.Description),
			Upstream: m.Spec.Info.Upstream,
			Size:     mirrorzSize(m.Status.SizeBytes),
		})
	}
	for i := range proxies.Items {
		p := &proxies.Items[i]
		if p.Spec.Services.HTTP == nil || !readyForCurrentGeneration(p.Status.Conditions, p.Generation) {
			continue
		}
		entries = append(entries, mirrorzMirror{
			CName:    p.Name,
			URL:      entryURL(baseURL, p.Name),
			Status:   mirrorzStatusForProxyMirror(p),
			Desc:     pickDescription(p.Spec.Info.Description),
			Upstream: p.Spec.Info.Upstream,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].CName < entries[j].CName })

	return &mirrorzDocument{
		Version: mirrorzVersion,
		Site: mirrorzSite{
			URL:  baseURL,
			Abbr: s.Site.Abbr,
			Name: s.Site.Name,
		},
		Info:    []struct{}{},
		Mirrors: entries,
	}, nil
}
