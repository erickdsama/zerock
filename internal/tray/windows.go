package tray

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unicode/utf16"
)

// On Windows the desktop helpers are PowerShell, which every supported Windows
// ships with, and its WinForms bindings for anything with a window. Scripts go
// in as -EncodedCommand and values as environment variables, so there is no
// quoting to get wrong.

// powershell prepares a hidden PowerShell process running script, with the
// given environment variables available to it.
func powershell(script string, env map[string]string) *exec.Cmd {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-WindowStyle", "Hidden", "-EncodedCommand", encodePowerShell(script))
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	hideConsole(cmd)
	return cmd
}

// encodePowerShell renders a script the way -EncodedCommand wants it: UTF-16LE,
// base64.
func encodePowerShell(script string) string {
	codes := utf16.Encode([]rune(script))
	raw := make([]byte, 0, len(codes)*2)
	for _, c := range codes {
		raw = append(raw, byte(c), byte(c>>8))
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// askWinForm shows the new-tunnel form as a WinForms dialog.
func askWinForm(prev formData) (formData, bool, error) {
	auto := "0"
	if prev.Autostart {
		auto = "1"
	}
	kind := prev.Type
	if kind == "" {
		kind = "http"
	}
	cmd := powershell(winFormScript, map[string]string{
		"ZK_KIND": kind, "ZK_PORT": prev.Port, "ZK_SUB": prev.Sub, "ZK_HOST": prev.Host,
		"ZK_AUTH": prev.Auth, "ZK_RPORT": prev.RemotePort, "ZK_NAME": prev.Name, "ZK_AUTO": auto,
		"ZK_L_TYPE": labelType, "ZK_L_PORT": labelPort, "ZK_L_SUB": labelSub, "ZK_L_HOST": labelHost,
		"ZK_L_AUTH": labelAuth, "ZK_L_RPORT": labelRemotePort, "ZK_L_NAME": labelName, "ZK_L_AUTO": labelAutostart,
	})
	out, err := cmd.Output()
	if err != nil {
		return formData{}, false, err
	}
	line := strings.TrimRight(string(out), "\r\n")
	if line == "cancel" {
		return formData{}, false, nil
	}
	fields := strings.Split(line, "\t")
	if len(fields) != 8 {
		return formData{}, false, fmt.Errorf("unexpected powershell output: %q", line)
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

// winFormScript lays the fields out top to bottom. It reads its values and
// labels from ZK_* environment variables and prints the eight values
// tab-separated, or "cancel".
const winFormScript = `
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
[System.Windows.Forms.Application]::EnableVisualStyles()

$form = New-Object System.Windows.Forms.Form
$form.Text = 'New tunnel'
$form.FormBorderStyle = 'FixedDialog'
$form.StartPosition = 'CenterScreen'
$form.MaximizeBox = $false
$form.MinimizeBox = $false
$form.TopMost = $true
$form.Font = New-Object System.Drawing.Font('Segoe UI', 9)
$script:y = 16

function Add-Label($text) {
  $l = New-Object System.Windows.Forms.Label
  $l.Text = $text
  $l.AutoSize = $true
  $l.ForeColor = [System.Drawing.SystemColors]::GrayText
  $l.Location = New-Object System.Drawing.Point(16, $script:y)
  [void]$form.Controls.Add($l)
  $script:y += 18
}
function Add-Field($text, $value) {
  Add-Label $text
  $t = New-Object System.Windows.Forms.TextBox
  $t.Text = $value
  $t.Location = New-Object System.Drawing.Point(16, $script:y)
  $t.Width = 348
  [void]$form.Controls.Add($t)
  $script:y += 34
  return $t
}

Add-Label $env:ZK_L_TYPE
$kind = New-Object System.Windows.Forms.ComboBox
$kind.DropDownStyle = 'DropDownList'
[void]$kind.Items.AddRange(@('http', 'tcp'))
$kind.SelectedItem = $env:ZK_KIND
if ($kind.SelectedIndex -lt 0) { $kind.SelectedIndex = 0 }
$kind.Location = New-Object System.Drawing.Point(16, $script:y)
$kind.Width = 120
[void]$form.Controls.Add($kind)
$script:y += 34

$port  = Add-Field $env:ZK_L_PORT  $env:ZK_PORT
$sub   = Add-Field $env:ZK_L_SUB   $env:ZK_SUB
$lhost = Add-Field $env:ZK_L_HOST  $env:ZK_HOST
$auth  = Add-Field $env:ZK_L_AUTH  $env:ZK_AUTH
$rport = Add-Field $env:ZK_L_RPORT $env:ZK_RPORT
$name  = Add-Field $env:ZK_L_NAME  $env:ZK_NAME

$auto = New-Object System.Windows.Forms.CheckBox
$auto.Text = $env:ZK_L_AUTO
$auto.AutoSize = $true
$auto.Checked = ($env:ZK_AUTO -eq '1')
$auto.Location = New-Object System.Drawing.Point(16, $script:y)
[void]$form.Controls.Add($auto)
$script:y += 36

$ok = New-Object System.Windows.Forms.Button
$ok.Text = 'Open tunnel'
$ok.DialogResult = [System.Windows.Forms.DialogResult]::OK
$ok.Location = New-Object System.Drawing.Point(264, $script:y)
$ok.Width = 100
$cancel = New-Object System.Windows.Forms.Button
$cancel.Text = 'Cancel'
$cancel.DialogResult = [System.Windows.Forms.DialogResult]::Cancel
$cancel.Location = New-Object System.Drawing.Point(158, $script:y)
$cancel.Width = 100
[void]$form.Controls.Add($ok)
[void]$form.Controls.Add($cancel)
$form.AcceptButton = $ok
$form.CancelButton = $cancel
$form.ClientSize = New-Object System.Drawing.Size(380, ($script:y + 44))
$form.Add_Shown({ $port.Focus() })

if ($form.ShowDialog() -ne [System.Windows.Forms.DialogResult]::OK) { Write-Output 'cancel'; exit 0 }
$autoV = if ($auto.Checked) { '1' } else { '0' }
Write-Output (@($kind.SelectedItem, $port.Text, $sub.Text, $lhost.Text, $auth.Text, $rport.Text, $name.Text, $autoV) -join "` + "`t" + `")
`

// winAlertScript shows an error box with the title and message from the
// environment.
const winAlertScript = `
Add-Type -AssemblyName System.Windows.Forms
[void][System.Windows.Forms.MessageBox]::Show($env:ZK_MSG, $env:ZK_TITLE, 'OK', 'Error')
`

// winCopyScript puts stdin on the clipboard.
const winCopyScript = `$text = [Console]::In.ReadToEnd(); Set-Clipboard -Value $text`
