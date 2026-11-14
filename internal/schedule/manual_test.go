package schedule

// A run a person asks for: its own identity in the ledger, the tick's
// dispatch, and an answer for each runner.

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// manualRig is a Manual over the same fakes a tick's harness uses, so a case
// can put an ask and a tick in one ledger.
type manualRig struct {
	h *harness
	m *Manual
}

func manualOver(t *testing.T, o *org.Organization, apply ...func(*ManualOptions)) manualRig {
	t.Helper()
	h := build(t, o)
	opts := ManualOptions{Publisher: h.q, Ledger: h.ledger}
	for _, fn := range apply {
		fn(&opts)
	}
	m, err := NewManual(opts)
	if err != nil {
		t.Fatalf("NewManual: %v", err)
	}
	return manualRig{h: h, m: m}
}

func (r manualRig) ask(t *testing.T, scope types.ScheduleScope, ref, name, ask string) ([]ManualRun, error) {
	t.Helper()
	e, ok := Find(r.h.org, scope, ref, name)
	if !ok {
		t.Fatalf("Find(%s, %s, %s) found nothing", scope, ref, name)
	}
	return r.m.Fire(t.Context(), ManualAsk{Company: r.h.org, Entry: e, Ask: ask,
		RequestedBy: "ada", At: tickAt(9, 0, 20)})
}

func TestNewManualRefusesAnUnusableWiring(t *testing.T) {
	t.Parallel()
	if _, err := NewManual(ManualOptions{Ledger: NewMemoryLedger()}); !errors.Is(err, ErrNoPublisher) {
		t.Fatalf("no publisher: err = %v, want ErrNoPublisher", err)
	}
	if _, err := NewManual(ManualOptions{Publisher: newQueue()}); !errors.Is(err, ErrNoLedger) {
		t.Fatalf("no ledger: err = %v, want ErrNoLedger", err)
	}
}

func TestAnAskFiresTheScheduleNowAndSaysWhoAsked(t *testing.T) {
	t.Parallel()
	r := manualOver(t, roleOrg())
	runs, err := r.ask(t, types.ScheduleScopeRole, "qa", "smoke", "op-1")
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if len(runs) != 1 || runs[0].Status != ManualFired || runs[0].Handle != "qa" ||
		runs[0].TraceID == "" || runs[0].RunID == "" {
		t.Fatalf("runs = %+v, want one fired run for qa with its trace and run id", runs)
	}
	tasks := r.h.q.inboxTasks()
	if len(tasks) != 1 || tasks[0].topic != seatInbox("qa") {
		t.Fatalf("inbox tasks = %v, want one on the seat's inbox", r.h.q.inboxTopics())
	}
	task, _ := events.DataAs[*types.TaskAssigned](tasks[0].ev)
	if task.Schedule != "smoke" || task.Description != "run smoke tests" || task.RequestedBy != "ada" ||
		!task.ScheduledAt.Equal(tickAt(9, 0, 20)) {
		t.Fatalf("task = %+v, want the schedule's task, asked by ada, due at the ask", task)
	}
	if !strings.Contains(task.Brief(), "ada asked for this run now") {
		t.Fatalf("the seat is not told a person asked for the run:\n%s", task.Brief())
	}
	// THE SCHEDULE'S OWN CONVERSATION, so the run continues the last fire's
	// session as a tick's does.
	e, _ := Find(r.h.org, types.ScheduleScopeRole, "qa", "smoke")
	if got := notify.KeyOf(tasks[0].ev); got != FirePartition(e) {
		t.Fatalf("partition = %q, want the schedule's conversation %q", got, FirePartition(e))
	}
	rows := r.h.ledger.rows(t)
	// RECORDED AS A RUN A PERSON ASKED FOR, under the ask's own label, so
	// the schedule's history tells it from a tick.
	if len(rows) != 1 || rows[0].Outcome != OutcomeFiredManually ||
		rows[0].FireLabel != ManualFireLabel("op-1") || rows[0].TraceID != runs[0].TraceID {
		t.Fatalf("ledger = %+v, want one fired_manually row under the ask's label and the run's trace", rows)
	}
}

