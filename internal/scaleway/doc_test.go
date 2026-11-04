package scaleway_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/scaleway"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// releaseSums is each release file of the pinned scw release an engine host or
// a box runs, with its SHA-256 from the release's SHA256SUMS.
var releaseSums = map[string]string{
	"linux_amd64":  "184e697e6ab8a8e825be1c5cf61d104341abe3598bb0e20b7a54618cb05231b3",
	"linux_arm64":  "28f2115b7fce9d23f5726dc38566c7c973f579804be78a46f908c1a57da71893",
	"darwin_amd64": "90dc1d10c622cbd6b8dc42490fd2b1f7803e1ea2201952b466e39730fa394c47",
	"darwin_arm64": "17b756a7a02659e4db49b27e92d0039776a124f79587ee3366fda83c2d2e3e0b",
}

// releaseSumsAt is the release [releaseSums] was read from: moving the pin is
// reading the new release's SHA256SUMS into the table.
const releaseSumsAt = "2.65.2"

// scwDownload is a download of a scw release file, with the release in its
// path, the release in its name, and the platform, a variable included.
var scwDownload = regexp.MustCompile(`scaleway-cli/releases/download/v([^/\s]+)/scaleway-cli_([^_\s]+)_([a-z]+)_([^\s"']+)`)

