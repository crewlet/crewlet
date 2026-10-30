package seataccess

import (
	"context"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
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

// Observation is what the vendor holds, by handle then lane.
type Observation map[string]map[Lane]Observed

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
	// GitHub. Each is sealed under [SinkName](Kind, field, handle).
	Credentials []string

	// Lanes are the lanes this kind grants, each with what it offers.
	Lanes []LaneSpec
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

	// CreatedBy and EnabledBy are who makes the account and who turns it
	// back on: [ByYou] for a GitHub App's creation and a Slack install.
	CreatedBy StepActor
	EnabledBy StepActor

	// Rotatable is whether a person can ask to rotate the credential.
	// GitHub issues hourly tokens, so it has nothing to rotate; Slack's
	// rotate revokes the bot token and waits for a person to approve the
	// reinstall.
	Rotatable bool
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
}

// Level returns the level with id, or false.
func (o Options) Level(id string) (Level, int, bool) {
	for i, level := range o.Levels {
		if level.ID == id {
			return level, i, true
		}
	}
	return Level{}, -1, false
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

	// ToolWaiting is a declared server whose credential is not sealed yet.
	ToolWaiting ToolState = "waiting"

	// ToolLive is a declared server with the seat's credential bound.
	ToolLive ToolState = "live"
)
