package engine

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A busy seat's router (ADR-0167): while one of a seat's executor runs holds
// it, the node holding the seat reads the seat's held inbox without consuming
// it ([queue.EventQueue.Backlog]) and hands the run, as a wake item, every
// wake from the run's own requester about the run's work or about a record the
// run created or wrote ([routeOf], [wroteAbout]). A wake
// the run reads is claimed and its delivery dropped after the run; one it never
// reads runs as a turn of its own. Another wake about the run's work, or from
// a lead of the seat, is triaged and gated (waketriage.go, ADR-0168). The
// package doc's "A busy seat's wake router".

// routeBackstop is how long a router waits for a nudge before it reads the
// held inbox again. The nudge rides a best-effort stream subscription, so a
// missed one only delays a join by this much; each read costs one throwaway
// consumer's proposal on JetStream, so the figure is that cost per busy seat:
// two minutes keeps a seat's runs of an hour to thirty reads.
const routeBackstop = 2 * time.Minute

// routeReadLimit is the most mail one read of a held inbox takes. A job holds
// at most [sandbox.MaxJobWakes] wake items, and a follow-up deeper than 64
// messages behind a run is on a seat so far behind that it waits its turn;
// 64 keeps a read to one JetStream fetch page and its decode to a few MB.
const routeReadLimit = 64

// routeOutcome is what a router decided for one wake and job, the
// [metrics.SeatWakesRouted] counter's `outcome`.
type routeOutcome string

const (
	// routeJoined: the wake was filed on the run's record for its job.
	routeJoined routeOutcome = "joined"
	// routeRefused: the run's record took no more ([sandbox.ErrWakeClosed],
	// [sandbox.ErrWakesSpent], [sandbox.ErrWakeTooLarge]).
	routeRefused routeOutcome = "refused"
	// routeLeftQueued: the rule matched nothing ([routeOf]), or the gate
	// left it for a turn of its own ([gateOf]).
	routeLeftQueued routeOutcome = "left_queued"
	// routeAbsorbed: the triage found it noise owing nothing, recorded worked
	// by the running turn ([Engine.absorbWake]).
	routeAbsorbed routeOutcome = "absorbed"
	// routeStarted: below the cap, related to no running run, it starts a turn
	// of its own beside them ([Engine.beside]).
	routeStarted routeOutcome = "started"
	// routeWaited: below the cap, it was set aside to wait for the run it
	// relates to, whatever else was counted of it ([Engine.setAside]).
	routeWaited routeOutcome = "waited"
)

// routeOutcomes is the closed set.
var routeOutcomes = []routeOutcome{routeJoined, routeRefused, routeLeftQueued, routeAbsorbed, routeStarted,
	routeWaited}

// Valid reports whether o is one of [routeOutcomes].
func (o routeOutcome) Valid() bool { return slices.Contains(routeOutcomes, o) }

// routeRule is the part of the rule a wake matched, the counter's `rule`.
type routeRule string

const (
	// ruleConversation: written in the conversation the run works in.
	ruleConversation routeRule = "conversation"
	// ruleWorkItem: about the work item the run was charged to.
	ruleWorkItem routeRule = "work_item"
	// ruleWrote: about a record the run's calls created or wrote ([wroteAbout]).
	ruleWrote routeRule = "wrote"
	// ruleNone: it matched neither, or is not a wake a run takes.
	ruleNone routeRule = "none"
	// ruleRun: set aside behind the run it relates to by the rule or the
	// triage's weighing, its own conversation free ([Engine.beside]).
	ruleRun routeRule = "run"
	// The triage's verdict on a wake the fixed rule left ([triageVerdict.rule]),
	// or none where it decided nothing.
	ruleTriageInput   routeRule = "triage_input"
	ruleTriageSteer   routeRule = "triage_steer"
	ruleTriageRequest routeRule = "triage_request"
	ruleTriageNoise   routeRule = "triage_noise"
	ruleTriageFailed  routeRule = "triage_failed"
	// A request from another writer put to the people who may let it join
	// (foldin.go): asked, approved, refused, or closed unanswered with its job.
	ruleFoldInAsked    routeRule = "foldin_asked"
	ruleFoldInApproved routeRule = "foldin_approved"
	ruleFoldInRefused  routeRule = "foldin_refused"
	ruleFoldInExpired  routeRule = "foldin_expired"
)

