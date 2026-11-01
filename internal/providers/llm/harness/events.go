package harness

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// classifyEvents finds the harness's own structured report of how the run ended:
// a limit ([harnessprofile.LimitEvent]) or, in a run the harness reported failed, a
// refused sign-in ([harnessprofile.AuthEvent]). Read before every sentinel,
// because a status code survives the rewording that silently retires one.
//
// THE LAST REPORT WINS. A stream is the run's history, and its last word is
// how the run ended: Claude Code retries a 401 in `api_retry` events, and a
// run whose token was refreshed and that then met a spent plan ended on the
// plan. Read in a fixed order, auth first, it read as a refused sign-in and set
// a verdict on every entry on the account while the limit sat nothing out.
//
// ONLY IN A FAILED RUN FOR A REFUSAL. A run that answered was signed in,
// whatever its stream said on the way.
//
// Every line is decoded and none is searched as text, for the reason
// [classifyFailureEvents] gives: a model writes inside an event's strings,
// never the fields the match reads.
func classifyEvents(p harnessprofile.Profile, out harnessOutput) (markerHit, bool) {
	if len(p.LimitEvents) == 0 && (len(p.AuthEvents) == 0 || !out.failed) {
		return markerHit{}, false
	}
	var (
		last  markerHit
		found bool
	)
	for line := range strings.SplitSeq(out.stdout, "\n") {
		line = strings.TrimSpace(line)
		doc, ok := decodeObject(line)
		if !ok {
			continue
		}
		if hit, matched := limitEventHit(p, doc, line, out); matched {
			last, found = hit, true
			continue
		}
		if !out.failed {
			continue
		}
		for _, event := range p.AuthEvents {
			if !matches(doc, event.Match) {
				continue
			}
			said := line
			if text, ok := textAt(doc, event.SaidPath); ok {
				said = text
			}
			last, found = markerHit{Kind: llm.KindAuth, Said: said, Source: sourceEvent}, true
			break
		}
	}
	return last, found
}

// dateLimit completes a limit the run reported with when it lifts and which
// limit it is, from the strongest evidence the run gave: the harness's own
// structured event first ([harnessprofile.LimitResetEvent], the LAST that matches,
// which is the newest word on a run that met the limit more than once), then
// the sentence's own reset (already read by the classification) and its
// wording ([harnessprofile.Profile.LimitWindows]), then [llm.LimitUnknown].
//
// A reset the event names that is not after the report is no reset, as one
// read from a sentence is not ([resetAt]): the window turned over as the run
// ended.
func dateLimit(p harnessprofile.Profile, hit markerHit, out harnessOutput) markerHit {
	if reset, window, ok := lastResetEvent(p, out.stdout); ok {
		if reset.After(out.at) {
			hit.Reset = reset.UTC()
		}
		hit.Window = window
	}
	if hit.Window == "" || hit.Window == llm.LimitUnknown {
		hit.Window = p.ReadsLimitWindow(hit.Said)
	}
	return hit
}

// lastResetEvent is the reset instant and window of the last event on stdout
// that matches one of the profile's [harnessprofile.LimitResetEvent]s, false when
// none does. A window the event does not name, or names in words the map does
// not hold, is [llm.LimitUnknown]; a reset it does not carry is zero.
func lastResetEvent(p harnessprofile.Profile, stdout string) (time.Time, llm.LimitWindow, bool) {
	if len(p.LimitResetEvents) == 0 {
		return time.Time{}, "", false
	}
	var (
		reset  time.Time
		window llm.LimitWindow
		found  bool
	)
	for line := range strings.SplitSeq(stdout, "\n") {
		doc, ok := decodeObject(strings.TrimSpace(line))
		if !ok {
			continue
		}
		for _, event := range p.LimitResetEvents {
			if !matches(doc, event.Match) {
				continue
			}
			found = true
			reset = time.Time{}
			if value, ok := lookup(doc, event.ResetsAt.Path); ok && len(event.ResetsAt.Path) > 0 {
				reset = readInstant(value, event.ResetsAt.Unit)
			}
			window = llm.LimitUnknown
			if name, ok := textAt(doc, event.Window.Path); ok {
				if mapped, known := event.Window.Map[name]; known {
					window = mapped
				}
			}
			break
		}
	}
	return reset, window, found
}

// readInstant reads an event's value as an instant in unit, zero when it is
// not one.
func readInstant(value any, unit harnessprofile.InstantUnit) time.Time {
	switch unit {
	case harnessprofile.InstantEpoch, harnessprofile.InstantEpochMS:
		n, ok := wholeNumber(value)
		if !ok || n <= 0 {
			return time.Time{}
		}
		if unit == harnessprofile.InstantEpochMS {
			return time.UnixMilli(n).UTC()
		}
		return time.Unix(n, 0).UTC()
	case harnessprofile.InstantRFC3339:
		text, ok := value.(string)
		if !ok {
			return time.Time{}
		}
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(text))
		if err != nil {
			return time.Time{}
		}
		return at.UTC()
	}
	return time.Time{}
}

// wholeNumber is a JSON number, or a string of digits, as an integer.
func wholeNumber(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		if v != math.Trunc(v) || v > math.MaxInt64 || v < math.MinInt64 {
			return 0, false
		}
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n, err == nil
	}
	return 0, false
}

// textAt is the non-blank string at path in doc, trimmed; false where the
// path is empty, absent or holds anything else.
func textAt(doc map[string]any, path harnessprofile.Path) (string, bool) {
	if len(path) == 0 {
		return "", false
	}
	value, ok := lookup(doc, path)
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return "", false
	}
	return strings.TrimSpace(text), true
}
