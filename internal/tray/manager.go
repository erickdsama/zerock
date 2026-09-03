package tray

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/erickdsama/zerock/internal/client"
	"github.com/erickdsama/zerock/internal/proto"
)

// State is where a managed tunnel is in its life.
type State int

const (
	// StateStopped means not running, by choice.
	StateStopped State = iota
	// StateConnecting is the first handshake, before any URL is known.
	StateConnecting
	// StateUp is serving.
	StateUp
	// StateReconnecting is between a dropped session and the next attempt.
	StateReconnecting
	// StateFailed means the agent gave up: the server refused the tunnel, or
	// closed it on purpose.
	StateFailed
)

// Status is a snapshot of one managed tunnel, safe to read without locks.
type Status struct {
	Name     string
	Spec     client.SavedTunnel
	State    State
	Target   string // public URL or host:port once connected
	TunnelID string
	Requests int64
	Since    time.Time
	Err      string
}

// Active reports whether the agent is still running, in any state.
func (s Status) Active() bool {
	return s.State == StateConnecting || s.State == StateUp || s.State == StateReconnecting
}

// Manager runs agents for saved tunnels, one goroutine each, and keeps their
// latest status. It knows nothing about the menu: it calls notify whenever
// something changed and lets the UI decide what to redraw.
type Manager struct {
	notify func()

	mu   sync.Mutex
	runs map[string]*run
}

// NewManager returns an empty manager. notify may be called from any goroutine
// and must not block.
func NewManager(notify func()) *Manager {
	return &Manager{notify: notify, runs: map[string]*run{}}
}

// run is one agent plus its status. It is the agent's Handler, so lifecycle
// callbacks land here directly.
type run struct {
	m      *Manager
	cancel context.CancelFunc
	done   chan struct{}

	mu sync.Mutex
	st Status
}

// Start opens a tunnel under name. Starting an already-active name is a no-op.
func (m *Manager) Start(name string, spec client.SavedTunnel, prof client.Profile) {
	m.mu.Lock()
	if r, ok := m.runs[name]; ok && r.status().Active() {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &run{
		m:      m,
		cancel: cancel,
		done:   make(chan struct{}),
		st:     Status{Name: name, Spec: spec, State: StateConnecting, Since: time.Now()},
	}
	m.runs[name] = r
	m.mu.Unlock()

	agent := client.NewAgent(client.AgentOptions{
		Profile:    prof,
		Type:       proto.TunnelType(spec.Type),
		LocalHost:  spec.Host,
		LocalPort:  spec.Port,
		Subdomain:  spec.Subdomain,
		RemotePort: spec.RemotePort,
		BasicAuth:  spec.BasicAuth,
		Reconnect:  true,
	}, r)

	go func() {
		defer close(r.done)
		err := agent.Run(ctx)
		r.mu.Lock()
		switch {
		case ctx.Err() != nil:
			r.st.State = StateStopped
		case err != nil:
			r.st.State, r.st.Err = StateFailed, err.Error()
		default:
			// The server closed the tunnel deliberately and the agent did not
			// retry. The close event already recorded why.
			r.st.State = StateFailed
			if r.st.Err == "" {
				r.st.Err = "closed by the server"
			}
		}
		r.mu.Unlock()
		m.notify()
	}()
	m.notify()
}

// Stop cancels a tunnel and waits briefly for the agent to wind down.
func (m *Manager) Stop(name string) {
	m.mu.Lock()
	r, ok := m.runs[name]
	m.mu.Unlock()
	if !ok {
		return
	}
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(2 * time.Second):
	}
}

// Forget stops a tunnel and drops it from the status list.
func (m *Manager) Forget(name string) {
	m.Stop(name)
	m.mu.Lock()
	delete(m.runs, name)
	m.mu.Unlock()
	m.notify()
}

// StopAll cancels every tunnel and waits for them, bounded, so quitting is
// quick even when the server is unreachable.
func (m *Manager) StopAll() {
	m.mu.Lock()
	runs := make([]*run, 0, len(m.runs))
	for _, r := range m.runs {
		runs = append(runs, r)
	}
	m.mu.Unlock()

	for _, r := range runs {
		r.cancel()
	}
	deadline := time.After(2 * time.Second)
	for _, r := range runs {
		select {
		case <-r.done:
		case <-deadline:
			return
		}
	}
}

// Snapshot returns every known tunnel's status, by name.
func (m *Manager) Snapshot() map[string]Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]Status, len(m.runs))
	for name, r := range m.runs {
		out[name] = r.status()
	}
	return out
}

// TunnelIDs lists the server-side ids of the tunnels this manager holds, so the
// UI can tell its own tunnels apart from the token's others.
func (m *Manager) TunnelIDs() map[string]bool {
	ids := map[string]bool{}
	for _, st := range m.Snapshot() {
		if st.TunnelID != "" {
			ids[st.TunnelID] = true
		}
	}
	return ids
}

// Summary rolls every status up into the state the tray icon should show.
// Trouble wins over success so a failing tunnel is never hidden behind a
// healthy one.
func Summary(all map[string]Status) (icon State, up int) {
	icon = StateStopped
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		st := all[name]
		switch st.State {
		case StateUp:
			up++
			if icon == StateStopped {
				icon = StateUp
			}
		case StateConnecting, StateReconnecting:
			if icon != StateFailed {
				icon = StateReconnecting
			}
		case StateFailed:
			icon = StateFailed
		}
	}
	return icon, up
}

func (r *run) status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.st
}

// OnConnect implements client.Handler.
func (r *run) OnConnect(ack proto.HelloAck, _ bool) {
	r.mu.Lock()
	r.st.State = StateUp
	r.st.Err = ""
	r.st.TunnelID = ack.TunnelID
	r.st.Target = ack.URL
	if ack.PublicAddr != "" {
		r.st.Target = ack.PublicAddr
	}
	r.mu.Unlock()
	r.m.notify()
}

// OnEvent implements client.Handler.
func (r *run) OnEvent(ev proto.Event) {
	r.mu.Lock()
	switch ev.T {
	case proto.EventRequest, proto.EventConn:
		r.st.Requests++
	case proto.EventClose, proto.EventNotice:
		r.st.Err = ev.Message
	}
	r.mu.Unlock()
	r.m.notify()
}

// OnDisconnect implements client.Handler.
func (r *run) OnDisconnect(err error, _ time.Duration) {
	r.mu.Lock()
	r.st.State = StateReconnecting
	if err != nil {
		r.st.Err = err.Error()
	} else if r.st.Err == "" {
		r.st.Err = "connection lost"
	}
	r.mu.Unlock()
	r.m.notify()
}
