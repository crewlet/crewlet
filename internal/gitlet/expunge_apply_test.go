package gitlet

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

// Cases of an expunge's decide and apply over one node's estate, no broker:
// records built here, the decide run in a snapshot of the rows a case seeds.

// purger asks every purge here: a credential bound to a human seat.
var purger = Principal{Kind: PrincipalOperator, OperatorID: "pat:7", Seat: "founder"}

// beginOver is a maintainer's expunge begin of repository id at epoch: retire
// rewritten into one kept pack per entry of kept and one dropping pack per entry
// of dropping, each holding that many objects, dropping layers, at applyClock.
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

// expungeDecidedIn runs one round of p's decide over e's rows at now.
func expungeDecidedIn(t testing.TB, e *estate, p ExpungeProposal, now time.Time) (expungeDecision, error) {
	t.Helper()
	var d expungeDecision
	err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) error {
		var err error
		d, err = decideExpunge(context.Background(), tx, p, DefaultDeletedRetention, now)
		return err
	})
	return d, err
}

// expunged decides p at applyClock and applies its record unless refused.
func (e *estate) expunged(t *testing.T, p ExpungeProposal) expungeDecision {
	t.Helper()
	d, err := expungeDecidedIn(t, e, p, applyClock)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if d.refusal == "" {
		e.apply(expungeRec(t, p))
	}
	return d
}

