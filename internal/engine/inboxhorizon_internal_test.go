package engine

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE TRACKER READS THE COMPANY'S INBOX HORIZON: the one at the top of the
// company document, `inbox.retention_days`, under the epoch key every domain
// that keeps an inbox reads.
//
// An applier reads no configuration — it reads the epoch the engine builds
// from the published company ([Company.Epoch]) — so the horizon crosses from
// config to the tracker by NAME, and a key the engine states under one name
// and the tracker reads under another compiles, applies and passes every
// tracker case, while every replay rebuilds a year of notices for a company
// that asked to keep a month. So the epoch here is the one the engine builds
// from a parsed company document, the applier is the register's, and the two
// notices sit either side of the company's thirty days: the older one is never
// written, the newer one is.
//
// Mutation: read the tracker's horizon under any key but [inbox.RetentionKey],
// or have Company.Epoch state the configured days under another, and the older
// notice is written.
func TestTheTrackerReadsTheCompanysHorizon(t *testing.T) {
	t.Parallel()
	db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"),
		store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })
	company := &Company{Config: parseCompany(t, `
name: Nimbus
inbox:
  retention_days: 30
roles:
  - name: SWE
    handle: swe
`)}
	epoch := company.Epoch()

	e := &Engine{}
	var heard []inbox.Movement
	e.SetOnInboxMoved(func(moved []inbox.Movement) { heard = append(heard, moved...) })
	entry, ok := registrationFor(worklet.Domain{}.Name())
	if !ok {
		t.Fatal("the register has no tracker domain")
	}
	applier, err := entry.NewApplier(&stateLog{nodeID: "node-a", applyHooks: e.applyHooks()})
	if err != nil {
		t.Fatalf("build the applier: %v", err)
	}

	// THE BROKER'S INSTANT IS NOW, and each change was authored that long
	// before it: a notice is dated by when it was said, and the horizon is
	// counted back from when the log stored it.
	stored := time.Unix(1_800_000_000, 0).UTC()
	day := 24 * time.Hour
	task := worklet.Task{
		V: worklet.DocumentVersion, ID: "t-1", Key: "ENG-1", Project: "ENG",
		Type: "task", Title: "a task", Status: worklet.StatusTodo,
		StatusGroup: worklet.GroupNotStarted, Priority: worklet.PriorityNormal,
		Rank: worklet.RankOrigin, CreatedAt: stored.Add(-60 * day),
		UpdatedAt: stored.Add(-60 * day),
	}
	said := func(op, assignee string, ago time.Duration) worklet.MutationRecord {
		title := op
		rec := inboxRecord(t, worklet.OpPatch, worklet.TaskPatch{Title: &title},
			&worklet.Notify{Kind: worklet.ChangeStatus,
				Snapshot: worklet.Snapshot{Key: "ENG-1", Assignee: assignee}})
		rec.OpID, rec.CreatedAt = "t-1-"+op, stored.Add(-ago)
		return rec
	}
	create := inboxRecord(t, worklet.OpCreate, task, nil)
	create.CreatedAt = stored.Add(-60 * day)
	for seq, rec := range []worklet.MutationRecord{
		create,
		said("forty-days-ago", "past-the-horizon", 40*day),
		said("twenty-days-ago", "inside-it", 20*day),
	} {
		body, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		record := statelog.Record{
			Envelope: statelog.Envelope{
				V: rec.V, Kind: string(rec.Subject.Kind),
				Subject: statelog.Subject{Kind: string(rec.Subject.Kind), ID: rec.Subject.ID},
				Op:      string(rec.Op), OpID: rec.OpID, Writer: rec.Writer,
			},
			Position: statelog.Position{Stream: worklet.Domain{}.Stream().Name, Seq: uint64(seq + 1)},
			Payload:  body,
			StoredAt: stored.Add(time.Duration(seq) * time.Second),
		}
		if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := applier.Apply(t.Context(), tx, record, statelog.ApplyOptions{
				Now: record.StoredAt, StoredAt: record.StoredAt, Epoch: epoch,
				MaxVariables: db.Caps().MaxVariables,
			})
			return err
		}); err != nil {
			t.Fatalf("apply %s: %v", rec.OpID, err)
		}
		applier.Committed(t.Context())
	}

	if len(heard) != 1 || heard[0].Handle != "inside-it" {
		t.Fatalf("the notices written moved %v, want inside-it's alone — the "+
			"company keeps its inbox thirty days, and a notice said forty days "+
			"ago is past it", heard)
	}
}
