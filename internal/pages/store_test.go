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
	if _, err := r.reader.Get(t.Context(), "ENG/Old Name", statelog.ReadSession); !errors.Is(err, pages.ErrNotFound) {
		t.Fatalf("the old address still resolves: %v", err)
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
		if _, err := r.store.EnsureContainer(t.Context(), "ENG", "Engineering",
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
	if _, err := r.reader.Get(t.Context(), page.Page.ID, statelog.ReadSession); !errors.Is(err, pages.ErrNotFound) {
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
