package builtin

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE MERGE REFUSALS THE CHANGE PAGE LEAVES TO THE ENGINE ARE WORDED FOR A
// PERSON (ADR-0060). The page's merge controls answer most of Gitlet's
// refusals in words of their own, keyed on the host's word, and draw the
// engine's words for the rest: a gate the change does not meet yet, a merge
// that would delete a branch it may not, a repository at its size, and a
// merge or a dequeue asked by or for somebody who may not make it. Each of
// those carries a person's words beside the sentence for a model, which stays
// the tool's answer: the gate's naming what the change still waits for and
// whoever can move it forward, as @handle, and never the host's sentence.
//
// Mutation: answer the gate's refusal without toPerson, or drop
// source_refused from mergeRefusalWords, and this fails.
func TestTheMergeRefusalsThePageLeavesToTheEngineAreWordedForAPerson(t *testing.T) {
	t.Parallel()
	unmet := []code.Precondition{
		{Kind: code.PreconditionApproval, State: code.PreconditionUnmet, Detail: "needs one more approval",
			WaitsOn: []code.Party{{Handle: "cto", Kind: code.PartyPerson}}},
		{Kind: code.PreconditionCheck, State: code.PreconditionUnmet, Detail: "ci failed on the head",
			WaitsOn: []code.Party{{Handle: "backend-engineer", Kind: code.PartyAgent},
				{Handle: "cto", Kind: code.PartyPerson}}},
	}
	for _, tc := range []struct {
		tool    string
		refusal code.MergeRefusal
		says    []string
	}{
		{CodeMergeChangeTool, code.MergeRefusalGateUnmet, []string{"an approval of its latest revision",
			"its required checks to pass", "@cto and @backend-engineer can move it forward"}},
		{CodeMergeChangeTool, code.MergeRefusalSourceRefused, []string{"Merge it keeping its branch."}},
		{CodeMergeChangeTool, code.MergeRefusalRepoSize, []string{"past the most a repository may hold"}},
		{CodeMergeChangeTool, code.MergeRefusalNotPermitted, []string{"asked for somebody who may not make it"}},
		{CodeDequeueChangeTool, code.MergeRefusalNotPermitted, []string{"may take it off the queue"}},
	} {
		res := mergeRefused(tc.tool, "web!42", "main", tc.refusal, "the host's sentence", unmet)
		said := res.PersonRefusal.Person()
		if said == "" {
			t.Errorf("%s answered by %s carries no person's words: the page draws the code's sentence alone",
				tc.refusal, tc.tool)
			continue
		}
		for _, want := range tc.says {
			if !strings.Contains(said, want) {
				t.Errorf("%s answered by %s says %q, want it to say %q", tc.refusal, tc.tool, said, want)
			}
		}
		for _, model := range []string{"the host's sentence", tc.tool, string(tc.refusal), "needs one more"} {
			if strings.Contains(said, model) {
				t.Errorf("%s answered by %s tells a person %q, which is the model's: %q", tc.refusal, tc.tool,
					model, said)
			}
		}
		if !strings.HasPrefix(res.Output, tc.tool+":") {
			t.Errorf("%s answered by %s lost the sentence for a model: %q", tc.refusal, tc.tool, res.Output)
		}
	}
}

// EVERY CONDITION A MERGE GATE JUDGES HAS A PERSON'S WORDS OF ITS OWN in the
// refusal catalog, held to Gitlet's closed set: the catalog keys its
// words on the host's names for them without importing the host, so a kind
// the host adds, or one spelled differently there, would be said as another
// condition of the target's gate, which tells a person nothing they can act
// on, and two kinds said alike tell them less than the page does.
//
// Mutation: drop a kind from caveat's gateWaitsFor, or give two kinds one
// phrase, and this fails.
func TestEveryGateConditionHasAPersonsWords(t *testing.T) {
	t.Parallel()
	said := func(kind string) string {
		return caveat.MergeGateUnmet([]caveat.GateCondition{{Kind: kind}}).Person()
	}
	unknown := said("a kind no build judges")
	seen := map[string]code.PreconditionKind{}
	for _, kind := range code.PreconditionKinds() {
		words := said(string(kind))
		if words == unknown {
			t.Errorf("%s is said as a condition the catalog does not know: %q", kind, words)
		}
		if other, twice := seen[words]; twice {
			t.Errorf("%s and %s are said alike: %q", kind, other, words)
		}
		seen[words] = kind
	}
}

// A MERGE REFUSAL CARRIES GITLET'S OWN WORD beneath its class, as a
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

// EVERY MERGE REFUSAL IS CLASSED BY WHAT CLEARS IT, by internal/mcp's one rule
// for every domain: the change or its repository in a state that refuses the
// merge until somebody changes it (closed, merged, a draft, archived, a pull
// mirror) conflict, as a race lost is, and never invalid or forbidden, which
// would tell a caller who asked nothing wrong to send something else or that
// they may not; a repository past the most it may keep quota_exceeded; a
// committer busy or a compaction behind unavailable, as a push's are, since a
// wait clears them and nobody the sentence could name changes anything; an
// entry no queue holds, which never becomes live again by itself, not_found.
// The table names EVERY value of code.MergeRefusals(), so a refusal the
// domain adds fails here until somebody decides its class, rather than
// falling into one unexamined; and each is answered in that class.
//
// Mutation: class change_not_open as invalid again → red; class busy as
// conflict again → red.
func TestEveryMergeRefusalIsClassedByWhatClearsIt(t *testing.T) {
	t.Parallel()
	want := map[code.MergeRefusal]tools.Refusal{
		code.MergeRefusalGateUnmet: tools.RefusalNotReady,

		code.MergeRefusalStale:       tools.RefusalStaleVersion,
		code.MergeRefusalHeadMoved:   tools.RefusalStaleVersion,
		code.MergeRefusalTargetMoved: tools.RefusalStaleVersion,

		code.MergeRefusalChangeNotOpen: tools.RefusalConflict,
		code.MergeRefusalChangeDraft:   tools.RefusalConflict,
		code.MergeRefusalRepoArchived:  tools.RefusalConflict,
		code.MergeRefusalRepoMirrored:  tools.RefusalConflict,
		code.MergeRefusalTargetMoving:  tools.RefusalConflict,
		code.MergeRefusalQueueFull:     tools.RefusalConflict,

		code.MergeRefusalRepoSize: tools.RefusalQuotaExceeded,

		code.MergeRefusalBusy:             tools.RefusalUnavailable,
		code.MergeRefusalCompactionBehind: tools.RefusalUnavailable,
		code.MergeRefusalRepoImporting:    tools.RefusalUnavailable,
		code.MergeRefusalExpunging:        tools.RefusalUnavailable,
		code.MergeRefusalUploadExpired:    tools.RefusalUnavailable,
		code.MergeRefusalUnavailable:      tools.RefusalUnavailable,
		code.MergeRefusalNotApplied:       tools.RefusalUnavailable,

		code.MergeRefusalNotPerson:    tools.RefusalForbidden,
		code.MergeRefusalNotPermitted: tools.RefusalForbidden,

		code.MergeRefusalRepoDeleted:    tools.RefusalNotFound,
		code.MergeRefusalChangeNotFound: tools.RefusalNotFound,
		code.MergeRefusalNotQueued:      tools.RefusalNotFound,

		code.MergeRefusalAlreadyQueued: tools.RefusalExists,

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
