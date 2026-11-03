// Package ledger keeps a seat honest about what it has already done within one
// turn, across its self_iterate rounds.
//
// Each round rebuilds its LLM conversation from scratch, so without a record
// kept outside those conversations a second round starts blind: it cannot tell
// that round one already posted to Slack, so it plans the post again and the
// side effect fires twice.
//
// ACROSS TURNS IS NOT THIS PACKAGE'S. A conversation's next turn continues the
// session its last turn left, transcript and all, and is told how the
// conversation's earlier turns ended from their rows on the executor log
// (internal/executordomain, rendered by internal/agent/prompts) — the engine's
// account of what a transcript cannot know, kept by the log every data node
// replicates rather than in a second history of the conversation beside it.
//
// THE STRUCTURE IS THE ENGINE'S, NEVER A SUMMARISER'S. Every line — which
// tool was called, whether it succeeded, which identifiers it was called
// with, what the round produced — is built from data already in hand, because
// the failure being prevented is a duplicated external side effect, and a
// summariser that drops the one line naming a delivery re-creates exactly
// that bug in a place where nothing downstream can catch it.
//
// What a model MAY rewrite is narrower and is named: a PAYLOAD past its
// budget (fit.go) — the body of a message whose call line still says where it
// went, the error document of a call still marked as failed. The alternative
// a rewrite replaces is a loss: a cut that keeps a payload's opening and reads
// as the payload. The rewriting itself is the caller's; this package only says
// what needs it.
//
// READS ARE MARKED, NOT MERGED WITH WRITES. Tool *results* are deliberately
// not carried across rounds, so a read the next round needs must be re-run;
// reads therefore render with a "(read)" marker and the prompt permits
// re-running exactly those. Telling a model "do not repeat" a jira_get_issue
// would push it to fabricate the data instead.
//
// A RECORD BOUNDED IN SIZE STILL ACCOUNTS FOR EVERY CALL. An executor run's
// calls come back from a row that keeps each call's payloads to a share and a
// long run's middle only as a tally (internal/sandbox, rowbytes.go), so a call
// carries the identity its run counted it by ([Call.Digest]) — or says its
// arguments, and so its identity, are withheld ([Call.Withheld]) — and a round
// the identities of the calls its record dropped ([Iteration.Elided]) and of
// the ones its calls' own work made ([Iteration.Absorbed]): what a later
// segment's call log is seeded by, what a delivery check reads as calls
// ([Called], the dropped ones alone), and what a reader is told it is not
// shown ([FormatElided]).
//
// The package imports nothing from crewlet but internal/plainjson, a leaf
// over the standard library that holds the one rule for JSON read as text, so
// it still drags nothing behind it. The turn loop, the runner and the parked
// state all hold ledger values, and a ledger that dragged the provider stack
// behind it would be held by all three; a test holds the import list.
package ledger
