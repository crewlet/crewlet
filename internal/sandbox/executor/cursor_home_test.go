package executor_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// cursorStandIns is a stand-in `cursor-agent` of the supported release that
// runs body, then prints a run's stream as Cursor does, ending on its result,
// under the session --new-session-id names; and the `node` Cursor's package
// ships beside it, the machine's own, which the run's helper runs on.
func cursorStandIns(t *testing.T, body string) map[string]string {
	t.Helper()
	return map[string]string{
		"cursor-agent": "#!/bin/sh\n" +
			"case \"$1\" in --version) echo " + executor.CursorAgentSupported + "; exit 0;; esac\n" +
			"s=; prev=; for a; do case \"$prev\" in --new-session-id) s=$a;; esac; prev=$a; done\n" +
			body +
			`printf '{"type":"system","subtype":"init","session_id":"%s"}\n' "$s"` + "\n" +
			`printf '{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]},"session_id":"%s"}\n' "$s"` + "\n" +
			`printf '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"%s"}\n' "$s"` + "\n",
		"node": "#!/bin/sh\nexec " + shellword.Quote(nodeForNotes(t)) + " \"$@\"\n",
	}
}

// cursorRecording is a stand-in's body that records, into record, its argv,
// its working directory's rules, the home's MCP servers and the history it
// was handed, as they stood while the harness ran.
func cursorRecording(record string) string {
	r := shellword.Quote(record)
	return `for a; do printf '%s\n' "$a"; done > ` + r + `/argv
cp -R .cursor/rules ` + r + `/rules 2> /dev/null
cp "$HOME/.cursor/mcp.json" ` + r + `/mcp.json 2> /dev/null
h=; prev=; for a; do case "$prev" in --conversation-history-file) h=$a;; esac; prev=$a; done
[ -z "$h" ] || cp "$h" ` + r + `/history.json
ls "$HOME"/.cursor/chats/*/ > ` + r + `/chats 2> /dev/null
`
}

// cursorBox is a real box with the Cursor runner and its stand-ins.
func cursorBox(t *testing.T, body string) (*executor.Runner, sandbox.Sandbox) {
	t.Helper()
	return standInBox(t, executor.NewCursorAgent(), cursorStandIns(t, body))
}

