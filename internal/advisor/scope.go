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
	sourceStops: UnreadStops, sourceMerges: UnreadMerges,
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
