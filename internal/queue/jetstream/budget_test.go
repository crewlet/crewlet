package jetstream

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
)

// fileQueue opens an embedded broker with a file store, which is the one a
// deployment runs and the one whose cap is taken from a volume — for a case
// whose subject is that cap, so it is opened under [SizedByTheDisk].
func fileQueue(t *testing.T) *Queue {
	t.Helper()
	q, err := Open(SizedByTheDisk(t.Context(), "the case is about the cap a file store takes from its volume"),
		Config{StoreDir: brokertest.TempDir(t)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return q
}

// sizedFileQueue is [fileQueue] with its cap declared at
// [brokertest.StoreMaxBytes], for a case that reserves gibibytes and whose
// subject is not where the cap comes from: a derived cap is three quarters of
// the runner's free space, so a case reserving three gibibytes would fail on
// a runner with less than four free.
func sizedFileQueue(t *testing.T) *Queue {
	t.Helper()
	return newQueueWith(t, Config{StoreDir: brokertest.TempDir(t), StoreMaxBytes: brokertest.StoreMaxBytes})
}

// budgetLog is a domain stream reserving ceiling bytes.
func budgetLog(name string, ceiling int64) DomainStream {
	return DomainStream{
		Name:       name,
		Subjects:   []string{"crewlet.budget." + name + ".>"},
		MaxBytes:   ceiling,
		Duplicates: 2 * time.Minute,
	}
}

// AN EMBEDDED BROKER'S BUDGET IS ITS OWN CAP, AND RESERVATIONS SPEND IT.
//
// Its account states no limit, so a budget read from the account alone is
// "unlimited" on exactly the topology that refuses a boot for want of room.
// The broker compares a new ceiling against the ceilings it has granted, not
// what they store: a stream reserving bytes and holding nothing has spent them.
func TestAnEmbeddedBrokersBudgetIsItsOwnCap(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		open func(t *testing.T) *Queue
		want BudgetSource
	}{
		"a file store":        {fileQueue, BudgetServerStore},
		"an in-memory broker": {newQueue, BudgetServerMemory},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q := tc.open(t)
			before, err := q.StreamBudget(t.Context())
			if err != nil {
				t.Fatalf("StreamBudget: %v", err)
			}
			if before.Source != tc.want || before.Limit <= 0 {
				t.Fatalf("budget = %+v, want a stated limit from %s", before, tc.want)
			}

			// A GIBIBYTE OR HALF WHAT IS LEFT, whichever is smaller: this
			// row's subject is a cap derived from the runner's free disk,
			// which a fixed gibibyte could exceed.
			ceiling := min(int64(1)<<30, before.Available()/2)
			if err := q.EnsureDomainStream(t.Context(), budgetLog("SPEND", ceiling)); err != nil {
				t.Fatalf("EnsureDomainStream: %v", err)
			}
			after, err := q.StreamBudget(t.Context())
			if err != nil {
				t.Fatalf("StreamBudget: %v", err)
			}
			if after.Committed-before.Committed != ceiling {
				t.Errorf("a %d-byte reservation moved the committed figure from %d to %d, "+
					"so the budget is counting what streams hold rather than what "+
					"they reserve", ceiling, before.Committed, after.Committed)
			}
		})
	}
}

// AN OPERATOR'S BROKER THAT STATES NO LIMIT STILL STATES WHAT IT RESERVED.
//
// A node sizes from its own free space in place of the cap it cannot read,
// and every ceiling already granted (the object bucket's, created before the
// state logs) is spent from that figure. Dropping the reservations with the
// limit would size the logs into bytes the bucket holds, and the server would
// refuse the last log created.
func TestAnUnstatedBrokersBudgetCarriesItsReservations(t *testing.T) {
	t.Parallel()
	q := newQueueWith(t, Config{URL: externaltest.StartUnstated(t, queue.MaxPayloadBytes).URL(),
		Token: externaltest.Token})
	before, err := q.StreamBudget(t.Context())
	if err != nil {
		t.Fatalf("StreamBudget: %v", err)
	}
	if before.Limit >= 0 || before.Source != BudgetUnstated {
		t.Fatalf("an operator's broker with no account limit read as %+v, want unstated", before)
	}
	const ceiling = int64(1) << 30
	if err := q.EnsureDomainStream(t.Context(), budgetLog("UNSTATED_SPEND", ceiling)); err != nil {
		t.Fatalf("EnsureDomainStream: %v", err)
	}
	after, err := q.StreamBudget(t.Context())
	if err != nil {
		t.Fatalf("StreamBudget: %v", err)
	}
	if after.Committed-before.Committed != ceiling {
		t.Errorf("a %d-byte reservation on a broker that states no limit moved the "+
			"committed figure from %d to %d", ceiling, before.Committed, after.Committed)
	}
}

