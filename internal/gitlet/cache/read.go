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

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE READ: one complete snapshot of a repository's rows, the target a round
// converges to, under the code domain's contract (gitlet.Reader.Snapshot):
// page 1 decides full or delta and fixes the position the whole snapshot is
// complete as of; a multi-page snapshot ends with a tail of the packs added
// since page 1; gitlet.ErrSnapshotMoved restarts from page 1. Every row is
// validated before anything is written: a ref goes into a file git parses
// line by line.

// snapshot is one complete answer, every page applied.
type snapshot struct {
	full bool
	// position is page 1's: what the WHOLE snapshot is complete as of.
	position      statelog.Position
	streamCreated time.Time
	// repo is the LAST page's row: the freshest settings.
	repo gitlet.RepoRef
	// packs is a set, because the tail re-sends rows an earlier page served.
	packs map[packKey]gitlet.PackRow
	// refs is in answer order; a full snapshot's are sorted before use. A
	// delta serves a ref moved between pages once per position; the later
	// row wins.
	refs []gitlet.RefRow
	// behind: an answer on the incarnation's own stream BELOW its position.
	// The read level decides what it means (converge): a log gone back
	// below a linearizable read, a lagging copy below any other.
	behind bool
	// restreamed: page 1 was a delta keyed to another stream than the
	// incarnation's, and this is the whole answer read again.
	restreamed bool
	// since is a delta's floor; pushes its pushes phase where page 1 asked
	// for it (asked): every push in (since, position], in (position, push
	// id) order, each once, folded into the index of pushes (provenance.go).
	// An asked phase with no row is no push.
	since  statelog.Position
	pushes []gitlet.IntroducedRow
	asked  bool
	// retiredSince is page 1's StateQuery.RetiredSince (retiredFloor), zero
	// for none. retiredFrom is the floor the retired phase is complete
	// above, the same on every page: since on a delta, retiredSince on a full
	// answer that honoured it, ZERO on one that served none, after which a
	// round treats every unnamed pack file as unaccounted for (swap.go).
	// retired is that phase's rows, in (position, pack id, part) order, each
	// once: what a round takes out of the live set.
	retiredSince, retiredFrom statelog.Position
	retired                   []gitlet.RetiredRow
	// layers is the published chain as of position, root first
	// (gitlet.RepoSnapshot.Layers); layersServed whether page 1 served it
	// (every full answer, and a delta whose chain moved above its floor). A
	// copy replaces its chain only when served.
	layers       []gitlet.LayerRow
	layersServed bool
}

// retiredChecksums is the checksums s's retired phase names, as a set, by
// which a full round reads a stray (swap.go's strays).
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

// logMark is the code log AS THIS PROCESS HAS SEEN IT ANSWERED: the last
// answer's stream and the highest position any page 1 answered at. One mark
// for the node, because every repository's rows are one log: a log recreated
// or gone back under one incarnation has done so under all, and the first
// round to see it tells the rest (Cache.reread).
//
// A LINEARIZABLE answer read after a marked position is at or past it on a
// log that never goes back, whichever node answers, because an answer's
// position is the checkpoint the code reader's transaction is handed
// (gitlet.Reader), never the checkpoint row alone, which lags uncommitted
// barriers and would read as the log gone back. A round compares with the mark AS IT STOOD BEFORE ITS READ BEGAN: a
// position recorded after its barrier says nothing about it.
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

// saw records a page 1's answer (the mark only rises on its stream) and
// reports whether it is on another stream, the log recreated, from which the
// mark starts again.
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

// read is the snapshot of id that an incarnation at at, on stream, converges
// to: full, or a delta from at with its pushes phase where pushes is set
// (provenance.go). retired is a full answer's StateQuery.RetiredSince
// (retiredFloor; zero for a check). A page-1 DELTA on another stream is the
// log RECREATED (a restore keeps the stamp, ADR-0083), which Since cannot
// tell the reader: asked again from nothing, once, with no retirements. An
// answer on the own stream below at is BEHIND, for the round to judge by its
// level (converge). gitlet.ErrSnapshotMoved restarts from page 1 with the
// SAME Since, at most MaxSnapshotRestarts times (page 1 decides full or delta
// afresh). Pages are validated by snapshot.add (ErrBadSnapshot).
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
		case errors.Is(err, gitlet.ErrSnapshotMoved):
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

