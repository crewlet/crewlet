package sandbox_test

import (
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"

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
	sandboxtest.Run(t, func(*testing.T) sandbox.PendingStore {
		return sandbox.NewCoordStore(memory.NewFleet())
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
		ConversationKey: "chat:D1:root-1", ConversationIdentity: "chat:D1",
	}
	preSplit := sandbox.PendingRun{
		TurnID: "t3", AgentHandle: "swe", ConversationKey: "chat:D1:root-1",
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
