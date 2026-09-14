package queries_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/store"
)

// pinned is the clock these answers run on. Pinned because a lease countdown
// and a next-run projection are both measured against it.
var pinned = time.Date(2026, 8, 23, 16, 0, 0, 0, time.UTC)

// Nothing here is about which third-party apps the fixture names: the org block plus a
// per-seat identity is the shape these answers project, and every third-party app
// carries it identically.
const companyDoc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
integrations:
  mattermost:
    enabled: true
    url: https://mm.example.com
    team: acme
  gitlab:
    enabled: true
    url: https://gitlab.example.com
    signing_secret: "${GL}"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    integrations:
      mattermost:
        bot_token: "${TOK}"
    schedules:
      - name: standup
        cron: "0 9 * * 1-5"
        task: Post the standup
  - name: CTO
    handle: cto
    llm: zulu
`

func company(t *testing.T) *config.Company {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(companyDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cfg
}

// answer runs one question against a registry built from these sources.
func answer(t *testing.T, s queries.Sources, what string, params map[string]any) any {
	t.Helper()
	if s.Now == nil {
		s.Now = func() time.Time { return pinned }
	}
	r := queries.NewRegistry()
	queries.Register(r, s)
	data, err := r.Answer(t.Context(), what, params, "operator")
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return data
}

// asMap round-trips an answer through JSON, which is what a client sees.
func asMap(t *testing.T, data any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return out
}

// --- schedules -------------------------------------------------------------

func TestSchedulesProjectsWhatIsConfigured(t *testing.T) {
	t.Parallel()
	// COMPUTED from the org rather than stored, so a schedule an operator
	// just added shows immediately rather than after its first fire.
	cfg := company(t)
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
	}, "schedules", nil))

	rows, _ := body["schedules"].([]any)
	if len(rows) != 1 {
		t.Fatalf("%d schedules, want the one the company declares: %v", len(rows), body)
	}
	row, _ := rows[0].(map[string]any)
	if row["name"] != "standup" || row["cron"] != "0 9 * * 1-5" {
		t.Errorf("row = %v", row)
	}
	if runners, _ := row["runners"].([]any); len(runners) != 1 || runners[0] != "ceo" {
		t.Errorf("runners = %v, want the seat that owns the schedule", row["runners"])
	}
	// The history is present and empty, not absent: a reader must be able
	// to tell "nothing has fired" from "this answer has no history half".
	if _, present := body["recent_runs"]; !present {
		t.Error("the answer carries no recent_runs key at all")
	}
}

// fakeRuns is a dispatch ledger with fixed contents.
type fakeRuns struct {
	runs []schedule.Run
	err  error

	// asked records the narrowed read's arguments, so a case about the
	// per-schedule listing can prove the identity reached the ledger
	// rather than only that some rows came back.
	asked struct {
		scope   types.ScheduleScope
		scopeID string
		name    string
		limit   int
	}
}

func (f *fakeRuns) Recent(context.Context, int) ([]schedule.Run, error) {
	return f.runs, f.err
}

func (f *fakeRuns) RecentFor(_ context.Context, scope types.ScheduleScope,
	scopeID, name string, limit int) ([]schedule.Run, error) {

	f.asked.scope, f.asked.scopeID = scope, scopeID
	f.asked.name, f.asked.limit = name, limit
	// THE STUB FILTERS TOO, so a case cannot pass by handing back rows
	// the narrowing would have excluded.
	out := []schedule.Run{}
	for _, run := range f.runs {
		if run.Scope == scope && run.ScopeID == scopeID && run.ScheduleName == name {
			out = append(out, run)
		}
	}
	return out, f.err
}

func TestSchedulesCarriesTheDispatchHistory(t *testing.T) {
	t.Parallel()
	cfg := company(t)
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
		Runs: &fakeRuns{runs: []schedule.Run{{
			FireKey: schedule.FireKey{
				Scope: "role", ScopeID: "ceo", ScheduleName: "standup",
				FireLabel: "20260823T0900", TargetHandle: "ceo",
			},
			ScheduledAt: pinned, FiredAt: pinned, Outcome: schedule.OutcomeFired,
			TraceID: "trace-1",
		}}},
	}, "schedules", nil))

	runs, _ := body["recent_runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("%d runs, want 1: %v", len(runs), body)
	}
	run, _ := runs[0].(map[string]any)
	if run["schedule_name"] != "standup" || run["outcome"] != "fired" {
		t.Errorf("run = %v", run)
	}
	if run["trace_id"] != "trace-1" {
		t.Errorf("the run does not link to its turn: %v", run)
	}
}

func TestAnUnreadableHistoryDoesNotBlankTheSchedules(t *testing.T) {
	t.Parallel()
	// The configured schedules are the half an operator opens this screen
	// for. Refusing to show them because the footnote is unreadable would
	// blank the page over its least important part.
	cfg := company(t)
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
		Runs:    &fakeRuns{err: context.DeadlineExceeded},
	}, "schedules", nil))

	if rows, _ := body["schedules"].([]any); len(rows) != 1 {
		t.Errorf("the schedules were lost with the history: %v", body)
	}
	if runs, _ := body["recent_runs"].([]any); len(runs) != 0 {
		t.Errorf("recent_runs = %v, want an empty list", runs)
	}
}

// --- integrations ----------------------------------------------------------

func TestIntegrationsSaysHowEachSurfaceIsWired(t *testing.T) {
	t.Parallel()
	cfg := company(t)
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
	}, "integrations", nil))

	rows, _ := body["integrations"].([]any)
	byKind := map[string]map[string]any{}
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		byKind[entry["key"].(string)] = entry
	}
	for _, kind := range []string{"gitlab", "mattermost"} {
		if _, present := byKind[kind]; !present {
			t.Errorf("%s is configured and absent from the answer", kind)
		}
	}
	if _, present := byKind["jira"]; present {
		t.Error("an integration the company does not configure was reported")
	}
	// LISTED rather than counted: "which seats reach Mattermost" is the
	// question that follows "how many", and the answer is already in the
	// config.
	seats, _ := byKind["mattermost"]["seats"].([]any)
	if len(seats) != 1 || seats[0] != "CEO" {
		t.Errorf("mattermost seats = %v, want the one seat with a bot of its own",
			byKind["mattermost"]["seats"])
	}
}

func TestAMissingSecretIsReportedAsFalseNotAsAbsent(t *testing.T) {
	t.Parallel()
	// THREE-VALUED, and the third value is the point: null means this
	// surface uses no secret at all, false means a route is refusing every
	// delivery, and only an operator can tell those apart.
	cfg := company(t)
	cfg.Integrations.GitLab.SigningSecret = ""
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
	}, "integrations", nil))

	rows, _ := body["integrations"].([]any)
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		if entry["key"] != "gitlab" {
			continue
		}
		present, ok := entry["secret_present"]
		if !ok {
			t.Fatal("the answer omits secret_present entirely")
		}
		if present != false {
			t.Fatalf("secret_present = %v, want false — the route is refusing "+
				"every delivery and that is not the same as using no secret", present)
		}
		return
	}
	t.Fatal("gitlab was not in the answer")
}

func TestASurfaceWithNoSecretReportsNull(t *testing.T) {
	t.Parallel()
	// Mattermost holds one outbound websocket per seat and verifies no
	// inbound delivery, so it has no secret of its own at all; Forge's app
	// id is the JWT AUDIENCE, in every manifest the operator installs.
	// Reporting either as a secret that is present would invite an operator
	// to go looking for the one they had not set.
	cfg := company(t)
	// SET DIRECTLY rather than parsed, because the assertion is about the
	// FIELD: the row is built from it, and it has to answer null however
	// the field got there.
	cfg.Integrations.ForgeAppID = "app-123"
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
	}, "integrations", nil))

	want := map[string]bool{"forge": false, "mattermost": false}
	rows, _ := body["integrations"].([]any)
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		kind, _ := entry["key"].(string)
		if _, wanted := want[kind]; !wanted {
			continue
		}
		want[kind] = true
		value, present := entry["secret_present"]
		if !present {
			t.Fatalf("%s OMITS secret_present, so \"uses no secret\" and "+
				"\"this build forgot to say\" are one answer", kind)
		}
		if value != nil {
			t.Fatalf("%s secret_present = %v, want null for a surface with no secret",
				kind, value)
		}
	}
	for kind, seen := range want {
		if !seen {
			t.Fatalf("%s was not in the answer", kind)
		}
	}
}

// --- fleet -----------------------------------------------------------------

func TestFleetReadsTheLeaseTable(t *testing.T) {
	t.Parallel()
	// /health answers about the node that served it, so behind a load
	// balancer a refresh tells a different story each time. The lease
	// table is the one place that knows which node holds what.
	backend := coordmemory.New()
	claim := func(resource, owner string, meta map[string]any) {
		t.Helper()
		if _, err := backend.TryAcquire(t.Context(), resource, coord.AcquireOptions{
			Owner: owner, TTL: time.Minute, Meta: meta,
		}); err != nil {
			t.Fatal(err)
		}
	}
	claim(coord.NodeResource("node-a"), "node-a:1", map[string]any{
		"roles": []any{"seats", "workers"}, "labels": map[string]any{"zone": "eu"},
	})
	claim(coord.SeatResource("ceo"), "node-a:1", nil)
	claim(coord.WorkerResource("scheduler"), "node-a:1", nil)

	cfg := company(t)
	body := asMap(t, answer(t, queries.Sources{
		Coord: backend, NodeID: "node-a",
		Company: func() *config.Company { return cfg },
	}, "fleet", nil))

	nodes, _ := body["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("%d nodes, want 1: %v", len(nodes), body)
	}
	node, _ := nodes[0].(map[string]any)
	if node["id"] != "node-a" {
		t.Errorf("node = %v", node)
	}
	if node["seats"] != float64(1) {
		t.Errorf("the node holds %v seats, want 1", node["seats"])
	}
	if labels, _ := node["labels"].(map[string]any); labels["zone"] != "eu" {
		t.Errorf("labels = %v, want what the presence lease carries", node["labels"])
	}
	if seats, _ := body["seats"].([]any); len(seats) != 1 {
		t.Errorf("%d seats, want 1", len(seats))
	}
	if duties, _ := body["duties"].([]any); len(duties) != 1 {
		t.Errorf("%d duties, want 1", len(duties))
	}
	if body["this_node"] != "node-a" {
		t.Errorf("this_node = %v", body["this_node"])
	}
}

func TestFleetNamesTheSeatsNoNodeCanRun(t *testing.T) {
	t.Parallel()
	// A seat pinned to a label no node carries is not "unclaimed yet", it
	// is unclaimable — and it stays that way until somebody changes the
	// config or starts a node that matches. A list of leases cannot say
	// that, because the seat has no lease to appear in.
	backend := coordmemory.New()
	if _, err := backend.TryAcquire(t.Context(), coord.NodeResource("node-a"),
		coord.AcquireOptions{Owner: "node-a:1", TTL: time.Minute, Meta: map[string]any{
			"roles": []any{"seats"}, "labels": map[string]any{"zone": "eu"},
		}}); err != nil {
		t.Fatal(err)
	}
	cfg := company(t)
	cfg.Roles[1].Placement = &config.RolePlacement{Labels: map[string]string{"zone": "us"}}

	body := asMap(t, answer(t, queries.Sources{
		Coord: backend, NodeID: "node-a",
		Company: func() *config.Company { return cfg },
	}, "fleet", nil))

	unplaceable, _ := body["unplaceable"].([]any)
	if len(unplaceable) != 1 {
		t.Fatalf("unplaceable = %v, want the seat pinned to a zone no node has", unplaceable)
	}
	entry, _ := unplaceable[0].(map[string]any)
	if entry["handle"] != "cto" {
		t.Errorf("unplaceable = %v", entry)
	}
}

func TestFleetNamesTheRolesNobodyIsRunning(t *testing.T) {
	t.Parallel()
	// A company whose workers role is unmanned still answers webhooks and
	// still runs turns; what it stops doing is every scheduled and
	// background duty, with no error anywhere. That silence is the whole
	// reason this is a field.
	backend := coordmemory.New()
	if _, err := backend.TryAcquire(t.Context(), coord.NodeResource("node-a"),
		coord.AcquireOptions{Owner: "node-a:1", TTL: time.Minute, Meta: map[string]any{
			"roles": []any{"seats"},
		}}); err != nil {
		t.Fatal(err)
	}
	cfg := company(t)
	body := asMap(t, answer(t, queries.Sources{
		Coord: backend, NodeID: "node-a",
		Company: func() *config.Company { return cfg },
	}, "fleet", nil))

	unmanned, _ := body["unmanned_roles"].([]any)
	var names []string
	for _, role := range unmanned {
		names = append(names, role.(string))
	}
	if len(names) != 2 {
		t.Fatalf("unmanned = %v, want ingress and workers", names)
	}
}

// EVERY ANSWER THE ADMIN WORKSPACE DRAWS NEEDS AN OPERATOR CREDENTIAL.
//
// The dashboard's rail marks all five Admin destinations `guarded: true`: it
// draws a lock on the row and its palette says "needs a token". That flag is
// presentation — the only thing that actually refuses is this registry — and
// `fleet` was registered public while its siblings were operator-only, so the
// client promised a guard the server did not keep. On a node with
// `api.allow_anonymous_read` an anonymous GET read the node ids, which node
// held which seat, the lease epochs and the rollout's progress.
//
// The two here are the two this registry gates by these sources;
// Configuration's four are [TestTheConfigQueryIsOperatorOnly] and Credentials
// is `/secrets`, a prefix guarded whole. Named rather than derived, because
// the mapping from a screen to the question it asks lives in each screen's own
// `useQuery` call and no gate can see across the two languages — so the
// registration check below is what stops this list going quiet: a name nothing
// registers fails rather than passing as "gated".
func TestTheAdminWorkspacesAnswersAreOperatorOnly(t *testing.T) {
	t.Parallel()
	cfg := company(t)
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{
		Coord:   coordmemory.New(),
		NodeID:  "node-a",
		Company: func() *config.Company { return cfg },
	})
	for _, what := range []string{"fleet", "integrations"} {
		// REGISTERED AT ALL, first. A question this registry does not
		// hold answers ErrUnknown to everybody, which is not the same
		// fact and would make the assertion below vacuous.
		if !slices.Contains(r.Names(), what) {
			t.Errorf("%s is not registered, so this case asserts nothing about it", what)
			continue
		}
		if !r.RequiresOperator(what) {
			t.Errorf("%s is an Admin answer the rail locks, but the registry "+
				"serves it to any caller", what)
		}
		if _, err := r.Answer(t.Context(), what, nil, ""); !errors.Is(
			err, queries.ErrUnauthorized) {

			t.Errorf("%s answered %v without a credential, want an "+
				"authorization refusal", what, err)
		}
	}
}

// AN UNREACHABLE LEASE TABLE IS "ASK AGAIN", NOT "THE SERVER BROKE".
//
// The fleet question returned the coordination store's error as it came, so a
// store blip reached a client as `query_failed` and a 500: the code a screen
// gives up on, for a condition that clears in seconds. The reference promised
// a 503 the whole time. The coordination contract already says which failures
// are "could not reach the store", and that is the one the registry turns into
// ErrUnavailable.
func TestAnUnreachableLeaseTableIsUnavailableRatherThanFailed(t *testing.T) {
	t.Parallel()
	faulty := coordtest.NewFaulty(coordmemory.New())
	faulty.Break(nil)
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Coord: faulty, NodeID: "node-a"})

	_, err := r.Answer(t.Context(), "fleet", nil, "op-1")
	if !errors.Is(err, queries.ErrUnavailable) {
		t.Fatalf("an unreachable lease table answered %v, want ErrUnavailable", err)
	}
	// The contract's own error survives underneath, for the log.
	if !errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("the coordination error was replaced rather than wrapped: %v", err)
	}
}

// AND A COORDINATION FAILURE THAT IS NOT AN OUTAGE STAYS A FAILURE. A record
// the store returned and this build could not read will not read better in
// five seconds, and a Retry-After would send a screen round a loop.
func TestACoordinationFailureThatIsNotAnOutageIsNotRetried(t *testing.T) {
	t.Parallel()
	faulty := coordtest.NewFaulty(coordmemory.New())
	faulty.Break(errors.New("lease record: unexpected end of JSON input"))
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Coord: faulty})

	_, err := r.Answer(t.Context(), "fleet", nil, "op-1")
	if err == nil {
		t.Fatal("a failed read answered successfully")
	}
	if errors.Is(err, queries.ErrUnavailable) {
		t.Errorf("a decode failure was reported as %v", queries.ErrUnavailable)
	}
}

func TestAQuestionWithNoSourceIsUnknownRatherThanEmpty(t *testing.T) {
	t.Parallel()
	// "Nothing here reads the lease table" and "the fleet is empty" are
	// different answers, and a dashboard that drew the second for the
	// first would report a company with no nodes that has several.
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{})
	for _, what := range []string{
		"fleet", "schedules", "integrations", "conversations",
		"agent_memory", "config", "config_audit", "config_diff",
	} {
		if _, err := r.Answer(t.Context(), what, nil, "operator"); err == nil {
			t.Errorf("%s answered from a registry with no source for it", what)
		}
	}
}

// --- agent memory ----------------------------------------------------------

func TestAgentMemoryServesBothHalves(t *testing.T) {
	t.Parallel()
	// The diary is what a seat chose to remember; the episodes are what it
	// did, summarised. A page showing one without the other reads as a
	// seat with half a history.
	db := openStore(t)
	diary := learning.NewDiary(db)
	episodes := learning.NewEpisodes(db)

	if err := diary.Write(t.Context(), learning.DiaryEntry{
		ID: "d-1", AgentID: "ceo", Kind: learning.DiaryLong,
		Content:   "remember the release window",
		CreatedAt: pinned,
	}); err != nil {
		t.Fatalf("diary: %v", err)
	}
	if _, err := episodes.Append(t.Context(), learning.Episode{
		ID: "ep-1", Handle: "ceo", TaskSummary: "answered the on-call page",
		StartedAt: pinned, EndedAt: pinned.Add(time.Minute),
	}); err != nil {
		t.Fatalf("episode: %v", err)
	}

	body := asMap(t, answer(t, queries.Sources{Diary: diary, Episodes: episodes},
		"agent_memory", map[string]any{"id": "ceo"}))

	if entries, _ := body["diary"].([]any); len(entries) != 1 {
		t.Errorf("diary = %v, want the one entry", body["diary"])
	}
	if entries, _ := body["episodes"].([]any); len(entries) != 1 {
		t.Errorf("episodes = %v, want the one episode", body["episodes"])
	}
}

func TestAgentMemoryOfASeatWithNoneIsEmptyRatherThanAbsent(t *testing.T) {
	t.Parallel()
	// Both keys are always present. A screen that had to tell "no diary
	// half in this answer" from "an empty diary" would be reading the
	// shape of the response to decide what to draw.
	db := openStore(t)
	body := asMap(t, answer(t, queries.Sources{
		Diary: learning.NewDiary(db), Episodes: learning.NewEpisodes(db),
	}, "agent_memory", map[string]any{"id": "nobody"}))

	for _, half := range []string{"diary", "episodes"} {
		value, present := body[half]
		if !present {
			t.Errorf("the answer omits %q entirely", half)
			continue
		}
		if entries, _ := value.([]any); len(entries) != 0 {
			t.Errorf("%s = %v, want an empty list", half, value)
		}
	}
}

func TestAgentMemoryNeedsASeat(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Diary: learning.NewDiary(db)})
	if _, err := r.Answer(t.Context(), "agent_memory", nil, "operator"); err == nil {
		t.Fatal("an agent_memory query with no id was answered")
	}
}

// --- the fleet's config columns --------------------------------------------

func TestFleetCarriesEachNodesConfigState(t *testing.T) {
	t.Parallel()
	// An epoch on its own is a number with nothing to compare it to, and a
	// node that STOPPED reporting is exactly the one an operator is
	// looking for — without the timestamp its stale row is
	// indistinguishable from one written a second ago.
	backend := coordmemory.New()
	if _, err := backend.TryAcquire(t.Context(), coord.NodeResource("node-a"),
		coord.AcquireOptions{Owner: "node-a:1", TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	plane := coordmemory.NewFleet()
	published, err := plane.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "rev-1", Payload: []byte("{}"), At: pinned})
	if err != nil {
		t.Fatal(err)
	}
	if err := plane.RecordApply(t.Context(), coord.NodeApply{
		NodeID: "node-a", Epoch: published.Epoch, RevisionID: "rev-1",
		Status: string(configplane.StatusOK), UpdatedAt: pinned,
	}); err != nil {
		t.Fatal(err)
	}

	body := asMap(t, answer(t, queries.Sources{
		Coord: backend, Plane: plane, NodeID: "node-a",
	}, "fleet", nil))

	if body["target_epoch"] != float64(1) {
		t.Errorf("target_epoch = %v, want the activation pointer", body["target_epoch"])
	}
	nodes, _ := body["nodes"].([]any)
	node, _ := nodes[0].(map[string]any)
	if node["config_epoch"] != float64(1) || node["config_status"] != "ok" {
		t.Errorf("config columns = %v", node)
	}
	reported, present := node["config_reported_at"]
	if !present {
		t.Fatal("the node's row omits config_reported_at, so a node that STOPPED " +
			"reporting is indistinguishable from one that reported a second ago")
	}
	if stamp, _ := reported.(string); stamp == "" {
		t.Errorf("config_reported_at = %v, want the stamp of its last report", reported)
	}
}

func TestAnUnreadableControlPlaneDoesNotBlankTheFleet(t *testing.T) {
	t.Parallel()
	// A fleet answer without the config column is still the answer to
	// "which node holds what", and refusing the whole view because one of
	// its columns is unreadable would blank the screen an operator opens
	// when nodes are dying.
	backend := coordmemory.New()
	if _, err := backend.TryAcquire(t.Context(), coord.NodeResource("node-a"),
		coord.AcquireOptions{Owner: "node-a:1", TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	// A plane that CANNOT be read, which is the case this covers: the
	// config column is unavailable while the lease view is fine.
	body := asMap(t, answer(t, queries.Sources{
		Coord: backend, Plane: brokenPlane{}, NodeID: "node-a",
	}, "fleet", nil))

	nodes, _ := body["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("the node list was lost with the config column: %v", body)
	}
	if body["target_epoch"] != float64(0) {
		t.Errorf("target_epoch = %v, want 0 when it cannot be read", body["target_epoch"])
	}
}

func TestAnUnreadableLeaseTableFailsTheFleetQuery(t *testing.T) {
	t.Parallel()
	// The other direction, and deliberately not symmetric: the lease table
	// IS the fleet. Answering an empty node list would report a company
	// with nothing running, which is the one thing this view must never
	// invent.
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{
		Coord:  brokenCoord(errors.New("store down")),
		NodeID: "node-a",
	})
	if _, err := r.Answer(t.Context(), "fleet", nil, "operator"); err == nil {
		t.Fatal("an unreadable lease table answered a fleet")
	}
}

// brokenCoord is a lease table that cannot be read.
func brokenCoord(err error) coord.Backend {
	faulty := coordtest.NewFaulty(coordmemory.New())
	faulty.Break(err)
	return faulty
}

func TestASeatOnAnIngressOnlyNodeIsStillUnplaceable(t *testing.T) {
	t.Parallel()
	// The placement selector and the node's ROLE are different
	// constraints. A company whose only label-matching node is
	// ingress-only has a seat nothing will ever claim — and a check that
	// looked at labels alone would report it as merely unclaimed.
	backend := coordmemory.New()
	if _, err := backend.TryAcquire(t.Context(), coord.NodeResource("edge"),
		coord.AcquireOptions{Owner: "edge:1", TTL: time.Minute, Meta: map[string]any{
			"roles": []any{"ingress"}, "labels": map[string]any{"zone": "eu"},
		}}); err != nil {
		t.Fatal(err)
	}
	cfg := company(t)
	cfg.Roles[1].Placement = &config.RolePlacement{Labels: map[string]string{"zone": "eu"}}

	body := asMap(t, answer(t, queries.Sources{
		Coord: backend, NodeID: "edge",
		Company: func() *config.Company { return cfg },
	}, "fleet", nil))

	unplaceable, _ := body["unplaceable"].([]any)
	if len(unplaceable) != 2 {
		t.Fatalf("unplaceable = %v, want both seats — the only node that matches "+
			"the label does not run seats", unplaceable)
	}
}

func TestASeatThatIsHeldIsNotReportedUnplaceable(t *testing.T) {
	t.Parallel()
	// The case that makes the claimed check load-bearing: an operator
	// NARROWS a placement while the seat is still held by the node that
	// took it under the old one. The seat is placed — it is running right
	// now — and reporting it as unplaceable would send somebody looking
	// for a fault that does not exist.
	backend := coordmemory.New()
	if _, err := backend.TryAcquire(t.Context(), coord.NodeResource("node-a"),
		coord.AcquireOptions{Owner: "node-a:1", TTL: time.Minute, Meta: map[string]any{
			"roles": []any{"seats"}, "labels": map[string]any{"zone": "eu"},
		}}); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.TryAcquire(t.Context(), coord.SeatResource("cto"),
		coord.AcquireOptions{Owner: "node-a:1", TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	cfg := company(t)
	cfg.Roles[1].Placement = &config.RolePlacement{Labels: map[string]string{"zone": "us"}}

	body := asMap(t, answer(t, queries.Sources{
		Coord: backend, NodeID: "node-a",
		Company: func() *config.Company { return cfg },
	}, "fleet", nil))

	for _, entry := range body["unplaceable"].([]any) {
		row, _ := entry.(map[string]any)
		if row["handle"] == "cto" {
			t.Fatalf("a seat that is HELD right now was reported unplaceable: %v", row)
		}
	}
}

// A NODE'S LIVE STATE REACHES ITS PEERS on the presence heartbeat, which is
// what makes the fleet view answer "where is the work" from any node rather
// than only from the one that served the request.
func TestFleetCarriesEachNodesOwnLiveStatus(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	claim := func(resource, owner string, meta map[string]any) {
		t.Helper()
		if _, err := backend.TryAcquire(t.Context(), resource, coord.AcquireOptions{
			Owner: owner, TTL: time.Minute, Meta: meta,
		}); err != nil {
			t.Fatal(err)
		}
	}
	claim(coord.NodeResource("node-a"), "node-a:1", map[string]any{
		"roles": []any{"seats"},
		coord.StatusKey: coord.NodeStatus{
			InFlight: 4, Posture: "shed",
		}.Meta(),
	})
	// A peer that publishes no status at all: a build older than the field.
	claim(coord.NodeResource("node-b"), "node-b:1", map[string]any{
		"roles": []any{"ingress"},
	})

	cfg := company(t)
	body := asMap(t, answer(t, queries.Sources{
		Coord: backend, NodeID: "node-a",
		Company: func() *config.Company { return cfg },
	}, "fleet", nil))

	nodes, _ := body["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("%d nodes, want 2: %v", len(nodes), body)
	}
	a, _ := nodes[0].(map[string]any)
	if a["in_flight"] != float64(4) || a["posture"] != "shed" {
		t.Errorf("node-a = %v", a)
	}
	if a["draining"] != false {
		t.Errorf("node-a draining = %v, want an explicit false", a["draining"])
	}

	// ABSENT IS NOT ZERO. A confident 0 would draw an idle row for a
	// process that is simply not saying, which is the one thing this
	// surface must never do.
	b, _ := nodes[1].(map[string]any)
	if _, present := b["in_flight"]; present {
		t.Errorf("a node that published no status reported %v in flight", b["in_flight"])
	}
	if _, present := b["posture"]; present {
		t.Errorf("a node that published no status reported a posture: %v", b)
	}
}

// CONFIGURED IS NOT ROUTED, and the answer says which.
//
// A third-party app's webhook route verifies and stores deliveries as soon as its
// config block is present; whether one then wakes a seat needs a parser, and
// this build has parsers for three of the seven. Without this field the two
// render identically — configured, secret present, deliveries arriving — so
// a tracker ingesting hundreds of events that reach nobody looks exactly like
// one that works.
func TestIntegrationsTellsRoutedFromMerelyConfigured(t *testing.T) {
	t.Parallel()
	cfg := company(t)
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
		// The engine names gitlab and nothing else, so mattermost is the
		// configured-but-unrouted side of the comparison.
		Routed: func(context.Context) []string { return []string{"gitlab"} },
	}, "integrations", nil))

	rows, _ := body["integrations"].([]any)
	byKind := map[string]map[string]any{}
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		byKind[entry["key"].(string)] = entry
	}
	if got := byKind["gitlab"]["routes"]; got != true {
		t.Errorf("gitlab routes = %v, want true", got)
	}
	if got := byKind["mattermost"]["routes"]; got != false {
		t.Errorf("mattermost routes = %v, want false — a surface the engine "+
			"registered no parser for ingests and wakes nobody, and that is "+
			"the whole point of the field", got)
	}
}

// CONFIGURED IS NOT RESOLVED EITHER, and the answer says which.
//
// A secret lives in the config as a ${VAR}. secret_present says an operator
// wrote one down; only this process knows what it resolved to. The gap is
// the failure that hides everywhere else: an unset variable renders as a
// secret present, the third-party app's settings page shows a healthy hook, and the
// route answers 503 to every delivery with nothing naming the variable.
func TestIntegrationsTellsAResolvedSecretFromAConfiguredOne(t *testing.T) {
	t.Parallel()
	// Its own document rather than the shared fixture: the comparison needs
	// two secret-bearing surfaces, one resolved and one not.
	cfg, err := config.ParseCompany([]byte(`
