package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
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

// Limits are the limits the running document holds every delegate to, the
// engine's defaults on a node running no company.
func (r delegatedRules) Limits() delegated.Limits {
	c := r.engine.Company()
	if c == nil || c.Config == nil {
		return delegated.DefaultLimits()
	}
	return c.Config.DelegatedDecisions.Limits()
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

// Limits are the limits the running document holds every delegate to.
func (d delegatedDesk) Limits() delegated.Limits {
	return delegatedRules(d).Limits()
}

// Release is what the rule naming kind adds to a merge's or a deploy's
// conditions.
func (d delegatedDesk) Release(kind delegated.Kind) delegated.Release {
	c := d.engine.Company()
	if c == nil || c.Config == nil {
		return delegated.Release{}
	}
	rule, _, ok := c.Config.DelegatedDecisions.RuleFor(kind)
	if !ok {
		return delegated.Release{}
	}
	return rule.Release()
}

func (d delegatedDesk) Held(ctx context.Context, id string) (sandbox.PendingRun, bool, error) {
	return d.answers().Held(ctx, id)
}

func (d delegatedDesk) RawArguments(run sandbox.PendingRun) (map[string]any, error) {
	return sandbox.NewCallSeal(d.engine.cipher).OpenRawArguments(run.TurnID, *run.HeldCall)
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

// answerAsEngine answers run's held call not carried out, as the engine, with
// said as its reason ([mergedAlready]).
func (d delegatedDesk) answerAsEngine(ctx context.Context, run sandbox.PendingRun, said string) error {
	_, err := d.answers().Deliver(ctx, types.ExecutorAnswerGiven{
		TurnID: run.TurnID, Agent: run.AgentID, AgentHandle: run.AgentHandle,
		Answer: said, Choice: sandbox.HeldDeny, AnsweredBy: engineActor, AnsweredByKind: string(iam.ActorSystem),
		LaunchID: run.LaunchID, Approval: run.HeldCall.ID,
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
	if e.answerMergedAlready(ctx, run) {
		return
	}
	if word, said := ownRequest(run); word != "" {
		// A DELEGATE'S OWN REQUEST goes to people now, not at its deadline
		// (ownRequest).
		if e.backends != nil && e.backends.Fleet != nil {
			escalateHeld(ctx, sandbox.NewCoordStore(e.backends.Fleet), sshServers{engine: e},
				delegatedDesk{engine: e}.RawArguments, run, word, said, time.Now())
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

// answerMergedAlready answers run's held merge as the engine where its pull
// request has already merged, so no delegate's turn is spent on it
// ([mergedAlready]). Reports whether it answered.
func (e *Engine) answerMergedAlready(ctx context.Context, run sandbox.PendingRun) bool {
	if e.backends == nil || e.backends.Fleet == nil || e.backends.Queue == nil {
		return false
	}
	desk := delegatedDesk{engine: e}
	return mergedAlready(ctx, run, desk.RawArguments, builtin.ReleaseDeps{Pulls: releasePulls{engine: e}}.MergedAlready,
		sshServers{engine: e}, desk.answerAsEngine)
}

// mergedAlready answers run's held merge, still waiting on its delegates, as
// the engine where merged says its pull request merged before anybody decided
// it: recorded in the access log as the engine's denial
// ([delegated.OverruledAlreadyMerged]), then answered not carried out, with
// the reason. A second request of a merge, held while the first was carried
// out, is the case: its delegate was woken for a merge already made and denied
// it as a duplicate. Anything it cannot read or record leaves the request to
// its delegates. Reports whether it answered.
func mergedAlready(ctx context.Context, run sandbox.PendingRun,
	open func(sandbox.PendingRun) (map[string]any, error),
	merged func(ctx context.Context, requester string, args map[string]any) (string, error),
	audit delegatedRecorder, answer func(ctx context.Context, run sandbox.PendingRun, said string) error) bool {

	held := run.HeldCall
	if held == nil || !held.Delegated() || delegated.Kind(held.Act) != delegated.KindPRMerge {
		return false
	}
	args, err := open(run)
	if err != nil {
		return false
	}
	said, err := merged(ctx, run.AgentHandle, args)
	switch {
	case err != nil:
		log.WarnContext(ctx, "delegated_merge_unread", "request", held.ID, "error", err.Error(),
			"detail", "whether its pull request merged could not be read; its delegates decide it")
		return false
	case said == "":
		return false
	}
	host, subject, stdin := builtin.RequestSubject(delegated.KindPRMerge, args)
	item := ""
	if run.WorkItem != nil {
		item = cmp.Or(run.WorkItem.Key, run.WorkItem.ID)
	}
	// RECORDED BEFORE IT ACTS, as every verdict is.
	if _, err := audit.RecordVerdict(ctx, builtin.DelegatedVerdict{
		At: held.HeldAt, Request: held.ID, Kind: held.Act, Rule: held.Delegation.Rule, Handle: run.AgentHandle,
		Turn: run.TurnID, ByKind: builtin.VerdictByEngine, Through: builtin.VerdictThroughDelegation,
		Verdict: string(delegated.Deny), Final: true, Overruled: []string{delegated.OverruledAlreadyMerged},
		Digest: held.Delegation.Digest, Host: host, Subject: subject, Stdin: stdin, Item: item,
	}); err != nil {
		log.WarnContext(ctx, "delegated_merge_unrecorded", "request", held.ID, "error", err.Error(),
			"detail", "the engine's answer could not be recorded in the access log; its delegates decide it")
		return false
	}
	if err := answer(ctx, run, said); err != nil {
		log.WarnContext(ctx, "delegated_merge_unanswered", "request", held.ID, "error", err.Error(),
			"detail", "the engine's answer did not reach the run; its delegates decide it")
		return false
	}
	log.InfoContext(ctx, "delegated_merge_already_merged", "request", held.ID, "seat", run.AgentHandle)
	return true
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
	// THE MERGE CAN LAND WHILE THE WAKE WAITS: a request held while the same
	// merge was being carried out reaches its delegate after it, with nothing
	// left to decide.
	if e.answerMergedAlready(ctx, run) {
		return nil, false
	}
	args, err := desk.RawArguments(run)
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
// past the company's deadline ([delegated.Limits]), or whose kind no live rule
// delegates to those delegates any more.
type delegatedSweep struct {
	engine *Engine
	claim  schedule.DutyFunc
	now    func() time.Time
	stop   context.CancelFunc
	done   chan struct{}
	// seen is this node's own clock on the calls it sweeps, touched by
	// the sweep's one goroutine alone.
	seen *heldSightings
}

// heldSightings is the sweep's own clock on the calls it sweeps: node, the
// node holding the duty, and when it first saw each call waiting on its
// delegates, by ID, on its clock, which times a call another node held
// ([sandbox.HeldCall.WaitedFor]). Forgotten at a restart or a move of the
// duty, which times it from the call's own instant and the clocks' allowance.
type heldSightings struct {
	node  string
	first map[string]time.Time
}

// sighted is when s first saw held waiting, now for a call new to it, kept for
// the pass after in next. A nil s is a node that never saw it.
func (s *heldSightings) sighted(held sandbox.HeldCall, now time.Time, next map[string]time.Time) (string,
	time.Time) {
	if s == nil {
		return "", time.Time{}
	}
	at, ok := s.first[held.ID]
	if !ok {
		at = now
	}
	next[held.ID] = at
	return s.node, at
}

// startDelegatedSweep arms the sweep on a node that runs workers.
func (e *Engine) startDelegatedSweep(ctx context.Context) {
	if !e.profile.RunsWorkers() || e.backends == nil || e.backends.Fleet == nil {
		return
	}
	s := &delegatedSweep{engine: e, claim: e.workerDuty(delegatedSweepName, delegatedSweepTTL),
		now: time.Now, done: make(chan struct{}), seen: &heldSightings{node: e.id}}
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

// pass escalates every request due and answers every growth act that lapsed,
// and logs what it could not.
func (s *delegatedSweep) pass(ctx context.Context) {
	rules := delegatedRules{engine: s.engine}
	store := sandbox.NewCoordStore(s.engine.backends.Fleet)
	sweepDelegated(ctx, store, rules, rules.Limits().Deadline,
		sshServers{engine: s.engine}, delegatedDesk{engine: s.engine}.RawArguments, s.seen, s.now())
	sweepLapsed(ctx, store, delegatedDesk{engine: s.engine}.answerAsEngine, s.now())
}

// sweepLapsed answers Deny, as the engine, every growth act left unanswered
// past its lapse ([sandbox.HeldCall.LapsesAt], ADR-0171): nothing runs, its
// tool records the lapse, and the agent is told why.
func sweepLapsed(ctx context.Context, store sandbox.PendingStore,
	answer func(context.Context, sandbox.PendingRun, string) error, now time.Time) {
	runs, err := store.ListActive(ctx)
	if err != nil {
		log.WarnContext(ctx, "lapse_sweep_unread", "error", err.Error())
		return
	}
	for _, run := range runs {
		held := run.HeldCall
		if held == nil || !held.Lapsed(now) || !run.HeldOpen() ||
			!slices.Contains(sandbox.Awaiting, run.Status) {
			continue
		}
		said := "Nobody answered within " + words.Count(int(sandbox.HeldLapse/time.Hour), "hour", "hours") +
			", so the request lapsed and nothing was sent. Ask again if it is still needed, with figures read now."
		if err := answer(ctx, run, said); err != nil {
			log.WarnContext(ctx, "lapse_unanswered", "run", run.TurnID, "held_call", held.ID, "error", err.Error())
			continue
		}
		log.InfoContext(ctx, "held_call_lapsed", "run", run.TurnID, "held_call", held.ID, "tool", held.Tool)
	}
}

// delegatedRecorder records the engine's own escalation of a request.
type delegatedRecorder interface {
	RecordVerdict(ctx context.Context, v builtin.DelegatedVerdict) (bool, error)
}

// sweepDelegated puts to people every active held call that waited on its
// delegates past deadline, timed on seen's node ([heldSightings]), or whose
// delegation the live rules no longer hold, each through escalateHeld.
func sweepDelegated(ctx context.Context, store sandbox.PendingStore, rules builtin.DelegatedRules,
	deadline time.Duration, audit delegatedRecorder, open func(sandbox.PendingRun) (map[string]any, error),
	seen *heldSightings, now time.Time) {

	runs, err := store.ListActive(ctx)
	if err != nil {
		log.WarnContext(ctx, "delegated_sweep_unread", "error", err.Error())
		return
	}
	next := map[string]time.Time{}
	defer func() {
		if seen != nil {
			seen.first = next
		}
	}()
	for _, run := range runs {
		held := run.HeldCall
		if held == nil || !held.Delegated() || !slices.Contains(sandbox.Awaiting, run.Status) ||
			run.Ending != nil || run.LaunchID != held.LaunchID {
			continue
		}
		node, since := seen.sighted(*held, now, next)
		word, said := ownRequest(run)
		if word == "" {
			delegates, _, _, live := rules.Rule(delegated.Kind(held.Act))
			word, said = dueEscalation(*held, delegates, live, deadline, node, since, now)
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
// longer the rule's, or the company's deadline passed since it was held, by
// node's clock, which first saw it waiting at seen ([sandbox.HeldCall.WaitedFor]).
func dueEscalation(held sandbox.HeldCall, delegates []string, live bool, deadline time.Duration,
	node string, seen, now time.Time) (string, string) {
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
	if held.WaitedFor(node, now, seen, deadline) {
		return delegated.OverruledDeadline, "No delegate answered within " +
			delegated.Limits{Deadline: deadline}.DeadlineWords() + ", so the request came to people."
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
