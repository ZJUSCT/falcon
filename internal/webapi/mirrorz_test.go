package webapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

// parseMirrorZBody decodes a served mirrorz.json body.
func parseMirrorZBody(body []byte) (map[string]any, error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// Fake clients do not populate apiserver timestamps. Valid catalog fixtures
// explicitly carry the creation and successful completion persisted in reality.
func catalogMirrorTimes(m *mirrorv1alpha1.Mirror) {
	m.CreationTimestamp = metav1.Unix(1788000000, 0)
	success := metav1.Unix(1788300000, 0)
	m.Status.LastSuccessfulSyncAt = &success
	if m.Status.LastSync == nil {
		m.Status.LastSync = &mirrorv1alpha1.MirrorSyncStatus{Phase: mirrorv1alpha1.SyncPhaseSucceeded}
	}
	if m.Status.LastSync != nil {
		m.Status.LastSync.FinishedAt = success.DeepCopy()
	}
	if m.Status.CurrentSync != nil {
		m.Status.CurrentSync.StartedAt = func() *metav1.Time { t := metav1.Unix(1788380000, 0); return &t }()
	}
}

func TestMirrorzStatusForMirror(t *testing.T) {
	finished, started, next := metav1.Unix(1788388984, 0), metav1.Unix(1788380000, 0), metav1.Unix(1788400000, 0)
	cases := []struct {
		name   string
		mutate func(*mirrorv1alpha1.Mirror)
		want   string
	}{
		{"successful sync", func(m *mirrorv1alpha1.Mirror) {
			m.Status.LastSync = &mirrorv1alpha1.MirrorSyncStatus{Phase: mirrorv1alpha1.SyncPhaseSucceeded, FinishedAt: &finished}
			m.Status.NextSyncAt = &next
		}, "S1788388984X1788400000N1788000000"},
		{"syncing retains old success without stale schedule", func(m *mirrorv1alpha1.Mirror) {
			m.Status.CurrentSync = &mirrorv1alpha1.MirrorCurrentSyncStatus{Phase: mirrorv1alpha1.SyncPhaseRunning, StartedAt: &started}
			m.Status.NextSyncAt = &next
		}, "Y1788380000O1788300000N1788000000"},
		{"paused uses pause time without stale schedule", func(m *mirrorv1alpha1.Mirror) {
			m.SetSyncPaused(true)
			m.Status.PausedAt = &finished
			m.Status.NextSyncAt = &next
		}, "P1788388984N1788000000"},
		{"failed sync retains old success and retry", func(m *mirrorv1alpha1.Mirror) {
			m.Status.LastSync = &mirrorv1alpha1.MirrorSyncStatus{Phase: mirrorv1alpha1.SyncPhaseFailed, StartedAt: &started, FinishedAt: &finished}
			m.Status.NextSyncAt = &next
		}, "F1788380000O1788300000X1788400000N1788000000"},
		{"queued retains old success without stale schedule", func(m *mirrorv1alpha1.Mirror) {
			m.Status.CurrentSync = &mirrorv1alpha1.MirrorCurrentSyncStatus{Phase: mirrorv1alpha1.SyncPhasePending, QueuedAt: &started}
			m.Status.NextSyncAt = &next
		}, "D1788380000O1788300000N1788000000"},
		{"snapshotting is not a running Job even with pause requested", func(m *mirrorv1alpha1.Mirror) {
			m.SetSyncPaused(true)
			m.Status.CurrentSync = &mirrorv1alpha1.MirrorCurrentSyncStatus{Phase: mirrorv1alpha1.SyncPhaseSnapshotting}
			m.Status.LastSync = &mirrorv1alpha1.MirrorSyncStatus{Phase: mirrorv1alpha1.SyncPhaseSucceeded, FinishedAt: &finished}
		}, "S1788388984N1788000000"},
		{"publication failure preserves sync success", func(m *mirrorv1alpha1.Mirror) {
			m.Status.LastAttempt = &mirrorv1alpha1.MirrorSyncStatus{Phase: mirrorv1alpha1.SyncPhaseFailed}
			m.Status.LastSync = &mirrorv1alpha1.MirrorSyncStatus{Phase: mirrorv1alpha1.SyncPhaseSucceeded, FinishedAt: &finished}
			m.Status.NextSyncAt = &next
		}, "S1788388984X1788400000N1788000000"},
		{"unknown result retains schedule", func(m *mirrorv1alpha1.Mirror) { m.Status.LastSync = nil; m.Status.NextSyncAt = &next }, ""},
		{"unknown current phase excludes stale schedule", func(m *mirrorv1alpha1.Mirror) {
			m.Status.CurrentSync = &mirrorv1alpha1.MirrorCurrentSyncStatus{}
			m.Status.NextSyncAt = &next
		}, ""},
		{"unknown result", func(m *mirrorv1alpha1.Mirror) { m.Status.LastSync = nil }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &mirrorv1alpha1.Mirror{}
			catalogMirrorTimes(m)
			tc.mutate(m)
			got, err := mirrorzStatusForMirror(m)
			if tc.want == "" {
				if err == nil || got != "" {
					t.Fatalf("expected explicit state error, got %q, %v", got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("status=%q, err=%v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestMirrorzStatusForProxyMirror(t *testing.T) {
	cases := []struct {
		cache bool
		want  string
	}{
		{true, "CN1788000000"},
		{false, "RN1788000000"},
	}
	for _, tc := range cases {
		proxy := &mirrorv1alpha1.ProxyMirror{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.Unix(1788000000, 0)}}
		if tc.cache {
			proxy.Spec.Cache = &mirrorv1alpha1.ProxyMirrorCacheSpec{}
		}
		if got, err := mirrorzStatusForProxyMirror(proxy); err != nil || got != tc.want {
			t.Errorf("mirrorzStatusForProxyMirror(cache=%t) = %q, want %q", tc.cache, got, tc.want)
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

// httpService is the enabled (declared) http spec.publish key the catalog
// fixtures use (a Mirror with every key absent is sync-only and must be
// omitted).
func httpService() mirrorv1alpha1.MirrorServicesSpec {
	return mirrorv1alpha1.MirrorServicesSpec{
		HTTP: &mirrorv1alpha1.MirrorHTTPServiceSpec{
			MirrorServiceSpec: mirrorv1alpha1.MirrorServiceSpec{
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
// given publish hostname whitelist.
func mirrorzTestServer(t *testing.T, hostnames []string) *Server {
	t.Helper()
	// Published and served, with a known on-disk usage.
	published := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "debian", Namespace: "mirrors"},
		Spec: mirrorv1alpha1.MirrorSpec{
			Info: mirrorv1alpha1.MirrorInfo{
				Description: "Debian 发行版软件包镜像",
				Upstream:    "rsync://ftp.debian.org/debian/",
			},
			Publish: httpService(),
		},
		Status: mirrorv1alpha1.MirrorStatus{
			ActivePVC:  "debian-sync-1",
			SizeBytes:  640141257728,
			LastSync:   &mirrorv1alpha1.MirrorSyncStatus{Phase: mirrorv1alpha1.SyncPhaseSucceeded},
			Conditions: []metav1.Condition{testCondition("Ready", metav1.ConditionTrue)},
		},
	}
	// A current-generation Ready=False endpoint must be omitted.
	notReady := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "ubuntu", Namespace: "mirrors"},
		Spec: mirrorv1alpha1.MirrorSpec{
			Info:    mirrorv1alpha1.MirrorInfo{Upstream: "rsync://archive.ubuntu.com/ubuntu/"},
			Publish: httpService(),
		},
		Status: mirrorv1alpha1.MirrorStatus{Conditions: []metav1.Condition{testCondition("Ready", metav1.ConditionFalse)}},
	}
	// A sync-only Mirror has no HTTP endpoint and must be omitted even when
	// its synchronization state is Ready.
	syncOnly := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "alpine", Namespace: "mirrors"},
		Status: mirrorv1alpha1.MirrorStatus{
			ActivePVC:  "alpine-sync-3",
			Conditions: []metav1.Condition{testCondition("Ready", metav1.ConditionTrue)},
		},
	}
	syncing := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "arch", Namespace: "mirrors"},
		Spec:       mirrorv1alpha1.MirrorSpec{Publish: httpService()},
		Status: mirrorv1alpha1.MirrorStatus{
			ActivePVC:   "arch-sync-2",
			CurrentSync: &mirrorv1alpha1.MirrorCurrentSyncStatus{Phase: mirrorv1alpha1.SyncPhaseRunning},
			Conditions:  []metav1.Condition{testCondition("Ready", metav1.ConditionTrue)},
		},
	}
	proxy := &mirrorv1alpha1.ProxyMirror{
		ObjectMeta: metav1.ObjectMeta{Name: "pypi-proxy", Namespace: "mirrors"},
		Spec: mirrorv1alpha1.ProxyMirrorSpec{
			Info: mirrorv1alpha1.ProxyMirrorInfo{
				Description: "PyPI 缓存代理",
				Upstream:    "https://pypi.org/simple/",
			},
			Publish: mirrorv1alpha1.ProxyMirrorServicesSpec{HTTP: httpService().HTTP},
		},
		Status: mirrorv1alpha1.ProxyMirrorStatus{Conditions: []metav1.Condition{testCondition("Ready", metav1.ConditionTrue)}},
	}

	catalogMirrorTimes(published)
	catalogMirrorTimes(syncing)
	proxy.CreationTimestamp = metav1.Unix(1788000000, 0)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(published, notReady, syncOnly, syncing, proxy).Build()
	return &Server{
		Client:           c,
		Site:             SiteConfig{URL: "https://mirrors.zjusct.io", Abbr: "ZJU", Name: "Zhejiang University Mirror"},
		PublishHostnames: hostnames,
		MirrorzEnabled:   true,
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
		t.Fatalf("got %d mirrors (%v), want 3 (not-ready ubuntu and sync-only alpine omitted)", len(doc.Mirrors), doc.Mirrors)
	}
	if doc.Mirrors[0].CName != "arch" || doc.Mirrors[0].Status != "Y1788380000O1788300000N1788000000" {
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
	if doc.Mirrors[1].CName != "debian" || doc.Mirrors[1].Status != "S1788300000N1788000000" {
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
		t.Errorf("debian desc = %q, want configured description", doc.Mirrors[1].Desc)
	}
	if doc.Mirrors[1].Upstream != "rsync://ftp.debian.org/debian/" {
		t.Errorf("debian upstream = %q", doc.Mirrors[1].Upstream)
	}
	if doc.Mirrors[2].CName != "pypi-proxy" || doc.Mirrors[2].Status != "RN1788000000" {
		t.Errorf("pypi-proxy entry wrong: %+v", doc.Mirrors[2])
	}
	if doc.Mirrors[2].URL != "https://mirrors.zjusct.io/pypi-proxy" {
		t.Errorf("pypi-proxy url = %q", doc.Mirrors[2].URL)
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

// TestHostReflectionHit: a request Host on the publish whitelist (port
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
		Spec:       mirrorv1alpha1.MirrorSpec{Publish: httpService()},
		Status: mirrorv1alpha1.MirrorStatus{
			ActivePVC:  "debian-sync-1",
			LastSync:   &mirrorv1alpha1.MirrorSyncStatus{Phase: mirrorv1alpha1.SyncPhaseSucceeded},
			Conditions: []metav1.Condition{testCondition("Ready", metav1.ConditionTrue)},
		},
	}
	catalogMirrorTimes(m)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(m).Build()
	return &Server{
		Client:           c,
		Site:             SiteConfig{URL: "https://mirrors.zjusct.io", Abbr: "ZJU"},
		PublishHostnames: hostnames,
		MirrorzEnabled:   true,
	}
}

// TestHandleMirrorZ checks the served document: content type, the spec-level
// shape (version/site/info/mirrors) and that it is valid JSON.
func TestHandleMirrorZ(t *testing.T) {
	s := singleMirrorServer(t, []string{"mirrors.zjusct.io"})
	srv := httptest.NewServer(s.MirrorzHandler())
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
	if entry["cname"] != "debian" || entry["status"] != "S1788300000N1788000000" || entry["url"] != "https://mirrors.zjusct.io/debian" {
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
	srv := httptest.NewServer(s.MirrorzHandler())
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
	s.MirrorzEnabled = false
	srv := httptest.NewServer(s.MirrorzHandler())
	defer srv.Close()

	resp, _ := get(t, srv.URL+"/mirrorz.json")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when catalog is disabled", resp.StatusCode)
	}
}

func TestMirrorZCanonicalNamesKeepCRPaths(t *testing.T) {
	mirror := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "aosp", Namespace: "mirrors"},
		Spec:       mirrorv1alpha1.MirrorSpec{Info: mirrorv1alpha1.MirrorInfo{CName: "AOSP"}, Publish: httpService()},
		Status:     mirrorv1alpha1.MirrorStatus{Conditions: []metav1.Condition{testCondition("Ready", metav1.ConditionTrue)}},
	}
	proxy := &mirrorv1alpha1.ProxyMirror{
		ObjectMeta: metav1.ObjectMeta{Name: "aur", Namespace: "mirrors"},
		Spec: mirrorv1alpha1.ProxyMirrorSpec{
			Info:    mirrorv1alpha1.ProxyMirrorInfo{CName: "AUR"},
			Publish: mirrorv1alpha1.ProxyMirrorServicesSpec{HTTP: httpService().HTTP},
		},
		Status: mirrorv1alpha1.ProxyMirrorStatus{Conditions: []metav1.Condition{testCondition("Ready", metav1.ConditionTrue)}},
	}
	catalogMirrorTimes(mirror)
	proxy.CreationTimestamp = metav1.Unix(1788000000, 0)
	s := &Server{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(mirror, proxy).Build(), Site: SiteConfig{URL: "https://example.org"}}
	doc, err := s.buildMirrorZ(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Mirrors) != 2 || doc.Mirrors[0].CName != "AOSP" || doc.Mirrors[0].URL != "https://example.org/aosp" || doc.Mirrors[1].CName != "AUR" || doc.Mirrors[1].URL != "https://example.org/aur" {
		t.Fatalf("canonical names must not change paths: %+v", doc.Mirrors)
	}
}

func TestHandleMirrorZTimestampInvariants(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*mirrorv1alpha1.Mirror)
		errorField string
	}{
		{"complete Y O N", func(m *mirrorv1alpha1.Mirror) {}, ""},
		{"missing success", func(m *mirrorv1alpha1.Mirror) { m.Status.LastSuccessfulSyncAt = nil }, "status.lastSuccessfulSyncAt"},
		{"zero success", func(m *mirrorv1alpha1.Mirror) { m.Status.LastSuccessfulSyncAt = &metav1.Time{} }, "status.lastSuccessfulSyncAt"},
		{"missing creation", func(m *mirrorv1alpha1.Mirror) { m.CreationTimestamp = metav1.Time{} }, "metadata.creationTimestamp"},
		{"missing start", func(m *mirrorv1alpha1.Mirror) { m.Status.CurrentSync.StartedAt = nil }, "status.currentSync.startedAt"},
		{"missing queue time", func(m *mirrorv1alpha1.Mirror) { m.Status.CurrentSync.Phase = mirrorv1alpha1.SyncPhasePending }, "status.currentSync.queuedAt"},
		{"missing pause time", func(m *mirrorv1alpha1.Mirror) { m.Status.CurrentSync = nil; m.SetSyncPaused(true) }, "status.pausedAt"},
		{"missing successful finish", func(m *mirrorv1alpha1.Mirror) {
			m.Status.CurrentSync = nil
			m.Status.LastSync = &mirrorv1alpha1.MirrorSyncStatus{Phase: mirrorv1alpha1.SyncPhaseSucceeded}
		}, "status.lastSync.finishedAt"},
		{"missing failed start", func(m *mirrorv1alpha1.Mirror) {
			m.Status.CurrentSync = nil
			m.Status.LastSync = &mirrorv1alpha1.MirrorSyncStatus{Phase: mirrorv1alpha1.SyncPhaseFailed}
		}, "status.lastSync.startedAt"},
		{"invalid schedule", func(m *mirrorv1alpha1.Mirror) {
			m.Status.CurrentSync = nil
			t := metav1.Unix(0, 0)
			m.Status.NextSyncAt = &t
		}, "status.nextSyncAt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &mirrorv1alpha1.Mirror{
				ObjectMeta: metav1.ObjectMeta{Name: "debian", Namespace: "mirrors", Generation: 1},
				Spec:       mirrorv1alpha1.MirrorSpec{Publish: httpService()},
				Status: mirrorv1alpha1.MirrorStatus{
					CurrentSync: &mirrorv1alpha1.MirrorCurrentSyncStatus{Phase: mirrorv1alpha1.SyncPhaseRunning},
					Conditions:  []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: 1}},
				},
			}
			catalogMirrorTimes(m)
			tc.mutate(m)
			s := &Server{MirrorzEnabled: true, Site: SiteConfig{URL: "https://example.org"}, Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(m).Build()}
			recorder := httptest.NewRecorder()
			s.MirrorzHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/mirrorz.json", nil))
			if tc.errorField != "" {
				if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), tc.errorField) || !strings.Contains(recorder.Body.String(), "Mirror mirrors/debian") || strings.Contains(recorder.Body.String(), `"mirrors":`) {
					t.Fatalf("expected explicit invariant error for %s, got %d %s", tc.errorField, recorder.Code, recorder.Body.String())
				}
				return
			}
			var doc mirrorzDocument
			if err := json.Unmarshal(recorder.Body.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != http.StatusOK || len(doc.Mirrors) != 1 || doc.Mirrors[0].Status != "Y1788380000O1788300000N1788000000" {
				t.Fatalf("expected full timestamps: code=%d, body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestMirrorZRejectsProxyWithoutCreationTime(t *testing.T) {
	p := &mirrorv1alpha1.ProxyMirror{
		ObjectMeta: metav1.ObjectMeta{Name: "pypi", Namespace: "mirrors"},
		Spec:       mirrorv1alpha1.ProxyMirrorSpec{Publish: mirrorv1alpha1.ProxyMirrorServicesSpec{HTTP: httpService().HTTP}},
		Status:     mirrorv1alpha1.ProxyMirrorStatus{Conditions: []metav1.Condition{testCondition("Ready", metav1.ConditionTrue)}},
	}
	s := &Server{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(p).Build(), MirrorzEnabled: true}
	w := httptest.NewRecorder()
	s.MirrorzHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/mirrorz.json", nil))
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "ProxyMirror mirrors/pypi") || !strings.Contains(w.Body.String(), "metadata.creationTimestamp") {
		t.Fatalf("expected explicit proxy timestamp error: %d %s", w.Code, w.Body.String())
	}
}

func TestMirrorZManualModeAndCancellation(t *testing.T) {
	queued, started, stopped := metav1.Unix(1788380000, 0), metav1.Unix(1788380100, 0), metav1.Unix(1788380200, 0)
	cases := []struct {
		name, phase           string
		paused, held, started bool
		want                  string
	}{
		{"held automatic queue", mirrorv1alpha1.SyncPhasePending, true, true, false, "P1788380200N1788000000"},
		{"manual queue", mirrorv1alpha1.SyncPhasePending, true, false, false, "D1788380000O1788300000N1788000000"},
		{"manual running", mirrorv1alpha1.SyncPhaseRunning, true, false, true, "Y1788380100O1788300000N1788000000"},
		{"cancelling held queue", mirrorv1alpha1.SyncPhaseCancelling, true, true, false, "D1788380000O1788300000N1788000000"},
		{"cancelling running job", mirrorv1alpha1.SyncPhaseCancelling, true, false, true, "Y1788380100O1788300000N1788000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &mirrorv1alpha1.Mirror{}
			catalogMirrorTimes(m)
			m.SetSyncPaused(tc.paused)
			m.Status.CurrentSync = &mirrorv1alpha1.MirrorCurrentSyncStatus{Phase: tc.phase, QueuedAt: &queued}
			if tc.held {
				m.Status.PausedAt = &stopped
			}
			if tc.started {
				m.Status.CurrentSync.StartedAt = &started
			}
			got, err := mirrorzStatusForMirror(m)
			if err != nil || got != tc.want {
				t.Fatalf("%q, %v, want %q", got, err, tc.want)
			}
		})
	}
	m := &mirrorv1alpha1.Mirror{}
	catalogMirrorTimes(m)
	m.Status.LastSync = &mirrorv1alpha1.MirrorSyncStatus{Phase: mirrorv1alpha1.SyncPhaseCancelled, StartedAt: &started, FinishedAt: &stopped}
	if got, err := mirrorzStatusForMirror(m); err != nil || got != "F1788380100O1788300000N1788000000" {
		t.Fatalf("cancelled sync: %q, %v", got, err)
	}
	m.SetSyncPaused(true)
	m.Status.PausedAt = &stopped
	if got, err := mirrorzStatusForMirror(m); err != nil || got != "P1788380200N1788000000" {
		t.Fatalf("paused after cancellation: %q, %v", got, err)
	}
}

