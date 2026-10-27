// Package query is THE SERVER-SIDE READS OF A REPOSITORY BY OBJECT ID: a
// tree's listing, a file's lines, a diff's stat or one file's patch, how two
// commits compare, and whether a commit landed in a branch and which push
// landed it — real git over a PINNED incarnation of the node's own cache
// (internal/code/cache), in the READ lane, every answer one page of at most
// code.MaxGitAnswerBytes of JSON with a cursor where the next begins. It runs
// on git nodes, holds no goroutine of its own, and writes nothing itself — the
// memo its reads fill is the cache's write: a read's processes run on its
// caller's goroutine and end with its context.
//
// # Which push, from the copy's index
//
// Every commit an answer names — its revision, a diff's or a comparison's
// commits and merge base, a landed-in read's commit and tip — is named with
// the push that introduced it (code.GitAnswer.PushedBy), read from the pinned
// copy's index of pushes (cache.Cache.Provenance), and the answer carries
// what that index holds (code.Coverage): both are in every page's envelope,
// measured into it before a page is cut. The index DECORATES and nothing
// waits on it: a copy whose index cannot be read yet — a run a previous
// process wrote, not yet checked — answers without either. A read this node
// answered as the repository's home (code.GitQuery.Home) marks its history
// wanted (cache.Cache.WantHistory), so the one node every walk asks first
// reads it back, once per fleet, for repositories seats ask about.
//
// A LANDED-IN READ (landed.go) is one ancestry check against the tip the
// asker resolved and the position it read it at, the copy brought there,
// then a segmented binary search of the branch's moves in the index — never a
// walk of the log or of the history — bounded by MaxLandedProbes. It names
// the first move after which every move kept the commit, and never a move the
// commit was reachable before: a landing older than the index's oldest move
// of the branch, or inside its gap, is answered with no push and the range it
// lies in (TestALandingTheIndexDoesNotHoldIsNotAttributed).
//
// # By object id, never by ref
//
// Every revision a read names is an object id, whole or abbreviated
// (code.ValidRev); what a ref points at is a row, which the code reads of the
// rows answer at whatever level their caller asks. An id names the same object
// at every position, so a read by one needs no level: the copy is read as it
// stands — or brought to the asker's floor, where it holds one — and an id the
// copy lacks is looked for ONCE more after a sync at the log's head (the asker
// may know of a push this node has not synced) before it is answered
// code.ErrObjectNotFound. That one look also stands behind every other refusal
// a copy could be behind about — a repository it calls deleted, a grant its
// row refuses — as the code host never refuses a person's git on a stale read
// (TestARefusalIsJudgedAtTheLogsHead): a refused read costs one barrier, and a
// served one none.
//
// AN ABBREVIATION IS EXPANDED FROM THE OBJECTS ALONE, by `git rev-parse
// --disambiguate=<hex>`, NEVER by cat-file's own reading of a short name,
// which tries it as a ref first: a tag or a branch named like an abbreviation
// would answer for another commit (objects.go has the measurement). One that
// matches more than one object is refused, asking for more of its digits.
//
// # Nothing a client wrote reaches git's command line
//
// A read's argument lists carry fixed options and validated whole ids — and
// `--disambiguate=` an abbreviation's validated digits, the one option
// argument a read builds. A PATH reaches git only on a standard input: `git
// cat-file --batch-check -Z`'s, which resolves `<id>:<path>` to the id of the
// tree or file it names, so every later process is handed that id, and `git
// diff-pairs -z`'s, which reads back the raw records diff-tree wrote, paths
// and all, to count or patch the files a diff read asks for (diff.go). A path
// is never a pathspec (a diff's paths are filtered here, over git's whole raw
// answer), and no option can hide in one: `--output=x` is a file's name there
// like any other. TestAPathReachesGitOnlyOnStandardInput holds every argument
// list a read runs to its template. diff-tree, diff-pairs and rev-list each
// take `--output`, which creates the file it names in the repository they run
// in: the writers gate (cache.TestNothingButSyncWritesACache) argues each of
// this package's run-time arguments — whole ids, and `-M` — in its allowance,
// and the template test is what keeps an `--output` out.
//
// # The share: half the read lane, around a read's long git only
//
// The read lane is also where a person's push scans, a sync round's peel and a
// fetch's want check wait, FIFO, for at most gitproc.SlotWait: sized past it, a
// burst of seat reads would refuse a person's push busy. So a read's own git —
// its kind's, the long part — runs under one unit of the node's seat git-work
// share (code.GitShare), half the read lane, taken after the read has found
// its objects and released the moment that git ends. A read waiting on its
// sync or its re-sync holds no read-lane slot and so no unit: held across a
// cold download, one read would refuse every other seat read on a node whose
// share is one (TestAReadWaitingOnItsSyncHoldsNoShare). The short processes
// that find a read's objects wait in the lane's own queue like any short read.
//
// # What this node cannot answer, another may
//
// A read this node cannot answer for a reason about THE NODE is
// code.ErrGitUnready, which the asker may take to another git node: its copy
// not brought to the read's need within statelog.ReadBudget or before its
// answer fell due (a cold copy, whose download goes on after the read stops
// waiting), or a sync that failed on what every node's copy is filled from —
// the code log, the object store, the rows, a repair the cache will not repeat
// (syncing); its share or its read lane full past their waits, or its waits
// for them or its own git running past its answer's due instant (share); a
// seat's read on a node holding no chart yet (no_chart) or confirmed behind
// the company's configuration (behind_config), where the grant is not judged
// at all — "this node cannot judge" is never "not permitted"; a failure of the
// node's own, a sync its disk or its stopping cache failed, or a short check
// of what the read names that ran its whole bound (node_failed); and a read of
// a repository this node holds no copy of, which it was not asked to fill
// (not_held). WHICH OF THEM IT IS DECIDES WHO FILLS: a walk lets one node at a
// time fill a copy it does not hold (code.GitQuery.Home), and moves that on to
// the next node only past a node that is bringing no copy up for the read —
// behind_config and node_failed — never past one filling, repairing or seconds
// from its chart, nor past one at its share, which holds its copy and is only
// busy (code.UnreadyReason.PassesFill). Every other refusal is about the read
// or the repository, and is the answer anywhere: a read whose own git ran the
// whole of its budget is code.ErrSliceTooLarge naming the narrowing, since
// another node would take as long.
//
// # Time: one budget of git, and an answer while the asker listens
//
// A read's own git — every process of its kind, a comparison's two walks and
// its stat or a landed-in search's ancestry checks as a listing's one — is
// given ONE budget of run time, gitproc.ReadDeadline, each process what the
// ones before it left: run out, the read is too large to answer anywhere. Its
// wait for each process's read-lane slot is charged nothing, since it is the
// node's contention and not the read's size. The short checks before it
// (objects.go) run to their own bound, outside it.
//
// And the caller's context ENDS WHEN THE ANSWER IS DUE (code.AnswerBy, the
// estate's git arm and its in-process run alike): every wait takes it, and a
// wait it ends is answered code.ErrGitUnready while the asker still listens
// (code.AnswerDue) — syncing during the sync, share during a wait for the
// node's git or a run of it — never the context's error and never too large:
// a run the due instant ended had less than its budget. So time the asker's
// attempt does not hold for — the read lane's queue and the short checks, left
// out of its sum — makes a read unready under load, never late
// (TestAReadWhoseAnswerFallsDueSaysSoWhileItsAskerListens,
// TestAReadsOwnGitHasOneBudgetItsProcessesShare).
//
// DAMAGE IS THE CACHE'S TO REPAIR, AND NO ANSWER: no process here names a
// gitproc.Repairer, so the cache's observer acts on what each reports at once
// (a derived file dropped and its round queued, a stored part's scrub made
// due), and a process whose Result.Damage is set — failed or not, since git
// answers some questions over damage it reports and exits 0 — gives no
// answer: the read is unready (syncing) while the node repairs. A repair under
// the request, as the code host's is, would hold the read for a round, a
// scrub or a rebuild no read's budget holds.
//
// # Who may read
//
// The grant is judged on the ANSWERING node, from its own epoch's chart and
// the repository's row as its copy holds it (code.GrantOf, ADR-0058): a
// person maintains and a token bound to nobody reads every repository, with no
// chart consulted; a seat reads where its tier and its relation let it, so a
// `visibility: unit` repository refuses a seat outside the owning unit
// code.ErrNotPermitted (TestAUnitRepositoryRefusesASeatOutsideIt). A read a seat
// makes for the colleague whose ask woke its turn (code.GitQuery.CeilingSeat)
// is judged for that colleague too, from the same chart and the same row, by
// the data node's one judgment (code.JudgeRead): one the colleague could not
// make itself is code.ErrAboveCeiling, even where the reading seat could
// (TestAReadMadeForASeatIsHeldToThatSeatsGrant). A deleted or purging
// repository is code.ErrRepoNotFound with the domain's sentence for it.
//
// # Who read it
//
// A read that answered a `visibility: unit` repository is recorded once, as
// its asker read it (Options.Fetched, types.CodeFetched via `tool`): by its
// kind, with the answer's encoded size — what the asker is sent beside its
// envelope. A page from the memo included: what an audit asks is who was handed
// the repository's contents, not who cost a git run, and a memo hit skipping
// the record would let a second reader of a page go unrecorded
// (TestAUnitRepositorysToolReadIsRecorded). A read of a `visibility: company`
// repository records nothing — every seat reads it by declaration — and nor
// does a read refused or unready, which handed nothing over. The engine
// publishes the record as an event of this node's own log, which its usage
// publisher derives into the day's repo_read record (internal/usage).
//
// # Pages
//
// A page is measured as its JSON, in encoding/json's own terms (page.go), and
// every cursor is a position in an answer that is a function of object ids
// alone, so a cursor one node issued is sound on any other: a listing's n-th
// entry, a stat's n-th file, a file's line or the byte inside an overlong one,
// a patch's byte. A listing's Total, a file's Lines and a diff's totals are the
// whole answer's on every page.
//
// # The memo
//
// Every page a read's git cuts is kept in the pinned copy's memo (the cache's,
// cache.MemoBatch), keyed by everything it is a function of, and a read that
// asks for a page already cut is answered from it with no git run (memo.go):
// asked as soon as the grant is judged for a read naming whole ids, before any
// git runs, since an entry is in a copy's memo only where a read of that copy
// found its objects there. git cannot seek inside an object or a listing, so
// the fiftieth page of a file is a read of everything before it: the run that
// misses cuts every page from the one asked to its answer's end and memoizes
// each, so reading a file page by page streams it once. And a read that
// misses a page another read of the node is cutting waits for that one and is
// answered from the memo it filled (read.await): seats asking one page at
// once — a review's fan-out right after a push — cost one git process, one
// write of the page's files and one unit of the share, where each ran its
// own.
//
// # Measured
//
// BenchmarkCodeRead over gittest.History's 32 MB composition — 44 files of one
// directory, every one rewritten in each of its 64 sessions, 14 080 commits
// pushed a session at a time — under a tree of 10 000 files of forty lines, a
// hundred to a directory, beside a 3 MB file of 60 000 lines (67 packs in
// all), five reads of each kind (-benchtime 5x, one count), git 2.56.0 on two
// E-cores of a Core Ultra 7 265U, the copy warm on the node that took the
// pushes, each the p50 and, in brackets, the slowest of five: the root's
// listing 3.1 ms (3.4); the whole tree's first page 26 ms (26) and a page nine
// tenths of the way through it 25 ms (26) — each lists the whole tree for its
// Total and looks up the size of every file in it, so a listing costs its
// tree, whatever page it is; the 3 MB file's first page 7.0 ms (7.4) and its
// fiftieth 7.2 ms (8.0) — each streams the whole file to count its lines; the
// stat of one session's pushes 4.1 ms (4.6); the stat of an edit across all
// 10 000 files 227 ms (231), and of the same edit filtered to one directory of
// a hundred 21 ms (22) — the walk of the two trees, and diff-pairs counting
// that directory's files alone; one file's patch from that edit 15 ms (15);
// and a comparison of the history's middle with its tip 13 ms (14), its two
// walks over half the history's commits. The root's listing on a node emptied
// first, which downloads the repository's 67 packs before it answers, took
// 118 ms (191) from the in-process store, and from the S3 fake — the fake's
// figures, not a bucket's: every request answered 100 ms after it arrived, 80
// MB/s a connection — 604 ms (638), and 1 095 ms (1 118) at 200 ms: five
// waves of the cache's FetchConcurrency GETs, each paying the first byte.
// None was unready: each answered inside statelog.ReadBudget. So every warm
// kind is more than an order of magnitude inside gitproc.ReadDeadline; what
// grows is a listing with its tree, a stat with the files it counts and a
// comparison with the history it walks; and code.GitShareWait keeps its
// two-second floor.
//
// THE MEMO, measured once over the same rig with it landed (-benchtime 5x,
// one count, the in-process arms alone, git 2.56.0 on the same two E-cores
// of a busier machine, whose warm arm read each kind in 4.6 to 262 ms where
// the run above read 3.1 to 227 — each ratio below is of one run's own arms):
// a read the memo misses (cache=miss) costs what the read with no memo
// (cache=warm) does within eight per cent, but for a file's pages, since it
// cuts every page to the file's end: the first 32 ms (p95 35), all 55 of
// them, against 9.5 with no memo, and the fiftieth 11.9 against 10.3. A hit
// (cache=memo) takes 21 µs for the root's listing, 79 and 90 for the whole
// tree's first page and the one nine tenths through it, 67 and 68 for the
// file's first and fiftieth, 35 for one session's stat, 76 and 57 for the
// wide edit's stat whole and filtered to one directory, 23 for the patch and
// 34 for the comparison: 130 to 3 723 times faster than the miss and 133 to
// 3 460 than the read with no memo, the wide edit's whole stat the most, every
// kind past the ten times the memo's premise asks, since a hit is one file's
// read and its decode where a miss is at least one git process.
//
// BenchmarkLandedIn over gittest.History's same 32 MB composition (14 080
// commits, nothing on top) with a branch moved through it 2 000 times by
// pushes, the commit asked about landed 1 000 moves ago, five reads of each
// arm, git 2.56.0 on four cores of the same Core Ultra 7 265U, the copy of a
// node that read the repository whole and so wrote its commit-graph at once: a
// landed-in read of a branch never forced takes 32 ms and 11 checks, one
// forced three times since 56 ms and 17, and one forced 66 times 284 ms and
// all MaxLandedProbes, answered with how far it reached — 2.6 to 4.4 ms a
// check, a git process each. One check walking by date over the same history,
// with no commit-graph — the commit against the branch's tip — takes 24 ms.
package query
