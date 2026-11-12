package atlas

import (
	"fmt"
	"slices"
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
// and how many more are open, or that its sources disagree. Empty for a quiet
// entry. It says only what the reader of e may read, since e carries only the
// signals they may.
func (e Entry) Reason() string {
	if len(e.Signals) > 0 {
		first := e.Signals[0]
		if i := slices.IndexFunc(e.Signals, func(s Signal) bool { return s.Severity == SeverityDanger }); i >= 0 {
			first = e.Signals[i]
		}
		sentence := signalReasons[first.Kind]
		if sentence == "" {
			sentence = "Something on it needs attention"
		}
		switch more := len(e.Signals) - 1; {
		case more == 1:
			return sentence + ", and one more thing needs attention"
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
