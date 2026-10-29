// The event-type taxonomy: which category each type is filed under, and which
// types are deliberately kept out of the durable log.
//
// IT LIVES HERE, in the package that owns the type registry, because two
// packages consume it and neither may import the other: internal/store writes
// the row and internal/observe decides what reaches the activity feed, and
// internal/observe already imports internal/store. It was two identical maps
// in those two packages, with nothing asserting they agreed — so a type placed
// in one and forgotten in the other would be written to the store and never
// shown, or shown and never written, with no test anywhere able to see it.

package events

import (
	"maps"
	"slices"
)

// RateAuthor names WHO DECIDES HOW OFTEN a type is published — never who
// publishes it, which is always this engine, and never who the event is about.
//
// It is a property of the TYPE because it is a property of the type's
// publisher: the loop, the delivery or the write that each occurrence follows
// from. The whole of what the engine can do about volume is decide what to
// record, and that decision needs to know whose hand is on the tap.
type RateAuthor string

const (
	// RateEngine is a rate this company's own work sets: a turn, a tick, a
	// duty, a schedule. Bounded by what the engine is doing, which is
	// bounded by the seats, budgets and caps an operator configured.
	RateEngine RateAuthor = "engine"

	// RateAuthenticated is a rate an ADMITTED caller sets — an operator
	// holding a token, a seat, a webhook delivery whose signature or shared
	// token verified. Unbounded in principle and attributable in practice:
	// there is a credential to revoke and an identity on the row, which is
	// the whole difference from RateAnonymous.
	RateAuthenticated RateAuthor = "authenticated"

	// RateAnonymous is a rate ANYBODY WHO CAN REACH THE LISTENER sets, with
	// no credential and no identity — a failed login, a refused signature,
	// a request to a route that does not exist.
	//
	// NO TYPE WITH THIS AUTHOR MAY BE CATEGORISED, which is the admission
	// rule [AdmissionViolations] enforces: the node estate is a local file
	// on a disk an operator sized, and a row per attempt hands the size of
	// it to whoever is making the attempts. What such a fact belongs on is
	// a metrics counter, with a COALESCED event — per source, per minute,
	// published by the engine's own loop and therefore RateEngine — as the
	// durable record.
	RateAnonymous RateAuthor = "anonymous"
)

// Valid reports whether a is one of the three authors.
//
// The zero value is not one, deliberately: an entry that states no author is
// not a claim that its rate is bounded, and [AdmissionViolations] reports it
// exactly as it reports an anonymous one.
func (a RateAuthor) Valid() bool {
	switch a {
	case RateEngine, RateAuthenticated, RateAnonymous:
		return true
	default:
		return false
	}
}

// placement is what the taxonomy knows about one CATEGORISED type: the
// category its rows are filed under, and who authors its rate.
//
// Two fields rather than a bare category string, because admission asks two
// questions and only one of them was ever written down. The second is checked
// rather than remembered — see [AdmissionViolations].
type placement struct {
	category string
	rate     RateAuthor
}

// ExclusionCause is WHY a type is kept out of the node estate, as a value.
//
// It sits beside the sentence rather than replacing it, because the two have
// different readers: a person needs the sentence, and a WALK cannot infer
// intent from one. "This is already a row" and "an unauthenticated caller sets
// this rate" are the same English shrug to a regexp and two different
// decisions to the engine — and only one of them is a rule a test can hold a
// later change to.
type ExclusionCause string

const (
	// CauseAlreadyRecorded is a type whose FACT is already a row, written
	// by something else under its own id. What this type carries is the
	// wake, or the nudge, that the row produced.
	CauseAlreadyRecorded ExclusionCause = "already_recorded"

	// CauseIntermediate is a state of a row the log also holds FINISHED.
	// Persisting it fills the log with the middles of things it has the
	// ends of.
	CauseIntermediate ExclusionCause = "intermediate_state"

	// CausePlacement is a fact about SCHEDULING rather than about the
	// company: which node is running a seat this minute. The live seat
	// state is what answers it.
	CausePlacement ExclusionCause = "placement"

	// CauseSampled is a periodic SNAPSHOT of a value something else
	// answers exactly — a tick's reading of a counter, not a change to it.
	CauseSampled ExclusionCause = "periodic_snapshot"

	// CauseAnonymousRate is a type whose rate an unauthenticated caller
	// authors ([RateAnonymous]). The per-occurrence fact belongs on a
	// metrics counter; what reaches the estate is the coalesced event the
	// engine's own loop publishes.
	CauseAnonymousRate ExclusionCause = "anonymous_rate"
)

