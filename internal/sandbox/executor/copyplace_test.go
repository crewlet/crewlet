package executor_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A PERSON'S ENTRY WHERE A COPY WOULD GO IS NEVER THE RUN'S: the run's end,
// and the next run's gate, remove every name a run listed as its copy, so a
// copy whose place in the run's skills is taken, by anything (a directory
// without SKILL.md, a skill under another name, a plain file, a link leading
// nowhere), is left out, said, and never listed. Each harness whose copies go
// into a skills directory of the person's home and that lists by a rule of
// its own (Grok Build, Kimi Code, Muse Code) leaves the person's entries as they were
// through a launch's checkout, a clone's nested firing, the run's end and
// the next run's gate.
//
// Mutation: list the copy before checking its place (drop the helper's place
// checks in copyPlanned and homeHolds), and a row goes red.
func TestACopyNeverTakesAPersonsEntryInTheRunsSkills(t *testing.T) {
	t.Parallel()
	for _, h := range []struct {
		name  string
		box   func(t *testing.T) (*executor.Runner, sandbox.Sandbox)
		skill func(p executor.Paths) string
		work  string
	}{
		{"grok", func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
			return standInBox(t, executor.NewGrok(), map[string]string{"grok": grokCheckoutStandIn})
		}, executor.Paths.GrokSkills, ""},
		{"kimi", kimiCheckoutBox, executor.Paths.KimiSkills, "workspace"},
		{"muse", func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
			return standInBox(t, executor.NewMuse(), map[string]string{"muse": museCheckoutStandIn})
		}, executor.Paths.MuseSkills, "workspace"},
	} {
		for _, kind := range []string{"a directory without SKILL.md", "a skill under another name", "a plain file",
			"a link leading nowhere"} {
			t.Run(h.name+", "+kind, func(t *testing.T) {
				t.Parallel()
				runner, box := h.box(t)
				p := executor.PathsFor(box)
				skills := h.skill(p)
				names := []string{"late:deploy", "later:deploy"}
				mine := func(name string) string { return "---\nname: other-" + strings.ReplaceAll(name, ":", "-") + "\n---\n" }
				for _, name := range names {
					place := filepath.Join(skills, name)
					switch kind {
					case "a directory without SKILL.md":
						plantFile(t, filepath.Join(place, "x.md"), mine(name))
					case "a skill under another name":
						plantFile(t, filepath.Join(place, "SKILL.md"), mine(name))
					case "a plain file":
						plantFile(t, place, mine(name))
					default:
						if err := os.MkdirAll(skills, 0o700); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink("nowhere-"+name, place); err != nil {
							t.Fatal(err)
						}
					}
				}
				skill := map[string]string{".agents/skills/deploy/SKILL.md": "---\nname: deploy\ndescription: Deploys.\n---\n"}
				plant(t, box, piCheckout(filepath.Join(h.work, "late"), skill))
				later := t.TempDir()
				for rel, body := range piCheckout("later", skill) {
					plantFile(t, filepath.Join(later, rel), body)
				}
				plant(t, box, map[string]string{"clone-src": filepath.Join(later, "later"),
					"clone-dst": filepath.Join(box.Home(), h.work, "later")})
				intact := func(when string) {
					t.Helper()
					for _, name := range names {
						place := filepath.Join(skills, name)
						var got, want string
						switch kind {
						case "a directory without SKILL.md":
							raw, _ := os.ReadFile(filepath.Join(place, "x.md"))
							got, want = string(raw), mine(name)
						case "a skill under another name":
							raw, _ := os.ReadFile(filepath.Join(place, "SKILL.md"))
							got, want = string(raw), mine(name)
						case "a plain file":
							raw, _ := os.ReadFile(place)
							got, want = string(raw), mine(name)
						default:
							got, _ = os.Readlink(place)
							want = "nowhere-" + name
						}
						if got != want {
							t.Errorf("%s: the person's %s at %s reads %q, want %q", when, kind, place, got, want)
						}
					}
				}
				runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"})
				intact("after a run that handed both checkouts")
				errs := boxFile(t, box, p.Err())
				for _, name := range names {
					src := "/" + strings.SplitN(name, ":", 2)[0] + "/.agents/skills/deploy is left out: "
					if !strings.Contains(errs, src) {
						t.Errorf("the run's error stream does not say the copy %s was left out:\n%s", name, errs)
					}
					if want := "the home's skills hold " + name + " already"; kind != "a skill under another name" &&
						!strings.Contains(errs, want) {
						t.Errorf("the run's error stream does not say %q:\n%s", want, errs)
					}
				}
				if err := os.Remove(filepath.Join(box.Home(), "clone-src")); err != nil {
					t.Fatal(err)
				}
				runToEnd(t, runner, box, sandbox.RunRequest{Brief: "again"})
				intact("after the next run's gate and end")
			})
		}
	}
}
