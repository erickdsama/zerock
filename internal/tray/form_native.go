package tray

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// Field labels, shared so both platforms ask the same questions in the same
// order.
const (
	labelType       = "Type"
	labelPort       = "Local port"
	labelSub        = "Subdomain (empty = random)"
	labelHost       = "Local host (empty = 127.0.0.1)"
	labelAuth       = "Basic auth user:pass (http only)"
	labelRemotePort = "Public port (tcp only)"
	labelName       = "Save as (empty = the subdomain)"
	labelAutostart  = "Start whenever the widget launches"
)

// askNativeForm shows the platform's form dialog. ok is false on Cancel.
func askNativeForm(prev formData) (formData, bool, error) {
	switch runtime.GOOS {
	case "darwin":
		data, ok, err := askCocoaForm(prev)
		if err == nil {
			return data, ok, nil
		}
		// The Cocoa form is built by osascript at run time; if that breaks
		// on some macOS version, plain dialogs still work.
		return askStepByStep(prev)
	case "windows":
		return askWinForm(prev)
	default:
		if _, err := exec.LookPath("zenity"); err == nil {
			return askZenityForm(prev)
		}
		if _, err := exec.LookPath("kdialog"); err == nil {
			return askStepByStep(prev)
		}
		return formData{}, false, errors.New("no dialog tool found (install zenity or kdialog)")
	}
}

// askZenityForm uses zenity's multi-field form. Its entries cannot be
// prefilled, so prev is only used to preselect the combos.
func askZenityForm(prev formData) (formData, bool, error) {
	const sep = "\x1f"
	kinds := "http|tcp"
	if prev.Type == "tcp" {
		kinds = "tcp|http"
	}
	auto := "no|yes"
	if prev.Autostart {
		auto = "yes|no"
	}
	out, err := exec.Command("zenity", "--forms", "--title", "New tunnel",
		"--text", "Share a local port on your zerock domain.",
		"--add-combo", labelType, "--combo-values", kinds,
		"--add-entry", labelPort,
		"--add-entry", labelSub,
		"--add-entry", labelHost,
		"--add-entry", labelAuth,
		"--add-entry", labelRemotePort,
		"--add-entry", labelName,
		"--add-combo", labelAutostart, "--combo-values", auto,
		"--separator", sep).Output()
	if err != nil {
		if isCancel(err) {
			return formData{}, false, nil
		}
		return formData{}, false, err
	}
	fields := strings.Split(strings.TrimRight(string(out), "\n"), sep)
	if len(fields) != 8 {
		return formData{}, false, fmt.Errorf("unexpected zenity output: %q", out)
	}
	return formData{
		Type:       fields[0],
		Port:       fields[1],
		Sub:        fields[2],
		Host:       fields[3],
		Auth:       fields[4],
		RemotePort: fields[5],
		Name:       fields[6],
		Autostart:  fields[7] == "yes",
	}, true, nil
}

// askCocoaForm builds an NSAlert with the fields as its accessory view. It is
// AppleScriptObjC run by osascript, so nothing is compiled and no framework is
// linked beyond what the tray already needs.
func askCocoaForm(prev formData) (formData, bool, error) {
	auto := "0"
	if prev.Autostart {
		auto = "1"
	}
	kind := prev.Type
	if kind == "" {
		kind = "http"
	}
	out, err := exec.Command("osascript", "-l", "AppleScript", "-e", cocoaFormScript, "--",
		kind, prev.Port, prev.Sub, prev.Host, prev.Auth, prev.RemotePort, prev.Name, auto).Output()
	if err != nil {
		return formData{}, false, err
	}
	line := strings.TrimRight(string(out), "\n")
	if line == "cancel" {
		return formData{}, false, nil
	}
	fields := strings.Split(line, "\t")
	if len(fields) != 8 {
		return formData{}, false, fmt.Errorf("unexpected osascript output: %q", line)
	}
	return formData{
		Type:       fields[0],
		Port:       fields[1],
		Sub:        fields[2],
		Host:       fields[3],
		Auth:       fields[4],
		RemotePort: fields[5],
		Name:       fields[6],
		Autostart:  fields[7] == "1",
	}, true, nil
}

