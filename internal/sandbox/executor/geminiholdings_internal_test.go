package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// These cases read a Gemini CLI session's kept chat file for which of a
// round's calls it holds ([readGeminiHoldings]), on 0.70.0's own files
// (testdata/gemini/0.70.0/holdings, whose manifest names each file's calls
// as the bridge logged them and what the next resume handed its model) and on
// files written here. Every arm that can hold a call has a case where the
// session lacks the call, and holds none.

// ghManifest is the fixtures' manifest: each round's calls as the bridge
// logged them, and each case's file, session, start and verdict.
type ghManifest struct {
	Calls map[string][]ghLogged `json:"calls"`
	Cases map[string]ghCase     `json:"cases"`
}

// ghLogged is one call as the bridge logged it.
type ghLogged struct {
	Appended, Seq      int
	Name, Args, Output string
	Failed, Abandoned  bool
}

// ghCase is one fixture: the chat file, the run's start, the calls, and the
// places held, or why it is no reading.
type ghCase struct {
	File, Session string
	Began         bool
	Lines         int64
	Unshadowed    *bool
	Calls         string
	Held          []int
	NoReading     string
	Compacted     bool
}

// ghFixtures is the manifest.
func ghFixtures(t testing.TB) ghManifest {
	t.Helper()
	var m ghManifest
	if err := json.Unmarshal(ghGunzipped(t, "manifest.json.gz"), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// ghGunzipped is the fixture file name, unzipped.
func ghGunzipped(t testing.TB, name string) []byte {
	t.Helper()
	if !strings.HasSuffix(name, ".gz") {
		name += ".gz"
	}
	f, err := os.Open(filepath.Join("testdata", "gemini", "0.70.0", "holdings", name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// ghAsk is the calls a round logged, as a relaunch asks of them: each keyed
// by its place, its arguments keyed by the log's rule, its output cut as the
// log cuts it, an abandoned one not asked.
func ghAsk(t testing.TB, logged []ghLogged) []sandbox.LoggedCall {
	t.Helper()
	calls := make([]sandbox.LoggedCall, 0, len(logged))
	for _, l := range logged {
		sum, err := toolargs.Key([]byte(l.Args))
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, ohCut(sandbox.LoggedCall{ID: strconv.Itoa(l.Appended), Asked: !l.Abandoned,
			Failed: l.Failed, Output: l.Output, Appended: l.Appended, Seq: l.Seq, Name: l.Name,
			Args: sandbox.ArgsKey{Whole: true, Sum: sum}}))
	}
	return calls
}

// ghMember is a chat file's member as the run's script packs it.
func ghMember(session string) string {
	return "chats/session-2026-11-23T03-07-" + session[:8] + ".jsonl"
}

// ghTarGz is a gzipped tar of one member, name, holding body, as the run's
// script packs a chat file.
func ghTarGz(t testing.TB, name string, body []byte) []byte {
	t.Helper()
	tarred, err := io.ReadAll(clArchive(t.(*testing.T), [3]string{name, "0", string(body)}))
	if err != nil {
		t.Fatal(err)
	}
	return ohGzip(t.(*testing.T), string(tarred))
}

// ghArchiveOf is the archive of a fixture case as its row describes it.
func ghArchiveOf(c ghCase) sandbox.SessionArchive {
	start := sandbox.RunStart{Began: c.Began, Lines: c.Lines}
	return sandbox.SessionArchive{Harness: GeminiCLIName, SessionID: c.Session, Format: geminiArchiveFormat,
		Version: "0.70.0", RunStart: start, Unshadowed: c.Unshadowed == nil || *c.Unshadowed}
}

// ghReadCase reads the fixture case name through [Runner.ReadHoldings], and
// fails it where its verdict is not the manifest's.
func ghReadCase(t *testing.T, m ghManifest, name string) {
	t.Helper()
	c, ok := m.Cases[name]
	if !ok {
		t.Fatalf("no fixture case %s", name)
	}
	calls := ghAsk(t, m.Calls[c.Calls])
	archive := ghTarGz(t, ghMember(c.Session), ghGunzipped(t, c.File))
	h, err := NewGeminiCLI().ReadHoldings(t.Context(), ghArchiveOf(c), func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(archive)), nil
	}, sandbox.HoldingsAsk{Calls: calls, Resumer: "0.70.0"})
	if c.NoReading != "" {
		if err == nil || h.Read {
			t.Errorf("%s (%s): a reading = %+v; want none", name, c.NoReading, h)
		}
		return
	}
	if err != nil || !h.Read {
		t.Errorf("%s: no reading: %v", name, err)
		return
	}
	var held []int
	for _, call := range calls {
		if in, _ := h.Holds(call); in {
			held = append(held, call.Appended)
		}
	}
	if !slices.Equal(held, c.Held) || h.Compacted != c.Compacted {
		t.Errorf("%s: held %v, compacted %t; want %v, %t", name, held, h.Compacted, c.Held, c.Compacted)
	}
}

// ghReadCases reads each of names ([ghReadCase]).
func ghReadCases(t *testing.T, names ...string) {
	t.Helper()
	m := ghFixtures(t)
	for _, name := range names {
		ghReadCase(t, m, name)
	}
}

// EACH GEMINI CLI RELEASE'S BRIDGED CALLS READ AS ITS FILE HOLDS THEM: every
// fixture of 0.70.0, read through the runner as a relaunch reads it, holds
// exactly the calls whose answers the next resume handed its model verbatim
// (or, for an edited file, the verdict its edit calls for), or is no reading.
//
// Mutation: loosen any rule of a counted part, and a case goes red.
func TestEachGeminiReleasesBridgedCallsReadAsItsFileHoldsThem(t *testing.T) {
	t.Parallel()
	m := ghFixtures(t)
	if len(m.Cases) < 30 {
		t.Fatalf("the manifest holds %d cases", len(m.Cases))
	}
	for name := range m.Cases {
		ghReadCase(t, m, name)
	}
}

// ghSession is the session every file written here is of.
const ghSession = "4759e486-b48b-459f-81aa-9b61a3bc8b09"

// ghHeader is ghSession's first line.
const ghHeader = `{"sessionId":"` + ghSession + `","projectHash":"p","startTime":"s","lastUpdated":"u","kind":"main"}`

// ghFile is a chat file of ghSession holding lines after its header.
func ghFile(lines ...string) string { return ghHeader + "\n" + strings.Join(lines, "\n") + "\n" }

// ghReply is a reply record id holding calls, its content a string.
func ghReply(id string, calls ...string) string {
	return `{"id":"` + id + `","type":"gemini","content":"","toolCalls":[` + strings.Join(calls, ",") + `]}`
}

// ghCallOf is a successful call of the bridge's tool, its id id.
func ghCallOf(id, tool, args string) string {
	return `{"id":"` + id + `","name":"mcp_crewlet_` + tool + `","args":` + args + `,"status":"success"}`
}

// ghAnswers is a person's record id of answers alone.
func ghAnswers(id string, answers ...string) string {
	return `{"id":"` + id + `","type":"user","content":[` + strings.Join(answers, ",") + `]}`
}

// ghAnswerOf is the answer to call id of tool, text wrapped as 0.70.0 wraps
// an MCP tool's one text part.
func ghAnswerOf(id, tool, text string) string {
	return `{"functionResponse":{"id":"` + id + `","name":"mcp_crewlet_` + tool + `","response":{"output":` +
		ohQuote(geminiWrapOpen+text+geminiWrapClose) + `}}}`
}

// ghRound is a file of one round: a reply calling echo with args, answered
// text.
func ghRound(args, text string) string {
	return ghFile(ghReply("r1", ghCallOf("c1", "echo", args)), ghAnswers("u1", ghAnswerOf("c1", "echo", text)))
}

// ghRead reads file, a chat file of ghSession its run began, for calls.
func ghRead(t *testing.T, file string, start sandbox.RunStart, calls ...sandbox.LoggedCall) (sandbox.SessionHoldings,
	error) {
	t.Helper()
	return readGeminiHoldings(t.Context(), clArchive(t, [3]string{ghMember(ghSession), "0", file}),
		ghSession, start, sandbox.HoldingsAsk{Calls: calls, Resumer: "0.70.0"})
}

// ghBegan is a run that began its session.
var ghBegan = sandbox.RunStart{Began: true}

// ghArchive is the archive of ghSession as its row describes it.
func ghArchive(version string) sandbox.SessionArchive {
	return sandbox.SessionArchive{Harness: GeminiCLIName, SessionID: ghSession, Format: geminiArchiveFormat,
		Version: version, RunStart: ghBegan, Unshadowed: true}
}

// A GEMINI CLI READING READS ONLY THE RELEASES IT WAS MEASURED ON, each written
// whole: a resumer outside the set, a writer other than the resumer, a member
// padded with blanks, a prefix of one, and none recorded are no reading,
// nothing opened. A member is read.
//
// Mutation: compare by prefix, or leave Version unchecked, and a case opens
// its archive.
func TestAGeminiReadingReadsOnlyReleasesItWasMeasuredOn(t *testing.T) {
	t.Parallel()
	call := ohCall(1, "echo", `{"t":1}`, "done")
	archive := ghTarGz(t, ghMember(ghSession), []byte(ghRound(`{"t":1}`, "done")))
	for name, tc := range map[string]struct {
		version, resumer string
		read             bool
	}{
		"a member":                        {"0.70.0", "0.70.0", true},
		"a resumer outside the set":       {"0.71.0", "0.71.0", false},
		"a writer other than the resumer": {"0.69.0", "0.70.0", false},
		"a newer writer than the resumer": {"0.71.0", "0.70.0", false},
		"a later patch of the member":     {"0.70.1", "0.70.1", false},
		"a member padded with blanks":     {" 0.70.0", " 0.70.0", false},
		"a prefix of a member":            {"0.70", "0.70", false},
		"a member's longer form":          {"0.70.0-preview.1", "0.70.0-preview.1", false},
		"a resumer nobody recorded":       {"", "", false},
	} {
		opened := false
		got, err := NewGeminiCLI().ReadHoldings(t.Context(), ghArchive(tc.version),
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
}

// A GEMINI CLI READING'S FLOOR IS ITS OLDEST RELEASE by its numbers, whatever
// the set's order.
//
// Not parallel: it swaps the set.
//
// Mutation: answer the set's first member, and this goes red.
func TestAGeminiReadingsFloorIsItsOldestRelease(t *testing.T) {
	saved := geminiHoldingsReleases
	t.Cleanup(func() { geminiHoldingsReleases = saved })
	for want, set := range map[string][]string{"0.70.0": {"0.71.0", "0.70.0"}, "0.9.1": {"0.10.0", "0.9.1"}} {
		geminiHoldingsReleases = set
		if got := (GeminiCLI{}).holdingsFloor(); got != want {
			t.Errorf("the floor of %q is %q; want %q", set, got, want)
		}
	}
}

// A GEMINI CLI READING'S RELEASES ARE THE RELEASES MEASURED: 0.70.0, the one
// whose files and resumes these fixtures are, which is the release this
// build drives; and the engine's tools are read by the name 0.70.0 gives them,
// `mcp_` and the server config reserves.
//
// Mutation: add a release, or read another harness's prefix, and this goes
// red.
func TestAGeminiReadingsReleasesAreTheReleasesMeasured(t *testing.T) {
	t.Parallel()
	if want := []string{geminiRelease}; !slices.Equal(geminiHoldingsReleases, want) {
		t.Errorf("the reading's releases are %q; want those measured, %q", geminiHoldingsReleases, want)
	}
	if got, want := geminiBridgedPrefix(), "mcp_"+config.ToolServerName+"_"; got != want {
		t.Errorf("Gemini CLI's prefix for the engine's tools is %q; want %q", got, want)
	}
	if got := geminiBridgeServer(); got != config.ToolServerName {
		t.Errorf("the guard checks for the server %q; want %q", got, config.ToolServerName)
	}
}

// A GEMINI CLI ASK IS VALIDATED BEFORE ANY READ: a run with no known start, one
// that began after a message, a box not found unshadowed, and an ask the log
// cannot make (none asked, a call keyed other than by its place) are no
// reading; a run that began its session, or after a count of lines, is read.
//
// Mutation: skip any check, and a case goes red.
func TestAGeminiAskIsValidatedBeforeAnyRead(t *testing.T) {
	t.Parallel()
	good := ohCall(1, "echo", `{}`, "done")
	with := func(edit func(*sandbox.SessionArchive)) sandbox.SessionArchive {
		a := ghArchive("0.70.0")
		edit(&a)
		return a
	}
	unasked := good
	unasked.Asked = false
	rekeyed := good
	rekeyed.ID = "c1"
	for name, tc := range map[string]struct {
		archive sandbox.SessionArchive
		calls   []sandbox.LoggedCall
		read    bool
	}{
		"a run that began its session": {ghArchive("0.70.0"), []sandbox.LoggedCall{good}, true},
		"a run after its lines": {with(func(a *sandbox.SessionArchive) { a.RunStart = sandbox.RunStart{Lines: 9} }),
			[]sandbox.LoggedCall{good}, true},
		"no known start": {with(func(a *sandbox.SessionArchive) { a.RunStart = sandbox.RunStart{} }),
			[]sandbox.LoggedCall{good}, false},
		"after a message": {with(func(a *sandbox.SessionArchive) {
			a.RunStart = sandbox.RunStart{After: "d04923d38bb0f6017037e74183378ef4"}
		}), []sandbox.LoggedCall{good}, false},
		"a box not found unshadowed": {with(func(a *sandbox.SessionArchive) { a.Unshadowed = false }),
			[]sandbox.LoggedCall{good}, false},
		"none asked":          {ghArchive("0.70.0"), []sandbox.LoggedCall{unasked}, false},
		"keyed other than so": {ghArchive("0.70.0"), []sandbox.LoggedCall{rekeyed}, false},
		"no call":             {ghArchive("0.70.0"), nil, false},
	} {
		err := (GeminiCLI{}).checkHoldings(tc.archive, sandbox.HoldingsAsk{Calls: tc.calls, Resumer: "0.70.0"})
		if (err == nil) != tc.read {
			t.Errorf("%s: checkHoldings = %v; want read %t", name, err, tc.read)
		}
	}
}

// A GEMINI CLI ASK PAST THE LOG'S BOUNDS IS NEVER READ: 257 calls in flight is
// no reading, 256 are read.
//
// Mutation: drop the ask's shared check, and this goes red.
func TestAGeminiAskPastTheLogsBoundsIsNeverRead(t *testing.T) {
	t.Parallel()
	for n, read := range map[int]bool{sandbox.MaxCallsInFlight: true, sandbox.MaxCallsInFlight + 1: false} {
		unlisted := make([]sandbox.CallIdentity, n)
		for i := range unlisted {
			unlisted[i] = sandbox.CallIdentity{Name: "x", InFlight: true}
		}
		err := (GeminiCLI{}).checkHoldings(ghArchive("0.70.0"), sandbox.HoldingsAsk{Resumer: "0.70.0",
			Calls: []sandbox.LoggedCall{ohCall(1, "echo", `{}`, "done")}, Unlisted: unlisted})
		if (err == nil) != read {
			t.Errorf("%d in flight: checkHoldings = %v; want read %t", n, err, read)
		}
	}
}

// A GEMINI CLI SESSION'S ID IS A CANONICAL UUID, as the runner mints one: an
// upper-case, braced or bare-hex form of the same uuid, and an OpenCode id,
// are no reading.
//
// Mutation: drop the canonical check, and a case goes red.
func TestAGeminiSessionIDIsACanonicalUUID(t *testing.T) {
	t.Parallel()
	for id, read := range map[string]bool{
		ghSession:                              true,
		strings.ToUpper(ghSession):             false,
		"{" + ghSession + "}":                  false,
		strings.ReplaceAll(ghSession, "-", ""): false,
		"ses_0a1b2c3d4e5f60718293a4b5c6d7e8f9": false,
	} {
		a := ghArchive("0.70.0")
		a.SessionID = id
		err := (GeminiCLI{}).checkHoldings(a, sandbox.HoldingsAsk{Resumer: "0.70.0",
			Calls: []sandbox.LoggedCall{ohCall(1, "echo", `{}`, "done")}})
		if (err == nil) != read {
			t.Errorf("%q: checkHoldings = %v; want read %t", id, err, read)
		}
	}
}

// ONLY A GEMINI CLI PART AFTER THE RUN'S START COUNTS: a run begun after line
// 11 holds round 2's parts alone, whose reply first appeared at line 12;
// after line 12 none, its reply's later copies notwithstanding; a resumed
// run's file holds none of the round before it.
//
// Mutation: count a reply whose first line is the start's (`>=`), or place a
// record by its latest copy's line, and a case goes red.
func TestOnlyAGeminiPartAfterTheRunsStartCounts(t *testing.T) {
	t.Parallel()
	ghReadCases(t, "round.start11", "round.start12", "round.run1")
}

// A GEMINI CLI FILE SHORTER THAN ITS START IS NOT READ: a start past its whole
// lines is a file cut or rewritten since the take checked it.
//
// Mutation: drop the start's check, and this goes red.
func TestAGeminiFileShorterThanItsStartIsNotRead(t *testing.T) {
	t.Parallel()
	ghReadCases(t, "round.start-past")
}

// A GEMINI CLI PART BEFORE THE NEWEST COMPACTION NEVER COUNTS: a summary
// between the rounds leaves round 2's parts alone, compacted; a run whose two
// compressions each straddle a call counts none, and a run after them none;
// of two summaries, found by their lines whatever they open with, only the
// newer restarts.
//
// Mutation: ignore the summary, find it by its words, or restart at the
// oldest, and a case goes red.
func TestAGeminiPartBeforeTheNewestCompactionNeverCounts(t *testing.T) {
	t.Parallel()
	ghReadCases(t, "round.snapshot", "compressed.run1", "compressed.run2")
	first, second := ohCall(1, "echo", `{"t":1}`, "one"), ohCall(2, "echo", `{"t":2}`, "two")
	snapshot := func(id string) string {
		return `{"$set":{"sessionId":"` + ghSession + `"}}` + "\n" + `{"id":"` + id + `","timestamp":"t",` +
			`"type":"user","content":[{"text":"  <scratchpad>a preamble</scratchpad><state_snapshot>a summary"}]}` +
			"\n" + `{"id":"` + id + `-ack","timestamp":"t","type":"gemini","content":[{"text":"` + geminiAck + `"}]}`
	}
	h, err := ghRead(t, ghFile(snapshot("s1"), ghReply("r1", ghCallOf("c1", "echo", `{"t":1}`)),
		ghAnswers("u1", ghAnswerOf("c1", "echo", "one")), snapshot("s2"),
		ghReply("r2", ghCallOf("c2", "echo", `{"t":2}`)), ghAnswers("u2", ghAnswerOf("c2", "echo", "two"))),
		ghBegan, first, second)
	if got := ohHeld(t, h, err, first, second); !slices.Equal(got, []int{2}) || !h.Compacted {
		t.Errorf("a round between two summaries and one after: held %v, compacted %t; want [2], true", got,
			h.Compacted)
	}
}

// ONLY A PART GEMINI CLI HANDS ITS MODEL VERBATIM COUNTS: of round.run0's
// fourteen calls, the twins, the escaped echo, the padded text and the 39,000
// bytes are held; a failure, the multi-part, the empty, the blank, the one
// with no content, the escaped tag and the truncated are told; of sizes.run0,
// only the two below the truncation.
//
// Mutation: drop the wrapper's check or the raw tag's, and a case goes red.
func TestOnlyAPartGeminiHandsItsModelVerbatimCounts(t *testing.T) {
	t.Parallel()
	ghReadCases(t, "round.run0", "sizes.run0", "agent-denied.run0", "kill-mid-answer.run0")
	// THE WRAPPER, BOTH ENDS, AND NO SECOND PART IN IT.
	call := ohCall(1, "echo", `{"t":1}`, "done")
	for output, held := range map[string]bool{
		geminiWrapOpen + "done" + geminiWrapClose: true,
		"done":                   false,
		geminiWrapOpen + "done":  false,
		"done" + geminiWrapClose: false,
		geminiWrapOpen + "do" + geminiWrapClose + "\n" + geminiWrapOpen + "ne" + geminiWrapClose: false,
		"<untrusted_context>done</untrusted_context>":                                            false,
		geminiWrapOpen + "done" + "\n</untrusted_context!":                                       false,
	} {
		file := ghFile(ghReply("r1", ghCallOf("c1", "echo", `{"t":1}`)), `{"id":"u1","type":"user","content":[`+
			`{"functionResponse":{"id":"c1","name":"mcp_crewlet_echo","response":{"output":`+ohQuote(output)+`}}}]}`)
		h, err := ghRead(t, file, ghBegan, call)
		if got := ohHeld(t, h, err, call); (len(got) == 1) != held {
			t.Errorf("output %q: held %v; want held %t", output, got, held)
		}
	} // A TEXT HOLDING THE CLOSING TAG RAW is two parts joined, whatever the log
	// says the tool returned.
	joined := "a\n" + geminiWrapNeedle + "\n<untrusted_context>\nb"
	twoParts := ohCall(1, "echo", `{"t":1}`, joined)
	h, err := ghRead(t, ghRound(`{"t":1}`, joined), ghBegan, twoParts)
	if got := ohHeld(t, h, err, twoParts); len(got) != 0 {
		t.Errorf("a text holding the closing tag raw: held %v; want it told", got)
	}
}

// A GEMINI CLI CALL ITS CUT LEFT UNANSWERED IS TOLD: cut while a call ran, the
// file holds both calls `scheduled`, and the resume's synthetic answers are
// `{output,error}`; neither counts, even with the log's text as the output.
//
// Mutation: count a scheduled call or an answer with an error beside its
// output, and a case goes red.
func TestAGeminiCallItsCutLeftUnansweredIsTold(t *testing.T) {
	t.Parallel()
	ghReadCases(t, "kill-mid-call.run0", "kill-mid-call.run1", "loop-rewind.run0")
	call := ohCall(1, "echo", `{"t":1}`, "done")
	scheduled := strings.Replace(ghCallOf("c1", "echo", `{"t":1}`), `"success"`, `"scheduled"`, 1)
	withError := strings.Replace(ghAnswerOf("c1", "echo", "done"), `}}}`, `,"error":"interrupted"}}}`, 1)
	for name, lines := range map[string][]string{
		"a scheduled call answered": {ghReply("r1", scheduled), ghAnswers("u1", ghAnswerOf("c1", "echo", "done"))},
		"an answer beside an error": {ghReply("r1", ghCallOf("c1", "echo", `{"t":1}`)), ghAnswers("u1", withError)},
	} {
		h, err := ghRead(t, ghFile(lines...), ghBegan, call)
		if got := ohHeld(t, h, err, call); len(got) != 0 {
			t.Errorf("%s: held %v; want it told", name, got)
		}
	}
}

// A GEMINI CLI PART IS THE ANSWER THE RESUME HANDS: the person's record's
// answer, never `toolCalls[].result` (swapped results change nothing;
// answers only there count nothing); a record holding text beside its answers
// counts none of them; and the first answer of an id, not a later one.
//
// Mutation: read `result`, drop the answers-only rule, or take the last answer
// of an id, and a case goes red.
func TestAGeminiPartIsTheAnswerTheResumeHands(t *testing.T) {
	t.Parallel()
	ghReadCases(t, "round.results-swapped", "round.no-user-answer", "round.mixed-user")
	call := ohCall(1, "echo", `{"t":1}`, "done")
	// TWO IDENTICAL CALLS OF ONE ID, one answer: the resume hands one answer,
	// which stands for neither.
	twins := []sandbox.LoggedCall{call, ohCall(2, "echo", `{"t":1}`, "done")}
	h, err := ghRead(t, ghFile(ghReply("r1", ghCallOf("c1", "echo", `{"t":1}`), ghCallOf("c1", "echo", `{"t":1}`)),
		ghAnswers("u1", ghAnswerOf("c1", "echo", "done"))), ghBegan, twins...)
	if got := ohHeld(t, h, err, twins...); len(got) != 0 {
		t.Errorf("two identical calls of one id and one answer: held %v; want both told", got)
	}
	for name, tc := range map[string]struct {
		lines []string
		held  bool
	}{
		"the first answer the call's": {[]string{ghReply("r1", ghCallOf("c1", "echo", `{"t":1}`)),
			ghAnswers("u1", ghAnswerOf("c1", "echo", "done")), ghAnswers("u2", ghAnswerOf("c1", "echo", "other"))}, true},
		"a later answer the call's": {[]string{ghReply("r1", ghCallOf("c1", "echo", `{"t":1}`)),
			ghAnswers("u1", ghAnswerOf("c1", "echo", "other")), ghAnswers("u2", ghAnswerOf("c1", "echo", "done"))}, false},
		"an answer after the next reply": {[]string{ghReply("r1", ghCallOf("c1", "echo", `{"t":1}`)),
			`{"id":"r2","type":"gemini","content":"next"}`, ghAnswers("u1", ghAnswerOf("c1", "echo", "done"))}, false},
		"two calls of one id": {[]string{ghReply("r1", ghCallOf("c1", "echo", `{"t":1}`), ghCallOf("c1", "echo", `{"t":1}`)),
			ghAnswers("u1", ghAnswerOf("c1", "echo", "done"))}, false},
		"an answer under another tool's name": {[]string{ghReply("r1", ghCallOf("c1", "echo", `{"t":1}`)),
			ghAnswers("u1", ghAnswerOf("c1", "other", "done"))}, false},
		"a reply whose content is parts": {[]string{`{"id":"r1","type":"gemini","content":[{"text":""}],"toolCalls":[` +
			ghCallOf("c1", "echo", `{"t":1}`) + `]}`, ghAnswers("u1", ghAnswerOf("c1", "echo", "done"))}, false},
	} {
		h, err := ghRead(t, ghFile(tc.lines...), ghBegan, call)
		if got := ohHeld(t, h, err, call); (len(got) == 1) != tc.held {
			t.Errorf("%s: held %v; want held %t", name, got, tc.held)
		}
	}
}

// A GEMINI CLI FAILURE IS NEVER HELD: a failed call whose words equal its
// logged output, which OpenCode's rule would hold, is told, and so is one
// whose answer is the log's words wrapped as a success's.
//
// Mutation: count a failed call by its class, and this goes red.
func TestAGeminiFailureIsNeverHeld(t *testing.T) {
	t.Parallel()
	ghReadCases(t, "round.fail-equal")
	failed := ohCall(1, "fail", `{"why":"no"}`, "failed: no")
	failed.Failed = true
	errored := strings.Replace(ghCallOf("c1", "fail", `{"why":"no"}`), `"success"`, `"error"`, 1)
	h, err := ghRead(t, ghFile(ghReply("r1", errored), ghAnswers("u1", ghAnswerOf("c1", "fail", "failed: no"))),
		ghBegan, failed)
	if got := ohHeld(t, h, err, failed); len(got) != 0 {
		t.Errorf("a failure answered in the log's words, wrapped: held %v; want it told", got)
	}
}

// A MASKED GEMINI CLI OUTPUT IS NEVER HELD: a `$patch` restating round 2's
// person's record with a masked output replaces its answers.
//
// Mutation: ignore a patch's updates, and this goes red.
func TestAMaskedGeminiOutputIsNeverHeld(t *testing.T) {
	t.Parallel()
	ghReadCases(t, "round.masked-patch")
}

// A REWOUND GEMINI CLI RESPONSE NEVER COUNTS: a `$rewindTo` naming round 2's
// reply drops it and what follows; one naming no record, which 0.70.0 reads
// as clearing the history, is no reading.
//
// Mutation: ignore a rewind, or read one naming nothing as nothing, and a case
// goes red.
func TestARewoundGeminiResponseNeverCounts(t *testing.T) {
	t.Parallel()
	ghReadCases(t, "round.rewind", "round.rewind-unknown")
}

// A MARKED CHANGE ENDS THE GEMINI CLI READING: a patch ordering the records, a
// line, key, type or status 0.70.0 does not write, a `$set` of the messages or
// of another session, another session's header, a key twice, an empty line.
//
// Mutation: drop any of the kind, key or session checks, and a case goes red.
func TestAMarkedChangeEndsTheGeminiReading(t *testing.T) {
	t.Parallel()
	ghReadCases(t, "round.orderids", "round.unknown-line", "round.unknown-key", "round.unknown-type",
		"round.unknown-status", "round.set-messages", "round.other-session", "round.set-other-session",
		"round.key-twice", "round.blank-line")
	call := ohCall(1, "echo", `{"t":1}`, "done")
	for name, line := range map[string]string{
		"a single-id patch":     `{"$patch":{"id":"u1","content":"x"}}`,
		"an update of no id":    `{"$patch":{"updates":[{"content":"x"}]}}`,
		"a removal of no id":    `{"$patch":{"removeIds":[1]}}`,
		"a header again":        ghHeader,
		"a line of no object":   `[]`,
		"a part of no object":   `{"id":"u9","type":"user","content":[1]}`,
		"a call of no status":   `{"id":"r9","type":"gemini","content":"","toolCalls":[{"id":"x","name":"y"}]}`,
		"an entry with no type": `{"id":"u9","content":"x"}`,
		"an entry with no id":   `{"type":"user","content":"x"}`,
	} {
		h, err := ghRead(t, ghFile(ghReply("r1", ghCallOf("c1", "echo", `{"t":1}`)),
			ghAnswers("u1", ghAnswerOf("c1", "echo", "done")), line), ghBegan, call)
		if err == nil || h.Read {
			t.Errorf("%s: a reading = %+v; want none", name, h)
		}
	}
}

// A LINE SEPARATOR LEAVES A GEMINI CLI SESSION UNREAD: raw U+2028 or U+2029,
// their escapes as the scrub writes them (in any case, across two reads), and
// a carriage return, anywhere; a raw U+0085, which 0.70.0's loader keeps, is
// read.
//
// Mutation: drop the escape's or the carriage return's scan, or its carry
// across reads, and a case goes red.
func TestALineSeparatorLeavesAGeminiSessionUnread(t *testing.T) {
	t.Parallel()
	ghReadCases(t, "separators.run0", "round-u2028.run0", "round-u2028.escaped", "round.cr", "round.nel")
	file := ghRound(`{"t":1}`, "done")
	for name, tc := range map[string]struct {
		file string
		read bool
	}{
		"none":                    {file, true},
		"an escape":               {strings.Replace(file, "done", `do\u2028ne`, 1), false},
		"a raw U+2029":            {strings.Replace(file, "done", "do\u2029ne", 1), false},
		"an upper-case escape":    {strings.Replace(file, "done", `do\U2028ne`, 1), false},
		"a carriage return":       {strings.Replace(file, `"r1"`, `"r1" `, 1) + "\r", false},
		"a separator in its tail": {file + `{"id":"torn` + "\u2028", false},
	} {
		for _, step := range []int{1, 3, 64 << 10} {
			var got []byte
			sep := &geminiSeparators{r: &ghSteps{r: strings.NewReader(tc.file), step: step}}
			_, err := io.CopyBuffer(&ghSink{&got}, sep, make([]byte, 64<<10))
			if read := err == nil; read != tc.read {
				t.Errorf("%s, read %d bytes at a time: %v; want read %t", name, step, err, tc.read)
			}
		}
	}
}

// ghSteps reads r at most step bytes at a time.
type ghSteps struct {
	r    io.Reader
	step int
}

func (s *ghSteps) Read(p []byte) (int, error) { return s.r.Read(p[:min(len(p), s.step)]) }

// ghSink appends what it is written.
type ghSink struct{ to *[]byte }

func (s *ghSink) Write(p []byte) (int, error) { *s.to = append(*s.to, p...); return len(p), nil }

// A TORN LAST GEMINI CLI LINE IS READ AS THE RESUME CUTS IT: dropped, the
// rest read as before.
//
// Mutation: parse the torn tail, and this goes red.
func TestATornLastGeminiLineIsReadAsTheResumeCutsIt(t *testing.T) {
	t.Parallel()
	ghReadCases(t, "round.torn")
}

// A GEMINI CLI PART'S ARGUMENTS ARE READ BY THE LOG'S OWN RULE (toolargs.Key):
// round.run0's escaped, numbered call is held; a file spelling the call's
// arguments in another key order is held; arguments of exactly the longest
// the log keeps whole are held, a byte longer told.
//
// Mutation: compare the arguments' text, or capture less than the log keeps,
// and a case goes red.
func TestAGeminiPartsArgumentsAreReadByTheLogsOwnRule(t *testing.T) {
	t.Parallel()
	ghReadCases(t, "round.run0")
	call := ohCall(1, "echo", `{"b":1, "a":"x"}`, "done")
	h, err := ghRead(t, ghRound(`{"a":"x","b":1}`, "done"), ghBegan, call)
	if got := ohHeld(t, h, err, call); len(got) != 1 {
		t.Errorf("arguments spelled in another order: held %v; want the call", got)
	}
	bound := sandbox.MaxSubmissionArgsBytes
	for size, held := range map[int]bool{bound: true, bound + 1: false} {
		args := `{"a":"` + strings.Repeat("p", size-len(`{"a":""}`)) + `"}`
		c := ohCall(1, "echo", args, "done")
		h, err := ghRead(t, ghRound(args, "done"), ghBegan, c)
		if got := ohHeld(t, h, err, c); (len(got) == 1) != held {
			t.Errorf("arguments of %d bytes: held %v; want held %t", size, got, held)
		}
	}
}

// A SCRUBBED TEXT NEVER STANDS FOR A CUT OUTPUT: an output the log cut, whose
// file text is its own but for the scrub's marker past the opening the log
// kept, at any offset across the reading's pieces, is told; the text itself
// is held.
//
// Mutation: drop the marker's check, or its carry across pieces, and a case
// goes red.
func TestAScrubbedTextNeverStandsForACutGeminiOutput(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("v", 40<<10)
	call := ohCut(ohCall(1, "echo", `{"t":1}`, text))
	if call.OutputBytes == 0 {
		t.Fatal("the log keeps the output whole")
	}
	h, err := ghRead(t, ghRound(`{"t":1}`, text), ghBegan, call)
	if got := ohHeld(t, h, err, call); len(got) != 1 {
		t.Fatalf("the text itself: held %v; want the call", got)
	}
	for at := textDigestBytes - 80; at < textDigestBytes; at++ {
		marked := text[:at] + redact.Marker + text[at+len(redact.Marker):]
		h, err := ghRead(t, ghRound(`{"t":1}`, marked), ghBegan, call)
		if got := ohHeld(t, h, err, call); len(got) != 0 {
			t.Errorf("the marker at %d: held %v; want it told", at, got)
		}
	}
}

// A GEMINI CLI SESSION ANOTHER SERVER COULD NAME IS NOT READ: a second server
// `crewlet_x` answered `mcp_crewlet_x_echo` in the bridge's place, and its
// file says nothing of it; its box failed the guard, so it is no reading,
// though the same file read as unshadowed would hold the call.
//
// Mutation: drop the guard's check, and this goes red.
func TestAGeminiSessionAnotherServerCouldNameIsNotRead(t *testing.T) {
	t.Parallel()
	m := ghFixtures(t)
	ghReadCase(t, m, "name-collision.run0")
	c := m.Cases["name-collision.run0"]
	shadowed := false
	if c.Unshadowed == nil || *c.Unshadowed != shadowed {
		t.Fatal("the fixture's box is not one that failed the guard")
	}
	calls := ghAsk(t, m.Calls[c.Calls])
	tr := clArchive(t, [3]string{ghMember(c.Session), "0", string(ghGunzipped(t, c.File))})
	h, err := readGeminiHoldings(t.Context(), tr, c.Session, sandbox.RunStart{Began: true},
		sandbox.HoldingsAsk{Calls: calls, Resumer: "0.70.0"})
	if got := ohHeld(t, h, err, calls...); len(got) != 1 {
		t.Errorf("the file read unguarded holds %v; want the second server's answer held, as the guard prevents", got)
	}
}

// A GEMINI CLI READING IS HELD TO ITS BOUNDS: plain past a lowered meter, an
// entry after the chat file, a store's stream failing past the archive, an
// archive cut short of its gzip trailer or failing its checksum past tar's
// end, an id past 512 bytes, ids, run records, calls and answers past theirs
// are no reading; each at its bound is read.
//
// Mutation: drop a bound, the tar's end check or either drain, and a case
// goes red.
func TestGeminiReadingIsHeldToItsBounds(t *testing.T) {
	t.Parallel()
	call := ohCall(1, "echo", `{"t":1}`, "done")
	file := ghRound(`{"t":1}`, "done")
	ask := sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{call}, Resumer: "0.70.0"}
	tarred, _ := io.ReadAll(clArchive(t, [3]string{ghMember(ghSession), "0", file}))
	plain := func(r io.Reader) error {
		h, err := readGeminiHoldings(t.Context(), r, ghSession, ghBegan, ask)
		if err == nil && !h.Read {
			t.Fatal("a reading of nothing with no error")
		}
		return err
	}
	if err := plain(bytes.NewReader(tarred)); err != nil {
		t.Fatalf("the file within every bound: %v", err)
	}
	if err := plain(boundedPlain(bytes.NewReader(tarred), int64(len(tarred)-1))); err == nil {
		t.Error("plain past its meter: read")
	}
	if err := plain(clArchive(t, [3]string{ghMember(ghSession), "0", file}, [3]string{"x", "0", "y"})); err == nil {
		t.Error("an entry after the chat file: read")
	}
	if _, err := (GeminiCLI{}).readHoldings(t.Context(), io.MultiReader(bytes.NewReader(ghTarGz(t,
		ghMember(ghSession), []byte(file))), ohFailing{}), ghArchive("0.70.0"), ask, plainBounds(8<<20)); err == nil {
		t.Error("a store's stream failing past the archive: read")
	}
	whole := ghTarGz(t, ghMember(ghSession), []byte(file))
	badSum := append([]byte(nil), whole...)
	badSum[len(badSum)-8] ^= 0xff
	for name, tc := range map[string]struct {
		archive []byte
		read    bool
	}{
		"its whole archive":                        {whole, true},
		"cut short of its trailer, past tar's end": {whole[:len(whole)-8], false},
		"a checksum its plaintext fails":           {badSum, false},
	} {
		h, err := (GeminiCLI{}).readHoldings(t.Context(), bytes.NewReader(tc.archive), ghArchive("0.70.0"), ask,
			plainBounds(8<<20))
		if read := err == nil && h.Read; read != tc.read {
			t.Errorf("%s: a reading = %+v, %v; want read %t", name, h.Read, err, tc.read)
		}
	}
	entries := func(n int, line func(i int) string) []string {
		lines := make([]string, n)
		for i := range lines {
			lines[i] = line(i)
		}
		return lines
	}
	user := func(i int) string { return `{"id":"u` + strconv.Itoa(i) + `","type":"user","content":"x"}` }
	calls := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = `{"id":"c` + strconv.Itoa(i) + `","status":"scheduled"}`
		}
		return `{"id":"r","type":"gemini","content":"","toolCalls":[` + strings.Join(parts, ",") + `]}`
	}
	answers := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = ghAnswerOf("c"+strconv.Itoa(i), "echo", "x")
		}
		return ghAnswers("u", parts...)
	}
	ids := geminiHoldingsIDs
	most := geminiHoldingsRunMost
	for name, tc := range map[string]struct {
		lines []string
		start sandbox.RunStart
		read  bool
	}{
		"an id of 512 bytes":         {[]string{user(0), `{"id":"` + strings.Repeat("i", 512) + `","type":"user"}`}, ghBegan, true},
		"an id of 513 bytes":         {[]string{user(0), `{"id":"` + strings.Repeat("i", 513) + `","type":"user"}`}, ghBegan, false},
		"ids at their bound":         {entries(ids, user), sandbox.RunStart{Lines: int64(ids + 1)}, true},
		"ids past their bound":       {entries(ids+1, user), sandbox.RunStart{Lines: int64(ids + 2)}, false},
		"run records at their bound": {entries(most, user), ghBegan, true},
		"run records past it":        {entries(most+1, user), ghBegan, false},
		"calls at their bound":       {[]string{calls(most)}, ghBegan, true},
		"calls past it":              {[]string{calls(most + 1)}, ghBegan, false},
		"answers at their bound":     {[]string{answers(most)}, ghBegan, true},
		"answers past it":            {[]string{answers(most + 1)}, ghBegan, false},
	} {
		h, err := ghRead(t, ghFile(tc.lines...), tc.start, call)
		if read := err == nil && h.Read; read != tc.read {
			t.Errorf("%s: a reading = %+v, %v; want read %t", name, h.Read, err, tc.read)
		}
	}
}

