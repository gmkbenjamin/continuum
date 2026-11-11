// Package notify shows a desktop notification on macOS, Linux and Windows.
// Failures are ignored: notifications are a courtesy, never a dependency.
package notify

import (
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
				`$x = $t.GetElementsByTagName('text'); $x.Item(0).AppendChild($t.CreateTextNode('` + psEsc(title) + `')) > $null;` +
				`$x.Item(1).AppendChild($t.CreateTextNode('` + psEsc(body) + `')) > $null;` +
				`[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('Continuum').Show([Windows.UI.Notifications.ToastNotification]::new($t))`
			_, _ = proc.Run(10*time.Second, "", "powershell", "-NoProfile", "-NonInteractive", "-Command", ps)
		}
	}()
}

func esc(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

func psEsc(s string) string { return strings.ReplaceAll(s, `'`, `''`) }
