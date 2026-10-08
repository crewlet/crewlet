package sandbox_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
)

// TestMain silences the engine logger. Every Open logs a line per applied
// schema file and the suite opens a database per subtest — several hundred
// lines of successful boot ahead of the one that says what failed.
func TestMain(m *testing.M) {
	logging.Configure(slog.LevelError, logging.FormatText, io.Discard)
	os.Exit(m.Run())
}

func TestPendingStoreContract(t *testing.T) {
	t.Parallel()
	// ONE implementation now — the run record lives in the fleet's
	// coordination store, because a detached run is recovered by whichever
	// node owns its seat NEXT. The record's own semantics are certified
	// against both coordination backends by internal/coord/coordtest; what
	// this suite covers is everything built on top of them, which is all of
	// the conditional flips: the at-most-once tail claim, the epoch fence,
	// the pause expiry.
	sandboxtest.Run(t, func(*testing.T) (sandbox.PendingStore, coord.SandboxRuns) {
		runs := memory.NewFleet()
		return sandbox.NewCoordStore(runs), runs
	})
}

// NOTHING EMPTY EVER ANSWERS ANYTHING, and that is a rule of the value rather
// than of the store that reads it.
//
// A run launched by a schedule tick or an A2A wake carries no conversation,
// and a wake that could not name one carries none either — so two absences
// comparing equal would make every such delivery the answer to every such run,
// and the first thing a person said to a seat would splice into somebody
// else's coding job. [sandbox.CoordStore.FindAwaitingByConversation] refuses an
// empty reference before it reads the bucket at all, which is why this case
// cannot be reached through the store and is asserted here instead: the rule
// has to hold for the next caller too.
func TestNoConversationAnswersNoParkedRun(t *testing.T) {
	t.Parallel()
	keyless := sandbox.PendingRun{TurnID: "t1", AgentHandle: "swe"}
	dm := sandbox.PendingRun{
		TurnID: "t2", AgentHandle: "swe",
		PartitionKey: "chat:D1:root-1", ConversationKey: "chat:D1",
	}
	cases := []struct {
		name string
		conv sandbox.ConversationRef
		run  sandbox.PendingRun
	}{
		{"a delivery naming no conversation, a run parked with none",
			sandbox.ConversationRef{}, keyless},
		{"a delivery naming no conversation, a run parked on a DM",
			sandbox.ConversationRef{}, dm},
		{"a reply on a DM line, a run parked with no conversation",
			sandbox.ConversationRef{Identity: "chat:D1", Partition: "chat:D1:root-1"},
			keyless},
	}
	for _, tc := range cases {
		if tc.conv.Answers(tc.run) {
			t.Errorf("%s: matched", tc.name)
		}
	}
}

// A REPLY IS ADMITTED ON ITS CONVERSATION ALONE. A direct message is one line
// however it is threaded, so a reply in the thread the question was asked in
// and a top-level reply on the same line both answer the run parked there —
// and a reply on another conversation answers it in neither shape, whatever
// its partition.
func TestAReplyIsAdmittedOnItsConversationAlone(t *testing.T) {
	t.Parallel()
	parked := sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe",
		PartitionKey: "chat:D1:root-1", ConversationKey: "chat:D1",
	}
	for name, reply := range map[string]sandbox.ConversationRef{
		"in the thread it asked in":  {Identity: "chat:D1", Partition: "chat:D1:root-1"},
		"top-level on the same line": {Identity: "chat:D1", Partition: "chat:D1"},
	} {
		if !reply.Answers(parked) {
			t.Errorf("a reply %s did not answer the run", name)
		}
	}
	for name, reply := range map[string]sandbox.ConversationRef{
		"on another line":                    {Identity: "chat:D2", Partition: "chat:D2:root-1"},
		"on another line, in the same batch": {Identity: "chat:D2", Partition: "chat:D1:root-1"},
	} {
		if reply.Answers(parked) {
			t.Errorf("a reply %s answered the run", name)
		}
	}
}

