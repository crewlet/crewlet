package cache

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// SyncKind is what handing out a cache's directory cost: a closed set
// ([SyncKinds], [SyncKind.Valid]), and the host's fetch.duration{sync}
// attribute.
type SyncKind string

const (
	// SyncNone is an answer from what the cache holds, with no estate call.
	SyncNone SyncKind = "none"
	// SyncDelta is the rows that moved since the cache's position.
	SyncDelta SyncKind = "delta"
	// SyncFull is the repository's whole answer.
	SyncFull SyncKind = "full"
)

// SyncKinds is every [SyncKind], in the order of what each costs.
var SyncKinds = []SyncKind{SyncNone, SyncDelta, SyncFull}

// Valid reports whether k is one of [SyncKinds].
func (k SyncKind) Valid() bool { return slices.Contains(SyncKinds, k) }

// Synced is a repository's directory, handed to a reader PINNED: Dir is valid
// until Release, and THERE IS NO OTHER WAY TO A REPOSITORY'S DIRECTORY — no
// path accessor that skips the pin, so no reader can hold a directory a
// rebuild or a purge is about to remove. Every holder defers Release.
type Synced struct {
	// Dir is the incarnation's directory, absolute and clean.
	Dir string
	// Repo is the repository row the cache last read.
	Repo code.RepoRef
	// Position: every row up to here is reflected in Dir.
	Position statelog.Position
	// Kind is what this hand-out cost.
	Kind SyncKind
	// Repairer names this reader to the cache's observer. A git process run
	// in Dir whose failure this reader repairs names it on its Cmd
	// (gitproc.Cmd.Repairer), and what that process reports is HELD for this
	// reader rather than acted on beside its repair (report.go): the repair
	// it asks ([Cache.Rebuild]) consumes it, and Release hands whatever it
	// did not to the ordinary report rules.
	Repairer gitproc.Repairer
	// Dropping is Dir's DROPPING VIEW — the object directory holding the
	// packs an expunge has left dropping (code.PackDropping),
	// Dir/objects-dropping — or "" where Dir holds none: what every process
	// that SERVES Dir names (gitproc.Cmd.Dropping), so an object a reader is
	// still on is answered by id, and what nothing that writes objects over
	// Dir ever names — a push's scratch leans on Dir's objects/ alone
	// (Cache.NewScratch), so no push relies on a dropping object.
	Dropping string
	// DropEpoch is the repository's drop epoch AS DIR'S OBJECTS/ HOLDS IT:
	// the epoch of the newest expunge begin whose split of the repository's
	// objects into live and dropping packs Dir's objects/ reflects — the
	// row's (Repo.DropEpoch) but while a begin's inputs are still in it,
	// leaving by the swap that has not yet been made, when it is the epoch
	// before. What a push checked over a scratch leaning on Dir carries
	// (code.Proposal.DropEpoch): its committer refuses one of another epoch,
	// so a push that relied on an input's object the begin made dropping is
	// never decided.
	DropEpoch int64

	// release unpins Dir, once: what [Synced.Release] calls.
	release func()
	// inc is which incarnation Dir is, and entry the repository it is one
	// of: what Rebuild reads to tell whether that incarnation is still the
	// one readers are handed. owner is the cache that handed it out — the
	// one whose entry and incarnation these are.
	inc   *incarnation
	entry *entry
	owner *Cache
}

// Release unpins Dir. Idempotent — a second call does nothing — and A NO-OP
// ON THE ZERO Synced, which every failed Sync answers: a holder that defers
// the release of a variable a later Sync may overwrite (a fetch asking once
// more at the log's head) releases whatever the variable holds when it
// returns, and a failed second Sync must leave it something safe to release
// rather than a nil function that panics on the request's goroutine.
func (s Synced) Release() {
	if s.release != nil {
		s.release()
	}
}

// handedOut refuses a Synced this cache did not hand out — a zero one, or
// another cache's — by name: every method that takes one acts on its
// incarnation, and only the cache that pinned it may.
func (c *Cache) handedOut(s Synced, what string) error {
	if s.inc == nil || s.entry == nil || s.owner != c {
		return fmt.Errorf("%w: %s names the Synced a sync of this cache handed out", ErrInvalid, what)
	}
	return nil
}

// entry is one repository's place in the cache. Lock order is ALWAYS write,
// then Cache.mu, then entry.mu; nothing holds Cache.mu or entry.mu across a
// git process, an estate call or a file copy, and nothing that holds either
// WAITS for write — an adoption's try of it and a holder's release are taken
// under entry.mu, and neither waits (adopt.go).
type entry struct {
	id string

	// write is what may change what the directory HOLDS, or must see it
	// whole: held WHOLE (writeExclusive) by every round for its whole body
	// and by Verify for its listing, and ONE of it by each adoption for its
	// links. A round writes the directory by steps that assume nothing else
	// does — a full one sweeps what its snapshot does not name and counts
	// what it finds, so a pack linked in beside it could be swept as a stray
	// or counted twice — and Verify's listing must see no step half done;
	// adoptions link distinct files, so they share it. Taken whole under a
	// context, so a Verify waiting behind a round's downloads gives up with
	// its caller; and an adoption only TRIES it: one that finds a round or a
	// listing holding it or waiting for it hands its pack to that holder
	// (handed) rather than make a push wait behind somebody else's
	// downloads — a weighted semaphore's try fails while a whole taker
	// waits, so adoptions never starve a round.
	write *semaphore.Weighted
	// handed is the pushed packs adoptions handed to whoever holds write
	// whole, by git checksum: staged in tmp/ (adopt.go), claimed by a round
	// about to download one, and installed by the holder before it lets go
	// (releaseWrite).
	handed map[string]*staging

	// mu guards everything below but stateMu, and every incarnation's
	// fields.
	mu sync.Mutex
	// current is the incarnation readers are handed; nil when there is no
	// directory yet.
	current *incarnation
	// retiring is every incarnation replaced or purged while a reader still
	// held it, removed as its last pin is released.
	retiring []*incarnation
	// running is the round in flight, and next the one queued behind it:
	// at most one of each (sync.go).
	running, next *round
	// dead: the repository was purged or evicted and the entry left the
	// cache; a caller that finds it so looks the repository up again.
	dead bool
	// maint is the maintenance running on an incarnation of the repository,
	// if any (maintain.go): one at a time a repository.
	maint *maintRun
	// swap is the swap in flight (swap.go), from the moment it stages the
	// next incarnation to its end: the index of pushes of the copy it leaves
	// takes no commit meanwhile (commitIndex), so every run of the record the
	// swap carries is on disk for it to link, and what a git process run in
	// the staged directory reports is that copy's (swapAt).
	swap *swapping
	// merges is every merge's store of the repository's packs standing now
	// (NewMergeScratch), by its directory, with the incarnation its links
	// came from: what a git process run over one reports is about that copy
	// (mergeAt). Each is taken back as its scratch is removed.
	merges map[string]*incarnation
	// verifying counts the Verify calls checking the repository: no
	// maintenance run starts while one does (verify.go), from its listing
	// to its fsck — which a run in the lane's last free slot would refuse.
	// A run already in flight is never stopped for one: its own files are
	// admitted instead (Write.Holds).
	verifying int
	// adopting counts the adoptions holding ONE of write outside a round
	// (Adopt, and a failed whole wait's settle in acquireWrite): no
	// maintenance run starts while one does (runnable), as none starts while
	// a round runs. An adoption ADDS A PACK, and a declaration may drop what
	// a run writes when one is added (dropFor) — which stops a run in flight
	// writing it, so one started between that drop and the pack's links
	// would write a file over the packs before the change and record it as
	// current after it. Milliseconds a push; a pass that finds one owes the
	// maintainer a wake when the last lets go (maintainerOwed).
	adopting int
	// lastAdded is when a round last added packs: the maintainer's debounce.
	lastAdded time.Time
	// maintainerOwed: a full round converged, or a pass found the repository
	// due while an adoption held it, and the maintainer is to be woken once
	// no round runs on it (run) and no adoption holds it (releaseWrite),
	// since a pass starts no run on one either holds (runnable).
	maintainerOwed bool
	// uncoveredSince is when the oldest pack the maintainer's index does not
	// cover arrived — set by the round that adds packs while none wait, and
	// cleared by a run whose index covers every pack — and zero when none
	// waits: the debounce's ceiling (MidxMaxWait).
	uncoveredSince time.Time
	// lastRebuild is when the last rebuild ATTEMPT started; lastAsked when a
	// rebuild of the current incarnation was last ASKED for, whether it was
	// refused, joined or started — the quiet that ends a line of attempts
	// (RebuildQuiet) is measured from it; and refusedLogged is when a
	// refusal was last logged: a repository failing on every request logs a
	// refusal once a RebuildCooldown, never once a request.
	lastRebuild, lastAsked, refusedLogged time.Time
	// wantHistory: this process has answered a read of the repository as
	// its home (code.GitQuery.Home), so the backfill reads its index's
	// history down to the start of the push history the rows keep
	// (Cache.WantHistory, provenance.go). In memory alone, and the
	// repository's rather than an incarnation's: a rebuild's product wants
	// what its predecessor wanted, and the index's Complete is what persists.
	wantHistory bool
	// bf is the repository's place in the backfill's schedule
	// (schedule.go), under the schedule's own mu; bfQueued says it is in a
	// due queue there — what a use reads holding this mu alone, so a use of
	// a copy the backfill has no step of due takes no lock of the
	// schedule's. bfFailures is the backfill's steps of it that failed in a
	// row, and bfRetryAt, on Options.Now's clock, when the last of them lets
	// it take the next: a step that fails waits on its own repository, never
	// the backfill's other repositories (backfillStep).
	bf         slot
	bfQueued   atomic.Bool
	bfFailures int
	bfRetryAt  time.Time
	// chained: the current incarnation serves from an installed published
	// chain (layers.go), as crewlet.code.cache.chained counts it — kept in
	// step by noteChained wherever the current incarnation or its chain
	// changes, so the gauge is a running total rather than a walk.
	chained bool

	// stateMu serialises persisting the state.
	stateMu sync.Mutex
}

