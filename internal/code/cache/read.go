package cache

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/semaphore"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE READ: one complete snapshot of a repository's rows, every page applied
// — the target a round converges to. The snapshot contract is the code
// domain's (code.Reader.Snapshot): page 1 decides full or delta and fixes the
// position the WHOLE snapshot is complete as of; later pages read at session
// from the cursor, possibly on other nodes; a snapshot of more than one page
// ends with a TAIL of the packs added since page 1; and a later page that
// finds the history moved under it answers code.ErrSnapshotMoved, after which
// the snapshot is read again from page 1. Every row is validated here, before
// anything is written: a ref goes into a file git parses line by line.

// snapshot is one complete answer, every page applied.
type snapshot struct {
	full bool
	// position is page 1's: what the WHOLE snapshot is complete as of.
	position      statelog.Position
	streamCreated time.Time
	// repo is the LAST page's row: the freshest settings.
	repo code.RepoRef
	// packs is a SET keyed by (pack id, part), because the tail re-sends
	// rows an earlier page served.
	packs map[packKey]code.PackRow
	// refs is in answer order; a full snapshot's are sorted by ref before
	// use. In a delta, a ref moved between two pages is served once at each
	// position, and the later row wins.
	refs []code.RefRow
	// behind is an answer on the incarnation's own stream BELOW its
	// position. What that means is the level's to say (converge): below a
	// linearizable read it is a log gone back, and below any other a copy
	// that has not caught up.
	behind bool
	// restreamed: page 1 was a delta keyed to another stream than the
	// incarnation's, and this is the whole answer read again.
	restreamed bool
	// since is a delta's floor — what it is a delta from — and pushes its
	// pushes phase, where page 1 was asked for it (asked): every push
	// applied in (since, position], in (position, push id) order, each once
	// — what the round folds into the incarnation's index of pushes
	// (provenance.go), so an asked phase with no row is no push.
	since  statelog.Position
	pushes []code.IntroducedRow
	asked  bool
	// retiredSince is page 1's StateQuery.RetiredSince: the floor a full
	// answer was asked to serve the retirements above (retiredFloor) — zero
	// where it asked for none. retiredFrom is the floor the answer's
	// retired phase is complete above, the same on every page: since on a
	// delta, retiredSince on a full answer that honoured it, ZERO on one
	// that served none — a round then reads every pack file no row names
	// as one no row accounts for (swap.go). retired is that phase's rows,
	// in (position, pack id, part) order, each once: what a round takes out
	// of the incarnation's live set.
	retiredSince, retiredFrom statelog.Position
	retired                   []code.RetiredRow
	// layers is the repository's published chain as of position, root first
	// (code.RepoSnapshot.Layers), and layersServed whether page 1 served it:
	// on every full answer, and on a delta whose chain moved above its floor.
	// A copy replaces the chain it records only when it is served.
	layers       []code.LayerRow
	layersServed bool
}

// retiredChecksums is the checksums s's retired phase names, as a set: what
// a full round reads a stray by (swap.go's strays).
func (s snapshot) retiredChecksums() map[string]bool {
	out := make(map[string]bool, len(s.retired))
	for _, r := range s.retired {
		out[r.GitChecksum] = true
	}
	return out
}

// kind is what reading s cost.
func (s snapshot) kind() SyncKind {
	if s.full {
		return SyncFull
	}
	return SyncDelta
}

// packKey is a pack row's key.
type packKey struct{ packID, part string }

// logMark is the code log AS THIS PROCESS HAS SEEN IT ANSWERED: the stream
// identity of the last answer and the highest position any page 1 answered
// at, for every repository. One mark for the node, because every repository's
// rows are one log: a log recreated or gone back under one repository's
// incarnation has done so under every one, and the first round of any of them
// to see it is the one that tells the rest (Cache.reread).
//
// A position the mark holds was one the log held when it was answered, so a
// LINEARIZABLE answer read after it — whose barrier is appended at the log's
// head — is at or past it on a log that never goes back. A round compares its
// answer with the mark AS IT STOOD BEFORE ITS READ BEGAN: a position another
// round recorded after this one's barrier is no statement about it.
type logMark struct {
	mu     sync.Mutex
	stream time.Time
	at     statelog.Position
}

