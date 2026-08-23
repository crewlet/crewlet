package queries_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
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

const companyDoc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
integrations:
  github:
    enabled: true
    webhook_secret: "${GH}"
  gitlab:
    enabled: true
    url: https://gitlab.example.com
    signing_secret: "${GL}"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    integrations:
      slack:
        bot_token: "${TOK}"
        signing_secret: "${SEC}"
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
		byKind[entry["kind"].(string)] = entry
	}
	for _, kind := range []string{"github", "gitlab", "slack"} {
		if _, present := byKind[kind]; !present {
			t.Errorf("%s is configured and absent from the answer", kind)
		}
	}
	if _, present := byKind["plane"]; present {
		t.Error("an integration the company does not configure was reported")
	}
	if byKind["slack"] == nil || byKind["slack"]["seats"] != float64(1) {
		t.Errorf("slack seats = %v, want the one seat with an app", byKind["slack"]["seats"])
	}
}

func TestAMissingSecretIsReportedAsFalseNotAsAbsent(t *testing.T) {
	t.Parallel()
	// THREE-VALUED, and the third value is the point: null means this
	// surface uses no secret at all, false means a route is refusing every
	// delivery, and only an operator can tell those apart.
	cfg := company(t)
	cfg.Integrations.GitHub.WebhookSecret = ""
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
	}, "integrations", nil))

	rows, _ := body["integrations"].([]any)
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		if entry["kind"] != "github" {
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
	t.Fatal("github was not in the answer")
}

func TestASurfaceWithNoSecretReportsNull(t *testing.T) {
	t.Parallel()
	// Forge's app id is the JWT AUDIENCE, in every manifest the operator
	// installs. Reporting it as a secret that is present would invite an
	// operator to go looking for the one they had not set.
	cfg := company(t)
	cfg.Integrations.ForgeAppID = "app-123"
	body := asMap(t, answer(t, queries.Sources{
		Company: func() *config.Company { return cfg },
	}, "integrations", nil))

	rows, _ := body["integrations"].([]any)
	for _, row := range rows {
		entry, _ := row.(map[string]any)
		if entry["kind"] != "forge" {
			continue
		}
		value, present := entry["secret_present"]
		if !present {
			t.Fatal("the answer OMITS secret_present, so \"uses no secret\" and " +
				"\"this build forgot to say\" are one answer")
		}
		if value != nil {
			t.Fatalf("secret_present = %v, want null for a surface with no secret", value)
		}
		return
	}
	t.Fatal("forge was not in the answer")
}

// --- conversations ---------------------------------------------------------

func TestConversationsListsASeatsThreads(t *testing.T) {
	t.Parallel()
	ledgerStore := ledgerstore.NewMemoryConversations()
	write := func(key, reply string, at time.Time) {
		t.Helper()
		if err := ledgerStore.Append(t.Context(), "ceo", key,
			ledger.Session{Reply: reply}, reply, at, 0); err != nil {
			t.Fatal(err)
		}
	}
	write("thread-a", "one", pinned)
	write("thread-b", "two", pinned.Add(time.Hour))

	body := asMap(t, answer(t, queries.Sources{Conversations: ledgerStore},
		"conversations", map[string]any{"handle": "ceo"}))

	threads, _ := body["conversations"].([]any)
	if len(threads) != 2 {
		t.Fatalf("%d threads, want 2: %v", len(threads), body)
	}
	first, _ := threads[0].(map[string]any)
	if first["conversation_key"] != "thread-b" {
		t.Errorf("first thread = %v, want the one that moved most recently", first)
	}
	// Entries are empty on a LISTING: the sidebar shows keys, and carrying
	// every entry of every thread would move a seat's whole history to
	// render a list of names.
	if entries, _ := body["entries"].([]any); len(entries) != 0 {
		t.Errorf("the listing carries entries: %v", entries)
	}
}

func TestConversationsOpensOneThread(t *testing.T) {
	t.Parallel()
	ledgerStore := ledgerstore.NewMemoryConversations()
	for i, reply := range []string{"first", "second"} {
		if err := ledgerStore.Append(t.Context(), "ceo", "thread-a",
			ledger.Session{Reply: reply}, reply,
			pinned.Add(time.Duration(i)*time.Minute), 0); err != nil {
			t.Fatal(err)
		}
	}
	body := asMap(t, answer(t, queries.Sources{Conversations: ledgerStore},
		"conversations", map[string]any{"handle": "ceo", "key": "thread-a"}))

	entries, _ := body["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("%d entries, want 2: %v", len(entries), body)
	}
	// Oldest first, because a conversation reads forwards.
	first, _ := entries[0].(map[string]any)
	if first["reply"] != "first" {
		t.Errorf("entries = %v, want the conversation in order", entries)
	}
}

func TestConversationsNeedsASeat(t *testing.T) {
	t.Parallel()
	// Answering for "no seat" would be a listing of nothing, which reads
	// as a seat that has said nothing rather than as a malformed request.
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Conversations: ledgerstore.NewMemoryConversations()})
	if _, err := r.Answer(t.Context(), "conversations", nil, "operator"); err == nil {
		t.Fatal("a conversations query with no handle was answered")
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
	db := openStore(t)
	backend := coordmemory.New()
	if _, err := backend.TryAcquire(t.Context(), coord.NodeResource("node-a"),
		coord.AcquireOptions{Owner: "node-a:1", TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	plane := db.ControlPlane()
	if _, err := plane.RecordActivation(t.Context(), "rev-1", "", pinned); err != nil {
		t.Fatal(err)
	}
	if err := plane.RecordApply(t.Context(), store.NodeApply{
		NodeID: "node-a", Epoch: 1, RevisionID: "rev-1",
		Status: configplane.StatusOK, UpdatedAt: pinned,
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
	db := openStore(t)
	backend := coordmemory.New()
	if _, err := backend.TryAcquire(t.Context(), coord.NodeResource("node-a"),
		coord.AcquireOptions{Owner: "node-a:1", TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	plane := db.ControlPlane()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	body := asMap(t, answer(t, queries.Sources{
		Coord: backend, Plane: plane, NodeID: "node-a",
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