// routeRules is the closed set.
var routeRules = []routeRule{ruleConversation, ruleWorkItem, ruleWrote, ruleNone, ruleRun, ruleTriageInput,
	ruleTriageSteer, ruleTriageRequest, ruleTriageNoise, ruleTriageFailed, ruleFoldInAsked, ruleFoldInApproved,
	ruleFoldInRefused, ruleFoldInExpired}

// Valid reports whether r is one of [routeRules].
func (r routeRule) Valid() bool { return slices.Contains(routeRules, r) }

// Why a wake stays on the inbox ([routeOf]), as seat_wake_left_queued says it.
const (
	stayKind        = "kind"          // not a ledgered type, or one answered on its own channel
	stayScheduled   = "scheduled"     // a schedule's fire, its own instance
	stayWorked      = "worked"        // the completion ledger records it worked
	stayNoRequester = "no_requester"  // the run works for nobody it could be from
	stayWriter      = "other_writer"  // from somebody other than the run's requester
	stayOtherWork   = "other_work"    // not the run's conversation, its item or a record it wrote
	stayNoWords     = "no_words"      // carries none of its writer's words to hand over
	stayRefused     = "record_closed" // the run's record took no more
)

// wakeOrigin is who one wake is from and what it is about, composed with the
// functions dispatch composes a turn's requester with ([requesterOf],
// [credentialRequesterOf], [workingForIn]) over the one wake.
type wakeOrigin struct {
	requester, login string
	workingFor       *types.WorkingFor
	conversation     string
	item             *types.WorkItem
	// room is the chat room it was written in, as its conversation's
	// identity begins ([roomOf]), "" for a wake that is no chat message.
	room string
}

// originOf is ev's [wakeOrigin] as seat handle's turn would read it.
func (e *Engine) originOf(handle string, ev *events.Event) wakeOrigin {
	evs := []*events.Event{ev}
	interactions := e.interactionsOf(evs, handle)
	login, _ := credentialRequesterOf(evs, evs, interactions, handle)
	item, _ := workItemOfEvent(ev)
	return wakeOrigin{
		requester:    requesterIdentity(e.Company(), requesterOf(evs, interactions, handle)),
		login:        login,
		workingFor:   workingForIn(evs),
		conversation: notify.ConversationIdentityOf(ev),
		item:         item,
		room:         roomOf(ev),
	}
}

// roomOf is the chat room ev was written in, namespaced by its transport as
// every conversation identity in it is ([notify.Namespaced]): a direct
// conversation's whole identity, a thread's prefix. "" for no chat message.
func roomOf(ev *events.Event) string {
	n, ok := events.DataAs[*types.ExternalNotification](ev)
	if !ok || n == nil {
		return ""
	}
	return notify.Namespaced(n.Metadata[notify.TransportField], n.Metadata[notify.ChannelField])
}

// routeOf is whether wake ev, from o, joins run, and by which rule, else why it
// stays. ONLY THE RUN'S OWN REQUESTER'S WAKES JOIN: a run is held to one
// requester (ADR-0109), so another writer's words never reach it here.
// worked says the completion ledger records one of ev's keys worked, wrote
// that ev is about a record the run created or wrote ([wroteAbout]).
func routeOf(run sandbox.PendingRun, ev *events.Event, o wakeOrigin, worked, wrote bool) (routeRule, string) {
	switch {
	// A TYPE THE LEDGER DOES NOT KEY would be claimed and run again. A
	// delegated review and a colleague's ask or answer are answered on a
	// channel of their own, which a run's turn answers only for its trigger.
	case !inbox.Ledgered(ev.Type) || slices.Contains(ownChannel, ev.Type):
		return ruleNone, stayKind
	// A FIRE IS AN INSTANCE OF ITS SCHEDULE with its own cap, never a
	// follow-up of the fire running (the answer match refuses it too).
	case notify.Scheduled(o.conversation):
		return ruleNone, stayScheduled
	case worked:
		return ruleNone, stayWorked
	case run.Requester == "" && run.RequesterLogin == "":
		return ruleNone, stayNoRequester
	case o.requester != run.Requester || o.login != run.RequesterLogin ||
		!sameWorkingFor(o.workingFor, run.WorkingFor):
		return ruleNone, stayWriter
	case run.ConversationKey != "" && o.conversation == run.ConversationKey:
		return ruleConversation, ""
	case sameItem(run.WorkItem, o.item):
		return ruleWorkItem, ""
	case wrote:
		return ruleWrote, ""
	}
	return ruleNone, stayOtherWork
}

