// Package setup is what an integration needs before it can work, said in one
// vocabulary every vendor and the dashboard share.
//
// # The problem it solves
//
// Connecting an integration means putting values in two different places: a
// credential into the fleet's sealed secret store, and everything else into
// the company document. Which values, where each one goes, what a person has
// to fetch from the vendor first and what the engine can generate itself are
// all facts only the vendor package knows, and until now they were written
// down in three places that could disagree: a CLI flag list, a config
// validator's error strings, and a docs page.
//
// A [Requirement] is that fact, once. A vendor package answers with a list of
// them; the API serves the list; the dashboard renders a form from it and
// knows nothing about any vendor. Adding a vendor adds no branch to the
// screen.
//
// # It is deliberately not a form description
//
// There is no widget name here, no ordering hint, no CSS. What a requirement
// carries is what is TRUE about the input: where it lives in the config, what
// it is called at the vendor, whether it is a credential, whether the engine
// can mint it, and which reconcile finding satisfying it clears. Everything
// about presentation is the dashboard's, which is why a requirement can also
// be read by a CLI, a test or a person reading JSON.
//
// # The finding vocabulary is the join
//
// [Requirement.Blocks] names the [integration.FindingKind] that this input
// being absent produces. That is what turns "what is wrong" into "what to
// type": a row reporting `credential_missing` can offer exactly the fields
// whose Blocks says they clear it, with no per-vendor mapping anywhere.
package setup

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
)

// Kind is what sort of value a requirement holds.
//
// The distinction that carries weight is [KindSecret] against everything
// else: a secret is written to the sealed store and the config gets a
// `${VAR}` pointer, while every other kind is written into the config
// itself. The rest exist so a form can validate before the engine has to.
type Kind string

const (
	// KindSecret is a credential. Never echoed back, never logged, and
	// never written into the company document as a literal.
	KindSecret Kind = "secret"

	// KindURL is an address, which must carry a scheme.
	KindURL Kind = "url"

	// KindID is an opaque identifier from the vendor: a cloud id, an
	// organization name, an app id.
	KindID Kind = "id"

	// KindChoice is one of a closed set the vendor defines.
	KindChoice Kind = "choice"

	// KindText is free text with no shape the engine can check.
	KindText Kind = "text"

	// KindHandle is a seat handle, which must name a seat this company
	// actually has. Distinct from KindID so a form can offer the roster
	// rather than a text box: a typo here is caught by config validation
	// far too late to be useful.
	KindHandle Kind = "handle"

	// KindToggle is on or off, and it is a JSON BOOLEAN in the document
	// rather than a string. Distinct from a two-option choice for that
	// reason alone: `"enabled": "true"` is refused by the strict reader,
	// so a toggle rendered as a choice would produce a patch the config
	// surface rejects on every submission.
	KindToggle Kind = "toggle"
)

// Kinds is every kind, in no significant order.
var Kinds = []Kind{KindSecret, KindURL, KindID, KindChoice, KindText, KindHandle, KindToggle}

// Valid reports whether k is a kind this build knows.
func (k Kind) Valid() bool {
	for _, known := range Kinds {
		if k == known {
			return true
		}
	}
	return false
}

// String makes a Kind printable without a conversion at every log site.
func (k Kind) String() string { return string(k) }

// Choice is one option of a [KindChoice] requirement.
type Choice struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Hint  string `json:"hint,omitempty"`
}

