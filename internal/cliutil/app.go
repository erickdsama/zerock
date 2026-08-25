// Package cliutil holds the command-line plumbing shared by every zerock
// binary: the dispatcher, flag handling and output styling. It deliberately
// depends on nothing but the version package, so a client-only build does not
// drag the server in behind it.
package cliutil

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/erickdsama/zerock/internal/version"
)

// Command is one CLI verb.
type Command struct {
	Name    string
	Summary string
	Usage   string
	Run     func(ctx context.Context, args []string) error
}

// App is a set of commands sharing one dispatcher. The two zerock binaries
// differ only in which commands they register: the full binary adds the
// server-side verbs, the client build does not.
type App struct {
	// Tagline follows the program name in the root help.
	Tagline string
	// Commands is the dispatch table, in help-listing order.
	Commands []Command
	// Examples is appended verbatim to the root help.
	Examples string
	// Hint may return extra guidance for a failed command, such as pointing at
	// the login step when no profile is configured. It may be nil.
	Hint func(error) string
}

// Main runs the app and returns a process exit code.
func (a App) Main(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(args) == 0 {
		a.printRootHelp(os.Stdout)
		return 0
	}

	name := args[0]
	switch name {
	case "-h", "--help", "help":
		if len(args) > 1 {
			if cmd, ok := a.lookup(args[1]); ok {
				fmt.Fprint(os.Stdout, cmd.Usage)
				return 0
			}
			fmt.Fprintf(os.Stderr, "zerock: unknown command %q\n", args[1])
			return 2
		}
		a.printRootHelp(os.Stdout)
		return 0
	case "-v", "--version":
		name = "version"
		args = args[:1]
	}

	cmd, ok := a.lookup(name)
	if !ok {
		fmt.Fprintf(os.Stderr, "zerock: unknown command %q\n\n", name)
		a.printRootHelp(os.Stderr)
		return 2
	}

	err := cmd.Run(ctx, args[1:])
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(os.Stdout, cmd.Usage)
		return 0
	case errors.Is(err, context.Canceled):
		return 0
	}

	fmt.Fprintf(os.Stderr, "%s %v\n", Red("error:"), err)
	if a.Hint != nil {
		if hint := a.Hint(err); hint != "" {
			fmt.Fprintf(os.Stderr, "\n  %s\n", Dim(hint))
		}
	}
	return 1
}

func (a App) lookup(name string) (Command, bool) {
	for _, c := range a.Commands {
		if c.Name == name {
			return c, true
		}
	}
	return Command{}, false
}

func (a App) printRootHelp(w io.Writer) {
	fmt.Fprintf(w, "zerock %s - %s\n\n", version.Version, a.Tagline)
	fmt.Fprintf(w, "Usage:\n  zerock <command> [flags]\n\nCommands:\n")
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	for _, c := range a.Commands {
		fmt.Fprintf(tw, "  %s\t%s\n", c.Name, c.Summary)
	}
	tw.Flush()
	fmt.Fprint(w, a.Examples)
}

// VersionCommand reports the build. Every binary carries it, so it lives here
// rather than on either side of the split.
func VersionCommand() Command {
	const usage = `Print the zerock version.

Usage:
  zerock version
`
	return Command{"version", "Print the zerock version", usage,
		func(_ context.Context, args []string) error {
			fs := NewFlagSet("version", usage)
			if err := ParseFlags(fs, args); err != nil {
				return err
			}
			fmt.Printf("zerock %s\n", version.String())
			fmt.Printf("  %s\n", Dim("built with "+version.GoVersion()))
			return nil
		}}
}

// NewFlagSet builds a flag set that prints a command's own usage text.
func NewFlagSet(name, usage string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	return fs
}

// ProfileFlag registers the shared --profile flag.
func ProfileFlag(fs *flag.FlagSet) *string {
	return fs.String("profile", "", "saved profile to use (default: the configured default)")
}

// NewTable returns a tabwriter for aligned list output.
func NewTable() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 8, 3, ' ', 0)
}

// SplitCSV splits a comma-separated flag value, dropping blanks.
func SplitCSV(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// StatusWord colours a token status.
func StatusWord(s string) string {
	switch s {
	case "active":
		return Green(s)
	case "revoked", "expired":
		return Amber(s)
	default:
		return s
	}
}
