package agentaccess

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// labelPrefix starts every token label this engine writes.
const labelPrefix = "crewlet-"

// laneTag names the lanes a token carries, in its label.
func laneTag(l lanes) string {
	switch {
	case l.jira && l.confluence:
		return "jira-confluence"
	case l.jira:
		return "jira"
	case l.confluence:
		return "confluence"
	}
	return ""
}

// tokenLabel is the label a seat's token is minted under:
// `crewlet-<handle>-<lanes>`, to which the mint appends `-<unix seconds>`.
//
// THE LANES ARE IN THE LABEL because Atlassian lists a token's label and not
// its scopes, and which products the live token reaches is exactly what a
// lane's on or off is.
func tokenLabel(handle string, l lanes) string {
	return labelPrefix + handle + "-" + laneTag(l)
}

// labelLanes reads which lanes a token carries from its label, and false for
// a token this engine did not mint for the seat.
//
// A token minted before lanes existed is labelled `<handle>-<unix>` and was
// minted with both products' scopes, so it reads
// as carrying both.
func labelLanes(label, handle string) (lanes, int64, bool) {
	stem, stamp, ok := cutStamp(label)
	if !ok {
		return lanes{}, 0, false
	}
	if stem == handle {
		return lanes{jira: true, confluence: true}, stamp, true
	}
	tag, own := strings.CutPrefix(stem, labelPrefix+handle+"-")
	if !own {
		return lanes{}, 0, false
	}
	switch tag {
	case "jira-confluence":
		return lanes{jira: true, confluence: true}, stamp, true
	case "jira":
		return lanes{jira: true}, stamp, true
	case "confluence":
		return lanes{confluence: true}, stamp, true
	}
	return lanes{}, 0, false
}

