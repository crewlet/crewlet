//go:build unix

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// procStatus is a /proc/<pid>/status of a process in state, its ids those
// given — real, effective, saved and file system — as the kernel prints them.
func procStatus(state, ids string) string {
	return "Name:\tharness\nUmask:\t0022\nState:\t" + state + "\nTgid:\t4242\nPid:\t4242\nPPid:\t1\n" +
		"Uid:\t" + strings.ReplaceAll(ids, " ", "\t") + "\nGid:\t1000\t1000\t1000\t1000\n"
}

// THE KILL OF AN E2B JOB WAITS ONLY ON ITS USER'S LIVING PROCESSES. In a VM,
// kill(-1) answers success wherever it counts a process, root's it is refused
// included, so the kill waits on the box user's processes read off /proc
// instead: one the kill reaches — its real or its saved user id the sender's,
// as the kernel lets a sender signal — that has not exited, and that is not the
// waiting shell itself. A zombie, another user's process and the shell are
// nothing to wait for, and the wait ends as soon as only they are left.
//
// Mutations: count the shell itself, or a zombie, and the wait never ends
// early; read the effective id where the saved one is meant, and a process
// the kill reaches through its saved id is not waited for.
func TestAnE2BKillWaitsOnlyOnItsUsersLivingProcesses(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		status string
		live   bool
	}{
		"nothing but the waiting shell":    {},
		"another user's process":           {status: procStatus("S (sleeping)", "0 0 0 0")},
		"a zombie of the user's":           {status: procStatus("Z (zombie)", "1000 1000 1000 1000")},
		"a living process of the user's":   {status: procStatus("S (sleeping)", "1000 1000 1000 1000"), live: true},
		"one whose saved id is the user's": {status: procStatus("S (sleeping)", "0 0 1000 0"), live: true},
		"one only its effective id names":  {status: procStatus("S (sleeping)", "0 1000 0 0")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if tc.status != "" {
				if err := os.MkdirAll(filepath.Join(root, "4242"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "4242", "status"), []byte(tc.status), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// THE WAITING SHELL'S OWN RECORD, the user's and living, which the
			// walk must pass over.
			script := "u=1000\n" + userLive(root) +
				`mkdir -p "$ROOT/$$" && printf '` + strings.ReplaceAll(procStatus("R (running)",
				"1000 1000 1000 1000"), "\t", `\t`) + `' > "$ROOT/$$/status"` + "\n" +
				"if userlive; then echo live; else echo none; fi\n"
			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", script)
			cmd.Env = []string{"ROOT=" + root, "PATH=" + os.Getenv("PATH")}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("the walk failed: %v\n%s", err, out)
			}
			want := "none"
			if tc.live {
				want = "live"
			}
			if got := strings.TrimSpace(string(out)); got != want {
				t.Fatalf("the walk answered %q, want %q", got, want)
			}
		})
	}
}
