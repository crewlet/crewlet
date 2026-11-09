package insights

import (
	"fmt"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/issuereport"
)

// A Record is one class's operating pattern in one ISO week: what an opted-in
// engine may share, and nothing else. EVERY STRING IN IT IS A NAMED TYPE that
// only [Records] makes, so no company text can reach one.
type Record struct {
	Schema  int               `json:"schema"`
	Class   Class             `json:"class"`
	Week    Week              `json:"week"`
	Version Version           `json:"version"`
	Facts   map[FactKey]Label `json:"facts"`
}

// Week is an ISO week, `2030-W02`: the finest instant a record holds.
type Week string

// Version is the build's release tag, or `main` for any other public build.
type Version string

// mainVersion is the version of a public build that is no release tag: its
// commit would say more than which code it is.
const mainVersion Version = "main"

var (
	weekShape    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^\d{4}-W(0[1-9]|[1-4]\d|5[0-3])$`) })
	versionShape = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^(v\d+\.\d+\.\d+(-(rc|alpha|beta)\.\d+)?|main)$`)
	})
)

// WeekOf is the ISO week at holds, in UTC.
func WeekOf(at time.Time) Week {
	year, week := at.UTC().ISOWeek()
	return Week(fmt.Sprintf("%04d-W%02d", year, week))
}

// VersionOf is what a record names the build by, or false for a build that
// sends nothing ([issuereport.Origin.InertReason]).
func VersionOf(o issuereport.Origin) (Version, bool) {
	switch {
	case o.InertReason() != "":
		return "", false
	case o.Tagged():
		return Version(o.Build.Version), true
	}
	return mainVersion, true
}

// Records is one record per catalogue class the findings raise, for the week
// at falls in, in the catalogue's order; none from a build that sends nothing.
// A finding whose evidence a class cannot read is left out of its record.
func Records(findings []advisor.Finding, at time.Time, origin issuereport.Origin) []Record {
	version, ok := VersionOf(origin)
	if !ok {
		return nil
	}
	var out []Record
	for _, e := range catalogue {
		if r, ok := e.record(findings, WeekOf(at), version); ok {
			out = append(out, r)
		}
	}
	return out
}

// record folds every finding of e's class into one record: the worst one's
// facts, and how many findings raised it.
func (e Entry) record(findings []advisor.Finding, week Week, version Version) (Record, bool) {
	var raised []map[string]int64
	for _, f := range findings {
		if Class(f.Class) != e.Class || f.Source != advisor.SourceEvidence {
			continue
		}
		ev := map[string]int64{}
		for _, fact := range f.Evidence {
			ev[fact.Key] = fact.Value
		}
		if e.readable(ev) {
			raised = append(raised, ev)
		}
	}
	if len(raised) == 0 {
		return Record{}, false
	}
	worst := slices.MinFunc(raised, func(a, b map[string]int64) int {
		switch {
		case e.worse(a, b):
			return -1
		case e.worse(b, a):
			return 1
		}
		return 0
	})
	facts := make(map[FactKey]Label, len(e.Facts))
	for _, fact := range e.Facts {
		var label Label
		if fact.folded {
			label, _ = findingsBucket.of(int64(len(raised)))
		} else {
			label, _ = fact.of(worst)
		}
		facts[fact.Key] = label
	}
	return Record{Schema: SchemaVersion, Class: e.Class, Week: week, Version: version, Facts: facts}, true
}

// readable reports whether every fact of e reads a label off ev.
func (e Entry) readable(ev map[string]int64) bool {
	for _, fact := range e.Facts {
		if fact.folded {
			continue
		}
		if _, ok := fact.of(ev); !ok {
			return false
		}
	}
	return true
}

// Valid reports why r is no record the catalogue allows, nil when it is one:
// the check a sender and the relay both make before anything leaves.
func (r Record) Valid() error {
	e, ok := entryOf(r.Class)
	switch {
	case !ok:
		return fmt.Errorf("insights: %q is no catalogue class", r.Class)
	case r.Schema != SchemaVersion:
		return fmt.Errorf("insights: schema %d, want %d", r.Schema, SchemaVersion)
	case !weekShape().MatchString(string(r.Week)):
		return fmt.Errorf("insights: %q is no ISO week", r.Week)
	case !versionShape().MatchString(string(r.Version)):
		return fmt.Errorf("insights: %q is no release tag or main", r.Version)
	case len(r.Facts) != len(e.Facts):
		return fmt.Errorf("insights: %s carries %d facts, want %d", r.Class, len(r.Facts), len(e.Facts))
	}
	for _, fact := range e.Facts {
		if !slices.Contains(fact.Values, r.Facts[fact.Key]) {
			return fmt.Errorf("insights: %s's %s is %q, outside its set", r.Class, fact.Key, r.Facts[fact.Key])
		}
	}
	return nil
}
