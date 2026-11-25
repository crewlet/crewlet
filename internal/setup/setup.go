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
//
// # The write order is the whole of writing
//
// A submission seals the credential, then points the config at it with a
// ${VAR}, then advances the epoch ([Write]). Pointer first leaves a route
// refusing every delivery; a value alone reaches no running seat, because a
// ${VAR} resolves from a snapshot taken at apply time. A rotation changes no
// pointer and still ends in a reload held to the revision the submission read,
// since a reload of a document a write in between changed reaches nothing.
// Every return after the first seal carries the names sealed ([Result.Secrets]),
// which holds because everything that can refuse without a write runs before
// the first seal. [SealError] is its own error, not a pointer read, because
// the secret store fails with the coordination store's unavailability.
//
// # A credential is checked before it is stored
//
// A vendor's Probe is one read-only call within [VerifyBudget] that answers a
// [Verdict] and writes nothing; a requirement says whether it is Checkable or,
// as a signing-only webhook secret, how the vendor proves it (CheckNote).
// setup/setuptest runs every probe through one suite whose meta-test hands it
// vendors that lie, because a probe answering "works" for a refused key is the
// failure nobody can see. A value that is not a credential is checked the same
// way when its requirement says Checkable (a connection the vendor's own block
// printed).
//
// # An outside step's output
//
// Some values are printed by a block a person runs at the vendor (Google
// Cloud's provider, AWS's stack output), drawn from what is saved
// ([PublicView.DrawnFrom]) with a key the first save mints. Such a value
// ([Requirement.PrintedBy]) is Required, so the setup stays unfinished until
// it is pasted, and never holds a save of the answers before it: a form that
// did would wait on a block it cannot draw until that save.
//
// # The words are split by job
//
// Never one help string per field: a requirement says a one-line Hint and an
// optional More, a tool a [Guide], and a verdict a Summary, Facts and
// Warnings. setuptest/copy.go holds those words to one set of rules, applied
// to every verdict and every tool, so a wall of text cannot return one vendor
// at a time.
package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/vendortext"
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

	// KindAddIdentity is a step a person takes at the vendor, never a value
	// the document holds: adding an identity Crewlet made, by its email, at a
	// role ([Requirement.Identity]). Nothing is submitted for it.
	KindAddIdentity Kind = "add_identity"

	// KindConsent is a sign-in a person makes at the vendor, never a value
	// the document holds: its control starts the integration's consent
	// sign-in (`POST /setup/integrations/{kind}/consent`, ADR-0178), and the
	// engine keeps the grant. Nothing is submitted for it.
	KindConsent Kind = "consent"

	// KindRedirect is an address a person registers at the vendor, never a
	// value the document holds: where the vendor sends the person back from
	// the engine's sign-in ([Requirement.Redirect]). Nothing is submitted for it.
	KindRedirect Kind = "redirect"
)

// Kinds is every kind, in no significant order.
var Kinds = []Kind{
	KindSecret, KindURL, KindID, KindChoice, KindText, KindHandle, KindEmail, KindToggle, KindAddIdentity, KindConsent,
	KindRedirect,
}

// AtVendor reports whether k is a step a person takes at the vendor (an
// identity added, a sign-in, an address registered): the document holds no
// value for it, so nothing is submitted for it and no save waits on it.
func (k Kind) AtVendor() bool { return k == KindAddIdentity || k == KindConsent || k == KindRedirect }

// RedirectStep is a [KindRedirect] requirement's address: URL, "" where the
// engine has no public address, and Key, the Tier A setting URL is built on,
// so a changed address is known to need registering again.
type RedirectStep struct {
	URL string `json:"url"`
	Key string `json:"key"`
}

// FillRedirects gives every [KindRedirect] requirement in reqs the engine's
// sign-in callback url and the setting key it is built on: the running
// engine's address, which no vendor's requirements can read off the document.
func FillRedirects(reqs []Requirement, url, key string) {
	for i := range reqs {
		if reqs[i].Kind == KindRedirect {
			reqs[i].Redirect = &RedirectStep{URL: url, Key: key}
		}
	}
}

// IdentityStep is a [KindAddIdentity] requirement's step: the email a person
// adds at the vendor, the role it needs, and where.
type IdentityStep struct {
	Email string `json:"email"`
	// Role is the vendor's own role ID and its name.
	Role IdentityRole `json:"role"`
	// Where is how to reach the vendor's page for it, the page linked.
	Where vendortext.Text `json:"where"`
	// Level is "company" for the company's own identity, "seat" for one
	// seat's, which Seat then names.
	Level string `json:"level"`
	Seat  string `json:"seat,omitempty"`
	// State is "present" where the engine saw the identity hold the role,
	// "missing" where it saw it not, and "unknown" before it has looked.
	State string `json:"state"`
}

