// Package home is a repository's HOME git node at work on its changes: the
// first git node of the repository's rendezvous order over the live git nodes
// (ADR-0059), the node its git reads are asked of first, is the one that owes
// its CHANGE WORK — reading each change's head revision against its target
// and recording what it found (internal/code's Revise), or landing a change
// whose head its target came to hold (internal/code's ChangeLanded). The rows
// are the work list: a `changes_due` read (internal/code's ReadChangesDue)
// names every live change whose head revision holds no facts against the
// branch it merges into, and every record the home lands takes its change off
// the list by its own row. Nothing here keeps a list of its own.
//
// # One loop, and the workers that register with it
//
// A [Loop] runs on every git node and schedules REPOSITORIES — a (worker,
// repository) pair a turn — onto a few goroutines of its own ([Concurrency]),
// for every [Worker] registered with it: the changes worker now
// ([WorkerChanges]), and M5's checks and merge queue and M6's mirrors after
// it, each a listing of the repositories it has work in (Worker.Due) and the
// work of one (Worker.Work), each read from the rows. A repository is worked
// by ONE turn of a worker at a time; asked again while it is being worked, it
// is worked once more after, at the furthest floor it was asked at, so a
// burst of records costs a repository a turn or two, never one a record.
//
// # Two ways a repository is scheduled, and the rows decide either
//
// A NUDGE: one watch on the code log's repository subjects (`crewlet.code.log.
// repo.>`, [Nudges]) tells the loop the subject and the position of every
// record a repository's subject takes — a push, a change's open, edit or
// retarget — as the broker stores it, headers alone; the loop asks its
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
// superseded, a landing of a change merged already is answered unchanged. A
// node that cannot read who serves git works nothing that turn rather than
// guess.
//
// # What a turn of the changes worker does
//
// It reads the repository's due changes — at the nudge's floor, or stale on a
// backstop pass — with the repository's path owners in the same snapshot, and
// for each: asks the repository's git reads for the revision's facts
// (internal/code's GitRevisionFacts, the engine's own read, through the
// router, which asks the home first — this node — and its runner-up when the
// home's copy cannot answer), then either lands the change — its head inside
// its target's tip, whatever push moved the target there — through the
// repository's committer, decided against the change's head and the tip the
// home read; or matches the touched paths against the path owners
// (code.MatchOwners) and asks the change writer to record the facts, the
// owners matched and the digest of the owners matched against. A refusal that
// the rows moved since — superseded, stale, a change closed or merged — is
// the rows answering, left to the next pass, which reads them again; one that
// decided nothing for a reason of the writer's or the log's — busy, the
// record refused or dropped — fails the turn, and the next pass lists the
// repository again.
//
// # Its bounds
//
// A TURN IS ONE BACKSTOP LONG: a repository with more due work than a turn
// finishes yields its goroutine to the next repository, and the next pass
// lists it again. CONCURRENCY TWO: a turn spends its time waiting on reads and
// records — a facts read holds one unit of the answering node's seat git-work
// share for its git, a record one of its repository's committer's turns — so
// two repositories at once keep a burst of nudges from queueing behind one
// large repository's turn. On a git node of four cores or fewer, two facts
// reads at once are its whole share, which a seat's read waits for at most
// code.GitShareWait before another git node answers it (PROVISIONAL, as M4's
// spec marks it, until BenchmarkHomeLoop measures a home's turns: [Concurrency]
// says what moves it).
//
// ITS IMPORTS: internal/code/query, internal/code/compact, internal/code/host
// and this package never import each other (home_test.go holds the four):
// each reaches the others' work through the estate's router — a facts read
// through its git walk, a record through its repository's committer — so a
// home's turn is the same whichever node answers each step of it.
package home
