package engine

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tools"
)

// The auxiliary calls a TURN makes — the prefetch before it starts, and the
// memory filter refresh_memory re-runs inside it — are recorded as the seat's
// spend under the worker that made them AND the turn they served. The per-turn
// row of every spend rollup keys on that turn, so a call recorded without it is
// spend the turn made and no turn's row shows. These drive the production
// seams — prefetchFor, and the memory tool as equip registers it — and read
// the records off the broker.

// memoryCompany is one seat with a note in its diary and an auxiliary model
// that answers, on an engine whose records land on q.
func memoryCompany(t *testing.T) (*Engine, *Company, *org.Role, *memory.Queue) {
	t.Helper()
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	db, err := store.Open(t.Context(), t.TempDir()+"/index.db", store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	registry, err := phase.NewRegistry([]phase.Entry{{Key: "aux", Provider: &answeringProvider{in: 20, out: 2}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	lead := &org.Role{Name: "Tech Lead", DeclaredHandle: "lead"}
	company := &Company{
		Config: &config.Company{},
		Org:    &org.Organization{Name: "Nimbus", Roles: []*org.Role{lead}},
		Models: registry,
		Tools:  tools.NewRegistry(),
	}
	e := &Engine{backends: &Backends{Queue: q, Store: db}}
	e.epoch.current.Store(company)

	agentID, ok := company.Org.AgentIDFor(lead)
	if !ok {
		t.Fatal("the seat has no agent id")
	}
	if err := e.diary(db, company).Write(t.Context(), learning.DiaryEntry{
		ID: "note-1", AgentID: agentID.String(), Kind: learning.DiaryLong,
		Content: "the staging login loop is a cookie domain mismatch",
	}); err != nil {
		t.Fatalf("write the note: %v", err)
	}
	return e, company, lead, q
}

// auxiliaryRecords collects every auxiliary_call_completed published to q.
func auxiliaryRecords(t *testing.T, q *memory.Queue) <-chan types.AuxiliaryCallCompleted {
	t.Helper()
	got := make(chan types.AuxiliaryCallCompleted, 16)
	if err := q.Subscribe(t.Context(), topics.Event(types.AuxiliaryCallCompleted{}.EventType()), "probe",
		func(_ context.Context, ev *events.Event) queue.Result {
			if p, ok := events.DataAs[*types.AuxiliaryCallCompleted](ev); ok && p != nil {
				got <- *p
			}
			return queue.Ack()
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	return got
}

// firstRecord waits for one record, bounded so a call that was never recorded
// fails in seconds rather than at the package timeout.
func firstRecord(t *testing.T, got <-chan types.AuxiliaryCallCompleted) types.AuxiliaryCallCompleted {
	t.Helper()
	select {
	case rec := <-got:
		return rec
	case <-time.After(5 * time.Second):
		t.Fatal("no auxiliary_call_completed was published")
		return types.AuxiliaryCallCompleted{}
	}
}

func TestThePrefetchsAuxiliaryCallsNameTheTurnTheyServed(t *testing.T) {
	t.Parallel()
	e, company, _, q := memoryCompany(t)
	got := auxiliaryRecords(t, q)

	e.prefetchFor(t.Context(), company, Request{
		Handle: "lead", RunID: "run-7", WorkKey: "work-7",
	}, "why does the staging login loop")

	rec := firstRecord(t, got)
	if rec.Worker != learning.PrefetchWorker || rec.TurnID != "run-7" || rec.WorkKey != "work-7" ||
		rec.RoleName != "Tech Lead" || rec.TotalTokens != 22 {
		t.Errorf("record = %+v, want the prefetch's call on run-7 of work-7", rec)
	}
}

func TestARecallToolsAuxiliaryCallNamesTheTurnThatCalledIt(t *testing.T) {
	t.Parallel()
	e, company, lead, q := memoryCompany(t)
	got := auxiliaryRecords(t, q)
	if err := e.equipEpoch(company); err != nil {
		t.Fatalf("equipEpoch: %v", err)
	}
	entry, ok := company.Tools.Lookup(builtin.RefreshMemoryTool)
	if !ok {
		t.Fatal("the epoch has no refresh_memory tool")
	}
	tool, ok := entry.Tool.(tools.SeatCallable)
	if !ok {
		t.Fatalf("refresh_memory is a %T, not a seat tool", entry.Tool)
	}
	res, err := tool.CallForTurn(t.Context(), &turnctx.Turn{
		RunID: "run-9", WorkKey: "work-9", Seat: lead, Org: company.Org,
	}, map[string]any{"context_hint": "the staging login loop"})
	if err != nil {
		t.Fatalf("refresh_memory: %v", err)
	}
	if res.Failed {
		t.Fatalf("refresh_memory failed: %s", res.Output)
	}

	rec := firstRecord(t, got)
	if rec.Worker != learning.RecallWorker || rec.TurnID != "run-9" || rec.WorkKey != "work-9" ||
		rec.RoleName != "Tech Lead" {
		t.Errorf("record = %+v, want the recall's call on run-9 of work-9", rec)
	}
}