// read is the mark as it stands.
func (m *logMark) read() (time.Time, statelog.Position) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stream, m.at
}

// saw records a page 1's answer — the mark only rises on its stream — and
// reports whether it is on another stream than the mark, the log recreated:
// the mark starts again from it.
func (m *logMark) saw(stream time.Time, at statelog.Position) (recreated bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.stream.IsZero():
		m.stream, m.at = stream, at
	case !m.stream.Equal(stream):
		m.stream, m.at = stream, at
		return true
	case !reached(m.at, at):
		m.at = at
	}
	return false
}

// set puts the mark at an answer that proved the log went back.
func (m *logMark) set(stream time.Time, at statelog.Position) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stream, m.at = stream, at
}

// read is the snapshot of id an incarnation at at, on the stream made at
// stream, converges to: full, or a delta from at, at fresh — a delta asked for
// its pushes phase where pushes says the incarnation keeps an index of pushes
// to fold them into (provenance.go). retired is the floor a full answer is
// asked to serve the retirements above (StateQuery.RetiredSince), zero for
// none: a round's is retiredFloor, and a check, which reads no retirement,
// asks none.
//
//  1. Page 1 asks from at (zero when full), and a full answer for the
//     retirements above retired.
//  2. A page-1 DELTA keyed to another stream than the incarnation's is the
//     log RECREATED under the cache — provisioned anew, its stamp not the
//     one the cache read under (a snapshot restore keeps it, ADR-0076) —
//     which Since cannot tell the reader: asked again from nothing, once
//     (restreamed), and for no retirements, since at is a position of a
//     log that is gone.
//  3. An answer on the incarnation's own stream below at is BEHIND; what that
//     means is the round's to decide by the level it read at (converge).
//  4. Next is followed to its end; a page that answers code.ErrSnapshotMoved
//     starts the read again from page 1 with the SAME Since, at most
//     MaxSnapshotRestarts times — page 1 decides full afresh wherever a sweep
//     or a reanchor moved under a delta.
//  5. Every page is validated (snapshot.add) — ErrBadSnapshot, naming the
//     page and the row.
func (c *Cache) read(ctx context.Context, id string, at, retired statelog.Position, stream time.Time, full,
	pushes bool, fresh statelog.Freshness) (snapshot, error) {

	since, retiredSince := at, retired
	if full {
		since = statelog.Position{}
	}
	restarts, restreamed := 0, false
	for {
		snap, err := c.readOnce(ctx, id, since, retiredSince, fresh, pushes && !since.IsZero(), false)
		switch {
		case errors.Is(err, code.ErrSnapshotMoved):
			if restarts == MaxSnapshotRestarts {
				// The log moving under every read is the log not read.
				return snapshot{}, fmt.Errorf("%w: %s's history moved under %d reads of its snapshot: %w",
					ErrLogRead, id, restarts+1, err)
			}
			restarts++
			c.log.Info("code_sync_restarted", "repo", id, "restart", restarts, "full", since.IsZero())
			continue
		case err != nil:
			return snapshot{}, err
		case !snap.full && !restreamed && !snap.streamCreated.Equal(stream):
			restreamed = true
			c.log.Info("code_cache_stream_changed", "repo", id, "was", stream, "now", snap.streamCreated)
			since, retiredSince = statelog.Position{}, statelog.Position{}
			continue
		case snap.streamCreated.Equal(stream) && !reached(snap.position, at):
			snap.behind = true
		}
		snap.restreamed = restreamed
		return snap, nil
	}
}

// retiredFloor is the floor a round of an incarnation at at asks a full
// answer for the retirements above (read's retired), so a full round can tell
// a pack file a compaction retired from one no row accounts for (swap.go): at
// itself — what the full answer stands in for a delta from, a delta's being
// always those above its own floor, so page 1 weighs the two answers like with
// like — or leaving, the incarnation's leavingSince, where it is lower: the
// floor the retirements of the pack files it still holds are all above, which
// a crash or a failed swap left it holding past the position its round
// recorded. Zero for an incarnation at none.
func retiredFloor(at, leaving statelog.Position) statelog.Position {
	if below, err := leaving.Before(at); !leaving.IsZero() && err == nil && below {
		return leaving
	}
	return at
}

