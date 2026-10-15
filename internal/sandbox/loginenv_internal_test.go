//go:build unix

package sandbox

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A PROFILE THAT FIGHTS THE RUN ENVIRONMENT: what an image's /etc/profile.d
// script, an E2B template's, or a ~/.profile a previous run left in a reused
// box can export. It also adds to PATH and exports a variable of its own, both
// of which a login shell exists to keep — so a case that finds neither has not
// run the profile at all, and cannot pass by never having been challenged.
const fightingProfile = `export OTEL_LOG_USER_PROMPTS=true
export GITHUB_TOKEN=from-the-profile
export TRICKY=from-the-profile
export HOME=/profile-moved-home
export PATH="/from-the-profile:$PATH"
export PROFILE_ONLY=kept
`

// report is a command that prints what a run sees, one variable a line, and
// whether any stash reached it.
const report = `printf 'OTEL_LOG_USER_PROMPTS=%s\nGITHUB_TOKEN=%s\nTRICKY=%s\nHOME=%s\nPATH=%s\nPROFILE_ONLY=%s\nSTASHES=%s\n' ` +
	`"$OTEL_LOG_USER_PROMPTS" "$GITHUB_TOKEN" "$TRICKY" "$HOME" "$PATH" "$PROFILE_ONLY" ` +
	`"$(env | grep -c '^` + runEnvStash + `')"`

// tricky is a value a shell would mangle if it were ever evaluated rather than
// copied: quotes of both kinds, a substitution, a backtick and a trailing
// space.
const tricky = `a'b"c $(echo pwned) ` + "`id`" + ` `

// seen parses report's output.
func seen(t *testing.T, out string) map[string]string {
	t.Helper()
	got := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		name, value, ok := strings.Cut(sc.Text(), "=")
		if ok {
			got[name] = value
		}
	}
	if len(got) != 7 {
		t.Fatalf("the command did not report: %q", out)
	}
	return got
}

// wantRunEnvOverProfile is what every case requires of what a run saw.
func wantRunEnvOverProfile(t *testing.T, got map[string]string, home string) {
	t.Helper()
	for name, want := range map[string]string{
		"OTEL_LOG_USER_PROMPTS": "false",
		"GITHUB_TOKEN":          "declared-by-config",
		"TRICKY":                tricky,
		"HOME":                  home,
		"PROFILE_ONLY":          "kept",
		"STASHES":               "0",
	} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
	// THE PROFILE'S PATH, which the run sets none of: finding a CLI a
	// version manager installed is what the login shell is for.
	if !strings.HasPrefix(got["PATH"], "/from-the-profile:") {
		t.Errorf("PATH = %q, want the profile's", got["PATH"])
	}
}

// A LOGIN SHELL'S PROFILE CANNOT REPLACE THE RUN ENVIRONMENT IN A DIRECT BOX,
// for a command or for a background job, whatever the value holds; and it still
// sets everything the run does not.
//
// Mutation: start the login shell with the run environment as it is and no
// script putting it back, and every run-env variable reads the profile's.
func TestADirectBoxsLoginShellKeepsTheRunEnvironmentOverItsProfile(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{Env: map[string]string{
		"GITHUB_TOKEN": "declared-by-config", "TRICKY": tricky,
	}})
	if err := box.WriteFile(t.Context(), filepath.Join(box.Home(), ".profile"), []byte(fightingProfile)); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"OTEL_LOG_USER_PROMPTS": "false"}

	result, err := box.Exec(t.Context(), report, ExecOptions{Env: env, Login: true})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("Exec: %v, %+v", err, result)
	}
	wantRunEnvOverProfile(t, seen(t, result.Stdout), box.Home())

	// THE BACKGROUND JOB, which is where the coding agent runs.
	out := filepath.Join(box.Home(), "report.txt")
	if _, err := box.StartBackground(t.Context(), "sh -c '"+strings.ReplaceAll(report, "'", `'\''`)+
		" > "+out+"'", ExecOptions{Env: env, Login: true}); err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	var blob []byte
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if blob, _ = os.ReadFile(out); strings.Contains(string(blob), "STASHES=") {
			break
		}
	}
	wantRunEnvOverProfile(t, seen(t, string(blob)), box.Home())

	// AND A COMMAND THAT ASKED FOR NO LOGIN SHELL SOURCES NO PROFILE.
	plain, err := box.Exec(t.Context(), `printf '%s' "$PROFILE_ONLY"`, ExecOptions{Env: env})
	if err != nil || plain.Stdout != "" {
		t.Errorf("a plain command ran the profile: %q, %v", plain.Stdout, err)
	}
}

