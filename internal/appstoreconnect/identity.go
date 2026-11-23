package appstoreconnect

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/es256"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seatname"
	"github.com/crewlet/crewlet/internal/words"
)

// A seat's own team key (package doc, "A seat's own key"), sealed at
// APP_STORE_CONNECT_KEY_<HANDLE>.

// The sealed fields of a seat's own key, and the client slot it signs as.
const (
	identityField = "KEY"
	retiringField = "RETIRING_KEY"
	ownSlot       = "own"
)

// TeamKeysURL is Apple's Team Keys page, where a person makes and revokes a
// team key.
const TeamKeysURL = "https://appstoreconnect.apple.com/access/integrations/api"

// identitySpec is the lane's identity: handed in, the company's by default.
var identitySpec = seataccess.IdentitySpec{
	Ways: []seataccess.IdentityWay{seataccess.WayHandedIn}, Default: seataccess.IdentityCompany, Company: true,
	Name: seatname.AppStoreConnect, Credential: identityField, Retiring: retiringField,
}

// keyFile is the name Apple gives a downloaded key: AuthKey_<KEY ID>.p8.
var keyFile = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^AuthKey_([A-Z0-9]{10})\.p8$`) })

// LevelRole is the Apple role a seat's own key holds for its level: Marketing
// reads what most agents need and changes nothing customers see; App Manager
// is the only role short of Admin that makes every operate-class call, and
// administer's few Admin-only acts go through the company's Admin key once
// an administrator allows them.
func LevelRole(l growth.Level) string {
	if l == growth.LevelRead || l == "" {
		return RoleMarketing
	}
	return RoleAppManager
}

// roleFit is how role sits against level: the role it needs, one that reaches
// more and is allowed with a warning, or one that does not fit. Admin never
// fits: it reaches users, keys and agreements.
func roleFit(role string, level growth.Level) (fits, broader bool) {
	need := LevelRole(level)
	switch {
	case role == need:
		return true, false
	case role == RoleAdmin || role == RoleAccountHolder:
		return false, false
	case need == RoleMarketing && (role == RoleAppManager || role == RoleDeveloper):
		return true, true
	}
	return false, false
}

// roleName is an Apple role as Apple names it.
func roleName(id string) string {
	for _, r := range roles() {
		if r.ID == id {
			return r.Name
		}
	}
	return id
}

// aliveOp is a read every role may make: a key it fails is dead or another
// team's, never a narrow one.
const aliveOp = "appCategories_getCollection"

// probeOps are the reads that tell the eight roles apart, from the operation
// table's roles (TestTheRoleProbeTellsEveryRoleApart). All reads; an app's is
// read on one of the seat's apps.
var probeOps = []string{
	"alternativeDistributionDomains_getCollection", "apps_appPriceSchedule_getToOneRelated",
	"apps_backgroundAssets_getToManyRelated", "apps_analyticsReportRequests_getToManyRelated",
	"financeReports_getCollection", "actors_getCollection", "apps_perfPowerMetrics_getToManyRelated",
}

// probeRoles are the roles a team key may hold.
var probeRoles = []string{RoleAdmin, RoleAppManager, RoleDeveloper, RoleMarketing, RoleFinance, RoleSales,
	RoleCustomerSupport, RoleAccessToReports}

// roleOf is the one role whose permits are exactly permitted, false where none
// or several are.
func roleOf(permitted []bool) (string, bool) {
	var found []string
	for _, role := range probeRoles {
		match := true
		for i, id := range probeOps {
			op, _ := Catalog().Operation(id)
			if growth.Permits(op, []string{role}) != permitted[i] {
				match = false
				break
			}
		}
		if match {
			found = append(found, role)
		}
	}
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}

// The check's reads, as the sheet lists them.
var identityChecks = []seataccess.GuideCheck{
	{ID: "sign", Label: "Sign in with the key and the team's issuer"},
	{ID: "alive", Label: "Make a read every role may make"},
	{ID: "role", Label: "Read its role from seven reads"},
	{ID: "apps", Label: "Find the agent's apps in the key's team"},
}

// keyCheck is one check of a key for a seat.
type keyCheck struct {
	key   Key
	level growth.Level
	apps  []string
	// only is the team the key was kept for, "" to try every connected team.
	only string
}

// checkKey reads k back: which team accepts it, its role, and whether that
// team reaches the seat's apps. A fault (Apple not answering) is an error;
// everything Apple answered is the verdict.
func (p *Product) checkKey(ctx context.Context, k keyCheck) (seataccess.IdentityCheck, error) {
	need := roleName(LevelRole(k.level))
	out := seataccess.IdentityCheck{ID: k.key.ID, Needs: need}
	teams, err := p.teamsNow()
	if err != nil {
		return out, err
	}
	var t *team
	var client *Client
	for _, candidate := range teams {
		if k.only != "" && candidate.id != k.only {
			continue
		}
		c := p.ownClient(candidate, k.key)
		_, err := c.Do(ctx, ownSlot, Request{Method: "GET", Path: "/v1/appCategories",
			Query: url.Values{"limit": {"1"}}, Read: true, Reserved: true})
		if status := statusOf(err); status == http.StatusUnauthorized {
			continue
		} else if err != nil && status == 0 {
			return out, fmt.Errorf("appstoreconnect: check key %s with the %s team: %w", k.key.ID, candidate.id, err)
		}
		t, client = candidate, c
		break
	}
	if t == nil {
		out.Reads = append(out.Reads, seataccess.IdentityRead{Label: identityChecks[0].Label})
		if k.only != "" {
			out.Status = seataccess.IdentityRefused
			out.Said = "Apple refuses key " + k.key.ID + ": it was revoked."
			out.Remedy = "Make a new key and hand it in, or set this agent back to the company's keys."
			return out, nil
		}
		out.Status = seataccess.IdentityWrongTeam
		out.Said = "Apple accepts key " + k.key.ID + " with none of the company's teams: it is another team's key, " +
			"or it was revoked."
		out.Remedy = "Make the key in the team this agent's apps are in."
		return out, nil
	}
	out.Team = t.id
	out.Reads = append(out.Reads,
		seataccess.IdentityRead{Label: identityChecks[0].Label, OK: true, Said: "The " + t.id + " team accepts it."},
		seataccess.IdentityRead{Label: identityChecks[1].Label, OK: true})
	teamApps, err := p.appsOf(ctx, t)
	if err != nil {
		return out, err
	}
	var missing []string
	for _, app := range k.apps {
		if !slices.ContainsFunc(teamApps, func(a appInfo) bool { return a.ID == app }) {
			missing = append(missing, app)
		}
	}
	probeApp := ""
	for _, app := range k.apps {
		if !slices.Contains(missing, app) {
			probeApp = app
			break
		}
	}
	if probeApp == "" && len(teamApps) > 0 {
		probeApp = teamApps[0].ID
	}
	role, read, err := p.probeRole(ctx, client, probeApp)
	if err != nil {
		return out, err
	}
	out.Reads = append(out.Reads, read)
	out.Role = roleName(role)
	switch fits, broader := roleFit(role, k.level); {
	case role == "":
		out.Status = seataccess.IdentityWrongRole
		out.Said = "Crewlet could not tell key " + k.key.ID + "'s role from Apple's answers."
		out.Remedy = "Make a key with the " + need + " role."
		return out, nil
	case role == RoleAdmin:
		out.Status = seataccess.IdentityWrongRole
		out.Said = "Key " + k.key.ID + " holds the Admin role, which Crewlet never gives an agent: it reaches " +
			"the team's users, keys and agreements."
		out.Remedy = "Make a key with the " + need + " role; Apple cannot change a key's role."
		return out, nil
	case !fits:
		out.Status = seataccess.IdentityWrongRole
		out.Said = "Key " + k.key.ID + " holds the " + out.Role + " role, and this agent's level needs " + need + "."
		out.Remedy = "Make a key with the " + need + " role; Apple cannot change a key's role."
		return out, nil
	case broader:
		out.Broader = true
		out.Said = "Key " + k.key.ID + " holds the " + out.Role + " role, which reaches more than " + need +
			"; Crewlet still holds every call to this agent's level."
	}
	appsRead := seataccess.IdentityRead{Label: identityChecks[3].Label, OK: len(missing) == 0}
	if len(missing) > 0 {
		appsRead.Said = "Not in the " + t.id + " team: " + words.And(missing) + "."
		out.Reads = append(out.Reads, appsRead)
		out.Status = seataccess.IdentityWrongTeam
		out.Said = "This agent's apps " + words.And(missing) + " are not in the " + t.id + " team, whose key this is."
		out.Remedy = "Make the key in the team this agent's apps are in."
		return out, nil
	}
	out.Reads = append(out.Reads, appsRead)
	out.Status = seataccess.IdentityReady
	if out.Said == "" {
		out.Said = "Key " + k.key.ID + " works in the " + t.id + " team at the " + out.Role + " role."
	}
	return out, nil
}

// probeRole reads the seven probes as the key and answers its role, "" where
// the answers match no one role. A 403 is a read the role may not make; any
// other answer from Apple, a 400 on a read it may make included, is one it may.
func (p *Product) probeRole(ctx context.Context, c *Client, app string) (string, seataccess.IdentityRead,
	error) {

	read := seataccess.IdentityRead{Label: identityChecks[2].Label}
	permitted := make([]bool, len(probeOps))
	for i, id := range probeOps {
		op, _ := Catalog().Operation(id)
		path := op.Path
		if strings.Contains(path, "{id}") {
			if app == "" {
				read.Said = "The team has no app to read the role on."
				return "", read, nil
			}
			path = strings.Replace(path, "{id}", url.PathEscape(app), 1)
		}
		_, err := c.Do(ctx, ownSlot, Request{Method: "GET", Path: path, Query: url.Values{"limit": {"1"}},
			Read: true, Reserved: true})
		switch status := statusOf(err); {
		case err == nil:
			permitted[i] = true
		case status == http.StatusForbidden:
		case status == http.StatusUnauthorized || status == http.StatusTooManyRequests || status >= 500 ||
			status == 0:
			return "", read, fmt.Errorf("appstoreconnect: read the key's role (%s): %w", id, err)
		default:
			permitted[i] = true
		}
	}
	role, ok := roleOf(permitted)
	if ok {
		read.OK, read.Said = true, roleName(role)+"."
	}
	return role, read, nil
}

// statusOf is Apple's HTTP status in err, 0 where err is no answer of Apple's.
func statusOf(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

// ownClient is a client that signs with key alone, in t's team.
func (p *Product) ownClient(t *team, key Key) *Client {
	c := NewClient(p.deps.Base, t.client.issuer, map[string]Key{ownSlot: key}, p.deps.HTTP)
	c.now, c.answered = p.deps.Now, p.clock
	return c
}

// handedKey is the key a person handed in, or the refusal of it.
func handedKey(in seataccess.IdentityInput) (Key, error) {
	key, err := es256.ParseKey(in.Value)
	if err != nil {
		return Key{}, &seataccess.Refusal{Said: "The file is not an App Store Connect key: Apple's key is a .p8 " +
			"file holding BEGIN PRIVATE KEY.", Remedy: "Choose the AuthKey_<Key ID>.p8 file Apple offered."}
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		if m := keyFile().FindStringSubmatch(strings.TrimSpace(in.FileName)); m != nil {
			id = m[1]
		}
	}
	if !ascKeyID().MatchString(id) {
		return Key{}, &seataccess.Refusal{Said: "Crewlet needs the key's ID: ten capital letters and digits.",
			Remedy: "Keep the file's name AuthKey_<Key ID>.p8, or type the Key ID shown beside the key in Team Keys."}
	}
	return Key{ID: id, Key: key}, nil
}

// ascKeyID is a team key's ID.
var ascKeyID = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Z0-9]{10}$`) })

