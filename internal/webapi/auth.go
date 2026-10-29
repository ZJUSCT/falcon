package webapi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type GitHubAuthConfig struct {
	ClientID             string
	ClientSecret         string
	AllowedUserIDs       []int64
	AllowedOrganizations []string
}

type authSession struct {
	UserID       int64  `json:"uid"`
	Expires      int64  `json:"exp"`
	Organization string `json:"org,omitempty"`
}

// Authenticator implements a deliberately small, stateless GitHub OAuth flow.
type Authenticator struct {
	Config    GitHubAuthConfig
	AdminHost string
	Client    *http.Client
	Now       func() time.Time
}

func (a *Authenticator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
func (a *Authenticator) enabled() bool {
	return a.Config.ClientID != "" && a.Config.ClientSecret != "" && a.AdminHost != ""
}
func (a *Authenticator) redirectURI() string { return "https://" + a.AdminHost + "/oauth/callback" }
func (a *Authenticator) allowed(id int64, organization string) bool {
	for _, x := range a.Config.AllowedUserIDs {
		if x == id {
			return true
		}
	}
	for _, org := range a.Config.AllowedOrganizations {
		if organization != "" && strings.EqualFold(org, organization) {
			return true
		}
	}
	return false
}
func (a *Authenticator) sign(v string) string {
	h := hmac.New(sha256.New, []byte(a.Config.ClientSecret))
	h.Write([]byte(v))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
func (a *Authenticator) cookieValue(id int64) string {
	return a.cookieValueWithOrganization(id, "")
}
func (a *Authenticator) cookieValueWithOrganization(id int64, organization string) string {
	p, _ := json.Marshal(authSession{UserID: id, Expires: a.now().Add(24 * time.Hour).Unix(), Organization: organization})
	s := base64.RawURLEncoding.EncodeToString(p)
	return s + "." + a.sign(s)
}
func (a *Authenticator) validCookie(r *http.Request) (int64, bool) {
	if !a.enabled() {
		return 0, false
	}
	c, e := r.Cookie("falcon_session")
	if e != nil {
		return 0, false
	}
	p := strings.Split(c.Value, ".")
	if len(p) != 2 || !hmac.Equal([]byte(p[1]), []byte(a.sign(p[0]))) {
		return 0, false
	}
	b, e := base64.RawURLEncoding.DecodeString(p[0])
	if e != nil {
		return 0, false
	}
	var s authSession
	if json.Unmarshal(b, &s) != nil || s.Expires < a.now().Unix() || !a.allowed(s.UserID, s.Organization) {
		return 0, false
	}
	return s.UserID, true
}
func (a *Authenticator) stateCookie(w http.ResponseWriter) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	s := base64.RawURLEncoding.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{Name: "falcon_oauth_state", Value: s, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	return s
}

func (a *Authenticator) login(w http.ResponseWriter, r *http.Request) {
	if !a.enabled() {
		http.Error(w, "OAuth is not configured", http.StatusServiceUnavailable)
		return
	}
	state := a.stateCookie(w)
	scope := "read:user"
	if len(a.Config.AllowedOrganizations) > 0 {
		scope += " read:org"
	}
	q := url.Values{"client_id": {a.Config.ClientID}, "redirect_uri": {a.redirectURI()}, "scope": {scope}, "state": {state}}
	http.Redirect(w, r, "https://github.com/login/oauth/authorize?"+q.Encode(), http.StatusFound)
}
func (a *Authenticator) callback(w http.ResponseWriter, r *http.Request) {
	if !a.enabled() {
		http.Error(w, "OAuth is not configured", http.StatusServiceUnavailable)
		return
	}
	c, e := r.Cookie("falcon_oauth_state")
	if e != nil || c.Value == "" || r.URL.Query().Get("state") != c.Value {
		http.Error(w, "invalid OAuth state", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}
	cl := a.Client
	if cl == nil {
		cl = &http.Client{Timeout: 10 * time.Second}
	}
	form := url.Values{"client_id": {a.Config.ClientID}, "client_secret": {a.Config.ClientSecret}, "code": {code}, "redirect_uri": {a.redirectURI()}}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "https://github.com/login/oauth/access_token", strings.NewReader(form.Encode()))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := cl.Do(req)
	if err != nil {
		http.Error(w, "OAuth exchange failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "OAuth exchange failed", http.StatusBadGateway)
		return
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if json.NewDecoder(resp.Body).Decode(&tok) != nil || tok.AccessToken == "" {
		http.Error(w, "OAuth exchange failed", http.StatusBadGateway)
		return
	}
	req, _ = http.NewRequestWithContext(r.Context(), http.MethodGet, "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err = cl.Do(req)
	if err != nil {
		http.Error(w, "GitHub user lookup failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "GitHub user lookup failed", http.StatusBadGateway)
		return
	}
	var user struct {
		ID int64 `json:"id"`
	}
	if json.NewDecoder(resp.Body).Decode(&user) != nil {
		http.Error(w, "GitHub user lookup failed", http.StatusBadGateway)
		return
	}
	organization := ""
	if !a.allowed(user.ID, "") {
		organization, err = a.memberOrganization(r.Context(), cl, tok.AccessToken)
		if err != nil {
			http.Error(w, "GitHub organization lookup failed", http.StatusBadGateway)
			return
		}
		if organization == "" {
			http.Error(w, "GitHub user is not allowed", http.StatusForbidden)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: "falcon_session", Value: a.cookieValueWithOrganization(user.ID, organization), Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 86400})
	http.SetCookie(w, &http.Cookie{Name: "falcon_oauth_state", MaxAge: -1, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// memberOrganization returns the first allowed organization with active membership.
func (a *Authenticator) memberOrganization(ctx context.Context, cl *http.Client, token string) (string, error) {
	for _, org := range a.Config.AllowedOrganizations {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user/memberships/orgs/"+url.PathEscape(org), nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := cl.Do(req)
		if err != nil {
			return "", err
		}
		var membership struct {
			State string `json:"state"`
		}
		if resp.StatusCode == http.StatusOK {
			err = json.NewDecoder(resp.Body).Decode(&membership)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			continue
		}
		if resp.StatusCode != http.StatusOK || err != nil {
			return "", fmt.Errorf("GitHub membership lookup failed (HTTP %d): %v", resp.StatusCode, err)
		}
		if membership.State == "active" {
			return org, nil
		}
	}
	return "", nil
}

func (a *Authenticator) session(w http.ResponseWriter, r *http.Request) {
	id, ok := a.validCookie(r)
	if !ok {
		writeJSONError(w, 401, "unauthorized")
		return
	}
	writeJSON(w, 200, map[string]interface{}{"authenticated": true, "githubUserID": id})
}
func (a *Authenticator) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "falcon_session", MaxAge: -1, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	w.WriteHeader(http.StatusNoContent)
}
func (a *Authenticator) require(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := a.validCookie(r); !ok {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSONError(w, 401, "unauthorized")
		} else {
			http.Redirect(w, r, "/oauth/login", http.StatusFound)
		}
		return false
	}
	return true
}
