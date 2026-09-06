package ledgerstore_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/store"
)

var base = time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)

func db(t *testing.T) *store.DB {
	t.Helper()
	d, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "l.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func completionImpls(t *testing.T) map[string]ledgerstore.Completions {
	t.Helper()
	// The SQL implementation is GONE, not omitted: the completion ledger
	// has to be agreed across the fleet, so it moved to the coordination
	// store — see internal/coord/fleet.go. The twin here and the fleet
	// implementation over the memory backend are the two this contract has,
	// and internal/coord/coordtest certifies the backends underneath.
	return map[string]ledgerstore.Completions{
		"memory": ledgerstore.NewMemoryCompletions(),
		"fleet":  ledgerstore.NewFleetCompletions(coordmemory.NewFleet()),
	}
}

func conversationImpls(t *testing.T) map[string]ledgerstore.Conversations {
	t.Helper()
	return map[string]ledgerstore.Conversations{
		"memory": ledgerstore.NewMemoryConversations(),
		"sql":    ledgerstore.NewConversations(db(t)),
	}
}

func TestARecordedTriggerReadsAsWorked(t *testing.T) {
	t.Parallel()
	for name, s := range completionImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := s.Record(ctx, "ceo", "wk-1", "turn-1", base); err != nil {
				t.Fatalf("Record: %v", err)
			}
			got := s.Worked(ctx, "ceo", []string{"wk-1", "wk-2"})
			if !got["wk-1"] {
				t.Error("a recorded key did not read as worked")
			}
			if got["wk-2"] {
				t.Error("an unrecorded key read as worked")
			}
		})
	}
}

func TestTheLedgerIsPerSeat(t *testing.T) {
	t.Parallel()
	// Two seats answering the same trigger is two turns, not one. Sharing
	// the key space would make whichever seat ran first silence the other.
	for name, s := range completionImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := s.Record(ctx, "ceo", "wk-1", "t1", base); err != nil {
				t.Fatalf("Record: %v", err)
			}
			if s.Worked(ctx, "cto", []string{"wk-1"})["wk-1"] {
				t.Error("one seat's completion silenced another's")
			}
		})
	}
}

func TestAnEmptyKeyIsNeverRecordedOrMatched(t *testing.T) {
	t.Parallel()
	// '' is the documented "a turn with no ledgerable trigger": it skips
	// the guard because there is nothing to collapse. Recording it would
	// make the first such turn silence every other one.
	for name, s := range completionImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := s.Record(ctx, "ceo", "", "t1", base); err != nil {
				t.Fatalf("Record: %v", err)
			}
			if s.Worked(ctx, "ceo", []string{""})[""] {
				t.Error("an empty key read as worked")
			}
			// And it must not poison a lookup that also asks about a real
			// key alongside it.
			if err := s.Record(ctx, "ceo", "wk-1", "t2", base); err != nil {
				t.Fatalf("Record: %v", err)
			}
			got := s.Worked(ctx, "ceo", []string{"", "wk-1"})
			if !got["wk-1"] {
				t.Error("a real key was lost when queried beside an empty one")
			}
		})
	}
}

func TestRecordingTwiceIsNotAnError(t *testing.T) {
	t.Parallel()
	// Two nodes completing one trigger is the case the ledger EXISTS to
	// collapse, so losing the write is the ordinary outcome and not a
	// failure to report. The record itself is first-writer-wins, which
	// internal/coord/coordtest asserts against every backend — including
	// that the second write does not move the record's age past the
	// retention the bucket enforces.
	for name, s := range completionImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := s.Record(ctx, "ceo", "wk-1", "t1", base); err != nil {
				t.Fatalf("Record: %v", err)
			}
			if err := s.Record(ctx, "ceo", "wk-1", "t2", base.Add(10*time.Hour)); err != nil {
				t.Fatalf("second Record: %v", err)
			}
			if !s.Worked(ctx, "ceo", []string{"wk-1"})["wk-1"] {
				t.Error("the key stopped being worked after a second record")
			}
		})
	}
}

