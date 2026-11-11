package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/schedule"
	sshint "github.com/crewlet/crewlet/internal/ssh"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Delegated decisions on this node (ADR-0125, internal/delegated): the live
// rules a held call is routed by, the delegate's wake once the call is parked,
// the request read where it waits when the delegate's turn starts, the desk
// the verdict tool carries a verdict out through, and the sweep that puts a
// request to people once its delegates ran out of time or delegation was
// turned off.

// delegatedSource is the wake's source, as a pickup's is its own.
const delegatedSource = "delegated_decisions"

// delegatedNamespace derives a delegate wake's id: the same on every node and
// every attempt, so a wake published twice is one delivery.
var delegatedNamespace = uuid.NewSHA1(uuid.NameSpaceOID, []byte("crewlet.delegated.review"))

// delegatedRules is [builtin.DelegatedRules] over the running document.
type delegatedRules struct{ engine *Engine }

// Rule is the live rule for kind: its delegates, its number and its
// spot-check rate.
func (r delegatedRules) Rule(kind delegated.Kind) ([]string, int, int, bool) {
	c := r.engine.Company()
	if c == nil || c.Config == nil {
		return nil, 0, 0, false
	}
	rule, n, ok := c.Config.DelegatedDecisions.RuleFor(kind)
	if !ok {
		return nil, 0, 0, false
	}
	return slices.Clone(rule.Delegates), n, rule.SampleRate(), true
}

// DelegatedToolDeps are the verdict tool's dependencies on this node, the
// zero value (which omits the tool) without a fleet store or a queue.
func DelegatedToolDeps(e *Engine) builtin.DelegatedDeps {
	if e == nil || e.backends == nil || e.backends.Fleet == nil || e.backends.Queue == nil {
		return builtin.DelegatedDeps{}
	}
	return builtin.DelegatedDeps{Desk: delegatedDesk{engine: e}}
}

// delegatedDesk is [builtin.DelegatedDesk] on this node.
type delegatedDesk struct{ engine *Engine }

var _ builtin.DelegatedDesk = delegatedDesk{}

func (d delegatedDesk) answers() sandbox.AnswerDesk {
	b := d.engine.backends
	return sandbox.AnswerDesk{Pending: sandbox.NewCoordStore(b.Fleet), Queue: b.Queue, Seal: d.engine.AnswerSeal()}
}

func (d delegatedDesk) Rule(kind delegated.Kind) ([]string, int, int, bool) {
	return delegatedRules(d).Rule(kind)
}

func (d delegatedDesk) SSHPatterns() []string {
	if s := d.engine.Company().SSH(); s != nil {
		return slices.Clone(s.AlwaysEscalate)
	}
	return nil
}

// RulePaths are the always-escalate path globs the rule naming kind adds.
func (d delegatedDesk) RulePaths(kind delegated.Kind) []string {
	c := d.engine.Company()
	if c == nil || c.Config == nil {
		return nil
	}
	rule, _, ok := c.Config.DelegatedDecisions.RuleFor(kind)
	if !ok {
		return nil
	}
	return slices.Clone(rule.AlwaysEscalatePaths)
}

func (d delegatedDesk) Held(ctx context.Context, id string) (sandbox.PendingRun, bool, error) {
	return d.answers().Held(ctx, id)
}

func (d delegatedDesk) Arguments(run sandbox.PendingRun) (map[string]any, error) {
	return sandbox.NewCallSeal(d.engine.cipher).OpenArguments(run.TurnID, *run.HeldCall)
}

// Leads reports whether lead is answerable for seat: one of its managers, or
// the lead of a unit that holds it.
func (d delegatedDesk) Leads(_ context.Context, lead, seat string) (bool, error) {
	c := d.engine.Company()
	if c == nil || c.Org == nil {
		return false, errors.New("engine: no company is running to read the chart from")
	}
	o := c.Org
	subject := o.SeatByHandle(seat)
	if subject == nil {
		return false, nil
	}
	for _, r := range leadChain(o, subject) {
		if r == lead {
			return true, nil
		}
	}
	for _, unit := range o.UnitChainFor(subject) {
		if l := o.EffectiveLead(unit); l != nil && l.Handle() == lead {
			return true, nil
		}
	}
	return false, nil
}

