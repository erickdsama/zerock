package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erickdsama/zerock/internal/namegen"
)

// The units are embedded in the binary rather than shipped as files, so they
// cannot be reviewed on disk. These tests render them and, where systemd is
// available, have systemd itself confirm they parse.

func TestServerUnitContent(t *testing.T) {
	unit := serverUnit("/usr/local/bin/zerock")

	for _, want := range []string{
		"ExecStart=/usr/local/bin/zerock serve --config /etc/zerock/zerock.yaml",
		"Restart=always",                           // survives a crash
		"WantedBy=multi-user.target",               // survives a reboot
		"StateDirectory=zerock",                    // /var/lib/zerock, matching the default data_dir
		"AmbientCapabilities=CAP_NET_BIND_SERVICE", // can still bind 80 and 443
		"DynamicUser=yes",
		"EnvironmentFile=-/etc/zerock/zerock.env",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the server unit is missing %q", want)
		}
	}

	// A literal %i in the server unit would be a copy-paste slip from the
	// templated one, and systemd would reject it in a non-template unit.
	if strings.Contains(unit, "%i") {
		t.Error("the server unit contains an instance specifier, which only belongs in a template unit")
	}
}

func TestTunnelUnitContent(t *testing.T) {
	unit := tunnelUnit("/usr/local/bin/zerock")

	for _, want := range []string{
		"ExecStart=/usr/local/bin/zerock $ZEROCK_ARGS",
		"EnvironmentFile=/etc/zerock/tunnels/%i.env",
		"Description=zerock tunnel (%i)",
		"Restart=always",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the tunnel unit is missing %q", want)
		}
	}

	// The Go template escapes these as %%i; a single %i surviving means the
	// escaping is right, and a stray %%i means it is not.
	if strings.Contains(unit, "%%i") {
		t.Error("the tunnel unit still contains %%i; the instance specifier was not unescaped")
	}

	// The token must never reach ExecStart, where ps would show it.
	if strings.Contains(unit, "ZEROCK_TOKEN") {
		t.Error("the tunnel unit references the token directly; it belongs in the environment file")
	}
}

func TestUnitsPassSystemdVerify(t *testing.T) {
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze not available")
	}

	// systemd-analyze checks that ExecStart exists, so point it at this test
	// binary, which certainly does.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	cases := map[string]string{
		"zerock.service":         serverUnit(self),
		"zerock-tunnel@.service": tunnelUnit(self),
	}
	for name, body := range cases {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for name := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := exec.Command(analyze, "verify", filepath.Join(dir, name)).CombinedOutput()
			report := strings.TrimSpace(string(out))

			// A missing EnvironmentFile is expected: the template unit points at
			// a per-instance file that only exists once a tunnel is added.
			var problems []string
			for _, line := range strings.Split(report, "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.Contains(line, "zerock.env") || strings.Contains(line, "tunnels/") {
					continue
				}
				problems = append(problems, line)
			}
			if len(problems) > 0 {
				t.Errorf("systemd rejected %s:\n%s", name, strings.Join(problems, "\n"))
			}
			// verify exits non-zero for real problems; the filtered output above
			// is the authority on whether any were ours.
			if err != nil && len(problems) > 0 {
				t.Errorf("systemd-analyze verify: %v", err)
			}
		})
	}
}

func TestRenderServerConfigModes(t *testing.T) {
	auto := renderServerConfig("example.com", "me@example.com", "/var/lib/zerock", "cloudflare", false)
	if !strings.Contains(auto, "mode: auto") {
		t.Error("the default config should request automatic certificates")
	}
	if !strings.Contains(auto, "domain: example.com") {
		t.Error("the domain was not substituted")
	}
	if !strings.Contains(auto, "me@example.com") {
		t.Error("the ACME email was not substituted")
	}

	proxied := renderServerConfig("example.com", "", "/var/lib/zerock", "cloudflare", true)
	if !strings.Contains(proxied, "mode: off") {
		t.Error("the behind-proxy config should not terminate TLS itself")
	}
	if !strings.Contains(proxied, "trust_proxy_headers: true") {
		t.Error("behind a proxy, forwarded client IPs should be trusted")
	}
}