func TestConversationHistoryReadsForwards(t *testing.T) {
	t.Parallel()
	// A conversation reads in the order it happened. Reversing it makes the
	// prompt claim the seat answered before it was asked.
	for name, s := range conversationImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			for i, reply := range []string{"first", "second", "third"} {
				if err := s.Append(ctx, "ceo", "thread-1",
					ledger.Session{Reply: reply}, "wk-"+reply,
					base.Add(time.Duration(i)*time.Minute), 0); err != nil {
					t.Fatalf("Append: %v", err)
				}
			}
			got, err := s.History(ctx, "ceo", "thread-1", 0)
			if err != nil {
				t.Fatalf("History: %v", err)
			}
			if len(got) != 3 {
				t.Fatalf("history = %d entries, want 3", len(got))
			}
			for i, want := range []string{"first", "second", "third"} {
				if got[i].Reply != want {
					t.Errorf("entry %d = %q, want %q", i, got[i].Reply, want)
				}
			}
		})
	}
}

func TestALimitKeepsTheNEWESTEntries(t *testing.T) {
	t.Parallel()
	// Ordering ascending and limiting keeps the OLDEST — the opposite of
	// what a follow-up turn needs, and a mistake that reads as working
	// until a conversation gets long.
	for name, s := range conversationImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			for i, reply := range []string{"first", "second", "third"} {
				if err := s.Append(ctx, "ceo", "t", ledger.Session{Reply: reply},
					"wk-"+reply, base.Add(time.Duration(i)*time.Minute), 0); err != nil {
					t.Fatalf("Append: %v", err)
				}
			}
			got, err := s.History(ctx, "ceo", "t", 2)
			if err != nil {
				t.Fatalf("History: %v", err)
			}
			if len(got) != 2 || got[0].Reply != "second" || got[1].Reply != "third" {
				t.Errorf("history = %v, want the newest two in order", replies(got))
			}
		})
	}
}

func TestOneWorkKeyAppendsOnce(t *testing.T) {
	t.Parallel()
	// A redelivery of a trigger this seat already answered. Recording it
	// twice tells the next turn it replied twice, which is the shape of the
	// bug the ledger exists to prevent.
	for name, s := range conversationImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			for range 2 {
				if err := s.Append(ctx, "ceo", "t", ledger.Session{Reply: "hi"},
					"wk-1", base, 0); err != nil {
					t.Fatalf("Append: %v", err)
				}
			}
			got, err := s.History(ctx, "ceo", "t", 0)
			if err != nil {
				t.Fatalf("History: %v", err)
			}
			if len(got) != 1 {
				t.Errorf("history = %d entries, want 1", len(got))
			}
		})
	}
}

func TestUnkeyedTurnsAreNotCollapsedOntoEachOther(t *testing.T) {
	t.Parallel()
	// '' means "no ledgerable trigger", not "the same trigger". Deduping on
	// it keeps ONE entry for every unkeyed turn the seat ever ran.
	for name, s := range conversationImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			for _, reply := range []string{"one", "two"} {
				if err := s.Append(ctx, "ceo", "t", ledger.Session{Reply: reply},
					"", base, 0); err != nil {
					t.Fatalf("Append: %v", err)
				}
			}
			got, err := s.History(ctx, "ceo", "t", 0)
			if err != nil {
				t.Fatalf("History: %v", err)
			}
			if len(got) != 2 {
				t.Errorf("history = %d entries, want both unkeyed turns", len(got))
			}
		})
	}
}

func TestTrimOnWriteBoundsAConversationThatNeverEnds(t *testing.T) {
	t.Parallel()
	// The retention sweep alone is not enough: a chat DM keys on the whole
	// CHANNEL, so one conversation never stops growing however recent its
	// entries are.
	for name, s := range conversationImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			for i := range 10 {
				if err := s.Append(ctx, "ceo", "dm", ledger.Session{Reply: string(rune('a' + i))},
					"wk-"+string(rune('a'+i)), base.Add(time.Duration(i)*time.Minute), 3); err != nil {
					t.Fatalf("Append: %v", err)
				}
			}
			got, err := s.History(ctx, "ceo", "dm", 0)
			if err != nil {
				t.Fatalf("History: %v", err)
			}
			if len(got) != 3 {
				t.Fatalf("history = %d entries, want the trim to have held it at 3", len(got))
			}
			if got[2].Reply != "j" {
				t.Errorf("newest = %q, want the last written", got[2].Reply)
			}
		})
	}
}

