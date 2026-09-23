package configapi

import (
	"fmt"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
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

// submitted is a write's request body with any `_summary` lifted out of it.
//
// TWO FORMS OF ONE BODY, because its two readers need different things. A
// merge needs bytes, and gets the body re-encoded when a key was lifted out.
// The document reader needs the lines the caller wrote, because every failure
// it reports names one: a re-encoded body is renumbered, so a typo twenty lines
// into a YAML body opening with its summary was reported a line or two above
// where it was written. It reads the parsed document instead, whose nodes keep
// their lines ([config.ParseCompanyNode]).
type submitted struct {
	// text is the body, re-encoded when a summary was lifted out of it.
	text []byte
	// doc is the document parsed from the body as it was sent, less its
	// summary. Nil when the body does not parse, which the reader reports
	// from text, in its own words and with its own lines.
	doc *yaml.Node
}

// asText is a body no summary was lifted from, read by nobody but a reader of
// bytes: what a programmatic caller hands a write.
func asText(body []byte) submitted { return submitted{text: body} }

// company reads the company a body carries, from the document as it was sent
// when there is one.
func (s submitted) company() (*config.Company, error) {
	if s.doc != nil {
		return config.ParseCompanyNode(s.doc)
	}
	return parseDocument(s.text)
}

// splitSummary lifts a top-level `_summary` out of a request body.
//
// The text is returned UNTOUCHED when the key is absent, which is the common
// case, and only a body that actually carries the key pays for a round trip.
// The parsed document comes back either way, so the reader never has to
// parse the body again, and reads the lines it was sent with.
func splitSummary(body []byte) (string, submitted, error) {
	if len(body) == 0 {
		return "", asText(body), nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil {
		// NOT AN ERROR HERE. A body that does not parse is the document
		// parser's report to make, in its own words and with its own line
		// numbers; failing here would replace a precise message with a
		// vague one.
		//
		//nolint:nilerr // Deliberate: see the paragraph above.
		return "", asText(body), nil
	}
	mapping := rootMapping(&doc)
	if mapping == nil {
		return "", submitted{text: body, doc: &doc}, nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != summaryKey {
			continue
		}
		value := mapping.Content[i+1]
		if value.Kind != yaml.ScalarNode {
			// A FAULT, placed at the key, so the refusal's problem points
			// at the one line that is wrong rather than at the document.
			return "", submitted{}, &config.Fault{
				Path: config.Path{summaryKey}, Kind: config.ErrShape, Line: value.Line,
				Detail: "must be a string naming what this write changes",
			}
		}
		summary := strings.TrimSpace(value.Value)
		mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
		rest, err := yaml.Marshal(&doc)
		if err != nil {
			return "", submitted{}, fmt.Errorf("removing %s: %w", summaryKey, err)
		}
		return summary, submitted{text: rest, doc: &doc}, nil
	}
	return "", submitted{text: body, doc: &doc}, nil
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

// takeSummary takes the summary for a write, lifting `_summary` out of the
// body, and answers the request itself when a required summary is missing.
//
// ONE FUNCTION FOR EVERY ROUTE THAT READS A BODY, and the reason is the
// precedence rather than the boilerplate: several copies of "split the body,
// then let the header win, then refuse if empty" are several chances for one
// of them to read the header FIRST and never lift `_summary` out, which
// parses green and then fails at the document parser, by name, as an unknown
// field. Only the hint differs per route, so only the hint is a parameter.
//
// required is false for a dry run, which stores nothing and so records no
// summary. The key is still lifted out, because the document a check reads
// has to be the one the write will read.
//
// It returns the remaining body, because splitting is what removes the key:
// a caller that ignored the second result would hand the parser a document
// with a `_summary` in it. ok is false when the request has been answered.
func takeSummary(w http.ResponseWriter, r *http.Request, body []byte, required bool, hint string) (summary string, rest submitted, ok bool) {
	summary, rest, err := splitSummary(body)
	if err != nil {
		refuseDocument(w, httpjson.CodeInvalidBody, err.Error(), "", &DocumentError{Err: err})
		return "", submitted{}, false
	}
	if header := r.Header.Get("X-Summary"); header != "" {
		// THE HEADER WINS when both are present. It is the more explicit
		// channel — a `_summary` can survive in a document somebody keeps
		// in version control long after it stopped describing the write.
		summary = header
	}
	if summary == "" && required {
		// Required, because the history is what an operator reads at 3am
		// to find the change that broke something. A list of revisions
		// with no summaries is a list of uuids.
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeSummaryRequired,
			map[string]string{"hint": hint})
		return "", submitted{}, false
	}
	return summary, rest, true
}