// A CONTAINER'S LOGIN SHELL KEEPS THE RUN ENVIRONMENT OVER ITS PROFILE, and
// carries what it puts back in the env file rather than on the command line.
//
// The runtime is not run: the exec's own shell line is, under a real login
// shell, with the environment the env file holds — which is exactly what
// `docker exec --env-file` starts that line with.
//
// Mutation: drop the script that puts the environment back and the profile
// wins; put a value in the script and the argv check goes red.
func TestAContainersLoginShellKeepsTheRunEnvironmentOverItsProfile(t *testing.T) {
	t.Parallel()
	box := &containerBox{
		layout:    boxLayout{id: "box", root: t.TempDir()},
		runtime:   "/usr/bin/docker",
		container: "crewlet-sbx-box",
		env:       map[string]string{"GITHUB_TOKEN": "declared-by-config", "TRICKY": tricky},
	}
	argv, err := box.execArgv(report, ExecOptions{
		Env: map[string]string{"OTEL_LOG_USER_PROMPTS": "false"}, Login: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	shell := argv[slices.Index(argv, box.container)+1:]
	if len(shell) != 4 || shell[0] != "/bin/sh" || shell[1] != "-l" || shell[2] != "-c" {
		t.Fatalf("the container runs %q, want a login shell", shell)
	}
	if joined := strings.Join(argv, " "); strings.Contains(joined, "declared-by-config") {
		t.Fatalf("a value reached the command line: %s", joined)
	}
	blob, err := os.ReadFile(box.layout.envFile())
	if err != nil {
		t.Fatal(err)
	}
	// The shell inside the container starts in the mounted home, where
	// its profile is; here that is a directory of the test's.
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".profile"), []byte(fightingProfile), 0o600); err != nil {
		t.Fatal(err)
	}
	var start []string
	for _, line := range strings.Split(strings.TrimSuffix(string(blob), "\n"), "\n") {
		if !strings.HasPrefix(line, "HOME=") {
			start = append(start, line)
		}
	}
	got := runShell(t, shell[0], shell[1:], append(start, "HOME="+home))
	wantRunEnvOverProfile(t, seen(t, got), DefaultHome)
}

// runShell runs a shell line on this host with exactly env, and its stdout.
func runShell(t *testing.T, shell string, args, env []string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), shell, args...)
	cmd.Env = append(env, "PATH=/usr/bin:/bin")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %q: %v", shell, args, err)
	}
	return string(out)
}

// THE BOX'S STASH PREFIX IS ITS OWN, and a name that is not a shell's is
// passed through rather than put back — a profile cannot assign one either.
//
// Mutation: stash a run variable named with the prefix and its own value is
// replaced by the stash of another; stash a non-shell name and the script
// fails on it.
func TestTheLoginScriptRefusesItsOwnPrefixAndLeavesOtherNamesAlone(t *testing.T) {
	t.Parallel()
	if _, _, err := loginScript("true", map[string]string{runEnvStash + "X": "1"}); err == nil ||
		!strings.Contains(err.Error(), runEnvStash) {
		t.Errorf("a run variable named with the stash prefix was accepted: %v", err)
	}
	script, start, err := loginScript("true", map[string]string{"a.b": "dotted", "OK": "v"})
	if err != nil {
		t.Fatal(err)
	}
	if start["a.b"] != "dotted" || start[runEnvStash+"a.b"] != "" || start[runEnvStash+"OK"] != "v" {
		t.Errorf("the start environment is %v", start)
	}
	if strings.Contains(script, "a.b") || strings.Contains(script, `"v"`) {
		t.Errorf("the script names %q", script)
	}
	if script, _, _ := loginScript("true", nil); script != "true" {
		t.Errorf("an empty run environment wrapped the command as %q", script)
	}
}
