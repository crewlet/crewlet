package sandbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
)

// TestMain silences the engine logger: every Open logs a line per applied
// schema file, hundreds of lines ahead of the one that says what failed. It
// also runs a box's teardown loop when re-executed as one
// ([runWriteBackWriter]).
func TestMain(m *testing.M) {
	logging.Configure(slog.LevelError, logging.FormatText, io.Discard)
	if dir := os.Getenv(writeBackWriterEnv); dir != "" {
		runWriteBackWriter(dir)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestPendingStoreContract(t *testing.T) {
	t.Parallel()
	// The record lives in the fleet's coordination store, since a detached run is
	// recovered by whichever node owns its seat next. internal/coord/coordtest
	// certifies the record itself; this suite covers the conditional flips built
	// on it: the at-most-once tail claim, the epoch fence, the pause expiry.
	sandboxtest.Run(t, func(*testing.T) (sandbox.PendingStore, coord.SandboxRuns) {
		runs := memory.NewFleet()
		return sandbox.NewCoordStore(runs), runs
	}, func(runs coord.SandboxRuns) sandbox.PendingStore { return sandbox.NewCoordStore(runs) })
}

// NOTHING EMPTY EVER ANSWERS ANYTHING, a rule of the value rather than of the
// store. A run launched by an A2A wake or a task assignment carries no
// conversation, so two absences comparing equal would splice the first thing a
// person said to a seat into somebody else's executor. The value decides which
// parked run a delivery answers ([sandbox.ConversationRef.Best]), so every
// caller matching a delivery to a run inherits the rule from it.
//
// A ROW NAMING ONLY A PARTITION IS ANSWERED BY NOTHING, even a reply in its own
// thread: the identity admits a delivery, and the partition only picks between
// rows the identity admitted.
func TestNoConversationAnswersNoParkedRun(t *testing.T) {
	t.Parallel()
	keyless := sandbox.PendingRun{TurnID: "t1", AgentHandle: "swe"}
	dm := sandbox.PendingRun{
		TurnID: "t2", AgentHandle: "swe",
		PartitionKey: "chat:D1:root-1", ConversationKey: "chat:D1",
	}
	partitionOnly := sandbox.PendingRun{
		TurnID: "t3", AgentHandle: "swe", PartitionKey: "chat:D1:root-1",
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
		{"a delivery naming no conversation, a run naming only a partition",
			sandbox.ConversationRef{}, partitionOnly},
		{"a reply in the run's own thread, a run naming only a partition",
			sandbox.ConversationRef{Identity: "chat:D1", Partition: "chat:D1:root-1"},
			partitionOnly},
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

// A ROW KEEPS WHAT THIS BUILD CANNOT READ. Every flip is a compare-and-swap of
// the WHOLE value, and mid-upgrade an older build makes some of them; decoding
// into the struct alone would delete a newer build's key (the item the run is
// charged to, the audience its question waits on) at its first claim. The key
// here is one no build declares, so the test is of the codec, not a field.
func TestAPendingRunKeepsKeysThisBuildDoesNotKnow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	runs := memory.NewFleet()
	store := sandbox.NewCoordStore(runs)
	const future = `{"engine":"v9","weights":[1,2,12345678901234567890]}`
	seed := `{"turn_id":"t1","agent_handle":"swe","status":"launching",` +
		`"launch_id":"l1","a_later_builds_field":` + future + `}`
	if created, err := runs.CreateSandboxRun(ctx, "t1", []byte(seed), nil); err != nil || !created {
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

// A RUN'S SPENT SESSIONS RIDE A KEY OF THEIR OWN SHAPE, in both directions of
// a rolling upgrade: a row the build before wrote names them as bare ids under
// `exhausted_sessions`, which this build carries in Extra and decodes; and a row
// this build writes names them with how each was spent under `spent_sessions`,
// which that build's decoder takes. One key in two shapes would leave each
// build unable to decode the other's rows.
//
// Mutation: name the spent sessions `exhausted_sessions` again, and both
// directions go red.
func TestARunsSpentSessionsRideAKeyOfTheirOwnShape(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	runs := memory.NewFleet()
	store := sandbox.NewCoordStore(runs)
	const earlier = `["s-0"]`
	seed := `{"turn_id":"t1","agent_handle":"swe","status":"running","launch_id":"l1",` +
		`"exhausted_sessions":` + earlier + `}`
	if created, err := runs.CreateSandboxRun(ctx, "t1", []byte(seed), nil); err != nil || !created {
		t.Fatalf("seed: created=%v err=%v", created, err)
	}
	got, found, err := store.Get(ctx, "t1")
	if err != nil || !found {
		t.Fatalf("a row the build before wrote does not decode: found=%v err=%v", found, err)
	}
	if string(got.Extra["exhausted_sessions"]) != earlier || len(got.SpentSessions) != 0 {
		t.Fatalf("the build before's spent sessions read as %v beside %s, want carried whole",
			got.SpentSessions, got.Extra)
	}

	if err := store.RetireSessions(ctx, "t1", executorkeep.LossContextExhausted, []string{"s-1"}); err != nil {
		t.Fatalf("RetireSessions: %v", err)
	}
	record, _, err := runs.SandboxRun(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	var before struct {
		Exhausted []string `json:"exhausted_sessions"`
	}
	if err := json.Unmarshal(record.Value, &before); err != nil || !slices.Equal(before.Exhausted, []string{"s-0"}) {
		t.Fatalf("the build before reads this build's row as %v (%v), want its own ids carried: %s",
			before.Exhausted, err, record.Value)
	}
	got, _, err = store.Get(ctx, "t1")
	if err != nil || len(got.SpentSessions) != 1 || got.SpentSessions[0] !=
		(sandbox.SpentSession{Session: "s-1", Loss: executorkeep.LossContextExhausted}) {
		t.Fatalf("this build reads its spent sessions as %+v (%v)", got.SpentSessions, err)
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

func (v *vanishingRuns) CreateSandboxRun(ctx context.Context, turnID string, value []byte,
	standing *coord.RunStanding) (bool, error) {
	v.creates++
	if v.creates == 1 {
		return false, nil
	}
	return v.runStore.CreateSandboxRun(ctx, turnID, value, standing)
}

// A ROW THAT VANISHES UNDER A LAUNCH IS CREATED AGAIN, not reported open:
// otherwise the launch starts a job in a box on a row that does not exist,
// which nothing would ever collect or reclaim.
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

// A LAUNCH UNDER A LEASE THE ROW HAS OUTGROWN IS THIS NODE NOT HOLDING THE
// SEAT ([sandbox.SeatLost]): said as anything else, a caller could not tell
// the seat's move from a broken launch, and would hand a refused relaunch of a
// resumed turn back for a retry rather than on to the seat's holder.
//
// Mutation: drop the %w ErrSeatNotHeld from writeFound's refusal of an
// outranked fence.
func TestALaunchUnderAnOutgrownLeaseIsASeatNotHeld(t *testing.T) {
	t.Parallel()
	store := sandbox.NewCoordStore(memory.NewFleet())
	ctx := context.Background()
	if _, err := store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: "t1", AgentHandle: "swe"},
		sandbox.Fence{Owner: "node-b:1", Epoch: 9}); err != nil {
		t.Fatalf("the newer lease's launch: %v", err)
	}
	_, err := store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: "t1", AgentHandle: "swe"},
		sandbox.Fence{Owner: "node-a:1", Epoch: 3})
	if !errors.Is(err, sandbox.ErrSeatNotHeld) {
		t.Fatalf("a launch under an outgrown lease = %v, want ErrSeatNotHeld", err)
	}
}
