package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erickdsama/zerock/internal/proto"
	"github.com/erickdsama/zerock/internal/version"
	"github.com/hashicorp/yamux"
)

// AgentOptions describes the tunnel to request.
type AgentOptions struct {
	Profile   Profile
	Type      proto.TunnelType
	LocalHost string
	LocalPort int
	Subdomain string
	// RemotePort requests a specific public port for a TCP tunnel.
	RemotePort int
	// BasicAuth is "user:pass" enforced at the edge before traffic is forwarded.
	BasicAuth string
	// Reconnect keeps retrying after the session drops. The subdomain granted on
	// the first connection is reused, so the public URL survives a reconnect.
	Reconnect bool
}

// Handler receives agent lifecycle notifications so the CLI can render them.
// Every method may be called from a different goroutine.
type Handler interface {
	// OnConnect fires each time a tunnel is established.
	OnConnect(ack proto.HelloAck, reconnected bool)
	// OnEvent fires for each server-pushed event.
	OnEvent(ev proto.Event)
	// OnDisconnect fires when a session drops. retryIn is zero when the agent
	// is giving up.
	OnDisconnect(err error, retryIn time.Duration)
}

// Agent maintains one tunnel session against a zerock server.
type Agent struct {
	opts AgentOptions
	h    Handler

	mu sync.Mutex
	// grantedSub is the subdomain the server assigned, remembered so a
	// reconnect asks for the same name rather than a fresh random one.
	grantedSub string

	// stopRetry is set when the server closes the tunnel deliberately, for
	// instance because an operator killed it or the token was revoked.
	// Reconnecting then would just undo their decision.
	stopRetry atomic.Bool
}

// NewAgent prepares an agent. Nothing connects until Run is called.
func NewAgent(opts AgentOptions, h Handler) *Agent {
	if opts.LocalHost == "" {
		opts.LocalHost = "127.0.0.1"
	}
	return &Agent{opts: opts, h: h}
}

// RefusedError is a tunnel the server declined. It carries the machine-readable
// code so the CLI can add advice for the common cases.
type RefusedError struct {
	Code    string
	Message string
}

func (e *RefusedError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

// Run connects and serves until ctx is cancelled, or until the session drops
// when Reconnect is false.
func (a *Agent) Run(ctx context.Context) error {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	first := true

	for {
		err := a.session(ctx, first)

		if ctx.Err() != nil {
			return nil
		}
		// A refusal is a decision, not a glitch: retrying cannot change the
		// answer, so surface it instead of looping.
		var refused *RefusedError
		if errors.As(err, &refused) {
			return err
		}
		if a.stopRetry.Load() {
			// The server ended this tunnel on purpose and said so.
			return nil
		}
		if !a.opts.Reconnect {
			if err != nil {
				return err
			}
			return nil
		}

		a.h.OnDisconnect(err, backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
		first = false
	}
}

// session runs one connection from handshake to disconnect.
func (a *Agent) session(ctx context.Context, first bool) error {
	conn, err := a.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	a.mu.Lock()
	sub := a.opts.Subdomain
	if sub == "" {
		sub = a.grantedSub
	}
	a.mu.Unlock()

	hostname, _ := os.Hostname()
	hello := proto.Hello{
		V:          proto.Version,
		Token:      a.opts.Profile.Token,
		Type:       a.opts.Type,
		Subdomain:  sub,
		LocalHost:  a.opts.LocalHost,
		LocalPort:  a.opts.LocalPort,
		RemotePort: a.opts.RemotePort,
		BasicAuth:  a.opts.BasicAuth,
		Agent:      version.UserAgent(),
		Hostname:   hostname,
	}

	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return err
	}
	if err := proto.WriteMsg(conn, hello); err != nil {
		return fmt.Errorf("send handshake: %w", err)
	}
	var ack proto.HelloAck
	if err := proto.ReadMsg(conn, &ack); err != nil {
		return fmt.Errorf("read handshake reply: %w", err)
	}
	if !ack.OK {
		return &RefusedError{Code: ack.Error, Message: ack.Message}
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return err
	}

	a.mu.Lock()
	a.grantedSub = ack.Subdomain
	a.mu.Unlock()

	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 20 * time.Second
	cfg.ConnectionWriteTimeout = 20 * time.Second
	cfg.LogOutput = io.Discard
	// The server opens streams, so the agent takes the yamux server role.
	session, err := yamux.Server(conn, cfg)
	if err != nil {
		return fmt.Errorf("start multiplexer: %w", err)
	}
	defer session.Close()

	a.h.OnConnect(ack, !first)

	// The server writes a closing message just before dropping the session, so
	// give the event reader a bounded moment to deliver it. Without this the
	// caller can report the tunnel closed before saying why.
	events := &eventTracker{done: make(chan struct{})}
	defer events.drain(250 * time.Millisecond)

	// Cancellation has to reach a blocking Accept, and closing the session is
	// the only way to unblock it.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			session.Close()
		case <-done:
		}
	}()

	for {
		stream, err := session.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("connection to server lost: %w", err)
		}
		go a.handleStream(stream, events)
	}
}

