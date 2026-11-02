package statelog_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// modelsAt is the instant every model reading here is evaluated at.
var modelsAt = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

const modelDay = 24 * time.Hour

// models is a reading of one entry's condition at [modelsAt].
func models(conditions ...statelog.ModelCondition) statelog.Reading {
	return statelog.Reading{Models: conditions, ModelsAt: modelsAt}
}

// modelKinds is which model kinds a reading raises, and on which entries.
func modelKinds(r statelog.Reading) map[statelog.Kind][]string {
	out := map[statelog.Kind][]string{}
	for _, a := range statelog.Evaluate(r) {
		for _, e := range a.Entries {
			out[a.Kind] = append(out[a.Kind], e.Key)
		}
		if len(a.Entries) == 0 {
			out[a.Kind] = nil
		}
	}
	return out
}

// THE OWNER'S THRESHOLDS, each driven at the threshold, just inside it and
// just outside it: thirty days and seven before a credential's computed expiry,
// the expiry itself, and fifteen minutes of a seat's wait on a limit. A
// threshold one second off is a different promise than the one the owner made,
// and the page, the health count and the log all read this one evaluation.
//
// Mutation: compare with < where the table says <= (or the reverse), take the
// notice from the warning's constant, or fire the limit alarm beside the
// waiting one rather than handing over, and a row here fails.
func TestModelAlarmsFireAtTheOwnersThresholds(t *testing.T) {
	t.Parallel()
	token := func(left time.Duration) statelog.ModelCondition {
		return statelog.ModelCondition{Key: "default", Variable: "CLAUDE_CODE_OAUTH_TOKEN",
			ExpiresAt: modelsAt.Add(left)}
	}
	sitting := &coord.ModelSitOut{Until: modelsAt.Add(time.Hour), Since: modelsAt.Add(-time.Hour),
		Window: "session", Said: "You've hit your session limit"}
	parked := func(waited time.Duration) statelog.ModelCondition {
		return statelog.ModelCondition{Key: "default", SittingOut: sitting,
			WaitingSince: modelsAt.Add(-waited)}
	}
	for name, tc := range map[string]struct {
		condition statelog.ModelCondition
		want      statelog.Kind // "" is "no model alarm"
	}{
		"a token 31 days out":            {token(31 * modelDay), ""},
		"a token 30 days and a second":   {token(30*modelDay + time.Second), ""},
		"a token exactly 30 days out":    {token(30 * modelDay), statelog.KindLLMCredentialExpiring},
		"a token 8 days out":             {token(8 * modelDay), statelog.KindLLMCredentialExpiring},
		"a token 7 days and a second":    {token(7*modelDay + time.Second), statelog.KindLLMCredentialExpiring},
		"a token exactly 7 days out":     {token(7 * modelDay), statelog.KindLLMCredentialExpiresSoon},
		"a token a second from expiring": {token(time.Second), statelog.KindLLMCredentialExpiresSoon},
		"a token expiring now":           {token(0), statelog.KindLLMSignInRefused},
		"a token expired yesterday":      {token(-modelDay), statelog.KindLLMSignInRefused},
		"a key with no known expiry":     {statelog.ModelCondition{Key: "default", Variable: "ANTHROPIC_API_KEY"}, ""},
		"a refusal newer than any answer": {statelog.ModelCondition{Key: "default",
			RefusedAt: modelsAt.Add(-time.Minute), RefusedSaid: "Failed to authenticate"}, statelog.KindLLMSignInRefused},
		"nothing signs in a model a seat runs": {statelog.ModelCondition{Key: "default", NoSignIn: true}, statelog.KindLLMSignInRefused},
		"a sit-out nobody waits on":            {statelog.ModelCondition{Key: "default", SittingOut: sitting}, statelog.KindLLMLimit},
		"a sit-out that has ended":             {statelog.ModelCondition{Key: "default", SittingOut: &coord.ModelSitOut{Until: modelsAt}}, ""},
		"a seat parked 14m59s":                 {parked(limitWait - time.Second), statelog.KindLLMLimit},
		"a seat parked exactly 15m":            {parked(limitWait), statelog.KindLLMLimitWaiting},
		"a seat parked an hour":                {parked(time.Hour), statelog.KindLLMLimitWaiting},
		"a seat parked 15m on a chain whose window this node no longer sees": {statelog.ModelCondition{Key: "default",
			WaitingSince: modelsAt.Add(-limitWait)}, statelog.KindLLMLimitWaiting},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := modelKinds(models(tc.condition))
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("raised %v, want nothing", got)
				}
				return
			}
			if len(got) != 1 || len(got[tc.want]) != 1 || got[tc.want][0] != "default" {
				t.Fatalf("raised %v, want %s on default alone", got, tc.want)
			}
		})
	}
}

