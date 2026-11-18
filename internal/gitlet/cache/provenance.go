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

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The index of pushes (package doc, "The index of pushes"), as immutable runs
// (provrun.go) the incarnation's record names. Not internal/provenance
// (ADR-0040's fences): the word here is a commit's. Runs cover disjoint ranges
// whole (a page cut inside a group commit's record leaves it to the next), so
// no push is weighed twice and a ref's moves in run order are in log order.
// The record changes only through commitIndex: only a round moves Through,
// only a full round makes an index or opens or widens its one gap (a coverage
// says one range), and the backfill's changes are guarded by what they read.
// An answer never says "no push" where the index holds no range. A run
// neither written nor checked here is answered without (errUnchecked), never
// waited on; one failing its trailer drops the whole index (dropFor;
// M3-decisions M3-O67).

// provenanceFile names this file as the writer of the index's declared writes
// (Writer.File): every run is placed or linked here.
const provenanceFile = "internal/gitlet/cache/provenance.go"

// errUnchecked is a run a previous process left that the backfill has not yet
// held to its trailer: nothing it says is read until then (M3-decisions M3-O67).
var errUnchecked = errors.New("cache: the index of pushes holds a run this process has not yet checked")

// errRunGone is a run its index names that something outside the engine
// removed: the index is lost, as a full round finds it (indexFull).
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

// clone is s with a copy of its runs: what every change starts from.
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
func (s *provenanceState) coverage() gitlet.Coverage {
	pos := func(p int64) statelog.Position { return statelog.Unpack(topics.CodeLogStream, p) }
	c := gitlet.Coverage{From: pos(s.From), Through: pos(s.Through), Complete: s.Complete}
	if s.gapped() {
		c.GapFrom, c.GapThrough = pos(s.GapFrom), pos(s.GapThrough)
	}
	return c
}

// generation is the generation of the code log s's positions are on.
func (s *provenanceState) generation() uint32 {
	return statelog.Unpack(topics.CodeLogStream, s.Through).Generation
}

// adjacent reports whether runs i and i+1 of s may merge: on the same side of
// its gap, so no unread push lies between them.
func (s *provenanceState) adjacent(i int) bool {
	return i >= 0 && i+1 < len(s.Runs) && s.above(s.Runs[i]) == s.above(s.Runs[i+1])
}

// widened is s with everything up to to left to its gap. ONE GAP: an open one
// widens from its own start and the runs above it are dropped, to be read
// again by its pages, since a coverage says one range.
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

// setRecord stores rec as inc's index (nil drops it), lets go of the mappings
// of runs it no longer names, and marks e for the backfill (backfillDirty);
// e.mu is held. Every change to the index but a drop of its files (dropFor)
// goes through here, so its bytes stay the budget's and the backfill looks
// again.
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
// stream; bumping provGen keeps work begun on an earlier index from
// committing into it. e.mu is held.
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

// commitIndex applies change to a clone of inc's index AS IT STANDS NOW, under
// e.mu, and stores it only where the index is still gen's and change answers
// true; it answers whether it stored. THE ONE WAY THE INDEX CHANGES but its
// making and its drop, so no writer stores over a record it did not read. Runs
// the stored index no longer names wait for the next state (retireRuns).
//
// A RETIRED incarnation takes no change: its replacement serves its own (a
// swap links every run, swap.go), so a page committed here is read for nothing
// and counted twice. Nor does one a swap is leaving (entry.swap): a commit
// could retire a run the swap has not linked yet, and the next state would
// trash it from under the next incarnation's index.
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
// runs (incarnation.retiredRuns): each stays on disk until a state that no
// longer names it is written (persist). e.mu is held. Not trashed by the
// retiring writer after its own state write: were that write to fail, the run
// would stay on disk, named and counted by nothing after the next write, until
// a full round listed crewlet/.
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

