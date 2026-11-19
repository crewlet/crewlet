package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/research"
)

// THE ADDRESSES A PAGE READ MAY GO TO. A page is read only at an address the
// engine saw in this turn from a source other than the model's own writing:
// the person's messages in the conversation and their page context, a search
// result its search tool returned, a link of a page the turn read, and a URL
// field of an API answer the turn read. An address the model composed after
// reading the company's data (`https://evil.example/?q=<data>`) is refused, so
// a page read is never a way out for that data; an address planted in a page
// was written before its author could see any.

// composedAddress is the refusal of a page read at an address nobody gave.
const composedAddress = "This address did not come from you, a search or a page, so it " +
	"was not read: search for it first."

// errComposedAddress classifies the refusal of a composed address.
var errComposedAddress = errors.New("the address did not come from the person, a search or a page")

// addressIn finds https addresses in prose.
var addressIn = regexp.MustCompile("https://[^\\s<>\"'`]+")

// addressesIn are the https addresses text holds, trailing punctuation dropped.
func addressesIn(text string) []string {
	var out []string
	for _, raw := range addressIn.FindAllString(text, -1) {
		out = append(out, strings.TrimRight(raw, ".,;:!?)]}"))
	}
	return out
}

// seenKey is raw as the seen set keys it, or false for an address no page read
// would take.
func seenKey(raw string) (string, bool) {
	u, err := research.Public(raw)
	if err != nil {
		return "", false
	}
	return u.String(), true
}

// see records raws as seen in this turn.
func (t *turn) see(raws ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seenAddresses == nil {
		t.seenAddresses = map[string]bool{}
	}
	for _, raw := range raws {
		if key, ok := seenKey(raw); ok {
			t.seenAddresses[key] = true
		}
	}
}

// seen reports whether raw was seen in this turn.
func (t *turn) seen(raw string) bool {
	key, ok := seenKey(raw)
	if !ok {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.seenAddresses[key]
}

// seePerson records the addresses the person wrote: their messages in the
// conversation, this question, and the page context they sent with it.
func (t *turn) seePerson(messages []assistantdomain.Message, question string,
	page *wire.PageContext) {

	for _, m := range messages {
		if m.Role == wire.RoleUser {
			t.see(addressesIn(m.Text)...)
		}
	}
	t.see(addressesIn(question)...)
	if page != nil {
		if raw, err := json.Marshal(page); err == nil {
			t.see(addressesIn(string(raw))...)
		}
	}
}

// seeAnswer records the addresses a read's answer gave the turn: a search's
// results, a page's links, or an API answer's URL fields. Never the answer's
// prose, and never an echo of the call's own arguments.
func (t *turn) seeAnswer(tool catalog.Tool, body []byte) {
	var doc any
	if json.Unmarshal(body, &doc) != nil {
		return
	}
	switch tool.Name {
	case catalog.SearchWebToolName:
		results, _ := field(doc, "results").([]any)
		for _, r := range results {
			if u, ok := field(r, "url").(string); ok {
				t.see(u)
			}
		}
	case catalog.FetchWebPageToolName:
		links, _ := field(doc, "links").([]any)
		for _, l := range links {
			if u, ok := l.(string); ok {
				t.see(u)
			}
		}
	case catalog.ReadAPITool:
		var found []string
		urlFields(doc, &found)
		t.see(found...)
	}
}

// field is v's member name, where v is an object.
func field(v any, name string) any {
	m, _ := v.(map[string]any)
	return m[name]
}

// urlFields collects every string an answer holds under a key that names an
// address: url, href, link, or one ending _url or _link.
func urlFields(v any, out *[]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			key := strings.ToLower(k)
			if s, ok := child.(string); ok && (key == "url" || key == "href" || key == "link" ||
				strings.HasSuffix(key, "_url") || strings.HasSuffix(key, "_link")) {
				*out = append(*out, s)
				continue
			}
			urlFields(child, out)
		}
	case []any:
		for _, child := range t {
			urlFields(child, out)
		}
	}
}

// chargeSearch charges the running segment's meter the tokens a web search's
// run spent, as its answer states them: the search ran on the person's model
// for this turn, so it is the turn's spend.
func (t *turn) chargeSearch(ctx context.Context, body []byte) {
	var answer struct {
		Tokens int `json:"tokens"`
	}
	t.mu.Lock()
	meter := t.meter
	t.mu.Unlock()
	if meter == nil || json.Unmarshal(body, &answer) != nil || answer.Tokens <= 0 {
		return
	}
	if _, err := meter.Spend(ctx, answer.Tokens); err != nil {
		t.svc.log.WarnContext(ctx, "assistant_search_charge_failed", "turn", t.id,
			"error", err)
	}
}
