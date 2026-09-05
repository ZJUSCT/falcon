package webapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

func get(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, body
}

func TestHandleRepoSpecOnlyYAML(t *testing.T) {
	m := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "debian", Namespace: "mirrors"},
		Spec: mirrorv1alpha1.MirrorSpec{
			Sync: mirrorv1alpha1.MirrorSyncSpec{Paused: false},
			Info: mirrorv1alpha1.MirrorInfo{
				Description: localized("Debian 镜像", "Debian mirror"),
				Upstream:    "rsync://ftp.debian.org/debian/",
			},
		},
		Status: mirrorv1alpha1.MirrorStatus{
			ActivePVC: "debian-sync-1", // must never leak
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(m).Build()
	srv := httptest.NewServer((&Server{Client: c}).Handler())
	defer srv.Close()

	for _, path := range []string{"/api/repos/debian", "/api/repos/debian.yaml", "/api/repos/debian.yml"} {
		resp, body := get(t, srv.URL+path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (body: %s)", path, resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/x-yaml" {
			t.Errorf("%s: content type = %q, want application/x-yaml", path, ct)
		}
		text := string(body)
		if want := "upstream: rsync://ftp.debian.org/debian/"; !strings.Contains(text, want) {
			t.Errorf("%s: YAML body missing %q:\n%s", path, want, text)
		}
		if strings.Contains(text, "status:") || strings.Contains(text, "debian-sync-1") || strings.Contains(text, "Degraded") {
			t.Errorf("%s: spec-only body leaked status:\n%s", path, text)
		}
	}
}

func TestHandleRepoJSONExtension(t *testing.T) {
	m := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "debian", Namespace: "mirrors"},
		Spec: mirrorv1alpha1.MirrorSpec{
			Info: mirrorv1alpha1.MirrorInfo{Upstream: "rsync://ftp.debian.org/debian/"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(m).Build()
	srv := httptest.NewServer((&Server{Client: c}).Handler())
	defer srv.Close()

	resp, body := get(t, srv.URL+"/api/repos/debian.json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q, want application/json", ct)
	}
	if !strings.Contains(string(body), `"upstream"`) || strings.Contains(string(body), `"status"`) {
		t.Errorf("JSON body must carry spec only:\n%s", body)
	}
}

func TestHandleRepoUnknown(t *testing.T) {
	m := &mirrorv1alpha1.Mirror{ObjectMeta: metav1.ObjectMeta{Name: "debian", Namespace: "mirrors"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(m).Build()
	srv := httptest.NewServer((&Server{Client: c}).Handler())
	defer srv.Close()

	resp, _ := get(t, srv.URL+"/api/repos/nonexistent")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	resp, _ = get(t, srv.URL+"/api/repos/")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("empty name: status = %d, want 404", resp.StatusCode)
	}
	resp, _ = get(t, srv.URL+"/api/repos")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("bare /api/repos: status = %d, want 404", resp.StatusCode)
	}
}

// TestHandleRepoAmbiguous: a Mirror and a ProxyMirror are distinct resources,
// so the same name may exist for both (even in one namespace) — the request
// must be rejected with 409 instead of guessing. The same applies to the same
// name in two namespaces of the same kind.
func TestHandleRepoAmbiguous(t *testing.T) {
	mirror := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "pypi", Namespace: "mirrors"},
	}
	proxy := &mirrorv1alpha1.ProxyMirror{
		ObjectMeta: metav1.ObjectMeta{Name: "pypi", Namespace: "mirrors"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(mirror, proxy).Build()
	srv := httptest.NewServer((&Server{Client: c}).Handler())
	defer srv.Close()

	resp, _ := get(t, srv.URL+"/api/repos/pypi")
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("cross-kind collision: status = %d, want 409", resp.StatusCode)
	}

	otherNS := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "debian", Namespace: "other"},
	}
	oneNS := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "debian", Namespace: "mirrors"},
	}
	c2 := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(otherNS, oneNS).Build()
	srv2 := httptest.NewServer((&Server{Client: c2}).Handler())
	defer srv2.Close()

	resp, _ = get(t, srv2.URL+"/api/repos/debian")
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("cross-namespace collision: status = %d, want 409", resp.StatusCode)
	}
}

// TestHandleRepoProxyMirrorSpec: ProxyMirror specs are served from the same
// endpoint (new concept — the legacy API had no analog; documented extension).
func TestHandleRepoProxyMirrorSpec(t *testing.T) {
	p := &mirrorv1alpha1.ProxyMirror{
		ObjectMeta: metav1.ObjectMeta{Name: "pypi-proxy", Namespace: "mirrors"},
		Spec: mirrorv1alpha1.ProxyMirrorSpec{
			Info: mirrorv1alpha1.ProxyMirrorInfo{Upstream: "https://pypi.org/simple/"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(p).Build()
	srv := httptest.NewServer((&Server{Client: c}).Handler())
	defer srv.Close()

	resp, body := get(t, srv.URL+"/api/repos/pypi-proxy")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "https://pypi.org/simple/") {
		t.Errorf("body missing upstream:\n%s", body)
	}
}
