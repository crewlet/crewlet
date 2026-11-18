package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// embeddingDutyCompany is a company with an embeddings provider, which is what
// arms the duty's tick; the provider itself is replaced by the test's own.
const embeddingDutyCompany = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
  embeddings:
    type: openai
    model: text-embedding-3-small
    api_key: sk-embed
    dimensions: 64
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`

// THE DUTY'S TICK IS BOUNDED BY ITS PROGRESS — through the tick the engine
// runs, not a bound a test assembled. Handed no budget, an embedder is refused
// and every tick embeds nothing; a deadline instead cuts a slow node off
// mid-stride; and a tick cut off unreported makes a wedged provider look like
// a quiet company. So a provider taking most of the budget a batch, and more
// in all, gets every call; one that answers has every source published; and
// one that never answers is cut off at the budget for that reason, and the
// tick reports it.
func TestTheEmbeddingTickIsBoundedByItsProgress(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNodeOf(t, embeddingDutyCompany)
	// The node's own duty would race this test's ticks for the same sources.
	e.stopEmbedding()
	provider := &pacedEmbedder{Fake: embeddings.NewFake(64)}
	// ONE INPUT A REQUEST, so three sources are three requests: enough to
	// run longer than the budget in all without filing hundreds of tasks.
	// The packing itself is the search package's to certify.
	limits := provider.Limits()
	limits.BatchInputs = 1
	provider.SetLimits(limits)
	var held embeddings.Embedder = provider
	e.embeddings.Store(&held)

	const sources = 3 // three requests of one
	seedTasks(t, e, sources)
	duty := e.newEmbedDuty(e.core.Load().log)
	if duty == nil {
		t.Fatal("a node with the vector log and a corpus built no duty")
	}
	duty.claim = nil
	const budget = 5 * time.Second
	duty.budget = budget

	// PROGRESS KEEPS IT RUNNING: each request takes two seconds of a
	// five-second budget, three of them more than the budget. They answer
	// with NO VECTORS, progress that publishes nothing, so no machine's
	// publish rate moves the case across the budget; not an error, which
	// would end the tick's requests at the first.
	var mu sync.Mutex
	answered := 0
	provider.set(func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(2 * time.Second):
			mu.Lock()
			answered++
			mu.Unlock()
			return nil
		}
	}, true)
	started := time.Now()
	report := duty.tick(t.Context())
	took := time.Since(started)
	mu.Lock()
	calls := answered
	mu.Unlock()
	if report.unwired || report.overran || calls != 3 {
		t.Fatalf("a tick advancing every request for %v reported %+v after %d whole "+
			"provider request(s), want all three made and nothing cut off", took, report, calls)
	}
	if took < budget {
		t.Fatalf("the tick took %v, so it never ran past its budget and this case "+
			"proves nothing about progress", took)
	}

	// AND IT IS WIRED: a provider that answers has every source published.
	provider.set(nil, false)
	if report = duty.tick(t.Context()); report.unwired || report.overran || report.published < sources {
		t.Fatalf("a tick whose provider answered reported %+v, want all %d sources "+
			"published", report, sources)
	}

	// SILENCE CUTS IT OFF, for that reason: a source the tick has not embedded
	// yet, and a provider that never answers.
	seedTasks(t, e, 1)
	var cause error
	provider.set(func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			cause = context.Cause(ctx)
			return cause
		case <-time.After(10 * budget):
			return errors.New("the provider was never cut off")
		}
	}, false)
	report = duty.tick(t.Context())
	if !report.overran || !errors.Is(cause, errTickOverran) {
		t.Fatalf("a tick whose provider never answered reported %+v, the provider "+
			"cut off by %v — want the tick overran, cut off by %v",
			report, cause, errTickOverran)
	}
}

// THE DUTY'S REFUSALS OUTLIVE ITS TICKS, AND ITS PROVIDER — NOT ITS
// CONFIGURATION. Held across ticks, or every refused input is isolated again
// (fifteen requests apiece) at the front of each; kept by a provider an apply
// rebuilt the same (a rotated key, say); dropped for one configured otherwise
// (a lowered `max_input_tokens`, another gateway), so the fix is tried at once.
func TestTheDutysRefusalsFollowTheProvidersConfiguration(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNodeOf(t, embeddingDutyCompany)
	e.stopEmbedding()
	refusing := embeddings.NewFake(64)
	refusing.Refuse("seeded task 0 ")
	var held embeddings.Embedder = refusing
	e.embeddings.Store(&held)
	seedTasks(t, e, 3)
	duty := e.newEmbedDuty(e.core.Load().log)
	if duty == nil {
		t.Fatal("a node with the vector log and a corpus built no duty")
	}
	duty.claim = nil

	if report := duty.tick(t.Context()); report.unwired || report.published != 2 {
		t.Fatalf("a tick with one refused source of three reported %+v", report)
	}
	first := duty.refusals
	if first == nil || first.Len() != 1 {
		t.Fatalf("the duty holds %v after one input was refused alone", first)
	}
	waitApplied(t, e.core.Load().log.Domain(search.Domain{}.Name()))
	if duty.tick(t.Context()); duty.refusals != first || first.Len() != 1 {
		t.Fatalf("the next tick with the same provider kept %p (%d), want %p still "+
			"holding the refusal", duty.refusals, duty.refusals.Len(), first)
	}

	// REBUILT THE SAME: a new slot, the same memory, nothing sent.
	same := embeddings.NewFake(64)
	var rebuilt embeddings.Embedder = same
	e.embeddings.Store(&rebuilt)
	if report := duty.tick(t.Context()); report.published != 0 || len(same.Requests()) != 0 {
		t.Fatalf("the first tick on a provider built again the same reported %+v "+
			"after %d request(s), want the refused source still held", report,
			len(same.Requests()))
	}
	if duty.refusals != first || first.Len() != 1 {
		t.Fatalf("a provider built again with the same configuration lost the " +
			"refusals of the one it replaced, so every refused input is isolated " +
			"again on every apply")
	}

	// CONFIGURED OTHERWISE (a narrower request): the refused source is
	// tried at once.
	narrower := embeddings.NewFake(64)
	narrower.SetLimits(embeddings.Limits{InputBytes: 8192, BatchInputs: 64, BatchBytes: 300_000})
	var reconfigured embeddings.Embedder = narrower
	e.embeddings.Store(&reconfigured)
	if report := duty.tick(t.Context()); report.published != 1 {
		t.Fatalf("the first tick on a provider configured otherwise reported %+v, "+
			"want the source the old configuration refused embedded at once", report)
	}
	if duty.refusals == first || duty.refusals.Len() != 0 {
		t.Fatalf("a provider configured otherwise inherited the refusals of the " +
			"one it replaced")
	}
}

// AN EMBEDDING TICK STOPS WHEN A RENEWAL SAYS ANOTHER NODE HOLDS THE DUTY —
// mid-request, for that reason, publishing nothing. The kept duty's own cases
// certify the renewal; this one holds the tick's WIRING: the provider is asked
// on the kept duty's context, not the turn's, one derived without its
// cancellation, or one bounded before the duty was kept.
func TestAnEmbeddingTickStopsWhenARenewalSaysAnotherNodeHoldsTheDuty(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNodeOf(t, embeddingDutyCompany)
	e.stopEmbedding()
	provider := &pacedEmbedder{Fake: embeddings.NewFake(64)}
	var held embeddings.Embedder = provider
	e.embeddings.Store(&held)
	seedTasks(t, e, 1)
	duty := e.newEmbedDuty(e.core.Load().log)
	if duty == nil {
		t.Fatal("a node with the vector log and a corpus built no duty")
	}
	clock := newFakeDutyClock()
	duty.renewal.clock = clock
	claim := newScriptedClaim(answer{held: true}, answer{held: false})
	duty.claim = claim.claim
	called, cut := make(chan struct{}, 1), make(chan error, 1)
	provider.set(func(ctx context.Context) error {
		select {
		case called <- struct{}{}:
		default:
		}
		<-ctx.Done()
		select {
		case cut <- context.Cause(ctx):
		default:
		}
		return context.Cause(ctx)
	}, false)

	done := make(chan tickReport, 1)
	go func() { done <- duty.tick(t.Context()) }()
	if a := claim.await(t); !a.held {
		t.Fatalf("the turn's claim answered %+v, want held", a)
	}
	// THE PROVIDER IS MID-REQUEST before the renewal: one landing earlier
	// proves only the tick's reads were cut off, not the provider call.
	select {
	case <-called:
	case r := <-done:
		t.Fatalf("the tick returned %+v before it asked the provider", r)
	case <-time.After(10 * time.Second):
		t.Fatal("the provider was never asked")
	}
	clock.tick(t, duty.renewal.every())
	if a := claim.await(t); a.held {
		t.Fatalf("the renewal answered %+v, want another node holding the duty", a)
	}
	select {
	case why := <-cut:
		if !errors.Is(why, errDutyLost) {
			t.Fatalf("the provider's request was cut off by %v, want %v", why, errDutyLost)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a renewal answered that another node holds the duty, and the " +
			"provider's request ran on")
	}
	select {
	case r := <-done:
		if r.published != 0 {
			t.Fatalf("a tick whose duty went to another node published %d source(s)", r.published)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the tick never returned")
	}
}

// pacedEmbedder is the fake embedder with every request first waiting on pace,
// and answering with no vectors at all while it is blank.
type pacedEmbedder struct {
	*embeddings.Fake

	mu    sync.Mutex
	pace  func(context.Context) error
	blank bool
}

// set paces every request, and answers it with no vectors while blank.
func (p *pacedEmbedder) set(pace func(context.Context) error, blank bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pace, p.blank = pace, blank
}

func (p *pacedEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	p.mu.Lock()
	pace, blank := p.pace, p.blank
	p.mu.Unlock()
	if pace != nil {
		if err := pace(ctx); err != nil {
			return nil, err
		}
	}
	if blank {
		return make([][]float32, len(texts)), nil
	}
	return p.Fake.EmbedBatch(ctx, texts)
}

// seededProjects numbers the projects seedTasks files, so every call names a
// new one: a key read off a microsecond clock (macOS) repeats, and the second
// create is refused.
var seededProjects atomic.Int64

// seedTasks files n tasks in a project of their own through the node's writer,
// and waits for the node to apply them.
func seedTasks(t *testing.T, e *Engine, n int) {
	t.Helper()
	writer := e.native.Load().writer
	at := time.Now().UTC()
	key := fmt.Sprintf("P%05d", seededProjects.Add(1))
	mustApply(t, "the project", func() (worklet.WriteResult, error) {
		return writer.WriteDocument(t.Context(), statelog.NewOpID(at, "project"),
			worklet.ProjectSubject(key), "", worklet.Project{
				V: worklet.DocumentVersion, Key: key, Name: "Seeded " + key,
				CreatedAt: at, UpdatedAt: at,
			}, worklet.ChangeProjectCreated, nil)
	})
	for i := range n {
		id := fmt.Sprintf("%s-t%04d", key, i)
		mustApply(t, "task "+id, func() (worklet.WriteResult, error) {
			return writer.CreateTask(t.Context(), statelog.NewOpID(at, "create"), worklet.Task{
				V: worklet.DocumentVersion, ID: id, Project: key, Type: "task",
				Title: fmt.Sprintf("seeded task %d about topic %d", i, i%7), Status: worklet.StatusTodo,
				StatusGroup: worklet.GroupNotStarted, Priority: worklet.PriorityNormal,
				CreatedAt: at, UpdatedAt: at,
			}, nil)
		})
	}
	running := e.core.Load().log.Domain(worklet.Domain{}.Name())
	waitApplied(t, running)
}