// THE TEST SUITES' EXTERNAL BROKER STATES ITS ACCOUNT'S LIMIT, and it is the
// shared test size: on a broker stating none, a node sizes its state logs from
// its volume's free space, so every suite booting against externaltest's
// server would pass or fail on how full the runner's disk is. Read through
// StreamBudget, as a node reads it.
func TestTheTestSuitesExternalBrokerStatesItsAccountLimit(t *testing.T) {
	t.Parallel()
	q := newQueueWith(t, Config{URL: externaltest.Start(t, queue.MaxPayloadBytes).URL(),
		Token: externaltest.Token})
	budget, err := q.StreamBudget(t.Context())
	if err != nil {
		t.Fatalf("StreamBudget: %v", err)
	}
	if budget.Source != BudgetAccount || budget.Limit != brokertest.StoreMaxBytes {
		t.Fatalf("the test broker's budget read as %+v, want the account's own "+
			"limit of %d (brokertest.StoreMaxBytes)", budget, brokertest.StoreMaxBytes)
	}
}

// A RESERVATION THE BROKER REFUSES FOR WANT OF ROOM IS NAMED, and one it can
// grant is granted. The broker says only `insufficient storage resources
// available`, so a caller must recognise the refusal without speaking this
// backend's codes to say what was needed and what to change.
func TestARefusedReservationIsNamed(t *testing.T) {
	t.Parallel()
	q := sizedFileQueue(t)
	budget, err := q.StreamBudget(t.Context())
	if err != nil {
		t.Fatalf("StreamBudget: %v", err)
	}
	err = q.EnsureDomainStream(t.Context(), budgetLog("TOO_BIG", budget.Available()+1))
	if !errors.Is(err, ErrInsufficientStorage) {
		t.Fatalf("a ceiling one byte past what the broker had left returned %v, "+
			"want %v", err, ErrInsufficientStorage)
	}
	if err := q.EnsureDomainStream(t.Context(), budgetLog("FITS", budget.Available())); err != nil {
		t.Fatalf("a ceiling of exactly what was left was refused: %v", err)
	}
}

// AND A REFUSED RESERVATION IS NOT READ BACK, because nothing was placed: the
// limits check runs only once no peer holds an assignment for the name, so
// the peer-race read-back has no object to wait for. Falling through would
// spend the whole read-back window per refused log and wrap the error a
// refused boot needs (the bytes, the limit, the Tier A field) in "(and it is
// not there: stream not found)", which reads as the cause.
func TestARefusedReservationIsNotReadBack(t *testing.T) {
	t.Parallel()
	q := sizedFileQueue(t)
	budget, err := q.StreamBudget(t.Context())
	if err != nil {
		t.Fatalf("StreamBudget: %v", err)
	}
	err = q.EnsureDomainStream(t.Context(), budgetLog("REFUSED", budget.Available()+1))
	if !errors.Is(err, ErrInsufficientStorage) {
		t.Fatalf("EnsureDomainStream = %v, want the refusal", err)
	}
	// THE READ-BACK'S OWN WORDS, which only a fall-through can produce.
	for _, unwanted := range []string{"and it is not there", "stream not found"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("the refusal carries %q, so it was read back after a create "+
				"that placed nothing:\n%v", unwanted, err)
		}
	}
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		t.Errorf("the refusal wraps a not-found from the read-back: %v", err)
	}
}

