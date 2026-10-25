package cache_test

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE SNAPSHOT CONTRACT AS THE CACHE CONSUMES IT (code.Reader.Snapshot), over
// rows a case writes directly, so a case controls exactly when a page moves.
// Nothing here is invented: page 1 decides full or delta as the reader does
// (no Since, another stream name or generation, or a Since below the swept
// tombstones), serves the phases packs, refs and — on a snapshot of more than
// one page — tail, in the reader's orders and with its cursor rules, and a
// later page answers code.ErrSnapshotMoved when the stream, the generation or
// (on a delta) the tombstone floor moved since the cursor was made. The fake
// is held honest from the other side by the host's suites, which run the
// cache against the real reader.

// fakeLog is one code log's rows.
type fakeLog struct {
	mu sync.Mutex
	// stream is the log's identity: what every answer's StreamCreated is.
	stream time.Time
	gen    uint32
	// seq is the checkpoint: every write advances it, any repository's.
	seq   uint64
	repos map[string]*fakeRepo
	// pageRows is the rows a page holds; zero is code.MaxSnapshotItems.
	pageRows int
	calls    []fakeCall
	pages    map[*code.StateCursor]int
	// hook runs before a call is answered — outside the log's lock, so it
	// may block, fail, or write through the log's writers — and its error
	// is the call's.
	hook func(fakeCall) error
	// after runs once a call's answer is computed and before it is returned:
	// it may block, or alter the answer.
	after func(fakeCall, *code.RepoSnapshot)
	// behindBy makes a non-linearizable page 1 answer this many records
	// behind the checkpoint, with no rows: a copy that has not caught up.
	behindBy uint64
}

// fakeRepo is one repository's rows.
type fakeRepo struct {
	ref               code.RepoRef
	packs             []code.PackRow
	refs              map[string]fakeRef
	tombstonesThrough int64
}

// fakeRef is one ref's row; an empty oid is a tombstone.
type fakeRef struct {
	oid string
	at  statelog.Position
}

// fakeCall is one call the log answered.
type fakeCall struct {
	Repo  string
	Level statelog.ReadLevel
	Since statelog.Position
	Page  int
}

// fakeState is a copy of the log's rows at one moment, for a restore.
type fakeState struct {
	seq   uint64
	gen   uint32
	repos map[string]*fakeRepo
}

func newFakeLog() *fakeLog {
	return &fakeLog{
		stream: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		repos:  map[string]*fakeRepo{}, pages: map[*code.StateCursor]int{},
	}
}

// at is the checkpoint as a position; f.mu is held.
func (f *fakeLog) at() statelog.Position {
	return statelog.Position{Stream: topics.CodeLogStream, Generation: f.gen, Seq: f.seq}
}

// create makes a repository with no refs and no packs.
func (f *fakeLog) create(id, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.repos[id] = &fakeRepo{
		ref: code.RepoRef{
			ID: id, Name: name, Status: code.RepoStatusActive, ManagedBy: code.ManagedConfig,
			DefaultBranch: "main",
		},
		refs: map[string]fakeRef{},
	}
}

// write is one record on repo: packs added and refs moved (an empty oid
// deletes one), at one position.
func (f *fakeLog) write(repo string, packs []code.PackRow, refs map[string]string) statelog.Position {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	pos := f.at()
	r := f.repos[repo]
	for _, p := range packs {
		p.Added = pos
		r.packs = append(r.packs, p)
	}
	for ref, oid := range refs {
		r.refs[ref] = fakeRef{oid: oid, at: pos}
	}
	return pos
}

// other is a record on another repository: the checkpoint moves, and
// nothing of any repository here.
func (f *fakeLog) other() statelog.Position {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	return f.at()
}

// sweep drops repo's tombstones, raising the floor below which a delta can
// no longer carry a deletion.
func (f *fakeLog) sweep(repo string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[repo]
	maps.DeleteFunc(r.refs, func(_ string, ref fakeRef) bool { return ref.oid == "" })
	r.tombstonesThrough = f.at().Packed()
	f.seq++
}

// settings moves repo's default branch.
func (f *fakeLog) settings(repo, branch string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.repos[repo].ref.DefaultBranch = branch
}

// purge removes repo from the rows.
func (f *fakeLog) purge(repo string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	delete(f.repos, repo)
}

// state copies the rows as they stand.
func (f *fakeLog) state() fakeState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fakeState{seq: f.seq, gen: f.gen, repos: cloneRepos(f.repos)}
}

