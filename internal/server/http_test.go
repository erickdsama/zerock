package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"API-X.example.com":      "api-x.example.com",
		"api-x.example.com:8443": "api-x.example.com",
		" api-x.example.com ":    "api-x.example.com",
		"[::1]:443":              "::1",
		"":                       "",
	}
	for in, want := range cases {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClientIPTrustsHeadersOnlyWhenConfigured(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.9:5555"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")

	untrusting := &Server{cfg: Config{}}
	if got := untrusting.clientIP(req); got != "10.0.0.9" {
		t.Errorf("without trust_proxy_headers: got %q, want the peer address 10.0.0.9", got)
	}

	trusting := &Server{cfg: Config{TrustProxyHeaders: true}}
	if got := trusting.clientIP(req); got != "203.0.113.7" {
		t.Errorf("with trust_proxy_headers: got %q, want 203.0.113.7", got)
	}
}

func TestRecorderCapturesStatusAndSize(t *testing.T) {
	rec := &recorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	rec.WriteHeader(http.StatusTeapot)
	rec.WriteHeader(http.StatusInternalServerError) // a second call must not win
	n, err := rec.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if rec.status != http.StatusTeapot {
		t.Errorf("status = %d, want %d", rec.status, http.StatusTeapot)
	}
	if rec.written != 5 {
		t.Errorf("written = %d, want 5", rec.written)
	}
}

func TestRecorderDefaultsToOKOnBareWrite(t *testing.T) {
	rec := &recorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	if _, err := rec.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if rec.status != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.status)
	}
}

func TestWantsJSON(t *testing.T) {
	cases := []struct {
		accept, path string
		want         bool
	}{
		{"text/html,application/xhtml+xml", "/", false}, // a browser gets HTML
		{"application/json", "/", true},
		{"", "/api/v1/tunnels", true},
		{"*/*", "/", true}, // curl with no Accept
		{"text/html", "/api/v1/tunnels", false},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", c.path, nil)
		if c.accept != "" {
			req.Header.Set("Accept", c.accept)
		}
		if got := wantsJSON(req); got != c.want {
			t.Errorf("wantsJSON(accept=%q path=%q) = %v, want %v", c.accept, c.path, got, c.want)
		}
	}
}

func TestRedirectToHTTPSLeavesACMEChallengesAlone(t *testing.T) {
	// Redirecting the challenge path would break certificate issuance for
	// anyone running in http-01 mode behind this listener.
	req := httptest.NewRequest("GET", "/.well-known/acme-challenge/token", nil)
	req.Host = "api-x.example.com"
	w := httptest.NewRecorder()
	redirectToHTTPS(w, req)
	if w.Code == http.StatusMovedPermanently {
		t.Error("ACME challenge paths must not be redirected")
	}

	req = httptest.NewRequest("GET", "/thing?a=1", nil)
	req.Host = "api-x.example.com"
	w = httptest.NewRecorder()
	redirectToHTTPS(w, req)
	if w.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", w.Code)
	}
	if got, want := w.Header().Get("Location"), "https://api-x.example.com/thing?a=1"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 512: "512 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 1048576: "1.0 MiB"}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