// Requirement is one input an integration cannot work without.
type Requirement struct {
	// Field is the requirement's identity within its vendor, and the key
	// a submission uses. The last segment of ConfigPath, normally.
	Field string `json:"field"`

	// Label is what a person reads beside the input.
	Label string `json:"label"`

	Kind Kind `json:"kind"`

	// ConfigPath is where the value (or its ${VAR} pointer) lives, in the
	// same dotted grammar a config diff and a validation error use, with
	// list positions as [n]. One grammar, so a requirement, a diff line
	// and a refusal all address the same place the same way.
	ConfigPath string `json:"config_path"`

	// SecretName is the name the sealed store holds the value under, for
	// a KindSecret. Empty means derive it; see [SecretNameFor].
	SecretName string `json:"secret_name,omitempty"`

	// Required reports whether the integration can work without this.
	// A false one is still worth showing: it is usually the difference
	// between working and working well.
	Required bool `json:"required"`

	// Mintable reports that the ENGINE can produce this value, so a
	// person should not be asked for it. A shared webhook token and a
	// signing secret are both mintable; a vendor's own API token is not.
	Mintable bool `json:"mintable,omitempty"`

	// Help is one sentence on what the value is for.
	Help string `json:"help,omitempty"`

	// Where says how to obtain it at the vendor, for a value only the
	// vendor can issue.
	Where string `json:"where,omitempty"`

	// VendorURL is the page at the vendor where Where happens.
	VendorURL string `json:"vendor_url,omitempty"`

	// Choices are the options for a KindChoice.
	Choices []Choice `json:"choices,omitempty"`

	// Format describes the shape a value must have, for a kind the engine
	// checks. Rendered as a hint, never as a regular expression.
	Format string `json:"format,omitempty"`

	// Blocks names the [integration.FindingKind] that an absent value
	// produces, empty when nothing reports it. This is the join between a
	// reconcile finding and the field that clears it.
	Blocks integration.FindingKind `json:"blocks,omitempty"`

	// Present reports that the DOCUMENT names something here: a literal
	// or a ${VAR}. It says nothing about whether that resolved.
	Present bool `json:"present"`

	// Resolved reports whether the value is actually usable in this
	// process. THREE-VALUED, like every other resolution claim on this
	// API: null is "this process cannot say", which is the honest answer
	// where nothing resolved the document, and reporting it as false
	// would tell an operator a working credential is broken.
	Resolved *bool `json:"resolved"`

	// Seat is the handle a per-seat requirement belongs to, empty for a
	// company-wide one.
	//
	// IT CHANGES WHAT ConfigPath MEANS, and it has to. A company-wide
	// path is absolute (`integrations.slack.typing_status`); a per-seat
	// one is RELATIVE TO THE SEAT (`integrations.slack.bot_token`),
	// because a seat is addressed by its handle rather than by its
	// position, and a merge patch cannot reach a list element without
	// replacing the whole list. So a per-seat write goes through the
	// entity route instead, where the handle IS the address.
	Seat string `json:"seat,omitempty"`

	// Stored is what the config document holds at ConfigPath right now: a
	// literal, a `${VAR}`, or nothing. NEVER SERIALISED, which is what the
	// `json:"-"` is doing and why it is safe to carry a credential here at
	// all: this value can be a literal secret on a company that wrote one,
	// and it exists only so the write path can tell a `${VAR}` it may
	// write through from a literal it must refuse.
	//
	// It is set by the vendor that declared the requirement, because that
	// function has already read the block. The alternative was a second
	// switch over every config path in the API layer, which is the same
	// list written twice and eventually two lists that disagree.
	Stored string `json:"-"`
}

// Satisfied reports whether this requirement needs nothing further.
//
// Present AND resolved, when resolution is knowable. A requirement whose
// value is written down but did not resolve is the exact state that looks
// configured from every other surface while the route refuses every
// delivery, so it is NOT satisfied. Where resolution is unknown, present is
// the most that can be claimed and it is claimed: refusing to call it
// satisfied on a standalone API would show every operator a permanent list
// of things to fix that are already fine.
func (r Requirement) Satisfied() bool {
	if !r.Present {
		return false
	}
	if r.Resolved != nil {
		return *r.Resolved
	}
	return true
}

// Outstanding returns the requirements that are required and not satisfied,
// in the order they were declared.
func Outstanding(reqs []Requirement) []Requirement {
	out := []Requirement{}
	for _, r := range reqs {
		if r.Required && !r.Satisfied() {
			out = append(out, r)
		}
	}
	return out
}

// nameRule is the whole-reference grammar, and it is envref's own: a value
// this does not match cannot be written as `${NAME}` and read back, so a
// secret stored under it would be unreachable from the config that points at
// it.
var nameRule = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidSecretName reports whether a name can be referenced from the config.
func ValidSecretName(name string) bool { return nameRule.MatchString(name) }

// SecretNameFor is the name a requirement's value is stored under.
//
// The vendor's own declaration wins. Failing that it is derived, so the
// common path asks a person for nothing: VENDOR_FIELD, plus _HANDLE for a
// per-seat requirement, upper-snaked. Every character the grammar refuses
// becomes an underscore, which is what makes a handle like `nova-1` into
// `NOVA_1` rather than an unreferenceable name.
func SecretNameFor(kind integration.Kind, r Requirement) string {
	if r.SecretName != "" {
		return r.SecretName
	}
	parts := []string{string(kind), r.Field}
	if r.Seat != "" {
		parts = append(parts, r.Seat)
	}
	return slug(strings.Join(parts, "_"))
}

