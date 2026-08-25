package server

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// writeConfig writes a config file and loads it.
func loadFrom(t *testing.T, body string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "zerock.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(path)
}

func TestLoadConfigAppliesDefaults(t *testing.T) {
	// "mode: off" is the shape init-config emits; YAML must not turn it into a
	// boolean on the way in.
	cfg, err := loadFrom(t, "domain: example.com\ntls:\n  mode: off\n")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.TLS.Mode != TLSOff {
		t.Errorf("tls.mode = %q, want off", cfg.TLS.Mode)
	}
	if cfg.ControlAddr != ":7223" {
		t.Errorf("control_addr = %q, want the default :7223", cfg.ControlAddr)
	}
	if cfg.APIHost != "zerock.example.com" {
		t.Errorf("api_host = %q, want zerock.example.com", cfg.APIHost)
	}
	if cfg.scheme() != "http" {
		t.Errorf("scheme = %q, want http in off mode", cfg.scheme())
	}
	// The API host must never be claimable as a tunnel.
	if !cfg.IsReserved("zerock") {
		t.Error("the api_host label should be reserved automatically")
	}
}

func TestLoadConfigRequiresDomain(t *testing.T) {
	if _, err := loadFrom(t, "tls:\n  mode: off\n"); err == nil {
		t.Fatal("expected an error when domain is missing")
	}
	if _, err := loadFrom(t, "domain: localhost\ntls:\n  mode: off\n"); err == nil {
		t.Fatal("expected an error for a domain with no dot")
	}
}

func TestAutoTLSNeedsADNSToken(t *testing.T) {
	// Without a DNS token, auto mode cannot get a wildcard, so it must fail at
	// startup rather than serve without certificates.
	_, err := loadFrom(t, "domain: example.com\ntls:\n  mode: auto\n  dns_provider: cloudflare\n")
	if err == nil || !strings.Contains(err.Error(), "dns_api_token") {
		t.Fatalf("got %v, want a complaint about dns_api_token", err)
	}

	t.Setenv("ZEROCK_DNS_API_TOKEN", "from-the-environment")
	cfg, err := loadFrom(t, "domain: example.com\ntls:\n  mode: auto\n  dns_provider: cloudflare\n")
	if err != nil {
		t.Fatalf("with the env var set: %v", err)
	}
	if cfg.TLS.DNSAPIToken != "from-the-environment" {
		t.Error("ZEROCK_DNS_API_TOKEN should supply the token so it stays out of the file")
	}
	if cfg.scheme() != "https" {
		t.Errorf("scheme = %q, want https", cfg.scheme())
	}
}

func TestSupportedDNSProviders(t *testing.T) {
	got := SupportedDNSProviders()
	for _, want := range []string{"cloudflare", "digitalocean"} {
		if !slices.Contains(got, want) {
			t.Errorf("%q is not in the supported list %v", want, got)
		}
	}
	// The list is used in error messages, so its order must be stable.
	if !slices.IsSorted(got) {
		t.Errorf("provider list is not sorted: %v", got)
	}
}

func TestEachProviderBuildsASolver(t *testing.T) {
	for _, name := range SupportedDNSProviders() {
		solver, err := dnsProvider(name, "a-token")
		if err != nil {
			t.Errorf("dnsProvider(%q): %v", name, err)
			continue
		}
		if solver == nil {
			t.Errorf("dnsProvider(%q) returned no solver", name)
		}
		// A provider with no credential cannot solve anything.
		if _, err := dnsProvider(name, ""); err == nil {
			t.Errorf("dnsProvider(%q) accepted an empty token", name)
		}
	}
	if _, err := dnsProvider("route53", "t"); err == nil {
		t.Error("an unregistered provider should be rejected")
	}
}

func TestDigitalOceanConfigLoads(t *testing.T) {
	cfg, err := loadFrom(t, "domain: example.com\ntls:\n  mode: auto\n  dns_provider: digitalocean\n  dns_api_token: dop_v1_x\n")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.TLS.DNSProvider != "digitalocean" {
		t.Errorf("dns_provider = %q", cfg.TLS.DNSProvider)
	}
}

func TestUnsupportedTLSSettings(t *testing.T) {
	if _, err := loadFrom(t, "domain: example.com\ntls:\n  mode: sideways\n"); err == nil {
		t.Error("expected an error for an unknown tls.mode")
	}
	if _, err := loadFrom(t, "domain: example.com\ntls:\n  mode: auto\n  dns_provider: route53\n  dns_api_token: x\n"); err == nil {
		t.Error("expected an error for an unsupported dns_provider")
	}
	if _, err := loadFrom(t, "domain: example.com\ntls:\n  mode: files\n  cert_file: /c.pem\n"); err == nil {
		t.Error("expected an error when key_file is missing")
	}
}

func TestPortRangeValidation(t *testing.T) {
	if _, err := loadFrom(t, "domain: example.com\ntls:\n  mode: off\ntcp_port_range:\n  from: 500\n  to: 100\n"); err == nil {
		t.Error("expected an error for an inverted port range")
	}
}

func TestSubdomainOf(t *testing.T) {
	cfg := Config{Domain: "example.com"}
	cases := map[string]struct {
		label string
		ok    bool
	}{
		"api-x.example.com":       {"api-x", true},
		"API-X.example.com":       {"api-x", true},
		"api-x.example.com.":      {"api-x", true},
		"example.com":             {"", false}, // the apex is not a subdomain
		"deep.api-x.example.com":  {"", false}, // only one label is supported
		"api-x.notexample.com":    {"", false},
		"api-x.example.com.evil.": {"", false},
	}
	for host, want := range cases {
		label, ok := cfg.subdomainOf(host)
		if ok != want.ok || label != want.label {
			t.Errorf("subdomainOf(%q) = %q, %v; want %q, %v", host, label, ok, want.label, want.ok)
		}
	}
}

