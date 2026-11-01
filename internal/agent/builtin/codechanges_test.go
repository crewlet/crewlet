package builtin_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// ---- a change the cases read --------------------------------------------- //

// siteChangeID is change site!7's id.
const siteChangeID = "0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d07"

// revHead is revision n's head, and revBase the base its facts recorded —
// each a whole id of its own, n from 1 to 9.
func revHead(n int64) string { return strings.Repeat(strconv.FormatInt(n, 10), 40) }
func revBase(n int64) string { return strings.Repeat(string(rune('a'+n)), 40) }

// changeFixture is the change site!7 as the code host's rows hold it — its
// detail, its revisions oldest first, its findings, reviews and comments in
// the order a page reads them — and a listing of the repository's changes,
// newest first; read answers a change read as the shipped reader pages one,
// each page resuming after the row its cursor names.
type changeFixture struct {
	repo      string
	detail    code.ChangeDetail
	revisions []code.RevisionRow
	findings  []code.FindingRow
	reviews   []code.ReviewRow
	comments  []code.CommentRow
	listing   []code.ChangeSummary
	at        statelog.Position
}

// siteChange is site!7: three revisions of feature into main, each with its
// facts — the third the head — opened by agent-ceo, its rows written up to
// codeAt(40).
func siteChange() *changeFixture {
	f := &changeFixture{repo: "site", at: codeAt(40)}
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	for n := int64(1); n <= 3; n++ {
		f.revisions = append(f.revisions, code.RevisionRow{N: n, Head: revHead(n), Pusher: "agent-ceo",
			PusherKind: code.PartyAgent, Position: codeAt(20 + uint64(n)), At: at.Add(time.Duration(n) * time.Hour),
			Facts: &code.RevisionFactsRow{TargetRef: "refs/heads/main", TargetOID: mainOID,
				TargetPosition: codeAt(10 + uint64(n)), BaseOID: revBase(n), PatchID: revBase(n),
				Touched: []string{"a.txt", fmt.Sprintf("r%d.txt", n)}, Node: "n1", Position: codeAt(30 + uint64(n))}})
	}
	row := code.ChangeRow{ID: siteChangeID, RepoID: siteID, Number: 7, Title: "Retry the upload",
		State: code.ChangeStateOpen, SourceRef: "refs/heads/feature", TargetRef: "refs/heads/main", Head: revHead(3),
		Revision: 3, Owner: "agent-ceo", OwnerKind: code.PartyAgent, OpenedAt: at, OpenedPosition: codeAt(21),
		Version: 1, Position: codeAt(23)}
	f.detail = code.ChangeDetail{
		ChangeSummary: code.ChangeSummary{ChangeRow: row, Repo: "site", Ref: "site!7", Access: "read",
			Readiness: &code.Readiness{Met: 3, Total: 5, First: "no approval counts on revision 3",
				Preconditions: []code.Precondition{{Kind: code.PreconditionApproval, State: code.PreconditionUnmet,
					Detail: "no approval counts on revision 3"}}}},
		Body:    "Retries the upload once a minute.",
		Authors: []code.ChangeAuthor{{Author: "agent-ceo", Kind: code.PartyAgent, FirstRevision: 1}},
		Conversation: &code.ConversationRow{Comments: 1, Reviews: 1, OpenFindings: 2, OpenBlockers: 1,
			Version: 3, Position: codeAt(39)},
		Mergeability: &code.MergeabilityRow{Revision: 3, TargetRef: "refs/heads/main", TargetOID: mainOID,
			State: code.MergeConflict, Conflicts: []string{"a.txt"}, At: at},
	}
	f.listing = []code.ChangeSummary{f.detail.ChangeSummary}
	return f
}

// newChangeHost is the code host holding site and its change site!7.
func newChangeHost(f *changeFixture) *codeHost {
	h := newCodeHost()
	h.changes = f.read
	return h
}

// read answers q as the shipped reader pages a change read: a listing of the
// repository's changes newest first by number, a page after the number its
// cursor names; and one change's revisions — with its detail on a first page
// — newest first after the revision a cursor names, or a part of its
// conversation after the row a cursor names.
func (f *changeFixture) read(q code.ReadQuery) (code.ReadAnswer, error) {
	out := code.ReadAnswer{Position: f.at, Level: q.Fresh.Level}
	if q.Repo != "" && q.Repo != f.repo {
		return code.ReadAnswer{}, fmt.Errorf("%w: %s", code.ErrRepoNotFound, q.Repo)
	}
	limit := q.Limit
	if q.Kind == code.ReadChanges {
		after := int64(math.MaxInt64)
		if q.Cursor != "" {
			n, err := strconv.ParseInt(q.Cursor, 10, 64)
			if err != nil {
				return code.ReadAnswer{}, fmt.Errorf("%w: %q", code.ErrBadCursor, q.Cursor)
			}
			after = n
		}
		for _, s := range f.listing {
			if s.Number < after && len(out.Changes) < limit {
				out.Changes = append(out.Changes, s)
			}
		}
		if len(out.Changes) == limit && out.Changes[limit-1].Number != f.listing[len(f.listing)-1].Number {
			out.Next = q.ChangesCursor(out.Changes[limit-1])
		}
		return out, nil
	}
	if q.Change != f.detail.Number {
		return code.ReadAnswer{}, fmt.Errorf("%w: %s", code.ErrChangeNotFound, code.ChangeRef(f.repo, q.Change))
	}
	switch q.Part {
	case code.PartFindings:
		out.Findings, out.Next = pageAfter(f.findings, code.FindingRow.Cursor, q.Cursor, limit)
	case code.PartReviews:
		out.Reviews, out.Next = pageAfter(f.reviews, code.ReviewRow.Cursor, q.Cursor, limit)
	case code.PartComments:
		out.Comments, out.Next = pageAfter(f.comments, code.CommentRow.Cursor, q.Cursor, limit)
	default:
		after := int64(math.MaxInt64)
		if q.Cursor == "" {
			d := f.detail
			out.Change = &d
		} else {
			n, err := strconv.ParseInt(q.Cursor, 10, 64)
			if err != nil {
				return code.ReadAnswer{}, fmt.Errorf("%w: %q", code.ErrBadCursor, q.Cursor)
			}
			after = n
		}
		for i := len(f.revisions) - 1; i >= 0; i-- {
			if r := f.revisions[i]; r.N < after && len(out.Revisions) < limit {
				out.Revisions = append(out.Revisions, r)
			}
		}
		if last := len(out.Revisions); last == limit && out.Revisions[last-1].N > 1 {
			out.Next = out.Revisions[last-1].Cursor()
		}
	}
	return out, nil
}

