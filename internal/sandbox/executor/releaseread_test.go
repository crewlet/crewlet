package executor_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// releaseTeller is a harness a box is asked the release of.
type releaseTeller interface {
	executor.Harness
	VersionCommand() string
}

// releaseReadBox is a real box whose stand-in for h's binary answers
// `--version` with printed, keeping the environment it was asked in at
// [releaseReadEnv]; anything else it is asked exits 0 saying nothing. Its
// profile sets every name h's launch unsets or pins to a value of its own, as an
// image or a seat's profile may.
func releaseReadBox(t *testing.T, h releaseTeller, printed string) (*executor.Runner, sandbox.Sandbox) {
	t.Helper()
	_, box := realBox(t)
	p := executor.PathsFor(box)
	binary, _, _ := strings.Cut(h.VersionCommand(), " ")
	writeBoxStandIn(t, box, p.BinDir()+"/"+binary, "#!/bin/sh\n"+
		`if [ "$1" = --version ]; then env > "$HOME/`+releaseReadEnv+`"; echo `+shellword.Quote(printed)+"; fi\nexit 0\n")
	var profile strings.Builder
	for _, name := range h.Output(p).Unset {
		profile.WriteString("export " + name + "=from-the-profile\n")
	}
	for _, pin := range h.Pins(t.Context(), sandbox.RunRequest{}, p) {
		profile.WriteString("export " + pin.Name + "=from-the-profile\n")
	}
	if err := box.WriteFile(t.Context(), box.Home()+"/.profile", []byte(profile.String())); err != nil {
		t.Fatalf("write the profile: %v", err)
	}
	return executor.New(h), box
}

// releaseReadEnv is the file under the box's home a stand-in keeps the
// environment of its `--version` in ([releaseReadBox]).
const releaseReadEnv = "release-read.env"

// EVERY READ OF A BOX'S RELEASE RUNS UNDER ITS LAUNCH'S SWITCHES, for every
// runner: the carry check's, the copy-continue check's and, for a harness whose
// point its box says, the point read's, asked with no release in hand. Each
// sees every pin the launch sets at the launch's value and none of the names
// the launch unsets, over a profile that sets them all; taken from the
// runner's own Pins and Output, so a switch added to a launch is held here too.
//
// Mutation: run the version command bare in any read (as it ran before), or
// take the pins without the unsets, and that runner's row goes red.
func TestEveryReleaseReadRunsUnderItsLaunchsSwitches(t *testing.T) {
	t.Parallel()
	for _, h := range []releaseTeller{executor.ClaudeCode{}, executor.Codex{}, executor.GeminiCLI{},
		executor.QwenCode{}, executor.OpenCode{}, executor.Copilot{}, executor.Grok{}, executor.Muse{},
		executor.Kimi{}, executor.Hermes{}, executor.Pi{}} {
		// Each read, and whether the runner says it asked the box's release:
		// a copy-continue check of a harness that holds no release to its copy
		// continues it unasked, and a box that says no point is not read.
		reads := map[string]func(*testing.T, *executor.Runner, sandbox.Sandbox) (bool, error){
			"carry check": func(t *testing.T, r *executor.Runner, b sandbox.Sandbox) (bool, error) {
				_, err := r.CheckCarry(t.Context(), b, sandbox.SessionArchive{})
				return true, err
			},
			"copy-continue check": func(t *testing.T, r *executor.Runner, b sandbox.Sandbox) (bool, error) {
				c, err := r.CheckInPlace(t.Context(), b)
				return !c.Continues || c.Why != "" || c.Version != "", err
			},
			"point read": func(t *testing.T, r *executor.Runner, b sandbox.Sandbox) (bool, error) {
				p, err := r.ReadBoxPoint(t.Context(), b, sandbox.PointAsk{Model: "anthropic/claude-sonnet-4-5"})
				return !strings.Contains(p.Why, "compacts at no point its box says"), err
			},
		}
		for read, ask := range reads {
			t.Run(h.Name()+"/"+read, func(t *testing.T) {
				t.Parallel()
				runner, b := releaseReadBox(t, h, "1.0.0")
				asked, err := ask(t, runner, b)
				if err != nil {
					t.Fatalf("the %s: %v", read, err)
				}
				// A box reads a missing file as empty.
				saw := boxFile(t, b, b.Home()+"/"+releaseReadEnv)
				if saw == "" {
					if asked {
						t.Fatalf("the %s asked the box no release", read)
					}
					return
				}
				env := map[string]string{}
				for line := range strings.Lines(saw) {
					if name, value, ok := strings.Cut(strings.TrimSuffix(line, "\n"), "="); ok {
						env[name] = value
					}
				}
				p := executor.PathsFor(b)
				pinned := map[string]bool{}
				for _, pin := range h.Pins(t.Context(), sandbox.RunRequest{}, p) {
					pinned[pin.Name] = true
					if got, ok := env[pin.Name]; !ok || got != pin.Value {
						t.Errorf("the %s ran with %s=%q, want the launch's pin %q", read, pin.Name, got, pin.Value)
					}
				}
				for _, name := range h.Output(p).Unset {
					if got, ok := env[name]; ok && !pinned[name] {
						t.Errorf("the %s ran with %s=%q, which the launch unsets", read, name, got)
					}
				}
			})
		}
	}
}