name: Acme
integrations:
  mattermost: {enabled: true, url: https://mm.example.com, team: acme}
  gitlab: {enabled: true, url: https://gitlab.example.com, signing_secret: "${GL}"}
  jira: {url: https://jira.example.com, token: t, webhook_secret: "${JR}"}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
		// The engine resolved gitlab's secret and not jira's — which is
		// exactly what an unset ${VAR} on one of them looks like.
		Verifiable: func(context.Context) []string { return []string{"gitlab"} },
	}, "integrations", nil))

	byKind := map[string]map[string]any{}
	rows, _ := body["integrations"].([]any)
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		byKind[entry["key"].(string)] = entry
	}
	if got := byKind["gitlab"]["secret_usable"]; got != true {
		t.Errorf("gitlab secret_usable = %v, want true", got)
	}
	if got := byKind["jira"]["secret_present"]; got != true {
		t.Errorf("jira secret_present = %v — the fixture must configure one, "+
			"or the comparison below proves nothing", got)
	}
	if got := byKind["jira"]["secret_usable"]; got != false {
		t.Errorf("jira secret_usable = %v, want false — its secret is written "+
			"down and this process could not resolve it, which is a route "+
			"refusing every delivery", got)
	}
	// A surface with no secret at all stays null on BOTH: it has nothing to
	// resolve, and "no secret here" must not read as "a secret that failed".
	if got := byKind["mattermost"]["secret_usable"]; got != nil {
		t.Errorf("mattermost secret_usable = %v, want null", got)
	}
}

