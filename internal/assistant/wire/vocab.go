package wire

import "slices"

// ProtocolVersion is the version of this vocabulary every frame and every
// request carries. See the package doc for how it moves.
const ProtocolVersion = 1

// The bounds the dashboard holds a person to before anything is sent, so a
// limit is met where somebody is typing rather than as a refusal after the
// press. Each is mirrored in `contract/assistant.ts` and held equal by
// TestTheDashboardBoundsTheAssistantAtTheEnginesLimits.
const (
	// InputMaxRunes is the longest question a turn takes, in Unicode code
	// points. Four thousand: a question is a sentence or a paragraph, and a
	// pasted document belongs in a page the assistant can read through a
	// tool, where it is fenced, rather than in the prompt as the person's
	// own words.
	InputMaxRunes = 4000

	// PageContextMaxBytes is the most a turn's page context may encode to.
	// Sixteen kibibytes: fifty resource ids with their filters take about
	// four, and an open draft's snapshot ([PageDraft]) the rest, which holds
	// a brand kit's every field with room over and forty fields averaging
	// three hundred bytes. A draft past it sheds fields from its end, each
	// counted in its `more`, rather than the turn being refused.
	PageContextMaxBytes = 16 << 10

	// PageContextMaxResources is the most resources a page context names.
	PageContextMaxResources = 50

	// PageContextMaxAccepts is the most page actions a page context offers.
	PageContextMaxAccepts = 32

	// PageDraftMaxFields is the most fields an open draft's snapshot lists;
	// the rest are counted in its `more`. Forty: the longest form a screen
	// draws (a brand kit's, at fifteen) with room over, and few enough that
	// the model reads each one.
	PageDraftMaxFields = 40

	// PageDraftValueMaxRunes is the longest value a draft field carries, in
	// Unicode code points. A longer value is cut there and marked
	// `truncated`: a field is a name, a sentence or a short list, and a long
	// body is read in full through the tool that reads its record.
	PageDraftValueMaxRunes = 1000

	// PageDraftHelpMaxRunes is the longest help a screen gives with its
	// draft: what the assistant can and cannot do there, in a few sentences.
	PageDraftHelpMaxRunes = 600

	// AuditReasonMaxRunes is the longest reason an administrator gives for a
	// compliance read of somebody else's conversation (D06).
	AuditReasonMaxRunes = 500

	// KeepaliveSeconds is how often a quiet stream writes a comment line.
	// Fifteen seconds sits well under the sixty-second idle timeout common
	// proxies apply, and the dashboard treats two missed keepalives as a
	// stalled stream and resumes from its last id.
	KeepaliveSeconds = 15
)

// FrameMaxBytes is the largest frame the encoder writes and the decoder
// reads: the per-turn frame log's whole byte budget, because a snapshot frame
// carries a turn's messages, steps and cards at once. A frame past it is a
// turn the orchestrator failed to bound, refused rather than cut.
const FrameMaxBytes = 4 << 20

// ActionContextInvalidate is the reserved [ActionEvent] name the server sends
// after a write lands, so open screens raise their read floor to the write's
// position. No screen may declare a page action under it.
const ActionContextInvalidate = "context.invalidate"

// EventKind names a frame on the live stream.
type EventKind string

// The ten kinds a stream carries.
const (
	// EventConversation opens every stream: which conversation, and
	// whether this stream resumes one already drawn.
	EventConversation EventKind = "conversation"
	// EventPageContext says what the turn was told about the screen, or
	// that the person sent it without the page.
	EventPageContext EventKind = "page_context"
	// EventStatus moves the run chip.
	EventStatus EventKind = "status"
	// EventDelta appends prose to one assistant message.
	EventDelta EventKind = "delta"
	// EventStep reports one tool step, at its start and at its end.
	EventStep EventKind = "step"
	// EventCard carries one engine-built card.
	EventCard EventKind = "card"
	// EventAction is an instruction for the page: a page action to apply,
	// or the reserved [ActionContextInvalidate].
	EventAction EventKind = "action"
	// EventDone ends a turn segment, always last.
	EventDone EventKind = "done"
	// EventError is a turn-level failure the person reads, always followed
	// by a done.
	EventError EventKind = "error"
	// EventSnapshot replaces what a resuming client drew, when the frame
	// log cannot serve the frames it missed.
	EventSnapshot EventKind = "snapshot"
)

