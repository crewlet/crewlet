package queries_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
	"github.com/crewlet/crewlet/internal/usage"
)

const readPage = "0b6f5a4e-6a41-4b6e-9d8c-3f1e2d4c5b6a"

// reads publishes one node's seat-day carrying reads, through the shipped
// applier — the rows a node's usage publisher replicates to every node.
func (f *spendFixture) reads(node, day, handle string, elided int, rs ...usage.Read) {
	f.t.Helper()
	f.seq++
	r := usage.Record{
		RecordEnvelope: usage.RecordEnvelope{Writer: node, Subject: usage.Subject{
			Kind: usage.KindSeat, Node: node, Day: day, Seat: f.agentID(handle)}},
		Handle: handle, Role: strings.ToUpper(handle),
		Turns: &usage.Turns{}, Reads: rs, ReadsElided: elided,
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

func read(via string, count int64, at time.Time, turn, query string) usage.Read {
	return usage.Read{PageID: readPage, Backend: "native", Via: via, Count: count,
		LastAt: at, LastTurnID: turn, LastQuery: query}
}

// A PAGE'S READERS ARE EVERY NODE'S, summed per seat and way of reading, with
// the newest read's details — and "today" is the COMPANY's day. spendNow is
// 09:00 on the 25th in Tokyo and still the 24th in UTC, so a count cut on the
// wrong clock reads yesterday's seats as today's.
func TestPageReadsSumsEveryNodeAndCutsTodayOnTheCompanyClock(t *testing.T) {
	t.Parallel()
	f := newSpendFixture(t)
	early := time.Date(2026, 9, 24, 1, 0, 0, 0, time.UTC)
	late := time.Date(2026, 9, 24, 23, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC).Add(-24 * time.Hour)
	f.reads("node-a", "2026-09-25", "lead", 0, read("get_page", 2, early, "run-a", ""))
	f.reads("node-b", "2026-09-25", "lead", 0, read("get_page", 1, late, "run-b", ""))
	f.reads("node-b", "2026-09-24", "sre", 3, read("search", 4, yesterday, "run-c", "dhcp lease"),
		// OFFERED IN A CATALOGUE is not a read, and never counts toward one.
		read("skill_injected", 9, yesterday, "run-c", ""))

	work := &stubWork{places: map[string]tracker.TurnPlace{
		"run-b": {TaskID: "t-1", Key: "ENG-412", Title: "Retry PXE boot", Ordinal: 2}}}
	sources := f.sources()
	sources.Work = work
	got, ok := askRaw(t, registryOver(t, sources), "page_reads",
		map[string]any{"page": readPage}).(queries.PageReadsAnswer)
	if !ok {
		t.Fatalf("page_reads answered %T", got)
	}
	if got.Since != "2026-08-27" || got.Until != "2026-09-25" || got.Days != 30 {
		t.Errorf("window = %s..%s (%d), want the thirty company days to the 25th",
			got.Since, got.Until, got.Days)
	}
	if got.DistinctSeatsToday != 1 {
		t.Errorf("distinct seats today = %d, want 1 — the sre read on the 24th, "+
			"which is yesterday on the company's clock", got.DistinctSeatsToday)
	}
	if got.Elided != 3 {
		t.Errorf("elided = %d, want the 3 the sre's day dropped", got.Elided)
	}
	want := []queries.PageReadRow{
		{Handle: "lead", Role: "LEAD", Via: "get_page", Count: 3, LastAt: late, LastTurnID: "run-b",
			LastWorkItem: &tracker.TurnPlace{TaskID: "t-1", Key: "ENG-412", Title: "Retry PXE boot", Ordinal: 2}},
		{Handle: "sre", Role: "SRE", Via: "search", Count: 4, LastAt: yesterday,
			LastTurnID: "run-c", LastQuery: "dhcp lease"},
	}
	if !reflect.DeepEqual(got.Readers, want) || got.ReadersTotal != 2 {
		t.Errorf("readers (%d) = %+v\nwant %+v", got.ReadersTotal, got.Readers, want)
	}
	if len(work.placesAsked) != 2 {
		t.Errorf("the tracker was asked to place %v, want both readers' runs", work.placesAsked)
	}
}

func TestPageReadsRefusesAWindowPastItsMonth(t *testing.T) {
	t.Parallel()
	f := newSpendFixture(t)
	r := registryOver(t, f.sources())
	for _, days := range []int{0, 31} {
		_, err := r.Answer(t.Context(), "page_reads", map[string]any{"page": readPage, "days": days}, "")
		if !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("days=%d: err = %v, want bad params", days, err)
		}
	}
	if _, err := r.Answer(t.Context(), "page_reads", map[string]any{}, ""); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("no page: err = %v, want bad params", err)
	}
}

