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
	"github.com/crewlet/crewlet/internal/words"
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

	// Timezone is the company's ONE clock, as an IANA name; empty is UTC. Read
	// it through [Company.Location].
	//
	// Every calendar edge is cut on it: where "today", "this week" and "this
	// month" begin (due filters, bands, overdue marks, a person's day), what a
	// relative or all-day date resolves to, and the wall clock of a schedule
	// naming no zone. Top-level because separate clocks disagreed about where a
	// company's day began (ADR-0018).
	//
	// For AUTHORED INSTANTS AND CALENDAR BOUNDARIES only: a duration measured
	// on a wall clock changes length twice a year.
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

	// Chat is which chat surface this company talks on.
	Chat Chat `yaml:"chat,omitempty" json:"chat,omitempty"`

	// Code is Gitlet, the engine's own git host: whether this company hosts its
	// repositories in the engine, and the limits a push is held to. See
	// [Code].
	Code Code `yaml:"code,omitempty" json:"code,omitzero"`

	// Artifacts is the company's artifacts policy. See [CompanyArtifacts].
	Artifacts CompanyArtifacts `yaml:"artifacts,omitempty" json:"artifacts,omitzero"`

	// Inbox is the company's policy over a person's inbox — the notices
	// every domain that keeps one writes to the people its changes
	// concern. See [Inbox].
	Inbox Inbox `yaml:"inbox,omitempty" json:"inbox,omitzero"`

	// AccessHistory is the company's policy over the record of its seats'
	// access and SSH use. See [AccessHistory].
	AccessHistory AccessHistory `yaml:"access_history,omitempty" json:"access_history,omitzero"`

	// Atlas is the company's policy over its atlas. See [Atlas].
	Atlas Atlas `yaml:"atlas,omitempty" json:"atlas,omitzero"`

	// Diagnostics is what this engine may say about its own defects outside
	// the company. See [Diagnostics].
	Diagnostics Diagnostics `yaml:"diagnostics,omitempty" json:"diagnostics,omitzero"`

	// Assistant is whether the Dashboard Assistant runs for this company,
	// on which models, and the bounds of one turn. See [Assistant].
	Assistant Assistant `yaml:"assistant,omitempty" json:"assistant,omitzero"`

	Advisor Advisor `yaml:"advisor,omitempty" json:"advisor,omitzero" desc:"The Advisor: the model it runs on. Its rules run whatever this says."`

	// DelegatedDecisions is which requests agent seats decide in a person's
	// place, within the engine's rules. See [DelegatedDecisions].
	DelegatedDecisions DelegatedDecisions `yaml:"delegated_decisions,omitempty" json:"delegated_decisions,omitzero"`

	// SignIn is how the company's people sign in. See [SignIn].
	SignIn SignIn `yaml:"sign_in,omitempty" json:"sign_in,omitzero"`

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

	// MCPRegistry is the community registry the dashboard searches beside
	// the curated MCP catalogue; absent, there is none. See [MCPRegistry].
	MCPRegistry MCPRegistry `yaml:"mcp_registry,omitempty" json:"mcp_registry,omitzero" desc:"The community MCP registry the dashboard searches beside the curated catalogue. Absent: none."`

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

	// NotificationCoalesceMaxBatch caps the events collected into one DRAIN,
	// before partitioning by conversation. It bounds a digest too (a digest
	// never exceeds a drain) and keeps the drain within the broker's ack budget
	// beside a whole turn; a larger backlog arrives as successive capped
	// drains.
	//
	// Per drain, not per digest: a drain spanning conversations shares the cap,
	// so 24 events over 4 conversations at 20 is two drains and 8 turns, not 4.
	//
	// Capped at 100: the digest IS the trigger, re-sent every round of the tool
	// loop, so past that successive batches beat one megaprompt.
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

// ClockMoves warns of a write that moves the company's clock (ADR-0018) from
// prior's zone to next's: every budget window, day and week then begins at
// the new zone's midnight, and every schedule naming no zone of its own fires
// at the new zone's hour.
//
// A TRANSITION, not a property of either document, so it is raised by the
// write that makes it, beside the document's own warnings, and by nothing
// that reads a revision afterwards. Worded for the person making the change,
// since it names no key to fix: nothing is wrong, only far-reaching.
func ClockMoves(next, prior *Company) []Warning {
	if next == nil || prior == nil {
		return nil
	}
	from, to := prior.Location().String(), next.Location().String()
	if from == to {
		return nil
	}
	w := clockMovesAdvice.raise(field("timezone"), fmt.Sprintf("This moves the company's "+
		"clock from %s to %s. Every budget's day, week and month will start at midnight "+
		"in %s, cutting the window running now, and every schedule without a time zone "+
		"of its own will run on %s time.", from, to, to, to))
	w.Item = from + " to " + to
	return []Warning{w}
}

// clockMovesAdvice is [ClockMoves]' warning, its Item the zone it moves from
// and the one it moves to.
var clockMovesAdvice = says("The company's time zone changes",
	"Budgets will start each day, week and month at the new zone's midnight, cutting the "+
		"current one short, and schedules without their own zone move too.")