// newEntry is repository id's place in the cache, with no incarnation yet.
func newEntry(id string) *entry {
	return &entry{id: id, write: semaphore.NewWeighted(writeExclusive), bf: slot{at: -1}}
}

// incarnation is one directory of a repository: repos/<id>.<n>.git. A
// rebuild builds the next number BESIDE the one readers hold rather than
// renaming a path under them, and so does a swap, taking packs out of a copy
// readers may hold (swap.go) — a path is a NAME, and a git process whose
// GIT_DIR is that name would otherwise open the new directory's files half
// way through its run.
type incarnation struct {
	n   int
	dir string

	// pins counts the readers holding the directory.
	pins int
	// retired: replaced or purged; the directory goes as the last pin is
	// released.
	retired bool
	// discarded: moved into trash/ (or given up on) — its bytes have left the
	// cache's total, and nothing it does moves the total again.
	discarded bool
	// converged: a full round ran on it in this process, so every later
	// round may be a delta — nothing but this process has written the
	// directory since — and a caller it already meets is answered from it
	// at once.
	converged bool
	// reread is why its next full round reads it whole again — a log
	// recreated or gone back under it (Cache.reread), or an in-place repair
	// (markWhole: a git process the pool refused there for its shape, or one
	// that named its derived files damaged) — counted when that round
	// completes; "" for none. The FIRST reason is kept: one whole read
	// answers every reason it was marked for, and is counted once.
	reread RebuildReason
	// marks counts the times it was marked to be read whole IN PLACE
	// (markWhole): a round converges it only when no mark was made since the
	// round began, since a round that swept it before the damage a mark
	// answers did not repair that damage.
	marks uint64
	// drop is the derived files, by declaration, a git process over it named
	// damaged (markWhole for RebuildDerived: the multi-pack-index's, the
	// commit-graph, or both), gathered by every mark, so its next full round
	// DROPS them before deciding its own index (apply). A mark of its own
	// rather than a reading of reread, which keeps only the first reason: a
	// copy already marked to be read whole for its shape, or for a log
	// recreated under it, would otherwise be read whole with the damaged
	// files kept, and the request that asked for the repair meet the same
	// damage on its retry. Taken by the round that drops them.
	drop derivedDrops
	// packChecks are the hashes of the packs hints named (gitproc.Hint),
	// by checksum, each in flight or decided (ConfirmHint, hint.go): a hint
	// about a pack being hashed waits for that hash rather than starting its
	// own, and one about a pack decided on the files it holds now hashes
	// nothing. A verdict is kept with the files' identity it was reached on,
	// so it outlives every write into the directory that leaves that pack's
	// files alone — an adoption of another pack, the maintainer's index — and
	// none that replaces them; a hash that decided nothing is replaced by the
	// next hint's.
	packChecks map[string]*packCheck
	// rebuilds is how many rebuild attempts in a row stand behind this
	// incarnation without having ended the failure that asked for them: none
	// for one no rebuild made, one more than its predecessor's for a
	// rebuild's product, and one more for each rebuild of it that failed —
	// IN A ROW: a rebuild asked for after RebuildQuiet with none asked
	// starts the line again from none.
	// unmoved: what it holds is what the last of those attempts read — it IS
	// that rebuild's product, or that rebuild failed definitively on its
	// rows — and nothing has written into it since: no round moved its packs
	// or its refs, wrote its index or swept a file out of it, and the
	// maintainer wrote no index and no graph into it. Cleared, too, with the
	// line. Both are what Rebuild refuses by (rebuild.go).
	rebuilds int
	unmoved  bool

	// state is the state as last persisted, but for its Derived map; nil when
	// there is none. The map is derived's — moved there from a boot's state
	// (fromState) and dropped from each state persist writes — because what a
	// round reads of the state it last wrote is its position, its stream and
	// whether there is one, never the map, and kept here too every repository
	// would hold its derived record twice.
	state *state
	// at is the in-memory position, at or past state's: a round whose delta
	// was empty moves it without rewriting the state, since nothing between
	// the two changed the directory.
	at statelog.Position
	// stream is the StreamCreated of the answer at came from.
	stream     time.Time
	repo       code.RepoRef
	refsDigest string
	refs       int
	packs      int
	// objects is how many objects its packs hold, as their rows count them:
	// set whole by every full round and raised by each pack a delta installs,
	// so it is exact on every incarnation this process has converged — the
	// only ones the maintainer runs on, whose memory it estimates by it.
	// Never persisted: a boot's first round is a full one.
	objects int64
	// diskBytes is the pack files and packed-refs, set only through setSize;
	// metaBytes the derived files', the sum of derived's Bytes, set only
	// through setDerived. Each keeps the cache's total in step. droppingBytes
	// is, of diskBytes, the dropping view's pack files' (objects-dropping/
	// pack), set only through setDropping, which keeps the node's dropping
	// total (crewlet.code.cache.dropping.bytes) in step.
	diskBytes, metaBytes int64
	droppingBytes        int64
	// dropEpoch is the repository's drop epoch as its objects/ holds it
	// (Synced.DropEpoch): the row's once a round leaves nothing leaving the
	// copy — every pack file it holds one the rows name where they name it —
	// and its installed chain holds no pack for a row that is not live
	// (liveChain), and otherwise what it was, since a begin's inputs, which
	// hold what the begin made dropping, leave objects/ by the swap alone.
	// A delta that finds the rows' epoch past it takes out every pack the
	// chain holds for no live row (applyDelta). In memory only: no reader is
	// handed a copy before its first full round in a process, which sets it.
	dropEpoch int64
	// derived is what it records of each declared derived file on disk, by
	// the declaration's name (state.go's derivedState): a declaration with
	// no file on disk has no entry. maintained is its last maintenance run,
	// the duty cycle's clock. Both are what its state persists; maintain.go
	// reads the derived files through covered, bitmapped and graphed.
	derived    map[DerivedName]derivedState
	maintained maintainedState
	// graphFailed: its graph-first run's commit-graph step failed (not a
	// slot refused, not a run stopped from outside), so its graph is due
	// only by the ordinary rules from now on (maintain.go, GRAPH FIRST),
	// rather than asked again every pass. Cleared by a graph written; never
	// persisted, so a boot asks again at once.
	graphFailed bool
	// vouched is every checkable file of it this process wrote or checked
	// (check.go), with the identity it had then: a pack's .idx and .rev by
	// the pack's checksum, the derived files by their path relative to the
	// incarnation. NEVER PERSISTED: a new process holds every file a
	// previous one left to its trailer once, at the incarnation's first full
	// round. A vouch spares a file the check, never the scrub — and no .pack
	// is vouched, since the check reads none. KEYED BY THE CHECKSUM'S BYTES,
	// one entry a pack holding both its identities, because the map lives
	// in every incarnation the node holds and grows by a pack a push: keyed
	// by each file's path, three strings of sixty bytes a pack, it cost 528
	// bytes a pack (measured, a .pack's vouch among them that nothing read),
	// and it costs 118 to 207 now — 168 at 300 000 packs, the spread the
	// map's own growth (Go 1.27.2, TestAPacksVouchesFitInAQuarterKilobyte):
	// 3 000 packs hold about half a megabyte, and a node holding a million
	// packs about 170 MB, against 530 MB before.
	vouched vouches
	// checkedOnce: a full round of this process has held its files to their
	// trailers — the check's `first` and every later one's `unvouched`.
	checkedOnce bool
	// indexRepair: its multi-pack-index was dropped as damaged, so the
	// maintainer's index is due AT ONCE — no debounce, no interval — in a
	// slot of the maintainer's own (maintain.go). Cleared by the index
	// written. A commit-graph dropped as damaged needs no mark: the
	// maintainer writes a missing graph first, at once, anyway.
	indexRepair bool
	// repair is what reports asked its next full round to do (report.go).
	repair repairMark
	// held is what each of its readers' runs reported that the reader's own
	// repair answers (report.go, HELD FOR THE REQUEST), by the reader's name
	// (Synced.Repairer): a key for every reader pin hands it to, taken by
	// that reader's release — and for every round applying to it, taken as
	// the round settles (roundReader) — so a report reaches a reader that is
	// still there or the ordinary rules and never waits on one already gone.
	held map[gitproc.Repairer][]heldReport
	// THE PROVENANCE INDEX'S IN-MEMORY HALF (provenance.go). mapped is every
	// run of its index this process holds mapped, by number — held by a
	// reader, or kept for the next within the node's bound on mapped runs
	// (runmaps.go), which may unmap one nobody holds — and runHolds the
	// mappings each reader holds (Cache.Provenance), by the reader's name,
	// let go by that reader's release: a run the index no longer names is
	// unmapped as its last holder lets go, and every one once the
	// incarnation is discarded. historyGone: the log under it was recreated
	// or went back (Cache.reread for RebuildStream or RebuildRewound, a delta
	// keyed to another stream), so its next full round drops its index
	// before anything else (EventHistoryGone) — taken by that round.
	// provGen counts the indexes it has had in this process, so a page the
	// backfill read for one is never committed into another; provNext is
	// the number the next run of any index of it takes in this process, so
	// no number names two files while a retired one waits for trash/; and
	// retiredRuns is every run file its index has stopped naming that the
	// state on the disk may still name — each moved into trash/ by the
	// first state written after it was retired, which names it no more
	// (persist).
	mapped      map[uint64]*mappedRun
	runHolds    map[gitproc.Repairer][]*mappedRun
	historyGone bool
	provGen     uint64
	provNext    uint64
	retiredRuns []string
	// THE MEMO'S IN-MEMORY HALF (memo.go), whose record — its cohorts and
	// their bytes — is derived[DerivedMemo]; nothing of it is an entry's.
	// memoMu is the memo's own lock, under which an answer is placed into its
	// tree or moved between its cohorts and a cohort, the tree or an entry is
	// taken out — never held across anything but those renames and the
	// listing, and never with entry.mu, which it is taken before. memoListed:
	// every cohort directory on the disk is in the record — this process made
	// the tree, or the janitor listed it (listMemos); a boot's copy is not,
	// until its first listing. memoVersion moves with every change to the
	// record (setMemo) and memoStated is the version the state last written
	// records, which the janitor writes again at its tick once they differ
	// (Cache.stateMemos). memoGen counts the memo's drops in this process, so
	// a batch begun before one places nothing after it, and memoShut is a memo
	// dropped whose round has not yet made the change it answered visible
	// (dropMemo, reopenMemo), or one carried away (carryMemo).
	memoMu                  sync.Mutex
	memoListed              bool
	memoVersion, memoStated uint64
	memoGen                 uint64
	memoShut                bool
	// written is when this process last changed each declaration's files —
	// a write that moved a file's identity, the round's own index, or a drop
	// — a time.Now() reading compared on the monotonic clock with a report's
	// Began (dropDamaged).
	written map[DerivedName]time.Time
	// damagedAt is when each declaration's files were last dropped for
	// damage, on Options.Now's clock: one dropped again within RebuildQuiet of
	// it is damaged AGAIN (D13 of report.go's doc), and waits for the ordinary
	// rules rather than being written again at once — whatever ordinary
	// writes came between, since a disk that keeps damaging what is written
	// to it damages those too. againLogged is the declarations logged so.
	damagedAt   map[DerivedName]time.Time
	againLogged derivedSet
	// memoryRefused is the steps the node's memory has refused it — each
	// one whose charge alone is more than the node can give its background
	// work — and said so: each logged and counted once an incarnation,
	// however many passes, rounds and checks find it again.
	memoryRefused maintStep
	// inherited: a previous process left it — Open indexed it from the disk
	// — so its bytes are none this process held to their rows as it placed
	// them, and its first full round in this process makes its first scrub
	// due (scrub.go).
	inherited bool
	// scrubbing: its scrub is running now, asked at scrubRunning: a report
	// meanwhile is answered by one evidence asked for (a report's, a
	// request's), which hashes every file it holds, and ends a routine one
	// to run as a report's in its place (scrub.go). scrubDue is why its next
	// scrub is due AT ONCE — zero when only its interval makes it due — and
	// scrubAsked when it was made so: the scrubber takes the due-at-once by
	// evidence, and within each the oldest first.
	scrubbing    bool
	scrubRunning scrubPrio
	scrubDue     scrubWhy
	scrubAsked   time.Time
	// scrubBase is when every byte it holds was last held to its checksum —
	// its last scrub's end, or, for one this process built, its first full
	// round's, every file of which this process placed after holding it to
	// its row — and zero until then: ScrubInterval runs from it. scrubbedAt
	// is when its last scrub ended and scrubFound whether that scrub found
	// anything: ScrubCooldown runs from them. All on Options.Now's clock.
	scrubBase, scrubbedAt time.Time
	scrubFound            bool
	// scrubBegan and scrubEnded are when its last scrub began and ended, as
	// time.Now() readings compared on the monotonic clock with repairWrite
	// and the Began of a failing process — a request's or a round's own;
	// scrubWhole is the packs that scrub found whole, all three files, by
	// checksum (and, while its findings wait for their repair, the packs a
	// rebuild past them links, less the ones they name). A repair's
	// escalation (Cache.escalate) takes them for a scrub of its own when
	// that scrub began after the process failed, or found nothing within
	// ScrubCooldown with no repair written since — the same bytes, which
	// would be found whole again.
	scrubBegan, scrubEnded time.Time
	scrubWhole             []string
	// repairWrite is when this process last wrote into it what can END a
	// pack's or an object's failure — a pack installed again over its files,
	// a pack it had lost installed again, a derived file dropped (a
	// multi-pack-index or a graph that placed an object wrongly is read
	// around once it is gone) — as a time.Now() reading, compared on the
	// monotonic clock with the Began of the git process whose pack or object
	// damage is being repaired, a request's or a round's own (the guard of
	// that repair, rebuild.go and repairDamaged). NOTHING ELSE: a push's
	// pack, an adoption and the maintainer's index or graph add what the
	// failing process never needed, and leave whatever failed it as it was —
	// taken for repairs, a repository taking pushes answered every request's
	// damage "as it stands" and never repaired it.
	repairWrite time.Time
	// forcedAt and forcedEnded are when its last FORCED check — the in-place
	// repair of a request's pack or object damage or of a round's own, every
	// index file held to its trailer — ended, on Options.Now's clock and as a
	// time.Now() reading: recorded as that check's round ends, whatever the
	// round met after it (its install failing on a pack it adds, its refs'
	// peel failing on the damage again), since what the check found waits on
	// the repair mark either way (recordCheck). A repair asked within
	// ScrubCooldown of it that names no pack it can fetch, with no repair
	// written since it ended — the round's own repairs included, so a round
	// that repaired something bounds nothing — is not given another
	// (rebuild.go, repairDamaged): the same files would hash the same, and
	// each forced round reads the rows whole and holds every reader of the
	// repository while it hashes a quarter of the copy's bytes.
	forcedAt, forcedEnded time.Time
	// lastUsed is when a reader last took or released it, or made a push's
	// scratch over it: set as the incarnation is made or read at boot, and
	// moved only by used, which wakes the scrubber at the first use since
	// scrubBase.
	lastUsed time.Time
	// stamped is the last use the disk records (lastUsedOn): read at boot,
	// moved forward by the janitor's recordUse.
	stamped time.Time

	// THE LIVE SET (swap.go). live is the incarnation's PACK ROWS, by pack
	// id, as the rows it last converged to hold them — a row left dropping by
	// an expunge's begin as much as a live one, each with its state: set
	// whole by every full round, a delta's added rows set and its retired
	// rows deleted, an adopted pack set — by pack id, so a pack adopted and
	// then served again by the next delta is one entry, never two. byChecksum
	// counts the entries naming each git checksum (two rows of one checksum
	// share one file), and of them the dropping ones, changed only through
	// setLive and deleteLive, so a pack's files leave only once NO row names
	// them — and leave objects/pack for the dropping view once only dropping
	// rows do (placeOf). In memory only: a boot's first round is full, so
	// both are built before a delta reads them. What a swap keeps and drops,
	// and where it keeps each, is read from them, never from the directory.
	live       map[string]livePack
	byChecksum map[string]checksumRows
	// droppingPacks is how many packs the live set places in the dropping
	// view (placeOf): kept in step by placeMoved, so a reader is handed the
	// view (droppingView) exactly while the copy holds a pack in it.
	droppingPacks int
	// leaving is the pack files the incarnation holds that it is to hold no
	// longer where they are, by where they are (their place) and the checksum
	// they are named by — or, for a file of a pack directory named like a
	// pack's whose name carries no checksum, by its name — each with the
	// event that took it out: EventPackRetired, or EventPackRemoved for a
	// stray no retirement accounts for. A pack whose files MOVE — out of
	// objects/pack into the dropping view, or back — leaves the place it was
	// in. Non-empty only between a round's apply and its swap, and after a
	// swap that failed, which the repository's next round tries again.
	leaving map[leaveKey]Event
	// leavingSince is the position the retirements of what is leaving are
	// all above, zero while nothing is (state.LeavingSince): persisted, and
	// what a full round asks its retirements from where it is below at, so a
	// round after a crash between a round and its swap reads those files as
	// the retirements they are.
	leavingSince statelog.Position
	// successor is the incarnation a swap replaced this one by (swap.go),
	// nil for one no swap replaced: what a verdict reached over this one's
	// files after the swap — a scrub's, a forced check's — is recorded on
	// the one serving them now, which holds the same inodes (serving).
	successor *incarnation

	// THE PUBLISHED CHAIN (layers.go). chain is the ROWS' published chain,
	// root first, as of the last snapshot that served it
	// (code.RepoSnapshot.LayersServed) — every full answer, and a delta
	// whose chain moved — and chainRead whether one has: in memory only,
	// since every process's first round is full and serves it. installed is
	// the layers the incarnation HOLDS, root first, each with the row it was
	// installed from: a prefix of chain after a full round, and between a
	// delta that moved the chain and the installer's placement a chain the
	// rows held before — known by its packs until it is cut. installedPacks
	// is every pack checksum an installed layer names, with the figures its
	// row counted as it was installed: the packs the chain HOLDS, which no
	// round takes out while a layer names them, live rows or none. chainGen
	// counts every change to chain or installed, so a placement planned
	// over one finds whether it moved. refused is the layers of chain this
	// node will not try again while the rows name each at the record that
	// added it — in memory, so a restart tries once more.
	chain          []code.LayerRow
	chainRead      bool
	chainGen       uint64
	installed      []code.LayerRow
	installedPacks map[string]livePack
	refused        map[string]layerRefusal

	// THE NODE'S TIP (maintain.go): its own commit-graph layer over the
	// commits no installed layer holds, the last line of commit-graph-chain.
	// tip is that layer — none in own mode, and none until the maintainer
	// writes one over a tail that holds a commit. tipOver is the digest of
	// the tail the last write that succeeded covered — writing the tip, or
	// finding the tail held no commit the layers lack — and tipWritten
	// whether one has since the tip last went: the tail moving off tipOver
	// is what makes the tip due again. Both are persisted with the tip
	// (derivedState.TipOver), so neither a restart nor a full round that
	// reads the same tail writes a quiet copy's tip again. tipFailed: a tip
	// the copy had none of failed to be written (not a slot refused, not a
	// run stopped from outside), so it is due by the ordinary rules from then
	// on rather than at every pass, as graphFailed is own mode's; never
	// persisted.
	tip        tipLayer
	tipOver    tailDigest
	tipWritten bool
	tipFailed  bool
	// tail is the TAIL as its rows count it — the live packs no installed
	// layer names, with their digest — kept in step wherever the live set or
	// the installed chain changes (setLive, deleteLive, liveWhole,
	// recordInstalled), so the maintainer reads it in one load rather than a
	// walk of every pack under the cache's lock.
	tail tailCount
	// chainSeq counts the beginnings and ends of the writes of its chain files
	// IN PLACE — the tip's run, a cut — and chainWriting is how many are in
	// flight: the scrub and Verify hold a chain file to what its record
	// names, which such a write moves apart from the file between the two
	// (git renames the tip's chain file in before its run's end records it,
	// and a cut writes the chain files before its record), so a chain file's
	// verdict stands only where none was in flight as its expectation was
	// read and the count has not moved by the end of its hash (chainStill).
	chainSeq     uint64
	chainWriting int
}

