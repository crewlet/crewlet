package executor_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// These cases read a Claude Code session's archive through
// [executor.Runner.ReadHoldings], as a relaunch asks: what a runner checks
// before it opens anything, and the slots it reads in. What a reading makes of
// the records is claudeholdings_internal_test.go's.

// rhSessionID is the session every archive here holds.
const rhSessionID = "5e55100a-0000-4000-8000-0000000000aa"

// rhSession is the session's archive as its row describes it, written by
// release version.
func rhSession(version string) sandbox.SessionArchive {
	return sandbox.SessionArchive{Harness: executor.ClaudeCodeName, SessionID: rhSessionID,
		Format: executor.ClaudeArchiveFormat, Version: version}
}

// rhBridged is a tool of the bridge's as Claude Code names an MCP server's
// tool, `mcp__<server>__<tool>`, under the server the engine serves a box's
// tools as.
const rhBridged = "mcp__" + config.ToolServerName + "__slow"

// rhArchive is the session's archive, tarred and gzipped as the take's scrub
// writes one: a brief, then a call to id of [rhBridged] answered with output.
// Each record carries its time and the reply the call's id, as Claude Code
// writes them: the resume walks from a leaf by time and pairs a call by id.
func rhArchive(t *testing.T, id, output string) []byte {
	t.Helper()
	return rhArchiveOf(t, id, rhBridged, output)
}