// recreate is the log made again — provisioned anew, its stamp not the one
// it had (ADR-0053) — holding an earlier state's rows: a new stream identity,
// and the rows and the checkpoint the state held.
func (f *fakeLog) recreate(st fakeState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stream = f.stream.Add(time.Hour)
	f.seq, f.gen, f.repos = st.seq, st.gen, cloneRepos(st.repos)
}

// restore is the log brought back from an earlier copy — a broker restored
// from a backup's snapshot, or yesterday's volume: the SAME stream identity
// (the stamp rides the snapshot's configuration, ADR-0053), and the rows and
// the checkpoint the copy held, so the positions past it name other records
// once the log is written again.
func (f *fakeLog) restore(st fakeState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq, f.gen, f.repos = st.seq, st.gen, cloneRepos(st.repos)
}

func cloneRepos(in map[string]*fakeRepo) map[string]*fakeRepo {
	out := make(map[string]*fakeRepo, len(in))
	for id, r := range in {
		c := *r
		c.packs = slices.Clone(r.packs)
		c.refs = maps.Clone(r.refs)
		out[id] = &c
	}
	return out
}

// callsOf is every call answered for repo, in order.
func (f *fakeLog) callsOf(repo string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.Repo == repo {
			out = append(out, c)
		}
	}
	return out
}

// firstPages is every page-1 call answered for repo, in order.
func (f *fakeLog) firstPages(repo string) []fakeCall {
	var out []fakeCall
	for _, c := range f.callsOf(repo) {
		if c.Page == 1 {
			out = append(out, c)
		}
	}
	return out
}

// set changes the log's knobs under its lock.
func (f *fakeLog) set(fn func(*fakeLog)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// RepoState answers one page, recording the call.
func (f *fakeLog) RepoState(ctx context.Context, q code.StateQuery) (code.RepoSnapshot, error) {
	return f.answer(ctx, q, true)
}

// silent is the same rows answered without recording a call or running a
// hook: what a case's own reference cache reads.
type silent struct{ f *fakeLog }

func (s silent) RepoState(ctx context.Context, q code.StateQuery) (code.RepoSnapshot, error) {
	return s.f.answer(ctx, q, false)
}

func (f *fakeLog) answer(ctx context.Context, q code.StateQuery, loud bool) (code.RepoSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return code.RepoSnapshot{}, err
	}
	f.mu.Lock()
	call := fakeCall{Repo: q.RepoID, Level: q.Fresh.Level, Since: q.Since, Page: 1}
	if q.Cursor != nil {
		call.Level, call.Page = statelog.ReadSession, f.pages[q.Cursor]
	}
	var hook func(fakeCall) error
	var after func(fakeCall, *code.RepoSnapshot)
	if loud {
		f.calls = append(f.calls, call)
		hook, after = f.hook, f.after
	}
	f.mu.Unlock()
	if hook != nil {
		if err := hook(call); err != nil {
			return code.RepoSnapshot{}, err
		}
	}
	f.mu.Lock()
	snap, err := f.page(q, call)
	if err == nil && snap.Next != nil {
		f.pages[snap.Next] = call.Page + 1
	}
	f.mu.Unlock()
	if err == nil && after != nil {
		after(call, &snap)
	}
	return snap, err
}

