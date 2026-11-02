package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE SATELLITE GUIDE'S BOOT LINE NAMES EVERY REGISTERED LOG, IN THE
// REGISTER'S ORDER.
//
// A `roles: [data, seats]` satellite serves no API, so the guide sends its
// operator to the `statelog_started` line a data node logs at boot, and prints
// one to compare against. Typed by hand, the printed line named five logs on a
// register of eight, and an operator holding their node's line beside it would
// read every log after the fifth as one their node should not be running. The
// guide prints what [stateLog]'s boot logs: `domains=` and the register's
// names, in order.
//
// Mutation: drop a name from the guide's line, or register a log the line
// does not name, and this fails naming the line.
func TestTheSatelliteGuidePrintsTheLogsADataNodeStarts(t *testing.T) {
	t.Parallel()
	page, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "docs", "guides",
		"satellite-nodes.md"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, domain := range registeredDomains() {
		names = append(names, domain.Name())
	}
	want := `domains="[` + strings.Join(names, " ") + `]"`
	var lines []string
	for _, line := range strings.Split(string(page), "\n") {
		if strings.HasPrefix(line, "statelog_started") {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		t.Fatal("docs/guides/satellite-nodes.md prints no statelog_started line, so " +
			"this case holds nothing")
	}
	for _, line := range lines {
		if !strings.Contains(line, want) {
			t.Errorf("docs/guides/satellite-nodes.md prints %q, want it to carry %s: "+
				"a data node starts every registered log, in the register's order", line, want)
		}
	}
}