// retiredFloor is the floor a round asks a full answer for the retirements
// above (read's retired), so a full round can tell a pack file a compaction
// retired from one no row accounts for (swap.go): at, matching a delta's own
// floor so page 1 weighs the two answers like with like, or leaving (the
// incarnation's leavingSince) where lower, since a crash or a failed swap
// left it holding pack files retired above that. Zero for an incarnation at
// none.
func retiredFloor(at, leaving statelog.Position) statelog.Position {
	if below, err := leaving.Before(at); !leaving.IsZero() && err == nil && below {
		return leaving
	}
	return at
}

// fullRounds is the node's slots for FULL READS of a repository's rows
// (MaxFullRounds; package doc, "A node reads few repositories whole at
// once"). First come, first served (semaphore.Weighted), so no read waits
// behind one that asked after it.
type fullRounds struct {
	slots *semaphore.Weighted
	size  int64
	rec   *metrics.Recorder

	// mu orders every change of the counts with the gauge's setting, so the
	// gauge never holds a superseded count. running is the slots held, most
	// the most held at once (which the suite holds to the bound), waiting
	// the reads waiting for one.
	mu                     sync.Mutex
	running, most, waiting int64
}

// The states crewlet.code.cache.full_rounds counts a full read in.
const (
	fullRunning = "running"
	fullWaiting = "waiting"
)

// newFullRounds is size slots, reported to rec with both states at zero from
// the start, so a node's panel shows its full reads before its first.
func newFullRounds(size int64, rec *metrics.Recorder) *fullRounds {
	f := &fullRounds{slots: semaphore.NewWeighted(size), size: size, rec: rec}
	f.count(0, 0)
	return f
}

// hold waits for a slot under ctx: the cache's lifetime for a round (its
// callers wait under their own bounds, MaxFullRounds), a check's or a hint's
// own bound for theirs. A free slot nobody waits for is taken without
// counting a wait.
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

// holdFull waits for a full-read slot for id: ErrStopped if the cache stops
// meanwhile, as every sync of a stopped cache is; the read's own bound ending
// first says what it waited for.
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

// logRead marks the estate's failure to answer a page as ErrLogRead, except
// gitlet.ErrRepoNotFound and gitlet.ErrSnapshotMoved, which are the log's
// ANSWERS (no such repository; restart the read) and pass as themselves.
func logRead(err error) error {
	if errors.Is(err, gitlet.ErrRepoNotFound) || errors.Is(err, gitlet.ErrSnapshotMoved) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrLogRead, err)
}

// readPacks reads the PACK ROWS of a full snapshot of id, for a hint's
// confirmation (hint.go): pages up to the first that serves a ref. A full
// snapshot serves every pack before any ref (gitlet.Reader.Snapshot's
// phases), so the packs are whole there without paying for every ref's page.
// Restarted from page 1 when the history moves, as read is.
func (c *Cache) readPacks(ctx context.Context, id string, fresh statelog.Freshness) (snapshot, error) {
	for restarts := 0; ; restarts++ {
		snap, err := c.readOnce(ctx, id, statelog.Position{}, statelog.Position{}, fresh, false, true)
		if !errors.Is(err, gitlet.ErrSnapshotMoved) || restarts == MaxSnapshotRestarts {
			return snap, err
		}
	}
}

