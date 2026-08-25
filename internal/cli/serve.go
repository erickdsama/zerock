package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/erickdsama/zerock/internal/server"
	"github.com/erickdsama/zerock/internal/version"
)

const usageServe = `Run the zerock server. This is the side that owns your domain.

Before the first run:
  1. Point a wildcard A/AAAA record at this host: *.example.com and example.com
  2. Write a config (start from 'zerock init-config --domain example.com')
  3. Open the control port (7223), 443 and, for TCP tunnels, the port range

On the very first run the server mints an admin token and prints it once.

Usage:
  zerock serve [flags]

Flags:
  --config path   config file (default /etc/zerock/zerock.yaml, or $ZEROCK_CONFIG_FILE)
  --log-level l   debug, info, warn or error (default info)
  --log-format f  text or json (default text)
`

func runServe(ctx context.Context, args []string) error {
	fs := newFlagSet("serve", usageServe)
	configPath := fs.String("config", defaultServerConfigPath(), "config file")
	logLevel := fs.String("log-level", "info", "log level")
	logFormat := fs.String("log-format", "text", "log format")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	log, err := newLogger(*logLevel, *logFormat)
	if err != nil {
		return err
	}

	cfg, err := server.LoadConfig(*configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no config at %s\n  %s", *configPath,
				dim("create one with: zerock init-config --domain example.com > "+*configPath))
		}
		return err
	}

	if warning := server.ConfigSecretWarning(*configPath); warning != "" {
		log.Warn(warning)
	}

	srv, err := server.New(cfg, log)
	if err != nil {
		return err
	}
	defer srv.Close()

	secret, err := srv.Bootstrap()
	if err != nil {
		return err
	}
	if secret != "" {
		printBootstrap(secret, cfg)
	}

	return srv.Run(ctx)
}

// printBootstrap shows the first admin token. It goes to stderr so a startup
// log captured to a file still records it, and it is deliberately loud: this is
// the only time the secret exists in readable form.
func printBootstrap(secret string, cfg server.Config) {
	line := strings.Repeat("─", 62)
	fmt.Fprintf(os.Stderr, "\n%s\n", dim(line))
	fmt.Fprintf(os.Stderr, "  %s\n\n", bold("First run: admin token created"))
	fmt.Fprintf(os.Stderr, "  %s\n\n", bold(secret))
	fmt.Fprintf(os.Stderr, "  %s\n", amber("Shown once only. Copy it now."))
	fmt.Fprintf(os.Stderr, "  %s\n", dim(fmt.Sprintf("zerock login --server %s --token %s", cfg.APIHost, secret)))
	fmt.Fprintf(os.Stderr, "%s\n\n", dim(line))
}

// newLogger builds the server's structured logger.
func newLogger(level, format string) (*slog.Logger, error) {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "info":
		lv = slog.LevelInfo
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		return nil, fmt.Errorf("unknown log level %q (want debug, info, warn or error)", level)
	}

	opts := &slog.HandlerOptions{Level: lv}
	switch strings.ToLower(format) {
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, opts)), nil
	case "text", "":
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), nil
	default:
		return nil, fmt.Errorf("unknown log format %q (want text or json)", format)
	}
}

// defaultServerConfigPath resolves where the server looks for its config.
func defaultServerConfigPath() string {
	if p := os.Getenv("ZEROCK_CONFIG_FILE"); p != "" {
		return p
	}
	return "/etc/zerock/zerock.yaml"
}

const usageInitConfig = `Print a starter server config to stdout.

Usage:
  zerock init-config --domain example.com [flags] > /etc/zerock/zerock.yaml

Flags:
  --domain name        the domain whose subdomains you will hand out (required)
  --email address      ACME contact address for certificate notices
  --dns-provider name  DNS provider for the wildcard certificate (default cloudflare)
  --data-dir path      where the database and certificates live (default /var/lib/zerock)
  --behind-proxy       emit a config for running behind Caddy, nginx or Traefik
`

