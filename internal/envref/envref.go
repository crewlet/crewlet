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

// NameGrammar is the NAME a reference carries between its braces, as a
// regular expression: the identifier rule the shell uses.
//
// EXPORTED FOR THE DASHBOARD'S GATE, which holds the screens' own copy of it
// (`contract/references.ts`) to this one: a screen deciding whether a value
// is a reference judged by a grammar of its own, and one that drifted would
// mask a reference as a secret or show a secret as a reference.
const NameGrammar = `[A-Za-z_][A-Za-z0-9_]*`

// pattern matches valid environment-variable NAMES only — the same
// identifier rule the shell uses.
//
// It deliberately does NOT match shell parameter expansions (${1:-x},
// ${line#host=}) or unbraced $VAR. Substituting those would silently mangle
// config-authored script content, and a sandbox setup step is exactly such
// content: an operator's helper script full of shell syntax must survive
// config resolution untouched.
var pattern = regexp.MustCompile(`\$\{(` + NameGrammar + `)\}`)

// wholeRef is the anchored form: the value is EXACTLY one reference and
// nothing else.
var wholeRef = regexp.MustCompile(`^\$\{(` + NameGrammar + `)\}$`)

// namePattern is the same identifier rule with no braces around it: the
// NAME on its own, as a secret store row is keyed and as the environment
// spells it.
var namePattern = regexp.MustCompile(`^` + NameGrammar + `$`)

// ValidName reports whether a reference could ever name this variable.
//
// The check a store makes before it accepts a name, and it belongs here for
// the reason the rest of this package does: a name outside the grammar is
// one no ${VAR} in any config can reach, so a store that took "my token" or
// "gitlab-token" would seal a value, list it, report success, and resolve
// nothing forever. The failure surfaces as an empty credential at whichever
// provider reads it, hours later and nowhere near the name that caused it.
func ValidName(name string) bool { return namePattern.MatchString(name) }

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

// Part is one run of a value: either literal text or one reference.
type Part struct {
	// Text is the run exactly as it appears in the value — a reference's
	// full `${NAME}` spelling included.
	Text string

	// Name is the variable a reference names, and empty for literal text.
	Name string
}

// Split cuts a value into its literal runs and its references, in order.
//
// Concatenating every part's Text gives the value back byte for byte, and no
// two literal runs are adjacent: what is between two references is ONE run.
// It is the grammar [Expand] substitutes by, so a caller that rewrites the
// literal half of a value leaves every reference exactly where the resolver
// will find it, and expanding the rewritten value gives what expanding the
// original gave.
func Split(value string) []Part {
	var out []Part
	at := 0
	for _, m := range pattern.FindAllStringSubmatchIndex(value, -1) {
		if m[0] > at {
			out = append(out, Part{Text: value[at:m[0]]})
		}
		out = append(out, Part{Text: value[m[0]:m[1]], Name: value[m[2]:m[3]]})
		at = m[1]
	}
	if at < len(value) {
		out = append(out, Part{Text: value[at:]})
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
// A literal comes back trimmed. A whole reference comes back as the variable's
// value, or EMPTY when unset, never its own literal text: a raw
// "${SLACK_BOT_TOKEN_CEO}" matches nothing any vendor accepts, so passing it
// through makes a seat authenticate as nobody, far from the config that named
// it. A nil lookup reads the process environment.
//
// internal/org keeps its own walk: it also skips a reference that resolves to
// EMPTY, leaving the previous value rather than blanking it, which is right
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

// SealedName is the one name a sealed value is derived under: VENDOR_FIELD,
// plus _SEAT for a value that belongs to one seat, upper-snaked
// (`DATADOG_APP_KEY_SRE`, `GITHUB_APP_KEY_SENIOR_ENGINEER`).
//
// The one naming function for setup.SecretNameFor, seataccess.SinkName and the
// org model's tool binding (config): it lives in this leaf because config is
// below the other two and all must agree byte for byte.
//
// A character the grammar refuses becomes an underscore rather than being
// dropped, so `nova-1` and `nova1` stay different seats instead of sealing one
// seat's credential under the other's name. No leading-digit guard is needed:
// a derived name begins with the vendor's own letters.
func SealedName(vendor, field, seat string) string {
	parts := []string{vendor, field}
	if seat != "" {
		parts = append(parts, seat)
	}
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(strings.Join(parts, "_"))) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
