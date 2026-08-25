// Package cli implements the full zerock command line: the client verbs plus
// the server-side ones that only make sense on the machine that owns the
// domain. The lean client build uses internal/clientcli directly and never
// reaches this package, which is what keeps the server out of it.
package cli

import (
	"github.com/erickdsama/zerock/internal/clientcli"
	"github.com/erickdsama/zerock/internal/cliutil"
)

// Local aliases for the shared helpers, so call sites read the same as they did
// when everything lived in one package.
var (
	bold       = cliutil.Bold
	dim        = cliutil.Dim
	red        = cliutil.Red
	green      = cliutil.Green
	amber      = cliutil.Amber
	newFlagSet = cliutil.NewFlagSet
	parseFlags = cliutil.ParseFlags
	newTable   = cliutil.NewTable
	statusWord = cliutil.StatusWord

	profileFlag    = clientcli.ProfileFlag
	resolveProfile = clientcli.ResolveProfile
)

// serverCommands are the verbs that run on the zerock host itself.
func serverCommands() []cliutil.Command {
	return []cliutil.Command{
		{Name: "serve", Summary: "Run the zerock server", Usage: usageServe, Run: runServe},
		{Name: "service", Summary: "Install zerock as a systemd service", Usage: usageService, Run: runService},
		{Name: "doctor", Summary: "Check that a running server actually works", Usage: usageDoctor, Run: runDoctor},
		{Name: "admin-token", Summary: "Mint an admin token from the database (recovery)", Usage: usageAdminToken, Run: runAdminToken},
		{Name: "init-config", Summary: "Print a starter server config", Usage: usageInitConfig, Run: runInitConfig},
	}
}

// Main runs the full CLI and returns a process exit code.
func Main(args []string) int {
	cmds := append(clientcli.Commands(), serverCommands()...)
	cmds = append(cmds, cliutil.VersionCommand())
	return cliutil.App{
		Tagline:  "share a local port on your own domain",
		Commands: cmds,
		Examples: clientcli.Examples,
		Hint:     clientcli.Hint,
	}.Main(args)
}
