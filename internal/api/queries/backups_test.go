package queries_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/store"
)

var backupNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

// register is a backup register holding fixed points, or one that cannot be
// read.
type register struct {
	points []coord.BackupPoint
	err    error
}

func (r register) BackupPoints(context.Context) ([]coord.BackupPoint, error) {
	return r.points, r.err
}

func reach(seq uint64) map[string]coord.Position {
	return map[string]coord.Position{
		"CREWLET_TRACKER_LOG": {Stream: "CREWLET_TRACKER_LOG", Generation: 2, Seq: seq},
		"CREWLET_PAGES_LOG":   {Stream: "CREWLET_PAGES_LOG", Generation: 1, Seq: seq / 2},
	}
}

// fourOwners is node-a's verified copy, node-b's NEWER verified one, node-c's
// newest of all and never verified, and an operator acknowledgement older than
// every copy.
func fourOwners() register {
	return register{points: []coord.BackupPoint{
		{Owner: "node-a", At: backupNow.Add(-26 * time.Hour), Dir: "/var/backups/a",
			Verified: true, Bytes: 4096, Streams: reach(700)},
		{Owner: "node-b", At: backupNow.Add(-2 * time.Hour), Dir: "/var/backups/b",
			Verified: true, Bytes: 8192, Streams: reach(900)},
		{Owner: "node-c", At: backupNow.Add(-time.Hour), Dir: "/var/backups/c",
			Verified: false, Bytes: 16384, Streams: reach(950)},
		{Owner: coord.OperatorBackupOwner, At: backupNow.Add(-30 * time.Hour),
			Verified: true, Streams: reach(600)},
	}}
}

