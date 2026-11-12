package seataccess

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/setup"
)

// Vendor is one integration's half of per-seat access: what it offers, the
// steps its transitions take, and a session that reads and writes the vendor.
//
// Each vendor package implements it and registers it from its own init
// ([Register]), so the engine's setup pass and the access routes reach every
// vendor through [Lookup] and no vendor is named in either.
type Vendor interface {
	// Kind is the integration whose accounts this manages: one of [Kinds].
	Kind() integration.Kind

	// Catalog is the static half of what this vendor offers per seat.
	Catalog() Catalog

	// Steps is the vendor's own steps for one op on one seat's lane, in the
	// order Apply takes them, drawn in the vendor's words. PURE: no call to
	// the vendor, because the preview route draws a plan with it and a
	// preview must not depend on the vendor answering. An op the vendor
	// cannot take for this lane (a rotate on GitHub, whose tokens are issued
	// hourly) answers no steps, and the plan omits it.
	Steps(seat SeatRef, lane Lane, op Op, want Desired, have Observed) []Step

	// Open builds a session against the vendor from the company's active
	// document. An error wrapping [integration.ErrNotConfigured] is a
	// company that does not provision this vendor (its block absent, off,
	// or without the connection's credentials); any other error is a fault.
	Open(ctx context.Context, in OpenInput) (Session, error)
}

// OpenInput is what a vendor builds its session from.
type OpenInput struct {
	// Config is the company's active document and Org its seats.
	Config *config.Company
	Org    *org.Organization

	// Env resolves the document's `${VAR}` references, the sealed store
	// included.
	Env *config.Resolver

	// Operator is the transient vendor credential a pass was handed
	// ([setup.PassInput.Operator]), empty where the vendor needs none or the
	// run is the loop's.
	Operator string

	// Sink seals a COMPANY value the session itself must keep current
	// before it can act: Slack's app-configuration token, whose refresh
	// token is single use, so a rotation is sealed before it is used. A
	// seat's credentials never go through it (the sync seals [Minted]).
	// Nil on a preview, where the session rotates nothing.
	Sink provision.TokenSink

	// WebhookBase is the public address vendors reach this deployment at
	// ([setup.PassInput.WebhookBase]), which Slack builds each app's
	// request and redirect URLs from. Empty on a preview, where the
	// session creates and restores nothing.
	WebhookBase string

	// Derive is a 32-byte key for one purpose, derived from the fleet's
	// secrets keyring and alike on every node ([secrets.Keyring.Derive]),
	// for a fingerprint the ledger may record in place of a value. Nil where
	// the caller holds no keyring, as a migration's read does.
	Derive func(info string) []byte

	// Reads is what the company half of the same pass already read at the
	// vendor, handed on so the session reads it no second time: Cloudflare's
	// inspection of the token and the account (its permission groups and
	// zones) is one read a pass, not two. Nil where the session opens on its
	// own (a preview, a migration), or the pass read nothing it can use. A
	// session takes only its own vendor's reads and never another's.
	Reads PassReads
}

// PassReads is what one vendor's company half read in a pass, as that vendor
// hands it to its own session. The vendor's own type carries it; the one
// method names the vendor, so a session never takes another's.
type PassReads interface {
	ReadFor() integration.Kind
}

// Session is one pass's connection to the vendor.
type Session interface {
	// Options is what the vendor offers on lane now: its levels in its own
	// order, read live where the vendor lists them (Datadog roles, Jira
	// project roles, Confluence space roles, GitLab custom roles) and from
	// its fixed set otherwise, and the scope the company's connection
	// reaches.
	Options(ctx context.Context, lane Lane) (Options, error)

	// Observe reads back every account the ledger records and every seat in
	// the document, and says what the vendor holds for each seat's lanes. A
	// seat with no account answers Exists false. A seat in Records that the
	// document no longer has is observed too: it is what lets a pass reach a
	// seat that left the document.
	//
	// ONE SEAT'S READ IS ONE SEAT'S FAULT. A vendor that could read some
	// seats and not others (one agent's own credential refused, one account
	// timing out) answers the rest with a [SeatFaults] error naming the
	// others, and the sync records each as that seat's fault and syncs the
	// rest. Any other error is the kind's, and a pass whose context is done
	// answers that.
	Observe(ctx context.Context, in ObserveInput) (Observation, error)

	// Apply takes one step. A vendor refusal is a [*Refusal]; any other
	// error is a fault. A [ByYou] step is not taken: Apply answers
	// [StepResult.Waiting] with where the person acts, or Done where the
	// vendor shows the person already did it.
	Apply(ctx context.Context, in ApplyInput) (StepResult, error)
}

