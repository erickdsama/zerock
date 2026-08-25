package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/erickdsama/zerock/internal/server"
	"github.com/erickdsama/zerock/internal/version"
)

const usageService = `Install zerock as a systemd service, so it keeps running after you log out.

Everything is embedded in this binary: nothing else needs to be copied to the
server.

Usage:
  zerock service install --domain <domain> [flags]
  zerock service uninstall [--purge]
  zerock service tunnel <name> -- <tunnel args...>

Examples:
  sudo zerock service install --domain example.com --email you@example.com
  sudo zerock service install --domain example.com --behind-proxy
  sudo zerock service install --domain example.com --dry-run
  sudo zerock service uninstall
  sudo zerock service tunnel api-x -- http 3000 --sub api-x --quiet

'install' copies this binary to /usr/local/bin, writes /etc/zerock/zerock.yaml
and /etc/zerock/zerock.env, installs the unit and starts it. Re-running keeps an
existing config, so it is also the upgrade path.

'tunnel' installs a long-lived agent: a tunnel on this machine that outlives your
SSH session. Use it when the app you are publishing is not on the zerock server.

Flags for 'install':
  --domain name        the domain whose subdomains you will hand out (required)
  --email address      ACME contact address for certificate notices
  --dns-provider name  DNS provider for the wildcard certificate (default cloudflare)
  --behind-proxy       configure for running behind Caddy, nginx or Traefik
  --prefix path     where to install the binary (default /usr/local)
  --dry-run         print what would change and exit
  --no-start        install everything but do not start the service

Flags for 'uninstall':
  --purge           also delete the config and the token database
  --dry-run         print what would be removed and exit

Flags for 'tunnel':
  --server host     zerock server (default: the current profile's)
  --token zk_...    token to use (default: the current profile's)
  --dry-run         print what would change and exit
`

// File modes for what install writes.
//
// configFileMode is deliberately world-readable: the unit uses DynamicUser, so
// the process reads its config as an unprivileged dynamic UID rather than as
// root. Anything secret goes in the environment file at secretFileMode, which
// systemd reads as root and hands over after dropping privileges.
const (
	configFileMode os.FileMode = 0o644
	secretFileMode os.FileMode = 0o600
)

// Paths the service commands write to. Grouped so install, uninstall and the
// dry-run output cannot drift apart.
type servicePaths struct {
	prefix    string
	binary    string
	configDir string
	config    string
	env       string
	unit      string
	tunnelDir string
}

func newServicePaths(prefix string) servicePaths {
	return servicePaths{
		prefix:    prefix,
		binary:    filepath.Join(prefix, "bin", "zerock"),
		configDir: "/etc/zerock",
		config:    "/etc/zerock/zerock.yaml",
		env:       "/etc/zerock/zerock.env",
		unit:      "/etc/systemd/system/zerock.service",
		tunnelDir: "/etc/zerock/tunnels",
	}
}

func runService(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("expected a subcommand: install, uninstall or tunnel")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "install":
		return runServiceInstall(ctx, rest)
	case "uninstall", "remove":
		return runServiceUninstall(ctx, rest)
	case "tunnel":
		return runServiceTunnel(ctx, rest)
	case "-h", "--help":
		fmt.Print(usageService)
		return nil
	default:
		return fmt.Errorf("unknown service subcommand %q (want install, uninstall or tunnel)", sub)
	}
}