// skillVariableKey is the substitution-identifier rule.
//
// The operator key space has to be provably identical to the ${name} render
// grammar the skill layer uses, or a key like base-url would be accepted
// here and then never substituted anywhere — a silent no-op at render time
// rather than an error at load.
var skillVariableKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate reports every Tier B rule this config breaks, joined: the runnable
// and the admission rules both.
//
// It is the check for a document somebody SUBMITS (a write, `crewlet config
// import`, `crewlet validate`) and validates the ORG too, as duplicate handles
// and human-seat violations surface only when the hierarchy is built. A
// revision being APPLIED gets [Company.ValidateRunnable] only.
//
// The CHARTER's rules judge a write against the revision it replaces, so an old
// fault never refuses an unrelated write; every writer also asks
// [Company.ValidateCharterChange].
func (c *Company) Validate() error {
	o, index := c.organization()
	return index.locate(errors.Join(c.validateRunnable(o), c.validateAdmission(o)))
}

// ValidateRunnable reports only the RUNNABLE rules: every rule except the
// admission ones.
//
// It is the check for a revision about to be APPLIED (reconcile, boot, reload,
// revert) and for building an epoch. During a rolling upgrade this build
// applies revisions a newer peer admitted under admission rules a build is free
// to change; nothing about RUNNING depends on them, so applying under runnable
// rules only keeps this build from refusing them and splitting the fleet's
// epoch. [Company.ValidateAdmission] reports them as warnings.
func (c *Company) ValidateRunnable() error {
	o, index := c.organization()
	return index.locate(c.validateRunnable(o))
}

// ValidateAdmission reports only the ADMISSION rules: those a submitted
// document is refused for and a stored revision is merely warned about. They
// are the validators [Company.validateAdmission] joins, each documented on its
// own. [org.Organization.ValidateAdmission] says why no NAME is held unique,
// [Code.validateAdmission] holds Gitlet's policy bounds ([CodeMaxRepos],
// [CodeMaxDeletedRetentionDays], [CodeMaxRemoteCredentials],
// [CodeMaxRemoteCredentialHosts]), [Company.validateFileRules] the
// WHOLE-DOCUMENT rules, [Company.validateAssistant] the `assistant:` block,
// whose chain is held again wherever it is handed out, and
// [Company.validateBrowserSeats] where a seat's browser may run.
func (c *Company) ValidateAdmission() error {
	o, index := c.organization()
	return index.locate(c.validateAdmission(o))
}