// Labels are the tags of a Worklet task, none for no task or another
// tracker's item.
func (d delegatedDesk) Labels(ctx context.Context, item *types.WorkItem) ([]string, error) {
	if item == nil || item.ID == "" || item.Backend != types.WorkWorklet {
		return nil, nil
	}
	tracker := d.engine.Tracker()
	if tracker == nil {
		return nil, errors.New("engine: this node reads no tracker")
	}
	detail, err := tracker.Task(ctx, item.ID, worklet.DetailWants{},
		statelog.Freshness{Level: statelog.ReadConsistentPrefix})
	if err != nil {
		return nil, err
	}
	return detail.Task.Tags, nil
}

func (d delegatedDesk) Allow(ctx context.Context, bucket string, limit int, now time.Time) (bool, error) {
	return d.engine.backends.Fleet.Allow(ctx, bucket, limit, time.Hour, now)
}

func (d delegatedDesk) Give(ctx context.Context, run sandbox.PendingRun,
	v sandbox.HeldVerdict) (sandbox.HeldCall, bool, error) {
	return sandbox.NewCoordStore(d.engine.backends.Fleet).GiveDelegateVerdict(ctx, run.TurnID, run.LaunchID,
		run.HeldCall.ID, v)
}

func (d delegatedDesk) Escalate(ctx context.Context, run sandbox.PendingRun, e sandbox.HeldEscalation) (bool, error) {
	return sandbox.NewCoordStore(d.engine.backends.Fleet).EscalateHeld(ctx, run.TurnID, run.LaunchID,
		run.HeldCall.ID, e)
}

// Answer gives the held call choice as the delegate's answer, stamped with the
// rule it decided under, through the answer desk a person's approval takes.
func (d delegatedDesk) Answer(ctx context.Context, run sandbox.PendingRun, choice, said, by string, rule int) error {
	_, err := d.answers().Deliver(ctx, types.ExecutorAnswerGiven{
		TurnID: run.TurnID, Agent: run.AgentID, AgentHandle: run.AgentHandle,
		Answer: said, Choice: choice, AnsweredBy: by, AnsweredByKind: string(iam.ActorAgent),
		LaunchID: run.LaunchID, Approval: run.HeldCall.ID, DelegatedRule: rule,
	})
	return err
}

func (d delegatedDesk) RecordVerdict(ctx context.Context, v builtin.DelegatedVerdict) (bool, error) {
	return sshServers(d).RecordVerdict(ctx, v)
}

// MayApprove reports whether the person holding seat may approve an operate
// command now: an administrator or a person the document names, decided as a
// check so the proof's age is not asked again, since it was at their answer.
func (s sshServers) MayApprove(ctx context.Context, seat string) (bool, error) {
	c := s.engine.Company()
	if c == nil || c.Config == nil {
		return false, nil
	}
	p, err := turnRequesters(s).Requester(ctx, seat)
	if err != nil {
		return false, err
	}
	if p.Kind != iam.KindPerson {
		return false, nil
	}
	d := authz.Decide(authz.WithCheck(ctx), p, authz.ActionSSHOperateApprove,
		authz.Object{Kind: authz.KindPerson, Named: c.Config.SSHMayApprove()}, ChartAuthorityOf(s.engine), time.Now())
	if d.Err != nil {
		return false, d.Err
	}
	return d.Allowed, nil
}

// ---- the delegate's wake ------------------------------------------------ //

