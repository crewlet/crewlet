package queries

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
)

// FleetsAnswer is `llm_fleets`: every LLM fleet with its members read as the
// balancer reads them, and what every helper and seat runs on now. Both
// readings carry the same words; `reading` says which one was asked.
type FleetsAnswer struct {
	Reading     string            `json:"reading"`
	Fleets      []FleetView       `json:"fleets"`
	Assignments []FleetAssignment `json:"assignments"`
}

// FleetView is one fleet, default-fleet first.
type FleetView struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	IsDefault bool   `json:"is_default"`
	// Class and Harness are its first member's, empty with no member.
	Class           string `json:"class"`
	Harness         string `json:"harness"`
	Strategy        string `json:"strategy"`
	ThresholdPoints int    `json:"threshold_points"`
	// Spread is the points between its most and least used members taking
	// new work ([fleet.Fleet.Spread]), null where it has none.
	Spread  *int          `json:"spread"`
	Members []FleetMember `json:"members"`
}

// FleetMember is one member, in the fleet's list order.
type FleetMember struct {
	Entry string `json:"entry"`
	Kind  string `json:"kind"`
	State string `json:"state"`
	// Until is when a member at its limit has room again, null where unknown.
	Until *time.Time `json:"until"`
	Why   string     `json:"why"`
	// Share is its usage share in whole points, null where unknown.
	Share        *int       `json:"share"`
	Overflow     bool       `json:"overflow"`
	LastServedAt *time.Time `json:"last_served_at"`
	// Serving is the subjects with work in flight on it now, the longest
	// running first: never a prediction, so empty while nothing runs.
	Serving []string `json:"serving"`
	// UnlistedServing is the work in flight on it the nodes' status records
	// counted but could not name beside the rest ([coord.ModelStatus]), and
	// UnlistedHomes the seat jobs' homes on it likewise: zero where they name
	// every one. Past them, Serving and the predictions under-report.
	UnlistedServing int `json:"unlisted_serving"`
	UnlistedHomes   int `json:"unlisted_homes"`
}

// NowKind says what an assignment's `now` is: the member its work in flight
// runs on, or the one its next piece of work would go to; or that neither is
// known, as a member it may run on has work or homes its records could not
// name.
type NowKind string

const (
	// NowRunning is the member the subject's work in flight runs on.
	NowRunning NowKind = "running"
	// NowNext is where the subject's next piece of work would go, the
	// predictions of every subject accounting for each other.
	NowNext NowKind = "next"
	// NowUnlisted is a subject with no work in flight named on any member it
	// may run on, where one of them has work in flight its records counted
	// but could not name, or, for a fleet's job with no home named, homes:
	// its work may run there, or its home steer its pick, so `now` is null
	// rather than a guess.
	NowUnlisted NowKind = "unlisted"
)

// NowKinds is the closed set.
var NowKinds = []NowKind{NowRunning, NowNext, NowUnlisted}

// Valid reports whether k is one of [NowKinds].
func (k NowKind) Valid() bool { return slices.Contains(NowKinds, k) }

// FleetAssignment is one helper or seat: what it is set to, and who serves it
// now. A seat carries each of its jobs; its own fields are its executor's.
type FleetAssignment struct {
	Subject string      `json:"subject"`
	Kind    string      `json:"kind"`
	Name    string      `json:"name"`
	Target  FleetTarget `json:"target"`
	Now     *string     `json:"now"`
	// NowKind says whether Now is running work or the next pick, or that
	// neither is known (Now null); absent where Now is null otherwise.
	NowKind      NowKind    `json:"now_kind,omitempty"`
	WaitingUntil *time.Time `json:"waiting_until"`
	Jobs         []FleetJob `json:"jobs,omitempty"`
}

// FleetJob is one of a seat's jobs, by the engine's name for it.
type FleetJob struct {
	Job          string      `json:"job"`
	Target       FleetTarget `json:"target"`
	Now          *string     `json:"now"`
	NowKind      NowKind     `json:"now_kind,omitempty"`
	WaitingUntil *time.Time  `json:"waiting_until"`
}

// FleetTarget is a fleet, or one pinned entry ("" for unset): exactly one key.
type FleetTarget struct {
	Fleet  *string `json:"fleet,omitempty"`
	Pinned *string `json:"pinned,omitempty"`
}

// Assignment kinds.
const (
	AssignmentSeat   = "seat"
	AssignmentHelper = "helper"
)