func TestTheSameAskMadeAgainIsNotSentTwice(t *testing.T) {
	t.Parallel()
	r := manualOver(t, roleOrg())
	if _, err := r.ask(t, types.ScheduleScopeRole, "qa", "smoke", "op-1"); err != nil {
		t.Fatalf("Fire: %v", err)
	}
	again, err := r.ask(t, types.ScheduleScopeRole, "qa", "smoke", "op-1")
	if err != nil {
		t.Fatalf("Fire again: %v", err)
	}
	if len(again) != 1 || again[0].Status != ManualAlreadyFired {
		t.Fatalf("the same ask again = %+v, want already_fired", again)
	}
	if got := len(r.h.q.inboxTasks()); got != 1 {
		t.Fatalf("inbox tasks = %d, want 1: a retried ask is the same run", got)
	}
	// ANOTHER ASK IS ANOTHER RUN, in the same minute too.
	other, err := r.ask(t, types.ScheduleScopeRole, "qa", "smoke", "op-2")
	if err != nil || other[0].Status != ManualFired {
		t.Fatalf("another ask = %+v, %v, want fired", other, err)
	}
}

// The property the label exists for: an ask and the cron's own tick of the
// same minute are two fires, in either order, and the catchup pass's question
// about a minute is not answered by an ask.
func TestAnAskNeitherTakesNorIsRefusedByTheTicksClaim(t *testing.T) {
	t.Parallel()
	r := manualOver(t, roleOrg())
	if runs, err := r.ask(t, types.ScheduleScopeRole, "qa", "smoke", "op-1"); err != nil || runs[0].Status != ManualFired {
		t.Fatalf("ask = %+v, %v", runs, err)
	}
	r.h.seed(tickAt(8, 59, 30))
	if got := r.h.tick(tickAt(9, 0, 30)); got != 1 {
		t.Fatalf("the 09:00 tick fired %d after an ask at 09:00, want 1", got)
	}
	if runs, err := r.ask(t, types.ScheduleScopeRole, "qa", "smoke", "op-2"); err != nil || runs[0].Status != ManualFired {
		t.Fatalf("ask after the tick = %+v, %v, want fired", runs, err)
	}
	e, _ := Find(r.h.org, types.ScheduleScopeRole, "qa", "smoke")
	held, err := r.h.ledger.MinuteClaimed(t.Context(), FireMinute{Scope: e.Scope, ScopeID: e.ScopeID,
		ScheduleName: "smoke", FireLabel: FireLabel(tickAt(10, 0, 0), mustZone(t, "UTC"))})
	if err != nil || held {
		t.Fatalf("MinuteClaimed(10:00) = %v, %v: an ask must not hold a minute it never stood for", held, err)
	}
}

func TestAnAskFansOutLikeATickAndAnswersEachRunner(t *testing.T) {
	t.Parallel()
	o := unitOrg(org.TargetEach)
	dev := o.AgentSeatByHandle("qa-dev")
	devID, _ := o.AgentIDFor(dev)
	r := manualOver(t, o, func(opts *ManualOptions) {
		opts.Paused = func(seat uuid.UUID) (bool, error) { return seat == devID, nil }
	})
	runs, err := r.ask(t, types.ScheduleScopeUnit, o.Units[0].Key(), "standup", "op-1")
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	got := map[string]ManualStatus{}
	for _, run := range runs {
		got[run.Handle] = run.Status
	}
	if len(runs) != 2 || got["qa-lead"] != ManualFired || got["qa-dev"] != ManualPaused {
		t.Fatalf("runs = %+v, want the lead fired and the paused member not", runs)
	}
	// A PAUSED SEAT'S ASK LEAVES NO ROW: a person was told, and may ask again.
	if rows := r.h.ledger.rows(t); len(rows) != 1 || rows[0].TargetHandle != "qa-lead" {
		t.Fatalf("ledger = %+v, want the one fired row", rows)
	}
}

