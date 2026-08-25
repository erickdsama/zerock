package clientcli

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const usageLS = `List the tunnels currently running on the server.

An admin token sees every tunnel; a plain token sees only its own.

Usage:
  zerock ls [flags]

Flags:
  --mine          only your own tunnels, even with an admin token
  --profile name  saved profile to use
`

// tunnelRow is the API's view of a live tunnel.
type tunnelRow struct {
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

func runLS(ctx context.Context, args []string) error {
	fs := newFlagSet("ls", usageLS)
	mine := fs.Bool("mine", false, "only your own tunnels")
	profile := profileFlag(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	api, _, err := apiFor(*profile)
	if err != nil {
		return err
	}

	path := "/api/v1/tunnels"
	if *mine {
		path += "?mine=true"
	}
	var out struct {
		Tunnels []tunnelRow `json:"tunnels"`
	}
	if err := api.Do(ctx, "GET", path, nil, &out); err != nil {
		return err
	}
	if len(out.Tunnels) == 0 {
		fmt.Printf("No tunnels running.\n  %s\n", dim("zerock http 3000"))
		return nil
	}

	tw := newTable()
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		dim("ID"), dim("PUBLIC"), dim("→ LOCAL"), dim("OWNER"), dim("AGENT"), dim("UP"), dim("TRAFFIC"))
	for _, t := range out.Tunnels {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			t.ID, bold(publicLabel(t)), t.LocalAddr, t.TokenLabel,
			orDash(t.AgentHost), t.Uptime,
			dim(fmt.Sprintf("%d req · %s ↑ %s ↓", t.Stats.Requests,
				humanBytes(t.Stats.BytesOut), humanBytes(t.Stats.BytesIn))))
	}
	return tw.Flush()
}

// publicLabel renders whichever public address applies to a tunnel type.
func publicLabel(t tunnelRow) string {
	if t.Type == "tcp" {
		return strings.TrimPrefix(t.URL, "tcp://")
	}
	return strings.TrimPrefix(strings.TrimPrefix(t.URL, "https://"), "http://")
}

const usageKill = `Close a running tunnel. The agent sees why and stops.

Usage:
  zerock kill <tunnel-id> [--profile name]

Find ids with 'zerock ls'.
`

func runKill(ctx context.Context, args []string) error {
	fs := newFlagSet("kill", usageKill)
	profile := profileFlag(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := exactArgs(fs, 1, "one tunnel id"); err != nil {
		return err
	}

	api, _, err := apiFor(*profile)
	if err != nil {
		return err
	}
	id := fs.Arg(0)
	if err := api.Do(ctx, "DELETE", "/api/v1/tunnels/"+id, nil, nil); err != nil {
		return err
	}
	fmt.Printf("%s closed tunnel %s\n", green("✓"), bold(id))
	return nil
}

const usageReserve = `Claim a subdomain so only your token can use it.

A reservation survives restarts and outlives any single tunnel, which is what
makes a subdomain safe to hand out or hard-code in a webhook.

Usage:
  zerock reserve <subdomain> [flags]

Examples:
  zerock reserve api-x
  zerock reserve staging --note "shared with the mobile team"

Flags:
  --note text       free-form note stored with the reservation
  --token-id id     reserve for another token (admin only)
  --profile name    saved profile to use
`

func runReserve(ctx context.Context, args []string) error {
	fs := newFlagSet("reserve", usageReserve)
	note := fs.String("note", "", "note to store with the reservation")
	tokenID := fs.String("token-id", "", "reserve on behalf of another token (admin)")
	profile := profileFlag(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := exactArgs(fs, 1, "one subdomain"); err != nil {
		return err
	}

	api, _, err := apiFor(*profile)
	if err != nil {
		return err
	}

	body := map[string]string{"sub": strings.ToLower(fs.Arg(0))}
	if *note != "" {
		body["note"] = *note
	}
	if *tokenID != "" {
		body["token_id"] = *tokenID
	}

	var out struct {
		URL string `json:"url"`
	}
	if err := api.Do(ctx, "POST", "/api/v1/reservations", body, &out); err != nil {
		return err
	}
	fmt.Printf("%s reserved %s\n", green("✓"), bold(out.URL))
	fmt.Printf("  %s\n", dim(fmt.Sprintf("use it with: zerock http <port> --sub %s", fs.Arg(0))))
	return nil
}

const usageUnreserve = `Give up a reserved subdomain so anyone on the server can use it.

Usage:
  zerock unreserve <subdomain> [--profile name]
`

func runUnreserve(ctx context.Context, args []string) error {
	fs := newFlagSet("unreserve", usageUnreserve)
	profile := profileFlag(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := exactArgs(fs, 1, "one subdomain"); err != nil {
		return err
	}

	api, _, err := apiFor(*profile)
	if err != nil {
		return err
	}
	sub := strings.ToLower(fs.Arg(0))
	if err := api.Do(ctx, "DELETE", "/api/v1/reservations/"+sub, nil, nil); err != nil {
		return err
	}
	fmt.Printf("%s released %s\n", green("✓"), bold(sub))
	return nil
}

const usageReservations = `List reserved subdomains.

An admin token sees every reservation; a plain token sees only its own.

Usage:
  zerock reservations [flags]

Flags:
  --mine          only your own reservations, even with an admin token
  --profile name  saved profile to use
`

func runReservations(ctx context.Context, args []string) error {
	fs := newFlagSet("reservations", usageReservations)
	mine := fs.Bool("mine", false, "only your own reservations")
	profile := profileFlag(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	api, _, err := apiFor(*profile)
	if err != nil {
		return err
	}

	path := "/api/v1/reservations"
	if *mine {
		path += "?mine=true"
	}
	var out struct {
		Reservations []struct {
			Subdomain string    `json:"sub"`
			TokenID   string    `json:"token_id"`
			Note      string    `json:"note"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"reservations"`
		Domain string `json:"domain"`
	}
	if err := api.Do(ctx, "GET", path, nil, &out); err != nil {
		return err
	}
	if len(out.Reservations) == 0 {
		fmt.Printf("No reservations.\n  %s\n", dim("zerock reserve api-x"))
		return nil
	}

	tw := newTable()
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", dim("HOST"), dim("TOKEN"), dim("SINCE"), dim("NOTE"))
	for _, r := range out.Reservations {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			bold(r.Subdomain+"."+out.Domain), r.TokenID,
			r.CreatedAt.Local().Format("2006-01-02"), orDash(r.Note))
	}
	return tw.Flush()
}

// orDash renders an empty value as a dash so columns stay readable.
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return dim("-")
	}
	return s
}

// humanBytes renders a byte count compactly.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	value := float64(n)
	for _, suffix := range []string{"K", "M", "G", "T"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f%s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1fP", value)
}