// fleetJobs are the seat jobs an assignment names, each with the phase that
// resolves it: the engine's own (config.ProviderJob), never a grouping.
var fleetJobs = []struct {
	job   config.ProviderJob
	phase phase.Phase
}{
	{config.JobExecutor, phase.Execute},
	{config.JobReview, phase.Review},
	{config.JobSubagent, phase.Subagent},
	{config.JobAuxiliary, phase.Auxiliary},
	{config.JobJudge, phase.Judge},
	{config.JobTriage, phase.Triage},
}

// llmFleets answers `llm_fleets` from the status records `llm_entries` reads:
// each member's state is [fleet.Read] over the fleet's merged view of its
// sign-in; what each member serves is the work in flight on it, from the
// runtime's own records (the status records' running work and the executors
// in flight); and an idle seat job's member is [fleet.Fleet.Pick] from the
// home the node holding the seat published, every prediction taking its pick
// as the engine would, so they account for each other.
func (s Sources) llmFleets(ctx context.Context, _ Params) (any, error) {
	reports, err := s.modelReports(ctx)
	if err != nil {
		return nil, err
	}
	out := FleetsAnswer{Reading: ReadingMember, Fleets: []FleetView{}, Assignments: []FleetAssignment{}}
	if readsConfig(ctx) {
		out.Reading = ReadingAdmin
	}
	company, organization := s.Company()
	if company == nil {
		return out, nil
	}
	now := s.clock()
	homes, served := newestHomes(reports)
	var executors []livestate.ExecutorEntry
	if s.State != nil {
		executors = s.State.ActiveExecutors()
	}
	running, serving := inFlight(reports, executors)
	unlistedRunning, unlistedHomes := unlisted(reports)
	r := &fleetReader{company: company, now: now, members: map[string]fleet.Member{},
		homes: homes, served: served, running: running, serving: serving,
		unlistedRunning: unlistedRunning, unlistedHomes: unlistedHomes}
	// A MEMBER WITH WORK IN FLIGHT TOOK IT LATELY: a tie goes elsewhere.
	for _, jobs := range running {
		for _, f := range jobs {
			if f.since.After(r.served[f.member]) {
				r.served[f.member] = f.since
			}
		}
	}
	r.recorded = maps.Clone(r.served)
	// THIS NODE'S SIGN-INS, as the Models page reads them, so a member whose
	// sign-in the page says is bad reads signed out here too.
	local := map[string]LocalModel{}
	if s.Models != nil {
		for _, m := range s.Models.LocalModels() {
			local[m.Key] = m
		}
	}
	for _, key := range company.Providers.ProviderOrder() {
		entry := company.Providers.LLM[key]
		e := s.modelEntry(company, organization, key, &entry, local[key], reports, ModelUsage{}, now)
		signals := entrySignals(e, entry.FleetKind())
		// NOTHING SAID IS NOT SIGNED OUT: an entry no node reports yet keeps
		// its chance to serve once and report.
		if local[key].Route == "" && !reported(reports, key) {
			signals.NotSignedIn = false
		}
		r.members[key] = fleet.Read(signals, now)
	}
	out.Assignments = r.assignments(company.Providers.PhaseModels())
	fleets := company.Providers.Fleets()
	keys := slices.Sorted(maps.Keys(fleets))
	// default-fleet first, the rest by key.
	slices.SortStableFunc(keys, func(a, b string) int {
		return cmp.Compare(boolRank(a != config.DefaultFleetKey), boolRank(b != config.DefaultFleetKey))
	})
	for _, key := range keys {
		out.Fleets = append(out.Fleets, r.view(key, fleets[key]))
	}
	return out, nil
}

// fleetReader is one answer's reading of the company's members, homes and
// work in flight.
type fleetReader struct {
	company *config.Company
	now     time.Time
	// members is every entry as the balancer reads it, LastServed unset.
	members map[string]fleet.Member
	// homes is each seat job's newest home across live nodes.
	homes map[fleet.Job]homeAt
	// served is when each member last took work: its newest home or work in
	// flight, then each prediction this answer makes ([fleetReader.pick]).
	served map[string]time.Time
	// recorded is served as the records left it, before any prediction.
	recorded map[string]time.Time
	// running is each subject's work in flight by job; serving the subjects
	// in flight on each member, the longest running first.
	running map[string]map[string]flight
	serving map[string][]string
	// unlistedRunning and unlistedHomes are the work in flight and the homes
	// on each member the records counted but could not name.
	unlistedRunning, unlistedHomes map[string]int
	// predicted counts the predictions made, which take their picks in turn.
	predicted int
}

// homeAt is a seat job's home on the record and when it was taken.
type homeAt struct {
	Member string
	At     time.Time
}