func TestRenderServerConfigCarriesTheChosenProvider(t *testing.T) {
	// A config naming the wrong provider is the failure this guards: the server
	// would try Cloudflare's API with a DigitalOcean token.
	do := renderServerConfig("novaminds.xyz", "", "/var/lib/zerock", "digitalocean", false)
	if !strings.Contains(do, "dns_provider: digitalocean") {
		t.Error("the chosen provider was not written into the config")
	}
	if strings.Contains(do, "dns_provider: cloudflare") {
		t.Error("the config still names cloudflare")
	}
	// The generated config must be loadable by the server that reads it.
	if !strings.Contains(do, "domain: novaminds.xyz") {
		t.Error("the domain was not substituted")
	}

	cf := renderServerConfig("example.com", "", "/var/lib/zerock", "cloudflare", false)
	if !strings.Contains(cf, "dns_provider: cloudflare") {
		t.Error("cloudflare was not written into the config")
	}
}

func TestDNSTokenHintIsProviderSpecific(t *testing.T) {
	// The wrong token scope is the most common reason issuance fails, so the
	// hint has to name the right one.
	if !strings.Contains(dnsTokenHint("cloudflare"), "DNS:Edit") {
		t.Error("the Cloudflare hint should name the required permissions")
	}
	if !strings.Contains(dnsTokenHint("digitalocean"), "write") {
		t.Error("the DigitalOcean hint should mention write scope")
	}
	if dnsTokenHint("something-else") == "" {
		t.Error("an unknown provider should still get a usable hint")
	}
}

func TestDNSEnvBodyNamesTheProvider(t *testing.T) {
	body := dnsEnvBody("digitalocean")
	if !strings.Contains(body, "ZEROCK_DNS_API_TOKEN=") {
		t.Error("the credential file must define the variable the unit reads")
	}
	if !strings.Contains(body, "DigitalOcean") {
		t.Error("the credential file should say which provider's token to paste")
	}
}

func TestValidInstanceName(t *testing.T) {
	for _, ok := range []string{"api-x", "api", "web.1", "a", "A1_b-c.d"} {
		if !validInstanceName(ok) {
			t.Errorf("validInstanceName(%q) = false, want true", ok)
		}
	}
	// Names systemd would need escaped, or that would escape the directory.
	for _, bad := range []string{"", "-leading", "with space", "with/slash", "../escape", "with@at"} {
		if validInstanceName(bad) {
			t.Errorf("validInstanceName(%q) = true, want false", bad)
		}
	}
}

// The install path writes to /etc and /usr/local, which a test cannot touch.
// The filesystem behaviour it relies on is exercised here against temp paths
// instead, since that is where the risk actually lives.

func TestPlanWriteFileIfAbsentKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zerock.yaml")
	if err := os.WriteFile(path, []byte("domain: mine.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := &plan{}
	p.writeFileIfAbsent("server config", path, "domain: OVERWRITTEN\n", 0o600)
	if err := p.run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	// This is what makes reinstalling safe as an upgrade.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "domain: mine.example.com\n" {
		t.Errorf("an existing config was overwritten: %q", body)
	}
}

func TestPlanWriteFileIfAbsentCreatesWhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.yaml")
	p := &plan{}
	p.writeFileIfAbsent("server config", path, "domain: example.com\n", 0o600)
	if err := p.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the file was not created: %v", err)
	}
	if string(body) != "domain: example.com\n" {
		t.Errorf("content = %q", body)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The config and credential files hold secrets.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %o, want 600", perm)
	}
}