// Valid reports whether c is a declared cause.
func (c ExclusionCause) Valid() bool {
	switch c {
	case CauseAlreadyRecorded, CauseIntermediate, CausePlacement,
		CauseSampled, CauseAnonymousRate:
		return true
	default:
		return false
	}
}

// ExclusionCauses is every declared cause, sorted. Exported so a docs guard
// reads the same vocabulary the engine files exclusions under.
//
// Sorted HERE rather than written in order, because the declaration order is
// the order the causes make sense in to a reader and the two would drift the
// first time one was added.
func ExclusionCauses() []ExclusionCause {
	return slices.Sorted(slices.Values([]ExclusionCause{
		CauseAlreadyRecorded, CauseIntermediate, CausePlacement,
		CauseSampled, CauseAnonymousRate,
	}))
}

// Exclusion is why a type is kept out of the node estate: the typed cause a
// walk reads, and the sentence a person does.
type Exclusion struct {
	// Cause is the decision, as a value.
	Cause ExclusionCause

	// Reason is the same decision as prose, for whoever is looking for the
	// type and not finding it. It says what holds the fact instead.
	Reason string
}

// categories maps an event type to its dashboard category.
//
// THE MAP IS THE ADMISSION LIST. A type that is not here is not written to the
// event store and does not reach the activity feed — the live projection keys
// "is this a persisted event" on the category being non-empty, so an absent
// entry is a silent drop in both places at once. That is how the sandbox panel
// once ended up showing rows that vanished on the next reload and 404'd when
// clicked.
//
// So the exclusions below are deliberate, and stated, rather than left as gaps.
//
// EVERY ENTRY ALSO STATES WHO AUTHORS ITS RATE, because admission is not only
// "is this worth keeping" but "can somebody outside this company decide how
// much of it there is". See [RateAuthor] and [AdmissionViolations].
//
// So is the one narrowing of the other half: a type in [unfed] is persisted
// like every other entry here and is kept out of the activity feed.
var categories = map[string]placement{
	// Lifecycle: the org coming and going, plus the config changes an
	// operator is most likely to go looking for after the fact — and the
	// runtime audit, which is the same question asked of everything else
	// a person did to the company through a running node: every operator
	// tool call that is not a proven read, and every backup. A seat
	// coming and going is not here: agent_spawned and agent_terminated are
	// live-only, and [excluded] says why.
	"org_started":               {"lifecycle", RateEngine},
	"org_stopped":               {"lifecycle", RateEngine},
	"config_revision_activated": {"lifecycle", RateAuthenticated},
	"config_revision_applied":   {"lifecycle", RateAuthenticated},
	"operator_acted":            {"lifecycle", RateAuthenticated},
	"backup_requested":          {"lifecycle", RateAuthenticated},
	// A person pausing and resuming a seat — the change itself, published
	// once by the writer whose compare-and-set won. Beside the runtime
	// audit rather than folded into it: operator_acted records that a call
	// was made, and these what it changed about the company, so a pause
	// taken and a pause refused as already taken read differently.
	"seat_paused":  {"lifecycle", RateAuthenticated},
	"seat_resumed": {"lifecycle", RateAuthenticated},

	// Task: work reaching a seat — including a detached sandbox run, which
	// is the execution of one, and a schedule firing, which creates one.
	//
	// THERE IS NO TASK LIFECYCLE HERE. The five that were — created,
	// started, completed, failed, delegated — described an engine-owned
	// task object the native tracker replaced, and none of them ever had a
	// publisher. Work's own record is the tracker's log, its history rows
	// and its change feed.
	"task_assigned":                   {"task", RateAuthenticated},
	"sandbox_run_started":             {"task", RateEngine},
	"sandbox_clarification_requested": {"task", RateEngine},
	"sandbox_run_completed":           {"task", RateEngine},
	// A run settled WITHOUT its turn resuming. Categorised beside the
	// completion rather than kept live-only: this is the durable record
	// that a turn was lost, and it is the one an operator goes looking for
	// after the fact — a live-only failure would be swept before anybody
	// asked why the work never came back.
	"sandbox_run_failed": {"task", RateEngine},
	// What an answer to a run's question became, on either route. The chat
	// route left no record at all, so this is the only account of whether a
	// person's reply resumed the run, found it already answered, or found it
	// gone — and of who gave it.
	"sandbox_run_answered": {"task", RateAuthenticated},
	"scheduled_task_fired": {"task", RateEngine},

	"a2a_channel_opened": {"a2a", RateEngine},
	"a2a_message_sent":   {"a2a", RateEngine},
	"a2a_channel_closed": {"a2a", RateEngine},

	// Nothing in Crewlet publishes these four. DACI discussion is
	// behavioural guidance carried on the org's own chat surfaces, and a
	// decision somebody must make is a structured ask on a work item
	// (ADR-0023) — a tracker record whose wakes are the tracker's own
	// `asked` and `answered`, not an event of this category. They stay
	// mapped as the seam an extension writes through, and they are why the
	// dashboard has a `decision` category to filter on at all.
	"decision_requested":     {"decision", RateEngine},
	"decision_resolved":      {"decision", RateEngine},
	"contribution_requested": {"decision", RateEngine},
	"contribution_received":  {"decision", RateEngine},

	// Notification: what arrived from outside, and what the engine decided
	// to do about it. Coalescing and a ledger-skipped redelivery are here
	// because operators watch when and how hard batching kicks in — and
	// because the entire point of emitting a skipped trigger is that it
	// should not be invisible.
	"external_notification":   {"notification", RateAuthenticated},
	"notification_skipped":    {"notification", RateAuthenticated},
	"notifications_coalesced": {"notification", RateAuthenticated},
	"turn_trigger_skipped":    {"notification", RateAuthenticated},

	// Webhook: one inbound delivery presented to one seat, from either edge
	// — a webhook route or a chat socket. Its own category rather than
	// `notification`, because it records what ARRIVED, before anything was
	// decided about it, and the integration counts read exactly this set.
	// The row is filed under the delivery's own label rather than this
	// type (internal/observe), so this is the one type whose rows never
	// carry its name.
	"inbound_delivery": {WebhookCategory, RateAuthenticated},

	// System: the engine talking about itself. A turn's start is stored
	// beside its completion, because the pair is what bounds a turn in
	// history: a turn that began and never finished is a start with no
	// completion, and a reader can only see that if both are rows. A
	// person's note to a running turn is AUTHENTICATED-rated: whoever sent
	// it holds a credential, and how many they send is theirs to decide.
	"budget_exhausted":             {"system", RateEngine},
	"llm_unavailable":              {"system", RateEngine},
	"agent_turn_started":           {"system", RateEngine},
	"agent_turn_completed":         {"system", RateEngine},
	"agent_turn_stopped":           {"system", RateEngine},
	"agent_turn_steered":           {"system", RateAuthenticated},
	"agent_phase_started":          {"system", RateEngine},
	"agent_phase_completed":        {"system", RateEngine},
	"phase.tool_skill_blocked":     {"system", RateEngine},
	"prompt.size":                  {"system", RateEngine},
	"turn.guard_breach":            {"system", RateEngine},
	"provider_fallback":            {"system", RateEngine},
	"skill_telemetry_write_failed": {"system", RateEngine},
	"subagent_batched":             {"system", RateEngine},

	// Auth: who signed in and how, what ended a session, what changed
	// about what a person may do, and whether a record on a fleet log was
	// written by the fleet at all. The live half of a trail whose durable,
	// fleet-wide half is `iam_history` — see internal/events/types' auth.go.
	//
	// EVERY RATE HERE IS SOMEBODY THIS ENGINE ALREADY TRUSTS, and the one
	// that looks like it should not be is the proof: a failed sign-in is
	// authored by whoever can reach the listener, so it is NOT a type — it
	// is a metrics counter, and the row is iam_login_failures, one per
	// client per minute, paced by the engine's own flush loop. The two
	// signature verdicts are engine-rated for the same reason: a key id is
	// whatever a frame says, so the node reports each (domain, key id)
	// once, under a cap on how many distinct ids it will ever name.
	"iam_session_started":    {"auth", RateAuthenticated},
	"iam_session_ended":      {"auth", RateAuthenticated},
	"iam_login_failures":     {"auth", RateEngine},
	"iam_stepup_completed":   {"auth", RateAuthenticated},
	"iam_credential_minted":  {"auth", RateAuthenticated},
	"iam_credential_revoked": {"auth", RateAuthenticated},
	"iam_grants_changed":     {"auth", RateAuthenticated},
	"iam_recovery_code_used": {"auth", RateAuthenticated},
	// AUTHENTICATED because only a caller the password admitted can
	// reach a second factor, and a row is written only by the wrong code
	// that takes a person's curve back up to its ceiling — never while a
	// run is held there — so at most CurveSteps a window per person on
	// each node: the rate is bounded by whose passwords are known to
	// somebody guessing, never by what a stranger can send.
	"iam_second_factor_throttled": {"auth", RateAuthenticated},
	"iam_mfa_reset":               {"auth", RateAuthenticated},
	"iam_invitation_cancelled":    {"auth", RateAuthenticated},
	"iam_password_changed":        {"auth", RateAuthenticated},
	"iam_password_reset_issued":   {"auth", RateAuthenticated},
	// AUTHENTICATED by the link it spends: written only once a secret this
	// engine minted proved itself and set a password, and a link sets one
	// password — a spent one opens nothing again.
	"iam_password_reset":            {"auth", RateAuthenticated},
	"iam_session_generation_bumped": {"auth", RateAuthenticated},
	"statelog_record_unverifiable":  {"auth", RateEngine},
	"statelog_record_tampered":      {"auth", RateEngine},
	// What the auxiliary model cost, coalesced per key per flush — PERSISTED
	// and NOT FED: see [unfed].
	"auxiliary_spend": {"system", RateEngine},
	// A read of a `visibility: unit` repository's contents — a fetch that
	// carried a pack, a code tool's answer — PERSISTED, since the usage
	// domain derives "who read it" from these rows, and NOT FED: see
	// [unfed]. AUTHENTICATED-rated: every one is a read the guard admitted
	// and the repository's grant allowed, so its pace is set by the
	// credentials and seats the company holds — a token's builds, a seat's
	// turns — never by a stranger.
	"code_fetched": {"system", RateAuthenticated},
	// An attempt of a check of a commit ENDED, as its row keeps the ending —
	// PERSISTED, since a node's day counts it from these rows, and NOT FED:
	// see [unfed]. AUTHENTICATED-rated: an attempt follows from a push the
	// guard admitted — a seat's, a person's, a token's — so its pace is set
	// by the credentials and seats the company holds, bounded by the cells
	// that run checks.
	"code_check_finished": {"system", RateAuthenticated},

	// Learning: the reflection subsystem and the skill lifecycle, grouped
	// so a dashboard's category filter can include or exclude all of that
	// traffic with one toggle.
	"turn_completed":               {"learning", RateEngine},
	"episode_written":              {"learning", RateEngine},
	"persist_decider_completed":    {"learning", RateEngine},
	"counterparty_profile_updated": {"learning", RateEngine},
	"skill_synthesized":            {"learning", RateEngine},
	"skill_refined":                {"learning", RateEngine},
	"skill_promoted":               {"learning", RateEngine},
	"reflection_completed":         {"learning", RateEngine},
	"prefetch_summary":             {"learning", RateEngine},
	"skill_used":                   {"learning", RateEngine},
	"skill_staled":                 {"learning", RateEngine},
	"skill_archived":               {"learning", RateEngine},
	"skill_revived":                {"learning", RateEngine},
	"compaction_requested":         {"learning", RateEngine},
	"compaction_completed":         {"learning", RateEngine},
	// A seat reading the knowledge base, stored beside the skill loads it
	// generalises: which pages the company's staff actually open is the
	// question a knowledge base is curated against.
	"knowledge_read": {"learning", RateEngine},
}

