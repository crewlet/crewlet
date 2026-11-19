package advisordomain_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/iam"
)

// sha is the digest of s, as a verdict or a fix is named.
func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// The keys the cases talk about.
const (
	idleKey    = "seats.idle_with_queue:seat:sre"
	starvedKey = "work.urgent_starved:item:42"
	poolKey    = "store.pool_waits:node:data-1"
)

// opening is an episode of key at severity, first observed at observed.
func opening(key string, severity advisordomain.Severity, observed time.Time) advisordomain.Opening {
	class, _, _ := strings.Cut(key, ":")
	return advisordomain.Opening{Episode: advisordomain.EpisodeID(key, observed), Key: key, Class: class,
		Source: advisordomain.SourceRule, Actor: advisordomain.ActorOwner, Area: "capacity",
		Severity: severity, SubjectKind: "seat", SubjectID: "sre", Revision: 1,
		VerdictDigest: sha("verdict 1"), FixDigest: sha("fix 1"),
		Body: json.RawMessage(`{"title":"An idle seat with work waiting"}`)}
}

// closing is an engine's close of episode as resolved.
func closing(episode string) advisordomain.Move {
	return advisordomain.Move{Episode: episode, Event: advisordomain.EventClosed,
		State: advisordomain.StateResolved, Reason: advisordomain.ReasonConditionCleared}
}

// dismissal is a person's dismissal of episode, holding its key at severity
// until until (zero: for good).
func dismissal(episode string, severity advisordomain.Severity, until time.Time) advisordomain.Move {
	return advisordomain.Move{Episode: episode, Event: advisordomain.EventClosed,
		State: advisordomain.StateDismissed, Reason: advisordomain.ReasonDismissedWontFix,
		Hold: &advisordomain.Hold{Until: until, Reason: "wont_fix", Severity: severity}}
}

// A FINDING APPLIES ONCE ON EVERY NODE: two data nodes on one log hold
// byte-identical rows, and a redelivery of every record changes none.
func TestAFindingAppliesOnceOnEveryNode(t *testing.T) {
	t.Parallel()
	log := openAdvisorLog(t)
	a, b := newWriteNode(t, log, "data-1"), newWriteNode(t, log, "data-2")
	o := opening(idleKey, advisordomain.SeverityCosts, writerClock)
	a.open(o)
	b.drain()
	b.move(personCall("ada"), idleKey, advisordomain.Move{Episode: o.Episode,
		Event: advisordomain.EventSnoozed, State: advisordomain.StateOpen, SnoozedUntil: writerClock.Add(7 * 24 * time.Hour)})
	if _, err := a.writer.RecordRun(t.Context(), engineCall(), advisordomain.RunRecord{
		Run: "0b6e3c52-7d1a-4f8e-9c2b-5a4d3e2f1a0b", Trigger: advisordomain.TriggerSchedule,
		StartedAt: writerClock, EndedAt: writerClock.Add(time.Minute), Status: advisordomain.RunDone,
		Classes: json.RawMessage(`[{"class":"seats.idle_with_queue","status":"done","observed":1}]`),
	}); err != nil {
		t.Fatalf("record a run: %v", err)
	}
	a.drain()
	b.drain()
	equal := func(x, y map[string][]string) bool {
		return maps.EqualFunc(x, y, func(p, q []string) bool { return slices.Equal(p, q) })
	}
	if got, want := a.dump(), b.dump(); !equal(got, want) {
		t.Fatalf("two nodes on one log hold different rows:\n%v\n%v", got, want)
	}
	before := a.dump()
	a.redeliver()
	if after := a.dump(); !equal(before, after) {
		t.Errorf("a redelivery changed the rows:\n%v\n%v", before, after)
	}
	row, lines := a.finding(o.Episode)
	if row.SnoozedUntil.IsZero() || len(lines) != 2 || lines[1].ByName != "ada" ||
		lines[1].ByKind != iam.ActorHuman {
		t.Errorf("the snooze landed as %+v with history %+v, want ada's snooze", row, lines)
	}
}

