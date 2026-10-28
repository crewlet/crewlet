package cache

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE PROVENANCE INDEX: which push INTRODUCED each commit, and every update
// each push LANDED on a ref — read from the code domain's push rows and kept
// beside the incarnation it serves, as immutable RUNS (provrun.go) the
// incarnation's record names (provenanceState). A derived file of the cache's
// own (derived.go's DerivedProvenance), so the budget counts it, it leaves
// with the repository and a rebuild links it rather than read the rows again.
// Not internal/provenance (ADR-0040's fences around text a model reads): the
// word here is a commit's, and this index answers which push put it where.
//
// # Eager, so a reviewer sees the push that just landed
//
// EVERY DELTA ROUND FOLDS THE PUSHES IT APPLIED BEFORE IT ANSWERS (sync.go
// apply): it asks the snapshot's pushes phase (code.StateQuery.Pushes) and
// writes them as one run over exactly (its position, the snapshot's], so after
// every round that answered the index is level with the incarnation — a
// debounced index would miss a just-pushed change exactly when a reviewer asks
// about it (PLAN's SCION AMENDMENTS, M3 item 1). A FULL round reads no pushes
// — its answer can span the whole history — and leaves what it passed over to
// the BACKFILL (backfill.go): every holder fills the GAP between its index and
// a full round's position, bounded by the pushes made while it was down, and
// reads a repository's HISTORY — down to the start of the push history the
// rows keep — only once it is WANTED, where this node has answered a seat's
// read as the repository's home (Cache.WantHistory, M3-decisions M3-O20), so a
// repository's 181 days are read once per fleet and only for repositories a
// seat asks about. An answer from an index that is not Complete carries its
// coverage (code.Coverage): never "no push" where the index holds no range.
//
// # Runs never overlap
//
// Every run is COMPLETE over its position range, and the ranges are disjoint:
// a delta's run covers (its floor, its position], a gap's page the positions
// it read whole — a page cut inside a group commit's record leaves that
// record to the next page — and a history page likewise. So no push is in two
// runs, a lookup weighs no duplicate, and the moves of a ref read in run order
// are in log order. A full round that finds a gap open widens it rather than
// opening a second: the gap becomes (its start, the round's position], and the
// runs a delta wrote above the old gap are dropped, to be read again by the
// gap's pages — never two gaps, which coverage could not say in one range.
//
// # One record, changed by one rule
//
// The record is IMMUTABLE once stored: every writer — a round's fold or its
// full read, the backfill's page, merge or horizon — decides its change from a
// record it read, does its slow work holding no lock (it reads the estate,
// builds a run, stages and places it), and then COMMITS: under the entry's mu,
// as one step, its change is applied to the record as it stands THEN, and
// only if that is still the index the work began on (incarnation.provGen) and
// the change still applies to it (commitIndex). A change that no longer
// applies is thrown away, its run moved into trash/ unnamed. So a round never
// waits for the backfill, the backfill never takes the repository's write
// lock, and neither can store a record over the other's: only a round moves
// Through, only a full round opens or widens the gap or makes an index anew,
// and the backfill's changes are each guarded by what they read.
//
// # Read through memory, never a read call a probe
//
// A run is MAPPED read-only (unix.Mmap) the first time a reader of its
// incarnation needs it, and every lookup is a binary search over that memory.
// A mapping is shared by every reader and HELD by each one that took it
// (Cache.Provenance) until that reader lets its pin go, and then KEPT for the
// copy's next reader within the node's bound on mapped runs — the process's
// count of mappings over MappedRunShare, past which the mapping let go
// longest ago is unmapped (runmaps.go) — so a busy repository's reads search
// memory the page cache backs, and a node that has read every copy it holds
// maps no more than the bound: kept until its index stopped naming it, every
// run of every copy ever read stayed mapped, and a node reading some ten
// thousand copies took the mappings the runtime's own heap needs. A run a
// merge or a drop has replaced is unmapped as its last holder lets go, and
// every mapping of an incarnation once it is discarded. Held past its file's
// name — renamed into trash/ and removed — a mapping would keep that file's
// blocks.
//
// # Nothing a run says is read from a run this process did not write or check
//
// A run this process wrote is VOUCHED for (check.go), and a rebuild carries
// the vouch of a run it links. One a previous process left is checked before
// it is mapped — by the backfill, behind the serving, a read slot a piece
// (checkRuns) — and a reader meeting a run still unchecked is answered without
// the index (errUnchecked) rather than waiting on a hash: the reads a seat
// makes are on a request's budget. A run that fails its trailer, in that
// check, a scrub or a verify, drops the whole index through the one removal of
// a derived file (dropFor) at the repair round it asks, and the backfill reads
// again what the dropped one held. M3-decisions M3-O67.

// provenanceFile is this file as the provenance index's declared writes name
// their writer (Writer.File): every placement of a run — a fold's, a page's, a
// merge's, a rebuild's link — is made here.
const provenanceFile = "internal/code/cache/provenance.go"

// errUnchecked is a run of the index a previous process left that the
// backfill has not yet held to its trailer: nothing it says is read until it
// is (M3-decisions M3-O67).
var errUnchecked = errors.New("cache: the index of pushes holds a run this process has not yet checked")

// errRunGone is a run its index names that is not on the disk — something
// outside the engine removed it — so the index holding it is lost, as a full
// round finds one lost (indexFull).
var errRunGone = errors.New("cache: a run the index of pushes names is gone from the disk")

// provenanceState is the provenance index's record: what derivedState
// carries for DerivedProvenance, persisted with the state. IMMUTABLE once
// stored (commitIndex): a state is encoded outside the entry's lock.
type provenanceState struct {
	// Stream is the identity of the code log the index was read from; every
	// position it records is on Through's generation.
	Stream time.Time `json:"stream"`
	// From and Through, packed positions: every push in (From, Through] is in
	// the index but those in the gap.
	From    int64 `json:"from"`
	Through int64 `json:"through"`
	// GapFrom and GapThrough are a range a full round passed over and the
	// backfill has not yet read, (GapFrom, GapThrough]; both zero for none.
	GapFrom    int64 `json:"gap_from,omitempty"`
	GapThrough int64 `json:"gap_through,omitempty"`
	// Complete is From being the start of the push history the rows keep:
	// the history read down to its end, or the rows' horizon past From.
	Complete bool `json:"complete,omitempty"`
	// Oldest is the position of the oldest push the rows held when last
	// asked, and OldestAt when (the horizon; HorizonEvery).
	Oldest   int64     `json:"oldest,omitempty"`
	OldestAt time.Time `json:"oldest_at,omitzero"`
	// Next is the number the next run takes: above every run the index
	// names, and every number this process placed a run under.
	Next uint64 `json:"next"`
	// Runs is every run, in position order, at most MaxProvenanceRuns.
	Runs []provenanceRun `json:"runs,omitempty"`
}

