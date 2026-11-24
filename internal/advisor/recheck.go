package advisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"
)

// AN APPLY RE-CHECKS FIRST (ADR-0164): a finding may sit while the company
// changes under it, so the engine runs its class again for it now and
// compares what the finding decides and the change it would send with what
// is stored. Only a finding that still holds, with the same change, is sent
// as it stands; one whose change moved is shown again; one whose condition
// ended or whose subject left is closed and nothing is sent.

// RecheckOutcome is what a re-check found.
type RecheckOutcome string

// The outcomes.
const (
	// RecheckHolds is the same verdict with the same change: send it.
	RecheckHolds RecheckOutcome = "holds"
	// RecheckChanged is the condition still there with another verdict or
	// change, which the person sees before anything is sent.
	RecheckChanged RecheckOutcome = "changed"
	// RecheckResolved is a condition that ended: closed, nothing sent.
	RecheckResolved RecheckOutcome = "resolved"
	// RecheckNotApplicable is a subject that left or a change that no longer
	// builds: closed as retired, nothing sent.
	RecheckNotApplicable RecheckOutcome = "not_applicable"
)

// ErrUnconcluded is a re-check whose run could not read the source the
// finding's class reads, or read it only in part and not the finding
// ([Reading.Cuts]), so it can say nothing about it: try again later.
var ErrUnconcluded = errors.New("advisor: the source this finding is read from could not be read whole just now")

// Recheck is what one run's reading r says of the stored finding s: its
// outcome, and the step that records it (nil where nothing moved).
func Recheck(s Stored, observed []Observation, r Reading) (RecheckOutcome, *Transition, error) {
	f := s.Observation.Finding
	if s.State != StateOpen {
		return "", nil, fmt.Errorf("advisor: a finding %s is not applied again", s.State)
	}
	if !r.Concludes(f) {
		return "", nil, ErrUnconcluded
	}
	key := s.Observation.Key()
	mine := slices.DeleteFunc(slices.Clone(observed), func(o Observation) bool { return o.Key() != key })
	if len(mine) == 0 && r.holdsOpen(f) {
		// Held open only because the reading may have left it out, so it
		// neither holds nor resolved.
		return "", nil, ErrUnconcluded
	}
	steps := Reconcile([]Stored{s}, mine, r)
	if len(steps) == 0 {
		return RecheckHolds, nil, nil
	}
	t := steps[0]
	switch {
	case t.Step == StepRefresh:
		return RecheckHolds, &t, nil
	case t.Step == StepRevise:
		return RecheckChanged, &t, nil
	case t.Step == StepClose && t.State == StateRetired:
		return RecheckNotApplicable, &t, nil
	case t.Step == StepClose:
		return RecheckResolved, &t, nil
	}
	return "", nil, fmt.Errorf("advisor: a re-check of an open finding took step %s", t.Step)
}

// ApplyDigest binds a re-check's answer to what it was read at: the finding,
// its revision, and the option it would send, by [OptionDigest]. A person's
// report that the change was sent names it, so a finding that moved since is
// re-checked rather than marked applied.
func ApplyDigest(id string, revision int, option string) string {
	return digestOf([]string{id, fmt.Sprint(revision), option})
}

// EffectHorizon is how long after a fix is applied a run may still find its
// condition before the fix is judged not to have cleared it: the setup's next
// run, which a change to it starts at once, given an hour; a pattern over the
// week, whose old records age out of its window over that week; and the
// model's, which reads the same week.
func EffectHorizon(f Finding) time.Duration {
	if GroupOf(f) == GroupSetup {
		return time.Hour
	}
	return Window
}

// WithoutIneffective is f with every option whose change did not clear it
// (by [OptionDigest]) taken out of its fix, so it is never offered again. A
// fix left with no option is handed to Ask Crewlet instead, with what was
// tried named in its prompt.
func WithoutIneffective(f Finding, ineffective []string) Finding {
	if len(ineffective) == 0 || len(f.Fix.Options) == 0 {
		return f
	}
	kept := slices.DeleteFunc(slices.Clone(f.Fix.Options), func(o Option) bool {
		return slices.Contains(ineffective, OptionDigest(o))
	})
	if len(kept) == len(f.Fix.Options) {
		return f
	}
	if len(kept) > 0 {
		f.Fix.Options = kept
		return f
	}
	tried := f.Fix.Options[0].Label
	f.Fix = Fix{Kind: FixAskCrewlet, Options: []Option{},
		Sentence: "Ask Crewlet what else would clear it: the change offered did not.",
		Prompt: fmt.Sprintf("The Advisor's finding %q still stands after %q was applied. Read what it rests on "+
			"and suggest another change.", f.Title, tried)}
	return f
}

// Holds reports whether entity, an entity as the configuration answers it,
// already holds every field ch would write: what an applied change leaves.
// The change is read back through the entity's own type, so a field it sets
// to the value the type leaves out (a grant turned off) holds once absent.
func Holds(entity any, ch Change) (bool, error) {
	changed, err := Changed(entity, ch)
	if err != nil {
		return false, err
	}
	after, err := asTypeOf(entity, changed)
	if err != nil {
		return false, err
	}
	before, err := asFields(entity)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(before, after), nil
}

// asTypeOf is fields decoded into a fresh value of entity's struct type and
// answered as that value's fields; fields themselves for an entity that is
// no struct.
func asTypeOf(entity any, fields map[string]any) (map[string]any, error) {
	t := reflect.TypeOf(entity)
	if t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return fields, nil
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	fresh := reflect.New(t).Interface()
	if err := json.Unmarshal(raw, fresh); err != nil {
		return nil, err
	}
	return asFields(fresh)
}

// Changed is entity with ch applied as a person sends it: ch.Merge merged
// in key by key, then ch.Set's fields replaced.
func Changed(entity any, ch Change) (map[string]any, error) {
	fields, err := asFields(entity)
	if err != nil {
		return nil, err
	}
	merge, err := asJSON(ch.Merge)
	if err != nil {
		return nil, err
	}
	set, err := asJSON(ch.Set)
	if err != nil {
		return nil, err
	}
	fields = mergeFields(fields, merge)
	for k, v := range set {
		fields[k] = v
	}
	return fields, nil
}
