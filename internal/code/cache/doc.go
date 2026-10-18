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
// what happened (ADR-0036). TestNothingButSyncWritesACache holds that from
// both sides: the files under internal/code that write the filesystem or run
// a git process that may write are a reviewed list, each with its reason, and
// seeded interleavings of what a node's cache meets — pushes, refused
// pushes, adoptions, syncs killed mid-step, crashes, evictions, rebuilds,
// maintenance, a log recreated backwards — end every time in caches equal to
// ones rebuilt from nothing at the same position.
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
// A log RESTORED from an older copy keeps its identity (ADR-0035), so only
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
// it from under a push. The pushed pack enters the cache only once its row has
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
// neither is fixed by asking again. A pack the store does not have, in a
// round that was not linearizable, is asked about again at linearizable
// before it is called missing: a copy behind the log may name a pack a purge
// has since removed.
//
// A full round that leaves [MidxRoundPacks] or more packs and no
// multi-pack-index writes one over exactly those packs — without a bitmap, a
// merge of the packs' own indexes — before its refs' peel reads them and
// before it answers: one push is one pack and nothing compacts them, and the
// request a cold cache was synced for would otherwise be served by git
// searching every pack's index for every object. Under that count the index
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
// # Kept while there is room
//
// A cache is evicted to its budget (git.cache_max_bytes) and for no other
// reason: over it, the repositories used least recently go first, each a
// directory renamed into trash/ for the janitor; under it, nothing goes,
// however long a repository has been idle. An idle repository costs disk the
// budget already bounds, while evicting it costs a GET a push the next time
// anybody fetches it — nothing compacts its packs — and a cache older than the
// rows' tombstone horizon is brought forward by a whole read that reuses every
// pack it holds. A repository a request holds is never evicted, even over the
// budget: it is served, and goes as soon as its last reader — or its
// maintenance run — lets go. The budget counts everything the cache holds on
// disk once — every incarnation, the downloads in flight, the pushes' scratch
// repositories, but not a file in scratch/ that is an incarnation's own under
// a second name — and a round that takes the cache past it kicks the janitor
// at once, so a burst of cold syncs never overshoots it for a tick. An
// eviction is decided under the cache's lock and its directory moved into
// trash/ after it, so no sync on the node waits on a rename.
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
// removal made its objects missing for the whole download.
//
// A file this process wrote or checked is VOUCHED for, in memory and never
// persisted, by its IDENTITY: device and inode, size and modification time —
// NO CTIME, since an adoption and a seeded rebuild hard-link files and every
// link moves an inode's ctime without changing a byte. A maintenance run
// vouches only for what it changed, so a run over a damaged bitmap git kept
// vouches nothing of it. A VERDICT HOLDS ONLY FOR THE FILE IT HASHED: the path
// is Lstat'ed before the open and both the open file and the path after the
// hash, so a file written in place, renamed over or removed meanwhile gets no
// verdict; and a repair acts only on a file still at the identity its finding
// names, so it never undoes the maintainer's rewrite or an earlier repair.
//
// Hashing is a request's core — the round's callers wait on it — so it holds
// ONE SLOT OF THE READ LANE a piece at a time (gitproc.Pool.HoldRead,
// [CheckPieceBytes]) and none while the round runs git, which waits in the
// same lane; one slot rather than several, so the largest repository's first
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
// maintainer has served from, upload-pack's bitmap pack reuse sends a damaged
// entry to the client byte for byte and exits 0, and the commit-graph spares
// every walk the commit's parse, so no git process on the node names it while
// every clone of it fails at the client (measured from git 2.40.4 to 2.56.0);
// and holding the packs before a copy first answered would hold the largest
// repository's first request for as long as reading twenty gigabytes takes.
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
// again after [RebuildCooldown] doubling to [RebuildBackoffCap], so an object
// store that is down neither wedges the copy's rounds nor is asked once a
// round.
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
// the same way (Cache.run), under the same rules. What the repair is follows
// from what was lost:
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
//     those very files and meets the damage again asks a rebuild
//     ([RebuildCorrupt]) — the damage was the packs'.
//   - A PACK OR AN OBJECT git could not read ([RebuildCorrupt],
//     [RebuildMissing]) loses at most the packs that hold the damage, so
//     those are what is fetched again, IN PLACE FIRST. A failure whose
//     process began before this process last wrote a REPAIR into the copy —
//     a pack installed again over its files, a lost pack installed again, a
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
//     the same incarnation — at most once a [ScrubCooldown] while it repairs
//     nothing: a forced round that found nothing, with no repair written
//     since, is not run again for the failures of the hour after it, which
//     go to the scrub's answer, so a damaged pack body a busy repository's
//     requests keep failing on costs one read of every index file rather than
//     one a request. A repair written into the copy while that runs answers
//     the failure from the copy as it stands too, asked again before the
//     scrub and before a rebuild. A round that found nothing has the copy's
//     SCRUB run at once for its callers — ahead of every routine scrub on the
//     node, which it stops — which finds a damaged pack body no index shows;
//     a scrub begun after the failing process, or within [ScrubCooldown] with
//     no repair written since, answers from its verdict, and what an earlier
//     scrub found that still waits for its repair is answered by that repair.
//     Only when neither finds anything to fetch again is the repository read
//     whole at the log's head into the NEXT incarnation, beside the one its
//     readers hold, which replaces it once converged — seeded with every pack
//     the scrub found whole, so it downloads nothing when the damage is in
//     the rows. A request whose copy such a rebuild replaced while its round
//     waited is answered from the rebuilt copy.
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
// again in place, a sound one is no repair, and a pack no row names is no
// damage of the copy. The same sentence is a row on 2.40 to 2.55, a pack's
// damage, whose pack is recorded as a hint beside it and confirmed the same
// way, so the repair fetches that pack rather than look for it. A pack is hashed ONCE however many
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
// rebuilds the repository — linking
// in every pack it has just found whole and downloading only the rest, so one
// damaged pack costs one GET — and checks the rebuild the same way; a rebuild
// it has seen whole has ended the damage it was for, so that incarnation's
// next failure is rebuilt at once.
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
//     reports may be the client's pack (counted `unrouted`). A chunk-format
//     report is narrowed to the files its process loads
//     (gitproc.Cmd.Loads).
//   - REPAIRED BY A ROUND NO REQUEST WAITS FOR, which the observer queues
//     under the entry's mutex and never its write lock, so it never holds the
//     run that reported. The round is FULL and LINEARIZABLE — a full round's
//     sweep removes every pack its snapshot does not name, and an adoption
//     installs a pack past the incarnation's own position — and it drops the
//     files as a request's repair does. The copy is not taken out of service:
//     it keeps answering at once meanwhile, as git did.
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
// be the client's pack (`unrouted`). Within [ScrubCooldown] of a scrub that
// found nothing, a report makes nothing due (`cooldown`), whatever was written
// since: a request that can provoke a report beside a busy repository is
// bounded by that and nothing else.
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
// bitmaps it asks for (ServeConfig) — and a commit-graph
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
// a bitmap about [BitmapBytesPerObject] an object, some 60 GB for the largest
// repository the defaults admit; the index without one [IndexBytesPerObject];
// the commit-graph [GraphBytesPerObject]; Verify's check
// [FsckBytesPerObject] — so each is charged its own figure, and room beside it
// for one transfer of the same repository (gitproc.TransferBytesPerObject),
// against the node's BUDGET: its memory as the git pool read it (the least of
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
// logged once an incarnation and counted, a refused bitmap and a refused
// graph are each kept on a gauge while they stay refused, and a refused
// bitmap is raised as code_maintenance_refused
// ([Cache.AlarmReading]), at the budget's own threshold: a git node's largest
// repository with a bitmap is bound by its memory before its disk. A check a
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
//	  lock                          this process's claim (internal/filelock)
//	  repos/<id>.<n>.git/           a repository's INCARNATION: a bare repository
//	    crewlet-state.json          what the directory reflects, renamed in last
//	  scratch/                      a request's own repositories: a push's scratch, a view
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
// implementation (internal/filelock) — and the claim passes to Open and is
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
// — replaced by a rebuild, its repository purged or evicted — is RETIRED: it
// stays where it is while a reader holds it, and moves into trash/ with its
// last release. Nothing renames a directory a reader holds — a path is a
// name, and a git process whose GIT_DIR is that name would otherwise open
// another directory's files half way through its run.
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
// reading — each state counts its own incarnation — so a warm node's boot
// costs a small read a repository, never a stat of every pack: at one pack
// a push, a node caching a million pushes holds three million files. The
// janitor removes what is in trash/ in the background, so a boot never waits
// on a large removal.
//
// A repository's LAST USE comes back from a restart too, to within
// [EvictInterval]: the boot reads it from the state file's modification
// time, and the janitor stamps each repository used since its last tick
// there — a metadata write, never a rewrite of the state — so a repository
// that is read all day and pushed to rarely is not the first a budget evicts
// after every restart.
//
// # Measured
//
// On four vCPUs of a 2.1 GHz Xeon shared with other builds, on ext4 (make
// bench, six counts, medians; bench_test.go says what each arm is): a warm
// boot — Prepare, Open and Stop — of 100 repositories takes 10 ms with 100
// packs each and 7 ms with 1 000 each, so what a repository holds is not in
// the figure, and of 1 000 repositories 38 ms, about 30 µs a repository past
// a few milliseconds of fixed cost (the hook's settle among it): about a third
// of a second at 10 000 repositories, config.CodeMaxRepos. The same 1 000
// repositories with no state to count — every incarnation walked, which is
// what every boot paid when Held was a walk of repos/ — take 1.4 s for their
// 300 000 files, about 5 µs a file, so a node caching a million pushes would
// have spent around fifteen seconds warm before it could serve. A step that
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
// gigabyte and over 10 000 packs at three counts, everything else at six.
// Every cold-sync figure is the S3 FAKE's, whose first byte is the arm's rather
// than a bucket's: what a first byte that long costs the node. A
// COLD SYNC of 2 000 pushes of 150 KB, from the S3 fake at 30 ms to every
// request's first byte and 80 MB/s a connection, takes 67 s one GET at a time,
// 4.5 s at FetchConcurrency's sixteen and 2.9 s at NodeFetches' thirty-two —
// 30, 448 and 680 bundles a second, and at 200 pushes, the round's index among
// them, 30, 396 and 529 — while from a store in the process, the node's own
// side alone, the same 2 000 take 2.9 s, 2.3 s and 2.1 s (679, 878 and 948 a
// second): no git runs but the round's index and its peel, so a cold sync is
// bound by its GETs and that side, never by a core. Sixteen is bound by its
// GETs, at 93% of the 484 a second sixteen of the 33 ms each took allow; at
// thirty-two each took 40 ms, the fake's server sharing the node's four cores.
// S3 itself answers in about 100 to 200 ms (AWS's performance guidelines), and
// with the fake answering that late (the firstbyte arms, three operations
// each, 2026-10-15, in a run that gave the 30 ms arm 30, 417 and 623 bundles a
// second) the same 2 000 take 13.4 s and 26.2 s at sixteen and 7.0 s and
// 13.3 s at thirty-two — 150 and 76, and 285 and 150, bundles a second, at
// 104 and 205 ms a GET: bound by the GETs alone, within a few percent of
// fetch over first byte, so the node's own side bounds a sync only at a first
// byte under about 35 ms, and 10 000 pushes' packs take about 67 to 131
// seconds at a repository's sixteen.
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
// The MULTI-PACK-INDEX, over a source repository's composition — every commit
// four files of one directory, so commits are a seventh of the objects
// (BenchmarkMidx says how the history is built). The maintainer's index with
// its bitmap and the commit-graph over 64 packs take 1.6 s at 100 MB (296 000
// objects, 42 000 commits), 21 s at 1 GB (3.0 million, 424 000) and 152 s at
// 5 GB (14.8 million, 2.1 million) — 16, 21 and 30 s a gigabyte — so
// MidxMinInterval rather than MidxDutyCycle paces every repository under about
// 3 GB of packs, and gitproc.PackWorkDeadline gives the gigabyte sixteen
// minutes, about 48 times its run; the index a full round writes, with no
// bitmap, 0.12 s, 1.1 s and 6.1 s. The bitmap's lookup table pays every
// request that opens it: over the gigabyte (three runs, medians) the pack a
// blobless clone's lazy fetch of one blob is served takes 5.0 ms with it and
// 6.2 ms without, and the pack a fetch of one new commit 8.2 and 9.0 ms —
// the difference what each spent reading the entry of every commit with a
// bitmap before it answered. Pushes of 4 KiB landed beside the
// maintainer's index cost counting a clone's objects about 0.06 ms each at
// 100 MB and 0.09 ms at 1 GB (4.5 ms with none and 12.0 with 128; 6.9 and
// 17.9 ms), and a clone's whole pack-objects nothing it shows up to 128 (1.06
// and 1.11 s; 10.7 s either way): the figures MidxPacks is set from. Over the
// 100 MB split into packs (BenchmarkPackLookup), a clone's pack-objects takes
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
// MidxRoundPacks is set from. Across 10 000 packs of the 100 MB the round's
// index costs 0.53 s and saves a clone 122 s (146 against 24), Missing over
// 1 000 commits 520 ms and a fetch 94 ms, while the maintainer's bitmap,
// which opens every pack it covers, clones in 32 s and fetches in 300 ms:
// past a thousand packs what a repository needs is fewer of them. Peeling
// 5 000 tags in as many packs takes 0.65 s without the round's index and
// 0.23 s with it, for a 0.19 s write, and a thousand 57 and 45 ms for a 35 ms
// write: the peel is no reason for the round's index below thousands of
// tagged packs.
//
// The MEMORY each whole-repository git process holds (BenchmarkMidx's memory
// arms: the one process's peak anonymous memory, sampled every millisecond),
// over the 100 MB and the gigabyte: the index without a bitmap 18.5 and
// 182 MiB (66 and 64 bytes an object), with its bitmap 136 MiB and 1 985 MiB
// (484 and 702), the commit-graph 12.7 and 112 MiB (45 and 39.5), the
// connectivity check 110 and 452 MiB (390 and 160), and a clone of main
// 132 MiB and 670 MiB walking the history with no bitmap, 36 and 330 MiB
// counting from the maintainer's — the figures BitmapBytesPerObject,
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
package cache
