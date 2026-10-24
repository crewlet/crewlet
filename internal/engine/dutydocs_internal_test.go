package engine

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/trashreap"
)

// dutyNames is every duty-name expression a workerDuty call passes, keyed by
// its source text, with the names it evaluates to — the dutyTTLs idiom, for the
// first argument rather than the second.
//
// A LIST, because one expression names several duties: the identity duties
// are claimed through one helper as `e.workerDuty(name, …)`, so `name` is
// every duty on the roster that helper is called for (identityDutyRoster,
// held to what the engine arms). Every other expression is one constant.
//
// ONLY workerDuty's: a workerHold is a lease given back when one piece of work
// ends (a provisioning pass at one third-party app), which is mutual exclusion
// rather than a duty, and the pages say so instead of listing it.
var dutyNames = map[string][]string{
	"schedule.DutyName":   {schedule.DutyName},
	"waiterDutyName":      {waiterDutyName},
	"maintenanceDutyName": {maintenanceDutyName},
	"integrationDutyName": {integrationDutyName},
	"learningDutyName":    {learningDutyName},
	"retentionDutyName":   {retentionDutyName},
	"trashreap.Name":      {trashreap.Name},
	"embedDutyName":       {embedDutyName},
	"objectCollectorDuty": {objectCollectorDuty},
	"chatPruneDutyName":   {chatPruneDutyName},
	"name":                identityDutyNames(),
}

// EVERY DUTY CLAIMED IS IN THE NAME TABLE, and every name in it is claimed.
//
// What keeps dutyNames honest, as TestEveryDutyClaimSiteIsInTheTTLTable keeps
// dutyTTLs: a duty added with a name the table does not list fails here, so it
// cannot reach the pages' gate below unwritten.
func TestEveryClaimedDutyIsInTheNameTable(t *testing.T) {
	t.Parallel()
	var named []string
	for _, site := range dutyClaims(t, sourcetree.Root(t)).sites {
		if site.helper != "workerDuty" {
			continue
		}
		named = append(named, site.name)
		if _, listed := dutyNames[site.name]; !listed {
			t.Errorf("%s claims the duty %s, which dutyNames does not list; add "+
				"it with the name it evaluates to", site.where, site.name)
		}
	}
	for expr := range dutyNames {
		if !slices.Contains(named, expr) {
			t.Errorf("dutyNames lists %q but no workerDuty call passes it; the "+
				"table is stale or the scan matched nothing", expr)
		}
	}
}

// THE PAGES LIST EVERY DUTY, AND ONLY DUTIES.
//
// architecture.md's workers table names each `worker:DUTY` lease and
// seat-ownership.md's singleton table says why each runs once — and both
// listed five of the eight there were while nothing noticed. Both directions:
// a duty the engine claims and a page omits is a lease an operator finds in
// `crewlet fleet` with nothing to say what it is, and a row naming no duty
// sends them looking for one that does not run.
func TestTheDutyTablesNameEveryDuty(t *testing.T) {
	t.Parallel()
	var claimed []string
	for _, names := range dutyNames {
		claimed = append(claimed, names...)
	}
	slices.Sort(claimed)
	for page, table := range map[string]struct {
		header, prefix string
	}{
		"docs/concepts/architecture.md":   {"| Lease | Duty |", "worker:"},
		"docs/concepts/seat-ownership.md": {"| Duty | What it does | Why once |", ""},
	} {
		listed := tableKeys(t, page, table.header, table.prefix)
		for _, name := range claimed {
			if !slices.Contains(listed, name) {
				t.Errorf("%s's duty table has no row for %q, which the engine "+
					"claims as a duty", page, table.prefix+name)
			}
		}
		for _, name := range listed {
			if !slices.Contains(claimed, name) {
				t.Errorf("%s's duty table has a row for %q, which is no duty "+
					"the engine claims", page, table.prefix+name)
			}
		}
	}
}

// tableKeys reads the markdown table under header in a docs page and returns
// the backticked key of each row's first cell, less prefix.
//
// A ROW WHOSE FIRST CELL IS NOT ONE KEY FAILS rather than being skipped: a row
// this cannot read is a row the gate would otherwise count as absent, in the
// direction that passes.
func tableKeys(t *testing.T, rel, header, prefix string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join(sourcetree.Root(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("open %s: %v", rel, err)
	}
	defer func() { _ = f.Close() }()
	var keys []string
	inTable := false
	scanner := bufio.NewScanner(f)
	// A table row is one line, and these pages' rows run to kilobytes.
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case !inTable:
			inTable = line == header
		case strings.HasPrefix(line, "|---") || strings.HasPrefix(line, "| ---"):
		case strings.HasPrefix(line, "|"):
			cell, _, _ := strings.Cut(strings.TrimPrefix(line, "|"), "|")
			key, ok := strings.CutPrefix(strings.TrimSpace(cell), "`")
			if ok {
				key, ok = strings.CutSuffix(key, "`")
			}
			if ok {
				key, ok = strings.CutPrefix(key, prefix)
			}
			if !ok || key == "" || strings.ContainsAny(key, "` ") {
				t.Fatalf("%s: a row under %q does not open with one `%sNAME` "+
					"cell: %s", rel, header, prefix, line)
			}
			keys = append(keys, key)
		default:
			if len(keys) == 0 {
				t.Fatalf("%s: the table under %q has no rows", rel, header)
			}
			return keys
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	if !inTable || len(keys) == 0 {
		t.Fatalf("%s has no table under %q, so its duties cannot be read", rel, header)
	}
	return keys
}
