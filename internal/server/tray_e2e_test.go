package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/erickdsama/zerock/internal/client"
	"github.com/erickdsama/zerock/internal/tray"
)

// waitState polls the manager until the named tunnel reaches a state.
func waitState(t *testing.T, m *tray.Manager, name string, want tray.State) tray.Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := m.Snapshot()[name]; ok && st.State == want {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never reached state %d: %+v", name, want, m.Snapshot()[name])
	return tray.Status{}
}

// The tray widget's manager is the same agent as "zerock http" wrapped in a
// status machine, so this checks the machine: up with a URL, counting requests,
// visible to the API under its own id, stopped on demand, and failed when the
// server refuses.
func TestTrayManagerLifecycle(t *testing.T) {
	h := startServer(t)
	prof := h.profile()

	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("hello from the tray"))
	}))
	defer local.Close()
	port, _ := strconv.Atoi(local.URL[strings.LastIndex(local.URL, ":")+1:])

	notified := make(chan struct{}, 64)
	m := tray.NewManager(func() {
		select {
		case notified <- struct{}{}:
		default:
		}
	})

	spec := client.SavedTunnel{Type: "http", Port: port, Subdomain: "tray-x"}
	m.Start("x", spec, prof)
	st := waitState(t, m, "x", tray.StateUp)
	if !strings.Contains(st.Target, "tray-x."+h.domain) {
		t.Fatalf("target = %q, want the granted subdomain", st.Target)
	}
	if st.TunnelID == "" {
		t.Fatal("no tunnel id recorded")
	}
	select {
	case <-notified:
	default:
		t.Fatal("manager never notified the UI")
	}

	// Starting again while active is a no-op.
	m.Start("x", spec, prof)
	if again := m.Snapshot()["x"]; again.TunnelID != st.TunnelID {
		t.Fatalf("second Start replaced the run: %q vs %q", again.TunnelID, st.TunnelID)
	}

	status, body := h.get(t, "tray-x", "/")
	if status != 200 || body != "hello from the tray" {
		t.Fatalf("edge returned %d %q", status, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.Snapshot()["x"].Requests < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := m.Snapshot()["x"].Requests; n != 1 {
		t.Fatalf("requests = %d, want 1", n)
	}

	// The API lists it, and TunnelIDs is what lets the UI filter it out of
	// "other tunnels".
	tunnels, err := client.NewAPI(prof).ListTunnels(context.Background(), true)
	if err != nil {
		t.Fatalf("ListTunnels: %v", err)
	}
	if len(tunnels) != 1 || !m.TunnelIDs()[tunnels[0].ID] {
		t.Fatalf("API tunnels %+v do not match manager ids %v", tunnels, m.TunnelIDs())
	}
	if icon, up := tray.Summary(m.Snapshot()); icon != tray.StateUp || up != 1 {
		t.Fatalf("Summary = %d, %d; want up, 1", icon, up)
	}

	m.Stop("x")
	waitState(t, m, "x", tray.StateStopped)
	if icon, up := tray.Summary(m.Snapshot()); icon != tray.StateStopped || up != 0 {
		t.Fatalf("Summary after stop = %d, %d", icon, up)
	}

	// A refusal (blocked subdomain) is final: the run fails and says why.
	m.Start("bad", client.SavedTunnel{Type: "http", Port: port, Subdomain: "www"}, prof)
	failed := waitState(t, m, "bad", tray.StateFailed)
	if failed.Err == "" {
		t.Fatal("failed run carries no reason")
	}
	if icon, _ := tray.Summary(m.Snapshot()); icon != tray.StateFailed {
		t.Fatalf("Summary with a failure = %d, want failed", icon)
	}

	// A closed tunnel from the server side (kill) also ends as failed, with
	// the server's message, and does not reconnect.
	m.Start("x", spec, prof)
	st = waitState(t, m, "x", tray.StateUp)
	if err := client.NewAPI(prof).CloseTunnel(context.Background(), st.TunnelID); err != nil {
		t.Fatalf("CloseTunnel: %v", err)
	}
	killed := waitState(t, m, "x", tray.StateFailed)
	if killed.Err == "" {
		t.Fatal("killed run carries no reason")
	}

	m.Forget("bad")
	if _, ok := m.Snapshot()["bad"]; ok {
		t.Fatal("Forget left the run behind")
	}
	m.StopAll()
}
