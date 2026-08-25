package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/erickdsama/zerock/internal/client"
	"github.com/erickdsama/zerock/internal/proto"
)

// call issues an API request against the harness's admin door.
func (h *harness) call(t *testing.T, token, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", h.admin, path), reader)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode %s %s (%q): %v", method, path, raw, err)
		}
	}
	return resp.StatusCode, out
}

func TestAPIRequiresABearerToken(t *testing.T) {
	h := startServer(t)

	status, _ := h.call(t, "", "GET", "/api/v1/whoami", nil)
	if status != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", status)
	}
	status, _ = h.call(t, "zk_bogus_"+strings.Repeat("z", 32), "GET", "/api/v1/whoami", nil)
	if status != http.StatusUnauthorized {
		t.Errorf("bad token: status = %d, want 401", status)
	}
	status, out := h.call(t, h.token, "GET", "/api/v1/whoami", nil)
	if status != http.StatusOK {
		t.Fatalf("good token: status = %d (%v)", status, out)
	}
	if out["domain"] != h.domain {
		t.Errorf("domain = %v, want %q", out["domain"], h.domain)
	}
}

func TestAPIRejectsTokenInQueryString(t *testing.T) {
	// Accepting a token as a query parameter would leak it into access logs.
	h := startServer(t)
	status, _ := h.call(t, "", "GET", "/api/v1/whoami?token="+h.token, nil)
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
}

func TestTokenLifecycleOverAPI(t *testing.T) {
	h := startServer(t)

	status, created := h.call(t, h.token, "POST", "/api/v1/tokens", map[string]any{
		"label": "ci", "scopes": []string{"tunnel"}, "max_tunnels": 2,
	})
	if status != http.StatusCreated {
		t.Fatalf("create: status = %d (%v)", status, created)
	}
	secret, _ := created["secret"].(string)
	if !strings.HasPrefix(secret, "zk_") {
		t.Fatalf("secret = %q, want a zk_ token", secret)
	}
	tokenInfo, _ := created["token"].(map[string]any)
	id, _ := tokenInfo["id"].(string)
	if id == "" {
		t.Fatal("no token id was returned")
	}
	// The stored hash must never appear in a response.
	if _, leaked := tokenInfo["hash"]; leaked {
		t.Error("the API response exposed the token hash")
	}

	// The new token works but cannot administer.
	if status, _ := h.call(t, secret, "GET", "/api/v1/whoami", nil); status != http.StatusOK {
		t.Errorf("new token whoami: status = %d", status)
	}
	if status, _ := h.call(t, secret, "GET", "/api/v1/tokens", nil); status != http.StatusForbidden {
		t.Errorf("tunnel token listing tokens: status = %d, want 403", status)
	}

	// Fetching it again must not re-reveal the secret.
	status, fetched := h.call(t, h.token, "GET", "/api/v1/tokens/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("get: status = %d", status)
	}
	if _, leaked := fetched["secret"]; leaked {
		t.Error("a later read returned the secret; it must be shown only once")
	}

	// Revoke, and confirm it stops working.
	if status, _ := h.call(t, h.token, "POST", "/api/v1/tokens/"+id+"/revoke", nil); status != http.StatusOK {
		t.Fatalf("revoke: status = %d", status)
	}
	if status, _ := h.call(t, secret, "GET", "/api/v1/whoami", nil); status != http.StatusUnauthorized {
		t.Errorf("revoked token: status = %d, want 401", status)
	}

	if status, _ := h.call(t, h.token, "DELETE", "/api/v1/tokens/"+id, nil); status != http.StatusNoContent {
		t.Errorf("delete: status = %d, want 204", status)
	}
	if status, _ := h.call(t, h.token, "GET", "/api/v1/tokens/"+id, nil); status != http.StatusNotFound {
		t.Errorf("get after delete: status = %d, want 404", status)
	}
}