func runServiceInstall(ctx context.Context, args []string) error {
	fs := newFlagSet("service install", usageService)
	domain := fs.String("domain", "", "the domain to serve subdomains of")
	email := fs.String("email", "", "ACME contact address")
	dnsProvider := fs.String("dns-provider", "cloudflare", "DNS provider for the wildcard certificate")
	behindProxy := fs.Bool("behind-proxy", false, "configure for running behind a reverse proxy")
	prefix := fs.String("prefix", "/usr/local", "where to install the binary")
	dryRun := fs.Bool("dry-run", false, "print what would change and exit")
	noStart := fs.Bool("no-start", false, "install but do not start")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if strings.TrimSpace(*domain) == "" {
		return errors.New("--domain is required, e.g. --domain example.com")
	}
	if !*behindProxy && !slices.Contains(server.SupportedDNSProviders(), *dnsProvider) {
		return fmt.Errorf("--dns-provider %q is not supported; built in: %s\n  %s",
			*dnsProvider, strings.Join(server.SupportedDNSProviders(), ", "),
			dim("for any other provider use --behind-proxy, or tls.mode files with a certificate from elsewhere"))
	}
	// Argument problems are reported before the root check, so a missing flag
	// does not need a second run under sudo to discover.
	if err := requireSystemd(); err != nil {
		return err
	}
	if err := requireRoot(*dryRun); err != nil {
		return err
	}

	p := newServicePaths(*prefix)
	plan := &plan{dryRun: *dryRun}

	self, err := selfPath()
	if err != nil {
		return err
	}

	dataDir := "/var/lib/zerock"
	configBody := renderServerConfig(*domain, *email, dataDir, *dnsProvider, *behindProxy)
	unitBody := serverUnit(p.binary)

	plan.copyFile("install this binary", self, p.binary, 0o755)
	plan.mkdir(p.configDir, 0o755)
	// The config must be readable by the service, which runs under DynamicUser
	// and so is not root. Secrets belong in the environment file instead: that
	// one is read by systemd as root before privileges are dropped, so it can
	// stay at 0600.
	plan.writeFileIfAbsent("server config", p.config, configBody, configFileMode)
	// An install from before the service ran unprivileged left the config at
	// 0600, which the service cannot read. Re-running install has to repair that
	// rather than preserve it.
	plan.ensureReadable("config permissions", p.config, configFileMode)
	plan.writeFileIfAbsent("DNS credential file", p.env, dnsEnvBody(*dnsProvider), secretFileMode)
	plan.writeFile("systemd unit", p.unit, unitBody, 0o644)

	if err := plan.run(); err != nil {
		return err
	}
	if *dryRun {
		fmt.Printf("\n%s\n", dim("nothing was changed; drop --dry-run to apply"))
		return nil
	}

	if err := systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	fmt.Printf("%s reloaded systemd\n", green("✓"))

	if *noStart {
		fmt.Printf("\n%s\n", dim("not started, as asked. Start it with: systemctl enable --now zerock"))
		return nil
	}

	// Starting a service that cannot work only produces a crash loop, so say
	// what is missing instead.
	if blocker := startBlocker(p.config, p.env); blocker != nil {
		fmt.Printf("\n%s %v\n\n", amber("Not started:"), blocker)
		fmt.Printf("  %s\n", dim("sudo nano "+p.env+"    # set ZEROCK_DNS_API_TOKEN"))
		fmt.Printf("  %s\n\n", dim("sudo systemctl enable --now zerock"))
		fmt.Printf("%s\n", dim("Then read the admin token it prints once:"))
		fmt.Printf("  %s\n", dim("sudo journalctl -u zerock | grep zk_"))
		return nil
	}

	// systemctl exits non-zero when the unit fails to start, which is precisely
	// when the journal matters most: returning that bare error would hide the
	// reason. Either way, fall through to the diagnosis below.
	enableErr := systemctl(ctx, "enable", "--now", "zerock")

	// Give it a moment to fail, so a bad config is reported here rather than
	// discovered later.
	time.Sleep(2 * time.Second)
	if enableErr != nil || !serviceActive(ctx, "zerock") {
		fmt.Fprintf(os.Stderr, "\n%s zerock did not stay up. Last lines of its log:\n\n", red("error:"))
		showJournal(ctx, "zerock", 30)
		explainStartFailure(ctx, p)
		return errors.New("the service failed to start")
	}

	fmt.Printf("%s zerock is running, and enabled at boot\n\n", green("✓"))
	fmt.Printf("  %s\n", dim("status:  systemctl status zerock"))
	fmt.Printf("  %s\n", dim("logs:    journalctl -u zerock -f"))
	fmt.Printf("  %s\n", dim("restart: systemctl restart zerock"))

	if token := findBootstrapToken(ctx); token != "" {
		fmt.Printf("\n%s\n\n", bold("Admin token (printed once, at first start):"))
		fmt.Printf("  %s\n\n", bold(token))
		fmt.Printf("  %s\n", dim(fmt.Sprintf("zerock login --server zerock.%s --token %s", *domain, token)))
	}
	return nil
}

