package executor

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/prereqtest"
)

// hermesGateProbe loads the run's carry script (argv[2]) beside the stand-in
// of Hermes's modules (argv[1]) and, in one process as the run's plugin does,
// renders each root of argv[3] and stamps it, then for each later step of
// argv[4] writes its files and renders and stamps again, printing each
// render's gate entries and whether each stamp could be told.
const hermesGateProbe = `import importlib.util, json, os, sys
sys.path.insert(0, sys.argv[1])
spec = importlib.util.spec_from_file_location("crewlet_carry", sys.argv[2])
c = importlib.util.module_from_spec(spec)
spec.loader.exec_module(c)
roots, steps, out = json.loads(sys.argv[3]), json.loads(sys.argv[4]), []
for files in [{}] + steps:
    for path, body in files.items():
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w") as f:
            f.write(body)
    gate = c.checkout_gate(roots, None)
    out.append({"gate": gate["checkouts"], "stamps": [c.checkout_stamp(r) for r in roots]})
print(json.dumps(out))
`

// hermesRenders runs [hermesGateProbe] over roots, trusting those named in
// trusted, within a minute (a walk without end fails the case), and returns
// each render's entries and stamps.
func hermesRenders(t *testing.T, home string, roots, trusted []string, steps []map[string]string) []struct {
	Gate   map[string]struct{ Skills, Said []string }
	Stamps []any
} {
	t.Helper()
	python := prereqtest.OnPath(t, "python3", "the case runs the run's carry script", "install python3")
	bin := t.TempDir()
	hermesLauncherIn(t, bin, filepath.Join(bin, "etc-hermes"))
	carry := filepath.Join(bin, "carry.py")
	config, _ := json.Marshal(map[string]any{"skills": map[string]any{"trusted_project_dirs": trusted}})
	for name, body := range map[string][]byte{carry: hermesCarryScript, filepath.Join(home, "config.yaml"): config} {
		if err := os.WriteFile(name, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rootsArg, _ := json.Marshal(roots)
	stepsArg, _ := json.Marshal(steps)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-I", "-c", hermesGateProbe, filepath.Join(bin, "py"), carry,
		string(rootsArg), string(stepsArg))
	cmd.Env = append(os.Environ(), "HERMES_HOME="+home)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("the carry's gate (%v, a walk without end is killed at its minute): %v\n%s", ctx.Err(), err,
			stderr.String())
	}
	var got []struct {
		Gate   map[string]struct{ Skills, Said []string }
		Stamps []any
	}
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the probe printed %q: %v", raw, err)
	}
	return got
}

// hermesSkillTree writes files below dir, a path each relative to it.
func hermesSkillTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A HERMES RENDER JUDGES A SKILL BY ITS CURRENT CONTENT: Hermes keeps a
// project skill's quarantine verdict for its process's life (measured on
// 0.23.3), and the plugin renders in the run's gateway, so a re-render asks
// afresh, judging each skill by what its files hold then, a script's as well
// as its SKILL.md's, and says what it leaves out; and a skill directory is
// stamped whole, so a change to any of its files renders the checkout again.
//
// Mutation: keep Hermes's cached verdict, or stamp only SKILL.md, and a check
// goes red.
func TestAHermesRenderJudgesASkillAsItsContentStandsNow(t *testing.T) {
	t.Parallel()
	home, root := t.TempDir(), filepath.Join(t.TempDir(), "repo")
	hermesSkillTree(t, root, map[string]string{".hermes/skills/s/SKILL.md": "---\nname: s\n---\nclean\n",
		".hermes/skills/t/SKILL.md": "---\nname: t\n---\nclean\n"})
	skills := filepath.Join(root, ".hermes", "skills")
	got := hermesRenders(t, home, []string{root}, []string{root}, []map[string]string{
		{filepath.Join(skills, "t", "scripts", "run.sh"): "QUARANTINE\n"},
		{filepath.Join(skills, "s", "SKILL.md"): "---\nname: s\n---\nQUARANTINE\n"},
	})
	if want := []string{filepath.Join(skills, "s"), filepath.Join(skills, "t")}; !slices.Equal(got[0].Gate[root].Skills, want) {
		t.Fatalf("the clean render copies %q, want %q", got[0].Gate[root].Skills, want)
	}
	if mustJSON(t, got[0].Stamps) == mustJSON(t, got[1].Stamps) {
		t.Errorf("the stamp %s did not move with a skill's script", mustJSON(t, got[0].Stamps))
	}
	for i, c := range []struct {
		name  string
		keeps []string
	}{{"t", []string{filepath.Join(skills, "s")}}, {"s", nil}} {
		g := got[i+1].Gate[root]
		line := "the skill " + filepath.Join(skills, c.name) + " is left out: Hermes's scan quarantines it"
		if !slices.Contains(g.Said, line) {
			t.Errorf("render %d says %q, want %q among it", i+2, g.Said, line)
		}
		if !slices.Equal(g.Skills, c.keeps) {
			t.Errorf("render %d copies %q, want %q", i+2, g.Skills, c.keeps)
		}
	}
}

// A HERMES CHECKOUT'S SKILL WALK IS BOUNDED, AND TRUST COMES FIRST: an
// untrusted root's skill directories are never walked by link (one linking
// to itself would walk without end, measured on 0.23.3), only probed for a
// SKILL.md to say they are left out; a trusted root's walk takes each
// directory once, so a link loop ends and its skill is found, and past 4,096
// directories its skills are left out, said, and it has no stamp, so no
// render of it is kept.
//
// Mutation: walk before the trust check, follow a loop's links again, or drop
// the bound, and a check goes red or the probe is killed at its minute.
func TestAHermesCheckoutsSkillWalkIsBoundedAndTrustComesFirst(t *testing.T) {
	t.Parallel()
	home, work := t.TempDir(), t.TempDir()
	untrusted, looped, wide := filepath.Join(work, "untrusted"), filepath.Join(work, "looped"), filepath.Join(work, "wide")
	for _, root := range []string{untrusted, looped} {
		hermesSkillTree(t, root, map[string]string{".hermes/skills/x/SKILL.md": "---\nname: x\n---\nbody\n"})
		for _, link := range []string{"a", "b"} {
			if err := os.Symlink(".", filepath.Join(root, ".hermes", "skills", link)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := range 4097 {
		if err := os.MkdirAll(filepath.Join(wide, ".hermes", "skills", "d"+strconv.Itoa(i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	got := hermesRenders(t, home, []string{untrusted, looped, wide}, []string{looped, wide}, []map[string]string{})[0]
	if g := got.Gate[untrusted]; len(g.Skills) != 0 || !slices.ContainsFunc(g.Said, func(s string) bool {
		return strings.Contains(s, "skills.trusted_project_dirs names the checkout's root")
	}) {
		t.Errorf("the untrusted root's entry is %+v, want no skill and its trust said", g)
	}
	if g := got.Gate[looped]; !slices.Equal(g.Skills, []string{filepath.Join(looped, ".hermes", "skills", "x")}) {
		t.Errorf("the looped root's skills are %q, want x once", g.Skills)
	}
	if g := got.Gate[wide]; len(g.Skills) != 0 || !slices.ContainsFunc(g.Said, func(s string) bool {
		return strings.Contains(s, "holds more than 4096 directories to walk")
	}) {
		t.Errorf("the wide root's entry is %+v, want its skills left out at the bound, said", g)
	}
	if got.Stamps[1] == nil || got.Stamps[2] != nil {
		t.Errorf("the stamps are %v, want the looped root's told and the wide root's none", got.Stamps)
	}
}

// mustJSON is v as JSON.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