// ONE KEY HAS AT MOST ONE NON-CLOSED EPISODE: a second opening while one is
// open is refused, and a condition that returns after a close is a new
// episode that says how often it was seen before.
func TestAKeyHoldsOneNonClosedEpisodeAtATime(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAdvisorLog(t), "data-1")
	first := opening(idleKey, advisordomain.SeverityCosts, writerClock)
	n.open(first)
	again := opening(idleKey, advisordomain.SeverityCosts, writerClock.Add(time.Hour))
	if _, err := n.writer.Open(t.Context(), engineCall(), again, advisordomain.NoIfMatch); !errors.Is(err, advisordomain.ErrExists) {
		t.Fatalf("a second episode beside an open one: %v, want ErrExists", err)
	}
	n.move(engineCall(), idleKey, closing(first.Episode))
	n.open(again)
	episodes, err := n.reader().Episodes(t.Context(), idleKey, stale)
	if err != nil {
		t.Fatalf("read the episodes: %v", err)
	}
	if len(episodes) != 2 || episodes[0].ID != again.Episode || episodes[0].SeenBefore != 1 ||
		episodes[1].State != advisordomain.StateResolved {
		t.Fatalf("the key's episodes are %+v, want the new one seen once before over the resolved one", episodes)
	}
	if k := n.key(idleKey); k.Episodes != 2 || k.Current != again.Episode {
		t.Errorf("the key reads %+v, want two episodes and the new one current", k)
	}
}

// A WRITE DECIDED FROM A KEY THAT MOVED SINCE IS REFUSED, and one decided where
// the key held nothing is refused once it holds something.
func TestAWriteDecidedFromAMovedKeyIsRefused(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAdvisorLog(t), "data-1")
	o := opening(idleKey, advisordomain.SeverityCosts, writerClock)
	if _, err := n.writer.Open(t.Context(), engineCall(), o, advisordomain.IfNoRecord); err != nil {
		t.Fatalf("open on a key never recorded: %v", err)
	}
	read := n.key(idleKey).Version
	if _, err := n.writer.Refresh(t.Context(), engineCall(), idleKey, advisordomain.Refresh{
		Episode: o.Episode, Revision: 1, Body: json.RawMessage(`{"title":"refreshed"}`)}, read); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	snooze := advisordomain.Move{Episode: o.Episode, Event: advisordomain.EventSnoozed,
		State: advisordomain.StateOpen, SnoozedUntil: writerClock.Add(time.Hour)}
	if _, err := n.writer.Move(t.Context(), personCall("ada"), idleKey, snooze, read); !errors.Is(err, advisordomain.ErrStaleVersion) {
		t.Errorf("a snooze decided before the refresh: %v, want ErrStaleVersion", err)
	}
	reopen := opening(idleKey, advisordomain.SeverityCosts, writerClock.Add(time.Hour))
	if _, err := n.writer.Open(t.Context(), engineCall(), reopen, advisordomain.IfNoRecord); !errors.Is(err, advisordomain.ErrStaleVersion) {
		t.Errorf("an opening decided where the key held nothing: %v, want ErrStaleVersion", err)
	}
	if _, err := n.writer.Move(t.Context(), personCall("ada"), idleKey, snooze, n.key(idleKey).Version); err != nil {
		t.Errorf("a snooze decided from the current version: %v", err)
	}
}

