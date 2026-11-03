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
	"schedule.DutyName":     {schedule.DutyName},
	"waiterDutyName":        {waiterDutyName},
	"maintenanceDutyName":   {maintenanceDutyName},
	"integrationDutyName":   {integrationDutyName},
	"learningDutyName":      {learningDutyName},
	"retentionDutyName":     {retentionDutyName},
	"trashreap.Name":        {trashreap.Name},
	"embedDutyName":         {embedDutyName},
	"objectCollectorDuty":   {objectCollectorDuty},
	"chatPruneDutyName":     {chatPruneDutyName},
	"codeRetentionDutyName": {codeRetentionDutyName},
	"name":                  identityDutyNames(),
}

// dutyPhrases is how the pages name each duty in prose, lower-cased: a list
// that names a duty uses one of these. Keyed by the duty's own name, so a
// duty dutyNames lists with no phrase here fails below rather than going
// unread.
var dutyPhrases = map[string][]string{
	schedule.DutyName:     {"scheduler"},
	waiterDutyName:        {"sandbox waiter"},
	maintenanceDutyName:   {"maintenance sweep"},
	integrationDutyName:   {"integration reconcile"},
	learningDutyName:      {"learning passes", "learning background passes", "skill clustering"},
	retentionDutyName:     {"trim"},
	trashreap.Name:        {"trash reaper"},
	embedDutyName:         {"embedding"},
	objectCollectorDuty:   {"collector"},
	chatPruneDutyName:     {"message horizon", "chat prune"},
	codeRetentionDutyName: {"code retention", "code-retention"},
	identitySweepDuty:     {"identity sweep"},
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

// EVERY HAND-KEPT LIST OF THE WORKERS ROLE NAMES EVERY DUTY.
//
// Beside the two tables above, the duties are listed in prose wherever a page
// says what the `workers` role runs: the fleet guide's and the scaling page's
// role rows, the configuration page's, the deployment guide's role bullet and
// its account of the singletons, the satellite guide's core column, the
// overview's diagram and the role's own doc in internal/seat/placement. Each
// was typed by hand and fell behind the engine in its own place (one left the
// trash reaper out, two the trash reaper and chat's horizon, one folded two
// sweeps into "the sweeps"), so each is read here, and every duty the engine
// claims must appear in each by one of the phrases the pages use for it
// ([dutyPhrases]).
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
