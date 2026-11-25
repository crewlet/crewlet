package executor_test

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// copilotRepoStandIn is the `copilot` the checkout rows run: it answers the
// release check and the guard's asks, records the home's instruction file, the instruction
// directories and the home's skills as the harness starts, and ends the run
// with an answer.
const copilotRepoStandIn = `#!/bin/sh
case "$1" in --version) echo 'GitHub Copilot CLI 1.0.114.'; exit 0;; config|mcp) exit 0;; esac
s="$HOME/seen"; mkdir -p "$s"
cat "$COPILOT_HOME/copilot-instructions.md" > "$s/instructions" 2> /dev/null
printf '%s' "${COPILOT_CUSTOM_INSTRUCTIONS_DIRS-unset}" > "$s/dirs"
[ ! -d "$COPILOT_HOME/skills" ] || cp -R "$COPILOT_HOME/skills" "$s/skills"
(cd "$COPILOT_HOME" && find . -type f -print) | sort > "$s/files"
printf '%s\n' '{"type":"assistant.message","data":{"content":"done"}}' '{"type":"result","exitCode":0}'
`

// withBoxNode puts the suite's node on box's PATH, as a box running Copilot
// from npm has one: a run with a checkout's launch set checks it first.
func withBoxNode(t *testing.T, box sandbox.Sandbox) {
	t.Helper()
	node := prereqtest.OnPath(t, "node", "a run with a checkout's launch set checks the box's node", "install node")
	writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/node", "#!/bin/sh\nexec "+node+" \"$@\"\n")
}

// plantTree writes files, by path relative to root.
func plantTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		plantFile(t, filepath.Join(root, rel), body)
	}
}

// copilotClone is the files of a finished clone's git directory under name.
func copilotClone(name string) map[string]string {
	return map[string]string{name + "/.git/HEAD": "ref: refs/heads/main\n", name + "/.git/index": "DIRC",
		name + "/.git/refs/heads/main": strings.Repeat("0", 40) + "\n"}
}

