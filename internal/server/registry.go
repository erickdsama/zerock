package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httputil"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erickdsama/zerock/internal/proto"
	"github.com/hashicorp/yamux"
)

// Tunnel is one live agent session holding a subdomain.
type Tunnel struct {
	ID        string           `json:"id"`
	Subdomain string           `json:"sub"`
	Type      proto.TunnelType `json:"type"`
	URL       string           `json:"url"`
	// PublicPort is the edge port for TCP tunnels, zero for HTTP.
	PublicPort int       `json:"public_port,omitempty"`
	LocalAddr  string    `json:"local_addr"`
	TokenID    string    `json:"token_id"`
	TokenLabel string    `json:"token_label"`
	AgentHost  string    `json:"agent_host,omitempty"`
	AgentVer   string    `json:"agent_version,omitempty"`
	RemoteIP   string    `json:"remote_ip"`
	StartedAt  time.Time `json:"started_at"`

	// basicAuth, when non-empty, is the "user:pass" the edge enforces.
	basicAuth string

	session *yamux.Session
	// proxy forwards HTTP traffic for this tunnel; nil for TCP tunnels.
	proxy *httputil.ReverseProxy
	// ready separates claiming a subdomain from being able to serve it. The
	// subdomain is claimed during the handshake so the acknowledgement can be
	// truthful, but the edge must not route to the tunnel until its session
	// exists. Loading this flag also publishes the session write that precedes
	// it, so a router that sees ready sees a usable session.
	ready atomic.Bool
	// readyCh closes when the tunnel becomes routable, so a request that lands
	// during the handshake can wait instead of being rejected.
	readyCh chan struct{}
	// closedCh closes when the tunnel is torn down, releasing any waiters.
	closedCh  chan struct{}
	readyOnce sync.Once
	closeOnce sync.Once

	mu       sync.Mutex
	events   io.WriteCloser
	requests int64
	bytesIn  int64
	bytesOut int64
	closed   bool
	// listener is the public TCP listener owned by a TCP tunnel.
	listener net.Listener
}

// Stats is a point-in-time snapshot of a tunnel's counters.
type Stats struct {
	Requests int64 `json:"requests"`
	BytesIn  int64 `json:"bytes_in"`
	BytesOut int64 `json:"bytes_out"`
}

// Stats returns the tunnel's counters.
func (t *Tunnel) Stats() Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return Stats{Requests: t.requests, BytesIn: t.bytesIn, BytesOut: t.bytesOut}
}

// MarshalJSON emits the tunnel with its live counters attached.
func (t *Tunnel) MarshalJSON() ([]byte, error) {
	type alias Tunnel // avoids recursing into this method
	return json.Marshal(struct {
		*alias
		Stats   Stats  `json:"stats"`
		UptimeS int64  `json:"uptime_seconds"`
		Uptime  string `json:"uptime"`
	}{
		alias:   (*alias)(t),
		Stats:   t.Stats(),
		UptimeS: int64(time.Since(t.StartedAt).Seconds()),
		Uptime:  time.Since(t.StartedAt).Truncate(time.Second).String(),
	})
}

// openStream opens a data stream to the agent, tagged with the data kind byte.
func (t *Tunnel) openStream() (net.Conn, error) {
	if !t.ready.Load() || t.session == nil {
		return nil, errors.New("tunnel is not ready to carry traffic")
	}
	stream, err := t.session.Open()
	if err != nil {
		return nil, err
	}
	if _, err := stream.Write([]byte{byte(proto.KindData)}); err != nil {
		stream.Close()
		return nil, err
	}
	return stream, nil
}

// attachEvents gives the tunnel the stream it pushes events on.
func (t *Tunnel) attachEvents(w io.WriteCloser) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = w
}

// emit pushes an event to the agent. Delivery is best effort: a wedged or gone
// agent must never block edge traffic, so a failed write just drops the stream.
func (t *Tunnel) emit(ev proto.Event) {
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	t.mu.Lock()
	w := t.events
	t.mu.Unlock()
	if w == nil {
		return
	}
	if err := proto.WriteMsg(w, ev); err != nil {
		t.mu.Lock()
		if t.events == w {
			t.events = nil
		}
		t.mu.Unlock()
		w.Close()
	}
}

// recordRequest folds one completed HTTP request into the counters.
func (t *Tunnel) recordRequest(in, out int64) {
	t.mu.Lock()
	t.requests++
	t.bytesIn += in
	t.bytesOut += out
	t.mu.Unlock()
}

// recordConn folds one completed TCP connection into the counters.
func (t *Tunnel) recordConn(in, out int64) {
	t.mu.Lock()
	t.requests++
	t.bytesIn += in
	t.bytesOut += out
	t.mu.Unlock()
}

