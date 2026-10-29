package code

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE APPLIER, CASE BY CASE: records built here, applied in writer
// transactions at positions on the code log's first generation, and the rows
// read back — what every node's copy is made of, without a broker between.

// applyClock is the broker instant every record here is stored at, unless a
// case says otherwise.
var applyClock = time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)

// estate is one node's replicated estate and the next position a record is
// applied at.
type estate struct {
	t    testing.TB
	db   store.ReplicatedHandle
	next uint64
}

// openApplyEstate is a fresh estate from the migrated image.
func openApplyEstate(t testing.TB) *estate {
	t.Helper()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = node.Close() })
	return &estate{t: t, db: db, next: 1}
}

// built is one record and the broker instant it is stored at.
type built struct {
	payload  []byte
	storedAt time.Time
}

// rec encodes a record of op on subject carrying payload.
func rec(t testing.TB, subject Subject, op OpKind, scope Scope, payload any) built {
	t.Helper()
	var body json.RawMessage
	if payload != nil {
		var err error
		if body, err = json.Marshal(payload); err != nil {
			t.Fatalf("encode the payload: %v", err)
		}
	}
	encoded, err := Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			OpID:    fmt.Sprintf("op-%s-%s-%d", subject.Kind, op, time.Now().UnixNano()),
			Subject: subject, Op: op, CreatedAt: applyClock, Gen: 1, Writer: "node-a",
			Scope: scope,
		},
		Mutation: body, Actor: "test", ActorKind: ActorSystem,
	})
	if err != nil {
		t.Fatalf("encode the %s on %s: %v", op, subject, err)
	}
	return built{payload: encoded, storedAt: applyClock}
}

// at is a record stored at a given broker instant.
func (b built) at(storedAt time.Time) built { b.storedAt = storedAt; return b }

// apply runs Gated then Apply for each record in its own writer transaction,
// at the next positions — the framework's loop, without the ledger and the
// checkpoint, which are the framework's and not under test here.
func (e *estate) apply(records ...built) {
	e.t.Helper()
	for _, b := range records {
		if err := e.applyErr(b); err != nil {
			e.t.Fatalf("apply the record at %d: %v", e.next-1, err)
		}
	}
}

// applyErr is [estate.apply] for one record, answering its error; the position
// is spent either way.
func (e *estate) applyErr(b built) error {
	_, err := e.applyRows(b)
	return err
}

// applyCounting applies one record and answers the rows its apply reported —
// what the framework counts against its transaction's row budget.
func (e *estate) applyCounting(b built) int {
	e.t.Helper()
	n, err := e.applyRows(b)
	if err != nil {
		e.t.Fatalf("apply the record at %d: %v", e.next-1, err)
	}
	return n
}

// applyRows is one record applied, and the rows its apply reported.
func (e *estate) applyRows(b built) (int, error) {
	ctx := context.Background()
	env, err := Domain{}.Envelope(b.payload)
	if err != nil {
		return 0, err
	}
	r := statelog.Record{
		Envelope: env, Payload: b.payload, StoredAt: b.storedAt,
		Position: statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: e.next},
	}
	e.next++
	a := NewApplier()
	var rows int
	err = e.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, gated, err := a.Gated(ctx, tx, r); err != nil || gated {
			return err
		}
		var err error
		rows, err = a.Apply(ctx, tx, r, statelog.ApplyOptions{
			StoredAt: b.storedAt, MaxVariables: e.db.Caps().MaxVariables,
		})
		return err
	})
	return rows, err
}

// countersMatchRows holds every count code_repos keeps of a repository's refs
// to a COUNT of the rows themselves.
func (e *estate) countersMatchRows(id string) {
	e.t.Helper()
	for counter, count := range map[string]string{
		"refs":       `target != ''`,
		"branches":   `target != '' AND substr(ref, 1, 11) = 'refs/heads/'`,
		"tags":       `target != '' AND substr(ref, 1, 10) = 'refs/tags/'`,
		"tombstones": `target = ''`,
	} {
		kept := e.num(`SELECT `+counter+` FROM code_repos WHERE id = ?`, id)
		rows := e.num(`SELECT COUNT(*) FROM code_refs WHERE repo_id = ? AND `+count, id)
		if kept != rows {
			e.t.Errorf("%s counts %d %s and its refs hold %d", id, kept, counter, rows)
		}
	}
}

// gated reports whether a record would be gated at the next position.
func (e *estate) gated(b built) statelog.Reason {
	e.t.Helper()
	ctx := context.Background()
	env, err := Domain{}.Envelope(b.payload)
	if err != nil {
		e.t.Fatal(err)
	}
	r := statelog.Record{Envelope: env, Payload: b.payload, StoredAt: b.storedAt,
		Position: statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: e.next}}
	var reason statelog.Reason
	if err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		var gated bool
		var err error
		reason, gated, err = NewApplier().Gated(ctx, tx, r)
		if !gated {
			reason = ""
		}
		return err
	}); err != nil {
		e.t.Fatalf("Gated: %v", err)
	}
	return reason
}

// one reads a single value.
func (e *estate) one(query string, args ...any) any {
	e.t.Helper()
	var v any
	if err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(context.Background(), query, args...).Scan(&v)
	}); err != nil {
		e.t.Fatalf("%q: %v", query, err)
	}
	return v
}

// num reads a single integer.
func (e *estate) num(query string, args ...any) int64 {
	e.t.Helper()
	switch v := e.one(query, args...).(type) {
	case int64:
		return v
	case nil:
		return 0
	default:
		e.t.Fatalf("%q answered %T", query, v)
	}
	return 0
}

// str reads a single text value.
func (e *estate) str(query string, args ...any) string {
	e.t.Helper()
	switch v := e.one(query, args...).(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		e.t.Fatalf("%q answered %T", query, v)
	}
	return ""
}

