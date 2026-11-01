// Package cache is THE NODE'S DISPOSABLE BARE REPOSITORIES: one per repository
// a git node has been asked to serve, from which real git upload-pack answers
// clones and fetches.
//
// # A cache is a function of rows
//
// A repository is ROWS in the code domain (internal/code): its refs, its live
// packs and its settings, at a log position. A cache answers "this node alone"
// as files rather than tables — nothing in it is ever the only copy of
// anything, the engine's backup never copies it (and its directory carries a
// CACHEDIR.TAG, so an operator's backup run with --exclude-caches skips it
// too), and deleting the whole directory loses nothing but time: what a cache
// holds is derived from the rows and the object store, and is derived again
// when it is gone. Its refs are exactly the repository's
// code_refs at the position its state names, its packs exactly the
// code_packs there, and [Cache.Sync] writes it by CONVERGENCE — derive the
// target from the rows, make the directory equal to it — never by replaying
// what happened (ADR-0077). TestNothingButSyncWritesACache holds that from
// both sides: the files under internal/code that write the filesystem or run
// a git process that may write are a reviewed list, each with its reason, and
// seeded interleavings of what a node's cache meets — pushes, refused
// pushes, adoptions, syncs killed mid-step, crashes, evictions, rebuilds,
// maintenance, a log recreated backwards, expunges crashed at each of their
// steps — end every time in caches equal to ones rebuilt from nothing at the
// same position.
//
// # Convergence, priced for a push on every node
//
// A sync after a push on another node is a DELTA: the rows that moved since
// this cache's position, usually one pack and one ref. So the steady-state
// work is proportional to what changed, never to the repository: the state
// is a handful of scalars, rewritten only when the directory changed — a
// repository fetched a hundred times a second while nobody pushes writes
// nothing — and a delta neither lists nor sweeps the directory. The FIRST
// round of a repository in each process asks for the WHOLE answer and
// sweeps the directory to it (the image: HEAD, config, packed-refs, the
// state, empty refs/heads and refs/tags, the packs the rows name and the
// index over them), which is what makes a cache exact from any starting
// point — a crash, a full disk, an operator's rm, a hand edit, a log
// restored behind it while the node was down — at the price of one full read
// per repository per boot, reusing every pack it already holds. Until that
// round, nothing is answered at once from the state a previous process left:
// it names positions of the log it was read from, which a restored log
// reuses for other records. Every later round is a delta, because nothing but
// this process can have written the directory since.
//
// A cache NEVER MOVES BACKWARDS FOR A STALE COPY: a delta from a copy behind
// it, on the stream it read from, changes nothing — a client that fetched
// from this node must not see its refs go back. It does go back when the LOG
// did, which only a read at the log's head can say. A log RECREATED under it
// — provisioned anew, its stamp not the one the cache read under — shows in
// any answer's stream identity, and a delta keyed to it is read again whole.
// A log RESTORED from an older copy keeps its identity (ADR-0076), so only
// where it ends shows it: a LINEARIZABLE answer below a position this process
// read — any repository's, since one log is under every one — is the log gone
// back, and the cache reads the repository whole and converges to what it
// holds. Either way every repository on the node is marked to be read whole
// at its next round, and answered nothing at once until then. Two answers
// read below linearizable are never concluded from as they stand, since a
// copy behind the log gives both: a full answer below the cache, and "no
// such repository" — each is asked again at the log's head first, so a
// stale copy never purges a repository nor takes a cache back.
//
// One shape escapes every check: a broker restored under a RUNNING node and
// written past every position the node read before any of its reads at the
// log's head lands. The restore runbook's stopped fleet rules it out, and
// the log's own checks make it rare — every data node ahead of the restored
// log refuses to serve until an operator re-anchors it, into a generation a
// delta reads whole — and a node that may have met it is restarted: its first
// rounds read every repository whole.
//
// # A push never runs in a cache
//
// receive-pack writes what a client sends, and a cache holds nothing a row
// does not name: under protocol v2 any object present is fetchable by id, so
// a pack a push left in a cache before its row applied — a push the committer
// refused, one whose node died before its record landed — would be served by
// this node alone, and a later push could connect against it and land a ref
// no other node can materialise. So a push's receive-pack runs in a SCRATCH
// repository ([Cache.NewScratch]) whose one alternate is the cache's objects
// and whose refs are empty: a thin pack is completed and checked against
// everything the cache holds without a byte of it entering the cache. Its
// advertisement runs in a VIEW ([Cache.AdvertiseView]) whose packed-refs is
// the incarnation's own file under a second name, a snapshot no sync's rename
// can move. Both pin the incarnation they lean on, so the budget never evicts
// it from under a push. A compaction's build writes its outputs into a scratch
// too ([Cache.NewMergeScratch]), but one leaning on a store of EXACTLY THE
// PACKS IT MERGES — hard links of the incarnation's files — since its
// `pack-objects --stdin-packs` walks every commit, tree and blob it can reach
// to fill in its name hashes, and over the incarnation that was the
// repository's whole history for a roll of a few packs (scratch.go's A MERGE'S
// REPOSITORIES). The pushed pack enters the cache only once its row has
// applied: by [Cache.Adopt], a hard link of the scratch's three files, or, as
// on every other node, through a sync. Adoptions share the directory, since
// each links files of its own and group commit answers a repository's pushes
// together, and an adoption never waits: one that meets a round or a listing
// writing the directory stages its files in tmp/ and hands them over, and
// that writer installs them — in place of the pack's GET, when it was about
// to download it — before it lets the directory go. So the node that took a
// push downloads it only when a round that read the push's row had already
// begun that pack's GET before the adoption handed it the staging, or when
// the links fail. [Cache.Missing] is a fetch's question beside it: the first
// of its wants the cache lacks, which sends the fetch to one linearizable
// sync — asked of the wants as a STREAM, read from wherever the host holds the
// request (a partial clone's checkout names every blob it lacks in one
// request, which the host spools), and ended at the first absent one. A
// check that met DAMAGE on the way answers the damage, never an absent id
// (gitproc.Result.Damage, read from the run's trace2 reports whatever its
// exit status): cat-file calls an object behind a truncated index
// `missing` and exits 0, and taken as absent the fetch's upload-pack refuses
// that want in band, past the first byte the host's repair can retry under.
//
// # The byte path is shaped for S3
//
// Packs come from the object store, which in production is S3: GETs are
// latency-bound and want WIDTH. A push stores ONE object, its pack BUNDLE —
// the pack, the index and the reverse index its receiving git wrote, back to
// back — so installing a pack is one GET, split into three files as it
// streams, and three renames: no node but the receiving one ever runs
// index-pack, and a cold sync is bound by the network rather than by a core.
// Up to [FetchConcurrency] downloads run at once, largest first — their files
// made by the round in that order, and each asking only once the one before
// it holds its GET, however the scheduler runs them — and at most
// [NodeFetches] across every sync on the node — half the node to one
// repository, so a second repository's sync is never starved behind the
// first one's longest downloads — each holding its slot for its GET alone
// and its copy buffer with the slot, so the node's download memory is
// NodeFetches buffers however many syncs wait. The round's share goes back
// with the slot: a bundle's syncs and renames hold neither, bounded instead
// by the packs a round and the node hold IN HAND, two shares of each, and a
// bundle's three files are synced at once, beside every other bundle being
// placed, so their syncs share journal commits. Every byte is SHA-256
// verified against its row on the way in (objstore), and the three sections
// are held to the row by code.CheckBundle; a bundle that is not its row's is
// refused and never retried, because the row and the bytes disagree and
// neither is fixed by asking again. A pack the store does not have is asked
// about again at linearizable before it is called missing — whatever level
// the round read at, since that read came before the store answered — and
// the read made after the not-found decides it. A copy behind the log may
// name a pack a purge has since removed, and even a read at the log's head
// may name a row that has left the log since — removed by a purge's part, or
// retired, its object kept only for its grace (ADR-0074) — whose object the
// collector has deleted meanwhile. A head that still names the pack makes it
// missing, final, at once: nothing is installed again, since no read makes
// the store hold the object. One that no longer names it converges the round
// to what the log holds, and a pack the store does not have from that answer
// is asked about the same way — each not-found by the read after its own,
// never by one made before it. That read is full whenever the answer that
// named the pack was, since a full answer names packs at or below the copy's
// position, which no delta from it reaches.
//
// A full round that leaves [MidxRoundPacks] or more packs and no
// multi-pack-index writes one over exactly those packs — without a bitmap, a
// merge of the packs' own indexes — before its refs' peel reads them and
// before it answers: one push is one pack until the repository's maintainer
// merges them (internal/code/compact), and the request a cold cache was
// synced for would otherwise be served by git searching every pack's index
// for every object. Under that count the index
// costs the cold request more than it saves it (Measured, below), so the
// round answers without one, and the maintainer's follows once the packs
// have stood still and the runs queued before it have ended — on four cores
// one at a time, most packs first, so on a node that just joined, after
// every repository with more packs it synced (maintain.go).
//
// # One sync at a time, shared
//
// A repository has one round in flight on a node and at most one queued
// behind it. A caller that needs only what the cache holds — stale, or a
// session floor it already reaches, of a repository this process has read
// whole — is answered at once; a LINEARIZABLE
// caller is answered only by a round that started after it arrived — the
// statelog.ReadIndex rule, one level up — so a burst of CI clones of one
// repository costs one barrier and one sync, and none of them is answered by
// a barrier taken before it asked. Rounds run under the cache's own lifetime,
// not a caller's: a client that hangs up never cancels the round the others
// are waiting on.
//
// # A node reads few repositories whole at once
//
// Every page of a FULL READ of a repository's rows — a full round's (every
// repository's first round in a process, a rebuild, the log recreated or gone
// back under the cache), and a check's or a hint's read of the same pages —
// is one read transaction on a data node: served out of one of the
// queue.ConcurrentAnswers answers its shared lane gives the whole fleet's
// reads at once (a seat's tracker read, a page write, the code.resolve every
// git request is admitted by), and scanned on one of the reader connections
// every one of those reads takes too, eight on the smallest data node — or,
// on a git node that holds the data, on its own store's. So a node has at
// most [MaxFullRounds] full reads in flight, the smallest data node's readers,
// each holding its slot from its first page to its last and the rest in line,
// first come, first served: a node coming up with hundreds of repositories
// asked of it asks no more of a data node than that node scans at once, never
// every page at once while the fleet's reads draw for a reader against them —
// and an asker that waits a whole read attempt behind them takes the data
// node for gone. A DELTA holds no slot: it is the round every node runs after
// every push, usually one page, and never waits behind a node coming up. A
// delta the log answers in full holds one from its second page, page 1 having
// decided. A round waits for its slot under the cache's lifetime, and its
// callers under their own bounds: the wait is part of the sync each asked
// for, never a budget of its own.
//
// # Kept while there is room
//
// A cache is evicted to its budget (git.cache_max_bytes) and for no other
// reason: over it, the repositories used least recently go first, each a
// directory renamed into trash/ for the janitor; under it, nothing goes,
// however long a repository has been idle. An idle repository costs disk the
// budget already bounds, while evicting it costs a GET a live pack the next
// time anybody fetches it — a pack a push until its maintainer merges them —
// and a cache older than the rows' tombstone horizon is brought forward by a
// whole read that reuses every pack it holds. A repository a request holds is
// never evicted, even over the budget: it is served, and goes as soon as its
// last reader — or its maintenance run — lets go. The budget counts
// everything the cache holds on disk once — every incarnation, the downloads
// in flight, the pushes' scratch repositories, but not a file in scratch/
// that is an incarnation's own under a second name — and a round that takes
// the cache past it kicks the janitor at once, so a burst of cold syncs never
// overshoots it for a tick. An eviction is decided under the cache's lock and
// its directory moved into trash/ after it, so no sync on the node waits on a
// rename.
//
// # What each repository costs the node is counted, and decides nothing
//
// The cache keeps each repository's HEAT ([Heat], heat.go): what this node
// served of it over the last hour and the last day ([HeatHour], [HeatDay]) —
// the git processes that read it here (every one opens with an advertisement
// of upload-pack, counted as its fetch), the clones among them (a transfer
// offering nothing the client holds, so it is sent each want's whole
// history), the pushes it received whatever became of each, and the bytes git
// wrote its clients — counted by the host as it answers (internal/code/host).
// Beside it, over the same windows, what the cache spent on its own account:
// the copies its budget evicted, and every repair it counted under
// crewlet.code.cache.rebuilds — one place counts a repair into both
// (Cache.rebuilt), so the two never disagree. [Cache.Status], [Cache.Copy] and
// [Heat.Hot] read it back, and the host's status answers the fleet with them.
//
// IT DECIDES NOTHING: the budget evicts by last use and the maintainer by what a
// copy holds, and neither reads it — it is reported, for a person or a granted
// seat to judge, and the health tools built on it set no thresholds. IN
// MEMORY: it describes one process, which no other node has to agree on, and
// a restarted node's status says since when it counts ([Status.Since]). Each
// window is heatBuckets buckets of a twenty-fourth of it, counted from the
// cache's opening on the monotonic clock, so a count is short of its window by
// at most a twenty-fourth and an NTP step moves nothing; a repository nothing
// used within the day is dropped. The clock is read under the lock a window
// is counted under, and never behind the latest slot it was acted at
// (heatNow), so a caller whose reading went stale waiting for that lock
// judges no repository idle that a later caller had just counted. Not a
// metric, since a repository's id is no closed set: the status reports the
// [HotRepos] hottest of each window.
//
// # Derived files
//
// A DERIVED FILE is one the cache writes from what an incarnation already
// holds — its packs, its refs — and that no row names, and every one is a
// DECLARATION in DerivedFiles (derived.go, ADR-0079): what it is a function
// of, every write that makes it (its trigger, lane, deadline, memory charge,
// the file and git subcommand that run it, and its argument list), the files
// it owns and the files a write of it holds only while it runs, its check, the
// part a damage report names it by, what it promises a reader, and what each
// change to its inputs — a pack added, a pack a compaction retired, a
// dropping pack an expunge's commit took out of the repository, a pack
// removed with no retirement, the refs rewritten, its own files reported
// damaged, the installed published chain moving — does to it. There are six:
// the INSTALLED CHAIN's two halves (below, "The installed chain": the
// published multi-pack-index layers with their bitmaps, and the commit-graph
// layers with the node's own TIP over them, each with the chain file the cache
// writes, which a change CUTS — Prefix — rather than drops), and four of the
// node's own: the MULTI-PACK-INDEX (a full round's, without a bitmap, once the
// snapshot names MidxRoundPacks packs; the maintainer's, with its bitmap and
// lookup table where the node's memory holds one and without one where it
// does not), the COMMIT-GRAPH (the maintainer's, with changed-path filters,
// written first on a copy that has none), the
// INDEX OF PUSHES (below: runs under crewlet/, written by the cache's own
// code and no git — a round's fold and the backfill) and the MEMO (below: the
// answers of reads by object id, one file an answer in crewlet/memo, a
// directory it owns whole) — the last two the declarations a change to the
// packs or the refs keeps. A retirement keeps the commit-graph too (X04): a
// compaction merges packs and drops no commit, so the graph over them names
// every commit the outputs hold, where a pack removed with no retirement may
// have taken commits with it — and so does AN EXPUNGE'S COMMIT, the one
// retirement that drops the graph, and the memo with it: the commits it took
// out are gone from the repository, over which a graph still naming one has
// git walk as though they were there, and the memo's answers may hold what
// it purged (below, "The dropping view"). Their files live inside the incarnation, so
// the budget counts them and they leave with the repository; a rebuild writes
// the first two again over its packs, links the index's runs and moves the
// memo's tree whole, and a swap links every one the events of what leaves it
// keep but the memo, whose tree it moves whole too. Each is a REGULAR
// file, as every file of the image is — the root's HEAD, config, packed-refs
// and state too: a symbolic link, or anything else, at one of their names is
// outside the image, so the next full round moves it into trash/ without
// following it — a fixed file is then written afresh, as a full round writes
// each that is not what it would write, and a derived file is left to its
// writer — and Verify names it. git reads through a link at a derived file's
// name, at config or at packed-refs, so a link named commit-graph, or named
// config over a file holding the configuration the pool requires, had git
// read a file outside the incarnation, outside the budget and outside what a
// rebuild replaces. A link at HEAD git does not read through: it refuses the
// whole repository unless the link's text begins refs/ (setup.c's
// validate_headref, which checks only that prefix — text that climbs back out
// of refs/ has git read the file it leads to), so every git process in the
// copy failed, a request's and the round's own alike, and a link whose target
// held a HEAD the sweep accepts was kept by every full round. The state file
// (format 2) records each derived file AS ITS DECLARATION, in one map keyed
// by the declaration's name — its files' bytes, the packs the write that made
// it covered and which write that was — beside the repository's last
// maintenance run, the duty cycle's clock; Layout.Held is the packs' bytes
// and that map's together, and the maintainer reads whether an index is
// there, has its bitmap and how many packs it covers, and whether a graph is
// there, from the same entries. A declaration with no file on disk has no
// entry, and files of one that no write this cache recorded made — a state
// a previous process could not leave readable, a bitmap a killed writer left
// with no index — are counted and never read as made, so the maintainer
// writes them again. A state of format 1, which spelled the two files' facts
// as fields of their own, is another build's: its incarnation is trashed and
// the repository synced cold, since nothing migrates a disposable cache.
//
// Every reader reads the declarations and nothing spells a derived file
// beside them: the IMAGE — one predicate a directory, by which the full
// round's sweep keeps a file and Verify admits it (sweep.go's image) — the
// one REMOVAL of a derived file IN PLACE, dropFor, called at the one place
// each change is made (a pack downloaded, adopted or linked from an adoption
// a round was handed, a pack put back, a pack swept, packed-refs rewritten,
// a damage report or a failed check), and the SWAP, which links into the
// next incarnation only what the events of the packs leaving keep, so a
// dropped file is never in the copy that change is first visible in (swap.go)
// — each before the change is visible to any git
// process — stopping first a maintenance run writing what it drops, since a
// file renamed in after the change would be built over the inputs before it,
// and no run starts while a round runs or an adoption holds the repository —
// which also takes the dropped files out of what the incarnation records it
// holds, and answers what it dropped, which is all a report's repair records
// as dropped (a declaration that keeps its files on a report is dropped by
// nothing, and a report of it asks no drop: it makes the copy's scrub due,
// as damage no drop repairs does, and a request that met it is repaired as a
// pack's damage is); the check, the scrub and Verify, which hold each owned file to
// its bytes as its declaration's Check says and refuse a Check they do not
// read; the maintainer's plan, which takes its steps, lanes, deadlines and
// charges from the declared writes, and the refusal lines, counters and
// gauge, which name a write's Step; the clearing after a write that did not
// succeed, of what that write declares it holds (Write.Holds), a lock by
// name and a directory listed at most once; and the writers gate
// (TestNothingButSyncWritesACache), whose declared half holds every git
// writer in the cache to a declared write and every declared write to its
// writer's file, argument list and all. Open refuses a build whose
// declarations leave anything unsaid (validateAll), and
// TestEveryDerivedFileIsDeclared holds the declarations to the files real git
// writes — a file a writer leaves that the image does not admit is trashed
// under a running reader, and a file the image admits that no writer makes
// is a hole anyone may fill.
//
// # The index of pushes
//
// Beside each copy the cache keeps an index of which push introduced each
// commit and every update each push landed on a ref (provenance.go), built
// from the push rows a snapshot serves rather than from the packs, which is
// what [Cache.Provenance] answers a query server's reads from. EAGER: every
// delta round folds the pushes it applied into one immutable run before it
// answers, so a push is named the moment the sync after it returns. BEHIND
// THE SERVING, one backfill goroutine per cache (backfill.go, started by
// [Cache.Start]) reads the rest a page at a time: the GAP a full round passed
// over, on every node holding a copy, and the HISTORY before an index's first
// full round only once [Cache.WantHistory] marks it wanted — the query
// server's read answered as the repository's home — each page read at a
// session floor the index has already seen, so a data node behind the log
// never answers a range in part, and paced by BackfillIdle times the fleet's
// git nodes for each data node ([Options.Fleet]); its next step is picked
// from a SCHEDULE every change to a copy's work keeps (schedule.go), never
// found by a walk of every copy under the lock every sync takes. Runs never
// overlap, and one record — the state's — names them, changed only by a
// compare-and-set against the incarnation's index generation, so a round
// under the write lock and the backfill under a pin never undo each other's
// work. A run this process did not write is held to its SHA-256 trailer
// before it is mapped, the scrub holds every run again, and one that fails
// drops the index: the full round that repairs the copy makes it again at
// its own position, and the backfill reads back below it what is wanted
// there. A run is read
// through a MAPPING of the process (unix.Mmap), and the kernel counts every
// mapping of the process against one node-wide sysctl, vm.max_map_count,
// past which every mapping fails, the Go heap's growth among them: so the
// cache holds at most a quarter of that count mapped (MappedRunShare, read at
// Open; 16 382 at Linux's default), keeping a run its last reader let go for
// the copy's next reader only while that bound has room (runmaps.go).
//
// # The memo
//
// Beside each copy the cache keeps the pages internal/code/query cut for its
// reads by object id (memo.go): one file an answer under crewlet/memo, a
// directory the declaration owns whole (Owned.Tree), keyed by everything the
// answer is a function of (NewMemoKey) and written as one read's run cuts its
// pages (Cache.Memo's batch: each staged with no sync, placed once the run
// has answered whole). A function of object ids, so never stale and only
// absent; the copy's, so an entry exists only where a read of it found the
// objects it names, and a read naming whole ids is answered from it before
// any git runs. Every read holds an entry to its own header — its key and the
// SHA-256 of its body — so the check and the scrub, which hash files behind
// the serving, pass over it (Check.hashed).
//
// The tree is COHORTS, crewlet/memo/<n>/<key>: an entry lives in the cohort
// that was the node's newest when it was last placed or read, a new one opened
// past every sixteenth of the share placed and a read moving what it finds in
// an older one into the newest — so the oldest cohort is the sixteenth of the
// memo used least recently, and the order of use is the disk's to keep. What
// the cache holds in memory is a copy's cohorts and their bytes (its record),
// 16 bytes a cohort, and NOTHING AN ENTRY, so the memo's memory follows the
// copies and not the answers. Its bytes are its writer's running total, per
// cohort, per copy and node-wide (Cache.memoBytes), never a measure's: a stat
// of its directory is the directory's few KiB. The janitor holds the memo to
// a MemoShare-th of the budget — the pages runs have staged for it counted
// with those it keeps — before it evicts any repository, and never evicts one
// for what the memo holds past that (overBudget), by DROPPING THE OLDEST
// COHORT whole: a rename of its directory in each copy holding it, a
// sixteenth of the share at once, so a trim falls due once that much has been
// placed again, not after every miss. It lists each copy's tree once a
// process behind the serving, its cohorts' names alone, counting a cohort a
// crash left unrecorded; and at its tick, once a minute at most, it writes a
// copy's state again once its memo's record moved. A history gone drops it
// whole and shuts it until the round that dropped it has swept the replaced
// history's packs, and no batch begun before the drop places anything after
// it. A rebuild or a swap MOVES it whole into the incarnation it promotes,
// one rename however many entries it holds (carryMemo, SeedMove), never a
// link an entry.
//
// # What this process did not write is checked before it serves, and every byte is scrubbed behind it
//
// A CHECKABLE file is one git reads to find an object or a parent — the
// commit-graph, the multi-pack-index with its bitmap and reverse index, and
// every pack's .idx and .rev — and git checks none of them as it reads: a
// damaged one is read with no word and can answer WRONG, where a damaged
// pack body at least fails. A graph naming another commit as a tip's parent
// lets a push force a protected branch as a fast-forward (git 2.55 and later
// answer the force check from the graph); an .idx naming another object's
// offset makes rev-list count nothing and a clone ship the wrong object; a
// bitmap missing objects makes every clone incomplete, upload-pack exiting 0.
// Each is a git HASHFILE, whose last twenty bytes are the SHA-1 of every byte
// before them, and a .idx and a .rev also carry their pack's checksum — its
// name — in the twenty before that. So a full round holds every checkable file
// to its trailer BEFORE it answers (check.go): at an incarnation's first full
// round in a process, everything a previous process left; at every later full
// round, whatever this process has neither written nor checked since. No git
// process runs.
//
// What fails is repaired by the same round, in place. A derived file is
// DROPPED and written again at once — the graph by the maintainer's graph-first
// run, the index as a repair (maintain.go's INDEX REPAIR) — never written
// over: git keeps a bitmap beside an index it would write unchanged, damaged
// or not, and cannot write an index over offsets it cannot read. A pack whose
// .idx or .rev fails, or that a full round finds at the wrong size, is
// INSTALLED AGAIN WITH ONE GET, renamed over its files in the same incarnation
// (install.go), never removed first: a git process that opened the pack reads
// the files it opened, one that opens it afterwards the new ones, where a
// removal made its objects missing for the whole download. What the check
// finds goes on the copy's REPAIR MARK as it ends (recordCheck), at the
// identity each file failed at, and comes off once a round has installed or
// dropped it — so a round that cannot act on it, its install failing on a
// pack it adds or the finding's own download failing, leaves it to the next
// round rather than to the next check, which skips a file damaged in place
// after this process vouched for it. A file the mark already names at the
// identity a check finds it failing at keeps the finding it has: a check
// that finds it again adds nothing, so a scrub's finding stays the scrub's,
// which answers for that scrub while it waits (below).
//
// A file this process wrote or checked is VOUCHED for, in memory and never
// persisted, by its IDENTITY: device and inode, size and — for every file but
// a .pack, which git FRESHENS without changing a byte whenever a process
// writes an object it holds, an engine write's build beside the copy among
// them — modification time; NO CTIME, since an adoption and a seeded rebuild
// hard-link files and every link moves an inode's ctime without changing a
// byte (check.go's fileID). A maintenance run
// vouches only for what it changed, so a run over a damaged bitmap git kept
// vouches nothing of it. A VERDICT HOLDS ONLY FOR THE FILE IT HASHED: the path
// is Lstat'ed before the open and both the open file and the path after the
// hash, so a file written in place, renamed over or removed meanwhile gets no
// verdict; and a repair acts only on a file still at the identity its finding
// names, so it never undoes the maintainer's rewrite or an earlier repair.
//
// The trailer rule, and the piece loop every hash of a node's files reads by,
// are internal/code's (code.HashTrailer, code.HashPieces): ONE implementation,
// which a published layer's check (code.CheckLayer) holds the layer's files
// by as well, so a layer fetched and the layer kept are judged alike
// (ADR-0008). Hashing is a request's core — the round's callers wait on it —
// so it holds ONE SLOT OF THE READ LANE a piece at a time
// (gitproc.Pool.HoldRead, [CheckPieceBytes]) and none while the round runs
// git, which waits in the same lane; one slot rather than several, so the largest repository's first
// round costs that one request seconds rather than taking every core the
// node's other requests are served with. A slot not had in gitproc.SlotWait
// fails the round busy, and the next round checks: a refused slot and an
// ended request decide nothing about a file. A READ THAT FAILS does — a file
// the disk cannot read is one git cannot serve — so it is counted
// `unreadable` and repaired as though damaged, never left to fail every round
// on the copy. What it costs, measured
// (BenchmarkCheck, this package's 64-pack histories of a source repository on
// four cores, -count 6): the files read are 23% of the copy's pack bytes
// (22.9 MB of 99 MB, 228 MB of 1.0 GB), hashed in 0.18–0.23 s a gigabyte of
// packs from the page cache and 0.24–0.47 s cold on that machine's disk — 95%
// of a warm first round's time, once a process. Read cold at a volume's
// throughput, it is the index bytes over it: 1.8 s a gigabyte of packs at a
// gp3 volume's 125 MB/s baseline, 39 s for the largest repository the
// defaults admit (code.repo_max_bytes, 20 GiB). [Cache.AdmitVerify]'s check
// holds each derived file to its trailer too: no git command names a damaged
// bitmap.
//
// The check reads index files alone, and only those this process has neither
// written nor checked, so two kinds of damage pass it, and a SCRUB behind the
// serving is what finds them (scrub.go). A PACK BODY: on a copy the
// maintainer has served from, upload-pack's pack-objects copies a stored
// object to the client as it is stored — damaged bytes from any pack, and a
// damaged entry header from the bitmap's preferred pack, which single reuse
// sends without reading it — and exits 0, and the commit-graph spares every
// walk the commit's parse, so no git process on the node names it while
// every clone of it fails at the client (measured on the node pin and the
// floor); and holding the packs before a copy first
// answered would hold the largest repository's first request for as long as
// reading twenty gigabytes takes.
// And damage that lands in a file AFTER this process vouched for it, with no
// change of its identity — media damage — which no vouch can see: a bitmap
// damaged so drops an object from every clone of its branch, upload-pack
// saying nothing. A scrub hashes EVERY file the copy holds, vouched or not —
// each .pack against its own trailer, which is its name, and every index file
// by the check's rule — once after the first round of a copy a previous
// process left (one this process built was held to its rows file by file as
// it was placed, so its interval starts there instead; and that first scrub
// reads every pack but no index file the round just checked, which would be
// a fifth of what it reads for a verdict minutes old), every [ScrubInterval]
// while the copy is used (the first use since its last scrub — a reader
// taking it, letting it go, or making a push's scratch over it — wakes the
// scrubber, which has no other reason to wake for it, wherever in the
// interval that use falls), and at once when a git process over it reports a
// pack or an object it could not read, names a pack in words, or crashes —
// but not within a [ScrubCooldown] of a scrub that found nothing, whatever is
// written meanwhile (`cooldown`), nor within one of a scrub whose findings
// still wait for their repair (`covered`: that repair is the report's
// answer); past it such a report scrubs the copy again, at most once a
// [ScrubCooldown], so damage that landed elsewhere in the copy meanwhile is
// not left to the interval. ONE SCRUB ON THE NODE AT A TIME, IN THE ORDER OF
// ITS EVIDENCE: a failing request's first, then a report's, then a copy's
// first, then the interval's, the least recently held to its checksums first
// among equals —
// and evidence does not wait behind a routine scrub: a request or a report
// stops the first or interval scrub running at its next piece, which is due
// again after it, where it once waited minutes behind a twenty-gigabyte copy
// nobody had reported. A read slot a piece, the slot waited for however long,
// since no caller waits on a routine scrub; and NO LOCK of the repository, so
// the copy keeps answering and its rounds and maintenance run beside it. Its
// pin keeps the copy from the budget and a retire, and is no use: a scrub
// never makes the next one due. What it finds — a file failing its checksum,
// or one the disk could not read — goes on the copy's repair mark at the
// identity it failed at, and the repair round installs a pack again over its
// files with one GET ([RebuildStored]), or drops a derived file to be written
// again at once — only while the file is still the one that failed, and never
// under a check. A pack whose .pack alone failed is the same pack fetched
// again, byte for byte, so the derived files built over it stay; one whose
// .idx failed takes them with it, since they may have been built from it. A
// GET that fails is logged (code_cache_pack_reinstall_failed) and ends
// nothing: the round goes on, the copy keeps serving, and that pack is asked
// again after [RebuildCooldown] doubling to [RebuildBackoffCap] — by no round
// and by no rebuild meanwhile — so an object store that is down neither
// wedges the copy's rounds nor is asked once a round.
// What it costs, measured (BenchmarkScrub, the same histories, six runs
// each): it reads 1.13 times the copy's pack bytes, 0.86 to 1.38 s a gigabyte
// of packs from the page cache — 18 to 30 s for the largest repository the
// defaults admit — and at the volume's own throughput read cold (194 s for
// that repository at a gp3 volume's 125 MB/s baseline). A request beside it
// waits for a read slot only when every other one is busy, and then for one
// piece at most: on a pool with one free slot shared with the scrub, the
// waits were 0.2 to 9.8 ms at the median and 51 to 59 ms at the most from the
// page cache (a whole 64 MiB piece hashes in 51 to 82 ms). COLD — a copy
// whose files have left the page cache, as a daily scrub of a large one
// mostly finds them — a piece is read at the volume's throughput while its
// slot is held: on this machine's disk, whose cold reads ran the scrub at 1.07
// to 1.41 s a gigabyte of packs, the waits were 0.4 to 12.5 ms at the median
// and 21 to 22 ms at the most (beside=read,cache=cold, three runs each of the
// 100 MB and 1 GB histories); at a gp3 volume's 125 MB/s baseline a whole
// piece takes 0.54 s to read, which is the longest such a request waits.
//
// # Damage is repaired once, in place where nothing is lost
//
// A git process the host ran over a cache that git found damaged — a pack, an
// index or a derived file it could not read, an object a row names that no
// pack holds, as the formats of its trace2 reports name them (gitproc), never
// their words — or that the pool refused to start because the directory is
// not the shape the engine writes (gitproc.ErrRepoShape), makes the host
// [Cache.Rebuild] it, read as [DamageReason] reads the failure; a round whose
// own git — the peel of the tags it sets — names the same damage is answered
// the same way (Cache.run, damagedRound), under the same rules, the round's
// callers waiting on the repair as the request does on its own. What the
// repair is follows from what was lost:
//
//   - A SHAPE loses nothing — the pool refuses the directory before git reads
//     a byte of it, so every pack is still whole — and is repaired IN PLACE:
//     the incarnation is marked to be read whole (markWhole), and the next
//     round's sweep puts its config right and moves aside whatever the image
//     never holds there (a commondir, an alternates file, an objects
//     directory that is not its own), reusing every pack it holds — no GET,
//     no second copy on disk, and none of a rebuild's refusals, since it
//     costs a read of the rows. The sweep then holds the directory to the
//     pool's own check (gitproc.CheckShape), so a shape it did not end — a
//     directory something keeps writing into — is seen there rather than by
//     the next request, and only that goes on to a rebuild.
//   - A DERIVED FILE loses nothing either: a multi-pack-index (its bitmap,
//     its reverse index) or a commit-graph is written here from the packs
//     the incarnation already holds, so damage git names in derived files
//     ALONE ([RebuildDerived]) is repaired in place the same way, the round
//     DROPPING the files the damage names (the [Failure]'s Drops: the index,
//     the graph, or both for a chunk-format message, which names neither) for
//     the round or the maintainer to write sound ones over the same packs —
//     unless this process wrote one of them after the failing process began
//     (the Failure's Began), which that process may not have read. A round
//     whose own git meets it marks the same drop; a round that had dropped
//     those very files and meets the damage again has met a pack's damage
//     ([RebuildCorrupt]), repaired as the next item says.
//   - A PACK OR AN OBJECT git could not read ([RebuildCorrupt],
//     [RebuildMissing]) loses at most the packs that hold the damage, so
//     those are what is fetched again, IN PLACE FIRST. A failure whose
//     process began before this process last wrote a REPAIR into the copy —
//     a pack installed again over its files, a lost pack installed again
//     (by whichever round put it back: its loss ends however it comes back), a
//     derived file dropped — may have been of what the repair replaced: it
//     is answered from the copy as it stands, once a round still repairing it
//     has ended, and the host runs it once more. A push's pack installed or
//     adopted is no repair: it replaces nothing the failing process read,
//     and on a repository pushed to every few seconds a guard that counted it
//     answered every failure from the damaged copy for ever. Otherwise the
//     copy is read whole by a FORCED round, every index file held to its
//     checksum whatever this process vouched for, and every pack whose index
//     files fail, that the copy lost (named by the rows at a position it had
//     read) or holds at another size, or that a confirmed hint named (the
//     [Failure]'s Packs) is fetched again over its files — one GET a pack,
//     the same incarnation. Its check SPENDS what forced it once it has run,
//     whatever the round meets after, what it found waiting on the copy's
//     repair mark (above) — so a forced round whose install fails on a pack
//     it adds, which the store does not hand over, leaves the rounds after it
//     a finding to install and nothing to hash again. At most once a
//     [ScrubCooldown] while nothing is written: a forced check with no repair
//     written since it ended — its verdict recorded as its round ends,
//     whatever that round met after it — is not run again for the failures
//     of the hour after it that name no pack it could fetch — none, or only
//     packs whose last download the store refused and whose next is held off
//     — which go to the scrub's answer, so a damaged pack body a busy
//     repository's requests keep failing on costs one read of every index
//     file rather than one a request. A repair written into the copy while
//     that runs answers the failure from the copy as it stands too, asked
//     again before the scrub and before a rebuild. A round that found nothing
//     has the copy's SCRUB run at once for its callers — ahead of every
//     routine scrub on the node, which it stops — which finds a damaged pack
//     body no index shows; a scrub begun after the failing process, or within
//     [ScrubCooldown] with no repair written since, answers from its verdict,
//     and what an earlier scrub found that still waits for its repair is
//     answered by that repair — its files the forced round found failing
//     again staying the scrub's findings (above) — either beside what a
//     round's check has found since that waits too, its pack none a rebuild
//     past them links.
//     Only when neither finds anything to fetch again is the repository read
//     whole at the log's head into the NEXT incarnation, beside the one its
//     readers hold, which replaces it once converged — seeded with every pack
//     the scrub found whole, so it downloads nothing when the damage is in
//     the rows — and never while a pack it would download is one whose last
//     download the store refused and whose next is held off: the rebuild
//     would ask the store for that very pack again within the backoff, so the
//     failure is answered with the store's own refusal of it until the
//     backoff passes and the forced round asks once more (a store that will
//     not hand a damaged pack over costs one GET a backoff window, never a
//     download of the repository). A request whose copy such a rebuild
//     replaced while its round waited is answered from the rebuilt copy.
//
// Ten requests failing on one damaged cache cost one repair, a request that
// failed on an incarnation already replaced costs none, and a sync queued
// behind a rebuild that fails is answered by an ordinary round on the
// incarnation still serving, never with the rebuild's failure. A rebuild is
// not repeated where it cannot help: damage a rebuild did not end is the
// object store's or the rows', so an incarnation holding exactly what such a
// rebuild read is not rebuilt again for a day ([RebuildBackoffCap]) unless
// something writes into it — and one still refused for its shape always has,
// since no round makes a directory of another shape — and a line of attempts
// that keep failing waits [RebuildCooldown] after the first and twice as long
// after each one after it, up to that day — every refusal counted, and
// logged once a cooldown. A line ends when the asking stops: a repository
// with no rebuild asked of it for [RebuildQuiet] meets its next failure as
// new damage. A check's own sync (below) repairs nothing: the damage it
// meets is the check's to report.
//
// A damage git names ONLY IN WORDS — git 2.56 reports a corrupt packed
// object through a bare "%s", a format that names nothing — is a HINT naming
// a pack (gitproc.Hint), and repairs nothing until [Cache.ConfirmHint] has
// hashed that pack, as a check does, against the row naming it: a pack that
// is not its row's is a [RebuildCorrupt] repair naming THAT pack, fetched
// again in place whichever git named it — a request's or a round's own — a
// sound one is no repair, and a pack no row names is no damage of the copy.
// The same sentence is a row on 2.55, a pack's damage, whose pack is recorded
// as a hint beside it and confirmed the same way, so the repair fetches that
// pack rather than look for it. A pack is hashed ONCE however many
// hints name it, on the cache's lifetime rather than any request's — every
// hint about a pack being hashed waits for that hash as long as its own
// caller does, and a caller that leaves leaves it running for the next —
// because damage that fails a hot repository fails every request in flight
// on it together, and a hash a balancer's cut ended would start from byte
// zero for the next. A verdict is kept with the pack's files' identity and
// reused while they are those files; the hash is bounded by [HashBudget] and
// runs a piece at a time under a read slot (gitproc.Pool.HoldRead); and a
// round's own git is confirmed once the round has let the entry's write lock
// go and ENDED, its incarnation pinned, so the round queued behind it runs at
// once and only the hinted round's own callers wait on a hash — never a
// linearizable caller, which every answer naming a ref is, and which no round
// begun before it answers (hint.go). A message only nominates.
//
// A check ([Cache.AdmitVerify], [Verification.Run]) is the operator's side of
// it: every pack the rows name hashed against its row as the bundle the push
// stored, every file outside the image named, packed-refs held to the digest
// written with it, and git's connectivity check. It is ADMITTED BEFORE IT
// READS ANYTHING — a free maintenance slot and its fsck's memory taken first,
// refused rather than queued when either is short, so a refusal costs nothing
// and the lane bounds how many run — and only of a copy this node HOLDS
// ([ErrNotHeld]): a check builds no copy, which would be a whole download of
// the repository. It stops no maintenance run — one in flight finishes, the
// files it holds while it runs admitted — and none starts on the repository
// while it is admitted. Each pass of it is bounded by what the copy holds
// ([CheckBudget]) and ended there ([ErrPastBound]), so a check wedged on a
// disk holds neither its slot nor the operator who asked. Asked to repair, it
// repairs the way what it found asks: where every problem is the copy's OWN
// DIRECTORY out of place — an entry outside the image, a fixed file missing or
// not what a full round writes, a pack no row names, a shape the pool refuses
// — it reads the copy whole IN PLACE, the full round's sweep putting the
// directory right with every pack, the multi-pack-index, its bitmap and the
// graph kept and nothing downloaded — but a pack no row names, which leaves
// the copy the check holds by a swap into its next incarnation, the
// multi-pack-index and the graph built over it with it, still nothing
// downloaded; DAMAGE — a pack missing, at another size
// or not its row's bytes, a derived file failing its checksum — and whatever
// the read in place leaves, a shape something wrote past its sweep included,
// it rebuilds into the next incarnation, linking in every pack it has just
// found whole and downloading only the rest, so one damaged pack costs one
// GET. The read in place is the check's own, as the sync before it is:
// damage its git meets is named to the check, never handed to the repair a
// git process asks for — a request's or a round's own, which takes its own
// steps (a forced round, a scrub, a rebuild seeded by that scrub) and stands
// in that process's windows. Rebuilt for what a sweep puts right, a copy had
// no index, bitmap or graph until the maintainer wrote them again — hours of
// bitmapless clones of a large repository, and for good on a node whose
// memory now refuses a bitmap. Either way the repaired copy is checked the
// same way, and a repair it has seen whole has ended the damage it was for,
// so that incarnation's next failure is rebuilt at once.
//
// # What any git process reports damaged is repaired
//
// git reads around most damage to a derived file — it warns, answers from the
// packs and walks by date — and a caller that got its answer never looks at
// the warning; the maintainer's own write over a damaged index says so and
// fails, every time. So the pool hands EVERY RUN to the cache's observer
// (gitproc.Pool.Observe, installed by [Open], given back by [Cache.Stop]; a
// reservation's runs too), and what a run over one of the cache's
// incarnations named of its derived files is repaired whether or not the run
// failed (report.go):
//
//   - ROUTED BY THE PROCESS'S DIRECTORY: a process in an incarnation reports
//     about it; one in a push's scratch or an advertisement view reports
//     about its alternate only the derived files, since its pack and object
//     reports may be the client's pack (counted `unrouted`); one over a
//     merge's store — a compaction's build — reports about the incarnation
//     the store's links came from everything but a derived file, of which
//     the store holds none. A chunk-format report is narrowed to the files
//     its process loads (gitproc.Cmd.Loads).
//   - REPAIRED BY A ROUND NO REQUEST WAITS FOR, which the observer queues
//     under the entry's mutex and never its write lock, so it never holds the
//     run that reported. The round is FULL and LINEARIZABLE — a full round's
//     sweep removes every pack its snapshot does not name, and an adoption
//     installs a pack past the incarnation's own position — and it drops the
//     files as a failing process's repair does. The copy is not taken out
//     of service: it keeps answering at once meanwhile, as git did.
//   - GUARDED, on the monotonic clock: a file this process wrote after the
//     reporting run began is kept (`stale`), since that run may have read
//     the one the write replaced — without it, every long clone that began
//     over a damaged bitmap would drop each rewrite in turn.
//   - NEVER UNDER A CHECK: a report recorded while a check of the repository
//     is admitted is `deferred`, and the check's release asks the round, so a
//     check reports what it found rather than a repair made between its
//     listing and its hashing.
//   - ONCE A RebuildQuiet: a derived part dropped for damage again within
//     RebuildQuiet of its last drop for damage is dropped and left to the
//     ordinary schedule (the graph graphFailed, the index its debounce and
//     interval) — logged code_cache_derived_damaged_again once an incarnation
//     and counted `again` — however many writes of it came between, the
//     maintainer's ordinary ones included: they land on the same disk. So a
//     disk that keeps damaging what is written to it costs one rewrite at once
//     a RebuildQuiet and the ordinary schedule's after it, never a loop.
//
// A PACK, AN OBJECT, A HINT OR A CRASH MAKES THE COPY'S SCRUB DUE AT ONCE, and
// drops nothing: a report of stored damage names no file it can be trusted
// about (`hash mismatch` does not say whether the pack, its index or the
// multi-pack-index placed the wrong bytes; a hint is words a request may
// reach; a pack-objects dying of a damaged reverse index says only that it
// died), so the bytes decide — the scrub hashes every file, and the repair
// round installs again what fails. A crash in a scratch or a view is its
// alternate's, like a derived report; its pack, object and hint reports may
// be the client's pack (`unrouted`) — but not a merge's, whose store holds the
// incarnation's packs alone, so every one of them is that copy's. Within
// [ScrubCooldown] of a scrub that
// found nothing, a report makes nothing due (`cooldown`), whatever was written
// since: a request that can provoke a report beside a busy repository is
// bounded by that and nothing else.
//
// A FAILURE ITS REQUEST REPAIRS IS THAT REQUEST'S. Every reader a sync or a
// repair hands a directory to is named ([Synced.Repairer]), and a process run
// in that directory whose failure the reader repairs carries the name on its
// Cmd (gitproc.Cmd.Repairer: the host's, over the incarnation, and the check
// of a fetch's wants, [Cache.Missing]). What such a run reports that the scrub
// would answer — a pack, an object, a hint, a crash — is HELD for the reader
// and makes nothing due: the repair the reader asks ([Cache.Rebuild] — a forced
// round in place, the scrub for its own callers, the rebuild) answers exactly
// that, and consumes it (counted `request`), so a request's repair of one pack
// is never paid for twice with a read of every byte of the copy beside it, nor
// raced by one. A repair of a shape or a derived file in place reads no pack
// and answers none of it, so it CARRIES what is held to the reader it hands
// the repaired copy to — the same request, letting go of the pin it failed
// over before it runs again — and the hold is the request's across its
// repairs, never one pin's: a retry that meets a pack's damage beneath a
// dropped graph has it consumed by that pack's repair. A reader that lets the
// copy go with something still held — its request failed past its first
// byte, failed in a way the host does not repair, never failed, left before
// its repair answered, or was repaired for a shape or a derived file alone
// and then succeeded — hands it to the ordinary rules as it does
// ([Synced.Release]), as though it had just arrived: no timer and no grace,
// because a request's lifetime always ends. A
// derived file's report that a drop repairs is never held — the drop is the
// repair's own first step and races nothing — and nor is one from a scratch
// or a view. A ROUND'S OWN GIT is held the same way: the peel of the tags a
// round sets names a reader the round mints (roundReader), and what it
// reports is consumed by the pack's or the object's repair the round asks of
// that failure — the same forced round, scrub and rebuild — and handed to the
// ordinary rules once the round's answer is known otherwise.
//
// A dropped file is written again at once because the maintainer is woken
// as the round lets the repository go — never from inside the round, since a
// pass starts no run on a repository a round holds: woken from inside, it met
// the round still running and left the copy to its next tick.
//
// # Background work never takes a person's slot
//
// The maintainer writes a multi-pack-index with its bitmap — the bitmap
// carrying git's lookup table, so a request that opens it loads only the
// bitmaps it asks for (ServeConfig) — and a commit-graph with version-2
// changed-path filters, every one its commits lack (the first write
// computes them all, and every later one reads them back), so a
// path's log and a file's blame skip every commit that did not touch the
// path — or, on a copy serving from an installed chain, the node's TIP
// commit-graph over it alone (below, "The installed chain") —
// once a repository's packs have gathered and stood still for
// [MidxDebounce] — or the oldest has waited [MidxMaxWait], so a repository
// that is never still is indexed too — in the pool's maintenance lane: half
// the node's cores, a free slot or none, never waiting, a repository a slot,
// in every slot but one — kept for a person's verify — and as many at once
// as [MaintenanceMemoryPerCore] holds beside the largest, so one repository's
// hours-long bitmap holds back nobody else's and a node's largest
// repositories falling due together do not take its memory at once; and at
// most a tenth of a core's time a repository ([MidxDutyCycle]). It holds a
// pin, never the repository's round, so no push or fetch waits for it; a
// full round that would sweep the directory under it stops it first,
// because a multi-pack-index naming a pack the sweep removes fails every
// read of the repository. A cache without the bitmap is slower, never wrong.
//
// A COPY WITH NO COMMIT-GRAPH IS GRAPHED FIRST: read whole for the first time
// (a cold sync, a rebuild, a node that just joined), it is due for its graph
// at once — no debounce, ahead of every index and bitmap, on the graph's own
// charge, and the graph alone in any slot of the lane, the one kept for a
// person's verify included, while an index it is due for as well waits for a
// slot of the maintainer's own — because without one every walk git makes
// over it goes by date, and a push of a commit dated before its parent walks
// every commit in each of them; the window is the graph's own write rather
// than the maintainer's queue (maintain.go, GRAPH FIRST).
//
// And never a step the node's memory cannot hold. Every git process the cache
// runs over a whole repository holds memory in proportion to its objects —
// a bitmap about [BitmapBytesPerObject] an object, some 36 GB for the largest
// repository the defaults admit; the index without one [IndexBytesPerObject];
// the commit-graph [GraphBytesPerObject]; Verify's check
// [FsckBytesPerObject] — so each is charged its own figure an object of what
// it walks — the repository's, but for the node's tip, which walks only the
// commits an installed chain lacks and is charged by the tail's objects
// ([TipGraphBytesPerObject]) — beside the fixed part a write holds whatever
// it covers (every commit-graph's [GraphBaseBytes], the trees its
// changed-path filters' diffs keep in git's delta base cache up to its cap;
// the index's [IndexBaseBytes]) — and room beside it for one transfer of the whole
// repository (gitproc.TransferBytesPerObject; Write.charge), against the
// node's BUDGET: its memory as the git pool read it (the least of
// the machine's and every cgroup limit above the process, v2's and v1's),
// read again at every maintainer pass so a container resized in place is held
// to its new limit, less gitproc.TransferMemory for every transfer the pool
// admits and [ServingMemoryPerCore] for every core the memory backs — the
// store, the broker, the engine and the read lane — never for cores it has no
// memory for. Everything in flight — the runs, the largest included, a
// round's index, a check — is held to it together. A step whose charge alone
// is past it never runs on the node: a refused bitmap leaves the repository
// indexed WITHOUT one, so a repository that outgrew the node's memory still
// keeps its pushes inside an index (a bitmap written while it could be kept
// until [MidxRoundPacks] packs gather outside it); each step refused is
// logged once an incarnation and counted, a refused bitmap, a refused graph
// and a refused tip are each kept on a gauge while they stay refused, and a
// refused bitmap — never a tip, whose copy is still served through its
// chain's bitmap — is raised as code_maintenance_refused
// ([Cache.AlarmReading]), at the budget's own threshold: a git node's largest
// repository with a bitmap is bound by its memory before its disk. The budget
// itself is ADVERTISED as it stands ([Cache.BackgroundBudget], which the
// engine's presence row carries to every git node on every beat, beside its
// status and never in it), so the git node a repository's maintenance is
// placed on is one whose own admissions would hold the work — the figure
// itself, never a second estimate of it. A check a
// person asks for whose charge is past the budget is refused
// ([ErrNoMemory]). And the first repository a pass holds back for room is
// given the room it needs, so a stream of small ones never starves it. A pool
// that cannot read the node's memory refuses to start, naming what: a budget
// guessed from nothing is the OOM it exists to prevent.
//
// # The directory
//
//	<git.cache_dir>/
//	  CACHEDIR.TAG                  the Cache Directory Tagging standard's tag
//	  lock                          this process's claim (internal/flock)
//	  repos/<id>.<n>.git/           a repository's INCARNATION: a bare repository
//	    crewlet-state.json          what the directory reflects, renamed in last
//	    objects-dropping/pack/      the packs an expunge's begin left dropping, which
//	                                only a git process serving the copy reads
//	    crewlet/provenance.<n>      the index of pushes' runs, which no git reads
//	    crewlet/memo/<n>/<key>      an answer of a read by object id, in cohort n, which no git reads
//	  scratch/                      a request's own repositories: a push's scratch, a view;
//	                                a merge's, an expunge's and its rescue's, each beside
//	                                the store it leans on; and a swap's next incarnation, staged
//	  tmp/                          every file before its rename into place
//	  trash/                        what is being removed, by the janitor
//	  hooks/proc-receive            the hook every push's receive-pack runs
//	  home/                         HOME for every git process — gitproc's
//	  lost+found/                   mkfs's, at a volume's root; never touched
//
// [Prepare] makes a directory a cache and [Open] opens it, in that order
// around gitproc.New, whose HOME the layout names. Prepare REFUSES a
// non-empty directory that does not carry this cache's tag, because Open
// empties tmp/ and scratch/ at every boot: git.cache_dir pointed at /var
// would otherwise empty /var/tmp. A directory holding nothing but mkfs's
// lost+found is empty — it is the root of a volume of the cache's own, the
// home the refusal recommends. And Prepare CLAIMS the directory for its
// process — the store's own rule, one file one process, through the same
// implementation (internal/flock) — and the claim passes to Open and is
// held until [Cache.Stop], because two engines pointed at one directory would
// empty each other's tmp/ at boot and race on every repository's files, and
// because what Prepare reads of the directory is what Open acts on.
//
// # Every write is a rename, and a step is synced once
//
// A file is STAGED in tmp/ and made durable there, then renamed over its
// destination: a git process that already opened the old file keeps its
// descriptor, a new one opens the whole new file, and a crash at any instant
// leaves either. What a file NAMES is durable before that file is renamed
// in: the files of one step are renamed into place and each directory they
// landed in is synced ONCE, before any file of a later step names them — so
// a round that installs a thousand packs into objects/pack pays one
// directory sync for them, not a thousand. So a READER NEVER SEES A REF
// BEFORE ITS OBJECTS: a pack's files go in before the refs that need them
// (the .pack, then the .rev, then the .idx — git counts a pack once its .idx
// exists), and packed-refs and HEAD before the state that claims their
// position. The state file is renamed in LAST, after everything it
// describes, so it is never ahead of the directory.
// It is constant-size — the installed pack set is the directory itself, a
// pack present once its .idx is (git's own rule) — because a list of every
// pack rewritten on every sync would be work proportional to the repository
// on the path of every push on every node.
//
// One file is the exception, and it is first: CACHEDIR.TAG is written in
// place, before tmp/ exists. Staging it would create tmp/ before the tag,
// and a crash between the two would leave a non-empty directory with no tag,
// which every later Prepare refuses for good; a tag cut short is recognised
// and completed instead.
//
// # Incarnations, and readers that pin them
//
// A reader is handed a repository's directory PINNED ([Synced]), and there is
// no other way to it: no path accessor skips the pin. Each repository's
// directory is an INCARNATION (repos/<id>.<n>.git). One taken out of service
// — replaced by a rebuild or a swap, its repository purged or evicted — is
// RETIRED: it stays where it is while a reader holds it, and moves into
// trash/ with its last release. Nothing renames a directory a reader holds —
// a path is a name, and a git process whose GIT_DIR is that name would
// otherwise open another directory's files half way through its run.
//
// # A pack leaves a copy a reader may hold by a swap
//
// A compaction RETIRES packs — the code domain moves their rows into
// code_packs_retired, the outputs holding every object of them — which a
// round learns from a delta's retired phase, or from the retirements a full
// answer serves above the copy's position; and a full round can meet a pack
// file no row names that no retirement accounts for — a log gone back, a row
// gone with no record retiring it. Neither leaves a copy a reader may hold IN
// PLACE: git opens a pack lazily, part way through a run, so no pin of a file
// could say which a running process will touch (ADR-0083). The round applies
// to its end on the incarnation it holds, and then SWAPS it (swap.go): the
// next incarnation is staged under scratch/ by HARD LINKS of every pack the
// live rows still name (the live set, kept by every round), HEAD, config, the
// packed-refs and each derived file the events of what leaves keep — a
// retirement keeps the commit-graph (X04: a merge drops no commit), a pack no
// retirement accounts for drops it, and both drop the multi-pack-index over
// the packs that left; an expunge's commit drops the graph and the memo and
// keeps the index, which never covered the packs it takes out — renamed into
// repos/ whole under the next number, and made current, the memo's tree moved
// into it whole as it is where nothing leaving drops it (carryMemo, as a
// rebuild's); the old one retires, whole for its readers until the last lets
// go. It costs links — three a live pack, one each for HEAD, config and
// packed-refs — and the memo's one rename, and no download and no git
// process, but a full round's own index, written over the next incarnation;
// and one sync a directory it wrote, made once the staging is complete and
// before the rename, since nothing reads the staging before it — four to six
// fsyncs, the state's and repos/'s among them, where synced step by step it
// took up to eleven, each while the round holds the repository; six to nine
// for a copy serving from a published chain, whose two directories are synced
// with the rest and whose two chain files are written from the rows.
// MEASURED (internal/code/compact's BenchmarkCompact, its swap arms: git
// 2.56.0 over the in-memory store, Go 1.27.2, one bounded run on two E-cores
// of an Intel Core Ultra 7 265U, which has SHA instructions, 2026-10-28), the
// sync that swapped a copy of 50 packs took 7.0 ms and linked 150 files, and
// one of 2 048 took 119 ms and linked 6 144 — the pack count's, never the
// bytes' — each with its download of the one output the compaction wrote
// from the in-memory store, which has no first byte to wait for. The
// promote carries every per-file verdict of the files it links (the same
// inodes) — their vouches, a hint's hash, the scrub's schedule, what a repair
// still owes them — and the repository's own records, every field of an
// incarnation classified in swapFields, which
// TestEveryIncarnationFieldIsClassifiedForTheSwap holds to the struct both
// ways. A maintenance run on the old copy is stopped first, and counted as a
// run of the duty cycle. What is learnt of the old copy's files after the
// swap belongs to the copy serving them now: a verdict reached over them;
// what a git process over them reports — that copy's scrub's evidence, never
// a drop, as is what the round's own index reports from the staged directory;
// and a request whose git failed over the old copy, answered from the copy
// serving as it stands, so it runs once more with its repair unspent. A swap
// that FAILS leaves the old incarnation serving — a superset of the rows —
// and the repository's next round tries again. A CRASH before the rename
// leaves the old one the highest in repos/ and the staged one in scratch/,
// which the boot moves aside; its state names the floor its leaving packs'
// retirements are above (LeavingSince), so the boot's first round reads them
// as the retirements they are and keeps the graph. An incarnation nobody can
// hold — one not yet converged and unpinned: a boot's first round, a
// rebuild's product — sheds what leaves it in place, as it always has.
//
// # The dropping view
//
// An EXPUNGE drops a repository's unreachable objects in two records
// (internal/code's ExpungeBegin and ExpungeCommit): its begin retires the
// packs it rewrote and adds what they held as packs KEPT, live, and as packs
// of what no ref reached when it was planned, DROPPING (code.PackDropping);
// its commit retires those, adding back live what a ref reached again
// meanwhile (its rescue). Between the two a dropping object is still the
// repository's to SERVE — to a client that already holds its id, a fetch of it
// by id, a read begun over it — and never one a PUSH may rely on: a ref landed
// on one would name an object the commit takes out of every copy. So an
// incarnation keeps a dropping pack apart, in an object directory of its own
// beside objects/ — its DROPPING VIEW, objects-dropping/pack
// (gitproc.DroppingDir) — which a git process reads only when it names it
// (gitproc.Cmd.Dropping, set in its environment as
// GIT_ALTERNATE_OBJECT_DIRECTORIES; TestOnlyServingReadsNameTheDroppingView
// holds every process that does to a closed table):
//
//   - EVERY PROCESS THAT SERVES THE COPY names it: upload-pack's
//     advertisement and its fetch (internal/code/host), the check of a
//     fetch's wants ([Cache.Missing]) and every read internal/code/query makes
//     over the copy, each handed it with the copy ([Synced.Dropping]: the view
//     where the live set places a pack there, "" where it places none); and
//     the cache's own reads of what the refs reach — a round's peel, a
//     placement's canary and Verify's connectivity check — name the view the
//     directory holds (heldView), which a round fills before its live set says
//     so.
//   - NOTHING THAT WRITES OBJECTS does: a push's scratch, an advertisement
//     view and an engine write lean on the copy's objects/ alone, their one
//     alternate, and a compaction's build on its merge's store, links of its
//     inputs from objects/pack, which refuses a pack only dropping rows name
//     (mergedLive); an expunge's build leans on a store of every live pack it
//     does not sort ([Cache.StageExpunge]), and its rescue on the copy's
//     objects/ alone, the dropping packs it sorts linked into its own
//     objects/pack ([Cache.StageRescue]) — so `--local` packs what only the
//     view holds; and the pool refuses a call naming the view beside an
//     alternate — never an alternates file naming it, which git follows from
//     every repository that names the copy as ITS alternate. And git loads
//     the first commit-graph its object directories hold, an alternate's
//     included, which a retirement keeps (X04) and so may name a commit only
//     a dropping pack holds: a process leaning on a copy that holds a view
//     has git hold every commit a graph names to the objects it can reach
//     (gitproc's GIT_COMMIT_GRAPH_PARANOIA), so its connectivity check never
//     passes over one.
//
// WHERE A PACK LIES is its rows' (packPlace): objects/pack where any live row
// names its checksum — a live row is one a push may rely on — and the view
// where only dropping rows do. Everything that places, keeps or judges a pack
// reads the place from the live set (placeOf) or a snapshot
// (snapshot.places): the install, which makes the view with the first pack it
// places there (ensureView); a delta's live set and a full round's sweep,
// whose strays are a pack's files where its rows no longer place them; the
// swap, which links each pack where its rows place it, so a next incarnation
// holding no dropping pack holds no view; the check, the scrub and Verify,
// which hold a pack at its place and name its files at the other a stray; a
// rebuild's seed; and the boot's walk of a copy with no state. A pack its rows
// move from one place to the other — a row of its checksum added or retired
// in the other state — is downloaded to its new place, its files at the old
// leaving with the swap. NO DERIVED FILE IS BUILT OVER THE VIEW: an index
// never covers it, the round's — over objects/pack's packs alone — and the
// maintainer's alike, and no published layer names a pack in it, nothing
// being published while an expunge is open. The budget counts its files (the
// state's DiskBytes, DroppingBytes of them, which a boot restores), and
// crewlet.code.cache.dropping.bytes is the node's total.
//
// THE EPOCH A COPY'S OBJECTS/ HOLDS ([Synced.DropEpoch]). A push carries the
// repository's drop epoch as the copy its scratch leaned on held it, and its
// committer refuses one of another (code.RefusalExpunging), so a push
// checked before a begin is never decided after it. That is the rows' epoch
// only once the copy holds every pack file where its rows place it: a begin's
// inputs hold everything it made dropping, and they leave objects/pack by the
// swap at the end of the round that applied it — so a copy whose round
// failed to swap, or that a reader was handed while the round ran, is at the
// epoch before. And a pack an installed layer names, which the chain holds
// past its row (layers.go), may hold what a later begin made dropping, so the
// epoch moves past a begin only once the chain holds no pack for a row that
// is not live: a delta that finds the rows past the copy's epoch takes every
// such pack out with its swap, cutting the chain below it — the cut the begin
// made of the rows' chain.
//
// A REF MAY REACH A DROPPING OBJECT between a begin and its commit — a push
// decided after the expunge read the refs and before its begin landed relied
// on objects then live, which the commit's rescue keeps live again. Until
// then every serving read reads it through the view, and a push that builds on
// it is refused by its own connectivity check, its scratch reading nothing of
// the view.
//
// THE COMMIT TAKES THE VIEW OUT, and with it what the expunge purged: a
// dropping pack no row names any more leaves the copy as EventPackExpunged —
// a delta's retired row whose live entry was dropping, and a full round's
// stray in the view, whatever accounts for it — by the swap where a reader may
// hold the copy, which then holds no view. It is the one change that DROPS THE
// COMMIT-GRAPH where a retirement keeps it (X04's one drop): the begin's own
// retirement keeps it, every commit then still in the copy, while after the
// commit a graph naming one it took out has git 2.56 answer `rev-list -1 <it>`
// and walk through it as though it were there (measured). The MEMO goes with
// it — an answer may quote what was purged — and the node's TIP over an
// installed chain, which may name a commit taken out; the index and the
// published layers stay, neither covering nor naming the view.
//
// A RESCUE BYTE FOR BYTE ONE OF THE DROPPING PACKS it was cut from — the
// rescue of a dropping pack a ref reaches whole — names that checksum live,
// so its files only MOVE out of the view, a retirement keeping every object:
// by a delta's move on every node, and on the maintainer that built it by
// the adoption (adopt.go's liveAdopted), which places it live before the
// commit's delta reads the row that moves it, so no delta finds it moved.
//
// # The installed chain
//
// A repository's MAINTAINER, one git node, writes its multi-pack-index as a
// chain of PUBLISHED LAYERS — git's own incremental layer, its bitmap and the
// commit-graph layer over the same packs — stored in the object store and
// named on the code log (code.LayerRef; derived, so no backup carries one and
// no audit asks after one); every git node INSTALLS the chain the rows name
// (layers.go): each layer fetched, held to its row (code.CheckLayer) and
// placed byte for byte, both chain files written by the cache from the rows'
// order, since a layer's files never name its base. The maintainer's
// publisher writes them (internal/code/compact); the cache installs whatever
// the rows name, whoever wrote them. No node writes a
// layer of its own: on the node pin a bitmapped layer costs the walk of the
// whole history however few objects it adds, and a layer without one turns
// off every bitmap beneath it, so the packs since the last publication stay
// outside the chain, the TAIL, which git reads beside it. A commit-graph
// layer costs only the commits it adds and turns nothing beneath it off, so
// each node writes ONE thing over the chain: its TIP, git's split
// commit-graph layer of the tail's commits over exactly the installed graph
// layers, written whole again once the tail's packs are no longer those it
// covered rather than stacked — which packs those were persisted beside it as
// their digest, so a restart writes a quiet copy's tip no more than a pass
// does (maintain.go, THE TIP); it goes with every change to the chain beneath it
// and every pack removed, and crosses a retirement that keeps the whole chain
// (tipGoes).
//
// An incarnation holding a chain is in CHAIN mode, one holding none in OWN
// mode (the maintainer's whole index and graph); git refuses the two side by
// side, so a placement drops own mode's files (EventLayersMoved), the
// maintainer plans no own-mode write in chain mode — the tip alone — and a
// round writes no index beside either shape (indexed). A FULL round installs the chain before it answers,
// keeping the longest prefix the copy holds sound — held to its Check by the
// round's check — and fetching the rest in its one download queue behind its
// packs. A DELTA never downloads a layer: it records the chain the snapshot
// served and hands what the copy lacks to the INSTALLER, the goroutine Start
// begins, which fetches behind the serving on at most a round's share of the
// node's downloads and places under the repository's write lock, holding each
// repository once — due in order, or waiting by when its backoff ends — so a
// pass costs what it takes up, never what it holds. THE CHAIN HOLDS ITS
// PACKS: a pack an installed layer names stays in the copy, its row retired
// or not, until a placement replaces the layer, and a stale layer whose
// replacement cannot be placed is cut after installRetryCap of its own. A
// PLACEMENT is a swap on a copy a reader may hold (swap.go's layer step) and
// in place on one nobody does, and before either is visible a CANARY reads
// once through the chain's bitmap — a layer over the wrong base passes every
// check of its own files and fails the first bitmapped read. A layer
// missing, corrupt, or written by a newer git is REFUSED, never an error: the
// copy serves from the prefix below it, the refusal kept in memory while the
// rows name that very record. A failed check or a scrub's finding is the full
// round's chain plan's to answer, never a drop: what it keeps stops below the
// failing layer (prefixOf, the one cut both chains share), and the round
// fetches that layer and every one above it again before it answers, cutting
// what it cannot fetch; a git process's report drops nothing in chain
// mode and makes the scrub due; a rebuild seeds every layer it holds to its
// row over packs it linked; Verify holds each layer to its row; and the
// maintainer's own files are adopted rather than fetched (AdoptLayer).
//
// # A boot starts from the disk, once
//
// A boot reads nothing but the directory, and reads it ONCE: Prepare reads
// repos/ under the claim and Open acts on that reading. Every repository's
// highest incarnation is kept with its state, and everything else under
// repos/ — a lower incarnation an interrupted rebuild left, a stray, a state
// of another build's format — moves into trash/. An incarnation whose state
// is missing or unreadable is KEPT, with no state, rather than thrown away:
// an interrupted first download of a large repository is not downloaded
// again. What the cache holds ([Layout.Held]) is summed from the same
// reading — each kept incarnation's state counts it — so a warm node's boot
// costs a small read a repository, never a stat of every pack: at one pack
// a push, a node caching a million pushes holds three million files.
// Everything the boot GIVES BACK — an older incarnation a crash left beside
// the kept one, and what trash/, scratch/ and tmp/ hold — is walked and
// counts each file ONCE, by its inode, and only where every name of the file
// lies in it: the kept incarnation holds hard links of what it shares with
// an older one — a rebuild links every pack a check found whole, a swap
// every live pack, HEAD, config, packed-refs and the derived files it links
// — and so do a copy a swap replaced, sitting in trash/ when a stop cut the
// janitor's removal short, and a failed swap's staging; its state counts
// those, so counted by every name they made the boot's budget claim room the
// volume did not have. The janitor removes what is in trash/ in the
// background, so a boot never waits on a large removal.
//
// A repository's LAST USE comes back from a restart too, to within
// [EvictInterval]: the boot reads it from the state file's modification
// time, and the janitor stamps each repository used since its last tick
// there — a metadata write, never a rewrite of the state — so a repository
// that is read all day and pushed to rarely is not the first a budget evicts
// after every restart. A state written for any reason carries the last use
// too, never the moment of the write (writeState): the maintainer's runs, the
// backfill and the memo's moves write states for copies nobody read, which
// would otherwise come back from a restart as the copies read last.
//
// # Measured
//
// On four vCPUs of a 2.1 GHz Xeon shared with other builds, on ext4 (make
// bench, six counts, medians; bench_test.go says what each arm is) — and the
// warm boot on four of a 2.8 GHz one, measured again for the state's format
// 2, each run alternated with one of the build before it so the machine's
// load fell on both alike: a warm boot — Prepare, Open and Stop — of 100
// repositories takes 17 ms with 100 packs each and 15 ms with 1 000 each, so
// what a repository holds is not in the figure, and of 1 000 repositories
// 57 ms, about 45 µs a repository past 12 ms of fixed cost (the hook's settle
// among it): about half a second at 10 000 repositories, config.CodeMaxRepos.
// Most of a repository's share is the read of its state (BenchmarkReadState:
// 31 µs, 28 allocations and 5.7 KiB), and format 2's one map of the derived
// files — an entry a declaration — is about 6 µs, five allocations and 1 KiB
// of it: format 1, which spelled the two files' facts as fields, read in
// 25 µs, so the map costs a boot of 10 000 repositories about 60 ms. The
// declarations the read holds a state to are built once a process
// (declaredSteps): built once a state they were 11 allocations and about
// 3 µs more again. The same 1 000 repositories with no state to count —
// every incarnation walked, which is what every boot paid when Held was a
// walk of repos/ — take 2.0 s for their 300 000 files, about 7 µs a file, so
// a node caching a million pushes would have spent around twenty seconds warm
// before it could serve. A step that
// renames 300 durable files into one directory costs 30 µs a file with one
// directory sync after the step and 274 µs a file with a sync after every
// rename, which is why durable.go syncs a step's directory once. A push's
// advertisement in a view costs about what it does in the cache once the
// view stops listing its alternate's refs (AdvertiseConfig): 18 ms against
// 14 ms at 10 000 refs and 117 ms against 112 ms at code.MaxRefsPerRepo
// (100 000), where the listing doubled it, to 26 ms and 222 ms.
//
// The byte path, on the same machine (nproc 4) with git 2.43.0, its
// CREWLET_BENCH_LARGE=1 arms too — the 5 GB index and the lookups over a
// gigabyte and over 10 000 packs at three counts, everything else at six —
// but the cold syncs, measured again on git 2.56.0 at the pack counts a
// repository's compactions leave it (BenchmarkColdSync, one bounded run of
// each arm, -benchtime 1x and one count, Go 1.27.2, the S3 fake read through
// aws-sdk-go-v2/service/s3 v1.114.3, on two E-cores of an Intel Core Ultra 7
// 265U, which has SHA instructions — every byte a cold sync reads is hashed —
// 2026-10-27 and 28). Every cold-sync figure is the S3 FAKE's,
// whose first byte is the arm's rather than a bucket's: what a first byte
// that long costs the node. A COLD SYNC of pushes of 150 KB, one pack each,
// from the S3 fake at 30 ms to every request's first byte and 80 MB/s a
// connection, takes 2.0 s one GET at a time, 0.26 s at FetchConcurrency's
// sixteen and 0.13 s at NodeFetches' thirty-two over the fifty packs a
// compacted repository is held near (code.CompactTargetPacks), and 75.5 s,
// 5.3 s and 2.9 s over the 2 048 a repository holds at the most
// (code.MaxPacksPerRepo) — 27, 389 and 705 bundles a second: no git runs but
// the round's index and its peel, so a cold sync is bound by its GETs and the
// node's own side (below), never by a core. Sixteen is bound by its GETs, at
// 86% of the 454 a second sixteen of the 35 ms each took allow. S3 itself
// answers in about 100 to 200 ms (AWS's performance guidelines), and with the
// fake answering that late the 2 048 take 13.8 s and 26.6 s at sixteen — the
// downloads 13.6 s and 26.4 s of it, inside the 27 s MaxPacksPerRepo is held
// to — and 7.0 s and 13.7 s at thirty-two: 149 and 77, and 292 and 149,
// bundles a second, at 104 and 204 ms a GET, bound by the GETs alone, within
// a few percent of fetch over first byte. So the node's own side bounds a
// sync only at a first byte under about 35 ms; the fifty of a compacted
// repository come up in 0.43 s and 0.88 s at sixteen, and the 10 000 of a
// history nothing compacted — the code host's first build, one pack a push —
// in 69.9 s and 129.6 s.
// The node's own side is a bundle's FILE WORK (BenchmarkBundleFiles: its files
// created, written, synced and renamed, without the GET or the hashing):
// sixteen at once, about 700 a second whether the syncs are made or not, bound
// by creating the files — a third of a millisecond each, one at a time in tmp/
// — while one at a time the syncs halve it, 386 against 826 a second, which is
// why a bundle's three are issued together and its GET's share is free before
// them. One push of 512 MiB among 200 makes the round 9.1 s, 2.5 s and 2.6 s:
// past one GET at a time it is that bundle's own read, four ranges at once
// (s3obj), that ends the round, and the whole node's width buys it nothing. A
// DELTA SYNC — one push of 150 KB on a warm cache — costs 7.7 ms on a
// repository of 1 000 refs and 56 ms at code.MaxRefsPerRepo (100 000), and
// 8.6 ms and 61 ms at 10 000 packs: with the fake log's walk of every ref
// taken out, about 7 ms and 46–48 ms whatever the pack count, the second
// packed-refs rewritten whole.
//
// MANY REPOSITORIES AT ONCE (BenchmarkColdJoin, one run at -benchtime 1x, git
// 2.56.0 on four cores of a Core Ultra 7 265U), every repository's read one
// page, against ONE data node modeled as the smallest — its sixty-four
// answers and its eight readers, each page holding one of each for 60 ms, a
// full page of refs to scan and the estate's crossing, and a reader given
// back to a waiting read drawn at random, as database/sql's pool does —
// beside two hundred other reads a second. ONE NODE RESTARTED over the cache
// it kept, asked for its 500 repositories at once, downloads nothing: it came
// up in 7.6 s with its full reads capped at four and in 3.9 to 4.3 s at eight
// and every cap above it, the readers' own rate, while the other reads
// waited 0, 105, 323, 842 and 1 030 ms at the 99th percentile at caps of four,
// eight, sixteen, thirty-two and sixty-four (at most 1 µs, 128 ms, 563 ms,
// 1.1 s and 1.5 s), and 3.6 s uncapped. FOUR NODES JOINING together with
// fresh caches, 500 repositories each: the 2 000 reads took about 16 s at
// every cap, while the other reads waited 352, 792, 1 030 and 1 556 ms at
// four, eight, sixteen and thirty-two (at most 0.6, 1.6, 2.5 and 2.5 s), and
// 15 s uncapped. The other reads' wait follows the full pages in flight per
// reader, whichever nodes hold them, and a cap past the readers brings no read
// sooner: the figures MaxFullRounds is set from.
//
// The INDEX OF PUSHES (BenchmarkProvenance, five operations of each arm, on
// the cold join's machine: four cores of a Core Ultra 7 265U, git 2.56.0). A
// delta sync's fold of one push — its run built, staged and synced, renamed
// in, its directory synced — takes 0.84 ms of a 4.2 ms round whose pack comes
// from a store in the process, a fifth of the cheapest round there is and its
// two syncs most of it, and 1.23 ms of a 68 ms one from the S3 fake at 30 ms a
// first byte (1.8%); with the merges its folds leave due taken between pushes
// (64 pushes on two E-cores of the same machine), a push costs 0.125 merges
// and 2.5 of the index's syncs — a fold's two and an eighth of a merge's four
// — and a seventh of a merge over a long run, each merge of eight taking seven
// runs away, where merged a pair at a time it merged after nearly every push:
// about six syncs. Runs of pushes of three commits each hold 64 bytes for
// each commit introduced, about 190 a person's push; a seat's push holds its
// run beside, each field its bytes and two more (its tag and its length) —
// about 80 at a run's usual sizes, a turn id's 36 bytes, a tracker's work
// item and a model id, and a coding box's push about 50 more, its run id's 36
// bytes and a coding agent's name ("claude-code") — by the format's arithmetic
// rather than a run of the benchmark, whose pushes are a person's. A lookup that misses, every run
// searched, over four million commits: 0.54, 3.7, 15.5 and 29.8 µs at 1, 8, 32
// and 64 runs ([MaxProvenanceRuns] says what that decides). A cold node's
// backfill of 30 000 pushes through a solo node — the data node's read, in
// process — read four pages of about 7 500 pushes, each about 3.9 MB, the
// read's 4 MiB bound reached before its 10 000 rows: 115 ms of the data node's
// read and about 148 ms a step, and 1.78 s in all at [BackfillIdle]'s pace.
// A merge of two runs of 100 000 pushes each (op=merge, three operations on
// two E-cores of the same machine, without the race detector) takes 134 ns a
// push where every push moves one branch and 665 ns where each moves a ref of
// its own, allocating 294 KB either way — the writer's buffer and one key's
// ordinals — where the merge that held both runs' refs by name took 2.1 µs a
// push and 519 bytes a ref (its review's measure, 198 MB for 400 000 refs).
// The backfill's pick of its next step among 10 000 copies
// (BenchmarkBackfillPick, a thousand operations on the same two E-cores)
// takes 0.73 µs where one copy has a step due and 2.5 µs where every one has
// — the warm-up after a restart — and a request's lookup of its copy beside
// picks run back to back waited 0.35 µs at the 99th percentile: the walk the
// schedule replaced held the cache-wide lock 56 to 127 ms a step at 10 000
// copies, and a request up to 280 ms behind walks run back to back (its
// review's measure).
//
// The MULTI-PACK-INDEX, over a source repository's composition — every commit
// four files of one directory, so commits are a seventh of the objects
// (BenchmarkMidx says how the history is built). On the node pin (git 2.56.0;
// one bounded run of each arm, -benchtime 1x and one count, on two E-cores of
// an Intel Core Ultra 7 265U, 2026-10-29; step=whole, the maintainer's
// declared writes over every pack after a tail of 64 pushes) the
// maintainer's index with its bitmap and the commit-graph with its
// changed-path filters take 1.16 s at 100 MB (296 000 objects, 42 000
// commits) and 14.2 s at 1 GB (3.0 million, 424 000) — 11.6 and 14.2 s a
// gigabyte, growing faster than the bytes as on git 2.43, whose 1.6 s, 21 s
// and 152 s at 5 GB (14.8 million, 2.1 million; 16, 21 and 30 s a gigabyte)
// are the one 5 GB figure there is — so MidxMinInterval rather than
// MidxDutyCycle paces every repository under about 3 to 4 GB of packs, and
// gitproc.PackWorkDeadline gives the gigabyte sixteen minutes, about 68
// times its run; the index without a bitmap 0.08 s and 0.65 s (on git 2.43
// the round's, 0.12 s, 1.1 s and 6.1 s). The bitmap's lookup table pays every
// request that opens it: over the gigabyte (git 2.43, three runs, medians)
// the pack a blobless clone's lazy fetch of one blob is served takes 5.0 ms
// with it and 6.2 ms without, and the pack a fetch of one new commit 8.2 and
// 9.0 ms — the difference what each spent reading the entry of every commit
// with a bitmap before it answered. Pushes of 4 KiB landed beside the
// maintainer's index cost counting a clone's objects about 0.06 ms each at
// 100 MB and 0.08 ms at 1 GB on the pin (3.8 ms with none and 11.8 with 128;
// 5.5 and 15.4 ms), and a clone's whole pack-objects nothing a run shows up
// to 128 (0.64 and 0.72 s; 6.50 and 6.79 s): the figures MidxPacks is set
// from, which git 2.43 put at 0.06 and 0.09 ms. Over the
// 100 MB split into packs (BenchmarkPackLookup, git 2.43), a clone's pack-objects takes
// 3.8, 5.1, 9.7, 12.3, 17.0 and 26 s across 8, 32, 128, 256, 512 and a
// thousand packs with no multi-pack-index, 4.0, 5.2, 9.6, 11.4, 14.0 and 18 s
// with the round's, and 0.83, 0.95, 1.44, 2.24, 3.72 and 6.3 s with the
// maintainer's; Missing over 1 000 commits 3.9 and 4.2 ms at eight packs with
// none and with the round's, 9.0 and 7.9 at 128, 17 and 12.6 at 256, 36 and 20
// at 512, 87 and 37 at a thousand; a fetch of the newest four commits 12.4 ms
// either way at eight, 13.9 and 13.2 at 128, 30 and 21 at a thousand — and
// with the maintainer's bitmap 13.5, 15.1 and 38 ms; and the round's index
// itself 105, 110, 117, 127, 135 and 162 ms. Over the gigabyte, at three runs,
// the round's index costs 1.3 s at 128, 256 and 512 packs and saves a clone
// 3, 20 and 69 s (114 against 111, 154 against 134, 236 against 167), a fetch
// 4.7, 2.0 and 3.6 ms, where the maintainer's bitmap clones in 14, 18 and
// 29 s. Where the threshold is decided the clone was measured again, six runs
// of each with their spreads: at 128 packs the round's index saves it nothing
// either size shows — 10.1 s with none and 10.2 with it over the 100 MB
// (10.0–10.8 and 9.6–10.7), 110 and 111 s over the gigabyte (108–111 and
// 104–116), which the three runs' 3 s was inside — and at 256 it saves 0.55 s
// and 11 s (12.9 against 12.4, 12.6–13.5 and 11.9–12.9; 151 against 140,
// 145–160 and 133–143), for writes of 0.13 s and 1.3 s: the figures
// MidxRoundPacks is set from, and the pin confirms them (git 2.56.0, the
// 100 MB at 128, 256 and 512 packs, one run each, 2026-10-29: the round's
// index, written in 0.11, 0.08 and 0.09 s, saves a clone 0.11, 0.62 and
// 3.1 s — 6.31 against 6.20, 8.42 against 7.80, 12.8 against 9.7 — where
// the maintainer's bitmap clones in 0.81, 1.31 and 2.14 s). Across 10 000 packs of the 100 MB the round's
// index costs 0.53 s and saves a clone 122 s (146 against 24), Missing over
// 1 000 commits 520 ms and a fetch 94 ms, while the maintainer's bitmap,
// which opens every pack it covers, clones in 32 s and fetches in 300 ms:
// past a thousand packs what a repository needs is fewer of them. Peeling
// 5 000 tags in as many packs takes 0.65 s without the round's index and
// 0.23 s with it, for a 0.19 s write, and a thousand 57 and 45 ms for a 35 ms
// write: the peel is no reason for the round's index below thousands of
// tagged packs.
//
// The MEMORY each whole-repository git process holds (the one process's peak
// anonymous memory, sampled every millisecond), over the 100 MB and the
// gigabyte. The maintainer's writes on the node pin (step=whole, the run
// above): the index without a bitmap 18.6 and 182 MiB (66 and 64 bytes an
// object), with its bitmap 158 MiB and 812 MiB (560 and 287 — some 86 MiB
// and about 257 bytes an object past it, where git 2.43 held 136 and
// 1 985 MiB, 484 and 702, more an object the larger the history), the
// commit-graph with 1 024 new changed-path filters 15.6 and 126 MiB (55 and
// 45; 12.7 and 112 MiB without them on git 2.43) — and computing every
// filter, as each graph's first write does, 112.8 and 250.1 MiB over a
// deeper tree's 481 160 and 4 198 280 objects (internal/code/query's own/
// arms, 2026-10-31), the delta base cache's 96 MiB of it. The rest on git 2.43
// (BenchmarkMidx's memory arms): the connectivity check 110 and 452 MiB (390
// and 160), and a clone of main 132 MiB and 670 MiB walking the history with
// no bitmap, 36 and 330 MiB counting from the maintainer's — the figures BitmapBytesPerObject,
// IndexBytesPerObject, GraphBytesPerObject, FsckBytesPerObject and
// gitproc.TransferBytesPerObject are set from. Each process's peak resident
// size, the packs' pages it maps included, was about one and a half to two
// times its anonymous memory over the 100 MB (VmHWM: 243 MiB for the bitmap),
// which the kernel takes back under pressure.
//
// A CHECK (BenchmarkVerify, at three runs and two over the gigabyte, which
// only CREWLET_BENCH_LARGE=1 builds) of a whole copy takes 9.5 ms over five
// pushes of 150 KB, 0.89 s over the 100 MB and 8.6 s over the gigabyte of the
// composition above — 9.4 and 8.9 ms a mebibyte, git's connectivity check
// nearly all of it: hashing every bundle back to back reads at about 1.1 GB/s
// from the page cache with the CPU's SHA extensions (0.95 ms a mebibyte at
// both sizes). A repair of one damaged pack — the check, the rebuild linking
// every other pack in and downloading that one from a store in the process,
// the check again — 30 ms, 1.5 s and 18.4 s. CheckBudget gives a pass about
// 1.1 s a mebibyte, the connectivity check's gitproc.PackWorkDeadline and the
// hashing's VerifyHashPerMiB.
//
// What a CLONE IS SENT, and what its pack-objects costs the node, under each
// value of ServeConfig's pack.allowPackReuse (BenchmarkPackReuse: pushes
// received thin, a cold round's install and the maintainer's bitmap,
// seventeen copies of each history on 2.56.0 and seventeen on 2.55.0, each
// arm held three to five times): over 200 pushes adding files 38.2–38.5 MB
// and 0.82–0.91 s of CPU with single, 41.6–41.8 MB and 0.94–1.14 s with
// multi; over 200 pushes of gittest.History's edits 20.8–21.9 MB and
// 0.50–0.56 s, 30.7–34.0 MB and 1.52–1.98 s; and over those edits once all
// but their last 20 pushes are one compacted pack 17.3–17.4 MB and
// 0.19–0.24 s, 19.1–19.2 MB and 0.30–0.34 s — multi against single on each
// copy 8.5–9%, 48–57% and 10% more bytes for 8–30% more CPU, 3.0–3.7 times
// and 45–71% more. On every copy the bitmap preferred one push's pack, about
// 0.5% of the objects (git's choice with no --preferred-pack, reported
// beside each arm with its rank by objects: 1, 10 and 81 seen). Which pack
// that is, and which copy of each edited object the multi-pack-index keeps
// (serve.go), are chosen by when each pack landed, to the second, and by the
// order the filesystem lists the pack directory, so the edits' bytes moved
// from copy to copy by up to 5% under single and 11% under multi; single
// sent fewer bytes for less CPU on every copy: the figures ServeConfig's
// single is set from.
//
// THE MEMO'S TRIM AND ITS LISTING (BenchmarkMemoTrim, three passes an arm,
// -count 1, on two E-cores of a Core Ultra 7 265U over ext4, no race): over
// warm copies whose memos hold seventeen cohorts each, the janitor's pass that
// drops the oldest cohort of every copy — the walk of the node's copies, a
// rename a copy, and the removal of what it renamed from trash/ — takes 55 ms
// at 1 000 copies and 0.66 s at 10 000, 55 and 66 µs a copy, whatever the
// cohorts hold: a trim reads no answer, and it is due once a sixteenth of the
// share has been placed. The boot's listing of every copy's tree, its
// cohorts' names alone, takes 15 ms and 0.17 s, 15 and 17 µs a copy. Before
// cohorts, a trim copied and sorted the last use of every entry the node
// knew at every pass the memo was past its share — 178 ms for that alone over
// 1 048 576 entries, as the review that replaced it measured on the same
// cores, some 1 s at the 6.1 million of a 400 GB budget's worst case — and
// fell due again at the next miss.
package cache