// tipLayer is the node's tip commit-graph layer as an incarnation holds it:
// its id — the graph file's own trailing checksum, its name and the last line
// of commit-graph-chain — and its file's bytes; the zero value is none.
type tipLayer struct {
	id    string
	bytes int64
}

// tailCount is the tail of an incarnation as its rows count it: the live packs
// no installed layer names, their objects and their bundles' bytes — what
// the node's tip covers, its charge and its deadline are sized by
// (maintain.go) — and their digest, what says whether the tip covers them.
type tailCount struct {
	packs          int
	objects, bytes int64
	digest         tailDigest
}

// add counts p into the tail, or out of it.
func (t *tailCount) add(p livePack, sign int64) {
	t.packs += int(sign)
	t.objects += sign * p.objects
	t.bytes += sign * p.size
	t.digest.flip(p.checksum)
}

// tailDigest is a TAIL's identity: which packs it holds, as a set, whatever
// order they arrived in and however it was reached — the XOR of the first
// sixteen bytes of every pack's git checksum, which a pack added or taken out
// moves in one step (tailCount.add) and which reads the same in every process
// for the same packs, so it is what the node's tip persists of the tail it
// covers. A git checksum is a SHA-1 of the pack's own bytes, as uniform as any
// hash of them, so two tails of different packs share a digest with a chance
// of one in 2^128 a comparison; and what that would cost is a tip not written
// again over a tail it does not cover — that tail's commits walked by date
// until it next moves, speed and never an answer. Not the tail's generation:
// a count of its changes moves at every full round's live set and every
// restart, and a quiet copy's tip would be written again at each.
type tailDigest [16]byte

