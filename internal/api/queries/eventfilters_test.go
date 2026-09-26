package queries_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
)

// twinUnitsCompany has two unit seats whose history was written under ONE role
// name — both were "Engineer" until a rename told them apart — so only their
// handles, and the ids derived from them, still separate what each did.
const twinUnitsCompany = `
name: Acme
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
  - name: Payments
    roles:
      - name: Payments Engineer
        handle: eng-payments
        llm: zulu
  - name: Search
    roles:
      - name: Search Engineer
        handle: eng-search
        llm: zulu
`

// filterFixture is a store holding one turn per unit seat, on one channel and
// one trace each, and a registry over it with the twin-unit company applied.
func filterFixture(t *testing.T) (*queries.Registry, map[string]string) {
	t.Helper()
	company, err := config.ParseCompany([]byte(twinUnitsCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ids := map[string]string{}
	for _, handle := range []string{"eng-payments", "eng-search"} {
		id, ok := org.DeriveAgentID("Acme", handle)
		if !ok {
			t.Fatalf("no id for %s", handle)
		}
		ids[handle] = id.String()
	}
	log := openStore(t).Events()
	at := time.Now().UTC().Add(-30 * time.Minute)
	for i, row := range []struct {
		id, handle, turn, channel, trace, typ string
		failed                                bool
	}{
		{"p-phase", "eng-payments", "turn-p", "ch-p", "trace-p", "agent_phase_completed", false},
		{"p-done", "eng-payments", "turn-p", "ch-p", "trace-p", "turn_completed", false},
		{"s-phase", "eng-search", "turn-s", "ch-s", "trace-s", "agent_phase_completed", true},
		{"s-done", "eng-search", "turn-s", "ch-s", "trace-s", "turn_completed", false},
	} {
		body := map[string]any{"agent_id": ids[row.handle], "role": "Engineer",
			"turn_id": row.turn, "channel_id": row.channel}
		if row.failed {
			body["failed"] = true
		}
		raw, _ := json.Marshal(body)
		if err := log.Append(t.Context(), store.EventRecord{
			ID: row.id, Type: row.typ, Source: "engine", Category: "task",
			Time: at.Add(time.Duration(i) * time.Second), TraceID: row.trace,
			Tags: store.ExtractTags(raw), Payload: raw,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return registryOver(t, queries.Sources{
		Events:  fleetOf(log),
		Company: func() *config.Company { return company },
	}), ids
}

func eventIDs(t *testing.T, r *queries.Registry, params map[string]any) []string {
	t.Helper()
	rows := ask(t, r, "events", params)["events"].([]store.EventRecord)
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	slices.Sort(out)
	return out
}

// ONE SEAT, BY ITS HANDLE — on the events, their axis and the turns alike, and
// for a UNIT seat whose history another seat's shares a role name with.
//
// The turns list took a role name, which a rename changes while the history
// keeps the old one — so it cannot tell these two seats' pasts apart — and the
// dashboard's seat rows link with `seat=<handle>`, which nothing read. The
// handle is resolved server-side to the id every node derives for the seat, so
// the answer is that seat's and nobody else's.
//
// Mutation: resolve `seat` to the role name, and each seat's answer carries the
// other's rows.
func TestOneSeatByHandleNarrowsEventsTheAxisAndTurns(t *testing.T) {
	t.Parallel()
	r, _ := filterFixture(t)

	if got := eventIDs(t, r, map[string]any{"seat": "eng-search"}); !slices.Equal(got, []string{"s-done", "s-phase"}) {
		t.Errorf("events of eng-search = %v, want its two rows alone", got)
	}
	series := askRaw(t, r, "event_series", map[string]any{"seat": "eng-search", "bucket": "hour"}).(queries.SeriesAnswer)
	if series.Total != 2 {
		t.Errorf("the axis for eng-search counts %d, the listing shows 2", series.Total)
	}
	turns := ask(t, r, "turns", map[string]any{"seat": "eng-payments"})["turns"].([]store.Turn)
	if len(turns) != 1 || turns[0].TurnID != "turn-p" {
		t.Errorf("turns of eng-payments = %+v, want turn-p alone", turns)
	}
}

// ONE CONVERSATION AND ONE TRACE, as filters that page and take a window.
//
// Mutation: drop `channel_id` from the filter reader, and the channel's page is
// the whole log.
func TestTheEventsCanBeNarrowedToAChannelAndATrace(t *testing.T) {
	t.Parallel()
	r, _ := filterFixture(t)
	if got := eventIDs(t, r, map[string]any{"channel_id": "ch-p"}); !slices.Equal(got, []string{"p-done", "p-phase"}) {
		t.Errorf("events on ch-p = %v, want the payments seat's two", got)
	}
	if got := eventIDs(t, r, map[string]any{"trace_id": "trace-s"}); !slices.Equal(got, []string{"s-done", "s-phase"}) {
		t.Errorf("events on trace-s = %v, want the search seat's two", got)
	}
	series := askRaw(t, r, "event_series", map[string]any{"channel_id": "ch-s", "bucket": "hour"}).(queries.SeriesAnswer)
	if series.Total != 2 || series.Failed != 1 {
		t.Errorf("the axis on ch-s counts %d with %d failed, want 2 with 1 — the failed "+
			"split rides the same filters", series.Total, series.Failed)
	}
}

// A SEAT NAMED BY ANYTHING BUT A HANDLE IS REFUSED, and one that cannot be
// resolved yet says so rather than answering empty.
//
// A role name pasted into `seat=` matches nothing, and an empty page reads as a
// seat that never did anything. Before a company is applied there is no name to
// derive a seat's id from, which is a different fact from a quiet seat.
func TestASeatThatIsNotAHandleOrCannotBeResolvedIsRefused(t *testing.T) {
	t.Parallel()
	r, _ := filterFixture(t)
	bare := registryOver(t, queries.Sources{Events: fleetOf(openStore(t).Events())})
	for _, what := range []string{"events", "event_series", "turns"} {
		params := map[string]any{"seat": "Engineer", "bucket": "hour"}
		if _, err := r.Answer(t.Context(), what, params, ""); !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("%s seat=Engineer: err = %v, want ErrBadParams", what, err)
		}
		params = map[string]any{"seat": "eng-search", "bucket": "hour"}
		if _, err := bare.Answer(t.Context(), what, params, ""); !errors.Is(err, queries.ErrUnavailable) {
			t.Errorf("%s seat= with no company: err = %v, want ErrUnavailable", what, err)
		}
	}
}
