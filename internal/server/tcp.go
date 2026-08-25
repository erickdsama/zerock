package server

import (
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/erickdsama/zerock/internal/proto"
)

// bindTCP reserves and binds a public port for a TCP tunnel.
//
// The bind happens before the agent is told its public address, so a port
// conflict surfaces as a refusal instead of an address that is advertised but
// refuses connections.
func (s *Server) bindTCP(requested int) (net.Listener, int, error) {
	rng := s.cfg.TCPPortRange

	if requested != 0 {
		if requested < rng.From || requested > rng.To {
			return nil, 0, fmt.Errorf("port %d is outside the server's range %d-%d", requested, rng.From, rng.To)
		}
		if s.reg.portTaken(requested) {
			return nil, 0, fmt.Errorf("port %d is already in use", requested)
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", requested))
		if err != nil {
			return nil, 0, fmt.Errorf("port %d could not be opened: %w", requested, err)
		}
		return ln, requested, nil
	}

	// Walk the range rather than trusting the registry alone: another process on
	// the host may hold a port zerock knows nothing about.
	for port := rng.From; port <= rng.To; port++ {
		if s.reg.portTaken(port) {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			continue
		}
		return ln, port, nil
	}
	return nil, 0, fmt.Errorf("no free port in %d-%d", rng.From, rng.To)
}

// acceptTCP splices each inbound connection onto its own stream.
func (s *Server) acceptTCP(tun *Tunnel, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			// A closed listener is the normal way this loop ends.
			return
		}
		go s.pipeTCP(tun, conn)
	}
}

// pipeTCP joins one public connection to one agent stream.
func (s *Server) pipeTCP(tun *Tunnel, public net.Conn) {
	defer public.Close()
	remote := hostOnly(public.RemoteAddr().String())
	started := time.Now()

	stream, err := tun.openStream()
	if err != nil {
		s.log.Debug("tcp stream open failed", "sub", tun.Subdomain, "err", err)
		return
	}
	defer stream.Close()

	tun.emit(proto.Event{T: proto.EventConn, Remote: remote, Message: "connection opened"})

	toAgent, toPublic := splice(public, stream)

	tun.recordConn(toAgent, toPublic)
	tun.emit(proto.Event{
		T:      proto.EventConn,
		Remote: remote,
		Bytes:  toPublic,
		Millis: float64(time.Since(started).Microseconds()) / 1000,
		Message: fmt.Sprintf("connection closed after %s (%s in, %s out)",
			time.Since(started).Truncate(time.Millisecond), humanBytes(toAgent), humanBytes(toPublic)),
	})
}

// splice copies in both directions until either side finishes, returning the
// bytes moved from a to b and from b to a. Each direction is half-closed as it
// drains so protocols that signal with EOF still work.
func splice(a, b net.Conn) (aToB, bToA int64) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		aToB, _ = io.Copy(b, a)
		halfClose(b)
	}()
	go func() {
		defer wg.Done()
		bToA, _ = io.Copy(a, b)
		halfClose(a)
	}()

	wg.Wait()
	return aToB, bToA
}

// halfClose shuts down the write side if the connection supports it, and falls
// back to a full close otherwise (yamux streams do not implement CloseWrite).
func halfClose(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// humanBytes renders a byte count compactly for log lines.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", value)
}
