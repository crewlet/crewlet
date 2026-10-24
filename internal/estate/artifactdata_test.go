package estate

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ledger is a data node's datastore as the cases reach it: every read answers
// a value naming what it was asked, every write records its call, and one
// store answers every refusal the domain has.
type ledger struct {
	mu    sync.Mutex
	calls []string
	keys  []string
}

// limited is the store whose every write is refused by the node's budget.
var limited = artifacts.IDFor("the limited store")

func (l *ledger) For(context.Context, string) (Backend, bool) {
	return Backend{ArtifactData: ArtifactDataHalf{Reader: l, Writer: l}}, true
}

func (l *ledger) wrote(op string, c artifactdata.Call) artifactdata.WriteResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, op+":"+c.Authority.Actor.Name+":"+string(c.Authority.Role))
	l.keys = append(l.keys, c.OpKey)
	return artifactdata.WriteResult{Verdict: artifacts.DataApplied, Seq: 42,
		Result: statelog.Result{Outcome: statelog.OutcomeApplied, OpID: c.OpKey}}
}

func (l *ledger) Docs(_ context.Context, id artifacts.ID, rd artifactdata.Reading, collection string,
	since int64, cursor string, limit int) (artifactdata.DocPage, error) {
	if id == limited {
		return artifactdata.DocPage{}, fmt.Errorf("%w: %s", artifactdata.ErrGone, id)
	}
	return artifactdata.DocPage{Docs: []artifactdata.Doc{{Collection: collection, ID: rd.Record +
		"|" + string(rd.Role) + "|" + string(rd.Fresh.Level)}}, Through: since,
		After: fmt.Sprintf("%s+%d", cursor, limit)}, nil
}

func (l *ledger) Doc(_ context.Context, _ artifacts.ID, _ artifactdata.Reading, collection, doc string) (
	artifactdata.Doc, error) {
	if doc == "missing" {
		return artifactdata.Doc{}, fmt.Errorf("%w: %s", artifactdata.ErrNotFound, doc)
	}
	return artifactdata.Doc{Collection: collection, ID: doc, Version: 7}, nil
}

func (l *ledger) Query(_ context.Context, _ artifacts.ID, _ artifactdata.Reading, collection string,
	q query.Query) (artifactdata.DocPage, error) {
	if len(q.Where) > 0 && q.Where[0].Path == "meta.zip" {
		return artifactdata.DocPage{}, fmt.Errorf("%w: meta.zip", query.ErrNotIndexed)
	}
	return artifactdata.DocPage{Docs: []artifactdata.Doc{{Collection: collection, ID: q.After}}}, nil
}

func (l *ledger) History(_ context.Context, _ artifacts.ID, _ artifactdata.Reading,
	filter artifactdata.HistoryFilter, cursor string, limit int) (artifactdata.HistoryPage, error) {
	return artifactdata.HistoryPage{Entries: []artifactdata.HistoryEntry{{OpID: filter.Field,
		Changes: []artifactdata.Change{{Collection: filter.Collection, Doc: filter.Doc}}}},
		After: fmt.Sprintf("%s+%d", cursor, limit)}, nil
}

func (l *ledger) Schema(context.Context, artifacts.ID) ([]artifacts.Collection, error) {
	return []artifacts.Collection{{Name: "review", Read: artifacts.ReadAll,
		Columns: []artifacts.Column{{Key: "done", Type: artifacts.ColumnCheckbox}}}}, nil
}

func (l *ledger) Store(_ context.Context, id artifacts.ID) (artifactdata.StoreInfo, error) {
	return artifactdata.StoreInfo{Artifact: id, Footprint: 99, Digest: "abc"}, nil
}

func (l *ledger) Usage(context.Context) (artifactdata.Usage, error) {
	return artifactdata.Usage{Stores: 2, Footprint: 512, Complete: true}, nil
}

func (l *ledger) State(_ context.Context, _ artifacts.ID, rd artifactdata.Reading) ([]artifactdata.StateEntry, error) {
	return []artifactdata.StateEntry{{Key: rd.Record, Value: []byte(`"dark"`)}}, nil
}