// excluded are the types deliberately kept OUT of the event store, each with
// the CAUSE it must stay out for and the reason in full.
//
// A map to [Exclusion] rather than a set, because the whole hazard here is
// that an exclusion and an oversight look identical from the outside: both are
// a type that is published and then vanishes. Writing the reason down is what
// makes the next reader able to tell which one they are looking at — and the
// completeness test prints it. The cause is the same fact in the form a WALK
// can read: [AdmissionViolations] holds the estate to a rule, and a rule
// cannot be stated in a sentence nothing parses.
//
// # Two facts that are kept out by having NO TYPE, and why they are not here
//
// A per-request AUTHORIZATION DECISION and a session TOUCH are both named by
// the identity design as things the audit trail deliberately does not hold —
// the first because it is orders of magnitude more voluminous than everything
// else in the store and is a fact about a poll rather than about the company,
// the second because it is a clock rather than an event. Neither is an entry
// below, because an entry here is for a type something PUBLISHES and keeps out
// of the estate: nothing in this build publishes either, and registering a
// type only to exclude it would be a wire name with no producer, which reads
// to the next person exactly like a type whose publisher was lost.
//
// # And NOTHING HERE REFUSES THEM — they stay out because no type exists
//
// [AdmissionViolations] cannot hold either one out, and it would be false to
// say it does. Both are caused by a caller the guard ADMITTED — a decision is
// taken on an authenticated request, a touch is a valid session being used —
// so a type for either would be filed [RateAuthenticated], and the walk
// refuses only an anonymous or an unstated author. What keeps them out is
// what each fact already is: a refused request is the 403 the authority
// table wrote and a log line, a request a Tier A token overreached on is a
// WARN log line on the node it reached, and a session's use moves nothing at
// all — its rotation is
// derived, so an hour of use writes nothing anywhere. A change that wanted a
// row per decision or per touch would add the type in a reviewed diff, and
// the reasons above — volume and a poll rather than a fact for the decision,
// a clock rather than an event for the touch — are what that review has to
// answer; no test here would stop it.
var excluded = map[string]Exclusion{
	"agent_turn_progress": {Cause: CauseIntermediate, Reason: "fires as each LLM round opens, answers and runs its tools, as a live-only signal; the " +
		"matching agent_phase_completed is its durable record, so persisting " +
		"this would fill the log with intermediate states of rows it also " +
		"holds finished"},
	"agent_spawned": {Cause: CausePlacement, Reason: "placement moves a seat between nodes on every " +
		"rebalance, so a durable row per claim would fill the audit log with " +
		"a fact about SCHEDULING rather than about the company. The live seat " +
		"state is what asks 'is this seat running, and where'"},
	"agent_terminated": {Cause: CausePlacement, Reason: "the counterpart of agent_spawned, and excluded for " +
		"the same reason; it is what returns a released seat to `terminated` " +
		"on a live screen rather than leaving it showing whatever it last did"},
	"a2a_request": {Cause: CauseAlreadyRecorded, Reason: "the ASK is already a row: the A2A service publishes " +
		"a2a_channel_opened and a2a_message_sent for the same exchange, under " +
		"the ids the audit trail is keyed on. This event is the WAKE it puts " +
		"on the target seat's inbox, so categorising it would write a second " +
		"row per ask saying the same thing — the same reason raw_webhook is " +
		"kept out below"},
	"a2a_message": {Cause: CauseAlreadyRecorded, Reason: "the ANSWER is already a row (a2a_message_sent). This " +
		"event is the wake it puts on the requester's inbox; see a2a_request"},
	"budget_meters": {Cause: CauseSampled, Reason: "a SNAPSHOT of the shared token counters, published by " +
		"every node on a 15-second tick and at once when a budget window first " +
		"refuses a charge, so a durable row per report is about " +
		"two million a year per node to answer a question the live projection " +
		"and the budgets query answer for free. What the audit log holds instead is " +
		"the spend the counter is charged with, recorded per phase in the " +
		"agent_phase_completed rows internal/tokens aggregates, so \"what did " +
		"we spend last month\" is answerable and \"what was the counter " +
		"reading at 14:03:15\" is not a question anybody asks"},
	"tool_skill_page_changed": {Cause: CauseAlreadyRecorded, Reason: "a NUDGE between nodes that one tool-skill " +
		"page moved, and the delivery that caused it is already a row, " +
		"written by the webhook receiver. What a skill change did to a " +
		"registry is a log line on each node, so a durable row per node " +
		"would record the same edit once for the wiki and again for every " +
		"member of the fleet"},
	"operator_tools_changed": {Cause: CauseAlreadyRecorded, Reason: "a NUDGE between nodes that one node built its " +
		"operator catalogue again, so every node tells the MCP clients " +
		"listening on it to list their tools again. The apply that moved the " +
		"catalogue is already a row (config_revision_applied), and a durable " +
		"row per node per rebuild would record one revision once more per " +
		"member of the fleet"},
	"sandbox_answer_given": {Cause: CauseAlreadyRecorded, Reason: "the WAKE an answer by turn puts on the seat's " +
		"inbox, and never a turn. What the answer became is " +
		"sandbox_run_answered, and that a person gave it is their " +
		"operator_acted row, so categorising this would write a third row " +
		"for one answer — the reason a2a_request is kept out above"},
	"reflection_due": {Cause: CauseAlreadyRecorded, Reason: "the WAKE that puts a finished turn in front of " +
		"reflection on the seat's holder. The turn is already a row " +
		"(turn_completed), and what reflecting on it did is its own row " +
		"(reflection_completed), so categorising this would write the turn a " +
		"second time — the reason a2a_request is kept out above"},
	"custody_batch": {Cause: CauseAlreadyRecorded, Reason: "a CARRIER, not an event: a node without `data` keeps " +
		"no event log, so it publishes its events in batches and one data " +
		"node writes each event inside as the row it is (ADR-0025). A row " +
		"for the batch would describe the transport and repeat every event " +
		"in it — and on the node that published it, it would be handed to " +
		"custody in a batch of its own"},
	"raw_webhook": {Cause: CauseAlreadyRecorded, Reason: "the delivery is ALREADY a row: both inbound edges — the " +
		"webhook receiver and the Mattermost socket — publish inbound_delivery " +
		"beside it, filed under the provider's own label with the provider's " +
		"bytes as its payload. This event is the wake an edge publishes for the " +
		"transports to route, so categorising it would write a second row per " +
		"delivery saying the same thing under a different id"},
}