// fullRounds is the node's slots for FULL READS of a repository's rows
// against the estate (MaxFullRounds): every page of a full read is one read
// transaction on a data node — one of the answers its lane gives the whole
// fleet's reads at once, and one of the reader connections every one of
// those reads takes too — so a node coming up with hundreds of repositories
// asked of it holds at most this many of them, however many it was asked
// for. Granted first come, first served (semaphore.Weighted), so no
// repository's read waits behind one that asked after it.
type fullRounds struct {
	slots *semaphore.Weighted
	size  int64
	rec   *metrics.Recorder

	// mu orders every change of the counts with the gauge's setting of
	// them, so the gauge never holds a count a later change superseded:
	// running is the slots held now, most the most held at once, and
	// waiting the reads waiting for one — what the gauge publishes and the
	// suite holds to the bound.
	mu                     sync.Mutex
	running, most, waiting int64
}

// The states crewlet.code.cache.full_rounds counts a full read in.
const (
	fullRunning = "running"
	fullWaiting = "waiting"
)

// newFullRounds is size slots, reported to rec — both states at zero from
// the start, so a node's panel shows its full reads before its first.
func newFullRounds(size int64, rec *metrics.Recorder) *fullRounds {
	f := &fullRounds{slots: semaphore.NewWeighted(size), size: size, rec: rec}
	f.count(0, 0)
	return f
}

// hold waits for a slot under ctx: the cache's lifetime for a round's read,
// whose callers each wait under their own bound (MaxFullRounds, THE WAIT IS
// THE CALLER'S), and a check's or a hint's own bound for theirs. A free slot
// nobody waits for is taken without counting a wait.
func (f *fullRounds) hold(ctx context.Context) error {
	if f.slots.TryAcquire(1) {
		f.count(1, 0)
		return nil
	}
	f.count(0, 1)
	if err := f.slots.Acquire(ctx, 1); err != nil {
		f.count(0, -1)
		return err
	}
	f.count(1, -1)
	return nil
}

// letGo gives a slot back.
func (f *fullRounds) letGo() {
	f.count(-1, 0)
	f.slots.Release(1)
}

// count moves the counts by what changed and sets the gauge to them.
func (f *fullRounds) count(running, waiting int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running += running
	f.waiting += waiting
	f.most = max(f.most, f.running)
	if f.rec != nil {
		f.rec.Set(metrics.CodeCacheFullRounds, float64(f.running), metrics.Attrs{"state": fullRunning})
		f.rec.Set(metrics.CodeCacheFullRounds, float64(f.waiting), metrics.Attrs{"state": fullWaiting})
	}
}

// holdFull waits for one of the node's full-read slots for id's read: a
// cache stopping while it waits is ErrStopped, as every sync of a stopped
// cache is, and the read's own bound ending first says what it waited for.
func (c *Cache) holdFull(ctx context.Context, id string) error {
	err := c.full.hold(ctx)
	switch {
	case err == nil:
		return nil
	case c.isStopped():
		return ErrStopped
	}
	return fmt.Errorf("cache: %s's full read stopped waiting for one of the %d this node reads at once: %w", id,
		c.full.size, err)
}

// logRead is the estate's failure to answer a page, marked as the code log's
// (ErrLogRead) — but code.ErrRepoNotFound, which is the log's ANSWER that no
// row holds the repository, and code.ErrSnapshotMoved, its answer that the
// history moved under the read, which read gives a fresh start; each is
// passed on as itself.
func logRead(err error) error {
	if errors.Is(err, code.ErrRepoNotFound) || errors.Is(err, code.ErrSnapshotMoved) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrLogRead, err)
}

// readPacks reads the PACK ROWS of a full snapshot of id at fresh — what a
// hint's confirmation looks a pack up in (hint.go): the snapshot's pages
// until the first that serves a ref, or its last. A full snapshot serves
// every pack before any ref (code.Reader.Snapshot's phases), so its packs as
// of page 1's position are whole there, and a repository of a hundred
// thousand refs costs it the pages of its packs rather than every ref's. Read
// again from page 1 when the history moves under it, as read is.
func (c *Cache) readPacks(ctx context.Context, id string, fresh statelog.Freshness) (snapshot, error) {
	for restarts := 0; ; restarts++ {
		snap, err := c.readOnce(ctx, id, statelog.Position{}, statelog.Position{}, fresh, false, true)
		if !errors.Is(err, code.ErrSnapshotMoved) || restarts == MaxSnapshotRestarts {
			return snap, err
		}
	}
}

