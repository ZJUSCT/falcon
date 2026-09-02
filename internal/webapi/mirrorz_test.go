package webapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

// localized builds a LocalizedString for tests.
func localized(zh, en string) mirrorv1alpha1.LocalizedString {
	return mirrorv1alpha1.LocalizedString{"zh": zh, "en": en}
}

// parseMirrorZBody decodes a served mirrorz.json body.
func parseMirrorZBody(body []byte) (map[string]any, error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func TestMirrorzStatusForMirrorPhase(t *testing.T) {
	cases := map[string]string{
		mirrorv1alpha1.PhaseReady:        "U",
		mirrorv1alpha1.PhaseSyncing:      "S",
		mirrorv1alpha1.PhasePublishing:   "S",
		mirrorv1alpha1.PhaseInitializing: "S",
		mirrorv1alpha1.PhasePaused:       "P",
		mirrorv1alpha1.PhaseDegraded:     "D",
		mirrorv1alpha1.PhasePending:      "D",
		"":                               "D",
	}
	for phase, want := range cases {
		if got := mirrorzStatusForMirrorPhase(phase); got != want {
			t.Errorf("mirrorzStatusForMirrorPhase(%q) = %q, want %q", phase, got, want)
		}
	}
}

func TestMirrorzStatusForProxyMirrorPhase(t *testing.T) {
	cases := map[string]string{
		mirrorv1alpha1.PhaseReady:    "U",
		mirrorv1alpha1.PhaseDegraded: "D",
		mirrorv1alpha1.PhasePending:  "S",
		"":                           "S",
	}
	for phase, want := range cases {
		if got := mirrorzStatusForProxyMirrorPhase(phase); got != want {
			t.Errorf("mirrorzStatusForProxyMirrorPhase(%q) = %q, want %q", phase, got, want)
		}
	}
}

func TestHostOnlyStripsPort(t *testing.T) {
	cases := map[string]string{
		"mirrors.zjusct.io":      "mirrors.zjusct.io",
		"mirrors.zjusct.io:8443": "mirrors.zjusct.io",
		"[2001:db8::1]:443":      "2001:db8::1",
		"2001:db8::1":            "2001:db8::1", // no port: unchanged
		"":                       "",
	}
	for in, want := range cases {
		if got := hostOnly(in); got != want {
			t.Errorf("hostOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

// httpService is the enabled http spec.services key the catalog fixtures
// declare (a Mirror with every service disabled is sync-only and must be
// omitted).
func httpService() mirrorv1alpha1.MirrorServicesSpec {
	return mirrorv1alpha1.MirrorServicesSpec{
		HTTP: mirrorv1alpha1.MirrorHTTPServiceSpec{
			MirrorServiceSpec: mirrorv1alpha1.MirrorServiceSpec{
				Enable: true,
				PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "web",
						Image: "nginxinc/nginx-unprivileged:1.31.0-alpine",
						Ports: []corev1.ContainerPort{{Name: "web", ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
					}},
				}},
			},
		},
	}
}

// mirrorzTestServer builds a Server over the standard fixtures with the
// given serving hostname whitelist.
func mirrorzTestServer(t *testing.T, hostnames []string) *Server {
	t.Helper()
	// Published and served, with a known on-disk usage.
	published := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "debian", Namespace: "mirrors"},
		Spec: mirrorv1alpha1.MirrorSpec{
			Info: mirrorv1alpha1.MirrorInfo{
				Description: localized("Debian 发行版软件包镜像", "Debian archive mirror"),
				Upstream:    "rsync://ftp.debian.org/debian/",
			},
			Services: httpService(),
		},
		Status: mirrorv1alpha1.MirrorStatus{Phase: mirrorv1alpha1.PhaseReady, ActivePVC: "debian-sync-1", SizeBytes: 640141257728},
	}
	// Never published (no activePVC): must be omitted even though Ready.
	neverPublished := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "ubuntu", Namespace: "mirrors"},
		Spec: mirrorv1alpha1.MirrorSpec{
			Info:     mirrorv1alpha1.MirrorInfo{Upstream: "rsync://archive.ubuntu.com/ubuntu/"},
			Services: httpService(),
		},
		Status: mirrorv1alpha1.MirrorStatus{Phase: mirrorv1alpha1.PhaseSyncing},
	}
	// Sync-only (activePVC set but no services): published content exists,
	// but there is no publish service to access it with — omitted.
	syncOnly := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "alpine", Namespace: "mirrors"},
		Status:     mirrorv1alpha1.MirrorStatus{Phase: mirrorv1alpha1.PhaseReady, ActivePVC: "alpine-sync-3"},
	}
	syncing := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "arch", Namespace: "mirrors"},
		Spec:       mirrorv1alpha1.MirrorSpec{Services: httpService()},
		Status:     mirrorv1alpha1.MirrorStatus{Phase: mirrorv1alpha1.PhaseSyncing, ActivePVC: "arch-sync-2"},
	}
	proxy := &mirrorv1alpha1.ProxyMirror{
		ObjectMeta: metav1.ObjectMeta{Name: "pypi-proxy", Namespace: "mirrors"},
		Spec: mirrorv1alpha1.ProxyMirrorSpec{
			Info: mirrorv1alpha1.ProxyMirrorInfo{
				Description: localized("PyPI 缓存代理", "Caching proxy for PyPI"),
				Upstream:    "https://pypi.org/simple/",
			},
		},
		Status: mirrorv1alpha1.ProxyMirrorStatus{Phase: mirrorv1alpha1.PhaseReady},
	}

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(published, neverPublished, syncOnly, syncing, proxy).Build()
	return &Server{
		Client:           c,
		Site:             SiteConfig{URL: "https://mirrors.zjusct.io", Abbr: "ZJU", Name: "Zhejiang University Mirror"},
		ServingHostnames: hostnames,
		CatalogEnabled:   true,
	}
}

