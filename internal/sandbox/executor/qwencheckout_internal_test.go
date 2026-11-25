package executor

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/shellword"
)

// THE BLANK-NAMES LINE IS SAID ONLY WHERE THE HELPER RAN: the line the
// helper prints for a context.fileName of blanks alone says that rules reach
// a checkout cloned during the run, which only the helper's hook hands; so a
// box with no helper build, whose gate says nothing is handed, keeps no such
// line in its skip log, and one with the helper keeps it once.
//
// Mutation: append the line whatever the helper did, or never, and a row
// goes red.
func TestTheBlankNamesLineIsSaidOnlyWhereTheHelperRan(t *testing.T) {
	t.Parallel()
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	const line = "crewlet: a settings file sets context.fileName to blank names only"
	for name, helped := range map[string]bool{"with the helper": true, "without it": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := Paths{Home: t.TempDir()}
			h := qwenCheckoutHome(p)
			for _, dir := range []string{p.WorkDir(), filepath.Dir(h.helper), h.skills} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(p.QwenContextHomes(), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if helped {
				if err := os.WriteFile(h.helper, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // a stand-in the gate runs
					t.Fatal(err)
				}
			}
			script := "qn=" + shellword.Quote(line) + "\n" + h.gate() + qwenBlankNamesSaid(h)
			cmd := exec.Command(sh, "-c", script) //nolint:gosec // the script under test
			cmd.Dir = p.Home
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("the gate failed: %v\n%s", err, stderr.String())
			}
			logged, _ := os.ReadFile(h.skipped()) //nolint:gosec // the case's own dir
			if n := strings.Count(string(logged), line); n != map[bool]int{true: 1, false: 0}[helped] {
				t.Errorf("the skip log holds the line %d times, helper ran %v:\n%s", n, helped, logged)
			}
		})
	}
}