// wakeDelegates puts a run's held call, parked for its delegates, to each of
// them ([sandbox.CoordinatorOptions.Parked]). A wake that does not land is
// logged: the deadline sweep puts the call to people when its time runs out.
func (e *Engine) wakeDelegates(ctx context.Context, run sandbox.PendingRun) {
	held := run.HeldCall
	if held == nil || !held.Delegated() {
		return
	}
	company := e.Company()
	if company == nil || company.Org == nil {
		return
	}
	d := held.Delegation
	if word, said := ownRequest(run); word != "" {
		// A DELEGATE'S OWN REQUEST is put to people now, not at its deadline:
		// its delegate never decides it, and a panel's other half never
		// allows alone.
		if e.backends != nil && e.backends.Fleet != nil {
			escalateHeld(ctx, sandbox.NewCoordStore(e.backends.Fleet), sshServers{engine: e},
				delegatedDesk{engine: e}.Arguments, run, word, said, time.Now())
		}
		return
	}
	var panel []string
	if len(d.Delegates) > 1 {
		panel = slices.Clone(d.Delegates)
	}
	for _, handle := range d.Delegates {
		seat := company.Org.AgentSeatByHandle(handle)
		id, ok := company.Org.AgentIDFor(seat)
		if !ok {
			log.WarnContext(ctx, "delegated_review_unaddressed", "delegate_seat", handle, "request", held.ID,
				"detail", "the chart holds no such agent seat; the request goes to people at its deadline")
			continue
		}
		ev := events.New(types.DelegatedReview{Agent: id.String(), AgentHandle: handle, Kind: held.Act,
			Rule: d.Rule, Panel: panel, RequestID: held.ID, HeldTurn: run.TurnID, HeldLaunch: run.LaunchID,
			Requester: run.AgentHandle, At: held.HeldAt}, events.TraceContext{TraceID: run.TraceID,
			ParentSpanID: run.SpanID})
		ev.ID = uuid.NewSHA1(delegatedNamespace, []byte(held.ID+"/"+handle))
		ev.Source = delegatedSource
		conversation := "delegated/" + held.ID
		notify.Stamp(ev, conversation, conversation)
		if err := e.wakes().Publish(ctx, topics.AgentInbox(id), ev); err != nil {
			log.WarnContext(ctx, "delegated_review_unpublished", "delegate_seat", handle, "request", held.ID,
				"error", err.Error(), "detail", "the request goes to people at its deadline")
		}
	}
}

// readDelegatedRequests reads every delegated review among a seat's delivery
// where its request waits, so the turn's brief shows it whole, and drops a
// review whose request no longer waits on this seat: it starts no turn.
func (e *Engine) readDelegatedRequests(ctx context.Context, handle string, evs []*events.Event) []*events.Event {
	out := evs[:0:0]
	for _, ev := range evs {
		r, ok := events.DataAs[*types.DelegatedReview](ev)
		if !ok || r == nil {
			out = append(out, ev)
			continue
		}
		request, waiting := e.delegatedRequest(ctx, handle, *r)
		if !waiting {
			log.InfoContext(ctx, "delegated_review_settled", "delegate_seat", handle, "request", r.RequestID,
				"detail", "the request no longer waits on this delegate; no turn runs for it")
			continue
		}
		ev.Data = &types.DelegatedReviewRead{DelegatedReview: *r, Request: request}
		out = append(out, ev)
	}
	return out
}

