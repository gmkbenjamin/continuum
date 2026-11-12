// Package notify shows a desktop notification on macOS, Linux and Windows.
// Failures are ignored: notifications are a courtesy, never a dependency.
package notify

import (
	"encoding/base64"
	"runtime"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/proc"
)

func Send(title, body string) {
	go func() {
		switch runtime.GOOS {
		case "darwin":
			script := `display notification "` + esc(body) + `" with title "` + esc(title) + `"`
			_, _ = proc.Run(5*time.Second, "", "osascript", "-e", script)
		case "linux", "freebsd", "openbsd", "netbsd":
			if p, err := proc.LookPath("notify-send"); err == nil {
				_, _ = proc.Run(5*time.Second, "", p, "--app-name=Continuum", title, body)
			}
		case "windows":
			ps := `[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] > $null;` +
				`$t = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02);` +
				`$x = $t.GetElementsByTagName('text'); $x.Item(0).AppendChild($t.CreateTextNode(` + psText(title) + `)) > $null;` +
				`$x.Item(1).AppendChild($t.CreateTextNode(` + psText(body) + `)) > $null;` +
				`[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('Continuum').Show([Windows.UI.Notifications.ToastNotification]::new($t))`
			_, _ = proc.Run(10*time.Second, "", "powershell", "-NoProfile", "-NonInteractive", "-Command", ps)
		}
	}()
}

func esc(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

// psText is a PowerShell expression for s. The text travels as base64, so no
// quote in it (PowerShell also ends strings at curly quotes) can break out.
func psText(s string) string {
	return `[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('` + base64.StdEncoding.EncodeToString([]byte(s)) + `'))`
}
