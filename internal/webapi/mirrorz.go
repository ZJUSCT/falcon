package webapi

import (
	"context"
	"fmt"
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
	URL          string `json:"url"`
	Abbr         string `json:"abbr"`
	Name         string `json:"name,omitempty"`
	Logo         string `json:"logo,omitempty"`
	LogoDarkmode string `json:"logo_darkmode,omitempty"`
	Homepage     string `json:"homepage,omitempty"`
	Issue        string `json:"issue,omitempty"`
	Request      string `json:"request,omitempty"`
	Email        string `json:"email,omitempty"`
	Group        string `json:"group,omitempty"`
	Disk         string `json:"disk,omitempty"`
	Note         string `json:"note,omitempty"`
	Big          string `json:"big,omitempty"`
	Disable      bool   `json:"disable,omitempty"`
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
	mirrorzPending      = "D"
	mirrorzSyncing      = "Y"
	mirrorzFailed       = "F"
	mirrorzPaused       = "P"
	mirrorzProxyCache   = "C"
	mirrorzProxyNoCache = "R"
)

// mirrorzStatusBuilder rejects inconsistent persisted state instead of
// publishing bare timestamp-bearing tokens. X alone is optional: no next
// schedule exists while a transaction is in progress or syncing is paused.
type mirrorzStatusBuilder struct {
	value string
	err   error
}

func (b *mirrorzStatusBuilder) require(field string, t *metav1.Time) {
	if b.err == nil && (t == nil || t.Unix() <= 0) {
		b.err = fmt.Errorf("mirrorz timestamp invariant: %s must contain a positive Unix timestamp", field)
	}
}

func (b *mirrorzStatusBuilder) timestamp(token, field string, t *metav1.Time) {
	b.require(field, t)
	if b.err == nil {
		b.value += token + strconv.FormatInt(t.Unix(), 10)
	}
}

func (b *mirrorzStatusBuilder) next(t *metav1.Time) {
	if t != nil {
		b.timestamp("X", "status.nextSyncAt", t)
	}
}

func (b *mirrorzStatusBuilder) result(created metav1.Time) (string, error) {
	b.timestamp("N", "metadata.creationTimestamp", &created)
	if b.err != nil {
		return "", b.err
	}
	return b.value, nil
}

// mirrorzStatusForMirror is only called for eligible HTTP publications. Every
// such Mirror must retain a successful sync completion, including when a later
// sync is queued, running or failed. O and N always carry the recorded times.
func mirrorzStatusForMirror(m *mirrorv1alpha1.Mirror) (string, error) {
	st := &m.Status
	b := mirrorzStatusBuilder{}
	// Validate the publication invariant even in states where O is forbidden.
	b.require("status.lastSuccessfulSyncAt", st.LastSuccessfulSyncAt)
	if st.LastSync == nil || (st.LastSync.Phase != mirrorv1alpha1.SyncPhaseSucceeded && st.LastSync.Phase != mirrorv1alpha1.SyncPhaseFailed && st.LastSync.Phase != mirrorv1alpha1.SyncPhaseCancelled) {
		return "", fmt.Errorf("mirrorz state invariant: eligible publication requires a valid lastSync")
	}
	if m.SyncPaused() && st.PausedAt != nil && (st.CurrentSync == nil || st.CurrentSync.Phase == mirrorv1alpha1.SyncPhasePending) {
		b.timestamp(mirrorzPaused, "status.pausedAt", st.PausedAt)
		return b.result(m.CreationTimestamp)
	}
	if current := st.CurrentSync; current != nil {
		switch current.Phase {
		case mirrorv1alpha1.SyncPhasePending:
			b.timestamp(mirrorzPending, "status.currentSync.queuedAt", current.QueuedAt)
			return b.result(m.CreationTimestamp)
		case mirrorv1alpha1.SyncPhaseCancelling:
			if current.StartedAt == nil {
				b.timestamp(mirrorzPending, "status.currentSync.queuedAt", current.QueuedAt)
				return b.result(m.CreationTimestamp)
			}
			fallthrough
		case mirrorv1alpha1.SyncPhaseRunning:
			b.timestamp(mirrorzSyncing, "status.currentSync.startedAt", current.StartedAt)
			b.timestamp("O", "status.lastSuccessfulSyncAt", st.LastSuccessfulSyncAt)
			return b.result(m.CreationTimestamp)
		case mirrorv1alpha1.SyncPhaseSnapshotting:
			// Snapshotting does not mean the completed Job is still running.
		default:
			return "", fmt.Errorf("mirrorz state invariant: invalid currentSync.phase %q", current.Phase)
		}
	}
	if m.SyncPaused() && st.CurrentSync == nil {
		b.timestamp(mirrorzPaused, "status.pausedAt", st.PausedAt)
		return b.result(m.CreationTimestamp)
	}
	switch {
	case st.LastSync != nil && st.LastSync.Phase == mirrorv1alpha1.SyncPhaseSucceeded:
		b.timestamp(mirrorzSuccess, "status.lastSync.finishedAt", st.LastSync.FinishedAt)
	case st.LastSync != nil && (st.LastSync.Phase == mirrorv1alpha1.SyncPhaseFailed || st.LastSync.Phase == mirrorv1alpha1.SyncPhaseCancelled):
		b.timestamp(mirrorzFailed, "status.lastSync.startedAt", st.LastSync.StartedAt)
		b.timestamp("O", "status.lastSuccessfulSyncAt", st.LastSuccessfulSyncAt)
	default:
		return "", fmt.Errorf("mirrorz state invariant: eligible publication requires a valid lastSync")
	}
	if !m.SyncPaused() {
		b.next(st.NextSyncAt)
	}
	return b.result(m.CreationTimestamp)
}