// unfed are the PERSISTED types the activity feed does not carry, each with the
// reason.
//
// A RULE CHANGE, made once and here rather than as a special case in a reader:
// "persisted" and "fed" were one fact — a categorised type was a row AND a
// line of the feed — and the feed is a ring of a few hundred rows (livestate.EventFeedLimit)
// for the WHOLE COMPANY, because every node's projection is fed by a fleet-wide
// broadcast. A type that is accounting rather than activity takes those rows
// from the events a person watching the feed is there for, and a projection
// that skipped it on its own would leave the startup seed, which reads the
// feed back out of the store, disagreeing with the stream about what the feed
// holds. So the class is declared beside the admission list, and every read
// of the feed's rows asks [KeptOutOfFeed]: the live projection's ring, its
// startup seed, and every view a reader scrolls on from the ring — the event
// log's older pages and its axis, the Live screen's activity strip and a
// seat's latest events — which ask for `feed_only` (store.ListQuery.FeedOnly),
// so a bar never counts and an older page never lists a row the ring would
// not hold.
//
// An unfed type is still a row with a category: it is in the store, in a
// turn's and a trace's history, and listed by every read that does not ask
// for the feed's rows — a type filter, a turn, a trace, a work key.
//
// MOVING A TYPE INTO THE CLASS moves every one of those reads at once, on
// every node: the history scatter carries `feed_only` to each node it asks
// (internal/eventfan), and each node leaves the class out of its own listing
// and its own axis by this list.
var unfed = map[string]string{
	"auxiliary_spend": "ACCOUNTING, NOT ACTIVITY: a coalesced record of what " +
		"the auxiliary model cost for one key, several per turn beside the " +
		"turn's own phases and a burst of them when a compaction runs. In " +
		"the feed they would push the turns, failures and deliveries a " +
		"reader watches out of a ring that holds the whole company's last " +
		"few hundred events; their spend reaches every spend figure through " +
		"the rollups, and a turn's page reads them by its turn id",
	"code_fetched": "AN AUDIT, NOT ACTIVITY: one record per reader of a " +
		"`visibility: unit` repository's contents per fifteen-second flush, " +
		"and a busy company's readers — a CI token fetching on every build, " +
		"seats reading files a line range at a time — write them faster than " +
		"a person reads a feed, pushing the turns, failures and deliveries a " +
		"reader watches out of a ring that holds the whole company's last few " +
		"hundred events. Who read a repository is answered where it is asked: " +
		"the code host's readers route and `code_repo_readers`, summed across " +
		"the fleet from the usage domain these rows are derived into",
	"code_check_finished": "COUNTED, NOT WATCHED: one record per attempt of a " +
		"check that ended, and every push of every change ends each check its " +
		"repository's checks file defines — a busy company's faster than a " +
		"person reads a feed, pushing the turns, failures and deliveries a " +
		"reader watches out of a ring that holds the whole company's last few " +
		"hundred events. An attempt's ending is read where it is asked — the " +
		"change's checks, the repository's — from its row on the code log; " +
		"these rows are what a node's day counts",
}

