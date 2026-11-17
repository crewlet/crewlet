package engine

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE FREEZE FITS ITS BOUND, whatever the log admits: the largest read a head
// can answer (most turns handed out, most siblings offered, each with an
// object, and a base whose every free-text field is at the log's bound and
// escapes at JSON's worst) encodes within sandbox.ConversationBaseMaxBytes, its
// share of the run's record. Only what the brief told is frozen: a turn its
// bound left out is neither absorbed nor offered, and stays for the next read.
// It lives here because the bound is the sandbox's, the limits the executor
// log's, and the sandbox links no state log; the read is frozen as dispatch
// freezes it ([executordomain.Head.Kept], conversation.go).
//
// Mutation: lower the bound below the largest read, or freeze what the brief
// did not tell, and this goes red.
func TestAFrozenReadFitsItsBound(t *testing.T) {
	t.Parallel()
	turn := func(i int) string { return fmt.Sprintf("0190f3a0-0000-7000-8000-%012d", i) }
	// A CONTROL CHARACTER, which JSON escapes as six bytes: every free-text
	// field at its bound, at the encoding's worst.
	const worst = "\x01"
	object := func() objstore.Object {
		return objstore.Object{Key: objstore.KeyAt(time.Now()), Hash: objstore.Hash(strings.Repeat("f", 64)),
			Size: executorkeep.SealedMaxBytes}
	}
	head := executordomain.Session{Handle: "swe", Turn: turn(0), Object: object(),
		ContentKey:     strings.Repeat(worst, executordomain.ContentKeyMaxBytes),
		Harness:        strings.Repeat(worst, executorkeep.HarnessMaxBytes),
		HarnessVersion: strings.Repeat(worst, executorkeep.HarnessVersionMaxBytes),
		SessionID:      strings.Repeat(worst, executorkeep.SessionIDMaxBytes),
		Home:           strings.Repeat(worst, executorkeep.PathMaxBytes),
		Workspace:      strings.Repeat(worst, executorkeep.PathMaxBytes),
		ContextTokens:  1 << 40, Interrupted: true, ArchiveFormat: 1 << 20, CapturedAt: time.Now().UTC(),
		EndedAt: time.Now().UTC(),
		// LOST, ITS END SAYING IT WENT ON PAST THE SESSION: the base carries
		// both, and when the session was kept.
		Ending: &executorkeep.Ending{Outcome: executorkeep.OutcomeLost, WentOn: true}}
	var endings []executordomain.Session
	told := []string{head.Turn}
	for i := 1; i <= executordomain.MaxEndings; i++ {
		ended := executordomain.Session{Handle: "swe", Turn: turn(i),
			Ending: &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}}
		if i <= executordomain.MaxSiblings {
			ended.Object = object()
		}
		endings = append(endings, ended)
		told = append(told, ended.Turn)
	}
	var siblings []executordomain.Session
	for _, e := range endings {
		if e.HasObject() {
			siblings = append(siblings, e)
		}
	}
	read := executordomain.Head{Session: &head, Endings: endings, Siblings: siblings,
		Position: statelog.Position{Stream: executordomain.Domain{}.Stream().Name, Generation: 1 << 22,
			Seq: 1 << 39}}
	largest := sandbox.NewConversationBase(read.Kept(), told)
	raw, err := json.Marshal(largest)
	if err != nil {
		t.Fatalf("encoding the read: %v", err)
	}
	switch {
	case len(largest.Absorbs) != executordomain.MaxAbsorbs || len(largest.Siblings) != executordomain.MaxSiblings:
		t.Fatalf("the largest read absorbs %d and offers %d, want %d and %d", len(largest.Absorbs),
			len(largest.Siblings), executordomain.MaxAbsorbs, executordomain.MaxSiblings)
	case largest.Session == nil || !largest.Session.WentOn || !largest.Session.Lost ||
		largest.Session.CapturedAt.IsZero():
		t.Fatalf("the largest read froze the base %+v, want it carrying every field a base can", largest.Session)
	case len(raw) > sandbox.ConversationBaseMaxBytes:
		t.Fatalf("the largest read encodes in %d bytes, past its bound of %d", len(raw),
			sandbox.ConversationBaseMaxBytes)
	}
	t.Logf("the largest freeze is %d bytes of %d", len(raw), sandbox.ConversationBaseMaxBytes)

	// ONLY WHAT THE BRIEF TOLD: the head and the newest ending, the rest left
	// out by the brief's bound.
	partial := sandbox.NewConversationBase(read.Kept(), []string{head.Turn, endings[len(endings)-1].Turn})
	if !slices.Equal(partial.Absorbs, []string{head.Turn, endings[len(endings)-1].Turn}) ||
		len(partial.Siblings) != 0 || partial.Session == nil {
		t.Fatalf("a read whose brief told two turns froze %+v", partial)
	}
	if untold := sandbox.NewConversationBase(read.Kept(), nil); untold.Session != nil || len(untold.Absorbs) != 0 {
		t.Fatalf("a read whose brief told nothing froze %+v", untold)
	}
}
