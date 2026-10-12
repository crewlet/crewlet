package slack_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/slack"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE INTEGRATION PAGE LISTS EXACTLY WHAT THE MANIFEST REQUESTS.
//
// docs/integrations/slack.md calls BotScopes and BotEvents its single source
// of truth, and an operator building an app by hand copies the page, not the
// Go. Nothing held the two together, so they could part in either direction
// without a sound: a scope the page lists and the manifest dropped is one a
// hand-built app still grants, and a scope the manifest gained and the page
// never named is one a hand-built app lacks — which, for the conversation
// scopes, is a server that exits at startup. Read in both directions, so a
// scope added on one side alone fails as surely as one removed.
func TestTheSlackPageListsTheManifestScopesAndEvents(t *testing.T) {
	t.Parallel()
	page := slackPage(t)

	section := between(t, page, "### Bot scopes and events", "\n## ")
	var documented []string
	for line := range strings.SplitSeq(section, "\n") {
		cells := strings.Split(line, "|")
		// A table row is | first | second |, so a row splits into at
		// least four pieces; the header and its rule name no scope.
		if len(cells) < 4 || strings.Contains(cells[1], "Scope") || strings.Contains(cells[1], "---") {
			continue
		}
		for _, m := range codeSpan.FindAllStringSubmatch(cells[1], -1) {
			documented = append(documented, m[1])
		}
	}
	sameSet(t, "bot scope", documented, slack.BotScopes)

	eventsLine := between(t, section, "Bot events:", "\n")
	var events []string
	// The line goes on to explain itself; the events are the spans before
	// that explanation starts.
	for _, m := range codeSpan.FindAllStringSubmatch(strings.SplitN(eventsLine, " — ", 2)[0], -1) {
		events = append(events, m[1])
	}
	sameSet(t, "bot event", events, slack.BotEvents)
}

var codeSpan = regexp.MustCompile("`([^`]+)`")

func slackPage(t *testing.T) string {
	t.Helper()
	path := filepath.Join(sourcetree.Root(t), "docs", "integrations", "slack.md")
	data, err := os.ReadFile(path) //nolint:gosec // a page this repository ships
	if err != nil {
		t.Fatalf("read %s: %v — this case certifies a page this repository "+
			"ships, so a missing one is a rename that was not swept", path, err)
	}
	return string(data)
}

// between returns the text after the first start and before the next end,
// and fails when either is missing: a heading that moved must not leave the
// check reading nothing and passing.
func between(t *testing.T, text, start, end string) string {
	t.Helper()
	_, after, ok := strings.Cut(text, start)
	if !ok {
		t.Fatalf("the Slack page no longer contains %q", start)
	}
	body, _, ok := strings.Cut(after, end)
	if !ok {
		t.Fatalf("nothing ends the Slack page's %q section at %q", start, end)
	}
	return body
}

func sameSet(t *testing.T, what string, documented, requested []string) {
	t.Helper()
	if len(documented) == 0 {
		t.Fatalf("read no %ss from the Slack page — this check was certifying nothing", what)
	}
	for _, d := range documented {
		if !slices.Contains(requested, d) {
			t.Errorf("the Slack page lists %s %q, which the manifest does not request", what, d)
		}
	}
	for _, r := range requested {
		if !slices.Contains(documented, r) {
			t.Errorf("the manifest requests %s %q, which the Slack page does not list", what, r)
		}
	}
}
