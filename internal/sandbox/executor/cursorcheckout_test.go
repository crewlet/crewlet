package executor_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// cursorMidRunClone is a stand-in body that, as Cursor runs, keeps the home's
// hooks.json as it stands in record, clones a checkout at ./late, then runs
// the run's postToolUse entry (the home's last) as Cursor does at an MCP
// call, twice, keeping each answer, and once at a Shell call.
func cursorMidRunClone(record string) string {
	r := shellword.Quote(record)
	return `cp "$HOME/.cursor/hooks.json" ` + r + `/hooks.json 2> /dev/null
cmd=$(node -e 'const h=JSON.parse(require("fs").readFileSync(process.env.HOME+"/.cursor/hooks.json","utf8")); process.stdout.write((h.hooks.postToolUse||[]).at(-1)?.command||"")')
mkdir -p late/.git/refs/heads late/.cursor/skills/sk
echo 'ref: refs/heads/main' > late/.git/HEAD; printf DIRC > late/.git/index; printf '%040d\n' 0 > late/.git/refs/heads/main
echo LATE-AGENTS > late/AGENTS.md
printf -- '---\nname: sk\ndescription: Dsk.\n---\n' > late/.cursor/skills/sk/SKILL.md
mcp='{"tool_name":"MCP:t_tool","tool_output":"{\"content\":[{\"type\":\"text\",\"text\":\"ORIG\"}],\"isError\":false}"}'
shell='{"tool_name":"Shell","tool_output":"{\"output\":\"x\"}"}'
if [ -n "$cmd" ]; then
  printf '%s' "$shell" | sh -c "$cmd" > ` + r + `/answer0
  printf '%s' "$mcp" | sh -c "$cmd" > ` + r + `/answer1
  printf '%s' "$mcp" | sh -c "$cmd" > ` + r + `/answer2
fi
`
}

// cursorHomeHooks is a home hooks.json of the person's own, written as a
// person may write one: hooks of other events, their own spacing.
const cursorHomeHooks = "{\n  \"version\": 1,\n    \"hooks\": {\"sessionStart\": [ {\"command\": \"home-start\"} ],\n" +
	"  \"stop\":[{\"command\":\"home-stop\",\"loop_limit\":2}]}, \"extra\": {\"k\": [1,2]}\n}\n"

