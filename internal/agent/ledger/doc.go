// Package ledger keeps a seat honest about what it has already done.
//
// Two scopes, same doctrine, one file each:
//
//   - iteration.go — within ONE turn, across its self_iterate rounds. Each
//     round rebuilds its LLM conversation from scratch, so without a record
//     kept outside those conversations a second round starts blind: it cannot
//     tell that round one already posted to Slack, so it plans the post again
//     and the side effect fires twice.
//   - conversation.go — across TURNS of one conversation. The second comment
//     on an issue, the reply three days later in a thread.
//
// THE STRUCTURE IS THE ENGINE'S, NEVER A SUMMARISER'S. Every line — which
// tool was called, whether it succeeded, which identifiers it was called
// with, what the seat replied — is built from data already in hand, because
// the failure being prevented is a duplicated external side effect, and a
// summariser that drops the one line naming a delivery re-creates exactly
// that bug in a place where nothing downstream can catch it.
//
// What a model MAY rewrite is narrower and is named: a PAYLOAD past its
// budget (fit.go) — the body of a message whose call line still says where it
// went, the error document of a call still marked as failed — and the oldest
// entries of a conversation too long to carry whole, which would otherwise be
// dropped outright. In both cases the alternative a rewrite replaces is a
// loss: a cut that keeps a payload's opening and reads as the payload, or an
// entry that is not shown at all. The rewriting itself is the caller's; this
// package only says what needs it.
//
// READS ARE MARKED, NOT MERGED WITH WRITES. Tool *results* are deliberately
// not carried across rounds or turns, so a read the next round needs must be
// re-run; reads therefore render with a "(read)" marker and the prompt permits
// re-running exactly those. Telling a model "do not repeat" a jira_get_issue
// would push it to fabricate the data instead. Across turns the rule is
// STRONGER, not weaker — a read from last Tuesday is stale by construction.
//
// The package imports nothing from crewlet. The turn context, the prompt
// builder and the API layer all hold ledger values, and a ledger that dragged
// the provider stack behind it would be held by all three.
package ledger