// A LONE BROKER'S GROWTH BUDGET IS EXACTLY THE ROOM IT HOLDS AN UPDATE TO.
//
// The capacity verb refuses a raise past it before a window opens, so a figure
// that understated the room would refuse raises the broker grants, and one that
// overstated it would let through the refusal the check exists to catch. Both
// directions are the broker's own answer here, one byte apart.
func TestAGrowthBudgetIsTheRoomAnUpdateIsHeldTo(t *testing.T) {
	t.Parallel()
	q := sizedFileQueue(t)
	const ceiling = int64(1) << 30
	if err := q.EnsureDomainStream(t.Context(), budgetLog("GROW", ceiling)); err != nil {
		t.Fatalf("EnsureDomainStream: %v", err)
	}
	room, err := q.GrowthBudget(t.Context())
	if err != nil {
		t.Fatalf("GrowthBudget: %v", err)
	}
	if room.Source != BudgetServerStore || room.Available() <= 0 {
		t.Fatalf("a lone file-store broker's growth budget = %+v, want its own stated room", room)
	}
	grow, err := q.DomainLog(t.Context(), "GROW")
	if err != nil {
		t.Fatalf("DomainLog: %v", err)
	}
	err = grow.SetMaxBytes(t.Context(), uint64(ceiling+room.Available()+1))
	if !jsprovision.OutOfCapacity(err) {
		t.Fatalf("a raise one byte past the stated room returned %v, want the "+
			"broker's refusal: the budget understates what it grants", err)
	}
	if err := grow.SetMaxBytes(t.Context(), uint64(ceiling+room.Available())); err != nil {
		t.Fatalf("a raise of exactly the stated room was refused, so the budget "+
			"overstates it: %v", err)
	}
}

// WHETHER A DOMAIN STREAM EXISTS IS THREE ANSWERS, and the ceiling it holds is
// the one it was created with.
func TestADomainStreamsCeilingIsReadBack(t *testing.T) {
	t.Parallel()
	q := sizedFileQueue(t)
	if held, found, err := q.DomainStreamCeiling(t.Context(), "ABSENT"); err != nil || found || held != 0 {
		t.Fatalf("an absent stream = (%d, %v, %v), want (0, false, nil)", held, found, err)
	}
	const ceiling = int64(3) << 30
	if err := q.EnsureDomainStream(t.Context(), budgetLog("PRESENT", ceiling)); err != nil {
		t.Fatalf("EnsureDomainStream: %v", err)
	}
	if held, found, err := q.DomainStreamCeiling(t.Context(), "PRESENT"); err != nil || !found || held != ceiling {
		t.Fatalf("a present stream = (%d, %v, %v), want (%d, true, nil)", held, found, err, ceiling)
	}
}

// droppedCeilingJS answers a stream's info the way a member answers a read of a
// stream another node has in flight: the first drops requests are never
// answered — each ends when its own deadline does — and every later one is
// answered with the ceiling the stream holds. A negative drops answers none.
type droppedCeilingJS struct {
	jetstream.JetStream
	ceiling int64
	drops   int

	// mu guards what follows: an unanswered request is still outstanding
	// while the next is asked, so two can be recorded at once.
	mu sync.Mutex
	// deadlines is the deadline each request was handed (zero for none) and
	// sent when each was sent. The case subtracts them inside its bubble,
	// where both are the bubble's instants; the time left computed here,
	// outside the bubble, reads as the machine's clock (internal/fixedwait).
	deadlines []time.Time
	sent      []time.Time
}