// readOnce reads every page of one snapshot — a delta asked for its pushes
// phase where pushes says so, a full one asked for the retirements above
// retiredSince, or, packsOnly, a full one's pages up to the first that serves
// a ref (readPacks) — or answers the first page's failure as it is: the
// estate's, as ErrLogRead (logRead). Every page the estate answers is timed
// (pageAnswered).
//
// A FULL READ HOLDS ONE OF THE NODE'S SLOTS (fullRounds, MaxFullRounds) from
// the page it first knows it is full to its last: its first, asked from
// nothing; its second, when the log answers a delta in full, since page 1
// decides and was asked as a delta's. A delta's pages hold none.
func (c *Cache) readOnce(ctx context.Context, id string, since, retiredSince statelog.Position,
	fresh statelog.Freshness, pushes, packsOnly bool) (snapshot, error) {

	held := false
	defer func() {
		if held {
			c.full.letGo()
		}
	}()
	if since.IsZero() {
		if err := c.holdFull(ctx, id); err != nil {
			return snapshot{}, err
		}
		held = true
	}
	asked := time.Now()
	page, err := c.fetch.RepoState(ctx, code.StateQuery{RepoID: id, Since: since, Fresh: fresh, Pushes: pushes,
		RetiredSince: retiredSince})
	if err != nil {
		return snapshot{}, logRead(err)
	}
	c.pageAnswered(page, time.Since(asked))
	if packsOnly && !page.Full {
		return snapshot{}, fmt.Errorf("%w: %s's snapshot from nothing answered a delta", ErrBadSnapshot, id)
	}
	snap := snapshot{
		full: page.Full, position: page.Position, streamCreated: page.StreamCreated,
		packs: map[packKey]code.PackRow{}, since: since, asked: pushes && !page.Full,
		retiredSince: retiredSince, retiredFrom: page.RetiredFrom,
	}
	seen := map[string]int{}
	for n := 1; ; n++ {
		if err = snap.add(id, n, page, seen); err != nil {
			return snapshot{}, err
		}
		if page.Next == nil || packsOnly && len(page.Refs) > 0 {
			break
		}
		if snap.full && !held {
			// A DELTA THE LOG ANSWERED IN FULL: every page from here is a
			// whole repository's, as a full round's are.
			if err = c.holdFull(ctx, id); err != nil {
				return snapshot{}, err
			}
			held = true
		}
		next := code.StateQuery{RepoID: id, Fresh: fresh, Cursor: page.Next}
		asked = time.Now()
		if page, err = c.fetch.RepoState(ctx, next); err != nil {
			return snapshot{}, logRead(err)
		}
		c.pageAnswered(page, time.Since(asked))
	}
	if snap.full {
		slices.SortFunc(snap.refs, func(a, b code.RefRow) int { return strings.Compare(a.Ref, b.Ref) })
	}
	return snap, nil
}

// pageAnswered records how long one page of a snapshot took the estate to
// answer, from the ask to the answer — the data node's lane, its reader and
// its scan, and the crossing; on a node that holds the data, its own store's
// — by what the page served, a whole repository's or what moved since a
// position: crewlet.code.cache.read.page.duration, production's reading of
// the page MaxFullRounds was measured against. A page refused is no page
// served, and is counted nowhere here.
func (c *Cache) pageAnswered(page code.RepoSnapshot, took time.Duration) {
	if c.metrics == nil {
		return
	}
	kind := SyncDelta
	if page.Full {
		kind = SyncFull
	}
	c.metrics.Observe(metrics.CodeCacheReadPageDuration, took, metrics.Attrs{"kind": string(kind)})
}

