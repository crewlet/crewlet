package code_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE REF HOLD (M3-decisions M3-O61, M3-O64): a live turn's hold on a seat's
// branch, decided at the turn's read of it and recorded — where it is recorded
// at all — on the repository's subject, moving the ref's held_position and
// the repository's version and nothing a cache or an idle clock reads.

// refRow is one ref's row as its table holds it.
type refRow struct {
	target, seat        string
	position, at, held  int64
	lastPush, lastActed int64
	version             int64
}

// refRowOf is repository id's ref as its row holds it, beside the repository's
// last push, idle clock and version.
func (n *node) refRowOf(id, ref string) refRow {
	n.t.Helper()
	var r refRow
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(n.t.Context(), `SELECT target, seat, position, at, held_position
			FROM code_refs WHERE repo_id = ? AND ref = ?`, id, ref).Scan(&r.target, &r.seat, &r.position,
			&r.at, &r.held); err != nil {
			return err
		}
		return tx.QueryRowContext(n.t.Context(), `SELECT COALESCE(last_push_at, 0), last_activity_at, version
			FROM code_repos WHERE id = ?`, id).Scan(&r.lastPush, &r.lastActed, &r.version)
	}); err != nil {
		n.t.Fatalf("read %s of %s: %v", ref, id, err)
	}
	return r
}

// seatPush is one push by seat as an agent — its box, or an engine write in
// its turn — of cmds.
func seatPush(id, seat string, cmds ...code.Command) code.PushEntry {
	e := entry(id, cmds...)
	e.Principal = agent(seat)
	return e
}

// lcRefHold is a live turn's hold on ref of the repository id, decided at the
// read seen.
func lcRefHold(actor code.Principal, id, turn, ref string, seen statelog.Position) code.LifecycleRequest {
	req := lcHold(actor, id, turn, seen)
	req.Ref = ref
	return req
}

// readRef is ref of the repository named name as dev's resolve answers it,
// and the checkpoint it was read at.
func (n *node) readRef(name, ref string) (code.ResolvedRef, statelog.Position) {
	n.t.Helper()
	answer, err := n.reader.ResolveRef(n.t.Context(), code.RefQuery{Repo: name, Refs: []string{ref}, Fresh: stale,
		Asker: agent("dev")})
	if err != nil || len(answer.Refs) != 1 {
		n.t.Fatalf("resolve %s of %s: %+v, %v", ref, name, answer, err)
	}
	return answer.Refs[0], answer.Position
}

