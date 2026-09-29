package config

import (
	"strings"

	"github.com/crewlet/crewlet/internal/org"
)

// Organization builds the runtime company from this revision: the seats,
// the hierarchy, and the org-wide facts every seat reads.
//
// It NORMALIZES the result: root seats carrying a unit reference move into
// that unit, leads and channels cascade, unit credentials layer under their
// agent members', a lead gains a manages entry for every direct member no
// direct member of its unit already manages, and a manages entry naming a
// unit expands to its seats ([org.Organization.Normalize] has the rules
// and their order). Doing that at the boundary is why nothing downstream
// has to know whether a seat was authored inside its unit or moved into it.
//
// The returned organization is a fresh tree: the config it came from is
// untouched, because a stored revision is read again on the next apply and
// a normalisation that mutated it would compound.
func (c *Company) Organization() (*org.Organization, error) {
	o, _ := c.organization()
	if err := o.Validate(); err != nil {
		return nil, err
	}
	return o, nil
}

// organization builds and normalises without validating, so [Company.Validate]
// can report the org's failures alongside its own rather than stopping at
// the first. It also returns where each seat and unit of the result was
// authored (see [identityIndex]), recorded before normalization moves any.
func (c *Company) organization() (*org.Organization, *identityIndex) {
	index := newIdentityIndex()
	o := &org.Organization{
		Name:           c.Name,
		Mission:        c.Mission,
		Vision:         c.Vision,
		Policies:       append([]string(nil), c.Policies...),
		TokenBudget:    c.TokenBudget,
		KnowledgeScope: append([]string(nil), c.Knowledge.KnowledgeScope...),
	}
	for i := range c.Roles {
		seat := c.Roles[i].Seat()
		index.addSeat(seat, &c.Roles[i], idx(field("roles"), i))
		o.Roles = append(o.Roles, seat)
	}
	for i := range c.Units {
		unit := c.Units[i].Unit()
		index.addUnit(unit, &c.Units[i], idx(field("units"), i))
		o.Units = append(o.Units, unit)
	}
	o.Normalize()
	return o, index
}

// DanglingRefs reports every reference this revision resolves to nothing:
// the organization's own ([org.Organization.DanglingRefs]: a unit's lead, a
// root seat's unit, a manages entry) followed by the ones only the whole
// document can see, which today is a GitLab access level keyed by a handle
// no seat answers to ([Company.DanglingSettingsRefs]).
//
// They are NOT validation failures, which is why they are a separate call
// rather than part of [Company.Validate]. Live config management bootstraps
// an org in pieces, so a unit can legitimately land before the seat that
// leads it, and every reader already treats a dangling lead as no lead.
// Refusing the revision would make the intermediate state unreachable;
// reporting it is what an operator needs.
//
// SPLIT BY WHAT EACH LAYER CAN SEE. The org package does not import config
// and its model carries no integrations, so a reference out of the
// integrations block cannot be checked there; it is checked here, against
// the same normalized organization, so both halves agree on which seats
// exist.
func (c *Company) DanglingRefs() []org.DanglingRef {
	o, _ := c.organization()
	return append(o.DanglingRefs(), c.DanglingSettingsRefs(o)...)
}

// accessLevelsPath is where the per-handle GitLab overrides live in the
// document, which is what a dangling key reports as its source.
const accessLevelsPath = "integrations.gitlab.provisioning.access_levels"

// routeToPath is where Datadog's fallback seat lives in the document.
const routeToPath = "integrations.datadog.route_to"

// SeatReference is one SETTING that names a seat by a handle, and what an org
// chart makes of it.
//
// # Why the settings name a seat at all, and what that costs
//
// Two settings route by a seat's handle: a GitLab access level override, keyed
// by the seat it grades, and Datadog's fallback seat. They live in the
// SETTINGS half, and the seat they name lives in the org chart — a log of its
// own, written per object. A rename there moves every reference the chart
// holds (a lead, a `manages:` entry) in its own record, and cannot move one in
// a document it does not own. So a setting names a seat by an address a
// rename can retire, and the seat is found the way every reference to a seat
// is: through [org.Organization.Role], its live handle first, then the handle
// it was created under, then the ones it has answered to since. A reference
// through a retired address therefore still reaches the seat — until a later
// hire is given that address (only the one a seat was CREATED under is never
// issued twice), when it silently re-points to the newcomer. That is what
// [SeatReference.Retired] exists to say before it happens.
type SeatReference struct {
	// Setting is the setting's path in the document: the access level map
	// for an override, `integrations.datadog.route_to` for the fallback.
	Setting string

	// Handle is the address as the setting wrote it.
	Handle string

	// Seat is the seat the handle names in the chart it was resolved
	// against, nil when nothing answers to it.
	Seat *org.Role
}

