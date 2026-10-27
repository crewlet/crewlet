package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// AN EXPUNGE'S DECIDE AND APPLY, case by case, over one node's estate with no
// broker between: records built here, the decide run in a snapshot of the rows
// a case seeds.

// purger is the person every request here is a purge of: a credential bound
// to their human seat.
var purger = Principal{Kind: PrincipalOperator, OperatorID: "pat:7", Seat: "founder"}

// beginOver is an expunge's begin of repository id at epoch, rewriting retire
// into packs it keeps — one per entry of kept, holding that many objects — and
// packs it leaves dropping — one per entry of dropping — dropping layers, its
// id minted and its outputs' keys at applyClock, as a maintainer builds one.
func beginOver(id string, epoch int64, retire []string, kept, dropping []int64, layers ...string) ExpungeBegin {
	xid := statelog.NewOpID(applyClock, "")
	b := ExpungeBegin{RepoID: id, ExpungeID: xid, Node: "node-a", Seen: codeAt(1), DropEpoch: epoch,
		Grace: PruneGrace, Retire: slices.Clone(retire), DropLayers: layers}
	for i, objects := range kept {
		b.Add = append(b.Add, outputOf(xid, i, objects, applyClock))
	}
	for i, objects := range dropping {
		b.Dropping = append(b.Dropping, outputOf(xid, len(kept)+i, objects, applyClock))
	}
	return b
}

// commitOver is the commit of b, retiring b's dropping packs and keeping a
// rescue pack per entry of rescue, holding that many objects.
func commitOver(b ExpungeBegin, rescue ...int64) ExpungeCommit {
	c := ExpungeCommit{RepoID: b.RepoID, ExpungeID: b.ExpungeID, CommitID: statelog.NewOpID(applyClock, ""),
		RequestID: b.RequestID, Node: "node-a", Seen: codeAt(1), DropEpoch: b.DropEpoch,
		Retire: sorted(idsOf(b.Dropping)...)}
	for i, objects := range rescue {
		c.Add = append(c.Add, outputOf(c.CommitID, i, objects, applyClock))
	}
	return c
}

// requestOf is a purge of repository id asked under the operation id request.
func requestOf(id, request string) ExpungeRequest {
	return ExpungeRequest{RepoID: id, RequestID: request, By: purger}
}

// expungeRec is p as the record that carries it.
func expungeRec(t testing.TB, p ExpungeProposal) built {
	op, _, _, payload := p.record()
	return rec(t, RepoSubject(p.RepoID()), op, Scope{RepoPath(p.RepoID())}, payload)
}

// expungeDecidedIn runs one round of an expunge record's decide over e's rows
// at now.
func expungeDecidedIn(t *testing.T, e *estate, p ExpungeProposal, now time.Time) (expungeDecision, error) {
	t.Helper()
	var d expungeDecision
	err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) error {
		var err error
		d, err = decideExpunge(context.Background(), tx, p, DefaultDeletedRetention, now)
		return err
	})
	return d, err
}

// expunged decides p over e's rows at the decide's clock and applies its
// record when the decide formed one, answering the decision.
func (e *estate) expunged(t *testing.T, p ExpungeProposal) expungeDecision {
	t.Helper()
	d, err := expungeDecidedIn(t, e, p, applyClock)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if d.refusal == "" && !d.collapsed {
		e.apply(expungeRec(t, p))
	}
	return d
}

// mustExpunge is [estate.expunged] of a record the decide must form.
func (e *estate) mustExpunge(t *testing.T, p ExpungeProposal) statelog.Position {
	t.Helper()
	at := codeAt(e.next)
	if d := e.expunged(t, p); d.refusal != "" || d.collapsed {
		t.Fatalf("the %s was refused %s (%s), or answered with an open request, where it is decided",
			opOf(p), d.refusal, d.detail)
	}
	return at
}

// opOf is the op p is, for a case's message.
func opOf(p ExpungeProposal) OpKind {
	op, _, _, _ := p.record()
	return op
}

// repoCounters is what code_repos counts of id's live packs.
func (e *estate) repoCounters(id string) [3]int64 {
	e.t.Helper()
	return [3]int64{e.num(`SELECT live_packs FROM code_repos WHERE id = ?`, id),
		e.num(`SELECT live_bytes FROM code_repos WHERE id = ?`, id),
		e.num(`SELECT live_objects FROM code_repos WHERE id = ?`, id)}
}

// liveRows is what id's live pack rows hold, counted from the rows.
func (e *estate) liveRows(id string) [3]int64 {
	e.t.Helper()
	return [3]int64{e.num(`SELECT COUNT(*) FROM code_packs WHERE repo_id = ? AND state = 'live'`, id),
		e.num(`SELECT COALESCE(SUM(size), 0) FROM code_packs WHERE repo_id = ? AND state = 'live'`, id),
		e.num(`SELECT COALESCE(SUM(objects), 0) FROM code_packs WHERE repo_id = ? AND state = 'live'`, id)}
}

