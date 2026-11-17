package builtin_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// viewedTool is code_mark_viewed over a host holding site!7 and writes, as the operator catalogue
// serves it.
func viewedTool(t *testing.T, f *changeFixture, writes *codeWrites) (tools.Callable, *gitletHost) {
	t.Helper()
	host := newChangeHost(f)
	wired := codeDeps(host)
	wired.Writer = writes
	tool, ok := readersCatalogueOver(wired, nil)[builtin.CodeMarkViewedTool]
	if !ok {
		t.Fatal("the operator catalogue over Gitlet's writes does not serve code_mark_viewed")
	}
	return tool, host
}

// A VIEWED MARK IS A PERSON'S ALONE: the operator catalogue serves code_mark_viewed beside Gitlet's
// writes and not without them, no seat's registry holds it, a seat or a credential bound to no seat is
// refused forbidden before anything is read, and its description names its bounds as numbers.
//
// Mutations: add code_mark_viewed to CodeTools, and a seat's registry holds it; offer it over the
// code reads alone, and a surface that changes no code marks files; admit a seat in the authority
// row and drop the tool's own person check, and a seat's turn reads the change.
func TestCodeMarkViewedIsAPersonsAlone(t *testing.T) {
	t.Parallel()
	writes := &codeWrites{}
	tool, host := viewedTool(t, siteChange(), writes)
	if _, ok := readersCatalogueOver(codeDeps(newGitletHost()), nil)[builtin.CodeMarkViewedTool]; ok {
		t.Error("the operator catalogue served code_mark_viewed over Gitlet with no writes wired")
	}
	wired := codeDeps(host)
	wired.Writer = writes
	if _, ok := codeRegistry(t, wired).Lookup(builtin.CodeMarkViewedTool); ok {
		t.Error("a seat's registry holds code_mark_viewed")
	}
	for _, caller := range []struct {
		name string
		ctx  context.Context
		turn *turnctx.Turn
	}{{"a credential bound to no seat", asUnboundToken(), nil}, {"a seat", asSeat("agent-cto"), writerTurn(t)}} {
		got, err := asSeatCallable(t, tool).CallForTurn(caller.ctx, caller.turn, map[string]any{"repo": "site",
			"change": "7", "viewed": []any{map[string]any{"path": "a.txt"}}})
		if err != nil || !got.Failed || tools.RefusalOf(got) != tools.RefusalForbidden || len(host.reads) != 0 ||
			len(writes.marks) != 0 {
			t.Errorf("%s answered %v %+v having read %d times, want forbidden before any read or write",
				caller.name, err, got, len(host.reads))
		}
	}
	for _, n := range []int{gitlet.MaxViewedPerMark, gitlet.MaxViewedFiles} {
		if !strings.Contains(tool.Description(), strconv.Itoa(n)) {
			t.Errorf("the description does not name %d", n)
		}
	}
}

// A VIEWED MARK IS SENT AT ITS READ: the write names the repository id and the change id the call's
// one read answered, the files as given (a deleted file with neither id nor mode), and that read's
// position as Seen; the read is at or past the caller's `seen`; the answer names the handle, the
// repository, the change, the outcome and the position, and no digest or key.
//
// Mutation: send the caller's `seen` as Seen rather than the read's position, red.
func TestCodeMarkViewedSendsTheGestureAtItsRead(t *testing.T) {
	t.Parallel()
	writes := &codeWrites{}
	f := siteChange()
	tool, host := viewedTool(t, f, writes)
	floor := codeAt(12)
	got, err := tool.Call(asBoundPerson(), map[string]any{"repo": "site", "change": "7", "seen": floor.String(),
		"viewed": []any{map[string]any{"path": "a.txt", "blob_oid": mainOID, "mode": "100644"},
			map[string]any{"path": "gone.txt"}}, "unviewed": []any{"b.txt"}})
	if err != nil || got.Failed {
		t.Fatalf("a person's viewed mark answered %v %q", err, got.Output)
	}
	writes.mu.Lock()
	marks := slices.Clone(writes.marks)
	writes.mu.Unlock()
	if len(marks) != 1 || marks[0].Files == nil {
		t.Fatalf("Gitlet was asked %+v, want one gesture over files", marks)
	}
	w := marks[0]
	want := []gitlet.ViewedFile{{Path: "a.txt", BlobOID: mainOID, Mode: "100644"}, {Path: "gone.txt"}}
	switch {
	case w.Handle != "founder" || w.Asker.Seat != "founder" || w.OpID == "":
		t.Errorf("the mark is %+v, want founder's own under an operation", w)
	case w.Files.RepoID != siteID || w.Files.ChangeID != siteChangeID:
		t.Errorf("the mark names %s of %s, want the change the read answered", w.Files.ChangeID, w.Files.RepoID)
	case !slices.Equal(w.Files.Viewed, want) || !slices.Equal(w.Files.Unviewed, []string{"b.txt"}):
		t.Errorf("the mark carries %+v and %v, want the files as given", w.Files.Viewed, w.Files.Unviewed)
	case w.Seen != f.at:
		t.Errorf("the mark was sent seen at %s, want the read's position %s", w.Seen, f.at)
	}
	host.mu.Lock()
	reads := slices.Clone(host.reads)
	host.mu.Unlock()
	// A CALL'S FIRST READ RESOLVES THE REPOSITORY, linearizable, which reaches past any floor; a read on a
	// repository the turn already resolved is floored at `seen` (changeRead.read).
	if len(reads) != 1 || !reads[0].Brief || !reads[0].DetailOnly ||
		(reads[0].Fresh.Level != statelog.ReadLinearizable && reads[0].Fresh.Floor(floor.Stream) != floor) {
		t.Errorf("the change was read %+v, want its row alone once, at or past %s", reads, floor)
	}
	for _, says := range []string{`"handle": "founder"`, `"repo": "site"`, `"change": "site!7"`,
		`"outcome": "applied"`, `"position"`} {
		if !strings.Contains(got.Output, says) {
			t.Errorf("the answer %s does not carry %s", got.Output, says)
		}
	}
	if strings.Contains(got.Output, "digest") || strings.Contains(got.Output, "key") {
		t.Errorf("the answer %s carries a digest or a key", got.Output)
	}
}

