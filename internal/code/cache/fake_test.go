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

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE SNAPSHOT CONTRACT AS THE CACHE CONSUMES IT (code.Reader.Snapshot), over
// rows a case writes directly, so a case controls exactly when a page moves.
// Nothing here is invented: page 1 decides full or delta as the reader does
// (no Since, another stream name or generation, or a Since below the swept
// tombstones or the swept retirements), and the floor the snapshot's
// retirements are served above (a delta's Since; a full answer's
// RetiredSince where it is on this generation, at or past the retired mark
// and no more than code.MaxFullRetirements below the checkpoint — else none),
// serves the phases packs, refs, a delta's pushes, the retirements and — on a
// snapshot of more than one page — tail, in the reader's orders and with its
// cursor rules, and a later page answers code.ErrSnapshotMoved when the
// stream, the generation, (on a delta) the tombstone floor or the retired
// floor moved since the cursor was made. The fake is held honest from the
// other side by the host's suites, which run the cache against the real
// reader.

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
	// reads is every read of pushes in log order answered (Read), and
	// readHook runs before each is answered — outside the log's lock — its
	// error the read's.
	reads    []code.ReadQuery
	readHook func(code.ReadQuery) error
	// pushes counts the push rows ever written, which names each one.
	pushes int
}

// fakeRepo is one repository's rows.
type fakeRepo struct {
	ref               code.RepoRef
	packs             []code.PackRow
	refs              map[string]fakeRef
	tombstonesThrough int64
	// pushes is every push row the repository still keeps, as the reader
	// serves one (code.IntroducedRow), in the order they were written.
	pushes []code.IntroducedRow
	// retired is every retirement the rows still keep (code_packs_retired),
	// as the reader serves one (code.RetiredRow), in the order the records
	// made them; retiredThrough is the packed position at or below which a
	// sweep took them out, so no retirement there is served any more.
	retired        []code.RetiredRow
	retiredThrough int64
	// layers is the repository's published chain as the rows keep it
	// (code_layers: code.LayerRow, root first), and layersAt the position of
	// the record that last moved it — what the reader serves a delta the
	// chain on (code.RepoSnapshot.LayersServed: a delta whose floor is below
	// it), and every full answer whatever it is.
	layers   []code.LayerRow
	layersAt statelog.Position
}

// fakePush is one push of a record: the packs it added, the refs it moved
// (an empty oid deletes one), the refs it moved by force, and the commits it
// introduced.
type fakePush struct {
	packs      []code.PackRow
	refs       map[string]string
	forced     map[string]bool
	introduced []string
}

// fakePusher is who one in three of a fake log's pushes are made by: a person,
// through the token bound to them, received by git.
var fakePusher = code.Principal{Kind: code.PrincipalOperator, OperatorID: "token:ceo", Seat: "ceo"}

// pusherOf is who made a fake log's n-th push, and how: one in three a seat's
// push the engine built, its run naming its turn, its work item and its model;
// one in three a coding box's push git received, its run naming as well the
// run that pushed and its coding agent; and the rest fakePusher's — so every
// index a case builds holds every kind of push, and a case that holds an index
// to the rows (indexAgainstLog) compares each push's whole PushedBy, its run
// included.
func pusherOf(n int) (code.Principal, code.PushVia) {
	run := code.RunProvenance{
		TurnID:      uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("turn-%d", n))).String(),
		WorkBackend: "native", WorkKey: fmt.Sprintf("ENG-%d", n), Model: "claude-sonnet-4-5",
	}
	switch n % 3 {
	case 0:
		return code.Principal{Kind: code.PrincipalAgent, Seat: "docs-writer", Run: run}, code.ViaEngine
	case 2:
		run.RunID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("run-%d", n))).String()
		run.CodingAgent = "claude-code"
		return code.Principal{Kind: code.PrincipalAgent, Seat: "swe", Run: run}, code.ViaGit
	}
	return fakePusher, code.ViaGit
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
	// RetiredSince is page 1's StateQuery.RetiredSince: the floor a full
	// answer was asked to serve its retirements above.
	RetiredSince statelog.Position
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
			// Searched by index, as a declared repository is by default: a
			// row always names its mode.
			DefaultBranch: "main", Search: code.SearchIndexed,
		},
		refs: map[string]fakeRef{},
	}
}

