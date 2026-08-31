package queries_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/store"
)

// pinned is the clock these answers run on. Pinned because a lease countdown
// and a next-run projection are both measured against it.
var pinned = time.Date(2026, 8, 23, 16, 0, 0, 0, time.UTC)

// Nothing here is about which vendors the fixture names: the org block plus a
// per-seat identity is the shape these answers project, and every vendor
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
}

func (f fakeRuns) Recent(context.Context, int) ([]schedule.Run, error) {
	return f.runs, f.err
}

func TestSchedulesCarriesTheDispatchHistory(t *testing.T) {
	t.Parallel()
	cfg := company(t)
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
		Runs: fakeRuns{runs: []schedule.Run{{
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
		Runs:    fakeRuns{err: context.DeadlineExceeded},
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

func TestAQuestionWithNoSourceIsUnknownRatherThanEmpty(t *testing.T) {
	t.Parallel()
	// "This node has no lease table" and "the fleet is empty" are
	// different answers, and a dashboard that drew the second for the
	// first would report a company with no nodes during a misconfiguration.
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{})
	for _, what := range []string{
		"fleet", "schedules", "integrations", "conversations",
		"agent_memory", "config", "config_audit", "config_diff",
	} {
		if _, err := r.Answer(t.Context(), what, nil, "operator"); err == nil {
			t.Errorf("%s answered on a node with no source for it", what)
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
	// A peer that publishes no status at all — an older build, or one
	// whose engine is not co-located.
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
// A vendor's webhook route verifies and stores deliveries as soon as its
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
// secret present, the vendor's settings page shows a healthy hook, and the
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

// AND A PROCESS THAT CANNOT SEE AN ENGINE DOES NOT GUESS.
func TestAnApiWithNoEngineCannotSayWhatResolved(t *testing.T) {
	t.Parallel()
	cfg := company(t)
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
		// No Verifiable: the standalone shape.
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
// A standalone API has no co-located engine to ask which parsers registered,
// so it answers null rather than false. Reporting false would tell an
// operator their integrations are broken on the one deployment shape that
// cannot see them.
func TestAnApiWithNoEngineCannotSayWhatRoutes(t *testing.T) {
	t.Parallel()
	cfg := company(t)
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
		// No Routed: the standalone shape.
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
// The empty-but-not-nil case: notifications started and no vendor registered.
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
// This reads the FIELD NAMES out of the room's own source and asserts the
// answer carries each. It is the cheap half of the gate `internal/e2e` gives
// the push protocol; the query channel had none at all.
func TestTheIntegrationsRoomReadsWhatThisAnswerSends(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile(filepath.Join(
		"..", "..", "..", "static", "dashboard", "js", "views", "integrations.js"))
	if err != nil {
		t.Skipf("the dashboard tree is not in this checkout: %v", err)
	}

	// EVERY vendor, because the per-vendor detail fields (url, seats) only
	// appear on the rows that have them — a fixture missing one reports its
	// field as a mismatch that is really a gap in the fixture.
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
	// Across every row, not just the first: `url` and `seats` are
	// per-vendor detail, so a field carried by ANY row is a field the
	// answer knows how to send.
	sent := map[string]bool{}
	for _, r := range rows {
		entry, _ := r.(map[string]any)
		for field := range entry {
			sent[field] = true
		}
	}

	// Every `row.<field>` and `data.<field>` the view reads.
	rowFields := regexp.MustCompile(`\brow\.([a-z_]+)`)
	dataFields := regexp.MustCompile(`\bdata\.([a-z_]+)`)

	for _, m := range rowFields.FindAllStringSubmatch(string(source), -1) {
		if !sent[m[1]] {
			t.Errorf("the room reads row.%s and the answer never sends it — "+
				"that field renders as undefined on every card", m[1])
		}
	}
	for _, m := range dataFields.FindAllStringSubmatch(string(source), -1) {
		// `integrations` is the row list itself.
		if _, ok := body[m[1]]; !ok {
			t.Errorf("the room reads data.%s and the answer never sends it", m[1])
		}
	}
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

// NULL, NOT ZERO, when nothing was counted. A node with no event log cannot
// say how many deliveries were dropped, and reporting 0 would tell an
// operator every one of them woke a seat.
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
				t.Errorf("%s %s = %v, want null on a node with no event log",
					entry["key"], field, entry[field])
			}
		}
	}
}

// AN UNREADABLE EVENT LOG REPORTS NULL, NOT ZERO — the same rule as a node
// with no log at all, and for the same reason: a zero that means "could not
// tell" is the number an operator would act on.
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