// add validates page n of the snapshot and takes its rows in. seen is each
// ref's index in snap.refs.
func (s *snapshot) add(id string, n int, page code.RepoSnapshot, seen map[string]int) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s's snapshot, page %d: "+format,
			append([]any{ErrBadSnapshot, id, n}, args...)...)
	}
	switch {
	case page.Repo.ID != id:
		return bad("it answers for repository %q", page.Repo.ID)
	case page.Position.Stream != topics.CodeLogStream || page.Position.Valid() != nil:
		return bad("its position %s is not one on %s", page.Position, topics.CodeLogStream)
	case page.Full != s.full || page.Position != s.position || !page.StreamCreated.Equal(s.streamCreated):
		return bad("it answers a snapshot (full %t at %s of the stream made %s) that page 1 (full %t at %s of %s) "+
			"did not", page.Full, page.Position, page.StreamCreated, s.full, s.position, s.streamCreated)
	case !gitref.ValidBranch(page.Repo.DefaultBranch):
		return bad("default branch %q is not a branch name", page.Repo.DefaultBranch)
	case page.RetiredFrom != s.retiredFrom:
		return bad("it serves the retirements above %s, and page 1 those above %s", page.RetiredFrom, s.retiredFrom)
	case !s.full && s.retiredFrom != s.since:
		// A delta's retirements are every one above its own floor: what it
		// takes out of the incarnation it is a delta of.
		return bad("a delta from %s serves the retirements above %s", s.since, s.retiredFrom)
	case s.full && !s.retiredFrom.IsZero() && s.retiredFrom != s.retiredSince:
		return bad("a full answer asked for the retirements above %s serves those above %s", s.retiredSince,
			s.retiredFrom)
	case n > 1 && (page.LayersServed || len(page.Layers) > 0):
		// The chain is the snapshot's first phase, read in page 1's own
		// transaction and never continued.
		return bad("it serves the published chain, which page 1 alone serves")
	case n == 1 && s.full && !page.LayersServed:
		return bad("a full answer serves no published chain, which every full answer serves, an empty one included")
	case n == 1 && !page.LayersServed && len(page.Layers) > 0:
		return bad("it serves %d layers of a published chain it says it does not serve", len(page.Layers))
	}
	if n == 1 && page.LayersServed {
		if err := s.addLayers(page.Layers); err != nil {
			return bad("%v", err)
		}
	}
	for _, p := range page.Packs {
		if err := checkPack(p); err != nil {
			return bad("pack %s/%s: %v", p.PackID, p.Part, err)
		}
		k := packKey{p.PackID, p.Part}
		if had, ok := s.packs[k]; ok && had != p {
			return bad("pack %s/%s is served twice, naming objects %s and %s", p.PackID, p.Part, had.Object, p.Object)
		}
		s.packs[k] = p
	}
	for _, r := range page.Refs {
		switch {
		case !gitref.ValidRef(r.Ref):
			return bad("ref %q is not a ref", r.Ref)
		case r.OID == "" && s.full:
			return bad("ref %s names no object in a full snapshot", r.Ref)
		case r.OID != "" && (!gitref.ValidOID(r.OID) || r.OID == gitref.ZeroOID):
			return bad("ref %s names %q, which is not an object id", r.Ref, r.OID)
		}
		i, twice := seen[r.Ref]
		switch {
		case !twice:
			seen[r.Ref] = len(s.refs)
			s.refs = append(s.refs, r)
		case s.full:
			return bad("ref %s is served twice", r.Ref)
		default:
			// A DELTA serves a ref again when it moved between two pages
			// (its row's position passed the cursor); the later row is the
			// newer one, and two rows at one position are no history.
			older, err := s.refs[i].Position.Before(r.Position)
			if err != nil || !older {
				return bad("ref %s is served twice at %s and %s", r.Ref, s.refs[i].Position, r.Position)
			}
			s.refs[i] = r
		}
	}
	for _, p := range page.Pushes {
		if err := s.addPush(p); err != nil {
			return bad("%v", err)
		}
	}
	for _, r := range page.Retired {
		if err := s.addRetired(r); err != nil {
			return bad("%v", err)
		}
	}
	s.repo = page.Repo
	return nil
}