// A REF HOLD MOVES NO CLOCK. A hold record on a seat's branch moves the ref's
// held_position and the repository's version — the per-ref mark a deletion
// listed before the turn's read is ordered by, and the replay guard every
// record moves — and leaves the ref's own `at` (its idle clock), its
// `position` (the cursor every cache's delta reads), its seat and its target,
// and the repository's last push and idle clock, as they were: a snapshot
// delta from before the hold names no ref. A hold on a ref no seat set is
// answered applied with NO record — such a ref is no seat's to lose — and one
// in an ARCHIVED repository is a record moving the mark and the version alike,
// because an archive can be undone between a deletion's listing and its
// decide.
//
// Mutations: write `at` in the hold's apply → red; write `position` → the delta
// names the ref, red; record a hold on a seat-less ref → a record is appended,
// red; apply the version only WHERE status = 'active' → the archived
// repository's held_position and version stay, red.
func TestARefHoldMovesNoClock(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	dev := agent("dev")
	id := n.made(lcPerson, "web", unitOwned("eng"))
	const (
		mine  = "refs/heads/seats/dev/x"
		other = "refs/heads/seats/dev/y"
	)
	n.push(id, seatPush("push-x", "dev", set(mine, code.ZeroOID, oidOf("x"))))
	n.push(id, entry("push-y", set(other, code.ZeroOID, oidOf("y"))))

	before := n.refRowOf(id, mine)
	if before.seat != "dev" || before.held != 0 {
		t.Fatalf("%s reads seat %q held at %d before any hold, want seat dev and never held", mine, before.seat,
			before.held)
	}
	resolved, seen := n.readRef("web", mine)
	if resolved.Seat != "dev" || resolved.At.IsZero() || resolved.OID != oidOf("x") {
		t.Fatalf("the resolve answered %+v, want the seat that set it and when", resolved)
	}
	got := n.lands(lcRefHold(dev, id, "turn-1", mine, seen))
	if got.Position.IsZero() {
		t.Fatalf("a hold on a seat's branch wrote no record: %+v", got)
	}
	after := n.refRowOf(id, mine)
	want := before
	want.held, want.version = after.held, after.version
	if after != want || after.held == 0 || after.held != after.version || after.version <= before.version {
		t.Errorf("the hold moved %+v to %+v, want only held_position and the version moved, to its record", before,
			after)
	}
	delta, err := n.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Since: seen, Fresh: stale})
	if err != nil || delta.Full || len(delta.Refs) != 0 {
		t.Errorf("a delta from the hold's read = full %v refs %+v (%v), want no ref: a hold moves none a cache "+
			"holds", delta.Full, delta.Refs, err)
	}

	// A REF NO SEAT SET: applied, and nothing written.
	end := n.end()
	if _, read := n.readRef("web", other); !n.lands(lcRefHold(dev, id, "turn-1", other, read)).Position.IsZero() ||
		n.end() != end {
		t.Errorf("a hold on a branch a person pushed wrote a record")
	}

	// AN ARCHIVED REPOSITORY: recorded all the same.
	n.lands(lcOn(code.LifecycleArchive, lcPerson, id))
	archived := n.refRowOf(id, mine)
	_, seen = n.readRef("web", mine)
	if n.lands(lcRefHold(dev, id, "turn-2", mine, seen)).Position.IsZero() {
		t.Fatal("a hold on a seat's branch of an archived repository wrote no record")
	}
	if got := n.refRowOf(id, mine); got.held <= archived.held || got.version <= archived.version {
		t.Errorf("the archived repository's hold moved %+v to %+v, want its mark and version moved", archived, got)
	}
}

// A REF DELETED AFTER THE TURN READ IT ANSWERS THE HOLD DELETED, before the
// turn reads anything through it, and the hold writes nothing: decided at or
// past the turn's read, the hold finds the branch a tombstone — here a
// person's push deleted it, the deleter any push may be — and refuses
// ref_deleted, an ErrRefNotFound to whoever reads its error.
//
// Mutation: publish the hold whatever the ref's row holds → a record follows
// the deletion, red.
func TestARefDeletedAfterTheTurnReadItAnswersTheHoldDeleted(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	id := n.made(lcPerson, "web", unitOwned("eng"))
	const ref = "refs/heads/seats/dev/x"
	n.push(id, seatPush("push-x", "dev", set(ref, code.ZeroOID, oidOf("x"))))
	_, seen := n.readRef("web", ref)
	n.push(id, entry("push-gone", set(ref, oidOf("x"), code.ZeroOID)))
	end := n.end()
	got := n.life(lcRefHold(agent("dev"), id, "turn-late", ref, seen))
	if got.Refusal != code.LifecycleRefDeleted || !errors.Is(got.Err(), code.ErrRefNotFound) ||
		!strings.Contains(got.Detail, ref) {
		t.Fatalf("a hold after the branch was deleted = %+v, want ref_deleted naming %s", got, ref)
	}
	if n.end() != end {
		t.Fatal("a hold on a deleted branch wrote a record")
	}
}

