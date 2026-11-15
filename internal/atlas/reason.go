package atlas

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// EVERY SENTENCE HERE IS THE ENGINE'S OWN: a closed word of the vocabulary and
// a count, never a name, a label or a value. A vendor's text, an agent's and a
// person's reach a reader only in the typed fields beside a sentence, which a
// surface draws as somebody's words; so a sentence is safe wherever it is read.

// signalReasons says what each signal means for its entry.
var signalReasons = map[SignalKind]string{
	SignalIntegrationBlocked: "The integration waits on a person",
	SignalIntegrationFailing: "The integration's last pass failed",
	SignalToolServerDown:     "An instance of the tool server failed to start",
	SignalToolServerWaiting:  "An instance of the tool server is still waiting to start",
	SignalSignInExpired:      "The tool server's sign-in has lapsed",
	SignalSignInExpiring:     "The tool server's sign-in lapses within a week",
	SignalSignInFailed:       "The tool server's sign-in failed to refresh",
	SignalModelSittingOut:    "The model's plan is spent, so it sits out until it resets",
	SignalModelFailing:       "The model's last call failed, with no answer since",
	SignalServerTestFailed:   "The server's last connection test failed",
	SignalStandingAlarm:      "A standing alarm is raised",
	SignalMonitorAlerting:    "A monitor is alerting",
	SignalMonitorWarning:     "A monitor is warning",
	SignalMonitorNoData:      "A monitor is receiving no data",
	SignalPipelineFailing:    "A pipeline's last run failed",
}

// Reason is why e is not quiet, in one sentence: its most urgent open signal
// and, beside it, a second one named or how many more are open; or that its
// sources disagree. Empty for a quiet entry. It says only what the reader of e
// may read, since e carries only the signals they may.
//
// A SECOND SIGNAL IS NAMED, never "one more thing needs attention", which told
// a reader there was something else and nothing of what. Two or more beyond
// the first are counted.
func (e Entry) Reason() string {
	if len(e.Signals) > 0 {
		at := 0
		if i := slices.IndexFunc(e.Signals, func(s Signal) bool { return s.Severity == SeverityDanger }); i >= 0 {
			at = i
		}
		first := e.Signals[at]
		sentence := reasonOf(first.Kind)
		switch more := len(e.Signals) - 1; {
		case more == 1:
			other := e.Signals[1-at]
			if other.Kind == first.Kind {
				return sentence + ", and one more like it"
			}
			return sentence + ", and " + lowerFirst(reasonOf(other.Kind))
		case more > 1:
			return fmt.Sprintf("%s, and %d more things need attention", sentence, more)
		}
		return sentence
	}
	if !e.Review {
		return ""
	}
	contested := slices.ContainsFunc(e.Facts, func(f FactView) bool { return f.Standing == StandingContested }) ||
		slices.ContainsFunc(e.Relations, func(ed Edge) bool { return ed.Standing == StandingContested })
	if contested {
		return "A newer report contradicts what a person recorded"
	}
	return "Two sources disagree, and nobody has settled it"
}

// reasonOf is what a signal of kind k means for its entry.
func reasonOf(k SignalKind) string {
	if sentence := signalReasons[k]; sentence != "" {
		return sentence
	}
	return "Something on it needs attention"
}

// lowerFirst is a sentence of signalReasons set after a comma: every one opens
// with an article, never a name.
func lowerFirst(sentence string) string {
	r, size := utf8.DecodeRuneInString(sentence)
	return strings.ToLower(string(r)) + sentence[size:]
}
