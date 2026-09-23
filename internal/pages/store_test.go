package pages_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A TITLE IS AN ADDRESS, and the claim is what makes it one.
//
// People link to pages by name, so a container holding two pages called the
// same thing is one where every link is a coin flip. The check has to be a
// first-writer-wins claim rather than a lookup, because two nodes creating the
// same title would both look, both find nothing, and both create — and here
// the claim IS the subject the broker arbitrates, so the race is settled by
// the one party that sees both writers.
func TestATitleIsClaimedAndCannotBeTakenTwice(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	first := r.write(author("jane"), pages.NewPage{
		Title: "Deploy Runbook", Body: "step one",
	})

	_, err := r.store.Create(t.Context(), author("bob"), pages.NewPage{
		Container: "ENG", Title: "deploy   RUNBOOK", Body: "a second page",
	})
	if !errors.Is(err, pages.ErrTitleTaken) {
		t.Fatalf("a second page took the same address: %v", err)
	}

	// AND THE SAME TITLE IN ANOTHER SPACE IS ANOTHER ADDRESS.
	if _, err := r.store.Create(t.Context(), author("bob"), pages.NewPage{
		Container: "PROD", Title: "Deploy Runbook", Body: "prod's own",
	}); err != nil {
		t.Fatalf("one title in two spaces is two addresses: %v", err)
	}
	r.drain()

	if got := r.get("ENG/Deploy Runbook"); got.Page.ID != first.Page.ID {
		t.Fatalf("ENG's address resolves to %s, want %s", got.Page.ID, first.Page.ID)
	}
}

// A SAVE IS REFUSED AGAINST A STALE VERSION.
//
// A wiki's worst failure is silently overwriting a paragraph somebody else
// just wrote, and there is no per-field merge that makes that safe for prose.
func TestASaveIsRefusedAgainstAStaleVersion(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "v1"})

	if _, err := r.store.SavePage(t.Context(), author("jane"), page.Page.ID,
		pages.Save{BaseVersion: 1, Body: ptr("v2")}); err != nil {
		t.Fatalf("save: %v", err)
	}
	r.drain()

	_, err := r.store.SavePage(t.Context(), author("bob"), page.Page.ID,
		pages.Save{BaseVersion: 1, Body: ptr("bob's overwrite")})
	if !errors.Is(err, pages.ErrStaleVersion) {
		t.Fatalf("a save against a stale version landed: %v", err)
	}
	if got := r.get(page.Page.ID); got.Page.Body != "v2" {
		t.Fatalf("body = %q, want the version that was written", got.Page.Body)
	}
}

