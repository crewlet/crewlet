package pages_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

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

// A REMARK A TURN MAKES AGAIN AFTER A DIFFERENT ONE IS A SECOND COMMENT, and
// its own retry is still one.
//
// A turn's comment id is derived from the turn and the body, which names WHICH
// remark it is and not WHEN: "blocked", "unblocked", "blocked" derived one id
// for the first and the third, and the third was the first's retry — nothing
// posted, while the thread read "unblocked" last. The tool hands the store the
// call's repeat count in the run, and the id carries it.
func TestARemarkMadeAgainAfterAnotherIsASecondComment(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})
	for _, in := range []pages.NewComment{
		{Body: "blocked", TurnKey: "turn-7"},
		{Body: "unblocked", TurnKey: "turn-7", Repeat: 1},
		{Body: "blocked", TurnKey: "turn-7", Repeat: 1},
		// ITS RETRY, which a re-run makes under the same count.
		{Body: "blocked", TurnKey: "turn-7", Repeat: 1},
	} {
		if _, _, err := r.store.Comment(t.Context(), agent("eng"), page.Page.ID,
			in); err != nil {
			t.Fatalf("comment %+v: %v", in, err)
		}
		r.drain()
	}
	thread, err := r.store.Thread(t.Context(), page.Page.ID)
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	var bodies []string
	for _, c := range thread {
		bodies = append(bodies, c.Body)
	}
	if !slices.Equal(bodies, []string{"blocked", "unblocked", "blocked"}) {
		t.Fatalf("the thread reads %q, want the remark made again after "+
			"\"unblocked\" posted once more and its retry collapsed", bodies)
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

// AND ONLY THE AUTHOR OR A MODERATOR REMOVES ONE.
//
// This verb had NO CHECK AT ALL: any caller that could reach it could take
// down any remark on any page, while [pages.Store.EditComment] three functions
// up refused exactly that. The asymmetry was invisible because nothing called
// it yet — the pages routes are its first caller.
func TestANonAuthorCannotRemoveAComment(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})
	comment, _, err := r.store.Comment(t.Context(), author("jane"), page.Page.ID,
		pages.NewComment{Body: "jane's remark"})
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()

	if _, err := r.store.RemoveComment(t.Context(), author("bob"),
		page.Page.ID, comment.ID, pages.CommentAuthority{}); !errors.Is(
		err, pages.ErrInvalid) {

		t.Fatalf("a second person removed somebody else's comment: %v", err)
	}
	r.drain()
	if thread, err := r.store.Thread(t.Context(), page.Page.ID); err != nil {
		t.Fatalf("thread: %v", err)
	} else if len(thread) != 1 {
		t.Fatalf("the comment came down anyway: %+v", thread)
	}

	// AND A MODERATOR DOES, which is the arm the deployment grant opens:
	// somebody has to be able to take down what nobody else can.
	if _, err := r.store.RemoveComment(t.Context(), author("bob"),
		page.Page.ID, comment.ID,
		pages.CommentAuthority{Moderate: true}); err != nil {

		t.Fatalf("a moderator could not remove a comment: %v", err)
	}
	r.drain()
	if thread, err := r.store.Thread(t.Context(), page.Page.ID); err != nil {
		t.Fatalf("thread: %v", err)
	} else if len(thread) != 0 {
		t.Fatalf("the moderator's removal did not land: %+v", thread)
	}

	// A REMARK THAT IS GONE IS NOT FOUND, for a moderator too, and nothing
	// is published: there is no author left to ask the check about and
	// nothing for a record to take down.
	end := r.logEnd()
	if _, err := r.store.RemoveComment(t.Context(), author("bob"),
		page.Page.ID, comment.ID,
		pages.CommentAuthority{Moderate: true}); !errors.Is(err, pages.ErrNotFound) {

		t.Fatalf("removing a remark already gone = %v, want ErrNotFound", err)
	}
	if got := r.logEnd(); got != end {
		t.Fatalf("removing a remark already gone put %d record(s) on the log",
			got-end)
	}

	// AND THE AUTHOR NEEDS NO ANSWER AT ALL, which is what keeps the
	// ordinary case reachable on a node that can decide nothing.
	second, _, err := r.store.Comment(t.Context(), author("jane"),
		page.Page.ID, pages.NewComment{Body: "another"})
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()
	if _, err := r.store.RemoveComment(t.Context(), author("jane"),
		page.Page.ID, second.ID, pages.CommentAuthority{}); err != nil {

		t.Fatalf("jane could not remove her own remark: %v", err)
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

// A WRITE NAMES ITS AUTHOR — every kind, an operator included — and names it
// BARE.
//
// A seat or an agent must state the handle it acts as: an audit trail whose
// author is empty is a list of changes nobody made. An operator has no seat —
// the operator surface deliberately gives a caller no way to name one — so it
// is named by the credential's own login, exactly as the tracker names it.
//
// It used to be named `"operator:" + OperatorID` when it stated no handle,
// which was a second spelling of one party: the audit feed reads this history
// beside the tracker's, where the same operator is the bare login, and showed
// one person as two. So there is no fallback name, and a write with no handle
// is refused whatever its kind.
func TestAWriteNamesItsAuthor(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for name, actor := range map[string]pages.Actor{
		"no author kind":               {},
		"an agent with no seat handle": {Kind: pages.AuthorAgent},
		"an operator named only by its credential": {
			Kind: pages.AuthorOperator, OperatorID: "pat:0b8f6b44",
		},
	} {
		if _, err := r.store.Create(t.Context(), actor, pages.NewPage{
			Container: "ENG", Title: "Runbook", Body: "x",
		}); !errors.Is(err, pages.ErrInvalid) {
			t.Errorf("a write by %s landed: %v", name, err)
		}
	}
	if got := r.logEnd(); got != 0 {
		t.Fatalf("refused writes put %d record(s) on the log", got)
	}
	written, err := r.store.Create(t.Context(), pages.Actor{
		Handle: "token:ops-3", Kind: pages.AuthorOperator, OperatorID: "token:ops-3",
	}, pages.NewPage{Container: "ENG", Title: "Runbook", Body: "x"})
	if err != nil {
		t.Fatalf("an operator write was refused: %v", err)
	}
	r.drain()
	if got := r.get(written.Page.ID).Page.Author; got != "token:ops-3" {
		t.Errorf("the operator's write is attributed to %q, want the "+
			"credential's own login, bare — the name every other row calls it", got)
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
//
// AND THE OPERATOR IS BUILT AS EVERY SURFACE BUILDS ONE, with the credential's
// own login as its handle. The rule was keyed on the handle being empty, which
// this case satisfied and no surface did, so in production an operator's
// unwatch muted its login and published a record, and an operator's create
// subscribed a login no wake can reach.
func TestAnOperatorsWatchToggleChangesNothing(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})
	before := r.consumed

	operator := pages.Actor{Handle: "token:ci", Kind: pages.AuthorOperator,
		OperatorID: "token:ci"}
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

	created := r.write(operator, pages.NewPage{Title: "Release notes", Body: "x",
		Watchers: []string{"jane"}})
	if got := r.get(created.Page.ID).Page.Watchers; !slices.Equal(got, []string{"jane"}) {
		t.Errorf("an operator's create is watched by %v, want only the seat it "+
			"named — a credential's login is no seat for a wake to reach", got)
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

// THE SKILLS CONTAINER REFUSES AN AGENT EVERY WRITE, not only a create.
//
// It used to be one check inside `write_page`, so a seat that could not create
// a page in the tool-skills container could still SAVE over one, RENAME one
// and COMMENT on one — and every surface added later inherited the hole
// silently, because nothing in the domain said the rule existed. The store is
// the one place every write goes through, which is why it is here and why
// [pages.ErrReserved] finally has a producer.
//
// A PERSON IS NOT REFUSED here. Whether a person may write a tool skill is a
// capability, and internal/authz decides it; what the store holds is that a
// page in the skills container is machinery injected into every seat's turn
// and excluded from search and routing, which an AGENT cannot tell from
// anything it holds.
func TestTheReservedRuleFiresOnSaveRenameAndComment(t *testing.T) {
	r := newRoundTrip(t)

	// THE PAGE IS PUT THERE BY A PERSON, which is both the setup and the
	// first half of the rule: the same call an agent is refused.
	page := r.write(author("ana"), pages.NewPage{
		Container: "TS", Title: "Chat conventions", Body: "thread your reply",
	}).Page
	remark, _, err := r.store.Comment(t.Context(), author("ana"), page.ID,
		pages.NewComment{Body: "see also the handbook"})
	if err != nil {
		t.Fatalf("a person could not comment on a skill page: %v", err)
	}
	r.drain()

	for name, write := range map[string]func(pages.Actor) error{
		"create": func(a pages.Actor) error {
			_, err := r.store.Create(t.Context(), a, pages.NewPage{
				Container: "ts", Title: "another", Body: "x",
			})
			return err
		},
		"save": func(a pages.Actor) error {
			_, err := r.store.SavePage(t.Context(), a, page.ID,
				pages.Save{BaseVersion: 1, Body: ptr("rewritten")})
			return err
		},
		"rename": func(a pages.Actor) error {
			_, err := r.store.Rename(t.Context(), a, page.ID,
				"Chat rules", false)
			return err
		},
		"comment": func(a pages.Actor) error {
			_, _, err := r.store.Comment(t.Context(), a, page.ID,
				pages.NewComment{Body: "why?"})
			return err
		},
		"comment edit": func(a pages.Actor) error {
			_, _, err := r.store.EditComment(t.Context(), a, page.ID,
				remark.ID, "rewritten")
			return err
		},
		"comment removal": func(a pages.Actor) error {
			_, err := r.store.RemoveComment(t.Context(), a, page.ID,
				remark.ID, pages.CommentAuthority{Moderate: true})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := write(agent("pm"))
			if !errors.Is(err, pages.ErrReserved) {
				t.Fatalf("an agent's %s into the skills container gave %v, "+
					"want ErrReserved", name, err)
			}
			if !strings.Contains(err.Error(), "TS") ||
				!strings.Contains(err.Error(), "tool skills") {
				t.Errorf("the refusal does not name the container and why: %v", err)
			}
		})
	}

	// AND AN ORDINARY CONTAINER IS UNTOUCHED, or the rule would be a ban
	// on agents writing anything.
	if _, err := r.store.Create(t.Context(), agent("pm"), pages.NewPage{
		Container: "ENG", Title: "Deploy notes", Body: "x",
	}); err != nil {
		t.Fatalf("an agent could not write an ordinary container: %v", err)
	}
}

// THE ORG ROOT REFUSES AN AGENT A NEW PAGE, AND NOTHING ELSE.
//
// The root holds what is true of the whole company, starting with the
// Onboarding page every seat reads first, and a new page at the top of the
// company is a person's decision — which is all `write_page` ever refused
// there. When the rule moved into the store it was folded into the skills
// container's, so an agent could no longer keep a root page current, rename
// it or remark on it, and the refusal told it the page was "excluded from
// knowledge search and from routing" — true of the skills container and false
// of this one, which is searched and routed like any other.
//
// Both halves, and the message: a create is refused naming the root's own
// reason, and every gesture on a page already there lands.
func TestTheOrgRootRefusesAnAgentOnlyANewPage(t *testing.T) {
	r := newRoundTrip(t)

	// THE ONBOARDING PAGE IS A PERSON'S, as the convention has it.
	page := r.write(author("ana"), pages.NewPage{
		Container: "HOME", Title: "Onboarding", Body: "who is here",
	}).Page

	_, err := r.store.Create(t.Context(), agent("pm"), pages.NewPage{
		Container: "home", Title: "Team rituals", Body: "x",
	})
	if !errors.Is(err, pages.ErrReserved) {
		t.Fatalf("an agent's new page in the org root gave %v, want ErrReserved", err)
	}
	if !strings.Contains(err.Error(), "HOME") ||
		!strings.Contains(err.Error(), "organisation's own container") {
		t.Errorf("the refusal does not name the root and why: %v", err)
	}
	if strings.Contains(err.Error(), "excluded from") {
		t.Errorf("the refusal says the root is excluded from search or routing, "+
			"which is the skills container's reason and false of this one: %v", err)
	}

	saved, err := r.store.SavePage(t.Context(), agent("pm"), page.ID,
		pages.Save{BaseVersion: 1, Body: ptr("who is here, and who joined")})
	if err != nil {
		t.Fatalf("an agent could not keep a root page current: %v", err)
	}
	r.drain()
	if _, err := r.store.Rename(t.Context(), agent("pm"), page.ID,
		"Onboarding for everyone", false); err != nil {
		t.Fatalf("an agent could not rename a root page: %v", err)
	}
	r.drain()
	remark, _, err := r.store.Comment(t.Context(), agent("pm"), page.ID,
		pages.NewComment{Body: "the second section is stale"})
	if err != nil {
		t.Fatalf("an agent could not remark on a root page: %v", err)
	}
	r.drain()
	if _, _, err := r.store.EditComment(t.Context(), agent("pm"), page.ID,
		remark.ID, "the third section is stale"); err != nil {
		t.Fatalf("an agent could not correct its own remark on a root page: %v", err)
	}
	r.drain()
	if _, err := r.store.RemoveComment(t.Context(), agent("pm"), page.ID,
		remark.ID, pages.CommentAuthority{}); err != nil {
		t.Fatalf("an agent could not take down its own remark on a root page: %v", err)
	}
	if saved.Page.Version != 2 {
		t.Errorf("the save landed at version %d, want 2", saved.Page.Version)
	}

	// AND A PERSON ADDS A PAGE THERE, which is what the container is for.
	if _, err := r.store.Create(t.Context(), author("ana"), pages.NewPage{
		Container: "HOME", Title: "Company values", Body: "x",
	}); err != nil {
		t.Fatalf("a person could not publish into the org root: %v", err)
	}
}

// A GESTURE RETRIED UNDER THE CALLER'S OWN KEY IS ONE OPERATION.
//
// An HTTP write whose answer was `unknown` can only be retried safely under
// the SAME operation id, because the ledger is what recognises a second
// arrival. Every write here minted a fresh id per call, so a retried comment
// posted twice — and there was no way for a caller with no turn to say "this
// is the same gesture". [pages.Actor.OpKey] is that way.
//
// The control is the same retry with no key, which is two operations: the
// comment posted twice is exactly what the key exists to prevent.
//
// AND THE RETRY IS ANSWERED WITH WHAT THE FIRST ONE WROTE. It is answered from
// the ledger rather than decided, so a remark or a page computed by this call
// would describe a decision nothing published — the comment's author and
// instant are the earlier copy's, and the page it trashed is trashed.
func TestAGestureRetriedUnderOneKeyIsOneOperation(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})

	keyed := author("ana")
	keyed.OpKey = statelog.NewOpID(time.Now(), "")
	var changes []string
	var remarks []pages.Comment
	var trashes []pages.Written
	for range 2 {
		remark, written, err := r.store.Comment(t.Context(), keyed, page.Page.ID,
			pages.NewComment{Body: "step 3 is stale"})
		if err != nil {
			t.Fatalf("comment: %v", err)
		}
		changes = append(changes, written.ChangeID)
		remarks = append(remarks, remark)
		r.drain()
		trashed, err := r.store.Trash(t.Context(), keyed, page.Page.ID)
		if err != nil {
			t.Fatalf("trash: %v", err)
		}
		changes = append(changes, trashed.ChangeID)
		trashes = append(trashes, trashed)
		r.drain()
	}
	if changes[0] != changes[2] || changes[1] != changes[3] {
		t.Errorf("a retry under one key was a new operation: %v", changes)
	}
	thread, err := r.store.Thread(t.Context(), page.Page.ID)
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	if len(thread) != 1 {
		t.Fatalf("a retried comment posted %d times", len(thread))
	}
	if !trashes[1].Outcome.Collapsed {
		t.Fatalf("the premise: the retried trash was answered from the ledger, "+
			"got %+v", trashes[1].Outcome)
	}
	held := thread[0]
	if got := remarks[1]; got.ID != held.ID || got.Author != held.Author ||
		!got.CreatedAt.Equal(held.CreatedAt) || got.Body != held.Body {
		t.Errorf("the retried comment answered %+v, want the remark the first "+
			"run posted %+v", got, held)
	}
	for i, trashed := range trashes {
		if trashed.Page.ID != page.Page.ID || trashed.Page.Title != "Runbook" ||
			trashed.Page.Status != pages.StatusTrashed {
			t.Errorf("trash %d answered the page (%q, %q, %s), want %s, "+
				"Runbook, trashed", i+1, trashed.Page.ID, trashed.Page.Title,
				trashed.Page.Status, page.Page.ID)
		}
	}

	// THE CONTROL: no key, and the retry is a second comment.
	unkeyed := author("bo")
	for range 2 {
		if _, _, err := r.store.Comment(t.Context(), unkeyed, page.Page.ID,
			pages.NewComment{Body: "and step 4"}); err != nil {
			t.Fatalf("comment: %v", err)
		}
		r.drain()
	}
	if thread, _ = r.store.Thread(t.Context(), page.Page.ID); len(thread) != 3 {
		t.Fatalf("the control holds %d comments, want 3 — so this test cannot "+
			"tell a keyed retry from an unkeyed one", len(thread))
	}
}

