// Package client implements the zerock agent and the CLI's API calls.
package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// DefaultControlPort is where a zerock server listens for agents.
const DefaultControlPort = 7223

// Profile is one server the CLI knows how to talk to. Several profiles let one
// machine drive tunnels across several domains.
type Profile struct {
	// Host is the server's hostname, e.g. "zerock.example.com". It serves the
	// management API over HTTPS and the control plane on ControlPort.
	Host string `json:"host"`
	// ControlPort defaults to DefaultControlPort when zero.
	ControlPort int `json:"control_port,omitempty"`
	// Token is the credential presented to both the control plane and the API.
	Token string `json:"token"`
	// APIBase overrides the derived API URL, for setups where the API sits
	// somewhere other than https://Host.
	APIBase string `json:"api_base,omitempty"`
	// Insecure skips TLS verification. Only for a server using a self-signed
	// certificate you have chosen to trust.
	Insecure bool `json:"insecure,omitempty"`
	// Plaintext dials the control plane without TLS, for a server running in
	// tls.mode off on a network you already trust. The token crosses the wire
	// in the clear, so this is opt-in and never a fallback.
	Plaintext bool `json:"plaintext,omitempty"`
}

// ControlAddr is the address the agent dials.
func (p Profile) ControlAddr() string {
	port := p.ControlPort
	if port == 0 {
		port = DefaultControlPort
	}
	return net.JoinHostPort(p.Host, strconv.Itoa(port))
}

// APIURL renders an absolute API URL for a path such as "/api/v1/tunnels".
func (p Profile) APIURL(path string) string {
	base := p.APIBase
	if base == "" {
		scheme := "https://"
		if p.Plaintext {
			scheme = "http://"
		}
		base = scheme + p.Host
	}
	return strings.TrimRight(base, "/") + path
}

// SavedTunnel is a tunnel definition kept in the config so it can be started
// by name, from the tray widget or a later CLI verb, instead of retyping the
// http/tcp flags each time.
type SavedTunnel struct {
	// Type is "http" or "tcp".
	Type string `json:"type"`
	// Port is the local port to forward to.
	Port int `json:"port"`
	// Host is the local host to forward to; 127.0.0.1 when empty.
	Host string `json:"host,omitempty"`
	// Subdomain to request; random when empty.
	Subdomain string `json:"sub,omitempty"`
	// RemotePort requests a specific public port for a TCP tunnel.
	RemotePort int `json:"remote_port,omitempty"`
	// BasicAuth is "user:pass" enforced at the edge (HTTP only).
	BasicAuth string `json:"auth,omitempty"`
	// Profile names the server to use; the default profile when empty.
	Profile string `json:"profile,omitempty"`
	// Autostart opens the tunnel as soon as the tray widget launches.
	Autostart bool `json:"autostart,omitempty"`
}

// LocalAddr is the address the tunnel forwards to.
func (t SavedTunnel) LocalAddr() string {
	host := t.Host
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(t.Port))
}

// Args renders the tunnel as the CLI arguments that would open it, e.g.
// "http 3000 --sub api-x", which is also how the tray widget asks for one.
func (t SavedTunnel) Args() string {
	parts := []string{t.Type, strconv.Itoa(t.Port)}
	if t.Subdomain != "" {
		parts = append(parts, "--sub", t.Subdomain)
	}
	if t.Host != "" && t.Host != "127.0.0.1" {
		parts = append(parts, "--host", t.Host)
	}
	if t.RemotePort != 0 {
		parts = append(parts, "--remote-port", strconv.Itoa(t.RemotePort))
	}
	if t.BasicAuth != "" {
		parts = append(parts, "--auth", t.BasicAuth)
	}
	return strings.Join(parts, " ")
}

// Config is the on-disk CLI configuration.
type Config struct {
	Default  string             `json:"default"`
	Profiles map[string]Profile `json:"profiles"`
	// Tunnels are saved tunnel definitions, keyed by a short name.
	Tunnels map[string]SavedTunnel `json:"tunnels,omitempty"`
}

