package tray

import (
	"os/exec"
	"syscall"
)

// hideConsole keeps a helper such as powershell.exe from flashing a console
// window: the tray itself is built with -H windowsgui and has none.
func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
