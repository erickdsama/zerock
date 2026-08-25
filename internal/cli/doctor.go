package cli

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/erickdsama/zerock/internal/client"
	"github.com/erickdsama/zerock/internal/server"
	"github.com/erickdsama/zerock/internal/version"
)

const usageDoctor = `Check that a zerock server is actually working.

Run it on the server to check the parts only visible from there: the config, the
listeners, the certificate and DNS. Run it anywhere to check that a saved profile
can reach the API.

Usage:
  zerock doctor [flags]

Examples:
  sudo zerock doctor                       on the server, using /etc/zerock/zerock.yaml
  zerock doctor --config ./zerock.yaml     against a specific config
  zerock doctor --profile prod             client-side checks only

Flags:
  --config path   server config to inspect (default /etc/zerock/zerock.yaml,
                  or $ZEROCK_CONFIG_FILE); skipped if it does not exist
  --profile name  saved profile for the client-side checks
  --timeout d     per-check timeout (default 8s)

What it cannot check: whether your ports are reachable from the internet. A
firewall between this host and the outside world looks identical to a working
server from here. Confirm that from another machine.
`

// checkState is the outcome of one check.
type checkState int

const (
	statePass checkState = iota
	stateWarn
	stateFail
	stateSkip
)

// check is one diagnostic result.
type check struct {
	name   string
	state  checkState
	detail string
	// hint is shown indented under a warning or failure.
	hint string
}

// report accumulates results and renders them.
type report struct {
	checks []check
}

func (r *report) add(c check) { r.checks = append(r.checks, c) }

func (r *report) pass(name, detail string) {
	r.add(check{name: name, state: statePass, detail: detail})
}
func (r *report) warn(name, detail, hint string) {
	r.add(check{name: name, state: stateWarn, detail: detail, hint: hint})
}
func (r *report) fail(name, detail, hint string) {
	r.add(check{name: name, state: stateFail, detail: detail, hint: hint})
}
func (r *report) skip(name, detail string) {
	r.add(check{name: name, state: stateSkip, detail: detail})
}

// render prints the report and returns an error when anything failed, so the
// command's exit code is usable in a script.
func (r *report) render() error {
	var failures, warnings int
	for _, c := range r.checks {
		var marker string
		switch c.state {
		case statePass:
			marker = green("✓")
		case stateWarn:
			marker = amber("!")
			warnings++
		case stateFail:
			marker = red("✗")
			failures++
		case stateSkip:
			marker = dim("-")
		}
		fmt.Printf("%s %-22s %s\n", marker, c.name, c.detail)
		if c.hint != "" {
			fmt.Printf("  %s\n", dim(c.hint))
		}
	}

	fmt.Println()
	switch {
	case failures > 0:
		return fmt.Errorf("%d check(s) failed", failures)
	case warnings > 0:
		fmt.Printf("%s\n", amber(fmt.Sprintf("%d warning(s); nothing fatal", warnings)))
	default:
		fmt.Printf("%s\n", green("all checks passed"))
	}
	return nil
}

func runDoctor(ctx context.Context, args []string) error {
	fs := newFlagSet("doctor", usageDoctor)
	configPath := fs.String("config", defaultServerConfigPath(), "server config to inspect")
	envPath := fs.String("env-file", "/etc/zerock/zerock.env", "service environment file to read credentials from")
	profile := profileFlag(fs)
	timeout := fs.Duration("timeout", 8*time.Second, "per-check timeout")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	r := &report{}
	r.pass("zerock build", version.String())

	// The service gets its credentials from an EnvironmentFile that systemd
	// reads, not the shell. Load it here so an interactive run sees the same
	// environment the running server does.
	loadServiceEnv(r, *envPath)

	cfg, haveConfig := doctorConfig(r, *configPath)
	// Even an unusable config should not stop the rest: systemd's view of the
	// service is often the fastest way to see what is actually wrong.
	running := doctorService(ctx, r)
	if haveConfig {
		doctorDNSCredential(r, cfg, *envPath)
		doctorListeners(ctx, r, cfg, *timeout, running)
		doctorCertificate(ctx, r, cfg, *timeout, running)
		doctorDNS(ctx, r, cfg, *timeout)
		doctorAdminAPI(ctx, r, cfg, *timeout)
	}
	doctorProfile(ctx, r, *profile, *timeout)

	return r.render()
}

