package server_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erickdsama/zerock/internal/client"
	"github.com/erickdsama/zerock/internal/proto"
	"github.com/erickdsama/zerock/internal/server"
)

// freePort returns a port nothing is listening on. A port could in principle be
// taken between here and its use, but on loopback in a test that is rare.
func freePort(t *testing.T) int {
	t.Helper()
	return freePorts(t, 1)[0]
}

// freePorts reserves n distinct ports at once. Reserving them one at a time
// lets the kernel hand a just-released port out again for the next request,
// which is how the harness once bound its admin API on top of its own control
// listener and failed with "address already in use".
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	listeners := make([]net.Listener, 0, n)
	ports := make([]int, 0, n)
	for i := 0; i < n; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve a port: %v", err)
		}
		listeners = append(listeners, ln)
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
	}
	for _, ln := range listeners {
		ln.Close()
	}
	return ports
}

// harness is a running server plus the addresses needed to talk to it.
type harness struct {
	edge    int
	control int
	admin   int
	domain  string
	token   string
}

// startServer brings up a TLS-off server on loopback and returns its addresses
// and bootstrap admin token.
func startServer(t *testing.T) *harness {
	t.Helper()

	// All four ports come from one reservation so none can repeat.
	ports := freePorts(t, 4)
	h := &harness{
		edge:    ports[0],
		control: ports[1],
		admin:   ports[2],
		domain:  "zerock.test",
	}

	cfg := server.DefaultConfig()
	cfg.Domain = h.domain
	cfg.DataDir = t.TempDir()
	cfg.TLS = server.TLSConfig{Mode: server.TLSOff}
	cfg.HTTPAddr = fmt.Sprintf("127.0.0.1:%d", h.edge)
	cfg.ControlAddr = fmt.Sprintf("127.0.0.1:%d", h.control)
	cfg.AdminAddr = fmt.Sprintf("127.0.0.1:%d", h.admin)
	// One base port, so the range can never come out inverted.
	tcpBase := ports[3]
	cfg.TCPPortRange = server.PortRange{From: tcpBase, To: tcpBase + 20}

	// The config is built in memory, so run it through the same validation the
	// file path uses.
	path := filepath.Join(t.TempDir(), "zerock.yaml")
	body := fmt.Sprintf(`domain: %s
data_dir: %s
control_addr: "%s"
http_addr: "%s"
admin_addr: "%s"
tls:
  mode: off
tcp_port_range:
  from: %d
  to: %d
`, cfg.Domain, cfg.DataDir, cfg.ControlAddr, cfg.HTTPAddr, cfg.AdminAddr,
		cfg.TCPPortRange.From, cfg.TCPPortRange.To)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := server.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := server.New(loaded, log)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	h.token, err = srv.Bootstrap()
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if h.token == "" {
		t.Fatal("Bootstrap returned no admin token for a fresh store")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("server did not shut down")
		}
		srv.Close()
	})

	waitFor(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", h.admin))
	return h
}

// waitFor blocks until url answers.
func waitFor(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s never came up", url)
}

// profile builds a CLI profile pointing at the harness.
func (h *harness) profile() client.Profile {
	return client.Profile{
		Host:        "127.0.0.1",
		ControlPort: h.control,
		Token:       h.token,
		APIBase:     fmt.Sprintf("http://127.0.0.1:%d", h.admin),
		Plaintext:   true,
	}
}

// get issues a request to the public edge for the given subdomain.
func (h *harness) get(t *testing.T, sub, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d%s", h.edge, path), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = sub + "." + h.domain
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request to %s: %v", req.Host, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// recordingHandler collects agent callbacks for assertions.
type recordingHandler struct {
	mu        sync.Mutex
	acks      []proto.HelloAck
	events    []proto.Event
	connected chan proto.HelloAck
}

func newRecordingHandler() *recordingHandler {
	return &recordingHandler{connected: make(chan proto.HelloAck, 8)}
}

func (r *recordingHandler) OnConnect(ack proto.HelloAck, _ bool) {
	r.mu.Lock()
	r.acks = append(r.acks, ack)
	r.mu.Unlock()
	select {
	case r.connected <- ack:
	default:
	}
}

func (r *recordingHandler) OnEvent(ev proto.Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recordingHandler) OnDisconnect(error, time.Duration) {}

// eventsOfType returns the recorded events of one type.
func (r *recordingHandler) eventsOfType(kind string) []proto.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []proto.Event
	for _, ev := range r.events {
		if ev.T == kind {
			out = append(out, ev)
		}
	}
	return out
}

// startAgent runs an agent and waits for it to connect.
func startAgent(t *testing.T, h *harness, opts client.AgentOptions) (*recordingHandler, proto.HelloAck) {
	t.Helper()
	opts.Profile = h.profile()
	handler := newRecordingHandler()
	agent := client.NewAgent(opts, handler)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := agent.Run(ctx); err != nil {
			t.Logf("agent stopped: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("agent did not stop")
		}
	})

	select {
	case ack := <-handler.connected:
		return handler, ack
	case <-time.After(15 * time.Second):
		t.Fatal("agent never connected")
	}
	return nil, proto.HelloAck{}
}

