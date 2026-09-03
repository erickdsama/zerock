package tray

import (
	"errors"

	"github.com/erickdsama/zerock/internal/client"
)

// formData is one filled-in new-tunnel form: the fields as the user typed them.
// It is what the native dialogs return and what gets parsed into a SavedTunnel.
type formData struct {
	Type, Port, Sub, Host, Auth, RemotePort, Name string
	Autostart                                     bool
}

// askForm shows the native form until it comes back valid or is cancelled. A
// mistake is shown as an alert and the form reopens with what was typed, where
// the platform's dialog can be prefilled.
func askForm() (name string, spec client.SavedTunnel, ok bool, err error) {
	var data formData
	for {
		data, ok, err = askNativeForm(data)
		if err != nil || !ok {
			return "", spec, false, err
		}
		name, spec, err = data.parse()
		if err == nil {
			return name, spec, true, nil
		}
		alert("Not a tunnel", err.Error())
	}
}

// parse hands the fields to the same parser the CLI-style spec uses, so
// validation lives in one place.
func (d formData) parse() (string, client.SavedTunnel, error) {
	kind := d.Type
	if kind == "" {
		kind = "http"
	}
	if d.Port == "" {
		return "", client.SavedTunnel{}, errors.New("the local port is required")
	}
	args := []string{kind, d.Port}
	if d.Sub != "" {
		args = append(args, "--sub", d.Sub)
	}
	if d.Host != "" {
		args = append(args, "--host", d.Host)
	}
	if kind == "http" && d.Auth != "" {
		args = append(args, "--auth", d.Auth)
	}
	if kind == "tcp" && d.RemotePort != "" {
		args = append(args, "--remote-port", d.RemotePort)
	}
	if d.Name != "" {
		args = append(args, "--name", d.Name)
	}
	if d.Autostart {
		args = append(args, "--autostart")
	}
	return parseSpecArgs(args)
}