// limitWait is the owner's fifteen minutes, named once for the table above.
const limitWait = statelog.LimitWaitingGrace

// THE CONSTANTS ARE THE OWNER'S, and the reference page says them: a retune
// here is a decision nobody made.
func TestTheModelThresholdsAreTheOwners(t *testing.T) {
	t.Parallel()
	if statelog.CredentialNoticeDays != 30 || statelog.CredentialWarningDays != 7 ||
		statelog.LimitWaitingGrace != 15*time.Minute {
		t.Fatalf("thresholds are %d days, %d days and %v; the owner fixed 30, 7 and 15m",
			statelog.CredentialNoticeDays, statelog.CredentialWarningDays, statelog.LimitWaitingGrace)
	}
	page := statelog.AlarmReference()
	for _, says := range []string{"at 30 days and again at 7", "waited 15m on it"} {
		if !strings.Contains(page, says) {
			t.Errorf("the reference does not say %q", says)
		}
	}
}

// ONE ALARM PER KIND, ITS ENTRIES LISTED, each with its own measurement, led by
// the longest-standing: two models whose limits stand are one `llm_limit` on
// the health count and two lines in the log.
func TestAModelKindOnTwoEntriesIsOneAlarmListingBoth(t *testing.T) {
	t.Parallel()
	older := &coord.ModelSitOut{Until: modelsAt.Add(time.Hour), Since: modelsAt.Add(-2 * time.Hour), Window: "weekly"}
	newer := &coord.ModelSitOut{Until: modelsAt.Add(time.Hour), Since: modelsAt.Add(-time.Minute), Window: "session"}
	got := statelog.Evaluate(models(
		statelog.ModelCondition{Key: "sonnet", SittingOut: newer},
		statelog.ModelCondition{Key: "aux", SittingOut: older},
	))
	if len(got) != 1 || got[0].Kind != statelog.KindLLMLimit {
		t.Fatalf("raised %v, want one llm_limit", kindsOf(got))
	}
	a := got[0]
	if len(a.Entries) != 2 || a.Entries[0].Key != "aux" || a.Entries[1].Key != "sonnet" {
		t.Fatalf("entries = %+v, want aux then sonnet", a.Entries)
	}
	if !strings.Contains(a.Detail, "aux sits out its weekly limit") {
		t.Errorf("detail = %q, want the longest-standing entry's (aux)", a.Detail)
	}
	if !strings.Contains(a.Entries[1].Detail, "sonnet sits out its session limit") ||
		!a.Entries[0].Since.Equal(older.Since) {
		t.Errorf("entries = %+v, want each with its own measurement and start", a.Entries)
	}
	// AND A NODE THAT GATHERED NO MODELS EVALUATES NONE, whatever rows it
	// was handed: an unstamped reading is one nobody measured.
	if got := statelog.Evaluate(statelog.Reading{Models: []statelog.ModelCondition{{Key: "aux", NoSignIn: true}}}); len(got) != 0 {
		t.Errorf("an unstamped reading raised %v", kindsOf(got))
	}
}