// doctorConfig reads the server config for diagnosis, and reports separately
// whether a server could actually start from it.
func doctorConfig(r *report, path string) (server.Config, bool) {
	cfg, err := server.LoadConfigForInspection(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			r.skip("config", fmt.Sprintf("no config at %s (not a server?)", path))
			return cfg, false
		}
		if errors.Is(err, os.ErrPermission) {
			r.fail("config", "cannot read "+path, "re-run with sudo")
			return cfg, false
		}
		r.fail("config", err.Error(), "fix the config, then: systemctl restart zerock")
		return cfg, false
	}
	r.pass("config", fmt.Sprintf("%s · domain %s · tls %s", path, cfg.Domain, cfg.TLS.Mode))

	if err := cfg.ValidateForServing(); err != nil {
		r.fail("config usable", err.Error(), "the server cannot start until this is fixed")
	} else {
		r.pass("config usable", "the server can start from this config")
	}

	// A world-readable config holding a credential is worth repeating here.
	if warning := server.ConfigSecretWarning(path); warning != "" {
		r.warn("config secrets", "a credential is in a readable config", warning)
	}
	return cfg, true
}

// loadServiceEnv mirrors what systemd does with EnvironmentFile, so the checks
// see the credentials the running service sees.
func loadServiceEnv(r *report, path string) {
	loaded, err := applyEnvFile(path)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			r.skip("service environment", "no "+path)
		case errors.Is(err, os.ErrPermission):
			// The common case for a non-root run, and it makes every credential
			// check misleading, so it is a warning rather than a skip.
			r.warn("service environment", "cannot read "+path,
				"re-run with sudo, or credentials given to the service will look missing")
		default:
			r.warn("service environment", err.Error(), "")
		}
		return
	}
	// Names are reported, never values.
	r.pass("service environment", fmt.Sprintf("%s · %d variable(s) loaded", path, loaded))
}

// doctorDNSCredential reports whether the ACME challenge has a credential, and
// says where it came from, since "set in the wrong place" looks identical to
// "not set" from the server's point of view.
func doctorDNSCredential(r *report, cfg server.Config, envPath string) {
	if cfg.TLS.Mode != server.TLSAuto {
		r.skip("dns credential", "not needed unless tls.mode is auto")
		return
	}
	if !cfg.HasDNSCredential() {
		r.fail("dns credential", "no DNS token available",
			fmt.Sprintf("put a %s token in %s as ZEROCK_DNS_API_TOKEN, then: systemctl restart zerock",
				cfg.TLS.DNSProvider, envPath))
		return
	}
	r.pass("dns credential", "present for the "+cfg.TLS.DNSProvider+" provider")
}

// doctorService reports what systemd thinks, which is the difference between
// "not running" and "running but broken".
func doctorService(ctx context.Context, r *report) bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		r.skip("service", "systemctl not available")
		// Unknown rather than false: without systemd the server may well be
		// running under something else.
		return true
	}
	out, err := exec.CommandContext(ctx, "systemctl", "is-active", "zerock").Output()
	state := strings.TrimSpace(string(out))
	if err != nil || state != "active" {
		if state == "" {
			state = "unknown"
		}
		hint := "journalctl -u zerock -n 40 --no-pager"
		if state == "activating" || state == "failed" {
			hint = "it is crash-looping; the reason is in: journalctl -u zerock -n 40 --no-pager"
		}
		r.fail("service", "zerock.service is "+state, hint)
		return false
	}

	detail := "active"
	if enabled, err := exec.CommandContext(ctx, "systemctl", "is-enabled", "zerock").Output(); err == nil {
		detail += " · " + strings.TrimSpace(string(enabled)) + " at boot"
	}
	r.pass("service", detail)
	return true
}

