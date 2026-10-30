package builtin_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// ---- the fixtures ------------------------------------------------------------ //

// standingFinding is a finding of site!7 that stands; reviewedID and the
// raised ids are what the fake change writer answers a review with.
const (
	standingFinding = "0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d10"
	reviewedID      = "0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d20"
)

// The blobs site!7's revisions change: a.txt modified, r3.txt added at
// revision 3.
var (
	aOld  = strings.Repeat("1", 39) + "a"
	aNew  = strings.Repeat("2", 39) + "a"
	r3New = strings.Repeat("3", 39) + "a"
)

// diffOf answers a revision's diff with files — those of them its paths name
// — and every other read by object id as the host does.
func diffOf(files ...code.DiffFile) func(q code.GitQuery) code.GitAnswer {
	return func(q code.GitQuery) code.GitAnswer {
		out := code.GitAnswer{Kind: q.Kind, RepoID: q.RepoID, Position: codeAt(12)}
		if q.Kind == code.GitDiff {
			out.Diff = &code.DiffAnswer{Base: q.Diff.Base, Head: q.Diff.Head, Files: []code.DiffFile{}}
			for _, f := range files {
				if slices.Contains(q.Diff.Paths, f.Path) {
					out.Diff.Files = append(out.Diff.Files, f)
				}
			}
		}
		return out
	}
}

// siteChangeFiles is the files every revision of site!7 changes.
func siteChangeFiles() []code.DiffFile {
	return []code.DiffFile{{Path: "a.txt", Status: "M", OldOID: aOld, NewOID: aNew},
		{Path: "r3.txt", Status: "A", NewOID: r3New}}
}

// reviewHost is site!7's code host answering its diffs with siteChangeFiles.
func reviewHost() *codeHost {
	host := newChangeHost(siteChange())
	host.git = diffOf(siteChangeFiles()...)
	return host
}

// reviewArgsOf is a review of site!7 requesting changes: a blocker with a
// suggestion on a.txt's new lines, a minor on its old ones, a nit on the file
// revision 3 adds, and one finding of the reviewer's own carried.
func reviewArgsOf() map[string]any {
	return map[string]any{"change": "site!7", "verdict": "request_changes", "summary": "Two problems.",
		"findings": []any{
			map[string]any{"path": "a.txt", "line_start": 3, "severity": "blocker", "category": "correctness",
				"title": "Retries forever", "body": "Nothing bounds it.", "suggestion": "retry(3)\n"},
			map[string]any{"path": "a.txt", "side": "old", "line_start": 2, "line_end": 4, "severity": "minor",
				"category": "style", "title": "Was clearer"},
			map[string]any{"path": "r3.txt", "line_start": 1, "severity": "nit", "category": "docs",
				"title": "Typo"},
		},
		"carries": []any{standingFinding}}
}

// ---- code_review_change ----------------------------------------------------- //

