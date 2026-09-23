package queries_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
	"github.com/crewlet/crewlet/internal/tracker"
)

// memorySandbox is a pending-run store with nothing in it: this sweep is
// about which names exist, not what they answer.
type memorySandbox struct{}

func (memorySandbox) ListActive(context.Context) ([]sandbox.PendingRun, error) {
	return nil, nil
}

func (memorySandbox) BridgeCallPage(context.Context, sandbox.PendingRun, uint64, int) (sandbox.BridgeCallPage, error) {
	return sandbox.BridgeCallPage{}, nil
}

// declaration finds the ONE file under the dashboard tree whose source matches
// `pattern`, and hands back its first capture.
//
// A GATE OVER A CONSTANT IS A GATE OVER THE CONSTANT. Reading it from a fixed
// path makes every such gate a second thing that breaks when a screen moves —
// and breaks LOUDLY but WRONGLY, reporting a drift between two lists neither of
// which changed. Keyed on the declaration, a move and a rename are both
// invisible, and the two failures that matter are the ones it names: nothing
// declares it, which is a gate certifying nothing; and TWO files declare it,
// which is two copies that can drift from each other as well as from the
// engine.
func declaration(t *testing.T, pattern string) string {
	t.Helper()
	re := regexp.MustCompile(pattern)
	var found []string
	err := filepath.WalkDir(dashboardTree, func(path string, d os.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir(), !strings.HasSuffix(path, ".ts") && !strings.HasSuffix(path, ".tsx"):
			return nil
		case strings.Contains(d.Name(), ".test."):
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if m := re.FindStringSubmatch(string(source)); m != nil {
			found = append(found, m[1])
		}
		return nil
	})
	if err != nil {
		t.Fatalf("the dashboard tree at %s could not be walked, so this gate "+
			"certifies nothing: %v", dashboardTree, err)
	}
	if len(found) != 1 {
		t.Fatalf("%d files under %s match %s, want exactly one — none is a "+
			"gate certifying nothing, and two are two copies that can drift "+
			"from each other as well as from the engine",
			len(found), dashboardTree, pattern)
	}
	return found[0]
}

// dashboardTree is the room source this sweep reads. Relative, because the
// package it certifies is the one that serves those rooms.
//
// THE SOURCE, not the build output. It pointed at `static/dashboard/js` — the
// hand-written bundle the React rewrite deleted — so WalkDir failed, the skip
// below fired, and both gates in this file certified nothing for the whole of
// that rewrite while reporting a pass. That is the exact failure they exist to
// catch, one level up.
const dashboardTree = "../../../dashboard/src"

// roomQueries scans the dashboard for every query kind a room asks for.
//
// FROM THE ROOMS' OWN SOURCE, never a list kept here: a hand-maintained one
// is exactly what drifts, and it would drift towards claiming the server
// answers more than it does.
func roomQueries(t *testing.T) map[string][]string {
	t.Helper()
	// Both call shapes: the `useQuery` hook a screen renders from, and the
	// direct `socket.query` a pager or an action uses.
	//
	// DIGITS IN THE NAME. The class was `[a-z_]+`, which cannot match
	// `a2a_channels` — so the one kind whose name carries a number was
	// invisible to a sweep whose whole job is to notice a missing name.
	// `\s*` after the paren: a formatter wraps a call whose arguments do not
	// fit, and `useQuery(\n  "config_diff",` is the same call as the one that
	// fits on a line. Without it the sweep reported a live reader as missing.
	calls := regexp.MustCompile(`\b(?:useQuery|query)\(\s*"([a-z0-9_]+)"`)
	out := map[string][]string{}
	err := filepath.WalkDir(dashboardTree, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() ||
			(!strings.HasSuffix(path, ".ts") && !strings.HasSuffix(path, ".tsx")) ||
			strings.HasSuffix(path, ".test.ts") || strings.HasSuffix(path, ".test.tsx") {
			return err
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range calls.FindAllStringSubmatch(string(source), -1) {
			room := filepath.Base(path)
			if !slices.Contains(out[m[1]], room) {
				out[m[1]] = append(out[m[1]], room)
			}
		}
		return nil
	})
	if err != nil {
		// FAILS rather than skips. The dashboard source is committed, so it
		// is always in a checkout — and a skip here is indistinguishable
		// from a pass, which is how this gate went quiet the last time the
		// tree moved.
		t.Fatalf("the dashboard source at %s could not be read, so this gate "+
			"certifies nothing: %v", dashboardTree, err)
	}
	if len(out) == 0 {
		t.Fatal("the sweep found no query calls at all, so it certifies nothing")
	}
	return out
}

