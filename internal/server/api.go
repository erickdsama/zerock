package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/erickdsama/zerock/internal/namegen"
	"github.com/erickdsama/zerock/internal/store"
	"github.com/erickdsama/zerock/internal/version"
)

// ctxKey is the private type for values this package puts on a request context.
type ctxKey int

const ctxToken ctxKey = iota

// newAPIHandler builds the management API. The same handler is mounted on the
// public API host and on the loopback admin address, so an operator locked out
// of DNS or TLS can still manage tokens.
func (s *Server) newAPIHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"version": version.Version,
			"domain":  s.cfg.Domain,
			"tunnels": len(s.reg.list("")),
			"uptime":  time.Since(s.startedAt).Truncate(time.Second).String(),
		})
	})

	mux.Handle("GET /api/v1/whoami", s.authed(store.ScopeTunnel, s.handleWhoami))

	mux.Handle("GET /api/v1/tokens", s.authed(store.ScopeAdmin, s.handleListTokens))
	mux.Handle("POST /api/v1/tokens", s.authed(store.ScopeAdmin, s.handleCreateToken))
	mux.Handle("GET /api/v1/tokens/{id}", s.authed(store.ScopeAdmin, s.handleGetToken))
	mux.Handle("POST /api/v1/tokens/{id}/revoke", s.authed(store.ScopeAdmin, s.handleRevokeToken))
	mux.Handle("DELETE /api/v1/tokens/{id}", s.authed(store.ScopeAdmin, s.handleDeleteToken))

	mux.Handle("GET /api/v1/tunnels", s.authed(store.ScopeTunnel, s.handleListTunnels))
	mux.Handle("DELETE /api/v1/tunnels/{id}", s.authed(store.ScopeTunnel, s.handleCloseTunnel))

	mux.Handle("GET /api/v1/reservations", s.authed(store.ScopeTunnel, s.handleListReservations))
	mux.Handle("POST /api/v1/reservations", s.authed(store.ScopeTunnel, s.handleReserve))
	mux.Handle("DELETE /api/v1/reservations/{sub}", s.authed(store.ScopeTunnel, s.handleRelease))

	mux.HandleFunc("/", s.handleLanding)
	return mux
}

// authed wraps a handler with bearer-token authentication and a scope check.
func (s *Server) authed(scope string, next func(http.ResponseWriter, *http.Request, *store.Token)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		raw, ok := strings.CutPrefix(header, "Bearer ")
		if !ok {
			// A query parameter is never accepted: it would end up in access
			// logs and browser history.
			w.Header().Set("WWW-Authenticate", `Bearer realm="zerock"`)
			writeAPIError(w, http.StatusUnauthorized, "unauthorized", "supply a token as: Authorization: Bearer zk_...")
			return
		}
		tok, err := s.store.Authenticate(strings.TrimSpace(raw))
		if err != nil {
			writeAPIError(w, http.StatusUnauthorized, "unauthorized", "token is missing, expired, revoked or invalid")
			return
		}
		if !tok.HasScope(scope) {
			writeAPIError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("token lacks the %s scope", scope))
			return
		}
		_ = s.store.TouchToken(tok.ID)
		next(w, r, tok)
	})
}