// EventKinds are the ten, in declaration order.
var EventKinds = []EventKind{
	EventConversation, EventPageContext, EventStatus, EventDelta, EventStep,
	EventCard, EventAction, EventDone, EventError, EventSnapshot,
}

// Valid reports whether k is a kind this build knows.
func (k EventKind) Valid() bool { return slices.Contains(EventKinds, k) }

// TurnState is where a turn stands: on its row, on the done frame that ends a
// segment, and on a conversation's summary.
type TurnState string

const (
	// TurnRunning is a turn whose loop is running on some node.
	TurnRunning TurnState = "running"
	// TurnWaitingForApproval is a turn parked on a proposal: its loop has
	// stopped with a call unanswered and holds no grant.
	TurnWaitingForApproval TurnState = "waiting_for_approval"
	// TurnCompleted answered.
	TurnCompleted TurnState = "completed"
	// TurnStopped was stopped by the person; nothing was changed by it.
	TurnStopped TurnState = "stopped"
	// TurnFailed ended on an error the person reads.
	TurnFailed TurnState = "failed"
	// TurnInterrupted lost its running node, and was closed by the first
	// node to find it so.
	TurnInterrupted TurnState = "interrupted"
	// TurnExpired was parked on a proposal nobody decided in time.
	TurnExpired TurnState = "expired"
	// TurnBudgetExhausted was refused a round by the company's or the
	// person's budget window.
	TurnBudgetExhausted TurnState = "budget_exhausted"
	// TurnSuperseded was parked on a proposal when the person asked the
	// conversation's next question. Its loop never resumes; its proposals
	// stay approvable until they expire, and one approved later is applied
	// and shown on its card without the loop.
	TurnSuperseded TurnState = "superseded"
)

// TurnStates are the nine, in declaration order.
var TurnStates = []TurnState{
	TurnRunning, TurnWaitingForApproval, TurnCompleted, TurnStopped, TurnFailed,
	TurnInterrupted, TurnExpired, TurnBudgetExhausted, TurnSuperseded,
}

// Valid reports whether s is a state this build knows.
func (s TurnState) Valid() bool { return slices.Contains(TurnStates, s) }

// Ended reports whether a segment of the turn is over: every state but
// running, which is what a done frame may carry.
func (s TurnState) Ended() bool { return s.Valid() && s != TurnRunning }

// Final reports whether the turn is over for good: every ended state but
// waiting for an approval, which resumes.
func (s TurnState) Final() bool { return s.Ended() && s != TurnWaitingForApproval }

// StatusState is the run chip's state while a segment runs.
type StatusState string

const (
	// StatusThinking is a round in flight with no step yet.
	StatusThinking StatusState = "thinking"
	// StatusWorking is a turn running its steps.
	StatusWorking StatusState = "working"
	// StatusWaitingForApproval is a turn parked on a proposal.
	StatusWaitingForApproval StatusState = "waiting_for_approval"
	// StatusApplying is an approved proposal executing.
	StatusApplying StatusState = "applying"
)

// StatusStates are the four, in declaration order.
var StatusStates = []StatusState{
	StatusThinking, StatusWorking, StatusWaitingForApproval, StatusApplying,
}

// Valid reports whether s is a state this build knows.
func (s StatusState) Valid() bool { return slices.Contains(StatusStates, s) }

// StepState is one tool step's state.
type StepState string

const (
	// StepRunning was sent and has not answered.
	StepRunning StepState = "running"
	// StepDone answered.
	StepDone StepState = "done"
	// StepFailed answered with a failure that is not a refusal.
	StepFailed StepState = "failed"
	// StepRefused was refused by the person's own access, by the
	// assistant's scope, or by the catalogue.
	StepRefused StepState = "refused"
	// StepStopped was cut by a Stop before it answered.
	StepStopped StepState = "stopped"
	// StepWaiting is a write or a page change waiting on the person's
	// decision of its proposal.
	StepWaiting StepState = "waiting"
)

