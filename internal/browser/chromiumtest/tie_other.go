//go:build !linux

package chromiumtest

import "os/exec"

// tie does nothing where there is no parent-death signal, macOS among them:
// there the DevTools pipe the test binary holds is what ends a launched
// Chromium with it (pipes).
func tie(*exec.Cmd) {}
