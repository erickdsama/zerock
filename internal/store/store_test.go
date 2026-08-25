package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateAndAuthenticate(t *testing.T) {
	s := newTestStore(t)
	tok, secret, err := s.CreateToken(CreateTokenOpts{Label: "laptop", Scopes: []string{ScopeTunnel}})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if !strings.HasPrefix(secret, "zk_"+tok.ID+"_") {
		t.Errorf("secret %q should embed the id %q", secret, tok.ID)
	}
	// The plaintext secret must never be persisted.
	if strings.Contains(tok.Hash, secret) || tok.Hash == "" {
		t.Errorf("stored hash %q looks wrong", tok.Hash)
	}

	got, err := s.Authenticate(secret)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != tok.ID {
		t.Errorf("got token %q, want %q", got.ID, tok.ID)
	}
}

func TestAuthenticateRejections(t *testing.T) {
	s := newTestStore(t)
	tok, secret, err := s.CreateToken(CreateTokenOpts{Label: "x", Scopes: []string{ScopeTunnel}})
	if err != nil {
		t.Fatal(err)
	}

	// A wrong secret for a real id must fail exactly like an unknown id, so the
	// error cannot be used to discover which ids exist.
	bad := "zk_" + tok.ID + "_wrongsecretwrongsecretwrongsecret"
	for name, presented := range map[string]string{
		"wrong secret":  bad,
		"unknown id":    "zk_nosuchid_" + strings.Repeat("a", 32),
		"not a token":   "hunter2",
		"missing parts": "zk_only",
		"empty":         "",
	} {
		if _, err := s.Authenticate(presented); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: got %v, want ErrNotFound", name, err)
		}
	}

	// The good secret still works, proving the rejections were specific.
	if _, err := s.Authenticate(secret); err != nil {
		t.Errorf("valid secret rejected: %v", err)
	}
}

