package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// recordingLog is the executor log's writes answering one scripted result, and
// keeping the last call and record each write was asked with.
type recordingLog struct {
	result   statelog.Result
	recorded bool
	err      error

	call    executordomain.Call
	begin   executordomain.NewBegin
	capture executordomain.NewCapture
	end     executordomain.NewEnd
}

func (l *recordingLog) Begin(_ context.Context, c executordomain.Call,
	n executordomain.NewBegin) (executordomain.BeginResult, error) {
	l.call, l.begin = c, n
	return executordomain.BeginResult{Result: l.result, Held: l.recorded}, l.err
}

func (l *recordingLog) Capture(_ context.Context, c executordomain.Call,
	n executordomain.NewCapture) (executordomain.CaptureResult, error) {
	l.call, l.capture = c, n
	return executordomain.CaptureResult{Result: l.result, Recorded: l.recorded}, l.err
}

func (l *recordingLog) End(_ context.Context, c executordomain.Call, n executordomain.NewEnd) (statelog.Result, error) {
	l.call, l.end = c, n
	return l.result, l.err
}

// THE SANDBOX'S RECORDS ARE THE LOG'S, UNDER THE OPERATION THEIR PLACE DERIVES:
// the sandbox states a capture and an end in its own words, because it links no
// state log, so the author, the operation and the record are stated here — and
// a capture's operation is its turn's, its launch's and its ordinal's at the
// instant the turn's operations are minted at, an end's its turn's, exactly as
// the log derives them, so every attempt at one record is one operation and
// every field the sandbox stamped reaches the log.
//
// Mutation: derive the capture's operation without its launch or its ordinal,
// the end's at the stamp's own clock, or drop a field of the stamp from the
// record, and a comparison below fails.
func TestTheSandboxsRecordsAreTheLogsUnderTheOperationTheirPlaceDerives(t *testing.T) {
	t.Parallel()
	minted := time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC)
	object := objstore.Object{Key: objstore.KeyAt(minted), Hash: objstore.Hash("h"), Size: 42}
	stamp := sandbox.SessionCapture{Minted: minted, Launch: "launch-2", LaunchSeq: 2, CaptureSeq: 3,
		Harness: "claude-code", HarnessVersion: "2.1.310", SessionID: "s-1", ContentKey: "k1",
		ArchiveFormat: 1, Home: "/home/agent", Workspace: "/workspace", SeatBudget: 64 << 20,
		ContextTokens: 61_000, Interrupted: true}
	log := &recordingLog{result: statelog.Result{Outcome: statelog.OutcomeApplied}, recorded: true}
	records := executorRecords{log: log}

	if answer, err := records.Capture(t.Context(), sandbox.CaptureRecord{Handle: "swe", Turn: "t1",
		ConversationKey: "chat:room", WorkKey: "w1", Object: object, Stamp: stamp}); answer != sandbox.LogLanded ||
		err != nil {
		t.Fatalf("Capture = %s, %v, want landed", answer, err)
	}
	if want := executordomain.CaptureOpID(minted, "swe", "t1", "launch-2", 3); log.call.OpID != want {
		t.Errorf("the capture was asked under %s, want %s", log.call.OpID, want)
	}
	if log.call.By != (iam.Actor{Name: "sandbox", Kind: iam.ActorSystem}) {
		t.Errorf("the capture names %+v as its author, want the sandbox runtime", log.call.By)
	}
	want := executordomain.NewCapture{Handle: "swe", Turn: "t1", ConversationKey: "chat:room", WorkKey: "w1",
		Harness: "claude-code", HarnessVersion: "2.1.310", SessionID: "s-1", Object: object, ContentKey: "k1",
		ArchiveFormat: 1, LaunchSeq: 2, CaptureSeq: 3, Home: "/home/agent", Workspace: "/workspace",
		SeatBudget: 64 << 20, ContextTokens: 61_000, Interrupted: true}
	if fmt.Sprintf("%+v", log.capture) != fmt.Sprintf("%+v", want) {
		t.Errorf("the log was asked to record\n%+v\nwant\n%+v", log.capture, want)
	}

	ending := executorkeep.Ending{Outcome: executorkeep.OutcomeStopped, StoppedBy: "ana"}
	if answer, err := records.End(t.Context(), sandbox.EndRecord{Handle: "swe", Turn: "t1",
		ConversationKey: "chat:room", WorkKey: "w1", Minted: minted, Ask: "fix it",
		Ending: ending}); answer != sandbox.LogLanded || err != nil {
		t.Fatalf("End = %s, %v, want landed", answer, err)
	}
	if want := executordomain.EndOpID(minted, "swe", "t1"); log.call.OpID != want {
		t.Errorf("the end was asked under %s, want %s", log.call.OpID, want)
	}
	if n := log.end; n.Handle != "swe" || n.Turn != "t1" || n.ConversationKey != "chat:room" ||
		n.WorkKey != "w1" || n.Ask != "fix it" || !reflect.DeepEqual(n.Ending, ending) {
		t.Errorf("the log was asked to end %+v", n)
	}
}