// forgetRuns lets go of every mapping of inc its index no longer names (all,
// once inc is discarded), unmapping it now or, if held, at its last release
// (runMaps.forget): a mapping kept past its file's trashing keeps its blocks.
// e.mu is held.
func (c *Cache) forgetRuns(inc *incarnation) {
	// The code index's files are mappings of inc's too (codeindex.go): let
	// go where inc is gone or its record moved past the opening.
	if o := inc.codeIndex; o != nil && (inc.discarded || o.rec != inc.codeIndexRecord()) {
		c.forgetCodeIndex(inc)
	}
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

// releaseRuns lets go of the runs by held, each kept idle for the next reader
// while its index names it (runMaps.let); e.mu is held.
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

// Provenance is the index of the incarnation s holds, every run mapped and
// held as long as s is, or the zero Provenance where it keeps none. An error
// is an index this node cannot read yet: an unchecked run (the backfill is
// woken to check it), a run that cannot be mapped, or the node's bound on
// mapped runs reached (runmaps.go). A reader answers without the index either
// way: an index decorates an answer, and no request waits on a hash.
//
// A run already mapped is HELD under the entry's lock as it is found, so the
// bound cannot unmap it before it is read; one not mapped is mapped outside
// the lock and installed only if the record still names it.
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
				// Mapped fresh by this reader: installed, unless another
				// reader installed one meanwhile, which this one holds
				// instead, letting its own go.
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

// mapRun maps run r of inc read-only, within the node's bound
// (runMaps.reserve), once it is vouched for at its current identity
// (errUnchecked otherwise, the stale vouch forgotten so the backfill checks
// it) and matches its record. The caller holds the mapping or drops it.
func (c *Cache) mapRun(e *entry, inc *incarnation, r provenanceRun) (*mappedRun, error) {
	rel := runRel(r.N)
	path := filepath.Join(inc.dir, rel)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// GONE: unvouched, so the backfill's next look finds it lost
			// while its index names it (checkRuns).
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
		// A vouch for another identity is none: the backfill's next pass
		// holds the run to its trailer (checkRuns).
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
	if err = c.maps.reserve(); err != nil {
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
	cov                       gitlet.Coverage
	indexed                   bool
	decodes                   *atomic.Int64
}

// Indexed reports whether the incarnation keeps an index at all — every
// answer from one that does carries its Coverage.
func (p Provenance) Indexed() bool { return p.indexed }

// Coverage is what the index holds.
func (p Provenance) Coverage() gitlet.Coverage { return p.cov }

// Commit is the push that introduced oid, or false where the index holds
// none: the newest that lists it, since a commit brought back after every ref
// reaching it was deleted is listed by both.
func (p Provenance) Commit(oid string) (gitlet.PushedBy, bool) {
	key, ok := oidBytes(oid)
	if !ok {
		return gitlet.PushedBy{}, false
	}
	// Every run, newest first: a run's pushes are above every earlier one's.
	for i := len(p.runs) - 1; i >= 0; i-- {
		if at, found := p.runs[i].commitPush(key, p.from); found {
			by, err := decodePush(p.runs[i].push(at))
			return by, err == nil
		}
	}
	return gitlet.PushedBy{}, false
}

// Moves is ref's landed updates at or below through within the index's
// coverage, in log order, as a VIEW over the runs: nothing is decoded until At
// asks, so a search over a busy branch's hundreds of thousands of moves
// decodes the few dozen it probes.
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
	gitlet.RefMove
	By gitlet.PushedBy
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
	m := Move{RefMove: gitlet.RefMove{Ref: v.ref, Old: hexOID(rec[21:41]), New: hexOID(rec[41:61]),
		Forced: rec[61]&moveForced != 0}}
	if at := int(binary.BigEndian.Uint32(rec[17:])); at < s.f.pushCount() {
		if by, err := decodePush(s.f.push(at)); err == nil {
			m.By = by
		}
	}
	return m
}

// Segments is every index where a segment of the view begins, in order: its
// first move, every forced move or deletion, and the first move above the gap
// (Gaps), found from flags and positions, never a decode. Inside a segment
// each move fast-forwards the one before with nothing unread between, so a
// commit's reachability from a move's new value only turns from no to yes.
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

// Gaps is the index of the first move above the index's gap, if any, by a
// binary search over positions: the gap's moves were never read, so what lies
// between that move and the one before it is unknown.
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

// WantHistory marks repoID's history WANTED in this process, so the backfill
// reads its index down to the start of the push history the rows keep. The
// query server calls it for a read it answered as the repository's home
// (gitlet.GitQuery.Home, M3-decisions M3-O20), so history is read once per
// fleet and only for repositories a seat asked about. In memory alone: the
// index's Complete is what persists.
func (c *Cache) WantHistory(repoID string) {
	c.mu.Lock()
	e := c.entries[repoID]
	c.mu.Unlock()
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.wantHistory {
		e.wantHistory = true
		c.backfillDirty(e)
	}
}

// indexing reports whether this cache's declarations keep an index of pushes:
// a case with declarations of its own (knobs.derived) may keep none.
func (c *Cache) indexing() bool { return c.image.decl(DerivedProvenance) != nil }

// foldPushes is a DELTA round's step for the index (sync.go apply), after its
// packs and refs and before its state: the pushes above Through become ONE run
// and Through moves to the snapshot's position, even with no push, so every
// answered round leaves the index level with the incarnation
// (TestTheIndexIsLevelWithItsIncarnation). The round's write lock keeps all
// else from moving Through. It answers whether it wrote a run; a run it
// retired waits for the round's state (retireRuns).
//
// THE INDEX NEVER FAILS A ROUND: it only decorates answers. What this step
// cannot write (no pushes phase, an index behind the delta's floor, a run not
// built or placed, a full index it cannot merge yet) goes to the GAP, widened
// to the snapshot's position, which answers' coverage reports.
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
	var rows []gitlet.IntroducedRow
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
		// AT THE BOUND (the backfill lags a burst of deltas), the round makes
		// its own room: its two cheapest adjacent runs merged first.
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
		// Dropped under the round's write lock, so by a report's repair,
		// whose next round makes the index again.
		return false
	}
	return true
}

