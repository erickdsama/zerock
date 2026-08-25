package clientcli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/erickdsama/zerock/internal/client"
	"github.com/erickdsama/zerock/internal/version"
)

const usageLogin = `Save a zerock server and token to a local profile.

The token is verified against the server before anything is written, so a typo
fails here rather than at your first tunnel.

Usage:
  zerock login --server <host> [--token zk_...] [flags]

Examples:
  zerock login --server zerock.example.com --token zk_ab12cd34_...
  zerock login --server zerock.example.com                 read the token from stdin
  zerock login --server zerock.acme.dev --profile acme     keep a second domain

Flags:
  --server host[:port]  server hostname, and control port if not 7223
  --token zk_...        token to save; read from stdin when omitted
  --profile name        profile name (default: the server hostname)
  --api-base url        override the API URL if it is not https://<host>
  --insecure            skip TLS verification (self-signed certificates only)
  --plaintext           talk to a tls.mode=off server without TLS (token sent
                        in the clear; only on a network you trust)
  --no-default          do not make this the default profile
  --no-verify           save without checking the token against the server
`

func runLogin(ctx context.Context, args []string) error {
	fs := newFlagSet("login", usageLogin)
	server := fs.String("server", "", "server hostname")
	token := fs.String("token", "", "token to save")
	profile := fs.String("profile", "", "profile name")
	apiBase := fs.String("api-base", "", "override the API URL")
	insecure := fs.Bool("insecure", false, "skip TLS verification")
	plaintext := fs.Bool("plaintext", false, "talk to a tls.mode=off server without TLS")
	noDefault := fs.Bool("no-default", false, "do not become the default profile")
	noVerify := fs.Bool("no-verify", false, "skip verifying the token")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *server == "" {
		return errors.New("--server is required, e.g. --server zerock.example.com")
	}

	secret := strings.TrimSpace(*token)
	if secret == "" {
		secret = strings.TrimSpace(os.Getenv("ZEROCK_TOKEN"))
	}
	if secret == "" {
		fmt.Fprint(os.Stderr, "Token: ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			return errors.New("no token supplied")
		}
		secret = strings.TrimSpace(line)
	}
	if secret == "" {
		return errors.New("no token supplied")
	}

	cfg, err := client.LoadConfig()
	if err != nil {
		return err
	}

	prof, err := buildProfile(*server, secret, *apiBase, *insecure, *plaintext)
	if err != nil {
		return err
	}

	name := *profile
	if name == "" {
		name = prof.Host
	}

	var label, domain string
	if !*noVerify {
		label, domain, err = verifyProfile(ctx, prof)
		if err != nil {
			return err
		}
	}

	cfg.Profiles[name] = prof
	if cfg.Default == "" || !*noDefault {
		cfg.Default = name
	}
	if err := cfg.Save(); err != nil {
		return err
	}

	fmt.Printf("%s saved profile %s → %s\n", green("✓"), bold(name), prof.ControlAddr())
	if label != "" {
		fmt.Printf("  %s\n", dim(fmt.Sprintf("token %q · domain %s", label, domain)))
	}
	if cfg.Default == name {
		fmt.Printf("  %s\n", dim("this is now the default profile"))
	}
	fmt.Printf("  %s\n", dim("config: "+client.ConfigPath()))
	return nil
}

// buildProfile assembles a profile from login flags.
func buildProfile(server, token, apiBase string, insecure, plaintext bool) (client.Profile, error) {
	prof := client.Profile{Token: token, APIBase: apiBase, Insecure: insecure, Plaintext: plaintext}

	// Reuse the config parser so "host", "host:port" and "https://host" all work.
	host, port, err := client.SplitServer(server)
	if err != nil {
		return prof, err
	}
	prof.Host, prof.ControlPort = host, port
	return prof, nil
}

// verifyProfile checks the token against the server and returns its label and
// the server's domain.
func verifyProfile(ctx context.Context, prof client.Profile) (label, domain string, err error) {
	var out struct {
		Token struct {
			Label  string   `json:"label"`
			Scopes []string `json:"scopes"`
		} `json:"token"`
		Domain string `json:"domain"`
	}
	api := client.NewAPI(prof)
	if err := api.Do(ctx, "GET", "/api/v1/whoami", nil, &out); err != nil {
		return "", "", fmt.Errorf("could not verify the token against %s: %w\n  %s",
			prof.APIURL(""), err, dim("use --no-verify to save it anyway"))
	}
	return out.Token.Label, out.Domain, nil
}