// validateAdmission is [Company.ValidateAdmission] over an organization the
// caller already built.
func (c *Company) validateAdmission(o *org.Organization) error {
	return errors.Join(o.ValidateAdmission(), c.validateUnitIDs(),
		c.validateSetupStepNames(), c.validateHumanSeatCode(), c.validateHumanSeatGrants(),
		c.validateAvatars(),
		c.validateLLMKeys(), c.validateLLMFleets(), c.validateLLMCredentials(), c.validateLLMModels(), c.validateSetupFileHomes(),
		c.validateBrowserSeats(),
		c.validateRunOptionalKeys(), c.validateReservedSandboxEnv(), c.validateUnitRooms(),
		c.validateFileRules(), c.validateMCPNames(), c.validateAssistant(), c.validateAdvisor(), c.validateSeatAccess(),
		c.validateMCPRegistry(), c.validateMCPAccess(o), c.validateMCPRequiredKeys(o), c.validateBoxToolLevels(),
		c.validateMCPLimits(o), c.Code.validateAdmission(field("code")), c.validateSSHApprovers(),
		c.validateDelegatedDecisions(o), c.validateGrowth())
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

// validateHumanSeatCode refuses on a human seat what a seat holds on a code
// host: its code access tier (`code`) and its own GitHub App
// (`integrations.github`).
//
// A person acts on every code host as their own identity, so both are agent
// settings: `code.access` caps nothing for a person and every engine path that
// makes an app skips a human seat. Either block reads as working and does
// nothing.
//
// An ADMISSION rule, not RUNNABLE beside [org.Role.Validate]: nothing running
// reads either ([org.Role.CodeAccess], [org.Role.GitHub]) off a human seat, so
// a stored revision carrying one runs as before and a newer peer may relax one.
func (c *Company) validateHumanSeatCode() error {
	var p problems
	for role, path := range c.EachRole() {
		if role.Kind != org.KindHuman {
			continue
		}
		if !role.Code.IsZero() {
			p.add(at(path, "code"), ErrConflict,
				"a human seat has no code access tier: a person acts on every "+
					"code host as their own identity (their bound token on "+
					"Gitlet, contact.github_login on GitHub), so a "+
					"tier here would cap nothing. Remove this block, or make "+
					"the seat an agent seat")
		}
		if role.Integrations.GitHub != nil {
			p.add(at(path, "integrations.github"), ErrConflict,
				"a human seat has no GitHub App of its own: an app is the identity "+
					"an agent acts as, and a person acts as their own login, "+
					"contact.github_login. Remove this block, or make the seat an "+
					"agent seat")
		}
	}
	return p.err()
}

// validateHumanSeatGrants refuses on a human seat what only an agent's role
// carries: the health grant (`health_tools`).
//
// The grant decides an AGENT on the verbs that name it (internal/authz); a
// person is decided by their own permissions and is never woken by an engine
// wake, so the field reads as a working setting and does nothing.
//
// AN ADMISSION RULE, for [Company.validateHumanSeatCode]'s reason: the chart
// answers a grant for an agent only, so a stored revision carrying one runs as
// before.
func (c *Company) validateHumanSeatGrants() error {
	var p problems
	for role, path := range c.EachRole() {
		if role.Kind == org.KindHuman && role.HealthTools {
			p.add(at(path, "health_tools"), ErrConflict,
				"a human seat carries no health grant: a person reads the "+
					"deployment's health as an administrator, through their own "+
					"permissions, and is never woken for it. Remove the field, or "+
					"make the seat an agent seat")
		}
	}
	return p.err()
}

// validateSetupStepNames refuses two sandbox setup steps of one name in the
// same list: `providers.sandbox.setup`, or one seat's `sandbox.setup`.
//
// A step's name is its IDENTITY. Its env and files are credentials a config
// read masks, and sending the read back restores each step's masks from the
// stored step of the same name. Duplicates give the restore nothing to tell
// them apart by, and it refuses to guess: every write carrying that list would
// be refused. A name is also what a setup failure points at.
//
// An ADMISSION rule: duplicates still apply every step in order. Compared
// exactly as the restore matches; a blank name is skipped, its own rule refuses
// it.
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

	// An UNRESOLVED REDACTION MASK, first: the one fault here that comes from
	// this process, not the author. A credential still holding the marker means
	// a config read was edited and sent back and the mask could not be matched
	// to what it hid: a NEW or RENAMED member has no prior value, and one the
	// stored revision does not identify exactly (a duplicate, or empty) cannot
	// be matched without guessing. Storing it would hand a provider the literal
	// "__redacted__" as an API key, failing hours later with an error naming
	// nothing of its origin. The message names every cause, as the restore
	// records none.
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
		said := fmt.Sprintf("%q is not a time zone. Pick one by its name, like "+
			"Europe/Berlin, America/New_York or UTC.", c.Timezone)
		if errors.Is(err, period.ErrHostClock) {
			said = fmt.Sprintf("%q is whatever zone each server is set to, so two servers "+
				"would start the company's day at different hours. Pick a named zone, "+
				"like Europe/Berlin or UTC.", c.Timezone)
		}
		p.addSaying(field("timezone"), ErrUnknownValue, said, "%v", err)
	}
	p.wrap(c.Artifacts.validate(field("artifacts")))
	p.wrap(c.Diagnostics.Insights.validate(field("diagnostics.insights")))

	// A SEAT MAY NOT BE CALLED WHAT "NOBODY" IS CALLED. Datadog's fallback
	// takes a seat handle or DatadogIgnore, so a seat of that name would be
	// silenced by its own name; only here is it caught, as the parser sees the
	// two as one string.
	//
	// AND THE FALLBACK MUST NAME SOMEBODY WHO CAN BE WOKEN. `route_to` is the
	// only routing floor, and it fails silently on a handle no seat has (one
	// `notification_undeliverable` warning per untagged alert, for ever) and on
	// a HUMAN seat (dropped as a self-action). Only here can either be caught:
	// the parser does not consult the roster (a bad monitor TAG must stay
	// visible as the typo it is), and the setup form is one of three write
	// paths.
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
					"(or to a human seat, which is dropped as a self-action) "+
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
					"which is where this deployment's address belongs: one "+
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
	p.within(field("notification_rate_limit"), float64(c.NotificationRateLimit), "")
	if w := c.NotificationCoalesceWindowSeconds; w < 0 || w > coalesceWindowMax {
		p.add(field("notification_coalesce_window_seconds"), ErrOutOfRange,
			"must be 0..%v seconds, got %v: the window is spent out of the "+
				"broker's ack-timeout budget, which also has to fit a whole turn",
			coalesceWindowMax, w)
	}
	p.within(field("notification_coalesce_max_batch"), float64(c.NotificationCoalesceMaxBatch), "")

	p.wrap(c.Providers.validate(field("providers")))
	p.wrap(c.TurnEngine.validate(field("turn_engine")))
	p.wrap(c.Learning.validate(field("learning")))
	p.wrap(c.Scheduling.validate(field("scheduling")))
	p.wrap(c.Integrations.validate(field("integrations")))
	p.wrap(c.validateKnowledgeBackend())
	p.wrap(c.Code.validate(field("code"), o))
	p.wrap(c.validateChat())
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
		if name == claudeContainerServer {
			// Claude Code's own name for its container's tools, whose calls'
			// results it has cleared from a session it continues as its
			// built-ins' own. See [claudeContainerServer].
			p.add(at(path, "name"), ErrConflict,
				"%q is reserved: Claude Code takes a server of that name for its "+
					"own container's tools, and has the results of their calls "+
					"cleared from a long session as its built-in tools' own, so "+
					"a run continuing the session would lose them. Rename the server",
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
	p.wrap(c.Inbox.validate(field("inbox")))
	p.wrap(c.SignIn.validate(field("sign_in")))
	p.wrap(c.AccessHistory.validate(field("access_history")))
	p.wrap(c.Atlas.validate(field("atlas")))
	p.wrap(c.Chat.Chatlet.validate(field("chat.chatlet")))

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
	if c.ConfluenceHeldBy() != "" && c.Integrations.Confluence != nil {
		p.add(field("knowledge.backend"), ErrConflict,
			"Doclet and integrations.confluence can't both run, because "+
				"pages would be split between them. Remove one")
	}
	if c.Knowledge.Backend == KnowledgeConfluence && c.Integrations.Confluence == nil {
		p.add(field("knowledge.backend"), ErrConflict,
			"knowledge.backend: confluence needs integrations.confluence")
	}
	if c.JiraHeldBy() != "" && c.Integrations.Jira != nil {
		p.add(field("tracker.backend"), ErrConflict,
			"Worklet and integrations.jira can't both run, because work "+
				"would be split between them. Remove one")
	}
	if c.Tracker.Backend == TrackerJira && c.Integrations.Jira == nil {
		p.add(field("tracker.backend"), ErrConflict,
			"tracker.backend: jira needs integrations.jira")
	}

	// A setting for a backend that is switched off reads as configuration
	// and configures nothing — the silence this whole rule exists to end.
	if c.KnowledgeBackendFor() == KnowledgeNone {
		if len(c.Knowledge.KnowledgeScope) > 0 {
			p.addSaying(field("knowledge.scope"), ErrConflict,
				"Search reach needs a knowledge base to search.",
				"a read scope needs a knowledge backend")
		}
		// VECTORS NEED SOMETHING TO SEARCH, and a knowledge base is not
		// the only thing there is: Worklet is a corpus too, and a company
		// that keeps its knowledge in Confluence's competitor's head and its
		// work here is entitled to semantic recall over the work.
		if c.Knowledge.Vectors != nil && *c.Knowledge.Vectors &&
			c.TrackerBackendFor() != TrackerWorklet {
			p.addSaying(field("knowledge.vectors"), ErrConflict,
				"Search by meaning needs a knowledge base or Worklet to search.",
				"vectors need a knowledge backend or Worklet to search")
		}
	}
	if c.Knowledge.Vectors != nil && *c.Knowledge.Vectors && c.Providers.Embeddings == nil {
		p.addSaying(field("knowledge.vectors"), ErrConflict,
			"Search by meaning needs an embeddings provider to compute embeddings with, "+
				"and this company has none.",
			"vectors need providers.embeddings to compute embeddings with")
	}
	return p.err()
}

// ConfluenceHeldBy is the knowledge base the document declares in
// Confluence's place, so an integrations.confluence block cannot be added
// beside it ([Company.validateKnowledgeBackend] refuses the pair), or "".
// An unset backend holds nothing: it derives from the block's presence.
func (c *Company) ConfluenceHeldBy() KnowledgeBackend {
	if c.Knowledge.Backend == KnowledgeDoclet {
		return KnowledgeDoclet
	}
	return ""
}

// JiraHeldBy is the tracker the document declares in Jira's place, as
// [Company.ConfluenceHeldBy] is for Confluence, or "".
func (c *Company) JiraHeldBy() TrackerBackend {
	if c.Tracker.Backend == TrackerWorklet {
		return TrackerWorklet
	}
	return ""
}

// Knowledge is which knowledge base the company runs and how much of it a
// search may read.
//
// The scope is the ONLY thing that narrows the query-time search, and it is
// role-independent on purpose: a unit's own space is IDENTITY (where its page
// activity routes, where it writes), and as a read scope it would leave an
// agent unable to read the page it was told to follow.
//
// Empty is unscoped. On Doclet that is the whole company: the engine IS the
// boundary. On Confluence it is whatever the asking seat's own account can
// read, so a credential-less seat gets nothing: an unscoped query on a shared
// admin token would show it pages its own user never could.
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
}