// Retired reports a reference that reaches its seat only through an address
// the seat no longer answers to: it works today, and names whoever the chart
// next gives a retired alias to.
func (r SeatReference) Retired() bool {
	return r.Seat != nil && r.Seat.Handle() != r.Handle
}

// SeatReferences resolves every seat this document's settings name against o:
// each GitLab access level override in key order — whether or not GitLab is
// enabled, because re-enabling it is exactly when a stale grant takes effect —
// then Datadog's fallback seat while the integration is enabled, unless it
// dismisses on purpose (`none`) or is not a handle at all (refused by
// validation, and nothing a chart can make name a seat).
//
// AGAINST THE CHART IT IS HANDED, which is what makes it the one reading for
// both halves' owners: a company file resolves against its own chart, and the
// continuous report against the chart a node is running.
func (c *Company) SeatReferences(o *org.Organization) []SeatReference {
	var out []SeatReference
	if gitlab := c.Integrations.GitLab; gitlab != nil && gitlab.Provisioning != nil {
		for _, handle := range sortedKeys(gitlab.Provisioning.AccessLevels) {
			out = append(out, SeatReference{
				Setting: accessLevelsPath, Handle: handle, Seat: o.Role(handle),
			})
		}
	}
	if dd := c.Integrations.Datadog; dd != nil && dd.Enabled {
		fallback := strings.TrimSpace(dd.RouteTo)
		if fallback != "" && fallback != DatadogIgnore && org.ValidHandle(fallback) {
			out = append(out, SeatReference{
				Setting: routeToPath, Handle: fallback, Seat: o.Role(fallback),
			})
		}
	}
	return out
}

// DanglingSettingsRefs reports each GitLab access level override whose key
// names no seat, in sorted key order, resolved against o.
//
// A stale key is worse than inert. An override follows the seat its key names
// ([GitLabProvisioning.OverrideFor]), so the entry left behind by a removed
// seat silently grants its level (maintainer, typically) to the next seat
// given that handle. Any seat counts, human seats included: a key naming a
// human seat does nothing while the seat is human (provisioning creates
// accounts for agent seats only), but it names a seat the operator can see in
// the chart. The grant this check exists to catch is the one waiting on a
// handle NOBODY answers to.
//
// Datadog's fallback is not here: a company file is REFUSED for one naming no
// agent seat, and the running pair reports it as an alert nobody receives.
func (c *Company) DanglingSettingsRefs(o *org.Organization) []org.DanglingRef {
	var out []org.DanglingRef
	for _, ref := range c.SeatReferences(o) {
		if ref.Setting == accessLevelsPath && ref.Seat == nil {
			out = append(out, org.DanglingRef{
				Kind: org.RefGitLabAccessLevel, From: accessLevelsPath, To: ref.Handle,
			})
		}
	}
	return out
}

// OverrideFor is the access level integrations.gitlab.provisioning's
// `access_levels` gives seat, reporting false when no key names it.
//
// A KEY NAMES THE SEAT IT RESOLVES TO ([org.Organization.Role]), not the seat
// whose handle it happens to equal. Compared with the handle in hand, an
// override stopped applying the moment the seat was renamed, and whoever was
// later given the old handle inherited the grant; followed through the chart,
// it stays with the seat — and a key that is another seat's live handle is
// that seat's, never this one's, however this one used to be called.
//
// THE STRONGEST ADDRESS WINS where several keys name one seat: its live
// handle, then the handle it was created under, then the ones it has answered
// to since, newest first — the order the chart resolves them in, so an
// operator who writes the current handle beside a stale one is the one heard.
func (p *GitLabProvisioning) OverrideFor(o *org.Organization, seat *org.Role) (GitLabAccessLevel, bool) {
	if p == nil || o == nil || seat == nil || len(p.AccessLevels) == 0 {
		return "", false
	}
	addresses := append([]string{seat.Handle(), seat.Origin()}, seat.FormerHandles...)
	for _, address := range addresses {
		level, keyed := p.AccessLevels[address]
		if keyed && o.Role(address) == seat {
			return level, true
		}
	}
	return "", false
}
