// Package setup is what an integration needs before it can work, said in one
// vocabulary every third-party app and the dashboard share.
//
// # The problem it solves
//
// Connecting an integration means putting values in two different places: a
// credential into the fleet's sealed secret store, and everything else into
// the company document. Which values, where each one goes, what a person has
// to fetch from the third-party app first and what the engine can generate
// itself are all facts only the integration package knows, and until now they
// were written down in three places that could disagree: a CLI flag list, a
// config validator's error strings, and a docs page.
//
// A [Requirement] is that fact, once. An integration package answers with a
// list of them; the API serves the list; the dashboard renders a form from it
// and knows nothing about any third-party app. Adding a third-party app adds
// no branch to the screen.
//
// # It is deliberately not a form description
//
// There is no widget name here, no ordering hint, no CSS. What a requirement
// carries is what is TRUE about the input: where it lives in the config, what
// it is called at the third-party app, whether it is a credential, whether
// the engine can mint it, and which reconcile finding satisfying it clears.
// Everything about presentation is the dashboard's, which is why a
// requirement can also be read by a CLI, a test or a person reading JSON.
//
// # The finding vocabulary is the join
//
// [Requirement.Blocks] names the [integration.FindingKind] that this input
// being absent produces. That is what turns "what is wrong" into "what to
// type": a row reporting `credential_missing` can offer exactly the fields
// whose Blocks says they clear it, with no per-integration mapping anywhere.
package setup

import (
	"encoding/json"
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

	// KindID is an opaque identifier from the third-party app: a cloud id, an
	// organization name, an app id.
	KindID Kind = "id"

	// KindChoice is one of a closed set the third-party app defines.
	KindChoice Kind = "choice"

	// KindText is free text with no shape the engine can check.
	KindText Kind = "text"

	// KindHandle is a seat handle, which must name a seat this company
	// actually has. Distinct from KindID so a form can offer the roster
	// rather than a text box: a typo here is caught by config validation
	// far too late to be useful.
	KindHandle Kind = "handle"

	// KindEmail is an address an account is identified by. Distinct from
	// KindText because free text may legitimately hold a space (a Datadog
	// role is "Datadog Read Only Role") and an address never can: a
	// trailing one off a copy is invisible in the box and authenticates as
	// nobody.
	KindEmail Kind = "email"

	// KindToggle is on or off, and it is a JSON BOOLEAN in the document
	// rather than a string. Distinct from a two-option choice for that
	// reason alone: `"enabled": "true"` is refused by the strict reader,
	// so a toggle rendered as a choice would produce a patch the config
	// surface rejects on every submission.
	KindToggle Kind = "toggle"
)