// AND A NODE THAT HAS RESOLVED NOTHING YET DOES NOT GUESS.
func TestANodeThatCannotSayWhatResolvedAnswersNull(t *testing.T) {
	t.Parallel()
	cfg := company(t)
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
		// No Verifiable: the shape of a node whose engine has not started
		// its notification service yet.
	}, "integrations", nil))

	rows, _ := body["integrations"].([]any)
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		usable, present := entry["secret_usable"]
		if !present {
			t.Fatalf("%v omits secret_usable entirely; null and absent are "+
				"different to a client", entry["key"])
		}
		if usable != nil {
			t.Fatalf("%v secret_usable = %v, want null — this process cannot "+
				"resolve anything, which is not a claim that the secret failed",
				entry["key"], usable)
		}
	}
}

// NOT KNOWING IS NOT KNOWING NOTHING.
//
// A node whose notification service has not started cannot say which parsers
// registered, so it answers null rather than false. Reporting false would tell
// an operator their integrations are broken over a window that closes on its
// own.
func TestANodeThatCannotSayWhatRoutesAnswersNull(t *testing.T) {
	t.Parallel()
	cfg := company(t)
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
		// No Routed: nothing has registered a parser yet.
	}, "integrations", nil))

	rows, _ := body["integrations"].([]any)
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		routes, present := entry["routes"]
		if !present {
			t.Fatalf("%v omits routes entirely; null and absent are different "+
				"to a client, and this must be null", entry["key"])
		}
		if routes != nil {
			t.Fatalf("%v routes = %v, want null — this process cannot see an "+
				"engine, which is not the same as nothing routing",
				entry["key"], routes)
		}
	}
}