// mustExpunge is [estate.expunged] of a record the decide must form.
func (e *estate) mustExpunge(t *testing.T, p ExpungeProposal) statelog.Position {
	t.Helper()
	at := codeAt(e.next)
	if d := e.expunged(t, p); d.refusal != "" {
		t.Fatalf("the %s was refused %s (%s), where it is decided", opOf(p), d.refusal, d.detail)
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

// A BEGIN KEEPS EVERY OBJECT OF ITS INPUTS: the inputs move to
// code_packs_retired at its position and broker instant; its outputs join
// code_packs — the reachable ones live (kind expunge), the rest dropping (kind
// cruft) — holding exactly the inputs' objects; the drop epoch and open expunge
// move to the record's; the live counters count live rows only. Outputs holding
// more objects than the inputs are refused before any record.
//
// Mutation: write the dropping pack live or count it live (the rows or counters
// say so); delete the inputs rather than retire them (a mid-read reader loses them);
// drop the arithmetic (the inventing begin is decided).
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

// A COMMIT NEEDS ONLY ITS DROPPING PACKS: its one precondition is its expunge,
// open at its epoch with exactly the dropping packs it retires, so a push landing
// after the begin does not refuse it. The dropping packs retire at its position,
// its rescue joins live, the open expunge clears, and expunged_at and what it took
// out (the dropping packs' objects and bytes less the rescue's) are recorded.
//
// Mutation: hold the commit to the begin's refs or version (the push between
// refuses it); delete the dropping rows rather than retire them (a mid-read
// reader loses them).
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

// seedLayers writes id's published chain as a case's rows, never the applier's:
// one layer per list of pack checksums, root first, one part each; it sets the
// published counts to the packs covered.
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

// A BEGIN REMOVES THE LAYERS OVER ITS INPUTS: a layer naming a retired pack can
// no longer load, nor can any layer above it, so a begin drops the chain from the
// lowest layer naming an input to the top — exactly those, in chain order, else it
// is stale — with their parts, recounts the published counts from the rows left,
// and moves layers_position so a cache's next delta serves the chain whole.
//
// Mutation: drop only the layers naming an input (the top-alone plan is decided,
// leaving a layer over a missing base); leave layers_position (a cache keeps the
// dropped layers).
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

// A PUBLICATION IS REFUSED WHILE A REPOSITORY EXPUNGES (rule 6g): between begin
// and commit the live packs need not be closed — a ref moved before the begin may
// reach an object only a dropping pack holds — so a layer waits for the commit. A
// compaction carrying no layer is no publication and is decided as ever.
//
// Mutation: drop rule 6g, and the mid-expunge publication is decided.
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

// A SECOND BEGIN IS REFUSED WHILE ONE IS OPEN: one expunge at a time, so a begin
// while another is uncommitted is stale, naming the open one, and its record, if
// it reached the log, applies as nothing. A begin at a passed epoch is stale too;
// one at the next epoch, after the first commits, is decided.
//
// Mutation: drop the decide's open-expunge rule (the second begin is decided over
// the first's dropping packs) or the apply's (the logged record replaces the open
// expunge).
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
// already committed or not the open one, at another epoch, or naming other packs
// than the dropping ones is stale, and its logged record applies as nothing; a
// commit retires only dropping packs, never a live one a ref may reach.
//
// Mutation: retire every named pack whatever its state, and the live pack named
// beside the dropping one leaves code_packs.
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
	// ONCE COMMITTED, a repeat commit is stale and its record applies as nothing.
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

// AN EXPUNGE REPLAYED FROM ZERO WRITES THE SAME ROWS: applies are pure in the
// record, its position and its instant, so two nodes applying one log hold one
// estate byte for byte; a begin or commit met again at a later position, which the
// version guard does not stop, moves nothing.
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

// openRequest is the purge request repository id holds open, and the position
// its record landed at: "" and 0 for none.
func (e *estate) openRequest(id string) (string, int64) {
	e.t.Helper()
	return e.str(`SELECT expunge_request FROM code_repos WHERE id = ?`, id),
		e.num(`SELECT expunge_request_position FROM code_repos WHERE id = ?`, id)
}

// purgeOver is a purge's begin serving request, grace 0 (keeping nothing a ref
// drop left), built as [beginOver] with its scan at e's last applied position:
// past the request, as a maintainer's scan is.
func (e *estate) purgeOver(id, request string, epoch int64, retire []string, kept, dropping []int64) ExpungeBegin {
	b := beginOver(id, epoch, retire, kept, dropping)
	b.RequestID, b.Grace, b.Seen = request, 0, codeAt(e.next-1)
	return b
}

// A PURGE REQUEST IS SERVED ONCE NO PACK DATES FROM BEFORE IT: it stays open
// until a commit leaves no pack whose content predates it — a cycle rewriting part
// keeps it, the one rewriting the rest serves it — and a repository holding no
// pack is served at the request's own record, since no cycle could serve it.
//
// Mutation: clear a request at any commit (the first cycle serves it with an
// unrewritten pack live), or keep one for a packless repository (open for ever).
func TestAPurgeRequestIsServedOnceNoPackDatesFromBeforeIt(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	request := func() (string, int64) { return e.openRequest("repo-a") }
	empty := requestOf("repo-a", statelog.NewOpID(applyClock, "code_purge_unreachable"))
	e.mustExpunge(t, ExpungeProposal{Request: &empty})
	if id, at := request(); id != "" || at != 0 {
		t.Fatalf("a repository holding no pack holds request %q at %d, want it served at once", id, at)
	}

	e.seed("repo-a", 0, 0, 3)
	asked := requestOf("repo-a", statelog.NewOpID(applyClock, "code_purge_unreachable"))
	at := e.mustExpunge(t, ExpungeProposal{Request: &asked})
	if id, pos := request(); id != asked.RequestID || pos != at.Packed() {
		t.Fatalf("the repository holds request %q at %d, want %s at %d", id, pos, asked.RequestID, at.Packed())
	}

	first := e.purgeOver("repo-a", asked.RequestID, 1, []string{idOf("repo-a", 0), idOf("repo-a", 1)},
		[]int64{1}, []int64{1})
	e.mustExpunge(t, ExpungeProposal{Begin: &first})
	c := commitOver(first)
	e.mustExpunge(t, ExpungeProposal{Commit: &c})
	if id, _ := request(); id != asked.RequestID {
		t.Fatalf("a cycle that left a pack from before the request live served it (%q)", id)
	}
	rest := e.purgeOver("repo-a", asked.RequestID, 2, []string{idOf("repo-a", 2)}, []int64{1}, nil)
	e.mustExpunge(t, ExpungeProposal{Begin: &rest})
	c = commitOver(rest)
	e.mustExpunge(t, ExpungeProposal{Commit: &c})
	if id, pos := request(); id != "" || pos != 0 {
		t.Fatalf("the cycle that rewrote the rest left request %q at %d open", id, pos)
	}
}

// EVERY PACK DATES ITS CONTENT AS A PURGE READS IT ([PackRow.Content]): a push's
// pack from the push's position; a compaction's output, and a grace-keeping
// expunge's kept, dropping and rescue packs, from the oldest pack each replaced; a
// purge's outputs and rescue from its scan, never past the begin's position. A
// retired row keeps its date, and a snapshot serves every pack's.
//
// Mutation: date a compaction's or grace-keeping expunge's output at its own
// record (the date passes the merged content); date a purge's outputs from its
// oldest input (nothing it judged reads judged); omit the date from the retired
// insert (the retired row reads 0).
func TestEveryPackDatesItsContentAsAPurgeReadsIt(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	content := func(pack string) int64 {
		return e.num(`SELECT content_position FROM code_packs WHERE repo_id = 'repo-a' AND pack_id = ?`, pack)
	}
	retired := func(pack string) int64 {
		return e.num(`SELECT content_position FROM code_packs_retired WHERE repo_id = 'repo-a' AND pack_id = ?`,
			pack)
	}
	one, two := codeAt(e.next).Packed(), codeAt(e.next+1).Packed()
	first := e.pushed(t, "repo-a", "push-1", "main", ZeroOID, oid("one"))
	second := e.pushed(t, "repo-a", "push-2", "main", oid("one"), oid("two"))
	if content(first) != one || content(second) != two {
		t.Fatalf("the pushes' packs date from %d and %d, want their own positions %d and %d", content(first),
			content(second), one, two)
	}

	cid := statelog.NewOpID(applyClock, "")
	merged := outputOf(cid, 0, 2, applyClock)
	e.apply(recordOf(t, Compaction{RepoID: "repo-a", CompactionID: cid, Node: "node-a", Seen: codeAt(e.next - 1),
		Retire: sorted(first, second), Add: []PackRef{merged}}))
	if content(merged.PackID) != one || retired(first) != one || retired(second) != two {
		t.Fatalf("the compaction's output dates from %d and its retired inputs from %d and %d, want %d, and "+
			"%d and %d as they were", content(merged.PackID), retired(first), retired(second), one, one, two)
	}

	third := e.pushed(t, "repo-a", "push-3", "main", oid("two"), oid("three"))
	graced := beginOver("repo-a", 1, sorted(merged.PackID, third), []int64{2}, []int64{1})
	graced.Seen = codeAt(e.next - 1)
	e.mustExpunge(t, ExpungeProposal{Begin: &graced})
	for _, out := range append(slices.Clone(graced.Add), graced.Dropping...) {
		if got := content(out.PackID); got != one {
			t.Errorf("pack %s of an expunge keeping a grace dates from %d, want its oldest input's %d",
				out.PackID, got, one)
		}
	}
	rescued := commitOver(graced, 1)
	rescued.Seen = codeAt(e.next - 1)
	e.mustExpunge(t, ExpungeProposal{Commit: &rescued})
	if got := content(rescued.Add[0].PackID); got != one || retired(graced.Dropping[0].PackID) != one {
		t.Fatalf("its rescue dates from %d and its retired dropping pack from %d, want both %d", got,
			retired(graced.Dropping[0].PackID), one)
	}

	scan := codeAt(e.next - 1)
	purge := e.purgeOver("repo-a", statelog.NewOpID(applyClock, "code_purge_unreachable"), 2,
		sorted(graced.Add[0].PackID, rescued.Add[0].PackID), []int64{2}, []int64{1})
	e.mustExpunge(t, ExpungeProposal{Begin: &purge})
	for _, out := range append(slices.Clone(purge.Add), purge.Dropping...) {
		if got := content(out.PackID); got != scan.Packed() {
			t.Errorf("pack %s of a purge dates from %d, want its scan's %d", out.PackID, got, scan.Packed())
		}
	}
	purged := commitOver(purge, 1)
	purged.Seen = codeAt(e.next - 1)
	e.mustExpunge(t, ExpungeProposal{Commit: &purged})
	if got := content(purged.Add[0].PackID); got != scan.Packed() {
		t.Fatalf("a purge's rescue dates from %d, want the purge's scan %d", got, scan.Packed())
	}

	// A SCAN PAST ITS BEGIN, which no correct log carries, dates nothing after
	// the begin.
	late := e.purgeOver("repo-a", statelog.NewOpID(applyClock, "code_purge_unreachable"), 3,
		[]string{purge.Add[0].PackID}, []int64{2}, nil)
	late.Seen = codeAt(e.next + 100)
	at := codeAt(e.next)
	e.apply(expungeRec(t, ExpungeProposal{Begin: &late}))
	if got := content(late.Add[0].PackID); got != at.Packed() {
		t.Fatalf("a purge whose scan is past its own record dates its pack from %d, want the record's %d", got,
			at.Packed())
	}
}

// EVERY PACK DATES WHEN ITS NEWEST CONTENT ARRIVED ([PackRow.Arrived]): a push's
// pack at the push's broker instant; a compaction's output, and an expunge's kept,
// dropping and rescue packs, at the latest pack each replaced — never their own
// record, since a rewrite brings no content. A retired row keeps its instant. A
// scheduled expunge keeps a young pack whole by it (internal/gitlet/compact's
// weekly cases read it through a Gitlet node's cache).
//
// Mutation: date a compaction's output at its own record (a month-old merge reads
// young for another fortnight) or at the OLDEST pack merged (yesterday's objects
// read old); omit the instant from the retired insert (the retired row reads 0).
func TestEveryPackDatesWhenItsNewestContentArrived(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	arrived := func(table, pack string) int64 {
		return e.num(`SELECT arrived_at FROM `+table+` WHERE repo_id = 'repo-a' AND pack_id = ?`, pack)
	}
	live := func(pack string) int64 { return arrived("code_packs", pack) }
	retired := func(pack string) int64 { return arrived("code_packs_retired", pack) }
	at := func(h int) time.Time { return applyClock.Add(time.Duration(h) * time.Hour) }
	enc := func(h int) int64 { return store.EncodeTime(at(h)) }

	e.apply(pushOf(t, "repo-a", pushEntry("push-1", move("refs/heads/main", ZeroOID, oid("one")))).at(at(0)))
	e.apply(pushOf(t, "repo-a", pushEntry("push-2", move("refs/heads/main", oid("one"), oid("two")))).at(at(1)))
	if live("push-1") != enc(0) || live("push-2") != enc(1) {
		t.Fatalf("the pushes' packs arrived at %d and %d, want their own instants %d and %d", live("push-1"),
			live("push-2"), enc(0), enc(1))
	}

	cid := statelog.NewOpID(applyClock, "")
	merged := outputOf(cid, 0, 2, applyClock)
	e.apply(recordOf(t, Compaction{RepoID: "repo-a", CompactionID: cid, Node: "node-a", Seen: codeAt(e.next - 1),
		Retire: sorted("push-1", "push-2"), Add: []PackRef{merged}}).at(at(2)))
	if live(merged.PackID) != enc(1) || retired("push-1") != enc(0) || retired("push-2") != enc(1) {
		t.Fatalf("the compaction's output arrived at %d and its retired inputs at %d and %d, want the latest "+
			"input's %d, and %d and %d as they were", live(merged.PackID), retired("push-1"), retired("push-2"),
			enc(1), enc(0), enc(1))
	}

	e.apply(pushOf(t, "repo-a", pushEntry("push-3", move("refs/heads/main", oid("two"), oid("three")))).at(at(3)))
	graced := beginOver("repo-a", 1, sorted(merged.PackID, "push-3"), []int64{2}, []int64{1})
	graced.Seen = codeAt(e.next - 1)
	e.apply(expungeRec(t, ExpungeProposal{Begin: &graced}).at(at(4)))
	for _, out := range append(slices.Clone(graced.Add), graced.Dropping...) {
		if got := live(out.PackID); got != enc(3) {
			t.Errorf("pack %s of an expunge arrived at %d, want its latest input's %d", out.PackID, got, enc(3))
		}
	}
	rescued := commitOver(graced, 1)
	rescued.Seen = codeAt(e.next - 1)
	e.apply(expungeRec(t, ExpungeProposal{Commit: &rescued}).at(at(5)))
	if got := live(rescued.Add[0].PackID); got != enc(3) || retired(graced.Dropping[0].PackID) != enc(3) {
		t.Fatalf("its rescue arrived at %d and its retired dropping pack at %d, want both %d", got,
			retired(graced.Dropping[0].PackID), enc(3))
	}
}

// pushed lands push on repository id moving refs/heads/<ref> from old to new,
// with a pack of its own named for the push, and answers the pack's id.
func (e *estate) pushed(t *testing.T, id, push, ref, old, new string) string {
	t.Helper()
	e.apply(pushOf(t, id, pushEntry(push, move("refs/heads/"+ref, old, new))))
	return push
}

// A PURGE REQUEST OUTLIVES A COMPACTION OF ITS PACKS: a compaction decided after
// the request (in flight when asked, or the next pass) merges the packs it waits
// on into an output added after it without judging them, so any expunge's commit
// while that output is live — here a scheduled one over a pack pushed since —
// leaves the request open; the purge cycle rewriting the output serves it.
//
// Mutation: judge a request by each pack row's add time, or date a compaction's
// output at its own record, and the commit over the newer pack serves the request
// with the merged content live.
func TestAPurgeRequestOutlivesACompactionOfItsPacks(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	first := e.pushed(t, "repo-a", "push-1", "main", ZeroOID, oid("one"))
	second := e.pushed(t, "repo-a", "push-2", "main", oid("one"), oid("two"))
	asked := requestOf("repo-a", statelog.NewOpID(applyClock, "code_purge_unreachable"))
	e.mustExpunge(t, ExpungeProposal{Request: &asked})

	cid := statelog.NewOpID(applyClock, "")
	merged := outputOf(cid, 0, 2, applyClock)
	compaction := Compaction{RepoID: "repo-a", CompactionID: cid, Node: "node-a", Seen: codeAt(e.next - 1),
		Retire: sorted(first, second), Add: []PackRef{merged}}
	if d, err := compactionDecidedIn(t, e, compaction, applyClock); err != nil || d.refusal != "" {
		t.Fatalf("the compaction of the packs the request waits on answered %s (%s), %v; want it decided",
			d.refusal, d.detail, err)
	}
	e.apply(recordOf(t, compaction))

	later := e.pushed(t, "repo-a", "push-3", "main", oid("two"), oid("three"))
	other := beginOver("repo-a", 1, []string{later}, []int64{1}, nil)
	other.Seen = codeAt(e.next - 1)
	e.mustExpunge(t, ExpungeProposal{Begin: &other})
	c := commitOver(other)
	e.mustExpunge(t, ExpungeProposal{Commit: &c})
	if id, _ := e.openRequest("repo-a"); id != asked.RequestID {
		t.Fatalf("a commit served the request (%q) while the compaction's output, holding everything the "+
			"person asked about, is live", id)
	}

	purge := e.purgeOver("repo-a", asked.RequestID, 2, []string{merged.PackID}, []int64{1}, []int64{1})
	e.mustExpunge(t, ExpungeProposal{Begin: &purge})
	c = commitOver(purge)
	e.mustExpunge(t, ExpungeProposal{Commit: &c})
	if id, at := e.openRequest("repo-a"); id != "" || at != 0 {
		t.Fatalf("the purge's cycle over the compaction's output left request %q at %d open", id, at)
	}
}

// A PURGE REQUEST OUTLIVES AN EXPUNGE THAT KEEPS A GRACE: a scheduled expunge
// keeps what a ref drop left within PruneGrace — the very objects a purge drops —
// so its commit serves no request on what it rewrote: neither one asked before it
// began (its kept pack holds the content) nor one asked while open (its dropping
// packs, then its rescue cut with the grace, hold it). The purge cycle rewriting
// what it kept serves each.
//
// Mutation: date an expunge's outputs from its scan whatever its grace (the commit
// of the one begun after the request serves it); judge by each pack row's add
// time (both are served: the first at the commit, the second at its own record,
// while its content is dropping).
func TestAPurgeRequestOutlivesAnExpungeThatKeepsAGrace(t *testing.T) {
	t.Parallel()
	for _, arm := range []struct {
		name           string
		askedFirst     bool
		kept, dropping []int64
	}{
		{"asked before it began", true, []int64{1}, []int64{1}},
		{"asked while it was open", false, nil, []int64{2}},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			e := openApplyEstate(t)
			e.apply(create(t, "repo-a", "a"))
			first := e.pushed(t, "repo-a", "push-1", "main", ZeroOID, oid("one"))
			second := e.pushed(t, "repo-a", "push-2", "main", oid("one"), oid("two"))
			asked := requestOf("repo-a", statelog.NewOpID(applyClock, "code_purge_unreachable"))
			if arm.askedFirst {
				e.mustExpunge(t, ExpungeProposal{Request: &asked})
			}
			// EVERY PACK FROM BEFORE THE REQUEST IS AN INPUT, so the outputs are the
			// only such content left.
			b := beginOver("repo-a", 1, sorted(first, second), arm.kept, arm.dropping)
			b.Seen = codeAt(e.next - 1)
			e.mustExpunge(t, ExpungeProposal{Begin: &b})
			if !arm.askedFirst {
				e.mustExpunge(t, ExpungeProposal{Request: &asked})
				if id, _ := e.openRequest("repo-a"); id != asked.RequestID {
					t.Fatalf("a request asked while dropping packs hold what it asks about was served at once "+
						"(%q)", id)
				}
			}
			c := commitOver(b, 1)
			c.Seen = codeAt(e.next - 1)
			e.mustExpunge(t, ExpungeProposal{Commit: &c})
			if id, _ := e.openRequest("repo-a"); id != asked.RequestID {
				t.Fatalf("the commit of an expunge keeping a grace served the request (%q) while what it kept "+
					"and rescued holds what the person asked about", id)
			}

			rewrite := []string{c.Add[0].PackID}
			for _, out := range b.Add {
				rewrite = append(rewrite, out.PackID)
			}
			purge := e.purgeOver("repo-a", asked.RequestID, 2, sorted(rewrite...), []int64{1}, nil)
			e.mustExpunge(t, ExpungeProposal{Begin: &purge})
			c = commitOver(purge)
			e.mustExpunge(t, ExpungeProposal{Commit: &c})
			if id, at := e.openRequest("repo-a"); id != "" || at != 0 {
				t.Fatalf("the purge's cycle over what the expunge kept left request %q at %d open", id, at)
			}
		})
	}
}

