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
// without SKILL.md, a plain file, a link leading nowhere), none of which a
// listing rule reads as a skill, is left out, said, and never listed. Each
// harness that lists by a rule of its own (Muse Code, Grok Build, Kimi Code)
// leaves the person's entries as they were through a launch's checkout, a
// clone's nested firing, the run's restore and the next run's gate.
//
// Mutation: drop the helper's place checks (copyPlanned's and homeHolds'),
// and a row goes red.
func TestACopyNeverTakesAPersonsEntryInTheRunsSkills(t *testing.T) {
	t.Parallel()
	for _, h := range []struct {
		name  string
		box   func(t *testing.T) (*executor.Runner, sandbox.Sandbox)
		skill func(p executor.Paths) string
		work  string
	}{
		{"muse", func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
			return standInBox(t, executor.NewMuse(), map[string]string{"muse": museCheckoutStandIn})
		}, executor.Paths.MuseSkills, "workspace"},
		{"grok", func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
			return standInBox(t, executor.NewGrok(), map[string]string{"grok": grokCheckoutStandIn})
		}, executor.Paths.GrokSkills, ""},
		{"kimi", kimiCheckoutBox, executor.Paths.KimiSkills, "workspace"},
	} {
		for _, kind := range []string{"a directory without SKILL.md", "a plain file", "a link leading nowhere"} {
			t.Run(h.name+", "+kind, func(t *testing.T) {
				t.Parallel()
				runner, box := h.box(t)
				p := executor.PathsFor(box)
				skills := h.skill(p)
				names := []string{"late:deploy", "later:deploy"}
				for _, name := range names {
					place := filepath.Join(skills, name)
					switch kind {
					case "a directory without SKILL.md":
						plantFile(t, filepath.Join(place, "x.md"), "MINE "+name+"\n")
					case "a plain file":
						plantFile(t, place, "MINE "+name+"\n")
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
						var got string
						switch kind {
						case "a directory without SKILL.md":
							raw, _ := os.ReadFile(filepath.Join(place, "x.md"))
							got = string(raw)
						case "a plain file":
							raw, _ := os.ReadFile(place)
							got = string(raw)
						default:
							target, _ := os.Readlink(place)
							got = "MINE " + strings.TrimPrefix(target, "nowhere-") + "\n"
						}
						if got != "MINE "+name+"\n" {
							t.Errorf("%s: the person's %s at %s reads %q", when, kind, place, got)
						}
					}
				}
				runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"})
				intact("after a run that handed both checkouts")
				errs := boxFile(t, box, p.Err())
				for _, name := range names {
					if want := "the home's skills hold " + name + " already"; !strings.Contains(errs, want) {
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