// TestMirrorZExcludesRedirectMode: a redirect-mode endpoint is not this site
// serving the mirror — a 302 away is no catalog entry, even with Ready=True.
func TestMirrorZExcludesRedirectMode(t *testing.T) {
	mirror := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "debian", Namespace: "mirrors", CreationTimestamp: metav1.Unix(1788000000, 0)},
		Spec: mirrorv1alpha1.MirrorSpec{
			Info:    mirrorv1alpha1.MirrorInfo{Upstream: "rsync://ftp.debian.org/debian/"},
			Publish: httpService(),
		},
		Status: mirrorv1alpha1.MirrorStatus{
			LastSync:   &mirrorv1alpha1.MirrorSyncStatus{Phase: mirrorv1alpha1.SyncPhaseSucceeded},
			Conditions: []metav1.Condition{testCondition("Ready", metav1.ConditionTrue)},
		},
	}
	mirror.Spec.Publish.HTTP.PodTemplate = corev1.PodTemplateSpec{}
	mirror.Spec.Publish.HTTP.Redirect = "mirrors.cernet.edu.cn"
	proxy := &mirrorv1alpha1.ProxyMirror{
		ObjectMeta: metav1.ObjectMeta{Name: "pypi", Namespace: "mirrors", CreationTimestamp: metav1.Unix(1788000000, 0)},
		Spec: mirrorv1alpha1.ProxyMirrorSpec{
			Publish: mirrorv1alpha1.ProxyMirrorServicesSpec{HTTP: httpService().HTTP},
		},
		Status: mirrorv1alpha1.ProxyMirrorStatus{Conditions: []metav1.Condition{testCondition("Ready", metav1.ConditionTrue)}},
	}
	proxy.Spec.Publish.HTTP.PodTemplate = corev1.PodTemplateSpec{}
	proxy.Spec.Publish.HTTP.Redirect = "mirrors.cernet.edu.cn"
	s := &Server{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(mirror, proxy).Build(), Site: SiteConfig{URL: "https://example.org"}}
	doc, err := s.buildMirrorZ(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Mirrors) != 0 {
		t.Fatalf("redirect-mode entries must stay out of the catalog: %+v", doc.Mirrors)
	}
}