// flip moves d by the pack checksum names, in or out: XOR is its own inverse,
// and a tail counts each pack's files once (setLive), so a pack is flipped in
// at most once while the tail holds it.
func (d *tailDigest) flip(checksum string) {
	var p tailDigest
	// A row's checksum is forty hex digits (code's bundle check, the one way a
	// pack enters a live set), so its first thirty-two decode whole.
	_, _ = hex.Decode(p[:], []byte(checksum[:min(len(checksum), 2*len(p))]))
	for i := range d {
		d[i] ^= p[i]
	}
}

// String is d as the state records it: thirty-two lowercase hex digits.
func (d tailDigest) String() string { return hex.EncodeToString(d[:]) }

// parseTailDigest reads a digest as String writes one, or refuses it.
func parseTailDigest(s string) (tailDigest, error) {
	var d tailDigest
	if len(s) != hex.EncodedLen(len(d)) || strings.ToLower(s) != s {
		return d, fmt.Errorf("%q is no tail's digest: %d lowercase hex digits", s, hex.EncodedLen(len(d)))
	}
	if _, err := hex.Decode(d[:], []byte(s)); err != nil {
		return d, fmt.Errorf("%q is no tail's digest: %w", s, err)
	}
	return d, nil
}

// covers is the tail as the tip's write covers it (derived.go's Covers).
func (t tailCount) covers() Covers {
	return Covers{Packs: t.packs, Objects: t.objects, DiskBytes: t.bytes}
}