// auditLog is one node's event store holding what three runtime calls left:
// a backup that landed, one that failed, and an operator tool call that is
// not a backup at all.
func auditLog(t *testing.T) *store.EventLog {
	t.Helper()
	log := openStore(t).Events()
	rows := []struct {
		id   string
		at   time.Time
		data events.Payload
		node string
	}{
		{"b-landed", backupNow.Add(-2 * time.Hour), types.NewBackupRequested(types.BackupRequested{
			ActorName: "maya", ActorKind: "human", OperatorID: "pat:maya-laptop",
			Dir: "/var/backups/b", Outcome: types.AuditApplied, Streams: 12,
		}), "node-b"},
		{"b-failed", backupNow.Add(-time.Hour), types.NewBackupRequested(types.BackupRequested{
			ActorName: "token:ops-cron", ActorKind: "operator",
			OperatorID: "token:ops-cron", Dir: "/full/disk",
			Outcome: types.AuditFailed,
		}), "node-a"},
		{"acted", backupNow.Add(-30 * time.Minute), types.NewOperatorActed(types.OperatorActed{
			ActorName: "maya", ActorKind: "human", OperatorID: "pat:maya-laptop",
			Transport: types.TransportAct, Tool: "update_work_item",
			Outcome: types.AuditApplied,
		}), "node-b"},
	}
	for _, row := range rows {
		ev := events.NewFrom(row.data, events.TraceContext{})
		ev.Source, ev.Node, ev.Timestamp = types.OperatorSource, row.node, row.at
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		if err := log.Append(t.Context(), store.EventRecord{
			ID: row.id, Type: ev.Type, Source: ev.Source, Time: row.at,
			Category: "lifecycle", Actor: ev.Actor(), Summary: ev.Summary(),
			Tags: store.ExtractTags(raw), Payload: raw,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return log
}

func backupSources(t *testing.T, r register, floor config.BackupFloor) queries.Sources {
	return queries.Sources{
		Backups: r, BackupFloor: floor, Events: fleetOf(auditLog(t)),
		Now: func() time.Time { return backupNow },
	}
}

func askBackups(t *testing.T, s queries.Sources) queries.BackupsAnswer {
	t.Helper()
	got, ok := answer(t, s, "backups", nil).(queries.BackupsAnswer)
	if !ok {
		t.Fatalf("backups answered %T", got)
	}
	return got
}

// THE ROW MARKED NEWEST IS THE POINT THE TRIM READS, under either policy.
//
// Under `engine` that is the newest VERIFIED copy — node-c's is newer and was
// never opened, which is exactly the file the trim must not delete the log
// against. Under `operator` the nodes' own copies count for nothing, however
// fresh, and the acknowledgement is the only point there is.
func TestTheNewestBackupIsThePointThePolicyCounts(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		floor   config.BackupFloor
		policy  config.BackupFloor
		newest  string
		counted []string
	}{
		{"", config.BackupFloorEngine, "node-b", []string{"node-a", "node-b", "operator"}},
		{config.BackupFloorOperator, config.BackupFloorOperator, "operator", []string{"operator"}},
	} {
		got := askBackups(t, backupSources(t, fourOwners(), c.floor))
		if got.Policy != c.policy {
			t.Errorf("floor %q answered policy %q, want %q", c.floor, got.Policy, c.policy)
		}
		var newest, counted []string
		for _, p := range got.Points {
			if p.Newest {
				newest = append(newest, p.Owner)
			}
			if p.Counted {
				counted = append(counted, p.Owner)
			}
		}
		if !slices.Equal(newest, []string{c.newest}) {
			t.Errorf("policy %s marked %v newest, want exactly %s", c.policy, newest, c.newest)
		}
		slices.Sort(counted)
		if !slices.Equal(counted, c.counted) {
			t.Errorf("policy %s counted %v, want %v", c.policy, counted, c.counted)
		}
	}
}

// Each point is a manifest's claims, newest first, the kind of owner said and
// its reach by stream in a stable order.
func TestEachPointCarriesWhatItsTakerAnnounced(t *testing.T) {
	t.Parallel()
	got := askBackups(t, backupSources(t, fourOwners(), config.BackupFloorEngine))
	owners := make([]string, 0, len(got.Points))
	for _, p := range got.Points {
		owners = append(owners, p.Owner)
	}
	if want := []string{"node-c", "node-b", "node-a", "operator"}; !slices.Equal(owners, want) {
		t.Fatalf("points in order %v, want newest first %v", owners, want)
	}
	b := got.Points[1]
	if b.Kind != queries.BackupOwnerNode || b.Dir != "/var/backups/b" || b.Bytes != 8192 ||
		!b.Verified || !b.TakenAt.Equal(backupNow.Add(-2*time.Hour)) {
		t.Errorf("node-b's point is %+v", b)
	}
	if want := []queries.BackupCover{
		{Stream: "CREWLET_PAGES_LOG", Generation: 1, Seq: 450},
		{Stream: "CREWLET_TRACKER_LOG", Generation: 2, Seq: 900},
	}; !slices.Equal(b.Covers, want) {
		t.Errorf("node-b covers %+v, want %+v", b.Covers, want)
	}
	if op := got.Points[3]; op.Kind != queries.BackupOwnerOperator || op.Bytes != 0 {
		t.Errorf("the acknowledgement reads %+v, want the operator kind and no size", op)
	}
}

// THE HISTORY IS EVERY BACKUP A PERSON ASKED FOR, failures included — which
// the register, holding one point per owner, cannot say — and nothing else the
// runtime audit holds. Each row names the node whose disk holds the copy, who
// asked — the author, what sort of actor that is and the credential they asked
// through — and the directory, off the listing alone.
func TestTheHistoryIsEveryRequestedBackupWithItsHost(t *testing.T) {
	t.Parallel()
	got := askBackups(t, backupSources(t, fourOwners(), config.BackupFloorEngine))
	if len(got.History) != 2 {
		t.Fatalf("history has %d rows, want the two backups and not the tool call: %+v",
			len(got.History), got.History)
	}
	failed, landed := got.History[0], got.History[1]
	if failed.ID != "b-failed" || failed.Outcome != types.AuditFailed ||
		failed.Node != "node-a" || failed.Dir != "/full/disk" ||
		failed.Actor != "token:ops-cron" || failed.ActorKind != "operator" {
		t.Errorf("the newest row is %+v, want the failed backup on node-a", failed)
	}
	if landed.ID != "b-landed" || landed.Outcome != types.AuditApplied ||
		landed.Node != "node-b" || landed.Actor != "maya" || landed.ActorKind != "human" ||
		landed.OperatorID != "pat:maya-laptop" || landed.Dir != "/var/backups/b" {
		t.Errorf("the older row is %+v, want maya's backup on node-b", landed)
	}
	if !got.Coverage.Complete || got.More {
		t.Errorf("a one-node history answered coverage %+v more=%v", got.Coverage, got.More)
	}
}

// An unreadable register fails the answer rather than answering "never backed
// up", which is the claim that would send somebody to take a backup during a
// coordination outage.
func TestAnUnreadableRegisterIsNotANeverBackedUpFleet(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, backupSources(t, register{err: errors.New("broker down")}, ""))
	if _, err := r.Answer(everyGrant(t), "backups", nil); err == nil {
		t.Fatal("an unreadable register answered")
	}
}

// THE DEPLOYMENT'S, on fleet:operate — every row names a directory on a named
// host holding the company's sealed credentials — and unregistered without
// both halves.
func TestTheBackupsAnswerIsTheDeployments(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, backupSources(t, fourOwners(), ""))
	needsExactly(t, r, "backups", iam.GrantFleetOperate)
	for name, s := range map[string]queries.Sources{
		"no register": {Events: fleetOf(auditLog(t))},
		"no history":  {Backups: fourOwners()},
	} {
		bare := queries.NewRegistry()
		queries.Register(bare, s)
		if slices.Contains(bare.Names(), "backups") {
			t.Errorf("backups is registered with %s", name)
		}
	}
}