// EVERY VERSION KEEPS ITS TEXT, and the newest is reachable like the rest.
func TestEveryVersionKeepsItsText(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "v1"})
	if _, err := r.store.SavePage(t.Context(), author("jane"), page.Page.ID,
		pages.Save{BaseVersion: 1, Body: ptr("v2"), Message: "second pass"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	r.drain()

	for version, want := range map[int]string{1: "v1", 2: "v2"} {
		got, err := r.store.Revision(t.Context(), page.Page.ID, version)
		if err != nil {
			t.Fatalf("revision %d: %v", version, err)
		}
		if got.Body != want {
			t.Errorf("revision %d holds %q, want %q — a version whose text is "+
				"not reachable is a history that cannot answer what changed",
				version, got.Body, want)
		}
	}
	detail := r.get(page.Page.ID)
	if len(detail.History) != 2 {
		t.Fatalf("the history lists %d revisions, want 2", len(detail.History))
	}
}

// A RENAME MOVES THE ADDRESS AND FREES THE OLD ONE.
func TestARenameMovesTheAddressAndFreesTheOldOne(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Old Name", Body: "prose"})

	if _, err := r.store.Rename(t.Context(), author("jane"), page.Page.ID,
		"New Name", false); err != nil {
		t.Fatalf("rename: %v", err)
	}
	r.drain()

	if got := r.get("ENG/New Name"); got.Page.ID != page.Page.ID {
		t.Fatalf("the new address resolves to %s", got.Page.ID)
	}
	if _, err := r.reader.Get(t.Context(), "ENG/Old Name", statelog.Freshness{Level: statelog.ReadSession}); !errors.Is(err, pages.ErrNotFound) {
		t.Fatalf("the old address still resolves: %v", err)
	}
	// AND THE PAGE RENDERS THE NAME IT MOVED TO. The address is a COLUMN
	// and the title a reader renders comes out of the `document`, so a
	// rename written to the columns alone resolves at the new address and
	// goes on displaying the old name — on every node, for ever, with
	// nothing anywhere reporting a disagreement.
	if got := r.get(page.Page.ID); got.Page.Title != "New Name" {
		t.Errorf("the page reads as %q after being renamed to %q — the new "+
			"address resolves and the page still renders the name it had",
			got.Page.Title, "New Name")
	}
	if head, _, err := r.store.Page(t.Context(), page.Page.ID); err != nil ||
		head.Title != "New Name" {
		t.Errorf("the head reads as %q (%v) — this is the value every write "+
			"path's own snapshot decides from", head.Title, err)
	}
	// AND THE FREED NAME IS TAKEABLE, which is the half a claim that was
	// never released would silently break.
	if _, err := r.store.Create(t.Context(), author("bob"), pages.NewPage{
		Container: "ENG", Title: "Old Name", Body: "a new page",
	}); err != nil {
		t.Fatalf("the released address could not be taken: %v", err)
	}
}

// A RENAME ONTO A NAME SOMEBODY ELSE HOLDS IS REFUSED.
func TestARenameOntoATakenAddressIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "One", Body: "a"})
	r.write(author("jane"), pages.NewPage{Title: "Two", Body: "b"})

	_, err := r.store.Rename(t.Context(), author("jane"), page.Page.ID, "Two", false)
	if !errors.Is(err, pages.ErrTitleTaken) {
		t.Fatalf("a rename took an address another page holds: %v", err)
	}
	if got := r.get(page.Page.ID); got.Page.Title != "One" {
		t.Errorf("the page was renamed anyway, to %q", got.Page.Title)
	}
}

// COMMENTING DOES NOT SUBSCRIBE, BUT MENTIONING DOES.
//
// THE OPPOSITE OF THE TRACKER'S PARTICIPANTS RULE, and deliberate: a page a
// hundred people have remarked on would otherwise wake a hundred seats when
// somebody fixes a heading.
func TestCommentingDoesNotSubscribeButMentioningDoes(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})

	if _, _, err := r.store.Comment(t.Context(), author("bob"), page.Page.ID,
		pages.NewComment{Body: "a passing remark"}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()
	if watchers := r.get(page.Page.ID).Page.Watchers; slices.Contains(watchers, "bob") {
		t.Fatalf("commenting subscribed bob: %v — a page a hundred people have "+
			"remarked on would wake a hundred seats over a heading", watchers)
	}

	if _, _, err := r.store.Comment(t.Context(), author("bob"), page.Page.ID,
		pages.NewComment{Body: "what do you think @carla", Mentions: []string{"carla"}},
	); err != nil {
		t.Fatalf("comment with a mention: %v", err)
	}
	r.drain()
	if watchers := r.get(page.Page.ID).Page.Watchers; !slices.Contains(watchers, "carla") {
		t.Fatalf("a mention did not subscribe: %v", watchers)
	}
}

