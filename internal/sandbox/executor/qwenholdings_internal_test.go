package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// These cases read a Qwen Code session's kept chat file for which of a
// round's calls it holds ([readQwenHoldings]); contentverdict_internal_test.go
// holds the verdict's own rules. Every arm that can hold a call has a case
// where the session lacks the call, and holds none.

// qhSession is the session every synthetic chat file here is of.
const qhSession = "3333aaaa-1818-4333-8444-00000000000a"

// qhChat is a chat file as 0.28.4 writes one, each record on the chain after
// the one before it, built a record at a time.
type qhChat struct{ lines []string }

// uuid is record i's uuid, from 0.
func (qhChat) uuid(i int) string { return fmt.Sprintf("aaaaaaaa-0000-4000-8000-%012d", i+1) }

// add appends a record of type typ written by provenance, rest its members
// after the eight every record holds.
func (c *qhChat) add(typ, provenance, rest string) *qhChat {
	c.lines = append(c.lines, qhRecordOpen(len(c.lines), typ, provenance)+rest+`}`)
	return c
}

// qhRecordOpen opens record i of a chain, of type typ written by provenance:
// the eight members every record holds.
func qhRecordOpen(i int, typ, provenance string) string {
	parent := "null"
	if i > 0 {
		parent = `"` + qhChat{}.uuid(i-1) + `"`
	}
	return `{"uuid":"` + qhChat{}.uuid(i) + `","parentUuid":` + parent + `,"sessionId":"` + qhSession +
		`","timestamp":"2026-11-23T00:00:00.000Z","type":"` + typ + `","provenance":"` + provenance +
		`","cwd":"/work/proj","version":"0.28.4"`
}

// system appends a system record of subtype holding payload.
func (c *qhChat) system(subtype, payload string) *qhChat {
	return c.add("system", "system", `,"subtype":"`+subtype+`","systemPayload":`+payload)
}

// brief appends a person's prompt.
func (c *qhChat) brief() *qhChat {
	return c.add("user", "real_user", `,"executionContext":{"modelId":"m"},"message":{"role":"user","parts":`+
		`[{"text":"the brief"}]},"promptId":"p"`)
}

// step appends a model step making calls ([qhCallPart]).
func (c *qhChat) step(calls ...string) *qhChat {
	return c.add("assistant", "assistant_output", `,"model":"m","message":{"role":"model","parts":[`+
		strings.Join(calls, ",")+`]},"usageMetadata":{"promptTokenCount":1}`)
}

// done appends the answer to call id of tool, its output text.
func (c *qhChat) done(id, tool, text string) *qhChat {
	q := ohQuote(text)
	return c.add("tool_result", "tool_result", `,"message":{"role":"user","parts":[{"functionResponse":{"id":"`+id+
		`","name":"mcp__crewlet__`+tool+`","response":{"output":`+q+`}}}]},"toolCallResult":{"callId":"`+id+
		`","status":"success","resultDisplay":`+q+`,"executionStatus":"success"}`)
}

// failed appends the failure of call id of tool, its text text.
func (c *qhChat) failed(id, tool, text string) *qhChat {
	q := ohQuote(text)
	return c.add("tool_result", "tool_result", `,"message":{"role":"user","parts":[{"functionResponse":{"id":"`+id+
		`","name":"mcp__crewlet__`+tool+`","response":{"error":`+q+`}}}]},"toolCallResult":{"callId":"`+id+
		`","status":"error","resultDisplay":`+q+`,"error":{},"errorType":"mcp_tool_error","executionStatus":"error"}`)
}

// edit replaces old with repl in record i, failing the case where it is not
// there.
func (c *qhChat) edit(t *testing.T, i int, old, repl string) *qhChat {
	t.Helper()
	if !strings.Contains(c.lines[i], old) {
		t.Fatalf("record %d holds no %q", i+1, old)
	}
	c.lines[i] = strings.Replace(c.lines[i], old, repl, 1)
	return c
}

// file is the chat file, each record ended by a break.
func (c *qhChat) file() string { return strings.Join(c.lines, "\n") + "\n" }

// qhCallPart is a step's call of the engine's tool, by id, with args.
func qhCallPart(id, tool, args string) string {
	return `{"functionCall":{"id":"` + id + `","name":"mcp__crewlet__` + tool + `","args":` + args + `}}`
}

// qhRound is a session that made one call of echo and had it answered.
func qhRound() *qhChat {
	c := &qhChat{}
	return c.system("session_approval_mode", `{"mode":"yolo"}`).brief().
		step(qhCallPart("call_1", "echo", `{"t":1}`)).done("call_1", "echo", "done")
}

// qhTar is session's chat file body as a kept archive holds it, unpacked.
func qhTar(t testing.TB, session, body string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: "projects/-work-proj/chats/" + session + ".jsonl",
		Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// qhRead reads file, qhSession's chat file, from start, for calls.
func qhRead(t *testing.T, file string, start sandbox.RunStart, calls ...sandbox.LoggedCall) (sandbox.SessionHoldings,
	error) {
	t.Helper()
	return readQwenHoldings(t.Context(), bytes.NewReader(qhTar(t, qhSession, file)), qhSession, start,
		sandbox.HoldingsAsk{Calls: calls, Resumer: "0.28.4"})
}

// qhArchive is qhSession's archive as its row describes it, written by
// version, its run having begun the session.
func qhArchive(version string) sandbox.SessionArchive {
	return sandbox.SessionArchive{Harness: QwenCodeName, SessionID: qhSession, Format: qwenArchiveFormat,
		Version: version, RunStart: ohBegan}
}

// qhOpen opens archive, a gzipped tar, as a store hands it out.
func qhOpen(archive []byte) func(context.Context) (io.ReadCloser, error) {
	return func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(archive)), nil }
}

// A QWEN READING READS ONLY RELEASES IT WAS MEASURED ON: the archive's writer
// and the ask's resumer, both 0.28.4, compared whole; any other, or one
// padded, is no reading and nothing is opened.
//
// Mutation: compare by prefix, or drop the writer's comparison with the
// resumer, and a case goes red.
func TestAQwenReadingReadsOnlyReleasesItWasMeasuredOn(t *testing.T) {
	t.Parallel()
	call := ohCall(1, "echo", `{"t":1}`, "done")
	archive := ohGzip(t, string(qhTar(t, qhSession, qhRound().file())))
	for name, tc := range map[string]struct {
		version, resumer string
		read             bool
	}{
		"the release measured":            {"0.28.4", "0.28.4", true},
		"an older release":                {"0.28.3", "0.28.3", false},
		"a writer other than the resumer": {"0.28.3", "0.28.4", false},
		"a release it opens":              {"0.28.40", "0.28.40", false},
		"a member padded with blanks":     {" 0.28.4", " 0.28.4", false},
		"a resumer nobody recorded":       {"", "", false},
	} {
		opened := false
		got, err := NewQwenCode().ReadHoldings(t.Context(), qhArchive(tc.version),
			func(context.Context) (io.ReadCloser, error) {
				opened = true
				return io.NopCloser(bytes.NewReader(archive)), nil
			}, sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{call}, Resumer: tc.resumer})
		switch {
		case tc.read && (err != nil || !got.Read):
			t.Errorf("%s: a reading = %+v, %v; want it read", name, got, err)
		case !tc.read && (got.Read || err == nil || opened):
			t.Errorf("%s: a reading = %+v, %v, opened %t; want none, nothing opened", name, got, err, opened)
		}
	}
	padded := sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{call}, Resumer: "0.28.4"}
	if err := (QwenCode{}).checkHoldings(qhArchive("0.28.3"), padded); err == nil {
		t.Error("a writer other than the resumer passes the reader's check; want it refused")
	}
}

// THE QWEN READING'S FLOOR IS ITS OLDEST RELEASE by its numbers, whatever the
// set's order.
//
// Not parallel: it swaps the set.
//
// Mutation: answer the set's first member, and this goes red.
func TestTheQwenReadingsFloorIsItsOldestRelease(t *testing.T) {
	saved := qwenHoldingsReleases
	t.Cleanup(func() { qwenHoldingsReleases = saved })
	for want, set := range map[string][]string{
		"0.28.4": {"0.29.0", "0.28.4", "0.28.10"},
		"0.9.1":  {"0.10.0", "0.9.1"},
	} {
		qwenHoldingsReleases = set
		if got := (QwenCode{}).holdingsFloor(); got != want {
			t.Errorf("the floor of %q is %q; want %q", set, got, want)
		}
	}
}