// AN ENGINE THAT ROUTES NOTHING SAYS SO, rather than reading as unknown.
//
// The empty-but-not-nil case: notifications started and no third-party app registered.
// That is a real measurement and must not collapse into "cannot say".
func TestAnEngineRoutingNothingIsNotUnknown(t *testing.T) {
	t.Parallel()
	cfg := company(t)
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
		Routed:  func(context.Context) []string { return []string{} },
	}, "integrations", nil))

	rows, _ := body["integrations"].([]any)
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		if got := entry["routes"]; got != false {
			t.Fatalf("%v routes = %v, want false — the engine answered, and "+
				"its answer was that nothing routes", entry["key"], got)
		}
	}
}

// THE ROOM READS WHAT THIS ANSWER SENDS.
//
// Nothing linked the two. The Integrations room was written against a payload
// of `key` / `enabled` / `inbound_path` / `last_at` and a top-level
// `traffic_known`, and the server sent `kind` / `configured` / `events` —
// so every card rendered an unbranded badge, a permanent "disabled" chip and
// a blank inbound path, and both sides' tests passed, because each was
// checked against its own idea of the other.
//
// This reads the FIELD NAMES out of the client's own declaration and asserts
// the answer carries each. It is the cheap half of the gate `internal/e2e`
// gives the push protocol; the query channel had none at all.
//
// TWICE NOW THIS GATE HAS POINTED AT A PATH THAT DOES NOT EXIST, so it is
// worth saying what it reads and why. It read
// `static/dashboard/js/views/integrations.js` — the hand-written bundle the
// React rewrite deleted — so os.ReadFile failed, the t.Skipf below it fired,
// and this certified NOTHING on every machine and in CI for the whole of that
// rewrite while reporting a pass. [rooms_test.go] records the identical bug
// being found and fixed in this same package; this file was missed, and
// nothing noticed, because nothing counted skips. So: the source, never the
// build output, and a missing file is FATAL — a broken checkout is not a
// reason to certify nothing.
//
// It reads the TYPE rather than the room's access sites, and that is the
// second lesson. The old sweep matched `row.<field>` and `data.<field>`
// literally. Integrations.tsx destructures (`const { data } = useQuery(…)`,
// then `data?.integrations`) and names a row `r` inside its map callback, so
// the `data.` half matched nothing at all and the `row.` half missed `r.key`
// — a sweep whose whole job is to notice a missing name, silently narrowing
// to whichever identifiers one file happened to use. A declared interface is
// ONE place, and a field added there without a server that sends it fails
// here.
func TestTheIntegrationsRoomReadsWhatThisAnswerSends(t *testing.T) {
	t.Parallel()
	// dashboardTree is rooms_test.go's, for the reason its comment gives:
	// the source tree, not the build output.
	typesPath := filepath.Join(dashboardTree, "protocol", "types.ts")
	source, err := os.ReadFile(typesPath)
	if err != nil {
		t.Fatalf("read %s: %v — this gate cannot run without the client's "+
			"declaration, and skipping would certify nothing while reporting "+
			"a pass", typesPath, err)
	}

	// EVERY third-party app, because the per-integration detail fields (url,
	// seats) only appear on the rows that have them: a fixture missing one
	// reports its field as a mismatch that is really a gap in the fixture.
	cfg := company(t)
	cfg.Integrations.Jira = &config.Jira{
		URL: "https://jira.example.com", Token: "t", WebhookSecret: "jr",
	}
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
		Routed:  func(context.Context) []string { return []string{"gitlab"} },
	}, "integrations", nil))

	rows, _ := body["integrations"].([]any)
	if len(rows) == 0 {
		t.Fatal("the answer carried no integrations, so this proves nothing")
	}
	// TWO READINGS, because the two halves of the contract are different.
	//
	// A REQUIRED field is required of EVERY row — the TypeScript interface
	// describes each element, not the set — so it is checked per entry. The
	// union was wrong for this: one integration carrying `endpoint` made the
	// gate accept another that omitted it, which is precisely the card
	// rendering undefined that this exists to catch.
	//
	// An OPTIONAL field is the other way round: `url` and `seats` are
	// per-integration detail, so a field carried by ANY row is one the answer
	// knows how to send, and only a field NO row carries is worth reporting.
	sent := map[string]bool{}
	for _, r := range rows {
		entry, _ := r.(map[string]any)
		for field := range entry {
			sent[field] = true
		}
	}

	// Every field the client declares on a row, and on the answer around it.
	for field, required := range declaredFields(t, string(source), "IntegrationRow") {
		if required {
			for _, r := range rows {
				entry, _ := r.(map[string]any)
				if _, ok := entry[field]; !ok {
					t.Errorf("IntegrationRow declares %s as REQUIRED and the row "+
						"for %v does not send it — that field renders as undefined "+
						"on that card", field, entry["key"])
				}
			}
			continue
		}
		switch {
		case sent[field]:
		default:
			// An optional field no row carries is within the type's contract,
			// so it is not a failure — but it is either dead client code or a
			// server that stopped sending something, and both are worth
			// seeing. The required half above is what fails.
			t.Logf("IntegrationRow declares %s (optional) and no row carries it", field)
		}
	}
	for field, required := range declaredFields(t, string(source), "IntegrationsAnswer") {
		if _, ok := body[field]; !ok && required {
			t.Errorf("IntegrationsAnswer declares %s as REQUIRED and the "+
				"answer never sends it", field)
		}
	}
}

// declaredFields returns the fields of one TypeScript interface, mapped to
// whether the client declares them REQUIRED (no `?`).
//
// Deliberately a small parser over the declaration rather than a sweep of
// access sites: an interface states the contract once, where a `row.x` /
// `r.x` / destructured-`x` sweep states it as many times as the room has
// spellings and silently covers only the spellings it guessed.
func declaredFields(t *testing.T, source, iface string) map[string]bool {
	t.Helper()

	start := regexp.MustCompile(`(?m)^export interface ` + iface + ` \{$`).FindStringIndex(source)
	if start == nil {
		t.Fatalf("no `export interface %s` in the client's protocol types — "+
			"it was renamed or removed, and this gate is asserting about nothing", iface)
	}
	end := regexp.MustCompile(`(?m)^\}$`).FindStringIndex(source[start[1]:])
	if end == nil {
		t.Fatalf("interface %s is not closed at column 0", iface)
	}
	body := source[start[1] : start[1]+end[0]]

	// A field line, at one level of indentation: `name?: type;`. The leading
	// `^  ` anchors to the interface's own fields, so a nested object literal
	// contributes nothing; the `[a-z_]` class excludes the `[key: string]:
	// unknown` index signature, which is not a field anybody reads by name.
	field := regexp.MustCompile(`(?m)^  ([a-z][a-z0-9_]*)(\??):`)
	out := map[string]bool{}
	for _, m := range field.FindAllStringSubmatch(body, -1) {
		out[m[1]] = m[2] == ""
	}
	if len(out) == 0 {
		t.Fatalf("interface %s declared no fields this could read; the shape "+
			"of the declaration changed and this gate stopped asserting", iface)
	}
	return out
}

// brokenPlane is a config plane that answers nothing, for the case where the
// fleet view has to survive one of its columns being unreadable.
type brokenPlane struct{}

func (brokenPlane) Payload(context.Context, string) ([]byte, bool, error) {
	return nil, false, errUnreadablePlane
}

func (brokenPlane) Activate(context.Context, coord.ActivationRequest) (coord.Activation, error) {
	return coord.Activation{}, errUnreadablePlane
}

func (brokenPlane) Target(context.Context) (coord.Activation, bool, error) {
	return coord.Activation{}, false, errUnreadablePlane
}

func (brokenPlane) RecordApply(context.Context, coord.NodeApply) error { return errUnreadablePlane }

func (brokenPlane) Fleet(context.Context) ([]coord.NodeApply, error) { return nil, errUnreadablePlane }

var errUnreadablePlane = errors.New("the coordination plane is unreachable")

// THE THREE NUMBERS ANSWER ONE QUESTION TOGETHER. "128 arrived" alone cannot
// tell a working integration from one whose every delivery reaches nobody;
// "128 arrived, 30 the routing gate dropped, 2 merges" can.
func TestIntegrationsCountsWhatBecameOfTheDeliveries(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	now := time.Now().UTC().Add(-time.Hour)
	write := func(id, kind, source string) {
		t.Helper()
		if err := log.Append(t.Context(), store.EventRecord{
			ID: id, Type: kind, Time: now, Source: "engine",
			Category: "notification", Summary: kind,
			Tags: map[string]string{"notification_source": source},
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// Two inbound deliveries on the edge's own category…
	for i, src := range []string{"gitlab", "gitlab", "mattermost"} {
		if err := log.Append(t.Context(), store.EventRecord{
			ID: "w" + strconv.Itoa(i), Type: "webhook:push", Time: now,
			Source: src, Category: "webhook", Summary: "push",
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// …and what became of them, on the engine's.
	write("s1", "notification_skipped", "gitlab")
	write("s2", "notification_skipped", "gitlab")
	write("c1", "notifications_coalesced", "mattermost")

	cfg := company(t)
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg }, Events: log,
	}, "integrations", nil))
	rows, _ := body["integrations"].([]any)
	byKind := map[string]map[string]any{}
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		byKind[entry["key"].(string)] = entry
	}
	if got := byKind["gitlab"]["skipped"]; got != float64(2) {
		t.Errorf("gitlab skipped = %v, want 2", got)
	}
	if got := byKind["gitlab"]["coalesced"]; got != float64(0) {
		t.Errorf("gitlab coalesced = %v, want 0", got)
	}
	if got := byKind["mattermost"]["coalesced"]; got != float64(1) {
		t.Errorf("mattermost coalesced = %v, want 1", got)
	}
	// The inbound count is unchanged and still comes from the edge's rows.
	if got := byKind["gitlab"]["inbound"]; got != float64(2) {
		t.Errorf("gitlab inbound = %v, want 2", got)
	}
}

// NULL, NOT ZERO, when nothing was counted. An answer with no event log to
// read cannot say how many deliveries were dropped, and reporting 0 would tell
// an operator every one of them woke a seat.
func TestUncountedOutcomesAreNullRatherThanZero(t *testing.T) {
	t.Parallel()
	cfg := company(t)
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
	}, "integrations", nil))
	rows, _ := body["integrations"].([]any)
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		for _, field := range []string{"skipped", "coalesced"} {
			if entry[field] != nil {
				t.Errorf("%s %s = %v, want null with no event log to read",
					entry["key"], field, entry[field])
			}
		}
	}
}

