// Package envref is THE ${VAR} reference grammar — one definition, shared by
// every reader.
//
// A config value may POINT AT a secret instead of carrying it:
// "${SLACK_BOT_TOKEN_CEO}" is a reference, resolved at construction time.
// Several independent parts of the engine have to agree on what a reference
// looks like — the config resolver substitutes them, secret redaction SKIPS
// them (a reference is an inert pointer, not a secret at rest), provisioners
// MINT credentials into them, and the org model uses them to tell a contact
// identity from a literal.
//
// Written once per consumer, each carries its own regex and they disagree.
// That is never a style question; it is a correctness or security bug. Two
// real ones:
//
//   - `\$\{[^}]+\}` (redaction) matched braces the resolver ignores, so a
//     literal secret containing "${line#host=}" counted as a reference and
//     was displayed UNMASKED.
//   - `\$\{(\w+)\}` (provisioning) accepted "${1}", a name the resolver
//     would never substitute — so a provisioner could mint a live credential
//     into a variable nothing will ever read.
//
// This package imports nothing else from the engine, which is what lets
// every one of those layers depend on it.
package envref

import (
	"os"
	"regexp"
	"strings"
)

// pattern matches valid environment-variable NAMES only — the same
// identifier rule the shell uses.
//
// It deliberately does NOT match shell parameter expansions (${1:-x},
// ${line#host=}) or unbraced $VAR. Substituting those would silently mangle
// config-authored script content, and a sandbox setup step is exactly such
// content: an operator's helper script full of shell syntax must survive
// config resolution untouched.
var pattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// wholeRef is the anchored form: the value is EXACTLY one reference and
// nothing else.
var wholeRef = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

// Has reports whether a value carries at least one reference.
//
// "Carries a reference" means "substitution would actually change it",
// which is precisely the property display-redaction keys off when deciding a
// value is a pointer rather than a secret at rest.
func Has(value string) bool { return pattern.MatchString(value) }

// Whole returns the variable name when the value is EXACTLY one reference,
// and reports false otherwise.
//
// This is the capture contract provisioners mint into: a credential may only
// be written into a config slot that is a whole reference, because anything
// else — a URL with a token embedded in it, say — has no single variable to
// own the value.
func Whole(value string) (string, bool) {
	m := wholeRef.FindStringSubmatch(strings.TrimSpace(value))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Names returns every variable referenced by a value, in order of first
// appearance and without duplicates.
func Names(value string) []string {
	matches := pattern.FindAllStringSubmatch(value, -1)
	if matches == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if _, dup := seen[m[1]]; dup {
			continue
		}
		seen[m[1]] = struct{}{}
		out = append(out, m[1])
	}
	return out
}

// Expand substitutes every reference using lookup.
//
// A name lookup reports its value and whether it was found. An UNRESOLVED
// reference expands to the empty string — matching shell behaviour — and is
// reported in the returned unresolved list so callers
// can warn by NAME. That distinction matters: a value like
// "Bearer ${TOKEN}" with TOKEN unset resolves to "Bearer ", which is
// truthy-but-broken, and a caller that only checked for emptiness would pass
// it straight to an API.
func Expand(value string, lookup func(name string) (string, bool)) (expanded string, unresolved []string) {
	if !Has(value) {
		return value, nil
	}
	out := pattern.ReplaceAllStringFunc(value, func(match string) string {
		name := pattern.FindStringSubmatch(match)[1]
		v, ok := lookup(name)
		if !ok {
			unresolved = append(unresolved, name)
			return ""
		}
		return v
	})
	return out, unresolved
}

// Resolve reads a config value that may be a WHOLE ${VAR} reference.
//
// A literal comes back trimmed and unchanged. A whole reference comes back as
// the variable's value, or EMPTY when the variable is unset — never as its
// own literal text. That last rule is the reason this is one function rather
// than a call to [Whole] at each site: a raw "${SLACK_BOT_TOKEN_CEO}" matches
// nothing any vendor will accept, so passing it through turns a missing
// variable into a seat that mysteriously authenticates as nobody, at a layer
// far away from the config that named it.
//
// It was written twice, byte for byte, in the Slack and Mattermost
// transports — the second carrying a comment saying it resolved to empty
// "for the same reason it does everywhere else", which is an author noticing
// the duplication and copying anyway.
//
// A nil lookup reads the process environment, which is what every caller
// outside a test passes.
//
// NOT every caller wants this policy: internal/org deliberately keeps its own
// walk, because it also skips a reference that resolves to EMPTY — leaving
// the previous value in place rather than blanking it — which is correct
// there and wrong here.
func Resolve(value string, lookup func(name string) (string, bool)) string {
	value = strings.TrimSpace(value)
	name, isRef := Whole(value)
	if !isRef {
		return value
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	resolved, ok := lookup(name)
	if !ok {
		return ""
	}
	return strings.TrimSpace(resolved)
}