// addLayers validates the published chain page 1 served and takes it in, as
// the domain keeps it (code.LayerRow): at most code.MaxPublishedLayers rows,
// each a layer every git node may hold to its files (code.LayerRef.Check) and
// none a replacement; Seq 1 up, one by one; the root over no base and every
// other layer over the one below it — a layer is valid only over its exact
// base, and the chain's order is the only thing that names it; no layer, and
// no commit-graph layer, twice; no pack in two layers, since each covers what
// none below it does; and every layer added at a position on the code log at
// or below the snapshot's. Whether the copy holds a layer's packs is the
// install's to decide (installable), never the read's: a chain over a pack it
// lacks serves from the prefix below.
func (s *snapshot) addLayers(rows []code.LayerRow) error {
	if len(rows) > code.MaxPublishedLayers {
		return fmt.Errorf("its published chain holds %d layers, past the %d a chain holds (code.MaxPublishedLayers)",
			len(rows), code.MaxPublishedLayers)
	}
	ids, graphs, packs := map[string]bool{}, map[string]bool{}, map[string]string{}
	for i, r := range rows {
		l := r.Layer
		if err := l.Check(); err != nil {
			return fmt.Errorf("layer %d of its published chain: %w", r.Seq, err)
		}
		switch {
		case r.Seq != i+1:
			return fmt.Errorf("layer %s is layer %d of its published chain, served as layer %d", l.LayerID, i+1,
				r.Seq)
		case len(l.Replaces) > 0:
			return fmt.Errorf("layer %d (%s) names the layers it replaces, which a published row never does", r.Seq,
				l.LayerID)
		case i == 0 && l.BaseID != "":
			return fmt.Errorf("the root of its published chain, %s, names a base %s", l.LayerID, l.BaseID)
		case i > 0 && l.BaseID != rows[i-1].Layer.LayerID:
			return fmt.Errorf("layer %d (%s) is over %q and the layer below it is %s", r.Seq, l.LayerID, l.BaseID,
				rows[i-1].Layer.LayerID)
		case ids[l.LayerID]:
			return fmt.Errorf("layer %s is in its published chain twice", l.LayerID)
		case l.GraphID != "" && graphs[l.GraphID]:
			return fmt.Errorf("commit-graph layer %s is in its published chain twice", l.GraphID)
		case r.Added.Stream != topics.CodeLogStream || r.Added.Valid() != nil:
			return fmt.Errorf("layer %d (%s) was added at %s, which is no position on %s", r.Seq, l.LayerID,
				r.Added, topics.CodeLogStream)
		case r.Added.Packed() > s.position.Packed():
			return fmt.Errorf("layer %d (%s) was added at %s, past the snapshot's %s", r.Seq, l.LayerID, r.Added,
				s.position)
		}
		ids[l.LayerID] = true
		if l.GraphID != "" {
			graphs[l.GraphID] = true
		}
		for _, cs := range l.Packs {
			if other, twice := packs[cs]; twice {
				return fmt.Errorf("pack-%s is covered by layers %s and %s of its published chain", cs, other,
					l.LayerID)
			}
			packs[cs] = l.LayerID
		}
	}
	s.layers, s.layersServed = slices.Clone(rows), true
	return nil
}

// addRetired validates one row of the snapshot's retired phase and takes it
// in: the retirement of a pack named by a checksum its files can carry, at a
// position on the code log in (retiredFrom, position], after every row before
// it in (position, pack id, part) order — so each is served once. A full
// answer that serves no retirements serves no row of them either.
func (s *snapshot) addRetired(r code.RetiredRow) error {
	switch {
	case s.retiredFrom.IsZero():
		return fmt.Errorf("pack %s/%s is served retired by a full answer that serves no retirements", r.PackID,
			r.Part)
	case !packChecksum().MatchString(r.GitChecksum):
		return fmt.Errorf("retired pack %s/%s names %q, which is no pack checksum", r.PackID, r.Part,
			r.GitChecksum)
	case r.Position.Stream != topics.CodeLogStream || r.Position.Valid() != nil:
		return fmt.Errorf("pack %s/%s was retired at %s, which is no position on %s", r.PackID, r.Part, r.Position,
			topics.CodeLogStream)
	case r.Position.Packed() <= s.retiredFrom.Packed() || r.Position.Packed() > s.position.Packed():
		return fmt.Errorf("pack %s/%s was retired at %s, outside the retired phase's (%s, %s]", r.PackID, r.Part,
			r.Position, s.retiredFrom, s.position)
	}
	if n := len(s.retired); n > 0 {
		last := s.retired[n-1]
		if cmp.Or(cmp.Compare(last.Position.Packed(), r.Position.Packed()), strings.Compare(last.PackID, r.PackID),
			strings.Compare(last.Part, r.Part)) >= 0 {
			return fmt.Errorf("pack %s/%s retired at %s is served after pack %s/%s retired at %s", r.PackID, r.Part,
				r.Position, last.PackID, last.Part, last.Position)
		}
	}
	s.retired = append(s.retired, r)
	return nil
}