// livePack is one row of the live set as the swap and a compaction's plan
// read it: the checksum its files are named by, the bundle's bytes and its two
// indexes' — what tells its pack section, and whether the pack is FULL
// (code.PackRow.Full), which a plan never merges ([Cache.Packs]) — the objects
// its row counts, and whether an expunge's begin left the row dropping. A pack
// id names one bundle for ever — a row is never rewritten — so two rows of one
// checksum carry the same figures. The row's object key and hash are not kept:
// nothing that reads the set fetches by them, and the set lives in every
// incarnation the node holds.
type livePack struct {
	checksum                                string
	size, indexBytes, reverseBytes, objects int64
	// dropping: the row is one an expunge's begin left dropping
	// (code.PackDropping), whose files the incarnation keeps in its dropping
	// view while no live row names the same checksum (placeOf).
	dropping bool
}

// liveOf is p, a row in state, as the live set keeps it: dropping exactly
// where state is code.PackDropping — the one state of a row the cache keeps
// out of objects/pack (read.go's checkPack refuses a state this build does
// not know).
func liveOf(p code.PackRef, state code.PackState) livePack {
	return livePack{checksum: p.GitChecksum, size: p.Size, indexBytes: p.IndexBytes, reverseBytes: p.ReverseBytes,
		objects: p.Objects, dropping: state == code.PackDropping}
}

// row is p, named id, as a plan reads it ([Cache.Packs]): every figure the
// live set keeps, and nothing a snapshot's row carries beside them — its
// object, its part, its kind, its state and its position stay zero.
func (p livePack) row(id string) code.PackRow {
	return code.PackRow{PackID: id, Size: p.size, IndexBytes: p.indexBytes, ReverseBytes: p.reverseBytes,
		GitChecksum: p.checksum, Objects: p.objects}
}

// checksumRows is the live set's rows naming one git checksum: how many, and
// how many of them are dropping — what says where the incarnation keeps the
// files they name (placeOfRows).
type checksumRows struct{ rows, dropping int }

// packPlace is WHERE AN INCARNATION KEEPS A PACK'S FILES (M6-D3): in
// objects/pack, which every git process run in it or leaning on it reads, or
// in its DROPPING VIEW, objects-dropping/pack, which only a process serving it
// reads (Synced.Dropping, gitproc.Cmd.Dropping) — so an object an expunge has
// left dropping is answered by id to a reader still on it, and never relied on
// by a push, whose scratch leans on objects/ alone.
type packPlace uint8

const (
	// placeNone: no row of the incarnation names the pack.
	placeNone packPlace = iota
	// placeLive: objects/pack — a live row names the pack, whatever dropping
	// rows name it too, since a live row is one a push may rely on.
	placeLive
	// placeDropping: the dropping view — every row naming the pack is one an
	// expunge's begin left dropping.
	placeDropping
)

// rel is the directory p's files live in, relative to an incarnation: the
// dropping view's for placeDropping, objects/pack otherwise.
func (p packPlace) rel() string {
	if p == placeDropping {
		return droppingRel
	}
	return packRel
}

// placeOfRows is where rows place the files they name.
func placeOfRows(r checksumRows) packPlace {
	switch {
	case r.rows == 0:
		return placeNone
	case r.dropping == r.rows:
		return placeDropping
	}
	return placeLive
}

// placeOf is where inc's live set places the files of the pack checksum cs
// (placeOfRows): by its rows alone — a pack an installed layer names is in
// objects/pack besides, whatever its rows say (layers.go, the chain holds its
// packs), which a caller asks of installedPacks. e.mu is held.
func placeOf(inc *incarnation, cs string) packPlace { return placeOfRows(inc.byChecksum[cs]) }

// leaveKey is one pack's files an incarnation holds that are leaving it: where
// they are, and the checksum they are named by — or, for a file of a pack
// directory named like a pack's whose name carries no checksum, its name.
type leaveKey struct {
	place packPlace
	name  string
}

// setLive records the row id names, whose files are p's, as a row of inc's
// live set; e.mu is held. A row already there is left as it is — a pack
// adopted and then served by the next delta is one row — and a pack a row
// names again where it lies is no longer leaving from there.
func setLive(inc *incarnation, id string, p livePack) {
	if _, ok := inc.live[id]; ok {
		return
	}
	if inc.live == nil {
		inc.live, inc.byChecksum = map[string]livePack{}, map[string]checksumRows{}
	}
	was := placeOf(inc, p.checksum)
	inc.live[id] = p
	r := inc.byChecksum[p.checksum]
	r.rows++
	if p.dropping {
		r.dropping++
	}
	inc.byChecksum[p.checksum] = r
	now := placeOf(inc, p.checksum)
	placeMoved(inc, p, was, now)
	delete(inc.leaving, leaveKey{now, p.checksum})
}

// placeMoved counts p — the pack whose files the live set placed at was and
// places at now — into or out of inc's tail and its dropping count: THE TAIL
// counts a pack's files once, whichever rows name them, and only those in
// objects/pack — a dropping pack holds no commit the node's tip covers,
// written as it is without the dropping view; and droppingPacks counts the
// packs placed in the view (droppingView). e.mu is held.
func placeMoved(inc *incarnation, p livePack, was, now packPlace) {
	switch {
	case was != placeLive && now == placeLive:
		tailMoved(inc, p, 1)
	case was == placeLive && now != placeLive:
		tailMoved(inc, p, -1)
	}
	switch {
	case was != placeDropping && now == placeDropping:
		inc.droppingPacks++
	case was == placeDropping && now != placeDropping:
		inc.droppingPacks--
	}
}

// tailMoved counts the pack p a live set gained (sign 1) or lost (-1) in
// objects/pack into inc's tail when no installed layer names it — one an
// installed layer names is the chain's, never the tail's. e.mu is held.
func tailMoved(inc *incarnation, p livePack, sign int64) {
	if _, chained := inc.installedPacks[p.checksum]; chained {
		return
	}
	inc.tail.add(p, sign)
}

// recountTail counts inc's tail afresh — every pack's files the live set
// places in objects/pack once, those no installed layer names — for an
// installed chain set whole (recordInstalled) or a live set carried into a
// swap's next incarnation (promote). e.mu is held.
func recountTail(inc *incarnation) {
	var t tailCount
	counted := make(map[string]bool, len(inc.byChecksum))
	for _, p := range inc.live {
		if counted[p.checksum] {
			continue
		}
		counted[p.checksum] = true
		if _, chained := inc.installedPacks[p.checksum]; !chained && placeOf(inc, p.checksum) == placeLive {
			t.add(p, 1)
		}
	}
	inc.tail = t
}