// liveOnly is the subset of [excluded] that still drives the live projection.
//
// A subset rather than the same set: agent_turn_progress, the two seat
// lifecycle events and the budget snapshot move a live row without joining the
// activity feed, while raw_webhook reaches the projector not at all, because it
// is published onto the inbound notification topic rather than onto
// crewlet.events.* — the feed hears a delivery through inbound_delivery.
var liveOnly = map[string]bool{
	"agent_turn_progress": true,
	"agent_spawned":       true,
	"agent_terminated":    true,
	"budget_meters":       true,
}

// Category names an event type's dashboard category and reports whether the
// type is placed at all. An unplaced type is neither persisted nor fed to the
// activity feed.
func Category(eventType string) (string, bool) {
	p, ok := categories[eventType]
	return p.category, ok
}

// RateAuthorOf reports who authors a categorised type's rate, and whether the
// type is placed at all.
//
// Only a PLACED type has one here: an excluded type is out of the estate
// whoever sets its pace, which is the point of excluding it.
func RateAuthorOf(eventType string) (RateAuthor, bool) {
	p, ok := categories[eventType]
	return p.rate, ok
}

// LiveOnly reports whether a type is excluded from the store while still
// driving the live projection.
func LiveOnly(eventType string) bool { return liveOnly[eventType] }