// A BEGIN KEEPS EVERY OBJECT OF ITS INPUTS: its inputs' rows leave code_packs
// for code_packs_retired, stamped with its position and broker instant, and
// its outputs join code_packs — the packs holding what a ref reached live, of
// kind expunge, and the packs holding the rest DROPPING, of kind cruft, still
// rows that name their objects — holding every object the inputs held; the
// repository's drop epoch and open expunge move to the record's; and the live
// counters count the live rows alone. A begin whose outputs hold more objects
// than its inputs is the builder's bug, refused before any record.
//
// Mutation: write the dropping pack live, or count it among the live packs,
// and the rows or the counters say so; delete the inputs rather than retire
// them, and a reader mid-read loses them; drop the arithmetic, and the begin
// that invents an object is decided.
func TestAnExpungeBeginKeepsEveryObjectOfItsInputs(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	e.seed("repo-a", 0, 0, 4)
	retire := []string{idOf("repo-a", 0), idOf("repo-a", 1), idOf("repo-a", 2)}

	invented := beginOver("repo-a", 1, retire, []int64{3}, []int64{1})
	if _, err := expungeDecidedIn(t, e, ExpungeProposal{Begin: &invented}, applyClock); !errors.Is(err,
		ErrInvalidProposal) {
		t.Fatalf("a begin whose outputs hold four objects for its inputs' three was decided: %v", err)
	}

	b := beginOver("repo-a", 1, retire, []int64{2}, []int64{1})
	at := e.mustExpunge(t, ExpungeProposal{Begin: &b})
	for _, id := range retire {
		if n := e.num(`SELECT COUNT(*) FROM code_packs_retired WHERE repo_id = 'repo-a' AND pack_id = ?
			AND retired_position = ? AND retired_at = ? AND kind = 'push' AND objects = 1`, id, at.Packed(),
			store.EncodeTime(applyClock)); n != 1 {
			t.Errorf("input %s is not retired at the begin's position and instant", id)
		}
	}
	for _, tc := range []struct {
		out         PackRef
		kind, state string
		what        string
	}{
		{b.Add[0], "expunge", "live", "the kept pack"},
		{b.Dropping[0], "cruft", "dropping", "the dropping pack"},
	} {
		if n := e.num(`SELECT COUNT(*) FROM code_packs WHERE repo_id = 'repo-a' AND pack_id = ? AND part = 'pack'
			AND kind = ? AND state = ? AND added_position = ? AND object = ? AND objects = ?`, tc.out.PackID,
			tc.kind, tc.state, at.Packed(), tc.out.Object.String(), tc.out.Objects); n != 1 {
			t.Errorf("%s is not a %s row of kind %s added at the begin's position", tc.what, tc.state, tc.kind)
		}
	}
	if got := e.num(`SELECT SUM(objects) FROM code_packs WHERE repo_id = 'repo-a' AND pack_id IN (?, ?)`,
		b.Add[0].PackID, b.Dropping[0].PackID); got != 3 {
		t.Fatalf("the begin's packs hold %d objects, want the inputs' three", got)
	}
	want := [3]int64{2, 10 + b.Add[0].Size, 1 + 2}
	if got := e.repoCounters("repo-a"); got != want || got != e.liveRows("repo-a") {
		t.Fatalf("the live counters are %v and the live rows %v, want both %v: the untouched input and the "+
			"kept pack, never the dropping one", got, e.liveRows("repo-a"), want)
	}
	if epoch, id := e.num(`SELECT drop_epoch FROM code_repos WHERE id = 'repo-a'`),
		e.str(`SELECT expunge_id FROM code_repos WHERE id = 'repo-a'`); epoch != 1 || id != b.ExpungeID {
		t.Fatalf("drop epoch %d and open expunge %q, want 1 and %s", epoch, id, b.ExpungeID)
	}
}