// StepStates are the six, in declaration order.
var StepStates = []StepState{
	StepRunning, StepDone, StepFailed, StepRefused, StepStopped, StepWaiting,
}

// Valid reports whether s is a state this build knows.
func (s StepState) Valid() bool { return slices.Contains(StepStates, s) }

// CardKind names an engine-built card.
type CardKind string

const (
	// CardTable is rows from a read, with its downloads.
	CardTable CardKind = "table"
	// CardEntity is one record, or a list of records.
	CardEntity CardKind = "entity"
	// CardProposal is a change waiting on the person's decision.
	CardProposal CardKind = "proposal"
	// CardOpenInApp is a link to a screen the person may open.
	CardOpenInApp CardKind = "open_in_app"
	// CardError is a refusal or a failure, in the engine's own words.
	CardError CardKind = "error"
	// CardNotice is a line the engine adds: a shortened table, a withheld
	// result.
	CardNotice CardKind = "notice"
	// CardOversize is a read whose answer was past what one step reads:
	// named for what it read, with the screen that shows all of it and what
	// would narrow it, never drawn as though it held the answer (ADR-0061).
	CardOversize CardKind = "oversize"

	// --- deep work (im-deep) ---

	// CardFile is the files a deep run delivered (ADR-0054).
	CardFile CardKind = "file"

	// CardBrief is a brief turn's answer ([TurnBrief]): what is going on,
	// what waits on the person, and one next step, each row a record the
	// engine read and resolved for them.
	CardBrief CardKind = "brief"
)

// CardKinds are the nine, in declaration order.
var CardKinds = []CardKind{
	CardTable, CardEntity, CardProposal, CardOpenInApp, CardError, CardNotice, CardOversize,
	CardFile, CardBrief,
}

// Valid reports whether k is a kind this build knows.
func (k CardKind) Valid() bool { return slices.Contains(CardKinds, k) }

// ProposalState is where a proposal stands (ADR-0051). The legal moves
// between them are internal/assistant/proposal's; the names are here.
type ProposalState string

const (
	// ProposalProposed waits on the person.
	ProposalProposed ProposalState = "proposed"
	// ProposalApproved was approved, by an arbitrated record, and has not
	// been sent.
	ProposalApproved ProposalState = "approved"
	// ProposalExecuting is a server write sent and not yet answered.
	ProposalExecuting ProposalState = "executing"
	// ProposalApplying is a page change sent to the page and not yet
	// reported.
	ProposalApplying ProposalState = "applying"
	// ProposalApplied landed: the act route answered applied or pending,
	// or the page reported it applied.
	ProposalApplied ProposalState = "applied"
	// ProposalFailed was refused when it executed, in the engine's words.
	ProposalFailed ProposalState = "failed"
	// ProposalUnknown was sent and its outcome cannot be confirmed; a
	// Retry sends the same operation again.
	ProposalUnknown ProposalState = "unknown"
	// ProposalDeclined was declined, or its turn was stopped.
	ProposalDeclined ProposalState = "declined"
	// ProposalExpired was decided by nobody before its expiry.
	ProposalExpired ProposalState = "expired"
	// ProposalStale found its object moved since its preview, at approval
	// or on the page.
	ProposalStale ProposalState = "stale"
)

// ProposalStates are the ten, in declaration order.
var ProposalStates = []ProposalState{
	ProposalProposed, ProposalApproved, ProposalExecuting, ProposalApplying,
	ProposalApplied, ProposalFailed, ProposalUnknown, ProposalDeclined,
	ProposalExpired, ProposalStale,
}

// Valid reports whether s is a state this build knows.
func (s ProposalState) Valid() bool { return slices.Contains(ProposalStates, s) }

// Settled reports whether a proposal can move no further: applied, failed,
// declined, expired or stale. Unknown is NOT settled, because a Retry moves it.
func (s ProposalState) Settled() bool {
	switch s {
	case ProposalApplied, ProposalFailed, ProposalDeclined, ProposalExpired, ProposalStale:
		return true
	}
	return false
}

