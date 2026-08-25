package server

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// TLSMode selects how the server obtains certificates for *.Domain.
type TLSMode string

const (
	// TLSAuto obtains a wildcard certificate from an ACME CA using a DNS-01
	// challenge. This is the only mode that can produce a wildcard cert, so it
	// is the default for a standalone deployment.
	TLSAuto TLSMode = "auto"
	// TLSFiles uses a certificate and key already on disk.
	TLSFiles TLSMode = "files"
	// TLSOff serves plain HTTP, for running behind a proxy that terminates TLS.
	TLSOff TLSMode = "off"
)

// Config is the server's YAML configuration.
type Config struct {
	// Domain is the apex under which tunnels get subdomains, e.g. "example.com"
	// gives "api-x.example.com".
	Domain string `yaml:"domain"`

	// ControlAddr is where agents connect. TLS is applied unless tls.mode is off.
	ControlAddr string `yaml:"control_addr"`
	// HTTPAddr serves ACME challenges and redirects to HTTPS. In tls.mode off it
	// serves tunnel traffic directly.
	HTTPAddr string `yaml:"http_addr"`
	// HTTPSAddr serves tunnel traffic. Ignored when tls.mode is off.
	HTTPSAddr string `yaml:"https_addr"`
	// AdminAddr is an always-on local door to the API. Keep it on loopback.
	AdminAddr string `yaml:"admin_addr"`

	// APIHost is the hostname that serves the management API over HTTPS.
	// Defaults to "zerock.<domain>".
	APIHost string `yaml:"api_host"`

	// DataDir holds the bbolt database and the ACME certificate cache.
	DataDir string `yaml:"data_dir"`

	TLS TLSConfig `yaml:"tls"`

	// ReservedSubdomains can never be tunneled. The API host label is always
	// added to this set.
	ReservedSubdomains []string `yaml:"reserved_subdomains"`

	// TCPPortRange bounds the public ports handed to TCP tunnels.
	TCPPortRange PortRange `yaml:"tcp_port_range"`

	// MaxReservationsPerToken applies when a token does not set its own limit.
	MaxReservationsPerToken int `yaml:"max_reservations_per_token"`

	// TrustProxyHeaders makes the edge believe X-Forwarded-For from its peer.
	// Only enable it when something trusted sits in front.
	TrustProxyHeaders bool `yaml:"trust_proxy_headers"`
}