// write is one record on repo: packs added and refs moved (an empty oid
// deletes one), at one position — one push, which introduced no commit the
// case names.
func (f *fakeLog) write(repo string, packs []code.PackRow, refs map[string]string) statelog.Position {
	return f.writePushes(repo, fakePush{packs: packs, refs: refs})
}

// writePushes is one record on repo holding every push of pushes, in the order
// group commit decided them, at one position: each push's packs added, its
// refs moved and its row written, as the code domain's apply writes them.
func (f *fakeLog) writePushes(repo string, pushes ...fakePush) statelog.Position {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	pos := f.at()
	r := f.repos[repo]
	for _, p := range pushes {
		for _, pack := range p.packs {
			pack.Added = pos
			r.packs = append(r.packs, pack)
		}
		f.pushes++
		principal, via := pusherOf(f.pushes)
		row := code.IntroducedRow{
			PushedBy: code.PushedBy{
				PushID:    uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("push-%d", f.pushes))).String(),
				Position:  pos,
				At:        time.Unix(1_800_000_000+int64(f.pushes), 0).UTC(),
				Principal: principal, Via: via,
			},
			Introduced: slices.Clone(p.introduced), Count: len(p.introduced),
		}
		for _, ref := range slices.Sorted(maps.Keys(p.refs)) {
			oid := p.refs[ref]
			old := r.refs[ref].oid
			if old == "" {
				old = code.ZeroOID
			}
			nw := oid
			if nw == "" {
				nw = code.ZeroOID
			}
			row.Moves = append(row.Moves, code.RefMove{Ref: ref, Old: old, New: nw, Forced: p.forced[ref]})
			r.refs[ref] = fakeRef{oid: oid, at: pos}
		}
		if len(p.packs) > 0 || len(p.refs) > 0 {
			r.pushes = append(r.pushes, row)
		}
	}
	return pos
}

// compact is one compaction record on repo, as the code domain's apply of one
// writes it: the packs whose ids retire leave the live rows for the retired
// ones, and outputs are added, at one position — which it answers.
func (f *fakeLog) compact(repo string, outputs []code.PackRow, retire ...string) statelog.Position {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	pos := f.at()
	f.retireAndAdd(repo, pos, outputs, retire)
	return pos
}

// retireAndAdd is the pack half of a record on repo at pos — a compaction's,
// a publication's: the packs whose ids retire leave the live rows for the
// retired ones, and outputs are added. f.mu is held.
func (f *fakeLog) retireAndAdd(repo string, pos statelog.Position, outputs []code.PackRow, retire []string) {
	r := f.repos[repo]
	f.retire(repo, pos, retire...)
	for _, out := range outputs {
		out.Added = pos
		r.packs = append(r.packs, out)
	}
}

// retire moves the pack rows of repo whose ids are ids out of the live rows
// for the retired ones, at pos, as every record that retires a pack does, and
// answers the checksums they named; f.mu is held.
func (f *fakeLog) retire(repo string, pos statelog.Position, ids ...string) map[string]bool {
	r := f.repos[repo]
	gone := map[string]bool{}
	for _, id := range ids {
		i := slices.IndexFunc(r.packs, func(p code.PackRow) bool { return p.PackID == id })
		if i < 0 {
			panic(fmt.Sprintf("the fake log: %s holds no pack %s to retire", repo, id))
		}
		p := r.packs[i]
		r.packs = slices.Delete(r.packs, i, i+1)
		r.retired = append(r.retired, code.RetiredRow{PackID: p.PackID, Part: p.Part, GitChecksum: p.GitChecksum,
			RetiredAt: time.Unix(1_800_000_000+int64(f.seq), 0).UTC(), Position: pos})
		gone[p.GitChecksum] = true
	}
	return gone
}

