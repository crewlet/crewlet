package builtin

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/tools"
)

// A MERGE REFUSAL CARRIES THE CODE HOST'S OWN WORD beneath its class, as a
// value. Several of the host's refusals share one class — a head that moved
// and a train whose base moved are both stale_version — and a person's
// surface answers each in its own words by reading the word (the act route's
// `refusal`, operator.Fail), never by parsing the sentence written for a
// model; a gate refusal, answered not_ready, carries it as much as any.
//
// Mutation: have writeRefusalError return a plain error again, and this
// fails.
func TestAMergeRefusalCarriesTheHostsOwnWord(t *testing.T) {
	t.Parallel()
	for _, refusal := range []code.MergeRefusal{code.MergeRefusalHeadMoved, code.MergeRefusalTargetMoving,
		code.MergeRefusalGateUnmet} {
		res := mergeRefused(CodeMergeChangeTool, "web!42", "main", refusal, "the host's sentence", nil)
		var host *HostRefusal
		if !errors.As(res.Cause, &host) {
			t.Fatalf("%s's refusal carries no host word beneath its class: %v", refusal, res.Cause)
		}
		if host.Refusal != string(refusal) || host.Detail != "the host's sentence" {
			t.Errorf("%s's refusal carries %+v, want its own word and sentence", refusal, host)
		}
	}
}

// EVERY MERGE REFUSAL IS CLASSED BY WHAT CLEARS IT (M5.md §2.10, read against
// mcp's definition of each class): conflict only for what clears on its own,
// so the same call shortly after lands; a change closed, merged or a draft,
// which waits on another call, invalid; an entry no queue holds, which never
// becomes live again by itself, not_found. The table names EVERY value of
// code.MergeRefusals(), so a refusal the domain adds fails here until
// somebody decides its class, rather than falling into one unexamined; and
// each is answered in that class.
//
// Mutation: class change_not_open as conflict again → red.
func TestEveryMergeRefusalIsClassedByWhatClearsIt(t *testing.T) {
	t.Parallel()
	want := map[code.MergeRefusal]tools.Refusal{
		code.MergeRefusalGateUnmet: tools.RefusalNotReady,

		code.MergeRefusalStale:       tools.RefusalStaleVersion,
		code.MergeRefusalHeadMoved:   tools.RefusalStaleVersion,
		code.MergeRefusalTargetMoved: tools.RefusalStaleVersion,

		code.MergeRefusalTargetMoving:     tools.RefusalConflict,
		code.MergeRefusalBusy:             tools.RefusalConflict,
		code.MergeRefusalCompactionBehind: tools.RefusalConflict,
		code.MergeRefusalQueueFull:        tools.RefusalConflict,

		code.MergeRefusalRepoImporting: tools.RefusalUnavailable,
		code.MergeRefusalExpunging:     tools.RefusalUnavailable,
		code.MergeRefusalUploadExpired: tools.RefusalUnavailable,
		code.MergeRefusalUnavailable:   tools.RefusalUnavailable,
		code.MergeRefusalNotApplied:    tools.RefusalUnavailable,

		code.MergeRefusalNotPerson:    tools.RefusalForbidden,
		code.MergeRefusalNotPermitted: tools.RefusalForbidden,
		code.MergeRefusalRepoArchived: tools.RefusalForbidden,
		code.MergeRefusalRepoMirrored: tools.RefusalForbidden,
		code.MergeRefusalRepoSize:     tools.RefusalForbidden,

		code.MergeRefusalRepoDeleted:    tools.RefusalNotFound,
		code.MergeRefusalChangeNotFound: tools.RefusalNotFound,
		code.MergeRefusalNotQueued:      tools.RefusalNotFound,

		code.MergeRefusalAlreadyQueued: tools.RefusalExists,

		code.MergeRefusalChangeNotOpen: tools.RefusalInvalid,
		code.MergeRefusalChangeDraft:   tools.RefusalInvalid,
		code.MergeRefusalQueueTarget:   tools.RefusalInvalid,
		code.MergeRefusalNoQueue:       tools.RefusalInvalid,
		code.MergeRefusalSourceRefused: tools.RefusalInvalid,
		code.MergeRefusalNotGreen:      tools.RefusalInvalid,
		code.MergeRefusalNotRed:        tools.RefusalInvalid,
		code.MergeRefusalNoLanding:     tools.RefusalInvalid,
		code.MergeRefusalRecordFull:    tools.RefusalInvalid,
	}
	all := code.MergeRefusals()
	if len(want) != len(all) {
		t.Errorf("the table classes %d refusals and the domain has %d: decide each new one's class here",
			len(want), len(all))
	}
	for _, r := range all {
		class, ok := want[r]
		if !ok {
			t.Errorf("%s has no class in the table: decide what clears it, and class it by that", r)
			continue
		}
		if got := mergeRefusalClass(r); got != class {
			t.Errorf("%s is classed %s, want %s", r, got, class)
		}
		res := mergeRefused(CodeMergeChangeTool, "web!42", "main", r, "the host's sentence", nil)
		if got := tools.RefusalOf(res); !res.Failed || got != class {
			t.Errorf("%s is answered %s, want %s", r, got, class)
		}
	}
}
