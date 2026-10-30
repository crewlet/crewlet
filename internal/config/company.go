package config

import (
	"errors"
	"fmt"
	"iter"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/queue"
)

// Company is Tier B — founder-owned, live-editable, versioned in the store.
//
// Everything that defines what the company IS and how its agents behave:
// identity, providers, integrations, tool servers, seats, units, learning
// and turn-engine settings, budgets. One revision is active at a time and is
// delivered to a running engine; nothing here needs a restart.
//
// Secrets in it are POINTERS. A ${VAR} survives parsing, storage and export
// untouched and is resolved only where a provider or transport is built —
// see [Resolver].
type Company struct {
	// Name is the company's identity. It is also half of every agent
	// seat's derived id (uuid5 over org name and handle), so renaming a
	// company orphans every seat's diary, onboarding markers and
	// counterparty profiles.
	Name string `yaml:"name" json:"name" js:"required" desc:"Company name. Half of every seat's derived id: effectively permanent."`

	Mission  string   `yaml:"mission,omitempty" json:"mission,omitempty" desc:"One line on what the company is for; reaches every seat's prompt."`
	Vision   string   `yaml:"vision,omitempty" json:"vision,omitempty" desc:"Longer statement of where the company is going."`
	Policies []string `yaml:"policies,omitempty" json:"policies,omitempty" desc:"Company-wide rules injected into every seat's prompt."`

	// Timezone is the company's ONE clock, as an IANA name; empty is UTC.
	// Read it through [Company.Location].
	//
	// Every calendar edge the engine cuts is cut on it: where "today",
	// "this week" and "this month" begin for a `due=` filter, a due band, a
	// row's overdue mark and a person's own day; the instant a relative date
	// ("next Friday") or an all-day date resolves to; and the wall clock a
	// schedule that names no zone of its own fires on. One clock, and a
	// top-level field rather than a setting of any one subsystem, because
	// the three this replaced (Worklet's own clock, the scheduler's
	// default zone and the UTC every other day boundary assumed) disagreed
	// about where one company's day began (ADR-0018).
	//
	// A clock for AUTHORED INSTANTS AND CALENDAR BOUNDARIES only: no
	// duration is measured against it, because a duration measured on a
	// wall clock changes length twice a year.
	Timezone string `yaml:"timezone,omitempty" json:"timezone,omitempty" desc:"The company's one clock, an IANA zone (default UTC): where its days and weeks begin, what a relative or all-day date resolves to, and what a schedule naming no zone fires on."`

	// Integrations is how external events REACH agents and how
	// notifications are delivered — the inbound side. Agent TOOLS are
	// configured separately under MCPServers; the two are deliberately
	// different blocks because they are different directions.
	Integrations Integrations `yaml:"integrations,omitempty" json:"integrations"`

	// Tracker is which work tracker this company runs.
	Tracker Tracker `yaml:"tracker,omitempty" json:"tracker,omitempty"`

	// Knowledge is which knowledge base this company runs, and the read
	// search.
	Knowledge Knowledge `yaml:"knowledge,omitempty" json:"knowledge"`

	// Artifacts is the company's artifacts policy. See [CompanyArtifacts].
	Artifacts CompanyArtifacts `yaml:"artifacts,omitempty" json:"artifacts,omitzero"`

	// Assistant is whether the Dashboard Assistant runs for this company,
	// on which models, and the bounds of one turn. See [Assistant].
	Assistant Assistant `yaml:"assistant,omitempty" json:"assistant,omitzero"`

	// SkillVariables is an operator-defined name -> value map substituted
	// into tool-skill text wherever a skill writes ${name}.
	//
	// It lets a static, knowledge-base-authored skill reference a per-org
	// fact its body cannot know — the tenant's wiki base URL, say, so
	// agents share human-clickable links rather than the gateway URLs the
	// tools return. The engine carries no integration-specific knowledge:
	// operators name the variables, skills reference them.
	//
	// Values render into LLM prompts (and the event store, and the
	// dashboard, like any other prompt content) — treat them like a policy
	// string and do not point them at secrets.
	SkillVariables map[string]string `yaml:"skill_variables,omitempty" json:"skill_variables,omitempty" desc:"name -> value facts substituted into tool-skill text. Keys must be identifiers."`

	Providers  Providers  `yaml:"providers,omitempty" json:"providers"`
	TurnEngine TurnEngine `yaml:"turn_engine,omitempty" json:"turn_engine"`
	Learning   Learning   `yaml:"learning,omitempty" json:"learning"`
	Scheduling Scheduling `yaml:"scheduling,omitempty" json:"scheduling"`

	// MCPServers are the tool servers agents call through. ALL tool
	// servers are declared here, including the ones an integration block
	// also mentions: integrations carry non-tool config (admin
	// credentials, webhook secrets) and the engine never derives an MCP
	// server from one.
	MCPServers []MCPServer `yaml:"mcp_servers,omitempty" json:"mcp_servers,omitempty" desc:"Tool servers, stdio or http. Per-agent credentials live in role.mcp_env."`

	// TokenBudget is the org-wide LLM token ceiling per calendar window,
	// across every seat; a window it does not name is uncapped. Charged
	// before a seat's own budget, so the first completion that would cross
	// it stops that turn. See [TokenBudget].
	TokenBudget TokenBudget `yaml:"token_budget,omitempty" json:"token_budget,omitzero" desc:"Org-wide token ceilings across all seats, per calendar window on the company clock: day, week and month, each optional. Absent = uncapped."`

	// NotificationRateLimit caps how many INBOUND notifications may wake one
	// seat per second; 0, the default, is unlimited.
	//
	// A safety valve against a webhook storm or a notification loop, never a
	// burst policy — a dropped notification is context lost, where a
	// coalesced one is context preserved, so bursts are coalescing's job.
	// It fails OPEN: a valve that cannot reach its counter must not stop
	// real notifications.
	NotificationRateLimit int `yaml:"notification_rate_limit,omitempty" json:"notification_rate_limit,omitempty" js:"min=0" desc:"Inbound notifications one seat may be woken by per second; 0 (default) = unlimited."`

	// NotificationCoalesceWindowSeconds is how long after the first
	// pending event an idle seat's inbox waits to absorb a burst before
	// dispatching. 0 adds no latency — a backlog that piled up while the
	// seat was busy still coalesces, because the drain collects everything
	// already queued.
	//
	// Capped at 60s: the window counts against the broker's ack-timeout
	// budget, which has to fit collection plus one whole turn, so an
	// unbounded linger reintroduces mid-drain redelivery.
	NotificationCoalesceWindowSeconds float64 `yaml:"notification_coalesce_window_seconds,omitempty" json:"notification_coalesce_window_seconds,omitempty" js:"min=0;max=60" desc:"Inbox linger before dispatch, 0..60s. 0 adds no latency."`

	// NotificationCoalesceMaxBatch caps the events collected into one
	// DRAIN, before it is partitioned by conversation — so it bounds a
	// digest as a consequence (a digest can never exceed it) and it bounds
	// the drain itself, which has to fit the broker's ack budget alongside
	// a whole turn. A larger backlog arrives as successive capped drains
	// rather than one unbounded megaprompt.
	//
	// Per drain rather than per digest, and the difference is visible: a
	// drain spanning several conversations shares the cap between them, so
	// 24 events over 4 conversations at 20 is two drains and 8 turns rather
	// than 4. Raise it for a company whose seats routinely serve several
	// busy threads at once.
	// Capped at 100: the digest IS the trigger, and a trigger is re-sent to
	// the model on every round of the tool loop — so a constituent count is
	// really a multiplier on the dominant repeated content of a turn. Five
	// times the default leaves room for a genuinely busy thread; past it,
	// one turn is reasoning over a hundred messages at once and successive
	// capped batches are strictly better than one megaprompt, which is the
	// cap's whole purpose.
	NotificationCoalesceMaxBatch int `yaml:"notification_coalesce_max_batch,omitempty" json:"notification_coalesce_max_batch,omitempty" js:"min=1;max=100" desc:"Events drained into one batch before partitioning by conversation, 1..100. Bounds a digest too, since a digest cannot exceed a drain."`

	// Workers are the reusable delegate templates a seat's executor hands
	// work to, keyed by the name it types into a `delegate` call. See
	// workers.go for why a template exists at all and why naming a tool in
	// one grants nothing.
	Workers map[string]Worker `yaml:"workers,omitempty" json:"workers,omitempty" desc:"Reusable delegate templates, keyed by the name an executor calls them by."`

	// Roles are the seats that belong to no unit — a CEO, a cross-cutting
	// advisor, the founder's own human seat.
	Roles []Role `yaml:"roles,omitempty" json:"roles,omitempty" desc:"Seats belonging to no unit."`

	// Units are the hierarchy, nesting to any depth.
	Units []Unit `yaml:"units,omitempty" json:"units,omitempty" desc:"The org hierarchy; units nest to any depth."`

	// SeatAccessMigrated are the integrations whose agent seats state their
	// access per seat ([Company.SeatAccessMigratedFor]). Written by the
	// engine's migration onto per-seat access, one integration at a time;
	// the access pass for an integration not listed acts on no seat.
	SeatAccessMigrated []string `yaml:"seat_access_migrated,omitempty" json:"seat_access_migrated,omitempty" desc:"Integrations whose agent seats state their access per seat. Written by the engine's migration."`
}