// THE QWEN READING'S RELEASES ARE THE RELEASES MEASURED: 0.28.4, the release
// the build drives.
//
// Mutation: empty the set, or add a release, and this goes red.
func TestTheQwenReadingsReleasesAreTheReleasesMeasured(t *testing.T) {
	t.Parallel()
	if want := []string{"0.28.4"}; !slices.Equal(qwenHoldingsReleases, want) {
		t.Errorf("the reading's releases are %q; want those measured, %q", qwenHoldingsReleases, want)
	}
	for _, release := range qwenHoldingsReleases {
		if _, ok := releaseNumbers(release); !ok || release != qwenRelease {
			t.Errorf("%q is unreadable or not the release the build drives", release)
		}
	}
}

// A QWEN ASK IS VALIDATED BEFORE ANY READ: a session id that is no canonical
// lowercase uuid, a run with no start or one named by a message, and an ask a
// log keyed by place cannot make are no reading.
//
// Mutation: skip a check, and a case goes red.
func TestAQwenAskIsValidatedBeforeAnyRead(t *testing.T) {
	t.Parallel()
	good := ohCall(1, "echo", `{}`, "done")
	with := func(edit func(*sandbox.SessionArchive)) sandbox.SessionArchive {
		a := qhArchive("0.28.4")
		edit(&a)
		return a
	}
	for name, tc := range map[string]struct {
		calls   []sandbox.LoggedCall
		archive sandbox.SessionArchive
		read    bool
	}{
		"an ask the log can make":  {[]sandbox.LoggedCall{good}, qhArchive("0.28.4"), true},
		"a run after its lines":    {[]sandbox.LoggedCall{good}, with(func(a *sandbox.SessionArchive) { a.RunStart = sandbox.RunStart{Lines: 17} }), true},
		"no call":                  {nil, qhArchive("0.28.4"), false},
		"keyed by its id":          {[]sandbox.LoggedCall{func() sandbox.LoggedCall { c := good; c.ID = "call_1"; return c }()}, qhArchive("0.28.4"), false},
		"held for a person":        {[]sandbox.LoggedCall{func() sandbox.LoggedCall { c := good; c.Held = true; return c }()}, qhArchive("0.28.4"), false},
		"a run with no start":      {[]sandbox.LoggedCall{good}, with(func(a *sandbox.SessionArchive) { a.RunStart = sandbox.RunStart{} }), false},
		"a run after a message":    {[]sandbox.LoggedCall{good}, with(func(a *sandbox.SessionArchive) { a.RunStart = sandbox.RunStart{After: "msg_a"} }), false},
		"an uppercase session id":  {[]sandbox.LoggedCall{good}, with(func(a *sandbox.SessionArchive) { a.SessionID = strings.ToUpper(qhSession) }), false},
		"a session id of no uuid":  {[]sandbox.LoggedCall{good}, with(func(a *sandbox.SessionArchive) { a.SessionID = "ses_0a1b2c3d" }), false},
		"a session id's file name": {[]sandbox.LoggedCall{good}, with(func(a *sandbox.SessionArchive) { a.SessionID = qhSession + ".jsonl" }), false},
	} {
		err := (QwenCode{}).checkHoldings(tc.archive, sandbox.HoldingsAsk{Calls: tc.calls, Resumer: "0.28.4"})
		if (err == nil) != tc.read {
			t.Errorf("%s: checkHoldings = %v; want read %t", name, err, tc.read)
		}
	}
}

// A QWEN ASK PAST THE LOG'S BOUNDS IS NEVER READ: 257 calls in flight, or two
// counts of dropped calls with no name, are no reading; 256 in flight are.
//
// Mutation: drop the shared check from the reader's, and this goes red.
func TestAQwenAskPastTheLogsBoundsIsNeverRead(t *testing.T) {
	t.Parallel()
	inFlight := func(n int) []sandbox.CallIdentity {
		return slices.Repeat([]sandbox.CallIdentity{{Name: "x", InFlight: true}}, n)
	}
	for name, tc := range map[string]struct {
		unlisted []sandbox.CallIdentity
		read     bool
	}{
		"256 in flight":      {inFlight(sandbox.MaxCallsInFlight), true},
		"257 in flight":      {inFlight(sandbox.MaxCallsInFlight + 1), false},
		"two unnamed counts": {[]sandbox.CallIdentity{{}, {}}, false},
	} {
		err := (QwenCode{}).checkHoldings(qhArchive("0.28.4"), sandbox.HoldingsAsk{Resumer: "0.28.4",
			Calls: []sandbox.LoggedCall{ohCall(1, "echo", `{}`, "done")}, Unlisted: tc.unlisted})
		if (err == nil) != tc.read {
			t.Errorf("%s: checkHoldings = %v; want read %t", name, err, tc.read)
		}
	}
}

// qhManifest is what the release's fixtures say of each case
// (testdata/qwen/<release>/holdings/manifest.json.gz).
type qhManifest struct {
	Prefix  string `json:"prefix"`
	Release string `json:"release"`
	Cases   map[string]struct {
		Session   string `json:"session"`
		Compacted bool   `json:"compacted"`
		Reading   string `json:"reading"`
		Start     struct {
			Began bool  `json:"began"`
			Lines int64 `json:"lines"`
		} `json:"start"`
		Calls []struct {
			Appended    int             `json:"appended"`
			Name        string          `json:"name"`
			Args        json.RawMessage `json:"args"`
			Output      string          `json:"output"`
			Failed      bool            `json:"failed"`
			InFlight    bool            `json:"in_flight"`
			Held        bool            `json:"held"`
			Adversarial bool            `json:"adversarial"`
		} `json:"calls"`
	} `json:"cases"`
}

// qhFixture is one case of the release's own runs: its chat file, the
// session, the run's start, the round's logged calls and those in flight, the
// calls the rules hold, and whether a compaction followed the start.
type qhFixture struct {
	chat      string
	session   string
	start     sandbox.RunStart
	calls     []sandbox.LoggedCall
	unlisted  []sandbox.CallIdentity
	held      []int
	compacted bool
}

// ask is the fixture's ask: every answered call asked, each output cut as
// the log cuts it, the in-flight calls unlisted.
func (f qhFixture) ask() sandbox.HoldingsAsk {
	return sandbox.HoldingsAsk{Calls: f.calls, Unlisted: f.unlisted, Resumer: "0.28.4"}
}

// read reads chat, the fixture's chat file or an edit of it, from start.
func (f qhFixture) read(t testing.TB, chat string, start sandbox.RunStart) (sandbox.SessionHoldings, error) {
	t.Helper()
	ctx := context.Background()
	return readQwenHoldings(ctx, bytes.NewReader(qhTar(t, f.session, chat)), f.session, start, f.ask())
}

// lines are the fixture's chat file's records, edit at will.
func (f qhFixture) lines() []string {
	return strings.SplitAfter(strings.TrimSuffix(f.chat, "\n"), "\n")
}

// qhJoin is lines joined back into a chat file.
func qhJoin(lines []string) string { return strings.TrimSuffix(strings.Join(lines, ""), "\n") + "\n" }

// qhFixtures are the release's cases.
func qhFixtures(tb testing.TB) map[string]qhFixture {
	tb.Helper()
	dir := filepath.Join("testdata", "qwen", "0.28.4", "holdings")
	var manifest qhManifest
	if err := json.Unmarshal(qhGunzip(tb, filepath.Join(dir, "manifest.json.gz")), &manifest); err != nil {
		tb.Fatal(err)
	}
	if manifest.Release != "0.28.4" || manifest.Prefix != qwenBridgedPrefix() {
		tb.Fatalf("the manifest is of %q under %q", manifest.Release, manifest.Prefix)
	}
	out := map[string]qhFixture{}
	for name, m := range manifest.Cases {
		if m.Reading != "read" {
			tb.Fatalf("%s: a reading of %q; every case reads", name, m.Reading)
		}
		f := qhFixture{chat: string(qhGunzip(tb, filepath.Join(dir, name+".chat.jsonl.gz"))), session: m.Session,
			start: sandbox.RunStart{Began: m.Start.Began, Lines: m.Start.Lines}, compacted: m.Compacted}
		for _, c := range m.Calls {
			if c.InFlight {
				f.unlisted = append(f.unlisted, sandbox.CallIdentity{Name: c.Name, InFlight: true})
				continue
			}
			logged := ohCut(ohCall(c.Appended, c.Name, string(c.Args), c.Output))
			logged.Failed = c.Failed
			f.calls = append(f.calls, logged)
			if c.Held {
				f.held = append(f.held, c.Appended)
			}
		}
		out[name] = f
	}
	return out
}