// flight is a job's work in flight: the member it runs on, and since when.
type flight struct {
	member string
	since  time.Time
}

// newestHomes is each seat job's home, the newest across live nodes' rows,
// and when each member last took any seat job's work.
func newestHomes(reports []nodeModels) (map[fleet.Job]homeAt, map[string]time.Time) {
	homes, served := map[fleet.Job]homeAt{}, map[string]time.Time{}
	for _, r := range reports {
		for key, m := range r.models {
			for _, h := range m.Homes {
				job := fleet.Job{Fleet: h.Fleet, Subject: h.Subject, Job: h.Job}
				if held, ok := homes[job]; !ok || h.At.After(held.At) {
					homes[job] = homeAt{Member: key, At: h.At}
				}
				if h.At.After(served[key]) {
					served[key] = h.At
				}
			}
		}
	}
	return homes, served
}

// inFlight is the work in flight across live nodes, by subject and job (the
// engine's job name), and the subjects in flight on each entry: each node's
// running calls and helper turns ([coord.ModelStatus.Running]) and every
// executor run launching or running in its box, on the entry it was launched on.
func inFlight(reports []nodeModels, executors []livestate.ExecutorEntry) (map[string]map[string]flight,
	map[string][]string) {
	type work struct {
		subject, job, member string
		since                time.Time
	}
	var all []work
	for _, r := range reports {
		for key, m := range r.models {
			for _, run := range m.Running {
				all = append(all, work{subject: run.Subject, job: jobOfPhase(run.Job), member: key, since: run.Since})
			}
		}
	}
	for _, x := range executors {
		if x.ProviderKey == "" || x.AgentHandle == "" ||
			(x.Status != livestate.ExecutorRunning && x.Status != livestate.ExecutorLaunching) {
			continue
		}
		since, _ := time.Parse(time.RFC3339Nano, x.StartedAt)
		all = append(all, work{subject: x.AgentHandle, job: string(config.JobExecutor), member: x.ProviderKey,
			since: since})
	}
	slices.SortStableFunc(all, func(a, b work) int { return a.since.Compare(b.since) })
	running, serving := map[string]map[string]flight{}, map[string][]string{}
	for _, w := range all {
		if running[w.subject] == nil {
			running[w.subject] = map[string]flight{}
		}
		// The longest running is the job's: a later call of the same job
		// on another member (a worker's) is served too.
		if _, held := running[w.subject][w.job]; !held {
			running[w.subject][w.job] = flight{member: w.member, since: w.since}
		}
		if !slices.Contains(serving[w.member], w.subject) {
			serving[w.member] = append(serving[w.member], w.subject)
		}
	}
	return running, serving
}

// unlisted is the work in flight and the homes on each entry that live
// nodes' rows counted past what they name ([coord.ModelStatus.RunningCount]).
func unlisted(reports []nodeModels) (running, homes map[string]int) {
	running, homes = map[string]int{}, map[string]int{}
	for _, r := range reports {
		for key, m := range r.models {
			running[key] += max(m.RunningCount-len(m.Running), 0)
			homes[key] += max(m.HomesCount-len(m.Homes), 0)
		}
	}
	return running, homes
}

// reported is whether a live node's record carries key's row.
func reported(reports []nodeModels, key string) bool {
	return slices.ContainsFunc(reports, func(r nodeModels) bool {
		_, ok := r.models[key]
		return ok
	})
}

// jobOfPhase is the engine's job name for a phase's work in flight: the
// executor's for onboarding, which runs on the seat's `llm`; a helper's work
// is named by the helper.
func jobOfPhase(ph string) string {
	switch phase.Phase(ph) {
	case phase.Execute, phase.Onboarding:
		return string(config.JobExecutor)
	}
	return ph
}

// entrySignals is what the merged reading of an entry says of its sign-in:
// the newest window reports, the latest sit-out across live nodes, and the
// newest answer and failure, which is a refusal where it is auth.
func entrySignals(e ModelEntry, kind fleet.Kind) fleet.Signals {
	s := fleet.Signals{Key: e.Key, Kind: kind}
	for _, l := range e.Limits {
		r := fleet.Reading{LimitReading: llm.LimitReading{Window: llm.LimitWindow(l.Window), Hours: l.Hours,
			Model: l.Model, State: llm.LimitState(l.State), Used: l.Used}, Seen: l.Seen}
		if l.ResetsAt != nil {
			r.ResetsAt = *l.ResetsAt
		}
		s.Readings = append(s.Readings, r)
	}
	if out := e.SittingOut; out != nil {
		s.SitOut = &fleet.SitOut{Until: out.Until, Window: llm.LimitWindow(out.Window)}
	}
	if e.LastSuccess != nil {
		s.Answered = *e.LastSuccess
	}
	if f := e.LastFailure; f != nil && f.Kind == llm.KindAuth.String() {
		s.Refused = f.At
	}
	// THE MODELS PAGE'S VERDICT: nothing signs it in, or its credential expired.
	s.NotSignedIn = e.Health == HealthNoSignIn || e.Health == HealthRejected
	return s
}