// A VIEWED MARK SAYS EACH REFUSAL IN ITS CLASS: not_permitted forbidden, ended conflict, invalid
// invalid, busy and unavailable unavailable, an unknown outcome unknown; a closed change refused
// before any write; 33 files refused before any read; a `seen` on another log refused with this
// tool's hint.
//
// Mutation: drop the ended arm of markRefused, and ended is answered as this node's fault, red.
func TestCodeMarkViewedSaysEachRefusalInItsClass(t *testing.T) {
	t.Parallel()
	args := func() map[string]any {
		return map[string]any{"repo": "site", "change": "7",
			"viewed": []any{map[string]any{"path": "a.txt", "blob_oid": mainOID, "mode": "100644"}}}
	}
	for refusal, class := range map[gitlet.MarkRefusal]tools.Refusal{
		gitlet.MarkNotPermitted: tools.RefusalForbidden, gitlet.MarkEnded: tools.RefusalConflict,
		gitlet.MarkInvalid: tools.RefusalInvalid, gitlet.MarkBusy: tools.RefusalUnavailable,
		gitlet.MarkUnavailable: tools.RefusalUnavailable,
	} {
		writes := &codeWrites{mark: func(gitlet.MarkWrite) (gitlet.MarkResult, error) {
			return gitlet.MarkResult{Refusal: refusal, Detail: "said"}, nil
		}}
		tool, _ := viewedTool(t, siteChange(), writes)
		got, err := tool.Call(asBoundPerson(), args())
		if err != nil || !got.Failed || tools.RefusalOf(got) != class {
			t.Errorf("%s answered %v %+v, want %s", refusal, err, got, class)
		}
	}
	for name, answer := range map[string]func(gitlet.MarkWrite) (gitlet.MarkResult, error){
		"an unknown outcome": func(gitlet.MarkWrite) (gitlet.MarkResult, error) {
			return gitlet.MarkResult{Outcome: statelog.OutcomeUnknown}, nil
		},
		"a lost answer": func(gitlet.MarkWrite) (gitlet.MarkResult, error) {
			return gitlet.MarkResult{}, estate.ErrOutcomeUnknown
		},
	} {
		tool, _ := viewedTool(t, siteChange(), &codeWrites{mark: answer})
		got, err := tool.Call(asBoundPerson(), args())
		var unknown *builtin.UnknownOutcome
		if err != nil || !got.Failed || !errors.As(got.Cause, &unknown) {
			t.Errorf("%s answered %v %+v, want unknown", name, err, got)
		}
	}

	closed := siteChange()
	closed.detail.State = gitlet.ChangeStateClosed
	writes := &codeWrites{}
	tool, _ := viewedTool(t, closed, writes)
	got, err := tool.Call(asBoundPerson(), args())
	if err != nil || !got.Failed || tools.RefusalOf(got) != tools.RefusalConflict || len(writes.marks) != 0 {
		t.Errorf("a closed change's mark answered %v %+v, want conflict before any write", err, got)
	}

	many := make([]any, gitlet.MaxViewedPerMark+1)
	for i := range many {
		many[i] = map[string]any{"path": strconv.Itoa(i) + ".txt"}
	}
	tool, host := viewedTool(t, siteChange(), &codeWrites{})
	wide := args()
	wide["viewed"] = many
	if got, err := tool.Call(asBoundPerson(), wide); err != nil || !got.Failed ||
		tools.RefusalOf(got) != tools.RefusalInvalid || len(host.reads) != 0 {
		t.Errorf("33 files answered %v %+v having read %d times, want invalid before any read", err, got,
			len(host.reads))
	}
	elsewhere := args()
	elsewhere["seen"] = "CREWLET_TRACKER_LOG@1:12"
	if got, err := tool.Call(asBoundPerson(), elsewhere); err != nil || !got.Failed ||
		tools.RefusalOf(got) != tools.RefusalInvalid || !strings.Contains(got.Output, "code_get_change answered") {
		t.Errorf("a seen on another log answered %v %+v, want invalid with this tool's hint", err, got)
	}
}
