package config

import (
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
		TokenBudget:    c.TokenBudget.Ceilings(),
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

// AccessLevelsSetting is where the per-handle GitLab overrides live in the
// document, which is what a dangling key reports as its source.
const AccessLevelsSetting = "integrations.gitlab.provisioning.access_levels"

// DanglingSettingsRefs reports each GitLab access level override whose key is
// no seat's handle in o, in sorted key order.
//
// A stale key is worse than inert. The override is looked up by handle when
// a seat's service account is provisioned ([GitLabProvisioning.OverrideFor]),
// so the entry left behind by a removed seat silently grants its level
// (maintainer, typically) to the next seat given the same handle. The
// document is checked whether or not GitLab is enabled, because re-enabling
// it is exactly when the stale grant would take effect.
//
// Any seat counts, human seats included. A key naming a human seat does
// nothing while the seat is human (provisioning creates accounts for agent
// seats only), but it names a seat the operator can see in the document, and
// a handle that is held cannot be taken by a new seat, since handles are
// unique. The grant this check exists to catch is the one waiting on a
// handle NOBODY holds.
func (c *Company) DanglingSettingsRefs(o *org.Organization) []org.DanglingRef {
	gitlab := c.Integrations.GitLab
	if gitlab == nil || gitlab.Provisioning == nil || len(gitlab.Provisioning.AccessLevels) == 0 {
		return nil
	}
	handles := make(map[string]struct{})
	for r := range o.AllRoles() {
		handles[r.Handle()] = struct{}{}
	}
	var out []org.DanglingRef
	for _, handle := range sortedKeys(gitlab.Provisioning.AccessLevels) {
		if _, found := handles[handle]; !found {
			out = append(out, org.DanglingRef{
				Kind: org.RefGitLabAccessLevel, From: AccessLevelsSetting, To: handle,
			})
		}
	}
	return out
}

// OverrideFor is the access level integrations.gitlab.provisioning's
// `access_levels` gives seat, reporting false when no key names it.
//
// BY THE SEAT'S HANDLE, which is its identity: a handle is minted when the
// seat is first written and no edit moves it, so a key that names a seat goes
// on naming that seat.
func (p *GitLabProvisioning) OverrideFor(seat *org.Role) (GitLabAccessLevel, bool) {
	if p == nil || seat == nil {
		return "", false
	}
	level, keyed := p.AccessLevels[seat.Handle()]
	return level, keyed
}