// doctorListeners dials each configured port locally. A refused connection here
// means the process is not listening, which is separate from a firewall.
func doctorListeners(ctx context.Context, r *report, cfg server.Config, timeout time.Duration, running bool) {
	type target struct {
		name string
		addr string
	}
	targets := []target{{"control port", cfg.ControlAddr}}
	if cfg.TLS.Mode == server.TLSOff {
		targets = append(targets, target{"edge (http)", cfg.HTTPAddr})
	} else {
		targets = append(targets, target{"edge (https)", cfg.HTTPSAddr})
	}
	if cfg.AdminAddr != "" {
		targets = append(targets, target{"admin api", cfg.AdminAddr})
	}

	for _, t := range targets {
		addr := dialableAddr(t.addr)
		conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
		if err != nil {
			r.fail(t.name, "not listening on "+t.addr,
				"the server may have failed to bind; check: journalctl -u zerock -n 30 --no-pager")
			continue
		}
		conn.Close()
		if !running {
			// zerock is not up, so whatever answered is a different process.
			// Reporting this as healthy would send the diagnosis the wrong way,
			// and a port already held is a common reason zerock cannot start.
			hint := "another process holds this port; find it with: ss -tlnp | grep " + portOf(t.addr)
			if t.name == "edge (https)" || t.name == "edge (http)" {
				hint += "\n  if that is a reverse proxy, let it keep the port and run zerock behind it: " +
					"zerock service install --domain " + cfg.Domain + " --behind-proxy"
			}
			r.warn(t.name, "something is listening on "+t.addr+", but it is not zerock", hint)
			continue
		}
		r.pass(t.name, "listening on "+t.addr)
	}
}

