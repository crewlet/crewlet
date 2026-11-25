package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// countingReader serves an endless body of `a` lines after head, counting
// every byte it hands over.
type countingReader struct {
	head io.Reader
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.head.Read(p)
	if errors.Is(err, io.EOF) {
		for i := range p {
			p[i] = "a\n"[i%2]
		}
		n, err = len(p), nil
	}
	c.read += n
	return n, err
}

// A SKILL'S NAME IS READ NO FURTHER THAN ITS FRONTMATTER'S CLOSE: a home of
// skills with long bodies costs a firing their frontmatters alone, not
// [maxFrontmatter] each, whether the frontmatter names the skill or not.
//
// Mutation: read the bound whole before parsing, or read on past the close,
// and the read passes one buffer.
func TestASkillsNameIsReadNoFurtherThanItsFrontmattersClose(t *testing.T) {
	t.Parallel()
	exact := func(l string) bool { return l == "---" }
	for _, front := range []string{"---\nname: x\n---\n", "---\ndescription: d\n---\n"} {
		r := &countingReader{head: strings.NewReader(front)}
		var name string
		named := readFrontmatter(r, exact, func(l string) bool {
			n, ok := strings.CutPrefix(l, "name: ")
			name = n
			return ok
		})
		if named != strings.Contains(front, "name:") || (named && name != "x") {
			t.Errorf("%q: named %v as %q", front, named, name)
		}
		if r.read > 4096 {
			t.Errorf("%q: %d bytes were read for its name, past one buffer of 4,096", front, r.read)
		}
	}
}

// A MUSE HOME IS READ AS MUSE CODE 1.4.25 LISTS IT (rows fn1-fn4): a skill
// directory directly in a home skill directory, a dot one too (unmeasured, so
// not assumed unlisted), takes the name its frontmatter gives, read past a
// long body; a directory holding no SKILL.md is no skill and takes no name,
// so the copies those hide are left out, said, and the rest are made.
//
// Mutation: skip dot directories, or take a directory without SKILL.md, and
// this goes red.
func TestAMuseHomeIsReadAsMuseListsIt(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	home := filepath.Join(t.TempDir(), "config-skills")
	long := strings.Repeat("body line\n", 8<<10)
	for dir, body := range map[string]string{
		".dot/SKILL.md":  "---\nname: late:dot\n---\n",
		"long/SKILL.md":  "---\ndescription: d\nname: 'late:long'\n---\n" + long,
		"late:bare/x.md": "not a skill\n",
	} {
		if err := os.MkdirAll(filepath.Join(home, filepath.Dir(dir)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, dir), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.museLaunch(t, "", home)
	files := map[string]string{"late/.git/": ""}
	for _, s := range []string{"dot", "long", "bare"} {
		files["late/.agents/skills/"+s+"/SKILL.md"] = "---\nname: " + s + "\ndescription: D" + s + ".\n---\n"
	}
	r.add(t, files)
	got := museContext(t, r.museFire(t, "s", "s"))
	for _, s := range []string{"dot", "long"} {
		line := "the skill " + r.workdir(t) + "/late/.agents/skills/" + s + " is left out: the home's " +
			filepath.Join(home, map[string]string{"dot": ".dot", "long": "long"}[s]) + " is listed as late:" + s +
			" too, and Muse Code would not list both"
		if !strings.Contains(got, "- "+line) {
			t.Errorf("the answer is\n%s\nwant %q", got, line)
		}
	}
	if !strings.Contains(got, "- late:bare: Dbare.") {
		t.Errorf("the answer is\n%s\nwant late:bare listed: a directory without SKILL.md takes no name", got)
	}
}

// A MUSE HOME THE FIRING CANNOT READ LEAVES EVERY COPY OUT, SAID: a home skill
// directory past the walk's bound of 1,024 skills may hold any name, so no
// copy is made on a guess that it does not.
//
// Mutation: skip a home directory that cannot be read, and the copy is made.
func TestAMuseHomeTheFiringCannotReadLeavesEveryCopyOut(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	home := filepath.Join(t.TempDir(), "config-skills")
	for i := range maxWalkSkills + 1 {
		dir := filepath.Join(home, fmt.Sprintf("s%04d", i))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: n\n---\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.museLaunch(t, "", home)
	r.add(t, map[string]string{"late/.git/": "",
		"late/.agents/skills/deploy/SKILL.md": "---\nname: deploy\ndescription: Deploys.\n---\n"})
	got := museContext(t, r.museFire(t, "s", "s"))
	if want := "the home's skills under " + home + " could not be read for the names they take"; !strings.Contains(got, want) {
		t.Errorf("the answer is\n%s\nwant %q", got, want)
	}
	if _, err := os.Lstat(filepath.Join(r.skills, "late:deploy")); err == nil {
		t.Error("a copy was made beside a home the firing could not read")
	}
}

// A LAUNCH NAMING A LISTING RULE THE HELPER DOES NOT KNOW IS REFUSED, naming
// the field, rather than run as the bare rule, which checks no home skill.
//
// Mutation: accept any listedAs, and the launch runs.
func TestALaunchRefusesAListingRuleItDoesNotKnow(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	spec := museSpec(r.skills)
	spec.ListedAs = "hermes"
	raw, _ := json.Marshal(launchConfig{State: r.state, Agents: r.agents, checkoutSpec: spec})
	config := filepath.Join(t.TempDir(), "launch.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	err := checkoutLaunch(config, r.work, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "listedAs") {
		t.Fatalf("the launch with listedAs hermes returned %v, want it refused naming listedAs", err)
	}
}