// A REVIEW IS ONE RECORD: its verdict, its summary, every finding it raises
// and every finding of the reviewer's own it carries go to the change's writer
// as ONE write, of the revision it judges — the head by default — each
// finding anchored where its lines were read: the revision's head on the new
// side and its recorded base on the old, its blob from ONE read of the
// revision's diff kept to the files the findings name, at the floor of the
// record that moved the revision's head. A seat's review names its turn and
// the model its call was made on; the answer names the review and its findings
// in the order raised.
//
// Mutations: a write per finding → three writes, red; an old-side finding
// anchored in the head → red; the diff read with no floor → red.
func TestReviewChangeIsOneRecord(t *testing.T) {
	t.Parallel()
	host := reviewHost()
	writer := &codeWrites{converse: func(w code.ConversationWrite) (code.ConversationResult, error) {
		return code.ConversationResult{Outcome: statelog.OutcomeApplied, ID: reviewedID,
			FindingIDs: []string{"f-1", "f-2", "f-3"}, Position: codeAt(41)}, nil
	}}
	turn := writerTurn(t)
	got := callWrite(t, toolloop.WithCallModel(everyGrant(), "claude-house"),
		codeRegistry(t, writeDeps(host, writer)), turn, builtin.CodeReviewChangeTool, reviewArgsOf())
	_, converses := writer.changesAsked()
	if got.Failed || len(converses) != 1 || converses[0].Review == nil {
		t.Fatalf("the review answered %q, sending %d writes", got.Output, len(converses))
	}
	w := converses[0]
	r := w.Review
	if r.ChangeID != siteChangeID || r.RepoID != siteID || r.Revision != 3 || r.Verdict != code.VerdictRequestChanges ||
		r.Summary != "Two problems." || !slices.Equal(r.Carries, []string{standingFinding}) ||
		r.TurnID != turn.RunID || r.Model != "claude-house" || w.Seen != codeAt(40) {
		t.Errorf("the review is %+v at %s, want site!7's revision 3 as the call named it, at the call's read",
			r, w.Seen)
	}
	want := []struct {
		side         code.Side
		commit, blob string
		start, end   int
	}{
		{code.SideNew, revHead(3), aNew, 3, 3}, {code.SideOld, revBase(3), aOld, 2, 4},
		{code.SideNew, revHead(3), r3New, 1, 1},
	}
	if len(r.Findings) != len(want) {
		t.Fatalf("the review carries %d findings, want %d", len(r.Findings), len(want))
	}
	for i, f := range r.Findings {
		if f.Side != want[i].side || f.CommitOID != want[i].commit || f.BlobOID != want[i].blob ||
			f.LineStart != want[i].start || f.LineEnd != want[i].end {
			t.Errorf("finding %d is %+v, want it on side %s of %s, blob %s, lines %d to %d", i, f, want[i].side,
				want[i].commit, want[i].blob, want[i].start, want[i].end)
		}
	}
	if s := r.Findings[0].Suggestion; s == nil || *s != "retry(3)\n" {
		t.Errorf("the blocker's suggestion is %v", s)
	}
	_, _, gits := host.asked()
	if len(gits) != 1 || gits[0].Kind != code.GitDiff || gits[0].Diff.Base != revBase(3) ||
		gits[0].Diff.Head != revHead(3) || !slices.Equal(gits[0].Diff.Paths, []string{"a.txt", "r3.txt"}) ||
		gits[0].Fresh.Floor(codeAt(1).Stream) != codeAt(23) {
		t.Errorf("the review read %+v, want ONE diff of revision 3 kept to its findings' files at the floor of "+
			"its head's record (%s)", gits, codeAt(23))
	}
	answer := decoded(t, got)
	if answer["id"] != reviewedID || answer["change"] != "site!7" {
		t.Errorf("answered %q, want the review's id", got.Output)
	}
	if ids, _ := answer["findings"].([]any); len(ids) != 3 || ids[0] != "f-1" || ids[2] != "f-3" {
		t.Errorf("answered findings %v, want the three in the order raised", answer["findings"])
	}
}

// A REVIEW NAMES THE FINDINGS IT CARRIES, and the revision it judges: the
// carried ids are sent as the call named them, for the change's writer to hold
// to the reviewer's own standing findings; an earlier revision is judged where
// the call names one, its findings anchored in that revision's own head and
// base; and a revision past the change's head, or a finding on a file the
// revision does not change, is refused with nothing written.
//
// Mutations: the revision taken as the head whatever the call names → red; a
// finding on an unchanged file sent → red.
func TestAReviewNamesTheFindingsItCarries(t *testing.T) {
	t.Parallel()
	host, writer := reviewHost(), &codeWrites{}
	reg := codeRegistry(t, writeDeps(host, writer))
	args := reviewArgsOf()
	args["revision"] = 2
	args["findings"] = []any{map[string]any{"path": "a.txt", "line_start": 3, "severity": "major",
		"category": "tests", "title": "Untested"}}
	if got := callWrite(t, everyGrant(), reg, writerTurn(t), builtin.CodeReviewChangeTool, args); got.Failed {
		t.Fatalf("the review of revision 2 answered %q", got.Output)
	}
	_, converses := writer.changesAsked()
	if len(converses) != 1 || converses[0].Review.Revision != 2 ||
		!slices.Equal(converses[0].Review.Carries, []string{standingFinding}) ||
		converses[0].Review.Findings[0].CommitOID != revHead(2) {
		t.Errorf("the review sent %+v, want revision 2 carrying %s, its finding in revision 2's head", converses,
			standingFinding)
	}
	for name, change := range map[string]map[string]any{
		"a revision past the head": {"revision": 4},
		"a file it does not change": {"findings": []any{map[string]any{"path": "b.txt", "line_start": 1,
			"severity": "nit", "category": "style", "title": "x"}}},
	} {
		host, writer := reviewHost(), &codeWrites{}
		args := reviewArgsOf()
		for k, v := range change {
			args[k] = v
		}
		got := callWrite(t, everyGrant(), codeRegistry(t, writeDeps(host, writer)), writerTurn(t),
			builtin.CodeReviewChangeTool, args)
		if _, sent := writer.changesAsked(); !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid ||
			len(sent) != 0 {
			t.Errorf("%s answered %q as %s, sending %d writes; want an invalid call and none", name, got.Output,
				tools.RefusalOf(got), len(sent))
		}
	}
}

