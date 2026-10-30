// Package seataccess is the shared contract for what each agent seat may do at
// each third-party app the engine manages accounts on, and the reconcile that
// holds the vendor to it.
//
// # Why a contract of its own
//
// Six vendor packages create an account per agent: Datadog, GitLab, Atlassian
// (for Jira and Confluence), Mattermost, Slack and GitHub. Before this package
// each decided on its own whether a seat was "on" (by finding a `${VAR}` in the
// seat's `mcp_env`), what it could do (one company-wide role), and what to do
// when the vendor disagreed (nothing: no pass read an account back). Three
// failures came from that shape, and the incident that started this work had
// all three: a seat's credential sealed into a variable a company block also
// referenced, an access level nobody could set per agent, and an account
// changed at the vendor that the engine never noticed.
//
// So the rules that are the SAME for every vendor live here, once:
//
//   - On or off is a stored setting (`roles[].integrations.<lane>.enabled`),
//     never inferred from a variable's presence ([Desired]).
//   - Access is stored in the vendor's own terms, and any grouping the engine
//     needs (is this a raise to an administration role) is derived from the
//     vendor's own order and never stored ([NeedsStepUp]).
//   - A seat's credential is sealed only under the name the engine derives for
//     it ([SinkName]), never one the document names, and never one a company
//     block references ([CompanyVariables]).
//   - Every pass computes the same plan from the document (desired), the
//     account ledger, and what the vendor shows now (observed), and the
//     preview route runs the same function ([Plan]), so a preview cannot
//     promise what a sync will not do.
//   - Off disables and keeps the account (D1); remove is a separate,
//     confirmed action allowed only when off. Drift in level or scope is
//     restored and reported (D2). An account somebody disabled at the vendor
//     is never re-enabled without being asked (D3).
//   - A credential minted and then not sealed is revoked, for every vendor
//     ([Minted.Revoke]).
//
// What is NOT the same for every vendor is each step's call, and that is the
// [Vendor] interface each vendor package implements and registers ([Register]).
//
// # Who has to agree on the ledger
//
// The whole company, now. The reconcile runs as a worker duty on whichever node
// holds it, and the access routes answer from whichever node serves the
// request, so the ledger lives on the fleet's coordination store beside the
// integration statuses ([Ledger], coord.SeatAccess), and every writer holds the
// kind's provisioning lease while it writes, which is what makes a plain put
// safe (the argument coord.Integrations makes in full).
package seataccess

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
)

// Lane is one place a seat holds access within an integration, and the key of
// its block under `roles[].integrations`.
//
// ONE LANE PER KIND, except Atlassian: its organization is where a seat's ONE
// account is created, and Jira and Confluence are two products that account is
// granted separately, each with its own roles and its own scope. So Atlassian
// has two lanes over one account, and every other kind has one lane named
// after itself.
//
// The values are STORED (document keys, ledger records, the access routes'
// paths), so a rename is a migration rather than an edit.
type Lane string

// The lanes this build manages.
const (
	LaneDatadog    Lane = "datadog"
	LaneGitHub     Lane = "github"
	LaneGitLab     Lane = "gitlab"
	LaneJira       Lane = "jira"
	LaneConfluence Lane = "confluence"
	LaneSlack      Lane = "slack"
	LaneMattermost Lane = "mattermost"
)

// Lanes is every lane, in the order an operator reads integrations
// ([integration.Kinds]): chat, then the tracker and its wiki, then the code
// hosts, then observability.
func Lanes() []Lane {
	return []Lane{
		LaneSlack, LaneMattermost, LaneJira, LaneConfluence,
		LaneGitHub, LaneGitLab, LaneDatadog,
	}
}

// Valid reports whether l is a lane this build manages. A value off the wire
// is a value rather than a panic, for [integration.Kind.Valid]'s reason.
func (l Lane) Valid() bool { return slices.Contains(Lanes(), l) }

// String makes a Lane printable without a conversion at every log site.
func (l Lane) String() string { return string(l) }

// Kind is the integration whose account this lane grants: Atlassian for Jira
// and Confluence, the lane's own name for every other.
func (l Lane) Kind() integration.Kind {
	switch l {
	case LaneJira, LaneConfluence:
		return integration.KindAtlassian
	default:
		return integration.Kind(l)
	}
}