// Kinds is every kind, in no significant order.
var Kinds = []Kind{
	KindSecret, KindURL, KindID, KindChoice, KindText, KindHandle, KindEmail, KindToggle,
}

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
	// Field is the requirement's identity within its third-party app, and the
	// key a submission uses. The last segment of ConfigPath, normally.
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

	// Connect marks a requirement as one of the few that ESTABLISH the
	// connection, rather than one that configures what happens over it.
	//
	// The two are different questions asked at different moments, and
	// putting them in that order matters: which seat a Datadog alert wakes
	// is not a thing to decide while pasting an API key. So these fields
	// LEAD the form, with a rule under them and the rest below.
	//
	// It used to decide VISIBILITY — the connect form showed these alone,
	// and the rest appeared only once the app was configured. That made an
	// app's settings a form its operator had never seen, with fields that
	// arrived from nowhere, so it is an ordering now and the form is the
	// same one in both directions.
	//
	// It is NOT the same as Required. A connect field can be optional to
	// the integration as a whole — Datadog routes alerts with no keys at
	// all — and still be the thing you are being asked for when you press
	// Connect.
	Connect bool `json:"connect,omitempty"`

	// Shared marks a value that is ONE value across every surface of its
	// tool, so the form asks for it once and writes it to all of them.
	//
	// Atlassian is why: Jira and Confluence are two blocks in the config and
	// one product family in the world. The account email, the API token, the
	// cloud id and the link address are the same Atlassian account and the
	// same Atlassian site, so asking for each of them twice — under two
	// headings, in one dialog — invited an operator to enter two different
	// answers to one question.
	//
	// NOT the same as "has the same field name". Jira's url and Confluence's
	// url are both `url` and are different addresses, which is exactly why
	// this is declared rather than inferred.
	Shared bool `json:"shared,omitempty"`

	// Hidden marks a value the form writes without asking for it.
	//
	// The `enabled` toggle on every inbound app is the case: connecting an
	// integration and leaving it switched off is not a thing anybody means,
	// so the question was a control whose only sensible answer was the one
	// it already had. It is still WRITTEN — the block needs the field, and a
	// company that has connected an app wants its route open — from Default,
	// through the same submission as everything else.
	//
	// Pausing an integration without disconnecting it is still a thing an
	// operator does on purpose. It is an edit to the company configuration,
	// which is where a setting nobody is asked for belongs.
	Hidden bool `json:"hidden,omitempty"`

	// Default is the value a form offers when this company has none.
	//
	// A SUGGESTION, never a stored value: nothing is written until
	// somebody submits, so a default that turns out to be wrong is one
	// they change rather than one they have to discover. It exists for
	// the field whose answer is the same for most companies — Datadog's
	// region is US1 for most of them — where "Choose one" makes everybody
	// answer a question that has an obvious answer.
	Default string `json:"default,omitempty"`

	// LinkText is the words in Help that become the link to VendorURL.
	//
	// A link reads as part of the sentence rather than after it: "Create
	// one on your API keys page" sends somebody to the page it names,
	// where a trailing "Open Datadog" makes them work out which of three
	// pages the form meant. Empty falls back to naming the app.
	LinkText string `json:"link_text,omitempty"`

	// Mintable reports that the ENGINE can produce this value, so a
	// person should not be asked for it. A shared webhook token and a
	// signing secret are both mintable; a third-party app's own API token is
	// not.
	Mintable bool `json:"mintable,omitempty"`

	// Shape is what a minted value has to LOOK like, for the fields where
	// the third-party app accepts one form and no other.
	//
	// A MINT THAT IGNORES THIS IS A CREDENTIAL THE ENGINE'S OWN VERIFIER
	// REJECTS, and every path that would catch it is closed by
	// construction: the mint writes into the secret store, the document
	// gets a `${VAR}`, and a reference is the one thing config validation
	// cannot check the shape of, because the reference is all that layer
	// ever sees. So GitLab was connected from the dashboard with a signing
	// secret that could not be an HMAC key for any delivery, its route
	// answered 503 to every one, and nothing anywhere said why.
	//
	// Empty is [ShapeToken], which is every field but one.
	Shape Shape `json:"shape,omitempty"`

	// Help is one sentence on what the value is for.
	Help string `json:"help,omitempty"`

	// Where says how to obtain it at the third-party app, for a value only the
	// third-party app can issue.
	Where string `json:"where,omitempty"`

	// VendorURL is the page at the vendor where Where happens.
	//
	// It may carry `{field}` placeholders, filled from what the form
	// currently holds for those fields. Atlassian's API keys live at a
	// per-organization address, so the link cannot be written down in
	// advance: until somebody has typed the organization id there is no
	// page to open, and a link to the console's front door sends them
	// somewhere they then have to navigate out of. The form renders plain
	// text until every placeholder resolves.
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
	// It is set by the third-party app that declared the requirement, because
	// that function has already read the block. The alternative was a second
	// switch over every config path in the API layer, which is the same list
	// written twice and eventually two lists that disagree.
	Stored string `json:"-"`

	// Effective is what a `${VAR}` in Stored resolves to, for a field that
	// is NOT a credential.
	//
	// A REFERENCE IS NOT AN ADDRESS, and a form draws links out of these
	// values: the Atlassian API keys page is per-organization, so its link
	// is built from the organization id, and once that id lived in the
	// sealed store the link was built out of the literal text
	// "${ATLASSIAN_ORG_ID}" and opened a console page for an organization
	// of that name.
	//
	// NEVER SET FOR A CREDENTIAL. See [FillEffective], which is the only
	// thing that writes it: a secret's resolved value has no path onto this
	// wire, and no link in this tree is built out of one.
	Effective string `json:"resolved_value,omitempty"`
}