// provenanceRun is one run as the record names it.
type provenanceRun struct {
	N     uint64 `json:"n"`
	Bytes int64  `json:"bytes"`
	// From and Through are the run's range, packed: it holds every push in
	// (From, Through].
	From    int64 `json:"from"`
	Through int64 `json:"through"`
	Pushes  int64 `json:"pushes"`
	Commits int64 `json:"commits"`
	Moves   int64 `json:"moves"`
}

// clone is s with a copy of its runs, to change: what every change starts
// from.
func (s *provenanceState) clone() *provenanceState {
	out := *s
	out.Runs = slices.Clone(s.Runs)
	return &out
}

// bytes is what s's runs hold: the record's Bytes.
func (s *provenanceState) bytes() int64 {
	var n int64
	for _, r := range s.Runs {
		n += r.Bytes
	}
	return n
}

// gapped reports whether s has a gap.
func (s *provenanceState) gapped() bool { return s.GapThrough > s.GapFrom }

// above reports whether r lies above s's gap — or s has none; a run is never
// inside it.
func (s *provenanceState) above(r provenanceRun) bool { return !s.gapped() || r.From >= s.GapThrough }

// named reports whether s names run n.
func (s *provenanceState) named(n uint64) bool {
	return slices.ContainsFunc(s.Runs, func(r provenanceRun) bool { return r.N == n })
}

// check refuses a record no write of the index leaves: its runs out of order,
// overlapping, in its gap or past its range; a gap outside its range; numbers
// taken twice or past Next; more runs than MaxProvenanceRuns; or bytes that
// are not its runs' (want, the entry's Bytes).
func (s *provenanceState) check(want int64) error {
	switch {
	case s.From > s.Through:
		return fmt.Errorf("an index of pushes over (%d, %d], which runs backwards", s.From, s.Through)
	case s.gapped() && (s.GapFrom < s.From || s.GapThrough > s.Through):
		return fmt.Errorf("an index of pushes over (%d, %d] with a gap (%d, %d] outside it", s.From, s.Through,
			s.GapFrom, s.GapThrough)
	case !s.gapped() && (s.GapFrom != 0 || s.GapThrough != 0):
		return fmt.Errorf("an index of pushes with an empty gap (%d, %d]", s.GapFrom, s.GapThrough)
	case len(s.Runs) > MaxProvenanceRuns:
		return fmt.Errorf("an index of pushes of %d runs, past the %d it holds", len(s.Runs), MaxProvenanceRuns)
	case s.bytes() != want:
		return fmt.Errorf("an index of pushes whose runs hold %d bytes and recorded at %d", s.bytes(), want)
	}
	seen := map[uint64]bool{}
	for i, r := range s.Runs {
		switch {
		case r.N == 0 || r.N >= s.Next || seen[r.N]:
			return fmt.Errorf("an index of pushes naming run %d twice, or past its next (%d)", r.N, s.Next)
		case r.From >= r.Through || r.Through <= s.From || r.Through > s.Through:
			return fmt.Errorf("an index of pushes over (%d, %d] holding a run over (%d, %d]", s.From, s.Through,
				r.From, r.Through)
		case i > 0 && s.Runs[i-1].Through > r.From:
			return fmt.Errorf("an index of pushes whose runs over (%d, %d] and (%d, %d] overlap", s.Runs[i-1].From,
				s.Runs[i-1].Through, r.From, r.Through)
		case s.gapped() && r.Through > s.GapFrom && r.From < s.GapThrough:
			return fmt.Errorf("an index of pushes holding a run over (%d, %d] in its gap", r.From, r.Through)
		case r.Bytes <= 0 || r.Pushes <= 0 || r.Commits < 0 || r.Moves < 0:
			return fmt.Errorf("an index of pushes holding run %d of %d bytes and %d pushes", r.N, r.Bytes,
				r.Pushes)
		}
		seen[r.N] = true
	}
	return nil
}

// coverage is s as an answer carries it.
func (s *provenanceState) coverage() code.Coverage {
	pos := func(p int64) statelog.Position { return statelog.Unpack(topics.CodeLogStream, p) }
	c := code.Coverage{From: pos(s.From), Through: pos(s.Through), Complete: s.Complete}
	if s.gapped() {
		c.GapFrom, c.GapThrough = pos(s.GapFrom), pos(s.GapThrough)
	}
	return c
}

// generation is the generation of the code log s's positions are on.
func (s *provenanceState) generation() uint32 {
	return statelog.Unpack(topics.CodeLogStream, s.Through).Generation
}

// adjacent reports whether runs i and i+1 of s may merge into one run: on
// the same side of its gap, so nothing between them is unread — positions
// between two runs on one side hold no push, since every push there is in a
// run.
func (s *provenanceState) adjacent(i int) bool {
	return i >= 0 && i+1 < len(s.Runs) && s.above(s.Runs[i]) == s.above(s.Runs[i+1])
}

// widened is s with everything up to to left to its gap: a full round's
// position past Through, or a delta whose pushes a round could not fold. ONE
// GAP: one already open is widened from its own start, and every run above it
// is dropped, to be read again by the gap's pages — never two gaps, which a
// coverage could not say in one range.
func (s *provenanceState) widened(to int64) *provenanceState {
	next := s.clone()
	if next.gapped() {
		next.Runs = slices.DeleteFunc(next.Runs, func(r provenanceRun) bool { return r.From >= next.GapThrough })
	} else {
		next.GapFrom = next.Through
	}
	next.GapThrough, next.Through = to, to
	return next
}

// record is inc's index, nil for none; e.mu is held.
func (inc *incarnation) record() *provenanceState { return inc.derived[DerivedProvenance].Provenance }

// setRecord stores rec as inc's index — nil drops it — lets go of the
// mappings of every run it no longer names, and marks e for the backfill's
// schedule (backfillDirty); e.mu is held. EVERY CHANGE TO THE INDEX GOES
// THROUGH HERE but a drop of its files (dropFor, which marks e too), so its
// bytes are the runs' and the budget's, and the backfill looks again at
// whatever it changed.
func (c *Cache) setRecord(e *entry, inc *incarnation, rec *provenanceState) {
	if rec == nil {
		c.setDerived(inc, DerivedProvenance, derivedState{}, false)
	} else {
		c.setDerived(inc, DerivedProvenance, derivedState{Bytes: rec.bytes(), Step: StepProvenance, Provenance: rec},
			true)
	}
	c.forgetRuns(inc)
	c.backfillDirty(e)
}