type stubBacklinks struct {
	links search.Backlinks
	err   error
}

func (s stubBacklinks) LinkedFrom(context.Context, string) (search.Backlinks, error) {
	return s.links, s.err
}

// A NODE THAT CANNOT SAY WHAT LINKS HERE SAYS WHY, and still serves the page.
// An index on its first lap answers `building` and a failed read
// `unavailable` — never an empty `linked_from`, which the rail draws as "no
// page or task links here" — and neither fails the document, since the
// backlinks are a signal beside it.
func TestThePageAnswerSaysWhyItCarriesNoBacklinks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want queries.LinkedFromStatus
	}{
		{"building", search.ErrIndexBuilding, queries.LinkedFromBuilding},
		{"unavailable", fmt.Errorf("search: name the pages: %w", store.ErrNoEstate),
			queries.LinkedFromUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newSpendFixture(t)
			sources := f.sources()
			sources.Pages = &stubPages{detail: pages.Detail{Page: pages.Page{ID: readPage, Title: "Deploying"}}}
			sources.Backlinks = stubBacklinks{err: tc.err}
			got, ok := askRaw(t, registryOver(t, sources), "page", map[string]any{"id": readPage}).(queries.PageAnswer)
			if !ok {
				t.Fatalf("page answered %T", got)
			}
			if got.Page.Title != "Deploying" {
				t.Errorf("the page itself = %+v, want it served", got.Page)
			}
			if got.LinkedFrom != nil || got.LinkedFromStatus != tc.want || !got.LinkedFromStatus.Valid() {
				t.Errorf("linked_from = %+v, status %q — want none, status %q",
					got.LinkedFrom, got.LinkedFromStatus, tc.want)
			}
		})
	}
}

// THE RAIL HAS A NOTE FOR EVERY REASON THE ENGINE GIVES, and none for a reason
// it does not: a status the dashboard does not know draws no note at all, and
// the section vanishes exactly where it should have said why.
func TestTheDashboardKnowsExactlyTheBacklinkStatuses(t *testing.T) {
	t.Parallel()
	got, err := clientsource.Union(clientsource.Tree(t), "LinkedFromStatus")
	if err != nil {
		t.Fatalf("%v — this gate cannot run without the client's declaration", err)
	}
	want := make([]string, 0, len(queries.LinkedFromStatuses))
	for _, st := range queries.LinkedFromStatuses {
		want = append(want, string(st))
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's LinkedFromStatus is %v; the engine sends %v", got, want)
	}
}

// A TOOL-SKILL PAGE SAYS WHO IT REACHED AS A SKILL, from BOTH ways it does —
// loaded by the seat and offered in a phase's catalogue — and an ordinary page
// carries no such list at all. And `linked_from` is the index's answer, passed
// through whole.
func TestThePageAnswerCarriesSkillLoadsAndBacklinks(t *testing.T) {
	t.Parallel()
	f := newSpendFixture(t)
	at := time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC)
	f.reads("node-a", "2026-09-25", "lead", 0,
		read("skill_loaded", 2, at, "run-a", ""), read("skill_injected", 5, at.Add(time.Hour), "run-a", ""))
	f.reads("node-b", "2026-09-24", "sre", 0, read("skill_injected", 1, at.Add(-time.Hour), "run-b", ""))
	f.reads("node-b", "2026-09-25", "sre", 0, read("get_page", 1, at, "run-c", ""))

	links := search.Backlinks{
		Pages:      []search.PageLink{{ID: "p-2", Container: "ENG", Title: "Scheduler on-call"}},
		Tasks:      []search.TaskLink{{ID: "t-1", Key: "ENG-412", Title: "Retry PXE boot", Status: "in_progress"}},
		PagesTotal: 1, TasksTotal: 1,
	}
	stub := &stubPages{detail: pages.Detail{Page: pages.Page{ID: readPage, Title: "Deploying"}, Skill: true}}
	sources := f.sources()
	sources.Pages, sources.Backlinks = stub, stubBacklinks{links: links}
	got, ok := askRaw(t, registryOver(t, sources), "page", map[string]any{"id": readPage}).(queries.PageAnswer)
	if !ok {
		t.Fatalf("page answered %T", got)
	}
	want := []queries.SkillLoad{
		{Handle: "lead", LastAt: at.Add(time.Hour), Count: 7, Loaded: 2, Offered: 5},
		{Handle: "sre", LastAt: at.Add(-time.Hour), Count: 1, Offered: 1},
	}
	if !reflect.DeepEqual(got.SkillLoadedBy, want) {
		t.Errorf("skill_loaded_by = %+v\nwant %+v", got.SkillLoadedBy, want)
	}
	if got.LinkedFrom == nil || !reflect.DeepEqual(*got.LinkedFrom, links) {
		t.Errorf("linked_from = %+v, want the index's answer", got.LinkedFrom)
	}

	stub.detail.Skill = false
	got = askRaw(t, registryOver(t, sources), "page", map[string]any{"id": readPage}).(queries.PageAnswer)
	if got.SkillLoadedBy != nil {
		t.Errorf("an ordinary page carries skill_loaded_by = %+v", got.SkillLoadedBy)
	}
}

