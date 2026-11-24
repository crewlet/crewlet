package advisor

import (
	"bytes"
	"cmp"
	"encoding/json"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// THE LIFECYCLE OF A STORED FINDING (ADR-0164): every run of a class re-derives
// every finding of that class, so the list says what is true now rather than
// what was true when somebody last looked. [Reconcile] decides, from what is
// stored and what one run observed, what the store records; it reads and writes
// nothing itself, and the duty publishes each [Transition] under the version
// it was decided from.

// State is where a stored finding is.
type State string

// The states: open and applied are live; the rest are closed and terminal.
const (
	StateOpen      State = "open"
	StateApplied   State = "applied"
	StateResolved  State = "resolved"
	StateRetired   State = "retired"
	StateExpired   State = "expired"
	StateDismissed State = "dismissed"
)

// States is every state.
func States() []State {
	return []State{StateOpen, StateApplied, StateResolved, StateRetired, StateExpired, StateDismissed}
}

// Valid reports whether s is a state this build knows.
func (s State) Valid() bool { return slices.Contains(States(), s) }

// Closed reports whether s ends the finding's episode.
func (s State) Closed() bool { return s.Valid() && s != StateOpen && s != StateApplied }

// CloseReason is why an episode closed.
type CloseReason string

// The reasons.
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
	// ReasonDuplicate is a model finding a rule's finding says too.
	ReasonDuplicate CloseReason = "duplicate"
	// ReasonReplaced is a model finding whose premise moved while what it
	// was about still stands, now said by a rule's truer finding.
	ReasonReplaced CloseReason = "replaced"
)

// CloseReasons is every reason.
func CloseReasons() []CloseReason {
	return []CloseReason{ReasonConditionCleared, ReasonFixedByThis, ReasonFixedElsewhere, ReasonSubjectGone,
		ReasonFixUnbuildable, ReasonNotConfirmed, ReasonDismissedWontFix, ReasonDismissedWrong, ReasonDismissedKnown,
		ReasonDuplicate, ReasonReplaced}
}

// Valid reports whether r is a reason this build knows.
func (r CloseReason) Valid() bool { return slices.Contains(CloseReasons(), r) }

// HoldFor is how long a dismissal holds its key quiet: a fix nobody will make
// for a quarter, one already known for a month, and one that was wrong for
// good, so it is never raised again (forGood). A severity that rises raises
// the key again early.
func HoldFor(reason CloseReason) (span time.Duration, forGood bool) {
	switch reason {
	case ReasonDismissedWontFix:
		return 90 * 24 * time.Hour, false
	case ReasonDismissedKnown:
		return 30 * 24 * time.Hour, false
	}
	return 0, reason == ReasonDismissedWrong
}

// SnoozeFor is how long a snoozed finding stays off the list before it is
// shown again, if a run still observes it.
const SnoozeFor = 7 * 24 * time.Hour

// Group is a family of classes run together on one cadence, from one reading.
type Group string

// The groups: the setup (the document, the integrations, the seats' pauses and
// the atlas), on every change of it and hourly; the evidence of what the
// company did over the week, hourly; and the model's findings, once a day.
const (
	GroupSetup    Group = "setup"
	GroupEvidence Group = "evidence"
	GroupModel    Group = "model"
)

// Groups is every group, in the order a run takes them.
func Groups() []Group { return []Group{GroupSetup, GroupEvidence, GroupModel} }

// Valid reports whether g is a group this build runs.
func (g Group) Valid() bool { return slices.Contains(Groups(), g) }

// Interval is how often g runs when nothing changes. The setup's hourly run is a
// backstop: a change to it runs it at once. The evidence classes read a
// week, so an hour moves their counts by a little; the model's pass is daily.
func (g Group) Interval() time.Duration {
	if g == GroupModel {
		return 24 * time.Hour
	}
	return time.Hour
}

// GroupOf is the group f's class runs in.
func GroupOf(f Finding) Group {
	switch {
	case f.Source == SourceModel:
		return GroupModel
	case f.Source == SourceEvidence && !slices.Contains(PausedClasses(), f.Class):
		return GroupEvidence
	}
	return GroupSetup
}

// StaleAfter is how many of its group's intervals a finding goes unconfirmed
// before it is shown as stale, and ExpireAfter how long it then stays stale
// before it is closed as not confirmed: three missed runs is a failing source
// or a stopped duty, not a slow hour; a week is long enough for an operator
// to have seen the list say so.
const (
	StaleAfter  = 3
	ExpireAfter = 7 * 24 * time.Hour
)

// Stale reports whether a finding of g last confirmed at confirmed is stale at now.
func Stale(g Group, confirmed, now time.Time) bool {
	return now.Sub(confirmed) > StaleAfter*g.Interval()
}