// newIndex is inc's index made anew, empty at position at of the log made at
// stream: the index of an incarnation read whole for the first time, or one
// whose index was dropped — another index than any inc had, so no work begun
// on one is committed into it (provGen). e.mu is held.
func (c *Cache) newIndex(inc *incarnation, stream time.Time, at int64) *provenanceState {
	inc.provGen++
	return &provenanceState{Stream: stream, From: at, Through: at, Next: max(inc.provNext, 1)}
}

// allocRun takes the number the next run of inc's index is placed under: one
// no run of this process has had in inc, and above every run its index names.
// e.mu is held.
func (inc *incarnation) allocRun() uint64 {
	n := max(inc.provNext, 1)
	if rec := inc.record(); rec != nil {
		n = max(n, rec.Next)
	}
	inc.provNext = n + 1
	return n
}

// runRel is run n's path relative to its incarnation.
func runRel(n uint64) string {
	var buf [runRelBytes]byte
	return string(appendRunRel(buf[:0], n))
}

// runRelBytes holds the longest run path: crewlet/provenance. and twenty
// digits.
const runRelBytes = len(engineRoot) + len("/provenance.") + 20

// appendRunRel appends run n's path relative to its incarnation to buf: a
// map read by string(appendRunRel(…)) of a buffer on the caller's stack
// allocates nothing (runMarks).
func appendRunRel(buf []byte, n uint64) []byte {
	buf = append(buf, engineRoot...)
	buf = append(buf, filepath.Separator)
	buf = append(buf, "provenance."...)
	return strconv.AppendUint(buf, n, 10)
}

// runNumber is the number a run's file name carries, or false for a name
// that is no run's.
func runNumber(name string) (uint64, bool) {
	if !provenanceRunName().MatchString(name) {
		return 0, false
	}
	n, err := strconv.ParseUint(name[len("provenance."):], 10, 64)
	return n, err == nil
}

// commitIndex stores the index change makes of inc's index AS IT STANDS NOW —
// a clone of it, which change edits and answers true for — under e.mu, as one
// step, where it is still the index the work began on (gen) and change still
// applies, and answers whether it stored anything; the runs the stored index
// no longer names wait on the incarnation for the next state written, which
// no longer names them either (retireRuns). THE ONE WAY THE INDEX CHANGES
// BUT ITS MAKING AND ITS DROP, so no writer stores a record over one it did
// not read.
//
// A RETIRED incarnation takes no change: a rebuild or a swap replaced it, and
// the one serving now reads its own — a swap's carries the record and links
// every run it names (swap.go), so a page committed into the one it left would
// be a run read for nothing, counted in the cache's total beside the very
// runs the two share until that copy's last reader let go. Nor does one a
// swap is leaving (entry.swap), for the whole of the swap: a commit could
// retire a run of the record it carries before the swap has linked it — and
// the next state written of that copy moves a retired run into trash/
// (retireRuns) — and leave the next incarnation without the index over a file
// the backfill moved.
func (c *Cache) commitIndex(e *entry, inc *incarnation, gen uint64, change func(next *provenanceState) bool) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	cur := inc.record()
	if cur == nil || inc.provGen != gen || inc.discarded || inc.retired || e.swap != nil && e.swap.inc == inc {
		return false
	}
	next := cur.clone()
	if !change(next) {
		return false
	}
	next.Next = max(next.Next, inc.provNext)
	c.setRecord(e, inc, next)
	retireRuns(inc, cur, next)
	return true
}

// dropIndex drops inc's index where it is still the index gen names — the
// repository its pushes were read from is gone — and answers whether it did;
// its runs wait for the next state written (retireRuns).
func (c *Cache) dropIndex(e *entry, inc *incarnation, gen uint64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	cur := inc.record()
	if cur == nil || inc.provGen != gen || inc.discarded {
		return false
	}
	c.setRecord(e, inc, nil)
	retireRuns(inc, cur, nil)
	return true
}

// retireRuns puts every run rec names and next does not among inc's retired
// runs (incarnation.retiredRuns): each stays on the disk, named by the state
// there, until a state that no longer names it is written, and goes then
// (persist). e.mu is held. Moved into trash/ by the writer that retired it
// once ITS state was written, a run whose state write failed stayed where it
// was, named by nothing once the next write succeeded and counted by no
// record or budget until a full round listed crewlet/ — rare once the copy is
// warm, and likeliest under a full disk, which fails every write.
func retireRuns(inc *incarnation, rec, next *provenanceState) {
	for _, path := range retiredFiles(inc.dir, rec, next) {
		if !slices.Contains(inc.retiredRuns, path) {
			inc.retiredRuns = append(inc.retiredRuns, path)
		}
	}
}

// retiredFiles is the paths of the runs rec names and next does not.
func retiredFiles(dir string, rec, next *provenanceState) []string {
	if rec == nil {
		return nil
	}
	var out []string
	for _, r := range rec.Runs {
		if next == nil || !next.named(r.N) {
			out = append(out, filepath.Join(dir, runRel(r.N)))
		}
	}
	return out
}

// withRun is runs with run among them, in position order.
func withRun(runs []provenanceRun, run provenanceRun) []provenanceRun {
	i, _ := slices.BinarySearchFunc(runs, run.From, func(r provenanceRun, from int64) int {
		return cmp.Compare(r.From, from)
	})
	return slices.Insert(runs, i, run)
}

// forgetRuns lets go of every mapping of inc its index no longer names — or
// all of them, once inc is discarded — unmapping those no reader holds now
// and leaving each held one to its last release (runMaps.forget); e.mu is
// held.
func (c *Cache) forgetRuns(inc *incarnation) {
	if len(inc.mapped) == 0 {
		return
	}
	rec := inc.record()
	for n, m := range inc.mapped {
		if rec != nil && !inc.discarded && rec.named(n) {
			continue
		}
		delete(inc.mapped, n)
		if c.maps.forget(m) {
			m.unmap()
		}
	}
}

// releaseRuns is reader by letting go of the runs it held, each kept idle
// for the next reader while its index names it (runMaps.let); e.mu is held.
func (c *Cache) releaseRuns(inc *incarnation, by gitproc.Repairer) {
	held := inc.runHolds[by]
	if len(held) == 0 {
		return
	}
	delete(inc.runHolds, by)
	for _, m := range held {
		if c.maps.let(m) {
			if inc.mapped[m.n] == m {
				delete(inc.mapped, m.n)
			}
			m.unmap()
		}
	}
}