// Kinds is every integration that manages per-seat access, in reading order.
// It is the set the access routes answer for.
func Kinds() []integration.Kind {
	var out []integration.Kind
	for _, lane := range Lanes() {
		if kind := lane.Kind(); !slices.Contains(out, kind) {
			out = append(out, kind)
		}
	}
	return out
}

// LanesOf is the lanes one integration grants, in reading order. Empty for a
// kind that manages no per-seat access.
func LanesOf(kind integration.Kind) []Lane {
	var out []Lane
	for _, lane := range Lanes() {
		if lane.Kind() == kind {
			out = append(out, lane)
		}
	}
	return out
}

// Access is GitHub's word for one permission's grant: read or write. Absent
// from [Permissions] is GitHub's "No access". The document's own type, so the
// document and the wire cannot spell a grant two ways.
type Access = config.GitHubAccess

// GitHub's two grants, in its own order.
const (
	AccessRead  = config.GitHubRead
	AccessWrite = config.GitHubWrite
)

// Permissions is a GitHub App's repository permissions, by GitHub's own key
// (`contents`, `issues`, `pull_requests`, `checks`, `actions`, `workflows`).
type Permissions map[string]Access

// Equal reports whether two permission sets grant the same, treating an
// absent key and a nil map as No access.
func (p Permissions) Equal(other Permissions) bool {
	for _, key := range unionKeys(p, other) {
		if p[key] != other[key] {
			return false
		}
	}
	return true
}

// Wider is the keys want grants more on than held, sorted.
func (p Permissions) Wider(held Permissions) []string {
	var out []string
	for _, key := range unionKeys(p, held) {
		if p[key].Rank() > held[key].Rank() {
			out = append(out, key)
		}
	}
	return out
}