// pageAfter is a page of rows read in their order: at most limit, after the
// row whose own cursor is after, and the cursor of the page that follows.
func pageAfter[T any](rows []T, cursor func(T) string, after string, limit int) ([]T, string) {
	start := 0
	if after != "" {
		start = slices.IndexFunc(rows, func(r T) bool { return cursor(r) == after }) + 1
	}
	end := min(start+limit, len(rows))
	if end < len(rows) {
		return rows[start:end], cursor(rows[end-1])
	}
	return rows[start:end], ""
}

// changeCall calls one of a seat's change tools in turn, and decodes its
// answer.
func changeCall(t *testing.T, host *codeHost, turn *turnctx.Turn, name string,
	args map[string]any) (tools.Result, map[string]any) {

	t.Helper()
	return callCode(t, codeRegistry(t, codeDeps(host)), turn, name, args)
}

// ---- one list ------------------------------------------------------------- //

// THE CHANGE TOOLS JOIN THE ONE LIST: code_list_changes and code_get_change
// are code reads like the nine before them — on the one list a seat's registry
// and the operator catalogue both take, right after the reads by object id
// (code_blame the last of them), described and shaped the same on both; each a
// proven, idempotent read a worker may be handed, that delivers nowhere; and
// offered by neither surface of a company that hosts no code.
//
// Mutations: CodeTools leaving either change tool out, or listing it before
// the reads by object id; a change tool annotated as a write → red.
func TestTheChangeToolsJoinTheOneList(t *testing.T) {
	t.Parallel()
	host := newChangeHost(siteChange())
	change, names := builtin.ChangeToolNames(), builtin.CodeToolNames()
	if blame := slices.Index(names, builtin.CodeBlameTool); blame < 0 || len(names) < blame+1+len(change) ||
		!slices.Equal(names[blame+1:blame+1+len(change)], change) {
		t.Fatalf("the code reads are %v, want the change tools %v right after the reads by object id", names,
			change)
	}
	var listed []string
	for _, tool := range builtin.CodeTools(codeDeps(host)) {
		listed = append(listed, tool.Name())
	}
	if !slices.Equal(listed, builtin.CodeToolNames()) {
		t.Errorf("CodeTools lists %v, want %v", listed, builtin.CodeToolNames())
	}
	reg := codeRegistry(t, codeDeps(host))
	operator := operatorCode(t, host, nil)
	for _, name := range builtin.ChangeToolNames() {
		entry, ok := reg.Lookup(name)
		if !ok {
			t.Errorf("a seat's registry holds no %s", name)
			continue
		}
		tool, ok := operator[name]
		if !ok {
			t.Errorf("the operator catalogue holds no %s", name)
			continue
		}
		seatSchema, _ := json.Marshal(entry.Tool.Parameters())
		opSchema, _ := json.Marshal(tool.Parameters())
		if string(seatSchema) != string(opSchema) || entry.Tool.Description() != tool.Description() {
			t.Errorf("%s is described or shaped differently on the two surfaces", name)
		}
		if !mcp.ReadOnlyProven(entry.Annotations) || entry.Annotations.Idempotent != mcp.Yes ||
			mcp.WritesToSharedSurface(entry.Annotations) || entry.Surface != "" {
			t.Errorf("%s is registered %+v delivering to %q, want a proven, idempotent read that delivers nowhere",
				name, entry.Annotations, entry.Surface)
		}
		if _, known := authz.ClassOf(authz.Action(name)); !known {
			t.Errorf("%s has no row in the authority table", name)
		}
	}
	for _, name := range builtin.ChangeToolNames() {
		if _, ok := codeRegistry(t, builtin.CodeDeps{}).Lookup(name); ok {
			t.Errorf("a company hosting no code was given %s", name)
		}
		if _, ok := operatorCode(t, nil, nil)[name]; ok {
			t.Errorf("an operator of a company hosting no code was offered %s", name)
		}
	}
}

// ---- whom a call reads for ------------------------------------------------- //