// A KEYED RETRY OF A WRITE THAT LANDED IS ANSWERED WITH WHAT THE OPERATION
// WROTE, never with what this call would have decided.
//
// The retry an `unknown` asks for is answered from the ledger before any
// decision runs ([statelog.Result.Collapsed]), so a write that reported what
// its decision read reported nothing on exactly that call: a retried save came
// back as a success carrying a page with no id, no title and version zero, and
// a retried create as a page id it had just minted — one no row held — while
// the page the operation did create went unreported.
func TestAKeyedRetryIsAnsweredWithWhatTheOperationWrote(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()

	create := pages.NewPage{Container: "ENG", Title: "Runbook", Body: "prose"}
	creator := keyedAuthor("ana")
	first, err := r.store.Create(t.Context(), creator, create)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	again, err := r.store.Create(t.Context(), creator, create)
	switch {
	case err != nil:
		t.Fatalf("the retried create: %v", err)
	case !again.Outcome.Collapsed:
		t.Fatalf("the premise: the retried create was answered from the "+
			"ledger, got %+v", again.Outcome)
	case again.Page.ID != first.Page.ID || again.ChangeID != first.ChangeID:
		t.Fatalf("the retried create answered page %s under %s, want the "+
			"first attempt's %s under %s", again.Page.ID, again.ChangeID,
			first.Page.ID, first.ChangeID)
	case again.Page.Version != 1 || again.Page.Body != "prose" ||
		again.Revision != first.Revision:
		t.Errorf("the retried create answered %+v at revision %d, want the "+
			"page the first attempt created at %d", again.Page, again.Revision,
			first.Revision)
	}
	if got := r.get(first.Page.ID); got.Page.Title != "Runbook" {
		t.Errorf("the page the create answered with reads %q", got.Page.Title)
	}

	editor := keyedAuthor("ana")
	save := pages.Save{BaseVersion: 1, Body: ptr("revised")}
	saved, err := r.store.SavePage(t.Context(), editor, first.Page.ID, save)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	resaved, err := r.store.SavePage(t.Context(), editor, first.Page.ID, save)
	switch {
	case err != nil:
		t.Fatalf("the retried save — refused on a version its own first "+
			"copy moved: %v", err)
	case !resaved.Outcome.Collapsed:
		t.Fatalf("the premise: the retried save was answered from the ledger, "+
			"got %+v", resaved.Outcome)
	case resaved.Page.ID != first.Page.ID || resaved.Page.Title != "Runbook" ||
		resaved.Page.Version != 2 || resaved.Page.Body != "revised":
		t.Errorf("the retried save answered (%q, %q, v%d, %q), want the page "+
			"its first copy saved", resaved.Page.ID, resaved.Page.Title,
			resaved.Page.Version, resaved.Page.Body)
	case resaved.Revision != saved.Revision:
		t.Errorf("the retried save answered revision %d, want its first "+
			"copy's %d", resaved.Revision, saved.Revision)
	}
}