// rhArchiveOf is [rhArchive]'s archive, its call one of tool.
func rhArchiveOf(t *testing.T, id, tool, output string) []byte {
	t.Helper()
	uuid := func(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }
	thread := `{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":` +
		`"the brief"},"uuid":"` + uuid(1) + `","timestamp":"2026-01-02T03:04:05.000Z","sessionId":"` + rhSessionID +
		`","version":"2.1.310"}` + "\n" +
		`{"parentUuid":"` + uuid(1) + `","isSidechain":false,"message":{"id":"msg_rh","role":"assistant","content":` +
		`[{"type":"tool_use","id":"` + id + `","name":"` + tool + `","input":{}}],` +
		`"stop_reason":"tool_use"},"type":"assistant","uuid":"` + uuid(2) + `","timestamp":"2026-01-02T03:04:06.000Z",` +
		`"sessionId":"` + rhSessionID +
		`","version":"2.1.310"}` + "\n" +
		`{"parentUuid":"` + uuid(2) + `","isSidechain":false,"type":"user","message":{"role":"user","content":` +
		`[{"tool_use_id":"` + id + `","type":"tool_result","content":[{"type":"text","text":"` + output + `"}]}]},` +
		`"uuid":"` + uuid(3) + `","timestamp":"2026-01-02T03:04:07.000Z","sessionId":"` + rhSessionID +
		`","version":"2.1.310"}` + "\n"
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: "projects/crewlet/" + rhSessionID + ".jsonl", Mode: 0o600,
		Size: int64(len(thread)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(thread)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// rhResumer continues every session here: a release the model of Claude
// Code's resume was read from.
const rhResumer = "2.1.322"

// rhAsk asks about calls, for a session [rhResumer] continues.
func rhAsk(calls ...sandbox.LoggedCall) sandbox.HoldingsAsk {
	return sandbox.HoldingsAsk{Calls: calls, Resumer: rhResumer}
}

// rhRead starts runner's reading of archive under ctx, for call; opened closes
// as it opens the archive, done once it has ended, its error in *err.
func rhRead(ctx context.Context, runner *executor.Runner, archive []byte,
	call sandbox.LoggedCall) (opened, done chan struct{}, err *error) {

	opened, done, err = make(chan struct{}), make(chan struct{}), new(error)
	go func() {
		defer close(done)
		_, *err = runner.ReadHoldings(ctx, rhSession("2.1.310"), func(context.Context) (io.ReadCloser, error) {
			close(opened)
			return io.NopCloser(bytes.NewReader(archive)), nil
		}, rhAsk(call))
	}()
	return opened, done, err
}

// rhBody is an archive's stream that calls closed as it is closed.
type rhBody struct {
	io.Reader
	closed func()
}

func (b *rhBody) Close() error {
	b.closed()
	return nil
}

// A READING TAKES A SLOT OF ITS OWN, OUTSIDE THE TRANSCRIPT READS': while a
// seat's read holds the seats' slot and persons' reads hold both of the
// process's, a reading reads, and as it closes its archive it holds the
// readings' one slot and none of theirs; while another reading holds that
// slot, a reading waits, opening nothing, and leaves with its caller's error.
//
// Not parallel: the slots are the process's own (transcript_test.go).
//
// Mutation: take the seats' slot as a seat's read does, and the first reading
// waits on the seat's read past this case's bound; take no slot, and the
// second reading opens its archive beside the first.
func TestAReadingTakesASlotOfItsOwnOutsideTheTranscriptReads(t *testing.T) {
	held := func() [3]int {
		return [3]int{executor.HoldingsReadsHeld(), executor.SeatTranscriptReadsHeld(), executor.TranscriptReadsHeld()}
	}
	if held() != [3]int{} {
		t.Fatal("slots are held before the case reads anything")
	}
	runner := executor.NewClaudeCode()
	call := sandbox.LoggedCall{ID: "toolu_slot", Asked: true, Output: "slot done"}
	archive := rhArchive(t, call.ID, call.Output)
	raw := gzipped(t, briefs(1))

	gate := make(chan struct{})
	letSeat := sync.OnceFunc(func() { close(gate) })
	var seatErr error
	seatOpened, seatDone := seatReadOf(t.Context(), raw, gate, &seatErr)
	t.Cleanup(func() {
		letSeat()
		<-seatDone
	})
	awaited(t, seatOpened, "a seat's read given free slots opens its archive")
	person := holdSlot(t, raw)
	var atClose [3]int
	body := &rhBody{Reader: bytes.NewReader(archive), closed: func() { atClose = held() }}
	readDone := make(chan struct{})
	var readErr error
	go func() {
		defer close(readDone)
		var got sandbox.SessionHoldings
		got, readErr = runner.ReadHoldings(t.Context(), rhSession("2.1.310"),
			func(context.Context) (io.ReadCloser, error) { return body, nil }, rhAsk(call))
		if readErr == nil && !got.Read {
			readErr = errors.New("no reading and no error")
		}
	}()
	awaited(t, readDone, "a reading beside a seat's read and two persons' reads")
	if readErr != nil {
		t.Fatalf("a reading beside the transcript reads: %v", readErr)
	}
	if atClose != [3]int{1, 1, 2} {
		t.Errorf("as its archive closed, the readings', the seats' and the process's slots held %v; want the "+
			"reading's own one beside the seat's read's and both of the process's", atClose)
	}
	letSeat()
	<-seatDone
	person.let()
	<-person.done
	if seatErr != nil || person.err != nil {
		t.Fatalf("the transcript reads: %v, %v", seatErr, person.err)
	}

	firstOpened, firstGate := make(chan struct{}), make(chan struct{})
	letFirst := sync.OnceFunc(func() { close(firstGate) })
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _ = runner.ReadHoldings(t.Context(), rhSession("2.1.310"), func(context.Context) (io.ReadCloser, error) {
			close(firstOpened)
			<-firstGate
			return io.NopCloser(bytes.NewReader(archive)), nil
		}, rhAsk(call))
	}()
	t.Cleanup(func() {
		letFirst()
		<-firstDone
	})
	awaited(t, firstOpened, "a reading given a free slot opens its archive")
	ctx, leave := context.WithCancel(t.Context())
	defer leave()
	waiting := &countedWait{Context: ctx, nth: 1, asked: make(chan struct{})}
	opened, done, err := rhRead(waiting, runner, archive, call)
	awaited(t, waiting.asked, "a reading waits for the readings' slot another holds")
	leave()
	awaited(t, done, "a reading waiting for the readings' slot ends once its caller leaves")
	select {
	case <-opened:
		t.Error("a reading opened its archive while another held the readings' slot")
	default:
	}
	if !errors.Is(*err, context.Canceled) {
		t.Errorf("the waiting reading ended %v, want its caller's own error", *err)
	}
	letFirst()
	<-firstDone
	if held() != [3]int{} {
		t.Errorf("once every read ended, the readings', the seats' and the process's slots held %v, want none",
			held())
	}
}

// A READING WHOSE CALLER LEFT AS ITS ARCHIVE OPENED ENDS WITH ITS CALLER'S
// ERROR, whatever the store made of the open stopping under it: a relaunch
// that gave up is told so, never the store's account of a cut connection.
//
// Mutation: answer an open that failed by the store's error alone, and the
// reading ends with the store's.
func TestAReadingWhoseCallerLeftAsItOpenedEndsWithItsCallersError(t *testing.T) {
	t.Parallel()
	call := sandbox.LoggedCall{ID: "toolu_left", Asked: true, Output: "left done"}
	ctx, leave := context.WithCancel(t.Context())
	defer leave()
	cut := errors.New("the store's connection was cut")
	got, err := executor.NewClaudeCode().ReadHoldings(ctx, rhSession("2.1.310"),
		func(context.Context) (io.ReadCloser, error) {
			leave()
			return nil, cut
		}, rhAsk(call))
	if got.Read || !errors.Is(err, context.Canceled) || errors.Is(err, cut) {
		t.Errorf("a reading whose caller left as it opened = %+v, %v; want no reading, with its caller's own error",
			got, err)
	}
}

// A READING IS ONLY OF A WRITER ITS RULES WERE MEASURED ON: Claude Code 2.1.310
// and later. No reading, and no archive opened, for a writer below it (even
// 2.1.300, which the carry accepts), none recorded, one that is no release, or
// one whose number overflows an int though its digits read as at or past the
// floor (a person's read and the carry take such a release for a current one);
// nor by OpenCode's runner asked for this ask's resumer, outside its set.
//
// Mutation: hold the writer to the carry's floor, or read an unreadable
// release as a current one, and a refused writer's session is read.
func TestAReadingIsOnlyOfAWriterItsRulesWereMeasuredOn(t *testing.T) {
	t.Parallel()
	call := sandbox.LoggedCall{ID: "toolu_writer", Asked: true, Output: "writer done"}
	archive := rhArchive(t, call.ID, call.Output)
	for name, tc := range map[string]struct {
		runner  *executor.Runner
		archive sandbox.SessionArchive
		read    bool
	}{
		"a writer at the floor":         {executor.NewClaudeCode(), rhSession("2.1.310"), true},
		"a later writer":                {executor.NewClaudeCode(), rhSession("2.1.322"), true},
		"a writer below the floor":      {executor.NewClaudeCode(), rhSession("2.1.309"), false},
		"a writer a box can continue":   {executor.NewClaudeCode(), rhSession("2.1.300"), false},
		"a writer the run recorded not": {executor.NewClaudeCode(), rhSession(""), false},
		"a writer that is no release":   {executor.NewClaudeCode(), rhSession("2.1"), false},
		"a writer whose number no int holds": {executor.NewClaudeCode(), rhSession("0.0.99999999999999999999"),
			false},
		"a later writer whose number no int holds": {executor.NewClaudeCode(),
			rhSession("2.1.99999999999999999999"), false},
		"OpenCode's runner, asked for a resumer outside its set": {executor.NewOpenCode(), sandbox.SessionArchive{Harness: executor.OpenCodeName,
			SessionID: "ses_s", Format: executor.OpenCodeArchiveFormat, Version: "2.2.5"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			opened := false
			got, err := tc.runner.ReadHoldings(t.Context(), tc.archive, func(context.Context) (io.ReadCloser, error) {
				opened = true
				return io.NopCloser(bytes.NewReader(archive)), nil
			}, rhAsk(call))
			if tc.read {
				if err != nil || !got.Read {
					t.Fatalf("a reading = %+v, %v; want it read", got, err)
				}
				return
			}
			if got.Read || err == nil || opened {
				t.Errorf("a reading = %+v, %v, the archive opened %t; want no reading, nothing opened",
					got, err, opened)
			}
		})
	}
}

// A READING IS ONLY FOR A RESUMER WHOSE RESUME THE LINE LAYER MODELS: a call is
// held only where the continuing release's resume hands its model the call's
// records, and the layer read that code for Claude Code 2.1.310 and 2.1.322
// alone. Each is read; every other resumer is no reading and opens nothing:
// none read (the box's `--version` answered nothing), a release between,
// before or after them (which may load, walk or drop records otherwise), and
// one unparseable or matching a modelled release only once parsed.
//
// Mutation: drop the check, and the unread resumer's session is read; hold the
// resumer to 2.1.310 through 2.1.322, and 2.1.311 is read; to 2.1.310 and
// later, and 2.1.323 is read; compare its numbers rather than the release
// whole, and 02.1.310 is read.
func TestAReadingIsOnlyForAResumerWhoseResumeTheModelWasReadFrom(t *testing.T) {
	t.Parallel()
	call := sandbox.LoggedCall{ID: "toolu_resumer", Asked: true, Output: "resumer done"}
	archive := rhArchive(t, call.ID, call.Output)
	for name, tc := range map[string]struct {
		resumer string
		read    bool
	}{
		"2.1.310, a modelled release":          {"2.1.310", true},
		"2.1.322, a modelled release":          {"2.1.322", true},
		"a resumer unread":                     {"", false},
		"a release just after the first":       {"2.1.311", false},
		"a release just before the second":     {"2.1.321", false},
		"a release before them":                {"2.1.309", false},
		"a release after them":                 {"2.1.323", false},
		"a release of two numbers":             {"2.1", false},
		"a release of no number":               {"2.1.x", false},
		"a release whose number no int holds":  {"2.1.99999999999999999999", false},
		"a modelled release spelled otherwise": {"02.1.310", false},
		"a release as the box printed it":      {"2.1.310 (Claude Code)", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ask := rhAsk(call)
			ask.Resumer = tc.resumer
			opened := false
			got, err := executor.NewClaudeCode().ReadHoldings(t.Context(), rhSession("2.1.310"),
				func(context.Context) (io.ReadCloser, error) {
					opened = true
					return io.NopCloser(bytes.NewReader(archive)), nil
				}, ask)
			if tc.read {
				if held, _ := got.Holds(call); err != nil || !held {
					t.Fatalf("a reading for a session %q continues = %+v, %v; want its asked call held",
						tc.resumer, got, err)
				}
				return
			}
			if got.Read || err == nil || opened {
				t.Fatalf("a reading for a session %q continues = %+v, %v, the archive opened %t; want no "+
					"reading, nothing opened", tc.resumer, got, err, opened)
			}
			if !strings.Contains(err.Error(), "claudeResumeRead") {
				t.Errorf("no reading for a session %q continues says %q; want it to point at what a release "+
					"passes to be read for", tc.resumer, err)
			}
		})
	}
}

