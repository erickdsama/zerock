//go:build !linux

package tray

// checkTrayHost is a no-op where the menu bar is always there.
func checkTrayHost() {}