func TestBuildMirrorZDocument(t *testing.T) {
	s := mirrorzTestServer(t, nil)

	doc, err := s.buildMirrorZ(t.Context(), "internal.host.example")
	if err != nil {
		t.Fatalf("buildMirrorZ: %v", err)
	}
	if doc.Version != 1.7 {
		t.Errorf("version = %v, want 1.7", doc.Version)
	}
	if doc.Site.URL != "https://mirrors.zjusct.io" || doc.Site.Abbr != "ZJU" || doc.Site.Name != "Zhejiang University Mirror" {
		t.Errorf("unexpected site: %+v", doc.Site)
	}
	if doc.Mirrors == nil || len(doc.Mirrors) != 3 {
		t.Fatalf("got %d mirrors (%v), want 3 (never-published ubuntu and sync-only alpine omitted)", len(doc.Mirrors), doc.Mirrors)
	}
	if doc.Mirrors[0].CName != "arch" || doc.Mirrors[0].Status != "S" {
		t.Errorf("arch entry wrong: %+v", doc.Mirrors[0])
	}
	// Unknown usage (sizeBytes unset) omits the size field.
	if doc.Mirrors[0].Size != "" {
		t.Errorf("arch size = %q, want omitted", doc.Mirrors[0].Size)
	}
	// Entry URL = <site url>/<CR name> — no trailing slash, no per-CR url field.
	if doc.Mirrors[0].URL != "https://mirrors.zjusct.io/arch" {
		t.Errorf("arch url = %q", doc.Mirrors[0].URL)
	}
	if doc.Mirrors[1].CName != "debian" || doc.Mirrors[1].Status != "U" {
		t.Errorf("debian entry wrong: %+v", doc.Mirrors[1])
	}
	// sizeBytes is rendered as the human-readable string the MirrorZ format
	// defines for mirrors[].size (binary units, two decimals).
	if doc.Mirrors[1].Size != "596.18G" {
		t.Errorf("debian size = %q, want \"596.18G\"", doc.Mirrors[1].Size)
	}
	if doc.Mirrors[1].URL != "https://mirrors.zjusct.io/debian" {
		t.Errorf("debian url = %q", doc.Mirrors[1].URL)
	}
	if doc.Mirrors[1].Desc != "Debian 发行版软件包镜像" {
		t.Errorf("debian desc = %q, want zh description", doc.Mirrors[1].Desc)
	}
	if doc.Mirrors[1].Upstream != "rsync://ftp.debian.org/debian/" {
		t.Errorf("debian upstream = %q", doc.Mirrors[1].Upstream)
	}
	if doc.Mirrors[2].CName != "pypi-proxy" || doc.Mirrors[2].Status != "U" {
		t.Errorf("pypi-proxy entry wrong: %+v", doc.Mirrors[2])
	}
	if doc.Mirrors[2].URL != "https://mirrors.zjusct.io/pypi-proxy" {
		t.Errorf("pypi-proxy url = %q", doc.Mirrors[2].URL)
	}

	// Helper-level edge cases, pinned next to the document they feed:
	// pickDescription prefers zh and tolerates missing languages.
	for _, tc := range []struct {
		desc map[string]string
		want string
	}{
		{desc: map[string]string{"zh": "中文", "en": "english"}, want: "中文"},
		{desc: map[string]string{"en": "english"}, want: "english"},
		{desc: map[string]string{"zh": "中文"}, want: "中文"},
		{desc: map[string]string{}, want: ""},
		{desc: nil, want: ""},
	} {
		if got := pickDescription(tc.desc); got != tc.want {
			t.Errorf("pickDescription(%v) = %q, want %q", tc.desc, got, tc.want)
		}
	}
	// mirrorzSize renders sizeBytes as the string the MirrorZ format expects
	// (binary 1024-based units, two decimals, "" for unknown sizes).
	for _, tc := range []struct {
		bytes int64
		want  string
	}{
		{bytes: 0, want: ""}, // unknown: omitted
		{bytes: 512, want: "512.00B"},
		{bytes: 1024, want: "1.00K"},
		{bytes: 640141257728, want: "596.18G"},
		{bytes: 2990078838784, want: "2.72T"},       // "2.72T" as seen in real catalogs
		{bytes: 9223372036854775807, want: "8.00E"}, // int64 max
	} {
		if got := mirrorzSize(tc.bytes); got != tc.want {
			t.Errorf("mirrorzSize(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}

// TestHostReflectionHit: a request Host on the serving whitelist (port
// stripped, case-insensitive) is reflected into the site section and every
// entry URL.
func TestHostReflectionHit(t *testing.T) {
	s := mirrorzTestServer(t, []string{"mirrors.zjusct.io", "mirror.zju.edu.cn"})

	doc, err := s.buildMirrorZ(t.Context(), "MIRROR.ZJU.EDU.CN:8443")
	if err != nil {
		t.Fatalf("buildMirrorZ: %v", err)
	}
	if doc.Site.URL != "https://mirror.zju.edu.cn" {
		t.Errorf("site.url = %q, want reflected host", doc.Site.URL)
	}
	for _, entry := range doc.Mirrors {
		want := "https://mirror.zju.edu.cn/" + entry.CName
		if entry.URL != want {
			t.Errorf("entry %s url = %q, want %q", entry.CName, entry.URL, want)
		}
	}
}

// TestHostReflectionMiss: an unknown Host (and an empty one) falls back to
// the configured site URL everywhere.
func TestHostReflectionMiss(t *testing.T) {
	s := mirrorzTestServer(t, []string{"mirrors.zjusct.io", "mirror.zju.edu.cn"})

	for _, requestHost := range []string{"evil.example.com", "mirrors.zjusct.io.evil.com", ""} {
		doc, err := s.buildMirrorZ(t.Context(), requestHost)
		if err != nil {
			t.Fatalf("buildMirrorZ(%q): %v", requestHost, err)
		}
		if doc.Site.URL != "https://mirrors.zjusct.io" {
			t.Errorf("host %q: site.url = %q, want configured site url", requestHost, doc.Site.URL)
		}
		for _, entry := range doc.Mirrors {
			want := "https://mirrors.zjusct.io/" + entry.CName
			if entry.URL != want {
				t.Errorf("host %q: entry %s url = %q, want %q", requestHost, entry.CName, entry.URL, want)
			}
		}
	}
}

// singleMirrorServer builds a Server listing exactly one published mirror.
func singleMirrorServer(t *testing.T, hostnames []string) *Server {
	t.Helper()
	m := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "debian", Namespace: "mirrors"},
		Spec:       mirrorv1alpha1.MirrorSpec{Services: httpService()},
		Status:     mirrorv1alpha1.MirrorStatus{Phase: mirrorv1alpha1.PhaseReady, ActivePVC: "debian-sync-1"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(m).Build()
	return &Server{
		Client:           c,
		Site:             SiteConfig{URL: "https://mirrors.zjusct.io", Abbr: "ZJU"},
		ServingHostnames: hostnames,
		CatalogEnabled:   true,
	}
}

// TestHandleMirrorZ checks the served document: content type, the spec-level
// shape (version/site/info/mirrors) and that it is valid JSON.
func TestHandleMirrorZ(t *testing.T) {
	s := singleMirrorServer(t, []string{"mirrors.zjusct.io"})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/mirrorz.json")
	if err != nil {
		t.Fatalf("GET /mirrorz.json: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q, want application/json", ct)
	}
	doc, err := parseMirrorZBody(body)
	if err != nil {
		t.Fatalf("invalid mirrorz.json: %v (%s)", err, body)
	}
	if doc["version"] != 1.7 {
		t.Errorf("version = %v, want 1.7", doc["version"])
	}
	site, ok := doc["site"].(map[string]any)
	if !ok || site["url"] != "https://mirrors.zjusct.io" || site["abbr"] != "ZJU" {
		t.Errorf("site wrong: %v", doc["site"])
	}
	if _, ok := doc["info"].([]any); !ok {
		t.Errorf("info must be a list, got %T", doc["info"])
	}
	mirrors, ok := doc["mirrors"].([]any)
	if !ok || len(mirrors) != 1 {
		t.Fatalf("mirrors wrong: %v", doc["mirrors"])
	}
	entry := mirrors[0].(map[string]any)
	if entry["cname"] != "debian" || entry["status"] != "U" || entry["url"] != "https://mirrors.zjusct.io/debian" {
		t.Errorf("mirror entry wrong: %v", entry)
	}
	// sizeBytes is unknown (zero): the size field is omitted.
	if _, has := entry["size"]; has {
		t.Errorf("mirror entry must not carry a size field: %v", entry)
	}
}

// TestHandleMirrorZReflectsRequestHost pins the end-to-end Host reflection
// behavior through the HTTP handler: the httptest request goes to 127.0.0.1,
// so the Host header decides which URL base the document carries.
func TestHandleMirrorZReflectsRequestHost(t *testing.T) {
	s := singleMirrorServer(t, []string{"mirrors.zjusct.io"})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/mirrorz.json", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "mirrors.zjusct.io"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /mirrorz.json: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	doc, err := parseMirrorZBody(body)
	if err != nil {
		t.Fatalf("invalid mirrorz.json: %v (%s)", err, body)
	}
	site := doc["site"].(map[string]any)
	if site["url"] != "https://mirrors.zjusct.io" {
		t.Errorf("site.url = %v, want reflected host", site["url"])
	}
	entry := doc["mirrors"].([]any)[0].(map[string]any)
	if entry["url"] != "https://mirrors.zjusct.io/debian" {
		t.Errorf("entry url = %v, want reflected host", entry["url"])
	}
}

// TestHandleMirrorZCatalogDisabled: catalog.enabled=false takes the
// /mirrorz.json endpoint away entirely.
func TestHandleMirrorZCatalogDisabled(t *testing.T) {
	s := mirrorzTestServer(t, nil)
	s.CatalogEnabled = false
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, _ := get(t, srv.URL+"/mirrorz.json")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when catalog is disabled", resp.StatusCode)
	}
}
