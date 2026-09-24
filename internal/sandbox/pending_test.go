package sandbox_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
	"github.com/crewlet/crewlet/internal/workkey"
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

// A ROW PARKED BEFORE THE SPLIT STILL KNOWS ITS UNIT OF WORK.
//
// Nothing rewrites a parked row, so a run suspended by a build from before
// ADR-0017 carries no `work_key` and its `turn_id` IS one. A resume days later
// has no trigger left to re-derive from, so reading the raw field would dedupe
// its conversation entry and its tracker writes against nothing.
func TestAPreSplitRunStillAnswersForItsUnitOfWork(t *testing.T) {
	t.Parallel()
	key := workkey.Derive([]string{"evt-a"})
	for name, tc := range map[string]struct {
		run  sandbox.PendingRun
		want string
	}{
		"post-split, keyed":  {sandbox.PendingRun{TurnID: uuid.NewString(), WorkKey: key}, key},
		"pre-split row":      {sandbox.PendingRun{TurnID: key}, key},
		"post-split, no key": {sandbox.PendingRun{TurnID: uuid.NewString()}, ""},
	} {
		if got := tc.run.UnitOfWork(); got != tc.want {
			t.Errorf("%s: UnitOfWork = %q, want %q", name, got, tc.want)
		}
	}
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
	preSplit := sandbox.PendingRun{
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
		{"a delivery naming no conversation, a row from before the split",
			sandbox.ConversationRef{}, preSplit},
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

// A ROW WHOSE STORED IDENTITY IS REALLY A PARTITION IS STILL ANSWERABLE IN THE
// THREAD IT ASKED IN.
//
// The row shape is reachable and permanent: a run parked by a build from
// before the split carries one value, this build resumes it — the fallback is
// what makes that possible — and if that resumed turn parks again it writes
// the value it was given into BOTH fields. The identity field then holds a
// THREAD where this build would have written the DM line, so a reply on that
// line is compared against the wrong grain and matches nothing at all: worse
// than the partition equality this replaced, which would still have found it
// for a reply in the same thread.
//
// So the identity branch accepts a partition match as well. For a well-formed
// row it admits nothing — a partition key is its identity or a finer cut of
// it, so equal partitions imply equal identities and the clause never decides
// anything — which is why the second half below is the same assertion made
// against a correct row.
func TestAnIdentityThatIsReallyAPartitionIsStillAnswerable(t *testing.T) {
	t.Parallel()
	rewritten := sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe",
		PartitionKey: "chat:D1:root-1", ConversationKey: "chat:D1:root-1",
	}
	wellFormed := sandbox.PendingRun{
		TurnID: "t2", AgentHandle: "swe",
		PartitionKey: "chat:D1:root-1", ConversationKey: "chat:D1",
	}
	// The person's reply on the DM line, in the thread the question was
	// asked in: the identity is the channel, the partition is the thread.
	reply := sandbox.ConversationRef{Identity: "chat:D1", Partition: "chat:D1:root-1"}
	if !reply.Answers(rewritten) {
		t.Error("a row carrying a thread where this build writes the DM line is " +
			"answerable by nothing, so its run waits out its pause TTL with the " +
			"reply already delivered")
	}
	if !reply.Answers(wellFormed) {
		t.Error("the control: a well-formed row stopped being answerable")
	}
	// AND NEITHER IS WIDENED TO ANOTHER CONVERSATION. The partition clause
	// can only ever admit a delivery that arrived in the very batch the run
	// was launched from, which no other conversation's reply does.
	elsewhere := sandbox.ConversationRef{Identity: "chat:D2", Partition: "chat:D2:root-1"}
	for _, run := range []sandbox.PendingRun{rewritten, wellFormed} {
		if elsewhere.Answers(run) {
			t.Errorf("a reply on another conversation answered %s", run.TurnID)
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
	if err := store.BeginLaunch(ctx, sandbox.PendingRun{
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
	if err := store.BeginLaunch(ctx, sandbox.PendingRun{
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
