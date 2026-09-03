package tray

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// The desktop integration below shells out to the tools each platform ships
// rather than binding a GUI toolkit: the widget stays one small binary, and
// what it needs (open a URL, copy text, show an error, ask a few fields) is
// exactly what those tools do. The new-tunnel form lives in form_native.go.

// openTarget opens a URL or file with the desktop's default handler.
func openTarget(target string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target).Start()
	case "windows":
		cmd := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", target)
		hideConsole(cmd)
		return cmd.Start()
	default:
		return exec.Command("xdg-open", target).Start()
	}
}

// copyText puts text on the clipboard.
func copyText(text string) error {
	var candidates [][]string
	switch runtime.GOOS {
	case "darwin":
		candidates = [][]string{{"pbcopy"}}
	case "windows":
		cmd := powershell(winCopyScript, nil)
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	default:
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			candidates = append(candidates, []string{"wl-copy"})
		}
		candidates = append(candidates,
			[]string{"xclip", "-selection", "clipboard"},
			[]string{"xsel", "--clipboard", "--input"},
			[]string{"wl-copy"})
	}
	for _, argv := range candidates {
		if _, err := exec.LookPath(argv[0]); err != nil {
			continue
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	}
	return errors.New("no clipboard tool found (install wl-clipboard, xclip or xsel)")
}

// alert shows a message the user has to see, such as why a tunnel could not be
// saved. Failures to show it are ignored: the same text is also logged.
func alert(title, message string) {
	switch runtime.GOOS {
	case "windows":
		_ = powershell(winAlertScript, map[string]string{"ZK_TITLE": title, "ZK_MSG": message}).Run()
	case "darwin":
		script := fmt.Sprintf(`display alert %s message %s`, appleString(title), appleString(message))
		_ = exec.Command("osascript", "-e", script).Run()
	default:
		if _, err := exec.LookPath("zenity"); err == nil {
			_ = exec.Command("zenity", "--error", "--title", title, "--text", message, "--width", "420").Run()
			return
		}
		if _, err := exec.LookPath("kdialog"); err == nil {
			_ = exec.Command("kdialog", "--title", title, "--error", message).Run()
			return
		}
		if _, err := exec.LookPath("notify-send"); err == nil {
			_ = exec.Command("notify-send", "--app-name=zerock", title, message).Run()
		}
	}
}

// appleString quotes text as an AppleScript string literal.
func appleString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
