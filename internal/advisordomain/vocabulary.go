package advisordomain

import "slices"

// THE CLOSED WORDS a finding, a run and an alarm are recorded in. The writer
// refuses any other; the applier stores what the record says as text, so a
// reader of an older build reads a newer word as a value, not a panic.

// Severity is how much a finding matters: it orders the list and decides
// whether a new episode breaks a key's hold.
type Severity string

// The three severities.
const (
	SeverityBlocks Severity = "blocks"
	SeverityCosts  Severity = "costs"
	SeverityNote   Severity = "note"
)

// Severities are the three, the gravest first.
var Severities = []Severity{SeverityBlocks, SeverityCosts, SeverityNote}

// Valid reports whether a severity is one this build knows.
func (s Severity) Valid() bool { return slices.Contains(Severities, s) }

// Rank is the severity's weight, `blocks` highest; zero for an unknown word.
func (s Severity) Rank() int {
	if i := slices.Index(Severities, s); i >= 0 {
		return len(Severities) - i
	}
	return 0
}

// Source is what found a finding: a detector's rule, or the model pass.
type Source string

// The two sources.
const (
	SourceRule  Source = "rule"
	SourceModel Source = "model"
)

// Sources are the two.
var Sources = []Source{SourceRule, SourceModel}

// Valid reports whether a source is one this build knows.
func (s Source) Valid() bool { return slices.Contains(Sources, s) }

// Actor is who has to act on a finding: the company's owner, or the engine.
type Actor string

// The two actors.
const (
	ActorOwner  Actor = "owner"
	ActorEngine Actor = "engine"
)

// Actors are the two.
var Actors = []Actor{ActorOwner, ActorEngine}

// Valid reports whether an actor is one this build knows.
func (a Actor) Valid() bool { return slices.Contains(Actors, a) }

// State is where an episode is. `open` and `applied` are non-closed; the other
// four are closed, and closed is terminal.
type State string

// The six states.
const (
	StateOpen      State = "open"
	StateApplied   State = "applied"
	StateResolved  State = "resolved"
	StateRetired   State = "retired"
	StateExpired   State = "expired"
	StateDismissed State = "dismissed"
)

// States are the six, the non-closed first.
var States = []State{StateOpen, StateApplied, StateResolved, StateRetired, StateExpired, StateDismissed}

// Valid reports whether a state is one this build knows.
func (s State) Valid() bool { return slices.Contains(States, s) }

// Closed reports a terminal state.
func (s State) Closed() bool { return s.Valid() && s != StateOpen && s != StateApplied }

// CloseReason is why an episode closed, from a closed set each closed state
// draws its own from ([CloseReason.Closes]).
type CloseReason string

// The nine close reasons.
const (
	ReasonConditionCleared CloseReason = "condition_cleared"
	ReasonFixedByThis      CloseReason = "fixed_by_this"
	ReasonFixedElsewhere   CloseReason = "fixed_elsewhere"
	ReasonSubjectGone      CloseReason = "subject_gone"
	ReasonFixUnbuildable   CloseReason = "fix_unbuildable"
	ReasonNotConfirmed     CloseReason = "not_confirmed"
	ReasonDismissedWontFix CloseReason = "dismissed_wont_fix"
	ReasonDismissedWrong   CloseReason = "dismissed_wrong"
	ReasonDismissedKnown   CloseReason = "dismissed_known"
	// ReasonDuplicate is a model finding a rule's finding says too, with
	// its numbers (ADR-0166).
	ReasonDuplicate CloseReason = "duplicate"
	// ReasonReplaced is a model finding whose premise moved while what it
	// was about still stands, now said by a rule's truer finding (ADR-0166).
	ReasonReplaced CloseReason = "replaced"
)

// closeReasons is each closed state's reasons.
var closeReasons = map[State][]CloseReason{
	StateResolved:  {ReasonConditionCleared, ReasonFixedByThis, ReasonFixedElsewhere},
	StateRetired:   {ReasonSubjectGone, ReasonFixUnbuildable, ReasonDuplicate, ReasonReplaced},
	StateExpired:   {ReasonNotConfirmed},
	StateDismissed: {ReasonDismissedWontFix, ReasonDismissedWrong, ReasonDismissedKnown},
}