func TestConversationsAreScopedToASeatAndAThread(t *testing.T) {
	t.Parallel()
	for name, s := range conversationImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := s.Append(ctx, "ceo", "t1", ledger.Session{Reply: "x"}, "wk", base, 0); err != nil {
				t.Fatalf("Append: %v", err)
			}
			for _, probe := range [][2]string{{"cto", "t1"}, {"ceo", "t2"}} {
				got, err := s.History(ctx, probe[0], probe[1], 0)
				if err != nil {
					t.Fatalf("History: %v", err)
				}
				if len(got) != 0 {
					t.Errorf("%v read another conversation's history: %v", probe, replies(got))
				}
			}
		})
	}
}

func TestAnEmptyHistoryIsNotAnError(t *testing.T) {
	t.Parallel()
	// The first turn of every conversation. It must be distinguishable from
	// a store that could not be read, which is why History raises rather
	// than swallowing — but "nothing yet" is not a failure.
	for name, s := range conversationImpls(t) {
		t.Run(name, func(t *testing.T) {
			got, err := s.History(context.Background(), "ceo", "fresh", 0)
			if err != nil {
				t.Fatalf("History: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("history = %v, want nothing", replies(got))
			}
		})
	}
}

func TestAppendedEntriesSurviveTheRoundTrip(t *testing.T) {
	t.Parallel()
	for name, s := range conversationImpls(t) {
		t.Run(name, func(t *testing.T) {
			want := ledger.Session{
				TurnID: "t-1", At: "2026-08-20T09:00:00Z",
				Trigger: "@alice: ping", Intent: "reply",
				Calls: "- slack_post({\"channel\":\"C1\"}) → success",
				Reply: "pong", Decision: "done", CompletedWork: "posted",
			}
			if err := s.Append(context.Background(), "ceo", "t", want, "wk", base, 0); err != nil {
				t.Fatalf("Append: %v", err)
			}
			got, err := s.History(context.Background(), "ceo", "t", 0)
			if err != nil {
				t.Fatalf("History: %v", err)
			}
			if len(got) != 1 || got[0] != want {
				t.Errorf("round trip changed the entry:\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestConversationPurgeDropsOldEntries(t *testing.T) {
	t.Parallel()
	for name, s := range conversationImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := s.Append(ctx, "ceo", "t", ledger.Session{Reply: "old"}, "wk-old", base, 0); err != nil {
				t.Fatalf("Append: %v", err)
			}
			if err := s.Append(ctx, "ceo", "t", ledger.Session{Reply: "new"}, "wk-new", base.Add(2*time.Hour), 0); err != nil {
				t.Fatalf("Append: %v", err)
			}
			n, err := s.Purge(ctx, base.Add(time.Hour))
			if err != nil {
				t.Fatalf("Purge: %v", err)
			}
			if n != 1 {
				t.Errorf("purged %d, want 1", n)
			}
			got, err := s.History(ctx, "ceo", "t", 0)
			if err != nil {
				t.Fatalf("History: %v", err)
			}
			if len(got) != 1 || got[0].Reply != "new" {
				t.Errorf("history = %v, want just the recent entry", replies(got))
			}
		})
	}
}

func replies(ss []ledger.Session) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.Reply)
	}
	return out
}