func runServiceUninstall(ctx context.Context, args []string) error {
	fs := newFlagSet("service uninstall", usageService)
	purge := fs.Bool("purge", false, "also delete the config and token database")
	dryRun := fs.Bool("dry-run", false, "print what would be removed and exit")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireSystemd(); err != nil {
		return err
	}
	if err := requireRoot(*dryRun); err != nil {
		return err
	}

	p := newServicePaths("/usr/local")
	plan := &plan{dryRun: *dryRun}

	if !*dryRun {
		// Errors are ignored: the service may already be stopped or absent, and
		// neither should abort the rest of the removal.
		_ = systemctl(ctx, "disable", "--now", "zerock")
		fmt.Printf("%s stopped and disabled zerock\n", green("✓"))
	}
	plan.remove("systemd unit", p.unit)
	if *purge {
		// Deleting the database destroys every token, which cannot be undone.
		plan.remove("config directory", p.configDir)
		plan.remove("token database and certificates", "/var/lib/zerock")
	}

	if err := plan.run(); err != nil {
		return err
	}
	if *dryRun {
		fmt.Printf("\n%s\n", dim("nothing was changed; drop --dry-run to apply"))
		return nil
	}
	if err := systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}

	if !*purge {
		fmt.Printf("\n%s\n", dim("kept "+p.configDir+" and /var/lib/zerock (your tokens). Use --purge to delete them."))
	}
	fmt.Printf("%s\n", dim("the binary at "+p.binary+" was left in place"))
	return nil
}