// expungeBegin is one expunge begin record on repo, as the code domain's apply
// of one writes it (code.ExpungeBegin): its inputs — the live packs whose ids
// retire — leave the live rows for the retired ones, kept are added live and
// dropping added DROPPING (code.PackDropping), the repository's drop epoch
// moves one up, and the published chain is cut from the lowest layer naming
// an input (its DropLayers) — at one position, which it answers. A record of
// its own that moves no ref.
func (f *fakeLog) expungeBegin(repo string, kept, dropping []code.PackRow, retire ...string) statelog.Position {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	pos := f.at()
	r := f.repos[repo]
	for _, id := range retire {
		if i := slices.IndexFunc(r.packs, func(p code.PackRow) bool { return p.PackID == id }); i >= 0 &&
			r.packs[i].State != code.PackLive {
			panic(fmt.Sprintf("the fake log: %s's pack %s is %s, and an expunge's input is live", repo, id,
				r.packs[i].State))
		}
	}
	inputs := f.retire(repo, pos, retire...)
	for _, out := range kept {
		out.Added, out.State, out.Kind = pos, code.PackLive, code.PackExpunge
		r.packs = append(r.packs, out)
	}
	for _, out := range dropping {
		out.Added, out.State, out.Kind = pos, code.PackDropping, code.PackCruft
		r.packs = append(r.packs, out)
	}
	r.ref.DropEpoch++
	if k := slices.IndexFunc(r.layers, func(l code.LayerRow) bool {
		return slices.ContainsFunc(l.Layer.Packs, func(cs string) bool { return inputs[cs] })
	}); k >= 0 {
		r.layers, r.layersAt = slices.Clone(r.layers[:k]), pos
	}
	return pos
}

// expungeCommit is one expunge commit record on repo, as the code domain's
// apply of one writes it (code.ExpungeCommit): every dropping pack retired,
// and rescue — what of them a ref reached again — added live, at one
// position, which it answers. The drop epoch stays: the commit closes the
// expunge its begin opened.
func (f *fakeLog) expungeCommit(repo string, rescue ...code.PackRow) statelog.Position {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	pos := f.at()
	r := f.repos[repo]
	var ids []string
	for _, p := range r.packs {
		if p.State == code.PackDropping {
			ids = append(ids, p.PackID)
		}
	}
	f.retire(repo, pos, ids...)
	for _, out := range rescue {
		out.Added, out.State, out.Kind = pos, code.PackLive, code.PackRescue
		r.packs = append(r.packs, out)
	}
	return pos
}

// publish is one publication record on repo, as the code domain's apply of
// one writes it: the published chain made chain — each layer a row in turn,
// Seq 1 up, a layer the chain held before keeping the position of the record
// that added it and every other added at this one — the packs whose ids
// retire leaving the live rows for the retired ones, and outputs added, at
// one position, which it answers. A record of its own that moves no ref.
func (f *fakeLog) publish(repo string, chain []code.LayerRef, outputs []code.PackRow,
	retire ...string) statelog.Position {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	pos := f.at()
	f.retireAndAdd(repo, pos, outputs, retire)
	r := f.repos[repo]
	rows := make([]code.LayerRow, len(chain))
	for i, l := range chain {
		added := pos
		if i < len(r.layers) && r.layers[i].Layer.LayerID == l.LayerID && r.layers[i].Layer.BaseID == l.BaseID {
			added = r.layers[i].Added
		}
		l.Replaces = nil
		rows[i] = code.LayerRow{Seq: i + 1, Layer: l, Added: added}
	}
	r.layers, r.layersAt = rows, pos
	return pos
}

// chainOf is repo's published chain as the rows hold it now.
func (f *fakeLog) chainOf(repo string) []code.LayerRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.repos[repo].layers)
}