// A GEMINI CLI READING STOPS BETWEEN LINES, AND BETWEEN PARTS, WHEN ITS
// CONTEXT ENDS: a caller gone before a file's third line, or before a reply's
// second call, is no reading.
//
// Mutation: ask the context per line alone, and the second case goes red.
func TestGeminiReadingStopsBetweenLinesAndParts(t *testing.T) {
	t.Parallel()
	call := ohCall(1, "echo", `{"t":1}`, "done")
	part := ghCallOf("c1", "echo", `{"t":1}`)
	for name, tc := range map[string]struct {
		file string
		asks int
	}{
		// The walk asks before each line and each element of a list: the
		// third ask is line 3's, the fourth line 2's second call's.
		"between lines": {ghFile(`{"$set":{}}`, `{"$set":{}}`, `{"$set":{}}`), 3},
		"between parts": {ghFile(ghReply("r1", part, strings.Replace(part, "c1", "c2", 1))), 4},
	} {
		ctx := &ohCountdown{Context: t.Context(), left: tc.asks - 1}
		_, err := readGeminiHoldings(ctx, clArchive(t, [3]string{ghMember(ghSession), "0", tc.file}), ghSession,
			ghBegan, sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{call}, Resumer: "0.70.0"})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s: %v; want the caller's end", name, err)
		}
	}
}

