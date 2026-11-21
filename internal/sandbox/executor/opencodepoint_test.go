package executor_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// pointStandIn is a stand-in `opencode` for the read of a box's point: it
// logs each call's arguments in order, keeps the environment and working
// directory config.get was asked in, answers `--version` as 2.3.0, and
// answers model.list and config.get with the files the case wrote into the
// box's home, but cut short where its output is a pipe, as 2.0.32 and 2.1.4
// cut theirs at 64 KiB.
const pointStandIn = `#!/bin/sh
printf '%s\n' "$*" >> "$HOME/opencode-calls"
answer() { if [ -p /dev/stdout ]; then printf '{"cut'; else cat "$HOME/$1"; fi; }
case "$*" in
--version) echo 'opencode v2.3.0' ;;
'auth import --standalone') cat > /dev/null ;;
'api --standalone model.list') answer models.json ;;
'api --standalone config.get') env > "$HOME/opencode-env"; pwd > "$HOME/opencode-cwd"; answer config.json ;;
*) exit 3 ;;
esac
`

// pointBox is a real box with the OpenCode runner's plumbing, the stand-in on
// its PATH, a login document, and a catalog and documents whose point is
// 210,000 for stub/m: its 250,000-token input limit, which a document states,
// less the 40,000 its buffer holds back.
func pointBox(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
	t.Helper()
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	box, err := local.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { box.Close(t.Context()) })
	runner := executor.NewOpenCode()
	if err = runner.Install(t.Context(), box); err != nil {
		t.Fatalf("Install: %v", err)
	}
	paths := executor.PathsFor(box)
	writeBoxStandIn(t, box, paths.BinDir()+"/opencode", pointStandIn)
	for path, content := range map[string]string{
		paths.CredentialDocument(): `[{"provider":"stub","key":"a login"}]`,
		box.Home() + "/models.json": `{"location":"/c","data":[{"id":"m","providerID":"stub",` +
			`"limit":{"context":300000,"input":250000},"time":{"released":0}}]}`,
		box.Home() + "/config.json": `[{"type":"document","path":"/c/opencode.json","info":` +
			`{"compaction":{"buffer":40000},"providers":{"stub":{"models":{"m":{"limit":{"context":300000}}}}}}}]`,
	} {
		if err = box.WriteFile(t.Context(), path, []byte(content)); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
	return runner, box
}

// THE POINT IS READ IN THE CONTINUATION'S BOX. One exec in the box: the login
// the box holds imported first, as the run's own script imports it before its
// server reads a provider's credential; the release asked where the launch
// has none; model.list and config.get answered into files, never through a
// pipe, which cuts OpenCode's output; the point decided from them. Nothing
// it was answered is left in the box.
//
// Mutation: answer model.list through a pipe, skip the login's import, or
// leave the answers in the box, and this goes red.
func TestThePointIsReadInTheContinuationsBox(t *testing.T) {
	t.Parallel()
	runner, box := pointBox(t)
	point, err := runner.ReadOpenCodePoint(t.Context(), box, sandbox.PointAsk{Model: "stub/m", Writer: "2.3.0"})
	if err != nil {
		t.Fatalf("ReadOpenCodePoint: %v", err)
	}
	if point.Mark != "" || point.Release != "2.3.0" || point.Tokens != 210000 {
		t.Fatalf("the box's point reads %+v, want 210,000 on 2.3.0", point)
	}
	calls := strings.Split(strings.TrimSpace(boxFile(t, box, box.Home()+"/opencode-calls")), "\n")
	want := []string{"auth import --standalone", "--version", "api --standalone model.list",
		"api --standalone config.get"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Errorf("the box was asked %q, want %q", calls, want)
	}
	left, err := box.Exec(t.Context(), "ls -A "+executor.PathsFor(box).WorkDir()+"/opencode-point 2>/dev/null",
		sandbox.ExecOptions{})
	if err != nil || strings.TrimSpace(left.Stdout) != "" {
		t.Errorf("the read left %q in the box (%v), want nothing", left.Stdout, err)
	}
}

// THE READ SEES THE BOX'S DOCUMENTS, NOT THE IMAGE'S. The run's script hands
// its server the engine's configuration in place of whatever
// OPENCODE_CONFIG_CONTENT the box's environment carries, so the read runs
// without it, under the run's own environment otherwise (a provider's key the
// run signs in by among it), and in the run's working directory, the box's
// own, where the run's server discovers its documents.
//
// Mutation: keep the environment's OPENCODE_CONFIG_CONTENT, or drop the run's
// environment, and this goes red.
func TestTheReadSeesTheBoxsDocumentsNotTheImages(t *testing.T) {
	t.Parallel()
	runner, box := pointBox(t)
	_, err := runner.ReadOpenCodePoint(t.Context(), box, sandbox.PointAsk{Model: "stub/m", Release: "2.3.0",
		Writer: "2.3.0", Env: map[string]string{"OPENCODE_CONFIG_CONTENT": `{"image":true}`,
			"STUB_API_KEY": "the run's key"}})
	if err != nil {
		t.Fatalf("ReadOpenCodePoint: %v", err)
	}
	env := boxFile(t, box, box.Home()+"/opencode-env")
	if strings.Contains(env, "OPENCODE_CONFIG_CONTENT=") || !strings.Contains(env, "STUB_API_KEY=the run's key") {
		t.Errorf("config.get was asked under %q, want the run's key and no OPENCODE_CONFIG_CONTENT", env)
	}
	run, err := box.Exec(t.Context(), "pwd", sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("pwd: %v", err)
	}
	if cwd := strings.TrimSpace(boxFile(t, box, box.Home()+"/opencode-cwd")); cwd != strings.TrimSpace(run.Stdout) {
		t.Errorf("config.get was asked in %q, want the run's own %q", cwd, run.Stdout)
	}
}