// sweepRetired takes repo's retirements at or below through out of the rows,
// as a retire_sweep record does, raising the mark below which a delta is
// answered in full — a record of its own, which moves the checkpoint.
func (f *fakeLog) sweepRetired(repo string, through statelog.Position) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[repo]
	r.retired = slices.DeleteFunc(r.retired, func(x code.RetiredRow) bool {
		return x.Position.Packed() <= through.Packed()
	})
	r.retiredThrough = max(r.retiredThrough, through.Packed())
	f.seq++
}

// dropPack takes repo's pack id out of the live rows with no retirement —
// what no record of the code domain does, and so what a cache meets as a
// pack file no row accounts for.
func (f *fakeLog) dropPack(repo, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[repo]
	r.packs = slices.DeleteFunc(r.packs, func(p code.PackRow) bool { return p.PackID == id })
	f.seq++
}

// expirePushes drops repo's push rows at or below through, as the push
// history's horizon drops the oldest (code.PushHistory) — a record of its
// own, which moves the checkpoint.
func (f *fakeLog) expirePushes(repo string, through statelog.Position) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[repo]
	r.pushes = slices.DeleteFunc(r.pushes, func(p code.IntroducedRow) bool {
		return p.Position.Packed() <= through.Packed()
	})
	f.seq++
}

// pushRows is repo's push rows as the log holds them now, in (position,
// push id) order.
func (f *fakeLog) pushRows(repo string) []code.IntroducedRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[repo]
	if r == nil {
		return nil
	}
	return sortedPushes(r.pushes)
}

// landedRows is repo's push rows as the log holds them now, in the order they
// landed: by position, and within one record in the order group commit
// decided them.
func (f *fakeLog) landedRows(repo string) []code.IntroducedRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[repo]
	if r == nil {
		return nil
	}
	return slices.Clone(r.pushes)
}

// sortedPushes is pushes in (position, push id) order, as the reader serves
// them.
func sortedPushes(pushes []code.IntroducedRow) []code.IntroducedRow {
	out := slices.Clone(pushes)
	slices.SortStableFunc(out, func(a, b code.IntroducedRow) int {
		return cmp.Or(cmp.Compare(a.Position.Packed(), b.Position.Packed()), strings.Compare(a.PushID, b.PushID))
	})
	return out
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
		c.pushes = slices.Clone(r.pushes)
		c.retired = slices.Clone(r.retired)
		c.layers = slices.Clone(r.layers)
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

func (s silent) Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	return s.f.read(ctx, q, false)
}

// Read answers one page of a repository's pushes in log order — the one read
// a cache makes of the code host (code.ReadIntroduced), its backfill's — as
// the reader serves it: (After, Through] ascending, Through zero for the
// checkpoint, or below Before descending; keyset after the cursor
// "<packed position>/<push id>"; at most Limit rows, or pageRows, or
// code.MaxSnapshotItems, with a cursor where more remain. A repository the log
// does not hold is code.ErrRepoNotFound; a session read names its floor.
func (f *fakeLog) Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	return f.read(ctx, q, true)
}