// A DISMISSAL HOLDS ITS KEY until the hold lapses or a graver episode comes,
// and a person's release lifts it, in the dismissed episode's history.
//
// Mutation: drop the severity comparison in keyState.holds and the graver
// opening is refused.
func TestADismissalHoldsItsKeyUntilItLapsesOrSeverityRises(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAdvisorLog(t), "data-1")
	first := opening(idleKey, advisordomain.SeverityCosts, writerClock)
	n.open(first)
	n.move(personCall("ada"), idleKey, dismissal(first.Episode, advisordomain.SeverityCosts, writerClock.Add(time.Hour)))
	for _, severity := range []advisordomain.Severity{advisordomain.SeverityNote, advisordomain.SeverityCosts} {
		o := opening(idleKey, severity, writerClock.Add(time.Minute))
		if _, err := n.writer.Open(t.Context(), engineCall(), o, advisordomain.NoIfMatch); !errors.Is(err, advisordomain.ErrHeld) {
			t.Errorf("a %s episode under a costs hold: %v, want ErrHeld", severity, err)
		}
	}
	graver := opening(idleKey, advisordomain.SeverityBlocks, writerClock.Add(time.Minute))
	n.open(graver)
	n.move(engineCall(), idleKey, closing(graver.Episode))
	n.advance(2 * time.Hour)
	n.open(opening(idleKey, advisordomain.SeverityCosts, writerClock.Add(2*time.Hour)))

	other := opening(starvedKey, advisordomain.SeverityCosts, writerClock)
	n.open(other)
	if _, err := n.writer.Release(t.Context(), personCall("ada"), starvedKey, advisordomain.NoIfMatch); !errors.Is(err, advisordomain.ErrIllegalMove) {
		t.Errorf("a release of a key with no hold: %v, want ErrIllegalMove", err)
	}
	n.move(personCall("ada"), starvedKey, dismissal(other.Episode, advisordomain.SeverityCosts, time.Time{}))
	if k := n.key(starvedKey); !k.HoldForGood || k.HoldEpisode != other.Episode {
		t.Fatalf("the key reads %+v, want a hold for good set by the dismissal", k)
	}
	if _, err := n.writer.Release(t.Context(), personCall("ada"), starvedKey, n.key(starvedKey).Version); err != nil {
		t.Fatalf("release: %v", err)
	}
	n.open(opening(starvedKey, advisordomain.SeverityNote, writerClock.Add(time.Hour)))
	_, lines := n.finding(other.Episode)
	if last := lines[len(lines)-1]; last.Event != advisordomain.EventReleased || last.ByName != "ada" {
		t.Errorf("the dismissed episode's history ends %+v, want ada's release", last)
	}
}

// ONLY THE LEGAL MOVES LAND: an applied fix is the only way to applied, an
// ineffective one the only way back (and its digest is kept on the key), and
// closed is terminal.
func TestOnlyTheLegalMovesLand(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAdvisorLog(t), "data-1")
	o := opening(idleKey, advisordomain.SeverityCosts, writerClock)
	n.open(o)
	fix := sha("fix 1")
	applied := advisordomain.Move{Episode: o.Episode, Event: advisordomain.EventApplied,
		State: advisordomain.StateApplied, FixDigest: fix, EffectDueAt: writerClock.Add(72 * time.Hour)}
	ineffective := advisordomain.Move{Episode: o.Episode, Event: advisordomain.EventIneffective,
		State: advisordomain.StateOpen, FixDigest: fix}
	snooze := advisordomain.Move{Episode: o.Episode, Event: advisordomain.EventSnoozed,
		State: advisordomain.StateOpen, SnoozedUntil: writerClock.Add(time.Hour)}
	illegal := func(label string, m advisordomain.Move) {
		t.Helper()
		if _, err := n.writer.Move(t.Context(), personCall("ada"), idleKey, m, advisordomain.NoIfMatch); !errors.Is(err, advisordomain.ErrIllegalMove) {
			t.Errorf("%s: %v, want ErrIllegalMove", label, err)
		}
	}
	illegal("ineffective while open", ineffective)
	n.move(personCall("ada"), idleKey, applied)
	illegal("applied twice", applied)
	illegal("snoozed while applied", snooze)
	n.move(engineCall(), idleKey, ineffective)
	if row, _ := n.finding(o.Episode); row.State != advisordomain.StateOpen || row.IneffectiveFix != fix ||
		row.AppliedBy != "ada" || !row.EffectDueAt.IsZero() {
		t.Errorf("after an ineffective fix the episode reads %+v", row)
	}
	n.move(engineCall(), idleKey, closing(o.Episode))
	illegal("snoozed once closed", snooze)
	illegal("closed twice", closing(o.Episode))
	if _, err := n.writer.Revise(t.Context(), engineCall(), idleKey, advisordomain.Revision{
		Episode: o.Episode, Revision: 2, Severity: advisordomain.SeverityCosts, VerdictDigest: sha("verdict 2"),
		FixDigest: fix, Body: json.RawMessage(`{}`)}, advisordomain.NoIfMatch); !errors.Is(err, advisordomain.ErrIllegalMove) {
		t.Errorf("a revision of a closed episode: %v, want ErrIllegalMove", err)
	}
	if k := n.key(idleKey); !slices.Equal(k.Ineffective, []string{fix}) || k.Current != "" {
		t.Errorf("the key reads %+v, want the ineffective fix kept and no current episode", k)
	}
}