// EVERY PAGE, SKILL AND EXAMPLE THAT DOWNLOADS SCW NAMES THE ONE PINNED
// RELEASE AND CHECKS IT AGAINST ITS PUBLISHED SHA-256, so what the docs say an
// agent and an engine host run is what [scaleway.CLIRelease] records.
func TestEveryScwDownloadNamesThePinnedRelease(t *testing.T) {
	t.Parallel()
	if releaseSumsAt != scaleway.CLIRelease {
		t.Fatalf("the checksums are release %s's and the pin is %s: read the new release's SHA256SUMS",
			releaseSumsAt, scaleway.CLIRelease)
	}
	root := sourcetree.Root(t)
	downloads := 0
	for _, dir := range []string{"docs", "skills", "examples"} {
		err := sourcetree.Walk(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".md") && !strings.HasSuffix(path, ".yaml") {
				return nil
			}
			body, err := os.ReadFile(path) //nolint:gosec // a file this repository ships
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			// THE CHECK BESIDE THE DOWNLOAD: a sum elsewhere on the page
			// verifies nothing the reader runs.
			for _, block := range blocksOf(string(body)) {
				for _, m := range scwDownload.FindAllStringSubmatch(block, -1) {
					downloads++
					if m[1] != scaleway.CLIRelease || m[2] != scaleway.CLIRelease {
						t.Errorf("%s downloads %q; name release %s, the one internal/scaleway pins", rel, m[0], scaleway.CLIRelease)
					}
					for _, platform := range platformsOf(m[3], m[4]) {
						if !strings.Contains(block, releaseSums[platform]) {
							t.Errorf("%s downloads the %s release file and does not check it against its SHA-256 "+
								"in the same block", rel, platform)
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if downloads == 0 {
		t.Fatal("no page downloads scw, so nothing was checked")
	}
	for _, line := range []string{
		"scaleway-cli/releases/download/v2.60.0/scaleway-cli_2.60.0_linux_amd64",
		"scaleway-cli/releases/download/v" + scaleway.CLIRelease + "/scaleway-cli_2.60.0_linux_amd64",
	} {
		if m := scwDownload.FindStringSubmatch(line); m == nil || m[1] == scaleway.CLIRelease && m[2] == scaleway.CLIRelease {
			t.Errorf("%q was not read as a download of another release", line)
		}
	}

	// THE CATALOGUE'S ENTRY IS READ AT THE SAME RELEASE, and serves the
	// namespaces the page declares.
	i := slices.IndexFunc(catalogue.Curated(), func(e catalogue.Entry) bool { return e.ID == "scaleway" })
	if i < 0 {
		t.Fatal("the curated catalogue has no scaleway entry")
	}
	entry := catalogue.Curated()[i]
	if !strings.Contains(entry.Docs, "v"+scaleway.CLIRelease+"/") || !strings.Contains(entry.Prerequisite, scaleway.CLIRelease) {
		t.Errorf("the catalogue entry reads %q and asks for %q, not release %s", entry.Docs, entry.Prerequisite, scaleway.CLIRelease)
	}
	page := scalewayPage(t)
	for _, arg := range entry.Args {
		if strings.HasPrefix(arg, "namespaces=") && !strings.Contains(page, `"`+arg+`"`) {
			t.Errorf("the page does not declare the catalogue's %s", arg)
		}
	}
	table := tableRows(t, between(t, page, "| Platform | Release file | SHA-256 |", "\n\n"))
	if len(table) != len(releaseSums) {
		t.Errorf("the page's checksum table has %d rows, want %d", len(table), len(releaseSums))
	}
	for _, row := range table {
		platform := strings.TrimSuffix(strings.TrimPrefix(strings.Trim(row[1], "`"), "scaleway-cli_"+scaleway.CLIRelease+"_"), "")
		if want := releaseSums[platform]; want == "" || row[2] != "`"+want+"`" {
			t.Errorf("the page gives %s the SHA-256 %s", row[1], row[2])
		}
	}
}

// blocksOf is each fenced code block of a page, and a YAML file whole.
func blocksOf(body string) []string {
	if !strings.Contains(body, "```") {
		return []string{body}
	}
	var out []string
	parts := strings.Split(body, "```")
	for i := 1; i < len(parts); i += 2 {
		out = append(out, parts[i])
	}
	return out
}

// platformsOf is the release files a download names: one where its
// architecture is spelled, every one of its system's where a variable picks it.
func platformsOf(system, arch string) []string {
	if !strings.HasPrefix(arch, "$") {
		return []string{system + "_" + arch}
	}
	var out []string
	for platform := range releaseSums {
		if strings.HasPrefix(platform, system+"_") {
			out = append(out, platform)
		}
	}
	slices.Sort(out)
	return out
}

// THE INTEGRATION PAGE STATES THE LEVEL TABLE EXACTLY: the sets each level
// adds, the organization sets every level grants, and the provisioner's three.
// A person checks an agent's policy against the page, so a set the page drops
// is one they would not know an agent holds.
func TestTheScalewayPageStatesTheLevelTable(t *testing.T) {
	t.Parallel()
	page := scalewayPage(t)
	ids, adds := scaleway.LevelAdds()
	rows := tableRows(t, between(t, page, "### Each agent's access is a level", "\nEvery level also grants"))
	if len(rows) != len(ids) {
		t.Fatalf("the page's level table has %d rows, the engine %d levels", len(rows), len(ids))
	}
	for i, row := range rows {
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
	orgLine := strings.TrimPrefix(between(t, page, "\nEvery level also grants ", " at Organization scope"),
		"\nEvery level also grants ")
	if got := listed(orgLine); !slices.Equal(got, scaleway.OrgSets()) {
		t.Errorf("the page says every level grants %q across the Organization, the engine %q", got, scaleway.OrgSets())
	}
	var own []string
	for _, row := range tableRows(t, between(t, page, "| Permission set | Why Crewlet needs it |", "\n\n")) {
		own = append(own, row[0])
	}
	if !slices.Equal(own, scaleway.ProvisionerSets()) {
		t.Errorf("the page tells a person to grant the provisioner %q, the engine needs %q", own, scaleway.ProvisionerSets())
	}
	for _, level := range scaleway.Levels() {
		if !strings.Contains(page, "`"+level+"`") {
			t.Errorf("the page never names the level %s", level)
		}
	}
}

func scalewayPage(t *testing.T) string {
	t.Helper()
	path := filepath.Join(sourcetree.Root(t), "docs", "integrations", "scaleway.md")
	data, err := os.ReadFile(path) //nolint:gosec // a page this repository ships
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// between is start and the text after it up to the next end, failing when
// either is missing so a moved heading never leaves the check reading nothing.
func between(t *testing.T, text, start, end string) string {
	t.Helper()
	_, after, ok := strings.Cut(text, start)
	if !ok {
		t.Fatalf("the Scaleway page no longer contains %q", start)
	}
	body, _, ok := strings.Cut(after, end)
	if !ok {
		t.Fatalf("nothing ends the Scaleway page's %q section at %q", start, end)
	}
	return start + body
}

// tableRows is each body row of the one table in section, its cells trimmed.
func tableRows(t *testing.T, section string) [][]string {
	t.Helper()
	var rows [][]string
	for line := range strings.SplitSeq(section, "\n") {
		line = strings.TrimSpace(line)
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

// listed is a sentence's list of names, split at its commas and its last "and".
func listed(list string) []string {
	parts := strings.Split(strings.TrimSpace(list), ", ")
	last := parts[len(parts)-1]
	if at := strings.LastIndex(last, " and "); at >= 0 {
		parts = append(parts[:len(parts)-1], last[:at], last[at+len(" and "):])
	}
	return parts
}
