package advisor

import (
	"slices"
	"strings"
)

// A SCOPE is what one run can conclude about at once: a group, or a group and
// the one source its classes read. A run that could not read a source says
// nothing about the findings of that scope, so they are neither resolved nor
// confirmed, and they go stale on their own clock.

// sourceWords is the [Answer.Unread] word of each record a detector reads.
var sourceWords = map[source]string{
	sourceWork: UnreadWork, sourceLedger: UnreadLedger, sourceAlarms: UnreadAlarms,
	sourcePauses: UnreadPauses, sourceRuns: UnreadRuns, sourceFires: UnreadFires,
	sourceStops: UnreadStops, sourceMerges: UnreadMerges, sourceEpisodes: UnreadEpisodes,
	sourceFlow:     UnreadFlow,
	sourceSkips:    UnreadSkips,
	sourceStaffing: UnreadStaffing,
	sourceCapacity: UnreadCapacity,
}

// unreadWordOf is the unread word of the source f's class reads, false for a
// class whose only source is the company document.
func unreadWordOf(f Finding) (string, bool) {
	switch f.Source {
	case SourceReconcile:
		return UnreadIntegrations, true
	case SourceAtlas:
		return UnreadAtlas, true
	case SourceStatic, SourceModel:
		return "", false
	}
	if slices.Contains(PausedClasses(), f.Class) {
		return UnreadPauses, true
	}
	if f.Class == ClassScheduleUnproductive {
		return UnreadFires, true
	}
	for _, d := range detectors {
		if d.class == f.Class {
			return sourceWords[d.source], true
		}
	}
	for _, d := range documented {
		if d.class == f.Class {
			return sourceWords[d.source], true
		}
	}
	return "", false
}

// ScopeOf is the scope f is concluded in: its group, then `/` and the unread
// word of its source where it reads one.
func ScopeOf(f Finding) string {
	if word, ok := unreadWordOf(f); ok {
		return string(GroupOf(f)) + "/" + word
	}
	return string(GroupOf(f))
}

// ScopesOf is every scope of g.
func ScopesOf(g Group) []string {
	switch g {
	case GroupSetup:
		return []string{string(g), string(g) + "/" + UnreadIntegrations, string(g) + "/" + UnreadAtlas,
			string(g) + "/" + UnreadPauses}
	case GroupEvidence:
		var out []string
		for _, word := range sourceWords {
			if word != UnreadPauses {
				out = append(out, string(g)+"/"+word)
			}
		}
		slices.Sort(out)
		return out
	}
	return []string{string(g)}
}

// cutScope splits a scope into its group and its source's word.
func cutScope(scope string) (Group, string, bool) {
	g, word, ok := strings.Cut(scope, "/")
	return Group(g), word, ok
}

// ClassOutcome is what one run concluded about one class (ADR-0166): it found
// something, judged the company clear, could not judge some of it and says
// why, or could not read its source. A class is never skipped in silence.
type ClassOutcome struct {
	Class   Class   `json:"class"`
	Outcome Outcome `json:"outcome"`
	// Reason says why, in words, for a silent or unread class, and what
	// went unjudged for one that found something; "" otherwise.
	Reason string `json:"reason"`
}

// Outcome is what a run concluded about a class.
type Outcome string

// The outcomes.
const (
	OutcomeFound  Outcome = "found"
	OutcomeClear  Outcome = "clear"
	OutcomeSilent Outcome = "silent"
	OutcomeUnread Outcome = "unread"
)

// ruleClasses is every class the rule groups run, each with the unread word
// of the source it reads, "" for the company document alone.
func ruleClasses() map[Class]string {
	out := map[Class]string{}
	for class := range catalogue {
		out[class] = ""
	}
	for _, d := range detectors {
		if !Shared(d.class) {
			out[d.class] = sourceWords[d.source]
		}
	}
	for _, d := range documented {
		out[d.class] = sourceWords[d.source]
	}
	for _, class := range PausedClasses() {
		out[class] = UnreadPauses
	}
	out[ClassScheduleUnproductive] = UnreadFires
	for _, class := range AtlasClasses() {
		out[class] = UnreadAtlas
	}
	return out
}

// Outcomes is what a rule run concluded about every class it runs, and about
// any other class it found, by class: found where it raised a finding, unread
// where its source could not be read, silent where it read and still could
// not judge, clear otherwise.
func Outcomes(found []Finding, unread []string, silent []Silence) []ClassOutcome {
	classes := ruleClasses()
	raised := map[Class]bool{}
	for _, f := range found {
		raised[f.Class] = true
		if _, ok := classes[f.Class]; !ok {
			word, _ := unreadWordOf(f)
			classes[f.Class] = word
		}
	}
	why := map[Class]string{}
	for _, s := range silent {
		why[s.Class] = s.Reason
		if _, ok := classes[s.Class]; !ok {
			// A silence about no class the run reads (a record cut) is
			// still said.
			classes[s.Class] = ""
		}
	}
	out := make([]ClassOutcome, 0, len(classes))
	for class, word := range classes {
		o := ClassOutcome{Class: class, Outcome: OutcomeClear, Reason: why[class]}
		switch {
		case word != "" && slices.Contains(unread, word):
			o.Outcome, o.Reason = OutcomeUnread, "Its source ("+word+") could not be read this run."
		case raised[class]:
			o.Outcome = OutcomeFound
		case o.Reason != "":
			o.Outcome = OutcomeSilent
		}
		out = append(out, o)
	}
	slices.SortFunc(out, func(a, b ClassOutcome) int { return strings.Compare(string(a.Class), string(b.Class)) })
	return out
}
