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

// TestReadOnlyListenerRejectsNonGET: the listener is strictly read-only —
// every non-GET method gets 405 with an Allow: GET header.
func TestReadOnlyListenerRejectsNonGET(t *testing.T) {
	m := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "debian", Namespace: "mirrors"},
		Status:     mirrorv1alpha1.MirrorStatus{Phase: mirrorv1alpha1.PhaseReady, ActivePVC: "debian-sync-1"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(m).Build()
	srv := httptest.NewServer((&Server{Client: c, Site: SiteConfig{URL: "https://mirrors.zjusct.io", Abbr: "ZJU"}}).Handler())
	defer srv.Close()

	for _, path := range []string{"/api/jobs", "/api/repos/debian", "/mirrorz.json"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			req, err := http.NewRequest(method, srv.URL+path, nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: status = %d, want 405", method, path, resp.StatusCode)
			}
			if allow := resp.Header.Get("Allow"); allow != http.MethodGet {
				t.Errorf("%s %s: Allow = %q, want GET", method, path, allow)
			}
		}
	}
}

// TestUnknownPathsAre404 checks the catch-all route.
func TestUnknownPathsAre404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	srv := httptest.NewServer((&Server{Client: c}).Handler())
	defer srv.Close()

	resp, _ := get(t, srv.URL+"/definitely-not-a-route")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