// Stored is a finding as the store keeps it.
type Stored struct {
	ID          string
	Version     uint64
	State       State
	Revision    int
	EffectDueAt time.Time
	// AppliedFix is the option an applied finding's person sent, by its digest.
	AppliedFix string
	// Ineffective are the options applied for its key that did not clear
	// it, by digest: never offered again ([WithoutIneffective]).
	Ineffective []string
	// Reason, ClosedAt and ClosedBy say how a closed episode ended, and who
	// closed it where a person did.
	Reason   CloseReason
	ClosedAt time.Time
	ClosedBy string
	// OpenedAt is when the episode opened.
	OpenedAt time.Time
	// VerdictDigest is what the episode decides now, by [VerdictDigest], and
	// ExplainedDigest the verdict the model's explanation was written for:
	// an explanation of another verdict is not shown.
	VerdictDigest, ExplainedDigest string
	// Observation is what the episode's newest record observed.
	Observation Observation
	// Body is the observation as the store keeps it, compared byte for byte.
	Body json.RawMessage
}

// Reading is what one run concluded about: the groups it ran, and the sources
// it could not read, whose classes it says nothing about.
type Reading struct {
	At     time.Time
	Groups []Group
	Unread []string
	// Held reports whether the company still has the subject; nil holds every one.
	Held func(Subject) bool
	// LastDone is when a run last concluded about each scope ([ScopeOf]), for
	// the findings this run says nothing about.
	LastDone map[string]time.Time
	// Partial is a reading cut short by its bound that listed what it had:
	// what it observed opens or refreshes, and what it did not observe stays
	// as it is rather than closing. Cuts is the same for the findings each
	// cut record may have dropped alone ([Answer.Partial], [Cut.Holds]).
	Partial bool
	Cuts    []Cut
}

// holdsOpen reports whether r, not observing f, says nothing of it: r was cut
// short, or a cut record may have dropped f.
func (r Reading) holdsOpen(f Finding) bool {
	return r.Partial || slices.ContainsFunc(r.Cuts, func(c Cut) bool { return c.Holds(f) })
}

// Concludes reports whether r says anything about f's class: its group ran,
// and the source its class reads was read.
func (r Reading) Concludes(f Finding) bool {
	if !slices.Contains(r.Groups, GroupOf(f)) {
		return false
	}
	word, ok := unreadWordOf(f)
	return !ok || !slices.Contains(r.Unread, word)
}

// Done is every scope r concluded about.
func (r Reading) Done() []string {
	var out []string
	for _, g := range r.Groups {
		for _, scope := range ScopesOf(g) {
			if _, word, ok := cutScope(scope); !ok || !slices.Contains(r.Unread, word) {
				out = append(out, scope)
			}
		}
	}
	return out
}

// Step is what the store records for one finding.
type Step string

// The steps.
const (
	// StepOpen starts an episode of a key no live finding holds.
	StepOpen Step = "open"
	// StepRevise records a verdict or a fix that moved, as a new revision.
	StepRevise Step = "revise"
	// StepRefresh records new numbers under the same verdict and fix.
	StepRefresh Step = "refresh"
	// StepClose ends the episode with State and Reason.
	StepClose Step = "close"
	// StepIneffective returns an applied finding to open: its fix did not clear it.
	StepIneffective Step = "ineffective"
)

// Transition is one step [Reconcile] asks of the store.
type Transition struct {
	Step Step
	// Stored is the finding the step is about, nil for StepOpen.
	Stored *Stored
	// Observed is what this run observed, nil where it observed nothing.
	Observed *Observation
	State    State
	Reason   CloseReason
	// Moved is each fact that moved since the stored observation.
	Moved []Moved
}

// Moved is one number or word that moved, as the finding's history says it.
type Moved struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Was   any    `json:"was"`
	Now   any    `json:"now"`
}

// Reconcile is every step that makes stored say what r observed. A finding r
// does not conclude about is left as it is, unless its group has not run whole
// for [StaleAfter] intervals and [ExpireAfter] more, when it expires.
func Reconcile(stored []Stored, observed []Observation, r Reading) []Transition {
	held := r.Held
	if held == nil {
		held = func(Subject) bool { return true }
	}
	live := map[string]*Stored{}
	for i := range stored {
		if !stored[i].State.Closed() {
			live[stored[i].Observation.Key()] = &stored[i]
		}
	}
	var out []Transition
	seen := map[string]bool{}
	for i := range observed {
		o := &observed[i]
		key := o.Key()
		if seen[key] || !r.Concludes(o.Finding) {
			continue
		}
		seen[key] = true
		s, ok := live[key]
		if !ok {
			out = append(out, Transition{Step: StepOpen, Observed: o, State: StateOpen})
			continue
		}
		kept := *o
		kept.Finding = WithoutIneffective(withWordsOf(o.Finding, s.Observation.Finding), s.Ineffective)
		if t, ok := again(s, &kept, r.At); ok {
			out = append(out, t)
		}
	}
	for _, key := range sortedKeys(live) {
		s := live[key]
		f := s.Observation.Finding
		switch {
		case seen[key]:
		case r.holdsOpen(f) && r.Concludes(f):
		case !r.Concludes(f):
			if last, ok := r.LastDone[ScopeOf(f)]; ok && r.At.Sub(last) > StaleAfter*GroupOf(f).Interval()+ExpireAfter {
				out = append(out, Transition{Step: StepClose, Stored: s, State: StateExpired, Reason: ReasonNotConfirmed})
			}
		case !held(f.Subject):
			out = append(out, Transition{Step: StepClose, Stored: s, State: StateRetired, Reason: ReasonSubjectGone})
		case s.State == StateApplied:
			out = append(out, Transition{Step: StepClose, Stored: s, State: StateResolved, Reason: ReasonFixedByThis})
		default:
			out = append(out, Transition{Step: StepClose, Stored: s, State: StateResolved, Reason: ReasonConditionCleared})
		}
	}
	return out
}