// wroteAbout reports whether o is about a record in wrote, what a run's calls
// created or wrote ([sandbox.BridgeLog.Wrote]): its work item by ref, or its
// conversation, the identity a write records ([types.WrittenRecord]). AN EXACT
// COMPARE, never a likeness the wake's own words could sway (ADR-0167).
func wroteAbout(wrote []types.WrittenRecord, o wakeOrigin) bool {
	var item, conversation types.WrittenRecord
	if o.item != nil {
		item = types.ItemWritten(*o.item)
	}
	if notify.Derived(o.conversation) {
		conversation = types.ConversationWritten(o.conversation)
	}
	return (item.Valid() && slices.Contains(wrote, item)) ||
		(conversation.Valid() && slices.Contains(wrote, conversation))
}

// ownChannel is the wake types answered on a channel of their own ([routeOf]).
var ownChannel = []string{types.DelegatedReviewType, types.A2ARequestType, types.A2AMessageType}

// sameWorkingFor reports whether a and b name the same requester to work for,
// nil only equal to nil.
func sameWorkingFor(a, b *types.WorkingFor) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Kind == b.Kind && a.Seat == b.Seat && a.Login == b.Login && slices.Equal(a.Apps, b.Apps)
}

// wakeFor is ev as the run's record holds it ([steer.Wake]): its id and keys
// ([deliveryKeys]), its writer as o names them, where it was written (its
// thread's stamped key and its record's position, which the claim reads its
// conversation through), the obligation a turn of its own would carry
// ([ReplyFor]), and its words and label as the turn's own ask would read them
// ([turnAskFor]). False for a wake carrying none of its writer's words.
func (e *Engine) wakeFor(handle string, ev *events.Event, o wakeOrigin) (steer.Wake, bool) {
	ask := turnAskFor(e.notifyPrompts(), e.liveParties(), handle, []*events.Event{ev})
	label := ask.Label()
	if label == nil {
		return steer.Wake{}, false
	}
	by, kind := o.requester, iam.ActorAgent
	switch {
	case o.login != "":
		by, kind = o.login, iam.ActorOperator
	case e.seatIsHuman(o.requester):
		kind = iam.ActorHuman
	}
	owes := ""
	if reply := ReplyFor([]*events.Event{ev}); reply.Awaited() {
		owes = reply.String()
	}
	message := ""
	if r, ok := receiptOf(ev); ok {
		message = r.String()
	}
	return steer.Wake{
		ID: ev.ID.String(), Triggers: []string{ev.ID.String()}, Keys: deliveryKeys(ev),
		By: by, ByKind: kind, WorkingFor: notify.WorkingForKey(o.workingFor),
		Conversation: o.conversation, Partition: notify.KeyOfAll([]*events.Event{ev}), Label: *label,
		OwesAnswer: obliges(ev), At: ev.Timestamp, Owes: owes, Seen: notify.TriggerOf(ev), Text: ask.Text(),
		Message: message,
	}, true
}

// seatIsHuman reports whether the live chart holds handle as a person's seat.
func (e *Engine) seatIsHuman(handle string) bool {
	company := e.Company()
	if handle == "" || company == nil || company.Org == nil {
		return false
	}
	role := company.Org.Role(handle)
	return role != nil && role.IsHuman()
}

// seatRouters is this node's routers, one per busy seat it holds: running is
// each seat's live one, and last the end of each seat's newest, which a router
// started after it waits for, so no two read one seat at once.
type seatRouters struct {
	mu      sync.Mutex
	running map[string]*seatRouter
	last    map[string]<-chan struct{}
	routes  map[string]*seatRoute
	stopped bool
	// marks is the receipts being written off a read's path ([Engine.tookOver]),
	// waited out as the routers stop.
	marks sync.WaitGroup
}

// seatRoute is what a seat's router and its screen of a flowing inbox
// ([Engine.beside]) decide its wakes under: one at a time, each decision once
// per job, so a wake both read is never triaged twice.
type seatRoute struct {
	mu   sync.Mutex
	memo *routeMemo
}

// seatRoute is handle's, made on first use and let go once no run holds it.
func (e *Engine) seatRoute(handle string) *seatRoute {
	routers := &e.wakeRouters
	routers.mu.Lock()
	defer routers.mu.Unlock()
	if routers.routes == nil {
		routers.routes = map[string]*seatRoute{}
	}
	r := routers.routes[handle]
	if r == nil {
		r = &seatRoute{memo: newRouteMemo()}
		routers.routes[handle] = r
	}
	return r
}

