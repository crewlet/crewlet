package executor

import (
	"bytes"
	"cmp"
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
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// These cases read an OpenCode session's kept export for which of a round's
// calls it holds ([readOpenCodeHoldings]); contentverdict_internal_test.go
// holds the verdict's own rules. Every arm that can hold a call has a case
// where the session lacks the call, and holds none.

// ohSession is the session every export here is of.
const ohSession = "ses_0a1b2c3d4e5f60718293a4b5c6d7e8f9"

// ohUser is the brief the run's session opens with.
const ohUser = `{"id":"msg_user","type":"user","text":"the brief","time":{"created":1}}`

// ohDoc is an export of ohSession holding messages, each step and compaction
// given an id of its own, as the import keys a message by its id.
func ohDoc(messages ...string) string {
	doc := `{"info":{"id":"` + ohSession + `","title":"a run"},"messages":[` + strings.Join(messages, ",") + `]}`
	for _, id := range []string{"msg_step", "msg_c"} {
		pieces := strings.Split(doc, `"id":"`+id+`"`)
		var b strings.Builder
		for i, piece := range pieces {
			if i > 0 {
				b.WriteString(`"id":"` + id + strconv.Itoa(i) + `"`)
			}
			b.WriteString(piece)
		}
		doc = b.String()
	}
	return doc
}

// ohStepOpen and ohStepClose are a settled assistant step around its items,
// as OpenCode writes one ([ohStep]).
const (
	ohStepOpen = `{"id":"msg_step","type":"assistant","agent":"build","model":{"id":"m"},"time":{"created":2,` +
		`"streamed":3,"completed":4},"content":[`
	ohStepClose = `],"finish":"tool-calls","cost":0}`
)

// ohStep is a settled assistant step holding items.
func ohStep(items ...string) string { return ohStepOpen + strings.Join(items, ",") + ohStepClose }

// ohQuote is s as a JSON string.
func ohQuote(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// ohDone is a completed part of the engine's tool name, called with input,
// answered with text, as OpenCode writes it.
func ohDone(name, input, text string) string {
	return `{"type":"tool","id":"call_x","name":"crewlet_` + name + `","executed":false,"state":{"status":` +
		`"completed","input":` + input + `,"content":[{"type":"text","text":` + ohQuote(text) + `}],` +
		`"metadata":{"truncated":false}},"time":{"created":1,"ran":2,"completed":3}}`
}

// ohFailed is a failed part of the engine's tool name, its message message.
func ohFailed(name, input, message string) string {
	return `{"type":"tool","id":"call_x","name":"crewlet_` + name + `","executed":false,"state":{"status":` +
		`"error","input":` + input + `,"error":{"type":"tool.execution","message":` + ohQuote(message) + `}},` +
		`"time":{"created":1,"ran":2,"completed":3}}`
}

// ohCall is the call the round's log took appended'th, admitted at the same
// place, of tool name with args, answered output, asked about.
func ohCall(appended int, name, args, output string) sandbox.LoggedCall {
	sum, err := toolargs.Key([]byte(args))
	if err != nil {
		panic(err)
	}
	return sandbox.LoggedCall{ID: strconv.Itoa(appended), Asked: true, Output: output, Appended: appended,
		Seq: appended, Name: name, Args: sandbox.ArgsKey{Whole: true, Sum: sum}}
}

// ohCut is c with its output cut as the run's log cuts one past what it
// keeps whole.
func ohCut(c sandbox.LoggedCall) sandbox.LoggedCall {
	c.Output, c.OutputBytes = kept.Record("the run's record").Text(c.Output, sandbox.MaxBridgeOutputBytes)
	return c
}

// ohRead reads doc, from start, for calls.
func ohRead(t *testing.T, doc string, start sandbox.RunStart, calls ...sandbox.LoggedCall) (sandbox.SessionHoldings,
	error) {
	t.Helper()
	return readOpenCodeHoldings(t.Context(), strings.NewReader(doc), ohSession, start,
		sandbox.HoldingsAsk{Calls: calls, Resumer: "2.3.2"})
}

// ohBegan is a run that began its session.
var ohBegan = sandbox.RunStart{Began: true}

// ohHeld is the places of the calls of calls h holds, failing the case where
// it read nothing.
func ohHeld(t *testing.T, h sandbox.SessionHoldings, err error, calls ...sandbox.LoggedCall) []int {
	t.Helper()
	if err != nil || !h.Read {
		t.Fatalf("no reading: %v", err)
	}
	var held []int
	for _, c := range calls {
		if in, under := h.Holds(c); in {
			held = append(held, c.Appended)
		} else if under != "" {
			t.Errorf("call %d told under %q; a content reading tells every call in the main list", c.Appended, under)
		}
	}
	return held
}

// ohNone fails the case where err is no refusal: the reading read nothing.
func ohNone(t *testing.T, h sandbox.SessionHoldings, err error) {
	t.Helper()
	if err == nil || h.Read {
		t.Fatalf("a reading = %+v; want none", h)
	}
}

// A MESSAGE IS DECODED IN PLACE, WITH ITS KEY FACTS: the decode keeps each
// member read in place, a name written twice by its last value (as JSON.parse
// reads it), and says which of its names it met, whether one was met twice
// and whether one was none of its own; a person's message reads none of them.
//
// Mutation: keep a name's first value, or drop the facts' twice or other, and
// this goes red.
func TestAMessageIsDecodedInPlaceWithItsKeyFacts(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"type":"user","text":"first","later":1,"text":"second","metadata":{}}`)
	w := decodeOpenCodeMessage(raw)
	if got := w.message().Text; got != "second" {
		t.Errorf("text written twice reads %q; want its last value", got)
	}
	if !w.keys.twice || !w.keys.other || !w.keys.has(msgMetadata) || w.keys.has(msgID) {
		t.Errorf("facts = %+v; want twice, other and metadata met, id not", w.keys)
	}
	if &w.Text[0] != &raw[strings.LastIndex(string(raw), `"second"`)] {
		t.Error("the text is a copy; want the sub-slice of the message it is")
	}
	plain := decodeOpenCodeMessage(json.RawMessage(`{"type":"user","text":"x"}`))
	if plain.keys.twice || plain.keys.other || plain.keys.within(namesMask(msgType, msgText)) != true {
		t.Errorf("facts of a plain message = %+v", plain.keys)
	}
}

// AN OPENCODE READING READS ONLY THE RELEASES IT WAS MEASURED ON, each written
// whole: a resumer outside the set, an archive written by another release
// than the resumer, a member padded with blanks, and a release the count
// reads but the reading was not measured on are no reading, nothing opened.
// A member is read.
//
// Not parallel: it swaps the count's set.
//
// Mutation: drop the set check, read openCodeCountRead's set, trim the
// release, or leave Version unchecked, and a case opens its archive.
func TestAnOpenCodeReadingReadsOnlyReleasesItWasMeasuredOn(t *testing.T) {
	saved := openCodeCountReleases
	t.Cleanup(func() { openCodeCountReleases = saved })
	openCodeCountReleases = append(slices.Clone(saved), "2.4.0")
	call := ohCall(1, "echo", `{"tag":"a"}`, "done")
	archive := ohGzip(t, ohDoc(ohUser, ohStep(ohDone("echo", `{"tag":"a"}`, "done"))))
	for name, tc := range map[string]struct {
		version, resumer string
		read             bool
	}{
		"a member":                        {"2.3.2", "2.3.2", true},
		"a resumer outside the set":       {"2.3.1", "2.3.1", false},
		"a writer other than the resumer": {"2.3.1", "2.3.2", false},
		"a member padded with blanks":     {" 2.3.2", " 2.3.2", false},
		"a release only the count reads":  {"2.4.0", "2.4.0", false},
		"a resumer nobody recorded":       {"", "", false},
	} {
		opened := false
		got, err := NewOpenCode().ReadHoldings(t.Context(), ohArchive(tc.version),
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
	// THE READER'S OWN CHECK, apart from the shared one every reading passes.
	padded := sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{call}, Resumer: " 2.3.2"}
	if err := (OpenCode{}).checkHoldings(ohArchive(" 2.3.2"), padded); err == nil {
		t.Error("a member padded with blanks passes the reader's check; want it compared whole")
	}
}

// ohArchive is the session's archive as its row describes it, written by
// version, its run having begun the session.
func ohArchive(version string) sandbox.SessionArchive {
	return sandbox.SessionArchive{Harness: OpenCodeName, SessionID: ohSession, Format: openCodeArchiveFormat,
		Version: version, RunStart: ohBegan}
}

// ohGzip is doc gzipped, as the take keeps an export.
func ohGzip(t *testing.T, doc string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(doc)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// THE READING'S RELEASES ARE THE RELEASES MEASURED: 2.3.2, whose own exports
// of the engine's bridged calls read as every case predicted; never empty, each one the count reads and of readable numbers.
//
// Mutation: empty the set, add a release, or add one the count does not
// read, and this goes red.
func TestTheReadingsReleasesAreTheReleasesMeasured(t *testing.T) {
	t.Parallel()
	if want := []string{"2.3.2"}; !slices.Equal(openCodeHoldingsReleases, want) {
		t.Errorf("the reading's releases are %q; want those measured, %q", openCodeHoldingsReleases, want)
	}
	for _, release := range openCodeHoldingsReleases {
		if _, ok := releaseNumbers(release); !ok || !openCodeCountRead(release) {
			t.Errorf("%q is unreadable or no release the count reads", release)
		}
	}
}

// THE READING'S FLOOR IS ITS OLDEST RELEASE by its numbers, whatever the
// set's order and however its numbers sort as text.
//
// Not parallel: it swaps the set.
//
// Mutation: answer a literal or the set's first member, or compare releases
// as text, and this goes red.
func TestTheReadingsFloorIsItsOldestRelease(t *testing.T) {
	saved := openCodeHoldingsReleases
	t.Cleanup(func() { openCodeHoldingsReleases = saved })
	for want, set := range map[string][]string{
		"2.3.2": {"2.4.0", "2.3.2", "2.3.10"},
		"2.9.1": {"2.10.0", "2.9.1"},
	} {
		openCodeHoldingsReleases = set
		if got := (OpenCode{}).holdingsFloor(); got != want {
			t.Errorf("the floor of %q is %q; want %q", set, got, want)
		}
	}
}

// THE ENGINE'S TOOLS ARE READ BY THE NAME OPENCODE GIVES THEM: the prefix
// OpenCode's profile states is the server's name config reserves and `_`, a
// part named so is the bridge's tool, and with no prefix no part is.
//
// Mutation: read another harness's profile, or drop the empty prefix's
// refusal, and this goes red.
func TestTheEnginesToolsAreReadByTheNameOpenCodeGivesThem(t *testing.T) {
	t.Parallel()
	if got, want := openCodeBridgedPrefix(), config.ToolServerName+"_"; got != want {
		t.Fatalf("OpenCode's prefix for the engine's tools is %q; want %q", got, want)
	}
	raw := json.RawMessage(ohQuote(config.ToolServerName + "_echo"))
	if tool, ok := bridgedToolName(raw, openCodeBridgedPrefix()); !ok || tool != "echo" {
		t.Errorf("%s names %q, %t; want the bridge's echo", raw, tool, ok)
	}
	for _, raw := range []string{`"echo"`, ohQuote(config.ToolServerName + "_echo")} {
		if tool, ok := bridgedToolName(json.RawMessage(raw), ""); ok {
			t.Errorf("with no prefix, %s names the bridge's %q; want none", raw, tool)
		}
	}
}

// AN ASK IS VALIDATED BY ITS READER BEFORE ANY READ: past 200 calls, none
// asked, a call with no admission place, an asked call not keyed by its place
// in decimal, with no place, held for a person, or keyed alike with another,
// a run with no known start, or a session id of no OpenCode shape is no
// reading, nothing opened. An unasked call with no place in the log is read.
//
// Mutation: skip the asked calls' checks, or refuse an unasked call for its
// key, and a case goes red.
func TestAnAskIsValidatedByItsReaderBeforeAnyRead(t *testing.T) {
	t.Parallel()
	good := ohCall(1, "echo", `{}`, "done")
	unplaced := ohCall(2, "echo", `{}`, "other")
	unplaced.Appended, unplaced.ID, unplaced.Asked = 0, "0", false
	tooMany := make([]sandbox.LoggedCall, sandbox.MaxBridgeCalls+1)
	for i := range tooMany {
		tooMany[i] = ohCall(i+1, "echo", `{}`, "done")
	}
	with := func(edit func(*sandbox.LoggedCall)) []sandbox.LoggedCall {
		c := good
		edit(&c)
		return []sandbox.LoggedCall{c}
	}
	for name, tc := range map[string]struct {
		calls   []sandbox.LoggedCall
		archive sandbox.SessionArchive
		read    bool
	}{
		"an ask the log can make":             {[]sandbox.LoggedCall{good, unplaced}, ohArchive("2.3.2"), true},
		"too many calls":                      {tooMany, ohArchive("2.3.2"), false},
		"no call":                             {nil, ohArchive("2.3.2"), false},
		"none asked":                          {with(func(c *sandbox.LoggedCall) { c.Asked = false }), ohArchive("2.3.2"), false},
		"no admission place":                  {with(func(c *sandbox.LoggedCall) { c.Seq = 0 }), ohArchive("2.3.2"), false},
		"keyed by another number":             {with(func(c *sandbox.LoggedCall) { c.ID = "7" }), ohArchive("2.3.2"), false},
		"keyed as its id would be":            {with(func(c *sandbox.LoggedCall) { c.ID = "call_x" }), ohArchive("2.3.2"), false},
		"no place in the log":                 {with(func(c *sandbox.LoggedCall) { c.Appended, c.ID = 0, "0" }), ohArchive("2.3.2"), false},
		"held for a person":                   {with(func(c *sandbox.LoggedCall) { c.Held = true }), ohArchive("2.3.2"), false},
		"a key two calls carry":               {[]sandbox.LoggedCall{good, func() sandbox.LoggedCall { c := good; c.Asked = false; return c }()}, ohArchive("2.3.2"), false},
		"a run with no known start":           {[]sandbox.LoggedCall{good}, func() sandbox.SessionArchive { a := ohArchive("2.3.2"); a.RunStart = sandbox.RunStart{}; return a }(), false},
		"a session id of no OpenCode's shape": {[]sandbox.LoggedCall{good}, func() sandbox.SessionArchive { a := ohArchive("2.3.2"); a.SessionID = "5e55100a-0000"; return a }(), false},
	} {
		err := (OpenCode{}).checkHoldings(tc.archive, sandbox.HoldingsAsk{Calls: tc.calls, Resumer: "2.3.2"})
		if (err == nil) != tc.read {
			t.Errorf("%s: checkHoldings = %v; want read %t", name, err, tc.read)
		}
	}
}

// AN ASK PAST THE LOG'S BOUNDS IS NEVER READ: 257 calls in flight, two counts
// of dropped calls with no name, or dropped names and digests past the tally's
// bound are no reading; 256 in flight, and dropped identities at the bound, are
// read, counted apart.
//
// Mutation: leave Unlisted unbounded, or count in-flight and dropped calls
// together, and a case goes red.
func TestAnAskPastTheLogsBoundsIsNeverRead(t *testing.T) {
	t.Parallel()
	identities := func(n int, id sandbox.CallIdentity) []sandbox.CallIdentity {
		out := make([]sandbox.CallIdentity, n)
		for i := range out {
			out[i] = id
		}
		return out
	}
	name := strings.Repeat("n", sandbox.MaxElidedIdentityBytes/4)
	atBound := identities(4, sandbox.CallIdentity{Name: name})
	for label, tc := range map[string]struct {
		unlisted []sandbox.CallIdentity
		read     bool
	}{
		"256 in flight":                 {identities(sandbox.MaxCallsInFlight, sandbox.CallIdentity{Name: "x", InFlight: true}), true},
		"257 in flight":                 {identities(sandbox.MaxCallsInFlight+1, sandbox.CallIdentity{Name: "x", InFlight: true}), false},
		"two unnamed counts":            {identities(2, sandbox.CallIdentity{}), false},
		"dropped names at the bound":    {atBound, true},
		"dropped names past the bound":  {append(slices.Clone(atBound), sandbox.CallIdentity{Name: "y"}), false},
		"in flight beside dropped ones": {append(identities(sandbox.MaxCallsInFlight, sandbox.CallIdentity{Name: "x", InFlight: true}), atBound...), true},
	} {
		err := (OpenCode{}).checkHoldings(ohArchive("2.3.2"), sandbox.HoldingsAsk{Resumer: "2.3.2",
			Calls: []sandbox.LoggedCall{ohCall(1, "echo", `{}`, "done")}, Unlisted: tc.unlisted})
		if (err == nil) != tc.read {
			t.Errorf("%s: checkHoldings = %v; want read %t", label, err, tc.read)
		}
	}
}

// ohClass is c's class, which the case states.
func ohClass(t *testing.T, c sandbox.LoggedCall) sandbox.CallClass {
	t.Helper()
	class, ok := sandbox.ClassOf(c)
	if !ok {
		t.Fatalf("call %d states no class", c.Appended)
	}
	return class
}

// A REPORTED SKILL LOAD IS TOLD: OpenCode's plugin reports a load its own
// `skill` tool made, which the log keeps as a `load_tool_skill` call asked
// like any other, and its session holds that tool's part, never one of
// `crewlet_load_tool_skill`; so the load is told, with or without the native
// part, and the run's other call beside it held.
//
// Mutation: read OpenCode's own `skill` part as the load's, or hold a class
// no part was counted for, and a case goes red.
func TestAReportedSkillLoadIsTold(t *testing.T) {
	t.Parallel()
	echo := ohCall(1, "echo", `{"t":1}`, "done")
	load := ohCall(2, "load_tool_skill", `{"name":"crewlet:review"}`, "loaded")
	native := strings.Replace(ohDone("load_tool_skill", `{"name":"crewlet:review"}`, "loaded"),
		`"crewlet_load_tool_skill"`, `"skill"`, 1)
	if !strings.Contains(native, `"name":"skill"`) {
		t.Fatal("the native part names no skill tool")
	}
	for name, doc := range map[string]string{
		"the native part": ohDoc(ohUser, ohStep(ohDone("echo", `{"t":1}`, "done"), native)),
		"no part":         ohDoc(ohUser, ohStep(ohDone("echo", `{"t":1}`, "done"))),
	} {
		h, err := ohRead(t, doc, ohBegan, echo, load)
		if got := ohHeld(t, h, err, echo, load); !slices.Equal(got, []int{1}) {
			t.Errorf("%s: held %v; want the run's echo alone, the load told", name, got)
		}
	}
}

// ONLY A PART AFTER THE RUN'S START COUNTS FOR ITS ROUND: an identical part
// before the message the run began after never counts; a run that began its
// session counts from the first; a start not found, or found twice, is no
// reading.
//
// Mutation: ignore the start, or take the first of two, and a case goes red.
func TestOnlyAPartAfterTheRunsStartCountsForItsRound(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "echo", `{"t":1}`, "done")
	before := ohStep(ohDone("echo", `{"t":1}`, "done"))
	start := `{"id":"msg_start","type":"user","text":"go on","time":{"created":5}}`
	after := sandbox.RunStart{After: "msg_start"}
	h, err := ohRead(t, ohDoc(ohUser, before, start), after, c)
	if got := ohHeld(t, h, err, c); got != nil {
		t.Errorf("a part before the start: held %v; want none", got)
	}
	h, err = ohRead(t, ohDoc(ohUser, before, start), ohBegan, c)
	if got := ohHeld(t, h, err, c); !slices.Equal(got, []int{1}) {
		t.Errorf("a run that began its session: held %v; want its part counted", got)
	}
	h, err = ohRead(t, ohDoc(ohUser, before), after, c)
	ohNone(t, h, err)
	h, err = ohRead(t, ohDoc(ohUser, start, before, start), after, c)
	ohNone(t, h, err)
}

// THE MESSAGE A RUN BEGAN AFTER IS NO PART OF THE RUN: a step holding a part
// identical to a run call's is the start, and does not count; a compaction as
// the start leaves Compacted false.
//
// Mutation: read the start message as the run's, and this goes red.
func TestTheMessageARunBeganAfterIsNoPartOfTheRun(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "echo", `{"t":1}`, "done")
	start := strings.Replace(ohStep(ohDone("echo", `{"t":1}`, "done")), "msg_step", "msg_start", 1)
	h, err := ohRead(t, ohDoc(ohUser, start), sandbox.RunStart{After: "msg_start"}, c)
	if got := ohHeld(t, h, err, c); got != nil {
		t.Errorf("held %v; want the start's part not counted", got)
	}
	compaction := `{"id":"msg_start","type":"compaction","status":"completed","summary":"s","time":{"created":5}}`
	h, err = ohRead(t, ohDoc(ohUser, compaction, ohStep(ohDone("echo", `{"t":1}`, "done"))),
		sandbox.RunStart{After: "msg_start"}, c)
	if got := ohHeld(t, h, err, c); !slices.Equal(got, []int{1}) || h.Compacted {
		t.Errorf("a compaction as the start: held %v, compacted %t; want held, not compacted", got, h.Compacted)
	}
}

// A PART BEFORE THE NEWEST COMPACTION NEVER COUNTS: a compaction after the
// start, completed or failed, drops every part before it, and says so; the
// newest one counts, not the first.
//
// Mutation: keep part facts across a compaction, drop them only at the
// first, and a case goes red.
func TestAPartBeforeTheNewestCompactionNeverCounts(t *testing.T) {
	t.Parallel()
	a, b := ohCall(1, "echo", `{"t":1}`, "a"), ohCall(2, "echo", `{"t":2}`, "b")
	compaction := func(status string) string {
		return `{"id":"msg_c","type":"compaction","status":"` + status + `","summary":"s","time":{"created":5}}`
	}
	// a's part lies before both compactions, one of b's between them and one
	// after the newest: b alone has one live part, its call's.
	for _, status := range []string{"completed", "failed", "running"} {
		h, err := ohRead(t, ohDoc(ohUser, ohStep(ohDone("echo", `{"t":1}`, "a")), compaction(status),
			ohStep(ohDone("echo", `{"t":2}`, "b")), compaction(status), ohStep(ohDone("echo", `{"t":2}`, "b"))),
			ohBegan, a, b)
		got := ohHeld(t, h, err, a, b)
		if !slices.Equal(got, []int{2}) || !h.Compacted {
			t.Errorf("a %s compaction: held %v, compacted %t; want b alone, compacted", status, got, h.Compacted)
		}
	}
	h, err := ohRead(t, ohDoc(ohUser, ohStep(ohDone("echo", `{"t":1}`, "a")), ohStep(ohDone("echo", `{"t":2}`, "b"))),
		ohBegan, a, b)
	if got := ohHeld(t, h, err, a, b); !slices.Equal(got, []int{1, 2}) || h.Compacted {
		t.Errorf("no compaction: held %v, compacted %t; want both, not compacted", got, h.Compacted)
	}
}

// ONLY A PART OPENCODE HANDS ITS MODEL VERBATIM COUNTS: every variant of a
// counted part a release could mark, cut, run elsewhere or hand otherwise is
// passed, and its call told.
//
// Mutation: read truncated as anything but exactly false, ignore unknown
// keys, or allow a provider's keys, and a case goes red.
func TestOnlyAPartOpenCodeHandsItsModelVerbatimCounts(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "echo", `{"t":1}`, "done")
	part := ohDone("echo", `{"t":1}`, "done")
	failedCall := ohCall(1, "echo", `{"t":1}`, "boom")
	failedCall.Failed = true
	failed := ohFailed("echo", `{"t":1}`, "boom")
	for name, tc := range map[string]struct {
		part string
		call sandbox.LoggedCall
	}{
		"truncated":                {strings.Replace(part, `"truncated":false`, `"truncated":true`, 1), c},
		"a second metadata member": {strings.Replace(part, `"truncated":false`, `"truncated":false,"outputPath":"x"`, 1), c},
		"metadata absent":          {strings.Replace(part, `,"metadata":{"truncated":false}`, ``, 1), c},
		"two text items":           {strings.Replace(part, `"content":[{"type":"text","text":"done"}]`, `"content":[{"type":"text","text":"done"},{"type":"text","text":""}]`, 1), c},
		"a file item":              {strings.Replace(part, `{"type":"text","text":"done"}`, `{"type":"file","text":"done"}`, 1), c},
		"an extra item key":        {strings.Replace(part, `{"type":"text","text":"done"}`, `{"type":"text","text":"done","x":1}`, 1), c},
		"executed":                 {strings.Replace(part, `"executed":false`, `"executed":true`, 1), c},
		"Code Mode's execute":      {strings.Replace(part, `"crewlet_echo"`, `"execute"`, 1), c},
		"another server's":         {strings.Replace(part, `"crewlet_echo"`, `"seat_echo"`, 1), c},
		"a name outside the rule":  {strings.Replace(part, `"crewlet_echo"`, `"crewlet_ec ho"`, 1), c},
		"aborted":                  {strings.Replace(failed, `"tool.execution"`, `"aborted"`, 1), failedCall},
		"an error with content":    {strings.Replace(failed, `"error":{`, `"content":[],"error":{`, 1), failedCall},
		"an error with metadata":   {strings.Replace(failed, `"error":{`, `"metadata":{"truncated":false},"error":{`, 1), failedCall},
		"an unknown state key":     {strings.Replace(part, `"metadata":`, `"compacted":1,"metadata":`, 1), c},
		"an unknown part key":      {strings.Replace(part, `"executed":false`, `"executed":false,"cleared":true`, 1), c},
		"a part with text":         {strings.Replace(part, `"executed":false`, `"executed":false,"text":"x"`, 1), c},
		"native":                   {strings.Replace(part, `"executed":false`, `"executed":false,"native":{}`, 1), c},
		"providerState":            {strings.Replace(part, `"executed":false`, `"executed":false,"providerState":{}`, 1), c},
		"providerResultState":      {strings.Replace(part, `"executed":false`, `"executed":false,"providerResultState":{}`, 1), c},
		"an unknown time key":      {strings.Replace(part, `"ran":2`, `"ran":2,"queued":1`, 1), c},
		"an input of no object":    {strings.Replace(part, `"input":{"t":1}`, `"input":"{\"t\":1}"`, 1), c},
		"running":                  {strings.Replace(part, `"status":"completed"`, `"status":"running"`, 1), c},
	} {
		if !strings.Contains(tc.part, "crewlet_") && !strings.Contains(tc.part, "execute") &&
			!strings.Contains(tc.part, "seat_") {
			t.Fatalf("%s: the variant did not apply", name)
		}
		if tc.part == part || tc.part == failed {
			t.Fatalf("%s: the variant did not apply", name)
		}
		h, err := ohRead(t, ohDoc(ohUser, ohStep(tc.part)), ohBegan, tc.call)
		if got := ohHeld(t, h, err, tc.call); got != nil {
			t.Errorf("%s: held %v; want the part passed", name, got)
		}
	}
	for name, tc := range map[string]struct {
		part string
		call sandbox.LoggedCall
	}{"completed": {part, c}, "failed": {failed, failedCall}} {
		h, err := ohRead(t, ohDoc(ohUser, ohStep(tc.part)), ohBegan, tc.call)
		if got := ohHeld(t, h, err, tc.call); !slices.Equal(got, []int{1}) {
			t.Errorf("the %s part as written: held %v; want it counted", name, got)
		}
	}
}

// A MARKED CHANGE ENDS THE READING: a fourth item type, an assistant step with
// an unknown key or `metadata`, a step `time` name outside its set (a release
// adding one is a format no measurement read), an item no object, a list cut
// part way or no list, a `completed` time null, 0, one a double reads as 0, a
// string or absent, and no `info` are no reading.
//
// Mutation: drop the item faults as the person reader does, read `completed`
// for its presence, or for a digit other than 0, let a step's `time` hold any
// name, and a case goes red.
func TestAMarkedChangeEndsTheReading(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "echo", `{"t":1}`, "done")
	step := ohStep(ohDone("echo", `{"t":1}`, "done"))
	for name, doc := range map[string]string{
		"a fourth item type":     ohDoc(ohUser, strings.Replace(step, `"content":[`, `"content":[{"type":"image"},`, 1)),
		"an unknown step key":    ohDoc(ohUser, strings.Replace(step, `"cost":0`, `"cost":0,"later":1`, 1)),
		"metadata":               ohDoc(ohUser, strings.Replace(step, `"cost":0`, `"cost":0,"metadata":{}`, 1)),
		"an unknown time name":   ohDoc(ohUser, strings.Replace(step, `"streamed":3`, `"lastToken":3,"streamed":3`, 1)),
		"an item no object":      ohDoc(ohUser, strings.Replace(step, `"content":[`, `"content":[7,`, 1)),
		"a list that is no list": ohDoc(ohUser, strings.Replace(ohStep(), `"content":[]`, `"content":{}`, 1)),
		"completed null":         ohDoc(ohUser, strings.Replace(step, `"completed":4`, `"completed":null`, 1)),
		"completed 0":            ohDoc(ohUser, strings.Replace(step, `"completed":4`, `"completed":0`, 1)),
		"completed 0.0e5":        ohDoc(ohUser, strings.Replace(step, `"completed":4`, `"completed":0.0e5`, 1)),
		"completed 1e-400":       ohDoc(ohUser, strings.Replace(step, `"completed":4`, `"completed":1e-400`, 1)),
		"completed a string":     ohDoc(ohUser, strings.Replace(step, `"completed":4`, `"completed":"4"`, 1)),
		"completed absent":       ohDoc(ohUser, strings.Replace(step, `,"completed":4`, ``, 1)),
		"a step with no content": ohDoc(ohUser, strings.Replace(ohStep(), `"content":[],`, ``, 1)),
		"no info":                strings.Replace(ohDoc(ohUser, step), `"info":{"id":"`+ohSession+`","title":"a run"},`, ``, 1),
		"info of no object":      strings.Replace(ohDoc(ohUser, step), `{"id":"`+ohSession+`","title":"a run"}`, `"`+ohSession+`"`, 1),
		"a message of no object": ohDoc(ohUser, `"words"`, step),
		"a type of no string":    ohDoc(ohUser, `{"type":7}`, step),
	} {
		if !strings.Contains(doc, `"messages"`) || doc == ohDoc(ohUser, step) {
			t.Fatalf("%s: the variant did not apply", name)
		}
		h, err := ohRead(t, doc, ohBegan, c)
		if err == nil || h.Read {
			t.Errorf("%s: a reading = %+v; want none", name, h)
		}
	}
}

// A STEP'S FIRSTTOKEN IS READ FOR NOTHING: 2.3.2 stamps every answered step's `time`
// with `firstToken`, and a step holding it, whatever its value, holds its
// call as a step without it does; written twice, it is no reading.
//
// Mutation: drop `firstToken` from the step's set, or read it where
// `completed` is read, and a case goes red.
func TestAStepsFirstTokenIsReadForNothing(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "echo", `{"t":1}`, "done")
	step := ohStep(ohDone("echo", `{"t":1}`, "done"))
	for _, value := range []string{`3`, `null`, `"3"`, `0`} {
		stamped := strings.Replace(step, `"streamed":3`, `"firstToken":`+value+`,"streamed":3`, 1)
		if stamped == step {
			t.Fatal("the variant did not apply")
		}
		h, err := ohRead(t, ohDoc(ohUser, stamped), ohBegan, c)
		if got := ohHeld(t, h, err, c); !slices.Equal(got, []int{1}) {
			t.Errorf("a step's firstToken %s: held %v; want [1]", value, got)
		}
	}
	twice := strings.Replace(step, `"streamed":3`, `"firstToken":3,"firstToken":3,"streamed":3`, 1)
	h, err := ohRead(t, ohDoc(ohUser, twice), ohBegan, c)
	ohNoneNamed(t, "firstToken twice", h, err)
}

// A KEY WRITTEN TWICE ENDS THE READING OR PASSES ITS PART: `messages` or
// `info` twice, a name twice in an assistant step or its `time`, is no
// reading; `state` twice in a part passes that part.
//
// Mutation: read the first value, or allow duplicates, and a case goes red.
func TestAKeyWrittenTwiceEndsTheReadingOrPassesItsPart(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "echo", `{"t":1}`, "done")
	part := ohDone("echo", `{"t":1}`, "done")
	step := ohStep(part)
	doc := ohDoc(ohUser, step)
	for name, twice := range map[string]string{
		"messages":         strings.Replace(doc, `"messages":[`, `"messages":[],"messages":[`, 1),
		"info":             strings.Replace(doc, `"messages":`, `"info":{"id":"`+ohSession+`"},"messages":`, 1),
		"a step's content": ohDoc(ohUser, strings.Replace(step, `"content":[`, `"content":[],"content":[`, 1)),
		"a step's time":    ohDoc(ohUser, strings.Replace(step, `"streamed":3`, `"streamed":3,"streamed":3`, 1)),
	} {
		h, err := ohRead(t, twice, ohBegan, c)
		ohNoneNamed(t, name, h, err)
	}
	stateTwice := strings.Replace(part, `"state":{`, `"state":{"status":"error"},"state":{`, 1)
	h, err := ohRead(t, ohDoc(ohUser, ohStep(stateTwice)), ohBegan, c)
	if got := ohHeld(t, h, err, c); got != nil {
		t.Errorf("state twice in a part: held %v; want the part passed", got)
	}
}

// ohNoneNamed is [ohNone] for the case named.
func ohNoneNamed(t *testing.T, name string, h sandbox.SessionHoldings, err error) {
	t.Helper()
	if err == nil || h.Read {
		t.Errorf("%s: a reading = %+v; want none", name, h)
	}
}

// A TEXT IS MATCHED BY ITS DECODED SIZE: a part whose text is escaped six
// bytes a character, the most an escape spans, is its class's text and held;
// one a character longer is another size and told.
//
// Mutation: compare a text's span as written with its class's size, and the
// first case goes red; drop the size from the match, and the second.
func TestATextIsMatchedByItsDecodedSize(t *testing.T) {
	t.Parallel()
	near := ohCut(ohCall(1, "echo", `{"t":1}`, strings.Repeat("y", 4<<10)))
	for characters, held := range map[int]bool{4 << 10: true, 4<<10 + 1: false} {
		escaped := strings.Repeat(`\u0079`, characters)
		doc := ohDoc(ohUser, ohStep(strings.Replace(ohDone("echo", `{"t":1}`, "PLACE"), "PLACE", escaped, 1)))
		h, err := ohRead(t, doc, ohBegan, near)
		if got := ohHeld(t, h, err, near); (got != nil) != held {
			t.Errorf("%d characters escaped six bytes each: held %v; want held %t", characters, got, held)
		}
	}
}

// A VALUE PAST ITS CAPTURE IS THERE TO A READING, of its kind, equal to no
// word: an `executed` past the bound passes its part, as one that is not
// false does, and an absent one counts; a message's or an item's `type` string
// past the word's buffer is a shape this build does not read, and an absent
// one no export.
//
// Mutation: read a value past its capture or its word as absent, and a case
// goes red.
func TestAPastValueIsPresentToAHoldingsReading(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "echo", `{"t":1}`, "done")
	part := ohDone("echo", `{"t":1}`, "done")
	long := strings.Repeat("x", exportValueMaxBytes+1)
	for name, tc := range map[string]struct {
		part string
		held bool
	}{
		"executed past the bound": {strings.Replace(part, `"executed":false`, `"executed":"`+long+`"`, 1), false},
		"executed absent":         {strings.Replace(part, `"executed":false,`, ``, 1), true},
	} {
		h, err := ohRead(t, ohDoc(ohUser, ohStep(tc.part)), ohBegan, c)
		if got := ohHeld(t, h, err, c); (got != nil) != tc.held {
			t.Errorf("%s: held %v; want held %t", name, got, tc.held)
		}
	}
	step := ohStep(part)
	for name, tc := range map[string]struct {
		doc  string
		want *holdingsError
	}{
		"a message's type past the buffer": {ohDoc(ohUser, strings.Replace(step, `"type":"assistant"`,
			`"type":"`+long+`"`, 1)), errOpenCodeShape},
		"a message's type absent": {ohDoc(ohUser, strings.Replace(step, `"type":"assistant",`, ``, 1)),
			errOpenCodeMalformed},
		"an item's type past the buffer": {ohDoc(ohUser, ohStep(strings.Replace(part, `"type":"tool"`,
			`"type":"`+long+`"`, 1))), errOpenCodeShape},
		"an item's type absent": {ohDoc(ohUser, ohStep(strings.Replace(part, `"type":"tool",`, ``, 1))),
			errOpenCodeMalformed},
	} {
		_, err := ohRead(t, tc.doc, ohBegan, c)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: %v; want %v", name, err, tc.want)
		}
	}
}

// AN EMPTY FAILURE NEVER HOLDS AN EMPTY OUTPUT: a failed part whose message is
// empty is OpenCode's own words to its model, never the call's, so a failed
// call the log has with an empty output is told; a failure written exactly is
// held.
//
// Mutation: match a text of a class of size zero, and this goes red.
func TestAnEmptyFailureNeverHoldsAnEmptyOutput(t *testing.T) {
	t.Parallel()
	for output, held := range map[string]bool{"": false, "boom": true} {
		c := ohCall(1, "echo", `{"t":1}`, output)
		c.Failed = true
		h, err := ohRead(t, ohDoc(ohUser, ohStep(ohFailed("echo", `{"t":1}`, output))), ohBegan, c)
		if got := ohHeld(t, h, err, c); (got != nil) != held {
			t.Errorf("a failure of %q: held %v; want held %t", output, got, held)
		}
	}
}

// ohAllocated is what reading the export doc opens for calls allocated, in
// bytes, the fewest of three reads after a warm-up ([allocationsOf]) so what
// the process builds once (the tool-name bound's profiles) is none of it, and
// the last read's holdings.
func ohAllocated(t *testing.T, doc func() io.Reader, calls ...sandbox.LoggedCall) (uint64, sandbox.SessionHoldings) {
	t.Helper()
	ask := sandbox.HoldingsAsk{Calls: calls, Resumer: "2.3.2"}
	var (
		h   sandbox.SessionHoldings
		err error
	)
	_, allocated := allocationsOf(t, func() {
		h, err = readOpenCodeHoldings(t.Context(), doc(), ohSession, ohBegan, ask)
	})
	if err != nil || !h.Read {
		t.Fatalf("no reading: %v", err)
	}
	return allocated, h
}

// A STATE IS READ ONCE WHATEVER ITS KEY ORDER: a part whose `content` comes
// before its `status` and `input` counts as the one written status first, and
// its 4 MiB answer is never held: the read allocates no more than a part of
// another server's alike in size, within a capture's bound.
//
// Not parallel: it reads the process's allocation count.
//
// Mutation: hold the answer to digest it, or decide the part where its
// content is read, and a case goes red.
func TestAStateIsReadOnceWhateverItsKeyOrder(t *testing.T) {
	text := strings.Repeat("z", 4<<20)
	c := ohCut(ohCall(1, "echo", `{"t":1}`, text))
	part := ohDone("echo", `{"t":1}`, text)
	content := `"content":[{"type":"text","text":` + ohQuote(text) + `}],`
	reordered := strings.Replace(strings.Replace(part, content, "", 1), `"state":{`, `"state":{`+content, 1)
	if reordered == part {
		t.Fatal("the content did not move")
	}
	doc := ohDoc(ohUser, ohStep(reordered))
	h, err := ohRead(t, doc, ohBegan, c)
	if got := ohHeld(t, h, err, c); !slices.Equal(got, []int{1}) {
		t.Fatalf("content first: held %v; want it counted", got)
	}
	other := strings.Replace(doc, `"crewlet_echo"`, `"notcrew_echo"`, 1)
	read, _ := ohAllocated(t, func() io.Reader { return strings.NewReader(doc) }, c)
	none, _ := ohAllocated(t, func() io.Reader { return strings.NewReader(other) }, c)
	if read > none+exportValueMaxBytes {
		t.Errorf("the answer read allocated %d bytes, a part never counted %d; want it never held", read, none)
	}
}

// AN ERROR PART COUNTS ONLY FOR A FAILURE HANDED UNTRIMMED: OpenCode hands a
// failure's output trimmed, and an empty or blank one in words of its own,
// so a padded, empty or blank failure is told; an exact one is held.
//
// Mutation: compare the message after trimming, and this goes red.
func TestAnErrorPartCountsOnlyForAFailureHandedUntrimmed(t *testing.T) {
	t.Parallel()
	for output, held := range map[string]bool{"x": true, " x\n": false, "": false, " \n": false} {
		c := ohCall(1, "echo", `{"t":1}`, output)
		c.Failed = true
		message := strings.TrimSpace(output)
		if message == "" {
			message = "MCP tool returned an error"
		}
		h, err := ohRead(t, ohDoc(ohUser, ohStep(ohFailed("echo", `{"t":1}`, message))), ohBegan, c)
		if got := ohHeld(t, h, err, c); (got != nil) != held {
			t.Errorf("a failure %q: held %v; want held %t", output, got, held)
		}
	}
}

// A BLANK OUTPUT IS HELD ONLY WHERE A REQUEST WAS MEASURED TO CARRY IT: an
// empty answer reaches the model as "(no tool output)", and a blank one is
// left told though it was carried verbatim, so neither is held. A text is
// blank only where every one of the digest's pieces is ([textDigestBytes]):
// one of words in either piece is held.
//
// Mutation: count an empty or blank completed part, or judge a text's blanks
// by one of its pieces, and this goes red.
func TestABlankOutputIsHeldOnlyWhereARequestWasMeasuredToCarryIt(t *testing.T) {
	t.Parallel()
	blanks := strings.Repeat(" ", textDigestBytes+100)
	for output, held := range map[string]bool{"": false, " \n\t ": false, blanks: false, "words" + blanks: true,
		blanks + "words": true} {
		c := ohCall(1, "echo", `{"t":1}`, output)
		if len(output) > sandbox.MaxBridgeOutputBytes {
			c = ohCut(c)
		}
		h, err := ohRead(t, ohDoc(ohUser, ohStep(ohDone("echo", `{"t":1}`, output))), ohBegan, c)
		if got := ohHeld(t, h, err, c); (got != nil) != held {
			t.Errorf("an output %.12q of %d bytes: held %v; want held %t", output, len(output), got, held)
		}
	}
}

// A SCRUBBED TEXT NEVER STANDS FOR A CUT OUTPUT: a part of a cut call's size
// and kept opening whose text past the opening holds the scrub's marker is
// told, since the scrub may have rewritten what the log did not keep, the
// marker within one of the digest's pieces or across two ([textDigestBytes]);
// so is a part of the kept opening at another size.
//
// Mutation: leave the marker unchecked, read it within each piece alone, or
// leave a cut output's size unchecked, and a case goes red.
func TestAScrubbedTextNeverStandsForACutOutput(t *testing.T) {
	t.Parallel()
	for where, at := range map[string]int{"within a piece": 8000, "across two pieces": textDigestBytes - 5} {
		text := strings.Repeat("w", at) + "[REDACTED:engine-secret]" + strings.Repeat("w", 1000)
		c := ohCut(ohCall(1, "echo", `{"t":1}`, text))
		h, err := ohRead(t, ohDoc(ohUser, ohStep(ohDone("echo", `{"t":1}`, text))), ohBegan, c)
		if got := ohHeld(t, h, err, c); got != nil {
			t.Errorf("a marker %s: held %v; want told", where, got)
		}
		plain := strings.ReplaceAll(text, "[REDACTED:engine-secret]", strings.Repeat("w", 24))
		c = ohCut(ohCall(1, "echo", `{"t":1}`, plain))
		h, err = ohRead(t, ohDoc(ohUser, ohStep(ohDone("echo", `{"t":1}`, plain))), ohBegan, c)
		if got := ohHeld(t, h, err, c); !slices.Equal(got, []int{1}) {
			t.Errorf("the text unscrubbed, its marker %s: held %v; want it", where, got)
		}
		h, err = ohRead(t, ohDoc(ohUser, ohStep(ohDone("echo", `{"t":1}`, plain+"w"))), ohBegan, c)
		if got := ohHeld(t, h, err, c); got != nil {
			t.Errorf("a part of another size, its marker %s: held %v; want told", where, got)
		}
	}
}

// AN UNKEPT OUTPUT IS NEVER HELD: the log keeps a marker in place of an
// output no record keeps, and the session's real answer never matches it.
//
// Mutation: match the marker as the text, and this goes red.
func TestAnUnkeptOutputIsNeverHeld(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "read_secret", `{"t":1}`, "[the output of this call is not kept]")
	h, err := ohRead(t, ohDoc(ohUser, ohStep(ohDone("read_secret", `{"t":1}`, "the real answer"))), ohBegan, c)
	if got := ohHeld(t, h, err, c); got != nil {
		t.Errorf("held %v; want told", got)
	}
}

// A PART'S ARGUMENTS ARE READ BY THE LOG'S OWN RULE: another key order and an
// escaped character key alike and are held; a key a closed schema dropped
// keys apart and is told; an input of the call's own arguments is held at the
// longest the log keeps whole ([sandbox.MaxSubmissionArgsBytes]) and passed a
// byte past it, whatever the walk's capture holds.
//
// Mutation: compare the raw text, decode under other options, or bound the
// input by the capture alone and raise the capture, and a case goes red.
func TestAPartsArgumentsAreReadByTheLogsOwnRule(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "echo", `{"a":"é","b":1.0}`, "done")
	for input, held := range map[string]bool{
		`{"b":1.0,"a":"é"}`:                                   true,
		`{"a":"é","b":1.0,"extra":true}`:                      false,
		`{"a":"é","b":1}`:                                     false,
		`{"a":"` + strings.Repeat("p", 64<<10) + `","b":1.0}`: false,
	} {
		h, err := ohRead(t, ohDoc(ohUser, ohStep(ohDone("echo", input, "done"))), ohBegan, c)
		if got := ohHeld(t, h, err, c); (got != nil) != held {
			t.Errorf("input %.40s: held %v; want held %t", input, got, held)
		}
	}
	bound := sandbox.MaxSubmissionArgsBytes
	for size, held := range map[int]bool{bound: true, bound + 1: false} {
		input := `{"a":"` + strings.Repeat("p", size-len(`{"a":""}`)) + `"}`
		c := ohCall(1, "echo", input, "done")
		h, err := ohRead(t, ohDoc(ohUser, ohStep(ohDone("echo", input, "done"))), ohBegan, c)
		if got := ohHeld(t, h, err, c); (got != nil) != held {
			t.Errorf("the call's own input of %d bytes: held %v; want held %t", size, got, held)
		}
	}
}

// A MESSAGE OF A KIND THIS BUILD DOES NOT READ LEAVES THE SESSION UNREAD: a
// twelfth kind is a release this build was not written for.
//
// Mutation: pass unknown kinds, and this goes red.
func TestAMessageOfAKindThisBuildDoesNotReadLeavesTheSessionUnread(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "echo", `{"t":1}`, "done")
	h, err := ohRead(t, ohDoc(ohUser, `{"type":"later-kind","time":{"created":2}}`,
		ohStep(ohDone("echo", `{"t":1}`, "done"))), ohBegan, c)
	ohNone(t, h, err)
}

// AN OPENCODE READING IS HELD TO ITS BOUNDS: plain past a lowered meter,
// another value after the document, `info` naming another session, and a body
// failing after the document are no reading.
//
// Mutation: drop a bound, the trailing check or the drain, and a case goes
// red.
func TestAnOpenCodeReadingIsHeldToItsBounds(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "echo", `{"t":1}`, "done")
	doc := ohDoc(ohUser, ohStep(ohDone("echo", `{"t":1}`, "done")))
	ask := sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{c}, Resumer: "2.3.2"}
	read := func(plain io.Reader, session string) error {
		h, err := readOpenCodeHoldings(t.Context(), plain, session, ohBegan, ask)
		if err == nil && !h.Read {
			t.Fatal("a reading of nothing with no error")
		}
		return err
	}
	if err := read(strings.NewReader(doc), ohSession); err != nil {
		t.Fatalf("the export within every bound: %v", err)
	}
	for name, err := range map[string]error{
		"plain past its meter":         read(boundedPlain(strings.NewReader(doc), int64(len(doc)-1)), ohSession),
		"a value after the document":   read(strings.NewReader(doc+` {}`), ohSession),
		"another session's":            read(strings.NewReader(doc), "ses_other"),
		"a body failing after the end": read(io.MultiReader(strings.NewReader(doc), ohFailing{}), ohSession),
	} {
		if err == nil {
			t.Errorf("%s: read; want no reading", name)
		}
	}
	archive := ohGzip(t, doc)
	_, err := (OpenCode{}).readHoldings(t.Context(), io.MultiReader(bytes.NewReader(archive), ohFailing{}),
		ohArchive("2.3.2"), ask, plainBounds(8<<20))
	if err == nil {
		t.Error("a store's stream failing past the archive: read; want no reading")
	}
}

// ohFailing is a stream that fails, as a store's does past a fault.
type ohFailing struct{}

func (ohFailing) Read([]byte) (int, error) { return 0, errors.New("the store's stream failed") }

// AN OPENCODE READING STOPS BETWEEN MESSAGES, AND BETWEEN PARTS, WHEN ITS
// CONTEXT ENDS: a caller gone after a step's first part, or after the first
// message, is no reading.
//
// Mutation: ask the context per message alone, and the first case goes
// red.
func TestAnOpenCodeReadingStopsBetweenMessagesWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "echo", `{"t":1}`, "done")
	part := ohDone("echo", `{"t":1}`, "done")
	for name, tc := range map[string]struct {
		doc  string
		asks int
	}{
		// The walk asks before each message and each item: the third ask is
		// the second part's, the second the second message's.
		"after a part":    {ohDoc(ohStep(part, part)), 3},
		"after a message": {ohDoc(ohUser, ohStep(part)), 2},
	} {
		ctx := &ohCountdown{Context: t.Context(), left: tc.asks - 1}
		_, err := readOpenCodeHoldings(ctx, strings.NewReader(tc.doc), ohSession, ohBegan,
			sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{c}, Resumer: "2.3.2"})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s: %v; want the caller's end", name, err)
		}
	}
}

// ohCountdown is a context its caller leaves once it has been asked left
// times whether it has.
type ohCountdown struct {
	context.Context
	left int
}

func (c *ohCountdown) Err() error {
	if c.left <= 0 {
		return context.Canceled
	}
	c.left--
	return nil
}

// THE READER TAKES NO SLOT OF ITS OWN: a reading under ReadHoldings, which
// holds the readings' one slot, completes; a reader taking the slot again
// would wait on itself until its caller left, which this case's liveness
// bound reports as its own failure.
//
// Not parallel: the slots are the process's own.
//
// Mutation: take the slots in the reader, and this goes red.
func TestTheReaderTakesNoSlotOfItsOwn(t *testing.T) {
	c := ohCall(1, "echo", `{"t":1}`, "done")
	archive := ohGzip(t, ohDoc(ohUser, ohStep(ohDone("echo", `{"t":1}`, "done"))))
	type reading struct {
		got sandbox.SessionHoldings
		err error
	}
	done := make(chan reading, 1)
	go func() {
		got, err := NewOpenCode().ReadHoldings(t.Context(), ohArchive("2.3.2"), func(context.Context) (io.ReadCloser,
			error) {
			return io.NopCloser(bytes.NewReader(archive)), nil
		}, sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{c}, Resumer: "2.3.2"})
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

// A READING'S CAUSE QUOTES NOTHING OF THE EXPORT: refused inside a 4 MiB part,
// its error holds none of the part's words; refused on a fault whose words
// run a mebibyte, its error is within the detail's bound whatever the fault
// says.
//
// Mutation: carry the cause whole in the reading's words, and this goes red.
func TestAReadingsCauseQuotesNothingOfTheExport(t *testing.T) {
	t.Parallel()
	c := ohCall(1, "echo", `{"t":1}`, "done")
	ask := sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{c}, Resumer: "2.3.2"}
	secret := strings.Repeat("the secret words ", 1<<18)
	for name, plain := range map[string]io.Reader{
		"inside a part": strings.NewReader(ohDoc(ohUser, ohStep(`{"type":"text","text":"`+secret+"\x01\"}"))),
		"on a long fault": io.MultiReader(strings.NewReader(`{"info":`),
			ohFault{errors.New(secret)}),
	} {
		_, err := readOpenCodeHoldings(t.Context(), plain, ohSession, ohBegan, ask)
		if err == nil {
			t.Fatalf("%s: read", name)
		}
		if msg := err.Error(); len(msg) > transcriptDetailLimit+200 || strings.Contains(msg, "secret words") &&
			name == "inside a part" {
			t.Errorf("%s: the cause is %d bytes, or quotes the export", name, len(msg))
		}
	}
}

// ohFault is a stream that fails with err.
type ohFault struct{ err error }

func (f ohFault) Read([]byte) (int, error) { return 0, f.err }

// A READING HOLDS NO MESSAGE: a 64 MiB message of one text part, past the 32
// MiB the decode walk held a message to, is read and its call held, and a 64
// MiB message of minimal items is read and its one part counted, each
// allocating within [ohReadingAllocates] whatever the message's length.
//
// Not parallel: it reads the process's allocation count.
//
// Mutation: hold a digested text whole, or capture an item's text, and a case
// goes red.
func TestAReadingHoldsNoMessage(t *testing.T) {
	const message = 64 << 20
	text := strings.Repeat("v", message-512)
	cut := ohCut(ohCall(1, "echo", `{"t":1}`, text))
	head, tail, _ := strings.Cut(ohDoc(ohUser, ohStep(ohDone("echo", `{"t":1}`, "PLACE"))), "PLACE")
	const item = `{"type":"reasoning","text":"r"},`
	itemsHead, itemsTail, _ := strings.Cut(ohDoc(ohUser, ohStep("PLACE")), "PLACE")
	itemsTail = ohDone("echo", `{"t":1}`, "done") + itemsTail
	for name, tc := range map[string]struct {
		doc  func() io.Reader
		call sandbox.LoggedCall
	}{
		"one text part": {func() io.Reader {
			return io.MultiReader(strings.NewReader(head), strings.NewReader(text), strings.NewReader(tail))
		}, cut},
		"minimal items": {func() io.Reader {
			items := &repeating{lines: []byte(item), left: int64(message / len(item) * len(item))}
			return io.MultiReader(strings.NewReader(itemsHead), items, strings.NewReader(itemsTail))
		}, ohCall(1, "echo", `{"t":1}`, "done")},
	} {
		allocated, h := ohAllocated(t, tc.doc, tc.call)
		if got := ohHeld(t, h, nil, tc.call); !slices.Equal(got, []int{1}) {
			t.Errorf("%s: held %v; want its call", name, got)
		}
		t.Logf("%s: allocated %d bytes", name, allocated)
		if allocated > ohReadingAllocates {
			t.Errorf("%s: allocated %d bytes; want at most %d", name, allocated, ohReadingAllocates)
		}
	}
}

// ohReadingAllocates is the most a reading of [TestAReadingHoldsNoMessage]'s
// messages allocates: the scanner's buffers, a capture's, the digest's 32 KiB
// piece, the reading's maps and its verdict, 124 KiB of the text part and 107
// KiB of the items measured under the race detector on the build host (Go
// 1.27.2), twice that; a 64 MiB message held once is 256 times it.
const ohReadingAllocates = 256 << 10

// ohManifest is what a release's fixtures say of each case: the session, the
// run's start, the round's logged calls, and the calls the reading holds.
type ohManifest map[string]struct {
	Session string `json:"session"`
	Began   bool   `json:"began"`
	Held    []int  `json:"held"`
	Calls   []struct {
		Appended, Seq              int
		Name, Args, Digest, Output string
		Failed, Held, Abandoned    bool
		Server                     string
	} `json:"calls"`
}

// ohFixture is one case of a release's own runs: its kept archive, the
// session and the run's start, the round's logged calls as the log keeps
// them, and the calls the rules hold.
type ohFixture struct {
	archive []byte
	session string
	began   bool
	calls   []sandbox.LoggedCall
	held    []int
}

// ohFixtures are release's cases (testdata/opencode/<release>/holdings), each
// call's output cut as the log cuts it, every call but an abandoned one asked.
func ohFixtures(tb testing.TB, release string) map[string]ohFixture {
	tb.Helper()
	dir := filepath.Join("testdata", "opencode", release, "holdings")
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json.gz"))
	if err != nil {
		tb.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		tb.Fatal(err)
	}
	var manifest ohManifest
	if err = json.NewDecoder(zr).Decode(&manifest); err != nil {
		tb.Fatal(err)
	}
	out := map[string]ohFixture{}
	for name, m := range manifest {
		archive, err := os.ReadFile(filepath.Join(dir, name+".json.gz"))
		if err != nil {
			tb.Fatal(err)
		}
		f := ohFixture{archive: archive, session: m.Session, began: m.Began, held: m.Held}
		for _, logged := range m.Calls {
			c := ohCut(ohCall(logged.Appended, logged.Name, cmp.Or(logged.Args, "{}"), logged.Output))
			c.Seq, c.Failed, c.Digest, c.Asked = logged.Seq, logged.Failed, logged.Digest, !logged.Abandoned
			if logged.Args == "" && logged.Digest == "" {
				c.Args.Whole = false
			}
			f.calls = append(f.calls, c)
		}
		out[name] = f
	}
	return out
}

// EACH RELEASE'S BRIDGED CALLS READ AS ITS OWN EXPORT HOLDS THEM: for each
// release of the set, the captures its own runs kept of the engine's bridged
// calls (outcomes on both of OpenCode's routes, arguments, parallel and
// identical calls, truncation, an end and a kill mid-call, Code Mode, a child
// session, a compaction), each kept as the take keeps it, are read through
// ReadHoldings, and exactly the calls their rules hold are held. The log's
// cut of each output is applied as the log applies it.
//
// Mutation: none of its own; the regression pin of the rules over real
// exports.
func TestEachReleasesBridgedCallsReadAsItsOwnExportHoldsThem(t *testing.T) {
	t.Parallel()
	if len(openCodeHoldingsReleases) == 0 {
		t.Fatal("the reading reads no release")
	}
	for _, release := range openCodeHoldingsReleases {
		for name, f := range ohFixtures(t, release) {
			got, err := NewOpenCode().ReadHoldings(t.Context(), sandbox.SessionArchive{Harness: OpenCodeName,
				SessionID: f.session, Format: openCodeArchiveFormat, Version: release,
				RunStart: sandbox.RunStart{Began: f.began}},
				func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(f.archive)), nil },
				sandbox.HoldingsAsk{Calls: f.calls, Resumer: release})
			var held []int
			if err == nil {
				held = ohHeld(t, got, err, f.calls...)
			}
			if err != nil || !slices.Equal(held, f.held) {
				t.Errorf("%s %s: held %v, %v; want %v", release, name, held, err, f.held)
			}
		}
	}
}