// A READING ANSWERS ONLY AN ASK WHOSE ASKED IDS NAME ONE CALL EACH: the session
// files every call's records under its id, so an asked id the log keeps none
// of, two asked calls of one id, and an unasked call carrying an asked id each
// leave no record whose call is known. An ask about nothing, with no call or
// more calls than a round's log lists, and a session id naming no thread are no
// reading either; none opens the archive. An asked call beside unasked ones,
// one with no id, is read.
//
// Mutation: drop the check of the unasked calls' ids, and an ask in which
// another call carries the asked id is read.
func TestAReadingAnswersOnlyAnAskWhoseAskedIDsNameOneCallEach(t *testing.T) {
	t.Parallel()
	asked := sandbox.LoggedCall{ID: "toolu_ask", Asked: true, Output: "ask done"}
	archive := rhArchive(t, asked.ID, asked.Output)
	tooMany := []sandbox.LoggedCall{asked}
	for i := range sandbox.MaxBridgeCalls {
		tooMany = append(tooMany, sandbox.LoggedCall{ID: fmt.Sprintf("toolu_n%d", i)})
	}
	for name, tc := range map[string]struct {
		session string
		calls   []sandbox.LoggedCall
		read    bool
	}{
		"an asked call": {rhSessionID, []sandbox.LoggedCall{asked}, true},
		"an asked call beside unasked ones": {rhSessionID, []sandbox.LoggedCall{{ID: "toolu_other"}, asked,
			{ID: ""}}, true},
		"no call asked":                        {rhSessionID, []sandbox.LoggedCall{{ID: asked.ID}}, false},
		"no call":                              {rhSessionID, nil, false},
		"more calls than a round's log lists":  {rhSessionID, tooMany, false},
		"an asked id the log keeps none of":    {rhSessionID, []sandbox.LoggedCall{{ID: "toolu ask", Asked: true}}, false},
		"an asked id past its bound":           {rhSessionID, []sandbox.LoggedCall{{ID: strings.Repeat("t", 65), Asked: true}}, false},
		"no asked id":                          {rhSessionID, []sandbox.LoggedCall{{Asked: true}}, false},
		"two asked calls of one id":            {rhSessionID, []sandbox.LoggedCall{asked, asked}, false},
		"an unasked call carrying an asked id": {rhSessionID, []sandbox.LoggedCall{asked, {ID: asked.ID}}, false},
		"a session whose id names no thread":   {"../" + rhSessionID, []sandbox.LoggedCall{asked}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			session := rhSession("2.1.310")
			session.SessionID = tc.session
			opened := false
			got, err := executor.NewClaudeCode().ReadHoldings(t.Context(), session,
				func(context.Context) (io.ReadCloser, error) {
					opened = true
					return io.NopCloser(bytes.NewReader(archive)), nil
				}, rhAsk(tc.calls...))
			if tc.read {
				if held, _ := got.Holds(asked); err != nil || !held {
					t.Fatalf("a reading = %+v, %v; want its asked call held", got, err)
				}
				return
			}
			if got.Read || err == nil || opened {
				t.Errorf("a reading = %+v, %v, the archive opened %t; want no reading, nothing opened",
					got, err, opened)
			}
		})
	}
}