func runServiceTunnel(ctx context.Context, args []string) error {
	fs := newFlagSet("service tunnel", usageService)
	server := fs.String("server", "", "zerock server")
	token := fs.String("token", "", "token to use")
	prefix := fs.String("prefix", "/usr/local", "where to install the binary")
	dryRun := fs.Bool("dry-run", false, "print what would change and exit")
	profile := profileFlag(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	// The instance name comes first, then the tunnel's own arguments. Those are
	// passed through untouched, so anything 'zerock http' accepts works here.
	rest := fs.Args()
	if len(rest) < 2 {
		return errors.New("expected an instance name and the tunnel arguments, e.g.\n  zerock service tunnel api-x -- http 3000 --sub api-x --quiet")
	}
	name, tunnelArgs := rest[0], rest[1:]
	if !validInstanceName(name) {
		return fmt.Errorf("%q is not a usable instance name (letters, digits, dots and hyphens)", name)
	}
	switch tunnelArgs[0] {
	case "http", "tcp":
	default:
		return fmt.Errorf("the tunnel arguments must start with http or tcp, got %q", tunnelArgs[0])
	}

	if err := requireSystemd(); err != nil {
		return err
	}
	if err := requireRoot(*dryRun); err != nil {
		return err
	}

	// Fall back to the saved profile so the token does not have to be retyped.
	host, secret := *server, *token
	if host == "" || secret == "" {
		_, prof, err := resolveProfile(*profile)
		if err != nil {
			return fmt.Errorf("%w\n  %s", err, dim("or pass --server and --token explicitly"))
		}
		if host == "" {
			host = prof.Host
			if prof.ControlPort != 0 {
				host = fmt.Sprintf("%s:%d", prof.Host, prof.ControlPort)
			}
		}
		if secret == "" {
			secret = prof.Token
		}
	}

	p := newServicePaths(*prefix)
	instanceEnv := filepath.Join(p.tunnelDir, name+".env")
	unitPath := "/etc/systemd/system/zerock-tunnel@.service"

	self, err := selfPath()
	if err != nil {
		return err
	}

	// The token goes in the environment file, not ExecStart, where ps would show
	// it to every user on the box.
	envBody := fmt.Sprintf("ZEROCK_SERVER=%s\nZEROCK_TOKEN=%s\nZEROCK_ARGS=%s\n",
		host, secret, strings.Join(tunnelArgs, " "))

	plan := &plan{dryRun: *dryRun}
	plan.copyFile("install this binary", self, p.binary, 0o755)
	plan.mkdir(p.tunnelDir, 0o700)
	plan.writeFile("tunnel unit template", unitPath, tunnelUnit(p.binary), 0o644)
	plan.writeFile("tunnel "+name, instanceEnv, envBody, secretFileMode)

	if err := plan.run(); err != nil {
		return err
	}
	if *dryRun {
		fmt.Printf("\n%s\n", dim("nothing was changed; drop --dry-run to apply"))
		return nil
	}
	if err := systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	unitName := "zerock-tunnel@" + name
	if err := systemctl(ctx, "enable", "--now", unitName); err != nil {
		return err
	}

	time.Sleep(2 * time.Second)
	if !serviceActive(ctx, unitName) {
		fmt.Fprintf(os.Stderr, "\n%s %s did not stay up. Last lines:\n\n", red("error:"), unitName)
		showJournal(ctx, unitName, 25)
		return errors.New("the tunnel service failed to start")
	}

	fmt.Printf("%s %s is running, and enabled at boot\n\n", green("✓"), bold(unitName))
	fmt.Printf("  %s\n", dim("logs:  journalctl -u "+unitName+" -f"))
	fmt.Printf("  %s\n", dim("stop:  systemctl disable --now "+unitName))
	return nil
}

// --- plan: describe the changes, then apply or print them ---

// step is one filesystem change.
type step struct {
	what   string
	target string
	apply  func() error
	// note explains an outcome that is not a plain write, such as a file being
	// kept because it already exists.
	note string
}

// plan collects steps so --dry-run and the real run describe the same work.
type plan struct {
	dryRun bool
	steps  []step
}

func (p *plan) add(s step) { p.steps = append(p.steps, s) }

func (p *plan) mkdir(path string, mode os.FileMode) {
	p.add(step{what: "directory", target: path, apply: func() error {
		return os.MkdirAll(path, mode)
	}})
}

func (p *plan) writeFile(what, path, body string, mode os.FileMode) {
	p.add(step{what: what, target: path, apply: func() error {
		return writeFileAtomic(path, []byte(body), mode)
	}})
}

// writeFileIfAbsent keeps an existing file, which is what makes install
// re-runnable as an upgrade without clobbering configuration.
func (p *plan) writeFileIfAbsent(what, path, body string, mode os.FileMode) {
	exists := fileExists(path)
	s := step{what: what, target: path, apply: func() error {
		if fileExists(path) {
			return nil
		}
		return writeFileAtomic(path, []byte(body), mode)
	}}
	if exists {
		s.note = "kept, already exists"
	}
	p.add(s)
}

// ensureReadable repairs a file the service must read but cannot, leaving any
// already-sufficient permissions untouched.
func (p *plan) ensureReadable(what, path string, mode os.FileMode) {
	info, err := os.Stat(path)
	if err != nil {
		// Nothing to repair: the write step above will have created it.
		p.add(step{what: what, target: path, note: "nothing to repair", apply: func() error { return nil }})
		return
	}
	if info.Mode().Perm()&0o044 != 0 {
		p.add(step{what: what, target: path, note: "already readable", apply: func() error { return nil }})
		return
	}
	current := info.Mode().Perm()
	p.add(step{
		what:   what,
		target: path,
		note:   fmt.Sprintf("repairing %04o, unreadable by the service, to %04o", current, mode),
		apply:  func() error { return os.Chmod(path, mode) },
	})
}

func (p *plan) copyFile(what, from, to string, mode os.FileMode) {
	if sameFile(from, to) {
		p.add(step{what: what, target: to, note: "already in place", apply: func() error { return nil }})
		return
	}
	p.add(step{what: what, target: to, apply: func() error {
		body, err := os.ReadFile(from)
		if err != nil {
			return fmt.Errorf("read %s: %w", from, err)
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return err
		}
		// A running binary cannot be written in place, so replace it by rename.
		return writeFileAtomic(to, body, mode)
	}})
}

func (p *plan) remove(what, path string) {
	if !fileExists(path) {
		p.add(step{what: what, target: path, note: "not present", apply: func() error { return nil }})
		return
	}
	p.add(step{what: what, target: path, apply: func() error { return os.RemoveAll(path) }})
}

// run applies the plan, or prints it when dryRun is set.
func (p *plan) run() error {
	for _, s := range p.steps {
		if p.dryRun {
			suffix := ""
			if s.note != "" {
				suffix = " " + dim("("+s.note+")")
			}
			fmt.Printf("  %s %s %s%s\n", dim("would write"), s.what+":", s.target, suffix)
			continue
		}
		if err := s.apply(); err != nil {
			return fmt.Errorf("%s (%s): %w", s.what, s.target, err)
		}
		if s.note != "" {
			fmt.Printf("%s %s: %s %s\n", green("✓"), s.what, s.target, dim("("+s.note+")"))
		} else {
			fmt.Printf("%s %s: %s\n", green("✓"), s.what, s.target)
		}
	}
	return nil
}

// --- helpers ---

// requireRoot reports a clear error when privileges are missing. A dry run needs
// none, since it changes nothing.
func requireRoot(dryRun bool) error {
	if dryRun || os.Geteuid() == 0 {
		return nil
	}
	return errors.New("this needs root; re-run with sudo")
}

// requireSystemd checks that systemd is actually managing this machine.
func requireSystemd() error {
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		return nil
	}
	return errors.New("systemd is not running on this machine\n  " +
		dim("run 'zerock serve --config /etc/zerock/zerock.yaml' under whatever supervisor you use"))
}