func TestPlanWriteFileAlwaysReplaces(t *testing.T) {
	// The unit is regenerated every install, so it must overwrite.
	path := filepath.Join(t.TempDir(), "zerock.service")
	if err := os.WriteFile(path, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &plan{}
	p.writeFile("systemd unit", path, "fresh\n", 0o644)
	if err := p.run(); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "fresh\n" {
		t.Errorf("content = %q, want the regenerated unit", body)
	}
}

func TestPlanDryRunChangesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zerock.yaml")

	p := &plan{dryRun: true}
	p.mkdir(filepath.Join(dir, "sub"), 0o755)
	p.writeFile("server config", path, "domain: example.com\n", 0o600)
	if err := p.run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	if fileExists(path) {
		t.Error("a dry run created a file")
	}
	if fileExists(filepath.Join(dir, "sub")) {
		t.Error("a dry run created a directory")
	}
}

func TestPlanCopyFileSkipsWhenAlreadyInPlace(t *testing.T) {
	// Reinstalling from /usr/local/bin/zerock must not try to copy the running
	// binary onto itself.
	dir := t.TempDir()
	path := filepath.Join(dir, "zerock")
	if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &plan{}
	p.copyFile("install this binary", path, path, 0o755)
	if len(p.steps) != 1 || p.steps[0].note != "already in place" {
		t.Fatalf("steps = %+v, want one no-op step", p.steps)
	}
	if err := p.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "binary" {
		t.Errorf("the binary was disturbed: %q", body)
	}
}

func TestPlanCopyFileInstallsBinary(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "src", "zerock")
	to := filepath.Join(dir, "dst", "bin", "zerock")
	if err := os.MkdirAll(filepath.Dir(from), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(from, []byte("ELF"), 0o755); err != nil {
		t.Fatal(err)
	}

	p := &plan{}
	p.copyFile("install this binary", from, to, 0o755)
	if err := p.run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	body, err := os.ReadFile(to)
	if err != nil {
		t.Fatalf("the binary was not installed: %v", err)
	}
	if string(body) != "ELF" {
		t.Errorf("content = %q", body)
	}
	info, err := os.Stat(to)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("the installed binary is not executable")
	}
}

func TestPlanRemoveIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "zerock.service")
	if err := os.WriteFile(present, []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(dir, "not-there")

	p := &plan{}
	p.remove("systemd unit", present)
	p.remove("systemd unit", absent)
	if err := p.run(); err != nil {
		t.Fatalf("uninstalling something absent should not fail: %v", err)
	}
	if fileExists(present) {
		t.Error("the unit was not removed")
	}
}

func TestWriteFileAtomicLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := writeFileAtomic(path, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want just the target file", names)
	}
}

func TestInstalledFileModes(t *testing.T) {
	// This is the invariant that broke a real install: the unit runs under
	// DynamicUser, so the process reading the config is neither root nor the
	// file's owner. A config it cannot read makes the service exit immediately.
	if configFileMode&0o044 == 0 {
		t.Errorf("configFileMode = %04o; a DynamicUser service could not read its own config", configFileMode)
	}
	// The credential file is different: systemd reads it as root before dropping
	// privileges, so it must not be exposed to anyone else.
	if secretFileMode&0o077 != 0 {
		t.Errorf("secretFileMode = %04o; the DNS token would be group or world readable", secretFileMode)
	}
	// The two must not be confused for one another.
	if configFileMode == secretFileMode {
		t.Error("the config and the credential file should not share a mode")
	}
}

func TestServerUnitJustifiesTheConfigMode(t *testing.T) {
	// If the unit ever stops using DynamicUser, the world-readable config is no
	// longer necessary and should be tightened back to 0600.
	unit := serverUnit("/usr/local/bin/zerock")
	if !strings.Contains(unit, "DynamicUser=yes") {
		t.Skip("the unit no longer uses DynamicUser; revisit configFileMode")
	}
	if !strings.Contains(unit, "EnvironmentFile=-/etc/zerock/zerock.env") {
		t.Error("the unit must read the credential file, which is where secrets belong")
	}
}

