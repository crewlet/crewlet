// Package cache is THE NODE'S DISPOSABLE BARE REPOSITORIES: one per repository
// a Gitlet node has been asked to serve, from which real git upload-pack answers
// clones and fetches.
//
// # A cache is a function of rows
//
// A cache is a repository's rows (internal/gitlet) as files, never the only copy
// of anything: the backup skips it (CACHEDIR.TAG too) and deleting it loses only
// time. Its refs and packs are exactly code_refs and code_packs at its state's
// position, and [Cache.Sync] writes it by CONVERGENCE — derive the target from the
// rows, make the directory equal — never by replaying events (ADR-0084).
// TestNothingButSyncWritesACache holds the writers to a reviewed list and seeded
// interleavings to caches equal to ones rebuilt from nothing.
//
// # Convergence, priced for a push on every node
//
// A sync after a push elsewhere is a DELTA, costing what changed, never the
// repository; it neither lists nor sweeps the directory. A repository's FIRST
// round in a process reads the WHOLE answer and sweeps to the image, which makes
// the cache exact after a crash, an rm or a restored log; until then a previous
// process's state answers nothing, since a restored log reuses its positions.
// A cache NEVER MOVES BACKWARDS FOR A STALE COPY, only when the LOG did: a
// recreated log shows in the stream identity, a restored one (ADR-0083) as a
// LINEARIZABLE answer below a position this process read, and either marks every
// repository to be read whole. A full answer below the cache, or "no such
// repository", is asked again at the log's head before it is acted on. One
// shape escapes: a broker restored under a RUNNING node and written past every
// position it read. The restore runbook's stopped fleet rules it out; a node
// that may have met it is restarted.
//
// # A push never runs in a cache
//
// Under protocol v2 any object present is fetchable by id, so a pack a refused
// push left would be served and built on by this node alone. receive-pack runs in
// a SCRATCH ([Cache.NewScratch]) whose one alternate is the cache's objects, its
// advertisement in a VIEW ([Cache.AdvertiseView]); a compaction's build
// ([Cache.NewMergeScratch]) leans on links of EXACTLY THE PACKS IT MERGES, since
// `--stdin-packs` walks everything its alternate reaches (scratch.go's A MERGE'S
// REPOSITORIES). A pushed pack enters only once its row applies ([Cache.Adopt],
// or a sync); an adoption never waits, handing a busy writer its staged files.
// [Cache.Missing] finds a fetch's first absent want; damage it meets is answered
// as damage (gitproc.Result.Damage), since cat-file calls an object behind a
// truncated index `missing`.
//
// # The byte path is shaped for S3
//
// S3 GETs want WIDTH. A push stores ONE object, its pack BUNDLE (pack, index,
// reverse index), so an install is one GET and three renames and no other node
// runs index-pack. Up to [FetchConcurrency] downloads run at once, largest first,
// at most [NodeFetches] on the node, half of it, so one repository never starves
// another; a slot is held for its GET alone, and a bundle's three files are
// synced at once beside others, sharing journal commits.
// Every byte is SHA-256 verified against its row and gitlet.CheckBundle; a
// mismatch is never retried, since asking again fixes neither. A pack the store
// lacks is asked about at linearizable by a read after the not-found, since a row
// may have left the log and its object its grace (ADR-0081). A full round leaving
// [MidxRoundPacks] or more packs writes a bitmapless multi-pack-index before it
// answers; below that it costs a cold request more than it saves (Measured).
//
// # One sync at a time, shared
//
// A repository has one round in flight and one queued. A stale caller is answered
// from what the cache holds; a LINEARIZABLE one only by a round begun after it
// arrived (statelog.ReadIndex's rule), so a burst of clones costs one sync. Rounds
// run under the cache's lifetime: a client hanging up cancels nobody's round.
//
// # A node reads few repositories whole at once
//
// Each page of a FULL READ holds one of a data node's reader connections, eight
// on the smallest, so a node runs at most [MaxFullRounds] full reads, first come
// first served: a node coming up asks no more than a data node scans at once, and
// never starves the fleet's other reads. A DELTA holds no slot until its second
// page. The wait is part of each caller's sync, never a budget of its own.
//
// # Kept while there is room
//
// A cache is evicted to its budget (gitlet.cache_max_bytes), least recently used
// first, and for no other reason: an idle repository costs disk the budget
// bounds, and evicting it costs a GET a pack at its next fetch. A held repository
// is never evicted, even over the budget. The budget counts each file once, and a
// round past it kicks the janitor; the rename into trash/ comes after the lock.
//
// # What each repository costs the node is counted, and decides nothing
//
// [Heat] (heat.go) counts what this node served of each repository and spent on
// it over [HeatHour] and [HeatDay], read by [Cache.Status]. IT DECIDES NOTHING:
// eviction and maintenance never read it; a person or a seat judges it. IN
// MEMORY, since it describes one process ([Status.Since]), on the monotonic
// clock. Not a metric, since repository ids are no closed set: the status names
// the [HotRepos] hottest.
//
// # Derived files
//
// A DERIVED FILE is one the cache writes from an incarnation's packs or refs that
// no row names, each a DECLARATION in DerivedFiles (derived.go, ADR-0086): its
// inputs, writes, owned files, check, and what each change to its inputs does to
// it. Eight: the installed chain's two halves, the node's own multi-pack-index
// and commit-graph, the index of pushes, the memo, the code index (codeindex.go)
// and the tag index. A retirement keeps the commit-graph (X04: a compaction drops no commit); an
// expunge's commit drops it ("The dropping view"). Every reader reads the
// declarations and nothing spells a derived file beside them; Open refuses one
// that leaves anything unsaid, and TestEveryDerivedFileIsDeclared holds them to
// what real git writes. Every image file is REGULAR: git reads through a link,
// outside the incarnation and the budget, so a full round trashes one unfollowed.
// A state of another format is another build's, trashed and synced cold: nothing
// migrates a disposable cache.
//
// # The index of pushes
//
// Which push introduced each commit and moved each ref (provenance.go), built
// from push rows, answered by [Cache.Provenance]. A delta folds its pushes before
// it answers; one backfill goroutine reads the rest from a SCHEDULE (schedule.go),
// never a walk under the lock every sync takes, at a session floor the index has
// seen. Its runs are named by one state record changed by compare-and-set, and at
// most a quarter of vm.max_map_count is mapped (MappedRunShare), past which every
// mapping of the process, the Go heap's too, fails.
//
// # The memo
//
// Answers of reads by object id (memo.go), keyed by everything they are a
// function of (NewMemoKey): never stale, only absent. The tree is COHORTS, an
// entry in the newest when placed or read, so the janitor holds the memo to a
// MemoShare-th of the budget by DROPPING THE OLDEST COHORT whole; memory holds
// cohorts, NOTHING AN ENTRY. A rebuild or a swap moves the tree whole.
//
// # The tag index
//
// One file a copy, crewlet/tags (tags.go): every tag in git's creatordate order, then
// each commit's tags, renamed in with packed-refs. A failed step sets the index aside,
// never the round, and past TagsRetry a read asks a round for it.
//
// # What this process did not write is checked before it serves, and every byte is scrubbed behind it
//
// git checks no index, bitmap or graph as it reads, and a damaged one answers
// WRONG. So a full round holds every such file this process has neither written
// nor VOUCHED for (by inode, size and, but for a .pack, mtime; never ctime, which
// links move) to its trailer before it answers (check.go), by gitlet.HashTrailer,
// the rule a published layer is checked by too (ADR-0008), one read slot a piece.
// A failing derived file is DROPPED, never written over; a failing pack is
// INSTALLED AGAIN WITH ONE GET over its files, never removed first. Findings wait
// on the REPAIR MARK until acted on. A SCRUB (scrub.go) hashes every byte behind
// the serving, one on the node at a time in the order of its evidence, for what
// the check misses: a pack body upload-pack copies as stored and exits 0, damage
// landing after a vouch. It runs after a previous process's copy's first round,
// every [ScrubInterval] of use, and on a report, but not within [ScrubCooldown]
// of one that found nothing; it holds NO LOCK, so the copy keeps serving. A
// verdict holds only for the file hashed, and a repair acts only on a file still
// at its finding's identity, never undoing a rewrite. A failed GET is retried
// from [RebuildCooldown] up to [RebuildBackoffCap].
//
// # Damage is repaired once, in place where nothing is lost
//
// Damage a git process reports by its trace2 formats, never its words, makes the
// host [Cache.Rebuild] the copy, and the repair follows what was lost: a SHAPE or
// a DERIVED FILE alone is put right in place; a PACK OR AN OBJECT is fetched again
// in place by a FORCED round, then the scrub; only when neither finds anything is
// the repository read whole into the NEXT incarnation, seeded with every pack
// found whole. Ten requests failing on one copy cost one repair, a forced check
// runs at most once a [ScrubCooldown] while nothing is written, and a rebuild is
// not repeated where it cannot help: damage it did not end is the store's or the
// rows', so it backs off from [RebuildCooldown] to [RebuildBackoffCap]. Damage in words is a HINT that repairs nothing until
// [Cache.ConfirmHint] hashes the pack, once however many name it. A check
// ([Cache.AdmitVerify]) is admitted before it reads anything and only of a copy
// held ([ErrNotHeld]); damage its own git meets is the check's to report.
//
// # What any git process reports damaged is repaired
//
// git reads around damage to a derived file with a warning, so the pool hands
// every run to the cache's observer (gitproc.Pool.Observe; report.go), and a
// derived file a run names is dropped by a round no request waits for — routed by
// the process's directory, never a file written after the run began, never under
// a check, once a RebuildQuiet. A pack, an object, a hint or a crash makes the
// scrub due and drops nothing, since it names no file it can be trusted about;
// what a request's own run reports is held for that request's repair
// ([Synced.Repairer]), so nothing is repaired twice.
//
// # Background work never takes a person's slot
//
// The maintainer writes the multi-pack-index with its bitmap and the commit-graph
// once packs stand still ([MidxDebounce], [MidxMaxWait]) in the maintenance lane:
// never waiting, a slot kept for a person's verify, a tenth of a core's time a
// repository ([MidxDutyCycle]), holding a pin, never the round. A copy with no
// graph is graphed first (maintain.go, GRAPH FIRST). Each step is charged its
// memory ([BitmapBytesPerObject] and its kin) against the node's budget, read from
// the machine and every cgroup; a step past it never runs, a refused bitmap is
// raised ([Cache.AlarmReading]), and the budget is advertised as it stands
// ([Cache.BackgroundBudget]). A pool that cannot read the memory refuses to start:
// a guessed budget is the OOM it exists to prevent. A copy without a bitmap
// is slower, never wrong.
//
// # The directory
//
//	<gitlet.cache_dir>/
//	  CACHEDIR.TAG                  the Cache Directory Tagging standard's tag
//	  lock                          this process's claim (internal/flock)
//	  memo-cohorts                  the newest cohort of the memo any process may have placed in
//	  repos/<id>.<n>.git/           a repository's INCARNATION: a bare repository
//	    crewlet-state.json          what the directory reflects, renamed in last
//	    objects-dropping/pack/      the packs an expunge's begin left dropping
//	    crewlet/provenance.<n>      the index of pushes' runs, which no git reads
//	    crewlet/memo/<n>/<key>      a memo answer in cohort n, which no git reads
//	  scratch/                      a request's own repositories, and a swap's staging
//	  tmp/                          every file before its rename into place
//	  trash/                        what is being removed, by the janitor
//	  hooks/proc-receive            the hook every push's receive-pack runs
//	  home/                         HOME for every git process — gitproc's
//	  lost+found/                   mkfs's, at a volume's root; never touched
//
// [Prepare] REFUSES a non-empty directory without the tag, since Open empties
// tmp/ and scratch/ (pointed at /var it would empty /var/tmp), and CLAIMS it
// until [Cache.Stop]: two engines on one directory would race on every file.
//
// # Every write is a rename, and a step is synced once
//
// A file is staged durable in tmp/ and renamed over its destination; a step's
// directories are synced ONCE before a later step names them. So a READER NEVER
// SEES A REF BEFORE ITS OBJECTS, and the state, constant-size, goes in LAST.
// CACHEDIR.TAG alone is written in place, first: a crash before it would leave a
// directory Prepare refuses for good.
//
// # Incarnations, and readers that pin them
//
// A reader gets a repository's directory only PINNED ([Synced]). An INCARNATION
// replaced or removed RETIRES: it stays while a reader holds it, since a git
// process whose GIT_DIR names it would otherwise read another directory's files.
//
// # A pack leaves a copy a reader may hold by a swap
//
// git opens a pack lazily mid-run, so no pack leaves a copy a reader may hold in
// place (ADR-0090): the round SWAPS (swap.go) to a next incarnation staged under
// scratch/ from hard links of every live pack, HEAD, config, packed-refs and each
// derived file the events keep, renamed into repos/ whole; the old one retires.
// It costs the pack count, never the bytes (Measured). Every incarnation field is
// classified for the promote (TestEveryIncarnationFieldIsClassifiedForTheSwap).
// A failed swap leaves the old superset serving; after a crash the state's
// LeavingSince has the first round read the leaving packs as retirements.
//
// # The dropping view
//
// Between an expunge's begin and commit, a dropping object is still served but
// never one a push may rely on. So dropping packs live in objects-dropping/pack,
// named only by processes that serve the copy (gitproc.Cmd.Dropping;
// TestOnlyServingReadsNameTheDroppingView), never by one that writes objects,
// which leans on objects/ alone; a kept graph may name a commit only the view
// holds, so those run with GIT_COMMIT_GRAPH_PARANOIA. WHERE A PACK LIES is its
// rows' (packPlace): objects/pack where any live row names it, the view where
// only dropping rows do. No derived file is built over the view, and nothing is
// published while an expunge is open.
//
// THE EPOCH A COPY'S OBJECTS/ HOLDS ([Synced.DropEpoch]) rides each push, and the
// committer refuses another (gitlet.RefusalExpunging), so a push checked before a
// begin is never decided after it. It is the rows' epoch only once every pack,
// those an installed layer holds past their row included, lies where its rows
// place it. The commit takes the view out with the swap and DROPS THE
// COMMIT-GRAPH (X04's one drop), the memo and the tip: over a graph naming a
// purged commit git walks it as though it were there.
//
// # The installed chain
//
// Every Gitlet node INSTALLS the published layers the rows name (layers.go,
// gitlet.CheckLayer; derived, so never audited: ADR-0091), written by the
// maintainer (internal/gitlet/compact). No node writes a multi-pack-index layer
// of its own: a bitmapped layer costs a walk of the whole history, and one without
// a bitmap turns off every bitmap beneath it. Each node writes only its TIP
// commit-graph over the tail (maintain.go, THE TIP), since a graph layer costs
// only its commits. git refuses a chain beside the node's own index, so a copy is
// in CHAIN or OWN mode and a placement drops own mode's files. A full round
// installs the chain before it answers; a delta hands it to the installer behind
// the serving. A PLACEMENT is a swap where a reader may hold the copy, after a
// CANARY read through the bitmap, since a layer over the wrong base passes every
// check of its own files. A bad layer is REFUSED, never an error: the copy serves
// from the prefix below it, and the chain holds its packs until replaced.
//
// # A boot starts from the disk, once
//
// Prepare reads repos/ once and Open acts on it: the highest incarnation is kept,
// stateless if its state is unreadable, everything else trashed. [Layout.Held]
// sums the states, never a stat of every pack. A repository's last use survives
// a restart as its state file's mtime ([EvictInterval]).
//
// # Measured
//
// Medians of make bench (bench_test.go names each arm), the figures the
// constants rest on:
//
//   - A WARM BOOT of 1 000 repositories: 57 ms. One directory sync a step: 30 µs
//     a renamed file, against 274 µs with a sync a rename.
//   - A view's ADVERTISEMENT: 117 ms against the cache's 112 ms at 100 000 refs.
//   - A COLD SYNC of 2 048 packs from the S3 fake: 75.5 s one GET at a time, 5.3 s
//     at FetchConcurrency, 2.9 s at NodeFetches. A delta sync: 7.7 ms.
//   - FULL READS (BenchmarkColdJoin): 500 repositories in 3.9 to 4.3 s at a cap
//     of eight, other reads' p99 105 ms; 3.6 s uncapped: MaxFullRounds.
//   - The index of pushes: a backfill page of about 7 500 pushes fills 4 MiB.
//   - THE SWAP (BenchmarkCompact): 7.0 ms at 50 packs, 119 ms at 2 048.
//   - The round's MULTI-PACK-INDEX saves a clone nothing at 128 packs and 0.55 s
//     at 100 MB at 256, for a 0.13 s write: MidxRoundPacks. A push outside
//     the maintainer's index costs a clone's counting 0.06 to 0.08 ms up to 128:
//     MidxPacks. That index, bitmap and graph take 14.2 s at 1 GB, so
//     MidxMinInterval paces repositories under 3 to 4 GB.
//   - MEMORY, each process's peak anonymous: the figures BitmapBytesPerObject,
//     IndexBytesPerObject, GraphBytesPerObject and FsckBytesPerObject hold.
//   - A Verify: about 9 ms a mebibyte, nearly all connectivity: CheckBudget.
//   - PACK REUSE: multi sent 8.5 to 57% more bytes than single: ServeConfig.
package cache