// seatRouter is one seat's router: its stop, its end, and its nudge, which
// coalesces into one read.
type seatRouter struct {
	cancel context.CancelFunc
	done   chan struct{}
	nudge  chan struct{}
}

// poke asks the router for a read, never blocking.
func (r *seatRouter) poke() {
	select {
	case r.nudge <- struct{}{}:
	default:
	}
}

// seatRunsMoved follows a seat's runs on this node ([sandbox.CoordinatorOptions.Busy]):
// a router while one holds the seat, none otherwise. READ, NEVER PASSED, so
// calls landing out of order end on the seat's state.
func (e *Engine) seatRunsMoved(handle string) {
	busy := false
	if rt := e.sandbox.Load(); rt != nil {
		busy, _ = rt.coordinator.SeatRuns(handle)
	}
	e.followSeat(handle, busy)
}

// followSeat starts handle's router where busy and none runs, and stops the
// one running where not; a router started while the last is still ending waits
// for it, so no two read one seat at once.
func (e *Engine) followSeat(handle string, busy bool) {
	routers := &e.wakeRouters
	routers.mu.Lock()
	defer routers.mu.Unlock()
	r, running := routers.running[handle]
	if !busy {
		delete(routers.routes, handle)
	}
	switch {
	case !busy && running:
		r.cancel()
		delete(routers.running, handle)
	// NO ROUTER WITHOUT A WAY TO READ AND FILE: an engine with no queue or
	// run steers has no inbox to read or record to file on.
	case busy && !running && !routers.stopped && e.backends != nil && e.backends.Queue != nil &&
		e.runSteers != nil:
		ctx, cancel := context.WithCancel(context.Background())
		next := &seatRouter{cancel: cancel, done: make(chan struct{}), nudge: make(chan struct{}, 1)}
		if routers.running == nil {
			routers.running, routers.last = map[string]*seatRouter{}, map[string]<-chan struct{}{}
		}
		prev := routers.last[handle]
		routers.running[handle], routers.last[handle] = next, next.done
		go e.runRouter(ctx, handle, next, prev)
	}
}

// nudgeRouter asks handle's router, where it has one, for a read
// ([Dispatcher.Idle]): mail it skipped while a dispatch ran is read now.
func (e *Engine) nudgeRouter(handle string) {
	e.wakeRouters.mu.Lock()
	r := e.wakeRouters.running[handle]
	e.wakeRouters.mu.Unlock()
	if r != nil {
		r.poke()
	}
}

// stopRouters stops every router and waits for each to end; none starts
// after.
func (e *Engine) stopRouters() {
	routers := &e.wakeRouters
	routers.mu.Lock()
	routers.stopped = true
	for _, r := range routers.running {
		r.cancel()
	}
	routers.running, routers.routes = nil, nil
	ends := slices.Collect(maps.Values(routers.last))
	routers.mu.Unlock()
	for _, end := range ends {
		<-end
	}
	routers.marks.Wait()
}

// routerStreamBudget bounds a router's withdrawal of its nudges' subscriptions,
// made as it ends on a context of its own: a broker that does not answer
// leaves the ephemeral subscription to the broker's own reaping.
const routerStreamBudget = 5 * time.Second

// runRouter is r's goroutine, until ctx ends: once prev (the seat's router
// before it) has ended, it reads the seat's held inbox at once, then on every
// nudge (a publish to the inbox or its lane, a dispatch of the seat ending)
// and at least every [routeBackstop], one read at a time.
func (e *Engine) runRouter(ctx context.Context, handle string, r *seatRouter, prev <-chan struct{}) {
	defer close(r.done)
	defer r.cancel()
	if prev != nil {
		select {
		case <-prev:
		case <-ctx.Done():
			return
		}
	}
	subject, group, err := seatHold{engine: e}.inboxOf(handle)
	if err != nil {
		log.DebugContext(ctx, "seat_router_not_started", "seat", handle, "error", err.Error())
		return
	}
	q := e.backends.Queue
	for _, pattern := range []string{subject, queue.LaneOf(subject)} {
		// BOTH: a backend stores lane mail under the lane's subject or the
		// inbox's ([queue.SubjectFor]), and a nudge must reach either way.
		unsub, err := q.SubscribeStream(ctx, pattern, func(context.Context, string, *events.Event) { r.poke() })
		if err != nil {
			log.WarnContext(ctx, "seat_router_unnudged", "seat", handle, "subject", pattern,
				"error", err.Error(), "detail", "the router reads the seat's inbox at its backstop alone")
			continue
		}
		defer func() {
			stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), routerStreamBudget)
			defer cancel()
			_ = unsub(stop)
		}()
	}
	route := e.seatRoute(handle)
	defer func() {
		route.mu.Lock()
		defer route.mu.Unlock()
		e.routerEnded(ctx, handle, route.memo)
	}()
	backstop := time.NewTimer(routeBackstop)
	defer backstop.Stop()
	for {
		route.mu.Lock()
		e.routeSeat(ctx, handle, subject, group, route.memo)
		route.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-r.nudge:
		case <-backstop.C:
		}
		backstop.Reset(routeBackstop)
	}
}