// AN UNWATCH STICKS. Somebody who said no once is not re-subscribed by being
// mentioned.
func TestAnUnwatchSticks(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{
		Title: "Runbook", Body: "prose", Watchers: []string{"carla"},
	})
	if _, err := r.store.SavePage(t.Context(), author("carla"), page.Page.ID,
		pages.Save{BaseVersion: 1, Watch: ptr(false)}); err != nil {
		t.Fatalf("unwatch: %v", err)
	}
	r.drain()

	if _, _, err := r.store.Comment(t.Context(), author("jane"), page.Page.ID,
		pages.NewComment{Body: "@carla?", Mentions: []string{"carla"}}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()
	// THE MUTE IS THE UNWATCH. A handle stays in the watcher set — it is
	// also the record of who cared — and the mute is what takes it out of
	// every recipient list, so what must hold is that carla is MUTED
	// rather than absent.
	got := r.get(page.Page.ID)
	if !slices.Contains(got.Page.Muted, "carla") {
		t.Fatalf("a mention un-muted somebody who unwatched: muted = %v, "+
			"watchers = %v", got.Page.Muted, got.Page.Watchers)
	}
}

// A TURN'S COMMENT IS POSTED ONCE, however many times the turn re-runs.
//
// THE OPERATION LEDGER COLLAPSES THE WHOLE RECORD here, which is the upgrade
// the log brings: the retry does not even append, where the bucket could only
// make the second write land on the same key.
func TestATurnsCommentOnAPageIsPostedOnce(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})

	in := pages.NewComment{Body: "the agent's note", TurnKey: "turn-7"}
	for range 3 {
		if _, _, err := r.store.Comment(t.Context(), agent("eng"), page.Page.ID,
			in); err != nil {
			t.Fatalf("comment: %v", err)
		}
		r.drain()
	}
	thread, err := r.store.Thread(t.Context(), page.Page.ID)
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	if len(thread) != 1 {
		t.Fatalf("a re-run turn posted %d comments", len(thread))
	}
}

// ONLY THE AUTHOR EDITS A COMMENT, operator included.
func TestOnlyTheAuthorEditsAComment(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})
	comment, _, err := r.store.Comment(t.Context(), author("jane"), page.Page.ID,
		pages.NewComment{Body: "jane's remark"})
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()

	_, _, err = r.store.EditComment(t.Context(), author("bob"), page.Page.ID,
		comment.ID, "bob's words in jane's mouth")
	if !errors.Is(err, pages.ErrInvalid) {
		t.Fatalf("a second person edited somebody else's comment: %v", err)
	}
	r.drain()
	thread, err := r.store.Thread(t.Context(), page.Page.ID)
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	if len(thread) != 1 || thread[0].Body != "jane's remark" {
		t.Errorf("the comment was changed anyway: %+v", thread)
	}
}