// A REF HOLD RECORD COSTS THE LOG ONE SMALL RECORD — what internal/tracker's
// code log day weighs a seat branch's hold at, as TestAHoldRecordCostsTheLog-
// OneSmallRecord measures a scratch repository's: the log's stream grown by the
// one record, as the broker counts it against the log's ceiling. Measured at
// the longest the engine writes one: a seat handle at iam.MaxLogin, a ref of
// code.MaxRefNameBytes in its namespace of a byte JSON escapes six-fold, and
// the operation id the engine derives for it — a run's id, a dot, "hold-" and
// the ref's fixed-width key, 111 bytes whatever the ref (builtin's holdOpID).
//
// Mutation: carry the ref twice in the hold's payload — past the bound, red.
func TestARefHoldRecordCostsTheLogOneSmallRecord(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c := testChart()
	long := strings.Repeat("h", iam.MaxLogin)
	c.seats[long] = code.RosterSeat{Handle: long, Tier: code.TierFullAccess, Unit: "sales"}
	n.chart = c
	id := n.made(lcPerson, "web", unitOwned("eng"))
	ns := code.SeatNamespace(long)
	ref := ns + strings.Repeat("<", code.MaxRefNameBytes-len(ns))
	n.push(id, seatPush("push-long", long, set(ref, code.ZeroOID, oidOf("long"))))
	answer, err := n.reader.ResolveRef(t.Context(), code.RefQuery{Repo: "web", Refs: []string{ref}, Fresh: stale,
		Asker: agent(long)})
	if err != nil || answer.Refs[0].Seat != long {
		t.Fatalf("resolve the longest seat branch: %+v, %v", answer, err)
	}
	turn := statelog.NewOpID(time.Now(), "")
	req := lcRefHold(agent(long), id, turn, ref, answer.Position)
	req.OpID = statelog.NewOpID(time.Now(), "") + ".hold-" + code.RefHoldKey(id, ref)
	if len(req.OpID) != 111 {
		t.Fatalf("the hold's operation id is %d bytes, want the 111 the engine derives", len(req.OpID))
	}
	before, err := n.log.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	n.lands(req)
	after, err := n.log.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if records := after.Messages - before.Messages; records != 1 {
		t.Fatalf("the hold appended %d records, want one", records)
	}
	grew := int64(after.Bytes - before.Bytes)
	t.Logf("the longest hold of a seat's branch takes the log %d bytes", grew)
	if grew > refHoldRecordBytes {
		t.Errorf("the longest hold of a seat's branch the engine writes takes the log %d bytes, over the %d the "+
			"code log's day weighs one at", grew, refHoldRecordBytes)
	}
}

// refHoldRecordBytes is the most one hold of a seat's branch takes the code log
// as the engine writes one, the figure internal/tracker's codeLogNegligible
// names for it.
const refHoldRecordBytes = 4096

// A REF HOLD IS VALIDATED AS ONE: a hold names a seat's branch or nothing, and
// is decided at the read it orders — a ref outside every seat's namespace, a
// tag, a name that is no ref, and a hold with no read are each refused before
// any decide, naming the field.
//
// Mutation: drop the Holdable term from Validate → main is accepted, red.
func TestARefHoldIsValidatedAsOne(t *testing.T) {
	t.Parallel()
	seen := statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1, Seq: 9}
	for _, c := range []struct {
		name  string
		ref   string
		seen  statelog.Position
		field string
	}{
		{"main", "refs/heads/main", seen, "ref:"},
		{"a tag", "refs/tags/seats/dev/v1", seen, "ref:"},
		{"no ref", "refs/heads/seats/dev/..", seen, "ref:"},
		{"the seats' root", "refs/heads/seats/dev", seen, "ref:"},
		{"no read", "refs/heads/seats/dev/x", statelog.Position{}, "seen:"},
	} {
		req := lcRefHold(agent("dev"), "0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d9e", "turn-1", c.ref, c.seen)
		err := req.Validate()
		if !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: Validate() = %v, want ErrInvalid naming %s", c.name, err, c.field)
		}
	}
	good := lcRefHold(agent("dev"), "0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d9e", "turn-1", "refs/heads/seats/dev/x", seen)
	if err := good.Validate(); err != nil {
		t.Errorf("a hold on a seat's branch = %v, want it valid", err)
	}
	if !code.Holdable("refs/heads/seats/qa/x") || code.Holdable("refs/heads/main") ||
		code.Holdable("refs/heads/seats/qa") {
		t.Error("Holdable answers a seat's branch, and only one, wrongly")
	}
	key := code.RefHoldKey("0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d9e", "refs/heads/seats/dev/"+strings.Repeat("<", 200))
	if len(key) != 69 || !strings.HasPrefix(key, "0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d9e:") {
		t.Errorf("a ref's hold key is %q (%d bytes), want the repository's id, ':' and 32 hex digits", key, len(key))
	}
}
