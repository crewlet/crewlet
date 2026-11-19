// Package query is THE SERVER-SIDE READS OF A REPOSITORY BY OBJECT ID: a
// tree's listing and each entry's last commit (treecommits.go), a file's lines,
// a diff's stat and patches (patches.go), a comparison, whether a commit landed
// in a branch and which push landed it, a path's history (log.go), blame
// (blame.go), a file found by name (find.go) — and of a change's revisions, by
// the ids their rows record: facts (facts.go), re-anchoring (anchor.go),
// interdiff (interdiff.go), excerpts around findings (excerpts.go) — the checks
// file a commit holds (checksfile.go) and its deploy and dependency files
// (codefiles.go) — real git over a PINNED incarnation of the node's own cache
// (internal/gitlet/cache), with its dropping view (gitproc.Cmd.Dropping), so an
// object an expunge is dropping is still served by id until its commit, in the
// READ lane. Every answer is one page of at most gitlet.MaxGitAnswerBytes of
// JSON with a cursor where the next begins; a checks file's is one answer of at
// most gitlet.MaxChecksFileBytes.
// It runs on Gitlet nodes, holds no goroutine of its own, and writes nothing
// itself: the memo its reads fill and the spool a revision's facts write a patch
// into are the cache's writes. A read's processes run on its caller's goroutine
// and end with its context, or with the server's stop.
//
// # The stop
//
// Whoever stops Gitlet stops these reads first (Server.Stop), before the cache
// they read: every read after it is answered unready about the node
// (node_failed), starting nothing, and every read in flight is CUT — its waits
// and its git end — answered the same while its asker still listens, and
// waited for. So once the stop returns no read of this node is being answered
// or recorded (Options.Fetched), which the engine's teardown orders before the
// usage day's last flush. Cut, not waited out: a read may run its asker's whole
// attempt, longer than the stop's budget.
//
// # Which push, from the copy's index
//
// Every commit an answer names — its revision, a diff's or a comparison's
// commits and merge base, a landed-in read's commit and tip, every commit a
// history's or a blame's page names — is named with the push that introduced it
// (gitlet.GitAnswer.PushedBy), read from the pinned copy's index of pushes
// (cache.Cache.Provenance), and the answer carries what that index holds
// (gitlet.Coverage): both are measured into every page's envelope before a page
// is cut, or into a commit's own cost where the cut finds it, so a page still
// fits. The index DECORATES and nothing
// waits on it: a copy whose index cannot be read yet answers without either. A
// read answered as the repository's home (gitlet.GitQuery.Home) marks
// its history wanted (cache.Cache.WantHistory), so the one node every walk asks
// first reads it back, once per fleet.
//
// The index also NARROWS a history (gitlet.LogQuery.PushedBy, log.go), so such
// a page is the node's and not the ids': never unstamped, never memoized
// (gitlet.GitQuery.ReadsIndex).
//
// A LANDED-IN READ (landed.go) is one ancestry check against the tip the asker
// resolved, the copy brought to the position it read it at, then a segmented
// binary search of the branch's moves in the index — never a walk of the log —
// bounded by MaxLandedProbes and by the read's one budget of git, either of
// which ends it saying how far it reached
// (TestALandingSearchTheBudgetEndsSaysHowFarItReached), as do the read's answer
// falling due and a read lane busy past gitproc.SlotWait: each check queues for
// a slot of its own, a wait the budget does not charge
// (TestALandingSearchItsAnswerFallingDueOrABusyLaneEndsSaysHowFarItReached). It
// names the first move after which every move kept the commit, never a move the
// commit was reachable before: a landing older than the index's oldest move of
// the branch, or inside its gap, is answered with no push and the range it lies
// in (TestALandingTheIndexDoesNotHoldIsNotAttributed). MaxLandedProbes rests on
// one check's git, a process each, at 6.7 ms p99 over 14 080 commits.
//
// # By object id, never by ref
//
// Every revision a read names is an object id, whole or abbreviated
// (gitlet.ValidRev); what a ref points at is a row, which the code reads of the
// rows answer at whatever level their caller asks. An id names the same object
// at every position, so a read by one needs no level: the copy is read as it
// stands, or brought to the asker's floor where it holds one, and an id the copy
// lacks is looked for ONCE more after a sync at the log's head (the asker may
// know of a push this node has not synced) before it is answered
// gitlet.ErrObjectNotFound. That one look also stands behind every other refusal
// a copy could be behind about — a repository it calls deleted, a grant its row
// refuses — as Gitlet never refuses a person's git on a stale read
// (TestARefusalIsJudgedAtTheLogsHead). A read whose asker names the head it read
// (gitlet.GitQuery.Head: a search of many repositories) syncs to that head
// instead, so a company-wide search naming a ref most repositories lack costs
// the one barrier its listing took, not one a repository
// (TestASecondLookSyncsToTheHeadItsAskerRead).
//
// AN ABBREVIATION IS EXPANDED FROM THE OBJECTS ALONE, by `git rev-parse
// --disambiguate=<hex>`, NEVER by cat-file's own reading of a short name, which
// tries it as a ref first: a tag or branch named like an abbreviation would
// answer for another commit (objects.go). One matching more than one object is
// refused, asking for more digits.
//
// Only the tags listing (floored at gitlet.RepoDetail.Version) and a code
// search (search.go, which resolves its ref with `rev-parse --verify`) read what
// refs name.
//
// # Nothing a client wrote reaches git's command line as an option
//
// A read's argument lists carry fixed options, validated whole ids and numbers
// the server formats (`--disambiguate=` an abbreviation's validated digits, a
// history's `--max-age=`, `--min-age=` and `--skip=`, a blame's `-L`). A
// search's text reaches git only as the ONE argument of `-e`, the plan's escaped
// alternation (codeindex.Plan.Grep), which git reads as a pattern whatever it
// spells; its ref follows `--end-of-options`, held to the ref grammar before
// (search.go). A PATH is data: it reaches git on a standard input
// (`git cat-file --batch-check -Z` resolves `<id>:<path>` to an id, so every
// later process is handed that id; `git diff-pairs -z` reads back diff-tree's
// raw records, diff.go) or, for the three reads whose git reads no path there,
// after `--`, where git reads an argument as a path and never an option: a
// history's path as the ONE argument, a literal pathspec (`:(literal)<path>`,
// log.go), a blame's as itself, blame taking a path rather than a pathspec
// (blame.go), and a directory walk's entries as literal pathspecs
// (treecommits.go). Never `log --stdin`, which takes a carriage return off a
// path's end. A diff's paths are filtered here over git's whole raw answer; an
// interdiff's are looked up at both heads as `<head>:<path>` lines of one
// cat-file's input (interdiff.go), so `--output=x` is a file's name like any
// other; a re-anchoring's diff-pairs is handed no path (anchor.go), nor an
// excerpt's (excerpts.go).
// TestAPathReachesGitOnlyAsData holds every argument list a read runs to its
// template. diff-tree, diff-pairs, rev-list, log and blame each take
// `--output`, which creates the file it names in the repository they run in:
// the writers gate (cache.TestNothingButSyncWritesACache) argues each run-time
// argument in its allowance, and the template test keeps an `--output` (and git
// grep's `--open-files-in-pager`, which runs a program) out.
//
// # The share: half the read lane, around a read's long git only
//
// The read lane is also where a person's push scans, a sync round's peel and a
// fetch's want check wait, FIFO, for at most gitproc.SlotWait: sized past it, a
// burst of seat reads would refuse a person's push busy. So a read's own git —
// its kind's, the long part — runs under one unit of the node's seat git-work
// share (gitlet.GitShare), half the read lane, taken after the read has found
// its objects and released the moment that git ends. A read waiting on its sync
// holds no read-lane slot and so no unit: held across a cold download, one read
// would refuse every other seat read on a node whose share is one
// (TestAReadWaitingOnItsSyncHoldsNoShare).
//
// # What this node cannot answer, another may
//
// A read this node cannot answer for a reason about THE NODE is
// gitlet.ErrGitUnready, which the asker may take to another Gitlet node: its
// copy not brought to the read's need within statelog.ReadBudget or before its
// answer fell due, or a sync that failed on what every node's copy is filled
// from (syncing); its share or read lane full past their waits, or its own git
// running past its answer's due instant (share); a seat's read on a node
// holding no chart yet (no_chart) or confirmed behind the company's
// configuration (behind_config), where the grant is not judged at all — "this
// node cannot judge" is never "not permitted"; a failure of the node's own, a
// short check that ran its whole bound, or a read the stop refused or cut
// (node_failed); and a repository this node holds no copy of, which it was not
// asked to fill (not_held). WHICH OF THEM IT IS DECIDES WHO FILLS: a walk lets
// one node at a time fill a copy it does not hold (gitlet.GitQuery.Home), and
// moves on only past a node that is bringing no copy up for the read —
// behind_config and node_failed — never past one filling, repairing or seconds
// from its chart, nor past one at its share, which holds its copy and is only
// busy (gitlet.UnreadyReason.PassesFill). Every other refusal is about the read
// or the repository, and is the answer anywhere: a read whose own git ran its
// whole budget is gitlet.ErrSliceTooLarge naming the narrowing, since another
// node would take as long.
//
// # Time: one budget of git, and an answer while the asker listens
//
// A read's own git — every process of its kind, a comparison's walks and stat,
// a patch's lookup and diff, a landed-in search's checks, a revision's facts'
// six as a listing's one — is given ONE budget of run time, gitproc.ReadDeadline,
// each process what the ones before it left. Run out, the read is too large to
// answer anywhere — save a landed-in search, which answers how far it reached
// (landed.go), a revision's patch-id, omitted rather than refused, and its
// classes, answered incomplete (facts.go), and a history's walk, whose page
// answers the commits it reached with its cursor past them (log.go). A wait for
// a process's read-lane slot is charged nothing, being the node's contention and
// not the read's size: the budget holds a read's git to its run time, never its
// wall time. A revision's facts run some 114 MB of patch a second, so
// MaxPatchIDDiffBytes' facts fit a quarter of the budget.
//
// And the caller's context ENDS WHEN THE ANSWER IS DUE (gitlet.AnswerBy): every
// wait takes it, and a wait it ends is answered gitlet.ErrGitUnready while the
// asker still listens (gitlet.AnswerDue) — syncing during the sync, share during
// a wait for the node's git or a run of it, a history's walk that reached a
// commit answering those it reached, a landed-in search answering how far it
// reached — never the context's error and never too large: a run the due instant
// ended had less than its budget. So time the asker's attempt does not hold for
// makes a read unready under load, never late
// (TestAReadWhoseAnswerFallsDueSaysSoWhileItsAskerListens,
// TestAReadsOwnGitHasOneBudgetItsProcessesShare).
//
// DAMAGE IS THE CACHE'S TO REPAIR, AND NO ANSWER: no process here names a
// gitproc.Repairer, so the cache's observer acts on what each reports at once,
// and a process whose Result.Damage is set — failed or not, since git answers
// some questions over damage it reports and exits 0 — gives no answer: the read
// is unready (syncing) while the node repairs. A repair under the request would
// hold the read past any budget.
//
// # A directory's last commits
//
// Each entry's commit is the one `git log -1 -- <entry>` names (treecommits.go):
// git simplifies history per path, following at a merge the first parent the
// entry's files are unchanged against and naming the merge where they differ
// from every parent, and it reads files, so a directory that never held one has
// no last commit. One walk of the directory as a whole would follow the
// directory's own history and name another commit for an entry a merge took from
// the side the directory did not.
//
// So ONE first-parent walk reads the directory's line: `log --first-parent -z
// --root --name-status --no-renames --diff-merges=first-parent
// --format=%H%x00%P`, the directory a literal pathspec after `--` (none at the
// root). `--name-status` frames the stream, so no name reads as an id; `%P` is read at any width. A commit of one
// parent answers the entries it changed; an entry a merge changed is left there
// and walked alone (`log -1 <merge> -- <entry>`). Once the walk has read 1 024
// records with at most 64 entries left, it ends and they go on in walks narrowed
// to their own literal pathspecs, at most 32 entries and 64 KiB of them a walk,
// which the changed-path filters serve for many paths at once.
//
// WINDOWS of answers are kept under the walk's first record, the commit every
// later commit shares answers with until the directory next changes, and the
// group walk probes for them at its first 64 records (cache.MemoProbe), so a
// push is answered by a walk of two records. A window begins at a name whose
// FNV-1a hash is 0 modulo its size, or 256 entries into one, and is trusted only
// for the names it holds, so an entry added or removed misreads nothing.
//
// The walks leave a tenth of the read's budget for the log of the page's
// commits; a page the budget cut holds what was answered, is marked
// out_of_time, and is never kept nor answered immutable. Once the page's own
// entries are answered, the walks leave only complete windows for the memo, and
// running out of budget, lane or time ends them without failing the page.
//
// # Who may read
//
// The grant is judged on the ANSWERING node, from its own epoch's chart and the
// repository's row as its copy holds it (gitlet.GrantOf, ADR-0088): a person
// maintains and a token bound to nobody reads every repository, with no chart
// consulted; a seat reads where its tier and its relation let it, so a
// `visibility: unit` repository refuses a seat outside the owning unit
// gitlet.ErrNotPermitted (TestAUnitRepositoryRefusesASeatOutsideIt). A read a
// seat makes for the colleague whose ask woke its turn
// (gitlet.GitQuery.CeilingSeat) is judged for that colleague too, from the same
// chart and row, by the data node's one judgment (gitlet.JudgeRead): one the
// colleague could not make itself is gitlet.ErrAboveCeiling, even where the
// reading seat could (TestAReadMadeForASeatIsHeldToThatSeatsGrant). A deleted
// or purging repository is gitlet.ErrRepoNotFound.
//
// # Who read it
//
// A read that answered a `visibility: unit` repository is recorded once, as its
// asker read it (Options.Fetched, types.CodeFetched via `tool`): by its kind,
// with the answer's encoded size. A page from the memo included: what an audit
// asks is who was handed the repository's contents, not who cost a git run, and
// a memo hit skipping the record would let a second reader go unrecorded
// (TestAUnitRepositorysToolReadIsRecorded). A read of a `visibility: company`
// repository records nothing — every seat reads it by declaration — and nor does
// a read refused or unready, which handed nothing over. The reads ledger
// (internal/gitlet/readaudit) publishes the records OFF THE READ'S PATH, so no
// answer waits on the broker; internal/usage derives repo_read from them.
//
// # Many tips at once
//
// A tips read (tips.go) answers up to gitlet.MaxTips objects with one objects
// check, tag batch, log and ahead/behind count under one budget of git, and
// memoizes each tip's facts and counts.
//
// # The copy's tags
//
// A tags read (tags.go) is answered from the copy's tag index with no git, newest
// first, by a keyset cursor its asker floors at the repository's last record. A
// history's page names each commit's tags (gitlet.TagMarks).
//
// # Pages
//
// A page is measured as its JSON, in encoding/json's own terms (page.go), and
// every cursor but the tags listing's is a position in an answer that is a
// function of object ids alone, so a cursor one node issued is sound on any
// other; the tags listing's is the last tag's key, `d:<seconds>:<name>`, which
// any node's listing resumes past. The positions: a listing's n-th entry, a
// stat's n-th file, a file's line or the byte inside an overlong one, a patch's
// byte, a history's n-th commit, a blame's line. A listing's Total, a file's
// Lines and a diff's totals are the whole answer's on every page. A history's
// and a blame's pages carry text the code tools fence — every commit's subject,
// every blamed line — and are measured AS SENT, each field fenced and every
// string neutralised (page.go), so a fenced page is never refused as too large;
// each field git leaves unbounded is described past its bound rather than cut.
//
// # The memo
//
// Every page a read's git cuts — but a landed-in read's, a revision's facts', a
// re-anchoring's and a checks file's (memoizable says why), and a history's page
// its budget or due instant cut short, or one narrowed by who pushed it — is
// kept in the pinned copy's memo (cache.MemoBatch), keyed by everything it is a
// function of, and a read asking for a page already cut is answered from it with
// no git run (memo.go): asked as soon as the grant is judged for a read naming
// whole ids, since an entry is in a copy's memo only where a read of that copy
// found its objects there. git cannot seek inside an object or a listing, so
// the fiftieth page of a file is a read of everything before it: the run that
// misses cuts every page from the one asked to its answer's end and memoizes
// each, so reading a file page by page streams it once. A read that misses a
// page another read of the node is cutting waits for that one and is answered
// from the memo it filled (read.await): a review's fan-out right after a push
// costs one git process, one write of the page's files and one unit of the
// share.
//
// A FIND (find.go) keeps nothing of its own: it walks the tree's recursive
// listing as the memo keeps it, running git at most once per tree.
//
// # Measured
//
// At 100 000 files a find took 407 ms the first time and 77 ms after, far inside
// the budget, so no index of names is kept beside the memo. A stat counts only
// the files its paths admit (diff.go): one directory's 18 of 5 866 changed files
// took 6 ms, the whole stat 0.9 s. A floor at a ref's resolve read position
// forces a sync round costing twenty times the read it delays; a floor at where
// the ref moved costs nothing, so a code tool floors a read by a ref there. A
// re-anchoring keeps no hunk (anchor.go), so its memory stays flat as the hunks
// grow.
package query