// A COMMIT NEEDS ONLY ITS DROPPING PACKS: a repository whose refs moved after
// the begin — a push landed between the two — still commits, since the
// commit's one precondition is its expunge, open at its epoch with exactly the
// dropping packs it retires. The dropping packs leave for code_packs_retired at
// the commit's position, its rescue joins code_packs live, the open expunge is
// cleared, and the commit's instant and what it took out are recorded: the
// dropping packs' objects and bytes less the rescue's.
//
// Mutation: hold the commit to the refs or the repository's version its begin
// left, and the push between them refuses it; retire the dropping packs by
// deleting their rows, and a reader mid-read loses them.
func TestAnExpungeCommitNeedsOnlyItsDroppingPacks(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	e.seed("repo-a", 0, 0, 3)
	b := beginOver("repo-a", 1, []string{idOf("repo-a", 0), idOf("repo-a", 1), idOf("repo-a", 2)},
		[]int64{1}, []int64{2})
	e.mustExpunge(t, ExpungeProposal{Begin: &b})
	// THE REFS MOVE between the begin and the commit.
	e.apply(pushOf(t, "repo-a", pushEntry("push-between", move("refs/heads/main", ZeroOID, oid("between")))))
	before := e.repoCounters("repo-a")

	c := commitOver(b, 1)
	at := e.mustExpunge(t, ExpungeProposal{Commit: &c})
	if n := e.num(`SELECT COUNT(*) FROM code_packs WHERE repo_id = 'repo-a' AND state = 'dropping'`); n != 0 {
		t.Fatalf("%d dropping packs after the commit, want none", n)
	}
	if n := e.num(`SELECT COUNT(*) FROM code_packs_retired WHERE repo_id = 'repo-a' AND pack_id = ?
		AND retired_position = ? AND kind = 'cruft'`, b.Dropping[0].PackID, at.Packed()); n != 1 {
		t.Fatalf("the dropping pack is not retired at the commit's position")
	}
	if n := e.num(`SELECT COUNT(*) FROM code_packs WHERE repo_id = 'repo-a' AND pack_id = ? AND kind = 'rescue'
		AND state = 'live' AND added_position = ?`, c.Add[0].PackID, at.Packed()); n != 1 {
		t.Fatalf("the rescue is not a live pack of kind rescue added at the commit's position")
	}
	rescue := c.Add[0]
	want := [3]int64{before[0] + 1, before[1] + rescue.Size, before[2] + rescue.Objects}
	if got := e.repoCounters("repo-a"); got != want || got != e.liveRows("repo-a") {
		t.Fatalf("the live counters are %v and the live rows %v, want both %v", got, e.liveRows("repo-a"), want)
	}
	if id, at := e.str(`SELECT expunge_id FROM code_repos WHERE id = 'repo-a'`),
		e.num(`SELECT expunged_at FROM code_repos WHERE id = 'repo-a'`); id != "" ||
		at != store.EncodeTime(applyClock) {
		t.Fatalf("the open expunge is %q and expunged_at %d, want none and the commit's instant", id, at)
	}
	if objects, bytes := e.num(`SELECT expunge_dropped_objects FROM code_repos WHERE id = 'repo-a'`),
		e.num(`SELECT expunge_dropped_bytes FROM code_repos WHERE id = 'repo-a'`); objects != 1 ||
		bytes != b.Dropping[0].Size-rescue.Size {
		t.Fatalf("the commit took out %d objects and %d bytes, want the dropping pack's 2 objects less the "+
			"rescue's 1, and %d bytes less %d", objects, bytes, b.Dropping[0].Size, rescue.Size)
	}
}

// seedLayers writes id's published chain straight into the estate — a case's
// rows, never the applier's — one layer for each list of pack checksums, root
// first, each layer's bytes one part, and counts the packs it covers.
func (e *estate) seedLayers(id string, layers ...[]string) []string {
	e.t.Helper()
	ids := make([]string, len(layers))
	base := ""
	covered := 0
	for i, packs := range layers {
		l := layerOver(fmt.Sprintf("%s-%d", id, i), base, nil, applyClock, packs...)
		encoded, err := json.Marshal(l.Packs)
		if err != nil {
			e.t.Fatal(err)
		}
		e.exec(`INSERT INTO code_layers (repo_id, seq, layer_id, base_id, graph_id, packs, midx_bytes,
			bitmap_bytes, graph_bytes, objects, commits, git, took_ms, added_position)
			VALUES (?, ?, ?, ?, '', ?, ?, ?, 0, ?, 0, ?, ?, 1)`, id, i+1, l.LayerID, base, string(encoded),
			l.MidxBytes, l.BitmapBytes, l.Objects, l.Git, l.TookMs)
		e.exec(`INSERT INTO code_layer_parts (repo_id, layer_id, part, object, hash, size) VALUES (?, ?, 0, ?, ?, ?)`,
			id, l.LayerID, l.Parts[0].Object.String(), string(l.Parts[0].Hash), l.Parts[0].Size)
		ids[i], base = l.LayerID, l.LayerID
		covered += len(packs)
	}
	e.exec(`UPDATE code_repos SET published_packs = ?, published_objects = ? WHERE id = ?`, covered, covered, id)
	return ids
}

// A BEGIN REMOVES THE LAYERS OVER ITS INPUTS: a layer naming a pack the begin
// retires is no layer a git node can load once it applies, and a layer above
// it no layer whose base stands, so a begin drops the chain's top from the
// lowest layer naming an input — exactly those, in chain order, else it is
// stale — with their parts; the published counts are recounted from the rows
// it leaves, and layers_position moves to it so a cache's next delta serves
// the chain whole.
//
// Mutation: drop only the layers naming an input, and the decide's plan of
// the top alone is decided and a layer is left over a missing base; leave
// layers_position where it was, and a cache keeps the dropped layers.
func TestAnExpungeBeginRemovesTheLayersOverItsInputs(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	e.seed("repo-a", 0, 0, 6)
	chain := e.seedLayers("repo-a", []string{sumOf("repo-a", 0), sumOf("repo-a", 1)},
		[]string{sumOf("repo-a", 2), sumOf("repo-a", 3)}, []string{sumOf("repo-a", 4)})
	retire := []string{idOf("repo-a", 2), idOf("repo-a", 5)}
	for name, layers := range map[string][]string{
		"none of them":    nil,
		"the top alone":   chain[2:],
		"the chain whole": chain,
	} {
		b := beginOver("repo-a", 1, retire, []int64{1}, []int64{1}, layers...)
		d, err := expungeDecidedIn(t, e, ExpungeProposal{Begin: &b}, applyClock)
		if err != nil || d.refusal != RefusalStale || !strings.Contains(d.detail, "a publication moved the chain") {
			t.Errorf("a begin dropping %s answered %s (%s), %v; want stale", name, d.refusal, d.detail, err)
		}
	}
	b := beginOver("repo-a", 1, retire, []int64{1}, []int64{1}, chain[1:]...)
	at := e.mustExpunge(t, ExpungeProposal{Begin: &b})
	if got := e.chainOf("repo-a"); !slices.Equal(got, chain[:1]) {
		t.Fatalf("the chain is %v after the begin, want its root alone %v", got, chain[:1])
	}
	if n := e.num(`SELECT COUNT(*) FROM code_layer_parts WHERE repo_id = 'repo-a' AND layer_id != ?`,
		chain[0]); n != 0 {
		t.Fatalf("%d parts of the dropped layers remain", n)
	}
	if packs, objects, layersAt := e.num(`SELECT published_packs FROM code_repos WHERE id = 'repo-a'`),
		e.num(`SELECT published_objects FROM code_repos WHERE id = 'repo-a'`),
		e.num(`SELECT layers_position FROM code_repos WHERE id = 'repo-a'`); packs != 2 || objects != 2 ||
		layersAt != at.Packed() {
		t.Fatalf("published %d packs of %d objects, the chain last moved at %d; want the root's two of two, "+
			"moved at the begin %d", packs, objects, layersAt, at.Packed())
	}
	e.livePublished(t, "repo-a", "the begin")
}

