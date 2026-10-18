package cache

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A DAMAGE GIT NAMES ONLY IN WORDS IS CONFIRMED AGAINST THE ROWS. git 2.56
// reports a corrupt packed object by formatting its sentence into a buffer
// and dying with die("%s", buf) — a format that names nothing, which the
// pool therefore reads as no kind — and for a packed object whose header
// states the wrong size it says nothing else. The pool recognises exactly
// that one sentence, anchored at both ends and to the run's own pack
// directory, and records the pack it nominates as a gitproc.Hint; the hint
// decides nothing. What decides is the pack's BYTES: [Cache.ConfirmHint]
// hashes the pack the hint names, as a verify does, against the row that
// names it, and answers a repair only when they differ. A sound pack answers
// none whatever the message said; a pack no row names is no damage of the
// copy.
//
// ONE HASH A PACK, on the CACHE'S lifetime, never a request's. Damage that
// fails a hot repository fails every request in flight on it at once, and a
// request a balancer cuts after its minute of quiet — or a client that gives
// up — would otherwise take its hash with it, the next starting again from
// byte zero, so on a disk too slow for one request's life no hash would ever
// decide. So the first hint about a pack STARTS its hash (incarnation's
// packChecks) and every hint about it — the first included — WAITS for its
// verdict for as long as its own caller does: a caller that leaves leaves the
// hash running, and the next one asked takes its verdict. The round's own
// git takes the same path, after the round has let the entry's write lock go
// AND ENDED (Cache.run) — the repository has no running round while the pack
// is hashed, its incarnation pinned instead — because a hash of the largest
// pack a push stores is minutes on a slow disk, and a round still running
// holds every linearizable caller of the repository behind it: such a caller
// never rides a round that began before it asked, and every answer that
// names a ref is one. So the round queued behind it runs at once, an
// adoption takes the lock it let go, and only the hinted round's own callers
// wait for the verdict — queued again for a round on the current incarnation
// should a rebuild replace theirs meanwhile.
//
// A VERDICT HOLDS FOR THE FILES IT WAS REACHED ON: it is kept with the pack's
// three files' identity — device, inode, size and modification time, each,
// as the hash found them — and reused while they are those files, whatever
// else is written into the copy. A round that installs the pack again or
// sweeps it renames other files in (or none), which another hint then hashes
// afresh; an adoption of another pack, or the maintainer's index, bitmap and
// graph, touch no pack's files, so a confirmed copy whose rebuild a cooldown
// refuses meets its next failing request with the verdict rather than another
// hash of the same gigabytes for every push. A hash during which the files
// moved decides nothing.
//
// BOUNDED BY HASH alone — HashBudget of the bundle's bytes, its rows' read
// and the hash, never CheckBudget's fsck term, which a hint runs none of —
// and a READ SLOT a piece at a time (CheckPieceBytes, gitproc.Pool.HoldRead):
// hashing is a core's work requests wait on, so it runs in the lane that
// counts a core a slot for it, and N repositories damaged together by one
// disk's incident hash on the read lane's cores rather than beside them. A
// hash the node cannot fit — no read slot within gitproc.SlotWait, or past its
// bound — decides nothing (unread), and the next hint hashes again.

// hintOutcome is the outcome attribute of crewlet.code.cache.hints — what one
// hint was answered — and of crewlet.code.cache.hint.hashes — what one hash
// decided, which is never gone.
type hintOutcome string

const (
	// hintConfirmed: the pack's bytes are not its row's.
	hintConfirmed hintOutcome = "confirmed"
	// hintSound: the pack's bytes are its row's.
	hintSound hintOutcome = "sound"
	// hintUnknown: no row names the pack, at the log's head.
	hintUnknown hintOutcome = "unknown"
	// hintUnread: no verdict — the rows or the pack could not be read in
	// time (no read slot within SlotWait, past its bound, a read failure),
	// or the pack's files moved under the hash.
	hintUnread hintOutcome = "unread"
	// hintGone: the hint's caller left before the verdict; the hash went on.
	hintGone hintOutcome = "gone"
)

// ConfirmHint holds the pack h nominates — a pack of s's incarnation, by
// its checksum — to the row that names it, and answers [RebuildCorrupt]
// when its bytes are not the row's, and no repair otherwise: the pack sound,
// no row naming it, no verdict reached, s not this cache's, or ctx ended
// first (the hash goes on, for the next hint). See the paragraphs above for
// what a hash costs and how long it is kept.
func (c *Cache) ConfirmHint(ctx context.Context, s Synced, h gitproc.Hint) (RebuildReason, bool) {
	if c.handedOut(s, "a hint's confirmation") != nil {
		return "", false
	}
	return c.confirmHint(ctx, s.entry, s.inc, h, nil)
}

