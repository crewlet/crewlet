// Package cache is THE NODE'S DISPOSABLE BARE REPOSITORIES: one per repository
// a git node has been asked to serve, from which real git upload-pack answers
// clones and fetches.
//
// # A cache is a function of rows
//
// A repository is ROWS in the code domain (internal/code): its refs, its live
// packs and its settings, at a log position. A cache answers "this node alone"
// as files rather than tables — nothing in it is ever the only copy of
// anything, it is never backed up (its directory carries a CACHEDIR.TAG, so
// backup tools skip it), and deleting the whole directory loses nothing but
// time: what a cache holds is derived from the rows and the object store, and
// is derived again when it is gone. Its refs are exactly the repository's
// code_refs at the position its state names, its packs exactly the
// code_packs there, and [Cache.Sync] writes it by CONVERGENCE — derive the
// target from the rows, make the directory equal to it — never by replaying
// what happened. TestNothingButSyncWritesACache holds that from both sides:
// the files under internal/code that write the filesystem or run a git
// process that may write are a reviewed list, each with its reason, and
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
// the links fail. [Cache.Missing] is a fetch's question beside it: which of
// its wants the cache lacks, which sends the fetch to one linearizable sync.
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
// A full round that leaves [MidxPacks] or more packs and no multi-pack-index
// writes one over exactly those packs — without a bitmap, a merge of the
// packs' own indexes — before its refs' peel reads them and before it
// answers: one push is one pack and nothing compacts them, and the request a
// cold cache was synced for would otherwise be served by git searching every
// pack's index for every object.
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
// # Damage is repaired by a rebuild, once
//
// A git process the host ran in a cache that git found damaged — a pack or an
// index it could not read, an object a row names that no pack holds — makes
// the host [Cache.Rebuild] it: the repository read whole at the log's head
// into the NEXT incarnation, beside the one its readers hold, which replaces
// it once converged. Ten requests failing on one damaged cache cost one
// rebuild, a request that failed on an incarnation already replaced costs
// none, and a sync queued behind a rebuild that fails is answered by an
// ordinary round on the incarnation still serving, never with the rebuild's
// failure. A rebuild is not repeated where it cannot help: damage a rebuild
// did not end is the object store's or the rows', so an incarnation holding
// exactly what such a rebuild read is not rebuilt again for a day
// ([RebuildBackoffCap]) unless something writes into it, and a line of
// attempts that keep failing waits [RebuildCooldown] after the first and
// twice as long after each one after it, up to that day — every refusal
// counted, and logged once a cooldown. A line ends when the asking stops: a
// repository with no rebuild asked of it for [RebuildQuiet] meets its next
// failure as new damage.
//
// [Cache.Verify] is the operator's side of it: every pack the rows name
// hashed against its row as the bundle the push stored, every file outside
// the image named, packed-refs held to the digest written with it, and git's
// connectivity check in a free maintenance slot — refused rather than queued
// when there is none. It stops no maintenance run — one in flight finishes,
// the files it holds while it runs admitted — and none starts on the
// repository while it checks, so the slot its fsck needs is not taken from
// under it. Asked to repair, it rebuilds the repository — linking in every
// pack it has just found whole and downloading only the rest, so one damaged
// pack costs one GET — and checks the rebuild the same way; a rebuild it has
// seen whole has ended the damage it was for, so that incarnation's next
// failure is rebuilt at once.
//
// # Background work never takes a person's slot
//
// The maintainer writes a multi-pack-index with its bitmap and a commit-graph
// once a repository's packs have gathered and stood still for
// [MidxDebounce] — or the oldest has waited [MidxMaxWait], so a repository
// that is never still is indexed too — in the pool's maintenance lane: half
// the node's cores, a free slot or none, never waiting, a repository a slot,
// in every slot but one — kept for a person's verify — and as many at once
// as [MaintenanceMemoryPerCore] holds beside the largest, so one repository's
// hours-long bitmap holds back nobody else's and a node's largest
// repositories falling due together do not take its memory at once; and at
// most a tenth of a core's time a repository ([MidxDutyCycle]). It holds a pin, never the repository's round,
// so no push or fetch waits for it; a full round that would sweep the
// directory under it stops it first, because a multi-pack-index naming a pack
// the sweep removes fails every read of the repository. A cache without the
// bitmap is slower, never wrong.
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
package cache