// EVERY CONVERSATION REFUSAL IS IN AN EXISTING CLASS: a rule of the change
// refusing the caller forbidden, a revision the node has not reached stale, a
// write only a live change takes conflict, what is gone not found, an ask past
// its bounds invalid, and what waiting clears unavailable — each with the
// decide's own sentence — and superseded, the home's own refusal, which no
// caller is ever answered, the engine's fault.
//
// Mutation: drop not_raiser from conversationClass → red.
func TestEveryConversationRefusalIsInAnExistingClass(t *testing.T) {
	t.Parallel()
	want := map[code.ConversationRefusal]tools.Refusal{
		code.ConversationRefusalNotPermitted:    tools.RefusalForbidden,
		code.ConversationRefusalNotRaiser:       tools.RefusalForbidden,
		code.ConversationRefusalNotAuthor:       tools.RefusalForbidden,
		code.ConversationRefusalUnknownRevision: tools.RefusalStaleVersion,
		code.ConversationRefusalChangeClosed:    tools.RefusalConflict,
		code.ConversationRefusalFindingGone:     tools.RefusalNotFound,
		code.ConversationRefusalThreadGone:      tools.RefusalNotFound,
		code.ConversationRefusalNoReviewer:      tools.RefusalInvalid,
		code.ConversationRefusalBusy:            tools.RefusalUnavailable,
		code.ConversationRefusalSuperseded:      tools.RefusalInternalError,
	}
	for _, refusal := range code.ConversationRefusals() {
		writer := &codeWrites{converse: func(code.ConversationWrite) (code.ConversationResult, error) {
			return code.ConversationResult{Refusal: refusal, Detail: "the " + string(refusal) + " sentence"}, nil
		}}
		got := callWrite(t, everyGrant(), codeRegistry(t, writeDeps(newChangeHost(siteChange()), writer)),
			writerTurn(t), builtin.CodeCommentOnChangeTool, map[string]any{"change": "site!7", "body": "Hm."})
		class := tools.RefusalOf(got)
		switch {
		case !got.Failed || class == "":
			t.Errorf("a write refused %s answered %q as %q", refusal, got.Output, class)
		case refusal != code.ConversationRefusalSuperseded && class == tools.RefusalInternalError:
			t.Errorf("a write refused %s is answered as the engine's own fault", refusal)
		case want[refusal] != "" && class != want[refusal]:
			t.Errorf("a write refused %s is answered %s, want %s", refusal, class, want[refusal])
		case !strings.Contains(got.Output, "the "+string(refusal)+" sentence"):
			t.Errorf("a write refused %s answered %q without its sentence", refusal, got.Output)
		}
	}
}

// ---- code_comment_on_change and the rest --------------------------------------- //