// hinted is a round's own git process, in inc, failing with hints and no
// damage its formats name (gitproc.Hint): what converge answers so Cache.run
// can confirm them once the round has let the entry's write lock go and
// stopped being its running round. rows is
// the round's snapshot when it was a full read — every pack row, for the
// confirmation to look the hint's pack up in — and nil otherwise. It reads as
// the git failure it wraps.
type hinted struct {
	inc   *incarnation
	name  string
	full  bool
	rows  *snapshot
	hints []gitproc.Hint
	err   error
}

func (h *hinted) Error() string { return h.err.Error() }

// Unwrap is the git failure.
func (h *hinted) Unwrap() error { return h.err }

// hintsOf is the hints err's git run recorded, if err is one.
func hintsOf(err error) []gitproc.Hint {
	var exit *gitproc.ExitError
	if !errors.As(err, &exit) {
		return nil
	}
	return exit.Result.Trace.Hints
}

// confirmRound confirms a round's hints (hinted), the entry's write lock
// already let go and the round no longer the running one (Cache.run): the
// first whose pack is not its row's makes the round's answer the damage it
// asks repaired (damaged, RebuildCorrupt); with none, the round's answer is
// git's failure.
func (c *Cache) confirmRound(e *entry, h *hinted) error {
	for _, hint := range h.hints {
		if reason, ok := c.confirmHint(c.life, e, h.inc, hint, h.rows); ok {
			return &damaged{inc: h.inc, name: h.name, reason: reason, swept: h.full, err: h.err}
		}
	}
	return h.err
}

// packCheck is one hash of a hint's pack against its row
// (incarnation.packChecks). done closes when the hash ends; verdict is its
// outcome, set before done closes and read only after. id is the pack's
// files as the hash found them as it began: its verdict holds for those
// files and no others.
type packCheck struct {
	done    chan struct{}
	id      bundleID
	verdict hintOutcome
}

// confirmHint is ConfirmHint's body over inc: it waits, for as long as ctx,
// for the verdict on h's pack — one reached on the pack's present files, a
// hash of them in flight, or one it starts (packCheck) — and answers it.
// rows is a full read of inc's rows the caller holds, or nil.
func (c *Cache) confirmHint(ctx context.Context, e *entry, inc *incarnation, h gitproc.Hint,
	rows *snapshot) (RebuildReason, bool) {

	//nolint:contextcheck // The hash runs on the cache's lifetime, never ctx's: a caller that leaves must leave it running for the next (see the file's doc).
	check := c.packCheck(e, inc, h, rows)
	if check == nil {
		// The cache is stopping: no hash starts, and nothing is repaired.
		c.countHint(hintUnread)
		return "", false
	}
	select {
	case <-check.done:
	case <-ctx.Done():
		c.countHint(hintGone)
		c.log.Debug("code_cache_hint_gone", "repo", e.id, "pack", h.Pack, "error", context.Cause(ctx))
		return "", false
	}
	c.countHint(check.verdict)
	if check.verdict == hintConfirmed {
		return RebuildCorrupt, true
	}
	return "", false
}

// packCheck is the check whose verdict answers a hint about h's pack in inc:
// the one decided on the pack's present files, the one in flight, or — for
// none, or one that decided nothing or whose files have since been replaced —
// a new one, its hash started on the cache's lifetime with inc pinned. nil
// when the cache is stopping.
func (c *Cache) packCheck(e *entry, inc *incarnation, h gitproc.Hint, rows *snapshot) *packCheck {
	stem := filepath.Join(inc.dir, "objects", "pack", "pack-"+h.Pack)
	id := identify(stem)
	e.mu.Lock()
	defer e.mu.Unlock()
	if check := inc.packChecks[h.Pack]; check != nil {
		select {
		case <-check.done:
			if check.verdict != hintUnread && check.id == id {
				return check
			}
		default:
			// In flight: its verdict is the one this hint waits for. Files
			// that moved under it make it decide nothing, and the hint
			// after asks again.
			return check
		}
	}
	if !c.enter() {
		return nil
	}
	check := &packCheck{done: make(chan struct{}), id: id}
	if inc.packChecks == nil {
		inc.packChecks = map[string]*packCheck{}
	}
	inc.packChecks[h.Pack] = check
	pinned := c.pin(e, inc, SyncNone)
	go func() {
		defer c.leave()
		defer pinned.Release()
		check.verdict = c.hashPack(e, inc, h, rows, stem, id)
		// Counted before it is answered: a waiter woken by the verdict
		// reads a counter that already holds it.
		if c.metrics != nil {
			c.metrics.Add(metrics.CodeCacheHintHashes, 1, metrics.Attrs{"outcome": string(check.verdict)})
		}
		close(check.done)
	}()
	return check
}