// doctorCertificate completes a TLS handshake against the local edge and reports
// what the server actually serves. This is the check that catches a wildcard
// that was never issued.
func doctorCertificate(ctx context.Context, r *report, cfg server.Config, timeout time.Duration, running bool) {
	if cfg.TLS.Mode == server.TLSOff {
		r.skip("certificate", "tls.mode is off; whatever is in front terminates TLS")
		return
	}

	if !running {
		r.skip("certificate", "zerock is not running; whatever is on "+cfg.HTTPSAddr+" is not serving zerock's certificate")
		return
	}

	// A name under the wildcard, so SNI exercises the same certificate a real
	// tunnel would be served with.
	sni := probeLabel() + "." + cfg.Domain
	dialer := &net.Dialer{Timeout: timeout}
	raw, err := dialer.DialContext(ctx, "tcp", dialableAddr(cfg.HTTPSAddr))
	if err != nil {
		r.fail("certificate", "could not connect to "+cfg.HTTPSAddr, "")
		return
	}
	defer raw.Close()

	conn := tls.Client(raw, &tls.Config{
		ServerName: sni,
		// The point is to inspect what is served, not to trust it; validation is
		// done explicitly below so the failure can be described precisely.
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	handshakeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := conn.HandshakeContext(handshakeCtx); err != nil {
		// Issuance is asynchronous, so a handshake can fail simply because the
		// certificate has not arrived yet. Saying which it is needs the log.
		r.warn("certificate", "TLS handshake failed: "+err.Error(),
			"either issuance is still in progress or it failed; watch it with: "+
				"journalctl -u zerock -f | grep -i acme")
		return
	}
	defer conn.Close()

	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		r.fail("certificate", "the server presented no certificate", "")
		return
	}
	leaf := certs[0]

	wildcard := "*." + cfg.Domain
	covered := false
	for _, name := range leaf.DNSNames {
		if name == wildcard {
			covered = true
			break
		}
	}
	daysLeft := int(time.Until(leaf.NotAfter).Hours() / 24)
	detail := fmt.Sprintf("%s · issuer %q · %d days left",
		strings.Join(leaf.DNSNames, ", "), leaf.Issuer.CommonName, daysLeft)

	switch {
	case !covered:
		r.fail("certificate", detail,
			fmt.Sprintf("the certificate does not cover %s, so tunnel subdomains will not validate", wildcard))
	case isStagingIssuer(leaf):
		r.warn("certificate", detail,
			"this is a Let's Encrypt STAGING certificate: browsers will reject it. "+
				"Remove 'ca: staging' from the config, delete the cache "+
				"(rm -rf "+cfg.DataDir+"/certmagic), and restart.")
	case daysLeft < 14:
		r.warn("certificate", detail, "renewal should have happened by now; check the ACME logs")
	default:
		r.pass("certificate", detail)
	}

	// Validate properly too, so a chain problem is not hidden by the inspection.
	roots, err := x509.SystemCertPool()
	if err == nil {
		opts := x509.VerifyOptions{DNSName: sni, Roots: roots, Intermediates: x509.NewCertPool()}
		for _, c := range certs[1:] {
			opts.Intermediates.AddCert(c)
		}
		if _, err := leaf.Verify(opts); err != nil {
			if isStagingIssuer(leaf) {
				r.skip("certificate trust", "not checked for a staging certificate")
			} else {
				r.fail("certificate trust", err.Error(),
					"a client would reject this certificate")
			}
		} else {
			r.pass("certificate trust", "validates against the system roots")
		}
	}
}

// isStagingIssuer reports whether a certificate came from a test CA.
func isStagingIssuer(cert *x509.Certificate) bool {
	name := strings.ToLower(cert.Issuer.CommonName + " " + strings.Join(cert.Issuer.Organization, " "))
	return strings.Contains(name, "staging") || strings.Contains(name, "(staging)")
}

// doctorDNS checks the names zerock actually serves.
//
// The wildcard is what every tunnel hostname needs, and the API host is where
// the management API and dashboard live. The apex is deliberately not required:
// pointing it elsewhere, at a site on another host, is a normal arrangement, so
// it is reported for information rather than judged.
func doctorDNS(ctx context.Context, r *report, cfg server.Config, timeout time.Duration) {
	resolver := &net.Resolver{}
	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	local := localAddresses()

	// resolve reports where a name points and whether that is this host.
	resolve := func(name string) (addrs []string, here bool, err error) {
		addrs, err = resolver.LookupHost(lookupCtx, name)
		if err != nil {
			return nil, false, err
		}
		for _, a := range addrs {
			if local[a] {
				return addrs, true, nil
			}
		}
		return addrs, false, nil
	}

	// The wildcard: without it no tunnel hostname resolves at all. The probe
	// label is random per run, because a fixed one gets negatively cached: after
	// the record is finally added, a resolver would keep returning NXDOMAIN for
	// the name we always ask about, and the check would lie.
	addrs, here, err := resolve(probeLabel() + "." + cfg.Domain)
	switch {
	case err != nil:
		r.fail("dns wildcard", "*."+cfg.Domain+" does not resolve",
			"add a wildcard A record: *."+cfg.Domain+" → this host's public IP")
	case here:
		r.pass("dns wildcard", strings.Join(addrs, ", ")+" (this host)")
	default:
		r.warn("dns wildcard", strings.Join(addrs, ", "),
			"none of these is an address on this host; correct if a proxy or load balancer "+
				"fronts this server, otherwise fix the wildcard record")
	}

	// The API host: the dashboard and management API are served here.
	addrs, here, err = resolve(cfg.APIHost)
	switch {
	case err != nil:
		r.warn("dns api host", cfg.APIHost+" does not resolve",
			"the dashboard and API will be unreachable by name; the wildcard normally covers this")
	case here:
		r.pass("dns api host", cfg.APIHost+" → "+strings.Join(addrs, ", ")+" (this host)")
	default:
		r.warn("dns api host", cfg.APIHost+" → "+strings.Join(addrs, ", "),
			"not an address on this host; fine behind a proxy, otherwise the dashboard will not be reachable")
	}

	// The apex is informational: zerock does not serve it unless api_host is set
	// to it.
	if addrs, here, err := resolve(cfg.Domain); err != nil {
		r.skip("dns apex", cfg.Domain+" does not resolve (not required)")
	} else if here {
		r.skip("dns apex", cfg.Domain+" → "+strings.Join(addrs, ", ")+" (this host)")
	} else {
		r.skip("dns apex", cfg.Domain+" → "+strings.Join(addrs, ", ")+" (elsewhere; not required by zerock)")
	}
}

// doctorAdminAPI calls the loopback health endpoint, which proves the server is
// serving rather than merely listening.
func doctorAdminAPI(ctx context.Context, r *report, cfg server.Config, timeout time.Duration) {
	if cfg.AdminAddr == "" {
		r.skip("health endpoint", "no admin_addr configured")
		return
	}
	url := "http://" + dialableAddr(cfg.AdminAddr) + "/healthz"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		r.fail("health endpoint", err.Error(), "")
		return
	}
	req.Header.Set("Accept", "application/json")

	httpClient := &http.Client{Timeout: timeout}
	resp, err := httpClient.Do(req)
	if err != nil {
		r.fail("health endpoint", err.Error(), "")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		r.fail("health endpoint", fmt.Sprintf("HTTP %d from %s", resp.StatusCode, url), "")
		return
	}
	r.pass("health endpoint", "200 from "+url)
}