// An UNKEYED turn writes nothing at all.
//
// Worked already refuses to MATCH an empty key, so a stored one would be
// invisible on the read path — which is exactly why the write guard needs its
// own assertion. The observation is the CALL, not a side effect: the guard is
// in the adapter, and a counting ledger underneath it sees whether the write
// was attempted at all.
//
// Found by mutation: removing the write guard changed no read.
func TestAnUnkeyedTurnLeavesNoRowBehind(t *testing.T) {
	t.Parallel()
	counter := &countingLedger{Fleet: coordmemory.NewFleet()}
	s := ledgerstore.NewFleetCompletions(counter)
	ctx := context.Background()

	if err := s.Record(ctx, "ceo", "", "t1", base); err != nil {
		t.Fatalf("an unkeyed turn was an error: %v", err)
	}
	if err := s.Record(ctx, "", "wk-1", "t1", base); err != nil {
		t.Fatalf("a seatless turn was an error: %v", err)
	}
	if counter.writes != 0 {
		t.Fatalf("%d writes reached the ledger, want none", counter.writes)
	}

	// THE CONTROL. Without it this passes for an adapter that writes
	// nothing at all.
	if err := s.Record(ctx, "ceo", "wk-1", "t2", base); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if counter.writes != 1 {
		t.Fatalf("a keyed turn made %d writes, want 1", counter.writes)
	}
}

// countingLedger counts the writes that reach it.
type countingLedger struct {
	*coordmemory.Fleet
	writes int
}

func (c *countingLedger) Record(ctx context.Context, scope, key, detail string, at time.Time) error {
	c.writes++
	return c.Fleet.Record(ctx, scope, key, detail, at)
}

func TestAConversationKeyCannotCollideWithAnother(t *testing.T) {
	t.Parallel()
	// The two halves are joined, and a separator that can appear in either
	// lets ("ceo", "a:b") and ("ceo:a", "b") land on one conversation —
	// which is a seat reading another thread's history as its own.
	//
	// Found by mutation: with ':' as the separator nothing failed, because
	// no test had ever put one in a handle or a thread id. Real thread keys
	// contain colons routinely (a channel id and a timestamp).
	for name, s := range conversationImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := s.Append(ctx, "ceo", "a:b", ledger.Session{Reply: "left"}, "wk-1", base, 0); err != nil {
				t.Fatalf("Append: %v", err)
			}
			if err := s.Append(ctx, "ceo:a", "b", ledger.Session{Reply: "right"}, "wk-2", base, 0); err != nil {
				t.Fatalf("Append: %v", err)
			}
			for _, probe := range []struct{ handle, conv, want string }{
				{"ceo", "a:b", "left"},
				{"ceo:a", "b", "right"},
			} {
				got, err := s.History(ctx, probe.handle, probe.conv, 0)
				if err != nil {
					t.Fatalf("History: %v", err)
				}
				if len(got) != 1 || got[0].Reply != probe.want {
					t.Errorf("(%q, %q) read %v, want just %q",
						probe.handle, probe.conv, replies(got), probe.want)
				}
			}
		})
	}
}