// A RENAME RETRIED AFTER ITS MOVE LANDED IS ANSWERED, NOT REFUSED.
//
// Which shape a rename is — a move to another address, or a retitle of the one
// it holds — is decided from the row before anything is published, and the
// retry of a move that landed finds the page already at its new address. Both
// shapes derived one id from the key, so the retry went to the page's subject
// under an id the ledger holds on the title's, and was refused as an operation
// reused: the caller that asked for the rename was told it had failed.
func TestARenameRetriedAfterItsMoveLandedIsAnswered(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})

	mover := keyedAuthor("jane")
	moved, err := r.store.Rename(t.Context(), mover, page.Page.ID, "Deploy runbook", false)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	again, err := r.store.Rename(t.Context(), mover, page.Page.ID, "Deploy runbook", false)
	if err != nil {
		t.Fatalf("the retried rename was refused: %v", err)
	}
	if again.Page.ID != page.Page.ID || again.Page.Title != "Deploy runbook" {
		t.Errorf("the retried rename answered (%q, %q), want the page at its "+
			"new address", again.Page.ID, again.Page.Title)
	}
	if again.Revision < moved.Revision {
		t.Errorf("the retried rename answered revision %d, below its first "+
			"copy's %d", again.Revision, moved.Revision)
	}
	if got := r.logEnd(); got != moved.Outcome.Position.Seq {
		t.Errorf("the retried rename put %d record(s) on the log",
			got-moved.Outcome.Position.Seq)
	}

	// AND A RETITLE RETRIED IS THE SAME OPERATION: collapsed, and answered
	// with the title it set.
	retitler := keyedAuthor("jane")
	if _, err := r.store.Rename(t.Context(), retitler, page.Page.ID,
		"DEPLOY RUNBOOK", false); err != nil {
		t.Fatalf("retitle: %v", err)
	}
	retitled, err := r.store.Rename(t.Context(), retitler, page.Page.ID,
		"DEPLOY RUNBOOK", false)
	switch {
	case err != nil:
		t.Fatalf("the retried retitle: %v", err)
	case !retitled.Outcome.Collapsed:
		t.Fatalf("the premise: the retried retitle was answered from the "+
			"ledger, got %+v", retitled.Outcome)
	case retitled.Page.Title != "DEPLOY RUNBOOK":
		t.Errorf("the retried retitle answered the title %q", retitled.Page.Title)
	}
}

