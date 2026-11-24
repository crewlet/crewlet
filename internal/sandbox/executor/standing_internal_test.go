package executor

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// standingRoute is how a runner's continued run comes to hand its model this
// launch's standing instructions as current, where its harness would
// otherwise replay an earlier launch's (docs/concepts/llm-backends.md).
type standingRoute string

const (
	// routeReload: the harness renders the system prompt afresh from what
	// this launch hands it, on every resume.
	routeReload standingRoute = "reload"
	// routeRewrite: the harness replays what its session stored, and the
	// runner rewrites that to this launch's text before the resume.
	routeRewrite standingRoute = "rewrite"
	// routeReimport: the harness replays what its session stored, and the
	// runner re-imports the session, which stores none, before the resume.
	routeReimport standingRoute = "rewrite (re-import)"
	// routeSupersede: the harness keeps the earlier text and adds this
	// launch's, marked as replacing it, in its own words.
	routeSupersede standingRoute = "supersede"
)

// standingText is the launch's standing instructions every row hands its
// runner, as a continued run is handed them.
const standingText = "THIS LAUNCH: the thread so far, its open questions and memory"

// standingProbe holds one runner to its route: it fails t where the runner's
// own launch no longer takes that route.
type standingProbe struct {
	route standingRoute
	probe func(t *testing.T, h Harness, req sandbox.RunRequest, session Session)
}