// A REVISION BUMPS AND SAYS WHAT MOVED; A REFRESH DOES NEITHER, and a refresh
// of a revision the episode has moved past writes nothing.
func TestARevisionBumpsAndARefreshDoesNot(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAdvisorLog(t), "data-1")
	o := opening(idleKey, advisordomain.SeverityCosts, writerClock)
	n.open(o)
	refresh := func(revision int, title string) error {
		_, err := n.writer.Refresh(t.Context(), engineCall(), idleKey, advisordomain.Refresh{Episode: o.Episode,
			Revision: revision, Body: json.RawMessage(`{"title":"` + title + `"}`)}, n.key(idleKey).Version)
		return err
	}
	if err := refresh(1, "refreshed"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	moved := json.RawMessage(`[{"key":"wait_h","label":"Hours waiting","was":4,"now":9}]`)
	revise := advisordomain.Revision{Episode: o.Episode, Revision: 2, Severity: advisordomain.SeverityBlocks,
		VerdictDigest: sha("verdict 2"), FixDigest: o.FixDigest, Body: json.RawMessage(`{"title":"revised"}`),
		Moved: moved}
	if _, err := n.writer.Revise(t.Context(), engineCall(), idleKey, revise, n.key(idleKey).Version); err != nil {
		t.Fatalf("revise: %v", err)
	}
	end, _ := n.log.End(t.Context())
	if err := refresh(1, "stale"); err != nil {
		t.Fatalf("a refresh of a past revision: %v, want nothing recorded and no refusal", err)
	}
	if after, _ := n.log.End(t.Context()); after != end {
		t.Errorf("a refresh of a past revision published a record")
	}
	if _, err := n.writer.Revise(t.Context(), engineCall(), idleKey, revise, advisordomain.NoIfMatch); !errors.Is(err, advisordomain.ErrStaleVersion) {
		t.Errorf("revision 2 twice: %v, want ErrStaleVersion", err)
	}
	row, lines := n.finding(o.Episode)
	if row.Revision != 2 || row.Severity != advisordomain.SeverityBlocks || string(row.Body) != `{"title":"revised"}` {
		t.Errorf("the episode reads %+v, want revision 2 as revised", row)
	}
	if len(lines) != 2 || lines[1].Event != advisordomain.EventRevised || string(lines[1].Moved) != string(moved) {
		t.Errorf("the history is %+v, want the opening and the revision with what moved", lines)
	}
}

