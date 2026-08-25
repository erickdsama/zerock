package server

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/erickdsama/zerock/internal/namegen"
	"github.com/erickdsama/zerock/internal/proto"
	"github.com/erickdsama/zerock/internal/store"
	"github.com/erickdsama/zerock/internal/version"
	"github.com/hashicorp/yamux"
)

// handshakeTimeout bounds how long an unauthenticated connection may occupy a
// slot before it must complete its Hello.
const handshakeTimeout = 15 * time.Second

// serveControl accepts agent sessions until the listener closes.
func (s *Server) serveControl(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.stopping.Load() {
				return
			}
			s.log.Error("control accept failed", "err", err)
			return
		}
		go s.handleControlConn(conn)
	}
}

// handleControlConn runs one agent session start to finish.
func (s *Server) handleControlConn(conn net.Conn) {
	remote := conn.RemoteAddr().String()
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return
	}
	var hello proto.Hello
	if err := proto.ReadMsg(conn, &hello); err != nil {
		s.log.Debug("handshake read failed", "remote", remote, "err", err)
		return
	}

	tun, tok, ack := s.authorize(hello, remote)
	if !ack.OK {
		s.log.Info("tunnel refused", "remote", remote, "error", ack.Error, "sub", hello.Subdomain)
		_ = proto.WriteMsg(conn, ack)
		return
	}
	if err := proto.WriteMsg(conn, ack); err != nil {
		s.releaseTunnel(tun)
		return
	}
	// The handshake is done; from here the session lives as long as the agent
	// keeps it open, so the deadline must go.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		s.releaseTunnel(tun)
		return
	}

	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 20 * time.Second
	cfg.ConnectionWriteTimeout = 20 * time.Second
	cfg.LogOutput = io.Discard
	// The server opens streams, so it takes the yamux client role.
	session, err := yamux.Client(conn, cfg)
	if err != nil {
		s.log.Error("yamux setup failed", "remote", remote, "err", err)
		s.releaseTunnel(tun)
		return
	}
	tun.session = session

	// The event stream is opened eagerly so the agent can print request logs
	// from the first request onward.
	if ev, err := session.Open(); err == nil {
		if _, err := ev.Write([]byte{byte(proto.KindEvents)}); err == nil {
			tun.attachEvents(ev)
		} else {
			ev.Close()
		}
	}

	// From here the session can carry traffic, so the tunnel becomes routable.
	// Nothing slow may run before this point: an agent has already been told its
	// URL and requests can be arriving.
	tun.markReady()
	_ = s.store.TouchToken(tok.ID)

	if tun.Type == proto.TypeTCP {
		tun.mu.Lock()
		ln := tun.listener
		tun.mu.Unlock()
		if ln == nil {
			s.log.Error("tcp tunnel has no listener", "sub", tun.Subdomain)
			s.releaseTunnel(tun)
			return
		}
		go s.acceptTCP(tun, ln)
	}

	s.log.Info("tunnel open",
		"id", tun.ID, "sub", tun.Subdomain, "type", tun.Type, "url", tun.URL,
		"token", tok.ID, "label", tok.Label, "remote", remote)

	// Block until the agent goes away or the tunnel is closed from the API.
	<-session.CloseChan()

	stats := tun.Stats()
	s.log.Info("tunnel closed",
		"id", tun.ID, "sub", tun.Subdomain, "requests", stats.Requests,
		"bytes_in", stats.BytesIn, "bytes_out", stats.BytesOut,
		"uptime", time.Since(tun.StartedAt).Truncate(time.Second).String())
	s.releaseTunnel(tun)
}

// releaseTunnel deregisters and tears down a tunnel.
func (s *Server) releaseTunnel(t *Tunnel) {
	if t == nil {
		return
	}
	s.reg.remove(t)
	t.Close("", false)
}

