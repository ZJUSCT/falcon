package webapi

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type GitHubAuthConfig struct {
	ClientID       string
	ClientSecret   string
	AllowedUserIDs []int64
}

type authSession struct {
	UserID  int64 `json:"uid"`
	Expires int64 `json:"exp"`
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
func (a *Authenticator) allowed(id int64) bool {
	for _, x := range a.Config.AllowedUserIDs {
		if x == id {
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
	p, _ := json.Marshal(authSession{id, a.now().Add(24 * time.Hour).Unix()})
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
	if json.Unmarshal(b, &s) != nil || s.Expires < a.now().Unix() || !a.allowed(s.UserID) {
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
	q := url.Values{"client_id": {a.Config.ClientID}, "redirect_uri": {a.redirectURI()}, "scope": {"read:user"}, "state": {state}}
	http.Redirect(w, r, "https://github.com/login/oauth/authorize?"+q.Encode(), http.StatusFound)
}
func (a *Authenticator) callback(w http.ResponseWriter, r *http.Request) {
	if !a.enabled() {
		http.Error(w, "OAuth is not configured", 503)
		return
	}
	c, e := r.Cookie("falcon_oauth_state")
	if e != nil || c.Value == "" || r.URL.Query().Get("state") != c.Value {
		http.Error(w, "invalid OAuth state", 400)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", 400)
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
		http.Error(w, "OAuth exchange failed", 502)
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
		http.Error(w, "OAuth exchange failed", 502)
		return
	}
	req, _ = http.NewRequestWithContext(r.Context(), http.MethodGet, "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err = cl.Do(req)
	if err != nil {
		http.Error(w, "GitHub user lookup failed", 502)
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
	if json.NewDecoder(resp.Body).Decode(&user) != nil || !a.allowed(user.ID) {
		http.Error(w, "GitHub user is not allowed", 403)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "falcon_session", Value: a.cookieValue(user.ID), Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 86400})
	http.SetCookie(w, &http.Cookie{Name: "falcon_oauth_state", MaxAge: -1, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/", 303)
}
func (a *Authenticator) session(w http.ResponseWriter, r *http.Request) {
	id, ok := a.validCookie(r)
	if !ok {
		writeJSONError(w, 401, "unauthorized")
		return
	}
	writeJSON(w, 200, "application/json", map[string]interface{}{"authenticated": true, "githubUserID": id})
}
func (a *Authenticator) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "falcon_session", MaxAge: -1, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	w.WriteHeader(204)
}
func (a *Authenticator) require(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := a.validCookie(r); !ok {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSONError(w, 401, "unauthorized")
		} else {
			http.Redirect(w, r, "/oauth/login", 302)
		}
		return false
	}
	return true
}
