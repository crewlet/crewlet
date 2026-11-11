package worklet

import (
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

// EVERY CURSOR A LISTING CANNOT RESUME IS AN ErrBadCursor, and so an
// ErrBadQuery: each surface answers it as a refusal of the request (a 400
// `bad_params`, a tool's "drop the cursor"), never as this node's fault.
//
// Every refusal the three cursor readers make, the work list's keyset
// ([cursorArms]), a priority list's offset ([pageListed]) and a thread's place
// ([parseCommentCursor]), because one spelled as a plain error was answered
// 500 to a reader pressing "Load more" after somebody added a seat.
//
// Mutation: build any one of the refusals with a plain fmt.Errorf and its case
// goes red.
func TestEveryCursorRefusalIsABadCursor(t *testing.T) {
	t.Parallel()
	encoded := func(body string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(body))
	}
	q, err := ParseQuery(MapParams(map[string]any{"container": "project:ENG",
		"sort": "rank"}), planNow, time.UTC)
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	resumed := func(cursor string) error {
		at := q
		at.Cursor = cursor
		_, err := cursorArms(at, nil, orderBasis{})
		return err
	}
	otherOrder := mintCursor([]any{"a0", "t-1"},
		cursorOrder(sortTerms(Query{Sort: []Sort{{Key: "updated"}}}, nil, orderBasis{})))

	cases := map[string]func() error{
		"a list cursor that is not base64": func() error { return resumed("not a cursor!") },
		"a list cursor that is not a page": func() error { return resumed(encoded("[1, 2]")) },
		"a list cursor naming no row":      func() error { return resumed(encoded(`{"k":["a0",null]}`)) },
		"a list cursor from another order": func() error { return resumed(otherOrder) },
		"a priority list cursor that is not base64": func() error {
			_, _, err := pageListed(nil, "not a cursor!", 5)
			return err
		},
		"a priority list cursor from another order": func() error {
			_, _, err := pageListed(nil, otherOrder, 5)
			return err
		},
		"a priority list cursor at no offset": func() error {
			_, _, err := pageListed(nil, mintCursor([]any{-1}, listCursorOrder), 5)
			return err
		},
		"a priority list cursor of two places": func() error {
			_, _, err := pageListed(nil, mintCursor([]any{1, 2}, listCursorOrder), 5)
			return err
		},
		"a comment cursor that is a word": func() error {
			_, err := parseCommentCursor("abc")
			return err
		},
		"a comment cursor with no id": func() error {
			_, err := parseCommentCursor("1700000000:")
			return err
		},
		"a comment cursor with no instant": func() error {
			_, err := parseCommentCursor("x:cm-1")
			return err
		},
	}
	for name, refuse := range cases {
		t.Run(name, func(t *testing.T) {
			err := refuse()
			if !errors.Is(err, ErrBadCursor) || !errors.Is(err, ErrBadQuery) {
				t.Fatalf("refused with %v, want an ErrBadCursor", err)
			}
		})
	}

	// AND THE CURSORS THE READERS MINT ARE RESUMED.
	if _, err := parseCommentCursor(formatCommentCursor(1700000000, "cm-1")); err != nil {
		t.Errorf("a minted comment cursor was refused: %v", err)
	}
	if _, _, err := pageListed(nil, mintCursor([]any{3}, listCursorOrder), 5); err != nil {
		t.Errorf("a minted priority list cursor was refused: %v", err)
	}
}

// AN ORDER'S DIGEST CARRIES WHAT ITS TERMS BIND AND ARE RELATIVE TO, so a
// cursor is refused by an order that renders the same and reads other values:
// a field sort of another field, and the seats' order after a chart apply
// moved it. Digested from the rendered order alone, `sort=f.effort`'s cursor
// resumed `sort=f.cost` at a value of another field, and a seat added to the
// chart moved every place after it with the old cursor still accepted.
//
// Mutation: digest [renderOrder] alone and every case goes red.
func TestAnOrdersDigestCarriesWhatItsTermsBind(t *testing.T) {
	t.Parallel()
	byField := func(id string) []sortTerm {
		return sortTerms(Query{Sort: []Sort{{Key: "f.effort"}}},
			map[string]resolvedField{"effort": {ID: id, Slug: "effort", Type: FieldNumber}},
			orderBasis{})
	}
	byAssignee := func(roster uint64) []sortTerm {
		return sortTerms(Query{Sort: []Sort{{Key: "assignee"}}}, nil,
			orderBasis{roster: roster})
	}
	for name, pair := range map[string][2][]sortTerm{
		"two fields of one type":   {byField("field-1"), byField("field-2")},
		"the seats' order applied": {byAssignee(7), byAssignee(9)},
		"the first seats' order":   {byAssignee(0), byAssignee(9)},
	} {
		if renderOrder(pair[0]) != renderOrder(pair[1]) {
			t.Fatalf("%s: the two orders render differently, so this case "+
				"asserts nothing about what they bind", name)
		}
		if cursorOrder(pair[0]) == cursorOrder(pair[1]) {
			t.Errorf("%s: one digest for two orders that read different values", name)
		}
	}
	minted, resumed := cursorOrder(byAssignee(7)), cursorOrder(byAssignee(7))
	if minted != resumed {
		t.Error("one roster digests two ways, so no cursor over it would resume")
	}
}

// A NAME IS ORDERED BY THE ROOT COLLATION, not by its lowercased bytes, which
// put every accented initial after every ASCII one: "Émile" after "Zoe". In
// the root collation an accent or a case orders only names otherwise equal,
// and the name's bytes and then the handle settle a tie, so the order is total.
//
// Mutation: compare strings.ToLower of the names and the first case goes red;
// drop the handle and the last does.
func TestSeatsAreOrderedByTheRootCollation(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		names map[string]string
		want  []string
	}{
		"an accented initial sorts with its letter": {
			names: map[string]string{"zoe": "Zoe Quinn", "emile": "Émile Zola",
				"emma": "emma Stone", "eve": "Eve Adams", "omer": "Ömer Kaya",
				"olga": "Olga Berg"},
			want: []string{"emile", "emma", "eve", "olga", "omer", "zoe"},
		},
		"case orders only names otherwise equal": {
			names: map[string]string{"b": "Ada", "a": "ada", "c": "Adam"},
			want:  []string{"a", "b", "c"},
		},
		"an accent orders only names otherwise equal": {
			names: map[string]string{"x": "Rene", "y": "René", "z": "Renee"},
			want:  []string{"x", "y", "z"},
		},
		"two seats of one name keep their handles' order": {
			names: map[string]string{"sam-2": "Sam", "sam-1": "Sam"},
			want:  []string{"sam-1", "sam-2"},
		},
	} {
		got := seatsByName(c.names)
		if len(got) != len(c.want) {
			t.Fatalf("%s: %v, want %v", name, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: %v, want %v", name, got, c.want)
				break
			}
		}
	}
}