// delegatedRequest is r's request as the delegate reads it, and false where it
// no longer waits on handle.
func (e *Engine) delegatedRequest(ctx context.Context, handle string,
	r types.DelegatedReview) (*types.DelegatedRequest, bool) {

	desk := delegatedDesk{engine: e}
	if e.backends == nil || e.backends.Fleet == nil {
		return &types.DelegatedRequest{Unread: "this node reaches no run records"}, true
	}
	run, found, err := sandbox.NewCoordStore(e.backends.Fleet).Get(ctx, r.HeldTurn)
	switch {
	case err != nil:
		return &types.DelegatedRequest{Unread: "the run holding it could not be read"}, true
	case !found || run.HeldCall == nil || run.HeldCall.ID != r.RequestID || !run.HeldCall.Delegated() ||
		!slices.Contains(sandbox.Awaiting, run.Status) || run.LaunchID != run.HeldCall.LaunchID:
		return nil, false
	case !slices.Contains(run.HeldCall.Delegation.Delegates, handle) ||
		slices.ContainsFunc(run.HeldCall.Delegation.Given, func(g sandbox.HeldVerdict) bool { return g.By == handle }):
		return nil, false
	}
	args, err := desk.Arguments(run)
	if err != nil {
		return &types.DelegatedRequest{Unread: "its arguments do not open on this node"}, true
	}
	switch delegated.Kind(run.HeldCall.Act) {
	case delegated.KindPRMerge:
		return mergeRequest(run, args), true
	case delegated.KindDeploy:
		return deployRequest(run, args), true
	}
	return sshRequest(e.Company(), run, args, desk.SSHPatterns()), true
}

// releaseChecks is what the engine holds a merge's or a deploy's allow to, as
// its delegate is told before it decides.
const releaseChecks = "Read the change before you answer: " + toolname.CodeGetChange + " for a Gitlet change, " +
	"your own GitHub access for a pull request. Whatever you answer, Crewlet puts an allow to people where, read " +
	"when you answer, "

// mergeRequest is a held ask_to_merge call as its delegate reads it: the
// change, the head and the method in the engine's words, the reason fenced,
// and the conditions an allow is held to.
func mergeRequest(run sandbox.PendingRun, args map[string]any) *types.DelegatedRequest {
	change, _ := args["change"].(string)
	head, _ := args["head"].(string)
	method, _ := args["method"].(string)
	reason, _ := args["reason"].(string)
	if method == "" {
		method = "merge"
	}
	about := fmt.Sprintf("%s asks to merge the change below at exactly the head %s, as a %s; an allow merges "+
		"that head as %s, and a change pushed to since is not merged.", run.AgentHandle, head, method,
		run.AgentHandle)
	if item := run.WorkItem; item != nil && item.Key != "" {
		about += " The run is on work item " + item.Key + "."
	}
	about += " " + releaseChecks + "the head is not the one asked about, a check its target requires has not " +
		"passed on it, it does not merge cleanly, its target's own gate holds it back, nobody but its authors " +
		"and " + run.AgentHandle + " approved that head, it or its item is labelled security, privacy or legal, " +
		"or it touches a path no delegate allows (CI pipelines, owners files, secret files, access policies, " +
		"infrastructure, migrations, company documents, and the rule's own)."
	return &types.DelegatedRequest{About: about, Digest: run.HeldCall.Delegation.Digest,
		Parts: []types.RequestPart{{Title: "Change", Text: change, Fenced: true}, {Title: "Head", Text: head},
			{Title: "Reason given", Text: reason, Fenced: true}}}
}

// deployRequest is a held ask_to_deploy call as its delegate reads it: the
// repository, the commit, the changes and the environment in the engine's
// words, the rollback and the reason fenced, and the conditions an allow is
// held to.
func deployRequest(run sandbox.PendingRun, args map[string]any) *types.DelegatedRequest {
	repo, _ := args["repo"].(string)
	commit, _ := args["commit"].(string)
	environment, _ := args["environment"].(string)
	rollback, _ := args["rollback"].(string)
	reason, _ := args["reason"].(string)
	var changes []string
	if list, ok := args["changes"].([]any); ok {
		for _, c := range list {
			if text, ok := c.(string); ok {
				changes = append(changes, text)
			}
		}
	}
	about := fmt.Sprintf("%s asks to deploy commit %s of the repository below to %s; an allow tells it it may "+
		"deploy exactly that commit there, once, with its own tools.", run.AgentHandle, commit, environment)
	if item := run.WorkItem; item != nil && item.Key != "" {
		about += " The run is on work item " + item.Key + "."
	}
	about += " " + releaseChecks + "a change it names has not merged, the changes merged into different " +
		"branches, the commit does not hold each change or is not on their branch, a check the branch requires " +
		"has not passed on the commit, a change or its item is labelled security, privacy or legal, or a " +
		"change touches a path no delegate allows. Judge the rollback yourself: escalate one that would not " +
		"undo the deploy."
	return &types.DelegatedRequest{About: about, Digest: run.HeldCall.Delegation.Digest,
		Parts: []types.RequestPart{{Title: "Repository", Text: repo, Fenced: true}, {Title: "Commit", Text: commit},
			{Title: "Changes", Text: words.And(changes), Fenced: true}, {Title: "Environment", Text: environment},
			{Title: "Rollback given", Text: rollback, Fenced: true}, {Title: "Reason given", Text: reason, Fenced: true}}}
}