// A KEY THAT IS NOT AN OPERATION ID THE ENGINE MINTED IS REFUSED, before
// anything is published.
//
// Every id a write derives from the key carries the key's own instant, and one
// outside the grammar carries none: read as minted at the epoch, every write
// under it would be answered `unknown` without being published, on every
// attempt, from the first time the ledger swept a row.
func TestAKeyOutsideTheOperationGrammarIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	actor := author("ana")
	for _, key := range []string{"request-7", "0b8f6b44-5c0e-4a8e-9d3b-2f1c6a7e9d10"} {
		actor.OpKey = key
		_, err := r.store.Create(t.Context(), actor, pages.NewPage{
			Container: "ENG", Title: "Runbook", Body: "prose",
		})
		if !errors.Is(err, pages.ErrInvalid) {
			t.Errorf("a create under the key %q = %v, want ErrInvalid", key, err)
		}
	}
	if got := r.logEnd(); got != 0 {
		t.Errorf("refused writes put %d record(s) on the log", got)
	}
}

// keyedAuthor is a person acting under an operation key of their own — one
// request's, which is what a retry of that request brings back.
func keyedAuthor(handle string) pages.Actor {
	actor := author(handle)
	actor.OpKey = statelog.NewOpID(time.Now(), "")
	return actor
}