// everySeam is a Sources with every seam present, which is what makes the
// registry list everything this build can answer.
//
// The seams are only tested for nil by Register, so zero values are enough
// and nothing here is called — this sweep is about WHICH NAMES exist, not
// what they answer. The per-kind tests in this package cover the answers.
func everySeam(t *testing.T) queries.Sources {
	t.Helper()
	surface, _ := configSurface(t, companyDoc)
	cfg := company(t)
	return queries.Sources{
		State:    &livestate.LiveState{},
		Events:   &store.EventLog{},
		Health:   func(context.Context) any { return nil },
		Company:  func() *config.Company { return cfg },
		Coord:    coordmemory.New(),
		Plane:    coordmemory.NewFleet(),
		Runs:     &fakeRuns{},
		Diary:    &learning.Diary{},
		Episodes: &learning.Episodes{},
		Skills:   &learning.Skills{},
		Channels: fakeChannels{},
		Budget:   coordmemory.NewFleet(),
		Sandbox:  memorySandbox{},
		Config:   surface,
		Work:     emptyWork{},
		Pages:    emptyPages{},
		// THE SEARCH INDEX IS ITS OWN SEAM, so a node with a board and
		// no index is a real shape this sweep can describe.
		WorkSearch:     emptyWork{},
		Conversations:  emptyConversations{},
		Counterparties: emptyCounterparties{},
		// THE RETENTION DOCUMENT, which the Fleet screen's replication
		// panels read. A pass-through on the real surface, so the seam is
		// a function rather than a reader — and this sweep is about which
		// names exist, so what it answers is nothing.
		Retention: func(context.Context) any { return nil },
	}
}

// emptyWork and emptyPages are the native readers with nothing in them, on
// fakeChannels' terms: this sweep is about which NAMES exist.
type emptyWork struct{}

func (emptyWork) Tasks(context.Context, tracker.Query, time.Time) (tracker.Answer, error) {
	return tracker.Answer{}, nil
}

func (emptyWork) Task(context.Context, string, tracker.DetailWants,
	statelog.Freshness) (tracker.TaskDetail, error) {

	return tracker.TaskDetail{}, nil
}

func (emptyWork) Views(context.Context, tracker.ViewQuery) (tracker.ViewListing, error) {
	return tracker.ViewListing{}, nil
}

func (emptyWork) ExpandedQuery(_ context.Context, params map[string]any,
	_ tracker.Viewer, now time.Time, loc *time.Location) (tracker.Query, error) {

	return tracker.ParseQuery(tracker.MapParams(params), now, loc)
}

func (emptyWork) Goals(context.Context, tracker.GoalQuery) (tracker.GoalListing, error) {
	return tracker.GoalListing{}, nil
}

func (emptyWork) Catalogue(context.Context, tracker.CatalogueQuery) (tracker.CatalogueAnswer, error) {
	return tracker.CatalogueAnswer{}, nil
}

func (emptyWork) Projects(context.Context, tracker.ProjectQuery) (
	tracker.ProjectListing, error) {

	return tracker.ProjectListing{}, nil
}

func (emptyWork) Project(context.Context, tracker.ProjectDetailQuery) (
	tracker.ProjectDetail, error) {

	return tracker.ProjectDetail{}, nil
}

func (emptyWork) Workload(context.Context, tracker.WorkloadQuery, time.Time) (
	tracker.WorkloadAnswer, error) {

	return tracker.WorkloadAnswer{}, nil
}

func (emptyWork) Activity(context.Context, tracker.ActivityQuery, time.Time) (
	tracker.ActivityAnswer, error) {

	return tracker.ActivityAnswer{}, nil
}

func (emptyWork) MyWork(context.Context, tracker.MyWorkQuery, time.Time) (
	tracker.MyWork, error) {

	return tracker.MyWork{}, nil
}

func (emptyWork) Person(context.Context, tracker.PersonQuery, time.Time) (tracker.PersonState, error) {
	return tracker.PersonState{}, nil
}

func (emptyWork) Inbox(context.Context, tracker.InboxQuery, time.Time) (
	tracker.InboxAnswer, error) {
	return tracker.InboxAnswer{}, nil
}

func (emptyWork) Routing(context.Context, tracker.RoutingQuery, time.Time) (
	tracker.RoutingAnswer, error) {
	return tracker.RoutingAnswer{}, nil
}

