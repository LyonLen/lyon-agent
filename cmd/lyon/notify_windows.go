//go:build windows

package main

import (
	"os"
	"os/exec"
	"strings"
	"sync"
)

// Windows-native notifications: people running agents are not watching the
// transcript, so the two moments that matter — a run finished, or a tool is
// waiting for approval — surface as system UI instead of TUI lines.
//
//   - notifyDone: a toast (registry AppUserModelID + ToastNotificationManager
//     via PowerShell). Fire-and-forget: the TUI event loop never waits.
//   - notifyDecision: a modal MessageBox — an approval is modal anyway, and
//     a window is harder to miss than a status line. The answer itself
//     still happens in the TUI (keys stay bound there); the box only says
//     "look at me".
//
// Both are no-ops when LYON_NOTIFY=off (or empty LYON_NOTIFY_UNLESS... see
// notifyEnabled) or when PowerShell is unavailable.

const notifyAppID = "Lyon.Agent"

var (
	notifyMu    sync.Mutex
	notifyToast bool // last toast attempt succeeded (registry + API probe)
)

// notifyEnabled gates every system notification.
func notifyEnabled() bool {
	switch strings.ToLower(os.Getenv("LYON_NOTIFY")) {
	case "off", "0", "false", "no":
		return false
	}
	return true
}

// notifyDone pops a toast: the run ended (ok or not). Title/body stay short
// — a notification is a glance, not a report.
func notifyDone(title, body string) {
	if !notifyEnabled() {
		return
	}
	go func() {
		if ensureToastRegistered() {
			if err := toast(title, body); err == nil {
				return
			}
		}
		// Toast unavailable (old PowerShell, missing WinRT): fall back to a
		// message box so the signal is never silently lost.
		messageBox(title, body)
	}()
}

// notifyDecision raises a MessageBox for a waiting approval.
func notifyDecision(title, body string) {
	if !notifyEnabled() {
		return
	}
	go messageBox(title, body)
}

// ensureToastRegistered creates a per-user AppUserModelID the toast API
// accepts. Cheap enough to run per call; sync.Once would pin a false
// negative for the whole process if the first probe failed transiently.
func ensureToastRegistered() bool {
	notifyMu.Lock()
	defer notifyMu.Unlock()
	// Probe: if a toast with this ID worked before, skip the registry work.
	if notifyToast {
		return true
	}
	ps := `New-Item -Path 'HKCU:\Software\Classes\AppUserModelID\' -Name '` + notifyAppID + `' -Force | Out-Null; ` +
		`New-ItemProperty -Path 'HKCU:\Software\Classes\AppUserModelID\' + '` + notifyAppID + `' -Name DisplayName -Value 'Lyon Agent' -PropertyType String -Force | Out-Null`
	if err := runPowerShell(ps); err != nil {
		return false
	}
	notifyToast = true
	return true
}

// toast renders a Windows 10/11 toast through the WinRT notification API.
func toast(title, body string) error {
	// XML-escape: titles contain CJK and bodies may carry & < >.
	esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	xml := `<toast><visual><binding template="ToastGeneric"><text>` +
		esc.Replace(title) + `</text><text>` + esc.Replace(body) + `</text></binding></visual></toast>`
	ps := `
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, Windows.Foundation, ContentType = WindowsRuntime] | Out-Null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] | Out-Null
$tmpl = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02)
$x = New-Object Windows.Data.Xml.Dom.XmlDocument
$x.LoadXml('` + strings.ReplaceAll(xml, "'", `\'`) + `')
$n = [Windows.UI.Notifications.ToastNotification]::new($x)
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('` + notifyAppID + `').Show($n)
`
	return runPowerShell(ps)
}

// messageBox shows a modal OK box (System.Windows.Forms — always present).
func messageBox(title, body string) {
	esc := strings.NewReplacer("'", "''") // single-quoted PowerShell strings
	ps := `Add-Type -AssemblyName System.Windows.Forms; ` +
		`[System.Windows.Forms.MessageBox]::Show('` + esc.Replace(body) + `','` + esc.Replace(title) + `','OK','Information') | Out-Null`
	_ = runPowerShell(ps) // best effort: box missing beats a wedged TUI
}

func runPowerShell(script string) error {
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-STA", "-Command", script)
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Run()
}

// truncNotif shortens free text for a one-line notification.
func truncNotif(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