// coalesceWindowMax bounds the inbox linger. See the field's own comment:
// the window is spent out of the broker's ack-timeout budget.
//
// THE CONTRACT'S OWN NUMBER, not a copy of it. [queue.BatchOptions] clamps to
// this regardless of who set the field — programmatic construction bypasses
// validation — and queue.go states that "config validation mirrors this cap".
// A second literal here made that a claim nothing checked, and two literals
// that must agree are two literals that eventually do not.
const coalesceWindowMax = queue.MaxLingerSeconds

// coalesceMaxBatchMax bounds one DRAIN — and a digest with it, since a digest
// cannot exceed the drain it came from. See the field's own comment for why
// 100.
const coalesceMaxBatchMax = 100

// DefaultCompany is a Tier B config with every default applied. The loader
// decodes into it, so an absent key keeps its default and a valueless key
// (`learning:`) reads as unset rather than as a wiped block.
func DefaultCompany() Company {
	return Company{
		Providers:                    DefaultProviders(),
		TurnEngine:                   DefaultTurnEngine(),
		Learning:                     DefaultLearning(),
		Scheduling:                   DefaultScheduling(),
		NotificationCoalesceMaxBatch: 20,
	}
}

// Location is the company's ONE clock (ADR-0018): the zone [Company.Timezone]
// names, or UTC when it names none.
//
// Validation refuses a name [period.LoadZone] does not load, so on every
// company a node admits or applies this is the configured clock. A company
// assembled in code and never validated may hold a name that does not load,
// and reads as UTC — the default clock, and the one reading that is the same
// on every node — rather than handing a caller a nil location to crash on.
func (c *Company) Location() *time.Location {
	if c == nil {
		return time.UTC
	}
	loc, err := period.LoadZone(c.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// skillVariableKey is the substitution-identifier rule.
//
// The operator key space has to be provably identical to the ${name} render
// grammar the skill layer uses, or a key like base-url would be accepted
// here and then never substituted anywhere — a silent no-op at render time
// rather than an error at load.
var skillVariableKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate reports every Tier B rule this config breaks, joined: the runnable
// rules and the admission rules both.
//
// It is the check for a document somebody SUBMITS: a PUT, a PATCH, a
// per-entity write, a setup write, `crewlet config import` and `crewlet
// validate`. A revision being APPLIED is held to the runnable rules only; see
// [Company.ValidateRunnable].
//
// It validates the ORG as well: building the hierarchy is where duplicate
// handles, unrunnable schedules and human-seat rule violations surface, and
// a config that parses into a company nobody can run has not been validated.
//
// The CHARTER's rules are not among them: they judge what a write changes
// against the revision it replaces rather than the document it leaves, so an
// old fault never refuses an unrelated write. Every writer asks
// [Company.ValidateCharterChange] beside this.
func (c *Company) Validate() error {
	o, index := c.organization()
	return index.locate(errors.Join(c.validateRunnable(o), c.validateAdmission(o)))
}

// ValidateRunnable reports only the RUNNABLE rules: everything a running
// company depends on, which is every rule except the admission ones.
//
// It is the check for a revision about to be APPLIED (a node's reconcile
// tick, a boot from the store, a reload and a revert) and for building an
// epoch. A revision is admitted under the rules of the build that wrote it,
// and the admission set is the part of the rule set a build is free to
// change: a later build may add a rule or relax one, and during a rolling
// upgrade this build applies the revisions a newer peer admitted. Nothing
// about RUNNING depends on an admission rule, so applying under the runnable
// rules only is what keeps one build of a rolling upgrade from refusing a
// revision its peer admitted — which would split the fleet's epoch. The
// admission violations are reported by [Company.ValidateAdmission] as
// warnings instead, and the next write that keeps them is refused.
func (c *Company) ValidateRunnable() error {
	o, index := c.organization()
	return index.locate(c.validateRunnable(o))
}

// ValidateAdmission reports only the ADMISSION rules: the rules a submitted
// document is refused for and a stored revision is merely warned about.
// Today they are the org's duplicate unit keys, a unit reference on a seat
// declared inside a unit of a different key and a schedule's cron the grammar
// refuses (see [org.Organization.ValidateAdmission], which also says why no
// NAME is held unique), a unit with no id, duplicate sandbox setup step names
// within one list, a GitHub App on a human seat, an avatar the dashboard
// cannot draw or one on a human seat ([Company.validateAvatars]), a
// providers.llm key outside its grammar or sharing another's credential bundle
// variable ([Company.validateLLMKeys]), a
// providers.llm entry's credential configured where its CLI never reads it or
// handed over under a variable name no store can hold
// ([LLMProvider.validateCredentials]), a model with no provider segment on a
// CLI that reads one ([LLMProvider.validateModel]), a setup step writing a
// file a box it reaches refuses ([Company.validateSetupFileHomes]), an
// executor box's `optional_keys` entry leaving nothing out
// ([Company.validateRunOptionalKeys]),
// and the five WHOLE-DOCUMENT rules in [Company.validateFileRules] — an
// `mcp_env` key naming no server, an `optional_keys` entry leaving nothing
// out, one http header spelled two ways where nothing says which is sent, two
// seats declaring one email, and a reference that is shaped like neither a
// handle nor a key. And the `assistant:` block ([Company.validateAssistant]),
// whose chain is held to its rules again wherever it is handed out, and the
// per-seat access rules ([Company.validateSeatAccess]): every seat block says
// whether it is on, holds access the vendor offers within the company's reach,
// and writes no credential the engine manages.
func (c *Company) ValidateAdmission() error {
	o, index := c.organization()
	return index.locate(c.validateAdmission(o))
}

// validateAdmission is [Company.ValidateAdmission] over an organization the
// caller already built.
func (c *Company) validateAdmission(o *org.Organization) error {
	return errors.Join(o.ValidateAdmission(), c.validateUnitIDs(),
		c.validateSetupStepNames(), c.validateHumanSeatApps(), c.validateAvatars(),
		c.validateLLMKeys(), c.validateLLMCredentials(), c.validateLLMModels(), c.validateSetupFileHomes(),
		c.validateRunOptionalKeys(), c.validateFileRules(), c.validateAssistant(),
		c.validateSeatAccess())
}

// validateUnitIDs requires an id on every authored unit, at any depth. See
// [Unit.requireIDs] for why this is an admission rule rather than a runnable
// one.
func (c *Company) validateUnitIDs() error {
	var p problems
	for i := range c.Units {
		p.wrap(c.Units[i].requireIDs(idx(field("units"), i)))
	}
	return p.err()
}

// validateHumanSeatApps refuses a per-seat GitHub App on a human seat.
//
// A seat's `integrations.github` is the seat's OWN app: the bot identity an
// agent acts as on GitHub. A person acts as their own login, which is
// `contact.github_login`, and every engine path that creates, installs or
// reconciles an app skips a human seat. The block therefore reads as a
// working setting and does nothing, which is the silence the org model's
// human-seat rule exists to end for every other agent-only field.
//
// CHECKED HERE rather than beside that rule in [org.Role.Validate], although
// the runtime seat carries the app too ([org.Role.GitHub]): that rule is a
// RUNNABLE one, and this is an ADMISSION rule, because nothing about running
// depends on it — every path that would act on the block skips a human seat,
// so a stored revision carrying one runs as it did.
func (c *Company) validateHumanSeatApps() error {
	var p problems
	for role, path := range c.EachRole() {
		if role.Kind != org.KindHuman || role.Integrations.GitHub == nil {
			continue
		}
		p.add(at(path, "integrations.github"), ErrConflict,
			"a human seat has no GitHub App of its own: an app is the identity "+
				"an agent acts as, and a person acts as their own login, "+
				"contact.github_login. Remove this block, or make the seat an "+
				"agent seat")
	}
	return p.err()
}

// validateSetupStepNames refuses two sandbox setup steps of one name in the
// same list: `providers.sandbox.setup`, or one seat's `sandbox.setup`.
//
// A step's name is its IDENTITY. Its env and files are credentials, and a
// config read masks them, so sending the read back restores each step's
// masks from the stored step of the same name. Two steps sharing a name
// give the restore nothing to tell them apart by, and it refuses to guess:
// every write carrying that list would then be refused with a standing mask
// on credentials nobody edited. A name is also what a setup failure and its
// log line point at, and a duplicate points at two steps.
//
// An ADMISSION rule rather than a runnable one, because nothing about running
// depends on it: duplicate names still apply every step in order. Compared as
// the exact string the restore matches on, and a blank name is skipped: the
// step's own rule already refuses it.
func (c *Company) validateSetupStepNames() error {
	var p problems
	if c.Providers.Sandbox != nil {
		p.wrap(uniqueSetupStepNames(at(at(field("providers"), "sandbox"), "setup"), c.Providers.Sandbox.Setup))
	}
	for role, path := range c.EachRole() {
		if role.Sandbox != nil {
			p.wrap(uniqueSetupStepNames(at(at(path, "sandbox"), "setup"), role.Sandbox.Setup))
		}
	}
	return p.err()
}

// uniqueSetupStepNames reports each name more than one step in this list
// carries, once per name, naming every step that carries it.
func uniqueSetupStepNames(path Path, steps []SandboxSetupStep) error {
	var p problems
	positions := map[string][]int{}
	var order []string
	for i := range steps {
		name := steps[i].IdentityKey()
		if strings.TrimSpace(name) == "" {
			continue
		}
		if _, seen := positions[name]; !seen {
			order = append(order, name)
		}
		positions[name] = append(positions[name], i)
	}
	for _, name := range order {
		found := positions[name]
		if len(found) < 2 {
			continue
		}
		places := make([]string, len(found))
		for i, position := range found {
			places[i] = idx(path, position).String()
		}
		p.add(path, ErrConflict,
			"duplicate setup step name %q: %d steps carry it (%s). A step's "+
				"credentials are restored by its name when a config read is "+
				"sent back, and a failure names the step, so give each of "+
				"these steps its own name",
			name, len(found), strings.Join(places, "; "))
	}
	return p.err()
}

// validateRunnable is [Company.ValidateRunnable] over an organization the
// caller already built, so [Company.Validate] normalizes the tree once for
// both classes.
func (c *Company) validateRunnable(o *org.Organization) error {
	var p problems

	// An UNRESOLVED REDACTION MASK, first, because it is the one fault here
	// that comes from this process rather than from the document's author.
	// A credential still holding the marker means a config read was edited
	// and sent back, and the mask could not be matched to what it hid: a
	// member that is NEW or RENAMED has no prior value of its own, and a
	// member whose identity the stored revision does not give to exactly one
	// member (a duplicate, or an empty one) cannot be matched without
	// guessing. Storing it silently would hand a provider the literal
	// "__redacted__" as an API key, and the failure would surface hours later
	// as an authentication error naming nothing about where it came from.
	//
	// The message names every cause, because the restore does not record
	// which one applied and an operator reading only "new or renamed" about
	// a unit they did neither to has nothing to act on.
	for _, path := range c.UnresolvedMasks() {
		p.add(path, ErrUnknownValue,
			"still holds the redaction marker %q: a masked credential could "+
				"not be matched to the value it hid. A seat is matched by its "+
				"handle, a unit by its key, and an MCP server and a sandbox "+
				"setup step by its name, so this happens when the member is "+
				"new or was renamed, or when the stored revision gives that "+
				"identity to more than one member or the member has none. Write "+
				"the real value or a ${VAR} reference here",
			Redacted)
	}

	if strings.TrimSpace(c.Name) == "" {
		p.add(field("name"), ErrMissing, "the company needs a name: it is half of every seat's derived id")
	}
	// THE ONE CLOCK, through the one reader every other zone name goes
	// through, so the company's clock and a schedule's own zone are
	// admitted by the same rule — a host's own clock refused by both.
	if _, err := period.LoadZone(c.Timezone); err != nil {
		p.add(field("timezone"), ErrUnknownValue, "%v", err)
	}
	p.wrap(c.Artifacts.validate(field("artifacts")))

	// A SEAT MAY NOT BE CALLED WHAT "NOBODY" IS CALLED.
	//
	// Datadog's fallback takes a seat handle or DatadogIgnore, and a company
	// with a seat of that name would have it silenced by its own name: every
	// alert meant for it dismissed, on a screen reporting the configuration
	// exactly as written. Refusing the name is the only place this can be
	// caught, because by the time the parser reads a fallback the two are
	// the same string.
	//
	// AND THE FALLBACK MUST NAME SOMEBODY WHO CAN BE WOKEN. `route_to` is
	// the only routing floor in this file, and the two ways it silently
	// fails are a handle no seat has — resolving to nothing, one
	// `notification_undeliverable` warning per untagged alert, for ever —
	// and one naming a HUMAN seat, which resolves fine and is then dropped
	// as a self-action. Both read as correct configuration on every screen,
	// which is the state the field exists to prevent. This is the only
	// place either can be caught: the parser deliberately does not consult
	// the roster (a bad monitor TAG must stay visible as the operator's
	// typo it is), and the setup form is one of three write paths.
	fallback := ""
	if dd := c.Integrations.Datadog; dd != nil && dd.Enabled {
		fallback = strings.TrimSpace(dd.RouteTo)
	}
	agents, handles := 0, make([]string, 0, 8)
	routed := false
	for role, path := range c.EachRole() {
		seat := role.Seat()
		if seat.Handle() == DatadogIgnore {
			p.add(at(path, "handle"), ErrUnknownValue,
				"a seat cannot be called %q: it is what integrations.datadog.route_to "+
					"means by nobody, so a seat of that name would be silenced by "+
					"its own handle. %q derives that handle from its name",
				DatadogIgnore, role.Name)
		}
		if !seat.IsAgent() {
			continue
		}
		agents++
		handles = append(handles, seat.Handle())
		if seat.Handle() == fallback {
			routed = true
		}
	}
	if fallback != "" && fallback != DatadogIgnore && !routed {
		switch {
		case !org.ValidHandle(fallback):
			p.add(field("integrations.datadog.route_to"), ErrUnknownValue,
				"%q is not a seat handle: a handle is lowercase letters, "+
					"digits and hyphens starting with a letter or digit, so "+
					"this names no seat and every untagged alert is verified, "+
					"counted and delivered to nobody", fallback)
		case agents == 0:
			p.add(field("integrations.datadog.route_to"), ErrUnknownValue,
				"%q names no seat: this company declares no agent seat at all, "+
					"so there is nobody an untagged alert can wake", fallback)
		default:
			p.add(field("integrations.datadog.route_to"), ErrUnknownValue,
				"%q is not an agent seat in this company. An alert naming no "+
					"owner wakes this handle, and one that resolves to nothing "+
					"— or to a human seat, which is dropped as a self-action — "+
					"is coverage that is not there. The agent seats are: %s",
				fallback, strings.Join(handles, ", "))
		}
	}

	for key := range c.SkillVariables {
		if !skillVariableKey.MatchString(key) {
			p.add(entry(field("skill_variables"), key), ErrUnknownValue,
				"keys must be substitution identifiers matching "+
					"[A-Za-z_][A-Za-z0-9_]*: a key like %q would never be "+
					"substituted into a skill's ${name} reference", key)
		}
		if key == ReservedBaseURLVariable {
			p.add(entry(field("skill_variables"), key), ErrConflict,
				"%s is reserved: the engine sets it from "+
					"`api.external_url` in the OPERATOR's own Tier A file, "+
					"which is where this deployment's address belongs — one "+
					"company document then runs at staging's address and at "+
					"production's without carrying either. Set it there and "+
					"delete this line",
				ReservedBaseURLVariable)
		}
		if key == ReservedKitVersionVariable {
			p.add(entry(field("skill_variables"), key), ErrConflict,
				"%s is reserved: the engine sets it to the newest artifact "+
					"kit this build embeds, which is the only kit release a "+
					"page linked now is sure to be served. A declared value "+
					"would keep naming one kit after an upgrade embeds the "+
					"next. Delete this line",
				ReservedKitVersionVariable)
		}
	}

	p.wrap(c.TokenBudget.validate(field("token_budget")))
	if c.NotificationRateLimit < 0 {
		p.add(field("notification_rate_limit"), ErrOutOfRange,
			"must not be negative, got %d", c.NotificationRateLimit)
	}
	if w := c.NotificationCoalesceWindowSeconds; w < 0 || w > coalesceWindowMax {
		p.add(field("notification_coalesce_window_seconds"), ErrOutOfRange,
			"must be 0..%v seconds, got %v: the window is spent out of the "+
				"broker's ack-timeout budget, which also has to fit a whole turn",
			coalesceWindowMax, w)
	}
	if b := c.NotificationCoalesceMaxBatch; b < 1 || b > coalesceMaxBatchMax {
		p.add(field("notification_coalesce_max_batch"), ErrOutOfRange,
			"must be between 1 and %d, got %d", coalesceMaxBatchMax,
			c.NotificationCoalesceMaxBatch)
	}

	p.wrap(c.Providers.validate(field("providers")))
	p.wrap(c.TurnEngine.validate(field("turn_engine")))
	p.wrap(c.Learning.validate(field("learning")))
	p.wrap(c.Scheduling.validate(field("scheduling")))
	p.wrap(c.Integrations.validate(field("integrations")))
	p.wrap(c.validateKnowledgeBackend())
	p.wrap(c.validateContainerKeys())
	p.wrap(c.validateProviderKeys())
	p.wrap(c.validateWorkers())
	p.wrap(c.validateExecutors())
	p.wrap(c.validateExecutorFloor())
	p.wrap(c.validateSandboxPlacement())

	seen := make(map[string]struct{}, len(c.MCPServers))
	for i := range c.MCPServers {
		path := idx(field("mcp_servers"), i)
		p.wrap(c.MCPServers[i].validate(path))
		name := c.MCPServers[i].Name
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			// Two servers under one name is not a merge — the second
			// wins wherever the engine indexes by name, so the first's
			// tools silently vanish from every prompt.
			p.add(at(path, "name"), ErrConflict, "duplicate MCP server name %q", name)
		}
		if name == ToolServerName {
			// The same collision one layer out: the engine writes the
			// seat's tool bridge into every executor box under this name,
			// and a company server called the same is overwritten there —
			// its tools gone from the run with nothing saying why. See
			// [ToolServerName].
			p.add(at(path, "name"), ErrConflict,
				"%q is reserved for the seat's own tool bridge, which every "+
					"executor box sees under that name; a server called the "+
					"same would be replaced by it in the box. Rename the server",
				name)
		}
		seen[name] = struct{}{}
	}

	for i := range c.Roles {
		p.wrap(c.Roles[i].validate(idx(field("roles"), i)))
	}
	for i := range c.Units {
		p.wrap(c.Units[i].validate(idx(field("units"), i)))
	}
	p.wrap(c.Tracker.Worklet.validate(field("tracker.worklet")))
	// A WORKLET BLOCK ON A COMPANY THAT DOES NOT RUN WORKLET describes
	// nothing, and the failure that produces is silence: an operator sets an
	// inbox horizon for inbox rows somebody else's tracker will never write.
	//
	// The check is against the DERIVED backend rather than the literal
	// field, because an empty `backend` with no Jira integration IS
	// Worklet, and refusing the default configuration would be the opposite
	// of the intent.
	if c.Tracker.Worklet != nil && c.TrackerBackendFor() != TrackerWorklet {
		p.add(field("tracker.worklet"), ErrConflict,
			"this company's tracker is %q, and `tracker.worklet` is Worklet's "+
				"policy, so nothing would read it. Remove the block, or run "+
				"Worklet", c.TrackerBackendFor())
	}

	// The hierarchy's own rules (duplicate handles, human seats carrying
	// runtime fields, schedules with no runner) are the org model's, and
	// they only exist once the tree is built.
	p.wrap(o.Validate())
	return p.err()
}

// ReservedBaseURLVariable is the skill variable carrying this deployment's
// external address, so a skill can compose a link a person can click.
//
// The engine sets it from `api.external_url` and this package refuses a
// company that declares it: two sources for one address is how a skill comes
// to link at the deployment the company used to run on.
const ReservedBaseURLVariable = "crewlet_base_url"

// ReservedKitVersionVariable is the skill variable carrying the newest artifact
// kit release this build embeds, so the artifacts skill tells a seat the exact
// `/_crewlet/kit/<version>/` to link.
//
// The engine sets it from its own embedded kit tree and this package refuses a
// company that declares it: a declared version is one an upgrade moves past
// without anybody editing the company document, and a page linked to a kit
// older than the brand slots never takes the company's brand.
const ReservedKitVersionVariable = "crewlet_kit_version"

// validateKnowledgeBackend holds the rule that a read scope needs the
// backend it narrows.
//
// The knowledge base is single-homed, and since Confluence is the only
// backend that is now structural rather than enforced — there is no pair of
// searchers left to disagree about what the company already knows. What is
// still worth refusing is a scope for a backend that is not configured: it
// reads as a working narrowing and narrows nothing, which is the silence
// this rule exists to end, one level down from the block itself.
//
// Selection keys on integration-block PRESENCE, because the scope list
// defaults to empty (which means unscoped) and so cannot be the signal.
func (c *Company) validateKnowledgeBackend() error {
	var p problems

	if b := c.Knowledge.Backend; b != "" && !b.Valid() {
		p.add(field("knowledge.backend"), ErrShape,
			"knowledge.backend must be doclet, confluence or none")
	}
	if b := c.Tracker.Backend; b != "" && !b.Valid() {
		p.add(field("tracker.backend"), ErrShape,
			"tracker.backend must be worklet, jira or none")
	}

	// A BACKEND AND ITS VENDOR TOGETHER IS THE MIRROR THE DOCTRINE FORBIDS.
	// Both would route, both would answer a search, and the company would
	// have two places its work lives with nothing keeping them in step —
	// which is precisely the cache with no invalidation story that the
	// no-task-engine decision was written against. Refused at the authored
	// path so the message names what to delete.
	if c.Knowledge.Backend == KnowledgeDoclet && c.Integrations.Confluence != nil {
		p.add(field("knowledge.backend"), ErrConflict,
			"Doclet and integrations.confluence cannot both run: pages would "+
				"live in two places and nothing would keep them in step. Remove one")
	}
	if c.Knowledge.Backend == KnowledgeConfluence && c.Integrations.Confluence == nil {
		p.add(field("knowledge.backend"), ErrConflict,
			"knowledge.backend: confluence needs integrations.confluence")
	}
	if c.Tracker.Backend == TrackerWorklet && c.Integrations.Jira != nil {
		p.add(field("tracker.backend"), ErrConflict,
			"Worklet and integrations.jira cannot both run: work would be filed "+
				"in two places and a unit's project key would name two trackers. "+
				"Remove one")
	}
	if c.Tracker.Backend == TrackerJira && c.Integrations.Jira == nil {
		p.add(field("tracker.backend"), ErrConflict,
			"tracker.backend: jira needs integrations.jira")
	}

	// A setting for a backend that is switched off reads as configuration
	// and configures nothing — the silence this whole rule exists to end.
	if c.KnowledgeBackendFor() == KnowledgeNone {
		if len(c.Knowledge.KnowledgeScope) > 0 {
			p.add(field("knowledge.scope"), ErrConflict,
				"a read scope needs a knowledge backend")
		}
		// VECTORS NEED SOMETHING TO SEARCH, and a knowledge base is not
		// the only thing there is: Worklet is a corpus too, and a company
		// that keeps its knowledge in Confluence's competitor's head and its
		// work here is entitled to semantic recall over the work.
		if c.Knowledge.Vectors != nil && *c.Knowledge.Vectors &&
			c.TrackerBackendFor() != TrackerWorklet {
			p.add(field("knowledge.vectors"), ErrConflict,
				"vectors need a knowledge backend or Worklet; with neither "+
					"there is no corpus to embed")
		}
	}
	if c.Knowledge.Vectors != nil && *c.Knowledge.Vectors && c.Providers.Embeddings == nil {
		p.add(field("knowledge.vectors"), ErrConflict,
			"vector recall needs providers.embeddings — there is nothing to "+
				"compute an embedding with")
	}
	return p.err()
}

// Knowledge is which knowledge base the company runs and how much of it a
// search may read.
//
// The scope is the ONLY thing that narrows the query-time search, and it is
// role-independent on purpose: a unit's own space is IDENTITY (where its page
// activity routes, where it writes) and letting an identity double as a read
// scope is how an agent ends up unable to read the page it was told to
// follow.
//
// Empty is unscoped. On Doclet that means the whole company, because the
// engine IS the boundary: every reader is a seat of one company and there is
// no second account to launder a read through. On Confluence it
// means whatever the asking seat's own account can read, which is why a
// credential-less seat searching unscoped there gets nothing: an unscoped
// query on a shared admin token would show one seat pages its own user never
// could.
type Knowledge struct {
	// Backend is which knowledge base this company runs.
	//
	// Empty DERIVES from what the company connected: one that declares
	// integrations.confluence gets `confluence`, and one that declares
	// nothing gets `doclet`, so a quickstart company has a wiki without
	// asking for one. Connecting Confluence already says where the pages
	// live (Doclet beside it is refused, as two places pages live with
	// nothing keeping them in step), so requiring `backend: confluence` as
	// well would be a second spelling of one decision, with a refusal for
	// the company that wrote only the first.
	Backend KnowledgeBackend `yaml:"backend,omitempty" json:"backend,omitempty" js:"enum=doclet|confluence|none" desc:"Which knowledge base: doclet, confluence, or none. Empty derives from integrations.confluence."`

	// KnowledgeScope narrows the search to these containers. Empty is
	// unscoped — see the type doc for what that means per backend.
	KnowledgeScope []string `yaml:"scope,omitempty" json:"scope,omitempty" desc:"Org-wide read scope. Empty = unscoped."`

	// SkillsContainer is where tool-skill pages live.
	//
	// THREE-VALUED, and the empty string is an answer: absent takes the
	// reserved default, a name takes that container, and an explicit ""
	// turns tool skills off — no sync, no routing exclusion, no search
	// exclusion. The off switch exists because the default reserves a real
	// key, and a company whose ordinary work container happens to be it
	// would otherwise have it silently dropped from every search.
	SkillsContainer *string `yaml:"skills_container,omitempty" json:"skills_container,omitempty" desc:"Container holding tool-skill pages. Absent takes the default; \"\" turns tool skills off."`

	// RootSpace is the container holding the ORGANISATION's own pages —
	// the root Onboarding page every seat's chain starts at.
	//
	// A unit writes in its own space and the org root had nowhere, so the
	// convention that every scope publishes an Onboarding page had a hole
	// at the top of the chain exactly where a new seat starts reading.
	RootSpace *string `yaml:"root_space,omitempty" json:"root_space,omitempty" desc:"Container for org-level pages such as the root Onboarding. Absent takes the default."`

	// Vectors adds semantic recall to the knowledge search and Worklet's
	// item search.
	//
	// A POINTER because the zero value is a real setting and the absent
	// value is a different one: unset DERIVES from whether the company
	// configured an embeddings provider (it already has one for the diary,
	// so a company that pays for embeddings gets the better search), and an
	// explicit false keeps the search purely lexical on a company that has
	// one for its diary and does not want its pages embedded — which is
	// why false stops the embedding duty as well, not only the query side.
	Vectors *bool `yaml:"vectors,omitempty" json:"vectors,omitempty" desc:"Fuse semantic recall into knowledge and work search; false also stops embedding the corpus. Unset derives from providers.embeddings."`
}

// KnowledgeBackend is which knowledge base a company runs.
type KnowledgeBackend string

// The knowledge backends.
const (
	// KnowledgeDoclet is Doclet, the engine's own knowledge base: the state
	// log's pages domain. Every change is one record on the pages log,
	// applied into the replicated estate every data node holds, and a node
	// without `data` reads it through the estate router. See internal/doclet.
	KnowledgeDoclet KnowledgeBackend = "doclet"

	// KnowledgeConfluence is Confluence, read live at query time.
	KnowledgeConfluence KnowledgeBackend = "confluence"

	// KnowledgeNone is a company with no knowledge base. The prefetch
	// block stays empty and search_knowledge is not registered.
	KnowledgeNone KnowledgeBackend = "none"
)

// KnowledgeBackends is every knowledge backend, in the order the docs name
// them: the closed set the schema offers, the validator accepts and the org
// projection's `services.knowledge` carries, so each of those reads one list.
var KnowledgeBackends = []KnowledgeBackend{KnowledgeDoclet, KnowledgeConfluence, KnowledgeNone}

// Valid reports whether b is a backend this build serves.
func (b KnowledgeBackend) Valid() bool { return slices.Contains(KnowledgeBackends, b) }

// Tracker is which work tracker the company runs.
//
// SEPARATE FROM THE KNOWLEDGE AXIS, because they are two products with
// separate routing and separate lead maps: a company running Worklet against
// a Confluence wiki, or Jira against Doclet, is an ordinary arrangement rather
// than a mixture to refuse.
type Tracker struct {
	// Backend is which tracker this company runs. Empty derives: `jira`
	// when integrations.jira is declared, `worklet` otherwise.
	Backend TrackerBackend `yaml:"backend,omitempty" json:"backend,omitempty" js:"enum=worklet|jira|none" desc:"Which work tracker: worklet, jira, or none. Empty derives from integrations.jira."`

	// Worklet is Worklet's policy: the settings that exist because the
	// company owns the tracker rather than renting one.
	//
	// PRESENT ONLY ON A COMPANY RUNNING WORKLET, and refused otherwise: an
	// inbox horizon on a company running Jira is config that describes
	// nothing, and the failure it produces is silence.
	Worklet *WorkletConfig `yaml:"worklet,omitempty" json:"worklet,omitempty"`
}

// WorkletConfig is the founder's policy over Worklet.
//
// # Why this and nothing else
//
// Everything else a tracker could be told is either a fact about the operator
// (which is Tier A, under `stream.`), a fact about the whole company rather
// than its tracker, or a decision the engine makes once for everybody. What is
// left is genuinely Worklet's own: how long a person's inbox keeps a row.
//
// The clock its dates mean USED to be here, and it is the company's top-level
// `timezone` now (ADR-0018): Worklet's "today" and the scheduler's 09:00
// are one company's calendar, and a clock held by one subsystem left every
// other day boundary cut on UTC.
type WorkletConfig struct {
	// InboxRetentionDays is how long a person's inbox keeps a row.
	//
	// THE ONE HORIZON HERE THAT DELETES ANYTHING, and it deletes a row
	// DERIVED from a record rather than the record: the history those rows
	// point at is untouched and answers for ever. A year because it is a
	// MAILBOX rather than an archive — the vendors' own defaults are a
	// quarter of that — and because the question "what was I told about in
	// 2024" is answered by the history, not by the inbox.
	InboxRetentionDays int `yaml:"inbox_retention_days,omitempty" json:"inbox_retention_days,omitempty" js:"min=0;max=3650" desc:"How long a person's inbox keeps a row (default 365, 30..3650). The history it points at is untouched."`
}

// The inbox horizon's default and bounds.
const (
	// DefaultInboxRetentionDays is a year.
	DefaultInboxRetentionDays = 365

	// MinInboxRetentionDays is a month, below which an inbox stops being
	// one: a person away for four weeks would come back to nothing.
	MinInboxRetentionDays = 30

	// MaxInboxRetentionDays is ten years, past which the field is not a
	// mailbox horizon but a second copy of the history.
	MaxInboxRetentionDays = 3650
)

// InboxRetention is how long an inbox row lives, with the default applied.
func (t *WorkletConfig) InboxRetention() time.Duration {
	days := DefaultInboxRetentionDays
	if t != nil && t.InboxRetentionDays > 0 {
		days = t.InboxRetentionDays
	}
	return time.Duration(days) * 24 * time.Hour
}

func (t *WorkletConfig) validate(path Path) error {
	var p problems
	if t == nil {
		return nil
	}
	if d := t.InboxRetentionDays; d != 0 && (d < MinInboxRetentionDays || d > MaxInboxRetentionDays) {
		p.add(at(path, "inbox_retention_days"), ErrOutOfRange,
			"%d is outside %d..%d — below a month an inbox stops being one, and "+
				"past ten years it is a second copy of the history it points at",
			d, MinInboxRetentionDays, MaxInboxRetentionDays)
	}
	return p.err()
}

// TrackerBackend is which work tracker a company runs.
type TrackerBackend string

// The trackers.
const (
	// TrackerWorklet is Worklet, the engine's own work tracker: the state
	// log's first domain. Every change is one record on the tracker log,
	// applied into the replicated estate every data node holds, and a node
	// without `data` reads it through the estate router. See internal/worklet.
	TrackerWorklet TrackerBackend = "worklet"

	// TrackerJira is Jira, whose state the engine reads through a seat's
	// own tools and never mirrors.
	TrackerJira TrackerBackend = "jira"

	// TrackerNone is a company with no tracker at all: nothing is filed or
	// routed as a work item, and a seat is woken by everything else — chat,
	// schedules, an A2A ask, and its connected integrations' webhooks (a
	// GitHub or GitLab issue among them).
	TrackerNone TrackerBackend = "none"
)

// TrackerBackends is every tracker, in the order the docs name them, read on
// [KnowledgeBackends]' terms.
var TrackerBackends = []TrackerBackend{TrackerWorklet, TrackerJira, TrackerNone}

// Valid reports whether b is a tracker this build serves.
func (b TrackerBackend) Valid() bool { return slices.Contains(TrackerBackends, b) }

// DefaultSkillsContainer is the container tool-skill pages live in when the
// config names none.
//
// "TS" is the key the publishing CLI writes into and the docs name, so a
// company that follows the guide works with nothing configured. It is a
// RESERVED key on either backend: excluded from knowledge search and from
// routing alike, because those pages are machinery and a seat told to read
// one would follow an instruction written for a different phase of a
// different turn.
const DefaultSkillsContainer = "TS"

// DefaultRootSpace is the container the ORGANISATION's own pages live in —
// the root Onboarding page every seat's reading chain starts at.
//
// Reserved for the same reason [DefaultSkillsContainer] is: a unit writes in
// its own space, the org root had nowhere, and a company that happened to name
// a unit's space HOME would have the two collide.
const DefaultRootSpace = "HOME"

// SkillsContainerKey is the container tool skills live in, or "" for a company
// that has turned them off — or has no knowledge base to hold them.
//
// UPPER, because every container comparison is case-insensitive and a config
// written in lower case must not silently mean a different container from the
// same word written in upper.
//
// # The empty string is an ANSWER, not an absence
//
// A company whose ordinary work container happens to be `TS` would otherwise
// have it silently dropped from every knowledge search and every routing
// decision, with no way to say so — the default reserving a real key is the
// cost of having a default at all. `skills_container: ""` is how an operator
// says "no container is reserved": every consumer already reads "" as "no
// exclusion and no sync", so the switch is this accessor and nothing else.
//
// ON THE COMPANY rather than on the block, because the answer depends on
// whether there is a knowledge base at all: `backend: none` has no container
// to name, and answering "TS" would have the engine watching a container
// nothing holds.
func (c *Company) SkillsContainerKey() string {
	if c.KnowledgeBackendFor() == KnowledgeNone {
		return ""
	}
	if c.Knowledge.SkillsContainer == nil {
		return DefaultSkillsContainer
	}
	return strings.ToUpper(strings.TrimSpace(*c.Knowledge.SkillsContainer))
}

// RootSpaceKey is the container org-level pages live in, on the same three
// values and the same terms as [Company.SkillsContainerKey].
func (c *Company) RootSpaceKey() string {
	if c.KnowledgeBackendFor() == KnowledgeNone {
		return ""
	}
	if c.Knowledge.RootSpace == nil {
		return DefaultRootSpace
	}
	return strings.ToUpper(strings.TrimSpace(*c.Knowledge.RootSpace))
}

// KnowledgeBackendFor resolves the backend, deriving an empty one.
func (c *Company) KnowledgeBackendFor() KnowledgeBackend {
	if c.Knowledge.Backend != "" {
		return c.Knowledge.Backend
	}
	if c.Integrations.Confluence != nil {
		return KnowledgeConfluence
	}
	return KnowledgeDoclet
}

// TrackerBackendFor resolves the tracker, deriving an empty one.
func (c *Company) TrackerBackendFor() TrackerBackend {
	if c.Tracker.Backend != "" {
		return c.Tracker.Backend
	}
	if c.Integrations.Jira != nil {
		return TrackerJira
	}
	return TrackerWorklet
}

// VectorsEnabled reports whether the corpus is embedded and searched by
// meaning. The engine's embedding model is the one reader, and the duty, the
// coverage gauge and a search's query vector all read that.
func (c *Company) VectorsEnabled() bool {
	if c.Knowledge.Vectors != nil {
		return *c.Knowledge.Vectors
	}
	return c.Providers.Embeddings != nil
}

// validateProviderKeys holds the rule that a seat may only name a model the
// company actually has.
//
// Without it a typo is invisible and permanent. `llm_review: claude-sonet`
// resolves through the runtime's fallback — per-phase, then the role's default
// chain, then the "default" key, then the first provider configured — so the
// seat boots, thinks, and bills, on a model the operator never chose. Nothing
// downstream can catch it: the fallback exists precisely so a role that names
// NOTHING still runs, and from inside resolution a name that misses and a name
// that was never written are the same absence.
//
// Applied to EVERY seat in the document, at any depth, and to every field each
// writes: the fields are [Company.ProviderReferences]'s, so the rule, a
// removal and the account of who uses an entry read one list. Each typo is
// reported at the field the operator typed it in. Validating the RESOLVED
// chain instead would hide half of them: the flat field wins over the
// mapping, so a typo inside `llm.review` under a role that also sets
// `llm_review` never appears in the resolved value at all, and it is still a
// typo, still in the file, and still what the operator will edit next.
//
// NOT THE ASSISTANT'S FIELDS: its chain is held by [Company.validateAssistant]
// at admission and again wherever it is handed out ([Company.AssistantChain]),
// because nothing about running a seat depends on it, and this is a runnable
// rule.
//
// Skipped entirely when providers.llm is empty. A company with no models is a
// documented authoring state (an org chart written before the credentials
// exist, and what a company created from the dashboard is until one is added),
// and it runs: every node applies it and places its seats, and each seat holds
// its work until a revision adds a provider. Rejecting every role's key
// against an empty map would turn that supported flow into a wall of errors
// about models the author has not added yet.
func (c *Company) validateProviderKeys() error {
	var p problems
	if len(c.Providers.LLM) == 0 {
		return nil
	}
	known := slices.Sorted(maps.Keys(c.Providers.LLM))
	for _, ref := range c.providerReferences() {
		if ref.Job == JobAssistant || ref.Job == JobDeepWork {
			continue
		}
		for _, key := range ref.Chain {
			if _, ok := c.Providers.LLM[key]; !ok {
				p.add(ref.Segments, ErrUnknownValue,
					"%q is not a configured provider: providers.llm has %s. "+
						"A key that misses is not an error at run time: the seat "+
						"falls back to another model and bills against it, so this "+
						"is the only place the typo can be seen",
					key, strings.Join(known, ", "))
			}
		}
	}
	return p.err()
}

// DeclaresIntegration reports whether this company still uses the named
// external surface at all.
//
// # Why it lives on the whole document rather than on Integrations
//
// For seven of the eight it IS a question about the `integrations:` block, and
// putting it there would be tidier. For Slack it is not: every agent carries
// its own app under `role.integrations.slack`, and the company-level `slack:`
// block holds working-indicator settings that a company using Slack heavily
// may never write. Asked of the block alone, a company with seven working
// Slack apps answers "no".
//
// That answer is not academic. Its one caller deletes a surface's fleet status
// row on false, and Slack's row is the only place the engine records the public
// base its Request URLs were set against — the single warning an operator ever
// gets that a moved address has stranded every agent's Slack app, since Slack
// serves no way to read that URL back. So the wrong answer here silently
// removes the warning from exactly the surface that cannot self-heal.
//
// # Keyed on the surface's own string, not on its type
//
// The names are integration.Kind's values, and this takes a plain string so the
// config tier does not import the package that defines them. An exhaustiveness
// test over integration.Kinds lives where both are already in scope, so a
// surface added there and forgotten here fails a suite rather than answering
// false for ever.
//
// # What it must NOT be used for
//
// A surface that has a reconcile pass. This asks about the DOCUMENT; a pass
// asks a richer question about the integration, and every one of them reads
// `enabled: false` as not configured where this sees a block and says yes. Two
// authorities that disagree on a paused integration is a status row flapping
// between them, which is why the reconcile loop asks this only about the
// surfaces that have no pass.
func (c *Company) DeclaresIntegration(surface string) bool {
	if c == nil {
		return false
	}
	in := c.Integrations
	switch surface {
	case "jira":
		return in.Jira != nil
	case "confluence":
		return in.Confluence != nil
	case "mattermost":
		return in.Mattermost != nil
	case "github":
		return in.GitHub != nil
	case "gitlab":
		return in.GitLab != nil
	case "datadog":
		return in.Datadog != nil
	case "atlassian":
		return in.Atlassian != nil
	case "slack":
		if in.Slack != nil {
			return true
		}
		for role := range c.EachRole() {
			if role.Integrations.Slack != nil {
				return true
			}
		}
		return false
	default:
		// A SURFACE THIS BUILD DOES NOT KNOW, which on a rolling upgrade is
		// a peer's. True rather than false, because the only caller deletes
		// on false and an older node must not erase a newer one's row — the
		// same rule integration.Kind.Valid states for the loop itself.
		return true
	}
}

// EachRole yields EVERY seat in the company, with the path an operator typed:
// the top-level `roles:` and every seat inside `units:`, to any depth.
//
// It exists because the walk was written inline once and covered only the
// top-level list, so a cross-field rule silently exempted every seat that
// belonged to a unit — which, in a company with an org chart, is most of them.
// A rule that holds for a seat in `roles:` and not for the identical seat one
// level down is not a rule, and the seats it missed are exactly the ones whose
// mistakes have no run-time symptom to find them by.
//
// AN ITERATOR rather than a callback, so a caller looking for ONE seat can
// stop at it: the engine's per-seat sandbox lookup ran the whole org chart to
// the end on every launch because a callback has no way to say "found it".
//
// EXPORTED because the ENGINE needs the same walk: a seat's sandbox block is
// looked up by handle at every executor launch, and a lookup that stopped at
// the top level answered nil for every seat in a unit — so each of them ran
// its box without the setup, env and tool servers its block declared. One
// walker, so the two can never disagree about which seats exist.
func (c *Company) EachRole() iter.Seq2[*Role, Path] {
	return func(yield func(*Role, Path) bool) {
		for i := range c.Roles {
			if !yield(&c.Roles[i], idx(field("roles"), i)) {
				return
			}
		}
		var walk func(units []Unit, path Path) bool
		walk = func(units []Unit, path Path) bool {
			for i := range units {
				unit := &units[i]
				here := idx(path, i)
				for j := range unit.Roles {
					if !yield(&unit.Roles[j], idx(at(here, "roles"), j)) {
						return false
					}
				}
				if !walk(unit.Children, at(here, "children")) {
					return false
				}
			}
			return true
		}
		walk(c.Units, field("units"))
	}
}

// SandboxPlacements is every cell an executor run could ACTUALLY land in,
// each mapped to the path of the first thing that named it.
//
// ONE COMPUTATION, TWO CALLERS, and they have to agree or the company is
// refused for a rule the engine then breaks anyway. Validation requires a
// container image only where a container is reached; the engine builds a
// backend only for what is reached. Built eagerly from the catalogue instead,
// the engine constructed a container backend for a company whose runs all go
// direct — and failed the apply demanding an image the validator had just
// refused as a field nothing would read.
//
// The DEFAULT IS ALWAYS REACHED when the catalogue is enabled: it is where a
// run on an entry that names no cell goes, including an entry a founder
// points a seat at later, editing Tier B live, so a company must be able to
// run it before one does.
func (c *Company) SandboxPlacements() map[Placement]string {
	reached := make(map[Placement]string)
	catalogue := c.Providers.Sandbox
	if !catalogue.Enabled() {
		return reached
	}
	if run := catalogue.RunIn(); run != "" {
		where := "providers.sandbox.default_run_in"
		if catalogue.DefaultRunIn == "" {
			// Resolved from the catalogue's own shape rather than
			// written, so the message points at the block, not at a
			// field the operator will not find.
			where = "providers.sandbox"
		}
		reached[run] = where
	}
	// EVERY BOX AN EXECUTOR CAN LAUNCH IN, placed as the engine places it
	// ([Company.SeatBoxes]): one per cell among the runner-capable members
	// of every agent seat's executor chain, not the head's alone. A launch
	// falls back to the chain's next member when the first is sitting out
	// on its node, and that member is placed by its own `run_in` — so a
	// cell only a fallback member names, counted for the head alone, would
	// have no backend built, and the very launch the fallback exists for
	// would fail with the manager's "no backend for e2b".
	//
	// A box whose entry names no cell is in the default's, which is reached
	// already; so every other cell is one an entry's `run_in` named, and the
	// path kept for it is the first such entry.
	for box := range c.SeatBoxes() {
		if box.Cell == "" {
			continue
		}
		if _, seen := reached[box.Cell]; seen {
			continue
		}
		reached[box.Cell] = at(entryPath(box.Executor), "run_in").String()
	}
	// DEEP WORK'S CELL, where the catalogue serves it: the engine host is
	// the engine's own confined cell (internal/engine), never the
	// catalogue's unconfined direct one.
	if cell, ok := c.DeepWorkPlacement(); ok && cell != PlacementDirect {
		if _, seen := reached[cell]; !seen {
			reached[cell] = "assistant.deep_work.llm"
		}
	}
	return reached
}

// executorSeat is one agent seat with an executor to run, and the chain that
// executor resolves to.
type executorSeat struct {
	role *Role
	path Path
	// chain is every member of the seat's executor chain, configured keys
	// only, in the order a launch tries them.
	chain []string
	// own reports a chain read off the seat's own `llm`, where false is the
	// company's fallback entry a seat naming no configured key falls to.
	own bool
	// named reports that the seat's `llm` names keys at all — configured or
	// not. A seat that named only keys the company lacks falls to the
	// fallback entry too (own is false), but its `llm` is a typo
	// [Company.validateProviderKeys] refuses where it is written, not a
	// seat that named nothing.
	named bool
}

// executorSeats yields every agent seat with the executor chain it resolves
// to, in [Company.EachRole]'s order — resolved by [phase.Resolve] over the
// seat as the engine builds it, from the company's providers in declaration
// order, which is the very call the engine's phase registry resolves a
// launch's chain through. ONE RESOLUTION, so the seats validation refuses
// and the chain a launch walks cannot disagree; a second copy of the rule
// here was once held to the registry by a test that never called it.
//
// A HUMAN SEAT IS SKIPPED. It is addressable and never spawned, so it runs no
// phase — and resolved, its empty `llm` falls to the company's fallback entry,
// which would then count as reached by a seat that never runs: a company was
// refused for a box its founder's seat would never open. NOTHING is yielded
// for a company with no providers.llm, which is valid — an org chart written
// before its credentials — and runs no executor at all.
func (c *Company) executorSeats() iter.Seq[executorSeat] {
	return func(yield func(executorSeat) bool) {
		order := c.Providers.ProviderOrder()
		if len(order) == 0 {
			return
		}
		for role, path := range c.EachRole() {
			if role.Kind == org.KindHuman {
				continue
			}
			own := slices.ContainsFunc(role.LLM.Default, func(key string) bool {
				_, ok := c.Providers.LLM[key]
				return ok
			})
			chain := phase.Resolve(role.Seat(), phase.Execute, order)
			if !yield(executorSeat{role: role, path: path, chain: chain, own: own,
				named: len(role.LLM.Default) > 0}) {
				return
			}
		}
	}
}

// ExecutorSeatsOn is the handles of the agent seats whose executor chain holds
// key, in [Company.EachRole]'s order: the seats `crewlet llm doctor` reports
// an entry as launching for.
//
// OFF [Company.executorSeats], the one resolution validation and a launch
// share, so the doctor cannot name a seat the engine never launches on the
// entry nor miss one it does. A chain holding key as a member that cannot run
// an executor still names the seat: the doctor says why that member is
// skipped at launch and what it serves instead.
func (c *Company) ExecutorSeatsOn(key string) []string {
	var out []string
	for seat := range c.executorSeats() {
		if slices.Contains(seat.chain, key) {
			out = append(out, seat.role.Seat().Handle())
		}
	}
	return out
}

// runners is the members of chain that can run an executor, in chain order:
// the ones a launch may pick ([LLMProvider.runsExecutor]).
func (c *Company) runners(chain []string) []string {
	var out []string
	for _, key := range chain {
		entry := c.Providers.LLM[key]
		if entry.runsExecutor() {
			out = append(out, key)
		}
	}
	return out
}

// executorKeys are the providers.llm entries an executor launch can land on,
// in key order: every runner-capable member of every agent seat's executor
// chain.
//
// ONLY ENTRIES A SEAT REACHES: an entry no executor chain holds launches
// nothing, so the cell it names is not reached and the backend behind it is
// not built. It is validated the day a seat points at it, like a seat added
// later. Sorted, so a message naming the first entry, or a backend built in
// this order, is the same on every run of the same config.
func (c *Company) executorKeys() []string {
	seen := map[string]struct{}{}
	for seat := range c.executorSeats() {
		for _, key := range c.runners(seat.chain) {
			seen[key] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// describeChain renders chain as an operator reads it: each entry's path and
// the CLI it drives.
func (c *Company) describeChain(chain []string) string {
	parts := make([]string, len(chain))
	for i, key := range chain {
		entry := c.Providers.LLM[key]
		parts[i] = fmt.Sprintf("providers.llm.%s (%s)", key, entry.AgentName())
	}
	return strings.Join(parts, ", ")
}

// validateExecutors refuses an agent seat whose executor chain has no member
// that can run it.
//
// Every agent seat's executor is its coding CLI's own agentic run, and only a
// CLI with an in-box runner can be one ([CodingAgents]). A launch walks the
// seat's chain for its first such member that is not sitting out and skips
// the rest, so a chain with NONE never launches: the seat is placed, takes
// its work, and fails every turn at launch. RUNNABLE for that reason — no
// turn of that seat runs — where a chain that merely carries a non-runner
// beside a runner is valid and only advised about
// ([Company.executorWarnings]).
//
// Reported where the remedy is written. A chain read off the seat's own `llm`
// is refused there. A seat naming no configured key runs on the company's
// fallback entry (the one called "default", else the first declared), which
// is ONE entry for every such seat, so it is refused once, at that entry's
// `agent`, naming the first seat that falls to it and how many more. Both
// remedies are named because both are real: the entry can move to a CLI
// that runs, or the seat's `llm` can point at one that does — and the other
// CLI stays the seat's reviewer, workers or side passes through llm_review,
// llm_subagent, llm_auxiliary or llm_judge, which any CLI can serve.
func (c *Company) validateExecutors() error {
	var p problems
	type fallen struct {
		first string
		more  int
	}
	fallback := map[string]*fallen{}
	var fallbackOrder []string
	for seat := range c.executorSeats() {
		if len(c.runners(seat.chain)) > 0 {
			continue
		}
		if seat.own {
			p.add(at(seat.path, "llm"), ErrConflict,
				"this seat's executor chain is %s, and no member of it can run the "+
					"executor: a seat's executor is its coding CLI's own run, which "+
					"only %s have. Point `llm` at an entry with agent: claude-code or "+
					"opencode (and keep these entries for the reviewer, workers and "+
					"side passes with llm_review, llm_subagent, llm_auxiliary or "+
					"llm_judge), or move one of them to claude-code or opencode",
				c.describeChain(seat.chain), names(CodingAgents))
			continue
		}
		if seat.named {
			// ITS OWN `llm` IS THE FAULT, refused where it is written: every
			// key it names is one the company lacks
			// ([Company.validateProviderKeys]). Counted here too, it was
			// reported a second time as a seat that "names no `llm`" —
			// contradicting the refusal beside it, and sending the author to
			// an entry the seat never asked for. Fixing the typo puts the
			// seat on its own chain, which this rule then judges.
			continue
		}
		key := seat.chain[0]
		handle := seat.role.Seat().Handle()
		if f := fallback[key]; f != nil {
			f.more++
			continue
		}
		fallback[key] = &fallen{first: handle}
		fallbackOrder = append(fallbackOrder, key)
	}
	for _, key := range fallbackOrder {
		f, entry := fallback[key], c.Providers.LLM[key]
		which := `the one called "default"`
		if key != "default" {
			which = `the first one declared, as no entry is called "default"`
		}
		seats := fmt.Sprintf("seat %q names no `llm` and falls to it", f.first)
		switch {
		case f.more == 1:
			seats += ", and 1 more seat does too"
		case f.more > 1:
			seats += fmt.Sprintf(", and %d more seats do too", f.more)
		}
		p.add(at(entryPath(key), "agent"), ErrConflict,
			"a seat that names no `llm` runs its executor on this entry, %s, and it "+
				"drives %s, which cannot run an executor: a seat's executor is its "+
				"coding CLI's own run, which only %s have. %s. Move this entry to "+
				"claude-code or opencode, or point those seats' `llm` at an entry "+
				"that drives one",
			which, entry.AgentName(), names(CodingAgents), seats)
	}
	return p.err()
}

// executorWarnings is one advisory per agent seat whose executor chain holds
// a member that can never run it beside one that can.
//
// A WARNING AND NOT A RULE, because the chain is valid: a launch skips the
// member and runs on the rest, and the member still serves every text phase
// that falls back to `llm` — the reviewer, workers, side passes and
// onboarding — which any CLI can. What the author should still know is that
// the executor never falls back to it, so a chain written as "Claude, then
// Codex when Claude's plan is spent" falls back to nothing. AT THE SEAT'S
// `llm`, which is where either reading is written.
func (c *Company) executorWarnings() []Warning {
	var out []Warning
	for seat := range c.executorSeats() {
		runners := c.runners(seat.chain)
		if len(runners) == 0 || len(runners) == len(seat.chain) {
			continue
		}
		var skipped []string
		for _, key := range seat.chain {
			if !slices.Contains(runners, key) {
				skipped = append(skipped, key)
			}
		}
		handle := seat.role.Seat().Handle()
		w := advisory(at(seat.path, "llm"), fmt.Sprintf("%s's executor never runs on "+
			"%s: a seat's executor is its coding CLI's own run, which only %s have, "+
			"so a launch skips it and runs on %s alone. That is fine if it is there "+
			"for the reviewer, workers and side passes, which fall back to `llm`; "+
			"otherwise take it out",
			handle, c.describeChain(skipped), names(CodingAgents), c.describeChain(runners)))
		w.Seat = handle
		out = append(out, w)
	}
	return out
}

// validateSandboxPlacement holds the rules that need BOTH the catalogue and
// the seats: whether the catalogue is there, which cells a company actually
// reaches, and whether the blocks configuring them are.
//
// Neither half can hold it alone. providers.sandbox is validated without the
// roles, so it cannot know that `run_in: container` is reached and an image is
// needed; an entry is validated without the catalogue, so it cannot know that
// the cell it names is not configured. Both failures are silent at run time —
// the seat takes its work and the launch dies inside a turn, every turn.
func (c *Company) validateSandboxPlacement() error {
	var p problems
	catalogue := c.Providers.Sandbox

	if catalogue == nil {
		// EVERY AGENT SEAT'S EXECUTOR RUNS IN A BOX, so a company with
		// one to run and a model to run it on cannot run without the
		// catalogue: every such seat would take its work and fail every
		// turn at launch. ONE refusal, at the block that is missing,
		// naming the first such seat — a problem per seat would bury the
		// one edit to make under the seats that need it.
		//
		// Not asked of a company with no providers.llm: that is an org
		// chart written before its credentials, which runs and holds
		// every seat's work until a revision adds a model, and the
		// catalogue with it. Asked only of an ABSENT block: a present one
		// that configures nothing is refused at this same path by its own
		// rule.
		first, more := "", 0
		for seat := range c.executorSeats() {
			if first == "" {
				first = seat.role.Seat().Handle()
				continue
			}
			more++
		}
		if first != "" {
			seats := fmt.Sprintf("seat %q has one", first)
			switch {
			case more == 1:
				seats += ", and 1 more seat does too"
			case more > 1:
				seats += fmt.Sprintf(", and %d more seats do too", more)
			}
			p.add(field("providers.sandbox"), ErrMissing,
				"every agent seat's executor runs as its coding CLI's own run, in a "+
					"box this block configures, and this company configures none: %s. "+
					"Add providers.sandbox — `local: {}` with `default_run_in: direct` "+
					"runs it on this host as the engine's user, `e2b:` in a remote box",
				seats)
		}
		return p.err()
	}
	if !catalogue.Enabled() {
		// Refused by the block's own rule, at this same path.
		return p.err()
	}

	// THE SAME QUESTIONS FOR EVERY ENTRY A LAUNCH CAN LAND ON — every
	// runner-capable member of every executor chain, the head and each
	// fallback alike: is the cell it names configured, and does an entry
	// that names none have a default to fall to. Each was unasked once, and
	// each failed at the seat's first turn — every turn — with a launch
	// error naming a backend no field in the file appeared to lack.
	keys := c.executorKeys()
	for _, key := range keys {
		entry := c.Providers.LLM[key]
		where := at(entryPath(key), "run_in")
		run := entry.RunIn
		if run == "" {
			run = catalogue.RunIn()
		}
		switch {
		case run == "":
			p.add(where, ErrMissing,
				"a seat's executor launches on this entry, in a box, and the "+
					"catalogue offers more than one place to run one (%s), so this "+
					"entry has to name one: or providers.sandbox.default_run_in has to",
				names(catalogue.available()))
		case !slices.Contains(Placements, run):
			// Spelling is the entry's own rule; it already reported.
		case !catalogue.Configured(run):
			p.add(where, ErrConflict,
				"%q needs the %s backend configured under providers.sandbox "+
					"(this catalogue has %s)",
				run, run.backend(), names(catalogue.available()))
		}
	}

	reached := c.SandboxPlacements()
	if len(reached) == 0 && len(keys) == 0 {
		// A CATALOGUE NOTHING CAN RUN. The engine builds a backend only
		// for a cell something reaches, so zero reached cells means no
		// manager and no runtime: a block that reads as configuration and
		// configures nothing, which is the reason an empty block is
		// refused too.
		//
		// It is only reachable on an AMBIGUOUS catalogue: any catalogue
		// that resolves a default reaches that default, precisely so a
		// seat added later has somewhere to go. And only with no entry a
		// launch can land on, because each of those that names no cell is
		// refused above, with this same default as one of its remedies —
		// a second refusal here would say the one mistake twice.
		p.add(at(field("providers.sandbox"), "default_run_in"), ErrMissing,
			"this catalogue configures more than one place to run an executor (%s) "+
				"and nothing names one, so no backend is built and a seat added later "+
				"has nowhere to run. Name the default, or remove the block while no "+
				"agent seat runs on a model",
			names(catalogue.available()))
	}
	if local := catalogue.localBlock(); local != nil {
		// THE IMAGE IS ONLY NEEDED WHERE A CONTAINER IS ACTUALLY RUN, and
		// only the walk above knows that. Required unconditionally it would
		// refuse a perfectly good direct-only company; unchecked, a seat's
		// first launch fails at container create, minutes into a turn.
		if named, wanted := reached[PlacementContainer]; wanted && strings.TrimSpace(local.Image) == "" {
			p.add(field("providers.sandbox.local.image"), ErrMissing,
				"%s runs in a container, so the local backend needs an image "+
					"with the coding-agent CLI installed", named)
		}
		if _, wanted := reached[PlacementContainer]; !wanted {
			for _, unread := range local.containerOnly() {
				if strings.TrimSpace(unread.value) == "" {
					continue
				}
				p.add(at(field("providers.sandbox.local"), unread.field), ErrConflict,
					"only a container box reads this, and no executor runs in one. "+
						"Give an entry a seat runs on `run_in: container` (or make it "+
						"providers.sandbox.default_run_in), or remove the field")
			}
		}
	}
	return p.err()
}