func (emptyWork) Search(context.Context, string, int) ([]tracker.Ranked, error) {
	return nil, nil
}

// emptyConversations and emptyCounterparties are the two per-seat stores with
// nothing in them, on emptyWork's terms.
type emptyConversations struct{}

func (emptyConversations) Threads(context.Context, string, int) ([]ledgerstore.Thread, error) {
	return nil, nil
}

func (emptyConversations) History(context.Context, string, string, int) ([]ledger.Session, error) {
	return nil, nil
}

type emptyCounterparties struct{}

func (emptyCounterparties) List(context.Context, string) ([]learning.Profile, bool, error) {
	return nil, false, nil
}

type emptyPages struct{}

func (emptyPages) List(context.Context, pages.Filter, statelog.Freshness) (pages.Listing, error) {
	return pages.Listing{}, nil
}

func (emptyPages) Get(context.Context, string, statelog.Freshness) (pages.Detail, error) {
	return pages.Detail{}, nil
}

func (emptyPages) Activity(context.Context, pages.PageActivityQuery) (pages.PageActivity, error) {
	return pages.PageActivity{}, nil
}

func (emptyPages) Revision(context.Context, string, int, statelog.Freshness) (
	pages.Revision, bool, error) {
	return pages.Revision{}, false, nil
}

func (emptyPages) Containers(context.Context, statelog.Freshness) ([]pages.ContainerListing, error) {
	return nil, nil
}

// fakeChannels is an A2A channel reader with nothing in it: this sweep is
// about which names exist, not what they answer.
type fakeChannels struct{}

func (fakeChannels) OpenChannels(context.Context) ([]coord.Channel, error) { return nil, nil }
func (fakeChannels) AllChannels(context.Context) ([]coord.Channel, error)  { return nil, nil }

// EVERY QUERY A ROOM MAKES IS A QUERY THIS SERVER ANSWERS.
//
// Nothing linked the two, and the cost was a whole feature: the Config
// room's entity editor listed a collection with query("config_entities"),
// opened one with {kind, id}, and no answer was ever registered under that
// name — so every list came back unknown_query and the editor was dead from
// the day it shipped. Both sides' tests passed, because each was written
// against its own idea of the other.
//
// This is the cheap half of the gate internal/e2e gives the push protocol,
// and it is deliberately about NAMES rather than fields: a name is checkable
// without standing up a node, and a name that nothing answers is the failure
// that renders as an empty room with no error anywhere.
func TestEveryQueryARoomMakesIsAnswered(t *testing.T) {
	t.Parallel()
	answered := registeredKinds(t)
	for kind, rooms := range roomQueries(t) {
		if !slices.Contains(answered, kind) {
			t.Errorf("%s asks for %q and nothing answers it, so the room renders "+
				"empty with no error anywhere", strings.Join(rooms, " and "), kind)
		}
	}
}

// registeredKinds is every name this build answers, from a Sources with
// every seam present.
//
// The NAMES only: Register gates each kind on its seam being non-nil, so
// this is the complete surface — and nothing here is invoked, because the
// question is which names exist rather than what they answer. The per-kind
// tests in this package cover the answers.
func registeredKinds(t *testing.T) []string {
	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, everySeam(t))
	names := r.Names()
	if len(names) == 0 {
		t.Fatal("a Sources with every seam registered nothing, so this sweep " +
			"certifies nothing")
	}
	return names
}

// AND EVERY QUERY THIS SERVER ANSWERS IS ONE SOMETHING ASKS FOR.
//
// The other direction, and the one that goes quiet rather than breaking: an
// answer nobody calls is code with tests, no readers, and no way to notice
// it stopped being right. The exceptions are named rather than assumed.
func TestEveryQueryThisServerAnswersHasAReader(t *testing.T) {
	t.Parallel()
	// Read by name from somewhere that is not a room's query() call.
	nonRoom := map[string]string{
		"stream": "the header's health poll reads it through api.js, not a room",
		// A documented PUBLIC read: docs/guides/configure-via-api.md drives
		// it as GET /query/config_entities?kind=roles, and configapi's own
		// comment points at it as the fetch a config loop makes. The
		// dashboard's Config screen is a viewer of the whole document rather
		// than an entity browser, so no room asks — which is not the same as
		// nobody reading it.
		"config_entities": "docs/guides/configure-via-api.md reads it over REST, not a room",
	}

	asked := roomQueries(t)
	for _, kind := range registeredKinds(t) {
		if _, ok := asked[kind]; ok {
			continue
		}
		if why, exempt := nonRoom[kind]; exempt {
			t.Logf("%s: %s", kind, why)
			continue
		}
		t.Errorf("this build answers %q and no room asks for it — either a "+
			"reader was lost, or the answer should go with whatever used to "+
			"call it", kind)
	}
}