// Diagnostics is what this engine may say about its own defects outside the
// company: today, its anonymous issue reports (ADR-0101).
type Diagnostics struct {
	// IssueReports is whether the engine reports its own defects to the
	// project. See [IssueReports].
	IssueReports IssueReports `yaml:"issue_reports,omitempty" json:"issue_reports,omitzero" desc:"Anonymous reports of the engine's own defects, off until an administrator turns them on."`
	// Insights is whether the engine shares typed weekly records of how the
	// company's agents work (ADR-0119), under a consent of its own.
	Insights InsightsSharing `yaml:"insights,omitempty" json:"insights,omitzero" desc:"Anonymous weekly records of how the company's agents work, off until an administrator turns them on."`
}

// InsightsShare is how the engine shares its operating insights.
type InsightsShare string

// The ways to share: never, each record after an administrator sends it, or
// every eligible record on its own.
const (
	InsightsOff    InsightsShare = "off"
	InsightsReview InsightsShare = "review"
	InsightsAuto   InsightsShare = "auto"
)

// InsightsSharing is the company's consent to share operating insights. A
// SETTING, so only an administrator changes it, and off is off: nothing is
// kept, queued or sent, and a revision that turns it off cancels what waits.
type InsightsSharing struct {
	Share InsightsShare `yaml:"share,omitempty" json:"share,omitempty" js:"enum=off|review|auto" desc:"off (the default), review (each record waits for an administrator to send it) or auto (every eligible record is sent)."`
	// Accepted is the catalogue schema an administrator accepted: a build
	// whose catalogue is newer shares nothing until it is accepted again.
	Accepted int `yaml:"accepted,omitempty" json:"accepted,omitempty" desc:"The insights catalogue schema an administrator accepted. A newer catalogue shares nothing until it is accepted again."`
	// Always is the classes sent without review in review mode.
	Always []string `yaml:"always,omitempty" json:"always,omitempty" desc:"Catalogue classes sent without waiting for review, in review mode."`
}

