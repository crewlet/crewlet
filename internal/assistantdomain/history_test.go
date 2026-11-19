package assistantdomain_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
)

// A PERSON'S CONVERSATIONS ARE LISTED NEWEST FIRST AND SEARCHED BY TITLE: a
// search matches the unsealed title in any case, pages through the matches
// alone under its cursor, never shows another person's conversation, and a
// search past its bound is refused. Mutation: match the search against the
// sealed column, or page over every row rather than the matches, and the
// pages go wrong.
func TestAPersonsConversationsAreListedNewestFirstAndSearchedByTitle(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ctx := t.Context()
	ada, bob := uuid.New(), uuid.New()
	c := person(ada, "ada")
	var ids []uuid.UUID
	for _, title := range []string{"Payroll for October", "Hiring plan", "payroll errors",
		"Brand refresh", "PAYROLL audit"} {
		id := n.conversation(c, title)
		n.openTurn(c, id)
		ids = append(ids, id)
	}
	n.openTurn(person(bob, "bob"), n.conversation(person(bob, "bob"), "Payroll, bob's"))

	all, err := n.reader.Conversations(ctx, ada, "", 0, "")
	if err != nil || len(all.Conversations) != 5 || all.NextCursor != nil {
		t.Fatalf("ada's whole listing = %d conversations, cursor %v (%v)", len(all.Conversations),
			all.NextCursor, err)
	}
	var seen []uuid.UUID
	var cursor string
	for page := 0; ; page++ {
		got, err := n.reader.Conversations(ctx, ada, cursor, 1, "  PayRoll ")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range got.Conversations {
			seen = append(seen, s.ID)
			if s.Title == nil || !strings.Contains(strings.ToLower(*s.Title), "payroll") {
				t.Errorf("a search for payroll answered %v", s.Title)
			}
		}
		if got.NextCursor == nil {
			break
		}
		if page > 3 {
			t.Fatal("the search pages past its three matches")
		}
		cursor = *got.NextCursor
	}
	if want := []uuid.UUID{ids[4], ids[2], ids[0]}; !slices.Equal(seen, want) {
		t.Errorf("the search answered %v, want ada's three payroll conversations newest first %v",
			seen, want)
	}
	none, err := n.reader.Conversations(ctx, ada, "", 0, "nothing like it")
	if err != nil || len(none.Conversations) != 0 || none.NextCursor != nil {
		t.Errorf("a search matching nothing = %+v (%v)", none, err)
	}
	if _, err := n.reader.Conversations(ctx, ada, "", 0, strings.Repeat("x",
		wire.SearchMaxRunes+1)); !errors.Is(err, assistantdomain.ErrInvalid) {
		t.Errorf("a search past its bound answered %v", err)
	}
}

// A TITLE A PERSON SET IS NEVER OVERWRITTEN by one the engine writes, in
// whichever order the two are published, and the listing says who named the
// conversation; the person may rename it again. A person's title is held to its
// bounds. Mutation: drop the applier's guard and the model's later title wins.
func TestAPersonSetTitleIsNeverOverwritten(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ctx := t.Context()
	ada := uuid.New()
	c := person(ada, "ada")
	id := n.conversation(c, "")
	n.openTurn(c, id)
	title := func() (string, wire.TitleSource) {
		t.Helper()
		got, err := n.reader.Conversation(ctx, ada, id)
		if err != nil {
			t.Fatal(err)
		}
		s := got.Summary()
		if s.Title == nil || s.TitleSetBy == nil {
			t.Fatalf("the conversation lists no title: %+v", s)
		}
		return *s.Title, *s.TitleSetBy
	}
	if _, err := n.writer.TitleConversation(ctx, fresh(c), id, "What is the payroll?",
		wire.TitleByModel); err != nil {
		t.Fatal(err)
	}
	if got, by := title(); got != "What is the payroll?" || by != wire.TitleByModel {
		t.Fatalf("the engine's title reads %q by %s", got, by)
	}
	if _, err := n.writer.TitleConversation(ctx, fresh(c), id, "October payroll",
		wire.TitleByPerson); err != nil {
		t.Fatal(err)
	}
	if _, err := n.writer.TitleConversation(ctx, fresh(c), id, "Payroll questions",
		wire.TitleByModel); err != nil {
		t.Fatal(err)
	}
	if got, by := title(); got != "October payroll" || by != wire.TitleByPerson {
		t.Errorf("after the engine's later title the conversation reads %q by %s, want the "+
			"person's", got, by)
	}
	if _, err := n.writer.TitleConversation(ctx, fresh(c), id, "Payroll, final",
		wire.TitleByPerson); err != nil {
		t.Fatal(err)
	}
	if got, _ := title(); got != "Payroll, final" {
		t.Errorf("a second rename reads %q", got)
	}
	for name, bad := range map[string]string{"blank": "   ",
		"too long": strings.Repeat("a", wire.TitleMaxRunes+1)} {
		if _, err := n.writer.TitleConversation(ctx, fresh(c), id, bad,
			wire.TitleByPerson); !errors.Is(err, assistantdomain.ErrInvalid) {
			t.Errorf("%s: a person's title answered %v", name, err)
		}
	}
}
