// Package query is THE SERVER-SIDE READS OF A REPOSITORY BY OBJECT ID: a
// tree's listing, a file's lines, a diff's stat or one file's patch, and how
// two commits compare — real git over a PINNED incarnation of the node's own
// cache (internal/code/cache), in the READ lane, every answer one page of at
// most code.MaxGitAnswerBytes of JSON with a cursor where the next begins.
// It runs on git nodes, holds no goroutine of its own, and writes nothing: a
// read's processes run on its caller's goroutine and end with its context.
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
// argument a read builds. A PATH reaches git only on `git cat-file
// --batch-check -Z`'s standard input, which resolves `<id>:<path>` to the id
// of the tree or file it names, so every later process is handed that id; a
// path is never a pathspec (a diff's paths are filtered here, over git's whole
// answer), and no option can hide in one: `--output=x` is a file's name there
// like any other. TestAPathReachesGitOnlyOnStandardInput holds every argument
// list a read runs to its template. diff-tree, diff and rev-list each take
// `--output`, which creates the file it names in the repository they run in:
// the writers gate (cache.TestNothingButSyncWritesACache) argues each of this
// package's run-time arguments — whole ids, and `-M` — in its allowance, and
// the template test is what keeps an `--output` out.
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
// not brought to the read's need within statelog.ReadBudget (a cold copy,
// whose download goes on after the read stops waiting) or a sync that failed
// here (syncing); its share or its read lane full past their waits (share); a
// seat's read on a node holding no chart yet (no_chart) or confirmed behind
// the company's configuration (behind_config), where the grant is not judged
// at all — "this node cannot judge" is never "not permitted". Every other
// refusal is about the read or the repository, and is the answer anywhere: a
// read the deadline ended is code.ErrSliceTooLarge naming the narrowing, since
// another node would take as long.
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
// code.ErrNotPermitted (TestAUnitRepositoryRefusesASeatOutsideIt). A deleted or
// purging repository is code.ErrRepoNotFound with the domain's sentence for it.
//
// # Pages
//
// A page is measured as its JSON, in encoding/json's own terms (page.go), and
// every cursor is a position in an answer that is a function of object ids
// alone, so a cursor one node issued is sound on any other: a listing's n-th
// entry, a stat's n-th file, a file's line or the byte inside an overlong one,
// a patch's byte. A listing's Total, a file's Lines and a diff's totals are the
// whole answer's on every page. A page past the first runs its git again: git
// cannot seek inside an object or a listing, so the fiftieth page of a file is
// a read of everything before it.
//
// # Measured
//
// BenchmarkCodeRead over gittest.History's 32 MB composition (about 2 800
// files and 13 000 commits, pushed a session at a time, plus a 3 MB file of
// 60 000 lines), five reads of each kind (-benchtime 5x, one count), git
// 2.56.0 on four cores of a Core Ultra 7 265U, the copy warm on the node that
// took the pushes: the root's listing 5.2 ms (the slowest of five 5.7), the
// whole tree's first page 5.1 ms (6.1), the 3 MB file's first page 9.7 ms
// (10.3) and its fiftieth 10.0 ms (10.4) — each streams the whole file to
// count its lines — the stat of one session's pushes 5.3 ms (6.0), one file's
// patch from it 8.4 ms (9.2), and a comparison of the history's middle with its
// tip 75 ms (75), its two walks over half the history's commits; and the root's listing
// on a node emptied first, which syncs the repository whole from the
// in-process store before it answers, 104 ms (111), never unready. So every
// warm kind is two orders of magnitude inside gitproc.ReadDeadline, the
// comparison's walk is what grows with a history, and code.GitShareWait keeps
// its two-second floor.
package query