// ghLineAllocates is the most [TestGeminiReadingHoldsNoLine] allows a 64 MiB
// line's reading to allocate: its scanner's and line reader's 64 KiB buffers,
// two 32 KiB pieces, the tar reader and the walk's maps, under 256 KiB; the
// line held once is 256 times it.
const ghLineAllocates = 256 << 10

// A GEMINI CLI READING HOLDS NO LINE: a person's record of one 64 MiB answer
// is read and its cut call held, within [ghLineAllocates] whatever the
// line's length.
//
// Not parallel: it reads the process's allocation count, the fewest bytes of
// three reads ([allocationsOf]).
//
// Mutation: hold an output or a line whole, and this goes red.
func TestGeminiReadingHoldsNoLine(t *testing.T) {
	text := strings.Repeat("v", 64<<20)
	call := ohCut(ohCall(1, "echo", `{"t":1}`, text))
	head, tail, _ := strings.Cut(ghRound(`{"t":1}`, "PLACE"), "PLACE")
	archive, _ := io.ReadAll(clArchive(t, [3]string{ghMember(ghSession), "0", head + text + tail}))
	ask := sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{call}, Resumer: "0.70.0"}
	var (
		tarred bytes.Reader
		h      sandbox.SessionHoldings
		err    error
	)
	_, allocated := allocationsOf(t, func() {
		tarred.Reset(archive)
		h, err = readGeminiHoldings(context.Background(), &tarred, ghSession, ghBegan, ask)
	})
	if got := ohHeld(t, h, err, call); len(got) != 1 {
		t.Fatalf("a 64 MiB answer: held %v; want the call", got)
	}
	if allocated > ghLineAllocates {
		t.Errorf("a 64 MiB line allocated %d bytes; want at most %d", allocated, ghLineAllocates)
	}
}