// keepKey checks a key a person handed in for a seat and answers what to keep.
func (p *Product) keepKey(ctx context.Context, in seataccess.IdentityInput) (seataccess.IdentityKept, error) {
	key, err := handedKey(in)
	if err != nil {
		return seataccess.IdentityKept{}, err
	}
	if err := p.keyTaken(in, key.ID); err != nil {
		return seataccess.IdentityKept{}, err
	}
	company := p.deps.Company()
	check, err := p.checkKey(ctx, keyCheck{key: key, level: growth.LevelOf(in.Want.Level), apps: in.Want.Scope})
	if err != nil {
		return seataccess.IdentityKept{}, err
	}
	kept := seataccess.IdentityKept{Check: check, Way: seataccess.WayHandedIn, ID: key.ID,
		Name:   seatname.For(seatname.AppStoreConnect, company.Name, in.Seat.Handle),
		Minted: []seataccess.Minted{{Field: identityField, Value: in.Value, CredentialID: key.ID}}}
	if rec := in.Record; rec.Kept() && rec.ID != key.ID {
		if old := p.sealed(in.Sinks[identityField]); old != "" {
			kept.Minted = append(kept.Minted, seataccess.Minted{Field: seataccess.RetiringField(identitySpec, rec.ID),
				Value: old, CredentialID: rec.ID})
		}
	}
	return kept, nil
}