// WHAT A CHECKOUT BELOW THE START DIRECTORY BRINGS A COPILOT RUN at launch,
// in a real box: its .github/instructions through COPILOT_CUSTOM_INSTRUCTIONS_DIRS
// after the person's own value; its root files and unscoped .claude/rules in the
// home's copilot-instructions.md after the run's and the home's own, a scoped
// rule left to Copilot's lazy load; its skills as `repo:<skill>`, .github's
// winning a name, one past 64 characters cut to 64. Its hooks, MCP servers,
// extensions, agents and settings reach nothing, and once the run ends the
// home's file is its own and no copy is left.
//
// Mutation: drop the gate, set the variable to the checkout alone, leave out
// the home's own text, keep a 65-character name, or leave the copies.
func TestWhatACheckoutBelowTheStartDirectoryBringsACopilotRun(t *testing.T) {
	t.Parallel()
	runner, box := standInBox(t, executor.NewCopilot(), map[string]string{"copilot": copilotRepoStandIn})
	withBoxNode(t, box)
	p := executor.PathsFor(box)
	home := box.Home()
	long := strings.Repeat("l", 60)
	files := copilotClone("repo")
	for rel, body := range map[string]string{
		"repo/.github/copilot-instructions.md":          "CPI\n",
		"repo/AGENTS.md":                                "AGENTS\n",
		"repo/CLAUDE.md":                                "CLAUDE\n",
		"repo/GEMINI.md":                                "GEMINI\n",
		"repo/.claude/rules/r.md":                       "RULE\n",
		"repo/.claude/rules/py.md":                      "---\napplyTo: \"**/*.py\"\n---\nPY-RULE\n",
		"repo/.github/instructions/all.instructions.md": "INSTR\n",
		"repo/.github/skills/check/SKILL.md":            "---\nname: check\ndescription: GitHub's.\n---\n",
		"repo/.claude/skills/check/SKILL.md":            "---\nname: check\ndescription: Claude's.\n---\n",
		"repo/.agents/skills/lint/SKILL.md":             "---\nname: lint\ndescription: Lints.\n---\n",
		"repo/.claude/skills/" + long + "/SKILL.md":     "---\nname: " + long + "\ndescription: Long.\n---\n",
		"repo/.github/hooks/repo.json":                  `{"hooks":{"sessionStart":[{"bash":"REPO-HOOK"}]}}`,
		"repo/.github/extensions/repo/extension.mjs":    "// REPO-EXTENSION\n",
		"repo/.github/agents/repo.agent.md":             "REPO-AGENT\n",
		"repo/.mcp.json":                                `{"mcpServers":{"REPO-MCP":{"command":"x"}}}`,
		"repo/.github/copilot/settings.json":            `{"REPO-SETTINGS":true}`,
		"repo/.claude/settings.json":                    `{"REPO-CLAUDE-SETTINGS":true}`,
	} {
		files[rel] = body
	}
	plantTree(t, home+"/workspace", files)
	plantFile(t, p.CopilotInstructions(), "HOME-OWN\n")
	plantFile(t, p.CopilotSkills()+"/mine/SKILL.md", "---\nname: mine\n---\n")

	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", System: "Work well.",
		Env: map[string]string{"COPILOT_CUSTOM_INSTRUCTIONS_DIRS": home + "/own"}})
	if !res.Success {
		t.Fatalf("the run failed: %s\n%s", res.Error, boxFile(t, box, p.Err()))
	}
	work, err := filepath.EvalSymlinks(home + "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	seen := func(name string) string { return boxFile(t, box, home+"/seen/"+name) }

	t.Run(".github/instructions", func(t *testing.T) {
		if got, want := seen("dirs"), home+"/own,"+work+"/repo/.github/instructions"; got != want {
			t.Errorf("COPILOT_CUSTOM_INSTRUCTIONS_DIRS was %q as Copilot started, want %q", got, want)
		}
	})
	t.Run("root files and rules", func(t *testing.T) {
		want := "\n\nHOME-OWN\n\n\nfor `repo` with root " + work + "/repo\n\n" +
			"CPI\n\n\nAGENTS\n\n\nCLAUDE\n\n\nGEMINI\n\n\nRULE\n"
		if got := seen("instructions"); !strings.HasPrefix(got, "Work well.") || !strings.HasSuffix(got, want) ||
			strings.Count(got, "HOME-OWN") != 1 {
			t.Errorf("copilot-instructions.md was\n%q\nas Copilot started, want the run's text, then\n%q", got, want)
		}
		if got := boxFile(t, box, p.CopilotInstructions()); got != "HOME-OWN\n" {
			t.Errorf("the home's copilot-instructions.md is %q after the run, want its own", got)
		}
	})
	t.Run("skills", func(t *testing.T) {
		if got := boxFile(t, box, home+"/seen/skills/repo:check/SKILL.md"); !strings.Contains(got, "GitHub's") {
			t.Errorf("repo:check was %q, want .github/skills' over .claude/skills'", got)
		}
		if got := boxFile(t, box, home+"/seen/skills/repo:lint/SKILL.md"); !strings.HasPrefix(got, "---\nname: \"repo:lint\"\n") {
			t.Errorf("repo:lint was %q, want .agents/skills' renamed", got)
		}
		entries, err := os.ReadDir(home + "/seen/skills")
		if err != nil {
			t.Fatal(err)
		}
		var cut []string
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "repo:lll") {
				cut = append(cut, e.Name())
			}
		}
		if len(cut) != 1 || len(cut[0]) != 64 {
			t.Errorf("the 65-character name was copied as %q, want one name of 64, which Copilot lists", cut)
		}
		left, err := os.ReadDir(p.CopilotSkills())
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range left {
			names = append(names, e.Name())
		}
		if !slices.Equal(names, []string{"mine"}) {
			t.Errorf("the home's skills after the run are %q, want its own alone", names)
		}
	})
	t.Run("nothing else of the checkout", func(t *testing.T) {
		var held []string
		for _, f := range strings.Fields(seen("files")) {
			held = append(held, boxFile(t, box, p.CopilotHome()+"/"+strings.TrimPrefix(f, "./")))
		}
		held = append(held, seen("instructions"), seen("dirs"))
		for _, mark := range []string{"PY-RULE", "REPO-HOOK", "REPO-EXTENSION", "REPO-AGENT", "REPO-MCP",
			"REPO-SETTINGS", "REPO-CLAUDE-SETTINGS"} {
			for _, h := range held {
				if strings.Contains(h, mark) {
					t.Errorf("%s reached the run's home: %.200s", mark, h)
				}
			}
		}
	})
}