// AN EXPLANATION IS BOUND TO ITS VERDICT: a revision leaves it naming the old
// verdict, and an explanation of a verdict that moved is refused.
func TestAnExplanationIsBoundToItsVerdict(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAdvisorLog(t), "data-1")
	o := opening(idleKey, advisordomain.SeverityCosts, writerClock)
	n.open(o)
	explain := advisordomain.Explanation{Episode: o.Episode, VerdictDigest: o.VerdictDigest,
		Text: "The seat sat idle for nine hours while two items waited for it."}
	if _, err := n.writer.Explain(t.Context(), engineCall(), idleKey, explain); err != nil {
		t.Fatalf("explain: %v", err)
	}
	if _, err := n.writer.Revise(t.Context(), engineCall(), idleKey, advisordomain.Revision{Episode: o.Episode,
		Revision: 2, Severity: advisordomain.SeverityCosts, VerdictDigest: sha("verdict 2"), FixDigest: o.FixDigest,
		Body: json.RawMessage(`{}`)}, advisordomain.NoIfMatch); err != nil {
		t.Fatalf("revise: %v", err)
	}
	if _, err := n.writer.Explain(t.Context(), engineCall(), idleKey, explain); !errors.Is(err, advisordomain.ErrStaleVersion) {
		t.Errorf("an explanation of a verdict that moved: %v, want ErrStaleVersion", err)
	}
	if row, _ := n.finding(o.Episode); row.ExplainedDigest == row.VerdictDigest || row.Explanation != explain.Text {
		t.Errorf("the episode reads %q for %s at verdict %s, want the old verdict's words", row.Explanation,
			row.ExplainedDigest, row.VerdictDigest)
	}
}

// A RUN IS REQUESTED AND RECORDED ONCE, and only a class that ran to done
// moves its last run.
func TestARunIsRequestedAndRecordedOnce(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAdvisorLog(t), "data-1")
	const run = "4c7d2e91-3b5a-4f60-8e1d-9a2b7c6d5e4f"
	if _, err := n.writer.RequestRun(t.Context(), personCall("ada"), advisordomain.RunRequest{Run: run, By: "ada"}); err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := n.writer.RequestRun(t.Context(), personCall("ada"), advisordomain.RunRequest{Run: run, By: "ada"}); !errors.Is(err, advisordomain.ErrExists) {
		t.Errorf("a second request of one run: %v, want ErrExists", err)
	}
	pending, err := n.reader().PendingRequests(t.Context(), stale)
	if err != nil || len(pending) != 1 || pending[0].ID != run || pending[0].By != "ada" {
		t.Fatalf("the pending requests are %+v (%v), want ada's", pending, err)
	}
	record := advisordomain.RunRecord{Run: run, Trigger: advisordomain.TriggerPerson, By: "ada",
		StartedAt: writerClock, EndedAt: writerClock.Add(time.Minute), Status: advisordomain.RunDone,
		Classes: json.RawMessage(`[{"class":"seats.idle_with_queue","status":"done"},` +
			`{"class":"capacity.add_seat","status":"unread"}]`)}
	if _, err := n.writer.RecordRun(t.Context(), engineCall(), record); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := n.writer.RecordRun(t.Context(), engineCall(), record); !errors.Is(err, advisordomain.ErrExists) {
		t.Errorf("a second record of one run: %v, want ErrExists", err)
	}
	r := n.reader()
	if pending, err := r.PendingRequests(t.Context(), stale); err != nil || len(pending) != 0 {
		t.Errorf("after its record the pending requests are %+v (%v)", pending, err)
	}
	latest, found, err := r.LatestRun(t.Context(), stale)
	if err != nil || !found || latest.ID != run || latest.Status != advisordomain.RunDone ||
		latest.RequestedAt.IsZero() {
		t.Errorf("the latest run is %+v (%v, %v), want the requested run, done", latest, found, err)
	}
	last, err := r.LastRunOfClasses(t.Context(), stale)
	if err != nil || len(last) != 1 || !last["seats.idle_with_queue"].Equal(record.EndedAt) {
		t.Errorf("the classes' last runs are %v (%v), want only the class that ran to done", last, err)
	}
}