// qhGunzip is the gzipped file's plain bytes.
func qhGunzip(tb testing.TB, file string) []byte {
	tb.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		tb.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		tb.Fatal(err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		tb.Fatal(err)
	}
	return plain
}

// qhHeld is the places of the fixture's calls h holds, failing the case where
// it read nothing.
func qhHeld(t *testing.T, f qhFixture, h sandbox.SessionHoldings, err error) []int {
	t.Helper()
	return ohHeld(t, h, err, f.calls...)
}

// ONLY A QWEN PART AFTER THE RUN'S START COUNTS: a continued session whose
// run began after line 17 holds its own run's alpha and charlie and tells
// delta, whose answer was cut off; where its run's alpha went unanswered, the
// first run's alpha before the start holds nothing. Read from a line before
// the start, the line after it is no brief, and the reading reads none.
//
// Mutation: ignore the start, or move it a line either way, and a case goes
// red.
func TestOnlyAQwenPartAfterTheRunsStartCounts(t *testing.T) {
	t.Parallel()
	fixtures := qhFixtures(t)
	ck, repeat := fixtures["continued-killed"], fixtures["continued-killed-repeat"]
	h, err := ck.read(t, ck.chat, ck.start)
	if got := qhHeld(t, ck, h, err); !slices.Equal(got, []int{1, 2}) {
		t.Errorf("the continued run: held %v; want its alpha and charlie, 1 and 2", got)
	}
	h, err = repeat.read(t, repeat.chat, repeat.start)
	if got := qhHeld(t, repeat, h, err); len(got) != 0 {
		t.Errorf("the continued run with its alpha unanswered: held %v; want none, the first run's alpha before "+
			"the start", got)
	}
	for _, lines := range []int64{ck.start.Lines - 1, ck.start.Lines + 1} {
		if h, err := ck.read(t, ck.chat, sandbox.RunStart{Lines: lines}); err == nil || h.Read {
			t.Errorf("a run read as beginning after line %d: %+v; want no reading", lines, h)
		}
	}
}

// THE QWEN RUN'S START LINE IS THE RUN BEFORE'S: a step on it opens no call
// the run can answer, so its call answered after the brief is no reading,
// never held; a compaction on it leaves the reading uncompacted, the run's
// call held.
//
// Mutation: count a step on the start line, or take a compaction on it, and
// a case goes red.
func TestAQwenRunsStartLineIsTheRunBefores(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "echo", `{"t":1}`, "done")
	start := sandbox.RunStart{Lines: 2}
	step := (&qhChat{}).system("session_approval_mode", `{"mode":"yolo"}`).
		step(qhCallPart("call_1", "echo", `{"t":1}`)).brief().done("call_1", "echo", "done")
	if h, err := qhRead(t, step.file(), start, c); err == nil || h.Read {
		t.Errorf("a call on the start line answered after the brief: %+v; want no reading", h)
	}
	compaction := (&qhChat{}).system("session_approval_mode", `{"mode":"yolo"}`).
		system("chat_compression", `{"compressedHistory":[]}`).brief().
		step(qhCallPart("call_1", "echo", `{"t":1}`)).done("call_1", "echo", "done")
	h, err := qhRead(t, compaction.file(), start, c)
	if got := ohHeld(t, h, err, c); !slices.Equal(got, []int{1}) || h.Compacted {
		t.Errorf("a compaction on the start line: held %v, compacted %t; want the run's call, uncompacted", got,
			h.Compacted)
	}
}

// A QWEN FILE SHORTER THAN ITS START IS NOT READ: the continued session cut
// to 16 lines, one short of where its run began, is no reading.
//
// Mutation: drop the length check, and this goes red.
func TestAQwenFileShorterThanItsStartIsNotRead(t *testing.T) {
	t.Parallel()
	ck := qhFixtures(t)["continued-killed"]
	cut := qhJoin(ck.lines()[:16])
	if h, err := ck.read(t, cut, ck.start); err == nil || h.Read {
		t.Errorf("a file of 16 lines, its run begun after 17: %+v; want no reading", h)
	}
	if h, err := ck.read(t, qhJoin(ck.lines()[:17]), ck.start); err != nil || !h.Read {
		t.Errorf("a file of exactly the lines its run began after: %+v, %v; want it read", h, err)
	}
}

// A QWEN PART BEFORE THE NEWEST COMPACTION NEVER COUNTS: the captured
// session compressed after its call, and its call is told, the reading
// marked compacted; a synthetic compaction after a part drops it, its step
// open or closed, and one before the part does not.
//
// Mutation: keep the parts a compaction drops, and this goes red.
func TestAQwenPartBeforeTheNewestCompactionNeverCounts(t *testing.T) {
	t.Parallel()
	f := qhFixtures(t)["compacted"]
	h, err := f.read(t, f.chat, f.start)
	if got := qhHeld(t, f, h, err); len(got) != 0 || !h.Compacted {
		t.Errorf("the compacted capture: held %v, compacted %t; want none held, compacted", got, h.Compacted)
	}
	c := ohCall(1, "echo", `{"t":1}`, "done")
	after := qhRound().system("chat_compression", `{"info":{},"compressedHistory":[]}`)
	h, err = qhRead(t, after.file(), ohBegan, c)
	if got := ohHeld(t, h, err, c); len(got) != 0 || !h.Compacted {
		t.Errorf("a part before a compaction: held %v, compacted %t; want none, compacted", got, h.Compacted)
	}
	committed := qhRound().step(qhCallPart("call_2", "echo", `{"t":2}`)).
		system("chat_compression", `{"compressedHistory":[]}`)
	h, err = qhRead(t, committed.file(), ohBegan, c)
	if got := ohHeld(t, h, err, c); len(got) != 0 {
		t.Errorf("a part of a closed step before a compaction: held %v; want none", got)
	}
	before := (&qhChat{}).system("chat_compression", `{"compressedHistory":[]}`).brief().
		step(qhCallPart("call_1", "echo", `{"t":1}`)).done("call_1", "echo", "done")
	h, err = qhRead(t, before.file(), ohBegan, c)
	if got := ohHeld(t, h, err, c); !slices.Equal(got, []int{1}) {
		t.Errorf("a part after a compaction: held %v; want its call", got)
	}
}