// keyTaken refuses a key another seat keeps, or one of the company's slots.
func (p *Product) keyTaken(in seataccess.IdentityInput, id string) error {
	for handle, rec := range in.Records {
		other := rec.Lanes[seataccess.LaneAppStoreConnect].Identity
		retiring := other != nil && slices.ContainsFunc(other.Retiring,
			func(r seataccess.RetiringIdentity) bool { return r.ID == id })
		if handle != in.Seat.Handle && (other.Kept() && other.ID == id || retiring) {
			return &seataccess.Refusal{Said: "Key " + id + " is already @" + handle + "'s own key.",
				Remedy: "Make a key for this agent alone, so Apple tells the two apart."}
		}
	}
	block := p.deps.Company().Integrations.AppStoreConnect
	for _, cid := range block.ConnectionIDs() {
		for _, s := range block.Connections[cid].Keys.Slots() {
			if strings.TrimSpace(p.deps.Resolve(s.Key.KeyID)) == id {
				return &seataccess.Refusal{Said: "Key " + id + " is the company's " + slotWords[s.Name][0] + " key.",
					Remedy: "Make a key for this agent alone."}
			}
		}
	}
	return nil
}

// sealed is the value sealed at name, "" where none is.
func (p *Product) sealed(name string) string {
	if name == "" {
		return ""
	}
	return p.deps.Resolve("${" + name + "}")
}