// ObserveInput is the seats a session reads back.
type ObserveInput struct {
	// Seats are the document's agent seats.
	Seats []SeatRef

	// Records are the ledger's records for this kind, by handle, including
	// seats the document no longer has.
	Records map[string]Record
}

// Handles is every handle a pass reads back, sorted and each once: the
// document's seats, and every seat the ledger still records.
func (in ObserveInput) Handles() []string {
	out := make([]string, 0, len(in.Seats)+len(in.Records))
	for _, seat := range in.Seats {
		out = append(out, seat.Handle)
	}
	for handle := range in.Records {
		out = append(out, handle)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Record is a handle's ledger record, or nil where the ledger holds none.
func (in ObserveInput) Record(handle string) *Record {
	if record, ok := in.Records[handle]; ok {
		return &record
	}
	return nil
}

// Observation is what the vendor holds, by handle then lane.
type Observation map[string]map[Lane]Observed

// SeatFaults is an [Session.Observe] error naming the seats the vendor could
// not read, each with why, beside an [Observation] of every other seat.
type SeatFaults map[string]error

// Error implements error, naming each seat in order.
func (f SeatFaults) Error() string {
	parts := make([]string, 0, len(f))
	for _, h := range slices.Sorted(maps.Keys(f)) {
		parts = append(parts, h+": "+f[h].Error())
	}
	return "seataccess: could not read " + strings.Join(parts, "; ")
}

// ApplyInput is one step for one seat's lane.
type ApplyInput struct {
	Seat SeatRef
	Lane Lane
	Step Step

	// Want is the document's setting and Have what the vendor showed when
	// the pass began.
	Want Desired
	Have Observed

	// Record is the ledger's record for the seat, nil where it has none. A
	// vendor addresses an existing account by Record.Account.ID.
	Record *Record

	// Sinks are the engine-owned names the seat's credentials are sealed
	// under, by field ([SinkNames]). Never values: a vendor that needs the
	// seat's current credential (to revoke it) resolves it through its
	// session's resolver.
	Sinks map[string]string

	// Prior is what this op's earlier steps did, in order: a create's
	// account id for the mint after it, a mint's credential id for the
	// retire after it ([Minted.CredentialID]). Empty for an op's first step.
	Prior []StepResult

	// HandIn is the credential a person pasted for this seat ([Ask.HandIn]),
	// empty where none was. Never logged.
	HandIn string `json:"-"`

	// Disconnect is a step of the integration's disconnect, after which no
	// pass reads this seat again ([SyncInput.Disconnect]).
	Disconnect bool

	// Release is a person keeping the account at the vendor ([Ask.Release]).
	Release bool
}

// HandsIn is a session whose seat credential a person creates at the vendor
// and pastes, because the vendor lets no credential create one: the paste
// route checks the value with it, and the seat's sync then seals it.
type HandsIn interface {
	// CheckHandIn proves value fit for the seat's lane, with reads only. A
	// [*Refusal] is the person's to fix; any other error is a fault.
	CheckHandIn(ctx context.Context, in HandInInput) (HandedIn, error)
}

// HandInInput is one pasted credential and the seat it is for.
type HandInInput struct {
	Seat SeatRef
	Lane Lane
	Want Desired

	// Record is the seat's ledger record, and Records every seat's of the
	// kind, so a credential another seat holds is refused.
	Record  *Record
	Records map[string]Record

	// Value is the pasted credential. Never logged or echoed.
	Value string
}

// HandedIn is a pasted credential that passed its check.
type HandedIn struct {
	// Verdict is what the check found, as a person reads it.
	Verdict setup.Verdict

	// Rotate reports a replacement for a credential the seat already holds.
	Rotate bool

	// Account and AccountID are the vendor account the credential proved,
	// and Label what the vendor shows of the credential (a token's prefix):
	// what an audit line records in place of the value.
	Account, AccountID, Label string
}

// StepResult is what one step did.
type StepResult struct {
	// AccountID and Account are set by a step that created the account: the
	// vendor's identifier, which the ledger records, and its own name for it.
	AccountID string
	Account   string

	// Minted are credentials the step created, which the sync seals under
	// the seat's engine-owned names. A value that cannot be sealed is revoked
	// through its Revoke, for every vendor.
	Minted []Minted

	// Revoked are the fields whose sealed values this step made dead (an off
	// that revoked the token, a rotate's old key), which the sync clears from
	// the store.
	Revoked []string

	// Waiting is set by a [ByYou] step the person has not taken yet.
	Waiting *Waiting

	// Disabled is set by a step that disabled the account, so the ledger
	// records that this engine did (D3).
	Disabled bool

	// Gone is set where the vendor shows the account deleted: the ledger
	// record is closed and the next pass creates a fresh one if the seat is
	// on.
	Gone bool

	// Did is the vendor's sentence for what the step actually did, where
	// the drawn step covered more than one case ("Removed its memberships,
	// since this GitLab token cannot block accounts"). The sync records it
	// as the step outcome's text; empty keeps the drawn text.
	Did string

	// Settling is set by a step whose grant the vendor has written and is
	// still applying, so the seat cannot use it yet. The sync records it on
	// the lane and ends OK: nobody owes a step, so it is never a [Waiting].
	Settling *Settling
}

// Settling is a grant the vendor is still applying: until Until the lane's
// tool reads waiting, for Reason, a sentence in the engine's own words ("the
// vendor is still applying the grant"). Past Until it clears itself.
type Settling struct {
	Reason string    `json:"reason"`
	Until  time.Time `json:"until"`
}

// active reports whether s still holds the lane's tool at now.
func (s *Settling) active(now time.Time) bool {
	return s != nil && now.Before(s.Until)
}

// Minted is one credential a step created.
type Minted struct {
	// Field is the credential's field ([Catalog.Credentials]), which names
	// the variable it is sealed under.
	Field string

	// Value is the credential. Never logged, never marshalled.
	Value string `json:"-"`

	// CredentialID is the vendor's identifier for the minted credential (a
	// key id, a token id), never the value, so a later step can tell it from
	// the credentials it replaces.
	CredentialID string

	// Revoke undoes the mint at the vendor. Nil only for a value that grants
	// nothing by itself (an Atlassian account's address). The sync calls it
	// with a context that outlives its own cancellation.
	Revoke func(ctx context.Context) error `json:"-"`
}

// Waiting is a step a person has to take at the vendor.
type Waiting struct {
	// ActionURL is where.
	ActionURL string

	// Detail says what, in the vendor's words.
	Detail string
}

// Catalog is the static half of what a vendor offers per seat: no call to the
// vendor is needed to read it.
type Catalog struct {
	// Kind is the integration.
	Kind integration.Kind

	// Credentials are the fields the engine seals per seat for this kind, the
	// credential itself first: `APP_KEY` for Datadog; `TOKEN` and `EMAIL`
	// for Atlassian; `BOT_TOKEN` for Mattermost; `BOT_TOKEN` and
	// `SIGNING_SECRET` for Slack; `APP_KEY` and `APP_WEBHOOK_SECRET` for
	// GitHub. Each is sealed under [SinkName](Kind, field, handle). Empty
	// only where every lane's credential is issued at use ([LaneSpec.Issued]).
	Credentials []string

	// Lanes are the lanes this kind grants, each with what it offers.
	Lanes []LaneSpec
}

// isCredential reports whether field is the sealed credential itself, the
// first of [Catalog.Credentials]: false for every field of a catalog whose
// lanes are all issued at use ([LaneSpec.Issued]), which seals none.
func (c Catalog) isCredential(field string) bool {
	return len(c.Credentials) > 0 && c.Credentials[0] == field
}

// Lane is the spec for one lane, or false where the catalog has none.
func (c Catalog) Lane(lane Lane) (LaneSpec, bool) {
	for _, spec := range c.Lanes {
		if spec.Lane == lane {
			return spec, true
		}
	}
	return LaneSpec{}, false
}

// LaneSpec is what one lane offers, statically.
type LaneSpec struct {
	Lane Lane

	// Levels is whether the lane grants by level (every lane but GitHub,
	// which grants permissions, and Slack, whose scopes live in the app's
	// manifest).
	Levels bool

	// LiveLevels is whether the levels are read live from the vendor rather
	// than taken from its fixed set.
	LiveLevels bool

	// Permissions is GitHub's permission set; nil elsewhere.
	Permissions []PermissionSpec

	// Scoped is whether the lane narrows by scope.
	Scoped bool

	// ScopeRequired is a Scoped lane whose agent, once on, must name at least
	// one scope entry: one with none would reach nothing. The access routes,
	// the sync and the document's rules refuse it alike
	// (config.SeatScopeRequired restates it, held equal by a test).
	ScopeRequired bool

	// CreatedBy and EnabledBy are who makes the account and who turns it
	// back on: [ByYou] for a GitHub App's creation and a Slack install.
	CreatedBy StepActor
	EnabledBy StepActor

	// Rotatable is whether a person can ask to rotate the credential.
	// GitHub issues hourly tokens, so it has nothing to rotate; Slack's
	// rotate revokes the bot token and waits for a person to approve the
	// reinstall.
	Rotatable bool

	// Releasable is whether a person may keep at the vendor an account a
	// step waits for them to delete ([ActionRelease]): an Expo robot keeps
	// its builds' author. False where what is kept is only a credential,
	// which would stay live and unwatched.
	Releasable bool

	// BoxEnv is each variable the lane's tool, a CLI in the seat's executor
	// box, reads one of the lane's credentials from, by its sealed field
	// (Expo's EXPO_TOKEN from TOKEN); empty where its tool is a server. The
	// engine sets each in the box of a seat on at the lane ([BoxDefaults]).
	BoxEnv []BoxVar

	// Issued is the field of a lane whose credential the engine issues at use
	// and never seals, its variable [SinkName] of it; empty where the
	// credential is sealed. The access answer reports it as the seat's
	// credential ([IssuedCredential]). Never one of [Catalog.Credentials].
	Issued string
}

// BoxVar is one variable a box CLI reads a seat credential from: Env, set to
// the seat's sealed Field, one of [Catalog.Credentials].
type BoxVar struct {
	Field string
	Env   string
}

// BoxDefaulter is a [Vendor] whose box needs more than its lanes' [BoxVar]s:
// settings a CLI reads beside the seat's credentials (an organization, a
// default project), a file holding a token issued at use, or a credential a
// CLI beside the lane's tool reads (a code host's token for git). The engine
// puts them in the box of every seat with one of the vendor's lanes on
// ([BoxDefaults]).
type BoxDefaulter interface {
	// BoxDefaults is what the engine puts in the box of the seat whose role
	// is role.
	BoxDefaults(c *config.Company, role *config.Role) BoxSet
}

// DocumentReach is a [Vendor] whose lanes' reach is written in the company
// document rather than read from the vendor, so it is known whether or not the
// vendor answers ([WithReach]). Its session's Options answers the same reach.
type DocumentReach interface {
	// Reach is what the company's connection reaches on lane, as the
	// document and its roster write it.
	Reach(c *config.Company, roster *org.Organization, lane Lane) Reach
}

// Reach is a lane's reach as the company document writes it: the entries a
// seat's scope must be within, or Open where it lists none and so reaches
// whatever the vendor accepts.
type Reach struct {
	Scope []string
	Open  bool
}

// WithReach is o with its reach from the document where v writes it there
// ([DocumentReach]), marked as read: a list the document holds is known even
// while the vendor cannot be asked. o unchanged for any other vendor.
func WithReach(o Options, v Vendor, c *config.Company, roster *org.Organization) Options {
	doc, ok := v.(DocumentReach)
	if !ok || !o.Scoped {
		return o
	}
	r := doc.Reach(c, roster, o.Lane)
	o.Scope, o.ScopeOpen, o.ScopeLive = slices.Clone(r.Scope), r.Open, true
	return o
}

// BoxSet is what a vendor supplies to one seat's box.
//
// A CREDENTIAL IS THE INTEGRATION'S, A SETTING THE PERSON'S. A line the
// document writes under a credential's name, or a setup file at a credential
// file's path, is ignored for the seat and said ([config.Warning]): the
// integration's state (on or off, its level, a rotation, a token the vendor
// refused) is the one authority on which identity the box acts as. A setting
// is beneath every layer a person writes, so their line wins.
type BoxSet struct {
	// Env is each setting and its value: a literal, or a `${VAR}` reference
	// the launch resolves. Never a secret, since a setting is not scrubbed
	// from the run's output.
	Env map[string]string

	// Credentials is each variable set to one of the seat's own credentials,
	// as the whole `${VAR}` reference of its sealed or issued name, resolved
	// through the seat's own resolution, scrubbed from the run's output and
	// never the harness's sign-in.
	Credentials map[string]string

	// Files are written into the box at launch, beside the files a person's
	// setup steps write and never into its environment, which holds at most a
	// file's path ([BoxFile.PathEnv]), so no file can sign a harness in.
	Files []BoxFile
}

// BoxFile is one file a vendor puts in a seat's box: at Path, relative to the
// box's home, holding the whole `${VAR}` reference Ref, resolved at launch
// through the seat's own resolution, so a credential issued at use is issued
// there, or Ref itself where it is literal text and no reference. Its value
// is scrubbed from the run's output as a credential's is. A reference is a
// credential file and literal text a setting ([BoxSet]).
type BoxFile struct {
	Path string
	Ref  string

	// PathEnv is the variable the launch sets to this file's absolute path in
	// the box, joined onto that box's own home, for a CLI pointed at the file
	// by one; empty for none. Never a sign-in, and beneath every other layer.
	PathEnv string

	// Rewritten is a file the completion poll keeps current for as long as
	// the run lasts, where Ref names a credential issued at use, which
	// renews under a running box; a launch alone writes the rest.
	Rewritten bool
}

// PermissionSpec is one permission a GitHub App can hold.
type PermissionSpec struct {
	// Key is GitHub's key (`contents`), Name its label (`Contents`).
	Key  string `json:"key"`
	Name string `json:"name"`

	// WriteOnly is Workflows, which GitHub grants as write or nothing.
	WriteOnly bool `json:"write_only,omitempty"`

	// AdministersOnWrite marks a permission whose write is an
	// administration grant for the step-up rule (D10): Contents and
	// Workflows.
	AdministersOnWrite bool `json:"administers_on_write,omitempty"`
}

// Options is what a vendor offers on one lane now.
type Options struct {
	Lane Lane `json:"lane"`

	// Levels are the vendor's levels in its own order, least access first.
	Levels []Level `json:"levels,omitempty"`

	// Live is whether Levels were read from the vendor just now.
	Live bool `json:"live"`

	// Permissions is GitHub's permission set.
	Permissions []PermissionSpec `json:"permissions,omitempty"`

	// Scope is every entry the company's connection reaches, which a seat's
	// scope must be within. ScopeOpen is a connection that does not list its
	// reach (a GitHub installation on every repository, a GitLab group with
	// no project list), where any entry the vendor accepts is within it.
	Scope     []string `json:"scope,omitempty"`
	ScopeOpen bool     `json:"scope_open,omitempty"`

	// Scoped is [LaneSpec.Scoped], set whatever Scope holds, so a lane whose
	// scope is open or unread is still a scoped lane.
	Scoped bool `json:"scoped,omitempty"`

	// ScopeNames is each Scope entry's name where the vendor names one apart
	// from its id, for the pickers; nil where the entry is its own name.
	ScopeNames map[string]string `json:"scope_names,omitempty"`

	// ScopeLive is whether Scope and ScopeOpen were answered by the vendor's
	// session just now; false where it could not be read, so an empty Scope
	// then says nothing about the company's reach.
	ScopeLive bool `json:"scope_live,omitempty"`

	// ScopeRequired is [LaneSpec.ScopeRequired].
	ScopeRequired bool `json:"scope_required,omitempty"`

	// ToolServer is the mcp_servers entry a per-seat tool server for the lane
	// is started from ([ToolServerOf]), so a remedy can name it; empty where
	// no server reads the lane's credential.
	ToolServer string `json:"tool_server,omitempty"`

	// Rotatable is whether a person can ask for a seat's credential on this
	// lane to be rotated ([LaneSpec.Rotatable]).
	Rotatable bool `json:"rotatable,omitempty"`

	// Releasable is whether a person may keep the account at the vendor
	// rather than delete it ([LaneSpec.Releasable]).
	Releasable bool `json:"releasable,omitempty"`

	// ActionURL is where a person takes the lane's [ByYou] steps at the
	// vendor, which each such step drawn without one of its own carries.
	ActionURL string `json:"action_url,omitempty"`
	// Refused is every vendor permission no level grants that a pasted
	// credential must not hold, and Unneeded every other one no level grants:
	// with the levels' [Level.Grants], each permission the vendor offers is
	// in exactly one. Nil where Crewlet grants permissions itself.
	Refused  []string `json:"refused,omitempty"`
	Unneeded []string `json:"unneeded,omitempty"`
	// VendorAccount is the account, organization or team the company
	// connected, as the vendor shows it, for a step that says where to act.
	VendorAccount string `json:"vendor_account,omitempty"`

	// Managed is [ManagedExternal] where the engine does not provision this
	// lane for the company (D4b): a seat's switch still binds its tool to
	// the credential handed in, and its access is managed outside Crewlet.
	// Empty where the engine provisions it.
	Managed string `json:"managed,omitempty"`

	// Multiple is a lane whose access is a SET of Levels rather than one:
	// Confluence's space permissions by operation, on a site without space
	// roles. Its Desired.Level is the chosen ids, sorted and space-separated,
	// and the picker offers a multi-select.
	Multiple bool `json:"multiple,omitempty"`

	// RequiredLevel is, on a [Options.Multiple] lane, the level every other
	// level in a set needs, so a set naming any level holds this one too:
	// Confluence's View (`read:space`), without which Confluence grants no
	// other space permission. The picker keeps it checked while anything
	// else is, and the sync and the access routes refuse a set without it
	// ([Options.MissingRequired]). Empty where no level is required.
	RequiredLevel string `json:"required_level,omitempty"`
}

// ManagedExternal is [Options.Managed] for a lane the engine does not
// provision for the company.
const ManagedExternal = "external"

// Level returns the level with id, or false.
func (o Options) Level(id string) (Level, int, bool) {
	for i, level := range o.Levels {
		if level.ID == id {
			return level, i, true
		}
	}
	return Level{}, -1, false
}

// LevelsOf is the levels a stored level names: the one it is, or each of a
// [Options.Multiple] set. ok is false where any of them is not offered.
//
// THE EMPTY LEVEL IS A LEVEL where the vendor offers one with an empty id
// (Jira's and Confluence's product access only), on a set lane as on any
// other: an empty set is that level, not a set of nothing.
func (o Options) LevelsOf(level string) (out []Level, ok bool) {
	if strings.TrimSpace(level) == "" {
		if l, _, found := o.Level(""); found {
			return []Level{l}, true
		}
		return nil, false
	}
	ids := []string{level}
	if o.Multiple {
		ids = strings.Fields(level)
	}
	for _, id := range ids {
		l, _, found := o.Level(id)
		if !found {
			return nil, false
		}
		out = append(out, l)
	}
	return out, true
}

// StepsInput is what a vendor draws one op's steps from.
type StepsInput struct {
	Seat SeatRef
	Lane Lane
	Op   Op
	Want Desired
	Have Observed

	// Options is what the lane offers now, so a step names a level by the
	// vendor's own name rather than by its id. Zero where the caller read
	// none, and a vendor then names what it can without it.
	Options Options

	// HandIn is the name a person gives what they make at the vendor for the
	// seat ([HandInNamer]), "" where the vendor names none or the caller
	// handed no document.
	HandIn string
}

// NamedSteps is a vendor whose steps name its levels by the vendor's own
// names, which only the lane's Options can tell it (a Jira project role, a
// Confluence space role, a GitLab custom role). [Plan] prefers it to
// [Vendor.Steps]. It draws the same steps in the same order as Steps does:
// only the words differ, so an Apply that redraws without Options takes the
// same step at the same index.
type NamedSteps interface {
	StepsFor(in StepsInput) []Step
}

// drawSteps is a vendor's steps for one op, named where the vendor can.
func drawSteps(v Vendor, in StepsInput) []Step {
	var out []Step
	if named, ok := v.(NamedSteps); ok {
		out = named.StepsFor(in)
	} else {
		out = v.Steps(in.Seat, in.Lane, in.Op, in.Want, in.Have)
	}
	for i := range out {
		if out[i].By == ByYou && out[i].ActionURL == "" {
			out[i].ActionURL = in.Options.ActionURL
		}
	}
	return out
}

// Level is one level a vendor offers, as the vendor defines it.
type Level struct {
	// ID is the identifier the vendor's API takes, which the document
	// stores; Name is the vendor's exact name for it.
	ID   string `json:"id"`
	Name string `json:"name"`

	// Custom is a role the company defined at the vendor rather than one the
	// vendor ships.
	Custom bool `json:"custom,omitempty"`

	// Administers marks an administration role (D10): Datadog Admin Role,
	// GitLab Maintainer and Owner, Jira Administrators, Confluence Admin,
	// Mattermost System Admin. DERIVED by the vendor from its own facts,
	// never stored in the document.
	Administers bool `json:"administers,omitempty"`

	// Note is the sentence that says what the level means where its name
	// cannot ("Product access only": the account has the product, and the
	// site's permission schemes decide what it sees).
	Note string `json:"note,omitempty"`

	// Unavailable is why no seat can be given the level yet ([LevelGap]),
	// empty where one can. A picker offers it disabled with this reason,
	// rather than as a choice a save would only fail at.
	Unavailable string `json:"unavailable,omitempty"`

	// Grants is every vendor permission the level holds, in the vendor's own
	// names, where a person grants them by hand at the vendor (DigitalOcean's
	// token scopes); nil where Crewlet grants them itself.
	Grants []string `json:"grants,omitempty"`
}

// ToolState is whether a seat can use its credential through a tool right now.
// The values are the wire's (`tool` on a lane).
type ToolState string

const (
	// ToolOff is a seat that is off, or whose account is not active.
	ToolOff ToolState = "off"

	// ToolBuiltin is a kind whose credential the engine's own tools read
	// directly, with no server to start.
	ToolBuiltin ToolState = "builtin"

	// ToolNoServer is a kind with a tool server the company has not declared
	// in `mcp_servers`: the seat has an account and no tool to use it with.
	ToolNoServer ToolState = "no_server"

	// ToolServerOff is a kind whose every tool server the company has turned
	// off (`enabled: false`): declared, and started for nobody until it is
	// turned back on.
	ToolServerOff ToolState = "server_off"

	// ToolWaiting is a declared server whose credential is not sealed yet.
	ToolWaiting ToolState = "waiting"

	// ToolLive is a declared server with the seat's credential bound.
	ToolLive ToolState = "live"
)