// Provenance is the provenance index of the incarnation s holds — every run
// mapped and held for as long as s is (released with it) — or the zero
// Provenance where the incarnation keeps none. An error is an index this
// node cannot read yet: a run a previous process left that the backfill has
// not yet held to its trailer (nothing is read of it, and the backfill is
// woken to check it), a run its record names that cannot be mapped, or a
// node whose readers hold every run its bound on mapped runs lets it map
// (runmaps.go). A reader answers without the index either way: an index
// decorates an answer, and no request waits on a hash.
//
// A run another reader mapped is HELD as it is found, under the entry's lock,
// so the bound cannot unmap it between finding it and reading it; one nobody
// has mapped — or that the bound has unmapped since — is mapped outside the
// lock and installed for the next reader once the record still names it.
func (c *Cache) Provenance(s Synced) (Provenance, error) {
	if err := c.handedOut(s, "a read of the index of pushes"); err != nil {
		return Provenance{}, err
	}
	e, inc := s.entry, s.inc
	for range 3 {
		e.mu.Lock()
		rec := inc.record()
		if rec == nil {
			e.mu.Unlock()
			return Provenance{}, nil
		}
		held := make(map[uint64]*mappedRun, len(rec.Runs))
		var want []provenanceRun
		for _, r := range rec.Runs {
			m := inc.mapped[r.N]
			switch {
			case m != nil && c.maps.hold(m):
				held[r.N] = m
				continue
			case m != nil:
				// Unmapped by the node's bound since its last reader let it
				// go: mapped again below.
				delete(inc.mapped, r.N)
			}
			want = append(want, r)
		}
		e.mu.Unlock()
		// letGo gives back what this reading took, when it answers nothing.
		letGo := func(fresh map[uint64]*mappedRun) {
			e.mu.Lock()
			for _, m := range held {
				if c.maps.let(m) {
					if inc.mapped[m.n] == m {
						delete(inc.mapped, m.n)
					}
					m.unmap()
				}
			}
			e.mu.Unlock()
			for _, f := range fresh {
				c.maps.drop(f)
			}
		}
		fresh := make(map[uint64]*mappedRun, len(want))
		for _, r := range want {
			m, err := c.mapRun(e, inc, r)
			if err != nil {
				letGo(fresh)
				// An unchecked run has marked the copy for the backfill
				// already (mapRun), which wakes it to check the run.
				return Provenance{}, err
			}
			fresh[r.N] = m
		}
		e.mu.Lock()
		if inc.record() != rec {
			// The index moved while its runs were mapped: map against the
			// record that names them now.
			e.mu.Unlock()
			letGo(fresh)
			continue
		}
		p := Provenance{from: rec.From, cov: rec.coverage(), indexed: true}
		if c.knobs != nil {
			p.decodes = c.knobs.decodes
		}
		if rec.gapped() {
			p.gapFrom, p.gapThrough = rec.GapFrom, rec.GapThrough
		}
		if inc.mapped == nil {
			inc.mapped = map[uint64]*mappedRun{}
		}
		if inc.runHolds == nil {
			inc.runHolds = map[gitproc.Repairer][]*mappedRun{}
		}
		for _, r := range rec.Runs {
			m := held[r.N]
			if m == nil {
				// Mapped fresh by this reader: installed, in place of a
				// mapping of the run the bound has unmapped meanwhile — or one
				// another reader installed meanwhile, which this one then
				// holds instead, its own let go.
				if cur := inc.mapped[r.N]; cur != nil && c.maps.hold(cur) {
					m = cur
				} else {
					m = fresh[r.N]
					delete(fresh, r.N)
					inc.mapped[r.N] = m
					c.maps.hold(m)
				}
			}
			inc.runHolds[s.Repairer] = append(inc.runHolds[s.Repairer], m)
			p.runs = append(p.runs, m.file)
		}
		e.mu.Unlock()
		for _, f := range fresh {
			c.maps.drop(f)
		}
		return p, nil
	}
	return Provenance{}, fmt.Errorf("cache: %s's index of pushes moved under three readings of it", e.id)
}

// mapRun maps run r of inc, read-only, once it is vouched for at the identity
// it has — errUnchecked otherwise, its stale vouch forgotten so the backfill
// checks it — and held to what its record says of it: within the node's
// bound on mapped runs (runMaps.reserve), as nobody's yet — the caller holds
// it, or drops it.
func (c *Cache) mapRun(e *entry, inc *incarnation, r provenanceRun) (*mappedRun, error) {
	rel := runRel(r.N)
	path := filepath.Join(inc.dir, rel)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// GONE: no vouch stands for a file that is not there, so the
			// backfill's next look at the copy checks the run — and finds it
			// lost while its index names it (checkRuns).
			e.mu.Lock()
			unvouch(inc, rel)
			c.backfillDirty(e)
			e.mu.Unlock()
		}
		return nil, fmt.Errorf("cache: opening %s's run %d: %w", e.id, r.N, err)
	}
	defer func() { _ = f.Close() }() // read-only, and the mapping outlives the descriptor
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("cache: %s's run %d: %w", e.id, r.N, err)
	}
	id := idOf(info)
	e.mu.Lock()
	ok := vouchedAt(inc, rel, id) && lstatID(path) == id
	if !ok {
		// A vouch for another identity is no vouch: forgotten, the run is
		// one the backfill's next pass holds to its trailer (checkRuns).
		unvouch(inc, rel)
		c.backfillDirty(e)
	}
	e.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s's run %d", errUnchecked, e.id, r.N)
	}
	if info.Size() != r.Bytes {
		return nil, fmt.Errorf("%w: %s's run %d is %d bytes, and its record says %d", errRunMalformed, e.id, r.N,
			info.Size(), r.Bytes)
	}
	if err := c.maps.reserve(); err != nil {
		return nil, fmt.Errorf("cache: mapping %s's run %d: %w", e.id, r.N, err)
	}
	data, err := unix.Mmap(int(f.Fd()), 0, int(info.Size()), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		c.maps.unreserve()
		return nil, fmt.Errorf("cache: mapping %s's run %d: %w", e.id, r.N, err)
	}
	file, err := openRunBytes(data)
	if err == nil && (file.h.from != r.From || file.h.through != r.Through || int64(file.h.pushes) != r.Pushes) {
		err = fmt.Errorf("%w: it holds (%d, %d] and %d pushes, and its record names (%d, %d] and %d",
			errRunMalformed, file.h.from, file.h.through, file.h.pushes, r.From, r.Through, r.Pushes)
	}
	m := &mappedRun{n: r.N, data: data, file: file}
	if err != nil {
		c.maps.drop(m)
		return nil, fmt.Errorf("cache: %s's run %d: %w", e.id, r.N, err)
	}
	return m, nil
}

