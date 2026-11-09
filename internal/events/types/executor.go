package types

import (
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/kept"
)

// The executor in its box. Every agent seat's executor is its harness's own
// run in a box (ADR-0066): the turn parks as soon as the executor is launched
// and the seat stays busy until the completion signal arrives, so these events
// are the only trace of work that outlives the turn that launched it — the
// run's own record is deleted the moment it settles.

func init() {
	events.Register[ExecutorLaunched]()
	events.Register[ExecutorFinished]()
	events.Register[ExecutorCollected]()
	events.Register[ExecutorAsked]()
	events.Register[ExecutorFailed]()
	events.Register[ExecutorStopped]()
	events.Register[ExecutorAnswerGiven]()
	events.Register[ExecutorAnswered]()
}

// ExecutorLaunched marks an executor launched in its box, after the
// pending-run row is persisted: the first launch of a round, and every
// relaunch that carries a round on.
type ExecutorLaunched struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	TurnID      string `json:"turn_id"`
	// WorkKey is the unit of work the run this belongs to was dispatched
	// for — see [AgentPhaseCompleted.WorkKey] and ADR-0017. Carried so the
	// work-key filter answers with a run's WHOLE record rather than only
	// its phases.
	WorkKey   string `json:"work_key,omitempty"`
	SandboxID string `json:"sandbox_id"`
	Harness   string `json:"harness"`
	// ProviderKey is the providers.llm entry the run was launched on, and
	// Model the model it runs under: the member of the seat's executor
	// chain the launch picked, which a fallback makes other than the chain's
	// head. On the announcement because a running run is drawn from it, and
	// which entry's plan a job is spending is what a person watching it asks
	// — the run's own record says so only once it is collected.
	ProviderKey     string `json:"provider_key,omitempty"`
	Model           string `json:"model,omitempty"`
	ConversationKey string `json:"conversation_key"`
	// WorkItem is the item the launching turn is charged to, absent when
	// it is on nothing. The run is part of that turn, so a running-runs
	// panel can say which item a box is working on without joining back to
	// a turn that may have parked days ago.
	WorkItem *WorkItem `json:"work_item,omitempty"`
	// Task is a short human-readable summary for the Executors board;
	// the full brief lives on the pending run, not on the wire.
	Task string `json:"task"`
	// LaunchID names THIS JOB — the sandbox.PendingRun row's launch id,
	// minted by the store — and StartedAt is when the store recorded the
	// launch. A turn can launch more than one job, and these are what pair
	// a start with the job's own [ExecutorCollected] record (which carries
	// the same id) and what a live-output request (`executor_tail`) names.
	// Absent on an announcement whose row could not be read back.
	LaunchID  string    `json:"launch_id,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
}

// EventType is the "executor_launched" wire type.
func (ExecutorLaunched) EventType() string { return "executor_launched" }

// Role is the seat the detached job belongs to; it stays busy until the run
// reports back.
func (e ExecutorLaunched) Role() string { return e.RoleName }

// AgentID is the instance that launched the run.
func (e ExecutorLaunched) AgentID() string { return e.Agent }

// SummaryFor names the harness, which is the one thing distinguishing two
// otherwise identical launches on the same seat.
func (e ExecutorLaunched) SummaryFor(actor string) string {
	return lead(actor, "launched its executor ("+e.Harness+")")
}

// ExecutorFinished signals that an executor's run in its box finished.
//
// A pure control signal. It carries the original TurnID so the engine can load
// the pending row, reconnect and collect the actual result from the sandbox —
// the outcome (success, delivered refs, tokens) is read at collect time and
// never rides on the event. The engine flips the row running→resumed atomically
// so the resume happens at most once even when this is delivered more than
// once, which it will be: successive poll ticks can both fire before the first
// claim lands, and queue delivery is at-least-once.
//
// LaunchID names WHICH job finished. The row is the turn's and a turn can run
// more than one job, so a duplicate of this signal can arrive after the job it
// was raised for has parked on a question or been replaced by the next one;
// the claim takes the row only while it still holds this launch, running.
type ExecutorFinished struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	TurnID      string `json:"turn_id"`
	LaunchID    string `json:"launch_id,omitempty"`
	// WorkKey is the unit of work the run this belongs to was dispatched
	// for — see [AgentPhaseCompleted.WorkKey] and ADR-0017. Carried so the
	// work-key filter answers with a run's WHOLE record rather than only
	// its phases.
	WorkKey   string `json:"work_key,omitempty"`
	SandboxID string `json:"sandbox_id"`
	Harness   string `json:"harness"`
}

// EventType is the "executor_finished" wire type.
func (ExecutorFinished) EventType() string { return "executor_finished" }

// Role is the seat waiting on the run — the seat this signal releases.
func (e ExecutorFinished) Role() string { return e.RoleName }

// AgentID is the instance whose suspended turn resumes.
func (e ExecutorFinished) AgentID() string { return e.Agent }

// SummaryFor is possessive ("Engineer's executor finished"), so it spells
// the actor into the line rather than going through lead: the job finished, and
// the seat did not do the finishing.
func (e ExecutorFinished) SummaryFor(actor string) string {
	if actor == "" {
		return "Executor finished"
	}
	return actor + "'s executor finished"
}

// ExecutorCollected is ONE LAUNCH of an executor's run, as its collection read
// it off the box: what the launch's harness spent and reported, what it delivered,
// its account of what it did and, where it did not finish, why. Published by
// the sandbox coordinator once per launch it collects — one that finished, one
// that stopped to ask a person, one a person stopped, and one lost with its
// host whose box could still be read — because every one of them spent what it
// spent.
//
// # Not a phase
//
// The executor's PHASE is the turn's own `execute` record
// ([AgentPhaseCompleted]), which the segment that resumes the turn publishes:
// the launch and the answer as the one round the engine saw, the calls the run
// made over the seat's bridge, the submission's decision — and no tokens,
// because the engine made no model call for it. A launch is part of that
// round's work, and a round can have several: an answer carries a parked run
// on in a second launch, and a launch lost with its host is relaunched. This
// record was once an `agent_phase_completed` with a phase word of its own, and
// every reader had to stitch the two back together — the spend rollup folded
// the word into the executor's band, the live projection kept it from moving
// the turn, the dashboard drew it as a sibling of the executor that launched
// it. Its own type says what it is, as [AuxiliarySpend]'s does, and nothing
// that reads a phase has to know to step around it.
//
// # The executor's spend
//
// It is where the executor's tokens are: every reader of spend — the event
// store's writer, the live projection's window — files it under the `execute`
// phase, with the harness's own turns as its calls (tokens.LaunchCalls), so the
// executor's band holds what its run cost however many launches the round
// took.
//
// # Once
//
// For the reason the launch is charged once: its publish sits inside the part
// of the collection that is retried, every reader counts it as spend, and a
// second copy would be a second charge on every surface that shows one. The
// launch's own record holds whether it went out
// (internal/sandbox's LaunchRecord.Published).
//
// # A failure of its turn
//
// `failed` is the key a phase's record carries and means what it means there:
// the store promotes it into the tag that marks the turn failed in the turn
// list, and the live projection makes it the seat's `last_error`. A launch
// that stopped to ask, was stopped by a person or was cut off by its host did
// not fail — what happened is its Notes.
type ExecutorCollected struct {
	Agent    string `json:"agent_id"`
	RoleName string `json:"role"`
	TurnID   string `json:"turn_id"`
	// WorkKey is the unit of work the run this belongs to was dispatched
	// for — see [AgentPhaseCompleted.WorkKey] and ADR-0017.
	WorkKey string `json:"work_key,omitempty"`
	// Iteration is the executor round that launched it — the iteration of
	// the `execute` record whose work this launch is — and LaunchID the
	// job ([ExecutorLaunched.LaunchID]). A round can launch more than once,
	// so a launch is (turn_id, iteration, launch_id).
	Iteration int    `json:"iteration"`
	LaunchID  string `json:"launch_id"`
	// Model and ProviderKey are the member of the seat's executor chain the
	// launch picked, as the launch recorded them: which a fallback makes
	// other than the chain's head, and which entry's plan the run spent.
	Model       string `json:"model"`
	ProviderKey string `json:"provider_key"`
	// Harness is the harness the run ran as, and SandboxID the box.
	Harness   string `json:"harness"`
	SandboxID string `json:"sandbox_id"`
	// Response is the report the launch wrote back: the words its harness ended
	// on, secret-redacted.
	Response string `json:"response"`
	// ActivityTranscript is the launch's own account of what it did — what
	// it said, its tool calls and what they ran, or its stderr where the harness
	// streams nothing better. Secret-redacted, and held to the record's
	// bound: a long one keeps its first 64 KiB and its last 192 KiB in whole
	// lines, with a note line where its middle was. It is the whole
	// observability surface of an agent that emits no telemetry of its own.
	ActivityTranscript string `json:"activity_transcript,omitempty"`
	// ActivityTranscriptElidedLines and ActivityTranscriptElidedBytes count
	// what that bound left out of the transcript's middle — whole lines, and
	// every byte not kept — so a screen can say the record is not the whole
	// log without parsing the note. Zero on a transcript kept whole.
	ActivityTranscriptElidedLines int `json:"activity_transcript_elided_lines,omitempty"`
	ActivityTranscriptElidedBytes int `json:"activity_transcript_elided_bytes,omitempty"`
	// The launch's tokens, as its harness reported them. TotalTokens is input
	// plus output, and CacheReadTokens and CacheWriteTokens are a BREAKDOWN
	// of InputTokens, never an addition to it — see
	// [AgentPhaseCompleted.CacheReadTokens].
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	// HarnessTurns is how many model turns the launch's harness says its own loop
	// took: the launch's provider calls, which the engine neither made nor
	// timed. Absent where the harness reported none (OpenCode reports none) —
	// the launch then counts as one call, the one the engine asked for. What
	// every spend figure counts a launch's calls by is tokens.LaunchCalls.
	HarnessTurns int `json:"harness_turns,omitempty"`
	// CostUSD is what the launch's own harness said it billed: a subscription
	// harness's spend never passes through the engine's token meter, so this is
	// the one number that sees it.
	CostUSD float64 `json:"cost_usd"`
	// DeliveredRefs are the branches and pull requests the launch reported,
	// read from its whole report — its `Delivered:` lines, or its
	// pull-request URLs where it named none — deduplicated and listed up to
	// a bound, and DeliveredRefsElided how many more it reported than that.
	// Zero on a record that lists them all.
	DeliveredRefs       []string `json:"delivered_refs,omitempty"`
	DeliveredRefsElided int      `json:"delivered_refs_elided,omitempty"`
	// PushedRefs are the refs the launch moved on Gitlet, as
	// Gitlet's push rows written under its launch say, read
	// when it was collected — the engine's account beside DeliveredRefs, the
	// launch's own. On this record because a launch that parks on a question
	// is resumed by an answer, which collects nothing, so the pushes of the
	// launch that asked are here or nowhere. PushedRefsElided and
	// PushedRefsUnread mean what they mean on
	// [AgentPhaseCompleted.PushedRefs]; all three absent on a launch given no
	// repository it could push to.
	PushedRefs       []PushedRefRecord `json:"pushed_refs,omitempty"`
	PushedRefsElided int               `json:"pushed_refs_elided,omitempty"`
	PushedRefsUnread string            `json:"pushed_refs_unread,omitempty"`
	// StartedAt is when the store recorded the launch, and DurationMS how
	// long it ran until this node collected it: the waiter's poll interval
	// is inside it, and the coding itself is the rest. Absent where the
	// launch's row held no instant.
	StartedAt  time.Time `json:"started_at,omitzero"`
	DurationMS int       `json:"duration_ms"`
	// WorkItem is the item the launching turn is charged to, absent when it
	// is on nothing — see [AgentTurnStarted.WorkItem].
	WorkItem        *WorkItem `json:"work_item,omitempty"`
	ConversationKey string    `json:"conversation_key"`
	// Notes is how a launch that did not fail ended without finishing: the
	// question it stopped to ask, who stopped it, or what cut it off.
	Notes string `json:"notes"`
	// Failed, Error and ErrorKind are a launch that did not succeed, with its
	// reason, redacted; the kind is [ExecutorErrorContextExhausted] where the
	// harness said its conversation no longer fits the model's context, and
	// [ExecutorErrorLaunchFailed] for every other cause.
	Failed    bool   `json:"failed"`
	Error     string `json:"error"`
	ErrorKind string `json:"error_kind"`
	// HarnessSession is the harness's own session the launch RAN, as the
	// run's script recorded it — which, for a launch asked to continue a
	// session its box could not, is the new one it started instead — and
	// HarnessVersion the harness's release, as its `--version` gave it. Both
	// are read in the box and held to the shapes a harness gives them, where
	// the box is read and again as the record is published (internal/sandbox's
	// ValidSessionID and ValidRelease): an id of letters, digits, `-` and `_`,
	// at most 128 of them, and a release of three dotted numbers, at most 64
	// bytes. Absent where the launch recorded none of that shape.
	HarnessSession string `json:"harness_session,omitempty"`
	HarnessVersion string `json:"harness_version,omitempty"`
	// SessionTakenBytes is the size of the sealed session the read that
	// collected the launch took out of its box to keep, where it took one:
	// its upload and its record are made after the read, off it, and how
	// they came out is stamped on the run's record and logged
	// (executor_session_kept, executor_session_not_kept). SessionNotKept is
	// why the read took none, or refused the one it took — a pack that
	// failed, a session past its bounds, secrets the box was handed that
	// could not be read back — redacted, and cut to a KiB, marked `…`, as
	// the executor log holds the same reason. Both absent where nothing was
	// taken: a harness that carries no session, a box that held none, or the
	// company's `turn_engine.conversation_session.enabled` off.
	SessionTakenBytes int64  `json:"session_taken_bytes,omitempty"`
	SessionNotKept    string `json:"session_not_kept,omitempty"`
	// ContextWindowTokens is the context window a Claude Code run was pinned to
	// compact at, from the model it ran (internal/sandbox's ClaudeCodeWindow).
	// Absent for a harness whose window the engine does not set.
	ContextWindowTokens int `json:"context_window_tokens,omitempty"`
	// Brief is which of the four briefs the launch handed its run:
	// `continued` (the turn's session, in the box it was last worked in),
	// `continued_new_box` (the turn's session, restored into a box that
	// replaced that one), `continued_conversation` (the session the turn's
	// conversation's last turn left, restored into a box of its own) or
	// `fresh` (a session of its own). Absent where the launch recorded none.
	//
	// HarnessSessionContinued says the run did continue the session it was
	// asked to — its box held it, or restored the archive carried in — and
	// HarnessSessionFellBack why a run asked to continue one started a
	// session of its own in its place, as its script said it: such a run is
	// handed the fresh brief by its script, so a `continued` brief beside
	// HarnessSessionContinued false is a run that read the fresh one. The
	// reason is one sentence, redacted and cut to a KiB, marked `…`
	// (internal/sandbox's MaxFellBackBytes): its whole is on the run's error
	// stream, where its script prints it too.
	Brief                   string `json:"brief,omitempty"`
	HarnessSessionContinued bool   `json:"harness_session_continued,omitempty"`
	HarnessSessionFellBack  string `json:"harness_session_fell_back,omitempty"`
}

// The kinds of a launch that did not succeed ([ExecutorCollected.ErrorKind]).
const (
	// ExecutorErrorLaunchFailed is any failure no more specific kind names.
	ExecutorErrorLaunchFailed = "launch_failed"
	// ExecutorErrorContextExhausted is a run whose conversation no longer
	// fits its model's context: a cause another launch of the same session
	// meets again, which is why it is told from the rest and why no later
	// launch continues that session (internal/sandbox's ranOutOfContext).
	ExecutorErrorContextExhausted = "context_exhausted"
)

// EventType is the "executor_collected" wire type.
func (ExecutorCollected) EventType() string { return "executor_collected" }

// Role is the seat whose executor the launch was; its tokens are that seat's.
func (e ExecutorCollected) Role() string { return e.RoleName }

// AgentID is the instance whose executor the launch was.
func (e ExecutorCollected) AgentID() string { return e.Agent }

// SummaryFor names the harness and what the launch spent, or the kind of its
// failure — the two things a feed row about a collected launch is read for.
// Possessive, as [ExecutorFinished]'s is: the engine collected the launch, and
// the seat did not do the collecting.
func (e ExecutorCollected) SummaryFor(actor string) string {
	who := "Executor"
	if actor != "" {
		who = actor + "'s executor"
	}
	agent := e.Harness
	if agent == "" {
		agent = "?"
	}
	if e.Failed {
		kind := e.ErrorKind
		if kind == "" {
			kind = "error"
		}
		return fmt.Sprintf("%s launch failed (%s, %s)", who, agent, kind)
	}
	return fmt.Sprintf("%s launch was collected (%s, %d tokens)", who, agent, e.TotalTokens)
}

// ExecutorAsked records an executor asking a person a question.
//
// The box never posts anything itself: the executor signals through its ask
// tool, the runner surfaces the question and audience, and the engine
// announces it with this event and resolves the audience into the people it is
// put to, on the run's record. That is how it reaches them — a row in each one's decisions
// on the dashboard and on the executors board — and NOTHING POSTS IT TO A
// CHAT SURFACE. The agent goes free while it waits, so nothing else marks the
// pause.
//
// Audience is "requester", "team", "manager" or a handle — an open set, since a
// named colleague is a legitimate audience.
type ExecutorAsked struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	TurnID      string `json:"turn_id"`
	// WorkKey is the unit of work the run this belongs to was dispatched
	// for — see [AgentPhaseCompleted.WorkKey] and ADR-0017. Carried so the
	// work-key filter answers with a run's WHOLE record rather than only
	// its phases.
	WorkKey   string `json:"work_key,omitempty"`
	SandboxID string `json:"sandbox_id"`
	// Question is what the run asked, in one sentence; QuestionContext its
	// Markdown context, and Decision the choice it asks for, absent for an
	// open question (ADR-0068).
	Question        string             `json:"question"`
	QuestionContext string             `json:"question_context,omitempty"`
	Decision        *decision.Decision `json:"decision,omitempty"`
	Audience        string             `json:"audience"`
	ConversationKey string             `json:"conversation_key"`
	// WorkItem is the item the parked turn is charged to — the one its row
	// recorded at launch — so a question can be shown against the work it
	// is about. Absent when the turn is on nothing.
	WorkItem *WorkItem `json:"work_item,omitempty"`
	// LaunchID is the job that asked ([ExecutorLaunched.LaunchID]), which an
	// answer names as its question, so a live surface can offer the answer
	// without reading the run's record first. Absent where the row named none.
	LaunchID string `json:"launch_id,omitempty"`
	// HeldCall is the tool call the question is about, where the run asked
	// because a call it made needs a person's approval (ADR-0077). Such a
	// question is answered through the approvals route, which a surface can
	// only offer once it knows the call is held.
	HeldCall *HeldCallRef `json:"held_call,omitempty"`
}

// HeldCallRef names a tool call a run holds for a person's approval: the id
// the approval answers it by, the server and tool it calls (the tool as the
// run called it, Raw as the server names it), the definition it was held on,
// and when. Never its arguments, which stay sealed on the run's record.
type HeldCallRef struct {
	ID     string    `json:"id"`
	Server string    `json:"server"`
	Tool   string    `json:"tool"`
	Raw    string    `json:"raw"`
	Digest string    `json:"digest"`
	HeldAt time.Time `json:"held_at,omitzero"`
}

// EventType is the "executor_asked" wire type.
func (ExecutorAsked) EventType() string {
	return "executor_asked"
}

// Role is the seat whose run asked, and whom the question is attributed to —
// the box never speaks in its own name.
func (e ExecutorAsked) Role() string { return e.RoleName }

// AgentID is the instance whose run asked.
func (e ExecutorAsked) AgentID() string { return e.Agent }

// SummaryFor names the audience, falling back to "someone": an unrouted
// question is still a question, and a line reading "asked  a question" would
// hide that the audience was missing.
func (e ExecutorAsked) SummaryFor(actor string) string {
	audience := e.Audience
	if audience == "" {
		audience = "someone"
	}
	return lead(actor, "asked "+audience+" a question")
}

// ExecutorFailed records an executor's run being settled without its turn
// ever resuming.
//
// THE SILENT ENDING, given a voice. `settleFailed` kills the box, deletes the
// run's record and frees the seat, and it published nothing, so a turn that
// had been destroyed presented to the seat, the dashboard and the requester as
// an identical silence. Several distinct conditions funnel into it, and the
// first symptom of any of them was a wait that never ended: the run vanished
// from the active board and the completion event that would have explained it
// had already been acked. A failure has to be at least as loud as the question
// [ExecutorAsked] already announces, and since a settled run
// has no record, this event is the only account of how it ended.
//
// Reason is a closed set — see the ExecutorFailure constants — because it is
// the one field a reader acts on differently.
type ExecutorFailed struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	TurnID      string `json:"turn_id"`
	// WorkKey is the unit of work the run this belongs to was dispatched
	// for — see [AgentPhaseCompleted.WorkKey] and ADR-0017. Carried so the
	// work-key filter answers with a run's WHOLE record rather than only
	// its phases.
	WorkKey   string `json:"work_key,omitempty"`
	SandboxID string `json:"sandbox_id"`
	Harness   string `json:"harness"`
	Reason    string `json:"reason"`
	// Detail is a human sentence naming what to do about it, never a stack
	// or a raw provider error: it reaches an operator's board.
	Detail string `json:"detail"`
	// ErrorKind is the classifier's word for the failure behind the reason, on
	// [ExecutorFailureResumeFailing] alone: whether the turn's re-entry met the
	// engine's own defect or its environment.
	ErrorKind string `json:"error_kind,omitempty"`
}

// The reasons a detached run is settled without resuming its turn.
//
// A NAMED SET, because these are not variations of one failure: an
// unreachable box is infrastructure, a missing conversation is a bug in this
// engine, a suspension that could not be recorded is the coordination store or
// this engine, an abandoned tail is a node that died, and a removed seat is an
// operator's own change. An operator seeing them merged into "the sandbox
// failed" would chase the wrong one.
const (
	// ExecutorFailureCollect — the job finished but its box could not be
	// read back, so there is no result for its turn to judge.
	ExecutorFailureCollect = "collect_unreachable"

	// ExecutorFailureNoExecuteState — the row carried none of its turn's
	// parked state, so there is no turn to resume into.
	ExecutorFailureNoExecuteState = "no_execute_state"

	// ExecutorFailureAbandoned — a tail the previous owner of this seat left
	// mid-flight, found by the recovery pass. Nothing will ever pick it up.
	ExecutorFailureAbandoned = "abandoned_tail"

	// ExecutorFailureSuspensionUnrecorded is a run whose turn suspended but
	// whose conversation never reached the run's record: the runner
	// recorded none, it would not serialize, or the record could not be
	// written and the run is still launching. The job was already
	// executing, so its box is reclaimed rather than left to its TTL. A run
	// that is no longer launching is not ended under this reason: its
	// conversation landed after all, or somebody else already ended it.
	ExecutorFailureSuspensionUnrecorded = "suspension_unrecorded"

	// ExecutorFailureClaimStranded is a run whose tail this node claimed and
	// then could not give back: the park or the resume the claim was taken
	// for did not land, and the row could not be reverted to the status it
	// was claimed from either. A row left in the at-most-once claim is read
	// by no completion poll, re-claimed by no redelivery and matched by no
	// answer, so the run is ended here rather than left to strand its box
	// until the seat changes hands. It names the coordination store: two
	// writes on one row failed in a row.
	ExecutorFailureClaimStranded = "claim_unreverted"

	// ExecutorFailureSeatRemoved is a run of a seat that left the company's
	// agent seats — removed from the chart, or made a person's and not an
	// agent's again within the mailbox retirement grace. It is ended when the
	// seat's mailbox is retired, because no resume, answer or completion can
	// reach a seat that is gone.
	ExecutorFailureSeatRemoved = "seat_removed"

	// ExecutorFailureResumeBroken is a run whose resume broke BEFORE the turn
	// it would continue began — a panic re-entering the suspended
	// conversation. It is not retried, because the conversation is the same
	// bytes on every attempt and a retry reaches the same defect; nothing of
	// the turn ran, so no completion of its own says what became of it, and
	// a person's reply that drove the resume goes back to the seat. A resume
	// that broke after its turn began is not announced under this or any
	// reason: that turn published its own failed completion.
	ExecutorFailureResumeBroken = "resume_broken"

	// ExecutorFailureDeadline is a run whose turn's wall-clock cap (a
	// scheduled fire's timeout) passed while the turn that would review its
	// job could not run: refused by a condition that ends by waiting (a
	// model's usage limit, a spent budget, a paused seat) or failing. Past the
	// cap nothing waits and nothing is retried, so the run is ended and the
	// seat freed rather than held for as long as the condition lasts. What
	// the job did was collected and charged; the work it pushed, if any, is
	// on its branch.
	ExecutorFailureDeadline = "deadline_passed"

	// ExecutorFailureResumeFailing is a run whose job finished and was
	// collected, but whose turn could not be resumed: every attempt failed
	// for the resume give-up window. It names the turn's own re-entry (its
	// models unreachable, a defect it keeps meeting), never the box, which
	// answered.
	ExecutorFailureResumeFailing = "resume_failing"
)

// EventType is the "executor_failed" wire type.
func (ExecutorFailed) EventType() string { return "executor_failed" }

// Role is the seat whose turn was lost.
func (e ExecutorFailed) Role() string { return e.RoleName }

// AgentID is the instance whose run failed.
func (e ExecutorFailed) AgentID() string { return e.Agent }

// SummaryFor names the reason, because the whole point of this event is that
// the reasons are told apart.
func (e ExecutorFailed) SummaryFor(actor string) string {
	reason := e.Reason
	if reason == "" {
		reason = "an unrecorded reason"
	}
	return lead(actor, "lost its executor's launch to "+reason)
}

// ExecutorStopped records an executor's run a PERSON ended: its seat was
// paused with `stop_running` while the run was launching or running, and the
// run was ended there rather than left to finish — its harness killed with its box,
// its tool-bridge session closed, and what it had spent by then collected and
// charged.
//
// NOT A FAILURE, which is why it is a type of its own rather than a reason of
// [ExecutorFailed]: that type is a failure by its very name (see
// failureEventTypes), so a stop announced through it would be counted with the
// runs that broke, end the turn waiting on it as failed, and draw its seat as in
// trouble for being stopped by somebody who meant to. A stopped turn is never a
// failed one, here as for a turn stopped at its next round.
//
// THE RUN'S OWN ACCOUNT, beside the turn's. The turn that was parked on the run
// is closed on the record as stopped by the same person, in the shape a turn
// stopped at its next round takes: [AgentTurnStopped] naming who, and the
// turn's own completion carrying `stopped`. A run stopped while its turn was
// still launching it is closed by that turn as it unwinds; one whose turn had
// parked is closed by whichever node finishes the run's ending.
//
// PUBLISHED ONCE, under an id derived from the run and its ending, by whichever
// node finishes that ending — the one that stopped it, or the seat's next
// holder if that node stopped first — so a repeat after a crash is the same
// event to every store that keeps it.
type ExecutorStopped struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	TurnID      string `json:"turn_id"`
	// WorkKey is the unit of work the run this belongs to was dispatched
	// for — see [AgentPhaseCompleted.WorkKey] and ADR-0017.
	WorkKey   string `json:"work_key,omitempty"`
	SandboxID string `json:"sandbox_id"`
	Harness   string `json:"harness"`
	// LaunchID is the job the stop ended — see [ExecutorLaunched.LaunchID]
	// — so a reader pairs the stop with the start it ends rather than with
	// whichever job the turn ran last.
	LaunchID string `json:"launch_id,omitempty"`

	// StoppedBy, StoppedByKind and OperatorID are who paused the seat with
	// the stop and the credential they did it through, in the three halves
	// of [SeatPaused.PausedBy], and Reason is what they gave.
	StoppedBy     string `json:"stopped_by"`
	StoppedByKind string `json:"stopped_by_kind"`
	OperatorID    string `json:"operator_id,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// EventType is the "executor_stopped" wire type.