const usageLogout = `Remove a saved profile.

Usage:
  zerock logout [--profile name]

With no --profile the default profile is removed.
`

func runLogout(_ context.Context, args []string) error {
	fs := newFlagSet("logout", usageLogout)
	profile := profileFlag(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	cfg, err := client.LoadConfig()
	if err != nil {
		return err
	}
	name := *profile
	if name == "" {
		name = cfg.Default
	}
	if name == "" {
		return errors.New("no profile to remove")
	}
	if _, ok := cfg.Profiles[name]; !ok {
		return fmt.Errorf("no profile named %q", name)
	}

	delete(cfg.Profiles, name)
	if cfg.Default == name {
		cfg.Default = ""
		// Falling back to the only remaining profile keeps later commands
		// working without another login.
		if names := cfg.Names(); len(names) == 1 {
			cfg.Default = names[0]
		}
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("%s removed profile %s\n", green("✓"), bold(name))
	return nil
}

const usageProfiles = `List saved profiles.

Usage:
  zerock profiles
`

func runProfiles(_ context.Context, args []string) error {
	fs := newFlagSet("profiles", usageProfiles)
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	cfg, err := client.LoadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Profiles) == 0 {
		fmt.Printf("No profiles yet.\n  %s\n", dim("zerock login --server zerock.example.com --token zk_..."))
		return nil
	}

	tw := newTable()
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", dim("PROFILE"), dim("CONTROL"), dim("API"), dim("TOKEN"))
	for _, name := range cfg.Names() {
		p := cfg.Profiles[name]
		marker := name
		if name == cfg.Default {
			marker = bold(name) + " " + green("(default)")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", marker, p.ControlAddr(), p.APIURL(""), maskToken(p.Token))
	}
	return tw.Flush()
}

const usageWhoami = `Show which token the CLI is using and what it can do.

Usage:
  zerock whoami [--profile name]
`

func runWhoami(ctx context.Context, args []string) error {
	fs := newFlagSet("whoami", usageWhoami)
	profile := profileFlag(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	api, prof, err := apiFor(*profile)
	if err != nil {
		return err
	}

	var out struct {
		Token struct {
			ID            string     `json:"id"`
			Label         string     `json:"label"`
			Scopes        []string   `json:"scopes"`
			Status        string     `json:"status"`
			ExpiresAt     *time.Time `json:"expires_at"`
			MaxTunnels    int        `json:"max_tunnels"`
			ActiveTunnels int        `json:"active_tunnels"`
		} `json:"token"`
		Domain        string `json:"domain"`
		ServerVersion string `json:"server_version"`
	}
	if err := api.Do(ctx, "GET", "/api/v1/whoami", nil, &out); err != nil {
		return err
	}

	tw := newTable()
	fmt.Fprintf(tw, "%s\t%s\n", dim("token"), out.Token.ID)
	fmt.Fprintf(tw, "%s\t%s\n", dim("label"), out.Token.Label)
	fmt.Fprintf(tw, "%s\t%s\n", dim("scopes"), strings.Join(out.Token.Scopes, ", "))
	fmt.Fprintf(tw, "%s\t%s\n", dim("status"), statusWord(out.Token.Status))
	if out.Token.ExpiresAt != nil {
		fmt.Fprintf(tw, "%s\t%s\n", dim("expires"), out.Token.ExpiresAt.Local().Format(time.RFC3339))
	}
	limit := "unlimited"
	if out.Token.MaxTunnels > 0 {
		limit = fmt.Sprintf("%d", out.Token.MaxTunnels)
	}
	fmt.Fprintf(tw, "%s\t%d active, limit %s\n", dim("tunnels"), out.Token.ActiveTunnels, limit)
	fmt.Fprintf(tw, "%s\t%s\n", dim("domain"), out.Domain)
	fmt.Fprintf(tw, "%s\t%s (server %s, cli %s)\n", dim("server"), prof.Host, out.ServerVersion, version.String())
	return tw.Flush()
}

// maskToken shows enough of a token to recognise it without exposing it.
func maskToken(t string) string {
	parts := strings.Split(t, "_")
	if len(parts) != 3 {
		return dim("(malformed)")
	}
	return dim(fmt.Sprintf("zk_%s_%s", parts[1], strings.Repeat("•", 8)))
}