// THE BACKUPS SCREEN READS WHAT THIS ANSWER SENDS, every shape held both ways,
// and knows exactly the owner kinds and outcomes the engine sends.
func TestTheBackupsScreenReadsWhatTheAnswerSends(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, backupSources(t, fourOwners(), ""))
	raw, err := r.Answer(everyGrant(t), "backups", nil)
	if err != nil {
		t.Fatal(err)
	}
	body := asMap(t, raw)
	holdShape(t, "BackupsAnswer", []map[string]any{body}, false)
	points := rowsOf(t, body["points"])
	holdShape(t, "BackupPointRow", points, false)
	var covers []map[string]any
	for _, p := range points {
		covers = append(covers, rowsOf(t, p["covers"])...)
	}
	holdShape(t, "BackupCover", covers, false)
	holdShape(t, "BackupRunRow", rowsOf(t, body["history"]), false)

	for name, engine := range map[string][]string{
		"BackupOwnerKind": stringsOf(queries.BackupOwnerKinds),
		"BackupOutcome":   stringsOf(queries.BackupOutcomes),
		"BackupPolicy":    stringsOf(config.BackupFloors),
	} {
		got, err := clientsource.Union(clientsource.Tree(t), name)
		if err != nil {
			t.Fatalf("%v — this gate cannot run without the client's declaration", err)
		}
		slices.Sort(got)
		if want := slices.Sorted(slices.Values(engine)); !slices.Equal(got, want) {
			t.Errorf("the dashboard's %s is %v; the engine sends %v", name, got, want)
		}
	}
	for _, k := range queries.BackupOwnerKinds {
		if !k.Valid() {
			t.Errorf("owner kind %q is listed and not valid", k)
		}
	}
}

// stringsOf renders a named-string enum's values for a comparison against the
// dashboard's own copy.
func stringsOf[S ~string](values []S) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, string(v))
	}
	return out
}