func TestReservedSubdomainsAreDeduplicated(t *testing.T) {
	cfg, err := loadFrom(t, "domain: example.com\ntls:\n  mode: off\nreserved_subdomains: [www, WWW, www, '']\n")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, r := range cfg.ReservedSubdomains {
		if r == "www" {
			count++
		}
		if r == "" {
			t.Error("an empty reserved subdomain should be dropped")
		}
	}
	if count != 1 {
		t.Errorf("www appears %d times, want 1", count)
	}
}

func TestConfigSecretWarning(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		// WriteFile is subject to umask, so set the mode explicitly.
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}

	withToken := "domain: example.com\ntls:\n  mode: auto\n  dns_provider: cloudflare\n  dns_api_token: secret-value\n"
	withoutToken := "domain: example.com\ntls:\n  mode: auto\n  dns_provider: cloudflare\n  dns_api_token: \"\"\n"

	// A token inline in a config other local users can read is worth flagging.
	exposed := write("exposed.yaml", withToken, 0o644)
	if got := ConfigSecretWarning(exposed); got == "" {
		t.Error("a readable config holding a DNS token should produce a warning")
	}

	// The same content locked down is fine.
	locked := write("locked.yaml", withToken, 0o600)
	if got := ConfigSecretWarning(locked); got != "" {
		t.Errorf("a 0600 config should not warn, got %q", got)
	}

	// A token supplied through the environment never touches the file, so a
	// readable config is not a problem. This is the shape install produces.
	t.Setenv("ZEROCK_DNS_API_TOKEN", "from-the-environment")
	envOnly := write("env-only.yaml", withoutToken, 0o644)
	cfg, err := LoadConfig(envOnly)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLS.DNSAPIToken == "" {
		t.Fatal("the environment token was not picked up")
	}
	if got := ConfigSecretWarning(envOnly); got != "" {
		t.Errorf("a token from the environment should not warn about the file, got %q", got)
	}

	// An empty value written in the file is not a secret either.
	empty := write("empty.yaml", "domain: example.com\ntls:\n  mode: off\n  dns_api_token: \"\"\n", 0o644)
	if got := ConfigSecretWarning(empty); got != "" {
		t.Errorf("an empty dns_api_token should not warn, got %q", got)
	}
}

func TestPropagationDefaultsAreGenerous(t *testing.T) {
	// certmagic's two-minute default timed out four times against DigitalOcean
	// before succeeding, and each timeout is a failed authorization against a
	// five-per-hour CA limit.
	var unset TLSConfig
	if got := unset.PropagationTimeout(); got < 4*time.Minute {
		t.Errorf("default propagation timeout is %s; too short to survive a slow provider", got)
	}
	if got := unset.PropagationDelay(); got < 10*time.Second {
		t.Errorf("default propagation delay is %s; checking immediately wastes an attempt", got)
	}
}

func TestPropagationOverrides(t *testing.T) {
	cfg, err := loadFrom(t, "domain: example.com\ntls:\n  mode: off\n  dns_propagation_delay: 45s\n  dns_propagation_timeout: 10m\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.TLS.PropagationDelay(); got != 45*time.Second {
		t.Errorf("delay = %s, want 45s", got)
	}
	if got := cfg.TLS.PropagationTimeout(); got != 10*time.Minute {
		t.Errorf("timeout = %s, want 10m", got)
	}
}

func TestPropagationRejectsNonsense(t *testing.T) {
	// Silently falling back to the default would hide a typo in a setting whose
	// whole purpose is to prevent an expensive failure.
	if _, err := loadFrom(t, "domain: example.com\ntls:\n  mode: off\n  dns_propagation_timeout: soon\n"); err == nil {
		t.Error("an unparseable duration should be rejected")
	}
}

func TestGeneratedConfigCarriesPropagationSettings(t *testing.T) {
	// The knob only helps if an operator can find it.
	cfg, err := loadFrom(t, "domain: example.com\ntls:\n  mode: auto\n  dns_provider: cloudflare\n  dns_api_token: x\n  dns_propagation_timeout: 5m\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLS.PropagationTimeout() != 5*time.Minute {
		t.Errorf("timeout = %s, want 5m", cfg.TLS.PropagationTimeout())
	}
}

func TestManagedNamesExcludesApexByDefault(t *testing.T) {
	// The apex and the wildcard both validate at _acme-challenge.<domain>. A
	// provider that replaces rather than appends breaks one of them, and the CA
	// then rejects the order with "incorrect TXT record". Asking for only the
	// wildcard removes the collision.
	cfg, err := loadFrom(t, "domain: zerock.name\ntls:\n  mode: off\n")
	if err != nil {
		t.Fatal(err)
	}
	names := cfg.ManagedNames()
	if len(names) != 1 || names[0] != "*.zerock.name" {
		t.Fatalf("ManagedNames = %v, want just the wildcard", names)
	}
	// The API host must still be covered, or the dashboard breaks.
	if !strings.HasSuffix(cfg.APIHost, ".zerock.name") {
		t.Errorf("api_host %q is not under the wildcard", cfg.APIHost)
	}
}

func TestManagedNamesCanIncludeApex(t *testing.T) {
	cfg, err := loadFrom(t, "domain: zerock.name\ntls:\n  mode: off\n  include_apex: true\n")
	if err != nil {
		t.Fatal(err)
	}
	names := cfg.ManagedNames()
	if len(names) != 2 || names[0] != "zerock.name" || names[1] != "*.zerock.name" {
		t.Fatalf("ManagedNames = %v, want the apex then the wildcard", names)
	}
}
