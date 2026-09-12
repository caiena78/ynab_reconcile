package main

import (
	"os/exec"
	"runtime"
)

// openBrowser launches the given URL in the OS default browser. Errors are
// non-fatal — the server keeps running even if this fails (e.g. headless).
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