// authorize validates a Hello and, on success, returns a registered tunnel.
// A refusal carries a machine-readable code so the CLI can explain itself.
func (s *Server) authorize(hello proto.Hello, remote string) (*Tunnel, *store.Token, proto.HelloAck) {
	refuse := func(code, msg string) proto.HelloAck {
		return proto.HelloAck{OK: false, Error: code, Message: msg, ServerVersion: version.Version}
	}

	if hello.V != proto.Version {
		return nil, nil, refuse(proto.ErrVersionMismatch,
			fmt.Sprintf("agent speaks protocol v%d, server speaks v%d; upgrade the side that is behind", hello.V, proto.Version))
	}
	switch hello.Type {
	case proto.TypeHTTP, proto.TypeTCP:
	default:
		return nil, nil, refuse(proto.ErrBadRequest, fmt.Sprintf("unknown tunnel type %q", hello.Type))
	}
	if hello.LocalPort < 1 || hello.LocalPort > 65535 {
		return nil, nil, refuse(proto.ErrBadRequest, "local port must be between 1 and 65535")
	}

	tok, err := s.store.Authenticate(hello.Token)
	if err != nil {
		return nil, nil, refuse(proto.ErrUnauthorized, "token is missing, expired, revoked or invalid")
	}
	if !tok.HasScope(store.ScopeTunnel) {
		return nil, nil, refuse(proto.ErrUnauthorized, "token lacks the tunnel scope")
	}
	if tok.MaxTunnels > 0 && s.reg.countForToken(tok.ID) >= tok.MaxTunnels {
		return nil, nil, refuse(proto.ErrTunnelLimit,
			fmt.Sprintf("token already holds its limit of %d concurrent tunnels", tok.MaxTunnels))
	}

	sub, ack := s.resolveSubdomain(hello, tok)
	if !ack.OK {
		return nil, nil, ack
	}

	localHost := hello.LocalHost
	if localHost == "" {
		localHost = "127.0.0.1"
	}
	tun := newTunnel()
	tun.Subdomain = sub
	tun.Type = hello.Type
	tun.LocalAddr = hostPort(localHost, hello.LocalPort)
	tun.TokenID = tok.ID
	tun.TokenLabel = tok.Label
	tun.AgentHost = hello.Hostname
	tun.AgentVer = hello.Agent
	tun.RemoteIP = hostOnly(remote)
	tun.StartedAt = time.Now().UTC()
	tun.basicAuth = hello.BasicAuth
	tun.URL = fmt.Sprintf("%s://%s.%s", s.cfg.scheme(), sub, s.cfg.Domain)

	switch hello.Type {
	case proto.TypeTCP:
		ln, port, err := s.bindTCP(hello.RemotePort)
		if err != nil {
			return nil, nil, refuse(proto.ErrNoPorts, err.Error())
		}
		tun.PublicPort = port
		tun.listener = ln
		tun.URL = fmt.Sprintf("tcp://%s.%s:%d", sub, s.cfg.Domain, port)
	case proto.TypeHTTP:
		// Built before the tunnel is registered, so the edge can never find a
		// tunnel whose proxy is still nil.
		tun.proxy = s.buildProxy(tun)
	}

	if err := s.reg.add(tun); err != nil {
		tun.Close("", false) // releases the port bound just above
		return nil, nil, refuse(proto.ErrSubTaken,
			fmt.Sprintf("%s.%s is already serving a live tunnel", sub, s.cfg.Domain))
	}

	return tun, tok, proto.HelloAck{
		OK:            true,
		TunnelID:      tun.ID,
		Subdomain:     sub,
		URL:           tun.URL,
		PublicAddr:    tcpPublicAddr(tun, s.cfg.Domain),
		ServerVersion: version.Version,
	}
}

// resolveSubdomain decides which label the tunnel gets: the requested one if
// the token may have it, otherwise a fresh random name.
func (s *Server) resolveSubdomain(hello proto.Hello, tok *store.Token) (string, proto.HelloAck) {
	ok := func(sub string) (string, proto.HelloAck) { return sub, proto.HelloAck{OK: true} }
	refuse := func(code, msg string) (string, proto.HelloAck) {
		return "", proto.HelloAck{OK: false, Error: code, Message: msg, ServerVersion: version.Version}
	}

	requested := strings.ToLower(strings.TrimSpace(hello.Subdomain))
	if requested == "" {
		// Random names collide rarely; a handful of attempts is plenty.
		for range 12 {
			candidate := namegen.New()
			if s.cfg.IsReserved(candidate) {
				continue
			}
			if _, live := s.reg.lookup(candidate); live {
				continue
			}
			res, err := s.store.Reservation(candidate)
			if err != nil || res != nil {
				continue
			}
			return ok(candidate)
		}
		return refuse(proto.ErrInternal, "could not find a free random subdomain; try again")
	}

	if !namegen.ValidSubdomain(requested) {
		return refuse(proto.ErrSubInvalid,
			"subdomain must be 1-63 characters of lowercase letters, digits and single hyphens")
	}
	if s.cfg.IsReserved(requested) {
		return refuse(proto.ErrSubBlocked, fmt.Sprintf("%q is reserved by the server and cannot be tunneled", requested))
	}
	res, err := s.store.Reservation(requested)
	if err != nil {
		return refuse(proto.ErrInternal, "could not read reservations")
	}
	if res != nil && res.TokenID != tok.ID {
		return refuse(proto.ErrSubReserved,
			fmt.Sprintf("%q is reserved by another token", requested))
	}
	if _, live := s.reg.lookup(requested); live {
		return refuse(proto.ErrSubTaken,
			fmt.Sprintf("%s.%s is already serving a live tunnel", requested, s.cfg.Domain))
	}
	return ok(requested)
}

// tcpPublicAddr renders the dialable address of a TCP tunnel.
func tcpPublicAddr(t *Tunnel, domain string) string {
	if t.Type != proto.TypeTCP {
		return ""
	}
	return fmt.Sprintf("%s.%s:%d", t.Subdomain, domain, t.PublicPort)
}

// hostOnly strips the port from a remote address, tolerating malformed input.
func hostOnly(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// errClosed reports whether an error is just a closed connection, which is the
// normal end of a session rather than a fault worth logging.
func errClosed(err error) bool {
	return err == nil ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, yamux.ErrSessionShutdown) ||
		errors.Is(err, yamux.ErrStreamClosed)
}
