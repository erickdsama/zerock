package clientcli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erickdsama/zerock/internal/client"
	"github.com/erickdsama/zerock/internal/proto"
)

const usageHTTP = `Share a local HTTP port on a subdomain of your domain.

Usage:
  zerock http <port> [flags]

Examples:
  zerock http 3000                      get a random subdomain
  zerock http 3000 --sub api-x          ask for api-x.yourdomain.com
  zerock http 8080 --auth demo:hunter2  require basic auth at the edge
  zerock http 3000 --host 192.168.1.20  forward to another host on your LAN

Flags:
  --sub name        subdomain to request; random if omitted or already taken
  --host addr       local host to forward to (default 127.0.0.1)
  --auth user:pass  require HTTP basic auth before traffic reaches you
  --no-reconnect    exit when the connection drops instead of retrying
  --quiet           print the URL and nothing else
  --profile name    saved profile to use
`

const usageTCP = `Share a local TCP port on a public port of your zerock server.

Any protocol that runs over TCP works: Postgres, Redis, SSH, a game server.
The server assigns a port from its configured range unless you ask for one.

Usage:
  zerock tcp <port> [flags]

Examples:
  zerock tcp 5432 --sub db              expose Postgres
  zerock tcp 22 --remote-port 20022     ask for a specific public port

Flags:
  --sub name          subdomain to request; random if omitted
  --remote-port n     public port to request (must be in the server's range)
  --host addr         local host to forward to (default 127.0.0.1)
  --no-reconnect      exit when the connection drops instead of retrying
  --quiet             print the address and nothing else
  --profile name      saved profile to use
`

