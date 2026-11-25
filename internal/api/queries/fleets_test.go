package queries_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
)

// fleetsCompany is the fleet opus of six Opus entries, the fleet busy of two,
// a Sonnet entry solo, and seats the tests point at them.
const fleetsCompany = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    max: {model: claude-opus-5-5}
    team2: {model: claude-opus-5-5}
    team: {model: claude-opus-5-5}
    key: {model: claude-opus-5-5, auth: {mode: api-key, api_key: "${ANTHROPIC_API_KEY}"}}
    spare: {model: claude-opus-5-5}
    old: {model: claude-opus-5-5}
    a: {model: claude-opus-5-5}
    b: {model: claude-opus-5-5}
    solo: {model: claude-sonnet-5-5}
  llm_fleets:
    opus: {name: Claude Opus, members: [max, team2, team, key, spare, old]}
    busy: {name: Busy, strategy: sequential, members: [a, b]}
roles:
  - name: Engineer
    handle: engineer
  - name: Night
    handle: night
  - name: Writer
    handle: writer
    llm: a
  - name: Unset
    handle: unset
`

// llmFleetSources is fleetsCompany over backend, with engineer and the
// assistant on opus, night on busy and its reviewer pinned to solo: refs set
// on the parsed document, as the config admits them.
func llmFleetSources(t *testing.T, backend coord.Backend, edit func(*config.Company)) queries.Sources {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(fleetsCompany))
	if err != nil {
		t.Fatal(err)
	}
	for role := range cfg.EachRole() {
		switch role.IdentityKey() {
		case "engineer":
			role.LLM.Default = config.ProviderKeys{config.FleetRef("opus")}
		case "night":
			role.LLM.Default = config.ProviderKeys{config.FleetRef("busy")}
			role.LLM.Review = config.ProviderKeys{"solo"}
		}
	}
	cfg.Assistant.Model = config.FleetRef("opus")
	if edit != nil {
		edit(cfg)
	}
	return queries.Sources{Coord: backend, NodeStatuses: recordsOf(backend),
		Company: companySource(t, cfg), NodeID: "node-a", Models: fakeModels{}}
}

func used(f float64) *float64 { return &f }

func llmFleetsOf(t *testing.T, s queries.Sources) queries.FleetsAnswer {
	t.Helper()
	got, ok := answer(t, s, "llm_fleets", nil).(queries.FleetsAnswer)
	if !ok {
		t.Fatalf("llm_fleets answered %T", got)
	}
	return got
}

func llmFleetNamed(t *testing.T, a queries.FleetsAnswer, key string) queries.FleetView {
	t.Helper()
	for _, f := range a.Fleets {
		if f.Key == key {
			return f
		}
	}
	t.Fatalf("no fleet %s in %+v", key, a.Fleets)
	return queries.FleetView{}
}

func assignmentOf(t *testing.T, a queries.FleetsAnswer, subject string) queries.FleetAssignment {
	t.Helper()
	for _, x := range a.Assignments {
		if x.Subject == subject {
			return x
		}
	}
	t.Fatalf("no assignment %s in %+v", subject, a.Assignments)
	return queries.FleetAssignment{}
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// A FLEET'S MEMBERS ARE READ FROM THE FLEET'S STATUS RECORDS, by the
// balancer's rule: a fresh window's share and warning, a live node's sit-out,
// a refusal newer than the last answer, and a reading past half an hour saying
// nothing. The spread is the pool's known shares; a seat job's member is the
// pick from the newest home any live node published.
func TestAFleetsMembersAreReadAsTheBalancerReadsThem(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	minute := pinned.Add(-time.Minute)
	presentNode(t, backend, "node-a", &coord.NodeStatusRecord{Models: []coord.ModelStatus{
		{Key: "max", SignIn: "token", Resolves: true, LastAnswer: minute,
			Limits: []coord.ModelLimit{{Window: "session", Hours: 5, State: "ok", Used: used(0.30), Seen: minute}},
			Homes:  []coord.FleetHome{{Fleet: "opus", Subject: "engineer", Job: "execute", At: pinned.Add(-10 * time.Minute)}}},
		{Key: "team2", SignIn: "token", Resolves: true,
			Limits: []coord.ModelLimit{{Window: "weekly", State: "ok", Used: used(0.22), Seen: minute}}},
		{Key: "team", SignIn: "token", Resolves: true,
			Limits: []coord.ModelLimit{{Window: "session", Hours: 5, State: "ok", Used: used(0.85), Seen: minute}}},
		{Key: "spare", SignIn: "token", Resolves: true, LastAnswer: pinned.Add(-time.Hour),
			LastFailure: &coord.ModelFailure{Kind: "auth", At: minute, Said: "Invalid token"}},
	}})
	presentNode(t, backend, "node-b", &coord.NodeStatusRecord{Models: []coord.ModelStatus{
		{Key: "key", SignIn: "api-key", Resolves: true,
			SittingOut: &coord.ModelSitOut{Until: pinned.Add(40 * time.Minute), Since: minute, Window: "unknown"}},
		{Key: "old", SignIn: "token", Resolves: true,
			Limits: []coord.ModelLimit{{Window: "session", State: "warning", Used: used(0.95),
				Seen: pinned.Add(-40 * time.Minute)}},
			Homes: []coord.FleetHome{{Fleet: "opus", Subject: "engineer", Job: "execute", At: minute}}},
	}})
	got := llmFleetsOf(t, llmFleetSources(t, backend, nil))
	if got.Reading != queries.ReadingAdmin {
		t.Errorf("reading = %q, want admin", got.Reading)
	}
	opus := llmFleetNamed(t, got, "opus")
	if opus.Class != "Claude Opus" || opus.Harness != "claude-code" || opus.Strategy != "balanced" ||
		opus.ThresholdPoints != config.DefaultFleetThreshold || opus.IsDefault {
		t.Errorf("opus = %+v", opus)
	}
	if opus.Spread == nil || *opus.Spread != 8 {
		t.Errorf("spread = %v, want 8: the pool's known shares 30 and 22", deref(opus.Spread))
	}
	type want struct {
		state, kind, why string
		share            any
		overflow         bool
	}
	wants := map[string]want{
		"max":   {"available", "plan", "", 30, false},
		"team2": {"available", "plan", "", 22, false},
		"team":  {"near", "plan", "Near its 5-hour limit", 85, false},
		"key":   {"limit", "api", "Throttled by the key's rate limit", 0, true},
		"spare": {"signedout", "plan", "Its sign-in was refused", nil, false},
		"old":   {"available", "plan", "", nil, false},
	}
	var order []string
	for _, m := range opus.Members {
		order = append(order, m.Entry)
		w := wants[m.Entry]
		if m.State != w.state || m.Kind != w.kind || m.Why != w.why || deref(m.Share) != w.share ||
			m.Overflow != w.overflow {
			t.Errorf("%s = %s %s %q share %v overflow %v, want %+v", m.Entry, m.State, m.Kind, m.Why,
				deref(m.Share), m.Overflow, w)
		}
	}
	if !slices.Equal(order, []string{"max", "team2", "team", "key", "spare", "old"}) {
		t.Errorf("members in %v, want the fleet's list order", order)
	}
	key := opus.Members[3]
	if key.Until == nil || !key.Until.Equal(pinned.Add(40*time.Minute)) {
		t.Errorf("key until = %v, want its sit-out's end", deref(key.Until))
	}
	// node-b's home (old) is newer than node-a's (max); old's unknown share
	// ranks as the lowest known (22), so the home keeps the work. node-a's
	// home would have lost it to team2 (30 leads 22 by 8).
	engineer := assignmentOf(t, got, "engineer")
	if engineer.Kind != queries.AssignmentSeat || deref(engineer.Target.Fleet) != "opus" ||
		deref(engineer.Now) != "old" || engineer.WaitingUntil != nil {
		t.Errorf("engineer = target %v now %v waiting %v, want opus on old", deref(engineer.Target.Fleet),
			deref(engineer.Now), deref(engineer.WaitingUntil))
	}
	if engineer.NowKind != queries.NowNext {
		t.Errorf("engineer's now is %q, want next: nothing runs", engineer.NowKind)
	}
	// NOTHING RUNS, SO NOBODY IS SERVED: a member's last serve is its newest
	// home, never a prediction.
	old := opus.Members[5]
	if len(old.Serving) != 0 || old.LastServedAt == nil || !old.LastServedAt.Equal(minute) {
		t.Errorf("old serving %v last served %v, want nobody and its newest home", old.Serving,
			deref(old.LastServedAt))
	}
	for _, m := range opus.Members {
		if len(m.Serving) != 0 {
			t.Errorf("%s serving %v while nothing runs", m.Entry, m.Serving)
		}
	}
	if m := opus.Members[0]; m.LastServedAt == nil {
		t.Errorf("max last served %v, want its older home", deref(m.LastServedAt))
	}
	if a := assignmentOf(t, got, "assistant"); deref(a.Now) != "team2" || a.NowKind != queries.NowNext {
		t.Errorf("Ask Crewlet now %v (%s), want team2 next: the least used", deref(a.Now), a.NowKind)
	}
}

// EVERY MEMBER AT ITS LIMIT: nobody serves the seat, which waits for the
// earliest instant any member has room. A seat pinned to an entry at its
// limit waits for that entry.
func TestASeatWaitsForItsFleetsEarliestRoom(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatusRecord{Models: []coord.ModelStatus{
		{Key: "a", SignIn: "token", Resolves: true,
			SittingOut: &coord.ModelSitOut{Until: pinned.Add(3 * time.Hour), Window: "weekly"}},
		{Key: "b", SignIn: "token", Resolves: true,
			SittingOut: &coord.ModelSitOut{Until: pinned.Add(time.Hour), Window: "session"}},
	}})
	got := llmFleetsOf(t, llmFleetSources(t, backend, nil))
	night := assignmentOf(t, got, "night")
	if night.Now != nil || night.WaitingUntil == nil || !night.WaitingUntil.Equal(pinned.Add(time.Hour)) {
		t.Errorf("night now %v waiting %v, want nobody until b's room", deref(night.Now), deref(night.WaitingUntil))
	}
	writer := assignmentOf(t, got, "writer")
	if deref(writer.Target.Pinned) != "a" || writer.Now != nil || writer.WaitingUntil == nil ||
		!writer.WaitingUntil.Equal(pinned.Add(3*time.Hour)) {
		t.Errorf("writer = target %v now %v waiting %v, want pinned to a until its room",
			deref(writer.Target.Pinned), deref(writer.Now), deref(writer.WaitingUntil))
	}
	for _, m := range llmFleetNamed(t, got, "busy").Members {
		if m.State != "limit" || len(m.Serving) != 0 {
			t.Errorf("busy member %s = %s serving %v, want at its limit serving nobody", m.Entry, m.State, m.Serving)
		}
	}
}

// WORK OR HOMES A ROW COULD NOT NAME ARE COUNTED AND NEVER GUESSED: a member
// whose records counted work in flight past what they name says how much,
// and a subject with none named on a member it may run on is unlisted, never
// predicted next; a fleet's job with no home named is unlisted where a member
// has homes it could not name, while one homed, or pinned elsewhere, is
// predicted as before.
//
// Mutation: drop the unlisted branch from fleetReader.flying, or read a
// member's homes as named whole, and this fails.
func TestWorkOrHomesARowCouldNotNameAreCountedAndNeverGuessed(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	minute := pinned.Add(-time.Minute)
	presentNode(t, backend, "node-a", &coord.NodeStatusRecord{Models: []coord.ModelStatus{
		{Key: "max", SignIn: "token", Resolves: true, RunningCount: 3,
			Running: []coord.FleetRunning{{Fleet: "opus", Subject: "nobody-here", Job: "review", Since: minute}}},
		{Key: "a", SignIn: "token", Resolves: true,
			Homes: []coord.FleetHome{{Fleet: "busy", Subject: "night", Job: "execute", At: minute}}},
		{Key: "b", SignIn: "token", Resolves: true, HomesCount: 4},
		{Key: "solo", SignIn: "token", Resolves: true},
	}})
	got := llmFleetsOf(t, llmFleetSources(t, backend, nil))
	for _, m := range llmFleetNamed(t, got, "opus").Members {
		if want := map[string]int{"max": 2}[m.Entry]; m.UnlistedServing != want {
			t.Errorf("opus member %s counts %d unlisted in flight, want %d", m.Entry, m.UnlistedServing, want)
		}
	}
	for _, m := range llmFleetNamed(t, got, "busy").Members {
		if want := map[string]int{"b": 4}[m.Entry]; m.UnlistedHomes != want {
			t.Errorf("busy member %s counts %d unlisted homes, want %d", m.Entry, m.UnlistedHomes, want)
		}
	}
	for _, subject := range []string{"engineer", "assistant"} {
		if a := assignmentOf(t, got, subject); a.Now != nil || a.NowKind != queries.NowUnlisted ||
			a.WaitingUntil != nil {
			t.Errorf("%s on opus = now %v (%s) waiting %v, want unlisted: max runs work its row could not name",
				subject, deref(a.Now), a.NowKind, deref(a.WaitingUntil))
		}
	}
	night := assignmentOf(t, got, "night")
	kinds := map[string]queries.NowKind{}
	for _, j := range night.Jobs {
		kinds[j.Job] = j.NowKind
	}
	if kinds["executor"] != queries.NowNext || kinds["review"] != queries.NowNext ||
		kinds["subagent"] != queries.NowUnlisted {
		t.Errorf("night's jobs read %v, want its homed executor and its reviewer pinned to solo next, its "+
			"unhomed subagent unlisted: b has homes its row could not name", kinds)
	}
	if w := assignmentOf(t, got, "writer"); deref(w.Now) != "a" || w.NowKind != queries.NowNext {
		t.Errorf("writer pinned to a = now %v (%s), want a next: a pinned entry has no home to steer it",
			deref(w.Now), w.NowKind)
	}
}

// A SEAT CARRIES EACH OF ITS JOBS BY THE ENGINE'S NAME, each resolved as the
// engine resolves it: a reviewer pinned beside a fleet stays pinned. Helpers
// carry their field: a fleet, or off.
func TestEveryAssignmentNamesTheEnginesJobs(t *testing.T) {
	t.Parallel()
	got := llmFleetsOf(t, llmFleetSources(t, coordmemory.New(), nil))
	night := assignmentOf(t, got, "night")
	var jobs []string
	for _, j := range night.Jobs {
		jobs = append(jobs, j.Job)
		switch j.Job {
		case "review":
			if deref(j.Target.Pinned) != "solo" || deref(j.Now) != "solo" {
				t.Errorf("night's review = %v on %v, want pinned to solo", deref(j.Target.Pinned), deref(j.Now))
			}
		default:
			if deref(j.Target.Fleet) != "busy" || deref(j.Now) != "a" {
				t.Errorf("night's %s = %v on %v, want busy's first member", j.Job, deref(j.Target.Fleet), deref(j.Now))
			}
		}
	}
	if !slices.Equal(jobs, []string{"executor", "review", "subagent", "auxiliary", "judge", "triage"}) {
		t.Errorf("jobs = %v, want the engine's six seat jobs", jobs)
	}
	assistant := assignmentOf(t, got, "assistant")
	if assistant.Kind != queries.AssignmentHelper || assistant.Name != "Ask Crewlet" ||
		deref(assistant.Target.Fleet) != "opus" || assistant.Now == nil || assistant.Jobs != nil {
		t.Errorf("assistant = %+v, want the helper on opus with no jobs", assistant)
	}
	advisor := assignmentOf(t, got, "advisor")
	if deref(advisor.Target.Pinned) != "" || advisor.Target.Fleet != nil || advisor.Now != nil {
		t.Errorf("advisor = %+v, want unset and off", advisor)
	}
	var names []string
	for _, a := range got.Assignments {
		names = append(names, a.Name)
	}
	if !slices.IsSorted(names) {
		t.Errorf("assignments in %v, want helpers and seats mixed by name", names)
	}
}

// DEFAULT-FLEET IS LISTED FIRST, EMPTY OR NOT, and a seat naming nothing runs
// on it only once it has a member; until then on the company's fallback,
// which reads as unset.
func TestTheDefaultFleetIsListedAndTakesUnsetSeatsOnceItHasAMember(t *testing.T) {
	t.Parallel()
	empty := llmFleetsOf(t, llmFleetSources(t, coordmemory.New(), nil))
	def := empty.Fleets[0]
	if def.Key != config.DefaultFleetKey || !def.IsDefault || len(def.Members) != 0 || def.Spread != nil ||
		def.Class != "" {
		t.Errorf("first fleet = %+v, want the empty default-fleet", def)
	}
	unset := assignmentOf(t, empty, "unset")
	if deref(unset.Target.Pinned) != "" || unset.Target.Fleet != nil || deref(unset.Now) != "max" {
		t.Errorf("unset = target %+v now %v, want unset on the first entry", unset.Target, deref(unset.Now))
	}
	joined := llmFleetsOf(t, llmFleetSources(t, coordmemory.New(), func(c *config.Company) {
		c.Providers.LLMFleets[config.DefaultFleetKey] = config.LLMFleet{Name: config.DefaultFleetKey,
			Members: []string{"solo"}}
	}))
	unset = assignmentOf(t, joined, "unset")
	if deref(unset.Target.Fleet) != config.DefaultFleetKey || deref(unset.Now) != "solo" {
		t.Errorf("unset = target %+v now %v, want default-fleet on solo", unset.Target, deref(unset.Now))
	}
	if unset.NowKind != queries.NowNext {
		t.Errorf("unset's now is %q, want next: nothing runs", unset.NowKind)
	}
	if def := joined.Fleets[0]; def.Class != "Claude Sonnet" || len(def.Members[0].Serving) != 0 {
		t.Errorf("default-fleet = %+v, want solo's class serving nobody while nothing runs", def)
	}
}

// A MEMBER READS THE SAME FLEETS, told as a member's reading: the words are
// the engine's own, never a vendor's sentence.
func TestAMemberReadsTheFleets(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatusRecord{Models: []coord.ModelStatus{
		{Key: "spare", SignIn: "token", Resolves: true,
			LastFailure: &coord.ModelFailure{Kind: "auth", At: pinned, Said: "Invalid token sk-ant"}},
	}})
	s := llmFleetSources(t, backend, nil)
	s.Now = func() time.Time { return pinned }
	r := queries.NewRegistry()
	queries.Register(r, s)
	member := iam.WithPrincipal(t.Context(), iam.Principal{ID: uuid.New(), Login: "jane",
		Kind: iam.KindPerson, Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantWorklet}})
	data, err := r.Answer(member, "llm_fleets", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(data)
	if body := string(raw); !strings.Contains(body, `"reading":"member"`) ||
		strings.Contains(body, "Invalid token") || !strings.Contains(body, `"state":"signedout"`) {
		t.Errorf("a member's reading = %s", body)
	}
}