// checkKept reads back the key a seat keeps, and whether Apple refuses the one
// it replaced yet.
func (p *Product) checkKept(ctx context.Context, in seataccess.IdentityInput) (seataccess.IdentityCheck, error) {
	rec := in.Record
	var out seataccess.IdentityCheck
	if rec.Kept() {
		value := p.sealed(in.Sinks[identityField])
		key, err := es256.ParseKey(value)
		if err != nil {
			out = seataccess.IdentityCheck{Status: seataccess.IdentityRefused, ID: rec.ID,
				Said:   "The agent's sealed key is missing or does not read as a .p8 key.",
				Remedy: "Hand its key in again."}
		} else {
			team := ""
			if rec.Check != nil {
				team = rec.Check.Team
			}
			out, err = p.checkKey(ctx, keyCheck{key: Key{ID: rec.ID, Key: key}, level: growth.LevelOf(in.Want.Level),
				apps: in.Want.Scope, only: team})
			if err != nil {
				return out, err
			}
		}
	}
	if rec != nil {
		for _, r := range rec.Retiring {
			gone, err := p.refuses(ctx, Key{ID: r.ID}, p.sealed(in.Sinks[seataccess.RetiringField(identitySpec, r.ID)]))
			if err != nil {
				return out, err
			}
			if gone {
				out.RetiringGone = append(out.RetiringGone, r.ID)
			}
		}
	}
	return out, nil
}