// Provenance is an incarnation's index of which push introduced each commit,
// read through its runs: valid while the Synced it was read for is held.
type Provenance struct {
	runs                      []runFile
	from, gapFrom, gapThrough int64
	cov                       code.Coverage
	indexed                   bool
	decodes                   *atomic.Int64
}

// Indexed reports whether the incarnation keeps an index at all — every
// answer from one that does carries its Coverage.
func (p Provenance) Indexed() bool { return p.indexed }

// Coverage is what the index holds.
func (p Provenance) Coverage() code.Coverage { return p.cov }

// Commit is the push that introduced oid — the newest of the pushes the index
// holds that list it, a commit brought back after every ref that reached it
// was deleted being listed by both — or false where the index holds none.
func (p Provenance) Commit(oid string) (code.PushedBy, bool) {
	key, ok := oidBytes(oid)
	if !ok {
		return code.PushedBy{}, false
	}
	// Every run, newest first: a run's pushes are above every earlier one's.
	for i := len(p.runs) - 1; i >= 0; i-- {
		if at, found := p.runs[i].commitPush(key, p.from); found {
			by, err := decodePush(p.runs[i].push(at))
			return by, err == nil
		}
	}
	return code.PushedBy{}, false
}

// Moves is ref's landed updates at positions at or below through within the
// index's coverage, in log order, as a VIEW over the runs: nothing is decoded
// until At asks for it, so a landed-in search over a busy branch's hundreds of
// thousands of moves decodes the few dozen it probes.
func (p Provenance) Moves(ref string, through statelog.Position) MoveView {
	v := MoveView{ref: ref, gapFrom: p.gapFrom, gapThrough: p.gapThrough, decodes: p.decodes}
	limit := through.Packed()
	for _, f := range p.runs {
		key, ord, ok := f.refOrdinal(ref)
		if !ok {
			continue
		}
		lo, hi := f.moveSpan(key, ord, p.from, limit)
		if hi > lo {
			v.spans = append(v.spans, moveSpan{f: f, lo: lo, hi: hi, at: v.n})
			v.n += hi - lo
		}
	}
	return v
}

// MoveView is one ref's moves across the runs, in log order.
type MoveView struct {
	ref                 string
	spans               []moveSpan
	n                   int
	gapFrom, gapThrough int64
	// decodes counts the moves At decoded: the suite's (knobs.decodes).
	decodes *atomic.Int64
}

// moveSpan is one run's part of a view: its moves [lo, hi), the view's
// moves from at.
type moveSpan struct {
	f      runFile
	lo, hi int
	at     int
}

// Move is one landed update and the push that made it.
type Move struct {
	code.RefMove
	By code.PushedBy
}

// Len is how many moves the view holds.
func (v MoveView) Len() int { return v.n }

// rec is move i's record and the span holding it.
func (v MoveView) rec(i int) ([]byte, moveSpan) {
	k, _ := slices.BinarySearchFunc(v.spans, i, func(s moveSpan, i int) int {
		switch {
		case i < s.at:
			return 1
		case i >= s.at+s.hi-s.lo:
			return -1
		}
		return 0
	})
	s := v.spans[k]
	return s.f.moveRec(s.lo + i - s.at), s
}

// Position is move i's position: a read of its fixed-width record, never a
// decode.
func (v MoveView) Position(i int) statelog.Position {
	rec, _ := v.rec(i)
	return statelog.Unpack(topics.CodeLogStream, int64(binary.BigEndian.Uint64(rec[9:])))
}

// Deletion reports whether move i deleted the ref: a read of its flags,
// never a decode.
func (v MoveView) Deletion(i int) bool {
	rec, _ := v.rec(i)
	return rec[61]&moveDeleted != 0
}

// Forced reports whether move i was forced — not a fast-forward of the ref's
// value before it — or deleted the ref: a read of its flags, never a decode.
func (v MoveView) Forced(i int) bool {
	rec, _ := v.rec(i)
	return rec[61]&segmentStart != 0
}

// segmentStart is the flags of a move that begins a segment (Segments): a
// forced move or a deletion.
const segmentStart = moveForced | moveDeleted

// At is move i, decoded with its push: the one call of a view that decodes.
func (v MoveView) At(i int) Move {
	if v.decodes != nil {
		v.decodes.Add(1)
	}
	rec, s := v.rec(i)
	m := Move{RefMove: code.RefMove{Ref: v.ref, Old: hexOID(rec[21:41]), New: hexOID(rec[41:61]),
		Forced: rec[61]&moveForced != 0}}
	if at := int(binary.BigEndian.Uint32(rec[17:])); at < s.f.pushCount() {
		if by, err := decodePush(s.f.push(at)); err == nil {
			m.By = by
		}
	}
	return m
}

// Segments is every index where a segment of the view begins — its first
// move, every forced move, every deletion, and the first move above the
// index's gap (Gaps) — in order: a scan of each move's flags byte and of the
// positions, never a decode. Inside a segment every move is a fast-forward of
// the one before it, read whole with nothing unread between them, so whether
// a commit is reachable from a move's new value can only turn from no to yes
// within one.
func (v MoveView) Segments() []int {
	if v.n == 0 {
		return nil
	}
	out := []int{0}
	for _, s := range v.spans {
		for j := s.lo; j < s.hi; j++ {
			if i := s.at + j - s.lo; i > 0 && s.f.moveRec(j)[61]&segmentStart != 0 {
				out = append(out, i)
			}
		}
	}
	for _, g := range v.Gaps() {
		if i, found := slices.BinarySearch(out, g); !found {
			out = slices.Insert(out, i, g)
		}
	}
	return out
}

// Gaps is every index of a move that the index's gap lies just before — the
// first move above the gap — a binary search over positions, never a decode:
// the moves of the gap were never read, so whatever lies between such a move
// and the one before it is unknown. None where the index has no gap, or no
// move lies above it.
func (v MoveView) Gaps() []int {
	if v.gapThrough <= v.gapFrom || v.n == 0 {
		return nil
	}
	lo, hi := 0, v.n
	for lo < hi {
		h := int(uint(lo+hi) >> 1)
		if v.Position(h).Packed() <= v.gapThrough {
			lo = h + 1
		} else {
			hi = h
		}
	}
	if lo >= v.n {
		return nil
	}
	return []int{lo}
}