// ONLY A PART QWEN HANDS ITS MODEL VERBATIM COUNTS: the measured round holds
// its eight verbatim calls and the batch over Qwen's budget its four; a held
// part given `parts`, a type of failure other than the tool's, a status
// other than success, a key of its own, a role other than the user's, Qwen's
// words for an empty output, or a stub logged as the bridge's output is not
// counted (or is no reading), and its call is told.
//
// Mutation: drop any of the checks, and a case goes red.
func TestOnlyAPartQwenHandsItsModelVerbatimCounts(t *testing.T) {
	t.Parallel()
	fixtures := qhFixtures(t)
	for name, want := range map[string][]int{
		"round":        {1, 2, 3, 5, 6, 10, 13, 18},
		"batch-budget": {10, 11, 12, 13},
	} {
		f := fixtures[name]
		h, err := f.read(t, f.chat, f.start)
		if got := qhHeld(t, f, h, err); !slices.Equal(got, want) {
			t.Errorf("%s: held %v; want %v", name, got, want)
		}
	}
	c := ohCall(1, "echo", `{"t":1}`, "done")
	const answer, result = 3, 3
	for name, edit := range map[string]func(*qhChat) *qhChat{
		"an answer given parts": func(r *qhChat) *qhChat {
			return r.edit(t, answer, `"response":{`, `"parts":[{"text":"x"}],"response":{`)
		},
		"a response given parts": func(r *qhChat) *qhChat {
			return r.edit(t, answer, `"response":{"output":"done"}`, `"response":{"output":"done","parts":[]}`)
		},
		"a failure of invalid parameters": func(r *qhChat) *qhChat {
			return r.edit(t, result, `"status":"success"`, `"status":"error"`).
				edit(t, result, `"executionStatus":"success"`, `"error":{},"errorType":"invalid_tool_params",`+
					`"executionStatus":"error"`)
		},
		"a status cancelled": func(r *qhChat) *qhChat {
			return r.edit(t, result, `"status":"success"`, `"status":"cancelled"`)
		},
		"a run not a success": func(r *qhChat) *qhChat {
			return r.edit(t, result, `"executionStatus":"success"`, `"executionStatus":"cancelled"`)
		},
		"a result with a key of its own": func(r *qhChat) *qhChat {
			return r.edit(t, result, `"executionStatus":"success"`, `"executionStatus":"success","extra":1`)
		},
		"an answer with a key of its own": func(r *qhChat) *qhChat {
			return r.edit(t, answer, `"response":{`, `"willContinue":false,"response":{`)
		},
		"a call with a key of its own": func(r *qhChat) *qhChat {
			return r.edit(t, 2, `"args":{"t":1}`, `"args":{"t":1},"thoughtSignature":"s"`)
		},
		"a role of the model's": func(r *qhChat) *qhChat {
			return r.edit(t, answer, `"message":{"role":"user"`, `"message":{"role":"model"`)
		},
		"another call's id in the result": func(r *qhChat) *qhChat {
			return r.edit(t, result, `"callId":"call_1"`, `"callId":"call_2"`)
		},
		"an answer of another name": func(r *qhChat) *qhChat {
			return r.edit(t, answer, `"name":"mcp__crewlet__echo","response"`, `"name":"mcp__crewlet__other",`+
				`"response"`)
		},
		"an output no string": func(r *qhChat) *qhChat {
			return r.edit(t, answer, `"output":"done"`, `"output":["done"]`)
		},
		"a response of an output and a failure": func(r *qhChat) *qhChat {
			return r.edit(t, answer, `"output":"done"`, `"output":"done","error":"done"`)
		},
	} {
		h, err := qhRead(t, edit(qhRound()).file(), ohBegan, c)
		if err == nil && slices.Equal(ohHeld(t, h, err, c), []int{1}) {
			t.Errorf("%s: its call held; want it told", name)
		}
	}
	h, err := qhRead(t, qhRound().file(), ohBegan, c)
	if got := ohHeld(t, h, err, c); !slices.Equal(got, []int{1}) {
		t.Fatalf("the unedited round: held %v; want its call", got)
	}
	fail := ohCall(1, "fail", `{}`, "probe failure")
	fail.Failed = true
	failure := func() *qhChat {
		return (&qhChat{}).brief().step(qhCallPart("call_1", "fail", `{}`)).failed("call_1", "fail", "probe failure")
	}
	h, err = qhRead(t, failure().file(), ohBegan, fail)
	if got := ohHeld(t, h, err, fail); !slices.Equal(got, []int{1}) {
		t.Fatalf("the unedited failure: held %v; want its call", got)
	}
	for name, edit := range map[string]func(*qhChat) *qhChat{
		"a failure of invalid parameters": func(r *qhChat) *qhChat {
			return r.edit(t, 2, `"errorType":"mcp_tool_error"`, `"errorType":"invalid_tool_params"`)
		},
		"a failure with words of its own": func(r *qhChat) *qhChat {
			return r.edit(t, 2, `"error":{}`, `"error":{"message":"x"}`)
		},
		"a failure that ran": func(r *qhChat) *qhChat {
			return r.edit(t, 2, `"executionStatus":"error"`, `"executionStatus":"success"`)
		},
		"a failure beside an output": func(r *qhChat) *qhChat {
			return r.edit(t, 2, `"response":{"error"`, `"response":{"output":"probe failure","error"`)
		},
	} {
		h, err := qhRead(t, edit(failure()).file(), ohBegan, fail)
		if err == nil && slices.Equal(ohHeld(t, h, err, fail), []int{1}) {
			t.Errorf("%s: its call held; want it told", name)
		}
	}
	words := "(mcp__crewlet__echo completed with no output)"
	logged := ohCall(1, "echo", `{"t":1}`, words)
	h, err = qhRead(t, (&qhChat{}).brief().step(qhCallPart("call_1", "echo", `{"t":1}`)).
		done("call_1", "echo", words).file(), ohBegan, logged)
	if got := ohHeld(t, h, err, logged); len(got) != 0 {
		t.Errorf("Qwen's words for an empty output: held %v; want it told", got)
	}
	for _, stub := range qwenStubPrefixes {
		text := stub + " the rest of the stub"
		logged := ohCall(1, "echo", `{"t":1}`, text)
		h, err := qhRead(t, (&qhChat{}).brief().step(qhCallPart("call_1", "echo", `{"t":1}`)).
			done("call_1", "echo", text).file(), ohBegan, logged)
		if got := ohHeld(t, h, err, logged); len(got) != 0 {
			t.Errorf("a part opening %q, logged as is: held %v; want it told", stub, got)
		}
	}
}

// A QWEN CALL ITS CUT LEFT UNANSWERED IS TOLD: killed mid-batch, its answered
// echo has no tool result and is told, whatever the telemetry and the bridge
// say; killed in its second round, the first round's echo is held.
//
// Mutation: count the telemetry's call or the bridge's answer, and a case
// goes red.
func TestAQwenCallItsCutLeftUnansweredIsTold(t *testing.T) {
	t.Parallel()
	fixtures := qhFixtures(t)
	for name, want := range map[string][]int{
		"killed-midbatch":     nil,
		"readonly-killed":     nil,
		"killed-second-round": {1},
		"killed-answering":    {1, 2},
	} {
		f := fixtures[name]
		h, err := f.read(t, f.chat, f.start)
		if got := qhHeld(t, f, h, err); !slices.Equal(got, want) {
			t.Errorf("%s: held %v; want %v", name, got, want)
		}
	}
}

// A QWEN TOOL NAME ANOTHER SERVER CAN SPELL IS NEVER HELD: the three
// captured collisions (a server `crewlet__dup` at the user's level and in a
// repository's settings, a server `crewlet_`) recorded a second server's call
// under a bridge-looking name, and an ask naming the bridge's `dup__x` or
// `_x` with the second server's output holds nothing; a 49-character tool,
// which Qwen's cut of a hashed name can reach, is never held, a 48-character
// one is, as `echo` is.
//
// Mutation: admit `__`, an opening `_` or 49 characters, and a case goes red.
func TestAQwenToolNameAnotherServerCanSpellIsNeverHeld(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"collision", "collision-underscore", "collision-workspace"} {
		f := qhFixtures(t)[name]
		h, err := f.read(t, f.chat, f.start)
		if got := qhHeld(t, f, h, err); len(got) != 0 {
			t.Errorf("%s: held %v; want the second server's call told", name, got)
		}
	}
	for tool, held := range map[string]bool{
		"echo":                  true,
		strings.Repeat("a", 48): true,
		strings.Repeat("a", 49): false,
		"dup__x":                false,
		"_x":                    false,
		"x-y_z":                 true,
	} {
		c := ohCall(1, tool, `{}`, "done")
		file := (&qhChat{}).brief().step(qhCallPart("call_1", tool, `{}`)).done("call_1", tool, "done").file()
		h, err := qhRead(t, file, ohBegan, c)
		if got := ohHeld(t, h, err, c); slices.Equal(got, []int{1}) != held {
			t.Errorf("tool %q: held %v; want held %t", tool, got, held)
		}
	}
}