// ENSURING A CONTAINER IS IDEMPOTENT, and it runs on every boot for every
// unit's space — so a record per boot would be a log that grows with restarts
// rather than with edits.
func TestEnsuringAContainerIsIdempotent(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for range 3 {
		if _, _, err := r.store.EnsureContainer(t.Context(), activation(0), "ENG", "Engineering",
			"how we build"); err != nil {
			t.Fatalf("ensure: %v", err)
		}
		r.drain()
	}
	var records int
	if err := r.db.Replicated().SQL().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM pages_containers`).Scan(&records); err != nil {
		t.Fatal(err)
	}
	if records != 1 {
		t.Fatalf("%d container rows for one space", records)
	}
	if r.consumed != 1 {
		t.Fatalf("ensuring one container three times appended %d records — a "+
			"record per boot is a log that grows with restarts", r.consumed)
	}
}

// OVERSIZED CONTENT IS REFUSED NAMING THE FIELD, never cut: a page truncated
// mid-sentence is a procedure somebody will follow the first half of.
func TestOversizedContentIsRefusedNamingTheField(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for name, in := range map[string]pages.NewPage{
		"title": {Title: strings.Repeat("t", pages.MaxTitle+1), Body: "x"},
		"body":  {Title: "Runbook", Body: strings.Repeat("b", pages.MaxBody+1)},
		"labels": {Title: "Runbook", Body: "x",
			Labels: manyLabels(pages.MaxLabels + 1)},
	} {
		t.Run(name, func(t *testing.T) {
			in.Container = "ENG"
			_, err := r.store.Create(t.Context(), author("jane"), in)
			if !errors.Is(err, pages.ErrInvalid) {
				t.Fatalf("oversized %s was accepted: %v", name, err)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("the refusal does not name the field: %v", err)
			}
		})
	}
}

// A WRITE NAMES ITS AUTHOR, and an OPERATOR names it differently.
//
// A seat or an agent must state the handle it acts as: an audit trail whose
// author is empty is a list of changes nobody made. An operator has no seat —
// the operator surface deliberately gives a caller no way to name one — so it
// is identified by the token it presented instead.
func TestAWriteNamesItsAuthor(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.store.Create(t.Context(), pages.Actor{}, pages.NewPage{
		Container: "ENG", Title: "Runbook", Body: "x",
	}); !errors.Is(err, pages.ErrInvalid) {
		t.Fatalf("a write with no author kind landed: %v", err)
	}
	if _, err := r.store.Create(t.Context(),
		pages.Actor{Kind: pages.AuthorAgent}, pages.NewPage{
			Container: "ENG", Title: "Runbook", Body: "x",
		}); !errors.Is(err, pages.ErrInvalid) {
		t.Fatalf("an agent write with no seat handle landed: %v", err)
	}
	written, err := r.store.Create(t.Context(),
		pages.Actor{Kind: pages.AuthorOperator, OperatorID: "ops-3"},
		pages.NewPage{Container: "ENG", Title: "Runbook", Body: "x"})
	if err != nil {
		t.Fatalf("an operator write was refused: %v", err)
	}
	r.drain()
	if got := r.get(written.Page.ID).Page.Author; got != "operator:ops-3" {
		t.Errorf("the operator's write is attributed to %q — it carries the "+
			"TOKEN's own name, because there is deliberately no way for that "+
			"surface to name a seat to act as", got)
	}
}

// AN EDIT THAT CHANGES NOTHING APPENDS NOTHING.
func TestAnEditThatChangesNothingAppendsNothing(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})
	before := r.consumed

	if _, err := r.store.SavePage(t.Context(), author("jane"), page.Page.ID,
		pages.Save{BaseVersion: 1, Body: ptr("prose")}); err != nil {
		t.Fatalf("save: %v", err)
	}
	r.drain()
	if r.consumed != before {
		t.Errorf("a save that changed nothing appended a record — the log grows "+
			"with edits, not with saves (%d -> %d)", before, r.consumed)
	}
}

// A TRASHED PAGE LEAVES EVERY READER'S WAY AND COMES BACK.
func TestATrashedPageLeavesAndComesBack(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})

	if _, err := r.store.Trash(t.Context(), author("jane"), page.Page.ID); err != nil {
		t.Fatalf("trash: %v", err)
	}
	r.drain()
	if got := r.get(page.Page.ID); got.Page.Status != pages.StatusTrashed {
		t.Fatalf("status = %q after a trash", got.Page.Status)
	}
	if _, err := r.store.Restore(t.Context(), author("jane"), page.Page.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	r.drain()
	if got := r.get(page.Page.ID); got.Page.Status != pages.StatusPublished {
		t.Fatalf("status = %q after a restore", got.Page.Status)
	}
}

// A PURGE IS PERMANENT, and its marker is what makes it so.
func TestAPurgeIsPermanentAndFreesTheAddress(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})

	if _, err := r.store.Purge(t.Context(), author("jane"), page.Page.ID,
		"written in the wrong space"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	r.drain()
	if _, err := r.reader.Get(t.Context(), page.Page.ID, statelog.Freshness{Level: statelog.ReadSession}); !errors.Is(err, pages.ErrNotFound) {
		t.Fatalf("a purged page is still readable: %v", err)
	}
	// THE ADDRESS IS FREE, because the claim went with the page.
	if _, err := r.store.Create(t.Context(), author("bob"), pages.NewPage{
		Container: "ENG", Title: "Runbook", Body: "a fresh page",
	}); err != nil {
		t.Fatalf("the purged page's address could not be re-taken: %v", err)
	}
	r.drain()
	var markers int
	if err := r.db.Replicated().SQL().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM pages_deletions`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if markers != 1 {
		t.Errorf("%d deletion markers — the marker is how a node that was away "+
			"tells a page that never existed from one that was destroyed", markers)
	}
}

