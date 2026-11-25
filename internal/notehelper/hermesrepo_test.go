package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// hermesSpec is the spec a Hermes run's launch gives the core
// (internal/sandbox/executor's hermesCheckouts): no slot or skill directory
// of its own, every checkout answered for by the gate, its copies in skills.
func hermesSpec(skills string) checkoutSpec {
	return checkoutSpec{Gated: true, Skills: skills, Copied: filepath.Join(filepath.Dir(skills), "copied"),
		Reserved: []string{"crewlet"}}
}

// hermesLaunch runs checkout-launch in r's working directory under Hermes's
// spec and budget, own the run's own text (nil for no file) and bytes the
// bound, handed gate on stdin: what it said on stderr.
func (r repoRun) hermesLaunch(t *testing.T, own []byte, bytes int, gate checkoutGate) string {
	t.Helper()
	ownFile := filepath.Join(t.TempDir(), "system.md")
	if own != nil {
		if err := os.WriteFile(ownFile, own, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(launchConfig{State: r.state, Agents: r.agents, checkoutSpec: hermesSpec(r.skills),
		Budget: &launchBudget{Form: budgetHermes, Own: ownFile, Bytes: bytes}})
	config := filepath.Join(t.TempDir(), "launch.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(gate)
	var errs strings.Builder
	if err := checkoutLaunch(config, r.work, bytesReader(in), &errs); err != nil {
		t.Fatalf("the launch failed: %v\n%s", err, errs.String())
	}
	return errs.String()
}

// bytesReader is b to read.
func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// hermesPeek is the roots hermes-peek names off the state the launch wrote.
func (r repoRun) hermesPeek(t *testing.T) []string {
	t.Helper()
	var out, errs strings.Builder
	if code := runHermesPeek(context.Background(), flags{config: filepath.Join(r.state, "nested.json")},
		strings.NewReader(""), &out, &errs, os.Getenv); code != exitDone {
		t.Fatalf("hermes-peek exited %d: %s", code, errs.String())
	}
	var got struct{ Roots []string }
	if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
		t.Fatalf("hermes-peek printed %q: %v", out.String(), err)
	}
	return got.Roots
}

// hermesFire runs one hermes-nested firing with gate on stdin and room
// characters: what it printed and said.
func (r repoRun) hermesFire(t *testing.T, gate checkoutGate, room int) (out, said string) {
	t.Helper()
	in, _ := json.Marshal(gate)
	var stdout, stderr strings.Builder
	if code := runHermesNested(context.Background(), flags{config: filepath.Join(r.state, "nested.json"), room: room},
		bytesReader(in), &stdout, &stderr, os.Getenv); code != exitDone {
		t.Fatalf("the firing exited %d: %s", code, stderr.String())
	}
	return stdout.String(), stderr.String()
}

// A HERMES LAUNCH HANDS WHOLE CHECKOUTS WHILE THE SYSTEM PROMPT FITS AND
// NAMES THE REST, counting the prompt as the script builds it: the run's own
// text, trailing newlines dropped as $(cat) drops them, a blank line, then
// the sections apart by blank lines, at most the budget's bytes. Checkouts
// fitting to the byte are all whole; a byte less leaves the last unclaimed,
// for a firing (its name is longer than its text); one too big whole is named
// by its files, said, and a later smaller one is whole again.
//
// Mutation: charge no separator, count the own text's trailing newlines,
// claim a checkout that does not fit, or stop at the first misfit, and a row
// goes red.
func TestAHermesLaunchHandsWholeCheckoutsWhileThePromptFits(t *testing.T) {
	t.Parallel()
	for _, c := range []string{"to the byte", "a byte less", "b too big"} {
		t.Run(c, func(t *testing.T) {
			t.Parallel()
			r := newRepoRun(t, map[string]string{"a/.git/": "", "b/.git/": "", "c/.git/": ""})
			work := r.workdir(t)
			text := func(n int) string { return "## AGENTS.md\n\n" + strings.Repeat("x", n) }
			gate := checkoutGate{Checkouts: map[string]gatedCheckout{
				filepath.Join(work, "a"): {Text: text(40), Files: []string{filepath.Join(work, "a", "AGENTS.md")}},
				filepath.Join(work, "b"): {Text: text(4000), Files: []string{filepath.Join(work, "b", "AGENTS.md")}},
				filepath.Join(work, "c"): {Text: text(10), Files: []string{filepath.Join(work, "c", "CLAUDE.md")}}}}
			block := func(name string) string {
				return "for `" + name + "` with root " + filepath.Join(work, name) + "\n\n" +
					gate.Checkouts[filepath.Join(work, name)].Text
			}
			named := "for `b` with root " + filepath.Join(work, "b") + "\n\nread " + filepath.Join(work, "b", "AGENTS.md") +
				" for its instructions, which whole pass what the run's system prompt has room for"
			own := len("STANDING RULES") + 2
			want, bytes := block("a")+"\n\n"+block("b")+"\n\n"+block("c"), 0
			switch c {
			case "to the byte":
				bytes = own + len(want)
			case "a byte less":
				want = block("a") + "\n\n" + block("b")
				bytes = own + len(want) + 2 + len(block("c")) - 1
			case "b too big":
				want = block("a") + "\n\n" + named + "\n\n" + block("c")
				bytes = own + len(want)
			}
			said := r.hermesLaunch(t, []byte("STANDING RULES\n\n"), bytes, gate)
			if got := read(t, r.agents); got != want {
				t.Errorf("the sections read\n%q\nwant\n%q", got, want)
			}
			if (c == "b too big") != strings.Contains(said, filepath.Join(work, "b")+" are named, not handed") {
				t.Errorf("the launch said %q", said)
			}
			waiting := r.hermesPeek(t)
			if (c == "a byte less") != (len(waiting) == 1 && waiting[0] == filepath.Join(work, "c")) {
				t.Errorf("hermes-peek names %q as waiting", waiting)
			}
		})
	}
}

// A CHECKOUT WHOSE NAME DOES NOT FIT WAITS: the launch leaves it unclaimed,
// so hermes-peek names it, and the first firing with room hands it whole;
// none waits while no instructions need room.
//
// Mutation: claim a checkout whose name does not fit, and this goes red.
func TestAHermesCheckoutWithNoRoomWaitsForALaterFiring(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{"late/.git/": ""})
	root := filepath.Join(r.workdir(t), "late")
	gate := checkoutGate{Checkouts: map[string]gatedCheckout{root: {Text: "## AGENTS.md\n\nLATE", Files: []string{root + "/AGENTS.md"}}}}
	r.hermesLaunch(t, []byte("OWN"), len("OWN")+10, gate)
	if _, err := os.Stat(r.agents); err == nil {
		t.Errorf("the launch wrote sections %q, want none", read(t, r.agents))
	}
	if got := r.hermesPeek(t); len(got) != 1 || got[0] != root {
		t.Fatalf("hermes-peek named %q, want the waiting checkout", got)
	}
	out, _ := r.hermesFire(t, gate, 1000)
	if want := "for `late` with root " + root + "\n\n## AGENTS.md\n\nLATE"; out != want {
		t.Errorf("the firing printed %q, want %q", out, want)
	}
	if got := r.hermesPeek(t); len(got) != 0 {
		t.Errorf("hermes-peek named %q after the hand, want none", got)
	}
}

// A CHECKOUT CLONED DURING A HERMES RUN IS HANDED ONCE, THROUGH ITS GATE: a
// candidate the gate does not answer for waits; one it does is handed whole
// within the result's room (counted in characters, as Hermes's len counts
// them), its gate's skill directories copied as `<tag>:<skill>` and listed,
// a skill whose name a home skill is found by left out, said once in the
// answer and the run's log with the gate's own lines; then nothing more.
//
// Mutation: claim without the gate, copy a skill the home takes, count bytes,
// or hand twice, and this goes red.
func TestACheckoutClonedDuringAHermesRunIsHandedOnceThroughItsGate(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	r.hermesLaunch(t, nil, 1000, checkoutGate{})
	skill := func(n string) string { return "---\nname: " + n + "\ndescription: Does " + n + ".\n---\nBody.\n" }
	r.add(t, map[string]string{"late/.git/": "", "late/.hermes/skills/fix/SKILL.md": skill("fix"),
		"late/.hermes/skills/deploy/SKILL.md": skill("deploy")})
	root := filepath.Join(r.workdir(t), "late")
	if out, said := r.hermesFire(t, checkoutGate{}, 1000); out != "" || said != "" {
		t.Errorf("a firing whose gate does not name the checkout printed %q, said %q", out, said)
	}
	text := "## AGENTS.md\n\n" + strings.Repeat("é", 3000) // 3,014 characters, 6,014 bytes
	gate := checkoutGate{Checkouts: map[string]gatedCheckout{root: {Text: text, Files: []string{root + "/AGENTS.md"},
		Skills: []string{root + "/.hermes/skills/fix", root + "/.hermes/skills/deploy"},
		Said:   []string{"the skill " + root + "/.hermes/skills/evil is left out: Hermes's scan quarantines it"}}},
		Taken: map[string]string{"late:deploy": "/home/user/.hermes/skills/mine"}}
	label := "for `late` with root " + root + "\n\n"
	out, said := r.hermesFire(t, gate, len("\n\n")+len(label)+3014+1500)
	if !strings.HasPrefix(out, label+text+"\n\n# Skills of "+root) {
		t.Errorf("the firing printed %q, want the instructions whole, then the skills", out)
	}
	if !strings.Contains(out, "- late:fix: Does fix. (file: "+r.skills+"/late:fix/SKILL.md)") ||
		strings.Contains(out, "late:deploy:") {
		t.Errorf("the firing listed %q, want late:fix alone", out)
	}
	lines := []string{"the skill " + root + "/.hermes/skills/deploy is left out: the home's skill /home/user/.hermes/skills/mine " +
		"is found by its name late:deploy", "the skill " + root + "/.hermes/skills/evil is left out: Hermes's scan quarantines it"}
	if !strings.HasSuffix(out, "# Left out of the repositories below the working directory\n\n- "+strings.Join(lines, "\n- ")) {
		t.Errorf("the firing printed %q, want what it left out last", out)
	}
	if _, err := os.Stat(r.skills + "/late:deploy"); err == nil {
		t.Error("the skill the home takes was copied")
	}
	if got := read(t, filepath.Join(r.state, "skipped")); got != "notehelper: "+lines[0]+"\nnotehelper: "+lines[1]+"\n" {
		t.Errorf("the run's skip log holds %q", got)
	}
	if said != "" {
		t.Errorf("the firing said %q on stderr, want its lines in its log alone", said)
	}
	if out, _ := r.hermesFire(t, gate, 100000); out != "" {
		t.Errorf("a later firing printed %q", out)
	}
}

// A HERMES ANSWER NEVER PASSES ITS ROOM: whole where the result has room,
// named one character short of it, its listing cut to what is left, and a
// firing with no room claims nothing.
//
// Mutation: drop the cut, or claim at no room, and a row goes red.
func TestAHermesAnswerStaysWithinItsRoom(t *testing.T) {
	t.Parallel()
	for _, room := range []int{0, 100, 2000, 5000, 100000} {
		t.Run(strconv.Itoa(room), func(t *testing.T) {
			t.Parallel()
			r := newRepoRun(t, nil)
			r.hermesLaunch(t, nil, 1000, checkoutGate{})
			files := map[string]string{"late/.git/": ""}
			var dirs []string
			root := filepath.Join(r.work, "late")
			for i := range 40 {
				name := "s" + strconv.Itoa(i)
				files["late/.hermes/skills/"+name+"/SKILL.md"] = "---\nname: " + name + "\ndescription: " +
					strings.Repeat("d", 80) + "\n---\n"
				dirs = append(dirs, root+"/.hermes/skills/"+name)
			}
			r.add(t, files)
			real := filepath.Join(r.workdir(t), "late")
			for i := range dirs {
				dirs[i] = strings.Replace(dirs[i], root, real, 1)
			}
			gate := checkoutGate{Checkouts: map[string]gatedCheckout{real: {Text: strings.Repeat("y", 1500),
				Files: []string{real + "/AGENTS.md"}, Skills: dirs}}}
			out, _ := r.hermesFire(t, gate, room)
			if n := len([]rune(out)); out != "" && n+len("\n\n") > room {
				t.Errorf("the answer holds %d characters past a room of %d", n+2, room)
			}
			if handed := r.hermesPeek(t); (room == 0 || room == 100) != (len(handed) == 1) {
				t.Errorf("room %d: the checkout still waiting: %v", room, handed)
			}
		})
	}
}

// EVERY FIRING'S LISTINGS STAY WITHIN THEIR BOUND, A CUT ONE INCLUDED: a
// listing cut where the byte before the bound is a newline, after another
// listing, charges the blank line before it, so Codex's, Pi's and Claude
// Code's answers never pass 64 KiB and Hermes's never its room, whatever
// line the cut lands on (each shift moves it by a byte).
//
// Mutation: leave the cut block's separator uncharged, and a row goes red.
func TestEveryFiringsListingStaysWithinItsBound(t *testing.T) {
	t.Parallel()
	handedWith := func(shift, total int) []handedCheckout {
		line := func(i int) handedSkill {
			return handedSkill{name: "big:s" + strconv.Itoa(i), description: strings.Repeat("d", 40+shift),
				path: "/home/u/skills/big:s" + strconv.Itoa(i) + "/SKILL.md"}
		}
		big := handedCheckout{name: "big", root: "/w/big"}
		for i := 0; i*(80+shift) < 2*total; i++ {
			big.skills = append(big.skills, line(i))
		}
		small := handedCheckout{name: "small", root: "/w/small", skills: []handedSkill{line(0)}}
		return []handedCheckout{small, big}
	}
	for _, c := range []struct {
		name   string
		bound  int
		answer func([]handedCheckout) string
	}{
		{"codex", maxListing, func(h []handedCheckout) string { return codexNestedContext(h, nil) }},
		{"pi", maxListing, func(h []handedCheckout) string { return piNestedText(h, nil) }},
		{"claude", maxListing, func(h []handedCheckout) string { return claudeNestedContext(h, nil) }},
		{"hermes", 3000, func(h []handedCheckout) string {
			return "\n\n" + hermesAnswer(h, nil, newNestedRoom(3000)) // the blank line after the tool's result
		}},
	} {
		for shift := range 160 {
			got := c.answer(handedWith(shift, c.bound))
			if !strings.Contains(got, listingCut) {
				t.Fatalf("%s, shift %d: the listing was not cut", c.name, shift)
			}
			if len(got) > c.bound {
				t.Errorf("%s, shift %d: %d bytes past a bound of %d", c.name, shift, len(got), c.bound)
			}
		}
	}
}