// routeSeat is one read of handle's held inbox (subject, group): every wake
// [routeOf] gives the run holding the seat is filed for its job, and every
// [candidate] it leaves is triaged and gated, each decided once per job (memo
// keeps which, by job and wake id), a lead's steers filed once all are weighed.
// NOTHING WHILE A DISPATCH OF THE SEAT RUNS here: the turn that launched the
// run may not have recorded its own triggers worked yet ([Dispatcher.Idle]
// nudges after). NOR WITHOUT EACH RUN'S WRITTEN SET ([Engine.writtenOf]): a
// wake is decided once per job, so one decided without it is never weighed
// with it.
func (e *Engine) routeSeat(ctx context.Context, handle, subject, group string, memo *routeMemo) {
	if e.dispatch != nil && e.dispatch.Running(handle) {
		return
	}
	runs, err := e.runSteers.store.ListActiveForSeat(ctx, handle)
	if err != nil {
		log.DebugContext(ctx, "seat_router_runs_unread", "seat", handle, "error", err.Error())
		return
	}
	runs = routableRuns(runs)
	e.expireFoldIns(ctx, handle, runs, memo)
	memo.prune(runs)
	if len(runs) == 0 {
		return
	}
	mail, err := e.backends.Queue.Backlog(ctx, subject, group, routeReadLimit)
	if err != nil {
		log.DebugContext(ctx, "seat_router_inbox_unread", "seat", handle, "error", err.Error())
		return
	}
	mail = slices.DeleteFunc(mail, func(ev *events.Event) bool {
		return ev == nil || !slices.ContainsFunc(runs, func(run sandbox.PendingRun) bool {
			return !memo.done(run.LaunchID, ev.ID.String())
		})
	})
	if len(mail) == 0 {
		return
	}
	wrote, ok := e.writtenOf(ctx, handle, runs)
	if !ok {
		return
	}
	worked := e.workedOf(ctx, handle, mail)
	read := &routeRead{wrote: wrote}
	for _, ev := range mail {
		e.routeWake(ctx, handle, runs, ev, worked, memo, read)
	}
	e.fileSteers(ctx, handle, read, memo)
}

// writtenOf is what each of runs created or wrote, by turn, off its turn's log
// ([sandbox.PendingStore.TurnLog], [sandbox.BridgeLog.Wrote]); false where a
// log could not be read, which leaves the read undecided for the next.
func (e *Engine) writtenOf(ctx context.Context, handle string, runs []sandbox.PendingRun) (
	map[string][]types.WrittenRecord, bool) {

	wrote := make(map[string][]types.WrittenRecord, len(runs))
	for _, run := range runs {
		turnLog, err := e.runSteers.store.TurnLog(ctx, run)
		if err != nil {
			log.DebugContext(ctx, "seat_router_log_unread", "seat", handle, "turn_id", run.TurnID,
				"error", err.Error())
			return nil, false
		}
		wrote[run.TurnID] = turnLog.Wrote
	}
	return wrote, true
}

// routableRuns is the runs of runs a wake can be routed to: launched, in a status
// that takes items. A copy: runs stays whole for a caller that also reads the
// runs no wake routes to, a run parked on another ([Engine.beside]).
func routableRuns(runs []sandbox.PendingRun) []sandbox.PendingRun {
	return slices.DeleteFunc(slices.Clone(runs), func(run sandbox.PendingRun) bool {
		return run.LaunchID == "" || !slices.Contains(sandbox.ItemStatuses, run.Status)
	})
}

// workedOf is which of mail's keys the completion ledger records worked, in
// one read; nil where there is no ledger or it could not be read (it fails
// open, as the dispatcher's read does).
func (e *Engine) workedOf(ctx context.Context, handle string, mail []*events.Event) map[string]bool {
	if e.runSteers.completions == nil {
		return nil
	}
	var keys []string
	for _, ev := range mail {
		keys = append(keys, deliveryKeys(ev)...)
	}
	if len(keys) == 0 {
		return nil
	}
	return e.runSteers.completions.Worked(ctx, handle, keys)
}

