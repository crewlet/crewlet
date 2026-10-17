package cache

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
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
// stream, converges to: full, or a delta from at, at fresh.
//
//  1. Page 1 asks from at (zero when full).
//  2. A page-1 DELTA keyed to another stream than the incarnation's is the
//     log RECREATED under the cache — provisioned anew, its stamp not the
//     one the cache read under (a snapshot restore keeps it, ADR-0035) —
//     which Since cannot tell the reader: asked again from nothing, once
//     (restreamed).
//  3. An answer on the incarnation's own stream below at is BEHIND; what that
//     means is the round's to decide by the level it read at (converge).
//  4. Next is followed to its end; a page that answers code.ErrSnapshotMoved
//     starts the read again from page 1 with the SAME Since, at most
//     MaxSnapshotRestarts times — page 1 decides full afresh wherever a sweep
//     or a reanchor moved under a delta.
//  5. Every page is validated (snapshot.add) — ErrBadSnapshot, naming the
//     page and the row.
func (c *Cache) read(ctx context.Context, id string, at statelog.Position, stream time.Time, full bool,
	fresh statelog.Freshness) (snapshot, error) {

	since := at
	if full {
		since = statelog.Position{}
	}
	restarts, restreamed := 0, false
	for {
		snap, err := c.readOnce(ctx, id, since, fresh, false)
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
			since = statelog.Position{}
			continue
		case snap.streamCreated.Equal(stream) && !reached(snap.position, at):
			snap.behind = true
		}
		snap.restreamed = restreamed
		return snap, nil
	}
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
		snap, err := c.readOnce(ctx, id, statelog.Position{}, fresh, true)
		if !errors.Is(err, code.ErrSnapshotMoved) || restarts == MaxSnapshotRestarts {
			return snap, err
		}
	}
}

// readOnce reads every page of one snapshot — or, packsOnly, a full one's
// pages up to the first that serves a ref (readPacks) — or answers the first
// page's failure as it is: the estate's, as ErrLogRead (logRead).
func (c *Cache) readOnce(ctx context.Context, id string, since statelog.Position,
	fresh statelog.Freshness, packsOnly bool) (snapshot, error) {

	page, err := c.fetch.RepoState(ctx, code.StateQuery{RepoID: id, Since: since, Fresh: fresh})
	if err != nil {
		return snapshot{}, logRead(err)
	}
	if packsOnly && !page.Full {
		return snapshot{}, fmt.Errorf("%w: %s's snapshot from nothing answered a delta", ErrBadSnapshot, id)
	}
	snap := snapshot{
		full: page.Full, position: page.Position, streamCreated: page.StreamCreated,
		packs: map[packKey]code.PackRow{},
	}
	seen := map[string]int{}
	for n := 1; ; n++ {
		if err = snap.add(id, n, page, seen); err != nil {
			return snapshot{}, err
		}
		if page.Next == nil || packsOnly && len(page.Refs) > 0 {
			break
		}
		next := code.StateQuery{RepoID: id, Fresh: fresh, Cursor: page.Next}
		if page, err = c.fetch.RepoState(ctx, next); err != nil {
			return snapshot{}, logRead(err)
		}
	}
	if snap.full {
		slices.SortFunc(snap.refs, func(a, b code.RefRow) int { return strings.Compare(a.Ref, b.Ref) })
	}
	return snap, nil
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
	s.repo = page.Repo
	return nil
}

// checkPack is a pack row this build can install, decided before any GET:
// the one part this build splits, and the row-level half of the one bundle
// rule (code.PackRef.Check) — the very rule the domain admitted the row by,
// so a row no git node can install is one no proposal was ever published
// with, rather than one this copy alone refuses on every git node for good.
func checkPack(p code.PackRow) error {
	if p.Part != "pack" {
		// A newer peer's new part is not a bundle this build can install.
		return fmt.Errorf("part %q is not one this build installs", p.Part)
	}
	return p.Ref().Check()
}
