package configapi

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Where a revision summary comes from.
//
// # Two channels, one meaning
//
// A write to this surface must say what it changed — the revision history is
// what an operator reads at 3am to find the change that broke something, and
// a list of revisions with no summaries is a list of uuids. The summary
// travels in the `X-Summary` header, or as a top-level `_summary` key in the
// body.
//
// # Why the body key exists at all
//
// Because the body is often the only thing a caller controls. A form post, a
// `curl -d @file`, a browser `fetch` behind a proxy that strips unknown
// headers, a CI step piping a document through a tool that does not take
// header arguments — all of them can put a key in a document and none of them
// can necessarily add a header. Requiring the header makes the surface
// unusable from those callers for a reason that has nothing to do with the
// write.
//
// # It has to be REMOVED, not ignored
//
// Tier B's parser refuses unknown fields, deliberately: a mistyped setting
// that silently did nothing is the failure this build refuses to have. So a
// `_summary` left in the document would be rejected by name — the key would
// be actively hostile rather than merely unused. It is lifted out before the
// document is parsed.

// summaryKey is the body key a caller may put a revision summary under.
const summaryKey = "_summary"

// splitSummary lifts a top-level `_summary` out of a request body.
//
// The body is returned UNTOUCHED when the key is absent, which is the common
// case and the one that matters: re-encoding would renumber every line, and
// the parser's errors name lines. Only a body that actually carries the key
// pays for a round trip.
func splitSummary(body []byte) (string, []byte, error) {
	if len(body) == 0 {
		return "", body, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil {
		// NOT AN ERROR HERE. A body that does not parse is the document
		// parser's report to make, in its own words and with its own line
		// numbers; failing here would replace a precise message with a
		// vague one.
		//
		//nolint:nilerr // Deliberate: see the paragraph above.
		return "", body, nil
	}
	mapping := rootMapping(&doc)
	if mapping == nil {
		return "", body, nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != summaryKey {
			continue
		}
		value := mapping.Content[i+1]
		if value.Kind != yaml.ScalarNode {
			return "", nil, fmt.Errorf(
				"%s must be a string naming what this write changes", summaryKey)
		}
		summary := strings.TrimSpace(value.Value)
		mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
		rest, err := yaml.Marshal(&doc)
		if err != nil {
			return "", nil, fmt.Errorf("removing %s: %w", summaryKey, err)
		}
		return summary, rest, nil
	}
	return "", body, nil
}

// rootMapping is the document's top-level mapping, or nil for anything else.
func rootMapping(doc *yaml.Node) *yaml.Node {
	node := doc
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return nil
		}
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	return node
}
