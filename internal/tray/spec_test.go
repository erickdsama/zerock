package tray

import (
	"testing"

	"github.com/erickdsama/zerock/internal/client"
)

func TestParseSpec(t *testing.T) {
	cases := []struct {
		in   string
		name string
		spec client.SavedTunnel
	}{
		{"3000", "http-3000", client.SavedTunnel{Type: "http", Port: 3000}},
		{"http 3000 --sub api-x", "api-x", client.SavedTunnel{Type: "http", Port: 3000, Subdomain: "api-x"}},
		{"3000 --sub API-X", "api-x", client.SavedTunnel{Type: "http", Port: 3000, Subdomain: "api-x"}},
		{"tcp 5432 --sub db --remote-port 20500", "db",
			client.SavedTunnel{Type: "tcp", Port: 5432, Subdomain: "db", RemotePort: 20500}},
		{"http localhost:8080 --auth demo:hunter2 --name demo --autostart", "demo",
			client.SavedTunnel{Type: "http", Port: 8080, BasicAuth: "demo:hunter2", Autostart: true}},
		{"http 3000 --host 192.168.1.20", "http-3000", client.SavedTunnel{Type: "http", Port: 3000, Host: "192.168.1.20"}},
		{"--sub api-x 3000", "api-x", client.SavedTunnel{Type: "http", Port: 3000, Subdomain: "api-x"}},
		{"3000 api-x", "api-x", client.SavedTunnel{Type: "http", Port: 3000, Subdomain: "api-x"}},
		{"tcp 5432 db", "db", client.SavedTunnel{Type: "tcp", Port: 5432, Subdomain: "db"}},
		{"TCP 5432", "tcp-5432", client.SavedTunnel{Type: "tcp", Port: 5432}},
	}
	for _, c := range cases {
		name, spec, err := ParseSpec(c.in)
		if err != nil {
			t.Errorf("ParseSpec(%q): %v", c.in, err)
			continue
		}
		if name != c.name || spec != c.spec {
			t.Errorf("ParseSpec(%q) = %q, %+v; want %q, %+v", c.in, name, spec, c.name, c.spec)
		}
	}

	for _, bad := range []string{
		"", "   ", "http", "abc", "http 70000", "udp 53", "http 3000 api-x extra", "3000 api-x --sub other",
		"tcp 5432 --auth a:b", "http 80 --remote-port 2000", "http 80 --auth nocolon", "http 80 --bogus",
	} {
		if _, _, err := ParseSpec(bad); err == nil {
			t.Errorf("ParseSpec(%q) should have failed", bad)
		}
	}
}

func TestUniqueName(t *testing.T) {
	taken := map[string]client.SavedTunnel{"api": {}, "api-2": {}}
	if got := uniqueName("api", taken); got != "api-3" {
		t.Errorf("uniqueName = %q, want api-3", got)
	}
	if got := uniqueName("db", taken); got != "db" {
		t.Errorf("uniqueName = %q, want db", got)
	}
}

func TestSavedTunnelArgsRoundTrip(t *testing.T) {
	for _, in := range []string{
		"http 3000 --sub api-x",
		"tcp 5432 --sub db --remote-port 20500",
		"http 8080 --host 192.168.1.20 --auth demo:hunter2",
	} {
		_, spec, err := ParseSpec(in)
		if err != nil {
			t.Fatalf("ParseSpec(%q): %v", in, err)
		}
		_, again, err := ParseSpec(spec.Args())
		if err != nil {
			t.Fatalf("ParseSpec(%q) from Args(): %v", spec.Args(), err)
		}
		if again != spec {
			t.Errorf("Args() of %q = %q does not round-trip: %+v vs %+v", in, spec.Args(), again, spec)
		}
	}
}

func TestIcoWrap(t *testing.T) {
	png := Icon(StateUp, false)
	ico := icoWrap(png, iconSize)
	if len(ico) != 22+len(png) {
		t.Fatalf("ico length %d, want header 22 + png %d", len(ico), len(png))
	}
	if ico[2] != 1 || ico[4] != 1 || int(ico[6]) != iconSize || int(ico[7]) != iconSize {
		t.Fatalf("bad ICONDIR/ENTRY header: % x", ico[:22])
	}
	if string(ico[22:30]) != string(png[:8]) {
		t.Fatal("PNG signature not at the declared offset")
	}
}

func TestEncodePowerShell(t *testing.T) {
	// "hi" in UTF-16LE is 68 00 69 00.
	if got := encodePowerShell("hi"); got != "aABpAA==" {
		t.Fatalf("encodePowerShell = %q", got)
	}
}