// A LATER REQUEST WIDENS THE OPEN ONE: a purge asked while another is open is
// recorded and moves the open request to its own id and position, since it covers
// more — a pack pushed between the two (the leak the later person purges), which
// no cycle of the earlier request takes, or what a ref dropped between them left,
// which a scan before the later request kept as reachable. So the earlier
// request's cycle (its scan between the two) serves neither; a cycle scanning
// past both serves the purge.
//
// Mutation: apply a request asked while one is open as nothing (the earlier one
// stays open); date a purge's packs from its begin rather than its scan (the
// earlier cycle serves the later person with the dropped history kept).
func TestALaterRequestWidensTheOpenOne(t *testing.T) {
	t.Parallel()
	for _, arm := range []struct {
		name string
		// between lands what comes between the two requests and answers its packs.
		between func(t *testing.T, e *estate) []string
	}{
		{"a pack pushed between them", func(t *testing.T, e *estate) []string {
			return []string{e.pushed(t, "repo-a", "push-leak", "leak", ZeroOID, oid("leak"))}
		}},
		{"a ref dropped between them", func(t *testing.T, e *estate) []string {
			drop := pushEntry("push-drop", move("refs/heads/side", oid("side"), ZeroOID))
			drop.Pack = nil
			e.apply(pushOf(t, "repo-a", drop))
			return nil
		}},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			e := openApplyEstate(t)
			e.apply(create(t, "repo-a", "a"))
			before := sorted(e.pushed(t, "repo-a", "push-1", "main", ZeroOID, oid("one")),
				e.pushed(t, "repo-a", "push-side", "side", ZeroOID, oid("side")))
			earlier := requestOf("repo-a", statelog.NewOpID(applyClock, "code_purge_unreachable"))
			e.mustExpunge(t, ExpungeProposal{Request: &earlier})
			scan := codeAt(e.next - 1)
			added := arm.between(t, e)
			later := requestOf("repo-a", statelog.NewOpID(applyClock, "code_purge_unreachable"))
			at := e.mustExpunge(t, ExpungeProposal{Request: &later})
			if id, pos := e.openRequest("repo-a"); id != later.RequestID || pos != at.Packed() {
				t.Fatalf("the repository holds request %q at %d, want the later one, %s at its own %d", id,
					pos, later.RequestID, at.Packed())
			}

			first := beginOver("repo-a", 1, before, []int64{2}, nil)
			first.RequestID, first.Grace, first.Seen = earlier.RequestID, 0, scan
			e.mustExpunge(t, ExpungeProposal{Begin: &first})
			c := commitOver(first)
			e.mustExpunge(t, ExpungeProposal{Commit: &c})
			if id, _ := e.openRequest("repo-a"); id != later.RequestID {
				t.Fatalf("the earlier request's cycle, its scan before the later request, served the "+
					"repository's purge (%q)", id)
			}

			rest := e.purgeOver("repo-a", later.RequestID, 2, sorted(append([]string{first.Add[0].PackID},
				added...)...), []int64{1}, []int64{1})
			e.mustExpunge(t, ExpungeProposal{Begin: &rest})
			c = commitOver(rest)
			e.mustExpunge(t, ExpungeProposal{Commit: &c})
			if id, pos := e.openRequest("repo-a"); id != "" || pos != 0 {
				t.Fatalf("the cycle past both requests left request %q at %d open", id, pos)
			}
		})
	}
}

// THE EXPUNGE'S RECORDS FIT ONE RECORD AT EVERY BOUND: the widest begin
// (MaxExpungeInputs inputs, MaxExpungeOutputs outputs, a full chain of layers
// dropped) and the widest commit each fit one record, in rows (MaxRecordRows) and
// bytes; one input or output more is refused before it is queued, naming it.
//
// Mutation: raise MaxExpungeOutputs past what expungeBeginRows leaves of the row
// budget, and the widest begin does not fit.
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
	if PackKind("bundle").Valid() || PackState("retired").Valid() || PackKind("").Valid() {
		t.Fatalf("a kind or state this build does not write is Valid")
	}
}