// THE LIMIT HANDS OVER TO THE WAIT, PER ENTRY, AND THE LOG SAYS SO: when the
// first seat has waited fifteen minutes, that entry's `llm_limit` clears and
// its `llm_limit_waiting` is raised, each line carrying the entry, while
// another entry's limit stands untouched.
//
// Mutation: key the tracker's instances on the kind alone, or drop the entry
// from the line, and this fails.
func TestALimitHandsOverToTheWaitPerEntry(t *testing.T) {
	t.Parallel()
	clock := modelsAt
	tracker := statelog.NewTracker(nil, func() time.Time { return clock })
	var logs bytes.Buffer
	statelog.LogTrackerTo(tracker, slog.New(slog.NewJSONHandler(&logs, nil)))
	lines := func(event string) []map[string]any {
		var out []map[string]any
		for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			var line map[string]any
			if json.Unmarshal([]byte(raw), &line) == nil && line["msg"] == event {
				out = append(out, line)
			}
		}
		return out
	}
	sit := &coord.ModelSitOut{Until: modelsAt.Add(3 * time.Hour), Since: modelsAt, Window: "session"}
	parkedAt := modelsAt.Add(time.Minute)
	at := func(now time.Time) statelog.Reading {
		return statelog.Reading{ModelsAt: now, Models: []statelog.ModelCondition{
			{Key: "default", SittingOut: sit, WaitingSince: parkedAt},
			{Key: "sonnet", SittingOut: sit},
		}}
	}

	tracker.Observe(t.Context(), statelog.Evaluate(at(clock)))
	raised := lines("alarm_raised")
	if len(raised) != 2 {
		t.Fatalf("two entries sitting out raised %d line(s), want one per entry: %v", len(raised), raised)
	}
	for _, line := range raised {
		if line["alarm"] != string(statelog.KindLLMLimit) || (line["entry"] != "default" && line["entry"] != "sonnet") {
			t.Errorf("raise line %v, want llm_limit naming its entry", line)
		}
	}

	clock = parkedAt.Add(limitWait)
	tracker.Observe(t.Context(), statelog.Evaluate(at(clock)))
	cleared := lines("alarm_cleared")
	if len(cleared) != 1 || cleared[0]["alarm"] != string(statelog.KindLLMLimit) || cleared[0]["entry"] != "default" {
		t.Fatalf("clear lines = %v, want default's llm_limit alone", cleared)
	}
	raised = lines("alarm_raised")
	if len(raised) != 3 || raised[2]["alarm"] != string(statelog.KindLLMLimitWaiting) || raised[2]["entry"] != "default" {
		t.Fatalf("raise lines = %v, want default's llm_limit_waiting after the first two", raised)
	}
	latest := tracker.Latest()
	if len(latest) != 2 || latest[0].Kind != statelog.KindLLMLimit || latest[1].Kind != statelog.KindLLMLimitWaiting ||
		len(latest[0].Entries) != 1 || latest[0].Entries[0].Key != "sonnet" {
		t.Errorf("Latest = %+v, want sonnet's llm_limit then default's wait", latest)
	}
}

// LATEST IS NOTHING BEFORE AN EVALUATION and the evaluation's alarms after, so
// the Models page reads exactly what the health count and the log were told.
func TestLatestIsTheLatestEvaluation(t *testing.T) {
	t.Parallel()
	tracker := statelog.NewTracker(nil, nil)
	if got := tracker.Latest(); got != nil {
		t.Fatalf("Latest before any evaluation = %v, want nil", got)
	}
	tracker.Observe(t.Context(), statelog.Evaluate(models(statelog.ModelCondition{Key: "aux", NoSignIn: true})))
	if got := tracker.Latest(); len(got) != 1 || got[0].Kind != statelog.KindLLMSignInRefused {
		t.Fatalf("Latest = %v, want the refusal just evaluated", got)
	}
	tracker.Observe(t.Context(), nil)
	if got := tracker.Latest(); len(got) != 0 {
		t.Fatalf("Latest after a clear = %v, want none", got)
	}
}