// everyChangeQuestion makes, in turn, a call of each change tool and each part
// of a change: every question they ask the code host.
func everyChangeQuestion(t *testing.T, host *codeHost, turn *turnctx.Turn) {
	t.Helper()
	reg := codeRegistry(t, codeDeps(host))
	callCode(t, reg, turn, builtin.CodeListChangesTool, map[string]any{"repo": "site"})
	for _, args := range []map[string]any{
		{"part": "summary"}, {"part": "merge_readiness"}, {"part": "files"}, {"part": "files", "since_revision": 1},
		{"part": "diff", "file": "a.txt"}, {"part": "findings", "revision": 2}, {"part": "reviews"},
		{"part": "comments"},
	} {
		args["change"] = "site!7"
		callCode(t, reg, turn, builtin.CodeGetChangeTool, args)
	}
}

// changeQuestionsFor holds every question host was asked to name ceiling as
// the seat it is made for — "" for none.
func changeQuestionsFor(t *testing.T, host *codeHost, ceiling string) {
	t.Helper()
	reads, _, gits := host.asked()
	if readsOf(reads, code.ReadChange) == 0 || readsOf(reads, code.ReadChanges) == 0 || len(gits) == 0 {
		t.Fatalf("the calls asked %d change reads, %d listings and %d git reads, want each kind",
			readsOf(reads, code.ReadChange), readsOf(reads, code.ReadChanges), len(gits))
	}
	for _, q := range reads {
		if q.CeilingSeat != ceiling {
			t.Errorf("a %s read (%s) was made for %q, want %q", q.Kind, q.Part, q.CeilingSeat, ceiling)
		}
	}
	for _, q := range gits {
		if q.CeilingSeat != ceiling {
			t.Errorf("a %s read by object id was made for %q, want %q", q.Kind, q.CeilingSeat, ceiling)
		}
	}
}

// A CHANGE READ IS MADE FOR WHOEVER WOKE THE TURN, at the access the change
// tools' table names — read — which the node that answers judges the
// colleague at on every question: in a turn a colleague seat woke, every read
// of a change's rows and every read by object id a change tool makes names
// that seat; in one a person woke, none does, a person maintaining every
// repository; and in one a stranger woke, nothing is read at all.
//
// Mutations: a change tool asking a question through anything but
// CodeDeps.read and gitRead (the colleague left off it), or caller skipped →
// red.
func TestAChangeReadIsMadeForWhoeverWokeTheTurn(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		requester string
		ceiling   string
	}{
		"a turn nobody woke":           {"", ""},
		"a turn a person woke":         {"founder", ""},
		"a turn a colleague seat woke": {"agent-ceo", "agent-ceo"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			host := newChangeHost(siteChange())
			turn := codeTurn(t)
			turn.Requester = c.requester
			everyChangeQuestion(t, host, turn)
			changeQuestionsFor(t, host, c.ceiling)
		})
	}
	t.Run("a turn a stranger woke", func(t *testing.T) {
		t.Parallel()
		host := newChangeHost(siteChange())
		turn := codeTurn(t)
		turn.Senders = []types.CanonicalIdentity{{ExternalID: "U-out", Platform: "slack"}}
		reg := codeRegistry(t, codeDeps(host))
		for _, call := range []struct {
			tool string
			args map[string]any
		}{
			{builtin.CodeListChangesTool, map[string]any{"needs": "me"}},
			{builtin.CodeGetChangeTool, map[string]any{"change": "site!7"}},
		} {
			got := refusedCode(t, reg, turn, call.tool, call.args)
			var refused *builtin.DecisionRefused
			if tools.RefusalOf(got) != tools.RefusalForbidden || !errors.As(got.Cause, &refused) ||
				refused.Decision.Reason != authz.ReasonCeiling {
				t.Errorf("%s in a stranger's turn answered %s %q, want refused with the ceiling's reason",
					call.tool, tools.RefusalOf(got), got.Output)
			}
		}
		if reads, resolves, gits := host.asked(); len(reads)+len(resolves)+len(gits) != 0 {
			t.Errorf("a stranger's turn asked the code host %d questions", len(reads)+len(resolves)+len(gits))
		}
	})
}

// ---- the listing ------------------------------------------------------------ //

