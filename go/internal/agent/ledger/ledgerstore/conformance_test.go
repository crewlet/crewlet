package ledgerstore_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
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
	return map[string]ledgerstore.Completions{
		"memory": ledgerstore.NewMemoryCompletions(),
		"sql":    ledgerstore.NewCompletions(db(t)),
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

func TestRecordingTwiceKeepsTheFirstTime(t *testing.T) {
	t.Parallel()
	// A redelivery must not move the completion forward past the retention
	// cutoff, which would keep resurrecting a row the sweep should have
	// taken.
	for name, s := range completionImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := s.Record(ctx, "ceo", "wk-1", "t1", base); err != nil {
				t.Fatalf("Record: %v", err)
			}
			if err := s.Record(ctx, "ceo", "wk-1", "t2", base.Add(10*time.Hour)); err != nil {
				t.Fatalf("second Record: %v", err)
			}
			n, err := s.Purge(ctx, base.Add(time.Hour))
			if err != nil {
				t.Fatalf("Purge: %v", err)
			}
			if n != 1 {
				t.Errorf("purged %d, want 1 — the second write moved the timestamp", n)
			}
		})
	}
}

func TestPurgeSparesRecentCompletions(t *testing.T) {
	t.Parallel()
	for name, s := range completionImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := s.Record(ctx, "ceo", "old", "t1", base); err != nil {
				t.Fatalf("Record: %v", err)
			}
			if err := s.Record(ctx, "ceo", "new", "t2", base.Add(2*time.Hour)); err != nil {
				t.Fatalf("Record: %v", err)
			}
			if _, err := s.Purge(ctx, base.Add(time.Hour)); err != nil {
				t.Fatalf("Purge: %v", err)
			}
			got := s.Worked(ctx, "ceo", []string{"old", "new"})
			if got["old"] {
				t.Error("an old completion survived the sweep")
			}
			if !got["new"] {
				t.Error("a recent completion was swept")
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
				Trigger: "@alice: ping", PlanSummary: "reply",
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

func TestAnUnkeyedTurnLeavesNoRowBehind(t *testing.T) {
	t.Parallel()
	// Worked already refuses to MATCH an empty key, so a stored one is
	// invisible on the read path — which is exactly why the write guard
	// needs its own assertion. Purge is where it shows: an unkeyed turn
	// that wrote a row would grow the table with entries nothing can ever
	// look up.
	//
	// Found by mutation: removing the write guard changed no read.
	for name, s := range completionImpls(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := s.Record(ctx, "ceo", "", "t1", base); err != nil {
				t.Fatalf("Record: %v", err)
			}
			if err := s.Record(ctx, "ceo", "wk-1", "t2", base); err != nil {
				t.Fatalf("Record: %v", err)
			}
			n, err := s.Purge(ctx, base.Add(time.Hour))
			if err != nil {
				t.Fatalf("Purge: %v", err)
			}
			if n != 1 {
				t.Errorf("purged %d rows, want 1 — an unkeyed turn wrote one", n)
			}
		})
	}
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

func TestAFailedCompletionReadAnswersNOTHINGRatherThanAPartialSet(t *testing.T) {
	t.Parallel()
	// The fail-open direction, probed rather than asserted from the code.
	//
	// TWO failure paths, and they are not the same. The query failing
	// outright is reachable by closing the database. The result set failing
	// PART WAY THROUGH iteration is not — and that is the one that decides
	// between "nothing is known" and a silent PARTIAL answer, where some
	// triggers read as worked, the rest as unknown, and the caller cannot
	// tell which half it got. A seat then skips part of a conversation.
	//
	// Both are exercised here, the second through a driver wrapper that
	// stops the rows mid-iteration. The SQL, the schema and the encoding
	// are all real; the only fiction is when the rows end.
	fault := storetest.FailReadsAfter(1, errors.New("connection reset mid-iteration"))
	d, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "fault.db"),
		store.Options{WrapDriver: fault.Wrap})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	s := ledgerstore.NewCompletions(d)
	ctx := context.Background()
	for _, k := range []string{"wk-1", "wk-2", "wk-3"} {
		if err := s.Record(ctx, "ceo", k, "t", base); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	// THE CONTROL. Without it every assertion below passes for a store
	// that never finds anything.
	if got := s.Worked(ctx, "ceo", []string{"wk-1", "wk-2", "wk-3"}); len(got) != 3 {
		t.Fatalf("healthy read found %v, want all three", got)
	}

	fault.Arm()
	got := s.Worked(ctx, "ceo", []string{"wk-1", "wk-2", "wk-3"})
	if len(got) != 0 {
		t.Errorf("a read that failed mid-iteration answered %v, want nothing known — "+
			"a partial answer marks some triggers worked and leaves the rest "+
			"unknown, with no way for the caller to tell", got)
	}
	fault.Disarm()

	// And it recovers: the fail-open answer is for the duration of the
	// failure, not a latched state.
	if got := s.Worked(ctx, "ceo", []string{"wk-1"}); !got["wk-1"] {
		t.Error("the read did not recover once the fault cleared")
	}

	// The other path: the query itself failing.
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := s.Worked(ctx, "ceo", []string{"wk-1"}); len(got) != 0 {
		t.Errorf("a read against a closed store answered %v, want nothing known", got)
	}
}

func TestAnUnscannableRowAlsoAnswersNOTHING(t *testing.T) {
	t.Parallel()
	// The THIRD failure path, and a separate branch from the other two: a
	// value the column cannot hold surfaces through rows.Scan, not through
	// rows.Err. The two lines look identical in the source and only one of
	// them is exercised by a transport failure.
	//
	// Found by mutation: making the Scan branch return its partial result
	// passed every test, including the mid-iteration one.
	fault := storetest.CorruptReadsAfter(1)
	d, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "corrupt.db"),
		store.Options{WrapDriver: fault.Wrap})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	s := ledgerstore.NewCompletions(d)
	ctx := context.Background()
	for _, k := range []string{"wk-1", "wk-2", "wk-3"} {
		if err := s.Record(ctx, "ceo", k, "t", base); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if got := s.Worked(ctx, "ceo", []string{"wk-1", "wk-2", "wk-3"}); len(got) != 3 {
		t.Fatalf("healthy read found %v, want all three", got)
	}

	fault.Arm()
	if got := s.Worked(ctx, "ceo", []string{"wk-1", "wk-2", "wk-3"}); len(got) != 0 {
		t.Errorf("a read that hit an unscannable row answered %v, want nothing known", got)
	}
}

func TestAStoredEmptyKeyStillNeverMatches(t *testing.T) {
	t.Parallel()
	// Two guards defend one property — Record refuses to write an empty key
	// and Worked refuses to query for one — and the second is invisible
	// while the first holds. It is not redundant: it is what stops a row
	// written by anything else (a migration, a future writer, a hand-fixed
	// database) from reading as "already worked" for EVERY unkeyed turn
	// this seat ever runs.
	//
	// Found by mutation: removing the query-side skip changed nothing,
	// because no test had ever put such a row there.
	d, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "raw.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.SQL().ExecContext(t.Context(),
		`INSERT INTO turn_completions (work_key, agent_handle, turn_id, completed_at)
		 VALUES ('', 'ceo', 't0', 0)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := ledgerstore.NewCompletions(d)
	if s.Worked(context.Background(), "ceo", []string{""})[""] {
		t.Error("a stored empty key read as worked")
	}
}