// ErrNoProfile means the CLI has nothing to connect to yet.
var ErrNoProfile = errors.New("no zerock server configured")

// ConfigPath returns the config file location, honouring ZEROCK_CONFIG and
// XDG_CONFIG_HOME.
func ConfigPath() string {
	if p := os.Getenv("ZEROCK_CONFIG"); p != "" {
		return p
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "zerock", "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".zerock", "config.json")
	}
	return filepath.Join(home, ".config", "zerock", "config.json")
}

// LoadConfig reads the config file, returning an empty config when none exists.
func LoadConfig() (*Config, error) {
	path := ConfigPath()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{Profiles: map[string]Profile{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]Profile{}
	}
	if cfg.Tunnels == nil {
		cfg.Tunnels = map[string]SavedTunnel{}
	}
	return &cfg, nil
}

// Save writes the config with owner-only permissions, since it holds tokens.
func (c *Config) Save() error {
	path := ConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	// Write then rename so an interrupted save cannot truncate a good config.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Names lists configured profile names in sorted order.
func (c *Config) Names() []string {
	out := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TunnelNames lists saved tunnel names in sorted order.
func (c *Config) TunnelNames() []string {
	out := make([]string, 0, len(c.Tunnels))
	for name := range c.Tunnels {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Resolve picks a profile and applies environment overrides. An empty name uses
// ZEROCK_PROFILE, then the configured default, then the sole profile if there is
// exactly one.
//
// ZEROCK_SERVER and ZEROCK_TOKEN alone are enough to work with no config file at
// all, which is what makes the agent usable from CI.
func (c *Config) Resolve(name string) (string, Profile, error) {
	if name == "" {
		name = os.Getenv("ZEROCK_PROFILE")
	}
	if name == "" {
		name = c.Default
	}
	if name == "" && len(c.Profiles) == 1 {
		name = c.Names()[0]
	}

	prof := c.Profiles[name]

	if server := os.Getenv("ZEROCK_SERVER"); server != "" {
		host, port, err := SplitServer(server)
		if err != nil {
			return name, prof, err
		}
		prof.Host, prof.ControlPort = host, port
	}
	if token := os.Getenv("ZEROCK_TOKEN"); token != "" {
		prof.Token = token
	}
	if os.Getenv("ZEROCK_INSECURE") == "1" {
		prof.Insecure = true
	}
	if os.Getenv("ZEROCK_PLAINTEXT") == "1" {
		prof.Plaintext = true
	}

	if prof.Host == "" || prof.Token == "" {
		if name != "" && len(c.Profiles) > 0 && prof.Host == "" {
			return name, prof, fmt.Errorf("%w: no profile named %q (have: %s)",
				ErrNoProfile, name, strings.Join(c.Names(), ", "))
		}
		return name, prof, fmt.Errorf("%w: run 'zerock login --server <host> --token <zk_...>' first", ErrNoProfile)
	}
	if name == "" {
		name = prof.Host
	}
	return name, prof, nil
}

// SplitServer parses "host", "host:port" or "https://host" into its parts.
func SplitServer(server string) (host string, port int, err error) {
	server = strings.TrimSpace(server)
	server = strings.TrimPrefix(strings.TrimPrefix(server, "https://"), "http://")
	server = strings.Trim(strings.TrimSuffix(server, "/"), ".")
	if server == "" {
		return "", 0, errors.New("server is empty")
	}
	if h, p, splitErr := net.SplitHostPort(server); splitErr == nil {
		parsed, convErr := strconv.Atoi(p)
		if convErr != nil || parsed < 1 || parsed > 65535 {
			return "", 0, fmt.Errorf("invalid port in %q", server)
		}
		return strings.ToLower(h), parsed, nil
	}
	if strings.Contains(server, "/") {
		return "", 0, fmt.Errorf("expected a host, got %q", server)
	}
	return strings.ToLower(server), 0, nil
}
