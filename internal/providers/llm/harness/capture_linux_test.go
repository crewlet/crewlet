package harness

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// A CREDENTIAL CAPTURE IS IN MEMORY, on linux: a child handed or printing a
// credential — a serve lifecycle's server with the password it mints, the
// token-minting command, the credential store's import and exports — writes
// into an anonymous regular file with no name and no block on any disk. The
// directory it would have been made in holds nothing, and no file was ever
// opened there.
//
// Mutation: ignore credential, and the capture is a file in the directory.
func TestACredentialCaptureIsInMemory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hooks := &captureHooks{betweenOpens: func(path string) {
		t.Errorf("a credential capture was opened by name as %s", path)
	}}
	c, w, err := newCapture(dir, testSetting, "stdout", true, streamSpec{kind: oneAnswer}, hooks)
	if err != nil {
		t.Fatalf("newCapture: %v", err)
	}
	defer c.r.Close()
	defer w.Close()
	if info, err := c.r.Stat(); err != nil || !info.Mode().IsRegular() {
		t.Errorf("the capture is %v (%v), want a regular file every runtime writes as one", info, err)
	}
	target, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", c.r.Fd()))
	if err != nil || !strings.HasPrefix(target, "/memfd:") {
		t.Errorf("the capture is %q (%v), want a file in memory", target, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the credential capture's directory holds %d files, want none", len(entries))
	}
}