// A PUBLICATION IS REFUSED WHILE A REPOSITORY EXPUNGES — rule 6g: between a
// begin and its commit the live packs need not be closed, a ref a push moved
// before the begin perhaps reaching an object only a dropping pack holds, so a
// layer is published only once the expunge commits. A compaction carrying no
// layer is no publication, and is decided as ever.
//
// Mutation: drop rule 6g, and the publication over the live packs mid-expunge
// is decided.
func TestAPublicationIsRefusedWhileARepositoryExpunges(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	e.seed("repo-a", 0, 0, 3)
	b := beginOver("repo-a", 1, []string{idOf("repo-a", 0)}, []int64{1}, nil)
	e.mustExpunge(t, ExpungeProposal{Begin: &b})
	live := []string{sumOf("repo-a", 1), sumOf("repo-a", 2), b.Add[0].GitChecksum}

	pub := publication("repo-a", e.next-1, layerOver("whole", "", nil, applyClock, live...), nil)
	d, err := compactionDecidedIn(t, e, pub, applyClock)
	if err != nil || d.refusal != RefusalStale || !strings.Contains(d.detail, "expunge "+b.ExpungeID+" is open") {
		t.Fatalf("a publication mid-expunge answered %s (%s), %v; want stale naming the expunge", d.refusal,
			d.detail, err)
	}
	cid := statelog.NewOpID(applyClock, "")
	plain := Compaction{RepoID: "repo-a", CompactionID: cid, Node: "node-a", Seen: codeAt(e.next - 1),
		Retire: []string{idOf("repo-a", 1), idOf("repo-a", 2)}, Add: []PackRef{outputOf(cid, 0, 2, applyClock)}}
	if d, err := compactionDecidedIn(t, e, plain, applyClock); err != nil || d.refusal != "" {
		t.Fatalf("a compaction with no layer mid-expunge answered %s (%s), %v; want it decided", d.refusal,
			d.detail, err)
	}

	c := commitOver(b)
	e.mustExpunge(t, ExpungeProposal{Commit: &c})
	pub = publication("repo-a", e.next-1, layerOver("whole", "", nil, applyClock, live...), nil)
	if d, err := compactionDecidedIn(t, e, pub, applyClock); err != nil || d.refusal != "" {
		t.Fatalf("the publication once the expunge committed answered %s (%s), %v; want it decided", d.refusal,
			d.detail, err)
	}
}

// A SECOND BEGIN IS REFUSED WHILE ONE IS OPEN: a repository expunges once at a
// time, so a begin decided while another has begun and not committed is stale,
// naming the open one — and a record of it that reached the log anyway applies
// as nothing, whole. A begin planned at an epoch the repository has passed is
// stale too, and one at the next epoch, once the first committed, is decided.
//
// Mutation: drop the decide's open-expunge rule, and the second begin is
// decided over the first's dropping packs; drop the apply's, and the record
// that reached the log replaces the open expunge.
func TestASecondBeginIsRefusedWhileOneIsOpen(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	e.seed("repo-a", 0, 0, 4)
	first := beginOver("repo-a", 1, []string{idOf("repo-a", 0)}, []int64{1}, nil)
	e.mustExpunge(t, ExpungeProposal{Begin: &first})

	second := beginOver("repo-a", 2, []string{idOf("repo-a", 1)}, []int64{1}, nil)
	if d := e.expunged(t, ExpungeProposal{Begin: &second}); d.refusal != RefusalStale ||
		!strings.Contains(d.detail, "expunge "+first.ExpungeID+" is open") {
		t.Fatalf("a second begin answered %s (%s), want stale naming the open expunge", d.refusal, d.detail)
	}
	before := e.dump(`SELECT * FROM code_repos`) + e.dump(`SELECT * FROM code_packs ORDER BY pack_id`)
	e.apply(expungeRec(t, ExpungeProposal{Begin: &second}))
	if after := e.dump(`SELECT * FROM code_repos`) + e.dump(`SELECT * FROM code_packs ORDER BY pack_id`); after != before {
		t.Fatalf("a second begin's record applied over an open expunge:\nbefore %s\nafter  %s", before, after)
	}

	c := commitOver(first)
	e.mustExpunge(t, ExpungeProposal{Commit: &c})
	again := beginOver("repo-a", 1, []string{idOf("repo-a", 1)}, []int64{1}, nil)
	if d := e.expunged(t, ExpungeProposal{Begin: &again}); d.refusal != RefusalStale ||
		!strings.Contains(d.detail, "drop epoch 1") {
		t.Fatalf("a begin planned at the epoch the first moved past answered %s (%s), want stale", d.refusal,
			d.detail)
	}
	next := beginOver("repo-a", 2, []string{idOf("repo-a", 1)}, []int64{1}, nil)
	e.mustExpunge(t, ExpungeProposal{Begin: &next})
}