// copilotBudgetStandIn is the `copilot` the budget rows run, its extension's
// part played by the script (copilot_steer.js): Copilot 1.0.114's static
// budget as a byte bound ($HOME/limit) on the home's copilot-instructions.md.
// A new session stores its attempt's kept sets. Past the bound the launch is
// refused before any request (section 13's stream, exit 1) and a new session
// deleted (row R19w) unless $HOME/leave says the delete failed. Within it the
// run answers, or with $HOME/late is stopped after its first answer (row
// R15h). Each attempt's home file, brief, directories and number are recorded.
const copilotBudgetStandIn = `#!/bin/sh
case "$1" in --version) echo 'GitHub Copilot CLI 1.0.114.'; exit 0;; config|mcp) exit 0;; esac
id=; prev=; for a in "$@"; do [ "$prev" = --session-id ] && id=$a; prev=$a; done
d="$COPILOT_HOME/session-state/$id"; s="$HOME/seen"; mkdir -p "$s"
n=$(($(cat "$s/attempts" 2> /dev/null | wc -l) + 1))
cp "$COPILOT_HOME/copilot-instructions.md" "$s/instructions-$n" 2> /dev/null
cat > "$s/brief-$n"
printf '%s\n' "$id" > "$s/id"
printf '%s|%s\n' "${COPILOT_CUSTOM_INSTRUCTIONS_DIRS-unset}" "$(cat "$HOME/.crewlet/copilot-attempt")" >> "$s/attempts"
size=$(wc -c < "$COPILOT_HOME/copilot-instructions.md" 2> /dev/null) || size=0
src=new; [ ! -f "$d/events.jsonl" ] || src=resume
mkdir -p "$d"; printf '{"type":"user.message","data":{"content":"brief"}}\n' >> "$d/events.jsonl"
[ "$src" = resume ] || { kf="$HOME/.crewlet/copilot-repo/kept"; if [ -f "$kf" ]; then cp "$kf" "$d/crewlet-checkouts"; else : > "$d/crewlet-checkouts"; fi; }
if [ "$size" -gt "$(cat "$HOME/limit")" ]; then
  [ "$src" = resume ] || [ -e "$HOME/leave" ] || rm -rf "$d"
  printf '%s\n' '{"type":"session.warning","data":{"warningType":"compaction_static_context_blocked","message":"Static system messages and tool definitions exceed the budget."}}' \
    '{"type":"assistant.turn_end","data":{}}' \
    '{"type":"session.error","data":{"errorType":"query","message":"No response was returned because static system messages and tool definitions exceed this model context budget."}}' \
    '{"type":"assistant.idle","data":{}}' '{"type":"result","exitCode":1}'
  exit 1
fi
printf '%s\n' '{"type":"assistant.message","data":{"content":"done"}}'
if [ -e "$HOME/late" ]; then
  printf '%s\n' '{"type":"session.warning","data":{"warningType":"compaction_static_context_blocked","message":"Static system messages exceed the budget."}}' \
    '{"type":"session.error","data":{"errorType":"query","message":"No response was returned."}}' '{"type":"result","exitCode":1}'
  exit 1
fi
printf '%s\n' '{"type":"result","exitCode":0}'
`

// copilotBudgetBox is a real box running [copilotBudgetStandIn] with limit,
// its working directory holding the checkouts alpha (a small AGENTS.md) and
// beta (a 4,000-byte AGENTS.md and a .github/instructions), those of names.
func copilotBudgetBox(t *testing.T, limit int, names ...string) (*executor.Runner, sandbox.Sandbox) {
	t.Helper()
	runner, box := standInBox(t, executor.NewCopilot(), map[string]string{"copilot": copilotBudgetStandIn})
	withBoxNode(t, box)
	files := map[string]string{"limit": strconv.Itoa(limit)}
	for _, name := range names {
		for rel, body := range copilotClone("workspace/" + name) {
			files[rel] = body
		}
	}
	if slices.Contains(names, "alpha") {
		files["workspace/alpha/AGENTS.md"] = "ALPHA\n"
	}
	if slices.Contains(names, "beta") {
		files["workspace/beta/AGENTS.md"] = strings.Repeat("B", 4000) + "\n"
		files["workspace/beta/.github/instructions/b.instructions.md"] = "B\n"
	}
	plantTree(t, box.Home(), files)
	return runner, box
}

// copilotAttemptsSeen is each attempt's instruction directories and attempt
// number as the stand-in recorded them.
func copilotAttemptsSeen(t *testing.T, box sandbox.Sandbox) []string {
	t.Helper()
	return strings.Fields(boxFile(t, box, box.Home()+"/seen/attempts"))
}