// ProposalScope says what a proposal changes.
type ProposalScope string

const (
	// ScopeServer is a write to the company, executed by the server
	// through the act route as the person.
	ScopeServer ProposalScope = "server"
	// ScopePage is a change to the screen the person has open, applied by
	// the page itself; nothing is saved.
	ScopePage ProposalScope = "page"
)

// ProposalScopes are the two, in declaration order.
var ProposalScopes = []ProposalScope{ScopeServer, ScopePage}

// Valid reports whether s is a scope this build knows.
func (s ProposalScope) Valid() bool { return slices.Contains(ProposalScopes, s) }

// ProposalRisk says how much care a proposal asks of the person approving it.
// Computed by the engine from what the change does, never declared by the
// model.
type ProposalRisk string

const (
	// RiskDestructive is a change that deletes or removes something that
	// is there now: a record, a member of a list (a seat from a unit), or
	// fields of the object it replaces. The card is drawn in the danger
	// tone and asks a deliberate confirmation before its approval.
	RiskDestructive ProposalRisk = "destructive"
)

// ProposalRisks are the risks a card names; a card naming none is an ordinary
// change.
var ProposalRisks = []ProposalRisk{RiskDestructive}

// Valid reports whether r is a risk this build knows.
func (r ProposalRisk) Valid() bool { return slices.Contains(ProposalRisks, r) }

// ChangeKind is one row of a proposal's "What will change" list, in the
// before and after grammar of decision D10.
type ChangeKind string

const (
	// ChangeFact is a plain row: the scope, the target, a consequence.
	ChangeFact ChangeKind = "fact"
	// ChangeChanged is a value that moves from one value to another.
	ChangeChanged ChangeKind = "changed"
	// ChangeAdded is a value that is set where there was none.
	ChangeAdded ChangeKind = "added"
	// ChangeRemoved is a value that is cleared.
	ChangeRemoved ChangeKind = "removed"
	// ChangeText is a long text shown as a word diff, with the whole change
	// behind a disclosure.
	ChangeText ChangeKind = "text"
)

// ChangeKinds are the five, in declaration order.
var ChangeKinds = []ChangeKind{ChangeFact, ChangeChanged, ChangeAdded, ChangeRemoved, ChangeText}

// Valid reports whether k is a kind this build knows.
func (k ChangeKind) Valid() bool { return slices.Contains(ChangeKinds, k) }

// WordOp is what happened to one run of words in a text change.
type WordOp string

const (
	// WordSame is unchanged text.
	WordSame WordOp = "same"
	// WordAdded is inserted text.
	WordAdded WordOp = "added"
	// WordRemoved is deleted text.
	WordRemoved WordOp = "removed"
)

// WordOps are the three, in declaration order.
var WordOps = []WordOp{WordSame, WordAdded, WordRemoved}

// Valid reports whether o is an op this build knows.
func (o WordOp) Valid() bool { return slices.Contains(WordOps, o) }

// ColumnType says how a table card's column is drawn and exported.
type ColumnType string

const (
	// ColumnText is a string.
	ColumnText ColumnType = "text"
	// ColumnNumber is a plain number.
	ColumnNumber ColumnType = "number"
	// ColumnTokens is a token count, drawn compact.
	ColumnTokens ColumnType = "tokens"
	// ColumnDuration is milliseconds, drawn as a duration.
	ColumnDuration ColumnType = "duration"
	// ColumnDatetime is an RFC 3339 instant, drawn on the company's clock.
	ColumnDatetime ColumnType = "datetime"
	// ColumnLabel is a value from one of the engine's closed sets (a
	// status, a priority, an activity, an outcome), carried as the slug the
	// API answers and drawn in the words the dashboard names it by
	// elsewhere, so the drawer and the screens agree. An export keeps the
	// slug.
	ColumnLabel ColumnType = "label"
)

// ColumnTypes are the six, in declaration order.
var ColumnTypes = []ColumnType{ColumnText, ColumnNumber, ColumnTokens, ColumnDuration,
	ColumnDatetime, ColumnLabel}

