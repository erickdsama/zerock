package cliutil

import (
	"flag"
	"io"
	"testing"
)

// newTestFlags builds a flag set shaped like the http command's.
func newTestFlags() (*flag.FlagSet, *string, *bool, *int) {
	fs := flag.NewFlagSet("http", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sub := fs.String("sub", "", "")
	quiet := fs.Bool("quiet", false, "")
	port := fs.Int("remote-port", 0, "")
	return fs, sub, quiet, port
}

func TestParseFlagsAcceptsFlagsAfterPositionals(t *testing.T) {
	// This is the form the documentation promises, and the one the standard
	// flag package refuses on its own.
	fs, sub, quiet, _ := newTestFlags()
	if err := ParseFlags(fs, []string{"3000", "--sub", "api-x", "--quiet"}); err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if *sub != "api-x" {
		t.Errorf("sub = %q, want api-x", *sub)
	}
	if !*quiet {
		t.Error("quiet = false, want true")
	}
	if fs.NArg() != 1 || fs.Arg(0) != "3000" {
		t.Errorf("operands = %v, want [3000]", fs.Args())
	}
}

func TestParseFlagsFormVariations(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"flags first", []string{"--sub", "api-x", "--quiet", "3000"}},
		{"flags last", []string{"3000", "--sub", "api-x", "--quiet"}},
		{"interleaved", []string{"--sub", "api-x", "3000", "--quiet"}},
		{"equals form", []string{"3000", "--sub=api-x", "--quiet"}},
		{"single dash", []string{"3000", "-sub", "api-x", "-quiet"}},
		{"bool then operand", []string{"--quiet", "--sub", "api-x", "3000"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs, sub, quiet, _ := newTestFlags()
			if err := ParseFlags(fs, c.args); err != nil {
				t.Fatalf("ParseFlags(%v): %v", c.args, err)
			}
			if *sub != "api-x" || !*quiet {
				t.Errorf("sub = %q, quiet = %v; want api-x, true", *sub, *quiet)
			}
			if fs.NArg() != 1 || fs.Arg(0) != "3000" {
				t.Errorf("operands = %v, want [3000]", fs.Args())
			}
		})
	}
}

func TestParseFlagsStopsAtDoubleDash(t *testing.T) {
	fs, sub, _, _ := newTestFlags()
	if err := ParseFlags(fs, []string{"3000", "--", "--sub", "literal"}); err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if *sub != "" {
		t.Errorf("sub = %q; nothing after -- should be parsed as a flag", *sub)
	}
	if got := fs.Args(); len(got) != 3 {
		t.Errorf("operands = %v, want three literal arguments", got)
	}
}

func TestParseFlagsRejectsUnknownFlag(t *testing.T) {
	fs, _, _, _ := newTestFlags()
	if err := ParseFlags(fs, []string{"3000", "--nonsense", "x"}); err == nil {
		t.Fatal("expected an error for an unknown flag")
	}
}

func TestParseFlagsHandlesNegativeAndBareDash(t *testing.T) {
	fs, _, _, port := newTestFlags()
	if err := ParseFlags(fs, []string{"-", "--remote-port", "-1"}); err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if *port != -1 {
		t.Errorf("remote-port = %d, want -1", *port)
	}
	if fs.NArg() != 1 || fs.Arg(0) != "-" {
		t.Errorf("operands = %v, want [-]", fs.Args())
	}
}

func TestParseFlagsWithNoArgs(t *testing.T) {
	fs, sub, _, _ := newTestFlags()
	if err := ParseFlags(fs, nil); err != nil {
		t.Fatalf("ParseFlags(nil): %v", err)
	}
	if *sub != "" || fs.NArg() != 0 {
		t.Error("an empty argument list should parse to nothing")
	}
}

func TestSplitCSV(t *testing.T) {
	got := SplitCSV(" tunnel , admin ,, ")
	if len(got) != 2 || got[0] != "tunnel" || got[1] != "admin" {
		t.Errorf("splitCSV = %v, want [tunnel admin]", got)
	}
	if SplitCSV("") != nil {
		t.Error("an empty string should produce no scopes")
	}
}
