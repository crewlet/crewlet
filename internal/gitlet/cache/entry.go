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

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
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

// Synced is a repository's directory handed to a reader PINNED: Dir is valid
// until Release. There is no other way to a repository's directory, so no
// reader holds one a rebuild or a purge is about to remove. Every holder
// defers Release.
type Synced struct {
	// Dir is the incarnation's directory, absolute and clean.
	Dir string
	// Repo is the repository row the cache last read.
	Repo gitlet.RepoRef
	// Position: every row up to here is reflected in Dir.
	Position statelog.Position
	// Kind is what this hand-out cost.
	Kind SyncKind
	// Repairer names this reader to the cache's observer. What a git process
	// naming it reports (gitproc.Cmd.Repairer) is HELD for this reader
	// (report.go): its repair ([Cache.Rebuild]) consumes it, and Release hands
	// the rest to the ordinary report rules.
	Repairer gitproc.Repairer
	// Dropping is Dir's dropping view (Dir/objects-dropping: the packs an
	// expunge left gitlet.PackDropping), or "" where Dir holds none. Every process
	// that SERVES Dir names it (gitproc.Cmd.Dropping), so a reader still on a
	// dropping object is answered by id; nothing that writes objects over Dir
	// names it, so no push relies on a dropping object (Cache.NewScratch).
	Dropping string
	// DropEpoch is the repository's drop epoch as Dir's objects/ holds it: the
	// row's (Repo.DropEpoch), or the epoch before while a begin's inputs are still
	// in objects/ awaiting their swap. A push checked over a scratch on Dir
	// carries it (gitlet.Proposal.DropEpoch) and its committer refuses another
	// epoch's, so no push relying on an object the begin made dropping is decided.
	DropEpoch int64

	// release unpins Dir, once: what [Synced.Release] calls.
	release func()
	// inc and entry are what Rebuild checks against the incarnation readers are
	// handed now; owner is the cache that handed it out.
	inc   *incarnation
	entry *entry
	owner *Cache
}

// Release unpins Dir. Idempotent, and A NO-OP ON THE ZERO Synced every
// failed Sync answers: a holder deferring the release of a variable a later
// Sync may overwrite must be left something safe to release, not a nil
// function that panics on the request's goroutine.
func (s Synced) Release() {
	if s.release != nil {
		s.release()
	}
}

// handedOut refuses a Synced this cache did not hand out (a zero one, or
// another cache's): only the cache that pinned an incarnation may act on it.
func (c *Cache) handedOut(s Synced, what string) error {
	if s.inc == nil || s.entry == nil || s.owner != c {
		return fmt.Errorf("%w: %s names the Synced a sync of this cache handed out", ErrInvalid, what)
	}
	return nil
}

// entry is one repository's place in the cache. Lock order is ALWAYS write,
// then Cache.mu, then entry.mu. Neither mutex is held across a git process, an
// estate call or a file copy, and nothing holding one WAITS for write: an
// adoption's try and a holder's release are taken under entry.mu, and neither
// waits (adopt.go).
type entry struct {
	id string

	// write guards what the directory HOLDS. Every round holds it WHOLE
	// (writeExclusive) for its body, and Verify for its listing: a full round
	// sweeps what its snapshot does not name and counts what it finds, so a pack
	// linked beside it could be swept or counted twice, and Verify must see no
	// step half done. Each adoption holds ONE, since adoptions link distinct
	// files. Whole is taken under a context, so a waiting Verify gives up with its
	// caller. An adoption only TRIES: finding it held or awaited whole, it hands
	// its pack to that holder (handed) rather than wait behind another's
	// downloads; a weighted semaphore's try fails while a whole taker waits, so
	// adoptions never starve a round.
	write *semaphore.Weighted
	// handed is the pushed packs adoptions handed to write's whole holder, by
	// git checksum: staged in tmp/ (adopt.go), claimed by a round about to
	// download one, and installed by the holder before it lets go (releaseWrite).
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
	// searched is when a search last asked this node's code index of the
	// repository (Cache.IndexTip): the code indexes' trim orders by it, not by
	// last use, which the indexer's own looks do not move. Per repository, so a
	// swap or a rebuild keeps it; zero until a search asks after boot.
	searched time.Time
	// maint is the maintenance running on an incarnation of the repository,
	// if any (maintain.go): one at a time a repository.
	maint *maintRun
	// swap is the swap in flight (swap.go), from staging the next incarnation to
	// its end: the leaving copy's index of pushes takes no commit meanwhile
	// (commitIndex), so every run the swap carries is on disk to link, and what a
	// git process in the staged directory reports is that copy's (swapAt).
	swap *swapping
	// merges is each standing merge's store of the repository's packs
	// (NewMergeScratch), by directory, with the incarnation its links came from:
	// what a git process over one reports is about that copy (mergeAt). Each goes
	// as its scratch is removed.
	merges map[string]*incarnation
	// verifying counts the Verify calls checking the repository: no maintenance
	// run starts while one does (verify.go), since a run in the lane's last free
	// slot would refuse its fsck. A run already in flight is not stopped; its
	// files are admitted instead (Write.Holds).
	verifying int
	// adopting counts adoptions holding ONE of write outside a round (Adopt, and
	// acquireWrite's settle after a failed whole wait): as during a round, no
	// maintenance run starts (runnable). An adoption adds a pack, which may drop
	// what a run writes (dropFor), stopping a run in flight; one started between
	// that drop and the pack's links would record a file written over the old
	// packs as current. A pass
	// that finds one owes the maintainer a wake when the last lets go
	// (maintainerOwed).
	adopting int
	// lastAdded is when a round last added packs: the maintainer's debounce.
	lastAdded time.Time
	// maintainerOwed: a full round converged, or a pass found the repository due
	// while an adoption held it (oweWake); the maintainer is woken once no round
	// runs (run) and no adoption holds it (releaseWrite), since a pass starts no
	// run while either does (runnable).
	maintainerOwed bool
	// runOwed: a pass found the repository due while a round held it, and the
	// run starts as that round ends (startOwed), before the round queued behind
	// it — where a wake could only race that next round for the gap.
	runOwed bool
	// uncoveredSince is when the oldest pack the maintainer's index does not
	// cover arrived, zero when none waits: the debounce's ceiling (MidxMaxWait).
	// Set by a round adding packs while none wait; cleared by a run covering all.
	uncoveredSince time.Time
	// lastRebuild is when the last rebuild ATTEMPT started; lastAsked when a
	// rebuild of the current incarnation was last ASKED for (refused, joined or
	// started), from which RebuildQuiet is measured; refusedLogged when a refusal
	// was last logged, so a repository failing every request logs once a
	// RebuildCooldown.
	lastRebuild, lastAsked, refusedLogged time.Time
	// wantHistory: this process answered a read of the repository as its home
	// (gitlet.GitQuery.Home), so the backfill reads its index's history down to
	// the rows' push history (Cache.WantHistory, provenance.go). In memory and per
	// repository: a rebuild's product wants what its predecessor did, and the
	// index's Complete is what persists.
	wantHistory bool
	// bf is the repository's place in the backfill's schedule (schedule.go),
	// under the schedule's mu; bfQueued says it is in a due queue there, read
	// under this mu alone, so a use of a copy with no step due takes no schedule
	// lock. bfFailures counts its failed steps in a row, and bfRetryAt (on
	// Options.Now's clock) is when the last lets it take the next: a failing step
	// delays only its own repository (backfillStep).
	bf         slot
	bfQueued   atomic.Bool
	bfFailures int
	bfRetryAt  time.Time
	// chained: the current incarnation serves from an installed published chain
	// (layers.go), as crewlet.code.cache.chained counts; kept in step by
	// noteChained wherever the current incarnation or its chain changes, so the
	// gauge is a running total, not a walk.
	chained bool

	// stateMu serialises persisting the state.
	stateMu sync.Mutex
}