// A LISTING IS READ AS ITS ARGUMENTS SAY: its filters are the reader's own —
// a repository by the name the turn resolved, a state, an owner, a work item,
// a reviewer, a need — at the seat's level, a page of `limit`; one that names
// nothing to list is refused before anything is read, as are a state and a
// need this build does not know and a limit past its bound.
//
// Mutation: a listing naming nothing sent to the reader; the repository read
// by the name as given rather than the turn's resolution → red.
func TestAListingIsReadAsItsArgumentsSay(t *testing.T) {
	t.Parallel()
	host := newChangeHost(siteChange())
	_, answer := changeCall(t, host, codeTurn(t), builtin.CodeListChangesTool, map[string]any{"repo": "site",
		"state": "open", "owner": "agent-ceo", "work": "native:task-1", "reviewer": "agent-cto", "needs": "review",
		"limit": 5})
	reads, _, _ := host.asked()
	var listing *code.ReadQuery
	for i := range reads {
		if reads[i].Kind == code.ReadChanges {
			listing = &reads[i]
		}
	}
	if listing == nil {
		t.Fatal("no listing was read")
	}
	if listing.Repo != "site" || listing.State != code.ChangeStateOpen || listing.Owner != "agent-ceo" ||
		listing.Work != "native:task-1" || listing.Reviewer != "agent-cto" || listing.Needs != code.NeedReview ||
		listing.Limit != 5 || listing.Fresh.Level != statelog.ReadLinearizable {
		t.Errorf("the listing was read as %+v", *listing)
	}
	if readsOf(reads, code.ReadRepo) != 1 {
		t.Errorf("the repository was resolved %d times, want once", readsOf(reads, code.ReadRepo))
	}
	changes, _ := answer["changes"].([]any)
	row, _ := changes[0].(map[string]any)
	if len(changes) != 1 || row["ref"] != "site!7" || row["version"] != float64(1) ||
		row["readiness"].(map[string]any)["first"] != "no approval counts on revision 3" {
		t.Errorf("the listing answered %v", answer)
	}
	if _, ok := row["id"]; ok {
		t.Errorf("a listed change carries its id: %v", row)
	}

	for name, args := range map[string]map[string]any{
		"nothing to list":       {"owner": "agent-ceo"},
		"a state of none":       {"state": "pending"},
		"a need of none":        {"needs": "lunch"},
		"a page past its bound": {"needs": "me", "limit": 51},
	} {
		host := newChangeHost(siteChange())
		got := refusedCode(t, codeRegistry(t, codeDeps(host)), codeTurn(t), builtin.CodeListChangesTool, args)
		if tools.RefusalOf(got) != tools.RefusalInvalid {
			t.Errorf("%s answered %s %q", name, tools.RefusalOf(got), got.Output)
		}
		if reads, _, _ := host.asked(); len(reads) != 0 {
			t.Errorf("%s asked the code host %d reads", name, len(reads))
		}
	}
}

// ---- one change ------------------------------------------------------------ //

// A CHANGE IS NAMED BY ITS NUMBER OR ITS REFERENCE: `change` is the number on
// `repo`, or the reference a listing names (site!7), which names the
// repository too; a reference naming another repository than `repo`, a
// number below one and a part's argument another part does not take are each
// refused before anything is read.
//
// Mutation: `repo` required beside a reference; an argument of another part
// ignored → red.
func TestAChangeIsNamedByItsNumberOrItsReference(t *testing.T) {
	t.Parallel()
	for name, args := range map[string]map[string]any{
		"a reference":           {"change": "site!7"},
		"a number on its repo":  {"repo": "site", "change": 7},
		"a number as a string":  {"repo": "site", "change": "7"},
		"a reference and repo":  {"repo": "site", "change": "site!7"},
		"a number as JSON says": {"repo": "site", "change": json.Number("7")},
	} {
		host := newChangeHost(siteChange())
		_, answer := changeCall(t, host, codeTurn(t), builtin.CodeGetChangeTool, args)
		if answer["repo"] != "site" {
			t.Errorf("%s answered %v", name, answer)
		}
	}
	for name, c := range map[string]struct {
		args map[string]any
		says string
	}{
		"another repository's reference": {map[string]any{"repo": "web", "change": "site!7"}, "name one repository"},
		"no change":                      {map[string]any{"repo": "site"}, "needs `change`"},
		"a number below one":             {map[string]any{"repo": "site", "change": 0}, "needs `change`"},
		"a number and no repository":     {map[string]any{"change": 7}, "needs a `repo`"},
		"a part of none":                 {map[string]any{"change": "site!7", "part": "everything"}, "`part` is one of"},
		"a revision of the summary":      {map[string]any{"change": "site!7", "revision": 2}, "`revision` chooses"},
		"since a revision of findings": {map[string]any{"change": "site!7", "part": "findings", "since_revision": 1},
			"`since_revision` reads"},
		"since a later revision": {map[string]any{"change": "site!7", "part": "files", "revision": 2,
			"since_revision": 2}, "a revision before `revision`"},
		"a file of the files":     {map[string]any{"change": "site!7", "part": "files", "file": "a.txt"}, "`file` names"},
		"a diff of no file":       {map[string]any{"change": "site!7", "part": "diff"}, "name it with `file`"},
		"a page of the readiness": {map[string]any{"change": "site!7", "part": "merge_readiness", "cursor": "2"}, "no next page"},
	} {
		host := newChangeHost(siteChange())
		got := refusedCode(t, codeRegistry(t, codeDeps(host)), codeTurn(t), builtin.CodeGetChangeTool, c.args)
		if tools.RefusalOf(got) != tools.RefusalInvalid || !strings.Contains(got.Output, c.says) {
			t.Errorf("%s answered %s %q, want refused saying %q", name, tools.RefusalOf(got), got.Output, c.says)
		}
		if reads, _, gits := host.asked(); len(reads)+len(gits) != 0 {
			t.Errorf("%s asked the code host %d questions", name, len(reads)+len(gits))
		}
	}
	host := newChangeHost(siteChange())
	got := refusedCode(t, codeRegistry(t, codeDeps(host)), codeTurn(t), builtin.CodeGetChangeTool,
		map[string]any{"change": "site!8"})
	if tools.RefusalOf(got) != tools.RefusalNotFound || !strings.Contains(got.Output, "has no change site!8") ||
		!strings.Contains(got.Output, builtin.CodeListChangesTool) {
		t.Errorf("a change the repository does not hold answered %s %q", tools.RefusalOf(got), got.Output)
	}
}