// Valid reports whether t is a type this build knows.
func (t ColumnType) Valid() bool { return slices.Contains(ColumnTypes, t) }

// Align is a table column's alignment.
type Align string

const (
	// AlignStart is text's alignment.
	AlignStart Align = "start"
	// AlignEnd is a number's.
	AlignEnd Align = "end"
)

// Aligns are the two, in declaration order.
var Aligns = []Align{AlignStart, AlignEnd}

// Valid reports whether a is an alignment this build knows.
func (a Align) Valid() bool { return slices.Contains(Aligns, a) }

// NoticeTone is a notice card's tone.
type NoticeTone string

const (
	// NoticeInfo is a fact the person may want: a truncation.
	NoticeInfo NoticeTone = "info"
	// NoticeWarning is something withheld or missing.
	NoticeWarning NoticeTone = "warning"
)

// NoticeTones are the two, in declaration order.
var NoticeTones = []NoticeTone{NoticeInfo, NoticeWarning}

// Valid reports whether t is a tone this build knows.
func (t NoticeTone) Valid() bool { return slices.Contains(NoticeTones, t) }

// ErrorCode is a turn-level failure on an [ErrorEvent]. A refusal of one
// step is not one of these: it is the step's own refusal, in the API's codes.
type ErrorCode string

const (
	// ErrorModelUnavailable is every model in the assistant's chain
	// failing for a reason none of the codes below names, or its CLI
	// missing or unable to start on the node.
	ErrorModelUnavailable ErrorCode = "model_unavailable"
	// ErrorModelSignedOut is the chain's CLI signed out: its login expired
	// or missing, or its token invalid. Asking again does not help until
	// somebody signs it in.
	ErrorModelSignedOut ErrorCode = "model_signed_out"
	// ErrorModelBusy is the chain's usage plan spent or rate limited, with
	// the reset where the CLI named one.
	ErrorModelBusy ErrorCode = "model_busy"
	// ErrorModelTimeout is the chain's CLI running out of its own time
	// before it answered.
	ErrorModelTimeout ErrorCode = "model_timeout"
	// ErrorTurnTimeout is the turn's wall clock running out.
	ErrorTurnTimeout ErrorCode = "turn_timeout"
	// ErrorStepLimit is the turn using every step without an answer.
	ErrorStepLimit ErrorCode = "step_limit"
	// ErrorBudgetExhausted is a budget window refusing the next round.
	ErrorBudgetExhausted ErrorCode = "budget_exhausted"
	// ErrorTurnInterrupted is the running node gone.
	ErrorTurnInterrupted ErrorCode = "turn_interrupted"
	// ErrorGrantEnded is the person's session ending mid-turn: a sign-out,
	// a revocation, a suspension or a seat move (ADR-0050).
	ErrorGrantEnded ErrorCode = "grant_ended"
	// ErrorInternal is a fault no retry of the question clears.
	ErrorInternal ErrorCode = "internal_error"
	// ErrorAccessWithdrawn is a parked turn that cannot carry on because
	// the person no longer holds an app it read through: what it read may
	// not be handed back to the model for them.
	ErrorAccessWithdrawn ErrorCode = "access_withdrawn"
	// ErrorBriefInvalid is a brief turn whose model did not write a brief
	// the engine could accept within its one retry.
	ErrorBriefInvalid ErrorCode = "brief_invalid"
)

// ErrorCodes are the twelve, in declaration order.
var ErrorCodes = []ErrorCode{
	ErrorModelUnavailable, ErrorModelSignedOut, ErrorModelBusy, ErrorModelTimeout,
	ErrorTurnTimeout, ErrorStepLimit, ErrorBudgetExhausted, ErrorTurnInterrupted,
	ErrorGrantEnded, ErrorInternal, ErrorAccessWithdrawn, ErrorBriefInvalid,
}

// Valid reports whether c is a code this build knows.
func (c ErrorCode) Valid() bool { return slices.Contains(ErrorCodes, c) }