// withWordsOf is o keeping every word of was (its headline, title, why and
// fix sentence and link) where its record cut o's words ([Finding.WordsCut]):
// a cut is no change in the condition, so it records nothing and takes no
// word or link away.
func withWordsOf(o, was Finding) Finding {
	if o.WordsCut {
		o.Headline, o.Title, o.Why = was.Headline, was.Title, was.Why
		o.Fix.Sentence, o.Fix.URL = was.Fix.Sentence, was.Fix.URL
	}
	return o
}

// again is the step a live finding observed again takes, false for none.
func again(s *Stored, o *Observation, now time.Time) (Transition, bool) {
	was := s.Observation.Finding
	moved := MovedFacts(was.Evidence, o.Finding.Evidence)
	switch {
	case s.State == StateApplied && !s.EffectDueAt.IsZero() && !now.Before(s.EffectDueAt):
		return Transition{Step: StepIneffective, Stored: s, Observed: o, State: StateOpen, Moved: moved}, true
	case VerdictDigest(was) != VerdictDigest(o.Finding) || FixDigest(was.Fix) != FixDigest(o.Finding.Fix):
		return Transition{Step: StepRevise, Stored: s, Observed: o, State: s.State,
			Moved: append(moved, movedVerdict(was, o.Finding)...)}, true
	}
	body, err := json.Marshal(o)
	if err == nil && bytes.Equal(body, s.Body) {
		return Transition{}, false
	}
	return Transition{Step: StepRefresh, Stored: s, Observed: o, State: s.State, Moved: moved}, true
}

// MovedFacts is every fact whose value differs between was and now, by key,
// with a fact only one side holds as moved from or to nothing.
func MovedFacts(was, now []Fact) []Moved {
	before := map[string]int64{}
	for _, f := range was {
		before[f.Key] = f.Value
	}
	var out []Moved
	after := map[string]bool{}
	for _, f := range now {
		after[f.Key] = true
		if v, ok := before[f.Key]; !ok || v != f.Value {
			m := Moved{Key: f.Key, Label: FactLabel(f.Key), Now: f.Value}
			if ok {
				m.Was = v
			}
			out = append(out, m)
		}
	}
	for _, f := range was {
		if !after[f.Key] {
			out = append(out, Moved{Key: f.Key, Label: FactLabel(f.Key), Was: f.Value})
		}
	}
	slices.SortFunc(out, func(a, b Moved) int { return cmp.Compare(a.Key, b.Key) })
	return out
}

// movedVerdict is the severity and the fix's sentence, where they moved.
func movedVerdict(was, now Finding) []Moved {
	var out []Moved
	if was.Severity != now.Severity {
		out = append(out, Moved{Key: "severity", Label: "Severity", Was: string(was.Severity), Now: string(now.Severity)})
	}
	if FixDigest(was.Fix) != FixDigest(now.Fix) {
		out = append(out, Moved{Key: "fix", Label: "Fix", Was: was.Fix.Sentence, Now: now.Fix.Sentence})
	}
	return out
}

// SubjectHeld reports whether c still has s: a seat, a seat's or a unit's
// schedule. Every other kind is the company's own or read elsewhere, so held.
func SubjectHeld(c *config.Company, s Subject) bool {
	switch s.Kind {
	case SubjectModel:
		_, ok := c.Providers.LLM[s.Name]
		return ok
	case SubjectSeat, SubjectGroup:
		role, _ := roleOf(c, s.Seat)
		return role != nil
	case SubjectSchedule:
		if s.Seat != "" {
			role, _ := roleOf(c, s.Seat)
			return role != nil && slices.ContainsFunc(role.Schedules, func(x org.Schedule) bool { return x.Name == s.Schedule })
		}
		for unit := range c.EachUnit() {
			if unit.ID == s.Unit {
				return slices.ContainsFunc(unit.Schedules, func(x org.Schedule) bool { return x.Name == s.Schedule })
			}
		}
		return false
	}
	return true
}

func sortedKeys(m map[string]*Stored) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