// A REVISION'S FILES ARE READ FROM ITS RECORDED BASE: `files` and `diff` read
// the git node's diff from the revision's base to its head — the head
// revision's, or the one `revision` names, read in a second read at session
// floored at the call's first — and with `since_revision` the interdiff of
// the two, from each revision's facts; every one at the floor of the records
// that moved the revisions' heads and their targets, never at a read's
// checkpoint. A revision past the head is refused; one whose base is not yet
// recorded is unavailable on a live change and not found on one that ended,
// naming how else to read it; one that shares no history with its target
// names its head's files.
//
// Mutations: the diff from the target's tip rather than the base; the
// interdiff built from one revision's facts; the floor at the first read's
// checkpoint; the second read at the seat's level → red.
func TestARevisionsFilesAreReadFromItsRecordedBase(t *testing.T) {
	t.Parallel()
	f := siteChange()
	host := newChangeHost(f)
	turn := codeTurn(t)
	changeCall(t, host, turn, builtin.CodeGetChangeTool, map[string]any{"change": "site!7", "part": "files"})
	changeCall(t, host, turn, builtin.CodeGetChangeTool, map[string]any{"change": "site!7", "part": "diff",
		"file": "a.txt", "revision": 2})
	changeCall(t, host, turn, builtin.CodeGetChangeTool, map[string]any{"change": "site!7", "part": "files",
		"since_revision": 1})
	reads, _, gits := host.asked()
	if len(gits) != 3 {
		t.Fatalf("the three calls asked %d git reads, want 3", len(gits))
	}
	floor := func(q code.GitQuery) statelog.Position { return q.Fresh.Floor(f.at.Stream) }
	if q := gits[0]; q.Kind != code.GitDiff || q.Diff.Base != revBase(3) || q.Diff.Head != revHead(3) ||
		q.Diff.File != "" || floor(q) != codeAt(23) || q.RepoID != siteID {
		t.Errorf("the head's files were read as %+v at %v, want its base to its head at its push", q.Diff, floor(q))
	}
	if q := gits[1]; q.Kind != code.GitDiff || q.Diff.Base != revBase(2) || q.Diff.Head != revHead(2) ||
		q.Diff.File != "a.txt" || floor(q) != codeAt(22) {
		t.Errorf("revision 2's patch was read as %+v at %v", q.Diff, floor(q))
	}
	if q := gits[2]; q.Kind != code.GitInterdiff || q.Interdiff.FromBase != revBase(1) ||
		q.Interdiff.FromHead != revHead(1) || q.Interdiff.ToBase != revBase(3) || q.Interdiff.ToHead != revHead(3) ||
		!slices.Equal(q.Interdiff.FromTouched, []string{"a.txt", "r1.txt"}) ||
		!slices.Equal(q.Interdiff.ToTouched, []string{"a.txt", "r3.txt"}) || floor(q) != codeAt(23) {
		t.Errorf("what changed since revision 1 was read as %+v at %v", q.Interdiff, floor(q))
	}
	var second *code.ReadQuery
	for i := range reads {
		if reads[i].Kind == code.ReadChange && reads[i].Cursor == "3" {
			second = &reads[i]
		}
	}
	if second == nil || second.Limit != 1 || second.Fresh.Level != statelog.ReadSession ||
		second.Fresh.Floor(f.at.Stream) != f.at {
		t.Errorf("revision 2 was read as %+v, want the page after revision 3 at session past the first read", second)
	}

	for name, c := range map[string]struct {
		change func(f *changeFixture)
		args   map[string]any
		want   tools.Refusal
		says   string
	}{
		"a revision past the head": {nil, map[string]any{"part": "files", "revision": 4}, tools.RefusalInvalid,
			"has 3 revisions"},
		"a live change's revision with no facts yet": {func(f *changeFixture) { f.revisions[2].Facts = nil },
			map[string]any{"part": "files"}, tools.RefusalUnavailable, "Try again shortly"},
		"a merged change's revision with no facts": {func(f *changeFixture) {
			f.revisions[2].Facts, f.detail.State = nil, code.ChangeStateMerged
		}, map[string]any{"part": "diff", "file": "a.txt"}, tools.RefusalNotFound, builtin.CodeLogTool},
		"a revision sharing no history with its target": {func(f *changeFixture) { f.revisions[2].Facts.BaseOID = "" },
			map[string]any{"part": "files"}, tools.RefusalNotFound, "shares no history"},
	} {
		f := siteChange()
		if c.change != nil {
			c.change(f)
		}
		host := newChangeHost(f)
		c.args["change"] = "site!7"
		got := refusedCode(t, codeRegistry(t, codeDeps(host)), codeTurn(t), builtin.CodeGetChangeTool, c.args)
		if tools.RefusalOf(got) != c.want || !strings.Contains(got.Output, c.says) {
			t.Errorf("%s answered %s %q, want refused %s saying %q", name, tools.RefusalOf(got), got.Output, c.want,
				c.says)
		}
		if _, _, gits := host.asked(); len(gits) != 0 {
			t.Errorf("%s asked the git node %d reads", name, len(gits))
		}
	}

	// AND AN INTERDIFF READS NO BASE IT DOES NOT HAVE: a revision sharing no
	// history with its target is compared by its touched paths all the same.
	f = siteChange()
	f.revisions[0].Facts.BaseOID = ""
	host = newChangeHost(f)
	changeCall(t, host, codeTurn(t), builtin.CodeGetChangeTool, map[string]any{"change": "site!7", "part": "files",
		"since_revision": 1})
	if _, _, gits := host.asked(); len(gits) != 1 || gits[0].Interdiff.FromBase != "" {
		t.Errorf("an interdiff from a revision of no base was read as %+v", gits)
	}
}