// A WEEK'S REPORT IS WRITTEN ONCE, and the reports page newest week first.
func TestAWeeksReportIsWrittenOnce(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAdvisorLog(t), "data-1")
	for i := range 3 {
		to := writerClock.Add(time.Duration(i) * 7 * 24 * time.Hour)
		report := advisordomain.Report{Week: advisordomain.Week(to), From: to.Add(-7 * 24 * time.Hour), To: to,
			Body: json.RawMessage(`{"opened":3}`)}
		if _, err := n.writer.WriteReport(t.Context(), engineCall(), report); err != nil {
			t.Fatalf("write %s: %v", report.Week, err)
		}
		if i == 0 {
			if _, err := n.writer.WriteReport(t.Context(), engineCall(), report); !errors.Is(err, advisordomain.ErrExists) {
				t.Errorf("a week written twice: %v, want ErrExists", err)
			}
		}
	}
	r := n.reader()
	page, next, err := r.Reports(t.Context(), "", 2, stale)
	if err != nil || len(page) != 2 || next == "" || page[0].Week <= page[1].Week {
		t.Fatalf("the first page is %+v next %q (%v), want two weeks newest first", page, next, err)
	}
	rest, last, err := r.Reports(t.Context(), next, 2, stale)
	if err != nil || len(rest) != 1 || last != "" || rest[0].Week != advisordomain.Week(writerClock) {
		t.Errorf("the second page is %+v next %q (%v), want the oldest week alone", rest, last, err)
	}
}

// A WEEK'S TALLY COUNTS WHAT HAPPENED IN IT: the episodes it opened, resolved,
// applied and dismissed, each by when that happened, so a period before them
// counts none. Mutation: count by state alone, and the earlier period counts them.
func TestAWeeksTallyCountsWhatHappenedInIt(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAdvisorLog(t), "data-1")
	from := time.Now().Add(-time.Minute)
	var ids []string
	for _, key := range []string{idleKey, starvedKey, poolKey} {
		o := opening(key, advisordomain.SeverityCosts, writerClock)
		n.open(o)
		ids = append(ids, o.Episode)
	}
	n.move(personCall("ada"), idleKey, advisordomain.Move{Episode: ids[0], Event: advisordomain.EventApplied,
		State: advisordomain.StateApplied, FixDigest: sha("fix 1"), EffectDueAt: writerClock.Add(time.Hour)})
	n.move(personCall("ada"), starvedKey, dismissal(ids[1], advisordomain.SeverityCosts, time.Time{}))
	n.move(engineCall(), poolKey, closing(ids[2]))
	to := time.Now().Add(time.Minute)
	r := n.reader()
	got, err := r.Tally(t.Context(), from, to, stale)
	if want := (advisordomain.Tally{Opened: 3, Resolved: 1, Applied: 1, Dismissed: 1}); err != nil || got != want {
		t.Errorf("the week's tally is %+v (%v), want %+v", got, err, want)
	}
	if got, err := r.Tally(t.Context(), from.Add(-7*24*time.Hour), from, stale); err != nil || got != (advisordomain.Tally{}) {
		t.Errorf("the week before counts %+v (%v), want nothing", got, err)
	}
	if _, err := r.Tally(t.Context(), to, from, stale); !errors.Is(err, advisordomain.ErrInvalid) {
		t.Errorf("a period ending before it starts: %v, want ErrInvalid", err)
	}
}

// AN ALARM SERIES HOLDS ONE OPEN EPISODE: a raise while one is open and a clear
// while none is publish nothing.
func TestAnAlarmSeriesHoldsOneOpenEpisode(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAdvisorLog(t), "data-1")
	raise := advisordomain.AlarmRaise{Node: "data-1", Kind: "pool_starved", Instance: "replicated", RaisedAt: writerClock}
	clearAt := advisordomain.AlarmClear{ClearedAt: writerClock.Add(5 * time.Minute)}
	step := func(label string, write func() error, records uint64) {
		t.Helper()
		before, _ := n.log.End(t.Context())
		if err := write(); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if after, _ := n.log.End(t.Context()); after-before != records {
			t.Errorf("%s published %d records, want %d", label, after-before, records)
		}
	}
	raiseIt := func() error { _, err := n.writer.RaiseAlarm(t.Context(), engineCall(), raise); return err }
	clearIt := func() error {
		_, err := n.writer.ClearAlarm(t.Context(), engineCall(), raise.Node, raise.Kind, raise.Instance, clearAt)
		return err
	}
	step("the first raise", raiseIt, 1)
	step("a raise while open", raiseIt, 0)
	step("the clear", clearIt, 1)
	step("a clear while none is open", clearIt, 0)
	step("the next raise", raiseIt, 1)
	episodes, err := n.reader().AlarmEpisodes(t.Context(), writerClock, stale)
	if err != nil || len(episodes) != 2 || episodes[0].ClearedAt.IsZero() || !episodes[1].ClearedAt.IsZero() {
		t.Errorf("the episodes are %+v (%v), want one cleared and one open", episodes, err)
	}
}