func (l *ledger) Write(_ context.Context, c artifactdata.Call, id artifacts.ID, in artifacts.DataWrite) (
	artifactdata.WriteResult, error) {
	if id == limited {
		return artifactdata.WriteResult{}, &artifactdata.RateLimited{Scope: "principal", Cost: 3,
			RetryAfter: 1500 * time.Millisecond}
	}
	res := l.wrote(fmt.Sprintf("write:%d", len(in.Ops)), c)
	return res, nil
}

func (l *ledger) Undo(_ context.Context, c artifactdata.Call, _ artifacts.ID, seq int64) (
	artifactdata.WriteResult, error) {
	if seq == 0 {
		return artifactdata.WriteResult{}, artifactdata.ErrNotUndoable
	}
	return l.wrote(fmt.Sprintf("undo:%d", seq), c), nil
}

func (l *ledger) Configure(_ context.Context, c artifactdata.Call, _ artifacts.ID,
	collections []artifacts.Collection) (artifactdata.WriteResult, error) {
	if len(collections) == 0 {
		return artifactdata.WriteResult{}, fmt.Errorf("%w: too many", artifactdata.ErrMigrationTooLarge)
	}
	return l.wrote(fmt.Sprintf("configure:%d", len(collections)), c), nil
}

func (l *ledger) PurgeArtifactState(_ context.Context, c artifactdata.Call, opID string, _ artifacts.ID) error {
	l.wrote("purge:"+opID, c)
	return nil
}