// Close tears the tunnel down. It is safe to call more than once. When final is
// true the agent is told not to reconnect, which is what separates an operator
// closing a tunnel from the server merely restarting under it.
func (t *Tunnel) Close(reason string, final bool) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.closeOnce.Do(func() { close(t.closedCh) })
	events, listener := t.events, t.listener
	t.events, t.listener = nil, nil
	t.mu.Unlock()

	if events != nil && reason != "" {
		// Best effort: tell the agent why before dropping the session so its
		// output explains the disconnect.
		_ = proto.WriteMsg(events, proto.Event{
			T: proto.EventClose, At: time.Now().UTC(), Message: reason, Final: final,
		})
	}
	if events != nil {
		events.Close()
	}
	if listener != nil {
		listener.Close()
	}
	if t.session != nil {
		t.session.Close()
	}
}

// registry tracks live tunnels by subdomain and by public TCP port.
type registry struct {
	mu      sync.RWMutex
	bySub   map[string]*Tunnel
	byPort  map[int]*Tunnel
	byToken map[string]int
}

func newRegistry() *registry {
	return &registry{
		bySub:   make(map[string]*Tunnel),
		byPort:  make(map[int]*Tunnel),
		byToken: make(map[string]int),
	}
}

// errSubBusy reports that a live tunnel already holds the subdomain.
var errSubBusy = errors.New("subdomain in use")

// add registers t, failing if its subdomain is already live.
func (r *registry) add(t *Tunnel) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.bySub[t.Subdomain]; exists {
		return errSubBusy
	}
	r.bySub[t.Subdomain] = t
	if t.PublicPort != 0 {
		r.byPort[t.PublicPort] = t
	}
	r.byToken[t.TokenID]++
	return nil
}

// remove deregisters t if it is still the tunnel holding its subdomain.
func (r *registry) remove(t *Tunnel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.bySub[t.Subdomain]; ok && cur == t {
		delete(r.bySub, t.Subdomain)
	}
	if t.PublicPort != 0 {
		if cur, ok := r.byPort[t.PublicPort]; ok && cur == t {
			delete(r.byPort, t.PublicPort)
		}
	}
	if n := r.byToken[t.TokenID]; n <= 1 {
		delete(r.byToken, t.TokenID)
	} else {
		r.byToken[t.TokenID] = n - 1
	}
}

// newTunnel allocates a tunnel with its lifecycle channels in place.
func newTunnel() *Tunnel {
	return &Tunnel{
		ID:       newTunnelID(),
		readyCh:  make(chan struct{}),
		closedCh: make(chan struct{}),
	}
}

// markReady makes a claimed tunnel routable. Call it only after the session is
// established.
func (t *Tunnel) markReady() {
	t.ready.Store(true)
	t.readyOnce.Do(func() { close(t.readyCh) })
}

// waitReady blocks until the tunnel can carry traffic, giving up after d.
//
// A request can arrive between an agent claiming its subdomain and its session
// being usable, which also happens on every reconnect. Waiting out that window
// turns what would be a spurious 404 into a slightly slow success.
func (t *Tunnel) waitReady(ctx context.Context, d time.Duration) bool {
	if t.ready.Load() {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-t.readyCh:
		return true
	case <-t.closedCh:
		return false
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

// lookup finds a live, routable tunnel by subdomain. A tunnel that has claimed
// the name but not finished its handshake is deliberately invisible.
func (r *registry) lookup(sub string) (*Tunnel, bool) {
	t, ok := r.claimed(sub)
	if !ok || !t.ready.Load() {
		return nil, false
	}
	return t, true
}

// claimed finds the tunnel holding a subdomain whether or not it is routable
// yet, which lets the edge wait for one mid-handshake.
func (r *registry) claimed(sub string) (*Tunnel, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.bySub[sub]
	return t, ok
}

// byID finds a live tunnel by its identifier.
func (r *registry) byID(id string) (*Tunnel, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.bySub {
		if t.ID == id && t.ready.Load() {
			return t, true
		}
	}
	return nil, false
}

// countForToken reports how many live tunnels a token holds.
func (r *registry) countForToken(tokenID string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byToken[tokenID]
}

// list returns live tunnels sorted by subdomain. A non-empty tokenID filters to
// that owner.
func (r *registry) list(tokenID string) []*Tunnel {
	r.mu.RLock()
	out := make([]*Tunnel, 0, len(r.bySub))
	for _, t := range r.bySub {
		if !t.ready.Load() {
			continue
		}
		if tokenID == "" || t.TokenID == tokenID {
			out = append(out, t)
		}
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Subdomain < out[j].Subdomain })
	return out
}

// closeAll shuts every tunnel down, used on server shutdown. It is never final:
// agents should reconnect once the server is back.
func (r *registry) closeAll(reason string) {
	r.mu.RLock()
	all := make([]*Tunnel, 0, len(r.bySub))
	for _, t := range r.bySub {
		all = append(all, t)
	}
	r.mu.RUnlock()
	for _, t := range all {
		t.Close(reason, false)
	}
}

// portTaken reports whether a public TCP port is already serving a tunnel.
func (r *registry) portTaken(port int) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.byPort[port]
	return ok
}

// newTunnelID returns an opaque, sortable-enough tunnel identifier.
func newTunnelID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("registry: " + err.Error())
	}
	return "tn_" + hex.EncodeToString(b[:])
}

// hostPort renders an address for display.
func hostPort(host string, port int) string { return fmt.Sprintf("%s:%d", host, port) }