// IdentityRole is a vendor role an identity is added at.
type IdentityRole struct {
	ID   string `json:"id"`
	Name string `json:"name"`
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

	// RequiredWhen gates this requirement on another field's answer: it is
	// neither shown nor required until that field holds that value, and
	// once it does it is both.
	//
	// GitHub is why: under "every repository in the organization" an
	// organization token is required, under "wherever an agent's app is
	// installed" it is never used, so no static [Required] is honest. Showing
	// and requiring are ONE decision, as a separate Shown flag would let a
	// form drift into asking a question whose answer it ignores.
	//
	// Nil is the ordinary requirement, answering to [Required] alone.
	RequiredWhen *Gate `json:"required_when,omitempty"`

	// Required reports whether the integration can work without this.
	// A false one is still worth showing: it is usually the difference
	// between working and working well.
	Required bool `json:"required"`

	// Connect marks a requirement as one of the few that ESTABLISH the
	// connection, rather than one that configures what happens over it.
	//
	// Which seat a Datadog alert wakes is not decided while pasting an API
	// key, so these fields LEAD the form, with a rule under them. An
	// ordering, not VISIBILITY: the form is the same one in both directions.
	//
	// NOT the same as Required: a connect field can be optional to the
	// integration (Datadog routes alerts with no keys) and still be what you
	// are asked for when you press Connect.
	Connect bool `json:"connect,omitempty"`

	// Shared marks a value that is ONE value across every surface of its
	// tool, so the form asks for it once and writes it to all of them.
	//
	// Atlassian is why: Jira and Confluence are two config blocks but one
	// account and site (email, API token, cloud id, link address), and asking
	// twice invites two answers to one question.
	//
	// NOT "has the same field name": Jira's and Confluence's `url` are
	// different addresses, which is why this is declared, not inferred.
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

	// Mintable reports that the ENGINE can produce this value, so a
	// person should not be asked for it. A shared webhook token and a
	// signing secret are both mintable; a third-party app's own API token is
	// not. How it is minted is its [Requirement.Shape].
	Mintable bool `json:"mintable,omitempty"`

	// MintFirst makes a Mintable value as soon as a form for it opens, before
	// anything is typed, so its public view (a block drawn from what is typed,
	// [PublicView.Live]) is there at once and one save finishes the setup.
	MintFirst bool `json:"mint_first,omitempty"` //wiregate:absent-is-false minted at the first save, as every other mint

	// Public is an engine-rendered view of a minted value the company holds,
	// for a person to copy to the vendor: a key's public half, a
	// certificate, or a command block holding one. The vendor's requirements
	// render it from the resolved value, through [PublicOf]; NEVER the value
	// itself or a private key, which [Requirement.MarshalJSON] refuses to
	// send. Nil where there is nothing to show.
	Public *PublicView `json:"public,omitempty"`

	// PrintedBy names the field of the same list whose [Requirement.Public]
	// block, run at the vendor, prints this value. Required to finish, it never
	// holds a save (the package doc's "An outside step's output").
	PrintedBy string `json:"printed_by,omitempty"`

	// Identity is a [KindAddIdentity] requirement's step, nil for any other
	// kind.
	Identity *IdentityStep `json:"identity,omitempty"`

	// Redirect is a [KindRedirect] requirement's address, nil for any other
	// kind and until [FillRedirects] runs.
	Redirect *RedirectStep `json:"redirect,omitempty"`

	// Checkable reports that this value, a credential or not, can be checked
	// at the vendor before it is stored, with a read-only call, so a form
	// offers a Test control for it. See [Probe].
	Checkable bool `json:"checkable,omitempty"`

	// CheckNote is, for a credential that cannot be checked ahead, when the
	// vendor proves it (a signing secret, by its first signed delivery). A
	// check of the field answers it as [VerifyUnchecked]; a form says it after More.
	CheckNote string `json:"check_note,omitempty"`

	// CheckWith names the other fields of the same list that a check of
	// this credential is made with, for a vendor that proves several
	// values in one check (Datadog's API and application keys, against
	// their region).
	//
	// Set on a [Requirement.Checkable] field only. A form offers one Test
	// control for the group, enabled once every named field has a value.
	// A credential named here is tested through this field, so it is not
	// Checkable itself and its [Requirement.CheckNote] says so. Empty, the
	// field is checked on its own.
	CheckWith []string `json:"check_with,omitempty"`

	// Shape is what a minted value has to LOOK like, for the fields where
	// the third-party app accepts one form and no other.
	//
	// A MINT THAT IGNORES THIS IS A CREDENTIAL THE ENGINE'S OWN VERIFIER
	// REJECTS, and nothing catches it: the mint writes into the secret store,
	// the document gets a `${VAR}`, and config validation sees only the
	// reference, whose shape it cannot check (a GitLab signing secret that
	// was no HMAC key made its route answer 503 to every delivery, silently).
	//
	// Empty is [ShapeToken], which is every field but one.
	Shape Shape `json:"shape,omitempty"`

	// Hint is the one line under the field, under fifteen words (setupapi's
	// copy test holds it): what the value is and where it is found, the
	// vendor page linked on the words that name it; `{field}` in its words or
	// a link reads another field of the form.
	Hint vendortext.Text `json:"hint,omitempty"`

	// More is what a person may want once about the field, behind a
	// disclosure under its hint. Empty where the hint says it all.
	More vendortext.Text `json:"more,omitempty"`

	// Choices are the options for a KindChoice.
	Choices []Choice `json:"choices,omitempty"`

	// Format describes the shape a value must have, for a kind the engine
	// checks, in words rather than as a regular expression.
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
	// API: null is "nothing resolved this", because the document leaves
	// the field empty or because it was read without a resolver, and
	// reporting that as false would tell an operator a working credential
	// is broken.
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
	// A REFERENCE IS NOT AN ADDRESS, and a form builds links from these
	// values: the per-organization Atlassian API keys link, built from the
	// literal "${ATLASSIAN_ORG_ID}", opened a page for an organization of that
	// name.
	//
	// NEVER SET FOR A CREDENTIAL. [FillEffective] is the only writer: a
	// secret's resolved value has no path onto this wire.
	Effective string `json:"resolved_value,omitempty"`
}