func (ExecutorStopped) EventType() string { return "executor_stopped" }

// Role is the seat whose run was stopped.
func (e ExecutorStopped) Role() string { return e.RoleName }

// AgentID is the instance whose run was stopped.
func (e ExecutorStopped) AgentID() string { return e.Agent }

// SummaryFor names who stopped the run, which is the one question a feed row
// about it is asked.
func (e ExecutorStopped) SummaryFor(actor string) string {
	by := e.StoppedBy
	if by == "" {
		by = "someone"
	}
	return lead(actor, "had its executor stopped by "+by)
}

// ExecutorAnswerGiven is the command an answer BY TURN puts on the seat's
// control topic: a person answering a parked executor's question by naming
// its turn, rather than by replying on the conversation it was asked in.
//
// # Why a second way in
//
// The chat route matches a reply to a run by conversation, and a run launched
// by a task assignment or a colleague's ask stored none, while one a schedule
// launched stores its schedule's, which nobody can reply into
// (internal/notify's Derived). Such a run parked on a question nobody could
// answer: there was no thread to reply in, and the question waited out its
// pause TTL while the person who knew the answer had nowhere to put it. An
// answer by turn names the run instead, and it can be given from any node.
//
// # Why on the seat's CONTROL topic
//
// Because only the node holding the seat can record and resume it, and the
// seat's topics are how anything reaches that node: durable subscriptions the
// holder, and only the holder, consumes. So an answer accepted on one node is
// carried out on another, and it survives a restart in between.
//
// The control topic and not the inbox, because the answer is addressed to a
// run, like the completion that topic was made for. The inbox is the seat's
// mail: held for as long as any of the seat's executors holds the seat, and
// worked oldest first after that. An answer that rode it waited behind every
// job the seat took on and every message it had not got to, and on a busy
// seat it never arrived. On the control topic it is recorded on arrival, and
// its resume waits for what a resume must (another of the seat's runs holding
// it, a turn in flight, a person's pause) and is then the seat's next turn.
//
// AND IT NAMES THE QUESTION, not only the run, because the resume can wait
// longer than the question does: see [ExecutorAnswerGiven.LaunchID]. It resumes
// the run only while the run still waits on the question it was given against.
//
// NEVER A TURN. The sandbox coordinator settles it, and whatever it becomes
// (resumed, not awaiting, gone) the delivery is spent there.
//
// KEPT OUT OF THE EVENT STORE, like the other inbox wakes: what happened to
// the answer is [ExecutorAnswered], and that the person gave it is their
// `operator_acted` row. See internal/events/category.go.
type ExecutorAnswerGiven struct {
	// TurnID is the run the answer is for — the parked run's own key.
	TurnID string `json:"turn_id"`

	// Agent is the seat that run belongs to, by its derived id, and its
	// control topic is the one this travels on (topics.AgentControl).
	// Carried in the payload as well, so the coordinator that settles it can
	// refuse one that reached the wrong seat. The id, because the topic is
	// named by it (ADR-0013).
	Agent string `json:"agent_id"`

	// AgentHandle is the seat's handle when the answer was given, for a
	// person reading the log line. Never compared: [Agent] is the routing
	// identity.
	AgentHandle string `json:"agent_handle"`

	// Answer is the person's reply, verbatim. Redacted where it is handed
	// to the run it carries on, like a chat reply. Empty where Choice says
	// it all.
	Answer string `json:"answer"`

	// Choice is the option of the run's question the person chose, by its
	// id, or empty for an answer in words alone. The run reads it as that
	// option's label and id beside the words (internal/sandbox's
	// AnswerText), checked against the question this answer is pinned to
	// ([ExecutorAnswerGiven.LaunchID]).
	Choice string `json:"choice,omitempty"`

	// AnsweredBy, AnsweredByKind and OperatorID are who answered, in the
	// three halves iam.ActorFor makes of a principal: the author (the seat
	// for a person the identity directory binds to one, the login for
	// anybody else), what sort of author that is (iam.ActorKind's
	// vocabulary), and the credential the answer came through. Three facts
	// for the reason `operator_acted` records three: a person reads who
	// answered, an audit asks what a credential did.
	AnsweredBy     string `json:"answered_by"`
	AnsweredByKind string `json:"answered_by_kind"`
	OperatorID     string `json:"operator_id,omitempty"`

	// LaunchID is the QUESTION this answers: the job the run held when it
	// asked, off the row the answer was accepted against. Every new question
	// comes with a new job — nothing but a launch opens one — so the job
	// identifies it. An answer reaches the seat some time after it was given
	// (after a seat handoff, after a NAK's backoff, as a retried copy), and
	// by then the run can have been
	// resumed by somebody else's answer and parked on a NEW question. Named
	// only by its turn, the late answer then resumed the run a second time,
	// as the answer to a question its giver never saw. Carrying the
	// question, it resumes the run only while the run still waits on that
	// one, and is spent as `not_awaiting` otherwise.
	//
	// REQUIRED. An answer naming no question cannot be told from one given
	// against a question the run has since moved past, so the node holding
	// the seat spends it without resuming the run (internal/sandbox's
	// Coordinator.AnswerByTurn); `answer_executor` stamps it off the row it
	// checked, and the answer desk will not deliver one without it.
	LaunchID string `json:"launch_id"`

	// Approval names the held call this answers, by its id, where the
	// question is a call to a tool at Needs approval (internal/sandbox's
	// HeldCall, ADR-0077), and is empty for every other answer. Stamped by
	// `POST /mcp-approvals/{id}` alone, which decides who may approve; the
	// node holding the seat records an answer to a held call only when it
	// names that call, and records none that names one for a question that
	// is not.
	Approval string `json:"approval,omitempty"`
}

