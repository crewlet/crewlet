package tracker_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A PASS'S LINE SAYS WHICH EDGES IT MIRRORED AND WHICH IT GAVE UP ON.
//
// `tracker_one_sided_repaired` counted every edge a pass settled, and the work
// tracker guide said it counted the ones it MIRRORED — so a pass whose
// blockers were all gone logged a number an operator reads as that many
// repairs, when every one of them was a dependency somebody now has to resolve
// by hand. One pass here settles one edge of each kind, which is the only shape
// that tells a split count from a total.
func TestTheOneSidedPassSaysWhatItMirroredAndWhatItStamped(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"dep-a", "blk-a", "dep-b", "blk-b"} {
		filedTask(t, r, id)
	}
	// THE AUTHORED HALF ALONE, twice: the residue a gesture leaves when
	// its mirror step did not run.
	for _, edge := range []struct{ dependent, blocker string }{
		{"dep-a", "blk-a"}, {"dep-b", "blk-b"},
	} {
		if _, err := r.writer.UpdateTask(t.Context(), "op-half-"+edge.dependent,
			edge.dependent, "ENG", tracker.NoIfMatch, tracker.TaskPatch{
				Relate: &tracker.RelationIntent{Add: []tracker.Relation{
					{Kind: tracker.RelationWaitingOn, Other: edge.blocker},
				}},
			}, tracker.ChangeRelations, nil); err != nil {
			t.Fatalf("write the authored half of %s: %v", edge.dependent, err)
		}
	}
	// AND ONE BLOCKER GONE, so that edge can never be mirrored.
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "blk-b", "ENG",
		false, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()

	logs := &onesidedLog{}
	worker, err := maintenance.New(maintenance.Options{
		Jobs: tracker.Jobs(tracker.DutyDeps{
			DB: r.db, Writer: r.writer, NodeID: "node-a",
			Logger: slog.New(slog.NewJSONHandler(logs, nil)),
		}),
		// Past the repair's age, so neither edge reads as a gesture
		// still in flight.
		Now: func() time.Time { return wednesday.AddDate(1, 0, 0) },
	})
	if err != nil {
		t.Fatalf("build the tracker's maintenance worker: %v", err)
	}
	swept, err := worker.Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if swept["tracker_one_sided"] != 2 {
		t.Fatalf("the pass settled %d edges, want both: %v", swept["tracker_one_sided"], swept)
	}

	passes := logs.records(t, "tracker_one_sided_repaired")
	if len(passes) != 1 {
		t.Fatalf("%d tracker_one_sided_repaired lines for one pass, want one", len(passes))
	}
	for key, want := range map[string]any{
		"edges":    float64(2),
		"mirrored": float64(1),
		"final":    float64(1),
	} {
		if passes[0][key] != want {
			t.Errorf("tracker_one_sided_repaired %s = %v, want %v — a stamped "+
				"edge is a dependency a person has to resolve, and counted "+
				"as a mirror it reads as the repair working", key, passes[0][key], want)
		}
	}
	finals := logs.records(t, "tracker_one_sided_final")
	if len(finals) != 1 || finals[0]["dependent"] != "dep-b" {
		t.Errorf("tracker_one_sided_final lines %v, want the one for dep-b", finals)
	}
}

// onesidedLog is a JSON log destination a test reads back.
type onesidedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *onesidedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// records decodes every line with this message.
func (l *onesidedLog) records(t *testing.T, msg string) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(l.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("a log line is not JSON: %q: %v", line, err)
		}
		if record["msg"] == msg {
			out = append(out, record)
		}
	}
	return out
}