// A COPILOT LAUNCH PAST ITS STATIC BUDGET IS STARTED AGAIN WITHOUT ITS LAST
// CHECKOUT'S LAUNCH SET, at most once more than the checkouts with one, on
// the same session, its brief again, each attempt numbered for the
// extension's state: (a) every set that fits is handed in one attempt; (b) the
// last set is dropped, named by a line in the home file and out of the
// instruction directories, and the run answers; (c) a checkout too big alone
// ends pointed at; (d) a run refused with no set left fails, typed, after
// checkouts+1 attempts; (e) never more.
//
// Mutation: drop the first set, relaunch onto a refusal with none left, keep
// the dropped directory, skip the brief, or number every attempt alike.
func TestACopilotLaunchPastItsBudgetDropsItsLastCheckoutsSet(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		limit    int
		names    []string
		attempts int
		kept     string
		fails    string
	}{
		"every set fits":       {limit: 1 << 20, names: []string{"alpha", "beta"}, attempts: 1, kept: "alpha beta"},
		"the last set dropped": {limit: 3000, names: []string{"alpha", "beta"}, attempts: 2, kept: "alpha"},
		"one too big alone":    {limit: 3000, names: []string{"beta"}, attempts: 2},
		"none left still over": {limit: 10, names: []string{"alpha", "beta"}, attempts: 3, fails: "with no repository's instructions handed"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := copilotBudgetBox(t, tc.limit, tc.names...)
			_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "the brief", System: "Work well."})
			attempts := copilotAttemptsSeen(t, box)
			if len(attempts) != tc.attempts || len(attempts) > len(tc.names)+1 {
				t.Fatalf("the run made %d attempts %q, want %d", len(attempts), attempts, tc.attempts)
			}
			for i, a := range attempts {
				if !strings.HasSuffix(a, "|"+strconv.Itoa(i+1)) {
					t.Errorf("attempt %d was numbered %q", i+1, a)
				}
				if brief := boxFile(t, box, box.Home()+"/seen/brief-"+strconv.Itoa(i+1)); brief != "the brief" {
					t.Errorf("attempt %d read the brief %q", i+1, brief)
				}
			}
			last := boxFile(t, box, box.Home()+"/seen/instructions-"+strconv.Itoa(len(attempts)))
			if tc.fails != "" {
				if res.Success || !strings.Contains(res.Error, tc.fails) {
					t.Fatalf("the run ended %v %q, want it refused %q", res.Success, res.Error, tc.fails)
				}
				return
			}
			if !res.Success {
				t.Fatalf("the run failed: %s\n%s", res.Error, boxFile(t, box, executor.PathsFor(box).Err()))
			}
			dirs := strings.Split(attempts[len(attempts)-1], "|")[0]
			for _, name := range tc.names {
				handed := strings.Contains(last, "for `"+name+"` with root ") && !strings.Contains(last, "/"+name+": its instructions arrive")
				if want := strings.Contains(tc.kept, name); handed != want {
					t.Errorf("%s handed %v in the accepted attempt, want %v:\n%.400s", name, handed, want, last)
				}
				if !handed && !strings.Contains(last, "/"+name+": its instructions arrive when you first view a file in it") {
					t.Errorf("the dropped %s is not named in the home file:\n%.400s", name, last)
				}
			}
			if strings.Contains(dirs, "beta") != strings.Contains(tc.kept, "beta") {
				t.Errorf("the accepted attempt's instruction directories are %q", dirs)
			}
		})
	}
}

