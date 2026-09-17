package webapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleVersion(t *testing.T) {
	srv := httptest.NewServer((&Server{Version: "v0.0.0-test"}).AdminHandler())
	defer srv.Close()

	resp, body := get(t, srv.URL+"/api/version")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"version": "v0.0.0-test"`) {
		t.Fatalf("body = %s, want it to contain the stamped version", body)
	}
}