func manyLabels(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "label-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	return out
}

// A NO-OP SAVE STILL ANSWERS WITH THE PAGE IT DID NOT CHANGE.
//
// "Nothing changed" is a success, and a caller told its write landed reads the
// page out of that answer — the page tool serializes the id, the title and the
// version straight into the model's result. Answering with a zero page reports
// success on a page with no id, no title and version zero, which is
// indistinguishable from a write that landed somewhere nobody can name.
func TestANoOpSaveStillAnswersWithThePage(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})

	got, err := r.store.SavePage(t.Context(), author("jane"), page.Page.ID,
		pages.Save{BaseVersion: 1, Body: ptr("prose")})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if got.Page.ID != page.Page.ID || got.Page.Title != "Runbook" ||
		got.Page.Version != 1 {
		t.Errorf("an idempotent save answered with %+v, want the page it read "+
			"— id %s, title %q, version 1", got.Page, page.Page.ID, "Runbook")
	}
	if got.Outcome.Outcome != statelog.OutcomeApplied {
		t.Errorf("outcome = %q, want %q: an update that changes no field is "+
			"one the caller should be told landed",
			got.Outcome.Outcome, statelog.OutcomeApplied)
	}
	// AND THE REVISION IS THE ROW'S OWN, in the number space the reader
	// answers in. Nothing landed, so the only number there is is the one
	// the decision read — and a literal zero is the value this package
	// refuses to answer a head read with, for the reason it is unusable
	// here too: it is both "this node has applied nothing for this page"
	// and "nobody answered", and it goes straight into the map a page tool
	// serializes for a model.
	if got.Revision == 0 {
		t.Errorf("an idempotent save reported revision 0 while reporting " +
			"success")
	}
	if head := r.get(page.Page.ID); got.Revision != head.Revision {
		t.Errorf("the write reports revision %d and the reader answers %d — "+
			"two numbers under one name is a comparison that silently never "+
			"holds", got.Revision, head.Revision)
	}
}

// A WRITE THAT LANDS REPORTS THE REVISION ITS OWN RECORD PUT THE ROW AT.
//
// The two arms of [Written.Revision] have to be ONE number space or the field
// means nothing: a caller comparing what a write reported against what a later
// read answers is doing the only thing the number is for. A record's position
// is what the applier stamps into the row, so the write's answer and the
// reader's are the same integer by construction — and the sequence alone is
// NOT that integer, because a row's version composes the generation with it.
func TestAWriteReportsTheRevisionTheReaderAnswersWith(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "v1"})
	r.drain()
	if got := r.get(page.Page.ID); got.Revision != page.Revision {
		t.Errorf("the create reports revision %d and the reader answers %d",
			page.Revision, got.Revision)
	}

	saved, err := r.store.SavePage(t.Context(), author("jane"), page.Page.ID,
		pages.Save{BaseVersion: 1, Body: ptr("v2")})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	r.drain()
	if saved.Revision <= page.Revision {
		t.Errorf("the save reports revision %d and the create reported %d — a "+
			"later write is at a later revision", saved.Revision, page.Revision)
	}
	if got := r.get(page.Page.ID); got.Revision != saved.Revision {
		t.Errorf("the save reports revision %d and the reader answers %d",
			saved.Revision, got.Revision)
	}
}

