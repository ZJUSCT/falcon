package webapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

// TestReadOnlyListenersRejectNonGET: both listeners are strictly read-only
// — every non-GET on a registered route gets 405 with an Allow: GET header.
func TestReadOnlyListenersRejectNonGET(t *testing.T) {
	m := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "debian", Namespace: "mirrors"},
		Status:     mirrorv1alpha1.MirrorStatus{ActivePVC: "debian-sync-1"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(m).Build()
	s := &Server{Client: c, Site: SiteConfig{URL: "https://mirrors.zjusct.io", Abbr: "ZJU"}}
	for name, tc := range map[string]struct {
		handler http.Handler
		paths   []string
	}{
		"admin":   {s.AdminHandler(), []string{"/api/jobs", "/api/repos/debian"}},
		"mirrorz": {s.MirrorzHandler(), []string{"/mirrorz.json"}},
	} {
		srv := httptest.NewServer(tc.handler)
		for _, path := range tc.paths {
			for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
				req, err := http.NewRequest(method, srv.URL+path, nil)
				if err != nil {
					t.Fatalf("new request: %v", err)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("%s %s %s: %v", name, method, path, err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusMethodNotAllowed {
					t.Errorf("%s %s %s: status = %d, want 405", name, method, path, resp.StatusCode)
				}
				if allow := resp.Header.Get("Allow"); allow != http.MethodGet {
					t.Errorf("%s %s %s: Allow = %q, want GET", name, method, path, allow)
				}
			}
		}
		srv.Close()
	}
}

// TestUnknownPathsAre404 checks the catch-all route.
func TestUnknownPathsAre404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	srv := httptest.NewServer((&Server{Client: c}).MirrorzHandler())
	defer srv.Close()

	resp, _ := get(t, srv.URL+"/definitely-not-a-route")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
