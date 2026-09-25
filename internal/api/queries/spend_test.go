package queries_test

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
	"github.com/crewlet/crewlet/internal/usage"
)

// spendCompany is a company on Tokyo's clock with a root seat and a seat
// inside a unit — the one whose handle the live rollup used to lose.
const spendCompany = `
name: Acme
timezone: Asia/Tokyo
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
roles:
  - name: Lead
    handle: lead
    llm: zulu
units:
  - name: Engineering
    roles:
      - name: Site Reliability
        handle: sre
        llm: zulu
`

// spendNow is 09:00 on 25 September 2026 in Tokyo — still the 24th in UTC,
// so a window cut on the wrong clock is a day off.
var spendNow = time.Date(2026, time.September, 25, 0, 0, 0, 0, time.UTC)

// spendFixture is a store, the spend company and a registry over both.
type spendFixture struct {
	t       *testing.T
	db      *store.DB
	company *config.Company
	seq     uint64
}

func newSpendFixture(t *testing.T) *spendFixture {
	t.Helper()
	company, err := config.ParseCompany([]byte(spendCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return &spendFixture{t: t, db: openStore(t), company: company}
}

func (f *spendFixture) sources() queries.Sources {
	return queries.Sources{
		State:   livestate.New(),
		Usage:   f.db.Replicated(),
		Company: func() *config.Company { return f.company },
		Now:     func() time.Time { return spendNow },
	}
}

// agentID is the id every node derives for a handle in this company.
func (f *spendFixture) agentID(handle string) string {
	id, ok := org.DeriveAgentID("Acme", handle)
	if !ok {
		f.t.Fatalf("no id for %s", handle)
	}
	return id.String()
}

// day publishes one node's seat-day through the shipped applier — the rows a
// node's usage publisher would have replicated to every node.
func (f *spendFixture) day(node, day, handle string, turns, failed int64, cells ...usage.Tokens) {
	f.t.Helper()
	f.seq++
	r := usage.Record{
		RecordEnvelope: usage.RecordEnvelope{Writer: node, Subject: usage.Subject{
			Kind: usage.KindSeat, Node: node, Day: day, Seat: f.agentID(handle)}},
		Handle: handle, Role: strings.ToUpper(handle),
		Turns:  &usage.Turns{Count: turns, Failed: failed},
		Tokens: cells,
	}
	body, err := r.Encode()
	if err != nil {
		f.t.Fatalf("encode: %v", err)
	}
	env, err := usage.Domain{}.Envelope(body)
	if err != nil {
		f.t.Fatalf("envelope: %v", err)
	}
	rec := statelog.Record{Envelope: env, Payload: body, Position: statelog.Position{
		Stream: usage.Domain{}.Stream().Name, Generation: 1, Seq: f.seq}}
	if err := f.db.Replicated().Tx(f.t.Context(), func(tx *sql.Tx) error {
		_, err := usage.NewApplier().Apply(f.t.Context(), tx, rec, statelog.ApplyOptions{
			MaxVariables: f.db.Replicated().Caps().MaxVariables})
		return err
	}); err != nil {
		f.t.Fatalf("apply %s %s %s: %v", node, day, handle, err)
	}
}

func spent(phase, model string, total int64) usage.Tokens {
	return usage.Tokens{Phase: phase, Model: model, ProviderKey: "zulu",
		Input: total, Total: total, Calls: 1}
}

func rollupOf(t *testing.T, r *queries.Registry, params map[string]any) tokens.Rollup {
	t.Helper()
	got, ok := askRaw(t, r, "tokens", params).(tokens.Rollup)
	if !ok {
		t.Fatalf("tokens answered %T, want a rollup", got)
	}
	return got
}

func seriesOf(t *testing.T, r *queries.Registry, params map[string]any) tokens.Series {
	t.Helper()
	got, ok := askRaw(t, r, "token_series", params).(tokens.Series)
	if !ok {
		t.Fatalf("token_series answered %T, want a series", got)
	}
	return got
}

// ONE NODE ANSWERS FOR EVERY NODE. The rows node A's publisher replicated are
// in the estate node B reads, so B's answer carries A's spend — which is the
// whole point of the domain. Read from B's own event log, this answer
// described B alone, under the company's name.
func TestANamedWindowAnswersEveryNodesSpend(t *testing.T) {
	t.Parallel()
	f := newSpendFixture(t)
	f.day("node-a", "2026-09-24", "lead", 2, 1, spent("execute", "sonnet", 100))
	f.day("node-b", "2026-09-24", "lead", 1, 0, spent("review", "haiku", 30))
	f.day("node-a", "2026-09-25", "sre", 1, 0, spent("sandbox", "cli", 7))

	got := rollupOf(t, registryOver(t, f.sources()), map[string]any{"days": 7})
	if got.Totals.TotalTokens != 137 || got.Totals.Calls != 3 {
		t.Errorf("totals = %+v, want both nodes' days summed", got.Totals)
	}
	seats := map[string]tokens.AgentRow{}
	for _, a := range got.ByAgent {
		seats[a.Handle] = a
	}
	lead := seats["lead"]
	if lead.TotalTokens != 130 || lead.Turns == nil || *lead.Turns != 3 || *lead.Failed != 1 {
		t.Errorf("lead = %+v (turns %v), want both nodes' 130 tokens over 3 turns, 1 failed",
			lead, lead.Turns)
	}
	if got.From != "2026-09-19" || got.To != "2026-09-25" || got.Days != 7 {
		t.Errorf("window = %s..%s (%d days), want the seven company days ending "+
			"Tokyo's today", got.From, got.To, got.Days)
	}
	if got.Since != "2026-09-18T15:00:00Z" {
		t.Errorf("since = %s, want Tokyo's midnight on the 19th", got.Since)
	}
	if got.Horizon == nil || got.Horizon.Days != usage.HorizonDays ||
		got.Horizon.Floor != "2026-03-28" {
		t.Errorf("horizon = %+v, want %d days back to 2026-03-28", got.Horizon, usage.HorizonDays)
	}
	if got.ByTurn != nil {
		t.Errorf("a named window lists turns %+v — a company day holds none", got.ByTurn)
	}
}

// THE PREVIOUS WINDOW REACHES BACK AS FAR AS THE OFFER DOES, and past it is a
// refusal naming `days`. The event log this read kept thirty days, so the
// previous half of a thirty- or ninety-day comparison was silently empty.
func TestThePreviousWindowIsServedAtSevenThirtyAndNinetyDays(t *testing.T) {
	t.Parallel()
	f := newSpendFixture(t)
	today := period.At(period.Day, spendNow, mustZone(t, "Asia/Tokyo"))
	for _, n := range []int{7, 30, 90} {
		// One cell on the first day of each previous window.
		f.day("node-a", today.Shift(-(2*n - 1)).Label, "lead", 1, 0,
			spent("execute", "sonnet", int64(n)))
	}
	r := registryOver(t, f.sources())
	for _, n := range []int{7, 30, 90} {
		got := rollupOf(t, r, map[string]any{"days": n, "previous": true})
		if got.Days != n || got.To != today.Shift(-n).Label {
			t.Errorf("previous of %d = %s..%s (%d days)", n, got.From, got.To, got.Days)
		}
		if got.Totals.TotalTokens < n {
			t.Errorf("previous of %d days counts %d tokens — its first day's %d is "+
				"missing", n, got.Totals.TotalTokens, n)
		}
	}
	for _, params := range []map[string]any{
		{"days": 91},
		{"days": 0},
		{"days": "a week"},
	} {
		_, err := r.Answer(t.Context(), "tokens", params, "")
		if !errors.Is(err, queries.ErrBadParams) || !errors.Is(err, tokens.ErrOutOfRange) ||
			!strings.Contains(err.Error(), "days=") {
			t.Errorf("%v: err = %v, want a refusal naming days", params, err)
		}
	}
}

// A WINDOW THAT STARTS BEFORE THE HORIZON IS REFUSED rather than floored: the
// rows are gone on every node, and an answer floored to what remains would put
// a heading over fewer days than it names.
func TestAWindowBeforeTheHorizonIsRefused(t *testing.T) {
	t.Parallel()
	f := newSpendFixture(t)
	r := registryOver(t, f.sources())
	_, err := r.Answer(t.Context(), "token_series",
		map[string]any{"since": "2026-03-01", "until": "2026-03-10"}, "")
	if !errors.Is(err, tokens.ErrOutOfRange) || !strings.Contains(err.Error(), "since=2026-03-01") {
		t.Errorf("err = %v, want the horizon refusal naming since", err)
	}
	// And a custom window's previous half past the floor names days.
	_, err = r.Answer(t.Context(), "tokens",
		map[string]any{"since": "2026-03-29", "until": "2026-04-27", "previous": true}, "")
	if !errors.Is(err, tokens.ErrOutOfRange) || !strings.Contains(err.Error(), "fewer days") {
		t.Errorf("err = %v, want the previous window refused", err)
	}
}

// EVERY WINDOW PARAMETER A SCREEN CAN GET WRONG IS REFUSED NAMING ITSELF.
func TestASpendWindowItCannotReadIsRefusedNamingTheParameter(t *testing.T) {
	t.Parallel()
	r := registryOver(t, newSpendFixture(t).sources())
	for _, tc := range []struct {
		what   string
		params map[string]any
		names  string
	}{
		{"tokens", map[string]any{"days": 7, "since": "2026-09-01", "until": "2026-09-02"}, "not both"},
		{"tokens", map[string]any{"since": "2026-09-01"}, "pair"},
		{"tokens", map[string]any{"since": "2026-09-02", "until": "2026-09-01"}, "since and until"},
		{"tokens", map[string]any{"since": "2026-06-01", "until": "2026-09-01"}, "at most 90"},
		{"tokens", map[string]any{"seat": "Not A Handle"}, "seat="},
		{"token_series", map[string]any{"days": 1, "bucket": "hour"}, "bucket"},
		{"token_series", map[string]any{"days": 1, "group": "turn"}, "group"},
	} {
		_, err := r.Answer(t.Context(), tc.what, tc.params, "")
		if !errors.Is(err, queries.ErrBadParams) || !strings.Contains(err.Error(), tc.names) {
			t.Errorf("%s %v: err = %v, want a refusal mentioning %q", tc.what, tc.params, err, tc.names)
		}
	}
}

// ONE SEAT, BY ITS HANDLE — including a seat no longer in the chart, whose days
// are still in the history under the id every node derives from its handle.
func TestOneSeatCanBeAskedForByHandleEvenAfterItLeft(t *testing.T) {
	t.Parallel()
	f := newSpendFixture(t)
	f.day("node-a", "2026-09-24", "lead", 1, 0, spent("execute", "sonnet", 10))
	f.day("node-a", "2026-09-24", "sre", 1, 0, spent("execute", "sonnet", 20))
	f.day("node-a", "2026-09-24", "gone", 1, 0, spent("execute", "sonnet", 40))
	r := registryOver(t, f.sources())

	got := rollupOf(t, r, map[string]any{"days": 7, "seat": "sre"})
	if got.Totals.TotalTokens != 20 || got.Seat != "sre" || len(got.ByAgent) != 1 {
		t.Errorf("rollup = %+v, want sre's 20 alone", got)
	}
	departed := rollupOf(t, r, map[string]any{"days": 7, "seat": "gone"})
	if departed.Totals.TotalTokens != 40 {
		t.Errorf("a departed seat's spend = %d, want its 40 — the history outlives "+
			"the chart", departed.Totals.TotalTokens)
	}
	series := seriesOf(t, r, map[string]any{"days": 7, "seat": "lead"})
	if series.Totals.TotalTokens != 10 || series.Seat != "lead" {
		t.Errorf("series = %+v, want lead's 10 alone", series.Totals)
	}
}

// ISO WEEKS ON THE COMPANY'S CLOCK, and a coding run inside Execute.
func TestTheSeriesIsCutInCompanyWeeksAndFoldsPhasesIntoFourBands(t *testing.T) {
	t.Parallel()
	f := newSpendFixture(t)
	f.day("node-a", "2026-09-20", "lead", 1, 0, spent("execute", "sonnet", 5)) // Sunday, W38
	f.day("node-a", "2026-09-21", "lead", 1, 0, spent("sandbox", "cli", 7))    // Monday, W39
	f.day("node-b", "2026-09-25", "sre", 1, 0, spent("subagent", "haiku", 3))

	got := seriesOf(t, registryOver(t, f.sources()),
		map[string]any{"days": 14, "bucket": "week"})
	var weeks []string
	for _, p := range got.Points {
		weeks = append(weeks, p.Window)
	}
	if !slices.Equal(weeks, []string{"2026-W37", "2026-W38", "2026-W39"}) {
		t.Fatalf("weeks = %v, want the three ISO weeks the 14 days touch", weeks)
	}
	if at := got.Points[2].At; at != "2026-09-20T15:00:00Z" {
		t.Errorf("W39 starts %s, want Tokyo's Monday midnight", at)
	}
	if w := got.Points[2]; w.Groups["execute"].TotalTokens != 7 || w.Groups["workers"].TotalTokens != 3 {
		t.Errorf("W39 = %+v, want the coding run in execute and the worker in workers", w.Groups)
	}
	var bands []string
	for _, b := range got.ByGroup {
		bands = append(bands, b.Group)
	}
	if !slices.Equal(bands, []string{"execute", "workers"}) {
		t.Errorf("bands = %v, want the phase bands in stacking order", bands)
	}
}

// A UNIT SEAT HAS A HANDLE IN THE ROLLUP. The handle map walked the company's
// ROOT roles only, so every seat inside a unit rolled up with an empty handle
// and its row linked nowhere.
func TestAUnitSeatHasAHandleInTheRollup(t *testing.T) {
	t.Parallel()
	f := newSpendFixture(t)
	sources := f.sources()
	sources.State.Apply(&livestate.Envelope{
		ID: "p1", Type: "agent_phase_completed",
		Timestamp: spendNow.Add(-time.Minute).Format(time.RFC3339Nano),
		Category:  "agent", Payload: map[string]any{
			"role": "Site Reliability", "phase": "execute", "total_tokens": 9,
		},
	})
	sources.Now = time.Now
	got := rollupOf(t, registryOver(t, sources), nil)
	if len(got.ByAgent) != 1 || got.ByAgent[0].Handle != "sre" {
		t.Errorf("by_agent = %+v, want the unit seat under its handle sre", got.ByAgent)
	}
	if handles := sources.RoleHandles(); handles["Site Reliability"] != "sre" || handles["Lead"] != "lead" {
		t.Errorf("handles = %v, want every seat in the chart", handles)
	}
}

// A REGISTRY WITHOUT THE USAGE DOMAIN answers a named window EMPTY and labelled
// as asked — never the live window relabelled.
func TestAWindowNobodyCanSeeIsLabelledAsAsked(t *testing.T) {
	t.Parallel()
	sources := newSpendFixture(t).sources()
	sources.Usage = nil
	got := rollupOf(t, registryOver(t, sources), map[string]any{"days": 14})
	if got.Days != 14 || got.Totals.Calls != 0 {
		t.Errorf("rollup = %+v, want an empty fourteen days", got)
	}
	if got.ByPhase == nil || got.ByAgent == nil || got.ByProvider == nil {
		t.Error("an empty rollup carries nil slices, which marshal to null")
	}
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