func unionKeys(a, b Permissions) []string {
	keys := slices.Collect(maps.Keys(a))
	for key := range b {
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// Desired is what the company document asks for one seat on one lane.
//
// IN THE VENDOR'S TERMS. Level is the identifier the vendor's API takes (a
// Datadog role id, a GitLab access level as its integer, a Jira project role
// id, a Confluence space role id, Mattermost's space-separated role names),
// never a cross-vendor word. GitHub has no levels: it holds Permissions.
//
// The wire shape is the dashboard's (`desired` on the access routes).
type Desired struct {
	// Enabled is the stored on or off. Off disables the vendor account and
	// keeps it (D1).
	Enabled bool `json:"enabled"`

	// Level is the vendor's identifier for the access held; empty on a lane
	// that has no levels (GitHub, Slack). Per lane:
	//
	//   - datadog: the role id.
	//   - gitlab: the access level as its integer (`30`), or
	//     `member_role:<id>` for a custom role on Ultimate.
	//   - jira: the project role id.
	//   - confluence: the space role id, or, where the site has no space
	//     roles, the space permissions as `<operation>:<target>` entries,
	//     sorted and space-separated (`create:page read:space`).
	//   - mattermost: the roles, space-separated in Mattermost's own order
	//     ([config.MattermostRoleSets]).
	Level string `json:"level,omitempty"`

	// Permissions is GitHub's per-permission grant; nil elsewhere.
	Permissions Permissions `json:"permissions,omitempty"`

	// Scope is the repositories, projects, spaces or channels the seat is
	// narrowed to. Empty means the vendor's default reach for the lane, which
	// [LaneSpec.ScopeEmpty] says in words.
	Scope []string `json:"scope,omitempty"`
}

// SameAccess reports whether two settings grant the same level, permissions
// and scope, ignoring on or off and the order of the scope list.
func (d Desired) SameAccess(other Desired) bool {
	return d.Level == other.Level && d.Permissions.Equal(other.Permissions) &&
		sameSet(d.Scope, other.Scope)
}

// SameLevel reports whether two settings grant the same level and
// permissions, ignoring scope.
func (d Desired) SameLevel(other Desired) bool {
	return d.Level == other.Level && d.Permissions.Equal(other.Permissions)
}

// SameScope reports whether two settings narrow to the same scope.
func (d Desired) SameScope(other Desired) bool { return sameSet(d.Scope, other.Scope) }

// Normalized is d with its scope trimmed, deduplicated and sorted, which is
// the form the ledger records and the plan compares.
func (d Desired) Normalized() Desired {
	out := d
	out.Level = strings.TrimSpace(d.Level)
	out.Scope = normalSet(d.Scope)
	if len(d.Permissions) > 0 {
		out.Permissions = maps.Clone(d.Permissions)
	}
	return out
}

func normalSet(in []string) []string {
	var out []string
	for _, item := range in {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func sameSet(a, b []string) bool { return slices.Equal(normalSet(a), normalSet(b)) }

// DisabledBy says who turned an account off: this engine, or somebody at the
// vendor. The difference is D3: an account this engine disabled is turned back
// on when its seat is, and one somebody else disabled never is without being
// asked, in case it was incident response.
type DisabledBy string

const (
	// DisabledByCrewlet is an account this engine disabled, recorded in the
	// ledger when it did.
	DisabledByCrewlet DisabledBy = "crewlet"

	// DisabledByHand is an account the vendor shows disabled that the ledger
	// does not record this engine disabling.
	DisabledByHand DisabledBy = "hand"
)

// Credential is the engine-owned variable a seat's credential is sealed in,
// and when and by whom it was sealed. Never the value.
type Credential struct {
	// Variable is the sealed name ([SinkName]); for a kind with two (an
	// Atlassian token and its account address, a Slack token and its signing
	// secret) the first is the credential and the rest ride in Others.
	Variable string   `json:"variable"`
	Others   []string `json:"others,omitempty"`

	// MintedAt is when the value was minted and sealed.
	MintedAt time.Time `json:"minted_at"`

	// By is who the pass that sealed it ran for, as [iam.Actor] names them.
	By string `json:"by,omitempty"`

	// Source is [CredentialMinted] for a credential Crewlet minted at the
	// vendor, and [CredentialExternal] for one handed in from outside
	// Crewlet (an operator's Secret, an app created elsewhere), which the
	// engine binds and never writes or rotates.
	Source string `json:"source,omitempty"`
}

// The two sources of a seat's credential ([Credential.Source]).
const (
	CredentialMinted   = "minted"
	CredentialExternal = "external"
)

// Observed is what the vendor showed on the last sync for one seat on one
// lane, read back every pass. The wire shape is the dashboard's (`observed`).
type Observed struct {
	// Exists is whether the account this engine made is there at all.
	Exists bool `json:"exists"`

	// Enabled is whether it can act on this lane: an active account at
	// most vendors, product access for an Atlassian lane, an installation
	// that is not suspended at GitHub.
	Enabled bool `json:"enabled"`

	// DisabledBy says who turned it off, where Enabled is false.
	DisabledBy DisabledBy `json:"disabled_by,omitempty"`

	// Account is the vendor's own name for the account (an address, a
	// username, a bot's @name) and AccountID its identifier, as the ledger
	// records it.
	Account   string `json:"account,omitempty"`
	AccountID string `json:"account_id,omitempty"`

	// Level, Permissions and Scope are what the vendor holds now, in the
	// same terms as [Desired].
	Level       string      `json:"level,omitempty"`
	Permissions Permissions `json:"permissions,omitempty"`
	Scope       []string    `json:"scope,omitempty"`

	// Ceiling is GitHub's only: the permissions the app is registered with,
	// which every token is narrowed from. A raise past it needs an owner to
	// accept the app's new permissions (D5).
	Ceiling Permissions `json:"ceiling,omitempty"`

	// Credential is the sealed variable and when it was minted; nil where
	// nothing is sealed for the seat.
	Credential *Credential `json:"credential,omitempty"`

	// CreatedAt is when this engine created the account.
	CreatedAt time.Time `json:"created_at,omitzero"`

	// ChangedByHandAt is when a sync found the level or scope changed at the
	// vendor by somebody else (D2), cleared once the document and the vendor
	// agree again.
	ChangedByHandAt time.Time `json:"changed_by_hand_at,omitzero"`
}

// Access is the observed grant in [Desired]'s shape, for comparing the two.
func (o Observed) Access() Desired {
	return Desired{
		Enabled: o.Exists && o.Enabled, Level: o.Level,
		Permissions: o.Permissions, Scope: o.Scope,
	}
}

// SeatRef names one agent seat for a vendor: its handle, which every account
// and every sealed name derives from (immutable, ADR-0013), and its display
// name, which accounts are labelled with.
type SeatRef struct {
	Handle string `json:"handle"`
	Name   string `json:"name"`
}