func TestRevokeStopsAuthentication(t *testing.T) {
	s := newTestStore(t)
	tok, secret, err := s.CreateToken(CreateTokenOpts{Label: "x", Scopes: []string{ScopeTunnel}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeToken(tok.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, err := s.Authenticate(secret); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked token authenticated: %v", err)
	}
	// Revocation is idempotent.
	if err := s.RevokeToken(tok.ID); err != nil {
		t.Errorf("second RevokeToken: %v", err)
	}
	reloaded, err := s.GetToken(tok.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status(time.Now().UTC()) != "revoked" {
		t.Errorf("status = %q, want revoked", reloaded.Status(time.Now().UTC()))
	}
}

func TestExpiredTokenIsRejected(t *testing.T) {
	s := newTestStore(t)
	// A negative TTL is not reachable through the API, so build the expiry by
	// hand to test the boundary the API relies on.
	tok, secret, err := s.CreateToken(CreateTokenOpts{Label: "x", Scopes: []string{ScopeTunnel}})
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Minute)
	tok.ExpiresAt = &past
	if err := s.putToken(tok); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(secret); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired token authenticated: %v", err)
	}
	if got := tok.Status(time.Now().UTC()); got != "expired" {
		t.Errorf("status = %q, want expired", got)
	}
}

func TestCreateTokenValidation(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.CreateToken(CreateTokenOpts{Label: "  ", Scopes: []string{ScopeTunnel}}); !errors.Is(err, ErrBadRequest) {
		t.Errorf("blank label: got %v, want ErrBadRequest", err)
	}
	if _, _, err := s.CreateToken(CreateTokenOpts{Label: "x", Scopes: []string{"root", "superuser"}}); !errors.Is(err, ErrBadRequest) {
		t.Errorf("unknown scopes: got %v, want ErrBadRequest", err)
	}
	// Unknown scopes are dropped rather than trusted.
	tok, _, err := s.CreateToken(CreateTokenOpts{Label: "x", Scopes: []string{"tunnel", "root", "tunnel"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(tok.Scopes) != 1 || tok.Scopes[0] != ScopeTunnel {
		t.Errorf("scopes = %v, want [tunnel]", tok.Scopes)
	}
}

func TestScopes(t *testing.T) {
	plain := &Token{Scopes: []string{ScopeTunnel}}
	if plain.HasScope(ScopeAdmin) {
		t.Error("a tunnel token must not have the admin scope")
	}
	admin := &Token{Scopes: []string{ScopeAdmin}}
	// Admin implies tunnel, so an operator's token can also open tunnels.
	if !admin.HasScope(ScopeTunnel) || !admin.HasScope(ScopeAdmin) {
		t.Error("admin should imply every scope")
	}
}

func TestReservations(t *testing.T) {
	s := newTestStore(t)
	a, _, err := s.CreateToken(CreateTokenOpts{Label: "a", Scopes: []string{ScopeTunnel}})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := s.CreateToken(CreateTokenOpts{Label: "b", Scopes: []string{ScopeTunnel}})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Reserve("api-x", a.ID, "note", 0); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	// Re-reserving one's own subdomain is a no-op, not an error, so a repeated
	// 'zerock reserve' does not fail.
	if _, err := s.Reserve("api-x", a.ID, "", 0); err != nil {
		t.Errorf("re-reserving own subdomain: %v", err)
	}
	if _, err := s.Reserve("api-x", b.ID, "", 0); !errors.Is(err, ErrConflict) {
		t.Errorf("other token reserving: got %v, want ErrConflict", err)
	}

	got, err := s.Reservation("api-x")
	if err != nil || got == nil || got.TokenID != a.ID {
		t.Fatalf("Reservation(api-x) = %+v, %v", got, err)
	}
	if got.Note != "note" {
		t.Errorf("note = %q, want %q", got.Note, "note")
	}

	// An unreserved subdomain is not an error, just absent.
	missing, err := s.Reservation("nothing-here")
	if err != nil || missing != nil {
		t.Errorf("Reservation(nothing-here) = %+v, %v; want nil, nil", missing, err)
	}

	if err := s.Release("api-x"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := s.Release("api-x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Release: got %v, want ErrNotFound", err)
	}
}

func TestReservationLimit(t *testing.T) {
	s := newTestStore(t)
	tok, _, err := s.CreateToken(CreateTokenOpts{Label: "a", Scopes: []string{ScopeTunnel}})
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"one", "two"} {
		if _, err := s.Reserve(sub, tok.ID, "", 2); err != nil {
			t.Fatalf("Reserve(%s): %v", sub, err)
		}
	}
	if _, err := s.Reserve("three", tok.ID, "", 2); !errors.Is(err, ErrBadRequest) {
		t.Errorf("past the limit: got %v, want ErrBadRequest", err)
	}
	// A zero limit means unlimited.
	if _, err := s.Reserve("three", tok.ID, "", 0); err != nil {
		t.Errorf("with no limit: %v", err)
	}
}

func TestDeleteTokenReleasesItsReservations(t *testing.T) {
	s := newTestStore(t)
	tok, _, err := s.CreateToken(CreateTokenOpts{Label: "a", Scopes: []string{ScopeTunnel}})
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := s.CreateToken(CreateTokenOpts{Label: "b", Scopes: []string{ScopeTunnel}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve("mine", tok.ID, "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve("theirs", other.ID, "", 0); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteToken(tok.ID); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}
	if got, _ := s.Reservation("mine"); got != nil {
		t.Error("deleting a token should release the subdomains it held")
	}
	// Another token's reservation must survive.
	if got, _ := s.Reservation("theirs"); got == nil {
		t.Error("deleting a token released an unrelated reservation")
	}
	if err := s.DeleteToken(tok.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting twice: got %v, want ErrNotFound", err)
	}
}

func TestCountAdmins(t *testing.T) {
	s := newTestStore(t)
	if n, err := s.CountAdmins(); err != nil || n != 0 {
		t.Fatalf("fresh store: got %d, %v; want 0", n, err)
	}
	if _, _, err := s.CreateToken(CreateTokenOpts{Label: "t", Scopes: []string{ScopeTunnel}}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountAdmins(); n != 0 {
		t.Errorf("a tunnel token counted as admin: %d", n)
	}
	admin, _, err := s.CreateToken(CreateTokenOpts{Label: "a", Scopes: []string{ScopeAdmin}})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountAdmins(); n != 1 {
		t.Errorf("got %d admins, want 1", n)
	}
	// A revoked admin must not keep the server from bootstrapping a new one.
	if err := s.RevokeToken(admin.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountAdmins(); n != 0 {
		t.Errorf("revoked admin still counted: %d", n)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := s1.CreateToken(CreateTokenOpts{Label: "keep", Scopes: []string{ScopeTunnel}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Reserve("kept", "irrelevant", "", 0); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if _, err := s2.Authenticate(secret); err != nil {
		t.Errorf("token did not survive a restart: %v", err)
	}
	if got, _ := s2.Reservation("kept"); got == nil {
		t.Error("reservation did not survive a restart")
	}
}