func TestAPIRefusesToLockYouOut(t *testing.T) {
	// Revoking or deleting the token you are authenticating with would leave the
	// server unmanageable, so both are refused.
	h := startServer(t)
	_, whoami := h.call(t, h.token, "GET", "/api/v1/whoami", nil)
	info, _ := whoami["token"].(map[string]any)
	id, _ := info["id"].(string)

	if status, _ := h.call(t, h.token, "POST", "/api/v1/tokens/"+id+"/revoke", nil); status != http.StatusBadRequest {
		t.Errorf("self revoke: status = %d, want 400", status)
	}
	if status, _ := h.call(t, h.token, "DELETE", "/api/v1/tokens/"+id, nil); status != http.StatusBadRequest {
		t.Errorf("self delete: status = %d, want 400", status)
	}
}

func TestCreateTokenValidationOverAPI(t *testing.T) {
	h := startServer(t)
	cases := []struct {
		name string
		body map[string]any
	}{
		{"no label", map[string]any{"scopes": []string{"tunnel"}}},
		{"bad duration", map[string]any{"label": "x", "expires_in": "soon"}},
		{"unknown field", map[string]any{"label": "x", "nonsense": true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if status, out := h.call(t, h.token, "POST", "/api/v1/tokens", c.body); status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (%v)", status, out)
			}
		})
	}
}

func TestReservationsOverAPI(t *testing.T) {
	h := startServer(t)

	status, out := h.call(t, h.token, "POST", "/api/v1/reservations", map[string]any{"sub": "api-x", "note": "n"})
	if status != http.StatusCreated {
		t.Fatalf("reserve: status = %d (%v)", status, out)
	}
	if want := "http://api-x.zerock.test"; out["url"] != want {
		t.Errorf("url = %v, want %q", out["url"], want)
	}

	// A server-reserved name can never be claimed.
	if status, _ := h.call(t, h.token, "POST", "/api/v1/reservations", map[string]any{"sub": "www"}); status != http.StatusConflict {
		t.Errorf("reserving www: status = %d, want 409", status)
	}
	// Invalid labels are rejected before they reach the store.
	if status, _ := h.call(t, h.token, "POST", "/api/v1/reservations", map[string]any{"sub": "Bad_Name"}); status != http.StatusBadRequest {
		t.Errorf("invalid subdomain: status = %d, want 400", status)
	}

	// Another token must not be able to take it, or release it.
	_, created := h.call(t, h.token, "POST", "/api/v1/tokens", map[string]any{"label": "other", "scopes": []string{"tunnel"}})
	otherSecret, _ := created["secret"].(string)
	if status, _ := h.call(t, otherSecret, "POST", "/api/v1/reservations", map[string]any{"sub": "api-x"}); status != http.StatusConflict {
		t.Errorf("another token reserving: status = %d, want 409", status)
	}
	if status, _ := h.call(t, otherSecret, "DELETE", "/api/v1/reservations/api-x", nil); status != http.StatusForbidden {
		t.Errorf("another token releasing: status = %d, want 403", status)
	}

	if status, _ := h.call(t, h.token, "DELETE", "/api/v1/reservations/api-x", nil); status != http.StatusNoContent {
		t.Errorf("release: status = %d, want 204", status)
	}
	if status, _ := h.call(t, h.token, "DELETE", "/api/v1/reservations/api-x", nil); status != http.StatusNotFound {
		t.Errorf("release twice: status = %d, want 404", status)
	}
}