// AN UNREADABLE EVENT LOG REPORTS NULL, NOT ZERO: the same rule as an answer
// with no log to read at all, and for the same reason. A zero that means
// "could not tell" is the number an operator would act on.
//
// A closed store fails the FIRST listing, so this covers the outer guard. The
// narrower one inside countOutcomes applies the identical rule to a second
// listing that fails on its own, which needs a transient store error this
// suite has no way to stage — it is defence in depth rather than a separate
// contract.
func TestAnUnreadableEventLogReportsNullOutcomes(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	// A closed store: every listing fails from here on.
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	cfg := company(t)
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg }, Events: log,
	}, "integrations", nil))
	rows, _ := body["integrations"].([]any)
	if len(rows) == 0 {
		t.Fatal("no integrations answered")
	}
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		for _, field := range []string{"skipped", "coalesced"} {
			if entry[field] != nil {
				t.Errorf("%s %s = %v, want null when the listing failed",
					entry["key"], field, entry[field])
			}
		}
	}
}

// THE THREE-VALUED RECONCILE FIELD, and the third value is the one that took
// a subsystem to be able to say at all.
//
// A node that could not read the fleet's rows has no findings to report, and
// rendering that as "no surface has been reconciled" would put an alarming
// claim on a screen over a store that was briefly unreachable. So null is
// "cannot say", an absent entry is "the loop has not reached this surface
// yet", and a present one is a real finding.
func TestIntegrationsCarriesWhatTheReconcileLoopFound(t *testing.T) {
	t.Parallel()
	cfg := company(t)

	t.Run("cannot say", func(t *testing.T) {
		t.Parallel()
		rows := integrationRows(t, queries.Sources{
			Company: func() *config.Company { return cfg },
		})
		for kind, row := range rows {
			if got, present := row["reconcile"]; !present || got != nil {
				t.Errorf("%s reconcile = %v, want null with no loop "+
					"findings to read", kind, got)
			}
		}
	})

	t.Run("a finding", func(t *testing.T) {
		t.Parallel()
		rows := integrationRows(t, queries.Sources{
			Company: func() *config.Company { return cfg },
			Reconciles: func(context.Context) []integration.State {
				return []integration.State{{
					Kind: integration.KindGitLab,
					Report: integration.Report{
						Phase: integration.PhaseDegraded, Actor: integration.ActorAdmin,
						Detail:    "ceo needs maintainer on api-gateway",
						ActionURL: "https://gitlab.example.com/api-gateway/-/settings",
					},
					Findings: []integration.Finding{
						{Kind: integration.FindingGrantShort, Subject: "ceo"},
						{Kind: integration.FindingGrantExcess, Subject: "cto"},
					},
					Outcome:  integration.OutcomeBlocked,
					Attempts: 2,
				}}
			},
		})

		got, _ := rows["gitlab"]["reconcile"].(map[string]any)
		if got == nil {
			t.Fatalf("gitlab carries no reconcile status: %v", rows["gitlab"]["reconcile"])
		}
		if got["phase"] != "degraded" || got["actor"] != "admin" {
			t.Errorf("phase/actor = %v/%v, want degraded/admin", got["phase"], got["actor"])
		}
		if got["detail"] != "ceo needs maintainer on api-gateway" {
			t.Errorf("detail = %v", got["detail"])
		}
		// THE FINDINGS TRAVEL TOO, and not only the one the report
		// promoted: the report says what to do next and the findings say
		// what is actually wrong, and an operator who fixes the first
		// should not wait a full pass to learn there was a second.
		findings, _ := got["findings"].([]any)
		if len(findings) != 2 {
			t.Fatalf("carried %d findings, want both", len(findings))
		}
		// AND EACH CARRIES ITS OWN VERDICT, which is what stops a reader
		// keeping a second copy of the closed set.
		//
		// These two findings are the discriminating pair: grant_short is a
		// real problem owed by an admin, grant_excess is an ADVISORY on a
		// working integration — same subject shape, same wire shape, and
		// nothing but the kind string to tell them apart until now. The
		// dashboard guessed, treating any finding naming an agent as that
		// agent not working, so one spare permission badged a healthy agent
		// amber underneath a card reading Connected.
		verdicts := map[string][2]string{}
		for _, raw := range findings {
			f, _ := raw.(map[string]any)
			kind, _ := f["kind"].(string)
			phase, _ := f["phase"].(string)
			actor, _ := f["actor"].(string)
			verdicts[kind] = [2]string{phase, actor}
		}
		if got := verdicts["grant_short"]; got != [2]string{"degraded", "admin"} {
			t.Errorf("grant_short verdict = %v, want degraded/admin", got)
		}
		if got := verdicts["grant_excess"]; got != [2]string{"ready", "admin"} {
			t.Errorf("grant_excess verdict = %v, want ready/admin — an advisory "+
				"that reads as a fault reports a working agent as broken", got)
		}
		// A surface the loop has not reached is absent rather than
		// asserted, which is not the same as the process being unable to
		// say: the field is null either way, and only the presence of
		// OTHER rows' answers tells them apart.
		if got := rows["mattermost"]["reconcile"]; got != nil {
			t.Errorf("mattermost reconcile = %v, want null until the loop reaches it", got)
		}
	})
}

// integrationRows answers the question and indexes the rows by surface.
func integrationRows(t *testing.T, sources queries.Sources) map[string]map[string]any {
	t.Helper()
	body := asMap(t, answer(t, sources, "integrations", nil))
	rows, _ := body["integrations"].([]any)
	out := map[string]map[string]any{}
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		out[entry["key"].(string)] = entry
	}
	return out
}