// A QWEN RECORD REPEATING A UUID LEAVES THE SESSION UNREAD: the continued
// session with line 33's uuid set to line 5's (line 34's parent with it, the
// chain otherwise whole), which a load measured to replay without lines 6 to
// 33, is no reading.
//
// Mutation: drop the uuids' check, and this goes red.
func TestAQwenRecordRepeatingAUuidLeavesTheSessionUnread(t *testing.T) {
	t.Parallel()
	ck := qhFixtures(t)["continued-killed"]
	lines := ck.lines()
	uuidOf := func(line string) string {
		var r struct{ UUID string }
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		return r.UUID
	}
	old, repeated := uuidOf(lines[32]), uuidOf(lines[4])
	lines[32] = strings.Replace(lines[32], old, repeated, 1)
	lines[33] = strings.Replace(lines[33], old, repeated, 1)
	if h, err := ck.read(t, qhJoin(lines), ck.start); err == nil || h.Read {
		t.Errorf("a uuid repeated: %+v; want no reading", h)
	}
}

// A QWEN CHAIN ITS LOAD WOULD NOT WALK IS NOT READ: a record whose parent is
// not the record before it, a first record with a parent, a later one with
// none, and a record of another session are each no reading.
//
// Mutation: drop the chain's check, and a case goes red.
func TestAQwenChainItsLoadWouldNotWalkIsNotRead(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "echo", `{"t":1}`, "done")
	r := qhRound()
	for name, file := range map[string]string{
		"a parent not the record before": r.edit(t, 3, `"parentUuid":"`+r.uuid(2)+`"`,
			`"parentUuid":"`+r.uuid(1)+`"`).file(),
		"a first record with a parent": qhRound().edit(t, 0, `"parentUuid":null`,
			`"parentUuid":"`+r.uuid(5)+`"`).file(),
		"a later record with none": qhRound().edit(t, 2, `"parentUuid":"`+r.uuid(1)+`"`,
			`"parentUuid":null`).file(),
		"another session's record": qhRound().edit(t, 1, qhSession, "33333333-1818-4333-8444-000000000002").file(),
		"a uuid not lowercase":     qhRound().edit(t, 3, `"uuid":"`+r.uuid(3)+`"`, `"uuid":"`+strings.ToUpper(r.uuid(3))+`"`).file(),
	} {
		if h, err := qhRead(t, file, ohBegan, c); err == nil || h.Read {
			t.Errorf("%s: %+v; want no reading", name, h)
		}
	}
}

// A MARKED CHANGE ENDS THE QWEN READING: a record of a type, subtype, member
// or part 0.28.4 does not write, another release's record, a key written
// twice, a carriage return, a blank line, a line no object, a compaction
// whose history is no list, and a tool result with a subtype are each no
// reading.
//
// Mutation: drop any one check, and its case goes red.
func TestAMarkedChangeEndsTheQwenReading(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "echo", `{"t":1}`, "done")
	const brief, step, answer = 1, 2, 3
	for name, edit := range map[string]func(*qhChat) *qhChat{
		"an unknown type": func(r *qhChat) *qhChat { return r.edit(t, brief, `"type":"user"`, `"type":"person"`) },
		"an unknown subtype": func(r *qhChat) *qhChat {
			return r.edit(t, 0, `"subtype":"session_approval_mode"`, `"subtype":"session_sources_snapshot"`)
		},
		"an unknown member": func(r *qhChat) *qhChat { return r.edit(t, brief, `"promptId":"p"`, `"promptId":"p","x":1`) },
		"a member missing":  func(r *qhChat) *qhChat { return r.edit(t, step, `,"usageMetadata":{"promptTokenCount":1}`, ``) },
		"an unknown part": func(r *qhChat) *qhChat {
			return r.edit(t, step, `"parts":[`, `"parts":[{"inlineData":{}},`)
		},
		"a person's text no string": func(r *qhChat) *qhChat {
			return r.edit(t, brief, `{"text":"the brief"}`, `{"text":1}`)
		},
		"another release's record": func(r *qhChat) *qhChat {
			return r.edit(t, brief, `"version":"0.28.4"`, `"version":"0.28.3"`)
		},
		"a key twice": func(r *qhChat) *qhChat {
			return r.edit(t, answer, `"toolCallResult":`, `"toolCallResult":{},"toolCallResult":`)
		},
		"a key twice in a call": func(r *qhChat) *qhChat {
			return r.edit(t, step, `"args":{"t":1}`, `"args":{"t":1},"args":{"t":1}`)
		},
		// Where JSON takes it as space, so only the line's own rule refuses it.
		"a carriage return between members": func(r *qhChat) *qhChat {
			return r.edit(t, brief, `,"promptId":"p"`, `,`+"\r"+`"promptId":"p"`)
		},
		"a carriage return before the break": func(r *qhChat) *qhChat {
			r.lines[brief] += "\r"
			return r
		},
		"a blank line":     func(r *qhChat) *qhChat { r.lines[brief] += "\n"; return r },
		"a line no object": func(r *qhChat) *qhChat { r.lines[brief] += "\n[]"; return r },
		"a second value":   func(r *qhChat) *qhChat { r.lines[brief] += " {}"; return r },
		"a history no list": func(r *qhChat) *qhChat {
			return r.system("chat_compression", `{"compressedHistory":{}}`)
		},
		"a compaction with no history": func(r *qhChat) *qhChat {
			return r.system("chat_compression", `{"info":{}}`)
		},
		"a tool result's subtype": func(r *qhChat) *qhChat {
			return r.edit(t, answer, `"toolCallResult":`, `"subtype":"code_mode_tool_result","toolCallResult":`)
		},
		"a step's subtype": func(r *qhChat) *qhChat {
			return r.edit(t, step, `"usageMetadata":`, `"subtype":"x","usageMetadata":`)
		},
		"a call with no id": func(r *qhChat) *qhChat {
			return r.edit(t, step, `"id":"call_1",`, ``)
		},
		"a call with no id beside one answered": func(r *qhChat) *qhChat {
			return r.edit(t, step, `"parts":[`, `"parts":[{"functionCall":{"name":"mcp__crewlet__echo","args":{}}},`)
		},
		"a call id twice in a step": func(r *qhChat) *qhChat {
			return r.edit(t, step, `"parts":[`, `"parts":[`+qhCallPart("call_1", "echo", `{}`)+`,`)
		},
		"a call id too long": func(r *qhChat) *qhChat {
			long := strings.Repeat("c", qwenCallIDMost+1)
			return r.edit(t, step, `"id":"call_1"`, `"id":"`+long+`"`).edit(t, answer, `"id":"call_1"`, `"id":"`+long+`"`).
				edit(t, answer, `"callId":"call_1"`, `"callId":"`+long+`"`)
		},
	} {
		h, err := qhRead(t, edit(qhRound()).file(), ohBegan, c)
		if err == nil || h.Read {
			t.Errorf("%s: %+v; want no reading", name, h)
		}
	}
	long := strings.Repeat("c", qwenCallIDMost)
	file := qhRound().edit(t, step, `"id":"call_1"`, `"id":"`+long+`"`).edit(t, answer, `"id":"call_1"`, `"id":"`+long+`"`).
		edit(t, answer, `"callId":"call_1"`, `"callId":"`+long+`"`).file()
	h, err := qhRead(t, file, ohBegan, c)
	if got := ohHeld(t, h, err, c); !slices.Equal(got, []int{1}) {
		t.Errorf("a call id at its bound: held %v; want its call", got)
	}
	h, err = qhRead(t, (&qhChat{}).brief().step(qhCallPart("call_1", "echo", `{"t":1}`), `{"text":"why","thought":true}`,
		`{"text":"so"}`).done("call_1", "echo", "done").add("user", "real_user",
		`,"executionContext":{},"subtype":"mid_turn_user_message","message":{"role":"user","parts":[{"text":"a note"}]},`+
			`"systemPayload":{}`).system("ui_telemetry", `{"uiEvent":{}}`).
		system("branch_checkpoint", `{}`).file(), ohBegan, c)
	if got := ohHeld(t, h, err, c); !slices.Equal(got, []int{1}) {
		t.Errorf("a step's thought and text, a note, telemetry and a checkpoint: held %v; want its call", got)
	}
}

