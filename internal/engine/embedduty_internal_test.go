package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// embeddingDutyCompany is a company with an embeddings provider, which is what
// arms the duty's tick; the provider itself is replaced by the test's own.
const embeddingDutyCompany = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
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
// runs, not a bound a test assembled.
//
// The bound is only as good as its wiring: an embedder handed no budget is
// refused, and every tick then embeds nothing while logging it; a tick derived
// with a deadline instead cuts off a slow node's work mid-stride; and a tick
// cut off for its silence must say so, or a wedged provider looks like a quiet
// company. So a tick whose provider takes most of the budget per batch, and
// more than the budget in all, makes every call; a tick whose provider answers
// publishes every source; and one whose provider never answers is cut off at
// the budget for that reason, and reports it.
func TestTheEmbeddingTickIsBoundedByItsProgress(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNodeOf(t, embeddingDutyCompany)
	// The node's own duty would race this test's ticks for the same sources.
	e.stopEmbedding()
	provider := &pacedEmbedder{Fake: embeddings.NewFake(64)}
	var held embeddings.Embedder = provider
	e.embeddings.Store(&held)

	const sources = 300 // three batches of at most 128
	seedTasks(t, e, sources)
	duty := e.newEmbedDuty(e.native.Load().log)
	if duty == nil {
		t.Fatal("a node with the vector log and a corpus built no duty")
	}
	duty.claim = nil
	const budget = 5 * time.Second
	duty.budget = budget

	// PROGRESS KEEPS IT RUNNING: each provider request takes two seconds of a
	// five-second budget, and the three take longer than the budget in all.
	// They answer with NO VECTORS — sources with nothing to embed — which is
	// progress like any answer and publishes nothing, so what each stretch
	// costs is the request alone and no machine's publish rate can move the
	// case across the budget. Not with an error: a failure that is not about
	// an input ends the tick's requests at the first.
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

// THE DUTY'S REFUSALS OUTLIVE ITS TICKS AND NOT ITS PROVIDER.
//
// The search package's duty is rebuilt every tick, so what the provider refused
// alone is held here, across them — or a refused input would be isolated again
// at the front of every tick. And a refusal is a fact about the provider as it
// was configured: an apply builds the provider again, and the one it builds
// starts with no memory, so a fixed gateway or a lowered `max_input_tokens` is
// tried at once rather than an hour later.
func TestTheDutysRefusalsOutliveItsTicksAndNotItsProvider(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNodeOf(t, embeddingDutyCompany)
	e.stopEmbedding()
	refusing := embeddings.NewFake(64)
	refusing.Refuse("seeded task 0 ")
	var held embeddings.Embedder = refusing
	e.embeddings.Store(&held)
	seedTasks(t, e, 3)
	duty := e.newEmbedDuty(e.native.Load().log)
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
	waitApplied(t, e.native.Load().log.Domain(search.Domain{}.Name()))
	if duty.tick(t.Context()); duty.refusals != first || first.Len() != 1 {
		t.Fatalf("the next tick with the same provider kept %p (%d), want %p still "+
			"holding the refusal", duty.refusals, duty.refusals.Len(), first)
	}

	// AN APPLY BUILDS THE PROVIDER AGAIN: a new slot, and no memory.
	var rebuilt embeddings.Embedder = embeddings.NewFake(64)
	e.embeddings.Store(&rebuilt)
	if report := duty.tick(t.Context()); report.published != 1 {
		t.Fatalf("the first tick on a rebuilt provider reported %+v, want the "+
			"source the old one refused embedded at once", report)
	}
	if duty.refusals == first || duty.refusals.Len() != 0 {
		t.Fatalf("a rebuilt provider inherited the refusals of the one it replaced")
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

// seedTasks files n tasks in a project of their own through the node's writer,
// and waits for the node to apply them.
func seedTasks(t *testing.T, e *Engine, n int) {
	t.Helper()
	writer := e.native.Load().writer
	at := time.Now().UTC()
	key := fmt.Sprintf("P%d", at.UnixNano()%100_000)
	mustApply(t, "the project", func() (tracker.WriteResult, error) {
		return writer.WriteDocument(t.Context(), statelog.NewOpID(at, "project"),
			tracker.ProjectSubject(key), "", tracker.Project{
				V: tracker.DocumentVersion, Key: key, Name: "Seeded " + key,
				CreatedAt: at, UpdatedAt: at,
			}, tracker.ChangeProjectCreated, nil)
	})
	for i := range n {
		id := fmt.Sprintf("%s-t%04d", key, i)
		mustApply(t, "task "+id, func() (tracker.WriteResult, error) {
			return writer.CreateTask(t.Context(), statelog.NewOpID(at, "create"), tracker.Task{
				V: tracker.DocumentVersion, ID: id, Project: key, Type: "task",
				Title: fmt.Sprintf("seeded task %d about topic %d", i, i%7), Status: tracker.StatusTodo,
				StatusGroup: tracker.GroupNotStarted, Priority: tracker.PriorityNormal,
				CreatedAt: at, UpdatedAt: at,
			}, nil)
		})
	}
	running := e.native.Load().log.Domain(tracker.Domain{}.Name())
	waitApplied(t, running)
}
