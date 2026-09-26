package runner

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

// recordingPublisher keeps every phase record it is handed.
type recordingPublisher struct {
	mu      sync.Mutex
	records []*types.AgentPhaseCompleted
}

func (p *recordingPublisher) Publish(_ context.Context, _ string, ev *events.Event) error {
	if rec, ok := events.DataAs[*types.AgentPhaseCompleted](ev); ok {
		p.mu.Lock()
		p.records = append(p.records, rec)
		p.mu.Unlock()
	}
	return nil
}

// A PANIC CLOSING A PHASE STILL LEAVES THE PHASE'S RECORD.
//
// runPhase publishes the record of a phase that fails or panics inside it, and
// the method holding the finished phase publishes the completed one. Between
// the two runs the code that reads what the phase produced, and a panic there
// would leave a phase that had run and billed with no record at all: its
// calls, its tokens and its rounds would reach nothing durable. The trigger
// here is a pass handed no submission tool to read, which is a nil dereference
// inside finishWork itself — the recovery under test is finishWork's own.
func TestAPanicClosingAPhasePublishesItsFailureRecord(t *testing.T) {
	t.Parallel()
	pub := &recordingPublisher{}
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	r, err := New(Config{
		Registry:  tools.NewRegistry(),
		Models:    &phase.Registry{},
		Seat:      prompts.Seat{Org: &org.Organization{Name: "Acme", Roles: []*org.Role{role}}, Role: role},
		Publisher: pub,
		Turn:      Turn{RunID: "t-closing", AgentID: "agent-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := r.cfg.Registry.Snapshot()
	surface, err := r.surfaceWith(t.Context(), phase.Execute, 1, nil, nil, snapshot, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ran := phaseResult{
		Rounds: 3, Elapsed: 2 * time.Second,
		Result: toolloop.Result{
			RoundsUsed: 3, InputTokens: 300, OutputTokens: 30,
			Executions: []toolloop.Execution{{Round: 2, Name: "slack_post", Output: "posted"}},
		},
	}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		r.finishWork(t.Context(), 1, work{
			res: ran, surface: surface, snapshot: snapshot, system: "the system", user: "the ask",
		})
	}()
	if recovered == nil {
		t.Fatal("finishWork swallowed the panic; the turn's guard has to see it to end the turn")
	}

	pub.mu.Lock()
	defer pub.mu.Unlock()
	if len(pub.records) != 1 {
		t.Fatalf("%d phase records were published, want the one failure record", len(pub.records))
	}
	rec := pub.records[0]
	if !rec.Failed || rec.ErrorKind != string(types.GuardUnhandledException) {
		t.Errorf("the record is failed=%v of kind %q; want a failed record of kind %s",
			rec.Failed, rec.ErrorKind, types.GuardUnhandledException)
	}
	if !strings.Contains(rec.Error, "nil pointer") {
		t.Errorf("the record's error %q does not name the panic", rec.Error)
	}
	// What the phase DID, whole: the reason the record has to exist.
	if rec.Phase != types.PhaseExecute || rec.RoundsUsed != 3 || rec.TotalTokens != 330 ||
		len(rec.ToolExecutions) != 1 || rec.SystemPrompt != "the system" {
		t.Errorf("the record carries phase %q, %d rounds, %d tokens, %d calls and system %q; want "+
			"execute, 3, 330, the one post and the phase's own prompt", rec.Phase, rec.RoundsUsed,
			rec.TotalTokens, len(rec.ToolExecutions), rec.SystemPrompt)
	}
	// Counted ONCE into the turn's own tally, which the turn-level event
	// reports beside its phases.
	if got := r.Spend().Total(); got != 330 {
		t.Errorf("the turn's tally is %d, want the phase's 330 once", got)
	}
}