// A GEMINI CLI READING'S CAUSE QUOTES NOTHING OF THE FILE: refused inside a 4
// MiB text, its error holds none of the text's words; refused on a fault whose
// words run a mebibyte, its error is within the detail's bound.
//
// Mutation: carry the cause whole in the reading's words, and this goes red.
func TestGeminiReadingsCauseQuotesNothing(t *testing.T) {
	t.Parallel()
	call := ohCall(1, "echo", `{"t":1}`, "done")
	ask := sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{call}, Resumer: "0.70.0"}
	secret := strings.Repeat("the secret words ", 1<<18)
	for name, plain := range map[string]io.Reader{
		"inside a text": clArchive(t, [3]string{ghMember(ghSession), "0",
			ghFile(`{"id":"u1","type":"user","content":"` + secret + "\x01\"}")}),
		"on a long fault": io.MultiReader(bytes.NewReader(func() []byte {
			b, _ := io.ReadAll(clArchive(t, [3]string{ghMember(ghSession), "0", ghFile()}))
			return b[:512]
		}()), ohFault{errors.New(secret)}),
	} {
		_, err := readGeminiHoldings(t.Context(), plain, ghSession, ghBegan, ask)
		if err == nil {
			t.Fatalf("%s: read", name)
		}
		if msg := err.Error(); len(msg) > transcriptDetailLimit+200 || strings.Contains(msg, "secret words") &&
			name == "inside a text" {
			t.Errorf("%s: the cause is %d bytes, or quotes the file", name, len(msg))
		}
	}
}