// standingWritten is what h's WriteConfig wrote into a fake box for req and
// session, and the box's paths.
func standingWritten(t *testing.T, h Harness, req sandbox.RunRequest, session Session) (*sandbox.FakeSandbox, Paths) {
	t.Helper()
	box := sandbox.NewFakeSandbox("box-1")
	p := PathsFor(box)
	if _, err := h.WriteConfig(t.Context(), box, req, session, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	return box, p
}

// standingFile is path's content in box, failing t where it was not written.
func standingFile(t *testing.T, box *sandbox.FakeSandbox, path string) string {
	t.Helper()
	raw, err := box.ReadFile(t.Context(), path)
	if err != nil {
		t.Fatalf("%s was not written: %v", path, err)
	}
	return string(raw)
}

// standingRoutes is every registered runner's route, by name. A runner
// registered without one fails the table, so a runner that lands (Cursor
// Agent) adds its row with it.
var standingRoutes = map[string]standingProbe{
	// Claude Code reuses a conversation's recorded prompt unless told not to:
	// a resume is started with --system-prompt-snapshot off on the release
	// that takes it, and every run appends this launch's file.
	ClaudeCodeName: {routeReload, func(t *testing.T, h Harness, req sandbox.RunRequest, session Session) {
		cmd := h.Command(req, session, PathsFor(sandbox.NewFakeSandbox("box-1")), "")
		for _, want := range []string{`--resume`, `--system-prompt-snapshot off`, `--append-system-prompt-file`} {
			if !strings.Contains(cmd, want) {
				t.Errorf("a continued run's script lacks %s", want)
			}
		}
	}},
	// Codex replays the developer message its rollout recorded and sends
	// nothing of the configuration again (measured on 0.171.0): the gate runs
	// the helper's codex-renew on the rollout, handing it this launch's text.
	CodexName: {routeRewrite, func(t *testing.T, h Harness, req sandbox.RunRequest, session Session) {
		box, p := standingWritten(t, h, req, session)
		if script := codexScript(req, session, p, codexRunTimes()); !strings.Contains(script,
			"-adapter codex-renew -config "+shellword.Quote(p.CodexRenew())) {
			t.Error("a continued run's script renews no rollout")
		}
		var c struct{ Instructions string }
		if err := json.Unmarshal([]byte(standingFile(t, box, p.CodexRenew())), &c); err != nil || c.Instructions != req.System {
			t.Errorf("the renew config holds %q (%v), want this launch's text", c.Instructions, err)
		}
	}},
	// Gemini CLI builds a resumed chat's system instruction afresh
	// (startChat, read in 0.70.0's bundle) from GEMINI_SYSTEM_MD, which every
	// run, a resumed one too, points at this launch's prompt.
	GeminiCLIName: {routeReload, func(t *testing.T, h Harness, req sandbox.RunRequest, session Session) {
		box, p := standingWritten(t, h, req, session)
		script := geminiScript(req, session, p)
		if !strings.Contains(script, "--resume") || !strings.Contains(script, "GEMINI_SYSTEM_MD="+shellword.Quote(p.GeminiSystem())) {
			t.Error("a continued run is not pointed at this launch's prompt")
		}
		if standingFile(t, box, p.System()) != req.System {
			t.Error("the prompt it adds is not this launch's text")
		}
	}},
	// Qwen Code rebuilds the system instruction as a session loads
	// (startChat, read in 0.28.4's bundle), reading QWEN_SYSTEM_IDENTITY_MD.
	QwenCodeName: {routeReload, func(t *testing.T, h Harness, req sandbox.RunRequest, session Session) {
		box, p := standingWritten(t, h, req, session)
		pinned := false
		for _, pin := range h.Pins(t.Context(), req, p) {
			pinned = pinned || pin == Pinned{Name: "QWEN_SYSTEM_IDENTITY_MD", Value: p.System()}
		}
		if !pinned || standingFile(t, box, p.System()) != req.System {
			t.Error("a continued run's identity file is not this launch's text")
		}
	}},
	// Copilot renders its instructions file afresh on every resume
	// (measured on 1.0.114).
	CopilotName: {routeReload, func(t *testing.T, h Harness, req sandbox.RunRequest, session Session) {
		box, p := standingWritten(t, h, req, session)
		if standingFile(t, box, p.CopilotInstructions()) != req.System {
			t.Error("a continued run's instructions file is not this launch's text")
		}
	}},
	// Muse Code re-reads run.system_prompt_append on every resume (measured
	// on 1.4.25).
	MuseName: {routeReload, func(t *testing.T, h Harness, req sandbox.RunRequest, session Session) {
		box, p := standingWritten(t, h, req, session)
		var s struct {
			Run *struct {
				SystemPromptAppend string `json:"system_prompt_append"`
			} `json:"run"`
		}
		if err := json.Unmarshal([]byte(standingFile(t, box, p.MuseSettings())), &s); err != nil ||
			s.Run == nil || s.Run.SystemPromptAppend != req.System {
			t.Errorf("a continued run's settings append %+v (%v), want this launch's text", s.Run, err)
		}
	}},
	// Kimi Code renders the profile once, as a session is created, and a
	// resume restores the record's last bind (measured on 2.7.2): the driver
	// renews the bind from this launch's profile before it opens the session.
	KimiName: {routeRewrite, func(t *testing.T, h Harness, req sandbox.RunRequest, session Session) {
		box, p := standingWritten(t, h, req, session)
		var s kimiSettings
		if err := json.Unmarshal([]byte(standingFile(t, box, p.KimiSettings())), &s); err != nil {
			t.Fatal(err)
		}
		profile := standingFile(t, box, p.KimiAgents()+"/"+kimiProfileDir+"/"+s.Profile+".md")
		renew := strings.Index(kimiDriverSource, "const renewed = await renew(continued, config)")
		open := strings.Index(kimiDriverSource, "call(\"GET\", `/api/v1/sessions/${continued}`)")
		if !strings.Contains(profile, req.System) || renew < 0 || open < renew {
			t.Error("a continued session is opened on a bind this launch's profile did not renew")
		}
	}},
	// Grok Build resumes on the system message its record's first line holds,
	// whatever the launch's agent file says (measured on 1.0.92): the gate
	// replaces that line's instructions with this launch's before the resume.
	GrokName: {routeRewrite, func(t *testing.T, h Harness, req sandbox.RunRequest, session Session) {
		box, p := standingWritten(t, h, req, session)
		want, _ := json.Marshal(req.System)
		if !strings.Contains(grokSessionGate(session, p, false), grokRenewRules(p)) ||
			!strings.Contains(standingFile(t, box, p.GrokRules()), strings.Trim(string(want), `"`)) {
			t.Error("a continued session's record is resumed on instructions this launch did not renew")
		}
	}},
	// Hermes Agent freezes the standing instructions in the session's local
	// policy as it creates it, and a resume keeps them (measured on 0.23.3):
	// the gate runs hermes_carry.py's renew on the store, handing it this
	// launch's file, before the resume (hermesSessionGate).
	HermesName: {routeRewrite, func(t *testing.T, h Harness, req sandbox.RunRequest, session Session) {
		box, p := standingWritten(t, h, req, session)
		renew := `"$(hcarry renew ` + shellword.Quote(p.HermesStateDB()) + ` "$sid" ` + shellword.Quote(p.System()) + `)"`
		if !strings.Contains(hermesSessionGate(req, session, p, false), renew) ||
			standingFile(t, box, p.System()) != req.System {
			t.Error("a continued session's frozen launch is resumed on instructions this launch did not renew")
		}
	}},
	// OpenCode renders a session's instruction sources once and tells it a
	// change as a diff (measured on 2.3.2), and an import writes no
	// instruction state: the gate re-imports the box's own copy, so the
	// resumed request leads with this launch's text whole (measured).
	OpenCodeName: {routeReimport, func(t *testing.T, h Harness, req sandbox.RunRequest, session Session) {
		box, p := standingWritten(t, h, req, session)
		var cfg struct{ Instructions []string }
		if err := json.Unmarshal([]byte(standingFile(t, box, p.OpenCodeConfig())), &cfg); err != nil {
			t.Fatal(err)
		}
		if len(cfg.Instructions) != 1 || cfg.Instructions[0] != p.System() ||
			standingFile(t, box, p.System()) != req.System {
			t.Errorf("a continued run lists instructions %q, want this launch's text at %s alone",
				cfg.Instructions, p.System())
		}
		if !strings.Contains(openCodeSessionGate(session, p, false), openCodeRenew(session, p)) {
			t.Error("a continued session the box holds is resumed without being re-imported")
		}
		openCodeReimportedLeads(t)
	}},
	// Pi keeps the session's first system message and, where a section
	// changed, adds "Updated system prompt section" with its whole new text
	// (1.6.0's diffSystemPromptSections; measured): the run's text is the
	// `crewlet` section, set under that one name on every start.
	PiName: {routeSupersede, func(t *testing.T, _ Harness, req sandbox.RunRequest, _ Session) {
		probe := startPiProbe(t, piExtensionSettings{System: req.System})
		probe.expect("loaded")
		event := map[string]any{"systemPromptOptions": map[string]any{"sections": map[string]any{
			section(t): "EARLIER LAUNCH"}}}
		probe.event("before_agent_start", event)
		probe.expect("entry")
		var left struct {
			SystemPromptOptions struct {
				Sections map[string]string `json:"sections"`
			} `json:"systemPromptOptions"`
		}
		if err := json.Unmarshal(probe.expect("handled").Event, &left); err != nil {
			t.Fatal(err)
		}
		if got := left.SystemPromptOptions.Sections; len(got) != 1 || got[section(t)] != req.System {
			t.Errorf("a continued run's start leaves the sections %v, want this launch's text under %q alone",
				got, section(t))
		}
		piSupersedes(t)
	}},
}

// piSupersedes holds Pi 1.6.0's resumed request (testdata, measured with the
// section's text changed between two launches) to the supersede route: the
// first message still carries the earlier text, and a later one, in Pi's
// replacing words for the extension's section, carries the WHOLE new text.
func piSupersedes(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile("testdata/pi/1.6.0/resumed.request.json")
	if err != nil {
		t.Fatal(err)
	}
	var messages []struct{ Role, Content string }
	if err = json.Unmarshal(raw, &messages); err != nil || len(messages) != 2 {
		t.Fatalf("the fixture holds %d messages (%v)", len(messages), err)
	}
	name := section(t)
	whole := "<" + name + ">\nSTANDING-B\nthread so far: this launch\nmemory: kept\n\n</" + name + ">"
	if !strings.Contains(messages[0].Content, "STANDING-A") ||
		messages[1].Content != `Updated system prompt section "`+name+`":`+"\n\n"+whole {
		t.Errorf("Pi's resumed request does not supersede section %q with its whole new text:\n%q", name, messages)
	}
}

// openCodeReimportedLeads holds OpenCode 2.3.2's request resuming a
// re-imported session (testdata, measured: a 30-line instructions file whose
// lines 10-12 changed between two launches) to the rewrite: one developer
// message, leading, holding the new file whole; no word of the earlier one
// and no "changed" message; the history before the brief kept.
func openCodeReimportedLeads(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile("testdata/opencode/2.3.2/reimported.request.json")
	if err != nil {
		t.Fatal(err)
	}
	var messages []struct{ Role, Content string }
	if err = json.Unmarshal(raw, &messages); err != nil || len(messages) != 4 {
		t.Fatalf("the fixture holds %d messages (%v)", len(messages), err)
	}
	var whole strings.Builder
	for i := 1; i <= 30; i++ {
		text := "stable policy text"
		if i >= 10 && i <= 12 {
			text = "thread-so-far-B"
		}
		fmt.Fprintf(&whole, "line %d %s\n", i, text)
	}
	all := string(raw)
	if messages[0].Role != "developer" || !strings.Contains(messages[0].Content,
		"Instructions from: /home/box/sys.md\n"+whole.String()) || strings.Contains(all, "thread-so-far-A") ||
		strings.Contains(all, "changed") || messages[1].Content != "brief one" || messages[3].Content != "brief two" {
		t.Errorf("OpenCode's re-imported request does not lead with the new instructions whole:\n%q", messages)
	}
}

// section is the prompt section the Pi extension adds the run's standing
// instructions under, read off its source.
func section(t *testing.T) string {
	t.Helper()
	const decl = `const section = "`
	i := strings.Index(piExtensionSource, decl)
	if i < 0 {
		t.Fatal("the Pi extension declares no section")
	}
	rest := piExtensionSource[i+len(decl):]
	return rest[:strings.Index(rest, `"`)]
}

// A CONTINUED RUN HANDS ITS MODEL THIS LAUNCH'S STANDING INSTRUCTIONS AS
// CURRENT, on every registered runner, by the route its row names: its
// harness reloads them on a resume, the runner rewrites what the session
// stored before it, or the harness supersedes the stored text with this
// launch's in its own words. The thread, questions, memory and knowledge of an
// earlier launch never read as current.
//
// Mutation: drop Claude Code's snapshot switch; Codex's, Kimi's, Grok
// Build's or Hermes Agent's renew before the resume; the reload channel of
// Gemini CLI, Qwen Code, Copilot or Muse Code; OpenCode's re-import or Pi's
// fixed section name; and that runner's row goes red. Register a runner with
// no row, and the table does.
func TestAContinuedRunHandsItsModelThisLaunchsStandingInstructions(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, h := range Registered() {
		seen[h.Name()] = true
		row, ok := standingRoutes[h.Name()]
		if !ok {
			t.Errorf("%s names no route by which a continued run hands its model this launch's standing "+
				"instructions", h.Name())
			continue
		}
		t.Run(h.Name()+"/"+string(row.route), func(t *testing.T) {
			t.Parallel()
			session := h.Session(sandbox.RunRequest{Resume: standingSessionID})
			if !session.Continues {
				t.Fatalf("%s does not continue the session it is asked to", h.Name())
			}
			row.probe(t, h, sandbox.RunRequest{Brief: "go on", Resume: standingSessionID, System: standingText},
				session)
		})
	}
	for name := range standingRoutes {
		if !seen[name] {
			t.Errorf("%s has a route row and no registered runner", name)
		}
	}
}

// standingSessionID is the session every row's run continues: a UUID, which
// every runner's session ids are.
const standingSessionID = "019a1f00-0000-7000-8000-0000000005a1"