// EVERY DATASTORE OPERATION CROSSES WHOLE: a node holding no data reaches a
// data node's datastore with every argument (the reading's record, role and
// level, the call's stamped authority and key), every answer and every
// refusal's identity, the node's write budget's Retry-After included.
//
// Mutation: drop a field from an operation's args type, or RateLimited from
// typedKinds, and a row goes red.
func TestEveryDatastoreOperationCrossesWhole(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	l := &ledger{}
	stop, err := Serve(t.Context(), f.start(t), ServerOptions{
		Self: "data-d", Local: l, Envelopes: fleetEnvelopes,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	f.placement.set(func(p *fakePlacement) { p.nodes = []string{"data-d"} })
	ctx := t.Context()
	d := f.client.ArtifactData()
	id := artifacts.IDFor("crossing")
	rd := artifactdata.Reading{Record: "bob", Role: artifacts.RoleContributor,
		Fresh: statelog.Freshness{Level: statelog.ReadSession}}
	jane := artifactdata.Call{
		Authority: artifacts.Authority{Actor: iam.Actor{Name: "jane", Kind: iam.ActorHuman},
			Role: artifacts.RoleEditor, Origin: artifacts.OriginDashboard,
			Limits: artifacts.Limits{DataBytes: 1, DataStoreBytes: 1}},
		OpKey: statelog.NewOpID(time.Now(), ""),
	}

	if page, err := d.Docs(ctx, id, rd, "review", 9, "c", 5); err != nil || len(page.Docs) != 1 ||
		page.Docs[0].ID != "bob|contributor|session" || page.Through != 9 || page.After != "c+5" {
		t.Errorf("docs = (%+v, %v)", page, err)
	}
	if _, err := d.Docs(ctx, limited, rd, "review", 0, "", 0); !errors.Is(err, artifactdata.ErrGone) {
		t.Errorf("a purged store's refusal crossed as %v", err)
	}
	if doc, err := d.Doc(ctx, id, rd, "review", "r1"); err != nil || doc.Version != 7 {
		t.Errorf("doc = (%+v, %v)", doc, err)
	}
	if _, err := d.Doc(ctx, id, rd, "review", "missing"); !errors.Is(err, artifactdata.ErrNotFound) {
		t.Errorf("a missing document's refusal crossed as %v", err)
	}
	if page, err := d.Query(ctx, id, rd, "review", query.Query{After: "cur"}); err != nil ||
		page.Docs[0].ID != "cur" {
		t.Errorf("query = (%+v, %v)", page, err)
	}
	if _, err := d.Query(ctx, id, rd, "review", query.Query{Where: []query.Filter{{Path: "meta.zip"}}}); !errors.Is(err, query.ErrNotIndexed) {
		t.Errorf("an unindexed query's refusal crossed as %v", err)
	}
	if page, err := d.History(ctx, id, rd, artifactdata.HistoryFilter{Collection: "review", Doc: "r1",
		Field: "done"}, "h", 6); err != nil || page.After != "h+6" || page.Entries[0].OpID != "done" ||
		page.Entries[0].Changes[0].Doc != "r1" {
		t.Errorf("history = (%+v, %v)", page, err)
	}
	if schema, err := d.Schema(ctx, id); err != nil || len(schema) != 1 || schema[0].Columns[0].Key != "done" {
		t.Errorf("schema = (%+v, %v)", schema, err)
	}
	if info, err := d.Store(ctx, id); err != nil || info.Footprint != 99 || info.Artifact != id {
		t.Errorf("store = (%+v, %v)", info, err)
	}
	if usage, err := d.Usage(ctx); err != nil || usage.Footprint != 512 || !usage.Complete {
		t.Errorf("usage = (%+v, %v)", usage, err)
	}
	if state, err := d.State(ctx, id, rd); err != nil || len(state) != 1 || state[0].Key != "bob" {
		t.Errorf("state = (%+v, %v)", state, err)
	}

	if res, err := d.Write(ctx, jane, id, artifacts.DataWrite{Ops: []artifacts.DataOp{{}, {}}}); err != nil ||
		res.Verdict != artifacts.DataApplied || res.Seq != 42 {
		t.Errorf("write = (%+v, %v)", res, err)
	}
	_, err = d.Write(ctx, jane, limited, artifacts.DataWrite{})
	var budget *artifactdata.RateLimited
	if !errors.As(err, &budget) || budget.RetryAfter != 1500*time.Millisecond || budget.Scope != "principal" {
		t.Errorf("a write budget's refusal crossed as %v", err)
	}
	if _, err := d.Undo(ctx, jane, id, 0); !errors.Is(err, artifactdata.ErrNotUndoable) {
		t.Errorf("an undo's refusal crossed as %v", err)
	}
	if _, err := d.Configure(ctx, jane, id, nil); !errors.Is(err, artifactdata.ErrMigrationTooLarge) {
		t.Errorf("a schema change's refusal crossed as %v", err)
	}
	for name, write := range map[string]func() error{
		"undo":      func() error { _, err := d.Undo(ctx, jane, id, 5); return err },
		"configure": func() error { _, err := d.Configure(ctx, jane, id, []artifacts.Collection{{}}); return err },
		"purge":     func() error { return d.PurgeArtifactState(ctx, jane, "step-1", id) },
	} {
		if err := write(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, want := range []string{"write:2:jane:editor", "undo:5:jane:editor", "configure:1:jane:editor",
		"purge:step-1:jane:editor"} {
		found := false
		for _, got := range l.calls {
			found = found || got == want
		}
		if !found {
			t.Errorf("the serving node never ran %s, with the caller's stamped authority: ran %v", want, l.calls)
		}
	}
	for i, key := range l.keys {
		if key != jane.OpKey {
			t.Errorf("write %s crossed with key %q, want the caller's %q", l.calls[i], key, jane.OpKey)
		}
	}
}

// A KEYED DATASTORE WRITE IS REPEATABLE AND AN UNKEYED ONE IS NOT, and a purge
// step, which names its own operation, is always.
func TestADatastoreWriteIsRepeatableExactlyWhenKeyed(t *testing.T) {
	t.Parallel()
	keyed := artifactdata.Call{OpKey: statelog.NewOpID(time.Now(), "")}
	if !opDataWrite.repeatable(dataWriteArgs{Call: keyed}) || !opDataUndo.repeatable(dataUndoArgs{Call: keyed}) ||
		!opDataConfigure.repeatable(dataConfigureArgs{Call: keyed}) {
		t.Error("a keyed datastore write is not repeatable")
	}
	if opDataWrite.repeatable(dataWriteArgs{}) {
		t.Error("an unkeyed datastore write is repeatable, so an unanswered one would write twice")
	}
	if opDataPurge.spec.class != opIdempotentWrite {
		t.Error("a purge step, which names its own operation, is not repeated")
	}
}