// A CHECKOUT CLONED DURING A CURSOR RUN IS HANDED AT ITS NEXT MCP CALL, ONCE:
// while the run runs, the home's hooks.json holds every entry of the home's
// own and one postToolUse entry of the run's, bounded by Cursor's timeout;
// that entry answers a Shell call {} and the first MCP call after the clone
// with the call's own content and the clone's instructions and skills as one
// more text item, and a later MCP call {}. Once over, the home's hooks.json is
// the home's own, byte for byte.
//
// Mutation: lay the entry first or without its timeout, drop a home entry,
// hand at a Shell call, hand twice, or leave the run's entry in the file, and
// this goes red.
func TestACheckoutClonedDuringACursorRunIsHandedAtItsNextMCPCall(t *testing.T) {
	t.Parallel()
	record := t.TempDir()
	runner, box := cursorBox(t, cursorMidRunClone(record))
	p := executor.PathsFor(box)
	plantFile(t, p.CursorHooks(), cursorHomeHooks)
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"})
	if !res.Success {
		t.Fatalf("the run failed: %s\n%s", res.Error, boxFile(t, box, p.Err()))
	}
	var during struct {
		Version int                          `json:"version"`
		Hooks   map[string][]json.RawMessage `json:"hooks"`
		Extra   json.RawMessage              `json:"extra"`
	}
	if err := json.Unmarshal([]byte(recordedFile(t, record, "hooks.json")), &during); err != nil {
		t.Fatalf("the home's hooks.json while the run ran does not read: %v", err)
	}
	compact := func(raw []byte) string {
		var b bytes.Buffer
		if json.Compact(&b, raw) != nil {
			return string(raw)
		}
		return b.String()
	}
	if during.Version != 1 || compact(during.Extra) != `{"k":[1,2]}` || len(during.Hooks["sessionStart"]) != 1 ||
		compact(during.Hooks["sessionStart"][0]) != `{"command":"home-start"}` || len(during.Hooks["stop"]) != 1 ||
		compact(during.Hooks["stop"][0]) != `{"command":"home-stop","loop_limit":2}` {
		t.Errorf("while the run ran, the home's own entries were not all there: %+v", during)
	}
	var ours []struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	if raw, _ := json.Marshal(during.Hooks["postToolUse"]); json.Unmarshal(raw, &ours) != nil || len(ours) != 1 ||
		ours[0].Timeout != 10 || !strings.HasPrefix(ours[0].Command, shellword.Quote(p.CursorNoteHelper())+" -adapter cursor-nested ") {
		t.Errorf("while the run ran, its postToolUse entries were %+v, want its one, bounded at 10 s", ours)
	}
	if got := strings.TrimSpace(recordedFile(t, record, "answer0")); got != "{}" {
		t.Errorf("a Shell call was answered %q, want {}", got)
	}
	work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	var answer struct {
		Updated struct {
			Content []struct{ Type, Text string } `json:"content"`
			IsError *bool                         `json:"isError"`
		} `json:"updated_mcp_tool_output"`
	}
	if err = json.Unmarshal([]byte(recordedFile(t, record, "answer1")), &answer); err != nil {
		t.Fatalf("the first MCP call's answer does not read: %v", err)
	}
	items := answer.Updated.Content
	if len(items) != 2 || items[0].Text != "ORIG" || answer.Updated.IsError == nil || *answer.Updated.IsError ||
		!strings.Contains(items[1].Text, "for `late` with root "+work+"/late\n\nLATE-AGENTS\n") ||
		!strings.Contains(items[1].Text, "- late:sk: Dsk. (file: "+p.CursorCheckoutSkills()+"/late:sk/SKILL.md)") {
		t.Errorf("the first MCP call after the clone was answered %+v, want its own item, then the clone's", answer)
	}
	if got := strings.TrimSpace(recordedFile(t, record, "answer2")); got != "{}" {
		t.Errorf("a later MCP call was answered %q, want {}", got)
	}
	if got := boxFile(t, box, p.CursorHooks()); got != cursorHomeHooks {
		t.Errorf("after the run the home's hooks.json is\n%q\nwant the home's own\n%q", got, cursorHomeHooks)
	}
}

// cursorRestoreStandIn is a stand-in body that keeps the home's hooks.json as
// it stands in record, then does what $HOME/mode says: exit 0, exit 1,
// signal its script as a teardown does (TERM, INT or HUP) and wait to be
// ended, or kill its script outright.
func cursorRestoreStandIn(record string) string {
	return `cp "$HOME/.cursor/hooks.json" ` + shellword.Quote(record) + `/hooks.json 2> /dev/null
case "$(cat "$HOME/mode")" in
ok) ;;
fail) exit 1;;
term|int|hup) kill -"$(tr a-z A-Z < "$HOME/mode")" "$PPID"; exec sleep 60;;
kill) kill -KILL "$PPID"; exit 0;;
esac
`
}