// hexOID is twenty bytes as an object id.
func hexOID(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 2*len(b))
	for i, c := range b {
		out[2*i], out[2*i+1] = digits[c>>4], digits[c&0x0f]
	}
	return string(out)
}

// WantHistory marks repoID's history as WANTED in this process: its index is
// read down to the start of the push history the rows keep, by the backfill,
// where until now it held only what the rounds since its first full read
// folded. Called by the query server for a read it answered as the
// repository's home (code.GitQuery.Home, M3-decisions M3-O20), so a
// repository's history is read by its effective home alone — once per fleet —
// and only for a repository a seat asked about. In memory alone: the index's
// Complete is what persists. A repository the cache holds no entry of has
// nothing to want.
func (c *Cache) WantHistory(repoID string) {
	c.mu.Lock()
	e := c.entries[repoID]
	c.mu.Unlock()
	if e == nil {
		return
	}
	e.mu.Lock()
	was := e.wantHistory
	e.wantHistory = true
	e.mu.Unlock()
	if !was {
		c.backfillDirty(e)
	}
}

// indexing reports whether this cache's declarations keep an index of pushes
// at all: a case opening a cache over declarations of its own
// (knobs.derived) may keep none, and then no round makes one.
func (c *Cache) indexing() bool { return c.image.decl(DerivedProvenance) != nil }

// foldPushes is a DELTA round's step for the index (sync.go apply), after its
// packs and its refs and before its state: the pushes its snapshot served
// above the index's Through written as ONE run, and Through moved to the
// snapshot's position — every delta's, one that served no push included — so
// after every round that answered the index is level with the incarnation
// (TestTheIndexIsLevelWithItsIncarnation). The round holds the repository's
// write lock, so nothing else moves Through meanwhile. It answers whether it
// wrote a run; a run it retired waits for the round's state (retireRuns).
//
// THE INDEX NEVER FAILS A ROUND: the copy the round converged is what its
// callers wait for, and an index is a decoration of their answers. What this
// step cannot write — a delta that served no pushes phase for it, an index
// behind the delta's floor, a run it could not build, stage or place, an
// index at its bound whose runs it cannot merge yet (one a previous process
// left, unchecked) — it leaves to the GAP, widened to the snapshot's
// position: the backfill reads it, and until then every answer says the
// index does not hold it (code.Coverage).
func (c *Cache) foldPushes(ctx context.Context, e *entry, inc *incarnation, snap snapshot) bool {
	e.mu.Lock()
	rec, gen := inc.record(), inc.provGen
	e.mu.Unlock()
	to := snap.position.Packed()
	if rec == nil || to <= rec.Through {
		return false
	}
	if c.knobs != nil && c.knobs.folded != nil {
		defer func(began time.Time) { c.knobs.folded(time.Since(began)) }(time.Now())
	}
	gap := func(why error) bool {
		c.log.Warn("code_cache_provenance_gap", "repo", e.id, "incarnation", inc.n,
			"from", rec.Through, "through", to, "error", why)
		c.commitIndex(e, inc, gen, func(next *provenanceState) bool {
			if next.Through >= to {
				return false
			}
			*next = *next.widened(to)
			return true
		})
		return false
	}
	switch {
	case !snap.asked:
		return gap(errors.New("the delta served no pushes phase"))
	case rec.Through < snap.since.Packed():
		return gap(fmt.Errorf("the index ends at %d, below the delta's floor %d", rec.Through, snap.since.Packed()))
	}
	// Rows at or below Through are the index's already: a round whose state
	// failed to persist is read again from the position put back (apply).
	var rows []code.IntroducedRow
	for _, p := range snap.pushes {
		if p.Position.Packed() > rec.Through {
			rows = append(rows, p)
		}
	}
	from := rec.Through
	level := func(next *provenanceState) bool {
		if next.Through != from {
			return false
		}
		next.Through = to
		return true
	}
	if len(rows) == 0 {
		c.commitIndex(e, inc, gen, level)
		return false
	}
	if len(rec.Runs) >= MaxProvenanceRuns {
		// AT THE BOUND, the round makes its own room — the backfill keeps an
		// index below it while it runs, so this is the index of a node whose
		// backfill lags a burst of deltas: its two cheapest adjacent runs
		// merged, from their mapped files, before the round's run is placed.
		i := cheapestPair(rec, func(i int) bool { return c.vouchedRun(e, inc, rec.Runs[i]) })
		if i < 0 {
			return gap(fmt.Errorf("%w: no two adjacent runs of its %d are this process's own or checked yet",
				errUnchecked, len(rec.Runs)))
		}
		ok, err := c.mergeSpan(ctx, e, inc, gen, rec, i, i+2)
		switch {
		case err != nil:
			return gap(fmt.Errorf("its %d runs could not make room: %w", len(rec.Runs), err))
		case !ok:
			return gap(fmt.Errorf("its %d runs moved while two of them were merged", len(rec.Runs)))
		}
	}
	built, err := buildRun(from, to, rows)
	if err != nil {
		return gap(err)
	}
	s, err := c.stageRun(built.write)
	if err != nil {
		return gap(err)
	}
	ok, err := c.storeRun(e, inc, gen, s, func(next *provenanceState, run provenanceRun) bool {
		if !level(next) {
			return false
		}
		next.Runs = withRun(next.Runs, run)
		return true
	})
	switch {
	case err != nil:
		return gap(err)
	case !ok:
		// Dropped or made anew under the round, which holds the write lock:
		// nothing a full round is — so a drop by a report's repair, whose
		// next round makes the index again at its own position.
		return false
	}
	return true
}

// cheapestPair is the adjacent pair of rec's runs, by the first's index, on
// one side of its gap, each of them ok to read (by its index), that hold the
// fewest pushes between them; -1 for none.
func cheapestPair(rec *provenanceState, ok func(i int) bool) int {
	best := -1
	for i := 0; i+1 < len(rec.Runs); i++ {
		if !rec.adjacent(i) || !ok(i) || !ok(i+1) {
			continue
		}
		if best < 0 || rec.Runs[i].Pushes+rec.Runs[i+1].Pushes < rec.Runs[best].Pushes+rec.Runs[best+1].Pushes {
			best = i
		}
	}
	return best
}