// oid is a deterministic object id.
func oid(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:40]
}

// settingsFor are a declared repository's settings with one rule per pattern.
func settingsFor(description string, patterns ...string) RepoSettings {
	s := RepoSettings{
		ManagedBy: ManagedConfig, Owner: Owner{Kind: OwnerUnit, Ref: "ENG"}, Visibility: VisibilityCompany,
		DefaultBranch: "main", Description: description, ConfigEpoch: 1,
	}
	for _, p := range patterns {
		s.Protections = append(s.Protections, Protection{Pattern: p, DirectPush: DirectPersons})
	}
	return s
}

// create is the create of repository id named name.
func create(t testing.TB, id, name string) built {
	return rec(t, NameSubject(name), OpCreate,
		Scope{NamePath(NameToken(name)), RepoPath(id)},
		CreateRepo{RepoID: id, Name: name, Settings: settingsFor("")})
}

// pushOf is a push record on repository id carrying entries.
func pushOf(t testing.TB, id string, entries ...PushEntry) built {
	return rec(t, RepoSubject(id), OpPush, Scope{RepoPath(id)}, PushBatch{RepoID: id, Pushes: entries})
}

// packOf is a one-object pack bundle's reference for a push.
func packOf(pushID string) *PackRef {
	sum := sha256.Sum256([]byte(pushID))
	key, err := objstore.ParseKey("0190f3c4-5b6a-7c8d-9e0f-" + hex.EncodeToString(sum[:])[:12])
	if err != nil {
		panic(err)
	}
	return &PackRef{
		PackID: pushID, Object: key, Hash: objstore.HashOf([]byte(pushID)),
		Size:       32 + IndexBytesFor(1) + ReverseBytesFor(1),
		IndexBytes: IndexBytesFor(1), ReverseBytes: ReverseBytesFor(1),
		GitChecksum: oid("pack" + pushID), Objects: 1,
	}
}

// pushEntry is one push of commands with its pack.
func pushEntry(id string, cmds ...Command) PushEntry {
	return PushEntry{
		PushID: id, Principal: Principal{Kind: PrincipalOperator, OperatorID: "op", Seat: "op"},
		Via: ViaGit, Node: "node-a", Commands: cmds, Pack: packOf(id), ReceivedAt: applyClock,
	}
}

// move is an ok command moving ref from old to new.
func move(ref, old, new string) Command {
	return Command{Ref: ref, Old: old, New: new, Result: ResultOK}
}

// A RECORD ON ONE REPOSITORY'S SUBJECT CHANGES ONLY THAT REPOSITORY: the
// subject is the arbitration unit, so a payload naming another repository is
// an error, never an apply to it.
func TestRefsChangeOnlyOnTheirRepoSubject(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"), create(t, "repo-b", "b"))
	wrong := rec(t, RepoSubject("repo-a"), OpPush, Scope{RepoPath("repo-a")},
		PushBatch{RepoID: "repo-b", Pushes: []PushEntry{
			pushEntry("p1", move("refs/heads/main", ZeroOID, oid("c1"))),
		}})
	if err := e.applyErr(wrong); err == nil {
		t.Fatal("a push on repo-a's subject naming repo-b applied")
	}
	if refs := e.num(`SELECT COUNT(*) FROM code_refs`); refs != 0 {
		t.Fatalf("%d refs were written by a record that named another repository", refs)
	}
}

// A CREATE CLAIMS ONLY THE NAME IT ARBITRATED, and never a name that is held
// or a repository the log already removed.
func TestACreateClaimsOnlyTheNameItArbitrated(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	lying := rec(t, NameSubject("a"), OpCreate, Scope{NamePath(NameToken("a")), RepoPath("repo-x")},
		CreateRepo{RepoID: "repo-x", Name: "b", Settings: settingsFor("")})
	if err := e.applyErr(lying); err == nil {
		t.Fatal("a create arbitrated on name a and naming b applied")
	}
	e.apply(create(t, "repo-a", "a"))
	e.apply(create(t, "repo-other", "a"))
	if holder := e.str(`SELECT repo_id FROM code_names WHERE name = 'a'`); holder != "repo-a" {
		t.Fatalf("the name a is held by %s, want the first create's", holder)
	}
	if n := e.num(`SELECT COUNT(*) FROM code_repos`); n != 1 {
		t.Fatalf("%d repositories exist after a second create of a held name, want one", n)
	}
	e.exec(`INSERT INTO code_deletions (repo_id, name, at, version) VALUES ('repo-gone', 'c', 0, 1)`)
	e.apply(create(t, "repo-gone", "c"))
	if n := e.num(`SELECT COUNT(*) FROM code_repos WHERE id = 'repo-gone'`); n != 0 {
		t.Fatal("a create of a repository the log removed made it again")
	}
}

// A PUSH APPLIES EACH ENTRY IN ORDER, a later entry seeing an earlier one's
// refs, and counts every push, pack and byte.
func TestAPushAppliesEachEntryInOrder(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	first, second := pushEntry("p1", move("refs/heads/main", ZeroOID, oid("A"))),
		pushEntry("p2", move("refs/heads/main", oid("A"), oid("B")))
	e.apply(pushOf(t, "repo-a", first, second))
	if got := e.str(`SELECT target FROM code_refs WHERE repo_id = 'repo-a' AND ref = 'refs/heads/main'`); got != oid("B") {
		t.Fatalf("main is at %s, want the second entry's %s", got, oid("B"))
	}
	for name, want := range map[string]int64{
		`SELECT refs FROM code_repos WHERE id = 'repo-a'`:       1,
		`SELECT live_packs FROM code_repos WHERE id = 'repo-a'`: 2,
		`SELECT live_bytes FROM code_repos WHERE id = 'repo-a'`: first.Pack.Size + second.Pack.Size,
		`SELECT pushes FROM code_repos WHERE id = 'repo-a'`:     2,
		`SELECT COUNT(*) FROM code_pushes`:                      2,
		`SELECT COUNT(*) FROM code_packs`:                       2,
		`SELECT version FROM code_repos WHERE id = 'repo-a'`:    statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 2}.Packed(),
	} {
		if got := e.num(name); got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
}