// A COMMENT SETS ITS THREAD AS A SECOND WRITE: a reply that also marks its
// thread resolved is the reply, then the thread's state as a step of the
// reply's operation decided at or past the reply's record; a thread marked
// with nothing said is that one write; a mark of a finding's thread is refused
// — a finding is answered by its own answer — and an anchored comment names
// the line, the revision and the blob it was read in.
//
// Mutations: the thread marked before the reply lands → red; the mark under a
// fresh operation → red.
func TestACommentSetsItsThreadAsASecondWrite(t *testing.T) {
	t.Parallel()
	const thread = "0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d30"
	writer := &codeWrites{}
	reg := codeRegistry(t, writeDeps(reviewHost(), writer))
	got := callWrite(t, everyGrant(), reg, writerTurn(t), builtin.CodeCommentOnChangeTool,
		map[string]any{"change": "site!7", "body": "Done, thanks.", "thread": thread, "resolve": true})
	_, converses := writer.changesAsked()
	if got.Failed || len(converses) != 2 || converses[0].Comment == nil || converses[1].ThreadResolve == nil {
		t.Fatalf("the reply answered %q, sending %+v", got.Output, converses)
	}
	reply, mark := converses[0], converses[1]
	if reply.Comment.ParentID != thread || mark.ThreadResolve.ThreadID != thread || !mark.ThreadResolve.Resolved ||
		!strings.HasPrefix(mark.OpID, reply.OpID+".") || mark.Seen != codeAt(16) {
		t.Errorf("the reply %+v and its mark %+v under %s and %s at %s, want the mark a step of the reply decided "+
			"at its record (%s)", reply.Comment, mark.ThreadResolve, reply.OpID, mark.OpID, mark.Seen, codeAt(16))
	}
	if answer := decoded(t, got); answer["thread_resolved"] != true {
		t.Errorf("answered %q, want the thread resolved", got.Output)
	}

	alone := &codeWrites{}
	callWrite(t, everyGrant(), codeRegistry(t, writeDeps(reviewHost(), alone)), writerTurn(t),
		builtin.CodeCommentOnChangeTool, map[string]any{"change": "site!7", "thread": thread, "resolve": false})
	if _, sent := alone.changesAsked(); len(sent) != 1 || sent[0].ThreadResolve == nil || sent[0].ThreadResolve.Resolved {
		t.Errorf("a thread opened again with nothing said sent %+v, want the one mark", sent)
	}

	refused := callWrite(t, everyGrant(), codeRegistry(t, writeDeps(reviewHost(), &codeWrites{})), writerTurn(t),
		builtin.CodeCommentOnChangeTool, map[string]any{"change": "site!7", "finding": standingFinding,
			"resolve": true})
	if !refused.Failed || tools.RefusalOf(refused) != tools.RefusalInvalid ||
		!strings.Contains(refused.Output, builtin.CodeResolveFindingTool) {
		t.Errorf("a finding's thread marked answered %q, want it sent to %s", refused.Output,
			builtin.CodeResolveFindingTool)
	}

	anchoredWriter := &codeWrites{}
	callWrite(t, everyGrant(), codeRegistry(t, writeDeps(reviewHost(), anchoredWriter)), writerTurn(t),
		builtin.CodeCommentOnChangeTool, map[string]any{"change": "site!7", "body": "Why here?",
			"anchor": map[string]any{"path": "a.txt", "line": 7}})
	_, sent := anchoredWriter.changesAsked()
	if len(sent) != 1 || sent[0].Comment == nil || sent[0].Comment.Anchor == nil ||
		*sent[0].Comment.Anchor != (code.CommentAnchor{Revision: 3, Path: "a.txt", Side: code.SideNew, Line: 7,
			BlobOID: aNew}) {
		t.Errorf("an anchored comment sent %+v, want line 7 of a.txt's new side at revision 3", sent)
	}
}

// AN ANSWER TO FINDINGS NAMES EVERY FINDING ONCE, each with the answer, its
// note and the commit the call names — a ref resolved to the commit it points
// at — in ONE write; and an ask for review is one write naming whom it asks
// and whom it withdraws.
//
// Mutation: the commit sent as the ref the call named → red.
func TestAnAnswerToFindingsNamesEveryFindingOnce(t *testing.T) {
	t.Parallel()
	const other = "0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d11"
	writer := &codeWrites{}
	reg := codeRegistry(t, writeDeps(reviewHost(), writer))
	got := callWrite(t, everyGrant(), reg, writerTurn(t), builtin.CodeResolveFindingTool,
		map[string]any{"change": "site!7", "findings": []any{standingFinding, other}, "resolution": "resolved",
			"note": "Bounded now.", "commit": "main"})
	_, converses := writer.changesAsked()
	if got.Failed || len(converses) != 1 || converses[0].FindingResolve == nil {
		t.Fatalf("the answer answered %q, sending %+v", got.Output, converses)
	}
	for i, id := range []string{standingFinding, other} {
		if f := converses[0].FindingResolve.Findings[i]; f != (code.FindingResolution{FindingID: id,
			Resolution: code.FindingResolved, Note: "Bounded now.", Commit: mainOID}) {
			t.Errorf("finding %d is answered %+v", i, f)
		}
	}

	asking := &codeWrites{}
	callWrite(t, everyGrant(), codeRegistry(t, writeDeps(reviewHost(), asking)), writerTurn(t),
		builtin.CodeRequestReviewTool, map[string]any{"change": "site!7", "reviewers": []any{"founder"},
			"withdraw": []any{"agent-ceo"}, "note": "Please look at the retry."})
	_, asks := asking.changesAsked()
	if len(asks) != 1 || asks[0].Reviewers == nil || len(asks[0].Reviewers.Request) != 1 ||
		asks[0].Reviewers.Request[0].Handle != "founder" || !slices.Equal(asks[0].Reviewers.Withdraw,
		[]string{"agent-ceo"}) || asks[0].Reviewers.Note != "Please look at the retry." {
		t.Errorf("the ask sent %+v", asks)
	}
}