// KeptOutOfFeed reports whether a type is one of the [unfed]: persisted, and
// deliberately not a line of the activity feed.
//
// THE NARROWING, NOT THE ADMISSION. A reader holding a categorised envelope
// asks this and nothing else, so the category stays the one statement that a
// type is persisted — a type this build does not know, which a newer peer's
// stored row can be, is a feed row exactly as it was.
func KeptOutOfFeed(eventType string) bool {
	_, kept := unfed[eventType]
	return kept
}

// Unfed returns every persisted type the activity feed does not carry, sorted —
// for a reader of the store that rebuilds the feed and has to name them in a
// filter.
func Unfed() []string { return slices.Sorted(maps.Keys(unfed)) }

// UnfedReasons returns every unfed type with its reason.
func UnfedReasons() map[string]string { return maps.Clone(unfed) }

// Excluded reports why a type is kept out of the event store, or "" if it is
// not deliberately excluded — which, for a type with no category either, means
// nobody has placed it.
func Excluded(eventType string) string { return excluded[eventType].Reason }

// WebhookCategory is the category every inbound DELIVERY row is filed under,
// and the one inbound_delivery alone carries.
//
// Its rows are the one family whose type column is not the event's type: an
// inbound_delivery is filed under the delivery's own label (`webhook:push`,
// `forge:avi:jira:created:issue`, `socket:posted`) with the provider's bytes as its
// payload — see internal/observe — because that is what an operator matches
// against their provider's console. Named rather than spelled in the map, since
// the integration counts (internal/api/queries) and the dashboard's delivery
// panels select on this value.
const WebhookCategory = "webhook"

