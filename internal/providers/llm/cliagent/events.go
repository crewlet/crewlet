package cliagent

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// classifyAuthEvents finds the CLI's own structured report of a refused
// sign-in ([cliprofile.AuthEvent]) in a run the CLI reported failed.
//
// ONLY IN A FAILED RUN. Claude Code retries a 401 and says so in a `system`
// `api_retry` event before it gives up, and a CLI that refreshes its token on
// a 401 can retry and then answer. A run that answered was signed in, whatever
// its stream said on the way, so its answer is not thrown away as a refusal.
//
// Every line is decoded and none is searched as text, for the reason
// [classifyFailureEvents] gives: a model writes inside an event's strings,
// never the fields the match reads.
func classifyAuthEvents(p cliprofile.Profile, out cliOutput) (markerHit, bool) {
	if !out.failed || len(p.AuthEvents) == 0 {
		return markerHit{}, false
	}
	for line := range strings.SplitSeq(out.stdout, "\n") {
		line = strings.TrimSpace(line)
		doc, ok := decodeObject(line)
		if !ok {
			continue
		}
		for _, event := range p.AuthEvents {
			if !matches(doc, event.Match) {
				continue
			}
			said := line
			if text, found := textAt(doc, event.SaidPath); found {
				said = text
			}
			return markerHit{Kind: llm.KindAuth, Said: said, Source: sourceEvent}, true
		}
	}
	return markerHit{}, false
}

// dateLimit completes a limit the run reported with when it lifts and which
// limit it is, from the strongest evidence the run gave: the CLI's own
// structured event first ([cliprofile.LimitResetEvent], the LAST that matches,
// which is the newest word on a run that met the limit more than once), then
// the sentence's own reset (already read by the classification) and its
// wording ([cliprofile.Profile.LimitWindows]), then [llm.LimitUnknown].
//
// A reset the event names that is not after the report is no reset, as one
// read from a sentence is not ([resetAt]): the window turned over as the run
// ended.
func dateLimit(p cliprofile.Profile, hit markerHit, out cliOutput) markerHit {
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
// that matches one of the profile's [cliprofile.LimitResetEvent]s, false when
// none does. A window the event does not name, or names in words the map does
// not hold, is [llm.LimitUnknown]; a reset it does not carry is zero.
func lastResetEvent(p cliprofile.Profile, stdout string) (time.Time, llm.LimitWindow, bool) {
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
func readInstant(value any, unit cliprofile.InstantUnit) time.Time {
	switch unit {
	case cliprofile.InstantEpoch, cliprofile.InstantEpochMS:
		n, ok := wholeNumber(value)
		if !ok || n <= 0 {
			return time.Time{}
		}
		if unit == cliprofile.InstantEpochMS {
			return time.UnixMilli(n).UTC()
		}
		return time.Unix(n, 0).UTC()
	case cliprofile.InstantRFC3339:
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
func textAt(doc map[string]any, path cliprofile.Path) (string, bool) {
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