// EventType is the "executor_answer_given" wire type.
func (ExecutorAnswerGiven) EventType() string { return "executor_answer_given" }

// AgentID is the seat whose run the answer is for.
func (e ExecutorAnswerGiven) AgentID() string { return e.Agent }

// SummaryFor names who answered, for a log line: the event itself is never on
// a feed.
func (e ExecutorAnswerGiven) SummaryFor(actor string) string {
	who := e.AnsweredBy
	if who == "" {
		who = actor
	}
	return lead(who, "answered a parked executor")
}

// AnswerVia is the route an answer reached a parked executor by.
type AnswerVia string

const (
	// AnswerViaChat — a reply on the conversation the question was asked
	// in, matched to the run by that conversation.
	AnswerViaChat AnswerVia = "chat"

	// AnswerViaOperator — an answer that named the run by its turn, through
	// the operator surface ([ExecutorAnswerGiven]).
	AnswerViaOperator AnswerVia = "operator"
)

// Valid reports whether v is one of the two routes.
func (v AnswerVia) Valid() bool { return v == AnswerViaChat || v == AnswerViaOperator }

// AnswerOutcome is what an answer that reached a parked executor became.
type AnswerOutcome string

const (
	// AnswerResumed — the answer resumed the suspended turn.
	AnswerResumed AnswerOutcome = "resumed"

	// AnswerNotAwaiting — the run exists and was not waiting for an answer
	// any more: another answer claimed it first, a reply too large for it
	// closed the question first ([AnswerTooLarge]), or it is running a job.
	AnswerNotAwaiting AnswerOutcome = "not_awaiting"

	// AnswerGone — the run is over: its record is gone, or it could not be
	// resumed at all and was ended.
	AnswerGone AnswerOutcome = "gone"

	// AnswerDeclined — an answer by turn the run was recorded with and could
	// not be resumed with, in every attempt the node holding its seat made:
	// the answer was let go of, the run waits on its question again, and the
	// person has to answer it again. A chat reply let go of the same way goes
	// on to its seat as the ordinary message it is, and announces nothing —
	// except a reply refused as too large ([AnswerTooLarge]), whose refusal
	// was announced as the run being told: its let-go before any turn told the
	// run (declined after its attempts, or a seal that did not open) is
	// announced on the chat route with the refusal's figures, the reply going
	// back to the seat as the message it was and the question waiting again,
	// under the anchor it was asked at.
	AnswerDeclined AnswerOutcome = "declined"

	// AnswerUnoffered is an answer by turn whose choice is not an option of
	// the question it names (ADR-0068): spent without being recorded, the
	// question still waiting. `answer_executor` checks the choice against the
	// question before it delivers, so one arriving here names a question
	// that is not the one the answer was checked against.
	AnswerUnoffered AnswerOutcome = "unoffered"

	// AnswerTooLarge is an answer past what its run may take as one
	// ([ExecutorAnswered.AnswerBytes] past [ExecutorAnswered.AnswerMaxBytes]).
	// By chat, a reply too large for the run's record to carry as its answer:
	// its REFUSAL is recorded as the question's outcome in the answer's
	// place, so the question no longer waits and nothing written after it is
	// taken for its answer, and the run is resumed told, in the engine's
	// words, that the person replied and the reply did not reach it — it
	// asks again for something shorter, or another way to receive it, under
	// a question of its own, or carries on and says so (ADR-0114). ONE
	// RECORD per run, question and delivery: every publish of it carries the
	// id and the instant (the reply's own) those derive, which the event
	// store keeps as one row. No `resumed` is published of it, which would
	// read the refusal as an answer the run carried on with; a refusal let go
	// of before any turn told the run is followed by [AnswerDeclined], and the
	// question waits again. By turn, an answer saying more than an answer may
	// (internal/sandbox's MaxAnswerTextBytes, measured as `answer_executor`
	// measures it, which refuses one before it is delivered): spent
	// unrecorded, with no ordinary form to go on as, the question still
	// waiting.
	AnswerTooLarge AnswerOutcome = "too_large"
)