// AND EVERY WAKE REASON HAS ENGLISH ON THE OTHER SIDE.
//
// The applier records, per change and per recipient, the ONE reason of twenty
// under which that person heard about it — the fact no commercial tracker
// keeps. It reaches a screen through `work_inbox`, and a reason the client has
// no phrase for renders as its own snake_case value: a log line where a
// sentence belongs, on the surface a person reads first.
//
// This is the `rooms` idiom one level down: the client's table is read from
// ITS OWN SOURCE rather than restated here, so the gate cannot drift towards
// claiming the pair agree. A phrase the client carries for a reason nothing
// writes is checked too — that is how a renamed reason leaves a dead entry
// behind and a live one missing.
func TestEveryWakeReasonReadsAsEnglishOnTheClient(t *testing.T) {
	t.Parallel()
	// FOUND RATHER THAN ADDRESSED — see [declaration]. This table has not
	// moved, but a gate that names a path is one more thing a reorganisation
	// breaks, and it breaks by reporting a drift that did not happen.
	table := declaration(t, `(?s)const PHRASES: Record<[^>]*> = \{(.*?)\n\};`)
	// The table is `key: { short: …, why: … }`, one per line.
	entry := regexp.MustCompile(`(?m)^\s{2}([a-z_]+):\s*\{`)
	phrased := map[string]bool{}
	for _, m := range entry.FindAllStringSubmatch(table, -1) {
		phrased[m[1]] = true
	}
	if len(phrased) == 0 {
		t.Fatal("no phrases were found at all, so this gate certifies nothing")
	}
	for _, reason := range tracker.Reasons {
		if !phrased[string(reason)] {
			t.Errorf("the engine writes %q and the client has no phrase for it, "+
				"so it renders as its own snake_case value on the one screen a "+
				"person reads first", reason)
		}
		delete(phrased, string(reason))
	}
	for leftover := range phrased {
		t.Errorf("the client phrases %q and nothing writes it — a renamed reason "+
			"leaves exactly this behind", leftover)
	}
}

// AND EVERY DIMENSION THE COST AXIS OFFERS IS ONE THE ENGINE ACCEPTS.
//
// `token_series` refuses an unknown `group` naming what it takes, which is the
// right refusal — and it turns a control offering a seventh value into a chart
// that never loads rather than one drawn on the wrong dimension. The screen's
// list and [tokens.Groups] are therefore one closed set written twice, and
// this is the gate that says so.
//
// The client's table is read from ITS OWN SOURCE rather than restated here,
// the `rooms` idiom: a gate carrying its own copy of the list drifts towards
// claiming the pair agree. A value the client offers and the engine dropped is
// checked too — that is how a renamed group leaves a dead control behind.
func TestEveryCostDimensionTheScreenOffersIsOneTheEngineAccepts(t *testing.T) {
	t.Parallel()
	// THE `GROUPS` TABLE ITSELF, not every `{value, label}` pair on the
	// screen: the compare control is the same shape one line away, and a
	// sweep over the whole file read its "previous" as a seventh dimension.
	//
	// FOUND RATHER THAN ADDRESSED. This read the screen at
	// `routes/Spend.tsx` and went red the day the screens were grouped by
	// workspace, reporting a drift between two lists that had not changed.
	// A gate over a constant is a gate over the constant, and the file it
	// happens to sit in is not the subject.
	block := declaration(t, `(?s)const GROUPS = \[(.*?)\] as const;`)
	entry := regexp.MustCompile(`value: "([a-z_]+)"`)
	offered := map[string]bool{}
	for _, m := range entry.FindAllStringSubmatch(block, -1) {
		offered[m[1]] = true
	}
	if len(offered) == 0 {
		t.Fatal("no dimensions were found at all, so this gate certifies nothing")
	}
	for _, group := range tokens.Groups {
		if !offered[string(group)] {
			t.Errorf("the engine buckets by %q and the screen does not offer it, "+
				"so a dimension the company can be read on is unreachable", group)
		}
		delete(offered, string(group))
	}
	for leftover := range offered {
		t.Errorf("the screen offers %q and the engine refuses it, so picking it "+
			"draws no chart at all — a renamed group leaves exactly this behind",
			leftover)
	}
}
