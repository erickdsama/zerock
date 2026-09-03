package client

import (
	"context"
	"strings"
)

// Tunnel is the API's view of one live tunnel, as returned by ListTunnels.
type Tunnel struct {
	ID         string `json:"id"`
	Subdomain  string `json:"sub"`
	Type       string `json:"type"`
	URL        string `json:"url"`
	PublicPort int    `json:"public_port"`
	LocalAddr  string `json:"local_addr"`
	TokenLabel string `json:"token_label"`
	AgentHost  string `json:"agent_host"`
	Uptime     string `json:"uptime"`
	Stats      struct {
		Requests int64 `json:"requests"`
		BytesIn  int64 `json:"bytes_in"`
		BytesOut int64 `json:"bytes_out"`
	} `json:"stats"`
}

// PublicLabel renders whichever public address applies to the tunnel type,
// without its scheme: "api-x.example.com" or "db.example.com:20500".
func (t Tunnel) PublicLabel() string {
	if t.Type == "tcp" {
		return strings.TrimPrefix(t.URL, "tcp://")
	}
	return strings.TrimPrefix(strings.TrimPrefix(t.URL, "https://"), "http://")
}

// ListTunnels returns the tunnels visible to the token. An admin token sees
// every tunnel unless mine is set; a plain token only ever sees its own.
func (a *API) ListTunnels(ctx context.Context, mine bool) ([]Tunnel, error) {
	path := "/api/v1/tunnels"
	if mine {
		path += "?mine=true"
	}
	var out struct {
		Tunnels []Tunnel `json:"tunnels"`
	}
	if err := a.Do(ctx, "GET", path, nil, &out); err != nil {
		return nil, err
	}
	return out.Tunnels, nil
}

// CloseTunnel asks the server to close a tunnel. Its agent is told why and
// does not reconnect.
func (a *API) CloseTunnel(ctx context.Context, id string) error {
	return a.Do(ctx, "DELETE", "/api/v1/tunnels/"+id, nil, nil)
}