// readOnce reads every page of one snapshot (packsOnly: up to the first that
// serves a ref, readPacks), or returns the first failure (logRead). Every
// answered page is timed (pageAnswered).
//
// A FULL READ HOLDS A SLOT (fullRounds) from the page it first knows it is
// full to its last: page 1 when asked from nothing, page 2 when the log
// answers a delta in full. A delta's pages hold none.
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
	page, err := c.fetch.RepoState(ctx, gitlet.StateQuery{RepoID: id, Since: since, Fresh: fresh, Pushes: pushes,
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
		packs: map[packKey]gitlet.PackRow{}, since: since, asked: pushes && !page.Full,
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
			// A delta the log answered in full: the rest are whole-repository
			// pages.
			if err = c.holdFull(ctx, id); err != nil {
				return snapshot{}, err
			}
			held = true
		}
		next := gitlet.StateQuery{RepoID: id, Fresh: fresh, Cursor: page.Next}
		asked = time.Now()
		if page, err = c.fetch.RepoState(ctx, next); err != nil {
			return snapshot{}, logRead(err)
		}
		c.pageAnswered(page, time.Since(asked))
	}
	if snap.full {
		slices.SortFunc(snap.refs, func(a, b gitlet.RefRow) int { return strings.Compare(a.Ref, b.Ref) })
	}
	return snap, nil
}

// pageAnswered records one page's ask-to-answer time by what it served (full
// or delta) as crewlet.code.cache.read.page.duration, production's check on
// the page MaxFullRounds was sized against. A refused page is not counted.
func (c *Cache) pageAnswered(page gitlet.RepoSnapshot, took time.Duration) {
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
func (s *snapshot) add(id string, n int, page gitlet.RepoSnapshot, seen map[string]int) error {
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
		// A delta's retirements are every one above its own floor.
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
			// A delta re-serves a ref that moved between pages; the later
			// row must be newer, since two rows at one position are no
			// history.
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
// the domain keeps it (gitlet.LayerRow): at most gitlet.MaxPublishedLayers,
// each passing gitlet.LayerRef.Check and none a replacement; Seq from 1; the
// root over no base and each other over the one below, since a layer is
// valid only over its exact base and the order is all that names it; no
// layer, commit-graph layer or pack twice; each added on the code log at or
// below the snapshot's position. Whether the copy holds a layer's packs is
// the install's to decide (installable), never the read's.
func (s *snapshot) addLayers(rows []gitlet.LayerRow) error {
	if len(rows) > gitlet.MaxPublishedLayers {
		return fmt.Errorf("its published chain holds %d layers, past the %d a chain holds (gitlet.MaxPublishedLayers)",
			len(rows), gitlet.MaxPublishedLayers)
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

// addRetired validates one retired row and takes it in: a valid pack
// checksum, a code-log position in (retiredFrom, position], strictly after
// the previous row in (position, pack id, part) order so each is served once,
// and none at all on a full answer that serves no retirements.
func (s *snapshot) addRetired(r gitlet.RetiredRow) error {
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

// addPush validates one row of a delta's asked-for pushes phase and takes it
// in: a code-log position in (since, position], strictly after the previous
// row in (position, push id) order so each is served once, and a push
// validPush accepts.
func (s *snapshot) addPush(p gitlet.IntroducedRow) error {
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

// validPush holds a served push to what the index of pushes encodes
// (provrun.go) before anything is written: a canonical UUID, moves of refs
// between object ids, commits that are object ids. addPush and the backfill
// both use it.
func validPush(p gitlet.IntroducedRow) error {
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
// the one part this build splits, and the row-level half of the bundle rule
// (gitlet.PackRef.Check), the very rule the domain admitted the row by, so
// no copy refuses for good a row the domain published.
func checkPack(p gitlet.PackRow) error {
	switch {
	case p.Part != "pack":
		// A newer peer's new part is not a bundle this build can install.
		return fmt.Errorf("part %q is not one this build installs", p.Part)
	case !p.State.Valid():
		// Nor is a state it cannot place (packPlace): a pack in
		// objects/pack is one a push may rely on, whatever the state says.
		return fmt.Errorf("state %q is not one this build places", p.State)
	}
	return p.Ref().Check()
}