// hashPack holds the pack h names, whose files are stem's, to its row, on the
// cache's lifetime and within HashBudget of the bundle's bytes counted from
// its start, and answers the verdict: sound or confirmed when the hash ran to
// its end on the files id names, unknown when no row names the pack, and
// unread when it could not decide.
func (c *Cache) hashPack(e *entry, inc *incarnation, h gitproc.Hint, rows *snapshot, stem string,
	id bundleID) hintOutcome {

	start := time.Now()
	row, found, err := c.packRow(e, inc, h.Pack, rows)
	switch {
	case err != nil:
		c.log.Info("code_cache_hint_unread", "repo", e.id, "pack", h.Pack, "error", err)
		return hintUnread
	case !found:
		c.log.Info("code_cache_hint_unknown", "repo", e.id, "pack", h.Pack, "object", h.Object)
		return hintUnknown
	}
	ref := row.Ref()
	bound := HashBudget(ref.PackBytes() + ref.IndexBytes + ref.ReverseBytes)
	hold := c.git.HoldRead
	if c.hashHook != nil {
		// The suite's: told the bound once the hash holds its first slot.
		var once sync.Once
		hold = func(ctx context.Context) (func(), error) {
			release, held := c.git.HoldRead(ctx)
			if held == nil {
				once.Do(func() { c.hashHook(h.Pack, bound) })
			}
			return release, held
		}
	}
	ctx, cancel := context.WithDeadline(c.life, start.Add(bound))
	defer cancel()
	sum, err := hashBundle(ctx, stem, make([]byte, copyBuffer), hold)
	switch {
	case ctx.Err() != nil || errors.Is(err, gitproc.ErrBusy):
		// Past its bound, the cache stopping, or no read slot free: no
		// verdict, and the next hint hashes again.
		c.log.Info("code_cache_hint_unread", "repo", e.id, "pack", h.Pack, "bound", bound, "error", err)
		return hintUnread
	case identify(stem) != id:
		// Installed again or swept while it was hashed: what was read is
		// no file the copy holds.
		c.log.Info("code_cache_hint_unread", "repo", e.id, "pack", h.Pack,
			"error", "the pack's files moved while they were hashed")
		return hintUnread
	case err == nil && sum == row.Hash:
		return hintSound
	}
	// Not the row's bytes — or not readable at all (a read the disk
	// refused): either way the copy does not hold the pack its row names.
	c.log.Warn("code_cache_hint_confirmed", "repo", e.id, "pack", h.Pack, "object", h.Object, "hashed", string(sum),
		"row", string(row.Hash), "error", err)
	return hintConfirmed
}

// packRow is the row naming pack among inc's rows: from rows when the caller
// holds a full read of them, else from the PACKS of a full snapshot read at
// session from inc's position (readPacks) — never every ref of a repository
// that may hold a hundred thousand, which the rows' pack phase comes before.
// A pack neither names is asked once more of the log's HEAD, linearizable:
// an adopted pack's row may be past what the round or the copy that answered
// had applied, and "no row names it" is a verdict kept with the pack's files
// (packCheck), so it is the head's answer and never a lagging copy's. Bounded
// by one read attempt (gitproc.ReadDeadline), HashBudget's term for it.
func (c *Cache) packRow(e *entry, inc *incarnation, pack string, rows *snapshot) (code.PackRow, bool, error) {
	ctx, cancel := context.WithTimeout(c.life, gitproc.ReadDeadline)
	defer cancel()
	find := func(s snapshot) (code.PackRow, bool) {
		for _, p := range s.packs {
			if p.GitChecksum == pack {
				return p, true
			}
		}
		return code.PackRow{}, false
	}
	e.mu.Lock()
	at := inc.at
	e.mu.Unlock()
	switch {
	case rows != nil:
		if row, ok := find(*rows); ok {
			return row, true, nil
		}
	case !at.IsZero():
		snap, err := c.readPacks(ctx, e.id, statelog.Freshness{Level: statelog.ReadSession, MinPosition: at})
		if err != nil {
			return code.PackRow{}, false, err
		}
		if row, ok := find(snap); ok {
			return row, true, nil
		}
	}
	snap, err := c.readPacks(ctx, e.id, statelog.Freshness{Level: statelog.ReadLinearizable})
	if err != nil {
		return code.PackRow{}, false, err
	}
	row, ok := find(snap)
	return row, ok, nil
}

// countHint counts one hint's outcome.
func (c *Cache) countHint(o hintOutcome) {
	if c.metrics != nil {
		c.metrics.Add(metrics.CodeCacheHints, 1, metrics.Attrs{"outcome": string(o)})
	}
}

// bundleID is a pack's three files' identities: its .pack, .idx and .rev.
type bundleID [3]fileID

// identify is the identity of stem's .pack, .idx and .rev as they are now.
func identify(stem string) bundleID {
	var id bundleID
	for i, ext := range []string{".pack", ".idx", ".rev"} {
		id[i] = lstatID(stem + ext)
	}
	return id
}
