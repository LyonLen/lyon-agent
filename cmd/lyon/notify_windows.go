//go:build windows

package main

import (
	"os"
	"os/exec"
	"strings"
	"sync"
)

// Windows notifications: people running agents are not watching the
// transcript, so the two moments that matter — a run finished, or a tool is
// waiting for approval — surface as a system toast in the bottom-right
// notification corner. No modal dialogs: an agent should never steal focus
// from whatever the user is doing (feedback: "要系统右下角那种，不要老式弹窗").
//
//   - notifyDone: standard toast, fire-and-forget — the TUI event loop
//     never waits on PowerShell.
//   - notifyDecision: same toast with duration="long" so an approval has
//     ~25s to be noticed. The answer itself still happens in the TUI
//     (keys stay bound there); the toast only says "look at me".
//
// Both are silent no-ops when LYON_NOTIFY=off/0/false/no, when the toast
// infrastructure is unavailable, or on non-Windows (notify_other.go).

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

// notifyDone pops a standard toast: the run ended (ok or not). Title/body
// stay short — a notification is a glance, not a report.
func notifyDone(title, body string) {
	fireToast(title, body, false)
}

// notifyDecision raises a long-lived toast for a waiting approval.
func notifyDecision(title, body string) {
	fireToast(title, body, true)
}

func fireToast(title, body string, long bool) {
	if !notifyEnabled() {
		return
	}
	go func() {
		if !ensureToastRegistered() {
			return // toast infra missing: stay silent, never block the TUI
		}
		_ = toast(title, body, long)
	}()
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

// toast renders a Windows 10/11 toast through the WinRT notification API —
// this is the bottom-right corner bubble, not a dialog. duration="long"
// keeps approval toasts around longer than the default ~7s.
func toast(title, body string, long bool) error {
	// XML-escape: titles contain CJK and bodies may carry & < >.
	esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	dur := ""
	if long {
		dur = ` duration="long"`
	}
	xml := `<toast` + dur + `><visual><binding template="ToastGeneric"><text>` +
		esc.Replace(title) + `</text><text>` + esc.Replace(body) + `</text></binding></visual></toast>`
	ps := `
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, Windows.Foundation, ContentType = WindowsRuntime] | Out-Null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] | Out-Null
$x = New-Object Windows.Data.Xml.Dom.XmlDocument
$x.LoadXml('` + strings.ReplaceAll(xml, "'", `\'`) + `')
$n = [Windows.UI.Notifications.ToastNotification]::new($x)
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('` + notifyAppID + `').Show($n)
`
	return runPowerShell(ps)
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
