package executordomain

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY RECORD AT EVERY CAP FITS THE DECLARED BOUND.
//
// The domain declares the largest record it publishes, and the declaration is a
// LIMIT: the publisher refuses a record past it `record_too_large`, after the
// turn it records already ended. So the worst record each bound admits is built
// here through the writer's own encoder, every text field at its cap and made
// of `<`, which JSON escapes to six bytes; and the declaration is held above a
// quarter of that worst case, so the gate reserve it sizes is not room no
// append could use.
//
// Mutation: raise CompletedWorkMaxBytes to 64 KiB and the worst end no longer
// fits; halve MaxRecordBytes and neither does it.
func TestEveryRecordAtEveryCapFitsTheDeclaredBound(t *testing.T) {
	t.Parallel()
	worst := func(n int) string { return strings.Repeat("<", n) }
	handle := strings.Repeat("a", iam.MaxLogin)
	turn := statelog.NewOpID(monday, "")
	conversation := worst(ConversationKeyMaxBytes)
	ending := Ending{
		Outcome: OutcomeStopped, Decision: worst(WordMaxBytes),
		Delivered: true, DeliveredTo: worst(WordMaxBytes), Answer: worst(WordMaxBytes),
		CompletedWork: worst(CompletedWorkMaxBytes), BlockedOn: worst(BlockedOnMaxBytes),
		StoppedBy: worst(ActorMaxBytes), StopReason: worst(StopReasonMaxBytes),
		Failure: worst(FailureMaxBytes),
	}.fit()
	if !ending.within() {
		t.Fatal("the premise: an ending fitted to its caps is outside them")
	}
	records := map[string]struct {
		subject Subject
		op      OpKind
		scope   ScopeSet
		payload any
	}{
		"an end": {ConversationSubject(handle, conversation, turn), OpEnd, ScopeSet{Subject: true},
			End{V: 1, Handle: handle, Turn: turn, ConversationKey: conversation,
				WorkKey: worst(WorkKeyMaxBytes), AskExcerpt: worst(AskExcerptMaxBytes),
				Ending: ending}},
		"a capture": {ConversationSubject(handle, conversation, turn), OpCapture, seatScope(handle),
			Capture{V: 1, Handle: handle, Turn: turn, ConversationKey: conversation,
				WorkKey: worst(WorkKeyMaxBytes), CLI: worst(CLIMaxBytes),
				CLIVersion: worst(CLIVersionMaxBytes), SessionID: worst(SessionIDMaxBytes),
				Object: objstore.KeyAt(monday).String(), ObjectHash: strings.Repeat("f", 64),
				ObjectSize: SealedMaxBytes, ContentKey: worst(secrets.MaxStreamKeyIDBytes),
				ArchiveFormat: 1 << 30, LaunchSeq: 1 << 62, CaptureSeq: 1 << 62,
				Home: worst(PathMaxBytes), Workspace: worst(PathMaxBytes),
				ReadThrough: 1 << 62, SeatBudget: 1 << 62}},
		"a delete": {ConversationSubject(handle, conversation, turn), OpDelete,
			ScopeSet{Subject: true}, Delete{V: 1, Handle: handle, Turn: turn,
				ConversationKey: conversation, Cutoff: monday}},
	}
	largest := 0
	stamp := statelog.Stamp{Writer: strings.Repeat("n", 128), Gen: 1 << 22}
	by := iam.Actor{Name: strings.Repeat("e", iam.MaxLogin), Kind: iam.ActorSystem,
		OperatorID: strings.Repeat("o", iam.MaxLogin)}
	for name, r := range records {
		decided, err := decision(stamp, by, r.subject, r.op, r.scope,
			statelog.NewOpID(monday, "executor-capture"), r.payload, monday.Add(time.Hour))
		if err != nil {
			t.Fatalf("%s: encode: %v", name, err)
		}
		size := len(decided.Payload)
		t.Logf("%s at its caps encodes to %d bytes (%.1f KiB)", name, size, float64(size)/1024)
		if size > MaxRecordBytes {
			t.Errorf("%s at its caps is %d bytes and the log's largest record is %d: the "+
				"publisher would refuse it", name, size, MaxRecordBytes)
		}
		largest = max(largest, size)
	}
	if largest*4 < MaxRecordBytes {
		t.Errorf("the worst record is %d bytes and the declaration %d: a bound four "+
			"times the worst record sizes a gate reserve no append could use",
			largest, MaxRecordBytes)
	}
	if MaxRecordBytes > statelog.MaxTransportRecordBytes {
		t.Errorf("the declaration %d is past what the transport carries (%d)",
			MaxRecordBytes, statelog.MaxTransportRecordBytes)
	}
}

// AN ENDING'S PROSE IS CUT TO ITS BOUND, MARKED, AND NEVER INSIDE A RUNE: a
// reviewer's account past its cap is recorded rather than refusing the turn's
// end, and the cut is visible to the turn that reads it.
//
// Mutation: cut with textcut.Bytes, which marks nothing, and the reader takes
// a severed account for the whole of it.
func TestAnEndingsProseIsCutToItsBoundAndMarked(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", CompletedWorkMaxBytes)
	fitted := Ending{Outcome: OutcomeCompleted, CompletedWork: long}.fit()
	if len(fitted.CompletedWork) > CompletedWorkMaxBytes {
		t.Fatalf("the fitted account is %d bytes past its %d-byte cap",
			len(fitted.CompletedWork), CompletedWorkMaxBytes)
	}
	if !strings.HasSuffix(fitted.CompletedWork, "…") {
		t.Error("a cut account carries no marker")
	}
	if err := (Ending{Outcome: OutcomeCompleted, Decision: strings.Repeat("d", WordMaxBytes+1)}).validate(); err == nil {
		t.Error("an engine word past its bound was accepted rather than refused")
	}
	if err := (Ending{Outcome: "abandoned"}).validate(); err == nil {
		t.Error("an outcome this build does not know was accepted")
	}
}
