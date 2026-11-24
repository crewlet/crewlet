package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/structured"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/toolsession"
	"github.com/crewlet/crewlet/internal/workkey"
)

// A busy seat's triage (ADR-0168): a wake the router's fixed rule leaves on
// the inbox ([routeOf]) that is about the run's work or from a lead of the
// seat is described by one cheap model call ([runTriage]) as input, steer,
// request or noise, and the ENGINE decides what follows ([gateOf]): who leads
// the seat from the chart, what a wake owes from its delivery, never from the
// model's words. The package doc's "A busy seat's wake router" holds the rest.

// triageDecision is what the triage says a wake is, the one closed enum its
// submission takes.
type triageDecision string

const (
	// triageInput: information the running work needs.
	triageInput triageDecision = "input"
	// triageSteer: its writer redirects the running work.
	triageSteer triageDecision = "steer"
	// triageRequest: it asks for work of its own.
	triageRequest triageDecision = "request"
	// triageNoise: it asks nothing and adds nothing the running work needs.
	triageNoise triageDecision = "noise"
)

// triageDecisions is the closed set, in the order the submission offers them.
var triageDecisions = []triageDecision{triageInput, triageSteer, triageRequest, triageNoise}

// Valid reports whether d is one of [triageDecisions].
func (d triageDecision) Valid() bool { return slices.Contains(triageDecisions, d) }

// triageVerdict is one triage's answer: a decision and its reason, or why
// there is none (failure), which leaves the wake on the inbox.
type triageVerdict struct {
	decision triageDecision
	reason   string
	failure  string
}

// failed reports a triage that decided nothing.
func (v triageVerdict) failed() bool { return !v.decision.Valid() }

// rule is the verdict as the router's counter names it.
func (v triageVerdict) rule() routeRule {
	switch v.decision {
	case triageInput:
		return ruleTriageInput
	case triageSteer:
		return ruleTriageSteer
	case triageRequest:
		return ruleTriageRequest
	case triageNoise:
		return ruleTriageNoise
	}
	return ruleTriageFailed
}

// triageRounds bounds one triage's model requests: the decision, and one more
// after the engine refused an invalid one. A model that twice ignores a closed
// four-value enum will not take a third telling, and the wake stays.
const triageRounds = 2

// triageTimeout bounds one triage: a harness's cold start and one short
// completion are seconds, so ninety leaves room for a slow vendor and still
// ends inside the router's two-minute backstop ([routeBackstop]).
const triageTimeout = 90 * time.Second

// maxJobTriages is the most wakes one job's triage weighs. Past sixteen other
// messages about one run's work in one job, the work is a conversation that
// belongs in a turn of its own, and every further call would be spent on it.
const maxJobTriages = 16

// triagesPerRead is the most triages one read of the held inbox makes, so a
// read lasts at most four [triageTimeout]s; the rest are weighed next read.
// Four is [sandbox.MaxJobWakes]: a burst no job could hold more of.
const triagesPerRead = sandbox.MaxJobWakes

// maxTriageReasonRunes bounds the reason a triage gives, which only a log
// line carries: a sentence.
const maxTriageReasonRunes = 300

// errTriageUndecided is a triage whose model submitted no decision.
var errTriageUndecided = errors.New("engine: the triage submitted no decision")

// triageDeps is what one triage runs on.
type triageDeps struct {
	provider llm.Provider
	key      string
	host     *toolsession.Host
	fence    func() error
	now      func() time.Time
}

