package boxhelper_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/browser/boxhelper"
)

// THE RELAY'S SCRIPT QUOTES EVERY PATH AND PASSES THE SUPERVISOR'S ENVIRONMENT
// ALONE: run by sh, a home and a wiring path holding spaces, quotes and a
// dollar reach node as the helper's path and the wiring's, word for word,
// after relay --wiring, node exec'd in the script's place; and node starts
// with the names the supervisor's environment lists, set or empty as the
// script has them, and none of the job's own: no NODE_OPTIONS, no NODE_DEBUG,
// no key. A stand-in node, first on PATH, records what it got.
//
// Mutations, each red: leave the wiring path unquoted; hand node the
// script's whole environment; pass a listed name the script has unset.
func TestTheRelaysScriptQuotesEveryPathAndPassesTheSupervisorsEnvironmentAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	got := filepath.Join(dir, "got")
	node := "#!/bin/sh\n{ printf '%s\\n' \"$$\" \"${NODE_OPTIONS-unset}\" \"${NODE_DEBUG-unset}\" " +
		"\"${CREWLET_TEST_KEY-unset}\" \"${LANG-unset}\" \"${TZ-unset}\" \"${PATH-unset}\"; " +
		"printf '%s\\000' \"$@\"; } > '" + got + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "node"), []byte(node), 0o755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, `a home's "own" $HOME`)
	wiring := filepath.Join(home, ".crewlet", `browser relay's $1.json`)
	path := dir + ":/usr/bin:/bin"
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", boxhelper.RelayScript(home, wiring)+"\necho not exec'd")
	cmd.Env = []string{"PATH=" + path, "NODE_OPTIONS=--require=/nowhere.cjs", "NODE_DEBUG=net",
		"CREWLET_TEST_KEY=the-run-key", "LANG="}
	out, err := cmd.Output()
	if err != nil || len(out) != 0 {
		t.Fatalf("the script answered %q (%v), want node exec'd in its place", out, err)
	}
	raw, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitN(string(raw), "\n", 8)
	args := strings.Split(strings.TrimSuffix(lines[7], "\x00"), "\x00")
	want := []string{filepath.Join(boxhelper.Folder(home), "helper.mjs"), "relay", "--wiring", wiring}
	if !slices.Equal(args, want) {
		t.Errorf("node was run with %q, want %q", args, want)
	}
	if env, wantEnv := lines[1:7], []string{"unset", "unset", "unset", "", "unset", path}; !slices.Equal(env, wantEnv) {
		t.Errorf("node started with NODE_OPTIONS, NODE_DEBUG, a key, LANG, TZ and PATH as %q, want %q", env, wantEnv)
	}
}