// doctorProfile exercises the same path the CLI uses, end to end.
func doctorProfile(ctx context.Context, r *report, profileName string, timeout time.Duration) {
	cfg, err := client.LoadConfig()
	if err != nil {
		r.skip("cli profile", "could not read the CLI config: "+err.Error())
		return
	}
	name, prof, err := cfg.Resolve(profileName)
	if err != nil {
		r.skip("cli profile", "no profile configured on this machine")
		return
	}
	r.pass("cli profile", fmt.Sprintf("%s → %s", name, prof.ControlAddr()))

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out struct {
		Token struct {
			Label  string   `json:"label"`
			Scopes []string `json:"scopes"`
		} `json:"token"`
		Domain string `json:"domain"`
	}
	if err := client.NewAPI(prof).Do(callCtx, "GET", "/api/v1/whoami", nil, &out); err != nil {
		r.fail("api reachable", err.Error(),
			"the API is served on the api_host over HTTPS, and on admin_addr over loopback")
		return
	}
	r.pass("api reachable", fmt.Sprintf("token %q (%s) · domain %s",
		out.Token.Label, strings.Join(out.Token.Scopes, ","), out.Domain))
}

// probeLabel returns a throwaway DNS label, fresh on every call so no resolver
// has a cached answer for it.
func probeLabel() string {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Any label is fine here; only its uniqueness matters, and a fixed
		// fallback is still better than failing the check outright.
		return "zerock-probe"
	}
	return "zerock-probe-" + hex.EncodeToString(b[:])
}

// portOf extracts the port from a listen address, for suggesting a command.
func portOf(listen string) string {
	if _, port, err := net.SplitHostPort(listen); err == nil {
		return ":" + port
	}
	return listen
}

// dialableAddr turns a listen address into one that can be dialled: a bare
// ":443" means every interface, which is not a destination.
func dialableAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if _, err := strconv.Atoi(port); err != nil {
		return listen
	}
	return net.JoinHostPort(host, port)
}

// localAddresses collects every address on this host, for comparing against DNS.
func localAddresses() map[string]bool {
	out := map[string]bool{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			out[ipnet.IP.String()] = true
		}
	}
	return out
}
