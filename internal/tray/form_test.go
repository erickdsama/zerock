package tray

import (
	"testing"

	"github.com/erickdsama/zerock/internal/client"
)

func TestFormDataParse(t *testing.T) {
	name, spec, err := formData{Type: "http", Port: "3000", Sub: "API-X", Auth: "demo:hunter 2", Autostart: true}.parse()
	if err != nil {
		t.Fatal(err)
	}
	want := client.SavedTunnel{Type: "http", Port: 3000, Subdomain: "api-x", BasicAuth: "demo:hunter 2", Autostart: true}
	if name != "api-x" || spec != want {
		t.Fatalf("got %q %+v, want api-x %+v", name, spec, want)
	}

	// Fields that belong to the other type are dropped rather than rejected,
	// since the page hides them but still submits them.
	name, spec, err = formData{Type: "tcp", Port: "5432", Auth: "left:over", RemotePort: "20500", Name: "pg"}.parse()
	if err != nil {
		t.Fatal(err)
	}
	if name != "pg" || spec.BasicAuth != "" || spec.RemotePort != 20500 {
		t.Fatalf("got %q %+v", name, spec)
	}

	for _, bad := range []formData{{}, {Port: "abc"}, {Port: "70000"}, {Port: "80", Auth: "nocolon"}} {
		if _, _, err := bad.parse(); err == nil {
			t.Errorf("%+v should have failed", bad)
		}
	}
}
