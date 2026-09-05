package pages_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/pages"
)

type clock struct{ at time.Time }

func (c *clock) now() time.Time { return c.at }

func newStore(t *testing.T) (*pages.Store, coord.Documents, *clock) {
	t.Helper()
	docs := memory.NewFleet()
	c := &clock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	s, err := pages.NewStore(pages.Options{Documents: docs, Now: c.now})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s, docs, c
}

func author(handle string) pages.Actor {
	return pages.Actor{Handle: handle, Kind: pages.AuthorHuman}
}

func agent(handle string) pages.Actor {
	return pages.Actor{Handle: handle, Kind: pages.AuthorAgent, TurnID: "turn-" + handle}
}

func write(t *testing.T, s *pages.Store, actor pages.Actor, in pages.NewPage) pages.Written {
	t.Helper()
	if in.Container == "" {
		in.Container = "ENG"
	}
	if in.Title == "" {
		in.Title = "a page"
	}
	got, err := s.Create(t.Context(), actor, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return got
}

func ptr[T any](v T) *T { return &v }

// A TITLE IS AN ADDRESS. People link to pages by name, so a container holding
// two pages called the same thing is one where every link is a coin flip —
// and the check has to be a first-writer-wins CLAIM rather than a lookup,
// because two nodes creating the same title would both look, both find
// nothing, and both create.
func TestATitleIsClaimedAndCannotBeTakenTwice(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	first := write(t, s, author("jane"), pages.NewPage{Title: "Deploy Runbook"})

	_, err := s.Create(t.Context(), author("eng"), pages.NewPage{
		Container: "ENG", Title: "Deploy Runbook",
	})
	if !errors.Is(err, pages.ErrTitleTaken) {
		t.Fatalf("a second page took the same title: %v", err)
	}
	if !strings.Contains(err.Error(), first.Page.ID) {
		t.Errorf("the refusal does not name the page that holds it: %v", err)
	}

	// NORMALISED, so a title is one address rather than several: somebody
	// linking to "Deploy Runbook" and somebody linking to
	// "deploy  runbook" mean the same page.
	for _, title := range []string{"deploy runbook", "DEPLOY RUNBOOK", "Deploy   Runbook"} {
		if _, err := s.Create(t.Context(), author("eng"), pages.NewPage{
			Container: "ENG", Title: title,
		}); !errors.Is(err, pages.ErrTitleTaken) {
			t.Errorf("%q was accepted beside %q", title, first.Page.Title)
		}
	}

	// A DIFFERENT CONTAINER IS A DIFFERENT ADDRESS SPACE, which is the
	// whole point of containers.
	if _, err := s.Create(t.Context(), author("eng"), pages.NewPage{
		Container: "PROD", Title: "Deploy Runbook",
	}); err != nil {
		t.Errorf("the same title in another container was refused: %v", err)
	}
}

// A CRASH BETWEEN THE CLAIM AND THE PAGE MUST NOT LOCK A TITLE. Without the
// grace rule, a node dying mid-create makes a name unusable until the hourly
// sweep, and the person retrying is told their own half-written page owns it.
func TestAnOrphanedTitleClaimIsSteppedOverPastTheGrace(t *testing.T) {
	t.Parallel()
	s, docs, c := newStore(t)

	// A claim with no page behind it, as a crashed create leaves.
	claim, err := pages.EncodeClaim(pages.TitleClaim{
		V: 1, Container: "ENG", Title: pages.NormalizeTitle("Deploy Runbook"),
		PageID: "never-landed", CreatedAt: c.at,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created, err := docs.CreateDocument(t.Context(), coord.FamilyPages,
		pages.TitleKey("ENG", "Deploy Runbook"), claim); err != nil || !created {
		t.Fatalf("seed the claim: created=%v err=%v", created, err)
	}

	// WITHIN THE GRACE a crashed create and one in flight are
	// indistinguishable, so the claim stands — stepping over it would
	// destroy a live create.
	c.at = c.at.Add(pages.OrphanGrace / 2)
	if _, err := s.Create(t.Context(), author("jane"), pages.NewPage{
		Container: "ENG", Title: "Deploy Runbook",
	}); !errors.Is(err, pages.ErrTitleTaken) {
		t.Fatalf("a claim inside the grace was stepped over: %v", err)
	}

	// PAST IT, only a crash explains a claim whose page does not exist.
	c.at = c.at.Add(pages.OrphanGrace)
	got, err := s.Create(t.Context(), author("jane"), pages.NewPage{
		Container: "ENG", Title: "Deploy Runbook",
	})
	if err != nil {
		t.Fatalf("an orphan claim locked the title: %v", err)
	}
	if got.Page.Title != "Deploy Runbook" {
		t.Errorf("title = %q", got.Page.Title)
	}
}

// A SAVE MUST STATE THE VERSION IT EDITED. A wiki's worst failure is silently
// overwriting a paragraph somebody else just wrote, and there is no per-field
// merge that makes that safe for prose — which is why this is required where
// a work item's If-Match is optional.
func TestASaveIsRefusedAgainstAStaleVersion(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	got := write(t, s, author("jane"), pages.NewPage{Body: "first"})

	if _, err := s.SavePage(t.Context(), author("jane"), got.Page.ID, pages.Save{
		Body: ptr("second"),
	}); !errors.Is(err, pages.ErrInvalid) {
		t.Fatalf("a save with no base version was accepted: %v", err)
	}

	second, err := s.SavePage(t.Context(), author("eng"), got.Page.ID, pages.Save{
		BaseVersion: got.Page.Version, Body: ptr("second"), Message: "rewrote the opening",
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if second.Page.Version != got.Page.Version+1 {
		t.Errorf("version = %d, want %d", second.Page.Version, got.Page.Version+1)
	}

	// The stale save is REFUSED, naming both versions so the editor knows
	// what to re-base on.
	_, err = s.SavePage(t.Context(), author("jane"), got.Page.ID, pages.Save{
		BaseVersion: got.Page.Version, Body: ptr("a conflicting edit"),
	})
	if !errors.Is(err, pages.ErrStaleVersion) {
		t.Fatalf("a stale save was accepted: %v", err)
	}
	if !strings.Contains(err.Error(), "version 1") || !strings.Contains(err.Error(), "version 2") {
		t.Errorf("the refusal does not name both versions: %v", err)
	}
}

// EVERY SAVE WRITES A REVISION, and the first one is the page as it was
// WRITTEN — a page whose original text was never a revision has no way back
// to what it said when it was created.
func TestEverySaveKeepsTheTextItReplaced(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	got := write(t, s, author("jane"), pages.NewPage{
		Body: "the original", Message: "first draft",
	})

	first, err := s.Revision(t.Context(), got.Page.ID, 1)
	if err != nil {
		t.Fatalf("the first revision was never written: %v", err)
	}
	if first.Body != "the original" || first.Message != "first draft" {
		t.Errorf("revision 1 = %+v", first)
	}

	if _, err := s.SavePage(t.Context(), author("eng"), got.Page.ID, pages.Save{
		BaseVersion: 1, Body: ptr("the rewrite"), Message: "tightened it",
	}); err != nil {
		t.Fatal(err)
	}
	second, err := s.Revision(t.Context(), got.Page.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if second.Body != "the rewrite" || second.Author != "eng" {
		t.Errorf("revision 2 = %+v", second)
	}
	// And revision 1 is UNTOUCHED, which is what immutable means.
	again, err := s.Revision(t.Context(), got.Page.ID, 1)
	if err != nil || again.Body != "the original" {
		t.Errorf("revision 1 changed: %+v, %v", again, err)
	}
}

// A CRASH BETWEEN THE REVISION AND THE HEAD MUST NOT LOCK A PAGE. Without the
// grace rule the page is uneditable until the hourly sweep, which is an
// outage for whoever is trying to fix it now.
func TestAnOrphanedRevisionIsOverwrittenPastTheGrace(t *testing.T) {
	t.Parallel()
	s, docs, c := newStore(t)
	got := write(t, s, author("jane"), pages.NewPage{Body: "first"})

	// A revision at version 2 with the head still at 1, as a crashed save
	// leaves.
	orphan, err := pages.EncodeRevision(pages.Revision{
		V: 1, ID: "orphan", PageID: got.Page.ID, Version: 2,
		Title: got.Page.Title, Body: "the write that died", CreatedAt: c.at,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created, err := docs.CreateDocument(t.Context(), coord.FamilyPages,
		pages.RevisionKey(got.Page.ID, 2), orphan); err != nil || !created {
		t.Fatalf("seed the orphan: created=%v err=%v", created, err)
	}

	// WITHIN THE GRACE it is indistinguishable from a save in flight.
	c.at = c.at.Add(pages.OrphanGrace / 2)
	if _, err := s.SavePage(t.Context(), author("eng"), got.Page.ID, pages.Save{
		BaseVersion: 1, Body: ptr("my edit"),
	}); !errors.Is(err, pages.ErrStaleVersion) {
		t.Fatalf("a revision inside the grace was overwritten: %v", err)
	}

	// PAST IT, the head is the authority on which versions exist.
	c.at = c.at.Add(pages.OrphanGrace)
	saved, err := s.SavePage(t.Context(), author("eng"), got.Page.ID, pages.Save{
		BaseVersion: 1, Body: ptr("my edit"),
	})
	if err != nil {
		t.Fatalf("an orphan revision locked the page: %v", err)
	}
	if saved.Page.Body != "my edit" {
		t.Errorf("body = %q", saved.Page.Body)
	}
	rev, err := s.Revision(t.Context(), got.Page.ID, 2)
	if err != nil || rev.Body != "my edit" {
		t.Errorf("revision 2 = %+v, %v", rev, err)
	}
}

// A RENAME TAKES THE NEW CLAIM BEFORE RELEASING THE OLD, so a page is never
// unreachable by title — and never leaves its old name free while it still
// answers to it.
func TestARenameMovesTheClaimInTheSafeOrder(t *testing.T) {
	t.Parallel()
	s, docs, _ := newStore(t)
	got := write(t, s, author("jane"), pages.NewPage{Title: "Old Name"})

	renamed, err := s.SavePage(t.Context(), author("jane"), got.Page.ID, pages.Save{
		BaseVersion: 1, Title: ptr("New Name"),
	})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if renamed.Page.Title != "New Name" {
		t.Errorf("title = %q", renamed.Page.Title)
	}

	// The old name is FREE afterwards, so somebody can reuse it.
	if _, err := s.Create(t.Context(), author("eng"), pages.NewPage{
		Container: "ENG", Title: "Old Name",
	}); err != nil {
		t.Errorf("the old title stayed claimed after a rename: %v", err)
	}
	// And the new one is HELD.
	if _, err := s.Create(t.Context(), author("eng"), pages.NewPage{
		Container: "ENG", Title: "New Name",
	}); !errors.Is(err, pages.ErrTitleTaken) {
		t.Errorf("the new title was not claimed: %v", err)
	}

	// A rename to a name somebody else holds is REFUSED, and the page keeps
	// the title it had — a half-applied rename would leave it addressable
	// by neither.
	other := write(t, s, author("eng"), pages.NewPage{Title: "Taken"})
	_ = other
	if _, err := s.SavePage(t.Context(), author("jane"), got.Page.ID, pages.Save{
		BaseVersion: renamed.Page.Version, Title: ptr("Taken"),
	}); !errors.Is(err, pages.ErrTitleTaken) {
		t.Fatalf("a rename onto a held title was accepted: %v", err)
	}
	live, _, err := s.Page(t.Context(), got.Page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if live.Title != "New Name" || live.Version != renamed.Page.Version {
		t.Errorf("the refused rename moved the page: %+v", live)
	}
	_ = docs
}

// A COMMENT DOES NOT SUBSCRIBE ITS COMMENTER, which is the opposite of the
// tracker's participants rule and deliberate: a page a hundred people have
// remarked on would otherwise wake a hundred seats every time somebody fixes
// a heading. A MENTION still subscribes its target, because it is directed.
func TestCommentingDoesNotSubscribeButMentioningDoes(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	got := write(t, s, author("jane"), pages.NewPage{})

	if _, _, err := s.Comment(t.Context(), agent("eng"), got.Page.ID,
		pages.NewComment{Body: "a typo in the third paragraph"}); err != nil {
		t.Fatal(err)
	}
	page, _, err := s.Page(t.Context(), got.Page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(page.Watchers, "eng") {
		t.Errorf("commenting subscribed the commenter: %v — a page many people "+
			"remark on would wake all of them on every edit", page.Watchers)
	}

	if _, _, err := s.Comment(t.Context(), agent("eng"), got.Page.ID,
		pages.NewComment{Body: "@ops is this still right?", Mentions: []string{"ops"}}); err != nil {
		t.Fatal(err)
	}
	page, _, err = s.Page(t.Context(), got.Page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(page.Watchers, "ops") {
		t.Errorf("a mention did not subscribe its target: %v", page.Watchers)
	}
}

// AN EDIT SUBSCRIBES ITS AUTHOR: somebody who wrote a paragraph wants to know
// when it is rewritten.
func TestEditingSubscribesTheEditor(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	got := write(t, s, author("jane"), pages.NewPage{Body: "first"})
	if _, err := s.SavePage(t.Context(), agent("eng"), got.Page.ID, pages.Save{
		BaseVersion: 1, Body: ptr("second"),
	}); err != nil {
		t.Fatal(err)
	}
	page, _, err := s.Page(t.Context(), got.Page.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"jane", "eng"} {
		if !slices.Contains(page.Watchers, want) {
			t.Errorf("%q does not watch a page they wrote on: %v", want, page.Watchers)
		}
	}
}

// AN UNWATCH STICKS, and a directed mention still reaches them — the same two
// rules the tracker's mute enforces.
func TestAnUnwatchSticksButAMentionStillArrives(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	got := write(t, s, author("jane"), pages.NewPage{Body: "first"})

	if _, err := s.SavePage(t.Context(), author("jane"), got.Page.ID, pages.Save{
		BaseVersion: 1, Watch: ptr(false),
	}); err != nil {
		t.Fatal(err)
	}
	page, _, err := s.Page(t.Context(), got.Page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(page.Watchers, "jane") || !slices.Contains(page.Muted, "jane") {
		t.Fatalf("the unwatch did not take: %+v", page)
	}

	// A LATER EDIT BY THEM DOES NOT RE-SUBSCRIBE, which is what makes an
	// unwatch mean anything.
	if _, err := s.SavePage(t.Context(), author("jane"), got.Page.ID, pages.Save{
		BaseVersion: page.Version, Body: ptr("their own later edit"),
	}); err != nil {
		t.Fatal(err)
	}
	page, _, _ = s.Page(t.Context(), got.Page.ID)
	if slices.Contains(page.Watchers, "jane") {
		t.Errorf("editing re-subscribed somebody who unwatched: %v", page.Watchers)
	}

	// A MENTION DOES, and clears the mute: a mute says "stop telling me
	// about this page" and a mention says "I am telling you".
	if _, _, err := s.Comment(t.Context(), agent("eng"), got.Page.ID,
		pages.NewComment{Body: "@jane?", Mentions: []string{"jane"}}); err != nil {
		t.Fatal(err)
	}
	page, _, _ = s.Page(t.Context(), got.Page.ID)
	if !slices.Contains(page.Watchers, "jane") || slices.Contains(page.Muted, "jane") {
		t.Errorf("a mention did not reach a muted person: %+v", page)
	}
}

// A COMMENT FROM A TURN IS IDEMPOTENT, so a re-run turn posts once.
func TestATurnsCommentOnAPageIsPostedOnce(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	got := write(t, s, author("jane"), pages.NewPage{})

	post := func() pages.Comment {
		c, _, err := s.Comment(t.Context(), agent("eng"), got.Page.ID,
			pages.NewComment{Body: "checked, still accurate", TurnKey: "turn-1"})
		if err != nil {
			t.Fatalf("comment: %v", err)
		}
		return c
	}
	if first, again := post(), post(); first.ID != again.ID {
		t.Errorf("a re-run turn posted twice: %s and %s", first.ID, again.ID)
	}
	thread, err := s.Thread(t.Context(), got.Page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(thread) != 1 {
		t.Errorf("the thread has %d comments after one turn ran twice", len(thread))
	}
}

// A CONTAINER IS CREATED ONCE HOWEVER MANY NODES ASK. Every node calls this
// on every apply for every unit's space, so a race must be a no-op rather
// than an error either reports.
func TestEnsuringAContainerIsIdempotent(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)

	const nodes = 8
	results := make(chan error, nodes)
	for range nodes {
		go func() {
			_, err := s.EnsureContainer(context.Background(), "eng", "Engineering", "the team's pages")
			results <- err
		}()
	}
	for range nodes {
		if err := <-results; err != nil {
			t.Fatalf("a concurrent EnsureContainer failed: %v", err)
		}
	}
	got, err := s.EnsureContainer(t.Context(), "ENG", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Key != "ENG" || got.Name != "Engineering" {
		t.Errorf("container = %+v, want the first writer's values kept", got)
	}
}

// A REMOVAL PURGES EVERYTHING AND FREES THE TITLE, and keeps the record.
func TestRemovingAPageFreesItsTitleAndKeepsTheRecord(t *testing.T) {
	t.Parallel()
	s, docs, _ := newStore(t)
	got := write(t, s, author("jane"), pages.NewPage{Title: "Temporary", Body: "x"})
	if _, _, err := s.Comment(t.Context(), author("jane"), got.Page.ID,
		pages.NewComment{Body: "a remark"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(t.Context(), author("jane"), got.Page.ID); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Page(t.Context(), got.Page.ID); !errors.Is(err, pages.ErrNotFound) {
		t.Errorf("the page survived removal: %v", err)
	}
	if thread, _ := s.Thread(t.Context(), got.Page.ID); len(thread) != 0 {
		t.Errorf("the thread survived: %d comments", len(thread))
	}
	if _, err := s.Revision(t.Context(), got.Page.ID, 1); !errors.Is(err, pages.ErrNotFound) {
		t.Errorf("a revision survived: %v", err)
	}
	// THE TITLE IS FREE, so somebody can write the page again.
	if _, err := s.Create(t.Context(), author("eng"), pages.NewPage{
		Container: "ENG", Title: "Temporary",
	}); err != nil {
		t.Errorf("the title stayed claimed after removal: %v", err)
	}
	// AND THE CHANGE KEYS STAY: they are what a redelivered feed message is
	// deduplicated against.
	changes, err := docs.Documents(t.Context(), coord.FamilyPages,
		pages.ChangePrefix(got.Page.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) == 0 {
		t.Error("the removal purged the record of what happened")
	}
}

// Every refusal names the field and why.
func TestOversizedContentIsRefusedNamingTheField(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	for _, tc := range []struct {
		name  string
		in    pages.NewPage
		field string
	}{
		{"a title", pages.NewPage{Title: strings.Repeat("x", pages.MaxTitle+1)}, "title"},
		{"no title", pages.NewPage{Title: "   "}, "title"},
		{"a body", pages.NewPage{Body: strings.Repeat("x", pages.MaxBody+1)}, "body"},
		{"no container", pages.NewPage{Container: " "}, "container"},
		{"a status", pages.NewPage{Status: "archived"}, "status"},
		{"too many labels", pages.NewPage{Labels: manyLabels(pages.MaxLabels + 1)}, "labels"},
		{"a long message", pages.NewPage{Message: strings.Repeat("x", pages.MaxMessage+1)}, "message"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := tc.in
			if in.Container == "" {
				in.Container = "ENG"
			}
			if in.Title == "" {
				in.Title = "a page " + tc.name
			}
			_, err := s.Create(t.Context(), author("jane"), in)
			if !errors.Is(err, pages.ErrInvalid) {
				t.Fatalf("got %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("the refusal does not name %q: %v", tc.field, err)
			}
		})
	}
}

func manyLabels(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("label-%d", i)
	}
	return out
}

// A write with no honest actor is refused rather than attributed to a guess.
func TestAWriteNeedsAnActor(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t)
	for _, actor := range []pages.Actor{{}, {Kind: "robot", Handle: "x"}, {Kind: pages.AuthorAgent}} {
		if _, err := s.Create(t.Context(), actor, pages.NewPage{
			Container: "ENG", Title: "t",
		}); !errors.Is(err, pages.ErrInvalid) {
			t.Errorf("actor %+v was accepted: %v", actor, err)
		}
	}
	got, err := s.Create(t.Context(), pages.Actor{Kind: pages.AuthorOperator, OperatorID: "ops"},
		pages.NewPage{Container: "ENG", Title: "by an operator"})
	if err != nil {
		t.Fatalf("an unbound operator token was refused: %v", err)
	}
	if got.Page.Author != "operator:ops" {
		t.Errorf("author = %q, want the token's own label", got.Page.Author)
	}
}

// A title is one address however it is typed, and a conversation key survives
// a rename — unlike the tracker's, which keys on the human item key.
func TestTitlesNormaliseAndTheConversationSurvivesARename(t *testing.T) {
	t.Parallel()
	for _, tc := range [][2]string{
		{"Deploy Runbook", "deploy runbook"},
		{"  Deploy   Runbook  ", "deploy runbook"},
		{"DEPLOY RUNBOOK", "deploy runbook"},
	} {
		if got := pages.NormalizeTitle(tc[0]); got != tc[1] {
			t.Errorf("NormalizeTitle(%q) = %q, want %q", tc[0], got, tc[1])
		}
	}
	if got := pages.ConversationKey("p-1"); got != "page:p-1" {
		t.Errorf("ConversationKey = %q", got)
	}
}