func (f *fakeLog) read(ctx context.Context, q code.ReadQuery, loud bool) (code.ReadAnswer, error) {
	if err := ctx.Err(); err != nil {
		return code.ReadAnswer{}, err
	}
	if q.Kind == code.ReadIndexed {
		// THE INDEXER'S LISTING, as code_repos_search_idx answers it: the
		// active and archived repositories searched by index, by name, in
		// one page.
		f.mu.Lock()
		defer f.mu.Unlock()
		var out code.ReadAnswer
		for _, r := range f.repos {
			if r.ref.Search == code.SearchIndexed && r.ref.Name > q.Cursor &&
				(r.ref.Status == code.RepoStatusActive || r.ref.Status == code.RepoStatusArchived) {
				out.Indexed = append(out.Indexed, code.IndexedRepo{ID: r.ref.ID, Name: r.ref.Name})
			}
		}
		slices.SortFunc(out.Indexed, func(a, b code.IndexedRepo) int { return strings.Compare(a.Name, b.Name) })
		return out, nil
	}
	in := q.Introduced
	switch {
	case q.Kind != code.ReadIntroduced || in == nil:
		return code.ReadAnswer{}, fmt.Errorf("%w: the fake log answers introduced and indexed reads alone, not %q",
			code.ErrInvalid, q.Kind)
	case q.Fresh.Level == statelog.ReadSession && q.Fresh.Floor(topics.CodeLogStream).IsZero():
		return code.ReadAnswer{}, fmt.Errorf("%w: a session read with no floor", code.ErrBadLevel)
	}
	var hook func(code.ReadQuery) error
	if loud {
		f.mu.Lock()
		f.reads = append(f.reads, q)
		hook = f.readHook
		f.mu.Unlock()
	}
	if hook != nil {
		if err := hook(q); err != nil {
			return code.ReadAnswer{}, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[in.RepoID]
	if r == nil {
		return code.ReadAnswer{}, fmt.Errorf("%w: %s", code.ErrRepoNotFound, in.RepoID)
	}
	through := in.Through.Packed()
	if in.Through.IsZero() {
		through = f.at().Packed()
	}
	rows := sortedPushes(r.pushes)
	if in.Descending {
		slices.Reverse(rows)
	}
	key := func(p code.IntroducedRow) string { return fmt.Sprintf("%020d/%s", p.Position.Packed(), p.PushID) }
	limit := cmp.Or(q.Limit, f.pageRows, code.MaxSnapshotItems)
	var out code.ReadAnswer
	for _, p := range rows {
		at := p.Position.Packed()
		switch {
		case in.Descending && at >= in.Before.Packed():
			continue
		case !in.Descending && (at <= in.After.Packed() || at > through):
			continue
		case q.Cursor != "" && !in.Descending && key(p) <= q.Cursor:
			continue
		case q.Cursor != "" && in.Descending && key(p) >= q.Cursor:
			continue
		}
		if len(out.Introduced) == limit {
			out.Next = key(out.Introduced[len(out.Introduced)-1])
			break
		}
		out.Introduced = append(out.Introduced, p)
	}
	return out, nil
}

// readsOf is every read of pushes answered for repo, in order.
func (f *fakeLog) readsOf(repo string) []code.ReadQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []code.ReadQuery
	for _, q := range f.reads {
		if q.Introduced != nil && q.Introduced.RepoID == repo {
			out = append(out, q)
		}
	}
	return out
}

func (f *fakeLog) answer(ctx context.Context, q code.StateQuery, loud bool) (code.RepoSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return code.RepoSnapshot{}, err
	}
	f.mu.Lock()
	call := fakeCall{Repo: q.RepoID, Level: q.Fresh.Level, Since: q.Since, Page: 1, RetiredSince: q.RetiredSince}
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
		case q.Fresh.Level == statelog.ReadSession && q.Fresh.Floor(topics.CodeLogStream).IsZero():
			return code.RepoSnapshot{}, fmt.Errorf("%w: a session read with no floor", code.ErrBadLevel)
		}
		full := q.Since.IsZero() || q.Since.Stream != topics.CodeLogStream ||
			q.Since.Generation != f.gen || q.Since.Packed() < r.tombstonesThrough ||
			q.Since.Packed() < r.retiredThrough
		// The floor the retirements are served above: a delta's own Since; a
		// full answer's RetiredSince only where every retirement above it
		// can be served — else none, so a floor named is a complete one.
		var retiredFrom statelog.Position
		switch {
		case !full:
			retiredFrom = q.Since
		case !q.RetiredSince.IsZero() && q.RetiredSince.Stream == topics.CodeLogStream &&
			q.RetiredSince.Generation == f.gen && q.RetiredSince.Packed() >= r.retiredThrough:
			above := 0
			for _, x := range r.retired {
				if x.Position.Packed() > q.RetiredSince.Packed() {
					above++
				}
			}
			if above <= code.MaxFullRetirements {
				retiredFrom = q.RetiredSince
			}
		}
		if f.behindBy > 0 && q.Fresh.Level != statelog.ReadLinearizable {
			// No rows, and so — on a full answer, which serves the chain
			// whatever it is — an empty chain over no pack.
			behind := cp
			behind.Seq -= min(f.behindBy, cp.Seq)
			return code.RepoSnapshot{Repo: f.repoRef(r, behind), Position: behind,
				StreamCreated: f.stream, Full: full, RetiredFrom: retiredFrom, LayersServed: full}, nil
		}
		cur = code.StateCursor{Full: full, Phase: "packs", From: cp, Seen: cp, Stream: f.stream,
			RetiredFrom: retiredFrom}
		if !full {
			cur.Since, cur.Pushes = q.Since, q.Pushes
		}
	} else {
		cur = *q.Cursor
		switch {
		case !cur.Stream.Equal(f.stream) || cur.From.Generation != f.gen:
			return code.RepoSnapshot{}, fmt.Errorf("%w: the log was recreated or reanchored", code.ErrSnapshotMoved)
		case !cur.Full && cur.Since.Packed() < r.tombstonesThrough:
			return code.RepoSnapshot{}, fmt.Errorf("%w: a deletion the delta needed was swept", code.ErrSnapshotMoved)
		case !cur.RetiredFrom.IsZero() && cur.RetiredFrom.Packed() < r.retiredThrough:
			return code.RepoSnapshot{}, fmt.Errorf("%w: a retirement the snapshot serves was swept",
				code.ErrSnapshotMoved)
		}
		cur.Seen = cp
	}
	out := code.RepoSnapshot{Repo: f.repoRef(r, cp), Position: cur.From, StreamCreated: cur.Stream, Full: cur.Full,
		RetiredFrom: cur.RetiredFrom}
	if first && (cur.Full || r.layersAt.Packed() > cur.Since.Packed()) {
		// THE CHAIN, page 1's first phase, whole: on every full answer, an
		// empty one included, and on a delta whose floor is below the record
		// that last moved it (the reader's layers_position).
		out.Layers, out.LayersServed = slices.Clone(r.layers), true
	}
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
			case cur.Phase == "refs" && cur.Pushes:
				cur.Phase, cur.After = "pushes", ""
				continue
			case (cur.Phase == "refs" || cur.Phase == "pushes") && !cur.RetiredFrom.IsZero():
				cur.Phase, cur.After = "retired", ""
				continue
			case (cur.Phase == "refs" || cur.Phase == "pushes" || cur.Phase == "retired") && !first:
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
	case "pushes":
		// Every push in (Since, From], in (position, push id) order: a push
		// past page 1's checkpoint is the next delta's, as the reader serves
		// it.
		var out []keyed
		for _, p := range sortedPushes(r.pushes) {
			if at := p.Position.Packed(); at > cur.Since.Packed() && at <= cur.From.Packed() {
				out = append(out, keyed{key: fmt.Sprintf("%020d/%s", at, p.PushID),
					add: func(s *code.RepoSnapshot) { s.Pushes = append(s.Pushes, p) }})
			}
		}
		return out
	case "retired":
		// Every retirement in (RetiredFrom, From], in (position, pack id,
		// part) order: one past page 1's checkpoint is the next delta's.
		var out []keyed
		for _, x := range r.retired {
			if at := x.Position.Packed(); at > cur.RetiredFrom.Packed() && at <= cur.From.Packed() {
				out = append(out, keyed{key: fmt.Sprintf("%020d/%s/%s", at, x.PackID, x.Part),
					add: func(s *code.RepoSnapshot) { s.Retired = append(s.Retired, x) }})
			}
		}
		slices.SortFunc(out, func(a, b keyed) int { return strings.Compare(a.key, b.key) })
		return out
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
	// A dropping pack is never counted among the live ones (code.PackDropping).
	ref.LivePacks, ref.LiveBytes = 0, 0
	for _, p := range r.packs {
		if p.State != code.PackDropping {
			ref.LivePacks++
			ref.LiveBytes += p.Size
		}
	}
	ref.Refs = 0
	for _, x := range r.refs {
		if x.oid != "" {
			ref.Refs++
		}
	}
	return ref
}
