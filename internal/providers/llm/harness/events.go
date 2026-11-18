package harness

import (
	"cmp"
	"encoding/json"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// classifyEvents finds the harness's own structured report of how the run
// ended: a limit ([harnessprofile.LimitEvent]) or, in a run reported failed, a
// refused sign-in ([harnessprofile.AuthEvent]). Read before every sentinel,
// because a status code survives the rewording that retires one.
//
// THE LAST REPORT WINS: Claude Code retries a 401 in `api_retry` events, so a
// run refreshed and then spent ended on the plan; read auth first, it set a
// refused-sign-in verdict on every entry on the account.
//
// A refusal counts only in a failed run: one that answered was signed in. Every
// line is decoded, none searched as text ([classifyFailureEvents]): a model
// writes inside an event's strings.
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
// structured report first ([harnessprofile.LimitReport], the LAST whose own
// window is reached, which is the newest word on a run that met the limit
// more than once), then the sentence's own reset (already read by the
// classification) and its wording ([harnessprofile.Profile.LimitWindows]), then
// [llm.LimitUnknown].
//
// A reset the event names that is not after the report is no reset, as one
// read from a sentence is not ([resetAt]): the window turned over as the run
// ended.
func dateLimit(p harnessprofile.Profile, hit markerHit, out harnessOutput) markerHit {
	if _, reached := readLimitReports(p, out.stdout); reached != nil {
		if reached.ResetsAt.After(out.at) {
			hit.Reset = reached.ResetsAt.UTC()
		}
		hit.Window = reached.Window
	}
	if hit.Window == "" || hit.Window == llm.LimitUnknown {
		hit.Window = p.ReadsLimitWindow(hit.Said)
	}
	return hit
}

// readLimitReports reads every limit report on stdout: each window's newest
// reading, and the last report whose own window was reached. A report's own
// window wins over its object of windows.
func readLimitReports(p harnessprofile.Profile, stdout string) ([]llm.LimitReading, *llm.LimitReading) {
	if len(p.LimitReports) == 0 {
		return nil, nil
	}
	newest := map[string]llm.LimitReading{}
	var reached *llm.LimitReading
	for line := range strings.SplitSeq(stdout, "\n") {
		doc, ok := decodeObject(strings.TrimSpace(line))
		if !ok {
			continue
		}
		for _, report := range p.LimitReports {
			if !matches(doc, report.Match) {
				continue
			}
			word, _ := textAt(doc, report.State.Path)
			state, known := report.State.Map[word]
			if !known {
				continue
			}
			for _, r := range windowsOf(report, doc) {
				newest[r.Key()] = r
			}
			own := llm.LimitReading{Window: llm.LimitUnknown, State: state,
				Used: fractionAt(doc, report.Used), ResetsAt: instantAt(doc, report.ResetsAt)}
			name, _ := textAt(doc, report.Window.Path)
			if w, mapped := report.Window.Map[name]; mapped {
				own.Window, own.Hours, own.Model = w.Window, w.Hours, w.Model
				if held, ok := newest[own.Key()]; ok && own.Used == nil {
					own.Used = held.Used
				}
				newest[own.Key()] = own
			}
			if state == llm.LimitReached {
				at := own
				reached = &at
			}
			break
		}
	}
	out := slices.Collect(maps.Values(newest))
	sortReadings(out)
	return out, reached
}

// windowsOf reads a report's optional object of windows; an entry the map does
// not name, or that says neither a state nor a fraction, is skipped.
func windowsOf(report harnessprofile.LimitReport, doc map[string]any) []llm.LimitReading {
	if report.Windows == nil {
		return nil
	}
	value, ok := lookup(doc, report.Windows.Path)
	all, isObject := value.(map[string]any)
	if !ok || !isObject {
		return nil
	}
	var out []llm.LimitReading
	for _, name := range slices.Sorted(maps.Keys(all)) {
		w, mapped := report.Window.Map[name]
		entry, isEntry := all[name].(map[string]any)
		if !mapped || !isEntry {
			continue
		}
		word, _ := textAt(entry, report.Windows.State)
		state, known := report.State.Map[word]
		used := fractionAt(entry, report.Windows.Used)
		if !known && used == nil {
			continue
		}
		if !known {
			state = llm.LimitOK
		}
		out = append(out, llm.LimitReading{Window: w.Window, Hours: w.Hours, Model: w.Model,
			State: state, Used: used, ResetsAt: instantAt(entry, report.Windows.ResetsAt)})
	}
	return out
}

// sortReadings orders a plan's windows as a person reads them: by
// [llm.LimitWindows], then by model.
func sortReadings(readings []llm.LimitReading) {
	slices.SortFunc(readings, func(a, b llm.LimitReading) int {
		return cmp.Or(cmp.Compare(slices.Index(llm.LimitWindows, a.Window), slices.Index(llm.LimitWindows, b.Window)),
			cmp.Compare(a.Model, b.Model))
	})
}

// observeLimits keeps what a run's stdout, read at `at`, reported of the
// entry's sign-in's windows, whatever the run's outcome.
func (p *Provider) observeLimits(stdout string, at time.Time) {
	if readings, _ := readLimitReports(p.profile, stdout); len(readings) > 0 {
		p.Plan().Observe(readings, at)
	}
}

// fractionAt is the non-negative finite number at path, nil where there is none.
func fractionAt(doc map[string]any, path harnessprofile.Path) *float64 {
	if len(path) == 0 {
		return nil
	}
	value, ok := lookup(doc, path)
	if !ok {
		return nil
	}
	f, ok := numberOf(value)
	if !ok || f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return &f
}

// instantAt is the instant at an [harnessprofile.EventInstant], zero where there is none.
func instantAt(doc map[string]any, at harnessprofile.EventInstant) time.Time {
	if len(at.Path) == 0 {
		return time.Time{}
	}
	value, ok := lookup(doc, at.Path)
	if !ok {
		return time.Time{}
	}
	return readInstant(value, at.Unit)
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