// recordedFile is a file the stand-in recorded, "" for none.
func recordedFile(t *testing.T, record, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(record, name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(raw)
}

// cursorToken is the run's bridge token, as its MCP server's header carries
// it.
const cursorToken = "STANDIN-BRIDGE-TOKEN-0123456789"

// WHAT A CURSOR RUN LOADS, AND WHAT A CHECKOUT BELOW ITS DIRECTORY BRINGS IT, in
// a real box with a checkout at ./repo and a home MCP server: while it runs,
// its standing instructions are a rule; the checkout's instructions a rule of
// their own, its rules copied (nested ones too, globs under its directory),
// `--add-dir` naming it for its skills, its MCP servers and hooks nowhere; the
// run's server beside the home's own. Once over, the home's file holds its own
// server alone, no home file the bridge's token, and the rules are gone.
//
// Mutation: hand the working directory's AGENTS.md, drop --add-dir, copy the
// checkout's mcp.json, copy its top-level rules alone or their globs as
// written, replace the home's server rather than add beside it, or leave the
// run's server in the file: this goes red.
func TestWhatACursorRunLoadsAndWhatACheckoutBelowItBringsIt(t *testing.T) {
	t.Parallel()
	record := t.TempDir()
	runner, box := cursorBox(t, cursorRecording(record))
	plant(t, box, map[string]string{
		"workspace/AGENTS.md":                         "WORKDIR-INSTRUCTIONS\n",
		"workspace/repo/.git/HEAD":                    "ref: refs/heads/main\n",
		"workspace/repo/AGENTS.md":                    "REPO-INSTRUCTIONS\n",
		"workspace/repo/.cursor/rules/own.mdc":        "---\nalwaysApply: true\n---\nREPO-RULE\n",
		"workspace/repo/.cursor/rules/sub/nested.mdc": "---\nalwaysApply: true\n---\nREPO-NESTED\n",
		"workspace/repo/.cursor/rules/go.mdc":         "---\ndescription: go\nglobs: src/**, \"lib/*.go\"\n---\nREPO-GLOB\n",
		"workspace/repo/.cursor/mcp.json":             `{"mcpServers":{"repo-mcp":{"command":"repo-mcp-server"}}}`,
		"workspace/repo/.cursor/hooks.json":           `{"version":1,"hooks":{"sessionStart":[{"command":"repo-hook"}]}}`,
		".cursor/mcp.json":                            `{"mcpServers":{"home-mcp":{"command":"home-mcp-server"}}}`,
	})
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", System: "STANDING-TEXT",
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Name: "crewlet", Transport: sandbox.TransportHTTP,
			URL: "http://127.0.0.1:9/mcp", Headers: map[string]string{"Authorization": "Bearer " + cursorToken}}}})
	if !res.Success {
		t.Fatalf("the run failed: %s\n%s", res.Error, boxFile(t, box, paths(box).Err()))
	}
	work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), "workspace"))
	if err != nil {
		t.Fatal(err)
	}

	// Instructions.
	if got := recordedFile(t, record, "rules/crewlet-standing.mdc"); !strings.Contains(got, "alwaysApply: true") ||
		!strings.Contains(got, "---\nSTANDING-TEXT") {
		t.Errorf("standing instructions: the run's rule is %q, want them, always applied", got)
	}
	checkout := recordedFile(t, record, "rules/crewlet-checkout-repo.mdc")
	if !strings.Contains(checkout, "REPO-INSTRUCTIONS") || strings.Contains(checkout, "WORKDIR-INSTRUCTIONS") ||
		!strings.Contains(checkout, work+"/repo") {
		t.Errorf("the checkout's instructions: its rule is %q, want its own AGENTS.md under its root", checkout)
	}
	if got := recordedFile(t, record, "rules/crewlet-checkout-repo--own.mdc"); !strings.Contains(got, "REPO-RULE") {
		t.Errorf("the checkout's own rule was not copied beside the run's: %q", got)
	}
	if got := recordedFile(t, record, "rules/crewlet-checkout-repo--sub--nested.mdc"); !strings.Contains(got, "REPO-NESTED") {
		t.Errorf("the checkout's nested rule was not copied beside the run's: %q", got)
	}
	// Cursor matches a rule's globs from the run's directory: prefixed with
	// the checkout's, they match its files as they do in the checkout.
	if got := recordedFile(t, record, "rules/crewlet-checkout-repo--go.mdc"); !strings.Contains(got,
		"globs: repo/src/**,repo/lib/*.go\n") || !strings.Contains(got, "REPO-GLOB") {
		t.Errorf("the checkout's glob rule was copied as %q, want its globs under repo/", got)
	}
	// Skills, through Cursor's own --add-dir.
	argv := strings.Split(recordedFile(t, record, "argv"), "\n")
	if i := slices.Index(argv, "--add-dir"); i < 0 || argv[i+1] != work+"/repo" {
		t.Errorf("skills: the run's argv %q names no --add-dir of the checkout", argv)
	}
	// MCP servers and hooks.
	var mcp struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err = json.Unmarshal([]byte(recordedFile(t, record, "mcp.json")), &mcp); err != nil {
		t.Fatalf("the home's MCP servers while the run ran do not read: %v", err)
	}
	var ours []string
	for name, server := range mcp.Servers {
		if strings.HasPrefix(name, "crewlet-") && strings.Contains(string(server), cursorToken) {
			ours = append(ours, name)
		}
	}
	if _, home := mcp.Servers["home-mcp"]; !home || len(ours) != 1 || len(mcp.Servers) != 2 {
		t.Errorf("while the run ran, the home's MCP servers were %v, want the home's own beside one of the run's",
			slices.Sorted(maps.Keys(mcp.Servers)))
	}
	if strings.Contains(recordedFile(t, record, "mcp.json"), "repo-mcp") || strings.Contains(strings.Join(argv, " "), "repo-hook") {
		t.Error("the checkout's MCP servers or hooks reached the run")
	}

	// After the run.
	if got := boxFile(t, box, paths(box).CursorMCP()); !strings.Contains(got, "home-mcp") || strings.Contains(got, "crewlet-") {
		t.Errorf("after the run, the home's MCP servers are %s, want the home's own alone", got)
	}
	if err = filepath.WalkDir(box.Home(), func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		raw, readErr := os.ReadFile(path)
		if readErr == nil && strings.Contains(string(raw), cursorToken) {
			t.Errorf("after the run, %s holds the bridge's token", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	left, _ := filepath.Glob(filepath.Join(work, ".cursor", "rules", "crewlet-*"))
	if len(left) != 0 {
		t.Errorf("after the run, its rules are still in its directory: %v", left)
	}
}

// A CONTINUED CURSOR SESSION IS HANDED BACK AS ITS CONVERSATION: a run whose
// box holds the session's chat (a real store of the measured release) starts
// its harness with that chat's conversation as Cursor's own history, under
// the session's own id, the old chat gone so Cursor files the conversation
// afresh with the run's current instructions; a run asked to continue a
// session its box neither holds nor was carried starts a new session under
// another id, saying why.
//
// Mutation: resume the chat with --resume, keep the old chat, hand the
// history under a new id, or continue a session that is not there: this goes
// red.
func TestAContinuedCursorSessionIsHandedBackAsItsConversation(t *testing.T) {
	t.Parallel()
	fixture, err := os.ReadFile(executor.CursorAgentFixture("stores/plain.store.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	record := t.TempDir()
	store := filepath.Join(t.TempDir(), "store.db")
	if err = os.WriteFile(store, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	// The first run files the fixture as its chat, as Cursor would.
	file := `case " $* " in *" --conversation-history-file "*) ;; *) mkdir -p "$HOME/.cursor/chats/digest/$s" && cp ` +
		shellword.Quote(store) + ` "$HOME/.cursor/chats/digest/$s/store.db";; esac` + "\n"
	runner, box := cursorBox(t, file+cursorRecording(record))
	first, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"})
	if !res.Success {
		t.Fatalf("the first run failed: %s", res.Error)
	}
	_, res = runToEnd(t, runner, box, sandbox.RunRequest{Brief: "carry on", Resume: first.SessionID})
	if !res.Success || !res.Continued {
		t.Fatalf("the second run did not continue the session: %+v\n%s", res, boxFile(t, box, paths(box).Err()))
	}
	argv := strings.Split(recordedFile(t, record, "argv"), "\n")
	if slices.Contains(argv, "--resume") || !slices.Contains(argv, "--conversation-history-file") {
		t.Errorf("the second run's argv %q hands no history, or resumes the chat", argv)
	}
	if i := slices.Index(argv, "--new-session-id"); i < 0 || argv[i+1] != first.SessionID {
		t.Errorf("the second run's argv %q does not start the session's own id %s", argv, first.SessionID)
	}
	var history struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err = json.Unmarshal([]byte(recordedFile(t, record, "history.json")), &history); err != nil ||
		len(history.Messages) != 6 {
		t.Errorf("the history handed holds %d messages (%v), want the chat's six", len(history.Messages), err)
	}
	if got := recordedFile(t, record, "chats"); strings.Contains(got, first.SessionID) {
		t.Errorf("the old chat was still filed when the harness started: %s", got)
	}

	other := uuid.NewString()
	_, res = runToEnd(t, runner, box, sandbox.RunRequest{Brief: "carry on", Resume: other})
	argv = strings.Split(recordedFile(t, record, "argv"), "\n")
	i := slices.Index(argv, "--new-session-id")
	if res.Continued || i < 0 || argv[i+1] == other || slices.Contains(argv, "--conversation-history-file") ||
		!strings.Contains(boxFile(t, box, paths(box).Err()), "it is not in this box") {
		t.Errorf("a session the box does not hold: continued %v, argv %q; want a new session, saying why",
			res.Continued, argv)
	}
}

// A CARRIED CURSOR SESSION CONTINUES ONLY ON THE BUILD THAT WROTE IT: a run's
// capture holds the build that wrote it beside the conversation; carried into
// a fresh box on the same build it is handed back, and carried with another
// build of the same date recorded it starts a new session, saying why.
//
// Mutation: pack no build, or restore without reading it: a row goes red.
func TestACarriedCursorSessionContinuesOnlyOnTheBuildThatWroteIt(t *testing.T) {
	t.Parallel()
	fixture, err := os.ReadFile(executor.CursorAgentFixture("stores/plain.store.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(t.TempDir(), "store.db")
	if err = os.WriteFile(store, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	file := `case " $* " in *" --conversation-history-file "*) ;; *) mkdir -p "$HOME/.cursor/chats/digest/$s" && cp ` +
		shellword.Quote(store) + ` "$HOME/.cursor/chats/digest/$s/store.db";; esac` + "\n"
	first, a := cursorBox(t, file)
	_, ran := runToEnd(t, first, a, sandbox.RunRequest{Brief: "go"})
	described, body, err := first.OpenCapture(t.Context(), a)
	if err != nil || body == nil {
		t.Fatalf("OpenCapture = %v, %v\n%s", body, err, boxFile(t, a, paths(a).Err()))
	}
	capture, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		build     string
		continues bool
	}{
		"the build that wrote it":        {executor.CursorAgentSupported, true},
		"another build of the same date": {"2026.11.22-0000000", false},
	} {
		archive := cursorRebuilt(t, capture, c.build)
		carried := described
		carried.Size = int64(len(archive))
		record := t.TempDir()
		second, b := cursorBox(t, cursorRecording(record))
		_, res := runToEnd(t, second, b, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
			Carried: &sandbox.CarriedSession{Archive: carried, Body: bytes.NewReader(archive)}})
		handed := slices.Contains(strings.Split(recordedFile(t, record, "argv"), "\n"), "--conversation-history-file")
		said := strings.Contains(boxFile(t, b, paths(b).Err()), "written by Cursor Agent "+c.build)
		if res.Continued != c.continues || handed != c.continues || said == c.continues {
			t.Errorf("%s: continued %v, history handed %v, said so %v; want continued %v\n%s", name,
				res.Continued, handed, said, c.continues, boxFile(t, b, paths(b).Err()))
		}
	}
}

// cursorRebuilt is a Cursor capture with its recorded build replaced by build,
// every other member as packed.
func cursorRebuilt(t *testing.T, capture []byte, build string) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(capture))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	tw := tar.NewWriter(zw)
	builds := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(h.Name, ".release") {
			raw, builds = []byte(build+"\n"), builds+1
		}
		h.Size = int64(len(raw))
		if err = tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err = tw.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	if err = tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	if builds != 1 {
		t.Fatalf("the capture records %d builds, want one", builds)
	}
	return out.Bytes()
}

// A CURSOR RUN IS STOPPED AT ITS TURN CAP, counting model responses: two
// calls of one response, each started and completed, are one. With a cap of
// one the run is interrupted at its first (Cursor has no cap of its own) and
// collects as stopped at its cap, never a success; with a cap of two it
// finishes; a cap reached by the last response before Cursor exits with no
// answer still holds, as the script counts the whole stream again once Cursor
// exits.
//
// Mutation: count a call's start and its completion as two responses, stop
// at the response after the cap, leave out the cap line, or skip the count
// once Cursor exits: a row goes red.
func TestACursorRunIsStoppedAtItsTurnCap(t *testing.T) {
	t.Parallel()
	// ON NODE, as Cursor runs: a background job starts with SIGINT ignored, which
	// a shell cannot trap again, and node, as Cursor does, handles it anyway.
	// "wait" holds the stand-in until the follower's SIGINT, the signal the
	// row rests on, its bound liveness alone (a broken follower's run ends a
	// success, read as red); "answer" ends on Cursor's result; "cut" exits
	// with no answer, as a run cut off after its last response does.
	standIn := func(end string) string {
		answer := `console.log(JSON.stringify({type: "result", subtype: "success", is_error: false, result: "done", session_id: s}))`
		switch end {
		case "wait":
			end = `setTimeout(() => { ` + answer + ` }, 15000)`
		case "answer":
			end = answer
		default:
			end = ""
		}
		return `exec "${0%/*}/node" -e '
const s = process.argv[1]
process.on("SIGINT", () => process.exit(130))
const call = (id, subtype) => console.log(JSON.stringify({type: "tool_call", subtype, call_id: id, model_call_id: "m1",
  tool_call: {shellToolCall: {args: {command: "true"}}}, session_id: s}))
call("c1", "started"); call("c1", "completed"); call("c2", "started"); call("c2", "completed")
` + end + `
' "$s"
`
	}
	for name, c := range map[string]struct {
		turns  int
		end    string
		capped bool
	}{
		"interrupted at its cap":          {turns: 1, end: "wait", capped: true},
		"one response under a cap of two": {turns: 2, end: "answer", capped: false},
		"a cap its last response reached": {turns: 1, end: "cut", capped: true},
	} {
		runner, box := cursorBox(t, standIn(c.end))
		_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", Limits: sandbox.Limits{MaxTurns: c.turns}})
		// The cap line in the stream, which Cursor's own answer outranks in
		// the result: a cap counted where none was reached shows here alone.
		marked := strings.Contains(boxFile(t, box, paths(box).Result()), `"crewlet.cursor.cap"`)
		if res.RoundCapped != c.capped || res.Success == c.capped || marked != c.capped {
			t.Errorf("%s: capped %v, success %v (%q), cap line %v; want capped %v", name, res.RoundCapped,
				res.Success, res.Error, marked, c.capped)
		}
	}
}

// A CURSOR RUN'S FAILURE CARRIES NO TOKEN: Cursor's own words for a refused
// turn, which can quote what the service answered, are collected redacted, a
// token by its shape.
//
// Mutation: collect the decoder's error unredacted: this goes red.
func TestACursorRunsFailureCarriesNoToken(t *testing.T) {
	t.Parallel()
	const minted = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJhdXRoMHxTVEFORElOIn0.c3RhbmRpbi1zaWduYXR1cmUtMDEyMzQ1Njc4OQ"
	runner := executor.NewCursorAgent()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Result(), `{"type":"system","subtype":"init","session_id":"s"}`+"\n"+
		`{"type":"turn_ended","status":"error","error":"refused the token `+minted+`"}`+"\n")
	b.Put(p.ExitCode(), "1")
	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Success || !strings.Contains(res.Error, "refused the token") || strings.Contains(res.Error, minted) {
		t.Errorf("the failure collected as %q, want Cursor's words without the token", res.Error)
	}
}

// A CUT-OFF CURSOR RUN'S SERVERS ARE TAKEN OUT BY THE NEXT RUN'S CLEAR,
// WHICHEVER HARNESS IT RUNS: the run's script laid its server into the home's
// mcp.json and was killed before it took it out, and a person has since added
// a server. Another runner's clear leaves the home's and the person's servers,
// takes out the cut-off run's and its token, and removes the record; a file the
// run created holding nothing else goes; a record that does not read fails the
// clear, naming it.
//
// Mutation: drop Cursor's mendHome, empty every server rather than the
// recorded ones, keep the file the run created, or keep the record: red.
func TestACutOffCursorRunsServersGoInTheNextRunsClear(t *testing.T) {
	t.Parallel()
	dead := `"crewlet-dead":{"url":"http://engine.example.com/mcp","headers":{"Authorization":"Bearer ` +
		cursorToken + `"}}`
	for _, c := range []struct {
		name, live, record, want, refused string
	}{
		{name: "the home's file", live: `{"mcpServers":{"home-mcp":{"command":"home"},` + dead +
			`,"added-later":{"command":"later"}},"note":"the home's"}`,
			record: `{"names":["crewlet-dead"],"created":false}`, want: "home-mcp added-later note"},
		{name: "a file the run created", live: `{"mcpServers":{` + dead + `}}`,
			record: `{"names":["crewlet-dead"],"created":true}`},
		{name: "a record that does not read", live: `{"mcpServers":{` + dead + `}}`,
			record: `{"created":true}`, refused: "cursor-mcp-added.json"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, b := cursorBox(t, "")
			p := paths(b)
			plantFile(t, p.CursorMCP(), c.live)
			plantFile(t, p.CursorMCPAdded(), c.record)
			err := executor.NewClaudeCode().ClearArtifacts(t.Context(), b)
			if c.refused != "" {
				if err == nil || !strings.Contains(err.Error(), c.refused) {
					t.Fatalf("the clear returned %v, want it refused naming %s", err, c.refused)
				}
				return
			}
			if err != nil {
				t.Fatalf("ClearArtifacts: %v", err)
			}
			if got := boxFile(t, b, p.CursorMCPAdded()); got != "" {
				t.Errorf("the record is left after the clear: %q", got)
			}
			got := boxFile(t, b, p.CursorMCP())
			if c.want == "" {
				if got != "" {
					t.Errorf("the run created mcp.json and it is left after the clear: %q", got)
				}
				return
			}
			if strings.Contains(got, "crewlet-dead") || strings.Contains(got, cursorToken) {
				t.Errorf("mcp.json after the clear is %s, want the cut-off run's server out", got)
			}
			for _, kept := range strings.Fields(c.want) {
				if !strings.Contains(got, `"`+kept+`"`) {
					t.Errorf("mcp.json after the clear is %s, want %s kept", got, kept)
				}
			}
		})
	}
}

// A CONTINUED CURSOR RUN ENDS ONLY ON ITS OWN END: its session's transcript
// ends in the earlier run's turn_ended, and a run that then records none of its
// own, here exiting 0 beside a report having printed only its start, never
// ended, so it is no success.
//
// Mutation: add the transcript's last turn_ended whoever wrote it: this goes
// red.
func TestAContinuedCursorRunEndsOnlyOnItsOwnEnd(t *testing.T) {
	t.Parallel()
	fixture, err := os.ReadFile(executor.CursorAgentFixture("stores/plain.store.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(t.TempDir(), "store.db")
	if err = os.WriteFile(store, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	// The first run files its chat and its transcript's end, as Cursor does;
	// the second prints its start, writes a report and exits.
	body := `case " $* " in *" --conversation-history-file "*)
  printf '{"type":"system","subtype":"init","session_id":"%s"}\n' "$s"; echo 'Outcome: succeeded' > "$HOME/.crewlet/findings.md"; exit 0;;
esac
mkdir -p "$HOME/.cursor/chats/digest/$s" "$HOME/.cursor/projects/w/agent-transcripts/$s" && cp ` + shellword.Quote(store) +
		` "$HOME/.cursor/chats/digest/$s/store.db"
printf '{"type":"turn_ended","status":"success"}\n' > "$HOME/.cursor/projects/w/agent-transcripts/$s/$s.jsonl"
`
	runner, box := cursorBox(t, body)
	first, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"})
	if !res.Success {
		t.Fatalf("the first run failed: %s", res.Error)
	}
	_, res = runToEnd(t, runner, box, sandbox.RunRequest{Brief: "carry on", Resume: first.SessionID})
	if res.Success {
		t.Fatalf("a continued run that recorded no end of its own collected as a success: %+v", res)
	}
}

// CURSOR'S WORKER ENDS WITH ITS RUN: Cursor starts `<package>/index.js
// worker-server` beside itself, which outlives cursor-agent whether it
// finished, was interrupted or terminated (measured on 2026.11.22-e9042b2),
// so the run's script ends it once the harness exits; a stand-in that leaves
// such a worker running finds it gone when the run is collected.
//
// Mutation: drop the script's end of the worker: this goes red.
func TestCursorsWorkerEndsWithItsRun(t *testing.T) {
	t.Parallel()
	runner, box := cursorBox(t, `"${0%/*}/node" "${0%/*}/index.js" worker-server < /dev/null > /dev/null 2>&1 &
i=0; until [ -s "$HOME/worker.pid" ] || [ "$i" -ge 200 ]; do sleep 0.05; i=$((i + 1)); done
`)
	worker := `require("fs").writeFileSync(process.env.HOME + "/worker.pid", String(process.pid)); setInterval(() => {}, 1000)`
	writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/index.js", worker)
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go"})
	if !res.Success {
		t.Fatalf("the run failed: %s", res.Error)
	}
	pid := strings.TrimSpace(boxFile(t, box, box.Home()+"/worker.pid"))
	if pid == "" {
		t.Fatal("the stand-in's worker never started, so nothing tests its end")
	}
	// GONE, OR A ZOMBIE its new parent has yet to reap: either has ended.
	if stat, err := os.ReadFile("/proc/" + pid + "/stat"); err == nil {
		if _, rest, _ := strings.Cut(string(stat), ") "); !strings.HasPrefix(rest, "Z") {
			t.Errorf("Cursor's worker (pid %s) outlived its run: %s", pid, stat)
		}
	}
}
