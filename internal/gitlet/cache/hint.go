package cache

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A DAMAGE GIT NAMES ONLY IN WORDS is a hint (gitproc.Hint) and decides
// nothing until ConfirmHint hashes the pack it names against the row naming it
// (package doc, "Damage is repaired once, in place where nothing is lost").
// ONE HASH A PACK, on the cache's lifetime: the first hint starts it
// (packCheck), every hint waits as long as its caller does, and a caller cut
// mid-hash leaves it running. A round's hints are hashed after it has ENDED
// (Cache.run), so no linearizable caller waits; its callers are queued again
// if a rebuild replaces the incarnation meanwhile. A verdict holds while the
// pack's files are those it was reached on (bundleID; fileID omits the .pack's
// time, which git freshens). The hash is bounded by HashBudget, never
// CheckBudget, in a read slot a piece at a time; no slot within
// gitproc.SlotWait, or past its bound, decides nothing (unread).

// hintOutcome is the outcome attribute of crewlet.code.cache.hints (what one
// hint was answered) and crewlet.code.cache.hint.hashes (what one hash
// decided, never gone).
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

// ConfirmHint holds the pack h nominates, a pack of s's incarnation by its
// checksum, to the row that names it. When the bytes are not the row's it
// answers [RebuildCorrupt] naming the pack in Failure.Packs, which
// [Cache.Rebuild] installs again in place with one GET; otherwise no repair:
// the pack sound, no row naming it, no verdict, s not this cache's, or ctx
// ended first (the hash goes on for the next hint). The caller sets
// Failure.Began from the run that recorded h.
func (c *Cache) ConfirmHint(ctx context.Context, s Synced, h gitproc.Hint) (Failure, bool) {
	if c.handedOut(s, "a hint's confirmation") != nil {
		return Failure{}, false
	}
	if !c.confirmHint(ctx, s.entry, s.inc, h, nil) {
		return Failure{}, false
	}
	return Failure{Reason: RebuildCorrupt, Packs: []string{h.Pack}}, true
}

// hinted is a round's own git failure in inc carrying hints (gitproc.Hint),
// with either no damage its formats name (2.56's bare "%s") or a classified
// damage beside them (2.55's row of the same sentence, or another the run
// reported, as DamageReason reads it). converge answers it so Cache.run
// confirms the hints once the round has let the write lock go and ended. rows
// is the round's snapshot when it was a full read, else nil; forced, that it
// was a forced in-place round (damaged.forced). It reads as the git failure
// it wraps.
type hinted struct {
	inc        *incarnation
	name       string
	full       bool
	rows       *snapshot
	hints      []gitproc.Hint
	classified bool
	damage     Failure
	forced     bool
	err        error
}

func (h *hinted) Error() string { return h.err.Error() }

// began is when the round's git that recorded the hints began: its
// classified damage's (Failure.Began), else its run's (gitproc.Result.Began),
// zero when nothing says.
func (h *hinted) began() time.Time {
	if h.classified && !h.damage.Began.IsZero() {
		return h.damage.Began
	}
	var exit *gitproc.ExitError
	if errors.As(h.err, &exit) {
		return exit.Result.Began
	}
	return time.Time{}
}

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

// confirmRound confirms a round's hints once the round has let the write lock
// go and stopped running (Cache.run), each pack once however many of its
// objects git named. It answers the damaged repair naming the packs found not
// their rows' (damaged.packs), under the reason git classified, else
// RebuildCorrupt. With no pack confirmed and nothing classified it answers
// git's failure and repairs nothing: a message only nominates.
func (c *Cache) confirmRound(e *entry, h *hinted) error {
	var packs []string
	asked := map[string]bool{}
	for _, hint := range h.hints {
		if asked[hint.Pack] {
			continue
		}
		asked[hint.Pack] = true
		if c.confirmHint(c.life, e, h.inc, hint, h.rows) {
			packs = append(packs, hint.Pack)
		}
	}
	if len(packs) == 0 && !h.classified {
		return h.err
	}
	d := &damaged{inc: h.inc, name: h.name, reason: RebuildCorrupt, swept: h.full, packs: packs,
		forced: h.forced, began: h.began(), err: h.err}
	if h.classified {
		d.reason = h.damage.Reason
	}
	return d
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

// confirmHint is ConfirmHint's body over inc: it waits, as long as ctx, for
// the verdict on h's pack (packCheck) and answers whether it confirmed the
// pack damaged. rows is a full read of inc's rows the caller holds, or nil.
func (c *Cache) confirmHint(ctx context.Context, e *entry, inc *incarnation, h gitproc.Hint,
	rows *snapshot) bool {

	//nolint:contextcheck // The hash runs on the cache's lifetime, never ctx's: a caller that leaves must leave it running for the next (see the file's doc).
	check := c.packCheck(e, inc, h, rows)
	if check == nil {
		// The cache is stopping: no hash starts, and nothing is repaired.
		c.countHint(hintUnread)
		return false
	}
	select {
	case <-check.done:
	case <-ctx.Done():
		c.countHint(hintGone)
		c.log.Debug("code_cache_hint_gone", "repo", e.id, "pack", h.Pack, "error", context.Cause(ctx))
		return false
	}
	c.countHint(check.verdict)
	return check.verdict == hintConfirmed
}

// packCheck is the check whose verdict answers a hint about h's pack in inc:
// one decided on the pack's present files, one in flight, or else a new one,
// hashed on the cache's lifetime with inc pinned. nil when the cache is
// stopping.
func (c *Cache) packCheck(e *entry, inc *incarnation, h gitproc.Hint, rows *snapshot) *packCheck {
	// A serving process reads the dropping view too, so the pack may be there.
	e.mu.Lock()
	place := placeOf(inc, h.Pack)
	e.mu.Unlock()
	stem := filepath.Join(inc.dir, place.rel(), "pack-"+h.Pack)
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
	pinned := c.pin(e, inc)
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
// holds a full read, else from the PACKS of a full snapshot read at session
// from inc's position (readPacks; packs precede refs), never every ref. A pack
// neither names is asked once more of the log's head, linearizable: an
// adopted pack's row may be past what the copy had applied, and "no row
// names it" is a kept verdict (packCheck), so never a lagging copy's.
// Bounded by one read attempt ([RowReadAttempt]), HashBudget's term for it.
func (c *Cache) packRow(e *entry, inc *incarnation, pack string, rows *snapshot) (gitlet.PackRow, bool, error) {
	ctx, cancel := context.WithTimeout(c.life, RowReadAttempt)
	defer cancel()
	find := func(s snapshot) (gitlet.PackRow, bool) {
		for _, p := range s.packs {
			if p.GitChecksum == pack {
				return p, true
			}
		}
		return gitlet.PackRow{}, false
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
		snap, err := c.readPacks(ctx, e.id, statelog.FloorAt(statelog.ReadSession, at))
		if err != nil {
			return gitlet.PackRow{}, false, err
		}
		if row, ok := find(snap); ok {
			return row, true, nil
		}
	}
	snap, err := c.readPacks(ctx, e.id, statelog.Freshness{Level: statelog.ReadLinearizable})
	if err != nil {
		return gitlet.PackRow{}, false, err
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
