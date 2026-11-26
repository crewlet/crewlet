package executor

import (
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/prereqtest"
)

// copilotRepoLaunched is a Copilot run's working directory under a fresh box
// home, its checkouts handed at launch by the real note helper as the run's
// gate runs it: the paths and the working directory as `pwd -P` reads it.
func copilotRepoLaunched(t *testing.T, files map[string]string) (Paths, string) {
	t.Helper()
	p, work := codexRepoBox(t, files)
	if err := os.MkdirAll(p.CopilotSkills(), 0o700); err != nil {
		t.Fatal(err)
	}
	bin, err := noteHelperBinary(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{p.CopilotNoteHelper(), p.CopilotCheckoutLaunch()} {
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeHostProgram(t, p.CopilotNoteHelper(), bin, 0o700)
	if err = os.WriteFile(p.CopilotCheckoutLaunch(), copilotCheckoutHome(p).launchConfig(copilotCheckouts(), nil), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(p.CopilotNoteHelper(), "-adapter", "checkout-launch", "-config", p.CopilotCheckoutLaunch()) //nolint:gosec // the engine's own helper
	cmd.Dir = work
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout-launch: %v\n%s", err, out)
	}
	return p, work
}

// A CHECKOUT CLONED DURING A COPILOT RUN IS LISTED AFTER THE MAIN AGENT'S
// NEXT TOOL, ONCE: the extension runs the real note helper after each of the
// main agent's tools and appends the listing of the skills it copied to that
// tool's own result, whole, the result's other fields kept; the next firing
// lists nothing again, and a subagent's tool hands nothing, leaving a later
// clone to the main agent's next tool.
//
// Mutation: hand on a subagent's firing, hand as additionalContext, replace the
// tool's result, or list a checkout twice.
func TestACheckoutClonedDuringACopilotRunIsListedOnce(t *testing.T) {
	p, work := copilotRepoLaunched(t, nil)
	probe := startCopilotProbe(t, copilotExtensionSettings{Helper: p.CopilotNoteHelper(),
		Nested: p.CopilotRepo() + "/nested.json"})
	probe.expect("joined")
	probe.expectArmed()
	clone := func(name string) {
		for rel, body := range map[string]string{".agents/skills/fix/SKILL.md": "---\nname: fix\ndescription: Fixes.\n---\n"} {
			path := filepath.Join(work, name, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		finishedClone(t, filepath.Join(work, name, ".git"))
	}
	fire := func(agent string) map[string]any {
		input := map[string]any{"toolName": "bash", "toolResult": map[string]any{"textResultForLlm": "cloned",
			"resultType": "success"}}
		if agent != "" {
			input["agentId"] = agent
		}
		probe.hook("onPostToolUse", input)
		return probe.expect("hooked").Output
	}
	listing := func(name string) string {
		return "cloned\n\n# Skills of " + work + "/" + name + "\n\nEach is a set of local instructions to follow, stored " +
			"in its `SKILL.md` file; to use one, read that file.\n- " + name + ":fix: Fixes. (file: " + p.CopilotSkills() +
			"/" + name + ":fix/SKILL.md)"
	}

	clone("late")
	out := fire("")
	result, _ := out["modifiedResult"].(map[string]any)
	if result["textResultForLlm"] != listing("late") || result["resultType"] != "success" || out["additionalContext"] != nil {
		t.Fatalf("the main agent's tool after the clone answered %v, want its result with the listing appended:\n%q",
			out, listing("late"))
	}
	if out := fire(""); out != nil {
		t.Errorf("the next firing answered %v, want nothing: the checkout was handed", out)
	}
	clone("later")
	if out := fire("sub-1"); out != nil {
		t.Errorf("a subagent's tool answered %v, want nothing", out)
	}
	result, _ = fire("")["modifiedResult"].(map[string]any)
	if text, _ := result["textResultForLlm"].(string); !strings.Contains(text, "later:fix") {
		t.Errorf("the main agent's next tool answered %v, want the later clone listed", result)
	}
}

// A REFUSED LAUNCH ATTEMPT IS DELETED ONLY WHERE IT IS A NEW SESSION OF ITS
// OWN: on Copilot's refusal of the launch (session.warning
// compaction_static_context_blocked) the extension opens Copilot's client on
// the internal parent-process connection, starts it and bulk-deletes exactly
// the session Copilot named it, as 1.0.114 does it (the stand-in refuses any
// other client, row R19g, or a call before start, R17a). A resumed session, a
// refusal after an answered request, and another warning delete nothing.
//
// Mutation: delete on resume, after a request, on any warning, or another id;
// build the client without _internalConnection; skip start().
func TestARefusedCopilotLaunchIsDeletedOnlyWhereItIsANewSessionOfItsOwn(t *testing.T) {
	blocked := map[string]any{"warningType": copilotStaticBlocked, "message": "Static system messages exceed"}
	for name, tc := range map[string]struct {
		source   string
		answered bool
		warning  map[string]any
		deletes  bool
	}{
		"a new session refused":     {source: "new", warning: blocked, deletes: true},
		"a resumed session refused": {source: "resume", warning: blocked},
		"a request answered first":  {source: "new", answered: true, warning: blocked},
		"another warning":           {source: "new", warning: map[string]any{"warningType": "compaction_static_context_budget"}},
	} {
		t.Run(name, func(t *testing.T) {
			p := startCopilotProbe(t, copilotExtensionSettings{Helper: "/no/helper", Nested: "/no/nested.json"})
			if joined := p.expect("joined"); strings.Join(joined.Hooks, ",") != "onAgentStop,onPostToolUse,onPostToolUseFailure,onPreToolUse,onSessionStart" {
				t.Fatalf("hooks = %v", joined.Hooks)
			}
			p.expectArmed()
			p.hook("onSessionStart", map[string]any{"source": tc.source})
			p.expect("hooked")
			if tc.answered {
				p.event("assistant.message", "", map[string]any{"content": "done"})
			}
			p.event("session.warning", "", tc.warning)
			if !tc.deletes {
				p.quiet(500*time.Millisecond, "the extension deleted a session it must keep")
				return
			}
			client := p.expect("client")
			if conn, _ := client.Input["_internalConnection"].(map[string]any); client.Input["mode"] != "coding-agent" ||
				conn["kind"] != "parent-process" {
				t.Fatalf("the client was made with %v, want Copilot's internal parent-process connection", client.Input)
			}
			if ids, _ := p.expect("bulkDelete").Input["sessionIds"].([]any); len(ids) != 1 || ids[0] != copilotProbeSession {
				t.Fatalf("bulkDelete deleted %v, want exactly its own session %q", ids, copilotProbeSession)
			}
		})
	}
}

// EACH LAUNCH ATTEMPT KEEPS ITS OWN STATE: a note a refused attempt sent went
// with its deleted session, so the next attempt, keyed by the attempt the
// script names, asks from the start and sends it again under the same inbox
// id, whose one confirm the accepted session's record holds. A restart within
// one attempt still sends nothing twice (TestARestartedCopilotExtensionSendsNoNoteTwice).
//
// Mutation: key the state by the run alone, and the second attempt asks past
// the note and never sends it.
func TestEachCopilotLaunchAttemptKeepsItsOwnState(t *testing.T) {
	route := newCopilotRoute(t)
	dir := t.TempDir()
	attempt := filepath.Join(dir, "copilot-attempt")
	settings := copilotExtensionSettings{URL: route.url, State: filepath.Join(dir, "state.json"), Run: "launch-1",
		Attempt: attempt}
	for n, want := range []string{"0", "0"} {
		if err := os.WriteFile(attempt, []byte(strconv.Itoa(n+1)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		p := startCopilotProbe(t, settings)
		p.expect("joined")
		p.expectArmed()
		p.event("assistant.turn_start", "", nil)
		poll := route.next("GET notes", "poll as the attempt's main agent works")
		if poll.after != want {
			t.Fatalf("attempt %d asked after=%q, want %s: an attempt counts nothing of another", n+1, poll.after, want)
		}
		poll.reply(http.StatusOK, copilotNote(1, "msg_a", "check X"))
		if sent := p.expect("send"); sent.Input["clientTurnId"] != CopilotClientTurnID("msg_a") {
			t.Fatalf("attempt %d sent %v", n+1, sent.Input)
		}
		route.next("GET notes", "poll past the note")
		p.end()
	}
}

// A REFUSED ATTEMPT'S STORE ROWS ARE FOUND BY NAME: the check a run makes
// before a relaunch counts the session in sessions.id and in every table keyed
// by session_id, as Copilot 1.0.114's session-store.db holds them (row R21:
// sessions, turns, search_index and the rest), reading it read-only; it
// passes a store without the session and a box with no store, and fails,
// naming what it found or why it could not read, with one stream line the
// decoder reads ([copilotRefusal]).
//
// Mutation: count sessions alone, count another id, or pass an unreadable
// store.
func TestARefusedAttemptsStoreRowsAreFoundByName(t *testing.T) {
	t.Parallel()
	node := prereqtest.OnPath(t, "node", "the store check runs on node", "install node")
	dir := t.TempDir()
	check := filepath.Join(dir, "check.mjs")
	if err := os.WriteFile(check, []byte(copilotStoreCheck), 0o600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "session-store.db")
	mk := exec.Command(node, "-e", `const { DatabaseSync } = require("node:sqlite")
const d = new DatabaseSync(process.argv[1])
d.exec("pragma journal_mode = wal")
d.exec("create table sessions (id text primary key, cwd text)")
d.exec("create table turns (id integer primary key, session_id text, user_message text)")
d.exec("create table assistant_usage_events (id integer primary key, session_id text)")
d.exec("create table other (id integer primary key, name text)")
d.exec("insert into sessions values ('kept', '/w'), ('gone-not', '/w')")
d.exec("insert into turns (session_id, user_message) values ('kept', 'brief'), ('kept', 'note'), ('other', 'x')")
d.exec("insert into assistant_usage_events (session_id) values ('kept')")
d.close()`, db) //nolint:gosec // the suite's own script
	if out, err := mk.CombinedOutput(); err != nil {
		t.Fatalf("making the store: %v\n%s", err, out)
	}
	run := func(store, id string) (int, copilotEvent) {
		t.Helper()
		out, err := exec.Command(node, check, store, id).Output() //nolint:gosec // the suite's own script
		code := 0
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		ev, _ := decodeCopilotEvent(out)
		return code, ev
	}
	if code, ev := run(db, "kept"); code != 1 || ev.Type != copilotRefusedMark ||
		ev.Data.Message != db+" still holds it: sessions 1, assistant_usage_events 1, turns 2" {
		t.Errorf("a store holding the session answered %d %+v", code, ev.Data)
	}
	for name, store := range map[string]string{"a store without it": db, "no store": filepath.Join(dir, "none.db")} {
		if code, ev := run(store, "absent"); code != 0 || ev.Type != "" {
			t.Errorf("%s answered %d %+v, want nothing found", name, code, ev)
		}
	}
	garbage := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(garbage, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, ev := run(garbage, "kept"); code != 1 || !strings.Contains(ev.Data.Message, "could not be read") {
		t.Errorf("an unreadable store answered %d %+v, want a failure saying so", code, ev.Data)
	}
	if !strings.Contains(copilotRefusal(copilotData{Reason: "store", Session: "kept", Message: "x"}), "session kept was not removed") {
		t.Error("the decoder does not name the store's leftover")
	}
}

// A NEW SESSION STORES ITS KEPT SETS AS IT STARTS: on sessionStart with source
// "new" the extension writes the launch sets its attempt kept into the
// session's directory, so a pack taken while Copilot runs carries them; a
// resumed session's stored sets stay as they are. Every session it starts in
// leaves the run's mark that it ran.
//
// Mutation: write on resume, store nothing until the run ends, or leave no mark.
func TestANewCopilotSessionStoresItsKeptSetsAsItStarts(t *testing.T) {
	dir := t.TempDir()
	kept, sessions := filepath.Join(dir, "kept"), filepath.Join(dir, "session-state")
	if err := os.WriteFile(kept, []byte("alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stored := filepath.Join(sessions, copilotProbeSession, copilotKeptSets)
	armed := filepath.Join(dir, "armed")
	for _, source := range []string{"resume", "new"} {
		if err := os.Remove(armed); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		p := startCopilotProbe(t, copilotExtensionSettings{Helper: "/no/helper", Nested: "/no/nested.json",
			Kept: kept, Sessions: sessions, Armed: armed})
		p.expect("joined")
		p.expectArmed()
		p.hook("onSessionStart", map[string]any{"source": source})
		p.expect("hooked")
		if _, err := os.Stat(armed); err != nil {
			t.Fatalf("a %s session left no mark that the extension ran: %v", source, err)
		}
		got, err := os.ReadFile(stored) //nolint:gosec // the case's own file
		switch {
		case source == "resume" && !errors.Is(err, os.ErrNotExist):
			t.Fatalf("a resumed session's sets were written (%q, %v), want the stored ones left alone", got, err)
		case source == "new" && string(got) != "alpha\n":
			t.Fatalf("a new session stored %q (%v), want its attempt's kept sets", got, err)
		}
		p.end()
	}
}

// A COPILOT PACK TAKEN WHILE THE RUN WORKS HOLDS ITS KEPT SETS: a save beside
// a working harness ([Copilot.PackSave]) packs the session's crewlet-checkouts
// with its events.jsonl, so the session continued from it launches with them.
//
// Mutation: leave the kept sets out of the pack.
func TestACopilotPackTakenMidRunHoldsItsKeptSets(t *testing.T) {
	t.Parallel()
	p := Paths{Home: t.TempDir()}
	dir := p.CopilotSessionDir("s-1")
	for name, body := range map[string]string{"events.jsonl": `{"type":"session.start","data":{}}` + "\n",
		copilotKeptSets: "alpha\n"} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	staged, dest := p.SessionPackStaging("save"), p.SessionPackHeld("save")
	script, err := (Copilot{}).PackSave(p, sessionRecord{id: "s-1"}, staged, dest)
	if err != nil {
		t.Fatal(err)
	}
	if _, errText, code := codexShell(t, p.Home, script); code != 0 {
		t.Fatalf("the save exited %d: %s", code, errText)
	}
	if got := codexMembers(t, codexFile(t, dest))[copilotSessionState+"/s-1/"+copilotKeptSets]; got != "alpha\n" {
		t.Errorf("the save holds the kept sets %q, want them beside the journal", got)
	}
}

// A COPILOT LAUNCH WALKS .claude/rules AS COPILOT DOES: the spec's tree slot
// names Copilot's form, which the helper refuses a slot without, so a
// checkout holding rules is launched with its unscoped rule handed and its
// scoped one left to Copilot's lazy load, and the nested firings read no
// instruction file (their spec names none).
//
// Mutation: leave the slot's form empty, and the launch is refused.
func TestACopilotLaunchWalksClaudeRulesAsCopilotDoes(t *testing.T) {
	t.Parallel()
	files := map[string]string{"repo/.git/": "", "repo/.claude/rules/r.md": "RULE\n",
		"repo/.claude/rules/py.md": "---\napplyTo: \"**/*.py\"\n---\nPY\n"}
	p, _ := copilotRepoLaunched(t, files)
	sections, err := os.ReadFile(p.CopilotRepoInstructions())
	if err != nil || !strings.Contains(string(sections), "RULE\n") || strings.Contains(string(sections), "PY") {
		t.Errorf("the launch handed %q (%v), want the unscoped rule alone", sections, err)
	}
	nested, err := os.ReadFile(p.CopilotRepo() + "/nested.json")
	if err != nil || strings.Contains(string(nested), ".claude/rules") {
		t.Errorf("the nested firings' spec is %s (%v), want it to name no instruction to read", nested, err)
	}
}
