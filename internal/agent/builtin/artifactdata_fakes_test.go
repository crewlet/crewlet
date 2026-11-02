package builtin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/presence"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fakeData is the datastore as the tools see it: collections and their rows in
// memory, read under the reading the call made, every write recorded with the
// authority it was stamped with.
//
// IT APPLIES THE READ RULE THE STORE APPLIES ([artifactdata.Readable]), so a
// case about which rows a caller sees is about the tools' own filtering on top
// of it rather than about a fake that showed everything.
type fakeData struct {
	mu sync.Mutex

	schema []artifacts.Collection
	rows   map[string][]artifactdata.Doc

	writes     []artifacts.DataWrite
	configured [][]artifacts.Collection
	calls      []artifactdata.Call
	readings   []artifactdata.Reading

	// verdict is what every write is answered; conflict the rows a
	// conflict hands back.
	verdict  artifacts.DataVerdict
	reason   string
	conflict []artifactdata.Doc
	unknown  bool
}

func newFakeData() *fakeData {
	return &fakeData{rows: map[string][]artifactdata.Doc{}, verdict: artifacts.DataApplied}
}

// collection declares one collection read by rule.
func (f *fakeData) collection(name string, rule artifacts.ReadRule) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.schema = append(f.schema, artifacts.Collection{Name: name, Read: rule,
		Create: artifacts.RoleContributor, Delete: artifacts.RoleEditor})
}

// row holds one row of collection, written by writer.
func (f *fakeData) row(collection, id string, writer iam.Actor, fields string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[collection] = append(f.rows[collection], artifactdata.Doc{Collection: collection,
		ID: id, Fields: json.RawMessage(fields), Version: int64(len(f.rows[collection]) + 1),
		CreatedBy: writer, UpdatedBy: writer, CreatedAt: fakeAt, UpdatedAt: fakeAt})
}

func (f *fakeData) ruleOf(collection string) (artifacts.ReadRule, bool) {
	for _, c := range f.schema {
		if c.Name == collection {
			return c.Read, true
		}
	}
	return "", false
}

func (f *fakeData) Docs(_ context.Context, _ artifacts.ID, rd artifactdata.Reading, collection string,
	_ int64, _ string, limit int) (artifactdata.DocPage, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.readings = append(f.readings, rd)
	rule, ok := f.ruleOf(collection)
	if !ok {
		return artifactdata.DocPage{}, fmt.Errorf("%w: collection %q", artifactdata.ErrNotFound, collection)
	}
	page := artifactdata.DocPage{Docs: []artifactdata.Doc{}, Through: 99, Complete: true}
	for _, d := range f.rows[collection] {
		if artifactdata.Readable(rule, rd, d.CreatedBy.Name) && len(page.Docs) < limit {
			page.Docs = append(page.Docs, d)
		}
	}
	return page, nil
}

func (f *fakeData) Doc(ctx context.Context, id artifacts.ID, rd artifactdata.Reading, collection,
	doc string) (artifactdata.Doc, error) {

	page, err := f.Docs(ctx, id, rd, collection, 0, "", artifacts.MaxQueryLimit)
	if err != nil {
		return artifactdata.Doc{}, err
	}
	for _, d := range page.Docs {
		if d.ID == doc {
			return d, nil
		}
	}
	return artifactdata.Doc{}, fmt.Errorf("%w: %s/%s", artifactdata.ErrNotFound, collection, doc)
}

func (f *fakeData) Query(ctx context.Context, id artifacts.ID, rd artifactdata.Reading, collection string,
	q query.Query) (artifactdata.DocPage, error) {

	page, err := f.Docs(ctx, id, rd, collection, 0, "", artifacts.MaxQueryLimit)
	if err != nil {
		return page, err
	}
	var kept []artifactdata.Doc
	for _, d := range page.Docs {
		match := true
		for _, w := range q.Where {
			if !strings.Contains(string(d.Fields), fmt.Sprintf(`"%s":%s`, w.Path, w.Value)) {
				match = false
			}
		}
		if match && (q.Limit == 0 || len(kept) < q.Limit) {
			kept = append(kept, d)
		}
	}
	page.Docs = kept
	return page, nil
}

func (f *fakeData) Schema(context.Context, artifacts.ID) ([]artifacts.Collection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.schema), nil
}

func (f *fakeData) result() artifactdata.WriteResult {
	if f.unknown {
		return artifactdata.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeUnknown,
			OpID: "op-unknown"}}
	}
	return artifactdata.WriteResult{Verdict: f.verdict, Reason: f.reason, Seq: 7,
		Conflict: f.conflict, Result: statelog.Result{Outcome: statelog.OutcomeApplied}}
}

func (f *fakeData) Write(_ context.Context, c artifactdata.Call, _ artifacts.ID,
	in artifacts.DataWrite) (artifactdata.WriteResult, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	f.writes = append(f.writes, in)
	return f.result(), nil
}

func (f *fakeData) Configure(_ context.Context, c artifactdata.Call, _ artifacts.ID,
	collections []artifacts.Collection) (artifactdata.WriteResult, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	f.configured = append(f.configured, collections)
	return f.result(), nil
}

func (f *fakeData) lastCall() artifactdata.Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return artifactdata.Call{}
	}
	return f.calls[len(f.calls)-1]
}

// touches records every presence touch the tools made.
type touches struct {
	mu    sync.Mutex
	got   []string
	turns []string
}

func (p *touches) Touch(room presence.Room, a presence.Agent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if room.Kind != presence.RoomArtifact {
		p.got = append(p.got, "a "+string(room.Kind)+" room")
		return
	}
	p.got = append(p.got, fmt.Sprintf("%s %s %s %s/%s", artifacts.ID(room.ID).Label(), a.Seat,
		a.Activity, a.Focus.Collection, a.Focus.Doc))
	p.turns = append(p.turns, a.Turn)
}

func (p *touches) all() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.got)
}

// artifactDataDeps is the datastore half over f, with a fresh turn ledger and
// no presence hub.
func artifactDataDeps(f *fakeData) builtin.ArtifactDataDeps {
	return builtin.ArtifactDataDeps{Store: f, Turns: builtin.NewDataTurns(nil)}
}