// deleteLive takes the row id names out of inc's live set, and answers the
// checksum of the files it named — "" for a row the set did not hold: a
// retirement of a pack the incarnation never held names nothing to drop —
// and where the set placed those files before and places them now (placeOf):
// a pack whose files no row names any more is placed nowhere, and one only
// dropping rows still name moves to the dropping view. e.mu is held.
func deleteLive(inc *incarnation, id string) (cs string, was, now packPlace) {
	p, ok := inc.live[id]
	if !ok {
		return "", placeNone, placeNone
	}
	was = placeOf(inc, p.checksum)
	delete(inc.live, id)
	r := inc.byChecksum[p.checksum]
	r.rows--
	if p.dropping {
		r.dropping--
	}
	if r.rows == 0 {
		delete(inc.byChecksum, p.checksum)
	} else {
		inc.byChecksum[p.checksum] = r
	}
	now = placeOf(inc, p.checksum)
	placeMoved(inc, p, was, now)
	return p.checksum, was, now
}

// liveWhole makes inc's live set exactly the rows packs: what a full round's
// snapshot names. e.mu is held.
func liveWhole(inc *incarnation, packs map[packKey]code.PackRow) {
	inc.live = make(map[string]livePack, len(packs))
	inc.byChecksum = make(map[string]checksumRows, len(packs))
	inc.tail, inc.droppingPacks = tailCount{}, 0
	for _, p := range packs {
		setLive(inc, p.PackID, liveOf(p.Ref(), p.State))
	}
}

// other is the place a pack's files are at that is not p: the dropping view
// for objects/pack and objects/pack for the view; none for none.
func (p packPlace) other() packPlace {
	switch p {
	case placeLive:
		return placeDropping
	case placeDropping:
		return placeLive
	}
	return placeNone
}

// places is where the rows s names place each pack's files, by checksum
// (placeOfRows) — every pack the repository holds, on a full snapshot.
func (s snapshot) places() map[string]packPlace {
	rows := make(map[string]checksumRows, len(s.packs))
	for _, p := range s.packs {
		r := rows[p.GitChecksum]
		r.rows++
		if p.State == code.PackDropping {
			r.dropping++
		}
		rows[p.GitChecksum] = r
	}
	out := make(map[string]packPlace, len(rows))
	for cs, r := range rows {
		out[cs] = placeOfRows(r)
	}
	return out
}

// placesAfter is where inc's live set places the files of each pack snap
// names once a round has applied snap to it (apply's live set): a full
// snapshot's own places, and a delta's over inc's live set as it stands — its
// rows added, a row the set holds already left as it is (setLive), and its
// retired rows taken out — for every checksum the delta names. e.mu is held.
func placesAfter(inc *incarnation, snap snapshot) map[string]packPlace {
	if snap.full {
		return snap.places()
	}
	rows := map[string]checksumRows{}
	touch := func(cs string) checksumRows {
		r, ok := rows[cs]
		if !ok {
			r = inc.byChecksum[cs]
		}
		return r
	}
	for _, p := range snap.packs {
		r := touch(p.GitChecksum)
		if _, held := inc.live[p.PackID]; !held {
			r.rows++
			if p.State == code.PackDropping {
				r.dropping++
			}
		}
		rows[p.GitChecksum] = r
	}
	for _, x := range snap.retired {
		p, held := inc.live[x.PackID]
		if x.Part != "pack" || !held {
			continue
		}
		r := touch(p.checksum)
		r.rows--
		if p.dropping {
			r.dropping--
		}
		rows[p.checksum] = r
	}
	out := make(map[string]packPlace, len(rows))
	for cs, r := range rows {
		out[cs] = placeOfRows(r)
	}
	return out
}

// heldView is the dropping view the incarnation — or a swap's staged next
// incarnation — at dir holds on the disk: its objects-dropping directory where
// there is one, "" where there is none. What the cache's own reads of what a
// copy's refs reach name (the round's peel, a placement's canary, Verify's
// connectivity check): a round places a dropping pack before the live set it
// ends with says so, and a ref a push revived onto a dropping object in an
// expunge's window reaches one, which read without the view is an object the
// copy holds named missing. Something else at the name is the pool's to
// refuse (gitproc.ShapeDropping), whatever is named.
func heldView(dir string) string {
	view := filepath.Join(dir, gitproc.DroppingDir)
	if info, err := os.Lstat(view); err == nil && info.IsDir() {
		return view
	}
	return ""
}

// liveChain reports whether every pack the layers of chain name is one inc's
// live set places in objects/pack (placeOf): what the copy's objects/ must
// hold besides its live packs before its drop epoch is the rows'
// (Synced.DropEpoch). A pack a layer names for no live row — the chain holds
// its packs (layers.go) between a publication that retired them and the
// installer's placement — holds what a publication retired, which an expunge
// begun since may have made dropping, and a push relying on that is what the
// epoch refuses. e.mu is held.
func liveChain(inc *incarnation, chain []code.LayerRow) bool {
	for _, r := range chain {
		for _, cs := range r.Layer.Packs {
			if placeOf(inc, cs) != placeLive {
				return false
			}
		}
	}
	return true
}

// droppingView is inc's dropping view as a process serving it names it
// (Synced.Dropping, gitproc.Cmd.Dropping) — its objects-dropping directory —
// where its live set places a pack there, and "" where it places none. e.mu
// is held.
func droppingView(inc *incarnation) string {
	if inc.droppingPacks == 0 {
		return ""
	}
	return filepath.Join(inc.dir, gitproc.DroppingDir)
}

// serving is the incarnation that serves inc's files now: inc, or the last of
// the swaps that replaced it, which holds the same inode for every file it
// kept. e.mu is held.
func serving(inc *incarnation) *incarnation {
	for inc.successor != nil {
		inc = inc.successor
	}
	return inc
}

// repaired stamps that this process has just written into inc what can end a
// pack's or an object's failure (incarnation.repairWrite), as a time.Now()
// reading; e.mu is held.
func repaired(inc *incarnation) { inc.repairWrite = time.Now() }

// fromState sets inc's in-memory fields from a state read at boot, taking
// its derived map rather than a copy (incarnation.state).
func (inc *incarnation) fromState(st *state) {
	inc.state = st
	inc.at = st.Position
	inc.stream = st.StreamCreated
	inc.repo = st.Repo
	inc.refsDigest = st.RefsDigest
	inc.refs = st.Refs
	inc.packs = st.Packs
	inc.diskBytes = st.DiskBytes
	inc.droppingBytes = st.DroppingBytes
	inc.metaBytes = st.derivedBytes()
	inc.derived, st.Derived = st.Derived, nil
	inc.maintained = st.Maintained
	inc.leavingSince = st.LeavingSince
}

// stateValue is the state inc's in-memory fields describe; the entry's mu is
// held. Its map is a copy: the state is encoded once the lock is let go,
// while a measure or a write may change the incarnation's own.
func (inc *incarnation) stateValue(id string) state {
	return state{
		Format:        stateFormat,
		RepoID:        id,
		StreamCreated: inc.stream,
		Position:      inc.at,
		Repo:          inc.repo,
		RefsDigest:    inc.refsDigest,
		Refs:          inc.refs,
		Packs:         inc.packs,
		DiskBytes:     inc.diskBytes,
		DroppingBytes: inc.droppingBytes,
		Derived:       maps.Clone(inc.derived),
		Maintained:    inc.maintained,
		LeavingSince:  inc.leavingSince,
		memoVersion:   inc.memoVersion,
		lastUsed:      inc.lastUsed,
	}
}

// covered is how many packs inc's multi-pack-index covers: none where there
// is none, or where no write this cache recorded made the one on disk
// (derivedState.covers). e.mu is held.
func (inc *incarnation) covered() int { return inc.derived[DerivedMidx].covers() }

// bitmapped reports that inc's multi-pack-index is the maintainer's with its
// bitmap, rather than one without — a full round's, or the maintainer's
// where the node's memory cannot hold the bitmap. e.mu is held.
func (inc *incarnation) bitmapped() bool { return inc.derived[DerivedMidx].Step == StepBitmap }