// newEntry is repository id's place in the cache, with no incarnation yet.
func newEntry(id string) *entry {
	return &entry{id: id, write: semaphore.NewWeighted(writeExclusive), bf: slot{at: -1}}
}

// incarnation is one directory of a repository: repos/<id>.<n>.git. A
// rebuild or a swap (swap.go) builds the next number BESIDE the one readers
// hold rather than renaming over it: a git process whose GIT_DIR is that path
// would otherwise open the new directory's files midway through its run.
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
	// converged: a full round ran on it in this process, so nothing else has
	// written it since: every later round may be a delta, and a caller it already
	// meets is answered at once.
	converged bool
	// reread is why its next full round reads it whole again (Cache.reread for a
	// log recreated or gone back; markWhole for an in-place repair), counted when
	// that round completes; "" for none. Only the FIRST reason is kept: one whole
	// read answers them all, counted once.
	reread RebuildReason
	// marks counts the times it was marked to be read whole IN PLACE
	// (markWhole): a round converges it only if no mark was made since the round
	// began, since a round that swept it before the damage did not repair it.
	marks uint64
	// drop is the derived files, by declaration, a git process named damaged
	// (markWhole for RebuildDerived), gathered over every mark, so its next full
	// round DROPS them before deciding its own index (apply). Apart from reread,
	// which keeps only the first reason: a copy already marked for another would
	// be read whole with the damaged files kept, and the retry meet the same
	// damage. Taken by the round that drops them.
	drop derivedDrops
	// packChecks are the hashes of packs hints named (gitproc.Hint), by
	// checksum, in flight or decided (ConfirmHint, hint.go): a hint about a pack
	// being hashed waits for that hash, and one about a pack decided on its
	// current files hashes nothing. A verdict carries the file identity it was
	// reached on, so it outlives writes that leave those files alone and none
	// that replace them; a hash that decided nothing yields to the next hint's.
	packChecks map[string]*packCheck
	// rebuilds is how many rebuild attempts IN A ROW stand behind this
	// incarnation without ending the failure that asked for them: 0 for one no
	// rebuild made, its predecessor's plus one for a rebuild's product, plus one
	// per failed rebuild of it; a rebuild asked after RebuildQuiet with none asked
	// starts again from 0. unmoved: it holds what the last attempt read (it is
	// that product, or that rebuild failed definitively on its rows) and no round,
	// sweep or maintainer write has touched it since; cleared with the line.
	// Rebuild refuses by both (rebuild.go).
	rebuilds int
	unmoved  bool

	// state is the state as last persisted, but for its Derived map; nil when
	// there is none. The map moves to derived (fromState) and is dropped from each
	// state persisted: a round reads only the state's position and stream, and
	// kept in both every repository would hold its derived record twice.
	state *state
	// at is the in-memory position, at or past state's: a round whose delta
	// was empty moves it without rewriting the state, since nothing between
	// the two changed the directory.
	at statelog.Position
	// stream is the StreamCreated of the answer at came from.
	stream     time.Time
	repo       gitlet.RepoRef
	refsDigest string
	refs       int
	packs      int
	// objects is how many objects its packs hold, as their rows count them: set
	// by every full round and raised by each pack a delta installs, so exact on
	// every incarnation this process converged, the only ones the maintainer runs
	// on and sizes memory by. Never persisted: a boot's first round is full.
	objects int64
	// diskBytes is the pack files and packed-refs (set only by setSize),
	// metaBytes the sum of derived's Bytes (only by setDerived), each keeping the
	// cache's total in step. droppingBytes is diskBytes' share in the dropping
	// view (objects-dropping/pack), set only by setDropping, which keeps
	// crewlet.code.cache.dropping.bytes in step.
	diskBytes, metaBytes int64
	droppingBytes        int64
	// dropEpoch is the drop epoch its objects/ holds (Synced.DropEpoch). It
	// becomes the row's once a round leaves nothing leaving the copy (every pack
	// file one the rows name there) and its installed chain holds no pack for a
	// non-live row (liveChain); until then it
	// stays, since a begin's inputs, holding what the begin made dropping, leave
	// objects/ only by the swap. A delta finding the rows' epoch past it takes out
	// every pack the chain holds for no live row (applyDelta). In memory only: a
	// copy's first full round in a process sets it before any reader.
	dropEpoch int64
	// derived is its record of each declared derived file on disk, by name
	// (state.go's derivedState); no file, no entry. maintained is its last
	// maintenance run, the duty cycle's clock. Both persist; maintain.go reads
	// them through covered, bitmapped and graphed.
	derived    map[DerivedName]derivedState
	maintained maintainedState
	// graphFailed: its graph-first run's commit-graph step failed (not a slot
	// refused, not a run stopped from outside), so the graph is due by the
	// ordinary rules (maintain.go, GRAPH FIRST), not every pass. Cleared by a
	// graph written; never persisted, so a boot asks again at once.
	graphFailed bool
	// tagsFailed is when its tag step last failed: an index set aside or
	// missing waits TagsRetry after it (tags.go). Never persisted: a boot's
	// full round builds it.
	tagsFailed time.Time
	// tagsBad is the tag index last logged unreadable, so a file is logged once.
	tagsBad fileID
	// vouched is every checkable file this process wrote or checked (check.go),
	// with the identity it had then: a pack's .idx and .rev under the pack's
	// checksum, derived files by path relative to the incarnation. NEVER
	// PERSISTED: a new process checks every inherited file to its trailer once,
	// at the first full round. A vouch spares a file the check, never the scrub;
	// no .pack is vouched, since the check reads none. Keyed by the checksum's
	// BYTES, one entry a pack, because the map lives in every incarnation and
	// grows a pack a push: 118 to 207 bytes a pack (about 170 MB at a million
	// packs), where keys by each file's path cost 528
	// (TestAPacksVouchesFitInAQuarterKilobyte).
	vouched vouches
	// checkedOnce: a full round of this process has held its files to their
	// trailers — the check's `first` and every later one's `unvouched`.
	checkedOnce bool
	// indexRepair: its multi-pack-index was dropped as damaged, so the
	// maintainer's index is due AT ONCE, with no debounce or interval, in a slot
	// of the maintainer's own (maintain.go); cleared by the index written. A
	// damaged commit-graph needs no mark: a missing graph is written first anyway.
	indexRepair bool
	// repair is what reports asked its next full round to do (report.go).
	repair repairMark
	// held is what its readers' runs reported that the reader's own repair
	// answers (report.go, HELD FOR THE REQUEST), by Synced.Repairer: a key for
	// each reader pin hands it to, taken by its release, and for each round
	// applying to it, taken as the round settles (roundReader), so a report
	// reaches a reader still there or the ordinary rules, never one gone.
	held map[gitproc.Repairer][]heldReport
	// THE PROVENANCE INDEX'S IN-MEMORY HALF (provenance.go). mapped is every run
	// of its index mapped here, by number, held by a reader or kept within the
	// node's bound (runmaps.go); runHolds is each reader's mappings
	// (Cache.Provenance), let go by its release. A run the index no longer names
	// is unmapped with its last holder, and all of them on discard. historyGone:
	// the log under it was recreated or went back (Cache.reread), so its next full
	// round drops its index first (EventHistoryGone). provGen counts its indexes,
	// so a backfill page read for one is never committed into another; provNext
	// numbers the next run of any index, so no number names two files while a
	// retired one awaits trash/. retiredRuns is the run files its index stopped
	// naming that the disk's state may still name, each trashed by the next state
	// written (persist).
	mapped      map[uint64]*mappedRun
	runHolds    map[gitproc.Repairer][]*mappedRun
	historyGone bool
	provGen     uint64
	provNext    uint64
	retiredRuns []string
	// codeIndex is the code index its record names, opened by the first reader
	// (Cache.CodeIndex) and kept while the record is inc's; nil until then and on
	// every change of the record. codeIndexOpening is the opening in progress, nil
	// for none: other readers wait for it rather than map the files again, a
	// search for no verifying one (Cache.codeIndex).
	codeIndex        *openedCodeIndex
	codeIndexOpening *indexOpening
	// codeIndexRefs is the refs the record's tip was last read under
	// (Cache.Index): an indexer's look that finds them still the copy's
	// reads no tip.
	codeIndexRefs indexRefs
	// THE MEMO'S IN-MEMORY HALF (memo.go); its record is derived[DerivedMemo].
	// memoMu, the memo's own lock, is held only across placing or moving answers,
	// taking out a cohort, the tree or an entry, and the listing; never with
	// entry.mu, which it is taken before. memoListed: every cohort directory on
	// disk is in the record (this process made the tree, or listMemos listed it);
	// a boot's copy is not until its first listing. memoUnlisted: the last
	// listing failed (code_cache_memo_unlisted), so the janitor retries only at
	// its tick; written under entry.mu, carried with memoListed (carryMemo).
	// memoVersion moves with every record change (setMemo); the janitor's tick
	// persists it once memoStated differs (Cache.stateMemos). memoGen counts
	// drops, so a batch begun before one places nothing after; memoShut: dropped,
	// its round's change not yet visible (dropMemo, reopenMemo), or carried away.
	memoMu                  sync.Mutex
	memoListed              bool
	memoUnlisted            bool
	memoVersion, memoStated uint64
	memoGen                 uint64
	memoShut                bool
	// written is when this process last changed each declaration's files (a
	// file's identity moved, the round's own index, or a drop), a time.Now()
	// reading compared on the monotonic clock with a report's Began (dropDamaged).
	written map[DerivedName]time.Time
	// damagedAt is when each declaration's files were last dropped for damage,
	// on Options.Now's clock: dropped again within RebuildQuiet, they are damaged
	// AGAIN (D13 of report.go's doc) and wait for the ordinary rules rather than
	// being rewritten at once, whatever writes came between: a disk that keeps
	// damaging what is written damages those too. againLogged is those logged.
	damagedAt   map[DerivedName]time.Time
	againLogged derivedSet
	// memoryRefused is the steps whose charge alone exceeds what the node's
	// memory gives background work: each logged and counted once an incarnation,
	// however often found again.
	memoryRefused maintStep
	// publishRefused is the charge of the repository's last publication the
	// node's memory refused (Cache.AdmitPublication), 0 for none: what each
	// maintainer pass judges for the gauge and the alarm (memory.go's
	// refusalWalk). Cleared by a publication admitted, or by the rows' chain
	// moving (setChain): another node published it.
	publishRefused int64
	// inherited: a previous process left it (Open indexed it from disk), so none
	// of its bytes were held to their rows here, and its first full round in this
	// process makes its first scrub due (scrub.go).
	inherited bool
	// scrubbing: its scrub is running, at scrubRunning. A report meanwhile is
	// answered by a running scrub evidence asked for (a report's, a request's),
	// which hashes every file, and ends a routine one to run a report's instead
	// (scrub.go). scrubDue is why its next scrub is due AT ONCE (zero when only
	// its interval makes it due) and scrubAsked when: the scrubber takes those by
	// evidence, then oldest first.
	scrubbing    bool
	scrubRunning scrubPrio
	scrubDue     scrubWhy
	scrubAsked   time.Time
	// scrubBase is when every byte it holds was last held to its checksum (its
	// last scrub's end, or for a copy this process built, its first full round's,
	// which placed every file after holding it to its row); zero until then.
	// ScrubInterval runs from it. scrubbedAt is when its last scrub ended and
	// scrubFound whether it found anything: ScrubCooldown runs from them. All on
	// Options.Now's clock.
	scrubBase, scrubbedAt time.Time
	scrubFound            bool
	// scrubBegan and scrubEnded are its last scrub's span as time.Now()
	// readings, compared on the monotonic clock with repairWrite and a failing
	// process's Began; scrubWhole is the packs that scrub found whole, all three
	// files, by checksum (and, while its findings await repair, the packs a
	// rebuild past them links, less those they name). An escalation
	// (Cache.escalate) takes that scrub as its own when it began after the
	// process failed, or found nothing within ScrubCooldown with no repair
	// written since: the same bytes would be found whole again.
	scrubBegan, scrubEnded time.Time
	scrubWhole             []string
	// repairWrite is when this process last wrote into it what can END a pack's
	// or an object's failure (a pack installed again over its files, a lost pack
	// installed again, a derived file dropped so git reads around it), as a
	// time.Now() reading compared on the monotonic clock with the failing
	// process's Began (rebuild.go, repairDamaged). NOTHING ELSE: a push's pack, an
	// adoption or the maintainer's index or graph leave whatever failed as it
	// was, and counting them would answer a busy repository's damage "as it
	// stands" and never repair it.
	repairWrite time.Time
	// forcedAt and forcedEnded are when its last FORCED check (the in-place
	// repair of a request's or a round's pack or object damage, every index file
	// held to its trailer) ended, on Options.Now's clock and as a time.Now()
	// reading: recorded as that check's round ends, whatever else the round met,
	// since its findings wait on the repair mark anyway (recordCheck). A repair
	// asked within ScrubCooldown of it that names no fetchable pack, with no
	// repair written since (the round's own included), gets no other (rebuild.go,
	// repairDamaged): the same files would hash the same, and each forced round
	// reads the rows whole and holds every reader while it hashes a quarter of
	// the copy's bytes.
	forcedAt, forcedEnded time.Time
	// lastUsed is when a reader last took or released it, or made a push's
	// scratch over it: set as the incarnation is made or read at boot, and moved
	// only by used.
	lastUsed time.Time
	// stamped is the last use the disk records (lastUsedOn): read at boot,
	// moved forward by the janitor's recordUse.
	stamped time.Time

	// THE LIVE SET (swap.go). live is the incarnation's PACK ROWS by pack id, as
	// last converged, dropping ones included: set whole by every full round,
	// changed by a delta's added and retired rows and by an adopted pack; by pack
	// id, so a pack adopted then served by the next delta is one entry.
	// byChecksum counts the rows naming each git checksum (two rows may share one
	// file) and how many are dropping, changed only by setLive and deleteLive, so
	// a pack's files leave only once NO row names them, and leave objects/pack
	// for the dropping view once only dropping rows do (placeOf). In memory only:
	// a boot's first round is full. A swap reads what it keeps and drops, and
	// where, from them, never from the directory.
	live       map[string]livePack
	byChecksum map[string]checksumRows
	// droppingPacks is how many packs the live set places in the dropping
	// view (placeOf): kept in step by placeMoved, so a reader is handed the
	// view (droppingView) exactly while the copy holds a pack in it.
	droppingPacks int
	// droppingObjects is how many objects those packs hold, each pack once, kept
	// beside droppingPacks by placeMoved: what Verify's connectivity check walks
	// besides objects, which counts objects/pack's alone (verifyCharge), since git
	// marks every object of every object directory it is handed.
	droppingObjects int64
	// leaving is the pack files the incarnation holds that are to leave where
	// they are, keyed by place and checksum (or by name, for a pack-named file
	// whose name carries no checksum), each with the event taking it out:
	// EventPackRetired, EventPackExpunged (a dropping pack no row names), or
	// EventPackRemoved (a stray). A pack MOVING between objects/pack and the
	// dropping view leaves its old place. Non-empty only between a round's apply
	// and its swap, or after a failed swap, which the next round retries.
	leaving map[leaveKey]Event
	// leavingSince is the position every retirement of what is leaving is above,
	// zero while nothing is (state.LeavingSince): persisted, so a full round after
	// a crash between a round and its swap asks its retirements from there and
	// reads those files as retirements.
	leavingSince statelog.Position
	// successor is the incarnation a swap replaced this one by (swap.go),
	// nil for one no swap replaced: what a verdict reached over this one's
	// files after the swap — a scrub's, a forced check's — is recorded on
	// the one serving them now, which holds the same inodes (serving).
	successor *incarnation

	// THE PUBLISHED CHAIN (layers.go). chain is the ROWS' published chain, root
	// first, as of the last snapshot serving it (gitlet.RepoSnapshot.LayersServed:
	// every full answer, and a delta whose chain moved); chainRead says one has.
	// installed is the layers the incarnation HOLDS, root first, with their rows:
	// a prefix of chain after a full round, and between a delta moving the chain
	// and the installer's placement an older chain, known by its packs until cut.
	// installedPacks is every pack an installed layer names, with its row's
	// figures: packs no round takes out while a layer names them. chainGen counts
	// every change to chain or installed, so a placement planned over one sees it
	// moved. refused is the layers this node won't retry while the rows name each
	// at the record that added it. All in memory: every process's first round is
	// full, and a restart tries a refused layer once more.
	chain          []gitlet.LayerRow
	chainRead      bool
	chainGen       uint64
	installed      []gitlet.LayerRow
	installedPacks map[string]livePack
	refused        map[string]layerRefusal

	// THE NODE'S TIP (maintain.go): its own commit-graph layer over the commits
	// no installed layer holds, the last line of commit-graph-chain; none in own
	// mode, and none until the maintainer writes one over a tail holding a
	// commit. tipOver is the digest of the tail the last successful write covered
	// (writing the tip, or finding the tail holds no commit the layers lack), and
	// tipWritten whether one has since the tip went: the tail moving off tipOver
	// makes the tip due again. Both persist with the tip (derivedState.TipOver),
	// so neither a restart nor a full round over the same tail rewrites a quiet
	// copy's tip. tipFailed: writing a tip the copy lacked failed (not a slot
	// refused, not a run stopped from outside), so it is due by the ordinary
	// rules rather than every pass, as graphFailed is own mode's; never persisted.
	tip        tipLayer
	tipOver    tailDigest
	tipWritten bool
	tipFailed  bool
	// tail is the TAIL as its rows count it (the live packs no installed layer
	// names, with their digest), kept in step by setLive, deleteLive, liveWhole
	// and recordInstalled, so the maintainer reads it in one load rather than
	// walking every pack under the cache's lock.
	tail tailCount
	// chainSeq counts the starts and ends of IN-PLACE writes of its chain files
	// (the tip's run, a cut), and chainWriting how many are in flight. Such a
	// write moves a chain file apart from its record for a while (git renames the
	// tip's chain file in before the run records it; a cut writes files before
	// its record), so the scrub's and Verify's verdict on a chain file stands
	// only if none was in flight as its expectation was read and the count has
	// not moved by the end of its hash (chainStill).
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

