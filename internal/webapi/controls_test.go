package webapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

func controlServer(t *testing.T, m *mirrorv1alpha1.Mirror) (*Server, client.Client) {
	t.Helper()
	if m.UID == "" {
		m.UID = "mirror-uid"
	}
	scheme := testScheme(t)
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(m).Build()
	return &Server{Client: c, Writer: c, APIReader: c, Namespace: "mirrors", Auth: &Authenticator{AdminHost: "admin.example.org", Config: GitHubAuthConfig{ClientID: "client", ClientSecret: "secret", AllowedUserIDs: []int64{1}}}}, c
}

func actionRequest(s *Server, host, origin, body string, authenticated bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "https://"+host+"/api/mirrors/debian/actions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	if authenticated {
		req.AddCookie(&http.Cookie{Name: "falcon_session", Value: s.Auth.cookieValue(1)})
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestMirrorActionsRequireAdminAndSameOrigin(t *testing.T) {
	cases := []struct {
		name, host, origin string
		auth               bool
		want               int
	}{
		{"public host", "mirrors.example.org", "https://mirrors.example.org", true, 403},
		{"anonymous", "admin.example.org", "https://admin.example.org", false, 401},
		{"cross site", "admin.example.org", "https://evil.example.org", true, 403},
		{"missing origin", "admin.example.org", "", true, 403},
		{"valid", "admin.example.org", "https://admin.example.org", true, 202},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, c := controlServer(t, &mirrorv1alpha1.Mirror{ObjectMeta: metav1.ObjectMeta{Name: "debian", Namespace: "mirrors"}})
			w := actionRequest(s, tc.host, tc.origin, `{"action":"pause"}`, tc.auth)
			if w.Code != tc.want {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
			}
			m := &mirrorv1alpha1.Mirror{}
			if err := c.Get(t.Context(), client.ObjectKey{Namespace: "mirrors", Name: "debian"}, m); err != nil {
				t.Fatal(err)
			}
			if m.Spec.Sync.Paused != (tc.want == 202) {
				t.Fatal("unauthorized request changed pause")
			}
		})
	}
}

func TestActionsSubmitBooleanRequestsWithoutChangingStatus(t *testing.T) {
	for _, action := range []string{"sync", "abort"} {
		for _, phase := range []string{"", mirrorv1alpha1.SyncPhasePending, mirrorv1alpha1.SyncPhaseRunning, mirrorv1alpha1.SyncPhaseSnapshotting, mirrorv1alpha1.SyncPhaseCancelling} {
			t.Run(action+"/"+phase, func(t *testing.T) {
				m := &mirrorv1alpha1.Mirror{ObjectMeta: metav1.ObjectMeta{Name: "debian", Namespace: "mirrors"}}
				if phase != "" {
					m.Status.CurrentSync = &mirrorv1alpha1.MirrorCurrentSyncStatus{Phase: phase, QueuedAt: &metav1.Time{Time: time.Now()}}
				}
				s, c := controlServer(t, m)
				key := mirrorv1alpha1.SyncRequestAnnotation
				if action == "abort" {
					key = mirrorv1alpha1.AbortRequestAnnotation
				}
				body, err := json.Marshal(mirrorAction{Action: action})
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; i++ {
					w := actionRequest(s, "admin.example.org", "https://admin.example.org", string(body), true)
					if w.Code != 202 {
						t.Fatalf("request failed: %s", w.Body.String())
					}
				}
				if err := c.Get(t.Context(), client.ObjectKeyFromObject(m), m); err != nil {
					t.Fatal(err)
				}
				if m.Annotations[key] != "true" {
					t.Fatal("boolean request not persisted")
				}
				if (phase == "" && m.Status.CurrentSync != nil) || (phase != "" && m.Status.CurrentSync.Phase != phase) {
					t.Fatal("API changed controller-owned status")
				}
			})
		}
	}
}