func TestReservedSubdomainBlocksAnotherTokensTunnel(t *testing.T) {
	h := startServer(t)

	// A second token reserves a name, then the first tries to tunnel on it.
	_, created := h.call(t, h.token, "POST", "/api/v1/tokens", map[string]any{"label": "owner", "scopes": []string{"tunnel"}})
	ownerSecret, _ := created["secret"].(string)
	if status, _ := h.call(t, ownerSecret, "POST", "/api/v1/reservations", map[string]any{"sub": "owned"}); status != http.StatusCreated {
		t.Fatalf("reserve as the owner: status = %d", status)
	}

	prof := h.profile() // the admin token, which does not hold the reservation
	agent := client.NewAgent(client.AgentOptions{
		Profile: prof, Type: proto.TypeHTTP, LocalPort: freePort(t), Subdomain: "owned",
	}, newRecordingHandler())

	err := agent.Run(t.Context())
	var refused *client.RefusedError
	if !asRefused(err, &refused) {
		t.Fatalf("got %v, want a RefusedError", err)
	}
	if refused.Code != proto.ErrSubReserved {
		t.Errorf("code = %q, want %q", refused.Code, proto.ErrSubReserved)
	}
}

func TestKillingATunnelOverAPI(t *testing.T) {
	h := startServer(t)
	backend := freePort(t)
	handler, ack := startAgent(t, h, client.AgentOptions{
		Type: proto.TypeHTTP, LocalPort: backend, Subdomain: "doomed",
	})

	status, out := h.call(t, h.token, "GET", "/api/v1/tunnels", nil)
	if status != http.StatusOK {
		t.Fatalf("list: status = %d", status)
	}
	tunnels, _ := out["tunnels"].([]any)
	if len(tunnels) != 1 {
		t.Fatalf("got %d tunnels, want 1", len(tunnels))
	}

	if status, _ := h.call(t, h.token, "DELETE", "/api/v1/tunnels/"+ack.TunnelID, nil); status != http.StatusNoContent {
		t.Fatalf("kill: status = %d, want 204", status)
	}
	if status, _ := h.call(t, h.token, "DELETE", "/api/v1/tunnels/"+ack.TunnelID, nil); status != http.StatusNotFound {
		t.Errorf("kill twice: status = %d, want 404", status)
	}

	// The agent must be told this was deliberate so it does not reconnect.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if closes := handler.eventsOfType(proto.EventClose); len(closes) > 0 {
			if !closes[0].Final {
				t.Error("an API kill should be marked final")
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the agent was never told the tunnel closed")
}

func TestHealthzNeedsNoToken(t *testing.T) {
	h := startServer(t)
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", h.admin))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["ok"] != true {
		t.Errorf("ok = %v, want true", out["ok"])
	}
}

func TestDashboardServedToBrowsers(t *testing.T) {
	h := startServer(t)

	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/", h.admin), nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	page := string(body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(page, "<title>zerock</title>") {
		t.Error("the dashboard page was not served")
	}

	// The page holds a token in the browser, so these are not optional.
	for header, want := range map[string]string{
		"X-Frame-Options": "DENY",
		"Cache-Control":   "no-store",
		"Referrer-Policy": "no-referrer",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	csp := resp.Header.Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'none'", "connect-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP is missing %q; got %q", directive, csp)
		}
	}
}

func TestDashboardIsSelfContained(t *testing.T) {
	// A reference to any external host would be blocked by the page's own CSP,
	// and would break on a server with no outbound access.
	h := startServer(t)
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/", h.admin), nil)
	req.Header.Set("Accept", "text/html")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	external := regexp.MustCompile(`(?i)(src|href)\s*=\s*["']?(https?:)?//`)
	if loc := external.FindString(string(body)); loc != "" {
		t.Errorf("the page references an external asset: %q", loc)
	}
}

func TestAPIClientsStillGetJSONAtRoot(t *testing.T) {
	// A script pointed at the API host must not be handed HTML.
	h := startServer(t)
	status, out := h.call(t, "", "GET", "/", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if out["service"] != "zerock" {
		t.Errorf("service = %v, want zerock", out["service"])
	}
	if out["api"] != "/api/v1" {
		t.Errorf("api = %v, want /api/v1", out["api"])
	}
}

func TestUnknownPathOnTheAPIHostIs404(t *testing.T) {
	h := startServer(t)
	if status, _ := h.call(t, "", "GET", "/nope", nil); status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}
