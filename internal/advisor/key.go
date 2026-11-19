package advisor

import (
	"strings"

	"github.com/crewlet/crewlet/internal/config"
)

// A FINDING'S KEY is its identity across runs: its class, its subject and, for
// a class that raises several findings on one subject, its [Finding.Focus].
// Never its words or numbers, which move while the condition stands, so one
// condition is one stored finding however often it is read (ADR-0164).

// MaxKeyBytes bounds a key: a class, a seat or schedule name and a document path.
const MaxKeyBytes = 512

// Key is f's identity: `class:subject`, then `#focus` where it has one.
func Key(f Finding) string {
	key := string(f.Class) + ":" + SubjectKey(f.Subject)
	if f.Focus != "" {
		key += "#" + f.Focus
	}
	return key
}

// SubjectKey names a subject as a key does: `seat:<handle>`,
// `schedule:<seat or unit>/<name>`, `integration:<name>`, `repository:<name>`,
// `atlas` or `company`. A kind this build does not name keys by its name.
func SubjectKey(s Subject) string {
	switch s.Kind {
	case SubjectCompany, SubjectAtlas:
		return string(s.Kind)
	case SubjectSeat:
		return "seat:" + s.Seat
	case SubjectSchedule:
		owner := s.Seat
		if owner == "" {
			owner = "unit/" + s.Unit
		}
		return "schedule:" + owner + "/" + s.Schedule
	}
	return string(s.Kind) + ":" + s.Name
}

// SubjectID is the subject's own id within its kind, as the wire names it.
func SubjectID(s Subject) string {
	_, id, _ := strings.Cut(SubjectKey(s), ":")
	return id
}

// ItemFocus is the focus of a finding about one work item, by its key.
func ItemFocus(key string) string { return "item:" + key }

// staticFocus is where in the document a warning sits, and what within it.
func staticFocus(w config.Warning) string {
	if w.Item == "" {
		return w.Path
	}
	return w.Path + "|" + w.Item
}