// tailCount is an incarnation's tail as its rows count it: the live packs no
// installed layer names, their objects and bundle bytes (what the node's tip,
// its charge and deadline are sized by, maintain.go), and their digest, which
// says whether the tip covers them.
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

// tailDigest is a TAIL's identity as a set of packs, whatever order or path
// reached it: the XOR of the first sixteen bytes of every pack's git checksum,
// moved in one step a pack (tailCount.add) and the same in every process, so
// the node's tip persists it. A checksum is a SHA-1 of the pack's bytes, so
// two different tails collide with chance 2^-128 a comparison, and a
// collision costs only speed: a tip not written again over a tail it does not
// cover. Not a count of the tail's changes, which moves at every full round
// and restart and would rewrite a quiet copy's tip each time.
type tailDigest [16]byte

// flip moves d by the pack checksum names, in or out: XOR is its own inverse,
// and a tail counts each pack's files once (setLive), so a pack is flipped in
// at most once while the tail holds it.
func (d *tailDigest) flip(checksum string) {
	var p tailDigest
	// A row's checksum is forty hex digits (gitlet's bundle check, the one way a
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

// livePack is one live-set row as the swap and the maintainer's plans read
// it: the checksum naming its files, the bundle's and its two indexes' bytes
// (its pack section, and whether it is FULL, gitlet.PackRow.Full, which a
// plan never merges, [Cache.Packs]), its row's object count, its content's
// date and arrival, and whether it is dropping. A pack id names one bundle
// forever, so two rows of a checksum carry the same figures. The object key
// and hash are not kept: nothing reading the set fetches by them, and the set
// lives in every incarnation.
type livePack struct {
	checksum                                string
	size, indexBytes, reverseBytes, objects int64
	// content is the row's gitlet.PackRow.Content, packed: what a purge's cycle
	// picks inputs by (internal/gitlet/compact). 0 where the set does not know it
	// yet: an ADOPTED pack (gitlet.PackRef carries no position) until a round
	// serves its row (setLive). Zero reads as older than every request, so a
	// purge's cycle rewrites an undated pack rather than passing it over.
	content int64
	// arrived is the row's gitlet.PackRow.Arrived, encoded (store.EncodeTime):
	// what an expunge on its maintainer's schedule keeps a young pack whole by. 0
	// where unknown, as for content, and read as arrived NOW, the side the grace
	// keeps whole: no scheduled expunge rewrites a pack before a round has served
	// its row.
	arrived int64
	// dropping: the row is one an expunge's begin left dropping
	// (gitlet.PackDropping), whose files the incarnation keeps in its dropping
	// view while no live row names the same checksum (placeOf).
	dropping bool
}

// liveOf is p, a row in state, as the live set keeps it: dropping exactly
// where state is gitlet.PackDropping, the one state kept out of objects/pack
// (read.go's checkPack refuses a state this build does not know), its content
// undated ([livePack.content]).
func liveOf(p gitlet.PackRef, state gitlet.PackState) livePack {
	return livePack{checksum: p.GitChecksum, size: p.Size, indexBytes: p.IndexBytes, reverseBytes: p.ReverseBytes,
		objects: p.Objects, dropping: state == gitlet.PackDropping}
}

// liveRow is a snapshot's row p as the live set keeps it: [liveOf], with the
// date its content is from and the instant its newest content arrived.
func liveRow(p gitlet.PackRow) livePack {
	lp := liveOf(p.Ref(), p.State)
	lp.content = p.Content.Packed()
	if !p.Arrived.IsZero() {
		lp.arrived = store.EncodeTime(p.Arrived)
	}
	return lp
}

// row is p, named id, as a plan reads it ([Cache.Packs]): every figure the
// live set keeps — its content's date zero where the set does not know it —
// and nothing a snapshot's row carries beside them: its object, its part, its
// kind, its state and the position it was added at stay zero.
func (p livePack) row(id string) gitlet.PackRow {
	r := gitlet.PackRow{PackID: id, Size: p.size, IndexBytes: p.indexBytes, ReverseBytes: p.reverseBytes,
		GitChecksum: p.checksum, Objects: p.objects}
	if p.content != 0 {
		r.Content = pos(p.content)
	}
	if p.arrived != 0 {
		r.Arrived = store.DecodeTime(p.arrived)
	}
	return r
}

// checksumRows is the live set's rows naming one git checksum: how many, and
// how many of them are dropping — what says where the incarnation keeps the
// files they name (placeOfRows).
type checksumRows struct{ rows, dropping int }

// packPlace is WHERE AN INCARNATION KEEPS A PACK'S FILES (M6-D3):
// objects/pack, read by every git process run in or leaning on it, or its
// DROPPING VIEW, objects-dropping/pack, read only by a process serving it
// (Synced.Dropping, gitproc.Cmd.Dropping), so an object an expunge left
// dropping is answered to a reader still on it but never relied on by a push,
// whose scratch leans on objects/ alone.
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

// placeOf is where inc's live set places the files of pack checksum cs, by
// its rows alone (placeOfRows): a pack an installed layer names is in
// objects/pack too (layers.go), which a caller asks of installedPacks. e.mu is
// held.
func placeOf(inc *incarnation, cs string) packPlace { return placeOfRows(inc.byChecksum[cs]) }

// leaveKey is one pack's files an incarnation holds that are leaving it: where
// they are, and the checksum they are named by — or, for a file of a pack
// directory named like a pack's whose name carries no checksum, its name.
type leaveKey struct {
	place packPlace
	name  string
}

// setLive records the row id, whose files are p's, in inc's live set; e.mu
// is held. A row already there is kept (a pack adopted then served by the
// next delta is one row), but learns its content's date and arrival if the
// adopted row lacked them ([livePack.content], [livePack.arrived]): rows are
// never rewritten, so a learned date is never wrong, and an adopted pack left
// undated would be passed over by every scheduled expunge for the
// incarnation's life. A pack named again where it lies no longer leaves it.
func setLive(inc *incarnation, id string, p livePack) {
	if held, ok := inc.live[id]; ok {
		learned := false
		if held.content == 0 && p.content != 0 {
			held.content, learned = p.content, true
		}
		if held.arrived == 0 && p.arrived != 0 {
			held.arrived, learned = p.arrived, true
		}
		if learned {
			inc.live[id] = held
		}
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

// placeMoved counts p, whose files the live set moved from was to now, into
// or out of inc's tail and dropping counts. THE TAIL counts a pack's files
// once, and only in objects/pack: a dropping pack holds no commit the node's
// tip, written without the view, covers. droppingPacks and droppingObjects
// count the view's packs and objects (droppingView). e.mu is held.
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
		inc.droppingObjects += p.objects
	case was == placeDropping && now != placeDropping:
		inc.droppingPacks--
		inc.droppingObjects -= p.objects
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

// recountTail counts inc's tail afresh (each pack in objects/pack once, those
// no installed layer names) for an installed chain set whole
// (recordInstalled) or a live set carried into a swap's next incarnation
// (promote). e.mu is held.
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

// deleteLive takes row id out of inc's live set and answers the checksum of
// its files ("" for a row the set lacks: nothing to drop) and their place
// before and after (placeOf): nowhere once no row names them, the dropping
// view once only dropping rows do. e.mu is held.
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
func liveWhole(inc *incarnation, packs map[packKey]gitlet.PackRow) {
	inc.live = make(map[string]livePack, len(packs))
	inc.byChecksum = make(map[string]checksumRows, len(packs))
	inc.tail, inc.droppingPacks, inc.droppingObjects = tailCount{}, 0, 0
	for _, p := range packs {
		setLive(inc, p.PackID, liveRow(p))
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
		if p.State == gitlet.PackDropping {
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

// placesAfter is where inc's live set places each pack snap names once a
// round applies snap (apply): a full snapshot's own places, or for a delta,
// inc's live set with its rows added (a held row kept, as setLive does) and
// its retired rows removed, for every checksum the delta names. e.mu is held.
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
			if p.State == gitlet.PackDropping {
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

// heldView is the dropping view the incarnation (or a swap's staged next one)
// at dir holds on disk, "" where none. The cache's own reads of what a copy's
// refs reach name it (the round's peel, a placement's canary, Verify's
// connectivity check): a round places a dropping pack before its live set
// says so, and a ref a push revived onto a dropping object in an expunge's
// window reaches one, which read without the view is named missing. Anything
// else at the name is the pool's to refuse (gitproc.ShapeDropping).
func heldView(dir string) string {
	view := filepath.Join(dir, gitproc.DroppingDir)
	if info, err := os.Lstat(view); err == nil && info.IsDir() {
		return view
	}
	return ""
}

// liveChain reports whether inc's live set places every pack chain's layers
// name in objects/pack (placeOf): a precondition of its drop epoch becoming
// the rows' (Synced.DropEpoch). A layer's pack for no live row, held between
// a publication retiring it and the installer's placement, holds what was
// retired, which an expunge begun since may have made dropping, and a push
// relying on that is what the epoch refuses. e.mu is held.
func liveChain(inc *incarnation, chain []gitlet.LayerRow) bool {
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
// apart from the index because a second process writes it and may be refused
// the slot the first had: read from the index alone, a repository with a
// current index and no graph would not be due again until MidxPacks more
// packs landed. e.mu is held.
func (inc *incarnation) graphed() bool { return inc.derived[DerivedGraph].Step == StepGraph }

// used records a reader's use of inc (pin, release, or newScratch's push
// scratch) on the cache's clock; the entry's mu is held. EVERY USE GOES
// THROUGH HERE: the FIRST use of any kind since scrubBase wakes the scrubber,
// which arms its wake for the interval's end (scrub.go) and has no other
// reason to wake for the copy, since only a use makes the interval count.
// Waking only once the interval passed, or only on a pin, would miss a busy
// copy used right after its scrub, or one whose first use was the release of
// a reader held across the scrub's end. A use of e's current copy with a
// backfill step due also moves it in that queue (touch): most recent first.
func (c *Cache) used(e *entry, inc *incarnation) {
	if !inc.scrubBase.IsZero() && !inc.lastUsed.After(inc.scrubBase) {
		c.kickScrubber()
	}
	inc.lastUsed = c.now()
	if e.current == inc && e.bfQueued.Load() {
		c.touch(e, inc.lastUsed)
	}
}

// noReader names a pin that is no reader's (a push's scratch or an
// advertisement view, scratch.go): no report is held under it, since its
// caller never repairs the incarnation's damage (report.go).
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
// observer holds the reader's reports under (report.go). It is no round's
// answer, so its kind is SyncNone; a round's own pin names its kind through
// pinAs. The entry's mu is held.
func (c *Cache) pin(e *entry, inc *incarnation) Synced {
	return c.pinAs(e, inc, SyncNone, true)
}

// pinAs is pin, whose pin and release are a USE (used) only when use is set.
// The indexer's look (Cache.indexSync) is none: it looks at every repository
// it indexes each IndexPoll, so as a use it would keep them all looking
// recently used and the budget would evict the ones people read. The entry's
// mu is held.
func (c *Cache) pinAs(e *entry, inc *incarnation, kind SyncKind, use bool) Synced {
	inc.pins++
	if use {
		c.used(e, inc)
	}
	by := c.reader(inc)
	return Synced{
		Dir:       inc.dir,
		Repo:      inc.repo,
		Position:  inc.at,
		Kind:      kind,
		Repairer:  by,
		Dropping:  droppingView(inc),
		DropEpoch: inc.dropEpoch,
		release:   sync.OnceFunc(func() { c.releaseAs(e, inc, by, use) }),
		inc:       inc,
		entry:     e,
		owner:     c,
	}
}

// release lets go of inc for a pin that is no reader's (noReader), as a use;
// a reader's pin lets go through its Synced, which hands held reports to the
// ordinary rules (handOn). The last release of a RETIRED incarnation moves it
// into trash/ and kicks the janitor, whose removal keeps a request letting go
// of a 20 GiB directory from waiting; of a current one while an eviction is
// due (overBudget), it kicks the janitor, so a repository too large to keep
// is evicted once no request holds it. Safe after Stop.
func (c *Cache) release(e *entry, inc *incarnation) {
	c.releaseAs(e, inc, noReader, true)
}

// releaseAs is release, a use only where use says so: a pin that was no use
// (pinAs) lets go as none.
func (c *Cache) releaseAs(e *entry, inc *incarnation, by gitproc.Repairer, use bool) {
	e.mu.Lock()
	if use {
		c.used(e, inc)
	}
	c.handOn(e, inc, by)
	c.releaseRuns(inc, by)
	gone := c.unpin(e, inc)
	e.mu.Unlock()
	c.trash(gone)
}

// unpin drops one pin on inc; the entry's mu is held. With the last pin, a
// retired incarnation is discarded and its directory answered for the caller
// to trash once it has let go of the entry (Cache.trash); a current one while
// an eviction is due (overBudget) kicks the janitor, since it was not
// evictable while held. A maintenance run's pin shares this, so its end does
// not leave eviction to the tick; but it is no USE, so lastUsed and the
// budget's order stay the readers'.
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

// retire takes inc out of service (replaced by a rebuild or a swap, or the
// repository purged or evicted); the entry's mu is held. A maintenance run on
// it is cancelled, not awaited: its pin keeps the directory until it ends.
// Unpinned, it is discarded and retire answers the directory for the caller
// to trash holding no lock (Cache.trash); pinned, it waits in retiring for its
// last reader and retire answers "".
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

// setSize sets what inc's pack files and packed-refs hold, keeping
// Cache.heldBytes in step; the entry's mu is held. Every change to those sizes
// goes through here, and every derived one through setDerived, so the total
// is never recomputed.
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

// setDerived records d as what inc holds of declaration name on disk (no
// entry when present is false), keeping metaBytes and the cache's totals in
// step; the entry's mu is held. EVERY CHANGE to an incarnation's derived
// record goes through here, so the bytes a boot restores from the state are
// the bytes the budget counted.
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
			// The memo's own total, which MemoShare holds it to without a walk.
			c.memoBytes.Add(d.Bytes - was)
		}
		if name == DerivedCodeIndex {
			c.codeIndexBytes.Add(d.Bytes - was)
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

// recordDerived sets inc's derived record from one measurement
// (image.derivedSizes); the entry's mu is held. Each declaration with a file
// on disk takes the measured bytes, keeping its write record while its main
// file is there and forgetting it where not (an index gone covers nothing, a
// graph gone is due again, a part beside no main file is counted, never read
// as made); one with no file has no entry. One with NO MAIN FILE (the
// provenance index's runs) keeps a record it has WHOLE
// (TestAMaintenanceRunKeepsTheProvenanceState). A TREE's bytes are its
// writer's running total (Owned.Tree, TestTheHeldBytesCountTheMemo): a
// measure leaves its record alone, gives an unrecorded tree one for the
// janitor to count (memo.go's foundMemo), and never removes one it did not
// find, since an answer may be placed between the measure and the lock.
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

// discard takes a retired incarnation nobody holds out of the cache, its
// bytes out of the totals whether or not its directory is ever moved (one a
// failed move left is the next boot's to clear), and answers the directory
// for its caller to trash holding NO LOCK (Cache.trash): a rename can wait on
// the filesystem's journal, and entry.mu blocks the repository's requests
// (Cache.mu, for an eviction or a purge, the node's). The next incarnation never waits for the move:
// it takes the next free number (newIncarnation). The entry's mu is held; ""
// when inc was already discarded.
func (c *Cache) discard(inc *incarnation) string {
	if inc.discarded {
		return ""
	}
	inc.discarded = true
	c.heldBytes.Add(-(inc.diskBytes + inc.metaBytes))
	c.droppingBytes.Add(-inc.droppingBytes)
	c.memoBytes.Add(-inc.derived[DerivedMemo].Bytes)
	c.codeIndexBytes.Add(-inc.derived[DerivedCodeIndex].Bytes)
	// No reader holds it, so every run of its index mapped goes now.
	c.forgetRuns(inc)
	return inc.dir
}

// trash moves a directory a discard answered into trash/, kicks the janitor
// and sets the gauges; "" does nothing. A failed rename is logged, not fatal:
// no entry names the directory, and the next boot trashes or converges it.
// Once Stop has begun it moves nothing, since the directory may already be
// the next Open's; a move begun before Stop is work Stop waits for (enter).
// Called holding no lock.
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

// newIncarnation makes e's next incarnation, one past the highest it has
// had. A FIRST one (current) is e's current from the moment it is made: there
// is nothing else to serve, and no reader is handed an incarnation before a
// round has converged it. A REBUILD's is built beside the current one, which
// serves until it converges (promote). The number is taken by os.Mkdir, never
// a rename: a directory already there (a purged repository's incarnation a
// reader still holds) keeps its name, and the next number is taken. A
// skeleton that cannot be written is moved into trash/.
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

// skeleton writes an empty bare repository git can run in into dir: HEAD
// naming main (the round's refs step names the repository's own default
// branch before any reader sees it), config gitproc.RepoConfig, a packed-refs
// of the header alone, and the empty ref and object directories. A swap's
// staged incarnation writes none of them: it links its predecessor's
// (swap.go).
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
