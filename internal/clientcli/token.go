package clientcli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const usageToken = `Manage API tokens. Every subcommand needs a token with the admin scope.

Usage:
  zerock token new --label <name> [flags]
  zerock token ls
  zerock token revoke <id>
  zerock token rm <id>

Examples:
  zerock token new --label "erick laptop"
  zerock token new --label ci --expires 720h --max-tunnels 2
  zerock token new --label ops --scopes tunnel,admin
  zerock token revoke ab12cd34

'revoke' keeps the record and its reservations but stops the token working.
'rm' deletes the record outright and frees the subdomains it held. Both close
that token's live tunnels immediately.

Flags for 'new':
  --label name        who or what the token is for (required)
  --scopes list       comma-separated: tunnel, admin (default tunnel)
  --expires duration  lifetime such as 720h; never expires if omitted
  --max-tunnels n     concurrent tunnel limit (0 = unlimited)
  --max-reservations n  subdomain limit (0 = server default)
  --profile name      saved profile to use
`

func runToken(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("expected a subcommand: new, ls, revoke or rm")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "new", "create", "add":
		return runTokenNew(ctx, rest)
	case "ls", "list":
		return runTokenList(ctx, rest)
	case "revoke":
		return runTokenRevoke(ctx, rest)
	case "rm", "delete":
		return runTokenDelete(ctx, rest)
	case "-h", "--help":
		fmt.Print(usageToken)
		return nil
	default:
		return fmt.Errorf("unknown token subcommand %q (want new, ls, revoke or rm)", sub)
	}
}

func runTokenNew(ctx context.Context, args []string) error {
	fs := newFlagSet("token new", usageToken)
	label := fs.String("label", "", "who or what the token is for")
	scopes := fs.String("scopes", "tunnel", "comma-separated scopes")
	expires := fs.String("expires", "", "lifetime such as 720h")
	maxTunnels := fs.Int("max-tunnels", 0, "concurrent tunnel limit")
	maxReservations := fs.Int("max-reservations", 0, "subdomain reservation limit")
	profile := profileFlag(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if strings.TrimSpace(*label) == "" {
		return errors.New("--label is required, e.g. --label \"erick laptop\"")
	}
	if *expires != "" {
		if _, err := time.ParseDuration(*expires); err != nil {
			return fmt.Errorf("--expires must be a duration such as 720h or 30m: %w", err)
		}
	}

	api, prof, err := apiFor(*profile)
	if err != nil {
		return err
	}

	body := map[string]any{
		"label":            *label,
		"scopes":           splitCSV(*scopes),
		"expires_in":       *expires,
		"max_tunnels":      *maxTunnels,
		"max_reservations": *maxReservations,
	}
	var out struct {
		Token  tokenRow `json:"token"`
		Secret string   `json:"secret"`
	}
	if err := api.Do(ctx, "POST", "/api/v1/tokens", body, &out); err != nil {
		return err
	}

	fmt.Printf("%s created token %s for %s\n\n", green("✓"), bold(out.Token.ID), bold(out.Token.Label))
	fmt.Printf("  %s\n\n", bold(out.Secret))
	fmt.Printf("  %s\n", amber("This is the only time the secret is shown. Store it now."))
	fmt.Printf("  %s\n", dim(fmt.Sprintf("zerock login --server %s --token %s", prof.Host, out.Secret)))
	return nil
}

// tokenRow is the API's view of a token.
type tokenRow struct {
	ID              string     `json:"id"`
	Label           string     `json:"label"`
	Scopes          []string   `json:"scopes"`
	Status          string     `json:"status"`
	CreatedAt       time.Time  `json:"created_at"`
	ExpiresAt       *time.Time `json:"expires_at"`
	LastUsed        *time.Time `json:"last_used_at"`
	MaxTunnels      int        `json:"max_tunnels"`
	MaxReservations int        `json:"max_reservations"`
	ActiveTunnels   int        `json:"active_tunnels"`
}

func runTokenList(ctx context.Context, args []string) error {
	fs := newFlagSet("token ls", usageToken)
	profile := profileFlag(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	api, _, err := apiFor(*profile)
	if err != nil {
		return err
	}
	var out struct {
		Tokens []tokenRow `json:"tokens"`
	}
	if err := api.Do(ctx, "GET", "/api/v1/tokens", nil, &out); err != nil {
		return err
	}
	if len(out.Tokens) == 0 {
		fmt.Println("No tokens.")
		return nil
	}

	tw := newTable()
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		dim("ID"), dim("LABEL"), dim("SCOPES"), dim("STATUS"), dim("LIVE"), dim("EXPIRES"), dim("LAST USED"))
	for _, t := range out.Tokens {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			t.ID, t.Label, strings.Join(t.Scopes, ","), statusWord(t.Status),
			liveCount(t), stamp(t.ExpiresAt), relative(t.LastUsed))
	}
	return tw.Flush()
}

// liveCount renders active tunnels against the token's limit.
func liveCount(t tokenRow) string {
	if t.MaxTunnels > 0 {
		return fmt.Sprintf("%d/%d", t.ActiveTunnels, t.MaxTunnels)
	}
	return fmt.Sprintf("%d", t.ActiveTunnels)
}

func runTokenRevoke(ctx context.Context, args []string) error {
	fs := newFlagSet("token revoke", usageToken)
	profile := profileFlag(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := exactArgs(fs, 1, "one token id"); err != nil {
		return err
	}

	api, _, err := apiFor(*profile)
	if err != nil {
		return err
	}
	id := fs.Arg(0)
	if err := api.Do(ctx, "POST", "/api/v1/tokens/"+id+"/revoke", nil, nil); err != nil {
		return err
	}
	fmt.Printf("%s revoked token %s\n", green("✓"), bold(id))
	fmt.Printf("  %s\n", dim("its live tunnels were closed; its reservations are kept"))
	return nil
}

func runTokenDelete(ctx context.Context, args []string) error {
	fs := newFlagSet("token rm", usageToken)
	profile := profileFlag(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := exactArgs(fs, 1, "one token id"); err != nil {
		return err
	}

	api, _, err := apiFor(*profile)
	if err != nil {
		return err
	}
	id := fs.Arg(0)
	if err := api.Do(ctx, "DELETE", "/api/v1/tokens/"+id, nil, nil); err != nil {
		return err
	}
	fmt.Printf("%s deleted token %s\n", green("✓"), bold(id))
	fmt.Printf("  %s\n", dim("its live tunnels were closed and its reservations released"))
	return nil
}

// stamp renders an optional timestamp as a date.
func stamp(t *time.Time) string {
	if t == nil {
		return dim("never")
	}
	return t.Local().Format("2006-01-02")
}

// relative renders an optional timestamp as an age.
func relative(t *time.Time) string {
	if t == nil {
		return dim("never")
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