// routeWake decides ev for the first of runs whose rule it matches, filing it
// there, or triages it for the first it is a [candidate] of
// ([Engine.triageRun]), and records the decision for every run it was weighed
// for in memo; read is the read's own state.
func (e *Engine) routeWake(ctx context.Context, handle string, runs []sandbox.PendingRun, ev *events.Event,
	worked map[string]bool, memo *routeMemo, read *routeRead) {

	id := ev.ID.String()
	mark := func(job string) { memo.mark(job, id) }
	o := e.originOf(handle, ev)
	isWorked := slices.ContainsFunc(deliveryKeys(ev), func(key string) bool { return worked[key] })
	why := ""
	for _, run := range runs {
		if memo.done(run.LaunchID, id) {
			continue
		}
		wrote := wroteAbout(read.wrote[run.TurnID], o)
		rule, stay := routeOf(run, ev, o, isWorked, wrote)
		if rule == ruleNone {
			if rel := e.relationsOf(ctx, handle, run, o, wrote); candidate(stay, rel) {
				e.triageRun(ctx, handle, run, ev, o, rel, memo, read)
				return
			}
			mark(run.LaunchID)
			why = stay
			continue
		}
		wake, ok := e.wakeFor(handle, ev, o)
		if !ok {
			mark(run.LaunchID)
			why = stayNoWords
			continue
		}
		err := e.runSteers.fileWake(ctx, run.TurnID, run.LaunchID, wake)
		switch {
		case err == nil:
			mark(run.LaunchID)
			e.noteRouted(ctx, handle, run, id, routeJoined, rule, "", nil)
			return
		case errors.Is(err, sandbox.ErrWakeClosed), errors.Is(err, sandbox.ErrWakesSpent),
			errors.Is(err, sandbox.ErrWakeTooLarge):
			// NEVER AGAIN FOR THIS JOB: the record said no, and its delivery
			// runs as a turn of its own after the run.
			mark(run.LaunchID)
			e.noteRouted(ctx, handle, run, id, routeRefused, rule, stayRefused, err)
			return
		default:
			// THE STORE DID NOT ANSWER: undecided, so the next read asks again.
			log.WarnContext(ctx, "seat_wake_not_filed", "seat", handle, "turn_id", run.TurnID,
				"wake_id", id, "error", err.Error(),
				"detail", "the run's record could not be reached; the next read files the wake again")
			return
		}
	}
	if why != "" {
		e.noteRouted(ctx, handle, sandbox.PendingRun{}, id, routeLeftQueued, ruleNone, why, nil)
		// A MESSAGE THAT RUNS AFTER the running work is told so, once; one
		// worked already runs nothing.
		if why != stayWorked {
			e.tellWaiting(ctx, handle, ev, id, o.login == "" && e.seatIsHuman(o.requester), runsAfterLine)
		}
	}
}

// noteRouted counts and says one decision of a router
// ([metrics.SeatWakesRouted]), as steer_note_filed says a note's filing.
func (e *Engine) noteRouted(ctx context.Context, handle string, run sandbox.PendingRun, wakeID string,
	outcome routeOutcome, rule routeRule, why string, err error) {

	e.metrics.Add(metrics.SeatWakesRouted, 1, metrics.Attrs{"outcome": string(outcome), "rule": string(rule)})
	switch outcome {
	case routeJoined:
		log.InfoContext(ctx, "seat_wake_joined", "seat", handle, "turn_id", run.TurnID, "job", run.LaunchID,
			"wake_id", wakeID, "rule", string(rule))
	case routeAbsorbed:
		log.InfoContext(ctx, "seat_wake_absorbed", "seat", handle, "turn_id", run.TurnID, "job", run.LaunchID,
			"wake_id", wakeID, "detail", "it asks nothing and adds nothing the running work needs; its delivery "+
				"is skipped as absorbed by the running turn")
	case routeRefused:
		log.InfoContext(ctx, "seat_wake_left_queued", "seat", handle, "turn_id", run.TurnID,
			"job", run.LaunchID, "wake_id", wakeID, "rule", string(rule), "reason", why, "error", err.Error(),
			"detail", "the run's record took no more for this job; the wake runs as a turn of its own")
	default:
		log.DebugContext(ctx, "seat_wake_left_queued", "seat", handle, "wake_id", wakeID, "reason", why)
	}
}