// refuses reports whether every connected team refuses the key sealed as
// value: revoked at Apple. A value that no longer reads proves nothing.
func (p *Product) refuses(ctx context.Context, key Key, value string) (bool, error) {
	parsed, err := es256.ParseKey(value)
	if err != nil {
		return false, nil //nolint:nilerr // Nothing sealed to read with: a person's revoke is not proven.
	}
	key.Key = parsed
	teams, err := p.teamsNow()
	if err != nil {
		return false, err
	}
	for _, t := range teams {
		_, err := p.ownClient(t, key).Do(ctx, ownSlot, Request{Method: "GET", Path: "/v1/appCategories",
			Query: url.Values{"limit": {"1"}}, Read: true, Reserved: true})
		switch status := statusOf(err); {
		case err == nil:
			return false, nil
		case status != http.StatusUnauthorized:
			return false, fmt.Errorf("appstoreconnect: read whether Apple refuses key %s: %w", key.ID, err)
		}
	}
	return true, nil
}

// dropKey gives up a seat's key: Crewlet cannot revoke a team key, so it keeps
// the key sealed as retiring and asks a person to revoke it at Apple.
func (p *Product) dropKey(in seataccess.IdentityInput) seataccess.IdentityDropped {
	out := seataccess.IdentityDropped{Waiting: &seataccess.Waiting{ActionURL: TeamKeysURL,
		Detail: "Revoke key " + in.Record.ID + " at Apple: Users and Access, Integrations, Team Keys."}}
	if value := p.sealed(in.Sinks[identityField]); value != "" {
		out.Minted = []seataccess.Minted{{Field: seataccess.RetiringField(identitySpec, in.Record.ID), Value: value,
			CredentialID: in.Record.ID}}
	}
	return out
}

// IdentityGuide is the sheet that sets up a seat's own key, in Apple's words.
func (accessVendor) IdentityGuide(c *config.Company, seat seataccess.SeatRef, _ seataccess.Lane,
	want seataccess.Desired) seataccess.IdentityGuide {

	name := seatname.For(seatname.AppStoreConnect, c.Name, seat.Handle)
	role := roleName(LevelRole(growth.LevelOf(want.Level)))
	return seataccess.IdentityGuide{
		Vendor: "Apple", Noun: "key", Records: "its submissions",
		Why:     "Apple then records its submissions under its own key, and you can revoke it alone.",
		Company: "the company's team keys", Name: name, NameRule: seatname.AppStoreConnect.Words,
		NameWhat: seatname.AppStoreConnect.What, Role: role, Never: "Admin",
		Steps: []seataccess.GuideStep{
			{Text: "Open Users and Access, then Integrations, then Team Keys."},
			{Text: "Press Generate API Key, or + if the team already has keys."},
			{Text: "Name:", Copy: &seataccess.GuideCopy{Label: "key's name", Value: name,
				Rule: seatname.AppStoreConnect.Words}},
			{Text: "Access: " + role + ". Never Admin."},
			{Text: "Press Generate."},
			{Text: "On the new key's row, press Download API Key. Apple offers the file once."},
		},
		Link: seataccess.GuideLink{Label: "Open Team Keys", URL: TeamKeysURL},
		Note: "Only the Account Holder or an Admin can make team keys. A key reaches every app of the team; " +
			"Crewlet keeps the agent to its own.",
		Back: seataccess.GuideBack{Kind: seataccess.BackFile, Accept: ".p8",
			Hint: "The file Apple offered: AuthKey_<Key ID>.p8", Label: "Key ID"},
		Checks: identityChecks,
	}
}