// insightClassShape is a catalogue class id, `wake.noop_share`.
var insightClassShape = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// validate holds the consent to its closed values. Whether a class is in this
// build's catalogue is the sharer's, which reads no class it does not name.
func (s InsightsSharing) validate(path Path) error {
	var p problems
	switch s.Share {
	case "", InsightsOff, InsightsReview, InsightsAuto:
	default:
		p.add(at(path, "share"), ErrUnknownValue, "%q is not off, review or auto", s.Share)
	}
	if s.Accepted < 0 {
		p.add(at(path, "accepted"), ErrOutOfRange, "a schema is never negative")
	}
	seen := map[string]bool{}
	for i, class := range s.Always {
		switch {
		case !insightClassShape.MatchString(class):
			p.add(extend(at(path, "always"), i), ErrUnknownValue, "%q is not a catalogue class", class)
		case seen[class]:
			p.add(extend(at(path, "always"), i), ErrConflict, "%q is listed twice", class)
		}
		seen[class] = true
	}
	return p.err()
}

// IssueReports is the company's choice to report engine defects to the project
// as anonymous GitHub issues.
//
// A SETTING, so it is admin's on every path onto the document: a lead's write
// reaching it is refused, as any setting's is (internal/api/configapi's
// admission). THE RELAY IT REPORTS TO IS NOT HERE: a Tier B address would let
// whoever can write the document point a typed report anywhere, for no use a
// company has, so it is a constant of internal/issuereport.
type IssueReports struct {
	// Enabled turns the reporter on. Off by default, and off is off: nothing
	// is counted, kept or sent, and a revision that turns it off stops a send
	// already under way.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Send anonymous reports of engine defects to the project's issue tracker. Each is a typed record: never a name, a message, a path, an address or anything else of the company's."`
}