// addPush validates one row of a delta's pushes phase and takes it in: a
// push the delta asked for, at a position on the code log in (since,
// position], after every row before it in (position, push id) order — so
// each push is served once — whose moves name refs and object ids and whose
// commits are object ids: what a run of the index of pushes encodes
// (provrun.go), held to it before anything is written.
func (s *snapshot) addPush(p code.IntroducedRow) error {
	switch {
	case !s.asked:
		return fmt.Errorf("push %s is served by a snapshot that asked for no pushes phase", p.PushID)
	case p.Position.Stream != topics.CodeLogStream || p.Position.Valid() != nil:
		return fmt.Errorf("push %s is at %s, which is no position on %s", p.PushID, p.Position,
			topics.CodeLogStream)
	case p.Position.Packed() <= s.since.Packed() || p.Position.Packed() > s.position.Packed():
		return fmt.Errorf("push %s is at %s, outside the delta's (%s, %s]", p.PushID, p.Position, s.since,
			s.position)
	}
	if n := len(s.pushes); n > 0 {
		last := s.pushes[n-1]
		if c := cmp.Compare(last.Position.Packed(), p.Position.Packed()); c > 0 ||
			c == 0 && last.PushID >= p.PushID {
			return fmt.Errorf("push %s at %s is served after push %s at %s", p.PushID, p.Position, last.PushID,
				last.Position)
		}
	}
	if err := validPush(p); err != nil {
		return err
	}
	s.pushes = append(s.pushes, p)
	return nil
}

// validPush holds one push as the rows served it to what a run of the index
// of pushes encodes (provrun.go) — its id a canonical UUID, its moves refs
// between object ids, its commits object ids — before anything is written of
// it: a delta's pushes phase (addPush) and the backfill's pages alike.
func validPush(p code.IntroducedRow) error {
	if id, err := uuid.Parse(p.PushID); err != nil || id.String() != p.PushID {
		return fmt.Errorf("push id %q is not a canonical UUID", p.PushID)
	}
	for _, m := range p.Moves {
		switch {
		case !gitref.ValidRef(m.Ref):
			return fmt.Errorf("push %s moved %q, which is not a ref", p.PushID, m.Ref)
		case !gitref.ValidOID(m.Old) || !gitref.ValidOID(m.New):
			return fmt.Errorf("push %s moved %s from %q to %q, which are not object ids", p.PushID, m.Ref, m.Old,
				m.New)
		}
	}
	for _, oid := range p.Introduced {
		if !gitref.ValidOID(oid) || oid == gitref.ZeroOID {
			return fmt.Errorf("push %s introduced %q, which is not an object id", p.PushID, oid)
		}
	}
	return nil
}

// checkPack is a pack row this build can install, decided before any GET:
// the one part this build splits, and the row-level half of the one bundle
// rule (code.PackRef.Check) — the very rule the domain admitted the row by,
// so a row no git node can install is one no proposal was ever published
// with, rather than one this copy alone refuses on every git node for good.
func checkPack(p code.PackRow) error {
	switch {
	case p.Part != "pack":
		// A newer peer's new part is not a bundle this build can install.
		return fmt.Errorf("part %q is not one this build installs", p.Part)
	case !p.State.Valid():
		// Nor is a state it cannot place (packPlace): a pack it put in
		// objects/pack would be one a push may rely on, whatever the state
		// says of it.
		return fmt.Errorf("state %q is not one this build places", p.State)
	}
	return p.Ref().Check()
}
