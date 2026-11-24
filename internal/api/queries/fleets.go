package queries

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
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
	// Serving is the subjects whose work it takes now.
	Serving []string `json:"serving"`
}

// FleetAssignment is one helper or seat: what it is set to, and who serves it
// now. A seat carries each of its jobs; its own fields are its executor's.
type FleetAssignment struct {
	Subject      string      `json:"subject"`
	Kind         string      `json:"kind"`
	Name         string      `json:"name"`
	Target       FleetTarget `json:"target"`
	Now          *string     `json:"now"`
	WaitingUntil *time.Time  `json:"waiting_until"`
	Jobs         []FleetJob  `json:"jobs,omitempty"`
}

// FleetJob is one of a seat's jobs, by the engine's name for it.
type FleetJob struct {
	Job          string      `json:"job"`
	Target       FleetTarget `json:"target"`
	Now          *string     `json:"now"`
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
// sign-in, and each seat job's member is [fleet.Fleet.Pick] from the home the
// node holding the seat published, so the page reads the balancer's rule.
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
	r := fleetReader{company: company, now: now, members: map[string]fleet.Member{},
		homes: homes, served: served}
	for _, key := range company.Providers.ProviderOrder() {
		entry := company.Providers.LLM[key]
		e := s.modelEntry(company, organization, key, &entry, LocalModel{}, reports, ModelUsage{}, now)
		r.members[key] = fleet.Read(entrySignals(e, entry.FleetKind()), now)
	}
	models := company.Providers.PhaseModels()
	serving := map[string][]string{}
	out.Assignments = r.assignments(models, serving)
	fleets := company.Providers.Fleets()
	keys := slices.Sorted(maps.Keys(fleets))
	// default-fleet first, the rest by key.
	slices.SortStableFunc(keys, func(a, b string) int {
		return cmp.Compare(boolRank(a != config.DefaultFleetKey), boolRank(b != config.DefaultFleetKey))
	})
	for _, key := range keys {
		out.Fleets = append(out.Fleets, r.view(key, fleets[key], serving))
	}
	return out, nil
}

// fleetReader is one answer's reading of the company's members and homes.
type fleetReader struct {
	company *config.Company
	now     time.Time
	// members is every entry as the balancer reads it, LastServed unset.
	members map[string]fleet.Member
	// homes is each seat job's newest home across live nodes.
	homes map[fleet.Job]homeAt
	// served is when each member last took a seat job's work, any job's.
	served map[string]time.Time
}

// homeAt is a seat job's home on the record and when it was taken.
type homeAt struct {
	Member string
	At     time.Time
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
	return s
}

// fleet is key's fleet read now, its members carrying their last serve.
func (r fleetReader) fleet(key string, spec config.LLMFleet) fleet.Fleet {
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

// assignments is every helper and agent seat, sorted by name, with what each
// runs on now, adding each subject to serving under the member it runs on.
func (r fleetReader) assignments(models phase.Models, serving map[string][]string) []FleetAssignment {
	out := []FleetAssignment{}
	helpers := []struct{ subject, name, ref string }{
		{string(config.JobAssistant), "Ask Crewlet", r.company.Assistant.Model},
		{string(config.JobAdvisor), "Advisor", r.company.Advisor.Model},
	}
	for _, h := range helpers {
		a := FleetAssignment{Subject: h.subject, Kind: AssignmentHelper, Name: h.name}
		a.Target, a.Now, a.WaitingUntil = r.helper(h.subject, h.ref, serving)
		out = append(out, a)
	}
	for role := range r.company.EachRole() {
		if role.Kind == org.KindHuman {
			continue
		}
		seat := role.Seat()
		a := FleetAssignment{Subject: role.IdentityKey(), Kind: AssignmentSeat, Name: role.Name,
			Jobs: make([]FleetJob, 0, len(fleetJobs))}
		for _, j := range fleetJobs {
			job := FleetJob{Job: string(j.job)}
			job.Target, job.Now, job.WaitingUntil = r.resolved(a.Subject, j.phase,
				phase.Resolve(seat, j.phase, models), serving)
			if j.job == config.JobExecutor {
				a.Target, a.Now, a.WaitingUntil = job.Target, job.Now, job.WaitingUntil
			}
			a.Jobs = append(a.Jobs, job)
		}
		out = append(out, a)
	}
	slices.SortStableFunc(out, func(a, b FleetAssignment) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// helper is what a helper's field ref runs on: a fleet's pick for the helper,
// homed under its own name, or the entry it pins. An unset helper is off.
func (r fleetReader) helper(subject, ref string, serving map[string][]string) (FleetTarget, *string, *time.Time) {
	if key, ok := config.FleetOf(ref); ok {
		spec, _ := r.company.Providers.Fleet(key)
		now, until := r.pick(key, spec, fleet.Job{Fleet: key, Subject: subject, Job: subject})
		addServing(serving, now, subject)
		return FleetTarget{Fleet: &key}, now, until
	}
	target := FleetTarget{Pinned: &ref}
	if ref == "" {
		return target, nil, nil
	}
	now, until := r.pinned(ref)
	addServing(serving, now, subject)
	return target, now, until
}

// resolved is what a seat job's resolution runs on now: its fleet's pick from
// the job's home, or its pinned head; the company's fallback is unset.
func (r fleetReader) resolved(subject string, ph phase.Phase, res phase.Resolution,
	serving map[string][]string) (FleetTarget, *string, *time.Time) {
	var (
		target FleetTarget
		now    *string
		until  *time.Time
	)
	switch {
	case res.Fleet != "":
		key := res.Fleet
		spec, _ := r.company.Providers.Fleet(key)
		target = FleetTarget{Fleet: &key}
		now, until = r.pick(key, spec, fleet.Job{Fleet: key, Subject: subject, Job: ph.String()})
	case len(res.Keys) == 0:
		target = FleetTarget{Pinned: ptr("")}
	default:
		head := res.Keys[0]
		target = FleetTarget{Pinned: &head}
		if res.Source == phase.SourceDefaultEntry || res.Source == phase.SourceFirstEntry {
			target = FleetTarget{Pinned: ptr("")}
		}
		now, until = r.pinned(head)
	}
	addServing(serving, now, subject)
	return target, now, until
}

// pick is the member key's fleet hands job now, or when it has room again.
func (r fleetReader) pick(key string, spec config.LLMFleet, job fleet.Job) (*string, *time.Time) {
	f := r.fleet(key, spec)
	if c, ok := f.Pick(r.homes[job].Member); ok {
		return &c.Key, nil
	}
	if at, _, ok := f.FirstRoom(); ok {
		return nil, &at
	}
	return nil, nil
}

// pinned is a pinned entry while it can take work, else when it has room.
func (r fleetReader) pinned(key string) (*string, *time.Time) {
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
func (r fleetReader) view(key string, spec config.LLMFleet, serving map[string][]string) FleetView {
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
			Serving: append([]string{}, serving[m.Key]...)}
		if !m.Until.IsZero() {
			until := m.Until
			member.Until = &until
		}
		if m.Known {
			share := m.Share
			member.Share = &share
		}
		if !m.LastServed.IsZero() {
			at := m.LastServed
			member.LastServedAt = &at
		}
		v.Members = append(v.Members, member)
	}
	return v
}

// addServing files subject under the member it runs on, once.
func addServing(serving map[string][]string, member *string, subject string) {
	if member != nil && !slices.Contains(serving[*member], subject) {
		serving[*member] = append(serving[*member], subject)
	}
}

// boolRank orders false before true.
func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}
