package cliutil

import (
	"flag"
	"fmt"
	"strings"
)

// ParseFlags parses args allowing flags to appear after positional arguments,
// so "zerock http 3000 --sub api-x" works. The standard flag package stops at
// the first operand, which would reject the most natural way to type these
// commands, so operands are moved behind the flags before parsing.
//
// Everything after a bare "--" is treated as an operand.
func ParseFlags(fs *flag.FlagSet, args []string) error {
	var flags, operands []string

	for i := 0; i < len(args); i++ {
		arg := args[i]

		if arg == "--" {
			operands = append(operands, args[i+1:]...)
			break
		}
		// A lone "-" is an operand by convention, and a negative number is not
		// a flag either.
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			operands = append(operands, arg)
			continue
		}

		flags = append(flags, arg)

		name := strings.TrimLeft(arg, "-")
		if idx := strings.IndexByte(name, '='); idx >= 0 {
			continue // value is attached, nothing more to consume
		}
		f := fs.Lookup(name)
		if f == nil {
			// Let flag.Parse produce the "unknown flag" message rather than
			// guessing at arity here.
			continue
		}
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			continue // booleans never take a following value
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}

	return fs.Parse(append(flags, operands...))
}

// ExactArgs validates positional argument count with a useful message.
func ExactArgs(fs *flag.FlagSet, n int, what string) error {
	if fs.NArg() == n {
		return nil
	}
	return fmt.Errorf("expected %s; got %d argument(s). Run 'zerock %s --help'",
		what, fs.NArg(), fs.Name())
}