// graphed reports that the maintainer's commit-graph is on disk. Recorded
// apart from the index, because the two are written by two processes and the
// second can be refused a slot the first had: a repository whose index is
// current and whose graph is missing is due for the graph alone, where read
// only from the index it would never be due again until MidxPacks more packs
// had landed. e.mu is held.
func (inc *incarnation) graphed() bool { return inc.derived[DerivedGraph].Step == StepGraph }

// used records a reader's use of inc — taking it (pin), letting it go
// (release), or making a push's scratch over it (newScratch) — on the cache's
// clock; the entry's mu is held. EVERY USE GOES THROUGH HERE, because the
// FIRST use since its bytes were last held to their checksums wakes the
// scrubber, wherever in the interval it falls and whichever kind of use it
// is: only a use makes a copy's interval count (a copy nobody uses is never
// due), so the scrubber reads it and arms its wake for the interval's end
// (scrub.go), and it has no other reason to wake for the copy. Woken only
// once the interval had passed, a copy used within it — every busy
// repository, right after its first scrub — was never woken for again; woken
// only by a reader taking it, a copy whose first use after a scrub was the
// RELEASE of a reader held across the scrub's end — a clone of a large
// repository, one long request at a time — was not either, since every
// reader taken after that release found the copy used already. Either way its
// next scrub waited for something unrelated to wake the scrubber. And a use
// of e's current copy while the backfill has a step of it due moves the copy
// in its queue (touch): the backfill takes the most recently used first.
func (c *Cache) used(e *entry, inc *incarnation) {
	if !inc.scrubBase.IsZero() && !inc.lastUsed.After(inc.scrubBase) {
		c.kickScrubber()
	}
	inc.lastUsed = c.now()
	if e.current == inc && e.bfQueued.Load() {
		c.touch(e, inc.lastUsed)
	}
}

// noReader is the name of a pin that is no reader's — a push's scratch or an
// advertisement view holding the incarnation it leans on (scratch.go): no
// report is held under it, since a process there is never one whose
// incarnation's damage its caller repairs (report.go).
const noReader gitproc.Repairer = 0

// reader mints a name for a reader of inc and opens its key in inc.held, so
// what a run naming it reports is held for it (report.go): a pin's (pin), or a
// round's own git runs' (roundReader), which hold no pin — the round holds
// the entry's write lock instead. The entry's mu is held.
func (c *Cache) reader(inc *incarnation) gitproc.Repairer {
	by := gitproc.Repairer(c.repairers.Add(1))
	if inc.held == nil {
		inc.held = map[gitproc.Repairer][]heldReport{}
	}
	inc.held[by] = nil
	return by
}

// pin hands inc to a reader — a use (used) — under a name of its own
// (Synced.Repairer), live on inc until the reader lets it go: what the
// observer holds the reader's reports under (report.go). The entry's mu is
// held.
func (c *Cache) pin(e *entry, inc *incarnation, kind SyncKind) Synced {
	inc.pins++
	c.used(e, inc)
	by := c.reader(inc)
	return Synced{
		Dir:       inc.dir,
		Repo:      inc.repo,
		Position:  inc.at,
		Kind:      kind,
		Repairer:  by,
		Dropping:  droppingView(inc),
		DropEpoch: inc.dropEpoch,
		release:   sync.OnceFunc(func() { c.release(e, inc, by) }),
		inc:       inc,
		entry:     e,
		owner:     c,
	}
}

// release is reader by letting inc go — a use (used) — handing every report
// held for it to the ordinary report rules (handOn). The last release of a
// RETIRED incarnation moves its directory into trash/ and kicks the janitor;
// the removal itself is the janitor's, so a request that lets go of a 20 GiB
// directory returns at once. The last release of a current incarnation while
// an eviction is due (overBudget) kicks the janitor too: a repository too large
// to keep is served while a request holds it, and evicted as soon as none
// does rather than at the next tick. Safe after Stop.
func (c *Cache) release(e *entry, inc *incarnation, by gitproc.Repairer) {
	e.mu.Lock()
	c.used(e, inc)
	c.handOn(e, inc, by)
	c.releaseRuns(inc, by)
	gone := c.unpin(e, inc)
	e.mu.Unlock()
	c.trash(gone)
}

// unpin drops one pin on inc; the entry's mu is held. With the last one, a
// retired incarnation is discarded, and unpin answers the directory its
// caller moves into trash/ once it has let go of the entry (Cache.trash);
// and a current one while an eviction is due (overBudget) kicks the janitor,
// since it was not evictable while held. What a reader's release and the
// maintainer's share — a maintenance run's pin keeps a repository from the
// budget exactly as a reader's does, so its end must not leave the eviction
// to the tick — but the maintainer's pin is no USE, so it leaves lastUsed as
// it was and the budget's order is the readers' alone.
func (c *Cache) unpin(e *entry, inc *incarnation) string {
	inc.pins--
	switch {
	case inc.pins > 0:
	case inc.retired:
		e.retiring = slices.DeleteFunc(e.retiring, func(r *incarnation) bool { return r == inc })
		return c.discard(inc)
	case c.overBudget() > 0:
		c.kick()
	}
	return ""
}

// retire takes inc out of service — a rebuild or a swap replaced it, or the
// repository was purged or evicted; the entry's mu is held. A maintenance run on it is
// cancelled — what it writes is about to be thrown away — without waiting for
// it, since its pin keeps the directory until it has ended. Unpinned, it is
// discarded at once, and retire answers the directory its caller moves into
// trash/ once it holds no lock (Cache.trash); pinned, it waits in retiring for
// its last reader, and retire answers "".
func (c *Cache) retire(e *entry, inc *incarnation) string {
	if inc.retired {
		return ""
	}
	inc.retired = true
	if m := e.maint; m != nil && m.inc == inc {
		m.cancel()
	}
	if e.current == inc {
		e.current = nil
		c.heldRepos.Add(-1)
		c.noteChained(e)
		// The backfill takes no more steps of it (schedule.go).
		c.backfillDirty(e)
	}
	if inc.pins > 0 {
		e.retiring = append(e.retiring, inc)
		return ""
	}
	return c.discard(inc)
}

// setSize sets what inc's pack files and packed-refs hold, keeping the
// cache's running total (Cache.heldBytes) in step; the entry's mu is held.
// Every change to those sizes goes through here, and every change to the
// derived files' through setDerived, so the total is never recomputed.
func (c *Cache) setSize(inc *incarnation, disk int64) {
	if !inc.discarded {
		c.heldBytes.Add(disk - inc.diskBytes)
	}
	inc.diskBytes = disk
}

// setDropping sets what inc's dropping view's pack files hold — a part of
// what setSize counts, set beside it wherever those files move — keeping the
// node's dropping total (Cache.droppingBytes) in step; the entry's mu is held.
func (c *Cache) setDropping(inc *incarnation, dropping int64) {
	if !inc.discarded {
		c.droppingBytes.Add(dropping - inc.droppingBytes)
	}
	inc.droppingBytes = dropping
}

// setDerived records d as what inc holds of the declaration name on disk —
// no entry when present is false, since a declaration with no file on disk
// has none — keeping metaBytes, the entries' bytes together, and the cache's
// running total in step; the entry's mu is held. EVERY CHANGE to what an
// incarnation records of its derived files goes through here, so the bytes a
// boot restores from the state (their sum) are the bytes the budget counted.
func (c *Cache) setDerived(inc *incarnation, name DerivedName, d derivedState, present bool) {
	was := inc.derived[name].Bytes
	if present {
		if inc.derived == nil {
			inc.derived = map[DerivedName]derivedState{}
		}
		inc.derived[name] = d
	} else {
		delete(inc.derived, name)
		d.Bytes = 0
	}
	if !inc.discarded {
		c.heldBytes.Add(d.Bytes - was)
		if name == DerivedMemo {
			// The memo's own running total beside the cache's: what its share
			// is held to (MemoShare), with no walk of the node's copies.
			c.memoBytes.Add(d.Bytes - was)
		}
	}
	inc.metaBytes += d.Bytes - was
}