// tokenDTO is the API view of a token. It exists so the stored secret hash can
// never be serialized into a response by accident.
type tokenDTO struct {
	ID              string     `json:"id"`
	Label           string     `json:"label"`
	Scopes          []string   `json:"scopes"`
	Status          string     `json:"status"`
	CreatedAt       time.Time  `json:"created_at"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
	LastUsed        *time.Time `json:"last_used_at,omitempty"`
	MaxTunnels      int        `json:"max_tunnels"`
	MaxReservations int        `json:"max_reservations"`
	ActiveTunnels   int        `json:"active_tunnels"`
}

func (s *Server) toDTO(t *store.Token) tokenDTO {
	return tokenDTO{
		ID: t.ID, Label: t.Label, Scopes: t.Scopes,
		Status: t.Status(time.Now().UTC()), CreatedAt: t.CreatedAt,
		ExpiresAt: t.ExpiresAt, RevokedAt: t.RevokedAt, LastUsed: t.LastUsed,
		MaxTunnels: t.MaxTunnels, MaxReservations: t.MaxReservations,
		ActiveTunnels: s.reg.countForToken(t.ID),
	}
}

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request, tok *store.Token) {
	writeJSON(w, http.StatusOK, map[string]any{
		"token":          s.toDTO(tok),
		"domain":         s.cfg.Domain,
		"server_version": version.Version,
	})
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request, _ *store.Token) {
	tokens, err := s.store.ListTokens()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]tokenDTO, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, s.toDTO(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// createTokenRequest is the body of POST /api/v1/tokens.
type createTokenRequest struct {
	Label  string   `json:"label"`
	Scopes []string `json:"scopes"`
	// ExpiresIn is a Go duration such as "720h". Empty means no expiry.
	ExpiresIn       string `json:"expires_in"`
	MaxTunnels      int    `json:"max_tunnels"`
	MaxReservations int    `json:"max_reservations"`
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request, _ *store.Token) {
	var req createTokenRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if len(req.Scopes) == 0 {
		req.Scopes = []string{store.ScopeTunnel}
	}
	var ttl time.Duration
	if req.ExpiresIn != "" {
		parsed, err := time.ParseDuration(req.ExpiresIn)
		if err != nil || parsed <= 0 {
			writeAPIError(w, http.StatusBadRequest, "bad_request", "expires_in must be a positive duration such as 720h")
			return
		}
		ttl = parsed
	}

	tok, secret, err := s.store.CreateToken(store.CreateTokenOpts{
		Label:           req.Label,
		Scopes:          req.Scopes,
		TTL:             ttl,
		MaxTunnels:      req.MaxTunnels,
		MaxReservations: req.MaxReservations,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.log.Info("token created", "id", tok.ID, "label", tok.Label, "scopes", tok.Scopes)
	writeJSON(w, http.StatusCreated, map[string]any{
		"token": s.toDTO(tok),
		// The only time the secret is ever returned.
		"secret":  secret,
		"warning": "store this secret now; it cannot be retrieved again",
	})
}

func (s *Server) handleGetToken(w http.ResponseWriter, r *http.Request, _ *store.Token) {
	tok, err := s.store.GetToken(r.PathValue("id"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": s.toDTO(tok)})
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request, actor *store.Token) {
	id := r.PathValue("id")
	if id == actor.ID {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "refusing to revoke the token making this request")
		return
	}
	if err := s.store.RevokeToken(id); err != nil {
		s.writeStoreError(w, err)
		return
	}
	// Revocation must take effect immediately, not at the next handshake.
	s.dropTunnelsForToken(id, "token revoked")
	s.log.Info("token revoked", "id", id, "by", actor.ID)
	tok, err := s.store.GetToken(id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": s.toDTO(tok)})
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request, actor *store.Token) {
	id := r.PathValue("id")
	if id == actor.ID {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "refusing to delete the token making this request")
		return
	}
	if err := s.store.DeleteToken(id); err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.dropTunnelsForToken(id, "token deleted")
	s.log.Info("token deleted", "id", id, "by", actor.ID)
	w.WriteHeader(http.StatusNoContent)
}

// dropTunnelsForToken closes every live tunnel a token holds.
func (s *Server) dropTunnelsForToken(tokenID, reason string) {
	for _, t := range s.reg.list(tokenID) {
		s.reg.remove(t)
		t.Close(reason, true)
	}
}

func (s *Server) handleListTunnels(w http.ResponseWriter, r *http.Request, tok *store.Token) {
	// Admins see the whole server; a plain token sees only what it owns.
	filter := tok.ID
	if tok.HasScope(store.ScopeAdmin) && r.URL.Query().Get("mine") != "true" {
		filter = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"tunnels": s.reg.list(filter)})
}

func (s *Server) handleCloseTunnel(w http.ResponseWriter, r *http.Request, tok *store.Token) {
	tun, ok := s.reg.byID(r.PathValue("id"))
	if !ok {
		writeAPIError(w, http.StatusNotFound, "not_found", "no live tunnel with that id")
		return
	}
	if tun.TokenID != tok.ID && !tok.HasScope(store.ScopeAdmin) {
		writeAPIError(w, http.StatusForbidden, "forbidden", "that tunnel belongs to another token")
		return
	}
	s.reg.remove(tun)
	tun.Close("closed from the API", true)
	s.log.Info("tunnel closed via api", "id", tun.ID, "sub", tun.Subdomain, "by", tok.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListReservations(w http.ResponseWriter, r *http.Request, tok *store.Token) {
	filter := tok.ID
	if tok.HasScope(store.ScopeAdmin) && r.URL.Query().Get("mine") != "true" {
		filter = ""
	}
	list, err := s.store.ListReservations(filter)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if list == nil {
		list = []*store.Reservation{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"reservations": list, "domain": s.cfg.Domain})
}

// reserveRequest is the body of POST /api/v1/reservations.
type reserveRequest struct {
	Subdomain string `json:"sub"`
	Note      string `json:"note"`
	// TokenID lets an admin reserve on another token's behalf.
	TokenID string `json:"token_id"`
}

func (s *Server) handleReserve(w http.ResponseWriter, r *http.Request, tok *store.Token) {
	var req reserveRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	sub := strings.ToLower(strings.TrimSpace(req.Subdomain))
	if !namegen.ValidSubdomain(sub) {
		writeAPIError(w, http.StatusBadRequest, "bad_request",
			"sub must be 1-63 characters of lowercase letters, digits and single hyphens")
		return
	}
	if s.cfg.IsReserved(sub) {
		writeAPIError(w, http.StatusConflict, "reserved", fmt.Sprintf("%q is reserved by the server", sub))
		return
	}

	owner := tok.ID
	limit := effectiveReservationLimit(tok, s.cfg.MaxReservationsPerToken)
	if req.TokenID != "" && req.TokenID != tok.ID {
		if !tok.HasScope(store.ScopeAdmin) {
			writeAPIError(w, http.StatusForbidden, "forbidden", "only an admin token can reserve for another token")
			return
		}
		target, err := s.store.GetToken(req.TokenID)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		owner = target.ID
		limit = effectiveReservationLimit(target, s.cfg.MaxReservationsPerToken)
	}

	res, err := s.store.Reserve(sub, owner, req.Note, limit)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.log.Info("subdomain reserved", "sub", sub, "token", owner, "by", tok.ID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"reservation": res,
		"url":         fmt.Sprintf("%s://%s.%s", s.cfg.scheme(), sub, s.cfg.Domain),
	})
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request, tok *store.Token) {
	sub := strings.ToLower(r.PathValue("sub"))
	res, err := s.store.Reservation(sub)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if res == nil {
		writeAPIError(w, http.StatusNotFound, "not_found", fmt.Sprintf("%q is not reserved", sub))
		return
	}
	if res.TokenID != tok.ID && !tok.HasScope(store.ScopeAdmin) {
		writeAPIError(w, http.StatusForbidden, "forbidden", "that reservation belongs to another token")
		return
	}
	if err := s.store.Release(sub); err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.log.Info("reservation released", "sub", sub, "by", tok.ID)
	w.WriteHeader(http.StatusNoContent)
}

// effectiveReservationLimit resolves a token's cap against the server default.
func effectiveReservationLimit(tok *store.Token, serverDefault int) int {
	if tok.MaxReservations > 0 {
		return tok.MaxReservations
	}
	if tok.HasScope(store.ScopeAdmin) {
		return 0 // unlimited
	}
	return serverDefault
}

// handleLanding answers requests to the API host that are not API calls. A
// browser gets the dashboard; anything else gets a description of the API.
func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.writeEdgeError(w, r, http.StatusNotFound, "not found", "No such endpoint on this zerock server.")
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{
			"service": "zerock", "version": version.Version, "domain": s.cfg.Domain,
			"api": "/api/v1", "health": "/healthz", "dashboard": "/",
		})
		return
	}

	// The page holds a token in the browser, so it is locked down: no framing, no
	// referrer leakage, and a CSP that permits only the inline stylesheet and
	// script it ships with and calls back to this origin alone.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; "+
			"connect-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// A credentialed page must never be cached by a shared proxy.
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, dashboardPage)
}

// --- helpers ---

// writeJSON writes v with the given status.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// writeAPIError writes a machine-readable failure.
func writeAPIError(w http.ResponseWriter, code int, kind, msg string) {
	writeJSON(w, code, map[string]string{"error": kind, "message": msg})
}

// writeStoreError maps store errors onto HTTP status codes.
func (s *Server) writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "not_found", "no such record")
	case errors.Is(err, store.ErrConflict):
		writeAPIError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, store.ErrBadRequest):
		writeAPIError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		s.log.Error("api failure", "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal", "the server could not complete the request")
	}
}

// decodeJSON reads a request body, rejecting unknown fields so a typo in a
// field name is reported instead of silently ignored. An empty body is allowed.
func decodeJSON(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// wantsJSON guesses whether the caller is a program rather than a browser.
func wantsJSON(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/json") {
		return true
	}
	if strings.Contains(accept, "text/html") {
		return false
	}
	return strings.HasPrefix(r.URL.Path, "/api/") || accept == "" || accept == "*/*"
}

// htmlEscape is a local alias so templates read cleanly.
func htmlEscape(s string) string { return html.EscapeString(s) }