func mirrorzStatusForProxyMirror(p *mirrorv1alpha1.ProxyMirror) (string, error) {
	b := mirrorzStatusBuilder{value: mirrorzProxyNoCache}
	if p.Spec.Cache != nil {
		b.value = mirrorzProxyCache
	}
	return b.result(p.CreationTimestamp)
}

func readyForCurrentGeneration(conditions []metav1.Condition, generation int64) bool {
	condition := meta.FindStatusCondition(conditions, "Ready")
	return condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == generation
}

func mirrorzCName(configured, name string) string {
	if configured != "" {
		return configured
	}
	return name
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
	if !s.MirrorzEnabled {
		writeJSONError(w, http.StatusNotFound, "mirrorz endpoint is disabled")
		return
	}
	doc, err := s.buildMirrorZ(r.Context(), r.Host)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, doc)
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
		// The catalog contains only an explicitly requested SERVING http
		// endpoint that the controller has declared Ready for this exact CR
		// generation. Sync-only, disabled, stale, unhealthy and redirect-mode
		// endpoints are omitted so MirrorZ consumers are never directed to
		// them — a 302 away from this site is not this site serving.
		if !m.Spec.Publish.HTTP.Serving() || !readyForCurrentGeneration(m.Status.Conditions, m.Generation) {
			continue
		}
		status, err := mirrorzStatusForMirror(m)
		if err != nil {
			return nil, fmt.Errorf("mirrorz catalog: Mirror %s/%s: %w", m.Namespace, m.Name, err)
		}
		entries = append(entries, mirrorzMirror{
			CName:    mirrorzCName(m.Spec.Info.CName, m.Name),
			URL:      entryURL(baseURL, m.Name),
			Status:   status,
			Desc:     m.Spec.Info.Description,
			Upstream: m.Spec.Info.Upstream,
			Size:     mirrorzSize(m.Status.SizeBytes),
		})
	}
	for i := range proxies.Items {
		p := &proxies.Items[i]
		if !p.Spec.Publish.HTTP.Serving() || !readyForCurrentGeneration(p.Status.Conditions, p.Generation) {
			continue
		}
		status, err := mirrorzStatusForProxyMirror(p)
		if err != nil {
			return nil, fmt.Errorf("mirrorz catalog: ProxyMirror %s/%s: %w", p.Namespace, p.Name, err)
		}
		entries = append(entries, mirrorzMirror{
			CName:    mirrorzCName(p.Spec.Info.CName, p.Name),
			URL:      entryURL(baseURL, p.Name),
			Status:   status,
			Desc:     p.Spec.Info.Description,
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
			Logo: s.Site.Logo, LogoDarkmode: s.Site.LogoDarkmode, Homepage: s.Site.Homepage, Issue: s.Site.Issue, Request: s.Site.Request, Email: s.Site.Email, Group: s.Site.Group, Disk: s.Site.Disk, Note: s.Site.Note, Big: s.Site.Big, Disable: s.Site.Disable,
		},
		Info:    []struct{}{},
		Mirrors: entries,
	}, nil
}
