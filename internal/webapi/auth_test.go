package webapi

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type fakeGitHubTransport struct {
	denied          bool
	membershipState string
}

func (f fakeGitHubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body string
	switch {
	case strings.HasSuffix(r.URL.Path, "/access_token"):
		body = `{"access_token":"token"}`
	case strings.HasPrefix(r.URL.Path, "/user/memberships/orgs/"):
		body = fmt.Sprintf(`{"state":%q}`, f.membershipState)
	default:
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

func TestAuthenticatorOrganizationCallback(t *testing.T) {
	a := &Authenticator{
		Config:    GitHubAuthConfig{ClientID: "id", ClientSecret: "secret", AllowedOrganizations: []string{"ZJUSCT"}},
		AdminHost: "admin.example",
		Client:    &http.Client{Transport: fakeGitHubTransport{membershipState: "active"}},
	}
	r := httptest.NewRequest(http.MethodGet, "https://admin.example/oauth/callback?code=c&state=s", nil)
	r.AddCookie(&http.Cookie{Name: "falcon_oauth_state", Value: "s"})
	w := httptest.NewRecorder()
	a.callback(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("callback status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	sessionReq := httptest.NewRequest(http.MethodGet, "https://admin.example/", nil)
	for _, cookie := range w.Result().Cookies() {
		sessionReq.AddCookie(cookie)
	}
	if _, ok := a.validCookie(sessionReq); !ok {
		t.Fatal("active organization member session rejected")
	}
}

func TestAuthenticatorLoginRedirectURI(t *testing.T) {
	a := &Authenticator{Config: GitHubAuthConfig{ClientID: "id", ClientSecret: "secret"}, AdminHost: "admin.example"}
	for _, organizations := range [][]string{nil, {"ZJUSCT"}} {
		a.Config.AllowedOrganizations = organizations
		r := httptest.NewRequest(http.MethodGet, "https://admin.example/oauth/login", nil)
		w := httptest.NewRecorder()
		a.login(w, r)
		location, err := url.Parse(w.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		scope := "read:user"
		if len(organizations) > 0 {
			scope += " read:org"
		}
		if location.Query().Get("redirect_uri") != "https://admin.example/oauth/callback" || location.Query().Get("scope") != scope {
			t.Fatalf("unexpected login URL: %s", location)
		}
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
	h := s.AdminHandler()
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