func (f *droppedCeilingJS) Stream(ctx context.Context, _ string) (jetstream.Stream, error) {
	deadline, bounded := ctx.Deadline()
	f.mu.Lock()
	f.deadlines, f.sent = append(f.deadlines, deadline), append(f.sent, time.Now())
	answer := f.drops >= 0 && len(f.sent) > f.drops
	f.mu.Unlock()
	if answer {
		return ceilingStream{maxBytes: f.ceiling}, nil
	}
	if !bounded {
		// NATS.GO'S OWN DEFAULT ends a request handed no deadline, five
		// seconds on; spent at once here, because the caller is told
		// the same nothing either way.
		return nil, context.DeadlineExceeded
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// ceilingStream reports the byte ceiling a stream was created with.
type ceilingStream struct {
	jetstream.Stream
	maxBytes int64
}

func (s ceilingStream) CachedInfo() *jetstream.StreamInfo {
	return &jetstream.StreamInfo{Config: jetstream.StreamConfig{MaxBytes: s.maxBytes}}
}

// A DOMAIN STREAM'S CEILING IS A READ: a request nobody answered is asked
// again, and a stream nobody would describe is unknown rather than absent.
// State-log start sizes the logs while booting peers create these streams and
// a member drops a read of one in flight ([jsprovision.ReadTerm]); the sizing
// counts unknown as absent, so a log sized off a dropped read keeps a ceiling
// short of what the stream holds, for good.
//
// Staged twice, in a synctest bubble ([inBubble]) so the re-ask cadence holds
// to the nanosecond: a first request dropped, asked again after a read term
// and a re-ask, then answered; every request dropped, which must come back as
// the silence it is. Mutation: hand the read no deadline and an ask carries
// none; ask it on a write's term and an ask is handed past the lookup ceiling.
func TestADomainStreamsCeilingIsAReadAskedAgainWhenDropped(t *testing.T) {
	t.Parallel()
	// THE READ TERM AND PAUSE SCALED DOWN, the lookup ceiling with them: a
	// dropped request is presumed lost only when its term ends, so
	// production's second apiece would cost the stagings two and thirty
	// seconds. The write's term stays production's clustered fifteen seconds,
	// so a read asked as a write shows in its next request's timing and its
	// ceiling.
	timing := jsprovision.Clustered(true).Timing()
	timing.ReadTerm, timing.ReAsk = 50*time.Millisecond, 20*time.Millisecond
	timing.Lookup = 10 * (timing.ReadTerm + timing.ReAsk)
	const ceiling = int64(3) << 30

	t.Run("its first request dropped", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			js := &droppedCeilingJS{ceiling: ceiling, drops: 1}
			q := &Queue{js: js, timing: timing}
			held, found, err := q.DomainStreamCeiling(t.Context(), "CREWLET_TRACKER_LOG")
			if err != nil || !found || held != ceiling {
				t.Fatalf("a stream whose first read was dropped = (%d, %v, %v), want "+
					"(%d, true, nil) — a request nobody answered is asked again "+
					"rather than being the answer", held, found, err, ceiling)
			}
			js.mu.Lock()
			defer js.mu.Unlock()
			if len(js.sent) != 2 {
				t.Fatalf("the ceiling was asked for %d time(s), want 2: the dropped "+
					"request and the one that was answered", len(js.sent))
			}
			for i, deadline := range js.deadlines {
				switch left := deadline.Sub(js.sent[i]); {
				case deadline.IsZero():
					t.Errorf("ask %d carried no deadline of its own, so the client's "+
						"five-second default decided it", i+1)
				case left > timing.Lookup:
					t.Errorf("ask %d was given %v, past the %v lookup ceiling — it is "+
						"bounded as a write", i+1, left, timing.Lookup)
				}
			}
			if gap, want := js.sent[1].Sub(js.sent[0]), timing.ReadTerm+timing.ReAsk; gap != want {
				t.Errorf("the dropped request was asked again %v later, want its "+
					"%v read term and %v re-ask — a read the server dropped is held "+
					"for a write's %v term rather than asked again", gap,
					timing.ReadTerm, timing.ReAsk, timing.AskTerm)
			}
		})
	})

	t.Run("every request dropped", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			js := &droppedCeilingJS{ceiling: ceiling, drops: -1}
			q := &Queue{js: js, timing: timing}
			held, found, err := q.DomainStreamCeiling(t.Context(), "CREWLET_TRACKER_LOG")
			if err == nil || found || held != 0 {
				t.Fatalf("a stream no request was answered about = (%d, %v, %v), "+
					"want (0, false, an error) — silence is neither a ceiling nor "+
					"an absent stream", held, found, err)
			}
			if !jsprovision.Unanswered(t.Context(), err) {
				t.Errorf("the error is not the silence itself, so the caller cannot "+
					"tell a broker that said nothing from one that refused: %v", err)
			}
			js.mu.Lock()
			defer js.mu.Unlock()
			if len(js.sent) < 2 {
				t.Errorf("the ceiling was asked for %d time(s) before the read gave "+
					"up; the lookup ceiling holds several, and one means it was "+
					"never asked again", len(js.sent))
			}
		})
	})
}