// A CACHED NEWER COPILOT PACKAGE MOVES NEITHER READ OF A COPILOT BOX'S
// RELEASE. Copilot 1.0.114 runs a newer package from its cache in place of its
// own unless COPILOT_AUTO_UPDATE is `false` (measured); the stand-in does the
// same, with a profile that turns it on. The carry check and the copy-continue
// check each read the release the run runs, 1.0.114, and continue the session
// it wrote, never refusing it as one an older release would continue.
//
// Mutation: drop COPILOT_AUTO_UPDATE from Copilot's Pins, or run either read
// without the launch's pins: the box reads 1.0.116 and refuses the session.
func TestACachedNewerCopilotPackageMovesNeitherRead(t *testing.T) {
	t.Parallel()
	_, b := realBox(t)
	p := executor.PathsFor(b)
	writeBoxStandIn(t, b, p.BinDir()+"/copilot", "#!/bin/sh\n"+
		`[ "$1" = --version ] || exit 1`+"\n"+
		`if [ "$COPILOT_AUTO_UPDATE" = false ]; then echo 'GitHub Copilot CLI 1.0.114.'; `+
		`else echo 'GitHub Copilot CLI 1.0.116.'; fi`+"\n")
	if err := b.WriteFile(t.Context(), b.Home()+"/.profile", []byte("export COPILOT_AUTO_UPDATE=true\n")); err != nil {
		t.Fatalf("write the profile: %v", err)
	}
	record := "id s-1\ncontinued yes\nwhy \nrelease GitHub Copilot CLI 1.0.114.\n"
	if err := b.WriteFile(t.Context(), p.SessionRecord(), []byte(record)); err != nil {
		t.Fatal(err)
	}
	runner := executor.NewCopilot()
	carried, err := runner.CheckCarry(t.Context(), b, sandbox.SessionArchive{Harness: executor.CopilotName,
		SessionID: "s-1", Format: executor.Copilot{}.ArchiveFormat(), Version: "1.0.114"})
	if err != nil || carried.Version != "1.0.114" || !carried.Continues {
		t.Errorf("the carry check read %+v, %v; want 1.0.114, continuing the session it wrote", carried, err)
	}
	kept, err := runner.CheckInPlace(t.Context(), b)
	if err != nil || kept.Version != "1.0.114" || !kept.Continues {
		t.Errorf("the copy-continue check read %+v, %v; want 1.0.114, continuing its copy", kept, err)
	}
}