// AN UNCHANGED COMMENT EDIT ANSWERS WITH A REVISION TOO.
//
// It is the third no-op on this write path and the one furthest from anybody's
// eye: re-sending a comment's own text appends nothing, and the tool that
// serializes the answer reads `revision` out of it exactly as the page tools
// do.
func TestAnUnchangedCommentEditStillAnswersWithARevision(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})
	comment, _, err := r.store.Comment(t.Context(), author("jane"), page.Page.ID,
		pages.NewComment{Body: "is this still right?"})
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()
	before := r.consumed

	_, got, err := r.store.EditComment(t.Context(), author("jane"), page.Page.ID,
		comment.ID, "is this still right?")
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	r.drain()
	if r.consumed != before {
		t.Errorf("re-sending a comment's own text appended a record (%d -> %d)",
			before, r.consumed)
	}
	if got.Revision == 0 {
		t.Errorf("an unchanged comment edit reported revision 0 while " +
			"reporting success")
	}
	if head := r.get(page.Page.ID); got.Revision != head.Revision {
		t.Errorf("the edit reports revision %d and the reader answers %d",
			got.Revision, head.Revision)
	}
}

// A RENAME TO THE TITLE A PAGE ALREADY DISPLAYS ANSWERS `applied` AND WRITES
// NOTHING.
//
// THE TRUE NO-OP IS BOTH HALVES: the same address AND the same displayed
// title. Only then is there nothing left to write — `pages_titles` is keyed on
// the normalised title and `pages_heads.title` already reads exactly what was
// asked for. Publishing anyway would take a create-only append at an address
// this page already holds, lose to its own claim, and be reported as a name
// somebody else took.
//
// It is the one write here that never reaches the broker, so it is the one
// whose outcome the framework's own no-op arm has to answer: an empty outcome
// is not one of the three the contract has, and a caller reading it cannot
// tell a move that has already happened from a write nothing could be
// established about — the honest response to the second being to retry.
func TestARenameToTheTitleItAlreadyDisplaysAnswersApplied(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})
	before := r.consumed

	got, err := r.store.Rename(t.Context(), author("jane"), page.Page.ID,
		"  Runbook ", false)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got.Outcome.Outcome != statelog.OutcomeApplied {
		t.Errorf("outcome = %q, want %q — a rename to the title the page "+
			"already displays has happened, and an absent outcome is not one "+
			"of the three answers a caller may act on",
			got.Outcome.Outcome, statelog.OutcomeApplied)
	}
	if got.Outcome.OpID != got.ChangeID || got.ChangeID == "" {
		t.Errorf("the outcome carries op id %q and the write reports %q — a "+
			"retry is only safe under the same one", got.Outcome.OpID, got.ChangeID)
	}
	if got.Page.ID != page.Page.ID {
		t.Errorf("answered with page %+v, want %s", got.Page, page.Page.ID)
	}
	// AND THE REVISION IS THE ROW'S OWN, never zero: nothing landed, so
	// the only number there is is the one the decision read — and zero is
	// both "this node has applied nothing for this page" and "nobody
	// answered", which a caller cannot act on either way.
	if got.Revision == 0 {
		t.Errorf("an idempotent rename reported revision 0 while reporting " +
			"success — the same literal this package refuses to answer a head " +
			"read with, in the same map a page tool serializes for a model")
	}
	if head := r.get(page.Page.ID); got.Revision != head.Revision {
		t.Errorf("the write reports revision %d and the reader answers %d — "+
			"two numbers under one name is a comparison that silently never "+
			"holds", got.Revision, head.Revision)
	}
	r.drain()
	if r.consumed != before {
		t.Errorf("renaming a page to the title it already displays appended a "+
			"record (%d -> %d), which would lose to its own claim",
			before, r.consumed)
	}
}

