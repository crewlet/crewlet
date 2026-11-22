package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// piFixture is a capture of Pi 1.6.0 run with the runner's own extension
// against a stand-in model and bridge (testdata/pi/1.6.0).
func piFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "pi", "1.6.0", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// piStream is a fixture's stream as a run's script leaves it: the mark the
// script prints before its harness ([piMarkLine]), then pi's own lines.
func piStream(t *testing.T, name string, cap int) []byte {
	t.Helper()
	return append([]byte(piMarkLine(cap)+"\n"), piFixture(t, name+".stream.jsonl")...)
}

// A PI RUN IS HEADLESS, MACHINE-READABLE AND TRUSTS ITS WORKING DIRECTORY, its
// model and effort as the entry names them, everything of the run's own in the
// one extension, and its session opened by its file: never the brief or the
// system text on its command line (`--append-system-prompt` would replace a
// trusted repository's APPEND_SYSTEM.md), never `--session-id` (which finds a
// session only in the directory it was made in).
//
// Mutation: drop `--approve` or `-e`, or pass the system text as a flag —
// this goes red.
func TestAPiRunIsHeadlessAndTrustsItsWorkingDirectory(t *testing.T) {
	t.Parallel()
	paths := Paths{Home: "/home/user"}
	req := sandbox.RunRequest{Model: "anthropic/claude-sonnet-4-5", Effort: "high", System: "SEAT TEXT",
		Brief: "BRIEF TEXT", Skills: []sandbox.Skill{{Name: "deploy", Description: "d", Body: "b"}}}
	script := piScript(req, Session{ID: "s-1"}, paths, paths.PiExtension(), time.Second)
	for _, want := range []string{
		`pi --mode json -p --approve --model 'anthropic/claude-sonnet-4-5' --thinking 'high' -e '/home/user/.pi/crewlet/extension.mjs' --skill '/home/user/.pi/crewlet/skills' "$@"`,
		`set -- "$@" --session "$f"`, `f='/home/user/.pi/sessions/s-1.jsonl'`,
		`printf '%s\n' '{"type":"crewlet.extension","cap":0}'`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %q:\n%s", want, script)
		}
	}
	for _, never := range []string{"SEAT TEXT", "BRIEF TEXT", "--append-system-prompt", "--session-id"} {
		if strings.Contains(script, never) {
			t.Errorf("the script holds %q", never)
		}
	}
	pinned := map[string]string{}
	for _, p := range (Pi{}).Pins(t.Context(), req, paths) {
		pinned[p.Name] = p.Value
	}
	if pinned["PI_CODING_AGENT_DIR"] != "/home/user/.pi/agent" || pinned["PI_OFFLINE"] != "1" {
		t.Errorf("pins = %v", pinned)
	}
}

// piGate runs the script's session gate for session in a home of its own,
// from a working directory of its own, and is what it settled: s, k, w, and
// the arguments it handed pi.
func piGate(t *testing.T, home, cwd string, session Session) []string {
	t.Helper()
	paths := Paths{Home: home}
	script := piHeader + "set --\n" + piSessionGate(session, paths, false) +
		`printf '%s\n' "$s" "$k" "$w" "$@"` + "\n"
	cmd := exec.Command("sh", "-c", script) //nolint:gosec // the runner's own script
	cmd.Dir = cwd
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the gate failed: %v\n%s", err, stderr.String())
	}
	return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
}