// Sentence is the code's sentence a person reads: what a turn's error frame
// says where nothing about the moment changes it, and what a transcript says
// of a turn that ended with it, long after the moment that may have
// sharpened it (a person's own budget rather than the company's, a CLI that
// cannot start rather than one that did not answer) is gone. Empty for a
// code this build does not know.
func (c ErrorCode) Sentence() string { return errorSentences[c] }

// errorSentences are [ErrorCode.Sentence]'s, one per code.
var errorSentences = map[ErrorCode]string{
	ErrorModelUnavailable: "The assistant's model did not answer.",
	ErrorModelSignedOut:   "The assistant's model is signed out, so it cannot answer.",
	ErrorModelBusy:        "The assistant's model has reached its usage limit for now.",
	ErrorModelTimeout:     "The assistant's model took too long to answer.",
	ErrorTurnTimeout:      "The turn took longer than it may. Ask again, or ask a narrower question.",
	ErrorStepLimit:        "This question needed more steps than a turn may take. Ask a narrower one.",
	ErrorBudgetExhausted:  "The company's token budget for this window is spent.",
	ErrorTurnInterrupted:  "The node answering you restarted before the turn finished.",
	ErrorGrantEnded:       "Your session ended, so the assistant stopped acting for you.",
	ErrorInternal:         "Something went wrong inside the engine. The reason is in this node's log.",
	ErrorAccessWithdrawn: "The assistant stopped: you no longer have access to something this " +
		"conversation read.",
	ErrorBriefInvalid: "The brief could not be written. Ask for it again.",
}

// ResourceKind is what a page context's resource or selection names.
type ResourceKind string

// The kinds a screen can name, each the dashboard's own object kind.
const (
	ResourceTask      ResourceKind = "task"
	ResourceProject   ResourceKind = "project"
	ResourceSeat      ResourceKind = "seat"
	ResourceUnit      ResourceKind = "unit"
	ResourcePage      ResourceKind = "page"
	ResourceContainer ResourceKind = "container"
	ResourceArtifact  ResourceKind = "artifact"
	ResourceTurn      ResourceKind = "turn"
	ResourceRun       ResourceKind = "run"
	ResourceSchedule  ResourceKind = "schedule"
	ResourceView      ResourceKind = "view"
)

// ResourceKinds are the eleven, in declaration order.
var ResourceKinds = []ResourceKind{
	ResourceTask, ResourceProject, ResourceSeat, ResourceUnit, ResourcePage,
	ResourceContainer, ResourceArtifact, ResourceTurn, ResourceRun,
	ResourceSchedule, ResourceView,
}

// Valid reports whether k is a kind this build knows.
func (k ResourceKind) Valid() bool { return slices.Contains(ResourceKinds, k) }

// Keyed reports whether a record of kind k is written in prose by a short key
// of its own (a task's "GOP-11", a project's "GOP"), which its
// [EntityCard.Key] then carries. Every other kind is written by its name, a
// seat's included: the assistant names a seat by its name, never its handle.
func (k ResourceKind) Keyed() bool { return k == ResourceTask || k == ResourceProject }

// RecordNoun is what the person calls the record a screen has open, as the
// drawer names it ("Brief me on this decision"): a selection's word for itself
// beside its [ResourceKind], which says only how the engine reads it. An ask
// is a decision the Inbox shows on a task, and a seat is an agent.
type RecordNoun string

// The nouns a screen names its open record by.
const (
	NounDecision RecordNoun = "decision"
	NounUpdate   RecordNoun = "update"
	NounTask     RecordNoun = "task"
	NounProject  RecordNoun = "project"
	NounAgent    RecordNoun = "agent"
	NounArtifact RecordNoun = "artifact"
	NounPage     RecordNoun = "page"
	NounTurn     RecordNoun = "turn"
	NounRun      RecordNoun = "run"
)

// RecordNouns are the nine, in declaration order.
var RecordNouns = []RecordNoun{
	NounDecision, NounUpdate, NounTask, NounProject, NounAgent, NounArtifact, NounPage,
	NounTurn, NounRun,
}

// Valid reports whether n is a noun this build knows.
func (n RecordNoun) Valid() bool { return slices.Contains(RecordNouns, n) }