// localPort extracts the port of a test server.
func localPort(t *testing.T, ts *httptest.Server) int {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestHTTPTunnelEndToEnd(t *testing.T) {
	h := startServer(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "host=%s xff=%s proto=%s path=%s",
			r.Host, r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Forwarded-Proto"), r.URL.Path)
	}))
	defer backend.Close()

	handler, ack := startAgent(t, h, client.AgentOptions{
		Type: proto.TypeHTTP, LocalPort: localPort(t, backend), Subdomain: "api-x",
	})
	if ack.Subdomain != "api-x" {
		t.Fatalf("got subdomain %q, want api-x", ack.Subdomain)
	}
	if want := "http://api-x.zerock.test"; ack.URL != want {
		t.Errorf("URL = %q, want %q", ack.URL, want)
	}

	status, body := h.get(t, "api-x", "/thing")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", status, body)
	}
	// The backend must see the public hostname, not the tunnel's internal target.
	if !strings.Contains(body, "host=api-x.zerock.test") {
		t.Errorf("backend saw the wrong Host: %q", body)
	}
	if !strings.Contains(body, "path=/thing") {
		t.Errorf("path not forwarded: %q", body)
	}
	if !strings.Contains(body, "xff=127.0.0.1") {
		t.Errorf("X-Forwarded-For not set: %q", body)
	}

	// The server pushes a request log back to the agent.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if reqs := handler.eventsOfType(proto.EventRequest); len(reqs) > 0 {
			if reqs[0].Status != 200 || reqs[0].Method != "GET" {
				t.Errorf("event = %+v, want a 200 GET", reqs[0])
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Error("no request event reached the agent")
}

func TestRandomSubdomainIsAssignedAndRoutes(t *testing.T) {
	h := startServer(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	_, ack := startAgent(t, h, client.AgentOptions{Type: proto.TypeHTTP, LocalPort: localPort(t, backend)})
	if ack.Subdomain == "" {
		t.Fatal("no subdomain was assigned")
	}
	if status, body := h.get(t, ack.Subdomain, "/"); status != http.StatusOK || body != "ok" {
		t.Errorf("got %d %q, want 200 \"ok\"", status, body)
	}
}

func TestUnknownSubdomainIs404(t *testing.T) {
	h := startServer(t)
	if status, _ := h.get(t, "nothing-here", "/"); status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestEdgeBasicAuth(t *testing.T) {
	h := startServer(t)
	var sawAuthHeader string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuthHeader = r.Header.Get("Authorization")
		w.Write([]byte("secret"))
	}))
	defer backend.Close()

	startAgent(t, h, client.AgentOptions{
		Type: proto.TypeHTTP, LocalPort: localPort(t, backend),
		Subdomain: "locked", BasicAuth: "demo:hunter2",
	})

	if status, _ := h.get(t, "locked", "/"); status != http.StatusUnauthorized {
		t.Errorf("without credentials: status = %d, want 401", status)
	}

	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/", h.edge), nil)
	req.Host = "locked." + h.domain
	req.SetBasicAuth("demo", "wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("with a wrong password: status = %d, want 401", resp.StatusCode)
	}

	req, _ = http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/", h.edge), nil)
	req.Host = "locked." + h.domain
	req.SetBasicAuth("demo", "hunter2")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "secret" {
		t.Errorf("with the right credentials: got %d %q", resp.StatusCode, body)
	}
	// Edge credentials must not be forwarded to the backend.
	if sawAuthHeader != "" {
		t.Errorf("backend saw the Authorization header %q; it should be stripped", sawAuthHeader)
	}
}

func TestLocalServiceDownGives502(t *testing.T) {
	h := startServer(t)
	// Point the agent at a port with nothing behind it.
	startAgent(t, h, client.AgentOptions{
		Type: proto.TypeHTTP, LocalPort: freePort(t), Subdomain: "dead",
	})
	if status, _ := h.get(t, "dead", "/"); status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
}