// A COMMIT IS HELD TO ITS EXPUNGE AND ITS DROPPING PACKS: one of an expunge
// that committed already, or of another than the open one, or at another
// epoch, or naming other packs than the repository's dropping ones, is stale;
// a record of it that reached the log anyway applies as nothing; and a commit
// retires a pack it names only where it is dropping, never a live one, which
// a ref may reach.
//
// Mutation: retire every pack the commit names whatever its state, and the
// live pack named beside the dropping one leaves code_packs.
func TestACommitIsHeldToItsExpungeAndItsDroppingPacks(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	e.seed("repo-a", 0, 0, 3)
	b := beginOver("repo-a", 1, []string{idOf("repo-a", 0)}, nil, []int64{1})
	e.mustExpunge(t, ExpungeProposal{Begin: &b})
	for name, edit := range map[string]func(*ExpungeCommit){
		"another expunge's": func(c *ExpungeCommit) { c.ExpungeID = statelog.NewOpID(applyClock, "") },
		"another epoch":     func(c *ExpungeCommit) { c.DropEpoch = 2 },
		"no dropping pack":  func(c *ExpungeCommit) { c.Retire = nil },
		"a live pack beside": func(c *ExpungeCommit) {
			c.Retire = sorted(append(c.Retire, idOf("repo-a", 1))...)
		},
	} {
		c := commitOver(b)
		edit(&c)
		if d := e.expunged(t, ExpungeProposal{Commit: &c}); d.refusal != RefusalStale {
			t.Errorf("a commit of %s answered %s (%s), want stale", name, d.refusal, d.detail)
		}
	}
	// A RECORD NO CORRECT LOG CARRIES: a commit naming a live pack beside
	// the dropping one retires the dropping one alone.
	c := commitOver(b)
	c.Retire = sorted(append(c.Retire, idOf("repo-a", 1))...)
	e.apply(expungeRec(t, ExpungeProposal{Commit: &c}))
	if n := e.num(`SELECT COUNT(*) FROM code_packs WHERE repo_id = 'repo-a' AND pack_id = ?`,
		idOf("repo-a", 1)); n != 1 {
		t.Fatalf("a commit retired the live pack it named")
	}
	if n := e.num(`SELECT COUNT(*) FROM code_packs WHERE repo_id = 'repo-a' AND state = 'dropping'`); n != 0 {
		t.Fatalf("the dropping pack outlived its commit")
	}
	// AND ONCE COMMITTED, the commit again is stale and its record applies
	// as nothing.
	again := commitOver(b)
	if d := e.expunged(t, ExpungeProposal{Commit: &again}); d.refusal != RefusalStale ||
		!strings.Contains(d.detail, "committed already") {
		t.Fatalf("a commit of an expunge that committed answered %s (%s), want stale", d.refusal, d.detail)
	}
	before := e.dump(`SELECT * FROM code_repos`)
	e.apply(expungeRec(t, ExpungeProposal{Commit: &again}))
	if after := e.dump(`SELECT * FROM code_repos`); after != before {
		t.Fatalf("a commit of no open expunge applied:\nbefore %s\nafter  %s", before, after)
	}
}

// AN EXPUNGE REPLAYED FROM ZERO WRITES THE SAME ROWS: its records' applies are
// pure in the record, its position and its instant, so two nodes applying one
// log hold one estate, byte for byte — and a record of the same begin or
// commit met again at a later position, which the version guard does not
// stop, moves nothing.
//
// Mutation: stamp a retired row or expunged_at with the clock rather than the
// record's instant, and the two estates differ.
func TestAnExpungeReplayedFromZeroWritesTheSameRows(t *testing.T) {
	t.Parallel()
	b := beginOver("repo-a", 1, []string{"push-1", "push-2"}, []int64{1}, []int64{1})
	c := commitOver(b, 1)
	records := []built{
		create(t, "repo-a", "a"),
		pushOf(t, "repo-a", pushEntry("push-1", move("refs/heads/main", ZeroOID, oid("one")))),
		pushOf(t, "repo-a", pushEntry("push-2", move("refs/heads/main", oid("one"), oid("two")))),
		expungeRec(t, ExpungeProposal{Request: &ExpungeRequest{RepoID: "repo-a",
			RequestID: statelog.NewOpID(applyClock, "code_purge_unreachable"), By: purger}}).at(applyClock.Add(time.Second)),
		expungeRec(t, ExpungeProposal{Begin: &b}).at(applyClock.Add(2 * time.Second)),
		pushOf(t, "repo-a", pushEntry("push-3", move("refs/heads/main", oid("two"), oid("three")))),
		expungeRec(t, ExpungeProposal{Commit: &c}).at(applyClock.Add(3 * time.Second)),
	}
	rows := func(e *estate) string {
		return e.dump(`SELECT * FROM code_repos`) + e.dump(`SELECT * FROM code_packs ORDER BY pack_id`) +
			e.dump(`SELECT * FROM code_packs_retired ORDER BY pack_id`)
	}
	one, two := openApplyEstate(t), openApplyEstate(t)
	one.apply(records...)
	two.apply(records...)
	if x, y := rows(one), rows(two); x != y {
		t.Fatalf("two nodes applying one log differ:\n%s\n%s", x, y)
	}
	before := rows(one)
	one.apply(expungeRec(t, ExpungeProposal{Begin: &b}), expungeRec(t, ExpungeProposal{Commit: &c}))
	if after := rows(one); after != before {
		t.Fatalf("the begin and the commit met again moved the rows:\nbefore %s\nafter  %s", before, after)
	}
}