// PublicView is text a form shows for a person to copy out, drawn as code:
// Text as the engine rendered it, and Language, how it is highlighted ("sh",
// "json"). Public material only ([Requirement.Public]).
type PublicView struct {
	Text     string `json:"text"`
	Language string `json:"language"`
	// DrawnFrom names the fields of the same list whose saved answers Text is
	// drawn from, so a form knows an unsaved change to one makes it stale.
	DrawnFrom []string `json:"drawn_from,omitempty"`
	// Live is a view the form redraws from what is typed in DrawnFrom, through
	// the integration's [Drawer], so it is never stale and never waits on a
	// save. It holds public names and public halves alone.
	Live bool `json:"live,omitempty"` //wiregate:absent-is-false drawn from what is saved, as every other view
}

// privateKeyBlock matches the armour of a PEM private key of any kind: PKCS#8,
// an encrypted one, or a key type's own (`EC`, `RSA`).
var privateKeyBlock = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)

// PublicOf is a public view of held, the resolved value it is rendered from,
// as text drawn in language: refused where text holds held itself or any PEM
// private key, so a renderer cannot hand a person what the store keeps.
func PublicOf(held, text, language string) (*PublicView, error) {
	if held = strings.TrimSpace(held); held != "" && strings.Contains(text, held) {
		return nil, errors.New("setup: a public view holds the value it was rendered from")
	}
	if privateKeyBlock.MatchString(text) {
		return nil, errors.New("setup: a public view holds a private key")
	}
	return &PublicView{Text: text, Language: language}, nil
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
// A form must open showing what the company already answered, or saving it
// blanks settings the operator did not retype. [Requirement.Stored] is
// `json:"-"` because it can hold a literal credential, so this emits it only
// for plain settings, already readable through GET /config.
//
// A CREDENTIAL'S REFERENCE IS NOT THE CREDENTIAL, and is the one thing a
// secret field may carry back: `${JIRA_TOKEN}` is a NAME, already visible
// there. A LITERAL in the same position never leaves. A METHOD ON THE TYPE, so
// no caller can serialise a Requirement any other way.
func (r Requirement) MarshalJSON() ([]byte, error) {
	// A PUBLIC VIEW HOLDING A PRIVATE KEY, or the literal a credential's
	// field stores, is a bug in the vendor that rendered it, and failing the
	// answer is how it is never sent.
	if r.Public != nil {
		literal := strings.TrimSpace(r.Stored)
		if privateKeyBlock.MatchString(r.Public.Text) ||
			(r.Kind == KindSecret && literal != "" && !IsReference(literal) && strings.Contains(r.Public.Text, literal)) {
			return nil, fmt.Errorf("setup: the public view of %s holds a credential", r.Field)
		}
	}
	// An alias, because marshalling the named type here would call this
	// method again, forever.
	type wire Requirement
	out := struct {
		wire
		Value string `json:"value,omitempty"`
		// Multiline and Accept are the Shape's, for a form that draws a box
		// keeping line breaks and offers a file of these types.
		Multiline bool   `json:"multiline,omitempty"`
		Accept    string `json:"accept,omitempty"`
	}{wire: wire(r), Multiline: r.Shape.Multiline(), Accept: r.Shape.Accept()}
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
// delivery, so it is NOT satisfied. Where resolution is unknown (a requirement
// read without a resolver), present is the most that can be claimed and it is
// claimed: refusing to call it satisfied would list things to fix that are
// already fine.
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
//
// A SET BELOW ITS Min is outstanding as its first field: the entry a form
// offers first, and a name a client can place the set's refusal by.
func Outstanding(reqs []Requirement) []Requirement {
	out := []Requirement{}
	for _, r := range reqs {
		if r.Needed(reqs) && !r.Satisfied() {
			out = append(out, r)
		}
	}
	return out
}

// Gate is "when this other field holds this value". See
// [Requirement.RequiredWhen].
type Gate struct {
	// Field is another requirement's [Requirement.Field] in the same list.
	Field string `json:"field"`
	// Equals is the value that opens the gate.
	Equals string `json:"equals"`
}

// Needed reports whether this requirement must be answered, given what the
// rest of the list currently holds.
//
// ONE IMPLEMENTATION, because three readers ask it (the form's required
// marker, [Outstanding] deciding whether a pass may run, and the submission
// path deciding whether to store an answer), and a gate honoured by two is
// broken by the third. The route's copy matters most: a form is a courtesy,
// a route the boundary.
//
// A gate naming a field not in the list is CLOSED: reading a typo as "always
// required" would turn a vendor's mistake into every company's blocked connect.
// A step at the vendor ([Kind.AtVendor]) never is: the pass reports whether it was taken.
func (r Requirement) Needed(among []Requirement) bool {
	if r.Kind.AtVendor() {
		return false
	}
	if r.RequiredWhen == nil {
		return r.Required
	}
	for _, other := range among {
		if other.Field == r.RequiredWhen.Field {
			return strings.TrimSpace(other.Stored) == r.RequiredWhen.Equals
		}
	}
	return false
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
// `NOVA_1` rather than an unreferenceable name. The derivation is
// [envref.SealedName], the one every layer shares.
func SecretNameFor(kind integration.Kind, r Requirement) string {
	if r.SecretName != "" {
		return r.SecretName
	}
	return envref.SealedName(string(kind), r.Field, r.Seat)
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
// Three functions, not one with a flag, as the wrong question has a visible
// consequence. A credential is HELD: it may be a `${VAR}`, so present and
// resolved are two facts and the gap is a silent outage. A plain setting is
// PLAIN: nothing resolves an organization name, so written down is the whole
// of it. A toggle is present when it is ON, as reporting `false` as written
// down would make a paused integration look complete.

// Noun is the field's value as a sentence names it: its label with the first
// letter lowered, unless the label opens with an acronym ("API key"), or its
// field's name for a field with no label.
func (r Requirement) Noun() string {
	label := strings.TrimSpace(r.Label)
	if label == "" {
		return strings.ReplaceAll(r.Field, "_", " ")
	}
	if len(label) < 2 || (label[1] >= 'A' && label[1] <= 'Z') {
		return label
	}
	return strings.ToLower(label[:1]) + label[1:]
}

// ValueProblem is the sentence for a submitted value this field refuses, ""
// for one it takes. value is trimmed already, and a whole `${VAR}` names a
// stored value rather than being one, so it is never judged here.
//
// A CREDENTIAL IS ONE UNBROKEN LINE: a pasted one with a space, a line break
// or an invisible character inside is never one a vendor issued
// ([secrets.StrayProblem]), and its shape, where it has one, holds it to the
// vendor's alphabet.
func (r Requirement) ValueProblem(value string) string {
	switch r.Kind {
	case KindConsent:
		return fmt.Sprintf("The %s is a sign-in at the vendor, not a value: press its Sign in button, "+
			"and the engine keeps what the vendor grants.", r.Noun())
	case KindRedirect:
		return fmt.Sprintf("The %s is an address you register at the vendor, not a value: the engine "+
			"answers it from its public address.", r.Noun())
	}
	if _, isRef := envref.Whole(value); isRef {
		return ""
	}
	switch {
	case r.Kind == KindSecret && r.Shape.Multiline():
		// A KEY SPANS LINES, so its shape judges it whole below.
	case r.Kind == KindSecret:
		if problem := secrets.StrayProblem(r.Noun(), value); problem != "" {
			return problem
		}
	case r.Kind.Tight() && strings.ContainsAny(value, " \t\r\n"):
		return fmt.Sprintf("The %s cannot contain a space, and %q has one inside it: an address, "+
			"an identifier and an email are each a single token, so a space in the middle is a "+
			"value nothing answers to rather than an untidy one.", r.Noun(), value)
	}
	return r.Shape.Problem(r.Noun(), value)
}

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
// resolve is a caller that cannot say, which answers null rather than a guess.
// Passing it in rather than reaching for the environment keeps this package
// free of ambient state: the API passes the engine's own chain, so what it
// reports is what the node answering actually resolved.
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