// vouchedRun reports whether inc's run r is one this process wrote or checked
// at the identity it has now: one a merge may read.
func (c *Cache) vouchedRun(e *entry, inc *incarnation, r provenanceRun) bool {
	rel := runRel(r.N)
	id := lstatID(filepath.Join(inc.dir, rel))
	e.mu.Lock()
	defer e.mu.Unlock()
	return vouchedAt(inc, rel, id)
}

// mergeSpan merges runs [lo, hi) of rec — adjacent, on one side of its gap,
// each vouched for — into one run, without the pushes at or below rec's From
// (the rows' horizon), and stores the record naming it in place of them
// (storeRun) where they are all still named side by side; where nothing of
// them is above From, they are retired with no run in their place. It answers
// whether it stored; the inputs wait for the next state written
// (retireRuns). Each input is mapped for the merge alone and read in order:
// the merge streams (mergeRuns), holding nothing a push or a ref.
func (c *Cache) mergeSpan(ctx context.Context, e *entry, inc *incarnation, gen uint64, rec *provenanceState,
	lo, hi int) (bool, error) {

	span := slices.Clone(rec.Runs[lo:hi])
	replace := func(next *provenanceState, run provenanceRun) bool {
		at := slices.IndexFunc(next.Runs, func(r provenanceRun) bool { return r.N == span[0].N })
		if at < 0 || at+len(span) > len(next.Runs) {
			return false
		}
		for k, r := range span {
			if next.Runs[at+k].N != r.N || k > 0 && !next.adjacent(at+k-1) {
				return false
			}
		}
		if run.N == 0 {
			next.Runs = slices.Delete(next.Runs, at, at+len(span))
		} else {
			next.Runs = slices.Replace(next.Runs, at, at+len(span), run)
		}
		return true
	}
	files := make([]runFile, 0, len(span))
	kept := 0
	for _, r := range span {
		m, err := c.mapRun(e, inc, r)
		if err != nil {
			return false, err
		}
		defer c.maps.drop(m)
		files = append(files, m.file)
		kept += m.file.pushCount() - m.file.firstAbove(rec.From)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var (
		ok  bool
		err error
	)
	if kept == 0 {
		ok = c.commitIndex(e, inc, gen, func(next *provenanceState) bool { return replace(next, provenanceRun{}) })
	} else {
		s, serr := c.stageRun(func(w io.Writer) (runHeader, error) { return mergeRuns(w, files, rec.From) })
		if serr != nil {
			return false, serr
		}
		ok, err = c.storeRun(e, inc, gen, s, replace)
	}
	if ok && err == nil && c.knobs != nil && c.knobs.merged != nil {
		c.knobs.merged(len(span))
	}
	return ok, err
}

// stagedRun is a run written whole into tmp/ and not yet placed: counted in
// the cache's tmp/ bytes until it is placed or dropped, since a merge of an
// index's largest runs stages as much as both of them hold.
type stagedRun struct {
	path string
	h    runHeader
	size int64
}

// stageRun writes a run through write into a staged file in tmp/, durable
// there, and answers it.
func (c *Cache) stageRun(write func(io.Writer) (runHeader, error)) (*stagedRun, error) {
	s := &stagedRun{}
	path, err := c.stage(hostbox.FileMode, func(f *os.File) error {
		var werr error
		if s.h, werr = write(f); werr != nil {
			return werr
		}
		info, serr := f.Stat()
		if serr != nil {
			return serr
		}
		s.size = info.Size()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cache: staging a run of the index of pushes: %w", err)
	}
	s.path = path
	c.tmpBytes.Add(s.size)
	return s, nil
}

// placeRun renames s over inc's run n — making the incarnation's crewlet/
// where it is missing, the one directory of the engine's own there — makes
// the rename durable before any state names the run, and answers the run as
// the record names it. A run renamed in whose rename could not be made
// durable is moved into trash/ again: nothing will name it.
func (c *Cache) placeRun(inc *incarnation, s *stagedRun, n uint64) (provenanceRun, error) {
	defer c.tmpBytes.Add(-s.size)
	dir := filepath.Join(inc.dir, engineRoot)
	made := false
	switch info, err := os.Lstat(dir); {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(dir, hostbox.DirMode); err != nil {
			return provenanceRun{}, removeStaged(s.path, fmt.Errorf("cache: making %s: %w", dir, err))
		}
		made = true
	case err != nil:
		return provenanceRun{}, removeStaged(s.path, err)
	case !info.IsDir():
		return provenanceRun{}, removeStaged(s.path, fmt.Errorf("cache: %s is not a directory: the next full "+
			"round moves it aside", dir))
	}
	path := filepath.Join(inc.dir, runRel(n))
	if err := c.place(s.path, path); err != nil {
		return provenanceRun{}, err
	}
	err := syncDir(dir)
	if err == nil && made {
		err = syncDir(inc.dir)
	}
	if err != nil {
		if terr := toTrash(c.dir, path, newV7()+"-"+filepath.Base(path)); terr != nil {
			err = errors.Join(err, terr)
		}
		return provenanceRun{}, err
	}
	return provenanceRun{N: n, Bytes: s.size, From: s.h.from, Through: s.h.through, Pushes: int64(s.h.pushes),
		Commits: int64(s.h.commits), Moves: int64(s.h.moves)}, nil
}

// storeRun places s as a new run of inc's index, vouched for as this
// process's own, and stores the record change makes of the index as it stands
// then, given that run (commitIndex) — THE ONE WAY A RUN JOINS AN INDEX. It
// answers whether it stored — the runs the stored record no longer names wait
// for the next state written (retireRuns) — and a change that no longer
// applies — the index dropped or made anew, or moved past what change needs
// of it — leaves the run named by nothing, no state either, and moves it into
// trash/ at once.
func (c *Cache) storeRun(e *entry, inc *incarnation, gen uint64, s *stagedRun,
	change func(next *provenanceState, run provenanceRun) bool) (bool, error) {

	e.mu.Lock()
	n := inc.allocRun()
	e.mu.Unlock()
	run, err := c.placeRun(inc, s, n)
	if err != nil {
		return false, err
	}
	c.vouchFiles(e, inc, runRel(n))
	ok := c.commitIndex(e, inc, gen, func(next *provenanceState) bool {
		if !change(next, run) {
			return false
		}
		next.Next = max(next.Next, n+1)
		return true
	})
	if !ok {
		c.trashRuns(e, inc, []string{filepath.Join(inc.dir, runRel(n))})
	}
	return ok, nil
}

// trashRuns moves the files of retired runs of inc into trash/ — a run no
// state names: one a written state no longer names (persist), or one no
// record ever named — each with its vouch. A file gone already is no
// failure: a sweep of runs no record names may have moved it first.
func (c *Cache) trashRuns(e *entry, inc *incarnation, paths []string) {
	for _, path := range paths {
		if !c.enter() {
			return
		}
		err := toTrash(c.dir, path, newV7()+"-"+filepath.Base(path))
		c.leave()
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			c.log.Warn("code_cache_trash_failed", "path", path, "error", err)
			continue
		}
		if rel, rerr := filepath.Rel(inc.dir, path); rerr == nil {
			e.mu.Lock()
			unvouch(inc, rel)
			e.mu.Unlock()
		}
	}
	if len(paths) > 0 {
		c.kick()
	}
}

// historyGone reports whether the log under inc is no longer the history its
// index was read from, as a FULL round's snapshot snap shows it — marked so by
// the round that saw the log recreated or gone back (incarnation.historyGone,
// taken here), or told by the index itself: recorded from another stream or
// another generation of the log, or past the position snap reads at. e.mu is
// not held.
func (c *Cache) historyGone(e *entry, inc *incarnation, snap snapshot) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	gone := inc.historyGone
	inc.historyGone = false
	if rec := inc.record(); rec != nil {
		gone = gone || !rec.Stream.Equal(snap.streamCreated) || rec.generation() != snap.position.Generation ||
			snap.position.Packed() < rec.Through
	}
	return gone
}

