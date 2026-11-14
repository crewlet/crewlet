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

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// The cases here read a Claude Code session's kept archive through its runner
// ([executor.Runner.ReadHoldings]), as the sandbox asks it for a relaunch: what
// a runner asks before it opens anything, and the slots it reads in. What a
// reading makes of the archive's records is claudeholdings_internal_test.go's.

// rhSessionID is the session every archive here holds.
const rhSessionID = "5e55100a-0000-4000-8000-0000000000aa"

// rhSession is the session's archive as its row describes it, its writer the
// release version.
func rhSession(version string) sandbox.SessionArchive {
	return sandbox.SessionArchive{Harness: executor.ClaudeCodeName, SessionID: rhSessionID,
		Format: executor.ClaudeArchiveFormat, Version: version}
}

// rhArchive is the session's archive: a main thread whose brief is followed
// by a call to id answered with output, tarred and gzipped as the take's
// scrub writes one.
func rhArchive(t *testing.T, id, output string) []byte {
	t.Helper()
	uuid := func(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }
	thread := `{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":` +
		`"the brief"},"uuid":"` + uuid(1) + `","sessionId":"` + rhSessionID + `","version":"2.1.310"}` + "\n" +
		`{"parentUuid":"` + uuid(1) + `","isSidechain":false,"message":{"role":"assistant","content":` +
		`[{"type":"tool_use","id":"` + id + `","name":"mcp__crewlet__slow","input":{}}],` +
		`"stop_reason":"tool_use"},"type":"assistant","uuid":"` + uuid(2) + `","sessionId":"` + rhSessionID +
		`","version":"2.1.310"}` + "\n" +
		`{"parentUuid":"` + uuid(2) + `","isSidechain":false,"type":"user","message":{"role":"user","content":` +
		`[{"tool_use_id":"` + id + `","type":"tool_result","content":[{"type":"text","text":"` + output + `"}]}]},` +
		`"uuid":"` + uuid(3) + `","sessionId":"` + rhSessionID + `","version":"2.1.310"}` + "\n"
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

// rhAsk asks about calls.
func rhAsk(calls ...sandbox.LoggedCall) sandbox.HoldingsAsk { return sandbox.HoldingsAsk{Calls: calls} }

// rhRead starts a reading of archive, the session's, by runner under ctx, for
// call; opened closes as it opens the archive, and done once it has ended,
// with what it ended with in *err.
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

// A READING TAKES THE SEATS' SLOT, INSIDE THE PROCESS'S TWO, as a seat's read
// does, and never a pool of its own. With a seat's read holding the seats'
// slot, a reading waits for it and, its caller gone, ends with its caller's
// error, having opened nothing; with both of the process's slots held by
// persons' reads, a reading that holds the seats' slot waits for one of them,
// and gives the seats' slot back as its caller leaves, having opened nothing;
// and given one of the process's slots and the seats', it reads, and its
// archive is closed while it still holds both.
//
// Not parallel: the slots are the process's own (transcript_test.go).
//
// Mutation: take the process's slots alone, and the first reading opens its
// archive beside the seat's read; or give the slots back before the archive is
// closed, and its Close sees them free.
func TestAReadingTakesTheSeatsSlotInsideTheProcesssTwo(t *testing.T) {
	if executor.TranscriptReadsHeld() != 0 || executor.SeatTranscriptReadsHeld() != 0 {
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
	ctx, leave := context.WithCancel(t.Context())
	defer leave()
	waiting := &countedWait{Context: ctx, nth: 1, asked: make(chan struct{})}
	opened, done, err := rhRead(waiting, runner, archive, call)
	awaited(t, waiting.asked, "a reading waits for the seats' slot")
	leave()
	awaited(t, done, "a reading waiting for the seats' slot ends once its caller leaves")
	select {
	case <-opened:
		t.Error("a reading opened its archive while a seat's read held the seats' slot")
	default:
	}
	if !errors.Is(*err, context.Canceled) {
		t.Errorf("the reading ended %v, want its caller's own error", *err)
	}
	letSeat()
	awaited(t, seatDone, "the seat's read ends once its archive opens")
	if seatErr != nil {
		t.Fatalf("the seat's read: %v", seatErr)
	}

	persons := make([]func(), 0, executor.TranscriptReadsAtOnce)
	for range executor.TranscriptReadsAtOnce {
		h := holdSlot(t, raw)
		persons = append(persons, func() {
			h.let()
			<-h.done
		})
	}
	ctx, leave = context.WithCancel(t.Context())
	defer leave()
	waiting = &countedWait{Context: ctx, nth: 2, asked: make(chan struct{})}
	opened, done, err = rhRead(waiting, runner, archive, call)
	awaited(t, waiting.asked, "a reading holding the seats' slot waits for one of the process's")
	if executor.SeatTranscriptReadsHeld() != 1 {
		t.Errorf("a reading waiting for one of the process's slots holds %d of the seats'",
			executor.SeatTranscriptReadsHeld())
	}
	leave()
	awaited(t, done, "a reading waiting for one of the process's slots ends once its caller leaves")
	select {
	case <-opened:
		t.Error("a reading opened its archive while persons' reads held both of the process's slots")
	default:
	}
	if !errors.Is(*err, context.Canceled) || executor.SeatTranscriptReadsHeld() != 0 {
		t.Errorf("a reading that left while it waited ended %v holding %d of the seats' slots, want its "+
			"caller's own error and none", *err, executor.SeatTranscriptReadsHeld())
	}
	for _, let := range persons {
		let()
	}

	person := holdSlot(t, raw)
	var atClose [2]int
	body := &rhBody{Reader: bytes.NewReader(archive), closed: func() {
		atClose = [2]int{executor.SeatTranscriptReadsHeld(), executor.TranscriptReadsHeld()}
	}}
	got, readErr := runner.ReadHoldings(t.Context(), rhSession("2.1.310"),
		func(context.Context) (io.ReadCloser, error) { return body, nil }, rhAsk(call))
	if readErr != nil || !got.Read {
		t.Fatalf("a reading given its slots = %+v, %v; want it read", got, readErr)
	}
	if atClose != [2]int{1, 2} {
		t.Errorf("as its archive was closed the reading held %d of the seats' slots and the process held %d, "+
			"want one of the seats' and both of the process's", atClose[0], atClose[1])
	}
	person.let()
	<-person.done
	if executor.SeatTranscriptReadsHeld() != 0 || executor.TranscriptReadsHeld() != 0 {
		t.Errorf("once every read ended, %d of the seats' slots and %d of the process's are held, want none",
			executor.SeatTranscriptReadsHeld(), executor.TranscriptReadsHeld())
	}
}

// A READING WHOSE CALLER LEFT AS ITS ARCHIVE OPENED ENDS WITH ITS CALLER'S
// ERROR, whatever the store made of the open stopping under it: a relaunch
// that gave up on its reading is told it gave up, never the store's account
// of a connection that was cut.
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

// and later. A writer below it, a release the run recorded none of, one that
// is no release, and one of the shape of a release whose number is past what
// an int holds, at or past the floor as its digits read, are each no reading,
// though a person's read and the carry take an unreadable release for a
// current one and a carried session's writer may be as old as 2.1.300; and a
// runner whose harness has no reading, OpenCode's, reads none. None of them
// opens the archive.
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
		"OpenCode's runner": {executor.NewOpenCode(), sandbox.SessionArchive{Harness: executor.OpenCodeName,
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

// A READING ANSWERS ONLY AN ASK WHOSE ASKED IDS NAME ONE CALL EACH: the session
// files every call's records under its id, so an asked call whose id is none
// the log keeps, two asked calls of one id, and an unasked call carrying an
// asked call's id each leave no record whose call is known; an ask that asks
// about nothing, holds no call or more than a round's log lists, and a
// session whose id names no thread are no reading either. None of them opens
// the archive; an asked call beside unasked ones, one of them with no id, is
// read.
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