// FINDINGS ARE RE-ANCHORED TO THE ASKED REVISION: a page of findings is read
// at the seat's level, the change and its head after it at session, and every
// finding raised on another revision than the one asked about — the head by
// default — is placed in it by ONE re-anchoring read naming that revision's
// head and base and each finding's own lines, floored at the head revision's
// push, the latest any finding was raised on; a finding raised on the asked
// revision is where it was raised, with nothing asked; and one on the old side
// of a revision that shares no history with its target is gone. Each finding's
// `at` is where its lines are, outdated or gone as the git node placed them.
//
// Mutations: every finding placed where it was raised (no re-anchoring); the
// re-anchoring read at the head whatever revision was asked about; the floor
// at the asked revision alone → red.
func TestFindingsAreReanchoredToTheAskedRevision(t *testing.T) {
	t.Parallel()
	raised := func(id string, revision int64, side code.Side, start, end int) code.FindingRow {
		return code.FindingRow{ID: id, ReviewID: "rv-" + id, Raiser: "agent-cto", RaiserKind: code.PartyAgent,
			Revision: revision, CommitOID: revHead(revision), Path: "a.txt", BlobOID: blobOID, Side: side,
			LineStart: start, LineEnd: end, Severity: code.SeverityMajor, Category: code.CategoryCorrectness,
			Title: "Off by one", Body: "The loop stops a line early.", State: code.FindingOpen}
	}
	ids := []string{"0192f6c1-0000-7000-8000-000000000001", "0192f6c1-0000-7000-8000-000000000002",
		"0192f6c1-0000-7000-8000-000000000003", "0192f6c1-0000-7000-8000-000000000004"}
	fixture := func() *changeFixture {
		f := siteChange()
		f.findings = []code.FindingRow{
			raised(ids[0], 1, code.SideNew, 10, 12),
			raised(ids[1], 2, code.SideNew, 20, 20),
			raised(ids[2], 3, code.SideNew, 30, 31),
			raised(ids[3], 1, code.SideOld, 5, 5),
		}
		return f
	}
	placedBy := func(host *codeHost) func(q code.GitQuery) code.GitAnswer {
		return func(q code.GitQuery) code.GitAnswer {
			out := code.GitAnswer{Kind: code.GitReanchor, RepoID: q.RepoID, Reanchor: &code.ReanchorAnswer{}}
			for i, a := range q.Reanchor.Anchors {
				out.Reanchor.Anchors = append(out.Reanchor.Anchors, code.Reanchored{ID: a.ID, Start: a.Start + 100,
					End: a.End + 100, Outdated: i == 0})
			}
			return out
		}
	}
	at := func(answer map[string]any) map[string]any {
		out := map[string]any{}
		for _, row := range answer["findings"].([]any) {
			f := row.(map[string]any)
			out[f["id"].(string)] = f["at"]
		}
		return out
	}

	t.Run("at the head", func(t *testing.T) {
		t.Parallel()
		host := newChangeHost(fixture())
		host.git = placedBy(host)
		_, answer := changeCall(t, host, codeTurn(t), builtin.CodeGetChangeTool, map[string]any{
			"change": "site!7", "part": "findings"})
		_, _, gits := host.asked()
		if len(gits) != 1 {
			t.Fatalf("the findings asked %d git reads, want one re-anchoring", len(gits))
		}
		q := gits[0]
		var anchored []string
		for _, a := range q.Reanchor.Anchors {
			anchored = append(anchored, a.ID)
			if a.Path != "a.txt" || a.BlobOID != blobOID {
				t.Errorf("anchor %s names %s in %s", a.ID, a.Path, a.BlobOID)
			}
		}
		if q.Kind != code.GitReanchor || q.Reanchor.Head != revHead(3) || q.Reanchor.Base != revBase(3) ||
			!slices.Equal(anchored, []string{ids[0], ids[1], ids[3]}) || q.Fresh.Floor(codeAt(1).Stream) != codeAt(23) {
			t.Errorf("the findings were placed by %+v (%v) at %v, want revision 3's head and base, the three raised "+
				"elsewhere, at the head's push", q.Reanchor, anchored, q.Fresh)
		}
		placed := at(answer)
		if answer["revision"] != float64(3) {
			t.Errorf("the answer is of revision %v, want 3", answer["revision"])
		}
		if got := placed[ids[2]].(map[string]any); got["start"] != float64(30) || got["end"] != float64(31) {
			t.Errorf("the finding raised on the head is at %v, want where it was raised", got)
		}
		if got := placed[ids[0]].(map[string]any); got["start"] != float64(110) || got["outdated"] != true {
			t.Errorf("the finding raised on revision 1 is at %v, want where the git node placed it", got)
		}
	})

	t.Run("at an earlier revision", func(t *testing.T) {
		t.Parallel()
		host := newChangeHost(fixture())
		host.git = placedBy(host)
		_, answer := changeCall(t, host, codeTurn(t), builtin.CodeGetChangeTool, map[string]any{
			"change": "site!7", "part": "findings", "revision": 2})
		_, _, gits := host.asked()
		if len(gits) != 1 {
			t.Fatalf("the findings asked %d git reads, want one re-anchoring", len(gits))
		}
		q := gits[0]
		var anchored []string
		for _, a := range q.Reanchor.Anchors {
			anchored = append(anchored, a.ID)
		}
		if q.Reanchor.Head != revHead(2) || q.Reanchor.Base != revBase(2) ||
			!slices.Equal(anchored, []string{ids[0], ids[2], ids[3]}) || q.Fresh.Floor(codeAt(1).Stream) != codeAt(23) {
			t.Errorf("the findings were placed in revision 2 by %+v (%v) at %v, want its head and base, floored at "+
				"the head revision's push", q.Reanchor, anchored, q.Fresh)
		}
		if got := at(answer)[ids[1]].(map[string]any); got["start"] != float64(20) {
			t.Errorf("the finding raised on revision 2 is at %v, want where it was raised", got)
		}
	})

	t.Run("in a revision sharing no history with its target", func(t *testing.T) {
		t.Parallel()
		f := fixture()
		f.revisions[2].Facts.BaseOID = ""
		host := newChangeHost(f)
		host.git = placedBy(host)
		_, answer := changeCall(t, host, codeTurn(t), builtin.CodeGetChangeTool, map[string]any{
			"change": "site!7", "part": "findings"})
		if got := at(answer)[ids[3]].(map[string]any); got["gone"] != true {
			t.Errorf("a finding on the old side of a revision of no base is at %v, want gone", got)
		}
		if _, _, gits := host.asked(); len(gits) != 1 || len(gits[0].Reanchor.Anchors) != 2 ||
			gits[0].Reanchor.Base != "" {
			t.Errorf("a revision of no base was asked %+v", gits)
		}
	})

	t.Run("all raised on the asked revision", func(t *testing.T) {
		t.Parallel()
		f := siteChange()
		f.findings = []code.FindingRow{raised(ids[2], 3, code.SideNew, 30, 31)}
		host := newChangeHost(f)
		changeCall(t, host, codeTurn(t), builtin.CodeGetChangeTool, map[string]any{"change": "site!7",
			"part": "findings"})
		if _, _, gits := host.asked(); len(gits) != 0 {
			t.Errorf("findings all raised on the head asked %d git reads", len(gits))
		}
	})
}