// runTriage is one triage of in on d: one tool, [prompts.TriageTool], under a
// fence minted for the call, within [triageRounds] and [triageTimeout].
func runTriage(ctx context.Context, d triageDeps, in prompts.WakeTriage) (triageVerdict, error) {
	ctx, cancel := context.WithTimeout(ctx, triageTimeout)
	defer cancel()
	if runes := []rune(in.Wake.Text); len(runes) > steer.MaxWakeRunes {
		in.Wake.Cut, in.Wake.Text = in.Wake.Cut+len(runes)-steer.MaxWakeRunes, string(runes[:steer.MaxWakeRunes])
	}
	surf := &triageSurface{}
	_, err := toolloop.Run(ctx, toolloop.Config{
		Provider: d.provider, ProviderKey: d.key, Surface: surf, Host: d.host,
		ServerTitle: in.Seat, MaxRounds: triageRounds, OwnPrompt: true,
		Messages: []llm.Message{{Role: llm.RoleSystem, Content: prompts.WakeTriageSystem},
			{Role: llm.RoleUser, Content: prompts.BuildWakeTriage(provenance.NewBoundary(), in)}},
		TerminateAfter: []string{prompts.TriageTool}, Fence: d.fence, Now: d.now,
	})
	if err != nil {
		return triageVerdict{}, fmt.Errorf("engine: the wake's triage: %w", err)
	}
	v, ok := surf.result()
	if !ok {
		return triageVerdict{}, errTriageUndecided
	}
	return v, nil
}

// triageSurface takes the decision and offers nothing to read.
type triageSurface struct {
	mu      sync.Mutex
	verdict triageVerdict
	got     bool
}

var _ toolloop.Surface = (*triageSurface)(nil)

func (*triageSurface) ToolDefs() []llm.ToolDef {
	enum := make([]string, 0, len(triageDecisions))
	for _, d := range triageDecisions {
		enum = append(enum, string(d))
	}
	return []llm.ToolDef{{Name: prompts.TriageTool, Description: "Submit what kind of message this is.",
		Parameters: map[string]any{"type": "object", "additionalProperties": false,
			"required": []string{"decision", "reason"},
			"properties": map[string]any{
				"decision": map[string]any{"type": "string", "enum": enum},
				"reason":   map[string]any{"type": "string", "maxLength": maxTriageReasonRunes},
			}}}}
}

func (*triageSurface) Revision() uint64                     { return 0 }
func (*triageSurface) AnnotationsOf(string) mcp.Annotations { return mcp.Annotations{} }
func (*triageSurface) Phase() string                        { return string(phase.Triage) }

