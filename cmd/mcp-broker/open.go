package main

import (
	"os/exec"
	"runtime"
)

// openBrowser opens url in the default browser.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		// rundll32 avoids cmd.exe, which would interpret & in the URL.
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	}
	return exec.Command("xdg-open", url).Start()
}
