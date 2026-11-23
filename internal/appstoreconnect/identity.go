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
	"github.com/crewlet/crewlet/internal/vendortext"
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

// identitySpec is the lane's identity: each seat's own key, handed in, and
// never the company's, which holds none (ADR-0179).
var identitySpec = seataccess.IdentitySpec{
	Ways: []seataccess.IdentityWay{seataccess.WayHandedIn}, Default: seataccess.IdentityOwn,
	Name: seatname.AppStoreConnect, Credential: identityField, Retiring: retiringField,
}

// keyFile is the name Apple gives a downloaded key: AuthKey_<KEY ID>.p8.
var keyFile = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^AuthKey_([A-Z0-9]{10})\.p8$`) })

// LevelRole is the Apple role a seat's own key holds for its level: Marketing
// reads what most agents need and changes nothing customers see; App Manager
// is the only role short of Admin that makes every operate-class call. An act
// only Admin may make is made by a person in App Store Connect.
func LevelRole(l growth.Level) string {
	if l == growth.LevelRead || l == "" {
		return RoleMarketing
	}
	return RoleAppManager
}

// roleFit is how role sits against level: the role it needs, one that reaches
// more and is allowed with a warning, or one that does not fit. Admin never
// fits: it reaches users, keys and agreements. Sales fits the read level, for
// an agent that reads sales, and Customer Support the operate level, for one
// that answers reviews: no other role an agent holds may make either.
func roleFit(role string, level growth.Level) (fits, broader bool) {
	need := LevelRole(level)
	switch {
	case role == need:
		return true, false
	case need == RoleMarketing && role == RoleSales, need == RoleAppManager && role == RoleCustomerSupport:
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
// table's roles (TestTheRoleProbeTellsEveryRoleApart), each one whose roles
// Apple publishes and no team-level program gates
// (TestEachRoleProbeHoldsApplesRoles). All reads; an app's is read on one of
// the seat's apps.
var probeOps = []string{
	"apps_appPriceSchedule_getToOneRelated", "apps_backgroundAssets_getToManyRelated",
	"apps_analyticsReportRequests_getToManyRelated", "financeReports_getCollection",
	"apps_perfPowerMetrics_getToManyRelated",
}

// probeRoles are the roles a team key may hold.
var probeRoles = []string{RoleAdmin, RoleAppManager, RoleDeveloper, RoleMarketing, RoleFinance, RoleSales,
	RoleCustomerSupport, RoleAccessToReports}

// probeAnswer is what Apple answered one probe read: known where the answer
// says whether the key's role may make it.
type probeAnswer struct {
	op    string
	known bool
	may   bool
	// said is the answer as a log and the sheet's detail line show it: the
	// status and Apple's code, never the key or its token.
	said string
}

// rolesOf is every role whose permits agree with each known answer.
func rolesOf(answers []probeAnswer) []string {
	var out []string
	for _, role := range probeRoles {
		if len(contradicted(role, answers)) == 0 {
			out = append(out, role)
		}
	}
	return out
}

// contradicted is each probe whose known answer role's permits disagree with.
func contradicted(role string, answers []probeAnswer) []string {
	var out []string
	for _, a := range answers {
		op, _ := Catalog().Operation(a.op)
		if a.known && growth.Permits(op, []string{role}) != a.may {
			out = append(out, a.op)
		}
	}
	return out
}

// answersLine is the probes' answers on one line, in probe order.
func answersLine(answers []probeAnswer) string {
	parts := make([]string, 0, len(answers))
	for _, a := range answers {
		parts = append(parts, a.op+" "+a.said)
	}
	return strings.Join(parts, "; ")
}

// The check's reads, as the sheet lists them, in the order the check makes
// them: a read that did not run is never drawn done.
var identityChecks = []seataccess.GuideCheck{
	{ID: "sign", Label: "Sign in with the key and the team's issuer"},
	{ID: "alive", Label: "Make a read every role may make"},
	{ID: "apps", Label: "Find the agent's apps in the key's team"},
	{ID: "role", Label: "Read its role from five reads"},
}

// The steps of identityChecks, by position.
const (
	stepSign = iota
	stepAlive
	stepApps
	stepRole
)

// keyCheck is one check of a key for a seat.
type keyCheck struct {
	key   Key
	level growth.Level
	apps  []string
	// only is the team the key was kept for, "" to try every connected team.
	only string
}

// refusedAt ends out at step with Apple's refusal: that step's read failed
// with Apple's reason, and no later step ran.
func refusedAt(out seataccess.IdentityCheck, step int, r *seataccess.Refusal) seataccess.IdentityCheck {
	out.Reads = append(out.Reads, seataccess.IdentityRead{Label: identityChecks[step].Label, Said: r.Said})
	out.Status, out.Said, out.Remedy = seataccess.IdentityRefused, r.Said, r.Remedy
	return out
}

// checkKey reads k back: which team accepts it, whether that team holds the
// seat's apps, listed with k itself, which also names the team's apps for the
// chooser, and its role. A fault (Apple not answering) is an error;
// everything Apple answered is the verdict, a refusal at the step it refused.
func (p *Product) checkKey(ctx context.Context, k keyCheck) (seataccess.IdentityCheck, error) {
	need := roleName(LevelRole(k.level))
	out := seataccess.IdentityCheck{ID: k.key.ID, Needs: need}
	teams, err := p.teamsNow()
	if err != nil {
		return out, err
	}
	var t *team
	var client *Client
	var signErr error
	for _, candidate := range teams {
		if k.only != "" && candidate.id != k.only {
			continue
		}
		c := p.ownClient(candidate, k.key)
		_, signErr = c.Do(ctx, ownSlot, Request{Method: "GET", Path: "/v1/appCategories",
			Query: url.Values{"limit": {"1"}}, Read: true, Reserved: true})
		if statusOf(signErr) == http.StatusUnauthorized {
			continue
		}
		t, client = candidate, c
		break
	}
	if t == nil {
		out.Reads = append(out.Reads, seataccess.IdentityRead{Label: identityChecks[stepSign].Label})
		if k.only != "" {
			out.Status = seataccess.IdentityRefused
			out.Said = "Apple refuses key " + k.key.ID + ": it was revoked."
			out.Remedy = "Make a new key and hand it in."
			return out, nil
		}
		out.Status = seataccess.IdentityWrongTeam
		out.Said = "Apple accepts key " + k.key.ID + " with none of the company's teams: it is another team's key, " +
			"or it was revoked."
		out.Remedy = "Make the key in the team this agent's apps are in."
		return out, nil
	}
	out.Team = t.id
	if signErr != nil {
		if r, ok := seataccess.RefusalOf(signErr); ok {
			return refusedAt(out, stepSign, r), nil
		}
		return out, fmt.Errorf("appstoreconnect: check key %s with the %s team: %w", k.key.ID, t.id, signErr)
	}
	out.Reads = append(out.Reads,
		seataccess.IdentityRead{Label: identityChecks[stepSign].Label, OK: true, Said: "The " + t.id + " team accepts it."},
		seataccess.IdentityRead{Label: identityChecks[stepAlive].Label, OK: true})
	// THE KEY BEING CHECKED LISTS THE APPS: it is the reach that matters for
	// this agent, and the company holds no key to list them with.
	teamApps, err := p.readApps(ctx, t, client, true)
	if r, ok := seataccess.RefusalOf(err); ok {
		return refusedAt(out, stepApps, r), nil
	}
	if err != nil {
		return out, err
	}
	var missing []string
	for _, app := range k.apps {
		if !slices.ContainsFunc(teamApps, func(a appInfo) bool { return a.ID == app }) {
			missing = append(missing, app)
		}
	}
	if len(missing) > 0 {
		named := p.appLabels(missing)
		out.Reads = append(out.Reads, seataccess.IdentityRead{Label: identityChecks[stepApps].Label,
			Said: "Not in the " + t.id + " team: " + words.And(named) + "."})
		out.Status = seataccess.IdentityWrongTeam
		out.Said = "This agent's apps " + words.And(named) + " are not in the " + t.id + " team, whose key this is."
		out.Remedy = "Make the key in the team this agent's apps are in."
		return out, nil
	}
	out.Reads = append(out.Reads, seataccess.IdentityRead{Label: identityChecks[stepApps].Label, OK: true})
	probeApp := ""
	if len(k.apps) > 0 {
		probeApp = k.apps[0]
	} else if len(teamApps) > 0 {
		probeApp = teamApps[0].ID
	}
	answers, err := p.probeRole(ctx, client, probeApp)
	if err != nil {
		return out, err
	}
	candidates := rolesOf(answers)
	log.InfoContext(ctx, "appstoreconnect_role_probe", "key", k.key.ID, "team", t.id,
		"answers", answersLine(answers), "roles", strings.Join(candidates, ","))
	return p.judgeRole(out, k, answers, candidates), nil
}

// judgeRole ends out at the role step from the roles Apple's answers leave:
// one, several that each fit the level, or none.
func (p *Product) judgeRole(out seataccess.IdentityCheck, k keyCheck, answers []probeAnswer,
	candidates []string) seataccess.IdentityCheck {

	need := out.Needs
	read := seataccess.IdentityRead{Label: identityChecks[stepRole].Label, Detail: answersLine(answers)}
	names := make([]words.Said, 0, len(candidates))
	for _, c := range candidates {
		names = append(names, words.Said(roleName(c)))
	}
	fitsAll, broader := len(candidates) > 0, false
	for _, c := range candidates {
		fits, wider := roleFit(c, k.level)
		// A SALES KEY NEEDS THE VENDOR NUMBER, so its role must be known, not one of several.
		fitsAll = fitsAll && fits && (len(candidates) == 1 || c != RoleSales)
		broader = broader || wider
	}
	switch {
	case len(candidates) == 0:
		var differs []words.Said
		for _, role := range probeRoles {
			differs = append(differs, words.Said(roleName(role)+" on "+words.And(contradicted(role, answers))))
		}
		read.Said = "Apple's answers fit no role."
		out.Reads = append(out.Reads, read)
		out.Status = seataccess.IdentityWrongRole
		out.Said = "Apple's answers about key " + k.key.ID + " fit no role: " +
			words.And(differs) + " disagree."
		out.Remedy = "Check the key's Access in Users and Access, Integrations, Team Keys; it needs the " + need +
			" role."
		return out
	case len(candidates) == 1 && candidates[0] == RoleAdmin:
		out.Role = roleName(RoleAdmin)
		read.Said = out.Role + "."
		out.Reads = append(out.Reads, read)
		out.Status = seataccess.IdentityWrongRole
		out.Said = "Key " + k.key.ID + " holds the Admin role, which Crewlet never gives an agent: it reaches " +
			"the team's users, keys and agreements."
		out.Remedy = "Make a key with the " + need + " role; Apple cannot change a key's role."
		return out
	case !fitsAll && len(candidates) == 1:
		out.Role = roleName(candidates[0])
		read.Said = out.Role + "."
		out.Reads = append(out.Reads, read)
		out.Status = seataccess.IdentityWrongRole
		out.Said = "Key " + k.key.ID + " holds the " + out.Role + " role, and this agent's level needs " + need + "."
		out.Remedy = "Make a key with the " + need + " role; Apple cannot change a key's role."
		return out
	case !fitsAll:
		read.Said = "One of " + words.Or(names) + "."
		out.Reads = append(out.Reads, read)
		out.Status = seataccess.IdentityWrongRole
		out.Said = "Apple's answers leave key " + k.key.ID + "'s role as one of " + words.Or(names) +
			", and not every one of them fits this agent's level."
		out.Remedy = "Check the key's Access in Users and Access, Integrations, Team Keys; if it is not " + need +
			", make a key with the " + need + " role."
		return out
	}
	read.OK = true
	out.Role = words.Or(names)
	read.Said = out.Role + "."
	if len(candidates) > 1 {
		read.Said = "One of " + out.Role + "."
	}
	out.Reads = append(out.Reads, read)
	out.Status = seataccess.IdentityReady
	switch {
	case broader:
		out.Broader = true
		out.Said = "Key " + k.key.ID + " holds the " + out.Role + " role, which reaches more than " + need +
			"; Crewlet still holds every call to this agent's level."
	case len(candidates) > 1:
		out.Said = "Key " + k.key.ID + " holds one of " + out.Role + ", which Apple's answers do not tell apart; " +
			"each fits this agent's level."
	default:
		out.Said = "Key " + k.key.ID + " works in the " + out.Team + " team at the " + out.Role + " role."
	}
	return out
}

// appLabels names each app by the name any team's held list gives it, its
// Apple ID beside: "Field Notes (6763222842)"; the ID alone where none does.
func (p *Product) appLabels(ids []string) []string {
	names := map[string]string{}
	for _, t := range p.heldTeams() {
		t.mu.Lock()
		for _, a := range t.apps {
			if a.Name != "" {
				names[a.ID] = a.Name
			}
		}
		t.mu.Unlock()
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if name := names[id]; name != "" {
			out = append(out, name+" ("+id+")")
			continue
		}
		out = append(out, id)
	}
	return out
}

// probeRole reads the five probes as the key and answers what Apple said to
// each. Only Apple's role denial (403 FORBIDDEN_ERROR) is "may not"; another
// 403 (a feature or program the team lacks), a 404 or a 409 says nothing of
// the role, and any other answer is a read the role may make.
func (p *Product) probeRole(ctx context.Context, c *Client, app string) ([]probeAnswer, error) {
	answers := make([]probeAnswer, 0, len(probeOps))
	for _, id := range probeOps {
		op, _ := Catalog().Operation(id)
		path := op.Path
		if strings.Contains(path, "{id}") {
			if app == "" {
				answers = append(answers, probeAnswer{op: id, said: "not read: the team has no app"})
				continue
			}
			path = strings.Replace(path, "{id}", url.PathEscape(app), 1)
		}
		_, err := c.Do(ctx, ownSlot, Request{Method: "GET", Path: path, Query: url.Values{"limit": {"1"}},
			Read: true, Reserved: true})
		a := probeAnswer{op: id, said: "200"}
		apiErr, answered := errors.AsType[*APIError](err)
		switch {
		case err == nil:
			a.known, a.may = true, true
		case !answered || apiErr.Status == http.StatusUnauthorized || apiErr.VendorUnavailable():
			return answers, fmt.Errorf("appstoreconnect: read the key's role (%s): %w", id, err)
		case apiErr.RoleDenied():
			a.known = true
		case apiErr.Status == http.StatusForbidden || apiErr.Status == http.StatusNotFound ||
			apiErr.Status == http.StatusConflict:
		default:
			a.known, a.may = true, true
		}
		if answered {
			a.said = strings.TrimSpace(fmt.Sprintf("%d %s", apiErr.Status, apiErr.Code))
		}
		answers = append(answers, a)
	}
	return answers, nil
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
	c := NewClient(p.deps.Base, t.issuer, map[string]Key{ownSlot: key}, p.deps.HTTP)
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

// vendorDigits is a team's vendor number.
var vendorDigits = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9]{1,20}$`) })

