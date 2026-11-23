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

// dutyNames is every duty-name expression a workerDuty or liveDuty call passes, keyed by
// its source text, with the names it evaluates to (dutyTTLs' idiom, for the
// first argument).
//
// A LIST, because the identity duties are claimed through one helper as
// `e.workerDuty(name, …)`, so `name` is every duty on identityDutyRoster.
//
// ONLY workerDuty's: a workerHold is mutual exclusion over one piece of work,
// not a duty, and the pages say so instead of listing it.
var dutyNames = map[string][]string{
	"schedule.DutyName":     {schedule.DutyName},
	"waiterDutyName":        {waiterDutyName},
	"maintenanceDutyName":   {maintenanceDutyName},
	"integrationDutyName":   {integrationDutyName},
	"atlasCollectDutyName":  {atlasCollectDutyName},
	"atlasWakeDutyName":     {atlasWakeDutyName},
	"learningDutyName":      {learningDutyName},
	"retentionDutyName":     {retentionDutyName},
	"trashreap.Name":        {trashreap.Name},
	"embedDutyName":         {embedDutyName},
	"objectCollectorDuty":   {objectCollectorDuty},
	"chatPruneDutyName":     {chatPruneDutyName},
	"delegatedSweepName":    {delegatedSweepName},
	"flowDutyName":          {flowDutyName},
	"codeRetentionDutyName": {codeRetentionDutyName},
	"name":                  identityDutyNames(),
}

// liveDuties are the duties claimed through liveDuty: held where the live
// projection is seeded rather than by the workers role (ADR-0162), so the
// pages' duty tables name them and the workers role's lists do not.
var liveDuties = []string{flowDutyName}

// dutyPhrases is how the pages name each duty in prose, lower-cased. A duty
// dutyNames lists with no phrase here fails TestEveryDutyListNamesEveryDuty.
var dutyPhrases = map[string][]string{
	schedule.DutyName:     {"scheduler"},
	waiterDutyName:        {"launch-node watch"},
	maintenanceDutyName:   {"maintenance sweep"},
	integrationDutyName:   {"integration reconcile"},
	atlasCollectDutyName:  {"atlas collect", "atlas-collect"},
	atlasWakeDutyName:     {"atlas wake", "atlas-wake"},
	learningDutyName:      {"learning passes", "learning background passes", "skill clustering"},
	retentionDutyName:     {"trim"},
	trashreap.Name:        {"trash reaper"},
	embedDutyName:         {"embedding"},
	objectCollectorDuty:   {"collector"},
	chatPruneDutyName:     {"message horizon", "chat prune"},
	delegatedSweepName:    {"delegated decisions sweep", "delegated-decisions"},
	flowDutyName:          {"flow duty", "tracker-flow"},
	codeRetentionDutyName: {"code retention", "code-retention"},
	identitySweepDuty:     {"identity sweep"},
}