// dial opens the control connection, with TLS unless the profile is plain.
func (a *Agent) dial(ctx context.Context) (net.Conn, error) {
	addr := a.opts.Profile.ControlAddr()
	dialer := &net.Dialer{Timeout: 15 * time.Second}

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	if a.opts.Profile.Plaintext {
		return conn, nil
	}

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         a.opts.Profile.Host,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: a.opts.Profile.Insecure,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("tls handshake with %s: %w", addr, err)
	}
	return tlsConn, nil
}

// handleStream dispatches one inbound stream on its kind byte.
func (a *Agent) handleStream(stream net.Conn, events *eventTracker) {
	var kind [1]byte
	if _, err := io.ReadFull(stream, kind[:]); err != nil {
		stream.Close()
		return
	}

	switch proto.StreamKind(kind[0]) {
	case proto.KindEvents:
		events.begin()
		defer events.finish()
		a.readEvents(stream)
	case proto.KindData:
		a.forward(stream)
	default:
		stream.Close()
	}
}

// readEvents drains the server's event stream until it ends.
func (a *Agent) readEvents(stream net.Conn) {
	defer stream.Close()
	for {
		var ev proto.Event
		if err := proto.ReadMsg(stream, &ev); err != nil {
			return
		}
		if ev.T == proto.EventClose && ev.Final {
			a.stopRetry.Store(true)
		}
		a.h.OnEvent(ev)
	}
}

// forward splices one stream onto the local service.
func (a *Agent) forward(stream net.Conn) {
	defer stream.Close()

	target := net.JoinHostPort(a.opts.LocalHost, strconv.Itoa(a.opts.LocalPort))
	local, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		// The server turns the resulting stream close into a 502 for HTTP
		// tunnels; reporting locally keeps the CLI honest about why.
		a.h.OnEvent(proto.Event{
			T:       proto.EventNotice,
			At:      time.Now(),
			Message: fmt.Sprintf("cannot reach %s: %v", target, err),
		})
		return
	}
	defer local.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(local, stream)
		closeWrite(local)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(stream, local)
		closeWrite(stream)
	}()
	wg.Wait()
}

// closeWrite half-closes when possible so EOF-signalling protocols still work.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// eventTracker lets session teardown wait for the event reader to finish
// delivering whatever the server sent last.
//
// A WaitGroup cannot do this job: Add would race with Wait, since streams are
// accepted concurrently with teardown.
type eventTracker struct {
	seen atomic.Bool
	done chan struct{}
	once sync.Once
}

// begin records that an event stream was opened.
func (e *eventTracker) begin() { e.seen.Store(true) }

// finish reports that the event reader has stopped.
func (e *eventTracker) finish() { e.once.Do(func() { close(e.done) }) }

// drain waits up to d for the event reader, and returns at once if there never
// was one.
func (e *eventTracker) drain(d time.Duration) {
	if !e.seen.Load() {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-e.done:
	case <-timer.C:
	}
}