// ---- one page, one answer ------------------------------------------------- //

// A GET_CHANGE PAGE FITS ONE TOOL ANSWER: every part of a change at its
// bound — a description at its longest beside a page of revisions whose
// target branches are the longest a ref may be, in the characters JSON writes
// six bytes for; a page of findings with their longest bodies and
// suggestions; reviews with their longest summaries; comments with their
// longest bodies; a git node's page cut at its bound; a readiness with a
// merge of many long conflicting paths — is answered within one tool answer,
// a page cut whole-row where its rows outgrow it, and the pages that follow
// read every row once, in order. A row that alone outgrows an answer is
// refused, naming that no argument narrows it; and so is the detail of a
// change whose description alone does.
//
// Mutations: a part answered whole (refused for its size); next left as the
// reader's after a cut (the rows between lost); the fences registered for
// every row of every probe (the render refuses a cut page) → red.
func TestAGetChangePageFitsOneToolAnswer(t *testing.T) {
	t.Parallel()
	prose := func(n int) string {
		const line = "The upload retries once a minute, five times, and then gives up. "
		return strings.Repeat(line, n/len(line)+1)[:n]
	}
	wide := strings.Repeat("<", code.MaxRefNameBytes-len("refs/heads/"))
	f := siteChange()
	f.detail.Body = prose(code.MaxChangeBodyBytes)
	f.revisions = nil
	for n := int64(1); n <= 60; n++ {
		f.revisions = append(f.revisions, code.RevisionRow{N: n, Head: revHead(1 + n%9), Pusher: "agent-ceo",
			PusherKind: code.PartyAgent, Position: codeAt(uint64(n)), Facts: &code.RevisionFactsRow{
				TargetRef: "refs/heads/" + wide, TargetOID: mainOID, BaseOID: revBase(1), Touched: []string{"a"}}})
	}
	f.detail.Revision, f.detail.Head = 60, revHead(1+60%9)
	suggestion := prose(code.MaxSuggestionBytes)
	for i := range 40 {
		f.findings = append(f.findings, code.FindingRow{ID: fmt.Sprintf("0192f6c1-0000-7000-8000-%012d", i),
			Raiser: "agent-cto", RaiserKind: code.PartyAgent, Revision: 60, CommitOID: mainOID, Path: "a.txt",
			BlobOID: blobOID, Side: code.SideNew, LineStart: 1, LineEnd: 2, Severity: code.SeverityMinor,
			Category: code.CategoryStyle, Title: prose(code.MaxFindingTitleBytes), Body: prose(code.MaxFindingBodyBytes),
			Suggestion: &suggestion, State: code.FindingOpen})
		f.reviews = append(f.reviews, code.ReviewRow{ID: fmt.Sprintf("rv-%d", i), Revision: 60,
			Reviewer: "agent-cto", ReviewerKind: code.PartyAgent, Verdict: code.VerdictComment,
			Summary: prose(code.MaxReviewSummaryBytes), Position: codeAt(uint64(100 + i))})
		f.comments = append(f.comments, code.CommentRow{ID: fmt.Sprintf("cm-%d", i), ThreadID: fmt.Sprintf("cm-%d", i),
			Author: "agent-cto", AuthorKind: code.PartyAgent, Body: prose(code.MaxCommentBytes),
			Position: codeAt(uint64(200 + i))})
	}
	conflicts := make([]string, 100)
	for i := range conflicts {
		conflicts[i] = fmt.Sprintf("%03d/%s", i, strings.Repeat("<", 600))
	}
	f.detail.Mergeability.Conflicts = conflicts
	for _, c := range []struct {
		part  string
		list  string
		count int
	}{
		{"summary", "revisions", 60}, {"findings", "findings", 40}, {"reviews", "reviews", 40},
		{"comments", "comments", 40},
	} {
		t.Run(c.part, func(t *testing.T) {
			t.Parallel()
			host := newChangeHost(f)
			var read []string
			pages := 0
			for cursor := ""; ; pages++ {
				got, answer := changeCall(t, host, codeTurn(t), builtin.CodeGetChangeTool, map[string]any{
					"change": "site!7", "part": c.part, "cursor": cursor})
				if len(got.Output) > builtin.ToolAnswerBytes {
					t.Fatalf("page %d is %d bytes, past %d", pages, len(got.Output), builtin.ToolAnswerBytes)
				}
				for _, row := range answer[c.list].([]any) {
					item := row.(map[string]any)
					key := fmt.Sprint(item["id"])
					if c.list == "revisions" {
						key = fmt.Sprint(item["n"])
					}
					read = append(read, key)
				}
				next, _ := answer["next"].(string)
				if next == "" {
					break
				}
				cursor = next
			}
			if len(read) != c.count || len(slices.Compact(slices.Clone(read))) != c.count {
				t.Errorf("the %s pages read %d rows (%v), want each of %d once", c.part, len(read), read, c.count)
			}
			if pages < 2 {
				t.Errorf("the %s came in %d pages, so no page was cut", c.part, pages+1)
			}
		})
	}

	t.Run("merge_readiness", func(t *testing.T) {
		t.Parallel()
		got, answer := changeCall(t, newChangeHost(f), codeTurn(t), builtin.CodeGetChangeTool, map[string]any{
			"change": "site!7", "part": "merge_readiness"})
		m := answer["mergeability"].(map[string]any)
		named, _ := m["conflicts"].([]any)
		if len(got.Output) > builtin.ToolAnswerBytes || len(named) == 0 || len(named) == 100 ||
			m["conflicts_omitted"] != float64(100-len(named)) {
			t.Errorf("the readiness named %d of 100 conflicts (%v omitted) in %d bytes", len(named),
				m["conflicts_omitted"], len(got.Output))
		}
	})

	// A GIT NODE'S PAGE, cut at its bound, wrapped as the tool wraps it: the
	// longest repository name and the longest change number, the patch's
	// fence and the answer's envelope.
	t.Run("a git node's page", func(t *testing.T) {
		t.Parallel()
		name := strings.Repeat("r", gitref.MaxRepoNameBytes)
		g := siteChange()
		g.repo, g.detail.Number = name, math.MaxInt64
		host := newChangeHost(g)
		host.detail.Name = name
		host.git = func(q code.GitQuery) code.GitAnswer {
			return atTheBound(t, code.GitAnswer{Kind: code.GitInterdiff, RepoID: q.RepoID},
				func(a code.GitAnswer, n int) code.GitAnswer {
					a.Diff = &code.DiffAnswer{Base: mainOID, Head: mainOID,
						Files: []code.DiffFile{{Path: "f", Status: "M", Added: 1, Deleted: 1}}, Total: math.MaxInt,
						Added: math.MaxInt, Deleted: math.MaxInt, Patch: strings.Repeat("x", n)}
					return a
				})
		}
		got, _ := changeCall(t, host, codeTurn(t), builtin.CodeGetChangeTool, map[string]any{
			"change": fmt.Sprintf("%s!%d", name, int64(math.MaxInt64)), "part": "diff", "file": "f",
			"since_revision": 1})
		if len(got.Output) > builtin.ToolAnswerBytes {
			t.Fatalf("the answer is %d bytes, past %d", len(got.Output), builtin.ToolAnswerBytes)
		}
		t.Logf("a git node's page at its widest: %d bytes of %d", len(got.Output), builtin.ToolAnswerBytes)
	})

	t.Run("a row past one answer", func(t *testing.T) {
		t.Parallel()
		g := siteChange()
		huge := strings.Repeat("<", code.MaxCommentBytes)
		g.comments = []code.CommentRow{{ID: "cm-1", ThreadID: "cm-1", Author: "agent-cto",
			AuthorKind: code.PartyAgent, Body: huge}}
		g.detail.Body = strings.Repeat("<", code.MaxChangeBodyBytes)
		for part, says := range map[string]string{"comments": "the next comment of site!7 alone is past one answer",
			"summary": "description and authors alone are past one answer"} {
			got := refusedCode(t, codeRegistry(t, codeDeps(newChangeHost(g))), codeTurn(t), builtin.CodeGetChangeTool,
				map[string]any{"change": "site!7", "part": part})
			if !strings.Contains(got.Output, says) {
				t.Errorf("%s past one answer answered %q, want it to say %q", part, got.Output, says)
			}
		}
	})
}