// THE GEMINI CLI READER TAKES NO SLOT OF ITS OWN: a reading under
// ReadHoldings, which holds the readings' one slot, completes and gives it
// back.
//
// Not parallel: the slots are the process's own.
//
// Mutation: take the slots in the reader, and this goes red.
func TestGeminiReadingTakesNoSlotOfItsOwn(t *testing.T) {
	call := ohCall(1, "echo", `{"t":1}`, "done")
	archive := ghTarGz(t, ghMember(ghSession), []byte(ghRound(`{"t":1}`, "done")))
	type reading struct {
		got sandbox.SessionHoldings
		err error
	}
	done := make(chan reading, 1)
	go func() {
		got, err := NewGeminiCLI().ReadHoldings(t.Context(), ghArchive("0.70.0"), func(context.Context) (io.ReadCloser,
			error) {
			return io.NopCloser(bytes.NewReader(archive)), nil
		}, sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{call}, Resumer: "0.70.0"})
		done <- reading{got, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || !r.got.Read || len(holdingsReads) != 0 {
			t.Errorf("a reading = %+v, %v, readings' slot held after %d; want it read and the slot given back",
				r.got, r.err, len(holdingsReads))
		}
	case <-time.After(time.Minute): //fixedwait:liveness
		t.Fatal("a reading of one round waited a minute: the reader waits on a slot its caller holds")
	}
}