// A REPOSITORY COUNTS THE OBJECTS ITS LIVE PACKS HOLD, moved beside its
// bytes from each pack row's own count: a push adds its pack's, and a purge
// part subtracts those of the live packs it removes. It is the bound the code
// host holds a spooled fetch request to — a want names one object — read from
// the row admission already holds rather than summed per fetch.
func TestARepositoryCountsTheObjectsItsLivePacksHold(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	first, second := pushEntry("p1", move("refs/heads/main", ZeroOID, oid("A"))),
		pushEntry("p2", move("refs/heads/main", oid("A"), oid("B")))
	first.Pack, second.Pack = packHolding("p1", 3), packHolding("p2", 7)
	e.apply(pushOf(t, "repo-a", first, second))
	if got := e.num(`SELECT live_objects FROM code_repos WHERE id = 'repo-a'`); got != 10 {
		t.Fatalf("live_objects = %d after pushes of 3 and 7 objects, want 10", got)
	}
	e.apply(rec(t, RepoSubject("repo-a"), OpDelete, Scope{RepoPath("repo-a")},
		LifecycleRecord{RepoID: "repo-a", ConfigEpoch: 1}), partOf(t, "repo-a", 1))
	if packs := e.num(`SELECT COUNT(*) FROM code_packs`); packs != 0 {
		t.Fatalf("the premise: purge part 1 left %d packs of two", packs)
	}
	if got := e.num(`SELECT live_objects FROM code_repos WHERE id = 'repo-a'`); got != 0 {
		t.Fatalf("live_objects = %d once a purge part removed every pack, want 0", got)
	}
}

// packHolding is a pack bundle's reference for a push, holding n objects.
func packHolding(pushID string, n int64) *PackRef {
	p := packOf(pushID)
	p.Objects, p.IndexBytes, p.ReverseBytes = n, IndexBytesFor(n), ReverseBytesFor(n)
	p.Size = 32*n + p.IndexBytes + p.ReverseBytes
	return p
}

// counters is the counted state of one repository.
func (e *estate) counters(id string) [4]int64 {
	return [4]int64{
		e.num(`SELECT refs FROM code_repos WHERE id = ?`, id),
		e.num(`SELECT live_bytes FROM code_repos WHERE id = ?`, id),
		e.num(`SELECT pushes FROM code_repos WHERE id = ?`, id),
		e.num(`SELECT COUNT(*) FROM code_pushes WHERE repo_id = ?`, id),
	}
}

// AN APPLIED PUSH NEVER APPLIES TWICE — a redelivery or a failover's second
// record carrying it at a LATER position, past the version guard, changes no
// row and no counter.
func TestAnAppliedPushNeverAppliesTwice(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	push := pushOf(t, "repo-a", pushEntry("p1", move("refs/heads/main", ZeroOID, oid("A"))))
	e.apply(push)
	before := e.counters("repo-a")
	e.apply(pushOf(t, "repo-a", pushEntry("p1", move("refs/heads/main", ZeroOID, oid("A")))))
	if after := e.counters("repo-a"); after != before {
		t.Fatalf("a push applied again moved the counters from %v to %v", before, after)
	}
}

// A PUSH REPEATED INSIDE ONE RECORD APPLIES ONCE: no decide forms one, and an
// insert aborted on the push's primary key would stall the log on every node.
func TestAPushRepeatedInsideOneRecordAppliesOnce(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	e.apply(pushOf(t, "repo-a",
		pushEntry("p1", move("refs/heads/main", ZeroOID, oid("A"))),
		pushEntry("p1", move("refs/heads/main", oid("A"), oid("B")))))
	if n := e.num(`SELECT COUNT(*) FROM code_pushes`); n != 1 {
		t.Fatalf("%d push rows, want one", n)
	}
	if got := e.str(`SELECT target FROM code_refs WHERE ref = 'refs/heads/main'`); got != oid("A") {
		t.Fatalf("main is at %s, want the first entry's effect only", got)
	}
}

// A REF MOVED TWICE IN ONE RECORD ENDS AT ITS LAST VALUE: a create, a delete
// and another ref's create leave a tombstone and one live ref.
func TestARefMovedTwiceInOneRecordEndsAtItsLastValue(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	e.apply(pushOf(t, "repo-a", pushEntry("p0", move("refs/heads/base", ZeroOID, oid("0")))))
	e.apply(pushOf(t, "repo-a",
		pushEntry("p1", move("refs/heads/main", ZeroOID, oid("A"))),
		pushEntry("p2", move("refs/heads/main", oid("A"), ZeroOID)),
		pushEntry("p3", move("refs/heads/feat", ZeroOID, oid("F")))))
	if got := e.str(`SELECT target FROM code_refs WHERE ref = 'refs/heads/main'`); got != "" {
		t.Fatalf("main holds %q, want a tombstone", got)
	}
	if got := e.str(`SELECT target FROM code_refs WHERE ref = 'refs/heads/feat'`); got != oid("F") {
		t.Fatalf("feat holds %q", got)
	}
	if refs := e.num(`SELECT refs FROM code_repos WHERE id = 'repo-a'`); refs != 2 {
		t.Fatalf("the repository counts %d refs, want the seeded one and feat", refs)
	}
}

