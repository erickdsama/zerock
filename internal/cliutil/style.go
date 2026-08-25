package cliutil

import (
	"os"
	"strings"
)

// colorEnabled reports whether ANSI styling should be emitted. It follows the
// NO_COLOR convention and stays quiet when output is redirected.
var colorEnabled = func() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}()

func wrap(code, s string) string {
	if !colorEnabled || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// The exported names are the whole styling vocabulary. Callers alias them
// locally so call sites stay short.
func Bold(s string) string  { return wrap("1", s) }
func Dim(s string) string   { return wrap("2", s) }
func Red(s string) string   { return wrap("31", s) }
func Green(s string) string { return wrap("32", s) }
func Amber(s string) string { return wrap("33", s) }
func Cyan(s string) string  { return wrap("36", s) }