// cocoaFormScript lays the fields out top to bottom in an accessory view.
// Arguments: type port sub host auth remote_port name autostart(0|1).
// Prints the same eight values tab-separated, or "cancel".
const cocoaFormScript = `
use AppleScript version "2.4"
use framework "Foundation"
use framework "AppKit"
use scripting additions

on field(view, y, label, value)
	set lbl to current application's NSTextField's labelWithString:label
	lbl's setFrame:(current application's NSMakeRect(0, y + 22, 340, 16))
	lbl's setFont:(current application's NSFont's systemFontOfSize:11)
	lbl's setTextColor:(current application's NSColor's secondaryLabelColor())
	view's addSubview:lbl
	set tf to current application's NSTextField's textFieldWithString:value
	tf's setFrame:(current application's NSMakeRect(0, y, 340, 22))
	view's addSubview:tf
	return tf
end field

on run argv
	set {kind, portV, subV, hostV, authV, rportV, nameV, autoV} to argv
	set app to current application's NSApplication's sharedApplication()
	app's activateIgnoringOtherApps:true

	set alert to current application's NSAlert's alloc()'s init()
	alert's setMessageText:"New tunnel"
	alert's setInformativeText:"Share a local port on your zerock domain."
	alert's addButtonWithTitle:"Open tunnel"
	alert's addButtonWithTitle:"Cancel"

	set view to current application's NSView's alloc()'s initWithFrame:(current application's NSMakeRect(0, 0, 340, 330))

	set typeLabel to current application's NSTextField's labelWithString:"` + labelType + `"
	typeLabel's setFrame:(current application's NSMakeRect(0, 308, 340, 16))
	typeLabel's setFont:(current application's NSFont's systemFontOfSize:11)
	typeLabel's setTextColor:(current application's NSColor's secondaryLabelColor())
	view's addSubview:typeLabel
	set popup to current application's NSPopUpButton's alloc()'s initWithFrame:(current application's NSMakeRect(0, 282, 120, 26)) pullsDown:false
	popup's addItemsWithTitles:{"http", "tcp"}
	popup's selectItemWithTitle:kind
	view's addSubview:popup

	set portF to my field(view, 240, "` + labelPort + `", portV)
	set subF to my field(view, 200, "` + labelSub + `", subV)
	set hostF to my field(view, 160, "` + labelHost + `", hostV)
	set authF to my field(view, 120, "` + labelAuth + `", authV)
	set rportF to my field(view, 80, "` + labelRemotePort + `", rportV)
	set nameF to my field(view, 40, "` + labelName + `", nameV)

	set autoBox to current application's NSButton's checkboxWithTitle:"` + labelAutostart + `" target:(missing value) action:(missing value)
	autoBox's setFrame:(current application's NSMakeRect(0, 8, 340, 20))
	autoBox's setState:(autoV as integer)
	view's addSubview:autoBox

	alert's setAccessoryView:view
	alert's |window|()'s setInitialFirstResponder:portF
	set response to alert's runModal()
	if response is not (current application's NSAlertFirstButtonReturn) then return "cancel"

	set parts to {popup's titleOfSelectedItem() as text, portF's stringValue() as text, subF's stringValue() as text, hostF's stringValue() as text, authF's stringValue() as text, rportF's stringValue() as text, nameF's stringValue() as text, (autoBox's state() as integer) as text}
	set {TID, AppleScript's text item delimiters} to {AppleScript's text item delimiters, tab}
	set joined to parts as text
	set AppleScript's text item delimiters to TID
	return joined
end run
`

// askStepByStep asks the essentials one dialog at a time. It is the fallback
// where no multi-field dialog exists; the rest can be set in the config.
func askStepByStep(prev formData) (formData, bool, error) {
	kind := prev.Type
	if kind == "" {
		kind = "http"
	}
	var ok bool
	var err error
	if kind, ok, err = promptChoice("New tunnel", labelType, []string{"http", "tcp"}, kind); err != nil || !ok {
		return formData{}, false, err
	}
	port, ok, err := promptLine("New tunnel", labelPort, prev.Port)
	if err != nil || !ok {
		return formData{}, false, err
	}
	sub, ok, err := promptLine("New tunnel", labelSub, prev.Sub)
	if err != nil || !ok {
		return formData{}, false, err
	}
	return formData{Type: kind, Port: strings.TrimSpace(port), Sub: strings.TrimSpace(sub)}, true, nil
}

// promptLine asks for one line of text. ok is false when the user cancelled.
func promptLine(title, message, initial string) (text string, ok bool, err error) {
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`set r to display dialog %s default answer %s with title %s
text returned of r`, appleString(message), appleString(initial), appleString(title))
		out, runErr := exec.Command("osascript", "-e", script).Output()
		return dialogResult(out, runErr)
	default:
		if _, lookErr := exec.LookPath("kdialog"); lookErr == nil {
			out, runErr := exec.Command("kdialog", "--title", title, "--inputbox", message, initial).Output()
			return dialogResult(out, runErr)
		}
		out, runErr := exec.Command("zenity", "--entry", "--title", title, "--text", message, "--entry-text", initial).Output()
		return dialogResult(out, runErr)
	}
}

// promptChoice asks the user to pick one of a few values.
func promptChoice(title, message string, choices []string, initial string) (string, bool, error) {
	switch runtime.GOOS {
	case "darwin":
		quoted := make([]string, len(choices))
		for i, c := range choices {
			quoted[i] = appleString(c)
		}
		script := fmt.Sprintf(`set r to choose from list {%s} with title %s with prompt %s default items {%s}
if r is false then error number -128
item 1 of r`, strings.Join(quoted, ", "), appleString(title), appleString(message), appleString(initial))
		out, runErr := exec.Command("osascript", "-e", script).Output()
		return dialogResult(out, runErr)
	default:
		if _, lookErr := exec.LookPath("kdialog"); lookErr == nil {
			args := append([]string{"--title", title, "--combobox", message}, choices...)
			args = append(args, "--default", initial)
			out, runErr := exec.Command("kdialog", args...).Output()
			return dialogResult(out, runErr)
		}
		args := []string{"--list", "--title", title, "--text", message, "--column", message}
		args = append(args, choices...)
		out, runErr := exec.Command("zenity", args...).Output()
		return dialogResult(out, runErr)
	}
}

// dialogResult interprets a dialog tool's output: exit status 1 is Cancel.
func dialogResult(out []byte, err error) (string, bool, error) {
	if err != nil {
		if isCancel(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.TrimRight(string(out), "\n"), true, nil
}

// isCancel reports whether a dialog tool exited because the user cancelled.
// zenity, kdialog and osascript all use exit status 1 for that.
func isCancel(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 1
}