// keepKey checks a key a person handed in for a seat and answers what to keep.
func (p *Product) keepKey(ctx context.Context, in seataccess.IdentityInput) (seataccess.IdentityKept, error) {
	key, err := handedKey(in)
	if err != nil {
		return seataccess.IdentityKept{}, err
	}
	if taken := p.keyTaken(in, key.ID); taken != nil {
		return seataccess.IdentityKept{}, taken
	}
	vendor := strings.TrimSpace(in.Detail)
	if vendor != "" && !vendorDigits().MatchString(vendor) {
		return seataccess.IdentityKept{}, &seataccess.Refusal{Said: "The vendor number is the team's digits.",
			Remedy: "Copy it from Payments and Financial Reports, or leave it empty for a key that is not Sales."}
	}
	company := p.deps.Company()
	check, err := p.checkKey(ctx, keyCheck{key: key, level: growth.LevelOf(in.Want.Level), apps: in.Want.Scope})
	if err != nil {
		return seataccess.IdentityKept{}, err
	}
	if check.Status == seataccess.IdentityReady && roleID(check.Role) == RoleSales {
		if vendor == "" {
			// A SALES KEY READS SALES, which Apple files under the vendor number.
			check.Status = seataccess.IdentityRefused
			check.Said = "Key " + key.ID + " holds the Sales role, which reads sales, and sales need the team's " +
				"vendor number."
			check.Remedy = "Hand the key in again with the vendor number, on Payments and Financial Reports."
		}
	} else {
		vendor = ""
	}
	kept := seataccess.IdentityKept{Check: check, Way: seataccess.WayHandedIn, ID: key.ID, Detail: vendor,
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

// keyTaken refuses a key another seat keeps.
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
	access := []vendortext.Part{vendortext.UI("Access"), vendortext.W(": "), vendortext.UI(role)}
	switch role {
	case roleName(RoleMarketing):
		access = append(access, vendortext.W(", or "), vendortext.UI("Sales"), vendortext.W(" for an agent that reads sales"))
	case roleName(RoleAppManager):
		access = append(access, vendortext.W(", or "), vendortext.UI("Customer Support"),
			vendortext.W(" for an agent that answers reviews"))
	}
	access = append(access, vendortext.W(". Never "), vendortext.UI("Admin"), vendortext.W("."))
	return seataccess.IdentityGuide{
		Vendor: "Apple", Noun: "key", Records: "its submissions",
		Why:  "Apple then records its submissions under its own key, and you can revoke it alone.",
		Name: name, NameRule: seatname.AppStoreConnect.Words,
		NameWhat: seatname.AppStoreConnect.What, Role: role, Never: "Admin",
		Steps: []seataccess.GuideStep{
			{Text: vendortext.Of(vendortext.W("Open "),
				vendortext.UI("Users and Access", "Integrations", "Team Keys").At(TeamKeysURL), vendortext.W("."))},
			{Text: vendortext.Of(vendortext.W("Press "), vendortext.UI("Generate API Key"), vendortext.W(", or "),
				vendortext.UI("+"), vendortext.W(" if the team already has keys."))},
			{Text: vendortext.Of(vendortext.UI("Name"), vendortext.W(":")), Copy: &seataccess.GuideCopy{Label: "key's name", Value: name,
				Rule: seatname.AppStoreConnect.Words}},
			{Text: vendortext.Of(access...)},
			{Text: vendortext.Of(vendortext.W("Press "), vendortext.UI("Generate"), vendortext.W("."))},
			{Text: vendortext.Of(vendortext.W("On the new key's row, press "), vendortext.UI("Download API Key"),
				vendortext.W(". Apple offers the file once."))},
		},
		Note: vendortext.Plain("Only the Account Holder or an Admin can make team keys. A key reaches every app of the team; " +
			"Crewlet keeps the agent to its own."),
		Back: seataccess.GuideBack{Kind: seataccess.BackFile, Accept: ".p8",
			Hint: vendortext.Plain("The file Apple offered: `AuthKey_<Key ID>.p8`"), Label: "Key ID",
			Detail: &seataccess.GuideDetail{Label: "Vendor number",
				Hint: "Only for a Sales key: digits, on Payments and Financial Reports."}},
		Checks: identityChecks,
	}
}