// fleet is key's fleet read now, its members carrying their last serve.
func (r *fleetReader) fleet(key string, spec config.LLMFleet) fleet.Fleet {
	f := fleet.Fleet{Key: key, Strategy: fleet.Strategy(spec.EffectiveStrategy()), Threshold: spec.Threshold()}
	for _, entry := range spec.Members {
		m, ok := r.members[entry]
		if !ok || slices.ContainsFunc(f.Members, func(x fleet.Member) bool { return x.Key == entry }) {
			continue
		}
		m.LastServed = r.served[entry]
		f.Members = append(f.Members, m)
	}
	return f
}

// runsOn is what a subject's job runs on now: where it sits.
type runsOn struct {
	target FleetTarget
	now    *string
	kind   NowKind
	until  *time.Time
}

// assignments is every helper and agent seat, sorted by name, with what each
// runs on now: its work in flight, else where its next piece would go.
func (r *fleetReader) assignments(models phase.Models) []FleetAssignment {
	out := []FleetAssignment{}
	helpers := []struct{ subject, name, ref string }{
		{string(config.JobAssistant), "Ask Crewlet", r.company.Assistant.Model},
		{string(config.JobAdvisor), "Advisor", r.company.Advisor.Model},
	}
	for _, h := range helpers {
		on := r.helper(h.subject, h.ref)
		out = append(out, FleetAssignment{Subject: h.subject, Kind: AssignmentHelper, Name: h.name,
			Target: on.target, Now: on.now, NowKind: on.kind, WaitingUntil: on.until})
	}
	for role := range r.company.EachRole() {
		if role.Kind == org.KindHuman {
			continue
		}
		seat := role.Seat()
		a := FleetAssignment{Subject: role.IdentityKey(), Kind: AssignmentSeat, Name: role.Name,
			Jobs: make([]FleetJob, 0, len(fleetJobs))}
		for _, j := range fleetJobs {
			on := r.resolved(a.Subject, string(j.job), j.phase, phase.Resolve(seat, j.phase, models))
			if j.job == config.JobExecutor {
				a.Target, a.Now, a.NowKind, a.WaitingUntil = on.target, on.now, on.kind, on.until
			}
			a.Jobs = append(a.Jobs, FleetJob{Job: string(j.job), Target: on.target, Now: on.now,
				NowKind: on.kind, WaitingUntil: on.until})
		}
		out = append(out, a)
	}
	slices.SortStableFunc(out, func(a, b FleetAssignment) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// helper is what a helper's field ref runs on: its work in flight, else a
// fleet's pick for the helper, homed under its own name, or the entry it
// pins. An unset helper is off.
func (r *fleetReader) helper(subject, ref string) runsOn {
	if key, ok := config.FleetOf(ref); ok {
		spec, _ := r.company.Providers.Fleet(key)
		job := fleet.Job{Fleet: key, Subject: subject, Job: subject}
		return r.flying(subject, subject, FleetTarget{Fleet: &key}, r.mayRun(spec.Members, job),
			func() (*string, *time.Time) { return r.pick(key, spec, job) })
	}
	target := FleetTarget{Pinned: &ref}
	if ref == "" {
		return runsOn{target: target}
	}
	return r.flying(subject, subject, target, r.mayRun([]string{ref}, fleet.Job{}),
		func() (*string, *time.Time) { return r.pinned(ref) })
}

// resolved is what a seat job's resolution runs on now: its work in flight,
// else its fleet's pick from the job's home, or its pinned head; the
// company's fallback is unset.
func (r *fleetReader) resolved(subject, job string, ph phase.Phase, res phase.Resolution) runsOn {
	switch {
	case res.Fleet != "":
		key := res.Fleet
		spec, _ := r.company.Providers.Fleet(key)
		homed := fleet.Job{Fleet: key, Subject: subject, Job: ph.String()}
		return r.flying(subject, job, FleetTarget{Fleet: &key}, r.mayRun(spec.Members, homed),
			func() (*string, *time.Time) { return r.pick(key, spec, homed) })
	case len(res.Keys) == 0:
		return runsOn{target: FleetTarget{Pinned: ptr("")}}
	}
	head := res.Keys[0]
	target := FleetTarget{Pinned: &head}
	if res.Source == phase.SourceDefaultEntry || res.Source == phase.SourceFirstEntry {
		target = FleetTarget{Pinned: ptr("")}
	}
	return r.flying(subject, job, target, r.mayRun([]string{head}, fleet.Job{}),
		func() (*string, *time.Time) { return r.pinned(head) })
}

// mayRun reports whether a subject with no work in flight named may yet have
// some on members, or a home among them its pick would take: one of them has
// work in flight its records counted past what they name, or, for a fleet's
// job (a zero job for a pinned entry) with no home named, homes likewise.
func (r *fleetReader) mayRun(members []string, job fleet.Job) bool {
	_, homed := r.homes[job]
	return slices.ContainsFunc(members, func(m string) bool {
		return r.unlistedRunning[m] > 0 || job != (fleet.Job{}) && !homed && r.unlistedHomes[m] > 0
	})
}

// flying is target's member with the subject's job in flight on it, else
// unlisted where it may run on work its records could not name
// ([fleetReader.mayRun]),
// else the one next answers.
func (r *fleetReader) flying(subject, job string, target FleetTarget, unnamed bool,
	next func() (*string, *time.Time)) runsOn {
	if f, ok := r.running[subject][job]; ok {
		member := f.member
		return runsOn{target: target, now: &member, kind: NowRunning}
	}
	if unnamed {
		return runsOn{target: target, kind: NowUnlisted}
	}
	now, until := next()
	on := runsOn{target: target, now: now, until: until}
	if now != nil {
		on.kind = NowNext
	}
	return on
}

// pick is the member key's fleet hands job's next piece of work, or when it
// has room again. The pick is taken, as the engine's is, so the next
// prediction's tie goes to another member rather than every idle job reading
// the first.
func (r *fleetReader) pick(key string, spec config.LLMFleet, job fleet.Job) (*string, *time.Time) {
	f := r.fleet(key, spec)
	if c, ok := f.Pick(r.homes[job].Member); ok {
		r.predicted++
		r.served[c.Key] = r.now.Add(time.Duration(r.predicted))
		return &c.Key, nil
	}
	if at, _, ok := f.FirstRoom(); ok {
		return nil, &at
	}
	return nil, nil
}

// pinned is a pinned entry while it can take work, else when it has room.
func (r *fleetReader) pinned(key string) (*string, *time.Time) {
	m, ok := r.members[key]
	if !ok || (m.State != fleet.Limit && m.State != fleet.SignedOut) {
		return &key, nil
	}
	if m.Until.IsZero() {
		return nil, nil
	}
	until := m.Until
	return nil, &until
}

// view is one fleet as the page reads it.
func (r *fleetReader) view(key string, spec config.LLMFleet) FleetView {
	f := r.fleet(key, spec)
	v := FleetView{Key: key, Name: spec.Name, IsDefault: key == config.DefaultFleetKey,
		Strategy: string(f.Strategy), ThresholdPoints: f.Threshold, Members: []FleetMember{}}
	if spread, ok := f.Spread(); ok {
		v.Spread = &spread
	}
	if len(f.Members) > 0 {
		first := r.company.Providers.LLM[f.Members[0].Key]
		v.Class, v.Harness = config.ModelClass(first.Model), first.HarnessName()
	}
	for _, m := range f.Members {
		member := FleetMember{Entry: m.Key, Kind: string(r.company.Providers.LLM[m.Key].FleetKind()),
			State: string(m.State), Why: m.Why, Overflow: m.Overflow(f.Strategy),
			Serving: append([]string{}, r.serving[m.Key]...), UnlistedServing: r.unlistedRunning[m.Key],
			UnlistedHomes: r.unlistedHomes[m.Key]}
		if !m.Until.IsZero() {
			until := m.Until
			member.Until = &until
		}
		if m.Known {
			share := m.Share
			member.Share = &share
		}
		// THE RECORD'S LAST SERVE, not a prediction's.
		if at, ok := r.lastServed(m.Key); ok {
			member.LastServedAt = &at
		}
		v.Members = append(v.Members, member)
	}
	return v
}

// lastServed is when member last took work as the records say, false for
// never.
func (r *fleetReader) lastServed(member string) (time.Time, bool) {
	at, ok := r.recorded[member]
	return at, ok && !at.IsZero()
}

// boolRank orders false before true.
func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}
