//go:build !windows

package tray

import "os/exec"

// hideConsole is a no-op where helpers do not open console windows.
func hideConsole(*exec.Cmd) {}