// sshRequest is a held ssh_operate call as its delegate reads it: the server,
// the command and its stdin whole, the reason, the decision it carries out and
// what the always-escalate list matches.
func sshRequest(c *Company, run sandbox.PendingRun, args map[string]any, patterns []string) *types.DelegatedRequest {
	held := run.HeldCall
	host, _ := args["host"].(string)
	command, _ := args["command"].(string)
	stdin, _ := args["stdin"].(string)
	reason, _ := args["reason"].(string)
	var h struct {
		address, user, description string
		port                       int
	}
	if s := c.SSH(); s != nil {
		if found, ok := s.Host(host); ok {
			h.address, h.user, h.description, h.port = found.Address, found.Users.Operate, found.Description,
				found.PortOrDefault()
		}
	}
	about := fmt.Sprintf("%s asks to run a command on the server %s (%s, port %d) as its operate user %s.",
		run.AgentHandle, host, h.address, h.port, h.user)
	if h.description != "" {
		about += " The server is: " + h.description + "."
	}
	if item := run.WorkItem; item != nil && item.Key != "" {
		about += " The run is on work item " + item.Key + "."
	}
	if act := held.Acting; act != nil {
		about += fmt.Sprintf(" It carries out a decision %s answered on %s.", act.AnsweredBy, act.Key)
	}
	parts := []types.RequestPart{{Title: "Command, exactly", Text: command, Fenced: true}}
	if stdin != "" {
		parts = append(parts, types.RequestPart{Title: fmt.Sprintf("Standard input, all %d bytes", len(stdin)),
			Text: stdin, Fenced: true})
	} else {
		parts = append(parts, types.RequestPart{Title: "Standard input", Text: "none"})
	}
	parts = append(parts, types.RequestPart{Title: "Reason given", Text: reason, Fenced: true})
	if act := held.Acting; act != nil {
		parts = append(parts, types.RequestPart{Title: "The decision's question", Text: act.Question, Fenced: true})
	}
	var escalates []string
	for _, m := range sshint.AlwaysEscalate(command, []byte(stdin), patterns) {
		escalates = append(escalates, "The command "+m.Says+".")
	}
	return &types.DelegatedRequest{About: about, Parts: parts, Digest: held.Delegation.Digest, Escalates: escalates}
}

// ---- the sweep ---------------------------------------------------------- //

// delegatedSweepName is the duty's lease, and its interval how often it looks:
// a request goes to people within a minute of its deadline, or of delegation
// being turned off.
const (
	delegatedSweepName     = "delegated-decisions"
	delegatedSweepInterval = time.Minute
	delegatedSweepTTL      = 3 * delegatedSweepInterval
)

// delegatedSweep puts to people every held call that waited on its delegates
// past [delegated.Deadline], or whose kind no live rule delegates to those
// delegates any more.
type delegatedSweep struct {
	engine *Engine
	claim  schedule.DutyFunc
	now    func() time.Time
	stop   context.CancelFunc
	done   chan struct{}
}