func TestSubdomainCollisionIsRefused(t *testing.T) {
	h := startServer(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer backend.Close()

	startAgent(t, h, client.AgentOptions{
		Type: proto.TypeHTTP, LocalPort: localPort(t, backend), Subdomain: "taken",
	})

	second := client.NewAgent(client.AgentOptions{
		Profile: h.profile(), Type: proto.TypeHTTP,
		LocalPort: localPort(t, backend), Subdomain: "taken",
	}, newRecordingHandler())

	err := second.Run(context.Background())
	if err == nil {
		t.Fatal("a second tunnel on the same subdomain should be refused")
	}
	var refused *client.RefusedError
	if !asRefused(err, &refused) {
		t.Fatalf("got %T (%v), want a RefusedError", err, err)
	}
	if refused.Code != proto.ErrSubTaken {
		t.Errorf("code = %q, want %q", refused.Code, proto.ErrSubTaken)
	}
}

func TestServerReservedSubdomainIsBlocked(t *testing.T) {
	h := startServer(t)
	agent := client.NewAgent(client.AgentOptions{
		Profile: h.profile(), Type: proto.TypeHTTP, LocalPort: freePort(t), Subdomain: "www",
	}, newRecordingHandler())

	err := agent.Run(context.Background())
	var refused *client.RefusedError
	if !asRefused(err, &refused) {
		t.Fatalf("got %v, want a RefusedError", err)
	}
	// Blocked is distinct from reserved: no reservation can ever free this name.
	if refused.Code != proto.ErrSubBlocked {
		t.Errorf("code = %q, want %q", refused.Code, proto.ErrSubBlocked)
	}
}

func TestBadTokenIsRefused(t *testing.T) {
	h := startServer(t)
	prof := h.profile()
	prof.Token = "zk_deadbeef_" + strings.Repeat("z", 32)
	agent := client.NewAgent(client.AgentOptions{
		Profile: prof, Type: proto.TypeHTTP, LocalPort: freePort(t),
	}, newRecordingHandler())

	err := agent.Run(context.Background())
	var refused *client.RefusedError
	if !asRefused(err, &refused) {
		t.Fatalf("got %v, want a RefusedError", err)
	}
	if refused.Code != proto.ErrUnauthorized {
		t.Errorf("code = %q, want %q", refused.Code, proto.ErrUnauthorized)
	}
}

func TestTCPTunnelEndToEnd(t *testing.T) {
	h := startServer(t)

	// A trivial echo service to prove raw bytes survive the round trip.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()

	_, ack := startAgent(t, h, client.AgentOptions{
		Type:      proto.TypeTCP,
		LocalPort: ln.Addr().(*net.TCPAddr).Port,
		Subdomain: "db",
	})
	if ack.PublicAddr == "" {
		t.Fatal("no public address was assigned to the TCP tunnel")
	}
	_, portStr, err := net.SplitHostPort(ack.PublicAddr)
	if err != nil {
		t.Fatalf("PublicAddr %q: %v", ack.PublicAddr, err)
	}

	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+portStr, 10*time.Second)
	if err != nil {
		t.Fatalf("dial the public port: %v", err)
	}
	defer conn.Close()

	want := "raw bytes over the tunnel"
	if _, err := conn.Write([]byte(want)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(want))
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(buf) != want {
		t.Errorf("echoed %q, want %q", buf, want)
	}
}

func TestWebSocketUpgradeSurvivesTheTunnel(t *testing.T) {
	h := startServer(t)

	// A minimal upgrade: the proxy must hand over the raw connection, after
	// which the two sides talk without the HTTP layer involved.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "not an upgrade", http.StatusBadRequest)
			return
		}
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("backend hijack: %v", err)
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		line, err := brw.ReadString('\n')
		if err != nil {
			return
		}
		brw.WriteString("echo:" + line)
		brw.Flush()
	}))
	defer backend.Close()

	handler, _ := startAgent(t, h, client.AgentOptions{
		Type: proto.TypeHTTP, LocalPort: localPort(t, backend), Subdomain: "ws",
	})

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", h.edge), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(15 * time.Second))

	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: ws.%s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n", h.domain)

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read the upgrade response: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "101") {
		t.Fatalf("expected a 101, got: %q", buf[:n])
	}

	fmt.Fprint(conn, "hello\n")
	n, err = conn.Read(buf)
	if err != nil {
		t.Fatalf("read the echo: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "echo:hello") {
		t.Errorf("got %q, want it to contain echo:hello", buf[:n])
	}

	// The edge should log the upgrade as a 101 rather than a plain 200.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range handler.eventsOfType(proto.EventRequest) {
			if ev.Status == http.StatusSwitchingProtocols {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Error("the upgrade was not reported as a 101")
}

func TestStreamingIsNotBuffered(t *testing.T) {
	h := startServer(t)

	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		fmt.Fprint(w, "data: first\n\n")
		rc.Flush()
		// Hold the response open: a buffering proxy would not deliver the first
		// chunk until this returns.
		<-release
		fmt.Fprint(w, "data: second\n\n")
		rc.Flush()
	}))
	defer backend.Close()
	defer close(release)

	startAgent(t, h, client.AgentOptions{
		Type: proto.TypeHTTP, LocalPort: localPort(t, backend), Subdomain: "sse",
	})

	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/sse", h.edge), nil)
	req.Host = "sse." + h.domain
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if err != nil {
		t.Fatalf("read the first chunk: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "first") {
		t.Errorf("first chunk = %q, want it to contain \"first\"", buf[:n])
	}
}

// asRefused unwraps a RefusedError without pulling errors into every test.
func asRefused(err error, target **client.RefusedError) bool {
	for err != nil {
		if r, ok := err.(*client.RefusedError); ok {
			*target = r
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