// A SCHEDULE SWITCHED OFF IS NOT RUN BY HAND EITHER: switching it off is
// somebody's decision that it does not run, and nothing is claimed or sent.
//
// Mutation: drop the enabled check, and the ask fires the switched-off
// schedule.
func TestAnAskForADisabledScheduleIsRefused(t *testing.T) {
	t.Parallel()
	r := manualOver(t, roleOrg(org.Schedule{Name: "smoke", Cron: "0 9 * * *", Task: "run smoke tests", Enabled: org.Off()}))
	runs, err := r.ask(t, types.ScheduleScopeRole, "qa", "smoke", "op-1")
	if !errors.Is(err, ErrScheduleDisabled) || runs != nil {
		t.Fatalf("ask of a disabled schedule = %+v, %v, want ErrScheduleDisabled", runs, err)
	}
	if n := r.h.ledger.count(); n != 0 || len(r.h.q.inboxTasks()) != 0 {
		t.Fatalf("claims = %d, tasks = %d, want nothing claimed or sent", n, len(r.h.q.inboxTasks()))
	}
}

func TestAnAskForAScheduleNothingRunsIsRefused(t *testing.T) {
	t.Parallel()
	o := unitOrg(org.TargetLead)
	o.Units[0].Lead = ""
	r := manualOver(t, o)
	if _, err := r.ask(t, types.ScheduleScopeUnit, o.Units[0].Key(), "standup", "op-1"); !errors.Is(err, ErrNoRunners) {
		t.Fatalf("err = %v, want ErrNoRunners", err)
	}
	if _, err := r.m.Fire(t.Context(), ManualAsk{Company: o}); !errors.Is(err, ErrNoAsk) {
		t.Fatalf("an ask naming no operation: err = %v, want ErrNoAsk", err)
	}
}

func TestAnAskClaimsNothingForAMailboxItCannotConfirm(t *testing.T) {
	t.Parallel()
	r := manualOver(t, roleOrg(), func(opts *ManualOptions) {
		opts.Mailboxes = refuseMailboxes("qa")
	})
	runs, err := r.ask(t, types.ScheduleScopeRole, "qa", "smoke", "op-1")
	if err != nil || runs[0].Status != ManualNotReady {
		t.Fatalf("runs = %+v, %v, want not_ready", runs, err)
	}
	if n := r.h.ledger.count(); n != 0 {
		t.Fatalf("claims = %d, want 0: asking again must be able to send it", n)
	}
}

func TestAnAskWhosePublishFailedIsUnknownAndNotSentAgain(t *testing.T) {
	t.Parallel()
	r := manualOver(t, roleOrg())
	r.h.q.refuse[seatInbox("qa")] = errors.New("broker down")
	runs, err := r.ask(t, types.ScheduleScopeRole, "qa", "smoke", "op-1")
	if err != nil || runs[0].Status != ManualUnknown {
		t.Fatalf("runs = %+v, %v, want unknown", runs, err)
	}
	delete(r.h.q.refuse, seatInbox("qa"))
	again, _ := r.ask(t, types.ScheduleScopeRole, "qa", "smoke", "op-1")
	if again[0].Status != ManualAlreadyFired || len(r.h.q.inboxTasks()) != 0 {
		t.Fatalf("the same ask again = %+v with %d tasks, want already_fired and none sent",
			again, len(r.h.q.inboxTasks()))
	}
}

func TestFindResolvesAScheduleByItsScopesIdOrName(t *testing.T) {
	t.Parallel()
	o := roleOrg()
	id, _ := o.AgentIDFor(o.AgentSeatByHandle("qa"))
	for _, ref := range []string{"qa", id.String()} {
		if _, ok := Find(o, types.ScheduleScopeRole, ref, "smoke"); !ok {
			t.Fatalf("Find by %q found nothing", ref)
		}
	}
	if _, ok := Find(o, types.ScheduleScopeUnit, "qa", "smoke"); ok {
		t.Fatal("Find matched a role's schedule under the unit scope")
	}
	// AN ASK'S LABEL IS NO MINUTE'S, whatever the ask: a tick's label is
	// thirteen characters of digits around a `T`.
	tick := regexp.MustCompile(`^\d{8}T\d{4}$`)
	for _, ask := range []string{"", "20260608T0900", "op-1"} {
		if label := ManualFireLabel(ask); tick.MatchString(label) {
			t.Fatalf("ManualFireLabel(%q) = %q, which a tick's label could be", ask, label)
		}
	}
	if !tick.MatchString(FireLabel(tickAt(9, 0, 0), mustZone(t, "UTC"))) {
		t.Fatal("the tick's label no longer has the shape this case holds an ask's apart from")
	}
}