// FillEffective records what each non-credential reference resolves to.
//
// ONE PLACE, in the API layer rather than in each vendor's own Requirements,
// because it is a property of answering a request rather than of what an app
// needs: the vendor lists say what a field IS, and this says what this
// process can currently read for it.
func FillEffective(reqs []Requirement, resolve func(string) (string, bool)) {
	for i := range reqs {
		// A CREDENTIAL NEVER. The one rule this function exists to keep.
		if reqs[i].Kind == KindSecret || !IsReference(reqs[i].Stored) {
			continue
		}
		reqs[i].Effective = Deref(reqs[i].Stored, resolve)
	}
}

// MarshalJSON adds the CURRENT VALUE for everything that is not a credential.
//
// A form has to open showing what this company already answered, or it is not
// an edit of a configuration: it is a blank form over one, and saving it
// blanks the settings the operator did not retype. That is what happened —
// the Datadog dialog offered "Choose one" over a region and a fallback seat
// the document already held.
//
// [Requirement.Stored] cannot do this: it is `json:"-"` precisely because it
// can hold a literal credential on a company that wrote one, so it is the
// wrong thing to put on a wire. This emits it only for the kinds that are
// plain settings — a region, a URL, a handle, a group — every one of which is
// already readable through GET /config by anybody this route answers.
//
// A CREDENTIAL'S REFERENCE IS NOT THE CREDENTIAL, and it is the one thing a
// secret field may carry back. `${JIRA_TOKEN}` is a NAME: it says which entry
// of the sealed store this field reads, which is already visible through GET
// /config to everybody this route answers, and it is what an operator has to
// see to know they are editing the right pointer rather than replacing it.
// A LITERAL in the same position is the credential itself and never leaves.
//
// A METHOD ON THE TYPE rather than a step in the API layer, so nothing can
// forget it and no future caller can serialise a Requirement any other way.
func (r Requirement) MarshalJSON() ([]byte, error) {
	// An alias, because marshalling the named type here would call this
	// method again, forever.
	type wire Requirement
	out := struct {
		wire
		Value string `json:"value,omitempty"`
	}{wire: wire(r)}
	switch {
	case r.Kind != KindSecret:
		out.Value = r.Stored
	case IsReference(r.Stored):
		out.Value = strings.TrimSpace(r.Stored)
	}
	return json.Marshal(out)
}

// IsReference reports a value that is WHOLLY a `${VAR}`, and is therefore a
// name rather than a credential.
//
// Whole, never merely containing one: `https://${HOST}/api` names a variable
// and carries a hostname beside it, so treating it as a pointer would put a
// fragment of a URL on a wire that refuses credentials, and writing through
// it would replace the address with a token.
func IsReference(value string) bool {
	_, whole := envref.Whole(strings.TrimSpace(value))
	return whole
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
// The third-party app's own declaration wins. Failing that it is derived, so
// the common path asks a person for nothing: VENDOR_FIELD, plus _HANDLE for a
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
// a derived name always begins with the third-party app's own kind, every one
// of which is letters. [TestEveryVendorKindDerivesAReferenceableName] is what
// holds that true as kinds are added.
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

// Held, Plain and Toggle are the three answers a third-party app gives per
// field, and together they are what a Requirement needs to know about the
// document: is something written down, is it usable, and what exactly is
// written.
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

// Tight reports a kind whose value cannot contain whitespace anywhere.
//
// An address, an opaque identifier and an email are single tokens: a space
// inside one is always a mistake, and a space around one is a paste artifact
// that is invisible in a form and fatal at the vendor. Free text is
// deliberately not on the list, because a role name is several words.
func (k Kind) Tight() bool {
	return k == KindURL || k == KindID || k == KindEmail
}

// Deref is the value a field actually carries: a whole `${VAR}` read through
// the store, anything else as written.
//
// For the DERIVATIONS a form makes from a value rather than for the value
// itself. Jira's and Confluence's forms decide which deployment they are
// looking at from the site address, and a reference is not an address: read
// literally, `${JIRA_URL}` has no `.atlassian.net` in it, so a Cloud site
// configured through the store was offered a Data Center's fields.
func Deref(value string, resolve func(string) (string, bool)) string {
	value = strings.TrimSpace(value)
	name, isRef := envref.Whole(value)
	if !isRef || resolve == nil {
		return value
	}
	if got, ok := resolve(name); ok {
		return strings.TrimSpace(got)
	}
	return ""
}

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