// A REFUSED ATTEMPT THAT LEFT ANYTHING IS NEVER LAUNCHED ONTO: where the
// refused session's directory is still there the run fails, typed, naming it,
// with no second attempt (g); a box whose node cannot load node:sqlite, which
// the check needs, fails a run whose relaunch is due, naming its node, and
// runs one whose launch fits (h).
//
// Mutation: relaunch without the check, skip the node's, or ask it up front.
func TestACopilotRunIsNeverLaunchedOntoALeftover(t *testing.T) {
	t.Parallel()
	t.Run("the session left", func(t *testing.T) {
		t.Parallel()
		runner, box := copilotBudgetBox(t, 3000, "alpha", "beta")
		plantFile(t, box.Home()+"/leave", "")
		_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"})
		id := strings.TrimSpace(boxFile(t, box, box.Home()+"/seen/id"))
		if attempts := copilotAttemptsSeen(t, box); len(attempts) != 1 || res.Success ||
			!strings.Contains(res.Error, "its session "+id+" was not removed") {
			t.Fatalf("the run made %q and ended %q, want one attempt and a failure naming %s", attempts, res.Error, id)
		}
	})
	// THE NODE IS ASKED ONLY ONCE A RELAUNCH IS DUE: a launch that fits runs on
	// a box whose node cannot load node:sqlite (or has no node at all).
	for name, limit := range map[string]int{"a node without node:sqlite, refused": 3000,
		"a node without node:sqlite, fitting": 1 << 20} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := copilotBudgetBox(t, limit, "alpha", "beta")
			writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/node",
				"#!/bin/sh\ncase \"$1\" in --version) echo v20.10.0;; *) exit 1;; esac\n")
			_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"})
			attempts := copilotAttemptsSeen(t, box)
			if limit > 3000 {
				if !res.Success || len(attempts) != 1 {
					t.Fatalf("a launch that fits made %q and ended %q, want it run", attempts, res.Error)
				}
				return
			}
			if len(attempts) != 1 || res.Success || !strings.Contains(res.Error, "this box's node is v20.10.0") {
				t.Fatalf("the run made %q and ended %q, want one refused attempt and a refusal naming v20.10.0",
					attempts, res.Error)
			}
		})
	}
}

// A RESUME LAUNCHES WITH THE SETS ITS SESSION STORED, NEVER SEARCHING: a run
// whose launch dropped beta's set stores alpha with its session, and the run
// that resumes it hands alpha alone though beta would fit now; a resume
// Copilot refuses fails, typed, and deletes nothing (i). A run stopped past
// its first answer is never relaunched and never said to have done nothing (k).
//
// Mutation: search again on resume, delete on a refused resume, or relaunch a
// run blocked after it answered.
func TestACopilotResumeLaunchesTheSetsItsSessionStored(t *testing.T) {
	t.Parallel()
	runner, box := copilotBudgetBox(t, 3000, "alpha", "beta")
	if _, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"}); !res.Success {
		t.Fatalf("the first run failed: %s", res.Error)
	}
	id := strings.TrimSpace(boxFile(t, box, box.Home()+"/seen/id"))
	p := executor.PathsFor(box)
	if got := boxFile(t, box, p.CopilotSessionDir(id)+"/crewlet-checkouts"); got != "alpha\n" {
		t.Fatalf("the session stored the sets %q, want alpha's", got)
	}
	resume := func(limit int) sandbox.Result {
		t.Helper()
		plantFile(t, box.Home()+"/limit", strconv.Itoa(limit))
		if err := os.RemoveAll(box.Home() + "/seen"); err != nil {
			t.Fatal(err)
		}
		_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "again", Resume: id})
		return res
	}
	if res := resume(1 << 20); !res.Success || len(copilotAttemptsSeen(t, box)) != 1 {
		t.Fatalf("the resume ended %q after %q, want one accepted attempt", res.Error, copilotAttemptsSeen(t, box))
	}
	if got := boxFile(t, box, box.Home()+"/seen/instructions-1"); !strings.Contains(got, "for `alpha` with root") ||
		strings.Contains(got, "BBBB") || !strings.Contains(got, "/beta: its instructions arrive") {
		t.Errorf("the resume was handed\n%.400s\nwant alpha's set alone, beta pointed at", got)
	}
	if res := resume(10); res.Success || !strings.Contains(res.Error, "refused to resume the session") ||
		len(copilotAttemptsSeen(t, box)) != 1 {
		t.Fatalf("the refused resume ended %q after %q, want one attempt and a typed failure", res.Error,
			copilotAttemptsSeen(t, box))
	}
	if _, err := os.Stat(p.CopilotSessionDir(id) + "/events.jsonl"); err != nil {
		t.Errorf("the refused resume removed its session: %v", err)
	}

	t.Run("blocked after its first answer", func(t *testing.T) {
		runner, box := copilotBudgetBox(t, 1<<20, "alpha")
		plantFile(t, box.Home()+"/late", "")
		_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"})
		if res.Success || !strings.Contains(res.Error, "a tool the run called may have run") ||
			len(copilotAttemptsSeen(t, box)) != 1 {
			t.Fatalf("the run ended %q after %q, want one attempt and the mid-run failure", res.Error,
				copilotAttemptsSeen(t, box))
		}
	})
}
