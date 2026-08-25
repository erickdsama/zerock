// Package server implements the zerock tunnel server: the control plane agents
// dial into, the public HTTP/TCP edge, and the management API.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/erickdsama/zerock/internal/store"
	"github.com/erickdsama/zerock/internal/version"
)

// Server is a running zerock server.
type Server struct {
	cfg   Config
	log   *slog.Logger
	store *store.Store
	reg   *registry

	startedAt time.Time
	stopping  atomic.Bool

	listeners []net.Listener
	servers   []*http.Server
}

// New opens the data directory and prepares a server. Call Run to serve.
func New(cfg Config, log *slog.Logger) (*Server, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	db, err := store.Open(filepath.Join(cfg.DataDir, "zerock.db"))
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:       cfg,
		log:       log,
		store:     db,
		reg:       newRegistry(),
		startedAt: time.Now(),
	}, nil
}

// Close releases the database.
func (s *Server) Close() error { return s.store.Close() }

// Bootstrap mints a first admin token when none exists, returning the secret so
// the caller can show it. An empty string means one already existed.
func (s *Server) Bootstrap() (string, error) {
	n, err := s.store.CountAdmins()
	if err != nil {
		return "", err
	}
	if n > 0 {
		return "", nil
	}
	_, secret, err := s.store.CreateToken(store.CreateTokenOpts{
		Label:  "bootstrap-admin",
		Scopes: []string{store.ScopeAdmin, store.ScopeTunnel},
	})
	if err != nil {
		return "", fmt.Errorf("bootstrap admin token: %w", err)
	}
	return secret, nil
}

// Run serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	tlsCfg, err := buildTLS(ctx, s.cfg, s.log)
	if err != nil {
		return err
	}

	frontend := s.newFrontend()

	// Control plane: agents dial in here.
	controlLn, err := net.Listen("tcp", s.cfg.ControlAddr)
	if err != nil {
		return fmt.Errorf("listen on control address %s: %w", s.cfg.ControlAddr, err)
	}
	if tlsCfg != nil {
		// A separate config avoids advertising HTTP ALPN on a non-HTTP port.
		controlTLS := tlsCfg.Clone()
		controlTLS.NextProtos = nil
		controlLn = tls.NewListener(controlLn, controlTLS)
	} else {
		s.log.Warn("control plane is running without TLS; agent tokens will cross the network in cleartext",
			"hint", "set tls.mode to auto or files, or terminate TLS in front of this port")
	}
	s.listeners = append(s.listeners, controlLn)
	go s.serveControl(controlLn)
	s.log.Info("control plane listening", "addr", s.cfg.ControlAddr, "tls", tlsCfg != nil)

	// Public edge.
	if tlsCfg == nil {
		if err := s.serveHTTP(s.cfg.HTTPAddr, frontend, nil, "edge"); err != nil {
			return err
		}
	} else {
		if err := s.serveHTTP(s.cfg.HTTPSAddr, frontend, tlsCfg, "edge"); err != nil {
			return err
		}
		// Port 80 only redirects. Certificates come from DNS-01, so nothing
		// here is load-bearing for renewal and a bind failure is survivable.
		if err := s.serveHTTP(s.cfg.HTTPAddr, http.HandlerFunc(redirectToHTTPS), nil, "redirect"); err != nil {
			s.log.Warn("could not bind the HTTP redirect listener; HTTPS is unaffected", "err", err)
		}
	}

	// Loopback management door, always available regardless of DNS or TLS state.
	if s.cfg.AdminAddr != "" {
		if err := s.serveHTTP(s.cfg.AdminAddr, s.newAPIHandler(), nil, "admin"); err != nil {
			return err
		}
	}

	s.log.Info("zerock is up",
		"version", version.String(), "domain", s.cfg.Domain,
		"api_host", s.cfg.APIHost, "tls", s.cfg.TLS.Mode)

	<-ctx.Done()
	return s.shutdown()
}

// serveHTTP starts one HTTP listener and tracks it for shutdown.
func (s *Server) serveHTTP(addr string, h http.Handler, tlsCfg *tls.Config, role string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s (%s): %w", addr, role, err)
	}
	srv := &http.Server{
		Handler:   h,
		TLSConfig: tlsCfg,
		// Headers must arrive promptly, but bodies and responses are left
		// unbounded: tunnelled traffic includes uploads, downloads, SSE and
		// WebSockets that legitimately stay open for a long time.
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.With("role", role).Handler(), slog.LevelDebug),
	}
	s.listeners = append(s.listeners, ln)
	s.servers = append(s.servers, srv)

	go func() {
		var err error
		if tlsCfg != nil {
			err = srv.ServeTLS(ln, "", "")
		} else {
			err = srv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !s.stopping.Load() {
			s.log.Error("listener stopped", "role", role, "addr", addr, "err", err)
		}
	}()
	s.log.Info("listening", "role", role, "addr", addr, "tls", tlsCfg != nil)
	return nil
}

// shutdown stops accepting, tells every agent why, and drains.
func (s *Server) shutdown() error {
	s.stopping.Store(true)
	s.log.Info("shutting down")

	for _, ln := range s.listeners {
		_ = ln.Close()
	}
	s.reg.closeAll("server is shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range s.servers {
		_ = srv.Shutdown(ctx)
	}
	return nil
}