// A ROW KEEPS WHAT THIS BUILD CANNOT READ.
//
// Every flip is a compare-and-swap of the WHOLE value, and in a fleet mid
// upgrade the older build makes some of them. Decoded into the struct alone,
// that build wrote back only the fields it knew, and a newer build's key —
// the item the run is charged to, the audience its question waits on — was
// deleted by the first claim it made. The key here is one no build declares,
// so what is being tested is the codec rather than any field.
func TestAPendingRunKeepsKeysThisBuildDoesNotKnow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	runs := memory.NewFleet()
	store := sandbox.NewCoordStore(runs)
	const future = `{"engine":"v9","weights":[1,2,12345678901234567890]}`
	seed := `{"turn_id":"t1","agent_handle":"swe","status":"launching",` +
		`"launch_id":"l1","a_later_builds_field":` + future + `}`
	if created, err := runs.CreateSandboxRun(ctx, "t1", []byte(seed)); err != nil || !created {
		t.Fatalf("seed: created=%v err=%v", created, err)
	}

	got, found, err := store.Get(ctx, "t1")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if string(got.Extra["a_later_builds_field"]) != future {
		t.Fatalf("decode did not keep the unknown key: %s", got.Extra)
	}
	if _, known := got.Extra["status"]; known {
		t.Fatalf("a key this build declares was carried as unknown too: %v", got.Extra)
	}

	// A FLIP, by this build, of a row carrying a key it cannot read.
	if err := store.SetStatus(ctx, "t1", sandbox.StatusRunning, sandbox.Fence{}); err != nil {
		t.Fatalf("flip: %v", err)
	}
	record, _, err := runs.SandboxRun(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	var written map[string]json.RawMessage
	if err := json.Unmarshal(record.Value, &written); err != nil {
		t.Fatal(err)
	}
	if string(written["a_later_builds_field"]) != future {
		t.Fatalf("the flip dropped a key this build does not know: %s", record.Value)
	}
	if string(written["status"]) != `"running"` {
		t.Fatalf("the flip itself did not land: %s", record.Value)
	}
}

// THE STRUCT WINS OVER EXTRA. Extra is filled only with what the struct could
// not place, so a known key in it is a caller's hand edit — and the value this
// build DECIDED is the one that has to land, or a status flip could be undone
// by a stale copy riding beside it.
func TestADecidedFieldWinsOverACarriedOne(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	runs := memory.NewFleet()
	store := sandbox.NewCoordStore(runs)
	if _, err := store.BeginLaunch(ctx, sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe",
		Extra: map[string]json.RawMessage{"status": json.RawMessage(`"resumed"`)},
	}, sandbox.Fence{}); err != nil {
		t.Fatal(err)
	}
	got, _, err := store.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != sandbox.StatusLaunching {
		t.Fatalf("status = %q: a carried key overrode the decided one", got.Status)
	}

	// A DECIDED ABSENCE WINS TOO. The work item is omitted when it is nil,
	// so a check of what the marshal emitted would find no "work_item" and
	// let the carried one write back the item this build cleared.
	if _, err := store.BeginLaunch(ctx, sandbox.PendingRun{
		TurnID: "t2", AgentHandle: "swe",
		Extra: map[string]json.RawMessage{
			"work_item": json.RawMessage(`{"backend":"native","id":"t-9","key":"ENG-9","project":"ENG"}`),
		},
	}, sandbox.Fence{}); err != nil {
		t.Fatal(err)
	}
	cleared, _, err := store.Get(ctx, "t2")
	if err != nil {
		t.Fatal(err)
	}
	if cleared.WorkItem != nil {
		t.Fatalf("work item = %+v: a carried key brought back a field this "+
			"build left empty", *cleared.WorkItem)
	}
}

// vanishingRuns is a run store whose record disappears between a launch's
// create and its reset: the first create of a turn finds the row there, and the
// read the reset makes next finds it gone — the turn's previous run finishing
// as the next one opens.
type vanishingRuns struct {
	runStore
	creates int
}

// runStore names the embedded store, whose own name is one of its methods.
type runStore = coord.SandboxRuns

func (v *vanishingRuns) CreateSandboxRun(ctx context.Context, turnID string, value []byte) (bool, error) {
	v.creates++
	if v.creates == 1 {
		return false, nil
	}
	return v.runStore.CreateSandboxRun(ctx, turnID, value)
}

// A ROW THAT VANISHES UNDER A LAUNCH IS CREATED AGAIN, not reported open. The
// reset that found nothing used to report success, so the launch went on to
// start a job — in a box — on a row that did not exist, which nothing would ever
// collect or reclaim.
//
// Mutation: return from the reset whatever it found, and the launch answers a
// job no row holds.
func TestALaunchWhoseRowVanishesCreatesItAgain(t *testing.T) {
	t.Parallel()
	runs := &vanishingRuns{runStore: memory.NewFleet()}
	store := sandbox.NewCoordStore(runs)
	opened, err := store.BeginLaunch(t.Context(), sandbox.PendingRun{
		TurnID: "t-vanished", AgentHandle: "swe",
	}, sandbox.Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	got, found, err := store.Get(t.Context(), "t-vanished")
	if err != nil || !found {
		t.Fatalf("the launch reported job %q open on no row (found %v, %v)", opened.LaunchID, found, err)
	}
	if got.LaunchID != opened.LaunchID || got.Status != sandbox.StatusLaunching {
		t.Fatalf("the row holds job %q in %q; the launch answered %q", got.LaunchID, got.Status, opened.LaunchID)
	}
}