// THE LOG'S ANSWER IS READ AS WHAT THE SANDBOX DOES NEXT: a capture its row
// names is landed, one the log applied and the row does not name is passed
// over, a definite refusal is refused with its error, and everything else —
// pending, unknown, a node that did not answer, no data node of a build that
// reads the record whole — is unsettled, to be asked again under the same
// operation. An end refused as already made is landed: the paths that end one
// turn derive one operation, and the first was taken.
//
// AN END NO DATA NODE CAN READ WHOLE STAYS OWED, mid-upgrade: a seats-only node
// of a newer build writes a field of the ending its older data nodes do not
// declare, they refuse it rather than record it short, and refused-and-dropped
// here the turn's end would never reach the log at all — so it is unsettled,
// as an estate nobody serves is, and the run's row keeps owing it until a data
// node of the writer's build records it.
//
// Mutation: read a pending capture as landed, a turn already ended as an end
// still owed, a node that did not answer as a refusal, or a record no data
// node's build reads whole as a malformed one, and a row fails.
func TestTheLogsAnswerIsReadAsWhatTheSandboxDoesNext(t *testing.T) {
	t.Parallel()
	applied := statelog.Result{Outcome: statelog.OutcomeApplied}
	unanswered := errors.New("estate: the write was sent to a data node that did not answer")
	unread := &estate.ErrUnknownKeys{Op: "executor.end", Keys: []string{"args.Ending.went_on"},
		Nodes: []string{"data-a"}, Unserved: &estate.ErrUnserved{Detail: "data-a: its build does not declare it"}}
	for _, tc := range []struct {
		name     string
		end      bool
		result   statelog.Result
		recorded bool
		err      error
		want     sandbox.LogAnswer
	}{
		{name: "a capture its row names", result: applied, recorded: true, want: sandbox.LogLanded},
		{name: "a capture another got ahead of", result: applied, want: sandbox.LogPassedOver},
		{name: "a capture of an ended turn", err: fmt.Errorf("%w: t1", executordomain.ErrTurnEnded),
			want: sandbox.LogRefused},
		{name: "a stale capture", err: executordomain.ErrStaleCapture, want: sandbox.LogRefused},
		{name: "a capture past the seat's budget", err: executordomain.ErrOverBudget, want: sandbox.LogRefused},
		{name: "a capture uploaded too long ago", err: executordomain.ErrUploadStale, want: sandbox.LogRefused},
		{name: "a session past its bound", err: executorkeep.ErrSessionTooLarge, want: sandbox.LogRefused},
		{name: "a malformed capture", err: executordomain.ErrInvalid, want: sandbox.LogRefused},
		{name: "a capture still pending", result: statelog.Result{Outcome: statelog.OutcomePending},
			want: sandbox.LogUnsettled},
		{name: "a capture no node answered", result: statelog.Result{Outcome: statelog.OutcomeUnknown},
			err: unanswered, want: sandbox.LogUnsettled},
		{name: "an end taken", end: true, result: applied, want: sandbox.LogLanded},
		{name: "an end another path made first", end: true,
			err: fmt.Errorf("%w: t1", executordomain.ErrTurnEnded), want: sandbox.LogLanded},
		{name: "a malformed end", end: true, err: executordomain.ErrInvalid, want: sandbox.LogRefused},
		{name: "an end of a stale ordinal is no refusal of an end", end: true,
			err: executordomain.ErrStaleCapture, want: sandbox.LogUnsettled},
		{name: "an end no node answered", end: true, result: statelog.Result{Outcome: statelog.OutcomeUnknown},
			err: unanswered, want: sandbox.LogUnsettled},
		{name: "an end no data node of the writer's build serves stays owed", end: true, err: unread,
			want: sandbox.LogUnsettled},
		{name: "a capture no data node of the writer's build serves", err: unread, want: sandbox.LogUnsettled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			records := executorRecords{log: &recordingLog{result: tc.result, recorded: tc.recorded, err: tc.err}}
			var answer sandbox.LogAnswer
			var err error
			if tc.end {
				answer, err = records.End(t.Context(), sandbox.EndRecord{Handle: "swe", Turn: "t1",
					Ending: executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}})
			} else {
				answer, err = records.Capture(t.Context(), sandbox.CaptureRecord{Handle: "swe", Turn: "t1"})
			}
			if answer != tc.want {
				t.Fatalf("answered %s (%v), want %s", answer, err, tc.want)
			}
			switch {
			case answer == sandbox.LogLanded || answer == sandbox.LogPassedOver:
				if err != nil {
					t.Errorf("a settled record answered the error %v", err)
				}
			case err == nil:
				t.Error("a refused or unsettled record answered no error to say why")
			case tc.err != nil && !errors.Is(err, tc.err):
				t.Errorf("the answer's error %v does not carry the log's %v", err, tc.err)
			case answer == sandbox.LogUnsettled && !strings.Contains(err.Error(), string(tc.result.Outcome)):
				t.Errorf("an unsettled answer says %q, not the log's outcome %q", err, tc.result.Outcome)
			}
		})
	}
}