// A DELETE LEAVES A TOMBSTONE at the record's position, and the count drops.
func TestADeleteLeavesATombstone(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"),
		pushOf(t, "repo-a", pushEntry("p1", move("refs/heads/main", ZeroOID, oid("A")))),
		pushOf(t, "repo-a", pushEntry("p2", move("refs/heads/main", oid("A"), ZeroOID))))
	at := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 3}.Packed()
	if got := e.str(`SELECT target FROM code_refs WHERE ref = 'refs/heads/main'`); got != "" {
		t.Fatalf("main holds %q after its delete", got)
	}
	if pos := e.num(`SELECT position FROM code_refs WHERE ref = 'refs/heads/main'`); pos != at {
		t.Fatalf("the tombstone is at %d, want the delete's %d", pos, at)
	}
	if refs := e.num(`SELECT refs FROM code_repos WHERE id = 'repo-a'`); refs != 0 {
		t.Fatalf("the repository counts %d refs after deleting its only one", refs)
	}
}

// THE COUNTERS ARE THE ROWS: what code_repos counts of a repository's refs —
// live refs, the branches and tags among them, and tombstones — equals a COUNT
// of its code_refs rows after every kind of move: a create, an update, a
// delete, a recreate of a tombstone, a delete of a ref the repository has no
// row for, a ref moved twice in one record, and a purge part. A read serves
// the counters rather than counting, so a counter that drifts is a number
// every surface shows wrong for ever.
func TestTheCountersAreTheRows(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	steps := []built{
		pushOf(t, "repo-a", pushEntry("p1",
			move("refs/heads/main", ZeroOID, oid("1")), move("refs/heads/feat", ZeroOID, oid("2")),
			move("refs/tags/v1", ZeroOID, oid("3")), move("refs/tags/v2", ZeroOID, oid("4")))),
		pushOf(t, "repo-a", pushEntry("p2",
			move("refs/heads/main", oid("1"), oid("5")), move("refs/heads/feat", oid("2"), ZeroOID),
			move("refs/tags/v1", oid("3"), ZeroOID))),
		pushOf(t, "repo-a", pushEntry("p3",
			move("refs/heads/feat", ZeroOID, oid("6")), move("refs/heads/never", oid("7"), ZeroOID))),
		pushOf(t, "repo-a",
			pushEntry("p4", move("refs/tags/v3", ZeroOID, oid("8"))),
			pushEntry("p5", move("refs/tags/v3", oid("8"), ZeroOID), move("refs/tags/v1", ZeroOID, oid("9")))),
	}
	for i, step := range steps {
		e.apply(step)
		e.countersMatchRows("repo-a")
		if t.Failed() {
			t.Fatalf("the counters left the rows at step %d", i+1)
		}
	}
	for counter, want := range map[string]int64{"refs": 4, "branches": 2, "tags": 2, "tombstones": 2} {
		if got := e.num(`SELECT ` + counter + ` FROM code_repos WHERE id = 'repo-a'`); got != want {
			t.Errorf("the repository counts %d %s, want %d", got, counter, want)
		}
	}

	e.apply(rec(t, RepoSubject("repo-a"), OpDelete, Scope{RepoPath("repo-a")},
		LifecycleRecord{RepoID: "repo-a", ConfigEpoch: 1}))
	// THE PART'S BUDGET REACHES EIGHT REFS past the repository's pushes — in
	// ref order, branches and the tombstone of `never` among them — and leaves
	// the rest, so what it subtracts is measured against a partial set.
	e.seed("repo-a", PurgePartRows-5-8, 10, 0)
	e.apply(partOf(t, "repo-a", 1))
	if left := e.num(`SELECT COUNT(*) FROM code_refs WHERE repo_id = 'repo-a'`); left != 6+10-8 {
		t.Fatalf("the premise: the purge part leaves %d refs, want %d", left, 6+10-8)
	}
	e.countersMatchRows("repo-a")
}

// SETTINGS REPLACE A REPOSITORY'S RULES WHOLE, archive and unarchive it, and
// never resurrect a deleted one.
func TestSettingsReplaceProtectionsWhole(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	settings := func(s RepoSettings) built {
		return rec(t, RepoSubject("repo-a"), OpSettings, Scope{RepoPath("repo-a")},
			SettingsRecord{RepoID: "repo-a", Settings: s})
	}
	e.apply(settings(settingsFor("", "refs/heads/main", "refs/tags/*")))
	e.apply(settings(settingsFor("", "refs/heads/release/*")))
	if got := e.str(`SELECT group_concat(pattern) FROM code_protections WHERE repo_id = 'repo-a'`); got != "refs/heads/release/*" {
		t.Fatalf("the rules are %q, want exactly the last settings' one", got)
	}
	archived := settingsFor("")
	archived.Archived = true
	e.apply(settings(archived))
	if s := e.str(`SELECT status FROM code_repos WHERE id = 'repo-a'`); s != string(RepoStatusArchived) {
		t.Fatalf("status %q after archiving", s)
	}
	e.apply(settings(settingsFor("")))
	if s := e.str(`SELECT status FROM code_repos WHERE id = 'repo-a'`); s != string(RepoStatusActive) {
		t.Fatalf("status %q after unarchiving", s)
	}
	e.apply(rec(t, RepoSubject("repo-a"), OpDelete, Scope{RepoPath("repo-a")},
		LifecycleRecord{RepoID: "repo-a", ConfigEpoch: 1}))
	e.apply(settings(settingsFor("after")))
	if s := e.str(`SELECT status FROM code_repos WHERE id = 'repo-a'`); s != string(RepoStatusDeleted) {
		t.Fatalf("settings on a deleted repository left it %q", s)
	}
}