// A QWEN PART IS ITS CALL'S OWN TOOL RESULT: a step of two calls of one tool
// whose answers swap their ids holds neither class; an answer to a call
// answered already, and an answer naming an earlier step's call, are no
// reading.
//
// Mutation: pair an answer by its name, or forget the answered, and a case
// goes red.
func TestAQwenPartIsItsCallsOwnToolResult(t *testing.T) {
	t.Parallel()
	a, b := ohCall(1, "echo", `{"t":"a"}`, "A"), ohCall(2, "echo", `{"t":"b"}`, "B")
	step := func() *qhChat {
		return (&qhChat{}).brief().step(qhCallPart("call_a", "echo", `{"t":"a"}`), qhCallPart("call_b", "echo", `{"t":"b"}`))
	}
	h, err := qhRead(t, step().done("call_a", "echo", "A").done("call_b", "echo", "B").file(), ohBegan, a, b)
	if got := ohHeld(t, h, err, a, b); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("the step answered: held %v; want both", got)
	}
	h, err = qhRead(t, step().done("call_b", "echo", "A").done("call_a", "echo", "B").file(), ohBegan, a, b)
	if got := ohHeld(t, h, err, a, b); len(got) != 0 {
		t.Errorf("the answers' ids swapped: held %v; want neither", got)
	}
	for name, file := range map[string]string{
		"a second answer":          step().done("call_a", "echo", "A").done("call_a", "echo", "A").file(),
		"an earlier step's answer": step().done("call_a", "echo", "A").step(qhCallPart("call_c", "echo", `{}`)).done("call_b", "echo", "B").file(),
		"an answer before a step":  (&qhChat{}).brief().done("call_a", "echo", "A").file(),
	} {
		if h, err := qhRead(t, file, ohBegan, a, b); err == nil || h.Read {
			t.Errorf("%s: %+v; want no reading", name, h)
		}
	}
}

// A QWEN FAILURE COUNTS ONLY AS SENT: the round's failure and its padded
// failure are held as sent, untrimmed; its failures that sent no text, which
// Qwen answers in its own words, are told; and a failure logged blank, a case
// not measured, is told whatever the file holds.
//
// Mutation: drop the blank rule, or match a failure trimmed, and a case goes
// red.
func TestAQwenFailureCountsOnlyAsSent(t *testing.T) {
	t.Parallel()
	f := qhFixtures(t)["round"]
	h, err := f.read(t, f.chat, f.start)
	held := qhHeld(t, f, h, err)
	for _, want := range []struct {
		appended int
		held     bool
	}{{2, true}, {13, true}, {11, false}, {12, false}} {
		if slices.Contains(held, want.appended) != want.held {
			t.Errorf("the round's call %d: held %t; want %t", want.appended, !want.held, want.held)
		}
	}
	blank := ohCall(1, "fail", `{}`, "   ")
	blank.Failed = true
	h, err = qhRead(t, (&qhChat{}).brief().step(qhCallPart("call_1", "fail", `{}`)).failed("call_1", "fail", "   ").file(),
		ohBegan, blank)
	if got := ohHeld(t, h, err, blank); len(got) != 0 {
		t.Errorf("a blank failure logged blank: held %v; want it told", got)
	}
	words := ohCall(1, "fail", `{}`, qwenFailureWords)
	words.Failed = true
	h, err = qhRead(t, (&qhChat{}).brief().step(qhCallPart("call_1", "fail", `{}`)).
		failed("call_1", "fail", qwenFailureWords).file(), ohBegan, words)
	if got := ohHeld(t, h, err, words); len(got) != 0 {
		t.Errorf("Qwen's words for a failure with no text: held %v; want it told", got)
	}
}

// TELEMETRY NEVER STANDS FOR A QWEN PART: killed mid-batch, the file's
// telemetry names echo's call ended in success, and echo is told.
//
// Mutation: read a call off `ui_telemetry`, and this goes red.
func TestTelemetryNeverStandsForAQwenPart(t *testing.T) {
	t.Parallel()
	f := qhFixtures(t)["killed-midbatch"]
	if !strings.Contains(f.chat, `"toolName":"mcp__crewlet__echo","phase":"ended","outcome":"success"`) {
		t.Fatal("the capture's telemetry names no echo call ended")
	}
	h, err := f.read(t, f.chat, f.start)
	if got := qhHeld(t, f, h, err); len(got) != 0 {
		t.Errorf("held %v; want echo told", got)
	}
}

// A QWEN PART'S ARGUMENTS ARE READ BY THE LOG'S OWN RULE: the round's
// arguments (numbers, escapes) are held as the bridge got them; a part's
// keys in another order, written with escapes, or at the log's 64 KiB bound,
// still match; one past the bound, or of other values, holds nothing.
//
// Mutation: compare the arguments raw, or drop the bound, and a case goes red.
func TestAQwenPartsArgumentsAreReadByTheLogsOwnRule(t *testing.T) {
	t.Parallel()
	f := qhFixtures(t)["round"]
	h, err := f.read(t, f.chat, f.start)
	if held := qhHeld(t, f, h, err); !slices.Contains(held, 5) {
		t.Errorf("the round's args call: held %v; want call 5", held)
	}
	pad := func(n int) string { return `{"a":"` + strings.Repeat("x", n-len(`{"a":"","b":1}`)) + `","b":1}` }
	for name, tc := range map[string]struct {
		logged, part string
		held         bool
	}{
		"keys in another order": {`{"a":1,"b":"x"}`, `{"b":"x","a":1}`, true},
		"escapes":               {`{"a":"é/"}`, `{"a":"é\/"}`, true},
		"at the bound":          {pad(sandbox.MaxSubmissionArgsBytes), pad(sandbox.MaxSubmissionArgsBytes), true},
		"past the bound":        {pad(sandbox.MaxSubmissionArgsBytes + 1), pad(sandbox.MaxSubmissionArgsBytes + 1), false},
		"other values":          {`{"a":1}`, `{"a":2}`, false},
	} {
		c := ohCall(1, "echo", tc.logged, "done")
		file := (&qhChat{}).brief().step(qhCallPart("call_1", "echo", tc.part)).done("call_1", "echo", "done").file()
		h, err := qhRead(t, file, ohBegan, c)
		if got := ohHeld(t, h, err, c); slices.Equal(got, []int{1}) != tc.held {
			t.Errorf("%s: held %v; want held %t", name, got, tc.held)
		}
	}
}

// A SCRUBBED TEXT NEVER STANDS FOR A CUT QWEN OUTPUT: an output the log cut
// whose text past the kept opening holds the scrub's marker, within one of
// the digest's pieces or across two ([textDigestBytes]), is told, since the
// scrub may have rewritten what the log did not keep; the same text with no
// marker is held.
//
// Mutation: leave the marker unchecked, and a case goes red.
func TestAScrubbedTextNeverStandsForACutQwenOutput(t *testing.T) {
	t.Parallel()
	read := func(text string) []int {
		c := ohCut(ohCall(1, "echo", `{"t":1}`, text))
		file := (&qhChat{}).brief().step(qhCallPart("call_1", "echo", `{"t":1}`)).done("call_1", "echo", text).file()
		h, err := qhRead(t, file, ohBegan, c)
		return ohHeld(t, h, err, c)
	}
	for where, at := range map[string]int{"within a piece": 8000, "across two pieces": textDigestBytes - 5} {
		text := strings.Repeat("w", at) + redact.Marker + "engine-secret]" + strings.Repeat("w", 1000)
		if got := read(text); len(got) != 0 {
			t.Errorf("a marker %s: held %v; want told", where, got)
		}
		plain := strings.Repeat("w", len(text))
		if got := read(plain); !slices.Equal(got, []int{1}) {
			t.Errorf("the text unscrubbed, its marker %s: held %v; want it", where, got)
		}
	}
}

