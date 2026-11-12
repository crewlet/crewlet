package collect

import (
	"errors"
	"net/url"
	"strings"

	"github.com/crewlet/crewlet/internal/atlas"
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
