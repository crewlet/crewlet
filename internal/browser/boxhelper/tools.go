package boxhelper

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"
)

// Tools is every tool a box's browser server lists its model: serve.mjs's
// ALLOWED, read from the module the engine embeds and writes into a box, so
// what the engine counts is the list a box serves, never a copy of it. A
// launch weighs it against a harness that hides every MCP server's tools past
// a count of them (internal/sandbox's EnsureBrowser).
func Tools() []string { return slices.Clone(tools()) }

// tools is [Tools], read once. The module is embedded from this repository,
// so a list this reader refuses is a build whose module it was not written
// for, which TestTheEngineReadsTheBrowsersToolsWholeFromItsModule refuses: it
// panics.
var tools = sync.OnceValue(func() []string {
	module, err := js.ReadFile("js/serve.mjs")
	if err != nil {
		panic(fmt.Sprintf("boxhelper: js/serve.mjs is not embedded: %v", err))
	}
	names, err := allowedTools(module)
	if err != nil {
		panic("boxhelper: " + err.Error())
	}
	return names
})

// The lines ALLOWED opens and closes on.
const (
	allowedOpen  = "export const ALLOWED = new Set(["
	allowedClose = "])"
)

// allowedLine is each line between them: one tool's name, quoted, then a
// comma. Built at first use, as the ensure line's grammars are, so crewlet's
// every start pays nothing for it at init.
var allowedLine = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^  "([a-z][a-z0-9_]*)",$`)
})

// allowedTools is the names module's ALLOWED lists, one a line. Any other line
// in the list is refused rather than skipped, since a reader that returns
// part of the list counts fewer tools than a box serves; so is a name listed
// twice, which the Set holds once.
func allowedTools(module []byte) ([]string, error) {
	_, rest, found := bytes.Cut(module, []byte("\n"+allowedOpen+"\n"))
	if !found {
		return nil, errors.New("serve.mjs declares no ALLOWED this reader reads: " + allowedOpen +
			" on a line of its own")
	}
	list, _, closed := bytes.Cut(rest, []byte("\n"+allowedClose+"\n"))
	if !closed {
		return nil, errors.New("serve.mjs's ALLOWED is not closed by " + allowedClose + " on a line of its own")
	}
	var names []string
	for line := range bytes.SplitSeq(list, []byte("\n")) {
		m := allowedLine().FindSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("serve.mjs's ALLOWED holds the line %q, where each is one name, quoted, "+
				"then a comma", string(line))
		}
		name := string(m[1])
		if slices.Contains(names, name) {
			return nil, fmt.Errorf("serve.mjs's ALLOWED lists %s twice", name)
		}
		names = append(names, name)
	}
	return names, nil
}