// The ledger FAILS OPEN on a read it could not complete.
//
// Not knowing whether work was done has one safe answer and it is the
// pre-ledger one — do the work. A read that failed closed would make a
// coordination-store blip look like a company that had already answered
// everything, and every seat would go quiet with nothing in the log to say
// why.
//
// The failure is injected at the CONTRACT rather than in a driver: the
// coordination store raises the error (so the decision belongs to whoever is
// about to do the work) and this is where that decision is made.
func TestAFailedCompletionReadAnswersNOTHINGRatherThanAPartialSet(t *testing.T) {
	t.Parallel()
	ledger := &faultyLedger{Fleet: coordmemory.NewFleet()}
	s := ledgerstore.NewFleetCompletions(ledger)
	ctx := context.Background()
	for _, k := range []string{"wk-1", "wk-2", "wk-3"} {
		if err := s.Record(ctx, "ceo", k, "t", base); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	// THE CONTROL. Without it every assertion below passes for a ledger
	// that never finds anything.
	if got := s.Worked(ctx, "ceo", []string{"wk-1", "wk-2", "wk-3"}); len(got) != 3 {
		t.Fatalf("healthy read found %v, want all three", got)
	}

	ledger.fail = errors.New("the coordination store is unreachable")
	got := s.Worked(ctx, "ceo", []string{"wk-1", "wk-2", "wk-3"})
	if len(got) != 0 {
		t.Errorf("a failed read answered %v, want nothing known — a partial answer "+
			"marks some triggers worked and leaves the rest unknown, with no way "+
			"for the caller to tell", got)
	}
	ledger.fail = nil
	if got := s.Worked(ctx, "ceo", []string{"wk-1"}); len(got) != 1 {
		t.Errorf("the ledger did not recover: %v", got)
	}
}

// faultyLedger is a coordination ledger whose reads can be made to fail.
type faultyLedger struct {
	*coordmemory.Fleet
	fail error
}

func (f *faultyLedger) Worked(ctx context.Context, scope string, keys []string) (map[string]bool, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	return f.Fleet.Worked(ctx, scope, keys)
}

// Threads is the OPERATOR's read of the same ledger a turn uses, and both
// implementations have to answer it identically — a memory twin that ordered
// differently would let a fleet test pass on the twin and fail on the store.

func TestThreadsListsWhatASeatIsCarrying(t *testing.T) {
	t.Parallel()
	for name, s := range conversationImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			write := func(conversation, reply string, at time.Time) {
				t.Helper()
				if err := s.Append(ctx, "ceo", conversation,
					ledger.Session{Reply: reply, At: at.Format(time.RFC3339)}, reply, at, 0); err != nil {
					t.Fatalf("Append: %v", err)
				}
			}
			write("thread-a", "one", base)
			write("thread-a", "two", base.Add(time.Minute))
			write("thread-b", "three", base.Add(time.Hour))
			// Another seat's conversation, which must not appear.
			if err := s.Append(ctx, "cto", "thread-c",
				ledger.Session{Reply: "theirs"}, "theirs", base, 0); err != nil {
				t.Fatal(err)
			}

			threads, err := s.Threads(ctx, "ceo", 0)
			if err != nil {
				t.Fatalf("Threads: %v", err)
			}
			if len(threads) != 2 {
				t.Fatalf("%d threads, want the two this seat holds: %+v", len(threads), threads)
			}
			// Newest activity first: a reader scanning a seat's threads is
			// looking for the one that moved most recently.
			if threads[0].Key != "thread-b" || threads[1].Key != "thread-a" {
				t.Fatalf("order = %s, %s — want newest activity first",
					threads[0].Key, threads[1].Key)
			}
			if threads[1].Entries != 2 {
				t.Errorf("thread-a holds %d entries, want 2", threads[1].Entries)
			}
			if !threads[1].LastAt.Equal(base.Add(time.Minute)) {
				t.Errorf("thread-a last at %v, want the newest entry's stamp", threads[1].LastAt)
			}
		})
	}
}

func TestThreadsIsBounded(t *testing.T) {
	t.Parallel()
	for name, s := range conversationImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			for i := range 10 {
				key := "thread-" + string(rune('a'+i))
				if err := s.Append(ctx, "ceo", key, ledger.Session{Reply: key}, key,
					base.Add(time.Duration(i)*time.Minute), 0); err != nil {
					t.Fatal(err)
				}
			}
			threads, err := s.Threads(ctx, "ceo", 3)
			if err != nil {
				t.Fatalf("Threads: %v", err)
			}
			if len(threads) != 3 {
				t.Fatalf("%d threads, want the limit of 3", len(threads))
			}
			// The limit keeps the RECENT ones. Keeping the oldest would be
			// the opposite of what a reader opening a seat wants.
			if threads[0].Key != "thread-j" {
				t.Errorf("first = %s, want the most recent", threads[0].Key)
			}
		})
	}
}

func TestASeatWithNoConversationsListsNothing(t *testing.T) {
	t.Parallel()
	for name, s := range conversationImpls(t) {
		t.Run(name, func(t *testing.T) {
			threads, err := s.Threads(context.Background(), "nobody", 0)
			if err != nil {
				t.Fatalf("Threads: %v", err)
			}
			if len(threads) != 0 {
				t.Errorf("threads = %+v, want none", threads)
			}
		})
	}
}