// AN ACCOUNT'S BUDGET IS IN CEILING UNITS, from whichever shape the account
// has.
//
// An untiered limit counts every replica of a ceiling, a tiered one is per
// replica count and leaves its top-level limits at zero, and "no tier for this
// replica count" grants nothing rather than everything.
func TestAnAccountsBudgetIsInCeilingUnits(t *testing.T) {
	t.Parallel()
	const gib = int64(1) << 30
	limited := func(store, memory int64, reservedStore, reservedMemory uint64) jetstream.Tier {
		return jetstream.Tier{
			ReservedStore: reservedStore, ReservedMemory: reservedMemory,
			Limits: jetstream.AccountLimits{MaxStore: store, MaxMemory: memory},
		}
	}
	for name, tc := range map[string]struct {
		info     jetstream.AccountInfo
		replicas int
		memory   bool
		want     StorageBudget
	}{
		"untiered, one replica": {
			info:     jetstream.AccountInfo{Tier: limited(30*gib, 0, uint64(4*gib), 0)},
			replicas: 1,
			want:     StorageBudget{Limit: 30 * gib, Committed: 4 * gib, Source: BudgetAccount},
		},
		"untiered, three replicas": {
			info:     jetstream.AccountInfo{Tier: limited(30*gib, 0, uint64(4*gib), 0)},
			replicas: 3,
			want:     StorageBudget{Limit: 10 * gib, Committed: 4 * gib, Source: BudgetAccount},
		},
		"tiered, the replica count's own tier": {
			info: jetstream.AccountInfo{Tiers: map[string]jetstream.Tier{
				"R1": limited(2*gib, 0, 0, 0),
				"R3": limited(30*gib, 0, uint64(4*gib), 0),
			}},
			replicas: 3,
			want:     StorageBudget{Limit: 30 * gib, Committed: 4 * gib, Source: BudgetAccount},
		},
		// UNDER ITS OWN SOURCE, which a bare zero could not carry: the
		// server refuses every create on such an account before comparing a
		// byte, so what clears it is a setting rather than room.
		"tiered, no tier for this replica count": {
			info: jetstream.AccountInfo{Tiers: map[string]jetstream.Tier{
				"R1": limited(30*gib, 0, 0, 0),
				"R5": limited(90*gib, 0, 0, 0),
			}},
			replicas: 3,
			want:     StorageBudget{Limit: 0, Source: BudgetAccountNoTier},
		},
		// A TIER THAT IS THERE AND STATES NOTHING is the same refusal: the
		// account's report lists a class it merely holds OBJECTS in, so
		// "present" does not mean "declared".
		"tiered, this node's class present with no limit on it": {
			info: jetstream.AccountInfo{Tiers: map[string]jetstream.Tier{
				"R1": limited(30*gib, 0, 0, 0),
				"R3": {ReservedStore: uint64(gib)},
			}},
			replicas: 3,
			want: StorageBudget{Limit: 0, Committed: gib,
				Source: BudgetAccountTierNoLimit},
		},
		// AND ZERO IS NOT THE SAME TEST AS BELOW ZERO. A tier declared
		// unlimited reports its limit negative and the broker creates
		// against it, so a `<= 0` reading would refuse the tiered
		// account with the most room of all.
		"tiered, and this node's class is unlimited": {
			info: jetstream.AccountInfo{Tiers: map[string]jetstream.Tier{
				"R3": limited(-1, -1, 0, 0),
			}},
			replicas: 3,
			want:     StorageBudget{Limit: -1, Source: BudgetUnstated},
		},
		// AND THE DISCRIMINATOR IS THE PRESENCE OF TIERS, NOT OF THIS
		// ONE: asking the second sends the case above to the untiered
		// branch, which reads its unset top-level MaxStore as a set limit.
		"tiered, and this node's class is the only one": {
			info: jetstream.AccountInfo{Tiers: map[string]jetstream.Tier{
				"R3": limited(30*gib, 0, uint64(gib), 0),
			}},
			replicas: 3,
			want:     StorageBudget{Limit: 30 * gib, Committed: gib, Source: BudgetAccount},
		},
		// AND WHAT IT HAS RESERVED IS CARRIED WITHOUT ONE: a node sizing
		// from its own disk spends the ceilings already granted (the object
		// bucket's, say) from that figure.
		"no limit stated": {
			info:     jetstream.AccountInfo{Tier: limited(-1, -1, uint64(3*gib), 0)},
			replicas: 1,
			want:     StorageBudget{Limit: -1, Committed: 3 * gib, Source: BudgetUnstated},
		},
		"memory storage reads the memory limit": {
			info:     jetstream.AccountInfo{Tier: limited(30*gib, 6*gib, 0, uint64(2*gib))},
			replicas: 1, memory: true,
			want: StorageBudget{Limit: 6 * gib, Committed: 2 * gib, Source: BudgetAccount},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := accountBudget(&tc.info, tc.replicas, tc.memory)
			if got != tc.want {
				t.Fatalf("accountBudget = %+v, want %+v", got, tc.want)
			}
			if !got.Source.Valid() {
				t.Errorf("source %q is not one this build knows", got.Source)
			}
		})
	}
	if (StorageBudget{Limit: 4 * gib, Committed: 5 * gib}).Available() != 0 {
		t.Error("an overspent limit reports room")
	}
	if (StorageBudget{Limit: -1}).Available() != -1 {
		t.Error("an unstated limit reports a number")
	}
}