// A CALL IS HELD ONLY WHERE ITS USE NAMES A TOOL OF THE BRIDGE'S, under
// config.ToolServerName as Claude Code names a server's tool: every call the
// log keeps is the bridge's, and Claude Code's request clears or withholds
// other tools' results by name, its own on a context hint and its memory
// server's (`mcp__memory__memory_*`). Told, not held: its own Bash, a memory
// server tool, and a tool of a server whose name only begins with the bridge's.
//
// Mutation: hold a call whatever its use names, and each told call is held;
// or spell the bridge's server otherwise than config does, and the bridge's
// call is told.
func TestACallIsHeldOnlyWhereItsUseNamesAToolOfTheBridges(t *testing.T) {
	t.Parallel()
	call := sandbox.LoggedCall{ID: "toolu_named", Asked: true, Output: "named done"}
	for name, tc := range map[string]struct {
		tool string
		held bool
	}{
		"a tool of the bridge's":                     {rhBridged, true},
		"Claude Code's own tool":                     {"Bash", false},
		"a tool of Claude Code's memory server":      {"mcp__memory__memory_view", false},
		"a tool of a server named past the bridge's": {"mcp__" + config.ToolServerName + "x__slow", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			archive := rhArchiveOf(t, call.ID, tc.tool, call.Output)
			got, err := executor.NewClaudeCode().ReadHoldings(t.Context(), rhSession("2.1.310"),
				func(context.Context) (io.ReadCloser, error) {
					return io.NopCloser(bytes.NewReader(archive)), nil
				}, rhAsk(call))
			if err != nil || !got.Read {
				t.Fatalf("a reading = %+v, %v; want a reading", got, err)
			}
			if held, _ := got.Holds(call); held != tc.held {
				t.Errorf("Holds = %t, its records %+v; want %t", held, got.Calls[call.ID], tc.held)
			}
		})
	}
}