// A SEAT IN A UNIT IS A SEAT.
//
// `roles:` at the top level is the seats belonging to NO unit, and a company
// of any size puts its agents in units instead. Listing only the top level
// answered an empty seat list, and a zero count, for every one of them: the
// Slack row then reported no per-seat app on a company running seven, and its
// secret_present was computed from a count that was always zero while the
// routes verified fine. The walk is company.EachRole, which is exported for
// exactly this and whose own doc records the first time a top-level-only
// lookup shipped.
func TestSeatsInUnitsAreReported(t *testing.T) {
	t.Parallel()
	const doc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
integrations:
  mattermost:
    enabled: true
    url: https://mm.example.com
    team: acme
  slack:
    typing_status: addressed
units:
  - name: Engineering
    roles:
      - name: SWE
        handle: swe
        llm: zulu
        integrations:
          mattermost:
            bot_token: "${MM}"
          slack:
            bot_token: "${BOT}"
            signing_secret: "${SIG}"
    children:
      - name: Platform
        roles:
          - name: SRE
            handle: sre
            llm: zulu
            integrations:
              mattermost:
                bot_token: "${MM2}"
`
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
	}, "integrations", nil))

	rows, _ := body["integrations"].([]any)
	byKind := map[string]map[string]any{}
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		byKind[entry["key"].(string)] = entry
	}

	// A seat one unit deep and a seat two deep, both carrying their own
	// identity, and neither at the top level.
	seats, _ := byKind["mattermost"]["seats"].([]any)
	if len(seats) != 2 {
		t.Fatalf("mattermost seats = %v, want the two seats in units", seats)
	}
	if seats[0] != "SRE" || seats[1] != "SWE" {
		t.Errorf("mattermost seats = %v, want them sorted", seats)
	}

	// And the Slack count, which decides whether the row claims a secret at
	// all, sees the one seat that has an app.
	if present := byKind["slack"]["secret_present"]; present != true {
		t.Errorf("slack secret_present = %v, want true: one seat in a unit "+
			"carries a signing secret", present)
	}
}

// A SURFACE'S REGISTRATION IS COMPARED WITH THE ADDRESS IN FORCE.
//
// A company's public base moves, and a registration made against the old one
// keeps pointing somewhere that no longer answers. Where a pass registers the
// hook the next tick moves it; where nothing does, only a person can, and
// this is the only thing that can tell them.
func TestAMovedPublicBaseIsReportedPerSurface(t *testing.T) {
	t.Parallel()
	cfg := company(t)
	cfg.Integrations.PublicBaseURL = "https://now.example.com"
	// SLACK IS THE SURFACE THIS IS FOR: its Request URL is a field a person
	// typed at the third-party app, so nothing converges it and the
	// comparison is the only thing that can say the address moved.
	cfg.Integrations.Slack = &config.Slack{}
	body := asMap(t, answer(t, queries.Sources{
		Company:    func() *config.Company { return cfg },
		PublicBase: func() string { return "https://now.example.com" },
		Reconciles: func(context.Context) []integration.State {
			return []integration.State{
				{Kind: integration.KindSlack, Endpoint: "https://old.example.com"},
				{Kind: integration.KindGitLab, Endpoint: "https://now.example.com"},
			}
		},
	}, "integrations", nil))

	rows, _ := body["integrations"].([]any)
	byKind := map[string]map[string]any{}
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		byKind[entry["key"].(string)] = entry
	}
	if got := byKind["slack"]["endpoint_current"]; got != false {
		t.Errorf("slack endpoint_current = %v, want false: it is registered elsewhere", got)
	}
	if got := byKind["slack"]["endpoint"]; got != "https://old.example.com" {
		t.Errorf("slack endpoint = %v, want the address it is registered at", got)
	}
	// AND THE ROW CARRYING ONLY THAT ADDRESS IS NOT A REPORT. It was written
	// by the setup write, not by a pass, and rendering it as one gave the
	// card an EMPTY phase as its status: Slack drew no state at all, on the
	// one surface whose moved address only a person can put right.
	if got := byKind["slack"]["reconcile"]; got != nil {
		t.Errorf("slack reconcile = %v, want null: no pass has reported on it", got)
	}
	if got := byKind["gitlab"]["endpoint_current"]; got != true {
		t.Errorf("gitlab endpoint_current = %v, want true", got)
	}
}

// A ${VAR} PUBLIC BASE IS COMPARED RESOLVED, ON BOTH SIDES.
//
// `public_base_url` may be a whole reference, and what a surface registered is
// the address that reference RESOLVED to. Comparing a registration against the
// reference itself never matches, so every company writing one would read
// "the address moved" on every surface, for ever — an action-needed badge
// nobody can clear, on deployments that are working perfectly.
func TestAReferencePublicBaseIsComparedResolved(t *testing.T) {
	t.Parallel()
	cfg := company(t)
	cfg.Integrations.PublicBaseURL = "${PUBLIC_URL}"
	cfg.Integrations.Slack = &config.Slack{}
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
		// What the node's own chain reads the reference as, which is what
		// the passes registered with.
		PublicBase: func() string { return "https://now.example.com" },
		Reconciles: func(context.Context) []integration.State {
			return []integration.State{
				{Kind: integration.KindSlack, Endpoint: "https://now.example.com"},
			}
		},
	}, "integrations", nil))

	rows, _ := body["integrations"].([]any)
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		if entry["key"] != "slack" {
			continue
		}
		if got := entry["endpoint_current"]; got != true {
			t.Errorf("endpoint_current = %v, want true: the registration holds "+
				"exactly what this node reads the reference as", got)
		}
		return
	}
	t.Fatal("no slack row")
}

// AND A PROCESS THAT CANNOT READ THE ADDRESS SAYS NOTHING, rather than false.
//
// A node that cannot read the address cannot know what the current one is.
// Answering false there would report every registration as stale on the
// strength of a value this node never had, which is the same three-valued rule
// Routed, Verifiable and Reconciles already follow.
func TestAnUnknowablePublicBaseLeavesTheAnswerNull(t *testing.T) {
	t.Parallel()
	cfg := company(t)
	cfg.Integrations.PublicBaseURL = "https://now.example.com"
	cfg.Integrations.Slack = &config.Slack{}
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
		// PublicBase deliberately unset: this process cannot say.
		Reconciles: func(context.Context) []integration.State {
			return []integration.State{
				{Kind: integration.KindSlack, Endpoint: "https://now.example.com"},
			}
		},
	}, "integrations", nil))

	rows, _ := body["integrations"].([]any)
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		if entry["key"] != "slack" {
			continue
		}
		if got := entry["endpoint_current"]; got != nil {
			t.Errorf("endpoint_current = %v, want null: this process has no "+
				"resolution chain and cannot say what the address is", got)
		}
		if got := entry["endpoint"]; got != "https://now.example.com" {
			t.Errorf("endpoint = %v, want the recorded address regardless", got)
		}
		return
	}
	t.Fatal("no slack row")
}

// AND A SURFACE NOTHING HAS RECORDED AN ADDRESS FOR SAYS NOTHING.
//
// Null is "cannot say", which is what every row said before this existed and
// what a surface says before it is ever set up. Reported as false it would
// put an action-needed badge on every integration in a fresh company.
func TestAnUnrecordedEndpointIsNullRatherThanMoved(t *testing.T) {
	t.Parallel()
	cfg := company(t)
	cfg.Integrations.PublicBaseURL = "https://now.example.com"
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
		Reconciles: func(context.Context) []integration.State {
			return []integration.State{{Kind: integration.KindGitLab}}
		},
	}, "integrations", nil))

	rows, _ := body["integrations"].([]any)
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		if got := entry["endpoint_current"]; got != nil {
			t.Errorf("%v endpoint_current = %v, want null", entry["key"], got)
		}
	}
}

// EVERY SURFACE THE LOOP CAN REPORT ON HAS A ROW HERE.
//
// The answer is a hand-written ladder, one branch per vendor, and the loop
// writes a status row for every `integration.Kind`. When the two drift the
// symptom is silence: the reconcile records what it found and the one screen
// an operator watches has nowhere to draw it. Atlassian was exactly that — a
// registered pass, a status row, and no row on the card.
//
// DERIVED FROM integration.Kinds rather than from a list beside it, because a
// hardcoded list in the test is how this happened the first time: the guard
// that was supposed to catch a vendor shipping without its wiring carried its
// own copy of the vendors.
func TestEveryIntegrationKindCanBeReported(t *testing.T) {
	t.Parallel()
	cfg := company(t)
	// Every block present, so each ladder branch is reachable. The values
	// only have to be non-nil: what is asserted is that a row appears.
	cfg.Integrations.Slack = &config.Slack{}
	cfg.Integrations.Mattermost = &config.Mattermost{Enabled: true, URL: "https://chat.example.com"}
	cfg.Integrations.GitHub = &config.GitHub{Enabled: true}
	cfg.Integrations.GitLab = &config.GitLab{Enabled: true}
	cfg.Integrations.Jira = &config.Jira{}
	cfg.Integrations.Confluence = &config.Confluence{}
	cfg.Integrations.Datadog = &config.Datadog{Enabled: true, RouteTo: "sre-lead"}
	cfg.Integrations.Atlassian = &config.Atlassian{OrgID: "acme"}

	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
	}, "integrations", nil))

	rows, _ := body["integrations"].([]any)
	seen := map[string]bool{}
	for _, row := range rows {
		if entry, ok := row.(map[string]any); ok {
			seen[entry["key"].(string)] = true
		}
	}
	for _, kind := range integration.Kinds {
		if !seen[string(kind)] {
			t.Errorf("%s is a surface the reconcile loop reports on, and the "+
				"integrations answer has no row for it", kind)
		}
	}
}

// THE ORGANIZATION IS NOT AN INBOUND SURFACE, AND ITS ROW MUST NOT PRETEND TO
// BE ONE.
//
// Atlassian is where an agent's ACCOUNT is created; the products that account
// works in are Jira and Confluence, each with its own webhook and its own
// parser. Nothing is ever addressed to the organization, so it has no delivery
// to verify and nothing to route.
//
// The row passed the organization API key as the `secret` argument — a
// PROVISIONING credential where a delivery-verification secret belongs — so it
// answered `secret_usable: false` (nothing verifies atlassian, because nothing
// needs to) and `routes: false` (no parser routes it, because nothing
// arrives). The dashboard drew those as "secret unresolved — every delivery is
// refused" and "routes nowhere", on an organization whose key had just created
// every agent's account. Both are null now: "not applicable", which is what
// the three-valued contract is for and what the screen already hides.
func TestTheAtlassianOrganizationClaimsNoIngress(t *testing.T) {
	t.Parallel()
	cfg := company(t)
	cfg.Integrations.Atlassian = &config.Atlassian{
		OrgID: "f1240761-c455-41b5-a7f5-4a64f9c6e729", APIKey: "${ATLASSIAN_API_KEY}",
	}
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
		// BOTH KNOWN, so a null here is this surface's own answer rather
		// than a process that could not say: with these nil every row
		// reports null and the test would pass over the old code too.
		Routed:     func(context.Context) []string { return []string{"jira"} },
		Verifiable: func(context.Context) []string { return []string{"jira"} },
	}, "integrations", nil))

	rows, _ := body["integrations"].([]any)
	var org map[string]any
	for _, row := range rows {
		if entry, _ := row.(map[string]any); entry["key"] == "atlassian" {
			org = entry
		}
	}
	if org == nil {
		t.Fatal("the Atlassian organization has no row at all")
	}
	for _, field := range []string{"secret_present", "secret_usable", "routes"} {
		if got := org[field]; got != nil {
			t.Errorf("atlassian %s = %v, want null: the organization receives no "+
				"delivery, so it has none to verify and none to route", field, got)
		}
	}
	// AND IT ADVERTISES NO ADDRESS. The row's default was `webhook` at
	// `/webhooks/<kind>`, so the organization named `/webhooks/atlassian`
	// — a route webhooks.go does not register — as the address to check a
	// settings page against.
	for _, field := range []string{"inbound_kind", "inbound_path"} {
		if got := org[field]; got != nil {
			t.Errorf("atlassian %s = %v, want null: nothing is ever addressed "+
				"to the organization, and no such route is served", field, got)
		}
	}
}

// THE FORGE RELAY ROUTES AS THE PRODUCT IT RELAYS, and the row has to answer
// for that rather than for its own name.
//
// A Cloud event the Forge app relays is republished as `jira` or `confluence`
// and parsed by that product's parser, so no parser is ever registered under
// `forge`. Asked about itself the relay answered `routes: false` on every
// Cloud deployment for ever — and the dashboard groups it under the Atlassian
// row, so a tenant whose relay was feeding both products correctly carried a
// permanent "Forge relay — routes nowhere" beside the two rows saying they
// routed fine.
func TestTheForgeRelayRoutesAsTheProductItRelays(t *testing.T) {
	t.Parallel()
	forgeRow := func(t *testing.T, routed []string) map[string]any {
		t.Helper()
		cfg := company(t)
		cfg.Integrations.ForgeAppID = "ari:cloud:ecosystem::app/a1b2"
		body := asMap(t, answer(t, queries.Sources{
			Company: func() *config.Company { return cfg },
			Routed:  func(context.Context) []string { return routed },
		}, "integrations", nil))
		rows, _ := body["integrations"].([]any)
		for _, row := range rows {
			if entry, _ := row.(map[string]any); entry["key"] == "forge" {
				return entry
			}
		}
		t.Fatal("the Forge relay has no row at all")
		return nil
	}

	// The relay's own name is in nothing and never will be: this is the
	// answer the old code gave for every Cloud company.
	if got := forgeRow(t, []string{"jira", "confluence"})["routes"]; got != true {
		t.Errorf("forge routes = %v with both products parsed, want true: a "+
			"relayed event is published as the product it belongs to", got)
	}
	// ONE PRODUCT IS ENOUGH, because a relayed event is one or the other and
	// the finer answer is on those two rows, immediately below this one.
	if got := forgeRow(t, []string{"confluence"})["routes"]; got != true {
		t.Errorf("forge routes = %v with Confluence parsed, want true", got)
	}
	// AND FALSE IS STILL REACHABLE, which is what stops this being "null
	// anything awkward": with neither product parsed, a relayed delivery
	// really does reach nobody.
	if got := forgeRow(t, []string{"slack"})["routes"]; got != false {
		t.Errorf("forge routes = %v with neither product parsed, want false", got)
	}
}

// AND MATTERMOST STILL ANSWERS, which is the half that makes the rule a rule
// rather than "null anything with no inbound address".
//
// Mattermost has no address either — the engine DIALS OUT — but it then
// receives everything said in its team, and whether a parser turns that into
// work for a seat is a real question with a real answer. Kind.Ingress cannot
// tell the two apart; Kind.Ingests can.
func TestMattermostStillReportsWhetherItRoutes(t *testing.T) {
	t.Parallel()
	cfg := company(t)
	cfg.Integrations.Mattermost = &config.Mattermost{URL: "https://chat.example.com", Team: "acme"}
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
		Routed:  func(context.Context) []string { return []string{"mattermost"} },
	}, "integrations", nil))

	rows, _ := body["integrations"].([]any)
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		if entry["key"] != "mattermost" {
			continue
		}
		if got := entry["routes"]; got != true {
			t.Fatalf("mattermost routes = %v, want true: it ingests over a "+
				"websocket and a parser routes it", got)
		}
		return
	}
	t.Fatal("mattermost has no row")
}

// A ROW FOLLOWS THE BLOCK THAT MAKES A DELIVERY REACH A SEAT.
//
// The GitHub row used to be reported on the org block OR on the agents' own
// apps, so that a company holding nothing but per-agent apps had a row.
// Neither half of that reading survives contact with what the engine does: an
// agent's app needs an INSTALLATION to see a repository at all, and the
// PARSER that turns a delivery into work for a seat is registered only where
// `integrations.github` is present and enabled — at boot
// ([Engine.startNotifications]) and on every apply
// ([Engine.reconcileGitHub]), both on the same condition. Without the block a
// seat app's delivery is verified at the webhook route and then dropped.
//
// So a company with installed apps and no block is not a surface with no row;
// it is a surface that is off, and a row for it is a row with nothing
// enabled — which the dashboard draws as Paused, its word for a state an
// operator chose. That is exactly what a disconnect leaves behind, which is
// where this was measured: the block went, the sealed per-seat values stayed,
// and the card sat on a state nobody had asked for.
func TestARowNeedsTheBlockThatRoutesItRatherThanALeftoverSecret(t *testing.T) {
	t.Parallel()
	rowFor := func(t *testing.T, cfg *config.Company, kind string) map[string]any {
		t.Helper()
		body := asMap(t, answer(t, queries.Sources{
			Company: func() *config.Company { return cfg },
		}, "integrations", nil))
		rows, _ := body["integrations"].([]any)
		for _, row := range rows {
			entry, _ := row.(map[string]any)
			if entry["key"] == kind {
				return entry
			}
		}
		return nil
	}
	// firstAgent is where a per-seat app is hung, so each case below reads
	// as the one fact it is about.
	firstAgent := func(t *testing.T, cfg *config.Company) *config.Role {
		t.Helper()
		for role := range cfg.EachRole() {
			if role.Seat().IsAgent() {
				return role
			}
		}
		t.Fatal("the fixture has no agent seat")
		return nil
	}

	t.Run("github keeps its row while the block enables it", func(t *testing.T) {
		t.Parallel()
		// THE CLAUSE THE SEAT COUNTER EXISTS FOR: the org block carries no
		// webhook secret of its own, so the row's claim to a credential
		// comes from the agent's app alone.
		cfg := company(t)
		cfg.Integrations.GitHub = &config.GitHub{Enabled: true}
		firstAgent(t, cfg).Integrations.GitHub = &config.RoleGitHub{
			AppID: 7, AppSlug: "acme-ceo", InstallationID: 9,
			PrivateKey: "${CEO_PEM}", WebhookSecret: "${CEO_HOOK}",
		}
		row := rowFor(t, cfg, "github")
		if row == nil {
			t.Fatal("no github row: an installed agent app on an enabled " +
				"surface is receiving deliveries and is invisible")
		}
		if got := row["secret_present"]; got != true {
			t.Errorf("github secret_present = %v, want true: the agent's own "+
				"app carries the only signing secret", got)
		}
	})

	t.Run("an uninstalled app claims no credential", func(t *testing.T) {
		t.Parallel()
		// The app record and both sealed values survive a disconnect —
		// GitHub has no API to delete an app — and the installation does
		// not. Counted, the row claimed a surface receiving events over an
		// app that reaches nothing.
		cfg := company(t)
		cfg.Integrations.GitHub = &config.GitHub{Enabled: true}
		firstAgent(t, cfg).Integrations.GitHub = &config.RoleGitHub{
			AppID: 7, AppSlug: "acme-ceo",
			PrivateKey: "${CEO_PEM}", WebhookSecret: "${CEO_HOOK}",
		}
		if got := rowFor(t, cfg, "github")["secret_present"]; got != false {
			t.Errorf("github secret_present = %v, want false: the app is not "+
				"installed, so it sees no repository and receives nothing", got)
		}
	})

	t.Run("github loses its row when the block goes", func(t *testing.T) {
		t.Parallel()
		cfg := company(t)
		cfg.Integrations.GitHub = nil
		firstAgent(t, cfg).Integrations.GitHub = &config.RoleGitHub{
			AppID: 7, AppSlug: "acme-ceo", InstallationID: 9,
			PrivateKey: "${CEO_PEM}", WebhookSecret: "${CEO_HOOK}",
		}
		if row := rowFor(t, cfg, "github"); row != nil {
			t.Errorf("github row = %v with no org block: no parser is "+
				"registered, so every verified delivery is dropped and the "+
				"card renders the residue as Paused", row)
		}
	})

	t.Run("slack loses its row when the block goes", func(t *testing.T) {
		t.Parallel()
		// THE SAME SHAPE, and the one an operator hit. Slack's org block is
		// the transport marker: dropping it retires the transport and
		// unregisters the parser, so the seats' apps deliver to nobody.
		cfg := company(t)
		cfg.Integrations.Slack = nil
		firstAgent(t, cfg).Integrations.Slack = &config.RoleSlack{
			BotToken: "${SLACK_BOT_TOKEN_CEO}", SigningSecret: "${SLACK_SIGNING_SECRET_CEO}",
		}
		if row := rowFor(t, cfg, "slack"); row != nil {
			t.Errorf("slack row = %v after a disconnect dropped the block "+
				"and left the seats: the card renders it as Paused, which is "+
				"a word for a state somebody chose", row)
		}
	})
}

// ENABLED: FALSE MEANS AN OPERATOR SWITCHED IT OFF, on every surface.
//
// The dashboard draws a tool whose every row reports `enabled: false` as
// PAUSED, which is a claim about somebody's intent. Two rows could reach that
// state without anybody intending anything, because they were reported on
// per-seat secrets as well as on the company block and then took `enabled`
// from the block alone — so an absent block and a paused one were the same
// row. A disconnect produces the first one every time.
//
// Asserted over EVERY surface rather than over the two that had the defect:
// what makes it safe to read `enabled: false` as an intent is that no arm
// anywhere can emit it for any other reason, and the next surface added is
// the one that would.
func TestAnEnabledFalseRowIsAlwaysADeliberatePause(t *testing.T) {
	t.Parallel()
	// EVERY BLOCK THIS BUILD CAN REPORT, each with whatever makes it
	// complete, and every seat carrying every per-seat app — which is the
	// shape a disconnect strands.
	cfg := company(t)
	cfg.Integrations.Slack = nil
	cfg.Integrations.GitHub = nil
	cfg.Integrations.GitLab = nil
	cfg.Integrations.Datadog = nil
	for role := range cfg.EachRole() {
		if !role.Seat().IsAgent() {
			continue
		}
		role.Integrations.Slack = &config.RoleSlack{
			BotToken: "${BOT}", SigningSecret: "${SIG}",
		}
		role.Integrations.GitHub = &config.RoleGitHub{
			AppID: 7, AppSlug: "acme-ceo", InstallationID: 9,
			PrivateKey: "${PEM}", WebhookSecret: "${HOOK}",
		}
	}
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
	}, "integrations", nil))
	rows, _ := body["integrations"].([]any)
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		if entry["enabled"] == false {
			t.Errorf("%v reports enabled: false with no block to say so, so "+
				"the dashboard draws residue as Paused", entry["key"])
		}
	}
}

// A SKILL LISTING PAST THE PAGE LIMIT SAYS SO, and the count is the seat's
// whole set rather than the page's length.
//
// The diary and the episodes ask their store for [queries.MemoryPageLimit] and
// get a recency feed, where "the most recent fifty" IS the question. Skills
// are a SET the seat loads from, so the page carries the size of the set it
// came from — and it once carried nothing, which is the one shape this tree
// does not allow a cut to have. The panel counts what it is given, so a seat
// with more skills than the page holds reported exactly the page limit: a
// number an operator has no reason to doubt and no way to check.
func TestASkillListingPastThePageLimitReportsWhatItCut(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	skills := learning.NewSkills(db)
	const held = queries.MemoryPageLimit + 7
	for i := range held {
		name := "skill-" + strconv.Itoa(i)
		if err := skills.Insert(t.Context(), learning.Skill{
			ID: name, AgentHandle: "ceo", Name: name,
			Description: "drafted from repeated work",
			CreatedAt:   pinned, UpdatedAt: pinned,
		}); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}

	body := asMap(t, answer(t, queries.Sources{Skills: skills},
		"agent_memory", map[string]any{"id": "ceo"}))

	rows, _ := body["skills"].([]any)
	if len(rows) != queries.MemoryPageLimit {
		t.Fatalf("the listing carries %d skill(s), want the page limit %d",
			len(rows), queries.MemoryPageLimit)
	}
	total, present := body["skills_total"]
	if !present {
		t.Fatal("the answer omits skills_total, so a page of the set is " +
			"indistinguishable from the whole of it")
	}
	if got, want := jsonInt(t, total), held; got != want {
		t.Errorf("skills_total = %d, want %d — the count a screen renders is "+
			"the seat's whole set, not the length of the page", got, want)
	}
}

// AND THE PAGE IS TAKEN IN THE STORE rather than out of a fully materialized
// library.
//
// The total and the page used to come from ONE unbounded listing that decoded
// every row the seat owns — content, frontmatter and all — to show fifty of
// them, so the answer stayed O(the whole catalogue) in I/O and allocations
// however small the page was. The bound is [learning.ListOptions.Limit] now,
// which the SQL honours, and the total is a COUNT beside it. Both halves are
// asserted here because either one alone is satisfiable by the bug: a listing
// that is bounded but counted off the page under-reports the set, and an
// honest total over an unbounded read is exactly what this replaced.
//
// The zero Limit is asserted too. It is the unbounded setting every other
// caller in the tree relies on — the prefetch's offer, the refiner's view of
// what is live — so a bound that leaked into the default would silently
// truncate all of them.
func TestTheSkillPageIsBoundedInTheStore(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	skills := learning.NewSkills(db)
	const held = queries.MemoryPageLimit + 7
	for i := range held {
		name := "skill-" + strconv.Itoa(i)
		if err := skills.Insert(t.Context(), learning.Skill{
			ID: name, AgentHandle: "ceo", Name: name,
			Description: "drafted from repeated work",
			Content:     strings.Repeat("a body a page never renders. ", 64),
			CreatedAt:   pinned, UpdatedAt: pinned,
		}); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}

	page, err := skills.List(t.Context(), "ceo",
		learning.ListOptions{Limit: queries.MemoryPageLimit})
	if err != nil {
		t.Fatalf("bounded listing: %v", err)
	}
	if len(page) != queries.MemoryPageLimit {
		t.Errorf("a listing bounded to %d read %d row(s), so the answer pays "+
			"for every skill the seat holds to render a page of them",
			queries.MemoryPageLimit, len(page))
	}
	whole, err := skills.List(t.Context(), "ceo", learning.ListOptions{})
	if err != nil {
		t.Fatalf("unbounded listing: %v", err)
	}
	if len(whole) != held {
		t.Errorf("the zero Limit read %d of %d skill(s): it is the unbounded "+
			"setting every non-paging caller depends on", len(whole), held)
	}

	body := asMap(t, answer(t, queries.Sources{Skills: skills},
		"agent_memory", map[string]any{"id": "ceo"}))
	rows, _ := body["skills"].([]any)
	if len(rows) != queries.MemoryPageLimit {
		t.Errorf("the answer carries %d skill(s), want the page limit %d",
			len(rows), queries.MemoryPageLimit)
	}
	if got := jsonInt(t, body["skills_total"]); got != held {
		t.Errorf("skills_total = %d, want %d — the total is counted over the "+
			"seat's set, never measured off the page", got, held)
	}
}

// AND IT IS PRESENT WHEN NOTHING WAS CUT, so a client never has to tell an
// absent key from a total of zero.
func TestSkillsTotalIsAlwaysPresent(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	body := asMap(t, answer(t, queries.Sources{Skills: learning.NewSkills(db)},
		"agent_memory", map[string]any{"id": "nobody"}))
	if _, present := body["skills_total"]; !present {
		t.Error("the answer omits skills_total for a seat with none")
	}
}

// jsonInt reads a number that survived a JSON round trip as either shape.
func jsonInt(t *testing.T, v any) int {
	t.Helper()
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		t.Fatalf("%v is not a number (%T)", v, v)
		return 0
	}
}

// AN EPOCH IS A NUMBER AND NOT A REVISION, and the fleet view carried only
// the number.
//
// `coord.NodeApply.RevisionID` is written by every node and stored by the
// plane precisely so a mid-transition read can name it — its own doc says "a
// node still on the previous revision is exactly what an operator is looking
// for" — and the answer dropped it. So a screen could say "node-b is two
// epochs behind" and not which revision it was behind, when that revision was
// activated, or what whoever activated it wrote about it.
func TestTheFleetNamesTheRevisionAndNotJustTheEpoch(t *testing.T) {
	t.Parallel()
	pinned := time.Date(2026, 5, 4, 9, 30, 0, 0, time.UTC)
	backend := coordmemory.New()
	for _, node := range []string{"node-a", "node-b"} {
		if _, err := backend.TryAcquire(t.Context(), coord.NodeResource(node),
			coord.AcquireOptions{Owner: node + ":1", TTL: time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	plane := coordmemory.NewFleet()
	published, err := plane.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "rev-2", Payload: []byte("{}"), At: pinned,
		Summary: "raise the CTO's budget",
	})
	if err != nil {
		t.Fatal(err)
	}
	// ONE NODE ON THE TARGET, one still on its predecessor — which is the
	// state the revision id exists to make visible.
	if err := plane.RecordApply(t.Context(), coord.NodeApply{
		NodeID: "node-a", Epoch: published.Epoch, RevisionID: "rev-2",
		Status: string(configplane.StatusOK), UpdatedAt: pinned,
	}); err != nil {
		t.Fatal(err)
	}
	if err := plane.RecordApply(t.Context(), coord.NodeApply{
		NodeID: "node-b", Epoch: published.Epoch - 1, RevisionID: "rev-1",
		Status: string(configplane.StatusOK), UpdatedAt: pinned,
	}); err != nil {
		t.Fatal(err)
	}

	body := asMap(t, answer(t, queries.Sources{
		Coord: backend, Plane: plane, NodeID: "node-a",
	}, "fleet", nil))

	byID := map[string]map[string]any{}
	for _, row := range rows(t, body["nodes"]) {
		byID[fmt.Sprint(row["id"])] = row
	}
	if got := byID["node-a"]["config_revision_id"]; got != "rev-2" {
		t.Errorf("node-a is on %v, want rev-2", got)
	}
	if got := byID["node-b"]["config_revision_id"]; got != "rev-1" {
		t.Errorf("node-b is on %v, want the predecessor it is still running", got)
	}

	// AND WHAT THE TARGET EPOCH STANDS FOR, beside the number every row is
	// compared against.
	activation, ok := body["activation"].(map[string]any)
	if !ok {
		t.Fatalf("activation = %#v, want the pointer's own record", body["activation"])
	}
	switch {
	case activation["revision_id"] != "rev-2":
		t.Errorf("activation names %v, want rev-2", activation["revision_id"])
	case activation["summary"] != "raise the CTO's budget":
		t.Errorf("activation summary = %v", activation["summary"])
	case activation["at"] == "" || activation["at"] == nil:
		t.Error("the activation carries no instant, so a screen cannot say when")
	}
	// ONE READ, so the two cannot disagree: an activation landing between
	// two reads would put "target 42" beside "revision r-41" and describe a
	// fleet that never existed.
	if body["target_epoch"] != activation["epoch"] {
		t.Errorf("target_epoch = %v and activation.epoch = %v — read twice, "+
			"they can describe different fleets", body["target_epoch"],
			activation["epoch"])
	}
}

// A FLEET WITH NO ACTIVATION CARRIES NO ACTIVATION RECORD, rather than an
// object of empty strings: a screen given one would render a revision named ""
// activated at the zero time, which is a worse answer than none.
func TestAFleetWithNoActivationCarriesNoRecord(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	if _, err := backend.TryAcquire(t.Context(), coord.NodeResource("node-a"),
		coord.AcquireOptions{Owner: "node-a:1", TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	body := asMap(t, answer(t, queries.Sources{
		Coord: backend, Plane: coordmemory.NewFleet(), NodeID: "node-a",
	}, "fleet", nil))
	if body["activation"] != nil {
		t.Errorf("activation = %#v on a fleet nobody has activated", body["activation"])
	}
	if body["target_epoch"] != float64(0) {
		t.Errorf("target_epoch = %v, want 0 so the client makes no comparison",
			body["target_epoch"])
	}
}

// HOW FAR A NODE'S OWN COPY HAS COME UP is a different question from its
// config epoch: a node can hold the current revision and still be hydrating
// the state it derives from the log, and only one of those makes its seats
// servable. The presence heartbeat has carried both counts since it existed
// and the fleet answer dropped them.
//
// ABSENT RATHER THAN ZERO for a node that published none, which is the rule
// `in_flight` already follows: a confident 0 of 0 reads as "ready" for a
// process that is simply not saying.
func TestTheFleetSaysHowFarANodesOwnCopyHasComeUp(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	claim := func(node string, meta map[string]any) {
		t.Helper()
		if _, err := backend.TryAcquire(t.Context(), coord.NodeResource(node),
			coord.AcquireOptions{Owner: node + ":1", TTL: time.Minute, Meta: meta},
		); err != nil {
			t.Fatal(err)
		}
	}
	// UNDER THE STATUS KEY, which is where the presence heartbeat puts it
	// and where [coord.StatusFromMeta] looks.
	claim("node-a", map[string]any{coord.StatusKey: coord.NodeStatus{
		ProjectionsReady: 2, ProjectionsTotal: 3,
	}.Meta()})
	claim("node-b", map[string]any{coord.StatusKey: coord.NodeStatus{}.Meta()})

	body := asMap(t, answer(t, queries.Sources{
		Coord: backend, NodeID: "node-a",
	}, "fleet", nil))
	byID := map[string]map[string]any{}
	for _, row := range rows(t, body["nodes"]) {
		byID[fmt.Sprint(row["id"])] = row
	}
	if got := byID["node-a"]["projections_ready"]; got != float64(2) {
		t.Errorf("node-a reports %v of its projections ready, want 2", got)
	}
	if got := byID["node-a"]["projections_total"]; got != float64(3) {
		t.Errorf("node-a reports a total of %v, want 3", got)
	}
	if _, present := byID["node-b"]["projections_total"]; present {
		t.Error("a node that published no projection counts carries a total " +
			"anyway, so a screen draws 0 of 0 — which reads as ready")
	}
}

// ONE SCHEDULE'S HISTORY, which the company-wide ledger cannot be.
//
// `schedules.recent_runs` is fifty rows across EVERY schedule, so a company
// with twenty hourly ones fills it in two and a half hours: "did the standup
// fire this week" was unanswerable while every row of the answer sat in the
// table, and a screen paging the company's and filtering is exactly how a
// reader concludes a schedule stopped running.
func TestOneSchedulesRunsAreReadableOnTheirOwn(t *testing.T) {
	t.Parallel()
	fired := time.Date(2026, 6, 8, 9, 0, 0, 0, time.UTC)
	ledger := &fakeRuns{runs: []schedule.Run{
		{FireKey: schedule.FireKey{
			Scope: types.ScheduleScopeRole, ScopeID: "ceo",
			ScheduleName: "standup", FireLabel: "20260608T0900", TargetHandle: "ceo",
		}, ScheduledAt: fired, FiredAt: fired, Outcome: schedule.OutcomeFired},
		// A SECOND UNIT DECLARING THE SAME NAME, which is what makes the
		// scope part of the identity rather than decoration.
		{FireKey: schedule.FireKey{
			Scope: types.ScheduleScopeUnit, ScopeID: "platform",
			ScheduleName: "standup", FireLabel: "20260608T0900", TargetHandle: "eng",
		}, ScheduledAt: fired, FiredAt: fired, Outcome: schedule.OutcomeFired},
	}}

	body := asMap(t, answer(t, queries.Sources{Runs: ledger}, "schedule_runs",
		map[string]any{"scope_type": "role", "scope_id": "ceo", "name": "standup"}))

	got := rows(t, body["runs"])
	if len(got) != 1 {
		t.Fatalf("%d runs, want the role's alone: %#v", len(got), body["runs"])
	}
	if got[0]["scope_id"] != "ceo" {
		t.Errorf("the run is %v's", got[0]["scope_id"])
	}
	// THE IDENTITY REACHED THE LEDGER, rather than the answer filtering a
	// company-wide read in the surface.
	if ledger.asked.scope != types.ScheduleScopeRole || ledger.asked.scopeID != "ceo" ||
		ledger.asked.name != "standup" {

		t.Errorf("the ledger was asked about %+v", ledger.asked)
	}
	if ledger.asked.limit != queries.MaxScheduleRuns {
		t.Errorf("the ledger was asked for %d rows, want the cap %d",
			ledger.asked.limit, queries.MaxScheduleRuns)
	}
}

// A SCHEDULE'S IDENTITY IS ALL THREE PARTS, and every one of them is required:
// a name alone would merge two teams' histories, and a scope this build does
// not know is refused naming the two rather than read as a filter that matches
// nothing.
func TestAScheduleRunsReadStatesWhatItIsMissing(t *testing.T) {
	t.Parallel()
	src := queries.Sources{Runs: &fakeRuns{}}
	for _, params := range []map[string]any{
		{"scope_id": "ceo", "name": "standup"},
		{"scope_type": "role", "name": "standup"},
		{"scope_type": "role", "scope_id": "ceo"},
		{"scope_type": "team", "scope_id": "ceo", "name": "standup"},
	} {
		if _, err := askNative(t, src, "schedule_runs", params); !errors.Is(
			err, queries.ErrBadParams) {

			t.Errorf("%v answered %v, want bad params", params, err)
		}
	}
}
