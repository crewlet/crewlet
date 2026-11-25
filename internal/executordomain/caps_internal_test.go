package executordomain

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY RECORD AT EVERY CAP FITS THE DECLARED BOUND.
//
// The declared largest record is a LIMIT: the publisher refuses a record past
// it `record_too_large`, after its turn already ended. So the worst record each
// bound admits is built through the writer's own encoder, every text field at
// its cap and of `<` (six bytes JSON-escaped), and the declaration is held
// within four times it, or the gate reserve it sizes is room no append uses.
//
// Mutation: raise CompletedWorkMaxBytes to 64 KiB, or MaxServed to 32, and the
// worst end no longer fits; halve MaxRecordBytes and neither does it; raise
// MediaMaxCount to 512 and the worst capture no longer fits.
func TestEveryRecordAtEveryCapFitsTheDeclaredBound(t *testing.T) {
	t.Parallel()
	worst := func(n int) string { return strings.Repeat("<", n) }
	handle := strings.Repeat("a", iam.MaxLogin)
	turn := statelog.NewOpID(monday, "")
	conversation := worst(ConversationKeyMaxBytes)
	ending := executorkeep.Ending{
		Outcome: executorkeep.OutcomeStopped, Decision: worst(executorkeep.WordMaxBytes),
		Delivered: true, DeliveredTo: worst(executorkeep.WordMaxBytes), Answer: worst(executorkeep.WordMaxBytes),
		CompletedWork: worst(executorkeep.CompletedWorkMaxBytes), BlockedOn: worst(executorkeep.BlockedOnMaxBytes),
		StoppedBy: worst(executorkeep.ActorMaxBytes), StopReason: worst(executorkeep.StopReasonMaxBytes),
		Failure:  worst(executorkeep.FailureMaxBytes),
		Unkept:   &executorkeep.Unkept{Loss: executorkeep.LossStoreRefused, LaunchSeq: 1 << 62, CaptureSeq: 1 << 62, Reason: worst(executorkeep.UnkeptReasonMaxBytes)},
		WentOn:   true,
		LastWord: &executorkeep.CapturePlace{LaunchSeq: 1 << 62, CaptureSeq: 1 << 62},
		SpentSessions: slices.Repeat([]executorkeep.SpentSession{{Session: worst(executorkeep.SessionIDMaxBytes),
			Loss: executorkeep.Loss(worst(executorkeep.WordMaxBytes))}}, executorkeep.SpentMaxSessions),
		NotContinued: slices.Repeat([]executorkeep.NotContinued{{Loss: executorkeep.LossContextExhausted,
			Instead: executorkeep.InsteadOlder, LostTurn: worst(executorkeep.TurnIDMaxBytes),
			OlderTurn: worst(executorkeep.TurnIDMaxBytes), Detail: worst(executorkeep.UnkeptReasonMaxBytes)}},
			executorkeep.NotContinuedMaxEntries),
	}.Fit()
	if !ending.Within() {
		t.Fatal("the premise: an ending fitted to its caps is outside them")
	}
	served := make([]string, MaxServed)
	for i := range served {
		served[i] = worst(ConversationKeyMaxBytes-1) + strconv.Itoa(i)
	}
	absorbs := make([]string, MaxAbsorbs)
	for i := range absorbs {
		absorbs[i] = statelog.NewOpID(monday, "")
	}
	media := make([]CaptureMedia, executorkeep.MediaMaxCount)
	for i := range media {
		media[i] = CaptureMedia{Digest: fmt.Sprintf("%064x", i), Object: objstore.KeyAt(monday).String(),
			ObjectHash: strings.Repeat("f", 64), ObjectSize: 1 << 62,
			ContentKey: worst(secrets.MaxStreamKeyIDBytes), Carried: true}
	}
	records := map[string]struct {
		subject Subject
		op      OpKind
		scope   ScopeSet
		payload any
	}{
		"an end": {ConversationSubject(handle, conversation, turn), OpEnd, seatScope(handle),
			End{V: 1, Handle: handle, Turn: turn, ConversationKey: conversation,
				WorkKey: worst(WorkKeyMaxBytes), AskExcerpt: worst(AskExcerptMaxBytes),
				Ending: ending, Served: served, SeenRemoval: 1 << 62}},
		"a capture": {ConversationSubject(handle, conversation, turn), OpCapture, seatScope(handle),
			Capture{V: 1, Handle: handle, Turn: turn, ConversationKey: conversation,
				WorkKey: worst(WorkKeyMaxBytes), Harness: worst(executorkeep.HarnessMaxBytes),
				HarnessVersion: worst(executorkeep.HarnessVersionMaxBytes), SessionID: worst(executorkeep.SessionIDMaxBytes),
				Entry:  worst(executorkeep.EntryMaxBytes),
				Object: objstore.KeyAt(monday).String(), ObjectHash: strings.Repeat("f", 64),
				ObjectSize: executorkeep.SealedMaxBytes, ContentKey: worst(secrets.MaxStreamKeyIDBytes),
				ArchiveFormat: 1 << 30, Media: media, LaunchSeq: 1 << 62, CaptureSeq: 1 << 62,
				Home: worst(executorkeep.PathMaxBytes), Workspace: worst(executorkeep.PathMaxBytes),
				ContextTokens: 1 << 62, Interrupted: true,
				ReadThrough: 1 << 62, Absorbs: absorbs, SeenRemoval: 1 << 62,
				SeatBudget: 1 << 62}},
		"a begin": {ConversationSubject(handle, conversation, turn), OpBegin, ScopeSet{Subject: true},
			Begin{V: 1, Handle: handle, Turn: turn, ConversationKey: conversation,
				WorkKey: worst(WorkKeyMaxBytes), ReadThrough: 1 << 62, Absorbs: absorbs,
				BaseTurn: absorbs[0], BaseObject: objstore.KeyAt(monday).String(),
				BaseHash: strings.Repeat("f", 64), SeenRemoval: 1 << 62}},
		"a delete": {ConversationSubject(handle, conversation, turn), OpDelete,
			seatScope(handle), Delete{V: 1, Handle: handle, Turn: turn,
				ConversationKey: conversation, Cutoff: monday}},
		"a seat's removal": {SeatSubject(handle), OpSeatDelete, ScopeSet{Subject: true},
			SeatDelete{V: 1, Handle: handle}},
		"a removal's retention": {SeatSubject(handle), OpDelete, ScopeSet{Subject: true},
			Delete{V: 1, Handle: handle, Cutoff: monday}},
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
