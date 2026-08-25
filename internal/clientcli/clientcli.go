// Package clientcli implements the commands that talk to a zerock server over
// the network. It imports the client and nothing from the server side, so a
// binary built from it carries no ACME, DNS-provider or database code.
package clientcli

import (
	"errors"
	"flag"

	"github.com/erickdsama/zerock/internal/client"
	"github.com/erickdsama/zerock/internal/cliutil"
)

// Local aliases for the shared helpers, so call sites read the same as they did
// when everything lived in one package.
var (
	bold       = cliutil.Bold
	dim        = cliutil.Dim
	green      = cliutil.Green
	red        = cliutil.Red
	amber      = cliutil.Amber
	cyan       = cliutil.Cyan
	newFlagSet = cliutil.NewFlagSet
	parseFlags = cliutil.ParseFlags
	exactArgs  = cliutil.ExactArgs
	newTable   = cliutil.NewTable
	splitCSV   = cliutil.SplitCSV
	statusWord = cliutil.StatusWord
)

// Commands returns the client verbs, in help-listing order. The version command
// is not among them: every binary adds it last, after its own commands.
func Commands() []cliutil.Command {
	return []cliutil.Command{
		{Name: "http", Summary: "Share a local HTTP port on a subdomain", Usage: usageHTTP, Run: runHTTP},
		{Name: "tcp", Summary: "Share a local TCP port on a public port", Usage: usageTCP, Run: runTCP},
		{Name: "ls", Summary: "List the tunnels currently running", Usage: usageLS, Run: runLS},
		{Name: "kill", Summary: "Close a running tunnel by id", Usage: usageKill, Run: runKill},
		{Name: "reserve", Summary: "Claim a subdomain so only your token can use it", Usage: usageReserve, Run: runReserve},
		{Name: "unreserve", Summary: "Give up a reserved subdomain", Usage: usageUnreserve, Run: runUnreserve},
		{Name: "reservations", Summary: "List reserved subdomains", Usage: usageReservations, Run: runReservations},
		{Name: "login", Summary: "Save a server and token to a profile", Usage: usageLogin, Run: runLogin},
		{Name: "logout", Summary: "Remove a saved profile", Usage: usageLogout, Run: runLogout},
		{Name: "profiles", Summary: "List saved profiles", Usage: usageProfiles, Run: runProfiles},
		{Name: "whoami", Summary: "Show which token the CLI is using", Usage: usageWhoami, Run: runWhoami},
		{Name: "token", Summary: "Manage API tokens (admin)", Usage: usageToken, Run: runToken},
	}
}

// Examples is the trailing block of the root help. Both binaries show it: the
// examples are what someone reaches for either way.
const Examples = `
Examples:
  zerock http 3000                     random subdomain, e.g. swift-otter-4f2
  zerock http 3000 --sub api-x         api-x.yourdomain.com
  zerock http 8080 --auth demo:hunter2 ask for basic auth at the edge
  zerock tcp 5432 --sub db             expose Postgres on a public port

Run 'zerock <command> --help' for the flags of one command.
Environment: ZEROCK_SERVER, ZEROCK_TOKEN, ZEROCK_PROFILE, ZEROCK_CONFIG.
`

// Hint turns the two failures that most often stop a first run into the command
// that fixes them.
func Hint(err error) string {
	if errors.Is(err, client.ErrNoProfile) {
		return "zerock login --server zerock.example.com --token zk_..."
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && apiErr.Status == 403 {
		return "this command needs a token with the admin scope"
	}
	return ""
}

// Main runs a client-only zerock and returns a process exit code.
func Main(args []string) int {
	return cliutil.App{
		Tagline:  "share a local port on your own domain",
		Commands: append(Commands(), cliutil.VersionCommand()),
		Examples: Examples,
		Hint:     Hint,
	}.Main(args)
}

// ProfileFlag registers the shared --profile flag.
func ProfileFlag(fs *flag.FlagSet) *string { return cliutil.ProfileFlag(fs) }

// ResolveProfile loads the config and returns the selected profile.
func ResolveProfile(name string) (string, client.Profile, error) {
	cfg, err := client.LoadConfig()
	if err != nil {
		return "", client.Profile{}, err
	}
	return cfg.Resolve(name)
}

// APIFor returns an API client for the selected profile.
func APIFor(profileName string) (*client.API, client.Profile, error) {
	_, prof, err := ResolveProfile(profileName)
	if err != nil {
		return nil, prof, err
	}
	return client.NewAPI(prof), prof, nil
}

// Unexported spellings for use inside this package.
var (
	profileFlag    = ProfileFlag
	resolveProfile = ResolveProfile
	apiFor         = APIFor
)