// A RESTORE BRINGS BACK THE SETTINGS IT CARRIES AND THE REFS THE DELETE NEVER
// TOUCHED — the visibility among them, which the repository was created
// without: a restore that kept the row's own would reopen a unit repository to
// every seat.
func TestARestoreBringsBackSettingsAndRefs(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"),
		pushOf(t, "repo-a", pushEntry("p1", move("refs/heads/main", ZeroOID, oid("A")))),
		rec(t, RepoSubject("repo-a"), OpDelete, Scope{RepoPath("repo-a")},
			LifecycleRecord{RepoID: "repo-a", ConfigEpoch: 1}))
	back := settingsFor("back")
	back.ConfigEpoch, back.Visibility = 2, VisibilityUnit
	e.apply(rec(t, RepoSubject("repo-a"), OpRestore, Scope{RepoPath("repo-a")},
		LifecycleRecord{RepoID: "repo-a", ConfigEpoch: 2, Settings: &back}))
	if s := e.str(`SELECT status FROM code_repos WHERE id = 'repo-a'`); s != string(RepoStatusActive) {
		t.Fatalf("status %q after the restore", s)
	}
	if d := e.one(`SELECT deleted_at FROM code_repos WHERE id = 'repo-a'`); d != nil {
		t.Fatalf("deleted_at is %v after the restore", d)
	}
	if v := e.str(`SELECT visibility FROM code_repos WHERE id = 'repo-a'`); v != string(VisibilityUnit) {
		t.Fatalf("visibility %q after the restore, want the restore's own %q", v, VisibilityUnit)
	}
	if got := e.str(`SELECT target FROM code_refs WHERE ref = 'refs/heads/main'`); got != oid("A") {
		t.Fatalf("main is %q after the restore", got)
	}
}

// seed writes bulk rows of a repository's history straight into the estate:
// pushes push rows, refs refs and the first packs packs ([estate.seedPacks]).
func (e *estate) seed(id string, pushes, refs, packs int) {
	e.t.Helper()
	ctx := context.Background()
	vars := e.db.Caps().MaxVariables
	if err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := store.InsertRows(ctx, tx, vars,
			`INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, via, commands,
				received_at, row_bytes) VALUES `, `(?, ?, ?, ?, 'seed', 'operator', 'git', '[]', 0, 2)`, ``, pushes,
			func(i int) []any { return []any{fmt.Sprintf("%s-push-%06d", id, i), id, i + 1, i} }); err != nil {
			return err
		}
		_, err := store.InsertRows(ctx, tx, vars,
			`INSERT INTO code_refs (repo_id, ref, target, position, at) VALUES `, `(?, ?, ?, 1, 0)`, ``,
			refs, func(i int) []any { return []any{id, fmt.Sprintf("refs/heads/r%06d", i), oid(id)} })
		return err
	}); err != nil {
		e.t.Fatalf("seed %s: %v", id, err)
	}
	e.exec(`UPDATE code_repos SET refs = refs + ?, branches = branches + ? WHERE id = ?`, refs, refs, id)
	e.seedPacks(id, 0, packs)
}

// seedPacks writes n live packs of a repository straight into the estate, the
// from-th of them first ([idOf], [sumOf]) — each a one-object pack of ten
// bytes added at the log's first position, its content dated from there as a
// push's is — and moves the live-pack counters by them, as a push's apply
// would.
func (e *estate) seedPacks(id string, from, n int) {
	e.t.Helper()
	ctx := context.Background()
	if err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := store.InsertRows(ctx, tx, e.db.Caps().MaxVariables,
			`INSERT INTO code_packs (repo_id, pack_id, part, object, hash, size, index_bytes,
				reverse_bytes, git_checksum, objects, kind, state, added_position, content_position) VALUES `,
			`(?, ?, 'pack', ?, ?, 10, 0, 0, ?, 1, 'push', 'live', 1, 1)`, ``, n,
			func(i int) []any {
				p := packOf(idOf(id, from+i))
				return []any{id, p.PackID, p.Object.String(), string(p.Hash), p.GitChecksum}
			})
		return err
	}); err != nil {
		e.t.Fatalf("seed %s's packs: %v", id, err)
	}
	e.exec(`UPDATE code_repos SET live_packs = live_packs + ?, live_bytes = live_bytes + ?,
		live_objects = live_objects + ? WHERE id = ?`, n, 10*n, n, id)
}