// TurnMode is what a question asks for: an answer in the assistant's own
// words, or a brief in a fixed shape ([BriefCard]).
type TurnMode string

const (
	// TurnAsk is an ordinary question, answered in prose and cards.
	TurnAsk TurnMode = "ask"
	// TurnBrief asks for a brief of the page the person is on, or of the
	// one record it has open: read only, answered by one brief card.
	TurnBrief TurnMode = "brief"
)

// TurnModes are the two, in declaration order.
var TurnModes = []TurnMode{TurnAsk, TurnBrief}

// Valid reports whether m is a mode this build knows.
func (m TurnMode) Valid() bool { return slices.Contains(TurnModes, m) }

// BriefTone is how loud a brief's row is.
//
// DANGER IS THE ENGINE'S: a row is drawn danger only when the record it cites
// carries a fact the engine read (a seat stopped, a task marked urgent, a
// failed turn); the model may choose warning or neutral, and a danger it
// chose without such a fact is drawn warning.
type BriefTone string

// The tones a brief's row is drawn in.
const (
	BriefDanger  BriefTone = "danger"
	BriefWarning BriefTone = "warning"
	BriefNeutral BriefTone = "neutral"
)

// BriefTones are the three, in declaration order.
var BriefTones = []BriefTone{BriefDanger, BriefWarning, BriefNeutral}

// Valid reports whether t is a tone this build knows.
func (t BriefTone) Valid() bool { return slices.Contains(BriefTones, t) }

// BriefActionKind is what a brief's action does. A brief never writes: an
// action opens a record's screen or asks a question in the same conversation.
type BriefActionKind string

const (
	// BriefOpen opens the screen of the record a row cites, at the address
	// the engine built and checked the person may open.
	BriefOpen BriefActionKind = "open"
	// BriefAsk asks a question in the same conversation.
	BriefAsk BriefActionKind = "ask"
)

// BriefActionKinds are the two, in declaration order.
var BriefActionKinds = []BriefActionKind{BriefOpen, BriefAsk}

// Valid reports whether k is a kind this build knows.
func (k BriefActionKind) Valid() bool { return slices.Contains(BriefActionKinds, k) }

// ViewerReason is why the assistant is not available to the viewer, the
// first failing condition, so the Ask pill is drawn disabled with a reason
// rather than hidden.
type ViewerReason string

const (
	// ViewerDisabled is a company whose configuration leaves the assistant
	// off.
	ViewerDisabled ViewerReason = "disabled"
	// ViewerNoModel is an enabled assistant whose model chain this node
	// cannot build.
	ViewerNoModel ViewerReason = "no_model"
	// ViewerNotAPerson is a caller that is not a person signed in through
	// a session: a machine token, a Tier A token's session, a service
	// account (D04).
	ViewerNotAPerson ViewerReason = "not_a_person"
	// ViewerNoPermission is a person the authority table refuses the
	// assistant.
	ViewerNoPermission ViewerReason = "no_permission"
	// ViewerModelNotReady is a person who may ask, on a chain every model
	// of which is known unusable right now (signed out, its plan spent, its
	// CLI unable to start), known without a call; the answer's
	// [ViewerAssistant.Model] says which and what to do. Checked after
	// every other reason, so only a person the assistant answers learns
	// the state of its model.
	ViewerModelNotReady ViewerReason = "model_not_ready"
)

// ViewerReasons are the five, in declaration order.
var ViewerReasons = []ViewerReason{
	ViewerDisabled, ViewerNoModel, ViewerNotAPerson, ViewerNoPermission, ViewerModelNotReady,
}

// Valid reports whether r is a reason this build knows.
func (r ViewerReason) Valid() bool { return slices.Contains(ViewerReasons, r) }

// ActionConfirm says whether a page action asks for the person's approval.
type ActionConfirm string

const (
	// ConfirmApprove shows an approval card; the default.
	ConfirmApprove ActionConfirm = "approve"
	// ConfirmNone applies at once with an Undo receipt, for a pure view
	// change the screen itself declares so (D11).
	ConfirmNone ActionConfirm = "none"
)