// selfPath resolves this executable, following symlinks so the copy is the real
// binary rather than a link that may not exist on the target path.
func selfPath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate this binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		return resolved, nil
	}
	return self, nil
}

// sameFile reports whether two paths are the same file on disk.
func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// writeFileAtomic writes through a temporary file and renames, so an interrupted
// write cannot leave a half-written config, and a running binary can be replaced.
func writeFileAtomic(path string, body []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".zerock-tmp"
	if err := os.WriteFile(tmp, body, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// systemctl runs one systemctl command, surfacing its output on failure.
func systemctl(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// serviceActive reports whether a unit is running.
func serviceActive(ctx context.Context, unit string) bool {
	return exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", unit).Run() == nil
}

// showJournal prints the last n lines of a unit's log.
func showJournal(ctx context.Context, unit string, n int) {
	cmd := exec.CommandContext(ctx, "journalctl", "-u", unit, "-n", fmt.Sprint(n), "--no-pager")
	out, _ := cmd.CombinedOutput()
	fmt.Fprintln(os.Stderr, strings.TrimSpace(string(out)))
}

// tokenPattern matches a zerock token in log output.
var tokenPattern = regexp.MustCompile(`zk_[a-z0-9]+_[a-z0-9]+`)

// findBootstrapToken digs the first-start admin token out of the journal. The
// service has only just started, so it retries briefly.
func findBootstrapToken(ctx context.Context) string {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cmd := exec.CommandContext(ctx, "journalctl", "-u", "zerock", "--no-pager")
		out, err := cmd.Output()
		if err == nil {
			if match := tokenPattern.Find(out); match != nil {
				return string(match)
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return ""
}

// readEnvFile parses a systemd EnvironmentFile into key/value pairs.
func readEnvFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if key == "" {
			continue
		}
		out[key] = value
	}
	return out, nil
}

// applyEnvFile puts an EnvironmentFile's variables into this process, mirroring
// what systemd does for the service. Values already set win, so an explicit
// override on the command line is respected. It returns how many were applied.
func applyEnvFile(path string) (int, error) {
	vars, err := readEnvFile(path)
	if err != nil {
		return 0, err
	}
	var n int
	for key, value := range vars {
		if value == "" || os.Getenv(key) != "" {
			continue
		}
		if err := os.Setenv(key, value); err == nil {
			n++
		}
	}
	return n, nil
}

// startBlocker reports why the service would not start with what is on disk, or
// nil if it would.
//
// This defers to the server's own validation rather than re-deriving it here. A
// second, independent guess at the same question is how an install came to start
// a service whose config it had just declared fine: an empty
// "dns_api_token: \"\"" read as a value to a regex, but not to the parser.
func startBlocker(configPath, envPath string) error {
	// The service sees the environment file, so this check must too.
	_, _ = applyEnvFile(envPath)

	cfg, err := server.LoadConfigForInspection(configPath)
	if err != nil {
		return err
	}
	return cfg.ValidateForServing()
}

// validInstanceName limits a systemd instance name to characters that do not
// need escaping.
var validInstanceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`).MatchString

// renderServerConfig produces the config file body.
func renderServerConfig(domain, email, dataDir, dnsProvider string, behindProxy bool) string {
	if behindProxy {
		return fmt.Sprintf(proxiedConfigTemplate, version.Version, domain, dataDir)
	}
	return fmt.Sprintf(autoConfigTemplate, version.Version, domain, dataDir, email,
		dnsProvider, strings.Join(server.SupportedDNSProviders(), ", "), dnsTokenHint(dnsProvider))
}

// dnsTokenHint says what the provider's credential actually needs, since the
// wrong scope is the most common reason issuance fails.
func dnsTokenHint(provider string) string {
	switch provider {
	case "cloudflare":
		return "Cloudflare: an API token with Zone:Read and DNS:Edit, scoped to this zone only."
	case "digitalocean":
		return "DigitalOcean: a personal access token with read and write scope."
	default:
		return "Use a credential permitted to create and delete TXT records in this zone."
	}
}

// explainStartFailure points at the handful of things that actually stop a fresh
// install from starting, so the journal above does not have to be read cold.
func explainStartFailure(ctx context.Context, p servicePaths) {
	fmt.Fprintf(os.Stderr, "\n%s\n", bold("Most likely causes:"))

	// A port already taken is invisible in the config and obvious in ss output.
	for _, port := range []string{"80", "443"} {
		if holder := whoHasPort(ctx, port); holder != "" {
			fmt.Fprintf(os.Stderr, "  %s port %s is already held by %s\n", red("·"), port, holder)
			fmt.Fprintf(os.Stderr, "    %s\n",
				dim("let it keep the port and run zerock behind it, with --behind-proxy"))
		}
	}

	fmt.Fprintf(os.Stderr, "  %s the DNS API token is missing, or lacks write access\n", dim("·"))
	fmt.Fprintf(os.Stderr, "    %s\n", dim("check "+p.env+" — a read-only token fails the DNS-01 challenge"))
	fmt.Fprintf(os.Stderr, "  %s something in the config is wrong\n", dim("·"))
	fmt.Fprintf(os.Stderr, "    %s\n", dim("run: zerock doctor"))
}

// whoHasPort reports which process is listening on a port, if ss can say.
func whoHasPort(ctx context.Context, port string) string {
	out, err := exec.CommandContext(ctx, "ss", "-tlnpH").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, ":"+port+" ") {
			continue
		}
		// users:(("nginx",pid=123,fd=7)) -> nginx
		if i := strings.Index(line, `users:(("`); i >= 0 {
			rest := line[i+len(`users:(("`):]
			if j := strings.Index(rest, `"`); j > 0 {
				return rest[:j]
			}
		}
		return "another process"
	}
	return ""
}