// PARALLEL QWEN CALLS OF ONE STEP ARE READ IN ANY ORDER: the captured
// read-only calls, run at once, answer out of their call order, and all three
// are held.
//
// Mutation: require answers in their calls' order, and this goes red.
func TestParallelQwenCallsOfOneStepAreReadInAnyOrder(t *testing.T) {
	t.Parallel()
	f := qhFixtures(t)["readonly-concurrent"]
	h, err := f.read(t, f.chat, f.start)
	if got := qhHeld(t, f, h, err); !slices.Equal(got, []int{1, 2, 3}) {
		t.Errorf("held %v; want all three", got)
	}
	a, b := ohCall(1, "echo", `{"t":"a"}`, "A"), ohCall(2, "echo", `{"t":"b"}`, "B")
	file := (&qhChat{}).brief().step(qhCallPart("call_a", "echo", `{"t":"a"}`), qhCallPart("call_b", "echo", `{"t":"b"}`)).
		done("call_b", "echo", "B").done("call_a", "echo", "A").file()
	h, err = qhRead(t, file, ohBegan, a, b)
	if got := ohHeld(t, h, err, a, b); !slices.Equal(got, []int{1, 2}) {
		t.Errorf("answers out of order: held %v; want both", got)
	}
}

// A QWEN READING IS HELD TO ITS BOUNDS: plain past a lowered meter, a second
// archive entry, a body failing after the archive, and a step of more calls
// than a reading reads are no reading; a step at the bound is read.
//
// Mutation: drop a bound, the end's check or the drain, and a case goes red.
func TestQwenReadingIsHeldToItsBounds(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "echo", `{"t":1}`, "done")
	ask := sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{c}, Resumer: "0.28.4"}
	file := qhRound().file()
	plain := qhTar(t, qhSession, file)
	read := func(plain io.Reader) error {
		h, err := readQwenHoldings(t.Context(), plain, qhSession, ohBegan, ask)
		if err == nil && !h.Read {
			t.Fatal("a reading of nothing with no error")
		}
		return err
	}
	if err := read(bytes.NewReader(plain)); err != nil {
		t.Fatalf("the file within every bound: %v", err)
	}
	var second bytes.Buffer
	tw := tar.NewWriter(&second)
	for _, name := range []string{qhSession, "other"} {
		_ = tw.WriteHeader(&tar.Header{Name: "projects/-work-proj/chats/" + name + ".jsonl", Typeflag: tar.TypeReg,
			Mode: 0o600, Size: int64(len(file))})
		_, _ = io.WriteString(tw, file)
	}
	_ = tw.Close()
	steps := func(calls int) string {
		parts := make([]string, calls)
		for i := range parts {
			parts[i] = qhCallPart("call_"+strconv.Itoa(i), "echo", `{"t":1}`)
		}
		return (&qhChat{}).brief().step(parts...).done("call_0", "echo", "done").file()
	}
	for name, err := range map[string]error{
		"plain past its meter": read(boundedPlain(bytes.NewReader(plain), int64(len(file)))),
		"a second entry":       read(&second),
		"another session's file": func() error {
			_, err := readQwenHoldings(t.Context(), bytes.NewReader(plain), "33333333-1818-4333-8444-000000000009", ohBegan, ask)
			return err
		}(),
		"a step past its calls": read(bytes.NewReader(qhTar(t, qhSession, steps(qwenStepCallsMax+1)))),
	} {
		if err == nil {
			t.Errorf("%s: read; want no reading", name)
		}
	}
	if err := read(bytes.NewReader(qhTar(t, qhSession, steps(qwenStepCallsMax)))); err != nil {
		t.Errorf("a step at its calls' bound: %v; want it read", err)
	}
	_, err := (QwenCode{}).readHoldings(t.Context(), io.MultiReader(bytes.NewReader(ohGzip(t, string(plain))),
		ohFailing{}), qhArchive("0.28.4"), ask, plainBounds(8<<20))
	if err == nil {
		t.Error("a store's stream failing past the archive: read; want no reading")
	}
}

// A QWEN READING STOPS BETWEEN LINES, AND BETWEEN A MESSAGE'S PARTS, WHEN ITS
// CONTEXT ENDS: a caller gone before a step's second part, or before the
// second line, is no reading.
//
// Mutation: ask the context per line alone, and the first case goes red.
func TestQwenReadingStopsBetweenLinesWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "echo", `{"t":1}`, "done")
	part := qhCallPart("call_1", "echo", `{"t":1}`)
	for name, tc := range map[string]struct {
		file string
		asks int
	}{
		// Each line asks once, then each part of its message: the step, the
		// first line here, asks at 1, its parts at 2 and 3.
		"between parts": {(&qhChat{}).step(part, `{"text":"x"}`).file(), 3},
		"between lines": {(&qhChat{}).brief().step(part).file(), 3},
	} {
		ctx := &ohCountdown{Context: t.Context(), left: tc.asks - 1}
		_, err := readQwenHoldings(ctx, bytes.NewReader(qhTar(t, qhSession, tc.file)), qhSession, ohBegan,
			sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{c}, Resumer: "0.28.4"})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s: %v; want the caller's end", name, err)
		}
	}
}

// qhReadingAllocates is the most a reading of [TestQwenReadingHoldsNoLine]'s
// lines allocates: the line reader's 64 KiB, the scanner's 66 KiB, the
// digest's 32 KiB piece, the uuids' first 64 KiB block, the reading's maps and
// its verdict, 279 KiB measured under the race detector on the build host (Go
// 1.27.2), twice that; a 64 MiB line held once is 117 times it.
const qhReadingAllocates = 558 << 10

// A QWEN READING HOLDS NO LINE: a 64 MiB tool result, its output cut by the
// log, is read and its call held, and a 64 MiB compaction's history is read
// past, each within [qhReadingAllocates] whatever the line's length.
//
// Not parallel: it reads the process's allocation count, the fewest bytes of
// three reads ([allocationsOf]).
//
// Mutation: hold a line or a digested text whole, and a case goes red.
func TestQwenReadingHoldsNoLine(t *testing.T) {
	const line = 64 << 20
	text := strings.Repeat("v", line-1024)
	cut := ohCut(ohCall(1, "echo", `{"t":1}`, text))
	small := ohCall(1, "echo", `{"t":1}`, "done")
	history := `{"compressedHistory":[{"role":"user","parts":[{"text":"` + strings.Repeat("h", line) + `"}]}]}`
	for name, tc := range map[string]struct {
		file string
		call sandbox.LoggedCall
	}{
		"a tool result": {(&qhChat{}).brief().step(qhCallPart("call_1", "echo", `{"t":1}`)).
			done("call_1", "echo", text).file(), cut},
		"a compaction": {(&qhChat{}).system("chat_compression", history).brief().
			step(qhCallPart("call_1", "echo", `{"t":1}`)).done("call_1", "echo", "done").file(), small},
	} {
		archive := qhTar(t, qhSession, tc.file)
		ask := sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{tc.call}, Resumer: "0.28.4"}
		var (
			h   sandbox.SessionHoldings
			err error
		)
		_, allocated := allocationsOf(t, func() {
			h, err = readQwenHoldings(t.Context(), bytes.NewReader(archive), qhSession, ohBegan, ask)
		})
		if got := ohHeld(t, h, err, tc.call); !slices.Equal(got, []int{1}) {
			t.Errorf("%s: held %v; want its call", name, got)
		}
		t.Logf("%s: allocated %d bytes", name, allocated)
		if allocated > qhReadingAllocates {
			t.Errorf("%s: allocated %d bytes; want at most %d", name, allocated, qhReadingAllocates)
		}
	}
}

// A QWEN READING'S CAUSE QUOTES NOTHING OF THE FILE: refused inside a 4 MiB
// line, its error holds none of the line's words; refused on a fault whose
// words run a mebibyte, its error is within the detail's bound.
//
// Mutation: carry the cause whole in the reading's words, and this goes red.
func TestQwenReadingsCauseQuotesNothingOfTheFile(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "echo", `{"t":1}`, "done")
	ask := sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{c}, Resumer: "0.28.4"}
	secret := strings.Repeat("the secret words ", 1<<18)
	inside := qhRound()
	inside.lines[1] = strings.Replace(inside.lines[1], `"the brief"`, `"`+secret+"\x01\"", 1)
	for name, plain := range map[string]io.Reader{
		"inside a line":   bytes.NewReader(qhTar(t, qhSession, inside.file())),
		"on a long fault": io.MultiReader(bytes.NewReader(qhTar(t, qhSession, qhRound().file())[:600]), ohFault{errors.New(secret)}),
	} {
		_, err := readQwenHoldings(t.Context(), plain, qhSession, ohBegan, ask)
		if err == nil {
			t.Fatalf("%s: read", name)
		}
		if msg := err.Error(); len(msg) > transcriptDetailLimit+200 || strings.Contains(msg, "secret words") &&
			name == "inside a line" {
			t.Errorf("%s: the cause is %d bytes, or quotes the file", name, len(msg))
		}
	}
}