// CategoryNames is every value the `category` column can hold, sorted.
//
// Exported so a docs generator and the guard test read the same list the
// engine files rows under, rather than a copy somebody has to remember to
// update.
func CategoryNames() []string {
	seen := map[string]bool{}
	for _, placed := range categories {
		seen[placed.category] = true
	}
	out := slices.Sorted(maps.Keys(seen))
	return out
}

// TypesByCategory groups every placed type under its category, each group
// sorted. The docs table is generated from this.
func TypesByCategory() map[string][]string {
	out := map[string][]string{}
	for eventType, placed := range categories {
		out[placed.category] = append(out[placed.category], eventType)
	}
	for _, group := range out {
		slices.Sort(group)
	}
	return out
}

// Exclusions returns every deliberately-excluded type with its cause and its
// reason.
func Exclusions() map[string]Exclusion {
	return maps.Clone(excluded)
}

// AdmissionViolations reports every way the node estate's admission rule is
// currently broken. AN EMPTY SLICE IS THE ONLY PASSING ANSWER.
//
// THE RULE: no event type whose rate an unauthenticated caller authors reaches
// the node estate. The estate is a local file on a disk an operator sized, and
// every row in it is something this company did; a type whose rate anybody who
// can reach the listener decides hands the size of that file, and the cost of
// every backup, snapshot and integrity check taken from it, to whoever is
// making the requests. A failed login is the worked example — an anonymous
// caller authors millions a day for free — so the per-attempt fact belongs on
// a metrics counter and only a COALESCED event, per source per minute and
// published by the engine's own loop, is a row.
//
// Reported rather than refused at startup, and never a panic: this is a
// property of two tables in this file, so a violation is a compile-time
// mistake caught by a test in this package, not a condition a running node can
// find itself in.
func AdmissionViolations() []string {
	return admissionViolations(categories, excluded)
}

// admissionViolations is [AdmissionViolations] over tables it is HANDED, so
// the rule can be exercised against a taxonomy that breaks it. A guard that
// can only ever be run against a correct table proves nothing about what it
// would do with a wrong one.
func admissionViolations(placed map[string]placement, kept map[string]Exclusion) []string {
	var out []string
	for eventType, p := range placed {
		switch {
		case p.rate == RateAnonymous:
			out = append(out, eventType+" is categorised as "+p.category+
				" and its rate is authored by an unauthenticated caller: "+
				"exclude it with cause "+string(CauseAnonymousRate)+" and "+
				"categorise the coalesced event instead")
		case !p.rate.Valid():
			out = append(out, eventType+" is categorised as "+p.category+
				" and states no rate author: an unstated author is not a "+
				"claim that the rate is bounded")
		}
	}
	for eventType, why := range kept {
		if _, alsoPlaced := placed[eventType]; alsoPlaced {
			out = append(out, eventType+" is both categorised and excluded ("+
				string(why.Cause)+"): the category wins, so the exclusion "+
				"excludes nothing")
		}
	}
	slices.Sort(out)
	return out
}