// CloseReasons are every reason, by the state they close into.
var CloseReasons = []CloseReason{
	ReasonConditionCleared, ReasonFixedByThis, ReasonFixedElsewhere, ReasonSubjectGone,
	ReasonFixUnbuildable, ReasonNotConfirmed, ReasonDismissedWontFix, ReasonDismissedWrong,
	ReasonDismissedKnown, ReasonDuplicate, ReasonReplaced,
}

// Valid reports whether a reason is one this build knows.
func (r CloseReason) Valid() bool { return slices.Contains(CloseReasons, r) }

// Closes reports whether the reason is one state closes with.
func (r CloseReason) Closes(state State) bool { return slices.Contains(closeReasons[state], r) }

// Event is one line of an episode's history: a [Move]'s event, or what the
// other ops on a finding record.
type Event string

// The history events.
const (
	EventOpened      Event = "opened"
	EventRevised     Event = "revised"
	EventApplied     Event = "applied"
	EventIneffective Event = "ineffective"
	EventSnoozed     Event = "snoozed"
	EventUnsnoozed   Event = "unsnoozed"
	EventUseful      Event = "useful"
	EventClosed      Event = "closed"
	EventExplained   Event = "explained"
	EventReleased    Event = "released"
)

// Events are every history line, in the order an episode meets them.
var Events = []Event{
	EventOpened, EventRevised, EventApplied, EventIneffective, EventSnoozed, EventUnsnoozed,
	EventUseful, EventClosed, EventExplained, EventReleased,
}

// MoveEvents are the events a [Move] carries.
var MoveEvents = []Event{
	EventApplied, EventIneffective, EventSnoozed, EventUnsnoozed, EventClosed, EventUseful,
}

// Valid reports whether an event is one this build knows.
func (e Event) Valid() bool { return slices.Contains(Events, e) }

// Trigger is what started a run.
type Trigger string

// The three triggers.
const (
	TriggerSchedule Trigger = "schedule"
	TriggerEvent    Trigger = "event"
	TriggerPerson   Trigger = "person"
)

// Triggers are the three.
var Triggers = []Trigger{TriggerSchedule, TriggerEvent, TriggerPerson}

// Valid reports whether a trigger is one this build knows.
func (t Trigger) Valid() bool { return slices.Contains(Triggers, t) }

// RunStatus is how a run ended.
type RunStatus string

// The four ways a run ends.
const (
	RunDone    RunStatus = "done"
	RunFailed  RunStatus = "failed"
	RunCeiling RunStatus = "ceiling"
	// RunInterrupted is a deep run whose holder stopped during it: it
	// concluded nothing, and the next run starts fresh.
	RunInterrupted RunStatus = "interrupted"
)

// RunStatuses are the four.
var RunStatuses = []RunStatus{RunDone, RunFailed, RunCeiling, RunInterrupted}

// Valid reports whether a status is one this build knows.
func (s RunStatus) Valid() bool { return slices.Contains(RunStatuses, s) }

// ClassStatus is how one class fared in a run: `done` is the one that
// confirms its findings ([Reader.LastRunOfClasses]).
type ClassStatus string

// The three ways a class fares.
const (
	ClassDone   ClassStatus = "done"
	ClassUnread ClassStatus = "unread"
	ClassFailed ClassStatus = "failed"
)

// ClassStatuses are the three.
var ClassStatuses = []ClassStatus{ClassDone, ClassUnread, ClassFailed}

// Valid reports whether a status is one this build knows.
func (s ClassStatus) Valid() bool { return slices.Contains(ClassStatuses, s) }

// RunKind is what a run does: its rules (with the daily model pass where one
// runs), or an investigation on the Advisor's model (ADR-0166).
type RunKind string

// The kinds; the empty kind is a run of the rules, as every run before kinds was.
const (
	RunKindRules RunKind = "rules"
	RunKindDeep  RunKind = "deep"
)

// RunKinds is every kind.
var RunKinds = []RunKind{RunKindRules, RunKindDeep}

// Valid reports whether k is a kind, the empty one included.
func (k RunKind) Valid() bool { return k == "" || slices.Contains(RunKinds, k) }

// OrRules is k, or the rules where it names none.
func (k RunKind) OrRules() RunKind {
	if k == "" {
		return RunKindRules
	}
	return k
}