func runHTTP(ctx context.Context, args []string) error {
	fs := newFlagSet("http", usageHTTP)
	sub := fs.String("sub", "", "subdomain to request")
	host := fs.String("host", "127.0.0.1", "local host to forward to")
	auth := fs.String("auth", "", "require basic auth as user:pass")
	noReconnect := fs.Bool("no-reconnect", false, "do not retry after a disconnect")
	quiet := fs.Bool("quiet", false, "print only the URL")
	profile := profileFlag(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	port, err := parsePortArg(fs, "http")
	if err != nil {
		return err
	}
	if *auth != "" && !strings.Contains(*auth, ":") {
		return errors.New("--auth expects user:pass")
	}

	return runTunnel(ctx, tunnelRequest{
		Type:        proto.TypeHTTP,
		Port:        port,
		Sub:         *sub,
		LocalHost:   *host,
		BasicAuth:   *auth,
		Reconnect:   !*noReconnect,
		Quiet:       *quiet,
		ProfileName: *profile,
	})
}

func runTCP(ctx context.Context, args []string) error {
	fs := newFlagSet("tcp", usageTCP)
	sub := fs.String("sub", "", "subdomain to request")
	remotePort := fs.Int("remote-port", 0, "public port to request")
	host := fs.String("host", "127.0.0.1", "local host to forward to")
	noReconnect := fs.Bool("no-reconnect", false, "do not retry after a disconnect")
	quiet := fs.Bool("quiet", false, "print only the address")
	profile := profileFlag(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	port, err := parsePortArg(fs, "tcp")
	if err != nil {
		return err
	}

	return runTunnel(ctx, tunnelRequest{
		Type:        proto.TypeTCP,
		Port:        port,
		Sub:         *sub,
		RemotePort:  *remotePort,
		LocalHost:   *host,
		Reconnect:   !*noReconnect,
		Quiet:       *quiet,
		ProfileName: *profile,
	})
}

// tunnelRequest is the parsed form of an http or tcp command.
type tunnelRequest struct {
	Type        proto.TunnelType
	Port        int
	Sub         string
	RemotePort  int
	LocalHost   string
	BasicAuth   string
	Reconnect   bool
	Quiet       bool
	ProfileName string
}

func runTunnel(ctx context.Context, req tunnelRequest) error {
	profileName, prof, err := resolveProfile(req.ProfileName)
	if err != nil {
		return err
	}

	view := &tunnelView{
		quiet:     req.Quiet,
		localAddr: fmt.Sprintf("%s:%d", req.LocalHost, req.Port),
		profile:   profileName,
		server:    prof.Host,
	}

	agent := client.NewAgent(client.AgentOptions{
		Profile:    prof,
		Type:       req.Type,
		LocalHost:  req.LocalHost,
		LocalPort:  req.Port,
		Subdomain:  req.Sub,
		RemotePort: req.RemotePort,
		BasicAuth:  req.BasicAuth,
		Reconnect:  req.Reconnect,
	}, view)

	err = agent.Run(ctx)

	var refused *client.RefusedError
	if errors.As(err, &refused) {
		return explainRefusal(refused, req)
	}
	if err != nil {
		return err
	}
	if !req.Quiet && view.connected.Load() {
		view.out.Lock()
		defer view.out.Unlock()
		fmt.Fprintf(os.Stderr, "\n%s %s\n", dim("tunnel closed;"), dim(view.summary()))
	}
	return nil
}

// explainRefusal turns a server refusal into advice about what to do next.
func explainRefusal(refused *client.RefusedError, req tunnelRequest) error {
	msg := refused.Error()
	switch refused.Code {
	case proto.ErrSubTaken:
		return fmt.Errorf("%s\n  %s", msg,
			dim("another tunnel holds that name right now; pick a different --sub or drop the flag for a random one"))
	case proto.ErrSubReserved:
		return fmt.Errorf("%s\n  %s", msg,
			dim("ask its owner to release it, or pick another --sub"))
	case proto.ErrSubBlocked:
		return fmt.Errorf("%s\n  %s", msg,
			dim("the server's operator sets these in reserved_subdomains; pick another --sub"))
	case proto.ErrUnauthorized:
		return fmt.Errorf("%s\n  %s", msg,
			dim("check the token with: zerock whoami"))
	case proto.ErrTunnelLimit:
		return fmt.Errorf("%s\n  %s", msg,
			dim("see what is already running with: zerock ls"))
	}
	return errors.New(msg)
}

// parsePortArg reads the single positional port argument.
func parsePortArg(fs *flag.FlagSet, _ string) (int, error) {
	if err := exactArgs(fs, 1, "one local port"); err != nil {
		return 0, err
	}
	raw := fs.Arg(0)
	// Tolerate ":3000" and "localhost:3000", which are easy habits to bring
	// from other tools.
	if idx := strings.LastIndex(raw, ":"); idx >= 0 {
		raw = raw[idx+1:]
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%q is not a valid port (expected 1-65535)", fs.Arg(0))
	}
	return port, nil
}

// tunnelView renders agent activity to the terminal. The agent calls these
// methods from one goroutine per in-flight request, so writes are serialized to
// keep log lines whole.
type tunnelView struct {
	quiet     bool
	localAddr string
	profile   string
	server    string

	out sync.Mutex

	connected atomic.Bool
	requests  atomic.Int64
	url       atomic.Value // string
	startedAt atomic.Int64 // unix nanos
}

func (v *tunnelView) OnConnect(ack proto.HelloAck, reconnected bool) {
	v.out.Lock()
	defer v.out.Unlock()
	v.connected.Store(true)
	v.url.Store(ack.URL)
	if v.startedAt.Load() == 0 {
		v.startedAt.Store(time.Now().UnixNano())
	}

	if v.quiet {
		if !reconnected {
			fmt.Println(publicTarget(ack))
		}
		return
	}
	if reconnected {
		fmt.Fprintf(os.Stderr, "%s reconnected as %s\n", green("+"), bold(publicTarget(ack)))
		return
	}

	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "  %s  %s\n", bold(publicTarget(ack)), dim("→ "+v.localAddr))
	fmt.Fprintf(os.Stderr, "  %s\n", dim(fmt.Sprintf("server %s · profile %s · tunnel %s", v.server, v.profile, ack.TunnelID)))
	if ack.Subdomain != "" {
		fmt.Fprintf(os.Stderr, "  %s\n", dim("keep this name with: zerock reserve "+ack.Subdomain))
	}
	fmt.Fprintf(os.Stderr, "\n%s\n\n", dim("Ctrl-C to stop"))
}

func (v *tunnelView) OnEvent(ev proto.Event) {
	if v.quiet {
		return
	}
	v.out.Lock()
	defer v.out.Unlock()
	stamp := dim(ev.At.Local().Format("15:04:05"))

	switch ev.T {
	case proto.EventRequest:
		v.requests.Add(1)
		fmt.Fprintf(os.Stderr, "%s  %s %-6s %s %s\n",
			stamp, statusBadge(ev.Status), ev.Method, truncate(ev.Path, 60),
			dim(fmt.Sprintf("%.0fms", ev.Millis)))
	case proto.EventConn:
		v.requests.Add(1)
		fmt.Fprintf(os.Stderr, "%s  %s %s %s\n", stamp, cyan("tcp"), ev.Remote, dim(ev.Message))
	case proto.EventNotice:
		fmt.Fprintf(os.Stderr, "%s  %s %s\n", stamp, amber("!"), ev.Message)
	case proto.EventClose:
		fmt.Fprintf(os.Stderr, "%s  %s %s\n", stamp, amber("!"), ev.Message)
	}
}

func (v *tunnelView) OnDisconnect(err error, retryIn time.Duration) {
	if v.quiet {
		return
	}
	v.out.Lock()
	defer v.out.Unlock()
	reason := "connection lost"
	if err != nil {
		reason = err.Error()
	}
	fmt.Fprintf(os.Stderr, "%s %s %s\n", amber("-"), reason,
		dim(fmt.Sprintf("· retrying in %s", retryIn.Truncate(time.Second))))
}

// summary describes what the tunnel did, for the closing line.
func (v *tunnelView) summary() string {
	n := v.requests.Load()
	unit := "requests"
	if n == 1 {
		unit = "request"
	}
	started := v.startedAt.Load()
	if started == 0 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %s in %s", n, unit,
		time.Since(time.Unix(0, started)).Truncate(time.Second))
}

// publicTarget renders the address to hand out, for either tunnel type.
func publicTarget(ack proto.HelloAck) string {
	if ack.PublicAddr != "" {
		return ack.PublicAddr
	}
	return ack.URL
}

// statusBadge colours an HTTP status by class.
func statusBadge(status int) string {
	text := strconv.Itoa(status)
	switch {
	case status == 101:
		return cyan(text) // protocol upgrade, e.g. a WebSocket
	case status >= 500:
		return red(text)
	case status >= 400:
		return amber(text)
	case status >= 200 && status < 300:
		return green(text)
	default:
		return dim(text)
	}
}

// truncate shortens a long path for a single-line log.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if decoded, err := url.PathUnescape(s); err == nil && len(decoded) <= max {
		return decoded
	}
	return s[:max-1] + "…"
}