// AN AGENT WRITES NOTHING TO THE ADVISOR: what it proposes, a person approves.
func TestAnAgentWritesNothingToTheAdvisor(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAdvisorLog(t), "data-1")
	agent := personCall("sre")
	agent.By.Kind = iam.ActorAgent
	if _, err := n.writer.Open(t.Context(), agent, opening(idleKey, advisordomain.SeverityCosts, writerClock),
		advisordomain.NoIfMatch); !errors.Is(err, advisordomain.ErrInvalid) {
		t.Errorf("an agent's opening: %v, want ErrInvalid", err)
	}
	if _, err := n.writer.RequestRun(t.Context(), agent, advisordomain.RunRequest{
		Run: "4c7d2e91-3b5a-4f60-8e1d-9a2b7c6d5e4f", By: "sre"}); !errors.Is(err, advisordomain.ErrInvalid) {
		t.Errorf("an agent's run request: %v, want ErrInvalid", err)
	}
}

// THE WRITER REFUSES WHAT THE LOG CANNOT KEEP, naming the field, and publishes
// nothing: the applier judges no text, so the writer is the only place a value
// can be stopped.
func TestTheWriterRefusesWhatTheLogCannotKeep(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAdvisorLog(t), "data-1")
	base := opening(idleKey, advisordomain.SeverityCosts, writerClock)
	openings := map[string]func(o *advisordomain.Opening){
		"a key with no class":          func(o *advisordomain.Opening) { o.Key = "seat:sre" },
		"a key of another class":       func(o *advisordomain.Opening) { o.Class = "capacity.add_seat" },
		"a key past its bound":         func(o *advisordomain.Opening) { o.Key += strings.Repeat("x", advisordomain.MaxKeyBytes) },
		"an unknown severity":          func(o *advisordomain.Opening) { o.Severity = "urgent" },
		"a verdict not a digest":       func(o *advisordomain.Opening) { o.VerdictDigest = "sha256:abc" },
		"a body that is not an object": func(o *advisordomain.Opening) { o.Body = json.RawMessage(`["title"]`) },
		"a body past its bound": func(o *advisordomain.Opening) {
			o.Body = json.RawMessage(`{"why":"` + strings.Repeat("x", advisordomain.MaxBodyBytes) + `"}`)
		},
		"a second revision":   func(o *advisordomain.Opening) { o.Revision = 2 },
		"a negative impact":   func(o *advisordomain.Opening) { o.ImpactMinutes = new(int64(-5)) },
		"an episode no uuid":  func(o *advisordomain.Opening) { o.Episode = "episode-1" },
		"a control character": func(o *advisordomain.Opening) { o.Area = "capa\ncity" },
	}
	for label, edit := range openings {
		o := base
		edit(&o)
		if _, err := n.writer.Open(t.Context(), engineCall(), o, advisordomain.NoIfMatch); !errors.Is(err, advisordomain.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", label, err)
		}
	}
	moves := map[string]advisordomain.Move{
		"a close with no reason": {Episode: base.Episode, Event: advisordomain.EventClosed,
			State: advisordomain.StateResolved},
		"a reason of another state": {Episode: base.Episode, Event: advisordomain.EventClosed,
			State: advisordomain.StateResolved, Reason: advisordomain.ReasonDismissedWrong},
		"a hold on a resolution": {Episode: base.Episode, Event: advisordomain.EventClosed,
			State: advisordomain.StateResolved, Reason: advisordomain.ReasonConditionCleared,
			Hold: &advisordomain.Hold{Severity: advisordomain.SeverityCosts}},
		"an applied fix with no effect check": {Episode: base.Episode, Event: advisordomain.EventApplied,
			State: advisordomain.StateApplied, FixDigest: base.FixDigest},
		"a snooze that never ends": {Episode: base.Episode, Event: advisordomain.EventSnoozed,
			State: advisordomain.StateOpen},
	}
	for label, m := range moves {
		if _, err := n.writer.Move(t.Context(), personCall("ada"), idleKey, m, advisordomain.NoIfMatch); !errors.Is(err, advisordomain.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", label, err)
		}
	}
	day, expiry := advisordomain.ExpiryFor(writerClock)
	expiry.ClosedBefore = writerClock.Add(-time.Hour)
	if _, err := n.writer.Expire(t.Context(), engineCall(), day, expiry); !errors.Is(err, advisordomain.ErrInvalid) {
		t.Errorf("an expiry reaching inside the retention: %v, want ErrInvalid", err)
	}
	if _, err := n.writer.WriteReport(t.Context(), engineCall(), advisordomain.Report{Week: "2031-W54",
		From: writerClock, To: writerClock.Add(time.Hour), Body: json.RawMessage(`{}`)}); !errors.Is(err, advisordomain.ErrInvalid) {
		t.Errorf("a week no year has: %v, want ErrInvalid", err)
	}
	if end, _ := n.log.End(t.Context()); end != 0 {
		t.Errorf("the log holds %d records after refusals", end)
	}
}