// THE QWEN READER TAKES NO SLOT OF ITS OWN: a reading under ReadHoldings,
// which holds the readings' one slot, completes and gives the slot back.
//
// Not parallel: the slots are the process's own.
//
// Mutation: take the slots in the reader, and this goes red.
func TestTheQwenReaderTakesNoSlotOfItsOwn(t *testing.T) {
	c := ohCall(1, "echo", `{"t":1}`, "done")
	archive := ohGzip(t, string(qhTar(t, qhSession, qhRound().file())))
	type reading struct {
		got sandbox.SessionHoldings
		err error
	}
	done := make(chan reading, 1)
	go func() {
		got, err := NewQwenCode().ReadHoldings(t.Context(), qhArchive("0.28.4"), qhOpen(archive),
			sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{c}, Resumer: "0.28.4"})
		done <- reading{got, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || !r.got.Read || HoldingsReadsHeld() != 0 {
			t.Errorf("a reading = %+v, %v, readings' slot held after %d; want it read and the slot given back",
				r.got, r.err, HoldingsReadsHeld())
		}
	case <-time.After(time.Minute): //fixedwait:liveness
		t.Fatal("a reading of one small step waited a minute: the reader waits on a slot its caller holds")
	}
}

// EACH QWEN RELEASE'S BRIDGED CALLS READ AS ITS FILE HOLDS THEM: every case
// its own runs kept (every output shape, stubs and the batch budget, a kill
// mid-batch, mid-answer and in a second round, a compaction, parallel
// read-only calls, `tool_call`, a continued run killed, the three
// collisions), kept as the take keeps it, is read through ReadHoldings, and
// exactly the calls the reference reading holds are held.
//
// Mutation: none of its own; the regression pin of the rules over real
// files.
func TestEachQwenReleasesBridgedCallsReadAsItsFileHoldsThem(t *testing.T) {
	t.Parallel()
	for name, f := range qhFixtures(t) {
		if len(f.calls) == 0 {
			h, err := f.read(t, f.chat, f.start)
			if err != nil || !h.Read {
				t.Errorf("%s: a file of no bridged call: %+v, %v; want it read", name, h, err)
			}
			continue
		}
		archive := ohGzip(t, string(qhTar(t, f.session, f.chat)))
		got, err := NewQwenCode().ReadHoldings(t.Context(), sandbox.SessionArchive{Harness: QwenCodeName,
			SessionID: f.session, Format: qwenArchiveFormat, Version: "0.28.4", RunStart: f.start}, qhOpen(archive),
			f.ask())
		var held []int
		if err == nil {
			held = qhHeld(t, f, got, err)
		}
		if err != nil || !slices.Equal(held, f.held) || got.Compacted != f.compacted {
			t.Errorf("%s: held %v, compacted %t, %v; want %v, %t", name, held, got.Compacted, err, f.held,
				f.compacted)
		}
	}
}

// THE QWEN BRIDGED PREFIX IS THE BRIDGE'S SERVER: `mcp__`, the server's name
// config reserves, and `__`, 14 bytes, which [qwenToolNameMost] counts on.
//
// Mutation: read another harness's profile, and this goes red.
func TestTheQwenBridgedPrefixIsTheBridgesServer(t *testing.T) {
	t.Parallel()
	want := "mcp__" + config.ToolServerName + "__"
	if got := qwenBridgedPrefix(); got != want || len(got) != 14 {
		t.Errorf("Qwen Code's prefix for the engine's tools is %q; want %q, 14 bytes", got, want)
	}
	if tool, ok := qwenBridgedTool(json.RawMessage(ohQuote(want + "echo"))); !ok || tool != "echo" {
		t.Errorf("%s names %q, %t; want the bridge's echo", want+"echo", tool, ok)
	}
}

// A QWEN CONTINUATION IS ASKED BY LOG PLACE: its calls are keyed by their
// place in the round's log, which its reading reads.
//
// Mutation: key them by none, and this goes red.
func TestAQwenContinuationIsAskedByLogPlace(t *testing.T) {
	t.Parallel()
	if got := (QwenCode{}).sessionTraits().CallKeying; got != sandbox.CallKeyingByLogPlace {
		t.Errorf("a Qwen Code session's calls are keyed %q; want %q", got, sandbox.CallKeyingByLogPlace)
	}
}

// qhLeast is the shortest record a reading reads past, record n of a chain
// (n past 0): a system record of the eight members every record holds, its
// subtype and a payload, each at the shortest value a reading reads.
func qhLeast(n int) string {
	return `{"uuid":"` + qhChat{}.uuid(n) + `","parentUuid":"` + qhChat{}.uuid(n-1) + `","sessionId":"` + qhSession +
		`","timestamp":0,"type":"system","provenance":0,"cwd":0,"version":"0.28.4","subtype":"ui_telemetry",` +
		`"systemPayload":0}`
}

// A QWEN UUID IS KEPT BY ITS LAST 64 BITS, its own, never a hash of it: two
// uuids differing there are two ids, and two sharing them read as one id
// twice, which only tells.
//
// Mutation: key the first 64 bits, or skip the variant's group, and a case
// goes red.
func TestAQwenUUIDIsKeptByItsLast64Bits(t *testing.T) {
	t.Parallel()
	key := func(s string) uint64 {
		var id [36]byte
		if !qwenUUID(s) || copy(id[:], s) != 36 {
			t.Fatalf("%q is no uuid as Qwen Code writes one", s)
		}
		return qwenUUIDKey(id)
	}
	if got := key("00000000-0000-4000-8123-456789abcdef"); got != 0x8123456789abcdef {
		t.Errorf("the key is %#x; want the last 64 bits, %#x", got, uint64(0x8123456789abcdef))
	}
	if key("11111111-2222-4333-8123-456789abcdef") != key("aaaaaaaa-bbbb-4ccc-8123-456789abcdef") {
		t.Error("two uuids sharing their last 64 bits keep two keys; want one, read as twice")
	}
	if key("00000000-0000-4000-8123-456789abcdef") == key("00000000-0000-4000-9123-456789abcdef") {
		t.Error("two uuids differing in the variant's group keep one key; want two")
	}
}

// A QWEN RECORD IS AT LEAST 256 BYTES with its break: the shortest a reading
// reads ([qhLeast]) is 266, and a record missing any of its members is no
// reading, so the 8 bytes [onceEach] keeps of each uuid holds a file at the
// 256 MiB plain bound to at most 8 MiB.
//
// Mutation: read a record missing a member, and this goes red.
func TestQwenRecordLeast(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "echo", `{"t":1}`, "done")
	with := func(least string) string {
		chat := (&qhChat{}).system("session_approval_mode", `{}`)
		chat.lines = append(chat.lines, least)
		return chat.brief().step(qhCallPart("call_1", "echo", `{"t":1}`)).done("call_1", "echo", "done").file()
	}
	least := qhLeast(1)
	h, err := qhRead(t, with(least), ohBegan, c)
	if got := ohHeld(t, h, err, c); !slices.Equal(got, []int{1}) {
		t.Fatalf("a file holding the least record: held %v, %v; want it read", got, err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal([]byte(least), &members); err != nil {
		t.Fatal(err)
	}
	for name, value := range members {
		short := strings.Replace(least, `"`+name+`":`+string(value)+`,`, ``, 1)
		short = strings.Replace(short, `,"`+name+`":`+string(value)+`}`, `}`, 1)
		if short == least {
			t.Fatalf("the least record holds no %s to drop", name)
		}
		if h, err := qhRead(t, with(short), ohBegan, c); err == nil || h.Read {
			t.Errorf("the least record without %s: read; want no reading", name)
		}
	}
	const plain = 256 << 20
	if records := plain / (len(least) + 1); len(least)+1 < 256 || records*8 > 8<<20 {
		t.Errorf("the least record is %d bytes: %d of them keep %d bytes of uuids; want at least 256, within 8 MiB",
			len(least)+1, records, records*8)
	}
}
