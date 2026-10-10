package queries

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/tracker"
)

// WHAT A MEMBER'S ANSWER LEAVES OUT.
//
// Most of what a member reads is decided at registration: a question is
// member or admin, and a member asking an admin's question is refused before
// it runs (ADR-0031). A handful of MEMBER questions also carry a field from
// the other side of the withholding rule — what a task, a completion or a seat
// SPENT, which is what the machine processed rather than what the company
// published. Those answers are the same question to both reaches and differ
// only in that field, so the field is withheld here rather than the question
// split in two: a second question per answer would be a second
// implementation, which is the divergence this package exists to prevent.
//
// WITHHELD MEANS ABSENT, never zero. A member told "0 tokens" is told
// something false about a task that cost a fortune, and a screen cannot tell
// a zero from a withheld one; an absent key is what every shape here already
// renders as "not known to this reader". Where the caller ASKED for spend by
// name — a row fact, an order, a threshold, a total — the question is REFUSED
// instead ([spendTerms]), because each of those promises an answer that
// carries it, and an order by cost is the cost by another name.
//
// The rule is the CALLER'S reach, read once per answer through [callerFrom],
// which only [Registry.AnswerWith] writes; a disabled guard's caller is an
// admin and reads everything, as it reads every admin question.

// readsSpend reports whether the caller of this answer reads what the machine
// spent: an admin key's reach (ADR-0031).
func readsSpend(ctx context.Context) bool { return callerFrom(ctx).IsAdmin() }

// spendTerms refuses a question that asks for spend by name, for a caller
// who does not read it, naming every term to drop — see [tracker.Query.SpendTerms].
func spendTerms(ctx context.Context, q tracker.Query) error {
	terms := q.SpendTerms()
	if len(terms) == 0 || readsSpend(ctx) {
		return nil
	}
	return fmt.Errorf("%w: what a task cost is an admin key's to read "+
		"(ADR-0031), and this question reads it through %s — ask without "+
		"those terms", ErrForbidden, strings.Join(terms, ", "))
}
