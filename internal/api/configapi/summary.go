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
// A write must say what it changed: an operator reads the history at 3am. The
// summary travels in the `X-Summary` header or a top-level `_summary` body
// key. The body key exists because the body is often all a caller controls (a
// form post, `curl -d @file`, a proxy stripping headers).
//
// It has to be REMOVED, not ignored: Tier B's parser refuses unknown fields
// deliberately, so a `_summary` left in the document is rejected by name. It
// is lifted out before parsing.

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

// asText is a body no summary was lifted from and no document was parsed out
// of — an empty one, or one that does not parse — read by nobody but a reader
// of bytes, which reports its failure in its own words.
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
// body.
//
// ONE FUNCTION FOR EVERY ROUTE THAT READS A BODY, for the precedence: a copy
// that read the header FIRST would never lift `_summary` out, and the document
// parser would then fail on it as an unknown field.
//
// The key is lifted from a dry run's body too, so a check reads the document
// the write will. Whether a summary is REQUIRED is [requireSummary]'s.
//
// It returns the remaining body: a caller that ignored it would hand the
// parser a `_summary`. ok is false when the request has been answered.
func takeSummary(w http.ResponseWriter, r *http.Request, body []byte) (summary string, rest submitted, ok bool) {
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
	return summary, rest, true
}

// requireSummary refuses a write that would store a revision with no audit
// summary, answering the request itself; it reports whether the write may go
// on. Only the hint differs per route, so only the hint is a parameter.
//
// A dry run stores nothing, records no summary and never asks.
//
// ASKED LAST, once the write is admitted and its document checked, never as
// the body is read: asked first, a credential with no permission to write was
// answered `400 summary_required` and met the real `403` only after writing a
// sentence for a change it could never make.
func requireSummary(w http.ResponseWriter, summary, hint string) bool {
	if summary != "" {
		return true
	}
	httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeSummaryRequired,
		map[string]string{"hint": hint})
	return false
}
