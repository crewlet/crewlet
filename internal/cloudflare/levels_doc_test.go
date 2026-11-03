package cloudflare_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/cloudflare"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE INTEGRATION PAGE STATES THE LEVEL TABLE EXACTLY: the groups each level
// adds, the provisioner's own groups and every group a level grants, each
// where the token builder lists it and with the levels that grant it. A person
// builds the provisioner token from the page, so a group the page drops is one
// the token lacks.
func TestTheCloudflarePageStatesTheLevelTable(t *testing.T) {
	t.Parallel()
	page := cloudflarePage(t)
	ids, adds := cloudflare.LevelAdds()

	levelRows := tableRows(t, between(t, page, "### Each agent's access is a level", "\nAccount Settings Read is in"))
	if len(levelRows) != len(ids) {
		t.Fatalf("the page's level table has %d rows, the engine %d levels", len(levelRows), len(ids))
	}
	for i, row := range levelRows {
		if row[0] != "`"+ids[i]+"`" {
			t.Errorf("level row %d is %s, want `%s`", i, row[0], ids[i])
		}
		list := row[1]
		if i > 0 {
			lead := "Everything in `" + ids[i-1] + "`, plus "
			if !strings.HasPrefix(list, lead) {
				t.Errorf("%s does not start %q: %q", ids[i], lead, list)
			}
			list = strings.TrimPrefix(list, lead)
		}
		if got := listed(list); !slices.Equal(got, adds[i]) {
			t.Errorf("the page says %s adds %q, the engine %q", ids[i], got, adds[i])
		}
	}

	own := tableRows(t, between(t, page, "### Creating the provisioner token", "\nThen add **every permission"))
	var ownNames []string
	for _, row := range own {
		ownNames = append(ownNames, row[0])
		if want := cloudflare.BuilderPath(row[0]); row[1] != want {
			t.Errorf("the page places %s at %q in the builder, want %q", row[0], row[1], want)
		}
	}
	sameSet(t, "provisioner's own group", ownNames, cloudflare.ProvisionerOwnGroups())

	granted := tableRows(t, between(t, page, "\nThen add **every permission", "\nZone Read and Account Settings Read"))
	var grantedNames []string
	for _, row := range granted {
		grantedNames = append(grantedNames, row[0])
		if want := cloudflare.BuilderPath(row[0]); row[1] != want {
			t.Errorf("the page places %s at %q in the builder, want %q", row[0], row[1], want)
		}
		var by []string
		for _, id := range ids {
			if groups, _ := cloudflare.Groups(id); slices.Contains(groups, row[0]) {
				by = append(by, id)
			}
		}
		var said []string
		for _, m := range codeSpan.FindAllStringSubmatch(row[2], -1) {
			said = append(said, m[1])
		}
		if !slices.Equal(said, by) {
			t.Errorf("the page says %v grant %s, the engine %v", said, row[0], by)
		}
	}
	sameSet(t, "group a level grants", grantedNames, slices.DeleteFunc(cloudflare.ProvisionerGroups(),
		func(g string) bool { return slices.Contains(cloudflare.ProvisionerOwnGroups(), g) }))
}

var codeSpan = regexp.MustCompile("`([^`]+)`")

func cloudflarePage(t *testing.T) string {
	t.Helper()
	path := filepath.Join(sourcetree.Root(t), "docs", "integrations", "cloudflare.md")
	data, err := os.ReadFile(path) //nolint:gosec // a page this repository ships
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// between is the text after start and before the next end, failing when
// either is missing so a moved heading never leaves the check reading nothing.
func between(t *testing.T, text, start, end string) string {
	t.Helper()
	_, after, ok := strings.Cut(text, start)
	if !ok {
		t.Fatalf("the Cloudflare page no longer contains %q", start)
	}
	body, _, ok := strings.Cut(after, end)
	if !ok {
		t.Fatalf("nothing ends the Cloudflare page's %q section at %q", start, end)
	}
	return body
}

// tableRows is each body row of the one table in section, its cells trimmed.
func tableRows(t *testing.T, section string) [][]string {
	t.Helper()
	var rows [][]string
	for line := range strings.SplitSeq(section, "\n") {
		if !strings.HasPrefix(line, "|") || strings.HasPrefix(line, "|---") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		rows = append(rows, cells)
	}
	if len(rows) < 2 {
		t.Fatalf("no table in %q", section)
	}
	return rows[1:]
}

// listed is a sentence's list of names. Only its last "and" separates, since
// a name may hold one ("SSL and Certificates Read").
func listed(list string) []string {
	parts := strings.Split(list, ", ")
	last := parts[len(parts)-1]
	if at := strings.LastIndex(last, " and "); at >= 0 {
		parts = append(parts[:len(parts)-1], last[:at], last[at+len(" and "):])
	}
	return parts
}

func sameSet(t *testing.T, what string, page, engine []string) {
	t.Helper()
	for _, name := range page {
		if !slices.Contains(engine, name) {
			t.Errorf("the page lists the %s %q, which the engine does not", what, name)
		}
	}
	for _, name := range engine {
		if !slices.Contains(page, name) {
			t.Errorf("the page does not list the %s %q", what, name)
		}
	}
}
