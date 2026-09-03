// Package tray is the zerock menu bar / system tray widget: one place to start,
// watch and stop tunnels without keeping a terminal open for each. It runs the
// same agent as "zerock http" in-process, and lists the token's other tunnels
// through the server API so tunnels started elsewhere show up too.
//
// It behaves the same on macOS and Linux. The platform-specific part is the
// tray itself (Cocoa on macOS, the StatusNotifierItem D-Bus protocol on Linux),
// which fyne.io/systray hides behind one API.
package tray

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/erickdsama/zerock/internal/client"
	"github.com/erickdsama/zerock/internal/cliutil"
	"github.com/erickdsama/zerock/internal/proto"
)

// specUsage is the flag help for a tunnel spec. Short on purpose: the port is
// all that is needed, and the CLI's flags work for everything else.
const specUsage = `Local port, and a subdomain if you want one:
  3000 api-x   ·   tcp 5432 db   ·   3000 --auth user:pass`

// ParseSpec turns "3000 api-x", "tcp 5432 db" or the CLI's own
// "http 3000 --sub api-x" into a saved tunnel and a name for it. The type may
// be omitted, in which case it is http. The name comes from --name, then the
// subdomain, then "<type>-<port>".
func ParseSpec(input string) (name string, spec client.SavedTunnel, err error) {
	return parseSpecArgs(strings.Fields(input))
}

// parseSpecArgs is ParseSpec on already-split words, so a value with spaces
// (a basic-auth password from the form) survives.
func parseSpecArgs(args []string) (name string, spec client.SavedTunnel, err error) {
	fs := cliutil.NewFlagSet("tunnel", specUsage)
	sub := fs.String("sub", "", "subdomain to request")
	host := fs.String("host", "", "local host to forward to")
	auth := fs.String("auth", "", "basic auth as user:pass")
	remotePort := fs.Int("remote-port", 0, "public port to request")
	nameFlag := fs.String("name", "", "name to save the tunnel under")
	autostart := fs.Bool("autostart", false, "start when the widget launches")

	if len(args) == 0 {
		return "", spec, errors.New("nothing to open: expected something like \"http 3000 --sub api-x\"")
	}
	if err := cliutil.ParseFlags(fs, args); err != nil {
		return "", spec, err
	}

	// Operands: [http|tcp] <port> [subdomain]. A leading type word is optional,
	// and a trailing word is the subdomain, so "3000 api-x" needs no flags.
	operands := fs.Args()
	kind := string(proto.TypeHTTP)
	if len(operands) > 0 {
		switch first := strings.ToLower(operands[0]); first {
		case string(proto.TypeHTTP), string(proto.TypeTCP):
			kind, operands = first, operands[1:]
		}
	}
	switch len(operands) {
	case 1:
	case 2:
		if *sub != "" {
			return "", spec, fmt.Errorf("subdomain given twice: %q and --sub %s", operands[1], *sub)
		}
		*sub, operands = operands[1], operands[:1]
	default:
		return "", spec, errors.New("expected a port, e.g. \"3000 api-x\"")
	}

	raw := operands[0]
	// Tolerate ":3000" and "localhost:3000", as the CLI does.
	if idx := strings.LastIndex(raw, ":"); idx >= 0 {
		raw = raw[idx+1:]
	}
	port, convErr := strconv.Atoi(raw)
	if convErr != nil || port < 1 || port > 65535 {
		return "", spec, fmt.Errorf("%q is not a valid port (expected 1-65535)", operands[0])
	}
	if *auth != "" && !strings.Contains(*auth, ":") {
		return "", spec, errors.New("--auth expects user:pass")
	}
	if *auth != "" && kind == string(proto.TypeTCP) {
		return "", spec, errors.New("--auth only applies to http tunnels")
	}
	if *remotePort != 0 && kind == string(proto.TypeHTTP) {
		return "", spec, errors.New("--remote-port only applies to tcp tunnels")
	}

	spec = client.SavedTunnel{
		Type:       kind,
		Port:       port,
		Host:       strings.TrimSpace(*host),
		Subdomain:  strings.ToLower(strings.TrimSpace(*sub)),
		RemotePort: *remotePort,
		BasicAuth:  *auth,
		Autostart:  *autostart,
	}
	if spec.Host == "127.0.0.1" || spec.Host == "localhost" {
		spec.Host = ""
	}

	name = strings.TrimSpace(*nameFlag)
	if name == "" {
		name = spec.Subdomain
	}
	if name == "" {
		name = fmt.Sprintf("%s-%d", kind, port)
	}
	return name, spec, nil
}

// uniqueName appends a counter when a saved tunnel already uses the name, so a
// second "http 3000" does not silently overwrite the first.
func uniqueName(name string, taken map[string]client.SavedTunnel) string {
	if _, ok := taken[name]; !ok {
		return name
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", name, i)
		if _, ok := taken[candidate]; !ok {
			return candidate
		}
	}
}