// page is one page of the snapshot q asks for; f.mu is held.
func (f *fakeLog) page(q code.StateQuery, call fakeCall) (code.RepoSnapshot, error) {
	r := f.repos[q.RepoID]
	if r == nil {
		return code.RepoSnapshot{}, fmt.Errorf("%w: %s", code.ErrRepoNotFound, q.RepoID)
	}
	cp := f.at()
	var cur code.StateCursor
	first := q.Cursor == nil
	if first {
		switch {
		case q.Fresh.Level == "" || !q.Fresh.Level.Valid():
			return code.RepoSnapshot{}, fmt.Errorf("%w: %q", code.ErrBadLevel, q.Fresh.Level)
		case q.Fresh.Level == statelog.ReadSession && q.Fresh.MinPosition.IsZero():
			return code.RepoSnapshot{}, fmt.Errorf("%w: a session read with no floor", code.ErrBadLevel)
		}
		full := q.Since.IsZero() || q.Since.Stream != topics.CodeLogStream ||
			q.Since.Generation != f.gen || q.Since.Packed() < r.tombstonesThrough
		if f.behindBy > 0 && q.Fresh.Level != statelog.ReadLinearizable {
			behind := cp
			behind.Seq -= min(f.behindBy, cp.Seq)
			return code.RepoSnapshot{Repo: f.repoRef(r, behind), Position: behind,
				StreamCreated: f.stream, Full: full}, nil
		}
		cur = code.StateCursor{Full: full, Phase: "packs", From: cp, Seen: cp, Stream: f.stream}
		if !full {
			cur.Since = q.Since
		}
	} else {
		cur = *q.Cursor
		switch {
		case !cur.Stream.Equal(f.stream) || cur.From.Generation != f.gen:
			return code.RepoSnapshot{}, fmt.Errorf("%w: the log was recreated or reanchored", code.ErrSnapshotMoved)
		case !cur.Full && cur.Since.Packed() < r.tombstonesThrough:
			return code.RepoSnapshot{}, fmt.Errorf("%w: a deletion the delta needed was swept", code.ErrSnapshotMoved)
		}
		cur.Seen = cp
	}
	out := code.RepoSnapshot{Repo: f.repoRef(r, cp), Position: cur.From, StreamCreated: cur.Stream, Full: cur.Full}
	room := f.pageRows
	if room == 0 {
		room = code.MaxSnapshotItems
	}
	for {
		rows := f.phase(r, cur)
		var after []keyed
		for _, k := range rows {
			if cur.After == "" || k.key > cur.After {
				after = append(after, k)
			}
		}
		served := min(room, len(after))
		for _, k := range after[:served] {
			k.add(&out)
			cur.After = k.key
		}
		room -= served
		if served == len(after) && room > 0 {
			// The phase is complete: a statement answered fewer rows than
			// the page had room for.
			switch {
			case cur.Phase == "packs":
				cur.Phase, cur.After = "refs", ""
				continue
			case cur.Phase == "refs" && !first:
				cur.Phase, cur.After = "tail", ""
				continue
			}
			return out, nil
		}
		next := cur
		out.Next = &next
		return out, nil
	}
}

// keyed is one row of a phase with its cursor key.
type keyed struct {
	key string
	add func(*code.RepoSnapshot)
}

// phase is every row of cur's phase, in the reader's order, keyed as its
// cursor keys them.
func (f *fakeLog) phase(r *fakeRepo, cur code.StateCursor) []keyed {
	addedKey := func(p code.PackRow) string {
		return fmt.Sprintf("%020d/%s/%s", p.Added.Packed(), p.PackID, p.Part)
	}
	packs := func(keep func(code.PackRow) bool, key func(code.PackRow) string) []keyed {
		var out []keyed
		for _, p := range r.packs {
			if keep(p) {
				out = append(out, keyed{key: key(p), add: func(s *code.RepoSnapshot) { s.Packs = append(s.Packs, p) }})
			}
		}
		slices.SortFunc(out, func(a, b keyed) int { return strings.Compare(a.key, b.key) })
		return out
	}
	switch cur.Phase {
	case "packs":
		if cur.Full {
			return packs(func(code.PackRow) bool { return true },
				func(p code.PackRow) string { return p.PackID + "/" + p.Part })
		}
		return packs(func(p code.PackRow) bool { return p.Added.Packed() > cur.Since.Packed() }, addedKey)
	case "tail":
		return packs(func(p code.PackRow) bool { return p.Added.Packed() > cur.From.Packed() }, addedKey)
	}
	var out []keyed
	for name, ref := range r.refs {
		row := code.RefRow{Ref: name, OID: ref.oid, Position: ref.at}
		add := func(s *code.RepoSnapshot) { s.Refs = append(s.Refs, row) }
		switch {
		case cur.Full && ref.oid != "":
			out = append(out, keyed{key: name, add: add})
		case !cur.Full && ref.at.Packed() > cur.Since.Packed():
			out = append(out, keyed{key: fmt.Sprintf("%020d/%s", ref.at.Packed(), name), add: add})
		}
	}
	slices.SortFunc(out, func(a, b keyed) int { return cmp.Compare(a.key, b.key) })
	return out
}

// repoRef is repo's row as a read at cp finds it.
func (f *fakeLog) repoRef(r *fakeRepo, cp statelog.Position) code.RepoRef {
	ref := r.ref
	ref.Position = cp
	ref.LivePacks = int64(len(r.packs))
	ref.LiveBytes = 0
	for _, p := range r.packs {
		ref.LiveBytes += p.Size
	}
	ref.Refs = 0
	for _, x := range r.refs {
		if x.oid != "" {
			ref.Refs++
		}
	}
	return ref
}
