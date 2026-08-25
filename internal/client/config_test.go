package client

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitServer(t *testing.T) {
	cases := []struct {
		in   string
		host string
		port int
	}{
		{"zerock.example.com", "zerock.example.com", 0},
		{"zerock.example.com:9000", "zerock.example.com", 9000},
		{"https://zerock.example.com", "zerock.example.com", 0},
		{"http://zerock.example.com/", "zerock.example.com", 0},
		{"ZEROCK.Example.COM", "zerock.example.com", 0},
		{"127.0.0.1:7223", "127.0.0.1", 7223},
	}
	for _, c := range cases {
		host, port, err := SplitServer(c.in)
		if err != nil {
			t.Errorf("SplitServer(%q): %v", c.in, err)
			continue
		}
		if host != c.host || port != c.port {
			t.Errorf("SplitServer(%q) = %q, %d; want %q, %d", c.in, host, port, c.host, c.port)
		}
	}

	for _, bad := range []string{"", "  ", "host:notaport", "host:0", "host:70000", "host/path"} {
		if _, _, err := SplitServer(bad); err == nil {
			t.Errorf("SplitServer(%q) should have failed", bad)
		}
	}
}

func TestProfileAddresses(t *testing.T) {
	p := Profile{Host: "zerock.example.com"}
	if got, want := p.ControlAddr(), "zerock.example.com:7223"; got != want {
		t.Errorf("ControlAddr = %q, want %q", got, want)
	}
	if got, want := p.APIURL("/api/v1/whoami"), "https://zerock.example.com/api/v1/whoami"; got != want {
		t.Errorf("APIURL = %q, want %q", got, want)
	}

	// A plaintext server is reached over http, not https.
	plain := Profile{Host: "box", Plaintext: true, ControlPort: 9000}
	if got, want := plain.ControlAddr(), "box:9000"; got != want {
		t.Errorf("ControlAddr = %q, want %q", got, want)
	}
	if got, want := plain.APIURL("/healthz"), "http://box/healthz"; got != want {
		t.Errorf("APIURL = %q, want %q", got, want)
	}

	// An explicit base wins, including its port and scheme.
	override := Profile{Host: "box", APIBase: "http://127.0.0.1:7224/"}
	if got, want := override.APIURL("/healthz"), "http://127.0.0.1:7224/healthz"; got != want {
		t.Errorf("APIURL = %q, want %q", got, want)
	}
}

// withConfigPath points the config helpers at a temporary file.
func withConfigPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("ZEROCK_CONFIG", path)
	// Environment overrides would otherwise leak in from the developer's shell.
	t.Setenv("ZEROCK_SERVER", "")
	t.Setenv("ZEROCK_TOKEN", "")
	t.Setenv("ZEROCK_PROFILE", "")
	t.Setenv("ZEROCK_INSECURE", "")
	t.Setenv("ZEROCK_PLAINTEXT", "")
	return path
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	path := withConfigPath(t)

	cfg := &Config{Profiles: map[string]Profile{
		"prod": {Host: "zerock.example.com", Token: "zk_a_b"},
	}, Default: "prod"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The file holds tokens, so it must not be group or world readable.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config permissions = %o, want 600", perm)
	}

	loaded, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	name, prof, err := loaded.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if name != "prod" || prof.Host != "zerock.example.com" || prof.Token != "zk_a_b" {
		t.Errorf("resolved %q, %+v", name, prof)
	}
}

func TestLoadConfigWhenNoFileExists(t *testing.T) {
	withConfigPath(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig with no file: %v", err)
	}
	if _, _, err := cfg.Resolve(""); !errors.Is(err, ErrNoProfile) {
		t.Errorf("got %v, want ErrNoProfile", err)
	}
}

func TestResolveUsesTheOnlyProfile(t *testing.T) {
	withConfigPath(t)
	// With exactly one profile and no default recorded, using it is unambiguous.
	cfg := &Config{Profiles: map[string]Profile{"only": {Host: "h", Token: "zk_a_b"}}}
	name, prof, err := cfg.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if name != "only" || prof.Host != "h" {
		t.Errorf("resolved %q, %+v", name, prof)
	}
}

func TestResolveNamesAMissingProfile(t *testing.T) {
	withConfigPath(t)
	cfg := &Config{Profiles: map[string]Profile{"prod": {Host: "h", Token: "t"}}}
	_, _, err := cfg.Resolve("staging")
	if !errors.Is(err, ErrNoProfile) {
		t.Fatalf("got %v, want ErrNoProfile", err)
	}
	// The message should list what does exist, so the fix is obvious.
	if got := err.Error(); !strings.Contains(got, "staging") || !strings.Contains(got, "prod") {
		t.Errorf("error %q should name both the missing and the available profiles", got)
	}
}

func TestEnvironmentOverridesConfig(t *testing.T) {
	withConfigPath(t)
	cfg := &Config{Profiles: map[string]Profile{"prod": {Host: "old", Token: "old"}}, Default: "prod"}

	t.Setenv("ZEROCK_SERVER", "new.example.com:9999")
	t.Setenv("ZEROCK_TOKEN", "zk_new_secret")
	t.Setenv("ZEROCK_PLAINTEXT", "1")

	_, prof, err := cfg.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if prof.Host != "new.example.com" || prof.ControlPort != 9999 {
		t.Errorf("ZEROCK_SERVER did not override: %+v", prof)
	}
	if prof.Token != "zk_new_secret" {
		t.Errorf("ZEROCK_TOKEN did not override: %q", prof.Token)
	}
	if !prof.Plaintext {
		t.Error("ZEROCK_PLAINTEXT did not take effect")
	}
}

func TestEnvironmentAloneIsEnough(t *testing.T) {
	// This is what makes the agent usable in CI with no config file.
	withConfigPath(t)
	t.Setenv("ZEROCK_SERVER", "ci.example.com")
	t.Setenv("ZEROCK_TOKEN", "zk_ci_secret")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	_, prof, err := cfg.Resolve("")
	if err != nil {
		t.Fatalf("Resolve with only environment variables: %v", err)
	}
	if prof.Host != "ci.example.com" || prof.Token != "zk_ci_secret" {
		t.Errorf("resolved %+v", prof)
	}
}

func TestNamesAreSorted(t *testing.T) {
	cfg := &Config{Profiles: map[string]Profile{"c": {}, "a": {}, "b": {}}}
	got := cfg.Names()
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("Names = %v, want [a b c]", got)
	}
}
