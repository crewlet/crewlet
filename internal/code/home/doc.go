// Package home is a repository's HOME git node at work on its changes: the
// first git node of the repository's rendezvous order over the live git nodes
// (ADR-0059), the node its git reads are asked of first, is the one that owes
// its CHANGE WORK — reading each change's head revision against its target
// and recording what it found (internal/code's Revise), merging it into its
// target's tip and recording whether it merges cleanly (internal/code's
// Mergeability), or landing a change whose head its target came to hold
// (internal/code's ChangeLanded) — and REFRESHING a change's stored readiness
// where what it reads moved since its merge judged it, merging nothing. The
// rows are the work list: a `changes_due` read (internal/code's
// ReadChangesDue) names every live change whose head revision holds no facts
// or no mergeability against the branch it merges into, whose readiness its
// review, an ask, an answer to its findings, its facts or its own row moved
// past, or whose moved target asks its merge again, and every record the home
// lands takes its change's work off the list by its own row. Nothing here
// keeps a list of its own.
//
// # One loop, and the workers that register with it
//
// A [Loop] runs on every git node and schedules REPOSITORIES — a (worker,
// repository) pair a turn — onto a few goroutines of its own ([Concurrency]),
// for every [Worker] registered with it: the changes worker
// ([WorkerChanges]), the checks worker ([WorkerChecks]) and the merge queue
// ([WorkerQueue]), and the imports and pull mirrors (internal/code/mirror,
// which the engine registers), each a listing of the
// repositories it has work in (Worker.Due) and the work of one
// (Worker.Work), each read from the rows. A repository is worked by ONE turn
// of a worker at a time; asked again while it is being worked, it
// is worked once more after, at the furthest floor it was asked at, so a
// burst of records costs a repository a turn or two, never one a record.
//
// # Two ways a repository is scheduled, and the rows decide either
//
// A NUDGE: a watch on the code log's repository subjects (`crewlet.code.log.
// repo.>`) and one on its repositories' checks subjects (`crewlet.code.log.
// checks.>`, [Nudges]) tell the loop the subject and the position of every
// record a repository's subject takes — a push, a change's open, edit or
// retarget — and every one its checks take — an attempt queued, claimed or
// ended, a head's dispatch — as the broker stores it, headers alone; the loop asks its
// placement whether this node is the repository's home ([Placement.Home]) and
// schedules it only where it is, AT THE RECORD'S POSITION, so the read that
// follows waits for this node's copy to hold the record before it reads the
// rows, rather than reading the rows from before it and finding nothing to
// do. A watch that may have missed records (a Gap) schedules a backstop pass
// at the position it reaches.
//
// THE BACKSTOP: every [Backstop] — a minute, and at once at Start — the loop
// asks every worker for its listing and its placement which of the listed
// repositories this node is home to, against ONE reading of who serves git
// ([Placement.HomeOf]), and schedules those. So a nudge only shortens the
// wait: a nudge lost — its consumer gone, a member restarting, the node's
// placement unreadable for a moment — leaves a repository's work for the next
// pass, a minute at the most, and never undone; and a repository's home
// moving to another node moves its work with it, the new home finding it in
// its next listing.
//
// NO LEASE AND NO EPOCH, as the compactor's (ADR-0060): two nodes that each
// take themselves for a repository's home — a presence view split for a
// moment — both read and both propose, and the records' own guards take one:
// facts of a revision no later than the facts held of it are refused
// superseded, so is a merge into a tip no later than a decided one, and a
// landing of a change merged already is answered unchanged. A node that
// cannot read who serves git works nothing that turn rather than guess.
//
// # What a turn of the changes worker does
//
// It reads the repository's due changes — at the nudge's floor, or stale on a
// backstop pass — with the repository's path owners in the same snapshot on
// a page naming a change owing its facts, and for each change owing its
// FACTS: asks the repository's git reads for the revision's facts
// (internal/code's GitRevisionFacts, the engine's own read,
// through the router, which asks the home first — this node — and its
// runner-up when the home's copy cannot answer), then either lands the change
// — its head inside its target's tip, whatever push moved the target there —
// through the repository's committer, decided against the change's head and
// the tip the home read; or matches the touched paths against the path owners
// (code.MatchOwners) and asks the change writer to record the facts — the
// trees of the base and the head and the touched paths' classes among them,
// as the git node read them — the owners matched and the digest of the owners
// matched against. Only FINAL facts are recorded
// (code.RevisionFactsAnswer.Final): a git node that ran its read's budget out
// before the patch id or the classes says so apart from a bound's omission,
// and such facts measure the node's moment, not the commits — recorded, they
// would hold for good, since a revise takes a revision's facts against a tip
// once — so they are the turn's failure for that change, which stays due its
// facts for the next pass. And for each
// change owing its MERGE — a new head revision, or one its moved target asks
// again within the repository's budget, the oldest merge first (merge.go) — it
// merges the change over this node's own copy, in a scratch beside it, and
// asks the change writer to record what the merge did, or lands the change
// where its target came to hold its head. And for each change owing no merge
// whose readiness is due again — what its readiness reads moved since its
// merge judged it, or this turn recorded its facts — it asks the change writer
// for a REFRESH (internal/code's Mergeability.Refresh): no copy synced, no
// scratch and no git, the writer judging the readiness again over the merge
// the row holds. A change-subject record — a review, an ask — nudges no
// repository: the watch sees subjects alone, and a change's subject names no
// repository, so its refresh waits the backstop, a minute at the most. A
// refusal that the rows moved since
// — superseded, stale, a change closed or merged — is the rows answering, left
// to the next pass, which reads them again; one that decided nothing for a
// reason of the writer's or the log's — busy, the record refused or dropped —
// or a merge that could not run fails the turn, and the next pass lists the
// repository again.
//
// # What a turn of the checks worker does
//
// It reads the repository's check work — the `checks_due` listing
// (internal/code's ReadChecksDue), a target's reading due again once it is
// [ChecksFileRefresh] old — and asks the repository's checks writer
// (code.checks): on its first page, for each TARGET whose branch moved past
// the tip its checks file was read at, the file read at its tip and recorded,
// so only a file that changed makes the heads waiting on it due again; then
// for each change, each with what it owes (code.CheckWork), in order: the next
// attempt of each check its head's infrastructure ended (a requeue); where its
// DISPATCH is owed, the attempts its change moved on from ended cancelled
// superseded, then every check the checks file at its TARGET'S TIP defines —
// never the head's: a change is never measured by a definition it brought —
// that the head holds no attempt of queued, or recorded skipped where what
// the head touches against that tip matches none of its paths, one an
// earlier dispatch errored for a file that did not run queued as its next
// attempt, and every name the target's gate requires errored defs_invalid
// where the file is none that runs. The page's dispatches are recorded LAST
// (code.CheckDispatched, a target's heads in records of at most
// code.MaxDispatchHeads), so a dispatch cut short is the next turn's, every
// queue that landed answered from its row. The tip's file is read through the
// git reads (internal/code's GitChecksFile, the engine's own read), once a
// tip and parsed once a blob a turn; what a head touches is its revision's
// facts, read as the change work reads them. Its refusals are the change
// work's: the rows moving is the next pass's, a write deciding nothing the
// turn's failure.
//
// # What a turn of the merge queue does
//
// One repository's queue work, a target a page (`queue_due`), each target's
// next step decided from ONE snapshot of its tip, its gate, its live entries
// and its live train: a train BUILT where none stands on the target's tip —
// candidates of [BatchOf] entries, up to the gate's depth, as one chain this
// node builds ([Chains]) and one candidate record; a standing train's tips
// queued the required checks the train's BASE defines; its longest green
// prefix LANDED in one land record; a red first candidate BISECTED over its
// entries' commits and its culprit ejected. Nothing of a train lives in the
// worker: a home that takes a repository over resumes its queue from the
// rows. Each landed entry that asked it has its work item closed as its
// enqueuer, through [Tasks] — the engine's tracker, which this package does
// not import. queue.go has the rules of each step.
//
// # Merge previews, beside the loop
//
// The same merge answers a comparison's MERGE PREVIEW ([Previews]): a read a
// principal makes of a repository's git nodes, served beside the node's git
// reads from exactly their inputs and answered as they are, so a preview and
// the change it would open say one thing. It shares the merge with the loop
// and nothing else.
//
// # Its bounds
//
// A TURN IS ONE BACKSTOP LONG: a repository with more due work than a turn
// finishes yields its goroutine to the next repository, and the next pass
// lists it again. CONCURRENCY TWO: a turn spends its time waiting on its reads
// and its records — a record one of its repository's committer's turns — so
// two repositories at once overlap one's records with the other's read, and a
// burst of nudges does not queue behind one large repository's records
// ([Concurrency] states what BenchmarkHomeLoop measured of it). ITS OWN GIT
// READS are bounded apart, every worker's together ([OwnReads]): each holds a
// unit of the answering node's seat git-work share for its git — counted as
// the engine's own (code.SeatWorkEngine) — and a seat's read waits for a unit
// at most code.GitShareWait before it is sent on, so the home runs ALL BUT
// ONE of its node's units at once, at least one: wherever the share has two
// units the seats keep one, and on a node of one (three cores or fewer) a seat
// waits behind one background read at the most, the turns past the bound
// waiting in the home rather than ahead of the seats in the share's own
// queue. A MERGE holds one slot of the node's maintenance lane,
// which never waits — a lane full of other maintenance answers the merge busy,
// the turn's failure — within [MergeabilityDeadline]; a moved target's merges
// are at most [MergeabilityPerPass] a repository a window, each once
// [MergeabilityRefresh] old.
//
// ITS IMPORTS: internal/code/query, internal/code/compact, internal/code/host,
// internal/code/checkrun, internal/code/mirror and this package never import
// each other (home_test.go holds the six) — the check runner's sweep of the
// checks whose runner is gone and a node's imports and pull mirrors are each a
// [Worker] the engine registers, satisfying this package's interface without
// importing it:
// each reaches the others' work through the estate's router — a facts read
// through its git walk, a record through its repository's committer — so a
// home's turn is the same whichever node answers each step of it.
package home
