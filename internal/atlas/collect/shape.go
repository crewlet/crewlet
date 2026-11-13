package collect

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/redact"
)

// What every source does to a vendor's text before the builder judges it: a
// name made a label, an address stripped to what the atlas keeps, an
// environment's name read as the atlas's word for it. One copy, so two
// sources cannot shape the same text two ways.

// PlainLabel is a vendor's name for something as a label takes it. Vendors'
// conventions put a scope in square brackets and a threshold after an angle
// bracket, both of which a label refuses as markup: brackets become
// parentheses, the other markup characters are dropped, and a name past a
// label's length is cut. What is still refused (a link, a credential's shape)
// is withheld by the builder the name is handed to.
func PlainLabel(raw string) string {
	// A NAME SHAPED LIKE A CREDENTIAL IS HANDED ON AS IT IS, for the builder
	// to refuse: reshaping or cutting one could leave a piece no rule knows.
	if redact.Contains(raw) {
		return raw
	}
	name := strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		switch r {
		case '[':
			return '('
		case ']':
			return ')'
		case '<', '>', '`':
			return -1
		}
		return r
	}, raw)), " ")
	if runes := []rune(name); len(runes) > atlas.MaxLabelRunes {
		name = strings.TrimSpace(string(runes[:atlas.MaxLabelRunes-1])) + "…"
	}
	return name
}

// environments is the atlas's word for each name of an environment a read
// takes; any other name suggests nothing.
var environments = map[string]string{
	"prod": "production", "production": "production",
	"staging": "staging",
	"dev":     "development", "development": "development",
	"test": "test",
}

// EnvironmentWord is the atlas's word for an environment a vendor names
// exactly name, "" for a name that is none of them. A source whose vendor
// writes the names in mixed case lowers them first.
func EnvironmentWord(name string) string { return environments[name] }

// errNoAddress is a value that is not an address with a host: a templated
// one above all, which names a variable rather than a place.
var errNoAddress = errors.New("not an address with a scheme and a host")

// Address is raw as the atlas keeps an address: its scheme, host, port and
// path, WITHOUT its user information, query and fragment, which is where a
// vendor's copy of an address carries a credential; and the address's host.
func Address(raw string) (address, host string, err error) {
	u, perr := url.Parse(strings.TrimSpace(raw))
	if perr != nil || u.Scheme == "" || u.Hostname() == "" || u.Opaque != "" || strings.Contains(raw, "{{") {
		return "", "", errNoAddress
	}
	clean := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath}
	return clean.String(), u.Hostname(), nil
}

// ZoneOf is the zone a name sits in: the longest of zones the name is under,
// so a name in a zone that is itself under another sits in the nearer one.
func ZoneOf(name atlas.Ref, zones []atlas.Ref) (atlas.Ref, bool) {
	var best atlas.Ref
	found := false
	for _, zone := range zones {
		if name.Key == zone.Key || !strings.HasSuffix(name.Key, "."+zone.Key) {
			continue
		}
		if !found || len(zone.Key) > len(best.Key) {
			best, found = zone, true
		}
	}
	return best, found
}

// KeySegment is a vendor's name as one segment of an entry's key: letters,
// digits and . _ + - as they are, every other byte as '~' and its two hex
// digits. Exact both ways, so two names never share a key and a name with a
// space or a slash still has one.
func KeySegment(name string) string {
	const hex = "0123456789abcdef"
	var out strings.Builder
	for i := range len(name) {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '+', c == '-':
			out.WriteByte(c)
		default:
			out.WriteByte('~')
			out.WriteByte(hex[c>>4])
			out.WriteByte(hex[c&0xf])
		}
	}
	return out.String()
}

// Counted is base with every request it sends counted, and the count so far:
// what a read's standing says the read cost. The copy shares base's transport
// (the vendor clients' own where base has none) and nothing else.
func Counted(base *http.Client) (*http.Client, func() int) {
	var calls atomic.Int64
	counting := *base
	next := base.Transport
	if next == nil {
		next = httpx.Transport()
	}
	counting.Transport = countedTransport{next: next, calls: &calls}
	return &counting, func() int { return int(calls.Load()) }
}

// countedTransport counts the requests a read makes.
type countedTransport struct {
	next  http.RoundTripper
	calls *atomic.Int64
}

func (t countedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return t.next.RoundTrip(req)
}