// TLSConfig configures certificate acquisition.
type TLSConfig struct {
	Mode TLSMode `yaml:"mode"`

	// Email is the ACME account contact. Recommended in auto mode.
	Email string `yaml:"email"`
	// CA overrides the ACME directory URL, e.g. the Let's Encrypt staging
	// endpoint while testing.
	CA string `yaml:"ca"`
	// DNSProvider names the DNS-01 solver. Only "cloudflare" is built in.
	DNSProvider string `yaml:"dns_provider"`
	// DNSAPIToken authenticates against the DNS provider. Prefer the
	// ZEROCK_DNS_API_TOKEN environment variable over writing it here.
	DNSAPIToken string `yaml:"dns_api_token"`

	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`

	// DNSPropagationDelay waits before the first propagation check, giving the
	// provider a chance to distribute the record to all of its nameservers.
	DNSPropagationDelay string `yaml:"dns_propagation_delay"`
	// IncludeApex adds the bare domain to the certificate alongside the
	// wildcard.
	//
	// It is off by default because both names validate at the same DNS record,
	// _acme-challenge.<domain>, with different values. That requires two TXT
	// records to coexist, and a DNS provider that replaces rather than appends
	// silently breaks one of them: the CA then reports an incorrect record and
	// the whole order fails. Requesting only the wildcard avoids the collision.
	// Turn this on only if the apex itself must be served over TLS.
	IncludeApex bool `yaml:"include_apex"`

	// DNSPropagationTimeout bounds how long to wait for the challenge record to
	// appear on every authoritative nameserver.
	//
	// The default is generous on purpose. A challenge that times out counts as a
	// failed authorization at the CA, and Let's Encrypt allows only five per
	// hour per identifier: waiting longer is far cheaper than retrying.
	DNSPropagationTimeout string `yaml:"dns_propagation_timeout"`
}

// Defaults for the DNS-01 propagation wait.
const (
	defaultPropagationDelay   = 30 * time.Second
	defaultPropagationTimeout = 5 * time.Minute
)

// ManagedNames returns the names to obtain a certificate for.
func (c Config) ManagedNames() []string {
	names := []string{"*." + c.Domain}
	if c.TLS.IncludeApex {
		// The apex first, so its authorization is the one already in place if a
		// provider does overwrite.
		names = []string{c.Domain, "*." + c.Domain}
	}
	return names
}

// PropagationDelay returns the configured delay, or the default.
func (c TLSConfig) PropagationDelay() time.Duration {
	if d, err := time.ParseDuration(c.DNSPropagationDelay); err == nil && d > 0 {
		return d
	}
	return defaultPropagationDelay
}

// PropagationTimeout returns the configured timeout, or the default.
func (c TLSConfig) PropagationTimeout() time.Duration {
	if d, err := time.ParseDuration(c.DNSPropagationTimeout); err == nil && d > 0 {
		return d
	}
	return defaultPropagationTimeout
}

// PortRange is an inclusive port span.
type PortRange struct {
	From int `yaml:"from"`
	To   int `yaml:"to"`
}

// DefaultConfig returns the configuration a fresh install starts from.
func DefaultConfig() Config {
	return Config{
		ControlAddr:             ":7223",
		HTTPAddr:                ":80",
		HTTPSAddr:               ":443",
		AdminAddr:               "127.0.0.1:7224",
		DataDir:                 "/var/lib/zerock",
		TLS:                     TLSConfig{Mode: TLSAuto, DNSProvider: "cloudflare"},
		ReservedSubdomains:      []string{"www", "mail", "admin", "api", "zerock"},
		TCPPortRange:            PortRange{From: 20000, To: 20999},
		MaxReservationsPerToken: 20,
	}
}

// LoadConfig reads a YAML config, applies defaults for anything omitted, and
// validates the result.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.applyEnv()
	if err := cfg.normalize(); err != nil {
		return cfg, err
	}
	if err := cfg.validateForServing(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// LoadConfigForInspection reads a config for diagnostics, applying defaults but
// not enforcing what is only needed in order to serve.
//
// A tool reporting on a server must be able to read the config even when the
// server could not start from it: refusing to parse would hide the very problem
// being diagnosed. In particular the DNS credential normally arrives through the
// service's environment file, which is not visible to an interactive command.
func LoadConfigForInspection(path string) (Config, error) {
	cfg := DefaultConfig()
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.applyEnv()
	if err := cfg.normalize(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// applyEnv lets secrets stay out of the config file.
func (c *Config) applyEnv() {
	if v := os.Getenv("ZEROCK_DNS_API_TOKEN"); v != "" {
		c.TLS.DNSAPIToken = v
	}
	if v := os.Getenv("ZEROCK_DOMAIN"); v != "" {
		c.Domain = v
	}
	if v := os.Getenv("ZEROCK_ACME_EMAIL"); v != "" {
		c.TLS.Email = v
	}
}

func (c *Config) normalize() error {
	c.Domain = strings.ToLower(strings.Trim(strings.TrimSpace(c.Domain), "."))
	if c.Domain == "" {
		return fmt.Errorf("config: domain is required")
	}
	if !strings.Contains(c.Domain, ".") {
		return fmt.Errorf("config: domain %q does not look like a registrable domain", c.Domain)
	}
	if c.APIHost == "" {
		c.APIHost = "zerock." + c.Domain
	}
	c.APIHost = strings.ToLower(c.APIHost)

	switch c.TLS.Mode {
	case "":
		c.TLS.Mode = TLSAuto
	case TLSAuto, TLSFiles, TLSOff:
	default:
		return fmt.Errorf("config: unknown tls.mode %q (want auto, files or off)", c.TLS.Mode)
	}

	if c.TCPPortRange.From <= 0 || c.TCPPortRange.To < c.TCPPortRange.From {
		return fmt.Errorf("config: tcp_port_range is invalid")
	}
	if c.DataDir == "" {
		return fmt.Errorf("config: data_dir is required")
	}

	// The API hostname must never be claimable as a tunnel.
	if label, ok := c.subdomainOf(c.APIHost); ok {
		c.ReservedSubdomains = append(c.ReservedSubdomains, label)
	}
	seen := map[string]bool{}
	var reserved []string
	for _, s := range c.ReservedSubdomains {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		reserved = append(reserved, s)
	}
	c.ReservedSubdomains = reserved
	return nil
}

// validateForServing enforces the settings a server needs in order to start,
// separately from the shape checks every reader wants.
func (c *Config) validateForServing() error {
	if c.TLS.Mode == TLSAuto {
		if _, ok := dnsProviders[c.TLS.DNSProvider]; !ok {
			return fmt.Errorf("config: tls.dns_provider %q is not supported; built in: %s (or use tls.mode files)",
				c.TLS.DNSProvider, strings.Join(SupportedDNSProviders(), ", "))
		}
		if c.TLS.DNSAPIToken == "" {
			return fmt.Errorf("config: tls.dns_api_token (or ZEROCK_DNS_API_TOKEN) is required for a wildcard certificate")
		}
	}
	if c.TLS.Mode == TLSFiles && (c.TLS.CertFile == "" || c.TLS.KeyFile == "") {
		return fmt.Errorf("config: tls.cert_file and tls.key_file are required in files mode")
	}
	for name, value := range map[string]string{
		"tls.dns_propagation_delay":   c.TLS.DNSPropagationDelay,
		"tls.dns_propagation_timeout": c.TLS.DNSPropagationTimeout,
	} {
		if value == "" {
			continue
		}
		if _, err := time.ParseDuration(value); err != nil {
			return fmt.Errorf("config: %s must be a duration such as 5m: %w", name, err)
		}
	}
	return nil
}

// ValidateForServing reports whether a config read for inspection would let a
// server start.
func (c Config) ValidateForServing() error { return c.validateForServing() }

// HasDNSCredential reports whether a credential for the DNS challenge is
// available, without exposing it.
func (c Config) HasDNSCredential() bool { return c.TLS.DNSAPIToken != "" }

// subdomainOf extracts the single leading label of host relative to the
// configured domain.
func (c *Config) subdomainOf(host string) (string, bool) {
	host = strings.ToLower(strings.Trim(host, "."))
	suffix := "." + c.Domain
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	label := strings.TrimSuffix(host, suffix)
	if label == "" || strings.Contains(label, ".") {
		return "", false
	}
	return label, true
}

// ConfigSecretWarning reports a config file that holds a credential while being
// readable by other local users, and returns "" when there is nothing to flag.
//
// The service runs unprivileged, so its config has to be readable by more than
// root. That is fine for settings and wrong for secrets, which belong in the
// environment file.
func ConfigSecretWarning(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	// Parse the file on its own rather than inspecting the merged config: a
	// token supplied through the environment never touches this file, and a
	// written-but-empty "dns_api_token: \"\"" is not a secret. Pattern matching
	// the text gets both of those wrong.
	var onDisk Config
	if err := yaml.Unmarshal(raw, &onDisk); err != nil {
		return ""
	}
	if strings.TrimSpace(onDisk.TLS.DNSAPIToken) == "" {
		return ""
	}

	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Sprintf("%s holds tls.dns_api_token and is readable by other local users (mode %04o); "+
			"move the token to ZEROCK_DNS_API_TOKEN instead", path, perm)
	}
	return ""
}

// IsReserved reports whether sub may not be used for a tunnel.
func (c *Config) IsReserved(sub string) bool {
	for _, r := range c.ReservedSubdomains {
		if r == sub {
			return true
		}
	}
	return false
}

// scheme is the URL scheme tunnels are advertised under.
func (c *Config) scheme() string {
	if c.TLS.Mode == TLSOff {
		return "http"
	}
	return "https"
}