// cutStamp splits `<stem>-<unix seconds>`.
func cutStamp(label string) (string, int64, bool) {
	i := strings.LastIndex(label, "-")
	if i <= 0 {
		return "", 0, false
	}
	stamp, err := strconv.ParseInt(label[i+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return label[:i], stamp, true
}

// ownToken is a live token this engine minted for a seat.
type ownToken struct {
	atlassian.APIToken
	lanes lanes
	stamp int64
}

// ownTokens is the live tokens this engine minted for a seat, newest first.
// A token past its expiry is not live; one whose expiry Atlassian did not
// say, or said in a shape this does not read, is.
func ownTokens(tokens []atlassian.APIToken, handle string, now time.Time) []ownToken {
	var out []ownToken
	for _, t := range tokens {
		l, stamp, ok := labelLanes(t.Label, handle)
		if !ok {
			continue
		}
		if expiry, err := time.Parse(time.RFC3339, t.Expiry); err == nil && !expiry.After(now) {
			continue
		}
		out = append(out, ownToken{APIToken: t, lanes: l, stamp: stamp})
	}
	slices.SortFunc(out, func(a, b ownToken) int {
		switch {
		case a.stamp > b.stamp:
			return -1
		case a.stamp < b.stamp:
			return 1
		}
		return strings.Compare(b.ID, a.ID)
	})
	return out
}

// renewWithin is how long before its expiry a token is replaced. Atlassian
// caps a token at a year and this engine mints for [atlassian.TokenLifetime],
// so the window is always reached well before the cap.
const renewWithin = 30 * 24 * time.Hour

// renewDue reports a token that expires inside the renewal window. A token
// whose expiry Atlassian did not say, or said in a shape this does not read,
// is not renewed on a guess.
func renewDue(t ownToken, now time.Time) bool {
	expiry, err := time.Parse(time.RFC3339, t.Expiry)
	return err == nil && expiry.Sub(now) < renewWithin
}

// appliedLanes is the lanes the last sync left on, for an account whose
// token is gone: the seat was turned off, or somebody revoked the token. The
// second is a credential to mint, not a lane somebody turned off.
func appliedLanes(rec *seataccess.Record) lanes {
	var out lanes
	if rec == nil {
		return out
	}
	for lane, record := range rec.Lanes {
		if record.Applied == nil || !record.Applied.Enabled {
			continue
		}
		switch lane {
		case seataccess.LaneJira:
			out.jira = true
		case seataccess.LaneConfluence:
			out.confluence = true
		}
	}
	return out
}

// Observe implements [seataccess.Session].
func (s *session) Observe(ctx context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	handles := map[string]bool{}
	for _, seat := range in.Seats {
		handles[seat.Handle] = true
	}
	for handle := range in.Records {
		handles[handle] = true
	}
	out := seataccess.Observation{}
	for _, handle := range slices.Sorted(maps.Keys(handles)) {
		var rec *seataccess.Record
		if held, ok := in.Records[handle]; ok {
			rec = &held
		}
		observed, err := s.observe(ctx, handle, rec)
		if err != nil {
			return nil, fmt.Errorf("atlassian: observe %s: %w", handle, err)
		}
		out[handle] = observed
	}
	return out, nil
}

// observe reads one seat's account, token and both lanes' access.
func (s *session) observe(ctx context.Context, handle string, rec *seataccess.Record) (map[seataccess.Lane]seataccess.Observed, error) {
	out := map[seataccess.Lane]seataccess.Observed{
		seataccess.LaneJira: {}, seataccess.LaneConfluence: {},
	}
	account, found, err := s.account(ctx, handle, rec)
	if err != nil || !found {
		return out, err
	}
	tokens, err := s.org.ListTokens(ctx, s.key, account.ID)
	if err != nil {
		return nil, refuseOrg(s, err)
	}
	own := ownTokens(tokens, handle, s.now())
	on := appliedLanes(rec)
	if len(own) > 0 {
		on = own[0].lanes
	}
	// GONE, OR ABOUT TO BE: the account holds no live token of this
	// engine's (revoked, or past its expiry), or its newest lapses inside
	// the renewal window [TokenLifetime] leaves room for. Whatever is sealed
	// for the seat is then, or soon will be, refused, so the plan mints a
	// replacement while a lane is on.
	gone := len(own) == 0 || renewDue(own[0], s.now())
	for _, lane := range []seataccess.Lane{seataccess.LaneJira, seataccess.LaneConfluence} {
		observed := seataccess.Observed{
			Exists: true, AccountID: account.ID, Account: accountName(account),
			Enabled: on.on(lane),
		}
		if !observed.Enabled {
			// Atlassian shows no disable of a service account at all, so a
			// lane that is off is one this engine turned off.
			observed.DisabledBy = seataccess.DisabledByCrewlet
		}
		if rec != nil && rec.Account != nil {
			observed.CreatedAt = rec.Account.CreatedAt
		}
		observed.CredentialGone = gone
		if !gone && rec != nil && rec.Credential != nil {
			cred := *rec.Credential
			observed.Credential = &cred
		}
		var lr seataccess.LaneRecord
		if rec != nil {
			lr = rec.Lanes[lane]
		}
		applied := ""
		var extra [][]string
		extra = append(extra, lr.Observed.Scope)
		if lr.Applied != nil {
			applied = lr.Applied.Level
			extra = append(extra, lr.Applied.Scope)
		}
		held, err := s.held(ctx, lane, account.ID, extra...)
		if err != nil {
			if !s.connected(lane) {
				// THE PRODUCT IS NOT CONNECTED: nothing to read, and the
				// steps that would write say so. The other lane still
				// observes.
				out[lane] = observed
				continue
			}
			return nil, err
		}
		observed.Level, observed.Scope = pickLevel(held, applied), s.observedScope(handle, lane, held)
		out[lane] = observed
	}
	return out, nil
}

// connected reports a product whose site client could be built.
func (s *session) connected(lane seataccess.Lane) bool {
	if lane == seataccess.LaneJira {
		return s.jira != nil
	}
	return s.conf != nil
}

// accountName is the vendor's own name for an account: its address where
// Atlassian assigned one, its display name otherwise.
func accountName(a atlassian.ServiceAccount) string {
	if strings.TrimSpace(a.Email) != "" {
		return a.Email
	}
	return a.DisplayName
}

// held is the level an account holds in each project or space of the lane's
// universe, keyed by project or space, as stored levels. A key absent from
// the map is one where it holds nothing.
func (s *session) held(ctx context.Context, lane seataccess.Lane, accountID string, extra ...[]string) (map[string]string, error) {
	out := map[string]string{}
	if lane == seataccess.LaneJira {
		roles, err := s.jiraRoles(ctx)
		if err != nil {
			return nil, err
		}
		for _, project := range s.universe(lane, extra...) {
			var ids []string
			for _, role := range roles {
				members, err := s.members(ctx, project, role.ID)
				if err != nil {
					return nil, err
				}
				if members[accountID] {
					ids = append(ids, strconv.FormatInt(role.ID, 10))
				}
			}
			if len(ids) > 0 {
				// TWO ROLES IN ONE PROJECT is drift whatever the document
				// asks; joined, the value can never equal a stored level,
				// so the plan restores it.
				out[project] = strings.Join(ids, " ")
			}
		}
		return out, nil
	}
	mode, err := s.roleMode(ctx)
	if err != nil {
		return nil, err
	}
	for _, key := range s.universe(lane, extra...) {
		space, found, err := s.space(ctx, key)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		var level string
		if mode.Roles() {
			level, err = s.spaceRoleOf(ctx, space.ID, accountID)
		} else {
			var perms []confluence.SpacePermission
			perms, err = s.spacePermsOf(ctx, space.ID, accountID)
			level = operationsOf(perms)
		}
		if err != nil {
			return nil, err
		}
		if level != "" {
			out[key] = level
		}
	}
	return out, nil
}

// operationsOf is a set of held permissions as one stored level.
func operationsOf(perms []confluence.SpacePermission) string {
	ops := make([]confluence.Operation, 0, len(perms))
	for _, p := range perms {
		ops = append(ops, p.Operation)
	}
	return confluence.OperationsLevel(ops)
}

// spaceRoleOf is the role an account holds in a space, read once per pass.
func (s *session) spaceRoleOf(ctx context.Context, spaceID, accountID string) (string, error) {
	if byAccount, ok := s.cache.spaceRole[spaceID]; ok {
		if role, ok := byAccount[accountID]; ok {
			return role, nil
		}
	}
	client, err := s.needConfluence()
	if err != nil {
		return "", err
	}
	role, _, err := client.SpaceRoleOf(ctx, spaceID, accountID)
	if err != nil {
		return "", refuseConfluence(s, err)
	}
	if s.cache.spaceRole[spaceID] == nil {
		s.cache.spaceRole[spaceID] = map[string]string{}
	}
	s.cache.spaceRole[spaceID][accountID] = role
	return role, nil
}

// spacePermsOf is the permissions an account holds in a space. The whole
// space is read once per pass and shared by every seat.
func (s *session) spacePermsOf(ctx context.Context, spaceID, accountID string) ([]confluence.SpacePermission, error) {
	if byAccount, ok := s.cache.spacePerms[spaceID]; ok {
		return byAccount[accountID], nil
	}
	client, err := s.needConfluence()
	if err != nil {
		return nil, err
	}
	all, err := client.SpacePermissions(ctx, spaceID)
	if err != nil {
		return nil, refuseConfluence(s, err)
	}
	s.cache.spacePerms[spaceID] = all
	return all[accountID], nil
}

// pickLevel is the one level an account holds across a lane's scope, as a
// stored level. One level everywhere is that level; mixed levels are drift,
// so the one reported is the first that differs from what the last sync
// applied, which is what makes the plan restore it. Nothing held is empty.
func pickLevel(held map[string]string, applied string) string {
	keys := slices.Sorted(maps.Keys(held))
	if len(keys) == 0 {
		return ""
	}
	for _, key := range keys {
		if held[key] != applied {
			return held[key]
		}
	}
	return held[keys[0]]
}

// observedScope is where an account holds anything, in [seataccess.Desired]'s
// terms: an empty scope stands for every project or space the company names,
// so a seat whose document scope is empty and which holds exactly that list
// reads back as empty, and the plan sees nothing to change.
func (s *session) observedScope(handle string, lane seataccess.Lane, held map[string]string) []string {
	scope := slices.Sorted(maps.Keys(held))
	if len(scope) > 0 && len(s.docScope(handle, lane)) == 0 && slices.Equal(scope, s.target(lane, nil)) {
		return nil
	}
	return scope
}

// docScope is the scope a seat's document block names for a lane.
func (s *session) docScope(handle string, lane seataccess.Lane) []string {
	d := s.doc(handle)
	switch {
	case lane == seataccess.LaneJira && d.jira != nil:
		return normalKeys(d.jira.Projects)
	case lane == seataccess.LaneConfluence && d.confluence != nil:
		return normalKeys(d.confluence.Spaces)
	}
	return nil
}