// Valid reports whether o is one of the six outcomes.
func (o AnswerOutcome) Valid() bool {
	switch o {
	case AnswerResumed, AnswerNotAwaiting, AnswerGone, AnswerDeclined, AnswerUnoffered, AnswerTooLarge:
		return true
	}
	return false
}

// ExecutorAnswered records what an answer to a parked executor's question
// became, on either route.
//
// THE CHAT ROUTE LEFT NO RECORD. A person's reply that resumed a run was an
// ordinary chat message the dispatcher handed to the coordinator, and nothing
// said afterwards that it had been taken as an answer — nor, when the run had
// already been answered or had ended, that the reply had reached nothing. Now
// both routes publish this, once per answer that REACHED a run, with the route
// it came by and who gave it. An answer that is still owed a retry has not
// become anything yet and publishes nothing; a chat message that matched no
// run was never an answer at all.
type ExecutorAnswered struct {
	Agent       string `json:"agent_id"`
	AgentHandle string `json:"agent_handle"`
	RoleName    string `json:"role"`
	TurnID      string `json:"turn_id"`
	// WorkKey is the unit of work the run was dispatched for — see
	// [AgentPhaseCompleted.WorkKey] and ADR-0017.
	WorkKey string `json:"work_key,omitempty"`
	// WorkItem is the item the parked turn is charged to, absent when it is
	// on nothing, so an answer is shown against the work it moved.
	WorkItem *WorkItem `json:"work_item,omitempty"`

	Via     AnswerVia     `json:"via"`
	Outcome AnswerOutcome `json:"outcome"`

	// AnsweredBy, AnsweredByKind and OperatorID are who gave the answer.
	// By the operator route they are [ExecutorAnswerGiven.AnsweredBy]'s three
	// halves. By chat, AnsweredBy is the sender as that transport names
	// them and AnsweredByKind is `human`, with no OperatorID: the chat
	// surface vouched for the sender, and no credential of this engine's
	// was presented.
	AnsweredBy     string `json:"answered_by,omitempty"`
	AnsweredByKind string `json:"answered_by_kind,omitempty"`
	OperatorID     string `json:"operator_id,omitempty"`

	// LaunchID is the question the answer was given to, by the job that
	// asked it, and AskedAt when it was put.
	LaunchID string    `json:"launch_id,omitempty"`
	AskedAt  time.Time `json:"asked_at,omitzero"`

	// Question, QuestionContext and Decision are what the run asked, as
	// its row held them ([ExecutorAsked]), and
	// AudienceHandles whom it was put to: the one record of the question
	// that outlives the run, read back as a person's answered decision
	// (ADR-0068).
	Question        string             `json:"question,omitempty"`
	QuestionContext string             `json:"question_context,omitempty"`
	Decision        *decision.Decision `json:"decision,omitempty"`
	AudienceHandles []string           `json:"audience_handles,omitempty"`

	// Choice is the option the person chose, by id, and Answer their own
	// words, redacted; either may be empty, never both on an answer by
	// turn. Empty on a reply [AnswerTooLarge], whose words are the very thing
	// no record of the run could carry, and which the run was never given.
	Choice string `json:"choice,omitempty"`
	Answer string `json:"answer,omitempty"`

	// AnswerBytes is what an answer [AnswerTooLarge] came to, and
	// AnswerMaxBytes what an answer may, on the measure it was refused on:
	// a chat reply as the run's record would hold it, and an answer by turn
	// by what it says, trimmed, before any secret in it is redacted — or,
	// where the record's own share refused it after that, as the record would
	// hold it, which no answer within its words' bound reaches. The two
	// figures the record gives its reader; absent on every other outcome.
	AnswerBytes    int `json:"answer_bytes,omitempty"`
	AnswerMaxBytes int `json:"answer_max_bytes,omitempty"`
}