// THE FINDINGS PAGE NEWEST FIRST, narrowed by state and severity, and the
// counts are the open ones by severity.
func TestTheFindingsPageNewestFirstByEveryFilter(t *testing.T) {
	t.Parallel()
	n := newWriteNode(t, openAdvisorLog(t), "data-1")
	keys := []struct {
		key      string
		severity advisordomain.Severity
	}{{idleKey, advisordomain.SeverityCosts}, {starvedKey, advisordomain.SeverityBlocks}, {poolKey, advisordomain.SeverityCosts}}
	var ids []string
	for _, k := range keys {
		o := opening(k.key, k.severity, writerClock)
		n.open(o)
		ids = append(ids, o.Episode)
	}
	n.move(engineCall(), poolKey, closing(ids[2]))
	r := n.reader()
	page, next, err := r.Findings(t.Context(), advisordomain.Filter{Limit: 2}, stale)
	if err != nil || len(page) != 2 || next == "" || page[0].ID != ids[2] || page[1].ID != ids[1] {
		t.Fatalf("the first page is %+v next %q (%v), want the two newest", page, next, err)
	}
	rest, last, err := r.Findings(t.Context(), advisordomain.Filter{Limit: 2, Cursor: next}, stale)
	if err != nil || len(rest) != 1 || last != "" || rest[0].ID != ids[0] {
		t.Errorf("the second page is %+v next %q (%v), want the oldest alone", rest, last, err)
	}
	open, _, err := r.Findings(t.Context(), advisordomain.Filter{Open: true, Severity: advisordomain.SeverityCosts}, stale)
	if err != nil || len(open) != 1 || open[0].ID != ids[0] {
		t.Errorf("the open costs are %+v (%v), want the idle seat alone", open, err)
	}
	closed, _, err := r.Findings(t.Context(), advisordomain.Filter{Closed: true}, stale)
	if err != nil || len(closed) != 1 || closed[0].ID != ids[2] {
		t.Errorf("the closed are %+v (%v), want the pool's alone", closed, err)
	}
	counts, err := r.Counts(t.Context(), stale)
	want := map[advisordomain.Severity]int{advisordomain.SeverityCosts: 1, advisordomain.SeverityBlocks: 1}
	if err != nil || !maps.Equal(counts, want) {
		t.Errorf("the counts are %v (%v), want %v", counts, err, want)
	}
	if _, _, err := r.Findings(t.Context(), advisordomain.Filter{Limit: advisordomain.MaxPageSize + 1}, stale); !errors.Is(err, advisordomain.ErrInvalid) {
		t.Errorf("a page past the bound: %v, want ErrInvalid", err)
	}
}
