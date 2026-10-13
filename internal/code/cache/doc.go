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
// is derived again when it is gone.
//
// # The directory
//
//	<git.cache_dir>/
//	  CACHEDIR.TAG                  the Cache Directory Tagging standard's tag
//	  lock                          this process's claim (internal/filelock)
//	  repos/<id>.<n>.git/           a repository's INCARNATION: a bare repository
//	    crewlet-state.json          what the directory reflects, renamed in last
//	  scratch/                      a request's own repositories (a push's scratch)
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
// directory sync for them, not a thousand. The state file is renamed in
// LAST, after everything it describes, so it is never ahead of the directory.
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
// — replaced by a rebuild, or its repository purged — is RETIRED: it stays
// where it is while a reader holds it, and moves into trash/ with its last
// release. Nothing renames a directory a reader holds — a path is a name, and
// a git process whose GIT_DIR is that name would otherwise open another
// directory's files half way through its run.
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
// rename, which is why durable.go syncs a step's directory once.
package cache