// startDelegatedSweep arms the sweep on a node that runs workers.
func (e *Engine) startDelegatedSweep(ctx context.Context) {
	if !e.profile.RunsWorkers() || e.backends == nil || e.backends.Fleet == nil {
		return
	}
	s := &delegatedSweep{engine: e, claim: e.workerDuty(delegatedSweepName, delegatedSweepTTL),
		now: time.Now, done: make(chan struct{})}
	// DETACHED from the caller's context, as every long-running loop here is.
	loop, stop := context.WithCancel(context.WithoutCancel(ctx))
	s.stop = stop
	e.delegatedSweep = s
	go s.run(loop)
}

// stopDelegatedSweep ends the sweep, waiting for an in-flight pass.
func (e *Engine) stopDelegatedSweep() {
	if e.delegatedSweep == nil {
		return
	}
	e.delegatedSweep.stop()
	<-e.delegatedSweep.done
	e.delegatedSweep = nil
}

func (s *delegatedSweep) run(ctx context.Context) {
	defer close(s.done)
	ticker := time.NewTicker(delegatedSweepInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if held, err := s.holds(ctx); err != nil {
			log.WarnContext(ctx, "delegated_sweep_unclaimed", "error", err.Error())
		} else if held {
			s.pass(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// holds is whether this node holds the duty this pass: a nil claim is a lone
// node, which always does.
func (s *delegatedSweep) holds(ctx context.Context) (bool, error) {
	if s.claim == nil {
		return true, nil
	}
	return s.claim(ctx)
}

// pass escalates every request due, and logs what it could not.
func (s *delegatedSweep) pass(ctx context.Context) {
	sweepDelegated(ctx, sandbox.NewCoordStore(s.engine.backends.Fleet), delegatedRules{engine: s.engine},
		sshServers{engine: s.engine}, delegatedDesk{engine: s.engine}.Arguments, s.now())
}

// delegatedRecorder records the engine's own escalation of a request.
type delegatedRecorder interface {
	RecordVerdict(ctx context.Context, v builtin.DelegatedVerdict) (bool, error)
}

// sweepDelegated puts to people every active held call that waited on its
// delegates past its deadline, or whose delegation the live rules no longer
// hold, recording each escalation once it is written, naming what it decided
// from the call's arguments as open opens them.
func sweepDelegated(ctx context.Context, store sandbox.PendingStore, rules builtin.DelegatedRules,
	audit delegatedRecorder, open func(sandbox.PendingRun) (map[string]any, error), now time.Time) {

	runs, err := store.ListActive(ctx)
	if err != nil {
		log.WarnContext(ctx, "delegated_sweep_unread", "error", err.Error())
		return
	}
	for _, run := range runs {
		held := run.HeldCall
		if held == nil || !held.Delegated() || !slices.Contains(sandbox.Awaiting, run.Status) ||
			run.Ending != nil || run.LaunchID != held.LaunchID {
			continue
		}
		word, said := ownRequest(run)
		if word == "" {
			delegates, _, _, live := rules.Rule(delegated.Kind(held.Act))
			word, said = dueEscalation(*held, delegates, live, now)
		}
		if word == "" {
			continue
		}
		escalateHeld(ctx, store, audit, open, run, word, said, now)
	}
}

// ownRequest is why a call one of its own delegates made goes to people at
// once, "" for one a delegate did not make: a delegate never decides its own
// request, and a panel's other delegate never allows alone.
func ownRequest(run sandbox.PendingRun) (string, string) {
	if !slices.Contains(run.HeldCall.Delegation.Delegates, run.AgentHandle) {
		return "", ""
	}
	return delegated.OverruledSelf, run.AgentHandle + " is a delegate of this rule, and no delegate decides " +
		"its own request, so it came to people."
}

// escalateHeld puts run's held call to people for word, said, and records the
// engine's escalation in the access log, naming what it decided from the
// call's arguments as open opens them. A call no longer waiting on its
// delegates is left alone.
func escalateHeld(ctx context.Context, store sandbox.PendingStore, audit delegatedRecorder,
	open func(sandbox.PendingRun) (map[string]any, error), run sandbox.PendingRun, word, said string,
	now time.Time) {

	held := run.HeldCall
	escalated, err := store.EscalateHeld(ctx, run.TurnID, run.LaunchID, held.ID, sandbox.HeldEscalation{
		Overruled: []string{word}, Engine: []string{said}, At: now.UTC()})
	switch {
	case err != nil:
		log.WarnContext(ctx, "delegated_escalation_failed", "request", held.ID, "error", err.Error())
		return
	case !escalated:
		return
	}
	// WHAT IT DECIDED, as every verdict on the request names it: the access
	// log refuses a record that names nothing.
	args, err := open(run)
	if err != nil {
		log.WarnContext(ctx, "delegated_escalation_unrecorded", "request", held.ID, "error", err.Error(),
			"detail", "the request was put to people; its arguments do not open here, so its record "+
				"in the access log names nothing it could hold")
		return
	}
	host, subject, stdin := builtin.RequestSubject(delegated.Kind(held.Act), args)
	item := ""
	if run.WorkItem != nil {
		item = cmp.Or(run.WorkItem.Key, run.WorkItem.ID)
	}
	if _, err := audit.RecordVerdict(ctx, builtin.DelegatedVerdict{
		At: held.HeldAt, Request: held.ID, Kind: held.Act, Rule: held.Delegation.Rule, Handle: run.AgentHandle,
		Turn: run.TurnID, ByKind: builtin.VerdictByEngine, Through: builtin.VerdictThroughDelegation,
		Verdict: string(delegated.Escalate), Final: true, Overruled: []string{word},
		Digest: held.Delegation.Digest, Host: host, Subject: subject, Stdin: stdin, Item: item,
	}); err != nil {
		log.WarnContext(ctx, "delegated_escalation_unrecorded", "request", held.ID, "error", err.Error(),
			"detail", "the request was put to people; its record in the access log did not land")
	}
}

// dueEscalation is why a call waiting on its delegates goes to people now, as
// the closed word and the sentence people read, "" for one that still waits:
// its kind no longer delegated (live false), a delegate it was put to no
// longer the rule's, or its deadline passed.
func dueEscalation(held sandbox.HeldCall, delegates []string, live bool, now time.Time) (string, string) {
	if !live {
		return delegated.OverruledOff, "Delegated decisions for " + held.Act + " were turned off, so the " +
			"request came to people."
	}
	for _, d := range held.Delegation.Delegates {
		if !slices.Contains(delegates, d) {
			return delegated.OverruledOff, d + " no longer decides " + held.Act + " requests, so the request " +
				"came to people."
		}
	}
	if !now.Before(held.HeldAt.Add(delegated.Deadline)) {
		return delegated.OverruledDeadline, "No delegate answered within " +
			strconv.Itoa(int(delegated.Deadline/time.Minute)) + " minutes, so the request came to people."
	}
	return "", ""
}

// ApprovesAct reports whether the person ctx resolves to may approve an act
// of kind now, at the proof its approval asks: for ssh_operate, an
// administrator or a person the document names, signed in again within the
// step-up window, as `POST /mcp-approvals/{id}` decides it (ADR-0125).
func (e *Engine) ApprovesAct(ctx context.Context, kind delegated.Kind) bool {
	if kind != delegated.KindSSHOperate {
		return false
	}
	p, how := iam.From(ctx)
	c := e.Company()
	if how != iam.Resolved || p.Kind != iam.KindPerson || c == nil || c.Config == nil {
		return false
	}
	d := authz.Decide(ctx, p, authz.ActionSSHOperateApprove,
		authz.Object{Kind: authz.KindPerson, Named: c.Config.SSHMayApprove()}, ChartAuthorityOf(e), time.Now())
	return d.Err == nil && d.Allowed
}