// "NOBODY LOADED THIS SKILL" IS AN EMPTY LIST ON THE WIRE, NOT AN ABSENT KEY:
// absent is what an ordinary page — or a node that does not read the usage
// domain — sends, and the dashboard draws "not loaded in 30 days" only from the
// empty list. An omitempty tag drops both alike, so this reads the ENCODED
// answer rather than the struct.
func TestAnUnloadedSkillSendsAnEmptyListNotAnAbsentKey(t *testing.T) {
	t.Parallel()
	f := newSpendFixture(t)
	stub := &stubPages{detail: pages.Detail{Page: pages.Page{ID: readPage, Title: "Deploying"}, Skill: true}}
	sources := f.sources()
	sources.Pages = stub
	wire := func() map[string]json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(askRaw(t, registryOver(t, sources), "page", map[string]any{"id": readPage}))
		if err != nil {
			t.Fatalf("marshal the page answer: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("decode the page answer: %v", err)
		}
		return fields
	}
	if got, ok := wire()["skill_loaded_by"]; !ok || string(got) != "[]" {
		t.Errorf("an unloaded tool skill sends skill_loaded_by = %s (present %v), want []", got, ok)
	}
	stub.detail.Skill = false
	if got, ok := wire()["skill_loaded_by"]; ok {
		t.Errorf("an ordinary page sends skill_loaded_by = %s, want the key absent", got)
	}
}

// THE SKILLS SCREEN'S "LOADED BY" COMES WITH THE LISTING, for every row in one
// read — both ways a skill reaches a seat — and only on a listing of skills.
func TestASkillListingCarriesWhoLoadedEachSkill(t *testing.T) {
	t.Parallel()
	f := newSpendFixture(t)
	at := time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC)
	f.reads("node-a", "2026-09-25", "lead", 0,
		read("skill_loaded", 2, at, "run-a", ""), read("skill_injected", 3, at, "run-a", ""))
	stub := &stubPages{list: []pages.Summary{{ID: readPage, Title: "Deploying", Skill: true},
		{ID: "p-quiet", Title: "Rollback", Skill: true}}, total: 2}
	sources := f.sources()
	sources.Pages = stub
	got := ask(t, registryOver(t, sources), "pages", map[string]any{"skills": true})
	loads, ok := got["skill_loaded_by"].(map[string][]queries.SkillLoad)
	if !ok {
		t.Fatalf("skill_loaded_by = %T, want a map by page id", got["skill_loaded_by"])
	}
	if l := loads[readPage]; len(l) != 1 || l[0].Loaded != 2 || l[0].Offered != 3 || l[0].Count != 5 {
		t.Errorf("Deploying's loads = %+v, want lead with 2 loaded and 3 offered", l)
	}
	if l, ok := loads["p-quiet"]; !ok || len(l) != 0 {
		t.Errorf("an unloaded skill = %+v (present %v), want an empty list", l, ok)
	}

	got = ask(t, registryOver(t, sources), "pages", map[string]any{"skills": false})
	if _, ok := got["skill_loaded_by"]; ok {
		t.Error("an ordinary listing carries skill_loaded_by")
	}
}