// cheapestPair is the first index of the pair of rec's runs that may merge
// (adjacent), both ok, holding the fewest pushes; -1 for none.
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

// mergeSpan merges runs [lo, hi) of rec (adjacent on one side of its gap,
// vouched for) into one run, dropping pushes at or below From (the rows'
// horizon), and stores it in their place (storeRun) where they are still named
// side by side; with nothing above From they are retired with no replacement.
// It answers whether it stored; inputs wait for the next state (retireRuns).
// The merge streams (mergeRuns), holding nothing a push or a ref.
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

// stagedRun is a run written whole into tmp/, not yet placed: counted in the
// cache's tmp/ bytes until then, since a merge stages as much as its inputs.
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

// placeRun renames s over inc's run n, making crewlet/ where missing, and
// makes the rename durable before any state names the run. A run whose rename
// could not be made durable goes into trash/: nothing will name it.
func (c *Cache) placeRun(inc *incarnation, s *stagedRun, n uint64) (provenanceRun, error) {
	defer c.tmpBytes.Add(-s.size)
	dir := filepath.Join(inc.dir, engineRoot)
	made := false
	switch info, err := os.Lstat(dir); {
	case errors.Is(err, fs.ErrNotExist):
		if err = os.Mkdir(dir, hostbox.DirMode); err != nil {
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
// process's own, and stores what change makes of the index as it then stands
// (commitIndex): THE ONE WAY A RUN JOINS AN INDEX. It answers whether it
// stored; where change no longer applies, the run, named by nothing, goes into
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

// trashRuns moves runs of inc no state names into trash/, each with its
// vouch. A file gone already is no failure: a sweep may have moved it first.
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

// historyGone reports whether the log under inc, as a FULL round's snap shows
// it, is no longer the history its index was read from: marked by a round
// (incarnation.historyGone, taken here), or another stream or generation, or
// the index past snap's position. e.mu is not held.
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
// round dropped what its history or checks asked: no index gets an empty one
// at the round's position, one behind it widens its gap up to it (widened).
// A record naming a run not on disk at its size is made anew. What it retires
// waits for the round's state (retireRuns), including every run file no
// record names (a crash between a run's rename and its state's), whose numbers
// it skips past so no run is renamed over one. The round holds the write lock;
// the backfill's commits are each decided against this record (commitIndex).
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

// seedIndex seeds a rebuild's index (seedRebuild): every run of the replaced
// incarnation's index hard-linked in (SeedLink) with its record and vouches (a
// link is the same inode), so a vouched run is read at once and any other is
// checked by the backfill first. Nothing for an index whose log a round
// marked gone (incarnation.historyGone). A run that cannot be linked seeds
// nothing and the rebuild goes on: the new incarnation's first full round
// trashes the stray links and makes an index anew.
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
		// The backfill committed into it while it was linked: a run it
		// names may have no link. The new incarnation reads its own.
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
