package artifactupload_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/artifactupload/edits"
	"github.com/crewlet/crewlet/internal/objectives"
)

// judged is the objective meter as the pipeline sees it, keeping every event.
type judged struct {
	mu     sync.Mutex
	events []judgedEvent
}

type judgedEvent struct {
	name     objectives.Name
	answered bool
}

func (j *judged) Record(n objectives.Name, _ time.Duration, answered bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.events = append(j.events, judgedEvent{n, answered})
}

// take answers the events since the last take, and forgets them.
func (j *judged) take() []judgedEvent {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := j.events
	j.events = nil
	return out
}

// EVERY CREATE AND PUBLISH IS ONE PUBLISH SUCCESS EVENT, a client's own refusal
// answered; only a publish that moves no new bytes is a latency event, and a
// refusal is none. A publish the object store had no room for is refused too,
// but for the deployment's reason rather than the client's: its answer is a
// 5xx (507), and a bucket left full is exactly what the objective's budget is
// there to burn, so it is judged unanswered. Mutation: count a refusal as
// unanswered, count a full store as answered, or time a publish that uploaded
// bytes, and a case fails.
func TestEveryPublishIsOneObjectiveEvent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{fillable: true})
	created := h.create(h.person("ada", h.opKey("create")), map[string]string{"index.html": page})
	success := judgedEvent{objectives.PublishSuccess, true}
	applied := judgedEvent{objectives.PublishApplied, true}
	expect := func(what string, want ...judgedEvent) {
		t.Helper()
		got := h.judged.take()
		if len(got) != len(want) {
			t.Fatalf("%s: events %+v, want %+v", what, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: event %d is %+v, want %+v", what, i, got[i], want[i])
			}
		}
	}
	expect("a create with uploaded bytes", success)

	if _, _, err := h.svc.Publish(t.Context(), h.person("ada", h.opKey("edit")), artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 1, Mode: manifest.ModePatch,
		Edits: []edits.Edit{{Old: `<script>function go()`, New: `<script>function go2()`}},
	}); err != nil {
		t.Fatal(err)
	}
	expect("an edit", applied, success)

	if _, _, err := h.svc.Publish(t.Context(), h.person("ada", h.opKey("inline")), artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 2, Mode: manifest.ModePatch,
		Files: []artifactupload.FileSpec{{Path: "a.txt", Inline: []byte("a")}},
	}); err != nil {
		t.Fatal(err)
	}
	expect("a publish with new bytes", success)

	if _, _, err := h.svc.Publish(t.Context(), h.person("ada", h.opKey("rollback")), artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 3, RestoreFrom: 1,
	}); err != nil {
		t.Fatal(err)
	}
	expect("a rollback", applied, success)

	_, _, err := h.svc.Publish(t.Context(), h.person("ada", h.opKey("moved")), artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 1, Mode: manifest.ModePatch,
		Edits: []edits.Edit{{Old: "Quarterly", New: "Annual"}},
	})
	var moved *artifactsdomain.VersionMoved
	if !errors.As(err, &moved) {
		t.Fatalf("the publish on a moved head answered %v", err)
	}
	expect("a publish on a moved head", success)

	h.fill(true)
	_, _, err = h.svc.Publish(t.Context(), h.person("ada", h.opKey("full")), artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 4, Mode: manifest.ModePatch,
		Files: []artifactupload.FileSpec{{Path: "b.txt", Inline: []byte("b")}},
	})
	var full *artifactupload.StoreFull
	if !errors.As(err, &full) {
		t.Fatalf("the publish the store had no room for answered %v", err)
	}
	expect("a publish the store had no room for", judgedEvent{objectives.PublishSuccess, false})
}
