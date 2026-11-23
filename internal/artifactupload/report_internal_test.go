package artifactupload

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactupload/lint"
)

// A PUBLISH'S WARNINGS ARE CUT TO WHAT ITS RECORD KEEPS, in its first answer
// as in a retry's: the first MaxReportWarnings, each message at most
// MaxWarningBytes, and one counting the rest. Fewer are answered whole.
//
// Mutation: answer r.warnings uncut from report, and this is red.
func TestAPublishsWarningsAreCutToWhatItsRecordKeeps(t *testing.T) {
	t.Parallel()
	var found []lint.Finding
	for i := range MaxReportWarnings + 10 {
		found = append(found, lint.Finding{Path: fmt.Sprintf("page-%02d.html", i), Code: lint.CodeRemoteBlocked,
			Message: "https://example.com/" + strings.Repeat("a", 2*MaxWarningBytes)})
	}
	got := (&run{warnings: found}).report().Warnings
	if len(got) != MaxReportWarnings+1 {
		t.Fatalf("%d warnings are answered as %d, want %d and one counting the rest", len(found), len(got),
			MaxReportWarnings)
	}
	for _, f := range got[:MaxReportWarnings] {
		if len(f.Message) > MaxWarningBytes {
			t.Errorf("a warning on %s is answered with %d bytes of message, past %d", f.Path, len(f.Message),
				MaxWarningBytes)
		}
	}
	if last := got[MaxReportWarnings]; last.Code != lint.CodeMoreWarnings ||
		last.Path != found[MaxReportWarnings].Path || !strings.Contains(last.Message, "and 10 more") {
		t.Errorf("the last warning answered is %+v, want one counting the 10 more from %s", last,
			found[MaxReportWarnings].Path)
	}
	few := found[:3]
	if got := (&run{warnings: few}).report().Warnings; len(got) != len(few) || got[0].Path != few[0].Path {
		t.Errorf("%d warnings are answered as %+v, want them whole", len(few), got)
	}
}