func (s *triageSurface) Admit(name string, _ map[string]any) toolloop.Admitted {
	return func(_ context.Context, call llm.ToolCall) (toolloop.ToolResult, error) {
		var got struct {
			Decision triageDecision `json:"decision"`
			Reason   string         `json:"reason"`
		}
		err := structured.Remarshal(call.Arguments, &got)
		if name != prompts.TriageTool || err != nil || !got.Decision.Valid() {
			return toolloop.ToolResult{Failed: true, Cause: mcp.RefusalInvalid,
				Output: "call " + prompts.TriageTool + " with {\"decision\": one of input, steer, request, " +
					"noise, \"reason\": \"one sentence\"}"}, nil
		}
		reason := strings.TrimSpace(got.Reason)
		if runes := []rune(reason); len(runes) > maxTriageReasonRunes {
			reason = string(runes[:maxTriageReasonRunes])
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.verdict, s.got = triageVerdict{decision: got.Decision, reason: reason}, true
		return toolloop.ToolResult{Output: "received"}, nil
	}
}

func (s *triageSurface) result() (triageVerdict, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.verdict, s.got
}

// triageDepsFor is what handle's triage for run runs on: the role's `triage`
// chain ([phase.Triage]), each member recorded in the auxiliary ledger as the
// running turn's [types.AuxWakeTriage] and charged to the seat's meter, asked
// before each call; this process's tool host; the seat's lease fence.
func (e *Engine) triageDepsFor(ctx context.Context, handle string, run sandbox.PendingRun) (triageDeps, error) {
	company := e.Company()
	if company == nil || company.Org == nil || company.Models == nil || e.sessions == nil {
		return triageDeps{}, errors.New("engine: this node runs no company with models and a tool host")
	}
	role := company.Org.Role(handle)
	if role == nil {
		return triageDeps{}, fmt.Errorf("engine: the chart holds no seat %q", handle)
	}
	walk, err := company.Models.Walk(role, phase.Triage)
	if err != nil {
		return triageDeps{}, err
	}
	members := walk.Members
	seam, _ := e.seamFor(company)
	use := auxspend.Use{Stage: types.AuxStageTurn, Purpose: types.AuxWakeTriage, TurnID: run.TurnID,
		WorkKey: run.WorkKey}
	// THE SEAT'S METER ON THE WRAPPER ALONE: the loop is handed no Budget, or
	// every call would be charged twice.
	if m := e.meterFor(company, handle); m != nil {
		m.observe(ctx)
		use.Budget = m
	}
	if members, err = seam.recordedMembers(role, members, use); err != nil {
		return triageDeps{}, err
	}
	provider, err := chain.New(members, chain.Options{Fleet: walk.Picks,
		Begin: func(key string) func() { return company.Models.Begin(role, phase.Triage, key) }})
	if err != nil {
		return triageDeps{}, err
	}
	return triageDeps{provider: provider, key: members[0].Key, host: e.sessions, fence: e.seatFence(handle),
		now: e.now}, nil
}

// triage is handle's triage of w for run, weighed for rel: a failure is a
// verdict of none, which leaves the wake on the inbox.
func (e *Engine) triage(ctx context.Context, handle string, run sandbox.PendingRun, w steer.Wake,
	rel wakeRelations) triageVerdict {

	deps, err := e.triageDepsFor(ctx, handle, run)
	if err != nil {
		return triageVerdict{failure: err.Error()}
	}
	requester := cmp.Or(run.Requester, run.RequesterLogin)
	v, err := runTriage(ctx, deps, prompts.WakeTriage{Seat: handle, Task: run.TaskDescription,
		Requester: requester, Wake: w, InConversation: rel.conversation, AboutItem: rel.item,
		AboutWritten: rel.wrote, ByLead: rel.lead})
	if err != nil {
		return triageVerdict{failure: err.Error()}
	}
	return v
}

// wakeRelations is why a wake is weighed for a run at all: written in its
// conversation, about its work item, about a record it created or wrote, or by
// somebody who leads the seat.
type wakeRelations struct{ conversation, item, wrote, lead bool }

// relationsOf is o's relations to run on seat handle, wrote whether it is about
// a record the run wrote ([wroteAbout]). The lead is the chart's answer
// ([ChartAuthority.Leads]), as `steer_turn` decides it, for a writer writing as
// a seat; a chart that cannot tell is no.
func (e *Engine) relationsOf(ctx context.Context, handle string, run sandbox.PendingRun, o wakeOrigin,
	wrote bool) wakeRelations {

	rel := wakeRelations{
		conversation: run.ConversationKey != "" && o.conversation == run.ConversationKey,
		item:         sameItem(run.WorkItem, o.item),
		wrote:        wrote,
	}
	if o.login == "" && o.requester != "" && o.requester != handle {
		leads, err := ChartAuthorityOf(e).Leads(ctx, o.requester, handle)
		rel.lead = err == nil && leads
	}
	return rel
}

// sameItem reports whether a and b name one work item, neither empty.
func sameItem(a, b *types.WorkItem) bool {
	return a != nil && b != nil && a.ID != "" && a.Backend == b.Backend && a.ID == b.ID
}

// candidate reports whether a wake [routeOf] left on the inbox for why is one
// the triage weighs, given its relations. ONLY A WAKE THE FIXED RULE LEFT FOR
// ITS WRITER OR ITS WORK: a type the ledger does not key, a schedule's fire,
// one already worked and one with no words stay as they are, with no call.
func candidate(why string, rel wakeRelations) bool {
	switch why {
	case stayNoRequester, stayWriter, stayOtherWork:
		return rel.conversation || rel.item || rel.wrote || rel.lead
	}
	return false
}

// gateAction is what the engine does with a triaged wake.
type gateAction int

const (
	// gateStay: it waits for a turn of its own.
	gateStay gateAction = iota
	// gateJoin: filed on the run's record as a wake item.
	gateJoin
	// gateSteer: filed as its writer's steer note.
	gateSteer
	// gateAbsorb: recorded worked by the running turn, as asking nothing.
	gateAbsorb
)

// wakeFacts is what the engine knows of a wake's writer and obligation, never
// from the triage: whether the writer is the run's requester, a lead of the
// seat (the chart) and a person, and whether the wake owes anything.
type wakeFacts struct{ requester, lead, person, owes bool }

// gateOf is the engine's decision on v for a wake of facts (ADR-0168). Input
// joins from anybody; a steer joins as a note only from a person who leads the
// seat, and anybody else's is a request; a request waits; noise is absorbed
// only owing nothing; a failure waits.
func gateOf(v triageVerdict, f wakeFacts) gateAction {
	switch {
	case v.failed():
		return gateStay
	case v.decision == triageInput:
		return gateJoin
	case v.decision == triageSteer && f.lead && f.person:
		return gateSteer
	case v.decision == triageNoise && !f.owes:
		return gateAbsorb
	}
	return gateStay
}

// owesAnything reports whether ev asks the seat for anything: an answer its
// delivery would oblige a turn to ([ReplyFor]), an address, or a question put
// to it ([obliges]). Such a wake is never absorbed (ADR-0120).
func owesAnything(ev *events.Event) bool {
	return obliges(ev) || ReplyFor([]*events.Event{ev}).Awaited()
}

// isRequester reports whether o is run's requester, which names one.
func isRequester(run sandbox.PendingRun, o wakeOrigin) bool {
	return (run.Requester != "" || run.RequesterLogin != "") && o.requester == run.Requester &&
		o.login == run.RequesterLogin && sameWorkingFor(o.workingFor, run.WorkingFor)
}

// routeMemo is what a router remembers across reads, by job: the wakes
// decided, each wake's triage verdict and how many triages each job made, and
// the wakes whose fold-in it saw asked and no answer settle (foldin.go); and,
// by turn and lead, a steer note's wakes whose filing the store did not
// answer, filed as exactly that note again.
type routeMemo struct {
	decided    map[string]map[string]bool
	verdicts   map[string]map[string]triageVerdict
	triaged    map[string]int
	foldIns    map[string]map[string]string
	unanswered map[string][]string
}

func newRouteMemo() *routeMemo {
	return &routeMemo{decided: map[string]map[string]bool{}, verdicts: map[string]map[string]triageVerdict{},
		triaged: map[string]int{}, foldIns: map[string]map[string]string{}, unanswered: map[string][]string{}}
}

// ask remembers that run's job's fold-in for wake is asked and unsettled.
func (m *routeMemo) ask(run sandbox.PendingRun, wake string) {
	if m.foldIns[run.LaunchID] == nil {
		m.foldIns[run.LaunchID] = map[string]string{}
	}
	m.foldIns[run.LaunchID][wake] = run.TurnID
}

// settle forgets job's fold-in for wake: an answer settled it.
func (m *routeMemo) settle(job, wake string) { delete(m.foldIns[job], wake) }

func (m *routeMemo) done(job, id string) bool { return m.decided[job][id] }

func (m *routeMemo) mark(job, id string) {
	if m.decided[job] == nil {
		m.decided[job] = map[string]bool{}
	}
	m.decided[job][id] = true
}

func (m *routeMemo) verdict(job, id string) (triageVerdict, bool) {
	v, ok := m.verdicts[job][id]
	return v, ok
}

func (m *routeMemo) keep(job, id string, v triageVerdict) {
	if m.verdicts[job] == nil {
		m.verdicts[job] = map[string]triageVerdict{}
	}
	m.verdicts[job][id] = v
	m.triaged[job]++
}

// prune forgets every job and turn runs no longer holds.
func (m *routeMemo) prune(runs []sandbox.PendingRun) {
	held := func(job string) bool {
		return slices.ContainsFunc(runs, func(run sandbox.PendingRun) bool { return run.LaunchID == job })
	}
	for job := range m.decided {
		if !held(job) {
			delete(m.decided, job)
		}
	}
	for job := range m.verdicts {
		if !held(job) {
			delete(m.verdicts, job)
			delete(m.triaged, job)
		}
	}
	for key := range m.unanswered {
		if !slices.ContainsFunc(runs, func(run sandbox.PendingRun) bool {
			return strings.HasPrefix(key, run.TurnID+"/")
		}) {
			delete(m.unanswered, key)
		}
	}
}

// routeRead is one read's own state: the triages it made and the steers it
// gathered, each run's lead's filed as one note once every wake is weighed,
// and what each run created or wrote, by turn ([Engine.writtenOf]).
type routeRead struct {
	triages int
	steers  []*steerBurst
	wrote   map[string][]types.WrittenRecord
}

// steerBurst is one lead's steer wakes for one run in one read.
type steerBurst struct {
	run   sandbox.PendingRun
	lead  string
	wakes []steer.Wake
}

func (r *routeRead) addSteer(run sandbox.PendingRun, lead string, w steer.Wake) {
	for _, b := range r.steers {
		if b.run.TurnID == run.TurnID && b.lead == lead {
			b.wakes = append(b.wakes, w)
			return
		}
	}
	r.steers = append(r.steers, &steerBurst{run: run, lead: lead, wakes: []steer.Wake{w}})
}

// Why a triaged wake stays, beside [routeOf]'s, as seat_wake_left_queued says.
const (
	stayTriageSpent = "triage_spent"  // the job weighed [maxJobTriages] already
	stayNotLead     = "not_lead"      // a steer from somebody who does not lead the seat
	stayOwes        = "owes"          // noise that owes an answer, so never absorbed
	stayRequest     = "request"       // asks for work of its own
	stayNotesSpent  = "notes_spent"   // the turn took every note it takes
	stayNoLedger    = "no_ledger"     // no completion ledger to record it worked in
	stayUndecided   = "triage_failed" // the triage decided nothing
)

// triageRun weighs ev, which [routeOf] left on the inbox for run and which is a
// [candidate] for rel, and acts on the gate's decision: a verdict asked once
// per wake and job, within the job's and the read's bounds, kept for every
// later read. A steer is gathered into read, filed after it ([Engine.fileSteers]).
func (e *Engine) triageRun(ctx context.Context, handle string, run sandbox.PendingRun, ev *events.Event,
	o wakeOrigin, rel wakeRelations, memo *routeMemo, read *routeRead) {

	id, job := ev.ID.String(), run.LaunchID
	w, ok := e.wakeFor(handle, ev, o)
	if !ok {
		memo.mark(job, id)
		e.noteRouted(ctx, handle, run, id, routeLeftQueued, ruleNone, stayNoWords, nil)
		return
	}
	// A FOLD-IN THE JOB ASKED FOR IT DECIDES IT, whatever this router
	// remembers: the row is the question's one copy (foldin.go).
	if f, asked := run.FoldInFor(id); asked {
		e.followFoldIn(ctx, handle, run, w, f, ev, memo)
		return
	}
	v, known := memo.verdict(job, id)
	if !known {
		switch {
		case memo.triaged[job] >= maxJobTriages:
			memo.mark(job, id)
			e.noteRouted(ctx, handle, run, id, routeLeftQueued, ruleNone, stayTriageSpent, nil)
			return
		case read.triages >= triagesPerRead:
			return
		}
		read.triages++
		v = e.triage(ctx, handle, run, w, rel)
		memo.keep(job, id, v)
		e.noteTriaged(ctx, handle, run, id, v)
	}
	facts := wakeFacts{requester: isRequester(run, o), lead: rel.lead,
		person: o.login == "" && e.seatIsHuman(o.requester), owes: owesAnything(ev)}
	switch gateOf(v, facts) {
	case gateJoin:
		w.Informs = !facts.requester
		e.fileJoin(ctx, handle, run, w, memo)
	case gateSteer:
		read.addSteer(run, o.requester, w)
	case gateAbsorb:
		e.absorbWake(ctx, handle, run, w, memo)
	default:
		why := stayRequest
		switch {
		case v.failed():
			why = stayUndecided
		case v.decision == triageSteer:
			why = stayNotLead
		case v.decision == triageNoise:
			why = stayOwes
		}
		// ANOTHER WRITER'S REQUEST IS PUT TO WHO MAY LET IT JOIN (foldin.go),
		// and waits on their answer undecided.
		if (why == stayRequest || why == stayNotLead) && foldable(o, facts) {
			asked, refused := e.askFoldIn(ctx, handle, run, w, ev, memo)
			if asked {
				return
			}
			why = refused
		}
		memo.mark(job, id)
		e.noteRouted(ctx, handle, run, id, routeLeftQueued, v.rule(), why, nil)
		e.tellWaiting(ctx, handle, ev, id, facts.person, runsAfterLine)
	}
}

// fileJoin files input w on run's record for its job, as the fixed rule's
// joins are: decided on an answer, refused for the job on a refusal, asked
// again on none.
func (e *Engine) fileJoin(ctx context.Context, handle string, run sandbox.PendingRun, w steer.Wake,
	memo *routeMemo) {

	const rule = ruleTriageInput

	err := e.runSteers.fileWake(ctx, run.TurnID, run.LaunchID, w)
	switch {
	case err == nil:
		memo.mark(run.LaunchID, w.ID)
		e.noteRouted(ctx, handle, run, w.ID, routeJoined, rule, "", nil)
	case errors.Is(err, sandbox.ErrWakeClosed), errors.Is(err, sandbox.ErrWakesSpent),
		errors.Is(err, sandbox.ErrWakeTooLarge):
		// NEVER AGAIN FOR THIS JOB: the record said no, and its delivery
		// runs as a turn of its own after the run.
		memo.mark(run.LaunchID, w.ID)
		e.noteRouted(ctx, handle, run, w.ID, routeRefused, rule, stayRefused, err)
	default:
		// THE STORE DID NOT ANSWER: undecided, so the next read asks again.
		log.WarnContext(ctx, "seat_wake_not_filed", "seat", handle, "turn_id", run.TurnID,
			"wake_id", w.ID, "error", err.Error(),
			"detail", "the run's record could not be reached; the next read files the wake again")
	}
}

// absorbWake records noise owing nothing worked by run's turn ([runSteers.took],
// under [absorbedKey]), so its delivery is skipped `absorbed` naming the turn.
// A ledger that did not take it leaves it undecided, asked again next read.
func (e *Engine) absorbWake(ctx context.Context, handle string, run sandbox.PendingRun, w steer.Wake,
	memo *routeMemo) {

	if e.runSteers.completions == nil {
		memo.mark(run.LaunchID, w.ID)
		e.noteRouted(ctx, handle, run, w.ID, routeLeftQueued, ruleTriageNoise, stayNoLedger, nil)
		return
	}
	if err := e.runSteers.took(ctx, handle, run.TurnID, absorbedKey, w); err != nil {
		log.WarnContext(ctx, "seat_wake_not_absorbed", "seat", handle, "turn_id", run.TurnID, "wake_id", w.ID,
			"error", err.Error(), "detail", "the completion ledger did not record it; the next read asks again")
		return
	}
	memo.mark(run.LaunchID, w.ID)
	e.runSteers.handOver(ctx, handle, w)
	e.noteRouted(ctx, handle, run, w.ID, routeAbsorbed, ruleTriageNoise, "", nil)
}

// steerNoteNamespace derives a lead's steer note's id from its turn and the
// wakes it was filed from, so a filing made again is the same note.
var steerNoteNamespace = uuid.MustParse("3c9e1f4a-7b2d-5c8e-9a1f-4e6d2b8c0a73")

// fileSteers files each lead's steer burst gathered in read ([Engine.fileSteer]).
func (e *Engine) fileSteers(ctx context.Context, handle string, read *routeRead, memo *routeMemo) {
	for _, b := range read.steers {
		e.fileSteer(ctx, handle, b, memo)
	}
}

// fileSteer files b as ONE note of its lead's (ADR-0067's seal and bounds): its
// wakes oldest first, as many as fit [steer.MaxNoteRunes], the rest left for
// the next. NOT WHILE A NOTE OF THE LEAD'S WAITS UNREAD on the record: a sealed
// note is never rewritten, and one a harness may already hold cannot be, so a
// burst arriving then waits and becomes the next note. A note full of the
// turn's waiting notes waits too; past the turn's note cap the wakes stay. An
// accepted note claims its wakes there ([runSteers.took], [steeredKey]): a
// note on a running turn's record is read by its run or its review.
func (e *Engine) fileSteer(ctx context.Context, handle string, b *steerBurst, memo *routeMemo) {
	run := b.run
	if e.runSteers.completions == nil {
		// NO NOTE IT COULD NOT CLAIM: its message would run again after it.
		for _, w := range b.wakes {
			memo.mark(run.LaunchID, w.ID)
			e.noteRouted(ctx, handle, run, w.ID, routeLeftQueued, ruleTriageSteer, stayNoLedger, nil)
		}
		return
	}
	if e.runSteers.leadNoteWaits(run, b.lead) {
		log.DebugContext(ctx, "seat_wake_steer_held", "seat", handle, "turn_id", run.TurnID, "lead", b.lead,
			"detail", "a note of the lead's waits unread; these join the next")
		return
	}
	wakes := slices.Clone(b.wakes)
	slices.SortStableFunc(wakes, func(x, y steer.Wake) int { return x.At.Compare(y.At) })
	key := run.TurnID + "/" + b.lead
	if ids := memo.unanswered[key]; ids != nil {
		again := slices.DeleteFunc(slices.Clone(wakes), func(w steer.Wake) bool { return !slices.Contains(ids, w.ID) })
		if len(again) == len(ids) {
			wakes = again
		}
		delete(memo.unanswered, key)
	}
	text, took := steerText(wakes)
	wakes = wakes[:took]
	if strings.TrimSpace(text) == "" {
		for _, w := range wakes {
			memo.mark(run.LaunchID, w.ID)
			e.noteRouted(ctx, handle, run, w.ID, routeLeftQueued, ruleTriageSteer, stayNoWords, nil)
		}
		return
	}
	ids := make([]string, 0, len(wakes))
	owes := turn.NoReply()
	for _, w := range wakes {
		ids = append(ids, w.ID)
		if owed := turn.ParseReply(w.Owes); w.Owes != "" && owed.Valid() {
			owes = strongerReply(owes, owed)
		}
	}
	note := steer.Note{ID: uuid.NewSHA1(steerNoteNamespace, []byte(run.TurnID+"/"+strings.Join(ids, "/"))).String(),
		Text: text, By: b.lead, ByKind: iam.ActorHuman, At: wakes[len(wakes)-1].At}
	if owes.Awaited() {
		note.Owes = owes.String()
	}
	filed, err := e.runSteers.File(ctx, run.TurnID, note)
	if err != nil {
		memo.unanswered[key] = ids
		log.WarnContext(ctx, "seat_wake_not_filed", "seat", handle, "turn_id", run.TurnID, "note_id", note.ID,
			"error", err.Error(), "detail", "the run's record could not be reached; the next read files the "+
				"lead's note again")
		return
	}
	switch filed.Status {
	case steer.StatusAccepted:
		for _, w := range wakes {
			if err := e.runSteers.took(ctx, handle, run.TurnID, steeredKey, w); err != nil {
				log.WarnContext(ctx, "steer_wake_unclaimed", "turn_id", run.TurnID, "wake_id", w.ID,
					"error", err.Error(), "detail", "the lead's note was filed, and the completion ledger did "+
						"not record its message worked: that message also runs as a turn of its own")
			}
			e.runSteers.seeThrough(ctx, handle, w)
			e.runSteers.handOver(ctx, handle, w)
			memo.mark(run.LaunchID, w.ID)
			e.noteRouted(ctx, handle, run, w.ID, routeJoined, ruleTriageSteer, "", nil)
		}
	case steer.StatusFull:
		log.DebugContext(ctx, "seat_wake_steer_held", "seat", handle, "turn_id", run.TurnID, "lead", b.lead,
			"detail", "the turn holds as many unread notes as it takes; the lead's waits for the next read")
	default:
		why := stayNotesSpent
		if filed.Status == steer.StatusClosed {
			why = stayRefused
		}
		for _, w := range wakes {
			memo.mark(run.LaunchID, w.ID)
			e.noteRouted(ctx, handle, run, w.ID, routeRefused, ruleTriageSteer, why,
				fmt.Errorf("the run's record answered %s", filed.Status))
		}
	}
}

// steerText is wakes' words joined as one note, oldest first, and how many of
// wakes it holds: whole wakes while they fit [steer.MaxNoteRunes], and the
// first alone, cut and saying how much more is where it was written, where it
// does not fit even alone.
func steerText(wakes []steer.Wake) (string, int) {
	const sep = "\n\n"
	var parts []string
	runes := 0
	for _, w := range wakes {
		text := strings.TrimSpace(w.Text)
		n := utf8.RuneCountInString(text)
		if len(parts) > 0 {
			n += len(sep)
		}
		if runes+n <= steer.MaxNoteRunes {
			parts, runes = append(parts, text), runes+n
			continue
		}
		if len(parts) == 0 {
			whole := []rune(text)
			tail := func(left int) string {
				return sep + "(" + strconv.Itoa(left) + " more characters of this message are where it was written.)"
			}
			keep := steer.MaxNoteRunes - utf8.RuneCountInString(tail(len(whole)))
			return string(whole[:keep]) + tail(len(whole)-keep), 1
		}
		break
	}
	return strings.Join(parts, sep), len(parts)
}

// leadNoteWaits reports whether a note lead filed waits on run's record for a
// step to read it: every person's note unread, but one a hook's live serve
// handed to run's job, as the record counts what waits.
func (s *runSteers) leadNoteWaits(run sandbox.PendingRun, lead string) bool {
	for _, n := range run.Steer.Notes {
		if n.IsWake() || n.For != "" || n.LateCall != nil || !n.Waiting() {
			continue
		}
		if live, ok := n.LiveServe(); ok && live.Launch == run.LaunchID {
			continue
		}
		if note, ok := s.open(run.TurnID, n); ok && note.By == lead {
			return true
		}
	}
	return false
}

// took records w worked by turnID's turn in the completion ledger: mark of each
// of its triggers first, then every key its delivery is dropped by, as a claim
// records a read wake ([runSteers.claim]). Record is a create, so a repeat
// changes nothing; the first key not taken is the error.
func (s *runSteers) took(ctx context.Context, handle, turnID string, mark func(string) string, w steer.Wake) error {
	at := s.now().UTC()
	keys := make([]string, 0, len(w.Triggers)+len(w.Keys))
	for _, trigger := range w.Triggers {
		keys = append(keys, mark(trigger))
	}
	for _, key := range append(keys, w.Keys...) {
		if err := s.completions.Record(ctx, handle, key, turnID, at); err != nil {
			return err
		}
	}
	return nil
}

// handOver tells the queued line w's deliveries went to the running work.
func (s *runSteers) handOver(ctx context.Context, handle string, w steer.Wake) {
	if s.handedOver != nil && len(w.Triggers) > 0 {
		s.handedOver(context.WithoutCancel(ctx), handle, w.Triggers)
	}
}

// steeredKey is the completion-ledger mark of a delivery filed as its lead's
// steer note on a running turn ([skipSteered]).
func steeredKey(trigger string) string { return workkey.Derive([]string{"wake-steered", trigger}) }

// absorbedKey is the completion-ledger mark of a delivery a running turn's
// triage absorbed ([skipTriaged]).
func absorbedKey(trigger string) string { return workkey.Derive([]string{"wake-absorbed", trigger}) }

// steeredReason is a steered skip's reason, naming the turn where the ledger does.
func steeredReason(turnID string) string {
	return "its writer leads the seat, and it was filed as their steer note on " + runningTurn(turnID) +
		": the turn's agent_turn_steered record says what became of it"
}

// triagedReason is a triaged skip's reason, naming the turn where the ledger does.
func triagedReason(turnID string) string {
	return "the triage of " + runningTurn(turnID) + " found it asks nothing and adds nothing that work needs"
}

// runningTurn names turnID as a skip's reason does.
func runningTurn(turnID string) string {
	if turnID == "" {
		return "a running turn"
	}
	return "running turn " + turnID
}

// noteTriaged says one triage's verdict (seat_wake_triaged), at WARN for one
// that decided nothing, once per wake and job since the verdict is kept.
func (e *Engine) noteTriaged(ctx context.Context, handle string, run sandbox.PendingRun, wakeID string,
	v triageVerdict) {

	if v.failed() {
		log.WarnContext(ctx, "seat_wake_triaged", "seat", handle, "turn_id", run.TurnID, "job", run.LaunchID,
			"wake_id", wakeID, "decision", "", "error", v.failure,
			"detail", "the triage decided nothing, so the wake waits for a turn of its own")
		return
	}
	log.InfoContext(ctx, "seat_wake_triaged", "seat", handle, "turn_id", run.TurnID, "job", run.LaunchID,
		"wake_id", wakeID, "decision", string(v.decision), "reason", v.reason)
}