// A PURGE REQUEST IS SERVED ONCE NO LIVE PACK PREDATES IT: the repository holds
// it open from its record until a commit leaves no live pack of the repository
// added at or before it — a cycle that rewrites part of what it held keeps it,
// the cycle that rewrites the rest serves it — and a repository holding no
// live pack is served at its own record, since no cycle could rewrite
// anything for it. A request asked while one is open is that one: decided as
// nothing, and answered with the open one's id and position.
//
// Mutation: clear a request at any commit, and the first cycle serves it with
// a pack it never rewrote still live; keep a request for a repository holding
// no pack, and it is open for ever.
func TestAPurgeRequestIsServedOnceNoLivePackPredatesIt(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	request := func() (string, int64) {
		return e.str(`SELECT expunge_request FROM code_repos WHERE id = 'repo-a'`),
			e.num(`SELECT expunge_request_position FROM code_repos WHERE id = 'repo-a'`)
	}
	empty := requestOf("repo-a", statelog.NewOpID(applyClock, "code_purge_unreachable"))
	e.mustExpunge(t, ExpungeProposal{Request: &empty})
	if id, at := request(); id != "" || at != 0 {
		t.Fatalf("a repository holding no live pack holds request %q at %d, want it served at once", id, at)
	}

	e.seed("repo-a", 0, 0, 3)
	asked := requestOf("repo-a", statelog.NewOpID(applyClock, "code_purge_unreachable"))
	at := e.mustExpunge(t, ExpungeProposal{Request: &asked})
	if id, pos := request(); id != asked.RequestID || pos != at.Packed() {
		t.Fatalf("the repository holds request %q at %d, want %s at %d", id, pos, asked.RequestID, at.Packed())
	}
	second := requestOf("repo-a", statelog.NewOpID(applyClock, "code_purge_unreachable"))
	if d := e.expunged(t, ExpungeProposal{Request: &second}); !d.collapsed || d.open != asked.RequestID ||
		d.openAt != at {
		t.Fatalf("a second request answered %+v, want the open one %s at %s", d, asked.RequestID, at)
	}

	first := beginOver("repo-a", 1, []string{idOf("repo-a", 0), idOf("repo-a", 1)}, []int64{1}, []int64{1})
	first.RequestID, first.Grace = asked.RequestID, 0
	e.mustExpunge(t, ExpungeProposal{Begin: &first})
	c := commitOver(first)
	e.mustExpunge(t, ExpungeProposal{Commit: &c})
	if id, _ := request(); id != asked.RequestID {
		t.Fatalf("a cycle that left a pack from before the request live served it (%q)", id)
	}
	rest := beginOver("repo-a", 2, []string{idOf("repo-a", 2)}, []int64{1}, nil)
	rest.RequestID, rest.Grace = asked.RequestID, 0
	e.mustExpunge(t, ExpungeProposal{Begin: &rest})
	c = commitOver(rest)
	e.mustExpunge(t, ExpungeProposal{Commit: &c})
	if id, pos := request(); id != "" || pos != 0 {
		t.Fatalf("the cycle that rewrote the rest left request %q at %d open", id, pos)
	}
}