// ActionConfirms are the two, in declaration order.
var ActionConfirms = []ActionConfirm{ConfirmApprove, ConfirmNone}

// Valid reports whether c is a confirmation this build knows.
func (c ActionConfirm) Valid() bool { return slices.Contains(ActionConfirms, c) }

// ActionState is what the page reports about a page action it was sent, on
// `PUT /assistant/actions/{id}`: a claim first, then one outcome.
type ActionState string

const (
	// ActionApplying claims the action, so a second tab or a retried press
	// cannot apply it twice.
	ActionApplying ActionState = "applying"
	// ActionApplied was applied by the screen's own setters.
	ActionApplied ActionState = "applied"
	// ActionStale found the page's revision moved since it was prepared.
	ActionStale ActionState = "stale"
	// ActionBlocked was refused by the screen's own gate.
	ActionBlocked ActionState = "blocked"
	// ActionInvalid did not parse as the action's own shape.
	ActionInvalid ActionState = "invalid"
	// ActionNoHandler reached a page that no longer offers the action.
	ActionNoHandler ActionState = "no_handler"
)

// ActionStates are the six, in declaration order.
var ActionStates = []ActionState{
	ActionApplying, ActionApplied, ActionStale, ActionBlocked, ActionInvalid,
	ActionNoHandler,
}

// Valid reports whether s is a state this build knows.
func (s ActionState) Valid() bool { return slices.Contains(ActionStates, s) }

// MessageRole is who said a message.
type MessageRole string

const (
	// RoleUser is the person.
	RoleUser MessageRole = "user"
	// RoleAssistant is the assistant.
	RoleAssistant MessageRole = "assistant"
)

// MessageRoles are the two, in declaration order.
var MessageRoles = []MessageRole{RoleUser, RoleAssistant}

// Valid reports whether r is a role this build knows.
func (r MessageRole) Valid() bool { return slices.Contains(MessageRoles, r) }

// OpenReason is why an open in app card may not be opened.
type OpenReason string

const (
	// OpenNotPermitted is a screen or a record the person may not open,
	// never told apart from one that does not exist.
	OpenNotPermitted OpenReason = "not_permitted"
)

// OpenReasons are the one, in declaration order.
var OpenReasons = []OpenReason{OpenNotPermitted}

// Valid reports whether r is a reason this build knows.
func (r OpenReason) Valid() bool { return slices.Contains(OpenReasons, r) }

// DraftKind says where an open draft's values live.
type DraftKind string

const (
	// DraftClient is a draft whose values live in the page until the person
	// saves it: a change to it is a page action the screen applies through
	// its own setters. Its id and revision, when set, name the saved record
	// the page opened it from.
	DraftClient DraftKind = "client"
	// DraftServer is a draft whose every value is saved, at its id and
	// revision: a change to it is a write to that record's route.
	DraftServer DraftKind = "server"
)

// DraftKinds are the two, in declaration order.
var DraftKinds = []DraftKind{DraftClient, DraftServer}

// Valid reports whether k is a draft kind this build knows.
func (k DraftKind) Valid() bool { return slices.Contains(DraftKinds, k) }

// DraftFieldKind is the shape of an open draft's field, and so of the value a
// change to it takes.
type DraftFieldKind string

const (
	// DraftFieldText is one value of text.
	DraftFieldText DraftFieldKind = "text"
	// DraftFieldList is a list, its items one per line in the snapshot and
	// a list of strings in a change.
	DraftFieldList DraftFieldKind = "list"
	// DraftFieldFile is a file the screen keeps (a logo): its snapshot value
	// is the file's name, and a change to it is a [DraftFileRef] to a file
	// of the person's own conversation, never the file's bytes or markup.
	DraftFieldFile DraftFieldKind = "file"
)

// DraftFieldKinds are the three, in declaration order.
var DraftFieldKinds = []DraftFieldKind{DraftFieldText, DraftFieldList, DraftFieldFile}

// Valid reports whether k is a draft field kind this build knows.
func (k DraftFieldKind) Valid() bool { return slices.Contains(DraftFieldKinds, k) }