// seedWeighed writes n push rows of a repository, each weighing width bytes
// by its row_bytes, stamped at the epoch and oldest first — rows a purge or
// the history horizon has to weigh, without carrying their bytes.
func (e *estate) seedWeighed(id string, n, width int) {
	e.t.Helper()
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, via, commands,
			received_at, row_bytes)
		SELECT printf('%s-weighed-%06d', ?, i), ?, i + 1, i, 'seed', 'operator', 'git', '[]', 0, ? FROM k`,
		n, id, id, width)
}

// exec runs one statement in a writer transaction — a case's hand-built row,
// never the applier's.
func (e *estate) exec(query string, args ...any) {
	e.t.Helper()
	if err := e.db.Tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(query, args...)
		return err
	}); err != nil {
		e.t.Fatalf("%q: %v", query, err)
	}
}

// partOf is purge part n of repository id.
func partOf(t testing.TB, id string, n int) built {
	return rec(t, RepoSubject(id), OpPurgePart, Scope{RepoPath(id)}, PurgePart{RepoID: id, Part: n})
}

// A PURGE PART IS BOUNDED AND IDEMPOTENT: it deletes exactly PurgePartRows
// rows, pushes first, and only the next part deletes anything — a redelivered
// or skipped one deletes nothing more.
func TestAPurgePartIsBoundedAndIdempotent(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"),
		rec(t, RepoSubject("repo-a"), OpDelete, Scope{RepoPath("repo-a")},
			LifecycleRecord{RepoID: "repo-a", ConfigEpoch: 1}))
	e.seed("repo-a", 2500, 1000, 10)
	total := func() int64 {
		return e.num(`SELECT COUNT(*) FROM code_pushes`) + e.num(`SELECT COUNT(*) FROM code_refs`) +
			e.num(`SELECT COUNT(*) FROM code_packs`)
	}
	e.apply(partOf(t, "repo-a", 1))
	if left := total(); left != 3510-PurgePartRows {
		t.Fatalf("part 1 left %d rows of 3510, want %d", left, 3510-PurgePartRows)
	}
	if pushes := e.num(`SELECT COUNT(*) FROM code_pushes`); pushes != 0 {
		t.Fatalf("part 1 left %d pushes — pushes go first", pushes)
	}
	if s := e.str(`SELECT status FROM code_repos WHERE id = 'repo-a'`); s != string(RepoStatusPurging) {
		t.Fatalf("status %q after a part", s)
	}
	if parts := e.num(`SELECT purge_parts FROM code_repos WHERE id = 'repo-a'`); parts != 1 {
		t.Fatalf("purge_parts = %d after part 1", parts)
	}
	before := total()
	e.apply(partOf(t, "repo-a", 1), partOf(t, "repo-a", 3))
	if after := total(); after != before {
		t.Fatalf("a repeated part and a skipped one deleted %d rows", before-after)
	}
}

// A PURGE PART IS BOUNDED BY WHAT ITS PUSHES WEIGH, not only by how many
// there are: push rows go oldest first until their widths reach
// PurgePartBytes — at most one row past it — and a part whose pushes were cut
// there deletes nothing else, so the refs wait for a part of their own. A
// push row is the domain's widest row and deleting one frees its overflow
// pages, so a part of PurgePartRows wide rows held every node's write lane
// for seconds.
func TestAPurgePartIsBoundedByWhatItsPushesWeigh(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"),
		rec(t, RepoSubject("repo-a"), OpDelete, Scope{RepoPath("repo-a")},
			LifecycleRecord{RepoID: "repo-a", ConfigEpoch: 1}))
	// A WIDTH THAT DOES NOT DIVIDE THE BUDGET, so the part passes it by a
	// row: eleven rows of 3 MiB are the first to reach 32 MiB.
	const width, heavy = 3 << 20, 15
	e.seedWeighed("repo-a", heavy, width)
	e.seed("repo-a", 0, 100, 0)
	pushes := func() int64 { return e.num(`SELECT COUNT(*) FROM code_pushes`) }
	refs := func() int64 { return e.num(`SELECT COUNT(*) FROM code_refs`) }

	e.apply(partOf(t, "repo-a", 1))
	took := (PurgePartBytes + width - 1) / width
	if left := pushes(); left != int64(heavy-took) {
		t.Fatalf("part 1 left %d of %d pushes of %d bytes, want %d: rows until their bytes "+
			"reach PurgePartBytes (%d), and no further", left, heavy, width, heavy-took, PurgePartBytes)
	}
	if left := refs(); left != 100 {
		t.Fatalf("a part cut by its pushes' bytes deleted %d refs — the refs wait for a part "+
			"whose pushes are done", 100-left)
	}
	e.apply(partOf(t, "repo-a", 2))
	if p, r := pushes(), refs(); p != 0 || r != 0 {
		t.Fatalf("part 2 left %d pushes and %d refs: the pushes left fit the budget, so the "+
			"part goes on to the refs", p, r)
	}
	e.countersMatchRows("repo-a")
}

// seedWeighedChanges writes n changes of a repository, numbered from 1, each
// with a revision, an author and a body its change says weighs width bytes —
// changes a purge part has to weigh, without carrying their bytes, as
// seedWeighed's pushes are: a part reads a body's width from its change and
// never reads the body.
func (e *estate) seedWeighedChanges(id string, n, width int) {
	e.t.Helper()
	e.seedChangeRows(id, n, 1, 1, 1)
	e.exec(`UPDATE code_changes SET body_bytes = ? WHERE repo_id = ?`, width, id)
}

// A PURGE PART WEIGHS ITS CHANGES' BODIES IN ONE BUDGET WITH ITS PUSHES: after
// its push rows and its narrow rows, a part takes whole changes in number
// order only while what its wide rows weigh — the pushes it took, then each
// body — is under PurgePartBytes: the first change whenever the pushes left
// room, and so at most one change past the bound, and none once the pushes
// reached it. A walk the bytes stopped cuts nothing, so the change after the
// last it took keeps every row. A change's body is the one wide value it
// keeps, and deleting it frees its overflow pages, which a count of rows does
// not see: by rows alone a part took every change here, on top of whatever
// its pushes weighed.
//
// Mutation: take the changes by their rows alone, and the bodies alone go in
// one part; weigh them apart from the pushes, and the parts after the pushes
// take eleven; cut the change a walk the bytes stopped at, and it loses its
// revision and its author.
func TestAPurgePartIsBoundedByWhatItsChangesWeigh(t *testing.T) {
	t.Parallel()
	// A WIDTH THAT DOES NOT DIVIDE THE BUDGET: eleven bodies of 3 MiB are the
	// first to reach 32 MiB.
	const width, seeded = 3 << 20, 15
	for name, tc := range map[string]struct {
		// pushes push rows of pushWidth bytes each go before the changes.
		pushes, pushWidth int
		// left is how many of the changes the part leaves.
		left int
	}{
		"bodies alone": {left: seeded - 11},
		// THIRTY MEBIBYTES OF PUSHES leave room for one change, whose body
		// takes the part past the bound.
		"after the pushes": {pushes: 3, pushWidth: 10 << 20, left: seeded - 1},
		// THIRTY-TWO leave none.
		"pushes at the bound": {pushes: 4, pushWidth: 8 << 20, left: seeded},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := openApplyEstate(t)
			e.apply(create(t, "repo-a", "a"),
				rec(t, RepoSubject("repo-a"), OpDelete, Scope{RepoPath("repo-a")},
					LifecycleRecord{RepoID: "repo-a", ConfigEpoch: 1}))
			if tc.pushes > 0 {
				e.seedWeighed("repo-a", tc.pushes, tc.pushWidth)
			}
			e.seedWeighedChanges("repo-a", seeded, width)
			e.apply(partOf(t, "repo-a", 1))
			if left := e.num(`SELECT COUNT(*) FROM code_pushes WHERE repo_id = 'repo-a'`); left != 0 {
				t.Fatalf("the part left %d of its %d pushes, which fit its bytes and go first", left, tc.pushes)
			}
			changes := e.num(`SELECT COUNT(*) FROM code_changes WHERE repo_id = 'repo-a'`)
			if changes != int64(tc.left) {
				t.Fatalf("the part left %d of %d changes whose bodies weigh %d bytes each, want %d: "+
					"whole changes while the part's wide rows weigh under PurgePartBytes (%d)",
					changes, seeded, width, tc.left, PurgePartBytes)
			}
			// THE CHANGES LEFT ARE WHOLE: nothing cut the first of them.
			prefix := "repo-a-change-%"
			for table, rows := range map[string]int64{
				"code_revisions":      e.num(`SELECT COUNT(*) FROM code_revisions WHERE change_id LIKE ?`, prefix),
				"code_change_authors": e.num(`SELECT COUNT(*) FROM code_change_authors WHERE change_id LIKE ?`, prefix),
				"code_change_bodies":  e.num(`SELECT COUNT(*) FROM code_change_bodies WHERE change_id LIKE ?`, prefix),
			} {
				if rows != changes {
					t.Errorf("%s holds %d rows of the %d changes left, one each: a walk the bytes stopped "+
						"cuts nothing", table, rows, changes)
				}
			}
		})
	}
}

// THE PUSH HISTORY'S HORIZON IS BOUNDED BY WHAT ITS ROWS WEIGH: a push record
// removes expired push rows oldest first until their widths reach
// HorizonBytesPerRecord, which is the most a record can add, so a wide
// history keeps pace without the record that removes it holding the write
// lane for tens of milliseconds more.
func TestTheHistoryHorizonIsBoundedByWhatItsRowsWeigh(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	const width, expired = 300_000, 10
	e.seedWeighed("repo-a", expired, width)
	e.apply(pushOf(t, "repo-a", pushEntry("fresh", move("refs/heads/main", ZeroOID, oid("A")))))
	took := (HorizonBytesPerRecord + width - 1) / width
	if left := e.num(`SELECT COUNT(*) FROM code_pushes WHERE repo_id = 'repo-a'`); left != int64(expired-took+1) {
		t.Fatalf("repo-a holds %d pushes, want %d: the %d expired less the %d whose bytes "+
			"reach HorizonBytesPerRecord (%d), plus its own", left, expired-took+1, expired, took,
			HorizonBytesPerRecord)
	}
	if oldest := e.num(`SELECT MIN(at) FROM code_pushes WHERE repo_id = 'repo-a'`); oldest != int64(took) {
		t.Fatalf("the oldest push repo-a keeps is at %d, want %d — oldest first", oldest, took)
	}
}

// A PUSH ROW RECORDS WHAT IT WEIGHS: row_bytes is the bytes of the commands,
// the introduced ids and, for a seat's push, the run the row stores — a coding
// box's run and coding agent among it — what deleting it costs, and what the
// horizon and a purge part are bounded by.
//
// Mutations: weigh a row by its commands and introduced ids alone
// (pushRowBytes dropping the run) — the seat's row weighs less than it holds;
// weigh it without the coding run (pushRowBytes dropping RunID) — the box's
// does.
func TestAPushRowRecordsWhatItWeighs(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	person := pushEntry("p1", move("refs/heads/main", ZeroOID, oid("A")),
		move("refs/tags/v1", ZeroOID, oid("B")))
	person.Introduced, person.IntroducedCount = []string{oid("A"), oid("B")}, 2
	seat := pushEntry("p2", move("refs/heads/seats/docs-writer/eng-12", ZeroOID, oid("C")))
	seat.Introduced, seat.IntroducedCount = []string{oid("C")}, 1
	seat.Principal = Principal{Kind: PrincipalAgent, Seat: "docs-writer", Run: RunProvenance{
		TurnID: "0192f00d-0000-7000-8000-000000000001", WorkBackend: "native", WorkKey: "ENG-12",
		Model: "claude-sonnet-4-5"}}
	seat.Via = ViaEngine
	box := pushEntry("p3", move("refs/heads/seats/docs-writer/eng-13", ZeroOID, oid("D")))
	box.Introduced, box.IntroducedCount = []string{oid("D")}, 1
	box.Principal = Principal{Kind: PrincipalAgent, Seat: "docs-writer", Run: RunProvenance{
		TurnID: "0192f00d-0000-7000-8000-000000000001", WorkBackend: "native", WorkKey: "ENG-13",
		Model: "claude-sonnet-4-5", RunID: "3f1c2b4a-5d6e-4f70-8a9b-0c1d2e3f4a5b", CodingAgent: "claude-code"}}
	e.apply(pushOf(t, "repo-a", person, seat, box))
	for _, id := range []string{"p1", "p2", "p3"} {
		got := e.num(`SELECT row_bytes FROM code_pushes WHERE push_id = ?`, id)
		want := e.num(`SELECT length(CAST(commands AS BLOB)) + length(CAST(introduced AS BLOB)) +
			length(CAST(turn_id AS BLOB)) + length(CAST(work_backend AS BLOB)) +
			length(CAST(work_key AS BLOB)) + length(CAST(model AS BLOB)) +
			length(CAST(run_id AS BLOB)) + length(CAST(coding_agent AS BLOB))
			FROM code_pushes WHERE push_id = ?`, id)
		if got != want || e.str(`SELECT introduced FROM code_pushes WHERE push_id = ?`, id) == "" {
			t.Errorf("push %s's row weighs %d bytes by row_bytes, and its commands, introduced ids and run "+
				"are %d", id, got, want)
		}
	}
	if run := e.num(`SELECT length(turn_id) FROM code_pushes WHERE push_id = 'p2'`); run == 0 {
		t.Fatal("the seat's push row holds no turn: the case weighs no run")
	}
	if run := e.num(`SELECT length(run_id) FROM code_pushes WHERE push_id = 'p3'`); run == 0 {
		t.Fatal("the box's push row holds no coding run: the case weighs none")
	}
}

// A PURGE LEAVES ONLY ITS MARKER: every row of the repository goes — its
// published chain and the parts of every layer among them — its name returns,
// and the marker drops every later record about it — counted — while the
// purge's own record, redelivered, changes nothing.
//
// Mutation: drop the chain's two statements from the purge, and its layer and
// its part outlive the repository.
func TestAPurgeLeavesOnlyItsMarker(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"),
		pushOf(t, "repo-a", pushEntry("p1", move("refs/heads/main", ZeroOID, oid("A")))),
		recordOf(t, publication("repo-a", 2, layerOver("root", "", nil, applyClock, packOf("p1").GitChecksum), nil)),
		rec(t, RepoSubject("repo-a"), OpSettings, Scope{RepoPath("repo-a")},
			SettingsRecord{RepoID: "repo-a", Settings: settingsFor("", "refs/heads/main")}),
		rec(t, RepoSubject("repo-a"), OpDelete, Scope{RepoPath("repo-a")},
			LifecycleRecord{RepoID: "repo-a", ConfigEpoch: 1}))
	if n := e.num(`SELECT COUNT(*) FROM code_layer_parts WHERE repo_id = 'repo-a'`); n != 1 {
		t.Fatalf("the premise: the repository's chain holds %d parts, want its layer's one", n)
	}
	// A RETIRED PACK STILL INSIDE ITS GRACE goes with its repository: no read
	// begins on a deleted one. SO DOES A REF DROP inside its own: the packs
	// whose objects it keeps go too.
	e.seedRetired("repo-a", 3, applyClock.Add(-time.Hour), 2)
	e.seedDrops("repo-a", 3, applyClock.Add(-time.Hour), 2)
	purge := rec(t, RepoSubject("repo-a"), OpPurge,
		Scope{NamePath(NameToken("a")), RepoPath("repo-a")},
		Purge{RepoID: "repo-a", Name: "a", Reason: "past retention"})
	e.apply(purge)
	for _, table := range []string{"code_repos", "code_refs", "code_packs", "code_pushes", "code_protections",
		"code_packs_retired", "code_ref_drops", "code_layers", "code_layer_parts"} {
		column := "repo_id"
		if table == "code_repos" {
			column = "id"
		}
		if n := e.num(`SELECT COUNT(*) FROM ` + table + ` WHERE ` + column + ` = 'repo-a'`); n != 0 {
			t.Errorf("%s holds %d rows of the purged repository", table, n)
		}
	}
	if n := e.num(`SELECT COUNT(*) FROM code_names WHERE token = ?`, NameToken("a")); n != 0 {
		t.Fatalf("the purged repository's name is still claimed")
	}
	env, _ := DecodeEnvelope(purge.payload)
	if id := e.str(`SELECT purge_record_id FROM code_deletions WHERE repo_id = 'repo-a'`); id != env.OpID {
		t.Fatalf("the marker names record %q, want the purge's %q", id, env.OpID)
	}
	later := pushOf(t, "repo-a", pushEntry("p9", move("refs/heads/main", ZeroOID, oid("Z"))))
	if reason := e.gated(later); reason != statelog.ReasonDeleted {
		t.Fatalf("a push after the purge is gated %q, want %q", reason, statelog.ReasonDeleted)
	}
	if rejects := e.num(`SELECT rejects FROM code_deletions WHERE repo_id = 'repo-a'`); rejects != 1 {
		t.Fatalf("the marker counted %d rejects, want the one", rejects)
	}
	if reason := e.gated(purge); reason != "" {
		t.Fatalf("the purge's own record, redelivered, is gated %q", reason)
	}
	e.apply(purge)
	if n := e.num(`SELECT COUNT(*) FROM code_deletions`); n != 1 {
		t.Fatalf("%d markers after the purge applied again", n)
	}
}

// A PURGE RELEASES ONLY ITS OWN NAME CLAIM: a claim on the same token naming
// another repository is somebody else's.
func TestAPurgeReleasesOnlyItsOwnNameClaim(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"),
		rec(t, RepoSubject("repo-a"), OpDelete, Scope{RepoPath("repo-a")},
			LifecycleRecord{RepoID: "repo-a", ConfigEpoch: 1}))
	e.exec(`UPDATE code_names SET repo_id = 'repo-b' WHERE token = ?`, NameToken("a"))
	e.apply(rec(t, RepoSubject("repo-a"), OpPurge, Scope{NamePath(NameToken("a")), RepoPath("repo-a")},
		Purge{RepoID: "repo-a", Name: "a", Reason: "past retention"}))
	if holder := e.str(`SELECT repo_id FROM code_names WHERE token = ?`, NameToken("a")); holder != "repo-b" {
		t.Fatalf("the claim is held by %q after purging repo-a, want repo-b's kept", holder)
	}
}