// slug upper-snakes a string into the whole-reference grammar.
//
// Every character the grammar refuses becomes an underscore rather than being
// dropped, which is what keeps two different seats on two different names: a
// handle of `nova.1` and one of `nova-1` are different seats, and trimming
// the separator would seal one seat's credential under the other's name.
//
// It does not guard the grammar's leading-digit rule, and does not need to:
// a derived name always begins with the vendor's own kind, every one of which
// is letters. [TestEveryVendorKindDerivesAReferenceableName] is what holds
// that true as kinds are added.
func slug(in string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(in)) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// ErrStaleBase reports a submission built on a revision that is no longer
// active, refused before anything was sealed.
//
// Distinct from the config surface's own raced error, which is raised after a
// revision has been stored: this one means nothing happened at all.
type ErrStaleBase struct {
	Base    string
	Current string
}

func (e *ErrStaleBase) Error() string {
	return fmt.Sprintf("setup: the active revision advanced from %q to %q", e.Base, e.Current)
}

// ErrLiteralInConfig reports a config slot holding a value that is neither
// empty nor a whole `${VAR}` reference.
//
// Refused rather than overwritten. Rewriting a literal would edit the company
// from a setup form, and the value it would destroy is a credential somebody
// put there on purpose. It is the same refusal every provisioner already
// makes, for the same reason.
type ErrLiteralInConfig struct {
	// Path is the config path holding the literal.
	Path string
}

func (e *ErrLiteralInConfig) Error() string {
	return fmt.Sprintf("setup: %s holds a value that is not a whole ${VAR} reference", e.Path)
}

// PointerFor decides what a secret requirement's config slot should hold.
//
// Three outcomes, and they are three different situations:
//
//   - the slot is empty, so it is given a `${NAME}` pointing at the derived
//     or declared name, and that pointer is written to the config;
//   - the slot already holds a whole `${VAR}`, so the value goes to THAT
//     name and the config is left alone, which is what makes rotating a
//     credential a change to the store and not to the company;
//   - the slot holds anything else, which is refused.
//
// The name it returns is where the value must be written, never a value.
func PointerFor(kind integration.Kind, r Requirement, current string) (name string, writePointer bool, err error) {
	current = strings.TrimSpace(current)
	if current == "" {
		derived := SecretNameFor(kind, r)
		if !ValidSecretName(derived) {
			return "", false, fmt.Errorf(
				"setup: %s would be stored as %q, which is not a name a ${VAR} can reference",
				r.ConfigPath, derived)
		}
		return derived, true, nil
	}
	if existing, ok := provision.SoleVar(current); ok {
		return existing, false, nil
	}
	return "", false, &ErrLiteralInConfig{Path: r.ConfigPath}
}

// Held, Plain and Toggle are the three answers a vendor gives per field, and
// together they are what a Requirement needs to know about the document: is
// something written down, is it usable, and what exactly is written.
//
// Three functions rather than one with a flag, because the three cases are
// genuinely different questions and picking the wrong one has a visible
// consequence. A credential is HELD: it may be a `${VAR}`, so present and
// resolved are two facts and the gap between them is a silent outage. A plain
// setting is PLAIN: nothing resolves an organization name, so written down is
// the whole of it, and claiming "cannot say" would put a permanent unknown on
// a field an operator reads straight off GET /config. A toggle is neither: it
// is present when it is ON, because reporting `false` as written down would
// make a paused integration look complete.

// Held is a value that may be a `${VAR}`, resolved through this process.
func Held(value string, resolve func(string) (string, bool)) (bool, *bool, string) {
	present, resolved := Resolution(value, resolve)
	return present, resolved, value
}

// Plain is a value nothing resolves: written down is the whole of it.
func Plain(value string) (bool, *bool, string) {
	present := strings.TrimSpace(value) != ""
	yes := present
	return present, &yes, value
}

// Toggle is on or off. Present means ON.
func Toggle(on bool) (bool, *bool, string) {
	yes := on
	if on {
		return true, &yes, "true"
	}
	return false, &yes, "false"
}

// Resolution reports what a value in the document amounts to.
//
// resolve is how the caller turns a stored value into a live one, and a nil
// resolve is a process that cannot say. Passing it in rather than reaching
// for the environment keeps this package free of ambient state and lets the
// API answer honestly on a node that resolved nothing.
func Resolution(stored string, resolve func(string) (string, bool)) (present bool, resolved *bool) {
	stored = strings.TrimSpace(stored)
	present = stored != ""
	if !present || resolve == nil {
		return present, nil
	}
	// A whole reference resolves through the store; anything else IS the
	// value, and a literal in the document is present and resolved by
	// definition.
	name, isRef := envref.Whole(stored)
	if !isRef {
		yes := true
		return true, &yes
	}
	value, ok := resolve(name)
	got := ok && strings.TrimSpace(value) != ""
	return true, &got
}
