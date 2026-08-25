package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartBlockerCatchesEmptyQuotedToken(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "zerock.yaml")
	env := filepath.Join(dir, "zerock.env")
	writeTest(t, config, "domain: example.com\ntls:\n  mode: auto\n  dns_provider: cloudflare\n  dns_api_token: \"\"\n")
	writeTest(t, env, "# nothing set\nZEROCK_DNS_API_TOKEN=\n")

	t.Setenv("ZEROCK_DNS_API_TOKEN", "")
	if err := startBlocker(config, env); err == nil {
		t.Fatal(`an empty dns_api_token: "" must block the start; a regex read it as a value and let a doomed service start`)
	}
}

func TestStartBlockerAcceptsATokenFromTheEnvFile(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "zerock.yaml")
	env := filepath.Join(dir, "zerock.env")
	writeTest(t, config, "domain: example.com\ntls:\n  mode: auto\n  dns_provider: cloudflare\n  dns_api_token: \"\"\n")
	writeTest(t, env, "ZEROCK_DNS_API_TOKEN=dop_v1_real\n")

	t.Setenv("ZEROCK_DNS_API_TOKEN", "")
	if err := startBlocker(config, env); err != nil {
		t.Fatalf("a token in the environment file should be enough: %v", err)
	}
}

func TestStartBlockerIgnoresCredentialsInOffMode(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "zerock.yaml")
	writeTest(t, config, "domain: example.com\ntls:\n  mode: off\n")
	t.Setenv("ZEROCK_DNS_API_TOKEN", "")
	if err := startBlocker(config, filepath.Join(dir, "absent.env")); err != nil {
		t.Fatalf("off mode needs no credential: %v", err)
	}
}

// writeTest writes a fixture file.
func writeTest(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStartBlockerAgainstTheRealGeneratedConfig(t *testing.T) {
	// The predecessor of this check passed its tests while failing in
	// production, because the fixtures were hand-written configs that omitted
	// the dns_api_token line entirely. The config init-config actually writes
	// contains `dns_api_token: ""`, which a regex read as a value. Generating
	// the fixture from the real template is what closes that gap.
	dir := t.TempDir()
	config := filepath.Join(dir, "zerock.yaml")
	env := filepath.Join(dir, "zerock.env")

	writeTest(t, config, renderServerConfig("novaminds.xyz", "me@example.com", "/var/lib/zerock", "digitalocean", false))
	writeTest(t, env, dnsEnvBody("digitalocean"))
	t.Setenv("ZEROCK_DNS_API_TOKEN", "")

	if !strings.Contains(readTest(t, config), `dns_api_token: ""`) {
		t.Fatal("the generated config no longer has an empty quoted token; update this test")
	}
	if err := startBlocker(config, env); err == nil {
		t.Fatal("a freshly generated config has no credential yet, so starting must be blocked")
	}

	// Filling in the credential file is the documented next step, and must unblock it.
	writeTest(t, env, "ZEROCK_DNS_API_TOKEN=dop_v1_real\n")
	t.Setenv("ZEROCK_DNS_API_TOKEN", "")
	if err := startBlocker(config, env); err != nil {
		t.Fatalf("after setting the token in %s the service should start: %v", env, err)
	}
}

func TestStartBlockerAgainstTheBehindProxyConfig(t *testing.T) {
	// A proxied install needs no credential and must never be blocked.
	dir := t.TempDir()
	config := filepath.Join(dir, "zerock.yaml")
	writeTest(t, config, renderServerConfig("novaminds.xyz", "", "/var/lib/zerock", "digitalocean", true))
	t.Setenv("ZEROCK_DNS_API_TOKEN", "")
	if err := startBlocker(config, filepath.Join(dir, "absent.env")); err != nil {
		t.Fatalf("behind-proxy mode should start with no credential: %v", err)
	}
}

// readTest reads a fixture back.
func readTest(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