// made records that the write step has just written inc's derived file name
// over covers packs: what the maintainer reads of it until a change drops it
// or a measure finds it gone. Its bytes are a measure's (recordDerived). The
// entry's mu is held.
func (c *Cache) made(inc *incarnation, name DerivedName, covers int, step Step) {
	d := inc.derived[name]
	d.Covers, d.Step = covers, step
	c.setDerived(inc, name, d, true)
}

// recordDerived sets what inc records of its derived files from one
// measurement of them (image.derivedSizes): each declaration with a file on
// disk takes that measure's bytes, keeping what it records of the write that
// made it while its main file is there and forgetting it where it is not —
// an index gone covers nothing, a graph gone is due again, and a part left
// beside no main file (a bitmap a killed writer left with no index) is
// counted, never read as made; a declaration with no file on disk has no
// entry. The entry's mu is held.
//
// A DECLARATION WITH NO MAIN FILE IS PRESENT WHERE ITS RECORD SAYS SO: the
// provenance index's runs have no file whose presence is the index's, so a
// measure has nothing to judge it by — its record names the runs and their
// bytes (provenanceState), kept WHOLE, and only files of it no record names are
// counted from the measure, never read as made. Reset by the main-file rule, it
// was wiped — its coverage and its runs forgotten — after every round's index
// and every maintenance run (TestAMaintenanceRunKeepsTheProvenanceState).
//
// A TREE'S BYTES ARE ITS WRITER'S RUNNING TOTAL (Owned.Tree): a measure stats
// nothing inside it, nor the directory, whose inode's few KiB would stand for a
// memo of gigabytes — written into the record after every round's index and
// every maintenance run, it overwrote what the memo counted with that
// (TestTheHeldBytesCountTheMemo). So a measure leaves a tree's record as its
// writer left it, and a tree with no record — a copy whose state named none, a
// previous process's — is given one for the janitor to count by listing it
// (memo.go's foundMemo). Nor does a measure that found no tree take the record
// away: the measure is made before its record is read, outside the entry's
// lock, and an answer placed between the two is in a tree the measure did not
// see.
func (c *Cache) recordDerived(inc *incarnation, m derivedMeasure) {
	c.image.each(c.image.all(), func(i int, d *Derived) {
		bytes, present := m.bytes[d.Name]
		s := inc.derived[d.Name]
		switch {
		case d.tree():
			if _, recorded := inc.derived[d.Name]; present && !recorded {
				c.foundMemo(inc)
			}
			return
		case !d.hasMain() && s.Step != "":
			return
		case !d.hasMain(), m.mains&(1<<i) == 0:
			s = derivedState{}
		}
		s.Bytes = bytes
		c.setDerived(inc, d.Name, s, present)
	})
}

// discard takes a retired incarnation nobody holds out of the cache — its
// bytes out of the total, whether or not its directory is ever moved: nothing
// names the directory any more, and one a failed move left is the next boot's
// to count and clear — and answers the directory, which its caller moves into
// trash/ (Cache.trash) once it holds NO LOCK: a rename can wait on the
// filesystem's journal, and the locks a discard is decided under are every
// request's to the repository (entry.mu) and, for an eviction or a purge,
// every request's to any repository on the node (Cache.mu). A repository's
// next incarnation never waits for the move either: it takes the next free
// number while the old directory is still there (newIncarnation). The
// entry's mu is held; "" when inc was already discarded.
func (c *Cache) discard(inc *incarnation) string {
	if inc.discarded {
		return ""
	}
	inc.discarded = true
	c.heldBytes.Add(-(inc.diskBytes + inc.metaBytes))
	c.droppingBytes.Add(-inc.droppingBytes)
	c.memoBytes.Add(-inc.derived[DerivedMemo].Bytes)
	// No reader holds it, so every run of its index mapped goes now.
	c.forgetRuns(inc)
	return inc.dir
}

// trash moves a directory a discard answered into trash/, kicks the janitor
// and sets the gauges; "" does nothing. A rename that fails leaves the
// directory where it is: no entry names it any more, and the next boot's
// reading trashes it as an older incarnation or converges it as the newest —
// so it is logged, never fatal. Once Stop has begun it moves nothing, for the
// same reason: the directory is the next Open's, which may be another
// process's already. A move that began before Stop is work Stop waits for
// (enter). Called holding no lock.
func (c *Cache) trash(dir string) {
	if dir == "" {
		return
	}
	c.setGauges()
	if !c.enter() {
		return
	}
	defer c.leave()
	if err := toTrash(c.dir, dir, newV7()+"-"+filepath.Base(dir)); err != nil {
		c.log.Warn("code_cache_trash_failed", "path", dir, "error", err)
		return
	}
	c.kick()
}

// newIncarnation makes e's next incarnation — one past the highest it has
// had. A FIRST incarnation (current: the entry has none) is e's current one
// from the moment it is made: there is nothing else to serve, and no reader
// is handed it before a round has converged it (a reader is answered at once
// only from an incarnation this process has converged). A REBUILD's is not:
// it is built beside the current one, which serves until the rebuild has
// converged its replacement (promote).
//
// Its skeleton is a bare repository git can run in: HEAD naming main (the
// round's refs step names the repository's own default branch before any
// reader sees it), config gitproc.RepoConfig, a packed-refs of the header
// alone, and the empty directories refs/heads, refs/tags, objects/pack and
// objects/info. The number is taken by os.Mkdir, never by a rename over a
// name: a directory already there under it — a purged repository's
// incarnation a reader still holds — keeps its name, and the next number is
// taken. A skeleton that cannot be written is moved into trash/.
func (c *Cache) newIncarnation(e *entry, current bool) (*incarnation, error) {
	e.mu.Lock()
	n := 1
	for _, inc := range append([]*incarnation{e.current}, e.retiring...) {
		if inc != nil && inc.n >= n {
			n = inc.n + 1
		}
	}
	e.mu.Unlock()
	repos := filepath.Join(c.dir, reposDir)
	var dir string
	for ; ; n++ {
		dir = filepath.Join(repos, e.id+"."+strconv.Itoa(n)+".git")
		err := os.Mkdir(dir, hostbox.DirMode)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("making %s: %w", dir, err)
		}
	}
	if err := c.skeleton(dir); err != nil {
		if terr := toTrash(c.dir, dir, newV7()+"-"+filepath.Base(dir)); terr != nil {
			err = errors.Join(err, terr)
		}
		return nil, err
	}
	// A new directory holds no memo tree, so nothing of one is unknown to it.
	inc := &incarnation{n: n, dir: dir, lastUsed: c.now(), memoListed: true}
	if current {
		e.mu.Lock()
		if e.current == nil {
			c.heldRepos.Add(1)
		}
		e.current = inc
		c.noteChained(e)
		e.mu.Unlock()
	}
	return inc, nil
}

// skeleton writes an empty bare repository's files into dir. A swap's staged
// incarnation writes none of them: it links its predecessor's (swap.go).
func (c *Cache) skeleton(dir string) error {
	for _, d := range []string{"refs/heads", "refs/tags", "objects/pack", "objects/info"} {
		if err := os.MkdirAll(filepath.Join(dir, d), hostbox.DirMode); err != nil {
			return fmt.Errorf("making %s: %w", filepath.Join(dir, d), err)
		}
	}
	for _, f := range []struct{ name, content string }{
		{"HEAD", string(headFor("main"))},
		{"config", gitproc.RepoConfig},
		{"packed-refs", packedRefsHeader},
	} {
		staged, err := c.stage(hostbox.FileMode, func(w *os.File) error {
			_, werr := w.WriteString(f.content)
			return werr
		})
		if err != nil {
			return err
		}
		if err = c.place(staged, filepath.Join(dir, f.name)); err != nil {
			return err
		}
	}
	for _, d := range []string{dir, filepath.Dir(dir)} {
		if err := syncDir(d); err != nil {
			return err
		}
	}
	return nil
}
