package webapi

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeGitHubTransport struct{ denied bool }

func (f fakeGitHubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body string
	if strings.HasSuffix(r.URL.Path, "/access_token") {
		body = `{"access_token":"token"}`
	} else {
		id := int64(42)
		if f.denied {
			id = 99
		}
		body = fmt.Sprintf(`{"id":%d}`, id)
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

func TestAuthenticatorSessionAndAllowlist(t *testing.T) {
	a := &Authenticator{Config: GitHubAuthConfig{ClientID: "id", ClientSecret: "secret", AllowedUserIDs: []int64{42}}, AdminHost: "admin.example"}
	a.Now = func() time.Time { return time.Unix(1000, 0) }
	r := httptest.NewRequest(http.MethodGet, "https://admin.example/", nil)
	httpCookie := a.cookieValue(42)
	r.AddCookie(&http.Cookie{Name: "falcon_session", Value: httpCookie})
	if id, ok := a.validCookie(r); !ok || id != 42 {
		t.Fatalf("valid session rejected")
	}
	r2 := httptest.NewRequest(http.MethodGet, "https://admin.example/", nil)
	r2.AddCookie(&http.Cookie{Name: "falcon_session", Value: a.cookieValue(7)})
	if _, ok := a.validCookie(r2); ok {
		t.Fatalf("disallowed user accepted")
	}
}

func TestAuthenticatorRequireRedirects(t *testing.T) {
	a := &Authenticator{Config: GitHubAuthConfig{ClientID: "id", ClientSecret: "secret"}, AdminHost: "admin.example"}
	r := httptest.NewRequest(http.MethodGet, "https://admin.example/", nil)
	w := httptest.NewRecorder()
	if a.require(w, r) {
		t.Fatal("unauthenticated request accepted")
	}
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/oauth/login" {
		t.Fatalf("status/location = %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestAuthenticatorCallback(t *testing.T) {
	a := &Authenticator{Config: GitHubAuthConfig{ClientID: "id", ClientSecret: "secret", AllowedUserIDs: []int64{42}}, AdminHost: "admin.example", Client: &http.Client{Transport: fakeGitHubTransport{}}}
	a.Now = func() time.Time { return time.Unix(1000, 0) }
	r := httptest.NewRequest(http.MethodGet, "https://admin.example/oauth/callback?code=c&state=s", nil)
	r.AddCookie(&http.Cookie{Name: "falcon_oauth_state", Value: "s"})
	w := httptest.NewRecorder()
	a.callback(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("callback status/location = %d %q", w.Code, w.Header().Get("Location"))
	}
	if !strings.Contains(w.Header().Get("Set-Cookie"), "falcon_session=") {
		t.Fatalf("session cookie missing: %q", w.Header().Get("Set-Cookie"))
	}
	// A GitHub ID outside the configured allowlist is denied.
	a.Client = &http.Client{Transport: fakeGitHubTransport{denied: true}}
	w = httptest.NewRecorder()
	a.callback(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("denied callback status = %d", w.Code)
	}
}

func TestAuthenticatorLoginRedirectURI(t *testing.T) {
	a := &Authenticator{Config: GitHubAuthConfig{ClientID: "id", ClientSecret: "secret"}, AdminHost: "admin.example"}
	r := httptest.NewRequest(http.MethodGet, "https://admin.example/oauth/login", nil)
	w := httptest.NewRecorder()
	a.login(w, r)
	if !strings.Contains(w.Header().Get("Location"), "redirect_uri=https%3A%2F%2Fadmin.example%2Foauth%2Fcallback") {
		t.Fatalf("redirect URI missing: %q", w.Header().Get("Location"))
	}
}

func TestAdminGatewayAuthenticationAndProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "ok")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	a := &Authenticator{Config: GitHubAuthConfig{ClientID: "id", ClientSecret: "secret", AllowedUserIDs: []int64{42}}, AdminHost: "admin.example"}
	s := &Server{Auth: a, UIUpstream: upstream.URL}
	h := s.Handler()
	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "https://admin.example/", nil))
	if unauth.Code != http.StatusFound {
		t.Fatalf("unauthenticated status = %d", unauth.Code)
	}
	authReq := httptest.NewRequest(http.MethodGet, "https://admin.example/", nil)
	authReq.AddCookie(&http.Cookie{Name: "falcon_session", Value: a.cookieValue(42)})
	authResp := httptest.NewRecorder()
	h.ServeHTTP(authResp, authReq)
	if authResp.Code != http.StatusOK || authResp.Header().Get("X-Upstream") != "ok" {
		t.Fatalf("authenticated proxy response = %d, header %q", authResp.Code, authResp.Header().Get("X-Upstream"))
	}
}