// indexFull is a FULL round's step for the index (sync.go apply), after the
// round has dropped what its history or its checks asked dropped: an
// incarnation with no index is given an empty one at the round's position,
// and one behind it leaves what lies between to its gap, opened or widened up
// to it (widened) for the backfill to read. A record naming a run that is not
// on disk at its size — a file something outside the engine removed — is no
// record: the index is made anew. What it retires waits for the round's
// state (retireRuns): the runs a widened gap or a lost run retired, and every
// run file no record names — a crash between a run's rename and its state's
// — whose number it also takes past, so no run this process places is renamed
// over one. The round holds the repository's write lock; the backfill's
// commits meanwhile are each decided against the record this one stores
// (commitIndex).
func (c *Cache) indexFull(e *entry, inc *incarnation, snap snapshot) {
	if !c.indexing() {
		return
	}
	p := snap.position.Packed()
	listed, err := c.image.listed(inc.dir, engineRoot)
	if err != nil {
		c.log.Warn("code_cache_provenance_unlisted", "repo", e.id, "incarnation", inc.n, "error", err)
	}
	onDisk := map[uint64]int64{}
	var top uint64
	for _, d := range listed {
		n, ok := runNumber(d.Name())
		if !ok {
			continue
		}
		top = max(top, n)
		if info, ierr := d.Info(); ierr == nil && info.Mode().IsRegular() {
			onDisk[n] = info.Size()
		}
	}
	e.mu.Lock()
	inc.provNext = max(inc.provNext, top+1)
	cur := inc.record()
	next := cur
	lost := cur != nil && err == nil && slices.ContainsFunc(cur.Runs, func(r provenanceRun) bool {
		size, held := onDisk[r.N]
		return !held || size != r.Bytes
	})
	switch {
	case cur == nil || lost:
		next = c.newIndex(inc, snap.streamCreated, p)
	case p > cur.Through:
		next = cur.widened(p)
	}
	if next != cur {
		c.setRecord(e, inc, next)
	}
	retireRuns(inc, cur, next)
	for n := range onDisk {
		path := filepath.Join(inc.dir, runRel(n))
		if !next.named(n) && (cur == nil || !cur.named(n)) && !slices.Contains(inc.retiredRuns, path) {
			inc.retiredRuns = append(inc.retiredRuns, path)
		}
	}
	e.mu.Unlock()
	if lost {
		c.log.Warn("code_cache_provenance_lost", "repo", e.id, "incarnation", inc.n,
			"why", "a run the index names is not on disk at its size: the index is made anew")
	}
}

// seedIndex is a rebuild's seeding of its index (seedRebuild): every run of
// the index of the incarnation it replaces hard-linked into the new one
// (SeedLink) and the record carried with them, each run's vouch with it — a
// link is the same inode — so a run this process wrote or checked is read at
// once and one it has not is checked by the backfill before anything reads it
// there, as it would have been in the old. Nothing at all for an index whose
// log a round has marked gone (incarnation.historyGone), which that round
// would drop: the new incarnation reads its own. A run that cannot be linked
// seeds nothing — the links made are left to the new incarnation's first full
// round, which moves every run no record names into trash/ — and the rebuild
// goes on: an index is a decoration, and the new incarnation's first round
// makes one anew.
func (c *Cache) seedIndex(e *entry, replaced, inc *incarnation) {
	if d := c.image.decl(DerivedProvenance); d == nil || d.Seed != SeedLink {
		return
	}
	e.mu.Lock()
	rec, gone, next := replaced.record(), replaced.historyGone, replaced.provNext
	e.mu.Unlock()
	if rec == nil || gone {
		return
	}
	dir := filepath.Join(inc.dir, engineRoot)
	if err := os.Mkdir(dir, hostbox.DirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		c.log.Warn("code_cache_rebuild_seed_failed", "repo", e.id, "file", engineRoot, "error", err)
		return
	}
	ids := make(map[string]fileID, len(rec.Runs))
	for _, r := range rec.Runs {
		rel := runRel(r.N)
		if err := os.Link(filepath.Join(replaced.dir, rel), filepath.Join(inc.dir, rel)); err != nil {
			c.log.Warn("code_cache_rebuild_seed_failed", "repo", e.id, "file", rel, "error", err)
			return
		}
		ids[rel] = lstatID(filepath.Join(inc.dir, rel))
	}
	for _, d := range []string{dir, inc.dir} {
		if err := syncDir(d); err != nil {
			c.log.Warn("code_cache_rebuild_seed_failed", "repo", e.id, "file", engineRoot, "error", err)
			return
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if replaced.record() != rec {
		// Changed while it was linked — the backfill committed into the
		// copy being replaced: a run it names may be one no link was made
		// for. The new incarnation reads its own.
		return
	}
	for rel, id := range ids {
		if vouchedAt(replaced, rel, id) {
			vouch(inc, rel, id)
		}
	}
	inc.provGen++
	inc.provNext = max(inc.provNext, next, rec.Next)
	c.setRecord(e, inc, rec)
	if len(rec.Runs) > 0 {
		c.log.Info("code_cache_provenance_seeded", "repo", e.id, "incarnation", inc.n, "runs", len(rec.Runs))
	}
}