// THE EXPUNGE'S RECORDS FIT ONE RECORD AT EVERY BOUND: the widest begin the
// committer takes — MaxExpungeInputs inputs, MaxExpungeOutputs outputs and as
// many layers dropped as a chain holds — and the widest commit each fit their
// record whole, in rows (within MaxRecordRows) and in bytes; and one input or
// one output more is refused before it is queued, naming it.
//
// Mutation: raise MaxExpungeOutputs past what expungeBeginRows leaves of the
// row budget, and the widest begin does not fit.
func TestTheExpungeRecordsFitOneRecord(t *testing.T) {
	t.Parallel()
	repo := "0190f3c4-0000-7000-8000-00000000e0de"
	seen := codeAt(7)
	widest := func(inputs, outputs, layers int) ExpungeBegin {
		xid := statelog.NewOpID(time.Now(), "")
		b := ExpungeBegin{RepoID: repo, ExpungeID: xid, Node: "node-a", Seen: seen, DropEpoch: 1, Grace: PruneGrace}
		for i := range inputs {
			b.Retire = append(b.Retire, statelog.DeriveOpID(time.Now(), "", "input", fmt.Sprint(i)))
		}
		slices.Sort(b.Retire)
		for i := range outputs {
			out := outputOf(xid, i, int64(i+1), time.Now())
			if i < outputs/2 {
				b.Add = append(b.Add, out)
			} else {
				b.Dropping = append(b.Dropping, out)
			}
		}
		for i := range layers {
			b.DropLayers = append(b.DropLayers, oid(fmt.Sprint("layer", i)))
		}
		return b
	}
	b := widest(MaxExpungeInputs, MaxExpungeOutputs, MaxPublishedLayers)
	if rows := (ExpungeProposal{Begin: &b}).decidedRows(); rows != 2_625 || rows > MaxRecordRows {
		t.Fatalf("the widest begin writes %d rows, want the 2 625 expungeBeginRows' doc states, under "+
			"MaxRecordRows (%d)", rows, MaxRecordRows)
	}
	fits := func(name string, p ExpungeProposal) {
		t.Helper()
		if _, err := p.measure(); err != nil {
			t.Fatalf("the widest %s the committer takes is refused: %v", name, err)
		}
		op, opID, _, payload := p.record()
		decision, err := recordFor(statelog.Stamp{Writer: "node-a", Gen: 1}, RepoSubject(repo), op,
			[]string{RepoPath(repo)}, opID, Actor{ID: "node-a", Kind: ActorSystem}, payload, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err := withinExpunge(decision, p); err != nil {
			t.Fatalf("the widest %s's record (%d bytes) does not fit: %v", name, len(decision.Payload), err)
		}
	}
	fits("begin", ExpungeProposal{Begin: &b})
	c := ExpungeCommit{RepoID: repo, ExpungeID: b.ExpungeID, CommitID: statelog.NewOpID(time.Now(), ""),
		Node: "node-a", Seen: seen, DropEpoch: 1}
	for i := range MaxExpungeOutputs {
		c.Retire = append(c.Retire, DeriveOutputPackID(b.ExpungeID, i))
		c.Add = append(c.Add, outputOf(c.CommitID, i, 1, time.Now()))
	}
	slices.Sort(c.Retire)
	if rows := (ExpungeProposal{Commit: &c}).decidedRows(); rows != 97 {
		t.Fatalf("the widest commit writes %d rows, want the 97 expungeCommitRows' doc states", rows)
	}
	fits("commit", ExpungeProposal{Commit: &c})
	for name, tc := range map[string]struct {
		b     ExpungeBegin
		field string
	}{
		"one input more":  {widest(MaxExpungeInputs+1, 1, 0), "begin.retire"},
		"one output more": {widest(1, MaxExpungeOutputs+1, 0), "begin.add"},
		"one layer more":  {widest(1, 1, MaxPublishedLayers+1), "begin.drop_layers"},
	} {
		if err := (ExpungeProposal{Begin: &tc.b}).Validate(); !errors.Is(err, ErrInvalidProposal) ||
			!strings.Contains(err.Error(), tc.field+":") {
			t.Errorf("%s: Validate = %v, want ErrInvalidProposal naming %s", name, err, tc.field)
		}
	}
}

// AN EXPUNGE PROPOSAL IS HELD TO ITS SHAPE before it is queued: each field no
// committer could decide is refused [ErrInvalidProposal], naming it, and a
// proposal every rule admits passes.
//
// Mutation: let Validate take a begin whose grace is neither PruneGrace nor a
// purge's 0, and the arm naming begin.grace passes.
func TestAnExpungeProposalIsHeldToItsShape(t *testing.T) {
	t.Parallel()
	repo := "0190f3c4-0000-7000-8000-00000000e0de"
	request := ExpungeRequest{RepoID: repo, RequestID: statelog.NewOpID(applyClock, "code_purge_unreachable"),
		By: purger}
	begin := beginOver(repo, 1, []string{"pack-1", "pack-2"}, []int64{1}, []int64{1}, oid("layer"))
	commit := commitOver(begin, 1)
	for name, p := range map[string]ExpungeProposal{
		"a request": {Request: &request}, "a begin": {Begin: &begin}, "a commit": {Commit: &commit},
	} {
		if err := p.Validate(); err != nil {
			t.Fatalf("%s every rule admits: %v", name, err)
		}
	}
	for name, tc := range map[string]struct {
		edit  func(r *ExpungeRequest, b *ExpungeBegin, c *ExpungeCommit) ExpungeProposal
		field string
	}{
		"two records": {func(r *ExpungeRequest, b *ExpungeBegin, _ *ExpungeCommit) ExpungeProposal {
			return ExpungeProposal{Request: r, Begin: b}
		}, "exactly one"},
		"none": {func(*ExpungeRequest, *ExpungeBegin, *ExpungeCommit) ExpungeProposal {
			return ExpungeProposal{}
		}, "exactly one"},
		"a request of a token bound to nobody": {func(r *ExpungeRequest, _ *ExpungeBegin, _ *ExpungeCommit) ExpungeProposal {
			r.By.Seat = ""
			return ExpungeProposal{Request: r}
		}, "request.by:"},
		"a request of a seat": {func(r *ExpungeRequest, _ *ExpungeBegin, _ *ExpungeCommit) ExpungeProposal {
			r.By = Principal{Kind: PrincipalAgent, Seat: "dev"}
			return ExpungeProposal{Request: r}
		}, "request.by:"},
		"a request id with no instant": {func(r *ExpungeRequest, _ *ExpungeBegin, _ *ExpungeCommit) ExpungeProposal {
			r.RequestID = "purge-1"
			return ExpungeProposal{Request: r}
		}, "request.request_id:"},
		"a begin whose id is named": {func(_ *ExpungeRequest, b *ExpungeBegin, _ *ExpungeCommit) ExpungeProposal {
			b.ExpungeID += ".named"
			return ExpungeProposal{Begin: b}
		}, "begin.expunge_id:"},
		"a begin at epoch 0": {func(_ *ExpungeRequest, b *ExpungeBegin, _ *ExpungeCommit) ExpungeProposal {
			b.DropEpoch = 0
			return ExpungeProposal{Begin: b}
		}, "begin.drop_epoch:"},
		"a begin with no grace on its own schedule": {func(_ *ExpungeRequest, b *ExpungeBegin, _ *ExpungeCommit) ExpungeProposal {
			b.Grace = 0
			return ExpungeProposal{Begin: b}
		}, "begin.grace:"},
		"a purge's begin keeping a grace": {func(_ *ExpungeRequest, b *ExpungeBegin, _ *ExpungeCommit) ExpungeProposal {
			b.RequestID = statelog.NewOpID(applyClock, "code_purge_unreachable")
			return ExpungeProposal{Begin: b}
		}, "begin.grace:"},
		"a begin naming no Seen": {func(_ *ExpungeRequest, b *ExpungeBegin, _ *ExpungeCommit) ExpungeProposal {
			b.Seen = statelog.Position{}
			return ExpungeProposal{Begin: b}
		}, "begin.seen:"},
		"a begin's inputs out of order": {func(_ *ExpungeRequest, b *ExpungeBegin, _ *ExpungeCommit) ExpungeProposal {
			b.Retire = []string{"pack-2", "pack-1"}
			return ExpungeProposal{Begin: b}
		}, "begin.retire[1]:"},
		"a begin with no output": {func(_ *ExpungeRequest, b *ExpungeBegin, _ *ExpungeCommit) ExpungeProposal {
			b.Add, b.Dropping = nil, nil
			return ExpungeProposal{Begin: b}
		}, "begin.add:"},
		"a dropping pack under another's id": {func(_ *ExpungeRequest, b *ExpungeBegin, _ *ExpungeCommit) ExpungeProposal {
			b.Dropping[0].PackID = b.Add[0].PackID
			return ExpungeProposal{Begin: b}
		}, "begin.dropping[0].pack_id:"},
		"a layer named twice": {func(_ *ExpungeRequest, b *ExpungeBegin, _ *ExpungeCommit) ExpungeProposal {
			b.DropLayers = append(b.DropLayers, b.DropLayers[0])
			return ExpungeProposal{Begin: b}
		}, "begin.drop_layers[1]:"},
		"a commit under its expunge's id": {func(_ *ExpungeRequest, _ *ExpungeBegin, c *ExpungeCommit) ExpungeProposal {
			c.CommitID = c.ExpungeID
			return ExpungeProposal{Commit: c}
		}, "commit.commit_id:"},
		"a rescue under the begin's derivation": {func(_ *ExpungeRequest, _ *ExpungeBegin, c *ExpungeCommit) ExpungeProposal {
			c.Add[0].PackID = DeriveOutputPackID(c.ExpungeID, 0)
			return ExpungeProposal{Commit: c}
		}, "commit.add[0].pack_id:"},
		"a rescue that lays out no bundle": {func(_ *ExpungeRequest, _ *ExpungeBegin, c *ExpungeCommit) ExpungeProposal {
			c.Add[0].IndexBytes++
			return ExpungeProposal{Commit: c}
		}, "commit.add[0]."},
	} {
		r, b, c := request, begin, commit
		b.Add, b.Dropping, b.DropLayers = slices.Clone(begin.Add), slices.Clone(begin.Dropping), slices.Clone(begin.DropLayers)
		c.Add = slices.Clone(commit.Add)
		err := tc.edit(&r, &b, &c).Validate()
		if !errors.Is(err, ErrInvalidProposal) || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: Validate = %v, want ErrInvalidProposal naming %s", name, err, tc.field)
		}
	}
}

// THE KINDS AND STATES A PACK ROW MAY HOLD ARE CLOSED SETS, each value Valid
// and nothing else: what a code_packs row's kind and state are written from.
func TestAPacksKindAndStateAreClosedSets(t *testing.T) {
	t.Parallel()
	for _, k := range PackKinds() {
		if !k.Valid() {
			t.Errorf("kind %q is in the set and Valid says it is not", k)
		}
	}
	for _, s := range PackStates() {
		if !s.Valid() {
			t.Errorf("state %q is in the set and Valid says it is not", s)
		}
	}
	if PackKind("import").Valid() || PackState("retired").Valid() || PackKind("").Valid() {
		t.Fatalf("a kind or state this build does not write is Valid")
	}
}