// piArchive is a carried archive of session's file holding body.
func piArchive(t *testing.T, session string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: ".pi/sessions/" + session + ".jsonl", Mode: 0o644,
		Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A NEW PI SESSION'S FILE IS ITS HEADER ALONE, under the id the runner minted
// and naming the run's working directory; a CARRIED SESSION IS RESTORED INTO
// THIS BOX'S WORKING DIRECTORY, its header written again naming it (pi
// continues a session only where its header says it was) and every other line
// as carried, its digest recorded; and a session neither held nor carried
// starts the minted fresh one and says why.
//
// Mutation: restore the file as carried, keep the old header's directory, or
// continue a session the box does not hold — this goes red.
func TestAPiSessionOpensInTheBoxsWorkingDirectory(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{"sh", "tar", "gzip", "cksum", "sed", "date"} {
		prereqtest.OnPath(t, tool, "the Pi session gate runs under sh with it", "install it")
	}
	const id = "22222222-2222-4222-8222-222222222222"
	cwd := t.TempDir()
	header := func(t *testing.T, file string) (map[string]any, []string) {
		t.Helper()
		raw, err := os.ReadFile(file) //nolint:gosec // the gate's own file
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
		var h map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &h); err != nil {
			t.Fatalf("the header %q: %v", lines[0], err)
		}
		return h, lines[1:]
	}

	t.Run("new", func(t *testing.T) {
		home := t.TempDir()
		got := piGate(t, home, cwd, Session{ID: id})
		file := Paths{Home: home}.PiSessionFile(id)
		if !slices.Equal(got, []string{id, "no", "", "--session", file}) {
			t.Fatalf("the gate settled %q", got)
		}
		h, rest := header(t, file)
		if h["id"] != id || h["cwd"] != cwd || h["type"] != "session" || h["version"] != 3.0 || len(rest) != 0 {
			t.Fatalf("the new session's file opens %v and holds %d lines more", h, len(rest))
		}
	})
	t.Run("carried", func(t *testing.T) {
		home := t.TempDir()
		paths := Paths{Home: home}
		if err := os.MkdirAll(paths.WorkDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		carried := piFixture(t, "steered.session.jsonl")
		if err := os.WriteFile(paths.SessionCarried(), piArchive(t, id, carried), 0o600); err != nil {
			t.Fatal(err)
		}
		got := piGate(t, home, cwd, Session{ID: id, Continues: true, Fresh: "fresh-1"})
		if !slices.Equal(got, []string{id, "yes", "", "--session", paths.PiSessionFile(id)}) {
			t.Fatalf("the gate settled %q", got)
		}
		h, rest := header(t, paths.PiSessionFile(id))
		if h["id"] != id || h["cwd"] != cwd {
			t.Fatalf("the restored file opens %v, want this box's directory %s", h, cwd)
		}
		want := strings.Split(strings.TrimSuffix(string(carried), "\n"), "\n")[1:]
		if !slices.Equal(rest, want) {
			t.Fatal("the restored file's lines after the header differ from those carried")
		}
		digests, err := os.ReadFile(paths.SessionDigests())
		if err != nil || !strings.HasPrefix(string(digests), id+" ") {
			t.Fatalf("the digests read %q (%v)", digests, err)
		}
	})
	t.Run("neither", func(t *testing.T) {
		home := t.TempDir()
		if err := os.MkdirAll(Paths{Home: home}.WorkDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		got := piGate(t, home, cwd, Session{ID: id, Continues: true, Fresh: "fresh-1"})
		if len(got) != 5 || got[0] != "fresh-1" || got[1] != "no" || !strings.Contains(got[2], "not in this box") ||
			got[4] != (Paths{Home: home}).PiSessionFile("fresh-1") {
			t.Fatalf("the gate settled %q", got)
		}
	})
}

// A PI RUN'S STREAM SAYS HOW IT ENDED, its exit code never (pi exits 0 on a
// model's error in JSON mode): an answer, the run's own spend summed over its
// requests and its compactions, the least window pi reported; a cap the
// extension enforced; a bridge that offered no tool; the provider's error,
// words alone; a resumed run's own request only; an extension that never ran,
// and a cap it did not hold, each a failure whatever the run answered.
//
// Mutation: read success off the last message whatever its stop, drop the
// compaction's usage, or take a run whose extension never ran for one that
// did — this goes red.
func TestAPiRunsStreamSaysHowItEnded(t *testing.T) {
	t.Parallel()
	unarmed := bytes.ReplaceAll(piStream(t, "steered", 0), []byte(`"customType":"crewlet-armed"`),
		[]byte(`"customType":"another"`))
	for _, tc := range []struct {
		name                string
		stream              []byte
		success, capped     bool
		says, text          string
		input, output, turn int
		window              int64
	}{
		{name: "tools", stream: piStream(t, "tool", 0), success: true, text: "Finished the tool tour.",
			input: 848, output: 96, turn: 8, window: 200000},
		{name: "a note answered", stream: piStream(t, "steered", 0), success: true, text: "Final answer of notes1.",
			input: 318, output: 36, turn: 3, window: 200000},
		{name: "compacted", stream: piStream(t, "compacted", 0), success: true, text: "Final after compaction.",
			input: 7512, output: 64, turn: 3, window: 8000},
		{name: "resumed", stream: piStream(t, "resumed", 0), success: true,
			text: "Hello from the stub, plain answer.", input: 106, output: 12, turn: 1, window: 200000},
		{name: "capped", stream: piStream(t, "capped", 1), capped: true, says: "turn cap of 1",
			input: 106, output: 12, turn: 1, window: 200000},
		{name: "a dead bridge", stream: piStream(t, "deadbridge", 0), says: "no tool of crewlet was offered",
			window: 200000},
		{name: "a rate limit", stream: piStream(t, "ratelimit", 0), says: `429 {"type":"error","error":{"type":"rate_limit_error"`,
			turn: 4, window: 200000},
		{name: "an extension that never ran", stream: unarmed, says: "extension never ran", text: "Final answer of notes1.",
			input: 318, output: 36, turn: 3, window: 200000},
		{name: "a cap not held", stream: piStream(t, "tool", 2), says: "turn cap of 2 was not enforced",
			text: "Finished the tool tour.", input: 848, output: 96, turn: 8, window: 200000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dec := &piEvents{}
			got := decodeAll(dec, tc.stream)
			if got.Success != tc.success || got.RoundCapped != tc.capped || !strings.Contains(got.Error, tc.says) ||
				got.Text != tc.text {
				t.Fatalf("result = success %v, capped %v, error %q, text %q", got.Success, got.RoundCapped,
					got.Error, got.Text)
			}
			if got.InputTokens != tc.input || got.OutputTokens != tc.output || got.Turns != tc.turn {
				t.Errorf("spend = %d in, %d out, %d turns; want %d, %d, %d", got.InputTokens, got.OutputTokens,
					got.Turns, tc.input, tc.output, tc.turn)
			}
			if w := dec.reportedWindow(); w != tc.window {
				t.Errorf("window = %d, want %d", w, tc.window)
			}
		})
	}
}

// piSessionLines is a fixture session's lines, each decoded.
func piSessionLines(t *testing.T, name string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for l := range bytes.Lines(piFixture(t, name)) {
		var m map[string]any
		if err := json.Unmarshal(l, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// piJoin is lines as a session file, each ended in a break.
func piJoin(t *testing.T, lines []map[string]any) []byte {
	t.Helper()
	var b bytes.Buffer
	for _, l := range lines {
		raw, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// piNoteAt is the index of the line holding the note handed over under inbox.
func piNoteAt(t *testing.T, lines []map[string]any, inbox string) int {
	t.Helper()
	for i, l := range lines {
		if d, ok := l["details"].(map[string]any); ok && d["inbox"] == inbox {
			return i
		}
	}
	t.Fatalf("no note under %s", inbox)
	return -1
}

// A PI NOTE IS HELD ONLY WHERE THE SESSION'S ACTIVE BRANCH SHOWS AN ANSWER AFTER
// IT, with no compaction between (which leaves it only inside a summary) and no
// edit taking it out first: handed over mid-run or at the finish and answered,
// held, a resumed session's too; killed before the answer, answered only by an
// error, folded by a compaction, on a branch the session left, or edited out,
// not held, so it is served again. A last line cut off decides nothing.
//
// Mutation: hold a note on its record alone, ignore compactions, read the file
// in order rather than along its branch, or count a torn last line — this
// goes red.
func TestAPiNoteIsHeldOnlyByAnAnswerOnItsBranch(t *testing.T) {
	t.Parallel()
	steered := piSessionLines(t, "steered.session.jsonl")
	at := piNoteAt(t, steered, "inbox-steer-1")
	answer := at + 1
	if role := steered[answer]["message"].(map[string]any)["role"]; role != "assistant" {
		t.Fatalf("the line after the note is %v, not its answer", role)
	}
	withStop := func(stop string) []byte {
		lines := piSessionLines(t, "steered.session.jsonl")
		lines[answer]["message"].(map[string]any)["stopReason"] = stop
		return piJoin(t, lines[:answer+1])
	}
	offBranch := func() []byte {
		lines := piSessionLines(t, "steered.session.jsonl")[:answer+1]
		lines[answer]["parentId"] = lines[at]["parentId"]
		return piJoin(t, lines)
	}
	edited := func() []byte {
		lines := piSessionLines(t, "steered.session.jsonl")[:answer+1]
		edit := map[string]any{"type": "context_edit", "id": "edit0001", "parentId": lines[at]["id"],
			"targetId": lines[at]["id"], "replacement": nil}
		lines[answer]["parentId"] = "edit0001"
		return piJoin(t, append(lines[:answer], edit, lines[answer]))
	}
	whole := piJoin(t, steered[:answer+1])
	for _, tc := range []struct {
		name   string
		record []byte
		inbox  string
		held   bool
	}{
		{name: "answered mid-run", record: piFixture(t, "steered.session.jsonl"), inbox: "inbox-steer-1", held: true},
		{name: "answered at the finish", record: piFixture(t, "settled.session.jsonl"), inbox: "inbox-final-1", held: true},
		{name: "carried and resumed", record: piFixture(t, "resumed.session.jsonl"), inbox: "inbox-steer-1", held: true},
		{name: "folded by a compaction", record: piFixture(t, "compacted.session.jsonl"), inbox: "inbox-comp-1"},
		{name: "killed before the answer", record: piJoin(t, steered[:at+1]), inbox: "inbox-steer-1"},
		{name: "answered by an error", record: withStop("error"), inbox: "inbox-steer-1"},
		{name: "answered then cut off", record: whole[:len(whole)-1], inbox: "inbox-steer-1"},
		{name: "answered, the line whole", record: whole, inbox: "inbox-steer-1", held: true},
		{name: "answered on another branch", record: offBranch(), inbox: "inbox-steer-1"},
		{name: "edited out before the answer", record: edited(), inbox: "inbox-steer-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := piNotesHeld(t.Context(), bytes.NewReader(tc.record),
				[]ServedTake{{InboxID: tc.inbox}, {InboxID: "never-served"}})
			if err != nil {
				t.Fatal(err)
			}
			if held := slices.Equal(got, []string{tc.inbox}); held != tc.held || (!tc.held && len(got) != 0) {
				t.Fatalf("held = %v, want %v held", got, tc.held)
			}
		})
	}
}

// A PI RUN'S EXTENSION IS WRITTEN WITH THE RUN'S OWN WIRING: every engine
// server registered and the tool bridge's own required, the notes route, the
// cap, the seat's system text told of notes, each skill by the absolute path it
// is written at with the name it is listed under; a fresh session's copy with
// the job's other token; and the agent directory's own server list empty, so
// no server there replaces one the extension registers.
//
// Mutation: leave the fresh copy on the continued token, drop the required
// bridge, or key a skill by a relative path — this goes red.
func TestAPiRunsExtensionCarriesItsOwnWiring(t *testing.T) {
	t.Parallel()
	box := sandbox.NewFakeSandbox("b")
	p := PathsFor(box)
	bridge := func(token string) map[string]sandbox.MCPServer {
		return map[string]sandbox.MCPServer{
			"crewlet": {Name: "crewlet", Transport: sandbox.TransportHTTP, URL: "http://e/mcp/" + token,
				AlwaysLoaded: true, CallTimeout: 90 * time.Second},
			"docs": {Name: "docs", Transport: sandbox.TransportHTTP, URL: "http://e/mcp/" + token + "/docs"},
		}
	}
	steer := func(token string) sandbox.SteerWiring {
		return sandbox.SteerWiring{Server: "crewlet", Tool: "steer", URL: "http://e/mcp/" + token + "/steer"}
	}
	loads := func(token string) sandbox.SkillWiring {
		return sandbox.SkillWiring{Server: "crewlet", Tool: "skill_loaded", URL: "http://e/mcp/" + token + "/skill"}
	}
	req := sandbox.RunRequest{System: "SEAT", MCPServers: bridge("old"), Steer: steer("old"), SkillLoads: loads("old"),
		Limits:      sandbox.Limits{MaxTurns: 7},
		Skills:      []sandbox.Skill{{Name: "deploy", Description: "Deploys.", Body: "Do it."}},
		FreshWiring: &sandbox.Wiring{MCPServers: bridge("new"), Steer: steer("new"), SkillLoads: loads("new")}}
	file, err := (Pi{}).WriteConfig(t.Context(), box, req, p)
	if err != nil || file != p.PiExtension() {
		t.Fatalf("WriteConfig = %q, %v", file, err)
	}
	read := func(path string) piExtensionSettings {
		t.Helper()
		raw, err := box.ReadFile(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		_, rest, ok := strings.Cut(string(raw), "const config = ")
		line, _, _ := strings.Cut(rest, "\n")
		var s piExtensionSettings
		if !ok || json.Unmarshal([]byte(line), &s) != nil {
			t.Fatalf("no settings in %s", path)
		}
		return s
	}
	skill := p.PiSkills() + "/crewlet:deploy/SKILL.md"
	for path, token := range map[string]string{p.PiExtension(): "old", p.PiExtensionFresh(): "new"} {
		s := read(path)
		if len(s.Servers) != 2 || s.Servers[0].Name != "crewlet" || s.Servers[0].Config["url"] != "http://e/mcp/"+token ||
			s.Servers[0].Config["exposure"] != "direct" || s.Servers[0].Config["timeout"] != 90.0 {
			t.Errorf("%s registers %+v", path, s.Servers)
		}
		if !slices.Equal(s.Required, []string{"crewlet"}) || s.Cap != 7 || s.Notes != "http://e/mcp/"+token+"/steer" ||
			s.SkillURL != "http://e/mcp/"+token+"/skill" || s.Skills[skill] != "crewlet:deploy" ||
			!strings.HasPrefix(s.System, "SEAT\n\n") {
			t.Errorf("%s is written %+v", path, s)
		}
	}
	if body, _ := box.ReadFile(t.Context(), skill); !strings.Contains(string(body), "Do it.") {
		t.Errorf("the skill reads %q", body)
	}
	if mcp, _ := box.ReadFile(t.Context(), p.PiAgentDir()+"/mcp.json"); string(mcp) != piHomeMCP {
		t.Errorf("the agent directory's servers read %q", mcp)
	}
}

// A PI SESSION CONTINUES IN A BOX NO OLDER THAN THE ONE THAT WROTE IT, both at
// or after 1.6.0, the release whose session format and hooks the runner reads.
//
// Mutation: drop either floor, or the order — this goes red.
func TestAPiSessionContinuesOnlyInABoxAsNewAsItsWriter(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		box, writer string
		ok          bool
	}{
		{"1.6.0", "1.6.0", true}, {"1.7.2", "1.6.0", true}, {"1.6.0", "1.7.0", false},
		{"1.5.2", "1.5.2", false}, {"1.6.0", "1.5.2", false},
	} {
		if ok, why := (Pi{}).Continues(tc.box, tc.writer); ok != tc.ok || ok == (why != "") {
			t.Errorf("Continues(%s, %s) = %v, %q", tc.box, tc.writer, ok, why)
		}
	}
}

// A CARRIED PI SESSION READS BACK AS ITS TRANSCRIPT: the brief, the model's
// words, each call by its tool and each result named by the call it answers,
// in recorded order, pi's own system messages and the extension's entries
// left out.
//
// Mutation: read a tool result's tool off the result rather than its call, or
// read the extension's entries as words — this goes red.
func TestACarriedPiSessionReadsBackAsItsTranscript(t *testing.T) {
	t.Parallel()
	const session = "11111111-1111-4111-8111-111111111111"
	got, err := newTranscriptBuilder(0, TranscriptReadBounds(), TranscriptFilter{}).decode(t.Context(), Pi{},
		bytes.NewReader(piArchive(t, session, piFixture(t, "tool.session.jsonl"))))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []EntryKind
	for _, e := range got.Entries {
		kinds = append(kinds, e.Kind)
	}
	if len(got.Entries) < 4 || got.Entries[0].Kind != EntryBrief || got.Entries[0].Text != "Do the scripted steps." {
		t.Fatalf("the transcript opens %v", kinds)
	}
	first := got.Entries[1:]
	if first[0].Kind != EntryModel || first[1].Kind != EntryToolCall || first[1].Tool != "bash" ||
		first[2].Kind != EntryToolResult || first[2].Tool != "bash" || !strings.Contains(first[2].Text, "shell-ok") {
		t.Fatalf("the first step reads %+v", first[:3])
	}
	if last := got.Entries[len(got.Entries)-1]; last.Kind != EntryModel || last.Text != "Finished the tool tour." {
		t.Fatalf("the transcript ends %+v", last)
	}
}