// EVERY DUTY CLAIMED IS IN THE NAME TABLE, and every name in it is claimed,
// so a new duty cannot skip the pages' gate below by not being written down.
func TestEveryClaimedDutyIsInTheNameTable(t *testing.T) {
	t.Parallel()
	var named []string
	for _, site := range dutyClaims(t, sourcetree.Root(t)).sites {
		if site.helper != "workerDuty" && site.helper != "liveDuty" {
			continue
		}
		named = append(named, site.name)
		for _, name := range dutyNames[site.name] {
			if (site.helper == "liveDuty") != slices.Contains(liveDuties, name) {
				t.Errorf("%s claims the duty %s through %s, and liveDuties says the "+
					"opposite of whether the workers role holds it", site.where, name, site.helper)
			}
		}
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

// THE PAGES LIST EVERY DUTY, AND ONLY DUTIES: architecture.md's workers table
// and seat-ownership.md's singleton table. A duty a page omits is a lease an
// operator finds in `crewlet fleet` with nothing to say what it is; a row
// naming no duty sends them looking for one that does not run.
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

// EVERY HAND-KEPT LIST OF THE WORKERS ROLE NAMES EVERY DUTY IT HOLDS: each
// prose list in dutyLists names every duty claimed through workerDuty by one
// of its [dutyPhrases], since each was typed by hand and falls behind the
// engine on its own. A duty in liveDuties is no workers duty and is not there.
//
// Mutation: drop the trash reaper from the fleet guide's row and this names
// the page and the duty.
func TestEveryDutyListNamesEveryDuty(t *testing.T) {
	t.Parallel()
	var claimed []string
	for _, names := range dutyNames {
		claimed = append(claimed, names...)
	}
	slices.Sort(claimed)
	for _, name := range claimed {
		if len(dutyPhrases[name]) == 0 {
			t.Errorf("dutyPhrases gives the duty %q no phrase, so no page can be held "+
				"to naming it", name)
		}
	}
	for _, list := range dutyLists {
		data, err := os.ReadFile(filepath.Join(sourcetree.Root(t), filepath.FromSlash(list.path)))
		if err != nil {
			t.Fatalf("read %s: %v", list.path, err)
		}
		text := strings.ToLower(list.read(strings.Split(string(data), "\n")))
		if text == "" {
			t.Errorf("%s has no %s, so its duties cannot be read", list.path, list.what)
			continue
		}
		for _, name := range claimed {
			if slices.Contains(liveDuties, name) {
				continue
			}
			named := slices.ContainsFunc(dutyPhrases[name], func(phrase string) bool {
				return strings.Contains(text, phrase)
			})
			if !named {
				t.Errorf("%s's %s does not name the duty %q (as any of %q)", list.path,
					list.what, name, dutyPhrases[name])
			}
		}
	}
}

// dutyLists are the hand-kept lists of the workers role's duties, each with
// how to find it in its file.
var dutyLists = []struct {
	path, what string
	read       func(lines []string) string
}{
	{"docs/guides/fleet.md", "workers row", lineStarting("| `workers` |")},
	{"docs/concepts/scaling.md", "workers row", lineStarting("| `workers` |")},
	{"docs/concepts/configuration.md", "workers row", lineStarting("| `workers` |")},
	{"docs/guides/deployment.md", "workers bullet", lineStarting("- **`workers`** runs")},
	{"docs/guides/deployment.md", "account of the singleton duties",
		lineStarting("- *Duplicate auto-drafted skill pages")},
	{"docs/guides/satellite-nodes.md", "list of the core's singleton duties",
		lineContaining("Every [singleton duty]")},
	{"docs/concepts/overview.md", "DUTY box", lineContaining(`DUTY["`)},
	{"internal/seat/placement/placement.go", "RoleWorkers doc", docAbove("RoleWorkers NodeRole")},
}

// lineStarting reads the first line that starts with prefix, once its
// indentation is gone.
func lineStarting(prefix string) func([]string) string {
	return func(lines []string) string {
		for _, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), prefix) {
				return line
			}
		}
		return ""
	}
}

// lineContaining reads the first line that holds substr.
func lineContaining(substr string) func([]string) string {
	return func(lines []string) string {
		for _, line := range lines {
			if strings.Contains(line, substr) {
				return line
			}
		}
		return ""
	}
}

// docAbove reads the comment lines directly above the line that holds decl.
func docAbove(decl string) func([]string) string {
	return func(lines []string) string {
		for i, line := range lines {
			if !strings.Contains(line, decl) {
				continue
			}
			var doc []string
			for j := i - 1; j >= 0 && strings.HasPrefix(strings.TrimSpace(lines[j]), "//"); j-- {
				doc = append([]string{lines[j]}, doc...)
			}
			return strings.Join(doc, " ")
		}
		return ""
	}
}

// tableKeys reads the markdown table under header in a docs page and returns
// the backticked key of each row's first cell, less prefix.
//
// A ROW WHOSE FIRST CELL IS NOT ONE KEY FAILS rather than being skipped, which
// would count it absent, in the direction that passes.
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