func TestPlanHonoursTheRequestedMode(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "zerock.yaml")
	secret := filepath.Join(dir, "zerock.env")

	p := &plan{}
	p.writeFileIfAbsent("server config", config, "domain: example.com\n", configFileMode)
	p.writeFileIfAbsent("DNS credential file", secret, "ZEROCK_DNS_API_TOKEN=\n", secretFileMode)
	if err := p.run(); err != nil {
		t.Fatal(err)
	}

	for path, want := range map[string]os.FileMode{config: configFileMode, secret: secretFileMode} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %04o, want %04o", filepath.Base(path), got, want)
		}
	}
}

func TestEnsureReadableRepairsAnUnreadableConfig(t *testing.T) {
	// This is the recovery path for a box installed before the config mode was
	// fixed: writeFileIfAbsent keeps the existing file, so without this repair
	// the service would keep failing to read it.
	path := filepath.Join(t.TempDir(), "zerock.yaml")
	if err := os.WriteFile(path, []byte("domain: example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	p := &plan{}
	p.writeFileIfAbsent("server config", path, "domain: REPLACED\n", configFileMode)
	p.ensureReadable("config permissions", path, configFileMode)
	if err := p.run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != configFileMode {
		t.Errorf("mode = %04o, want %04o", got, configFileMode)
	}
	// The repair must fix permissions without discarding the operator's config.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "domain: example.com\n" {
		t.Errorf("the existing config was replaced: %q", body)
	}
}

func TestEnsureReadableLeavesSufficientPermissionsAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zerock.yaml")
	if err := os.WriteFile(path, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}

	p := &plan{}
	p.ensureReadable("config permissions", path, configFileMode)
	if len(p.steps) != 1 || p.steps[0].note != "already readable" {
		t.Fatalf("steps = %+v, want a no-op", p.steps)
	}
	if err := p.run(); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	// 0640 is group-readable, which is enough; widening it would be a
	// gratuitous loosening of whatever the operator chose.
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("mode = %04o, want it left at 0640", got)
	}
}

func TestEnsureReadableOnAMissingFile(t *testing.T) {
	p := &plan{}
	p.ensureReadable("config permissions", filepath.Join(t.TempDir(), "absent.yaml"), configFileMode)
	if err := p.run(); err != nil {
		t.Errorf("a missing file should not fail the plan: %v", err)
	}
}

func TestEnsureReadableRespectsDryRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zerock.yaml")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	p := &plan{dryRun: true}
	p.ensureReadable("config permissions", path, configFileMode)
	if err := p.run(); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("a dry run changed the mode to %04o", got)
	}
}

func TestServerUnitLimitsRestartStorms(t *testing.T) {
	// A crash loop against an ACME CA burned a real rate limit, so the unit must
	// stop retrying rather than restart forever, and must not retry every 2s.
	unit := serverUnit("/usr/local/bin/zerock")

	if !strings.Contains(unit, "StartLimitBurst=") || !strings.Contains(unit, "StartLimitIntervalSec=") {
		t.Error("the unit should give up after repeated rapid failures")
	}
	// The start limit only works in [Unit]; systemd ignores it in [Service].
	unitSection := unit[strings.Index(unit, "[Unit]"):strings.Index(unit, "[Service]")]
	if !strings.Contains(unitSection, "StartLimitBurst=") {
		t.Error("StartLimitBurst must be in the [Unit] section to take effect")
	}
	if strings.Contains(unit, "RestartSec=2s") {
		t.Error("a 2 second restart interval is what allowed the hammering")
	}
}

func TestProbeLabelIsUniquePerCall(t *testing.T) {
	// A fixed probe name gets negatively cached by resolvers, so after a missing
	// DNS record is finally added the check would keep reporting NXDOMAIN.
	seen := map[string]bool{}
	for range 100 {
		label := probeLabel()
		if seen[label] {
			t.Fatalf("probeLabel repeated %q; a cached NXDOMAIN would make the DNS check lie", label)
		}
		seen[label] = true
		// It has to be a usable DNS label.
		if !namegen.ValidSubdomain(label) {
			t.Fatalf("probeLabel produced %q, which is not a valid DNS label", label)
		}
	}
}