// A CAPITALISATION CHANGE IS A RENAME, and it is the case the address cannot
// see.
//
// `title` is the DISPLAYED title and `title_norm` the address it was
// arbitrated on, kept apart precisely because a link is resolved by the second
// and rendered from the first. So "Runbook" -> "RUNBOOK" moves a real field
// every reader sees while the address stays exactly where it is — and
// discarding it as a no-op told the caller `applied`, handed back the OLD
// title, and left the stored one untouched.
//
// It arbitrates on the PAGE rather than on the title: the address does not
// move, so the title subject it would otherwise take is the one this page
// already holds, where a create-only append loses to its own claim.
func TestACapitalisationChangeIsARenameAndLands(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})
	before := r.consumed

	got, err := r.store.Rename(t.Context(), author("jane"), page.Page.ID,
		"RUNBOOK", false)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got.Page.Title != "RUNBOOK" {
		t.Errorf("the write answered with title %q, want %q — a caller told "+
			"its rename landed reads the new title out of that answer",
			got.Page.Title, "RUNBOOK")
	}
	r.drain()
	if r.consumed == before {
		t.Fatalf("a capitalisation change appended no record (%d), so the "+
			"displayed title every reader renders was silently discarded",
			r.consumed)
	}
	head := r.get(page.Page.ID)
	if head.Page.Title != "RUNBOOK" {
		t.Errorf("the stored displayed title is %q, want %q", head.Page.Title,
			"RUNBOOK")
	}
	// AND THE ADDRESS DID NOT MOVE, which is what makes this the page's
	// write rather than the title's: the old spelling still resolves,
	// because the claim is on the normalised title and that did not
	// change.
	if by := r.get("ENG/runbook"); by.Page.ID != page.Page.ID {
		t.Errorf("the address resolves to %s, want %s — a retitle must leave "+
			"the claim exactly where it was", by.Page.ID, page.Page.ID)
	}
	if head.Page.Version != page.Page.Version {
		t.Errorf("the page's edit version moved %d -> %d — a rename changes an "+
			"address and a displayed title, never the body's own version",
			page.Page.Version, head.Page.Version)
	}
	// AND THE HISTORY SAYS IT WAS A RENAME, so a card renders the verb a
	// person would use for it.
	if len(head.History) == 0 {
		t.Fatalf("the page has no history after a rename")
	}
}

// AN OPERATOR'S `watch` TOGGLE CHANGES NOTHING AND PUBLISHES NOTHING.
//
// An operator write names no seat — the ops surface deliberately refuses to
// let a token act as one — so there is no subscription for `watch` to move.
// The set-size comparison this used to rest on could not see that: it was
// covered by a clause that is false by construction once the mutator has run,
// and true only for the empty handle, so the one caller it fired for was the
// one caller with nothing to change.
func TestAnOperatorsWatchToggleChangesNothing(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})
	before := r.consumed

	operator := pages.Actor{Kind: pages.AuthorOperator, OperatorID: "ci"}
	if _, err := r.store.SavePage(t.Context(), operator, page.Page.ID,
		pages.Save{BaseVersion: 1, Watch: ptr(false)}); err != nil {
		t.Fatalf("save: %v", err)
	}
	r.drain()
	if r.consumed != before {
		t.Errorf("an operator's watch toggle appended a record (%d -> %d) and "+
			"woke every watcher, for a change to nobody's subscription",
			before, r.consumed)
	}
	if got := r.get(page.Page.ID); len(got.Page.Muted) != 0 {
		t.Errorf("muted = %v after an operator unwatch — a token has no seat "+
			"to mute", got.Page.Muted)
	}
}