func runInitConfig(_ context.Context, args []string) error {
	fs := newFlagSet("init-config", usageInitConfig)
	domain := fs.String("domain", "", "the domain to serve subdomains of")
	email := fs.String("email", "", "ACME contact address")
	dnsProvider := fs.String("dns-provider", "cloudflare", "DNS provider for the wildcard certificate")
	dataDir := fs.String("data-dir", "/var/lib/zerock", "data directory")
	behindProxy := fs.Bool("behind-proxy", false, "emit a config for running behind a reverse proxy")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *domain == "" {
		return errors.New("--domain is required, e.g. --domain example.com")
	}

	if *behindProxy {
		fmt.Printf(proxiedConfigTemplate, version.Version, *domain, *dataDir)
		return nil
	}
	if !slices.Contains(server.SupportedDNSProviders(), *dnsProvider) {
		return fmt.Errorf("--dns-provider %q is not supported; built in: %s\n  %s",
			*dnsProvider, strings.Join(server.SupportedDNSProviders(), ", "),
			dim("for any other provider use tls.mode files with a certificate from elsewhere"))
	}
	fmt.Print(renderServerConfig(*domain, *email, *dataDir, *dnsProvider, false))
	return nil
}

const autoConfigTemplate = `# zerock server config (generated by zerock %s)
#
# DNS, before starting:
#   *.%[2]s   A   <this host's public IP>
#   %[2]s     A   <this host's public IP>
#
# The wildcard certificate is obtained over DNS-01, so port 80 does not need to
# be reachable for issuance or renewal.

domain: %[2]s
data_dir: %[3]s

control_addr: ":7223"     # agents dial in here
https_addr: ":443"        # public tunnel traffic
http_addr: ":80"          # redirects to https
admin_addr: "127.0.0.1:7224"  # local management door; keep it on loopback

tls:
  mode: auto
  email: "%[4]s"
  # Built in: %[6]s. For anything else, use
  # mode: files with a wildcard certificate obtained elsewhere.
  dns_provider: %[5]s
  # Prefer the ZEROCK_DNS_API_TOKEN environment variable over writing the token
  # here, so it stays out of backups.
  # %[7]s
  dns_api_token: ""
  # Add the bare domain to the certificate as well as the wildcard. Off by
  # default: both names validate at the same _acme-challenge record, and a DNS
  # provider that replaces rather than appends breaks one of them, failing the
  # whole order. The wildcard already covers every subdomain, including the
  # api_host, so leave this off unless the apex itself must serve TLS.
  include_apex: false
  # How long to wait for the DNS challenge record to reach every nameserver.
  # A timeout here counts as a failed authorization at the CA, and Let's Encrypt
  # allows only five per hour, so waiting is cheaper than retrying. Raise these
  # if issuance keeps timing out.
  dns_propagation_delay: 30s
  dns_propagation_timeout: 5m
  # Uncomment while testing to avoid Let's Encrypt rate limits:
  # ca: staging

# Subdomains nobody can tunnel. The api_host label is always added.
reserved_subdomains: [www, mail, admin, api, zerock]

# Public ports handed to 'zerock tcp' tunnels. Open these in your firewall.
tcp_port_range:
  from: 20000
  to: 20999

max_reservations_per_token: 20

# Only enable this when a trusted proxy sits in front and sets the header.
trust_proxy_headers: false
`

const proxiedConfigTemplate = `# zerock server config (generated by zerock %s)
#
# For running behind Caddy, nginx or Traefik, which terminates TLS.
#
# Your proxy must send *.%[2]s to http_addr below, and it must
# proxy the control port through as raw TCP (or terminate TLS for it), because
# agents send their token over that connection.
#
# Caddy example:
#   *.%[2]s {
#     reverse_proxy 127.0.0.1:8080
#   }

domain: %[2]s
data_dir: %[3]s

control_addr: ":7223"
http_addr: "127.0.0.1:8080"   # your proxy forwards tunnel traffic here
admin_addr: "127.0.0.1:7224"

tls:
  mode: off

# Your proxy is in front, so forwarded client IPs can be believed.
trust_proxy_headers: true

reserved_subdomains: [www, mail, admin, api, zerock]

tcp_port_range:
  from: 20000
  to: 20999

max_reservations_per_token: 20
`
