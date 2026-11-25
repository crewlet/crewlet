package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE DOCS QUOTE WHAT A RUN IS TOLD OF ITS BROWSER: docs/concepts/
// code-sandbox.md quotes the paragraph a ready browser's run is told and the
// line a withheld one's is, each as the run's standing instructions carry it,
// the reason's words standing as "*the reason*", so a change to either text
// changes the page in the same commit.
//
// Mutations, each red: change a word of browserReadyBrief; change the
// withheld line's frame in Brief.
func TestTheDocsQuoteWhatARunIsToldOfItsBrowser(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "docs", "concepts", "code-sandbox.md"))
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	if !strings.Contains(page, "\n> "+browserReadyBrief+"\n") {
		t.Errorf("code-sandbox.md quotes no paragraph that is the one a ready browser's run is told:\n%s",
			browserReadyBrief)
	}
	withheld := BrowserOutcome{Record: types.ExecutorBrowser{State: browser.EnsureWithheld,
		Reason: browser.ReasonCellNotContainer}}
	line := strings.Replace(withheld.Brief(), browserWithheldWords(browser.ReasonCellNotContainer), "*the reason*", 1)
	if !strings.Contains(page, "\n> "+line+"\n") {
		t.Errorf("code-sandbox.md quotes no line that is the one a withheld browser's run is told:\n%s", line)
	}
}