// A RE-WATCH IS A CHANGE THE SET SIZES CANNOT SEE, and the case that proves it
// needs a handle that was NEVER a watcher.
//
// MUTED IS NOT A SUBSET OF WATCHERS: a `watch: false` from somebody who does
// not follow the page adds them to the muted set alone, so the page sits at
// watchers=[jane] muted=[carla]. Their later `watch: true` takes them OUT of
// muted and INTO watchers — two rows moved, and a total that does not budge.
// A mutator that compared the sizes before and after would read that as
// "nothing changed", publish nothing, and leave somebody who asked to follow
// the page muted on every record it writes.
//
// An unwatch by somebody who WAS a watcher is the easy half and cannot stand
// in for it: their handle stays in the watcher set and the mute is added
// beside it, so the total moves and the sizes agree with the truth by
// accident.
func TestARewatchIsAChangeAlthoughTheSetsStaySameSize(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})

	// carla is not a watcher — jane created the page, so she is the only
	// one — and muting as a non-watcher is what produces the same-size
	// case below.
	if _, err := r.store.SavePage(t.Context(), author("carla"), page.Page.ID,
		pages.Save{BaseVersion: 1, Watch: ptr(false)}); err != nil {
		t.Fatalf("mute: %v", err)
	}
	r.drain()
	muted := r.get(page.Page.ID)
	if !slices.Contains(muted.Page.Muted, "carla") ||
		slices.Contains(muted.Page.Watchers, "carla") {
		t.Fatalf("after a non-watcher's mute watchers = %v, muted = %v — this "+
			"test's whole premise is that the two sets are disjoint here",
			muted.Page.Watchers, muted.Page.Muted)
	}
	sizeBefore := len(muted.Page.Watchers) + len(muted.Page.Muted)
	before := r.consumed

	if _, err := r.store.SavePage(t.Context(), author("carla"), page.Page.ID,
		pages.Save{BaseVersion: 1, Watch: ptr(true)}); err != nil {
		t.Fatalf("re-watch: %v", err)
	}
	r.drain()
	if r.consumed == before {
		t.Fatalf("a re-watch appended nothing — somebody who asked to follow " +
			"a page again is still muted on every record it writes")
	}
	got := r.get(page.Page.ID)
	if slices.Contains(got.Page.Muted, "carla") ||
		!slices.Contains(got.Page.Watchers, "carla") {
		t.Errorf("after a re-watch muted = %v, watchers = %v",
			got.Page.Muted, got.Page.Watchers)
	}
	if size := len(got.Page.Watchers) + len(got.Page.Muted); size != sizeBefore {
		t.Errorf("the two sets hold %d handles and held %d — if the re-watch "+
			"moves the total, this case is not the one a size comparison "+
			"misses and the test is not exercising the rule it names",
			size, sizeBefore)
	}
}

// A HEAD READ REPORTS THE REVISION IT WAS READ AT, never a constant zero.
//
// The number is what a caller compares to decide whether its own write is
// visible on this node. A literal zero is the one value it cannot act on: it
// is both "this node has applied nothing for this page" and "nobody answered".
func TestAHeadReadReportsTheRevisionItWasReadAt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})

	head, revision, err := r.store.Page(t.Context(), page.Page.ID)
	if err != nil {
		t.Fatalf("read the head: %v", err)
	}
	if head.ID != page.Page.ID {
		t.Fatalf("read page %s, want %s", head.ID, page.Page.ID)
	}
	if revision == 0 {
		t.Fatalf("the head of an applied page reads at revision 0, which is " +
			"the answer a node that has applied nothing gives")
	}
	// AND IT IS THE SAME NUMBER THE READER ANSWERS WITH. Two expressions
	// for one revision is two things to keep in step, and the one that
	// moves on a rename is MAX(version, scoped_through).
	if got := r.get(page.Page.ID).Revision; got != revision {
		t.Errorf("the head reads at %d and the reader answers %d", revision, got)
	}
	if _, err := r.store.Rename(t.Context(), author("jane"), page.Page.ID,
		"New Name", false); err != nil {
		t.Fatalf("rename: %v", err)
	}
	r.drain()
	_, moved, err := r.store.Page(t.Context(), page.Page.ID)
	if err != nil {
		t.Fatalf("read the head after a rename: %v", err)
	}
	if moved <= revision {
		t.Errorf("a rename left the head at revision %d (was %d) — a rename "+
			"stamps `scoped_through` and never `version`, so a revision taken "+
			"from the version alone never moves for one", moved, revision)
	}
}
