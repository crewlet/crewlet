package tracker

import (
	"slices"
	"strings"
)

// Party is who a personal question is about.
//
// # A PERSON IS ONE PARTY WITH TWO IDENTITIES
//
// A human teammate holds a SEAT in the chart and, if they run the company,
// also holds one of Tier A's `api.auth.tokens`. Those two are deliberately
// different on a record: a write made through their credential is attributed
// to the TOKEN's own name with author kind `operator`, never to a seat handle,
// because a tracker whose author field is chosen by the writer is not an audit
// trail (see internal/api/operator). That rule is right and it is not what this
// type changes.
//
// What it changes is the READ. Every personal question — "what am I assigned",
// "what am I watching", "what was I asked", "what is in my inbox" — matches
// rows by the handle stored on them, and the rows one person leaves behind
// carry BOTH names: a work item a founder files through their assistant has
// `founder` as its reporter and its watcher, while the dashboard answers for
// the seat `jane-founder` that token is bound to. Asked about one identity,
// every block came back empty for a person who is reporter and watcher on
// eleven items.
//
// # Why a set rather than two reads merged by the caller
//
// Because each block is bounded and ordered. `assigned` is twenty rows in
// priority-then-due order; two twenty-row reads merged afterwards is not the
// same answer, it is the top twenty of one list glued to the top twenty of
// another, and the rows that should have been in neither top twenty stay
// hidden while ones that should have been dropped survive. The same holds for
// the inbox's keyset page. One predicate over both identities is the only
// shape that keeps a bound correct.
//
// # One identity still comes back
//
// [Party.Handle] is the SEAT, which is who the person is in the chart and what
// every answer reports itself under. The aliases are only ever matched
// against: nothing renders them, nothing routes to them, and a caller is never
// handed a credential's name as though it were a colleague's.
type Party struct {
	// Handle is the seat, and it is required: a personal question with
	// nobody's name on it is everybody's. For a token nobody is bound to,
	// this is the token's own id — which is the identity its rows carry,
	// so the answer is still that party's own work.
	Handle string

	// OperatorID is the `api.auth.tokens[].id` bound to that seat with
	// `contact.crewlet_operator_id`, and empty for the ordinary seat that
	// has none. It is an ALIAS and never an address: the surface resolves
	// it from the company chart, which this package deliberately does not
	// hold.
	OperatorID string
}

// PartyOf is the one-identity party, for a caller that has a handle and no
// chart to resolve an alias from.
//
// NAMED rather than left as a struct literal at each call site, because the
// two fields are both strings: `Party{handle, operatorID}` transposed compiles
// and answers about a credential instead of a person.
func PartyOf(handle string) Party { return Party{Handle: handle} }

// Handles is every identity this party's rows may be filed under, the seat
// first, trimmed, de-duplicated and with the empties dropped.
//
// THE EMPTIES ARE DROPPED BECAUSE OF THE NEGATIVE PREDICATES. The `assignee`
// column is NOT NULL and defaults to the empty string, so an unassigned task
// carries it — and a `collaborating` block whose exclusion listed an empty
// member alongside a handle would silently drop every unassigned task
// somebody was brought onto. A positive `IN` with an empty member is merely
// useless; the negative one is wrong, and both come from this list.
func (p Party) Handles() []string {
	out := make([]string, 0, 2)
	for _, handle := range []string{p.Handle, p.OperatorID} {
		handle = strings.TrimSpace(handle)
		if handle == "" || slices.Contains(out, handle) {
			continue
		}
		out = append(out, handle)
	}
	return out
}

// args is [Party.Handles] as statement arguments, so a caller writes
// `placeholders(len(ids))` once against the same slice it binds.
func (p Party) args() []any {
	ids := p.Handles()
	out := make([]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, id)
	}
	return out
}

// Named reports a party a question can be answered about.
func (p Party) Named() bool { return strings.TrimSpace(p.Handle) != "" }

// String renders the party for a message a person reads: the seat alone, or
// the seat and the credential it also answers for.
//
// BUILT FROM [Party.Handles] rather than from the fields, so the name in a
// refusal is exactly the set the predicate bound — a message that named an
// identity the query did not use would send a reader looking in the wrong
// place.
func (p Party) String() string {
	ids := p.Handles()
	switch len(ids) {
	case 0:
		return "nobody"
	case 1:
		return ids[0]
	}
	return ids[0] + " (and operator " + ids[1] + ")"
}