// EventType is the "executor_answered" wire type.
func (ExecutorAnswered) EventType() string { return "executor_answered" }

// Role is the seat whose run was answered.
func (e ExecutorAnswered) Role() string { return e.RoleName }

// AgentID is the instance whose run was answered.
func (e ExecutorAnswered) AgentID() string { return e.Agent }

// SummaryFor says what the answer became, which is the one thing a reader of
// the feed wants from it.
func (e ExecutorAnswered) SummaryFor(actor string) string {
	switch e.Outcome {
	case AnswerResumed:
		return lead(actor, "resumed its executor with an answer")
	case AnswerNotAwaiting:
		return lead(actor, "was answered, but its executor was not waiting")
	case AnswerGone:
		return lead(actor, "was answered after its executor had ended")
	case AnswerDeclined:
		if e.Via == AnswerViaChat && e.AnswerBytes > 0 {
			// A REFUSED REPLY LET GO OF: its too_large record said the
			// executor would be told, and it never was.
			return lead(actor, fmt.Sprintf("was answered with a reply of %s, too large for its "+
				"executor to take, and the executor could not be resumed told so: the reply goes back "+
				"to the seat as the message it was, and the question waits for an answer again",
				kept.SizeUp(e.AnswerBytes)))
		}
		return lead(actor, "was answered, but its executor could not carry on with the answer")
	case AnswerUnoffered:
		return lead(actor, "was answered with a choice its executor's question does not offer")
	case AnswerTooLarge:
		// WHAT IT CAME TO ROUNDED UP ([kept.SizeUp]), beside the bound in
		// [kept.Size]'s: rounded down like the bound, a reply one byte past
		// 512 KiB read "512 KiB, past the 512 KiB".
		if e.Via == AnswerViaOperator {
			return lead(actor, fmt.Sprintf("was answered with an answer of %s, past the %s an "+
				"answer may be; the question is still waiting for a shorter one",
				kept.SizeUp(e.AnswerBytes), kept.Size(e.AnswerMaxBytes)))
		}
		// WHAT IS RECORDED, never what has not happened yet: the reply was
		// refused as the question's outcome, and the executor is to be
		// resumed told so — a let-go before any turn told it says so in a
		// `declined` record of its own.
		return lead(actor, fmt.Sprintf("was answered with a reply of %s, past the %s its executor's "+
			"record can carry as an answer, so the reply was refused; its executor is resumed told it "+
			"did not reach it, to ask again for less or carry on without it",
			kept.SizeUp(e.AnswerBytes), kept.Size(e.AnswerMaxBytes)))
	}
	return lead(actor, "was answered")
}