// THE HOME'S HOOKS.JSON IS THE HOME'S OWN, BYTE FOR BYTE, HOWEVER A CURSOR RUN
// ENDS: put back once the harness has stopped, succeeded or failed, and on
// each teardown signal (TERM, INT, HUP), the harness ended first; a run
// killed outright is put back by the next run's clear, whichever harness it
// drives. The home's own file comes back as it was, one the run made is gone,
// and nothing held is left.
//
// Mutation: drop the put-back after the harness, the EXIT trap or a signal's,
// or Cursor's homeKeeper, and a row goes red.
func TestACursorRunPutsTheHomesHooksBackHoweverItEnds(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"ok", "fail", "term", "int", "hup", "kill"} {
		for _, own := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, home's own %v", mode, own), func(t *testing.T) {
				t.Parallel()
				record := t.TempDir()
				runner, box := cursorBox(t, cursorRestoreStandIn(record))
				p := executor.PathsFor(box)
				plant(t, box, map[string]string{"mode": mode})
				if own {
					plantFile(t, p.CursorHooks(), cursorHomeHooks)
				}
				handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "go"})
				if err != nil {
					t.Fatalf("Start: %v", err)
				}
				awaitOver(t, runner, box, handle, time.Minute, "the stand-in ends at once or asks its script to")
				if !strings.Contains(recordedFile(t, record, "hooks.json"), "cursor-nested") {
					t.Fatalf("the run laid no hook while it ran:\n%s", recordedFile(t, record, "hooks.json"))
				}
				if mode == "kill" {
					if err := executor.NewClaudeCode().ClearArtifacts(t.Context(), box); err != nil {
						t.Fatalf("the next run's clear: %v", err)
					}
				}
				raw, err := os.ReadFile(p.CursorHooks())
				switch {
				case own && string(raw) != cursorHomeHooks:
					t.Errorf("after the run the home's hooks.json is\n%q\nwant the home's own", raw)
				case !own && !os.IsNotExist(err):
					t.Errorf("after the run the home holds a hooks.json it never had: %q (%v)", raw, err)
				}
				for _, held := range []string{"", ".none", ".link", ".tmp"} {
					if _, err := os.Lstat(p.CursorHeldHooks() + held); err == nil {
						t.Errorf("%s is left after the run", p.CursorHeldHooks()+held)
					}
				}
			})
		}
	}
}

// A CURSOR RUN LAYS NO HOOK BESIDE ANOTHER POST-TOOL HOOK: Cursor runs each
// post-tool hook on the call's own result and hands the model only one
// answer (measured), so where the home's hooks.json, the project's, or Claude
// Code's settings, which Cursor reads too, hold one, the run lays none and
// says once, naming the file, that a checkout cloned during it is not handed;
// so it does for a home hooks.json it cannot read. The run is not refused,
// and the home's file is left as it was.
//
// Mutation: skip a source, lay the hook all the same, refuse the run, or say
// it twice, and a row goes red.
func TestACursorRunLaysNoHookBesideAnotherPostToolHook(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ rel, body, file string }{
		"the home's":     {".cursor/hooks.json", `{"version":1,"hooks":{"postToolUse":[{"command":"theirs"}]}}`, "/.cursor/hooks.json"},
		"the project's":  {"workspace/.cursor/hooks.json", `{"hooks":{"postToolUse":[{"command":"theirs"}]}}`, "/workspace/.cursor/hooks.json"},
		"Claude Code's":  {".claude/settings.json", `{"hooks":{"PostToolUse":[{"hooks":[{"type":"command","command":"x"}]}]}}`, "/.claude/settings.json"},
		"an unread home": {".cursor/hooks.json", "{not json", "/.cursor/hooks.json does not read"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			record := t.TempDir()
			runner, box := cursorBox(t, cursorRestoreStandIn(record))
			p := executor.PathsFor(box)
			plant(t, box, map[string]string{"mode": "ok", c.rel: c.body})
			home, _ := os.ReadFile(p.CursorHooks())
			_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"})
			if !res.Success {
				t.Fatalf("the run was refused: %s\n%s", res.Error, boxFile(t, box, p.Err()))
			}
			if got, _ := os.ReadFile(record + "/hooks.json"); string(got) != string(home) {
				t.Errorf("while the run ran the home's hooks.json was %q, want it untouched %q", got, home)
			}
			errs := boxFile(t, box, p.Err())
			line := "crewlet: a checkout cloned during the run is not handed its instructions and skills: "
			if n := strings.Count(errs, line); n != 1 || !strings.Contains(errs, c.file) {
				t.Errorf("the error stream says it %d times, naming %s, want once:\n%s", n, c.file, errs)
			}
		})
	}
}