// Inbox is the founder's policy over a person's inbox: the notices every domain
// that keeps one (Worklet and Gitlet) writes to each person its changes
// concern.
//
// # Why at the top of the company, and not under the tracker
//
// A person's inbox is not one domain's: every domain whose changes are a
// person's news keeps notices under the same rules (internal/inbox). A horizon
// held by one domain's block left other sources on a horizon of their own or of
// nobody's, so one inbox aged out on as many clocks as it has sources (the
// split ADR-0018 ended for the company's clock).
type Inbox struct {
	// RetentionDays is how long a person's inbox keeps a notice.
	//
	// THE ONE HORIZON HERE THAT DELETES ANYTHING, and it deletes a row
	// DERIVED from a record rather than the record: the history those rows
	// point at is untouched and answers for ever. A year because it is a
	// MAILBOX rather than an archive — the vendors' own defaults are a
	// quarter of that — and because the question "what was I told about in
	// 2024" is answered by the history, not by the inbox. ZERO IS UNSET,
	// which [Inbox.Retention] reads as the default: no retention below a
	// month is one, so the zero value says nothing a setting could mean.
	RetentionDays int `yaml:"retention_days,omitempty" json:"retention_days,omitempty" js:"min=0;max=3650" desc:"How long a person's inbox keeps a notice (default 365, 30..3650). The history it points at is untouched."`
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

// Days is how many days an inbox notice lives, with the default applied: what
// the company states, and the epoch every applier writing a notice reads.
func (i Inbox) Days() int {
	if i.RetentionDays > 0 {
		return i.RetentionDays
	}
	return DefaultInboxRetentionDays
}

// Retention is how long an inbox notice lives, with the default applied.
func (i Inbox) Retention() time.Duration {
	return time.Duration(i.Days()) * 24 * time.Hour
}

func (i Inbox) validate(path Path) error {
	var p problems
	// ZERO IS UNSET, the default; anything written is held to its range.
	if d := i.RetentionDays; d != 0 {
		p.within(at(path, "retention_days"), float64(d), "under a month, notices are gone before "+
			"people see them, and past ten years they only repeat the history they point at")
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
// "TS" is the key the publishing harness writes into and the docs name, so a
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
// that has turned them off or has no knowledge base to hold them.
//
// UPPER, because container comparison is case-insensitive and a lower-case
// config must not silently mean another container.
//
// The empty string is an ANSWER, not an absence: a company whose ordinary work
// container is `TS` would otherwise have it silently dropped from every
// knowledge search and routing decision. `skills_container: ""` reserves no
// container; every consumer reads "" as no exclusion and no sync. ON THE
// COMPANY, not the block: `backend: none` has no container to name.
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
// company has.
//
// Without it a typo is invisible and permanent: `llm_review: claude-sonet`
// resolves through the runtime's fallback (the role's chain, "default", the
// first provider), so the seat boots and bills on a model nobody chose.
//
// Applied to EVERY seat at any depth and every field each writes
// ([Company.ProviderReferences]'s), reported where typed: the RESOLVED chain
// would hide a typo in a shadowed `llm.review`. Helpers and fleets have rules
// of their own. A fleet reference stands alone and names a fleet; entry keys
// are skipped while providers.llm is empty, a documented authoring state.
func (c *Company) validateProviderKeys() error {
	var p problems
	known := slices.Sorted(maps.Keys(c.Providers.LLM))
	for _, ref := range c.providerReferences() {
		if ref.Job == JobAssistant || ref.Job == JobAdvisor || ref.Fleet != "" ||
			c.fleetReferenceProblem(&p, ref.Segments, ref.Chain) || len(known) == 0 {
			continue
		}
		for _, key := range ref.Chain {
			if _, ok := c.Providers.LLM[key]; !ok {
				p.add(ref.Segments, ErrUnknownValue,
					"%q is not a configured provider. Use one of providers.llm's: %s. "+
						"A key that misses is not an error at run time: the seat "+
						"falls back to another model and bills against it, so this "+
						"is the only place the typo can be seen",
					key, words.Or(known))
			}
		}
	}
	return p.err()
}

// DeclaresIntegration reports whether this company uses the named surface.
//
// On the whole document, not Integrations: for Slack every agent carries its
// own app (`role.integrations.slack`) and the company-level `slack:` block may
// never be written, so asked of the block a company with seven working apps
// answers "no". Its one caller then deletes the surface's fleet status row,
// Slack's only record of the base its Request URLs were set against: the one
// warning that a moved address stranded every app.
//
// It takes integration.Kind's values as a plain string, so config imports
// nothing; a test over integration.Kinds catches a surface forgotten here. Not
// for a surface with a reconcile pass, which reads `enabled: false` as off.
func (c *Company) DeclaresIntegration(surface string) bool {
	if c == nil {
		return false
	}
	// A CHAT VENDOR IS DECLARED WHERE [chatVendors] SAYS, which is the one
	// statement of it the chat backend is derived from too: Slack counts on
	// a seat's own app as well as on the org block.
	if vendor, ok := chatVendorNamed(surface); ok {
		return vendor.declaredAt(c) != ""
	}
	in := c.Integrations
	if slices.Contains(growthBlockKeys(), surface) {
		return in.growthBlock(surface) != nil
	}
	switch surface {
	case "jira":
		return in.Jira != nil
	case "confluence":
		return in.Confluence != nil
	case "github":
		return in.GitHub != nil
	case "gitlab":
		return in.GitLab != nil
	case "datadog":
		return in.Datadog != nil
	case "cloudflare":
		return in.Cloudflare != nil
	case "expo":
		return in.Expo != nil
	case "digitalocean":
		return in.DigitalOcean != nil
	case "scaleway":
		return in.Scaleway != nil
	case "aws":
		return in.AWS != nil
	case "google_cloud":
		return in.GoogleCloud.Started()
	case "ssh":
		return in.SSH != nil
	case "atlassian":
		return in.Atlassian != nil
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
// One walk, so a cross-field rule cannot exempt the seats in units (most of
// them, with an org chart) while holding for the same seat in `roles:`.
//
// AN ITERATOR rather than a callback, so a caller looking for ONE seat can stop
// at it.
//
// EXPORTED because the ENGINE needs the same walk: a lookup of a seat's sandbox
// block by handle that stopped at the top level answered nil for every seat in
// a unit, so its box ran without its setup, env and tool servers.
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

// SandboxPlacements is every cell an executor run could ACTUALLY land in, each
// mapped to the path of the first thing that named it.
//
// ONE COMPUTATION, TWO CALLERS, and they must agree or the company is refused
// for a rule the engine then breaks anyway: validation requires a container
// image only where a container is reached, and the engine builds a backend only
// for what is reached.
//
// The DEFAULT IS ALWAYS REACHED when the catalogue is enabled: a run on an
// entry naming no cell goes there. So is `container`, on a company whose
// checks run ([Company.checksAt]).
func (c *Company) SandboxPlacements() map[Placement]string {
	reached := c.SeatPlacements()
	if at := c.checksAt(); at != "" {
		if _, seen := reached[PlacementContainer]; !seen {
			reached[PlacementContainer] = at
		}
	}
	return reached
}

// SeatPlacements is [Company.SandboxPlacements] of the seats alone: the
// catalogue's default and every cell a seat's box is placed in, each mapped to
// the path of the first thing that named it. A check's container is not one:
// it starts on a Gitlet node, never on a node for running seats.
func (c *Company) SeatPlacements() map[Placement]string {
	reached := make(map[Placement]string)
	catalogue := c.Providers.Sandbox
	if !catalogue.Enabled() {
		return reached
	}
	if run := catalogue.RunIn(); run != "" {
		reached[run] = catalogue.runInAt()
	}
	// EVERY BOX AN EXECUTOR CAN LAUNCH IN, placed as the engine places it
	// ([Company.SeatBoxes]): one per cell among the runner-capable members
	// of every agent seat's executor chain, not the head's alone. A launch
	// falls back to the chain's next member when the first is sitting out
	// on its node, and that member is placed by its own `run_in` — so a
	// cell only a fallback member names, counted for the head alone, would
	// have no backend built, and the very launch the fallback exists for
	// would fail with the manager's "no backend for container".
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
	return reached
}

// ChecksRefusal is "" where this company's checks run, each in a container
// made from providers.sandbox.local.image on the Gitlet node that claims it
// (or by the double), else why none runs: no code hosted, or no image.
//
// THE ONE RESOLUTION of whether a check runs, read by the cells a backend is
// built for ([Company.SandboxPlacements]) and by the check runner
// (internal/gitlet/checkrun) at each attempt against the epoch it runs under,
// so the two cannot disagree. A check never runs `direct`: it runs a change's
// own code, which a direct box would run as the engine's own user on the
// engine host, where it reads what the engine reads, the keyring that signs
// every run's token among it.
func (c *Company) ChecksRefusal() string {
	switch {
	case c == nil || c.CodeBackendFor() != CodeGitlet:
		return "this company hosts no code (code.backend is not gitlet), so none of its checks runs"
	case c.checksAt() == "":
		return "a check runs in a container made from providers.sandbox.local.image, which this company " +
			"does not set, so none of its checks runs"
	}
	return ""
}

// checksAt is the path of what makes this company's checks run — its
// providers.sandbox.local.image, or the double — or "" where none runs.
// Opting in through the image rather than a switch of its own: a check needs
// that image either way, and a switch beside it could only repeat it or
// contradict it.
func (c *Company) checksAt() string {
	if c == nil || c.CodeBackendFor() != CodeGitlet {
		return ""
	}
	catalogue := c.Providers.Sandbox
	switch {
	case catalogue == nil:
		return ""
	case catalogue.Fake:
		return "providers.sandbox.fake"
	case catalogue.Local != nil && strings.TrimSpace(catalogue.Local.Image) != "":
		return "providers.sandbox.local.image"
	}
	return ""
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
	// fleet is the fleet the executor runs on (its `llm`, or default-fleet
	// for a seat naming nothing), whose members are chain.
	fleet string
}

// executorSeats yields every agent seat with the executor chain it resolves to,
// in [Company.EachRole]'s order, by [phase.Resolve] over the seat as the engine
// builds it: the very call the engine's phase registry resolves a launch's
// chain through, so validation and a launch cannot disagree.
//
// A HUMAN SEAT IS SKIPPED. It is never spawned, and resolved, its empty `llm`
// falls to the fallback entry, which would count as reached by a seat that
// never runs and refuse a company for a box nobody opens. NOTHING is yielded
// for a company with no providers.llm, which is valid and runs no executor.
// A seat on a fleet has its members for chain, and one on a fleet with none
// (or no fleet) is not yielded: it waits, warned of or refused elsewhere.
func (c *Company) executorSeats() iter.Seq[executorSeat] {
	return func(yield func(executorSeat) bool) {
		models := c.Providers.PhaseModels()
		if len(models.Order) == 0 {
			return
		}
		for role, path := range c.EachRole() {
			if role.Kind == org.KindHuman {
				continue
			}
			res := phase.Resolve(role.Seat(), phase.Execute, models)
			if slices.ContainsFunc(role.LLM.Default, isFleetRef) && (res.Fleet == "" || len(res.Keys) == 0) {
				continue
			}
			if !yield(executorSeat{role: role, path: path, chain: res.Keys,
				own: res.Source == phase.SourceSeat, named: len(role.LLM.Default) > 0, fleet: res.Fleet}) {
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
// the harness it drives.
func (c *Company) describeChain(chain []string) string {
	parts := make([]string, len(chain))
	for i, key := range chain {
		entry := c.Providers.LLM[key]
		parts[i] = fmt.Sprintf("providers.llm.%s (%s)", key, entry.HarnessName())
	}
	return strings.Join(parts, ", ")
}

// validateExecutors refuses an agent seat whose executor chain has no member
// that can run it.
//
// Only a harness with an in-box runner can be an executor
// ([ExecutorHarnesses]). A launch walks the chain for its first such member not
// sitting out, so a chain with NONE takes work and fails every turn at launch:
// RUNNABLE. A non-runner beside a runner is valid, only advised about
// ([Company.executorWarnings]).
//
// Reported where the remedy is written: a chain from the seat's own `llm`
// there; seats on the fallback entry ("default", else the first) once, at its
// `harness`, naming the first seat and how many more.
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
		if seat.fleet != "" {
			p.add(at(seat.path, "llm"), ErrConflict,
				"this seat's executor runs on fleet %s, whose members are %s, and none of them "+
					"can run the executor: a seat's executor is its harness's own run, which "+
					"only %s have. Point `llm` at a fleet or an entry on %s, and keep this fleet "+
					"for the reviewer, workers and side passes with llm_review, llm_subagent, "+
					"llm_auxiliary, llm_judge or llm_triage",
				seat.fleet, c.describeChain(seat.chain), words.And(ExecutorHarnesses()),
				words.Or(ExecutorHarnesses()))
			continue
		}
		if seat.own {
			p.add(at(seat.path, "llm"), ErrConflict,
				"this seat's executor chain is %s, and no member of it can run the "+
					"executor: a seat's executor is its harness's own run, which "+
					"only %s have. Point `llm` at an entry with harness: %s "+
					"(and keep these entries for the reviewer, workers and "+
					"side passes with llm_review, llm_subagent, llm_auxiliary, "+
					"llm_judge or llm_triage), or move one of them to %s",
				c.describeChain(seat.chain), words.And(ExecutorHarnesses()), words.Or(ExecutorHarnesses()),
				words.Or(ExecutorHarnesses()))
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
		p.add(at(entryPath(key), "harness"), ErrConflict,
			"a seat that names no `llm` runs its executor on this entry, %s, and it "+
				"drives %s, which cannot run an executor: a seat's executor is its "+
				"harness's own run, which only %s have. %s. Move this entry to "+
				"%s, or point those seats' `llm` at an entry that drives one",
			which, entry.HarnessName(), words.And(ExecutorHarnesses()), seats, words.Or(ExecutorHarnesses()))
	}
	return p.err()
}

// executorWarnings is one advisory per agent seat whose executor chain holds
// a member that can never run it beside one that can.
//
// A WARNING AND NOT A RULE, because the chain is valid: a launch skips the
// member and runs on the rest, and the member still serves every text phase
// that falls back to `llm` — the reviewer, workers, side passes and
// onboarding — which any harness can. What the author should still know is that
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
		w := executorSkipsAdvice.raise(at(seat.path, "llm"), fmt.Sprintf("%s's executor never runs on "+
			"%s: a seat's executor is its harness's own run, which only %s have, "+
			"so a launch skips it and runs on %s alone. That is fine if it is there "+
			"for the reviewer, workers and side passes, which fall back to `llm`; "+
			"otherwise take it out",
			handle, c.describeChain(skipped), words.And(ExecutorHarnesses()), c.describeChain(runners)))
		models := make([]words.Said, len(skipped))
		for i, key := range skipped {
			models[i] = words.Said(c.ModelName(key))
		}
		w.Seat, w.Item = handle, words.And(models)
		out = append(out, w)
	}
	return out
}

// executorSkipsAdvice is [Company.executorWarnings]' rule; the warning's Item
// names the models the executor never runs on.
var executorSkipsAdvice = says("Backup models that never run the work",
	"A model in this agent's list cannot run its main work, so it never takes over. Remove "+
		"it, unless it is there for side tasks.")

// validateSandboxPlacement holds the rules that need BOTH the catalogue and
// the seats: whether the catalogue is there, which cells a company actually
// reaches, and whether the blocks configuring them are.
//
// Neither half can hold it alone. providers.sandbox is validated without the
// roles, so it cannot know that `run_in: container` is reached and an image is
// needed; an entry is validated without the catalogue, so it cannot know
// whether a default is there for it to fall to. Both failures are silent at
// run time — the seat takes its work and the launch dies inside a turn, every
// turn.
func (c *Company) validateSandboxPlacement() error {
	var p problems
	catalogue := c.Providers.Sandbox

	if catalogue == nil {
		// EVERY AGENT SEAT'S EXECUTOR RUNS IN A BOX, so a company with one to
		// run and a model to run it on cannot run without the catalogue: every
		// such seat would fail every turn at launch. ONE refusal, at the
		// missing block, naming the first such seat; a problem per seat would
		// bury the one edit to make.
		//
		// Not asked of a company with no providers.llm: that runs and holds
		// every seat's work until a model is added. Asked only of an ABSENT
		// block: a present one that configures nothing is refused at this path
		// by its own rule.
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
				"every agent seat's executor runs as its harness's own run, in a "+
					"box this block configures, and this company configures none: %s. "+
					"Add providers.sandbox: `local: {}` with `default_run_in: direct` "+
					"runs it on this host as the engine's user; `default_run_in: container` "+
					"with a `local.image` holding the harness runs it in a container on this host",
				seats)
		}
		return p.err()
	}
	if !catalogue.Enabled() {
		// Refused by the block's own rule, at this same path.
		return p.err()
	}

	// THE SAME QUESTION FOR EVERY ENTRY A LAUNCH CAN LAND ON — every
	// runner-capable member of every executor chain, the head and each
	// fallback alike: does an entry that names no cell have a default to fall
	// to. Unasked, the seat fails at its first turn — every turn — with a
	// launch error naming a placement no field in the file appeared to lack.
	keys := c.executorKeys()
	for _, key := range keys {
		if c.Providers.LLM[key].RunIn == "" && catalogue.RunIn() == "" {
			p.add(at(entryPath(key), "run_in"), ErrMissing,
				"a seat's executor launches on this entry, in a box, and `local:` "+
					"runs one in either of two cells (%s), so this entry has to name "+
					"one: or providers.sandbox.default_run_in has to",
				words.And(Placements))
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
		// It is only reachable on `local:` with no default: a catalogue
		// that resolves a default reaches that default, precisely so a seat
		// added later has somewhere to go. And only with no entry a launch
		// can land on, because each of those that names no cell is refused
		// above, with this same default as one of its remedies — a second
		// refusal here would say the one mistake twice.
		p.add(at(field("providers.sandbox"), "default_run_in"), ErrMissing,
			"`local:` runs an executor in either of two cells (%s) and nothing "+
				"names one, so no backend is built and a seat added later has nowhere "+
				"to run. Name the default, or remove the block while no agent seat "+
				"runs on a model",
			words.And(Placements))
	}
	if local := catalogue.localBlock(); local != nil {
		// THE IMAGE IS ONLY NEEDED WHERE A SEAT'S CONTAINER IS ACTUALLY RUN,
		// and only the walk above knows that. Required unconditionally it
		// would refuse a perfectly good direct-only company; unchecked, a
		// seat's first launch fails at container create, minutes into a turn.
		// A check reaches the container only through the image, so only a
		// seat ever asks for one; on a company hosting code, it holds what
		// the checks run too.
		hosting := c.CodeBackendFor() == CodeGitlet
		if named, wanted := reached[PlacementContainer]; wanted && strings.TrimSpace(local.Image) == "" {
			holding := "with the harness installed"
			if hosting {
				holding += ", and git 2.40 or newer and what this company's checks build with, " +
					"since its checks run in containers made from it too"
			}
			p.add(field("providers.sandbox.local.image"), ErrMissing,
				"%s runs in a container, so the local backend needs an image %s", named, holding)
		}
		if _, wanted := reached[PlacementContainer]; !wanted {
			remedy := "Give an entry a seat runs on `run_in: container` (or make it " +
				"providers.sandbox.default_run_in), or remove the field"
			if hosting {
				remedy = "Give an entry a seat runs on `run_in: container` (or make it " +
					"providers.sandbox.default_run_in), set providers.sandbox.local.image to " +
					"run this company's checks in containers, or remove the field"
			}
			for _, unread := range local.containerOnly() {
				if strings.TrimSpace(unread.value) == "" {
					continue
				}
				p.add(at(field("providers.sandbox.local"), unread.field), ErrConflict,
					"only a container box reads this, and no executor or check runs in one. %s", remedy)
			}
		}
	}
	return p.err()
}
