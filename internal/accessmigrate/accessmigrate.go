// Package accessmigrate moves a company document written before per-seat
// integration access onto it, integration by integration, and marks each one
// it moved, so the access pass for an integration acts only once its seats say
// what they are.
//
// # What the old document said, read as it said it
//
// Before per-seat access an agent was "on" at a vendor when its seat named a
// `${VAR}` where that vendor's pass looked for a credential (a key in the
// seat's `mcp_env`, a seat block's token), and what it could do was one
// company-wide setting (Datadog's `provisioning.role`, GitLab's
// `access_level`) or a GitHub tier. Those rules are FROZEN HERE, spelled as the
// engine that wrote the document read them, because a migration describes a
// past format: the vendor packages move on, and this must keep reading what
// they used to.
//
// # What it writes, per integration
//
//   - `enabled` on every agent seat's block: true where the old rules put the
//     seat in the vendor's plan, false everywhere else (D9).
//   - The company role copied into each enabled seat's access, in the vendor's
//     own terms: a Datadog role id looked up live by the old role's name (D7),
//     a GitLab access level, GitHub permissions from the tier's own table,
//     a Mattermost bot's channels written out, since an empty list now means
//     none, and Jira and Confluence as "Product access only" (the empty
//     level), which is all the old engine granted: no role is invented.
//   - Where the engine provisions the integration, each seat's credential
//     re-sealed under the name the engine derives for it
//     ([seataccess.SinkName]). Where the old variable is also named by a
//     company block or another seat (the incident that started this work),
//     nothing is moved: the value may be the company's, the seat gets a fresh
//     credential from its next sync, and the seat's ledger record names the
//     variable, which the Agents tab warns about until it is replaced (D12).
//     Where the engine does not provision the integration
//     ([config.Company.SeatCredentialMode]), a credential exclusive to the seat was handed
//     in from outside Crewlet and stays exactly where it is.
//   - The retired company fields removed, so no later write carries them back
//     ([config.CarryUnknown] keeps what a build cannot represent).
//   - The integration added to the document's [MarkerKey] list, which is the
//     gate the access pass reads.
//
// AN INTEGRATION THAT CANNOT BE MIGRATED STAYS GATED. A vendor that does not
// answer (Datadog's role list, an account read) leaves that integration out of
// this revision and in [Migration.Pending], with the reason the pass reports;
// every other integration moves on. Nothing is ever guessed: no default role,
// no account assumed absent.
//
// The plan is pure ([Plan]); reading the vendors, moving a value, writing the
// revision and seeding the ledger are [Run]'s, in that order.
package accessmigrate

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// MarkerKey is the company document's top-level list of the integrations whose
// seats this package has migrated, which the access pass for an integration
// reads before it acts on any seat: an integration missing from it is gated.
const MarkerKey = "seat_access_migrated"

// Move is one seat credential re-sealed under the engine's own name.
type Move struct {
	Handle string
	Kind   integration.Kind
	// Field is the credential's field ([seataccess.Catalog.Credentials]).
	Field string
	// From is the variable the seat named, To the engine's name for it.
	From, To string
}

// Notice is a company credential that may hold a seat's value (D12).
type Notice struct {
	Kind integration.Kind
	// Variable is the company credential, by name; Seats the seats that
	// named it as their own.
	Variable string
	Seats    []string
}

// Seed is an account the engine made before the ledger existed, recorded in it
// by the migration: found at the vendor by the engine's own naming, or, for a
// GitHub App, named by the seat's own block.
type Seed struct {
	Kind    integration.Kind
	Handle  string
	Name    string
	Account seataccess.Account
	// Lanes are what the vendor showed for the seat's lanes, where it was
	// read.
	Lanes map[seataccess.Lane]seataccess.Observed
}

// Facts are what the vendors answered for the integrations a migration reads:
// the old company roles' ids, and the accounts the engine made before the
// ledger, by seat. An integration in Unavailable is left gated with that
// reason.
type Facts struct {
	// Levels looks an old company role up at its vendor by name.
	Levels ResolveLevel

	// Accounts is each provisioned integration's accounts as its vendor
	// shows them, by handle and lane.
	Accounts map[integration.Kind]seataccess.Observation

	// Unavailable is each integration whose vendor could not be read, with
	// why.
	Unavailable map[integration.Kind]error

	// Managed is a deployment whose company document only named systems
	// write ([config.Company.SeatCredentialMode]).
	Managed bool
}

// ResolveLevel turns a level the old document named into the vendor's
// identifier for it: a Datadog role's name into its role id. ok is false where
// the vendor lists no such level; an error is a vendor that could not answer.
type ResolveLevel func(lane seataccess.Lane, name string) (id string, ok bool, err error)

// ErrLevelUnknown reports an old company role the vendor does not list, which
// leaves nothing to copy into the seats and is a person's to decide.
var ErrLevelUnknown = errors.New("accessmigrate: the vendor lists no such role")

// levelUnknown is [ErrLevelUnknown] for one role, worded for a person: the
// access pass reports it as why the integration waits.
type levelUnknown struct{ vendor, name string }

func (e *levelUnknown) Error() string {
	return fmt.Sprintf("%s lists no role named %q, the role the company gave its agents", e.vendor,
		e.name)
}

func (e *levelUnknown) Is(target error) bool { return target == ErrLevelUnknown }

// Migration is what one company document needs.
type Migration struct {
	// Migrated are the integrations this migration writes and marks.
	Migrated []integration.Kind

	// Pending are the integrations it could not migrate, with why: each
	// stays gated, and the next run tries again.
	Pending map[integration.Kind]error

	// Seats is each agent seat's lanes as the migration writes them, by
	// handle. A lane whose block already states `enabled` is not here: it
	// was written by a person or by an earlier migration, and is kept.
	Seats map[string]map[seataccess.Lane]seataccess.Desired

	// Moves are the credentials to re-seal before the revision is written,
	// and Notices the company credentials a seat shared.
	Moves   []Move
	Notices []Notice

	// Kept are the credentials a seat already names under the engine's own
	// name, which move nothing and are recorded as the seat's own where the
	// name holds a value.
	Kept []Move

	// Seeds are ledger rows for accounts the engine made before the ledger.
	Seeds []Seed

	// positions are the seat credential positions to drop or repoint, as
	// found; written by [Apply].
	positions []position
}

// Empty reports a migration with nothing to write.
func (m Migration) Empty() bool { return len(m.Migrated) == 0 }

// Unmigrated is every integration the company connects that the document's
// [MarkerKey] list does not hold: what the next migration has to do, and what
// the access pass is gated on.
func Unmigrated(root map[string]any) []integration.Kind {
	integrations := object(root["integrations"])
	marked := strings2(root[MarkerKey])
	var out []integration.Kind
	for _, kind := range seataccess.Kinds() {
		if kindConnected(integrations, kind) && !slices.Contains(marked, string(kind)) {
			out = append(out, kind)
		}
	}
	return out
}

// Needed reports a document with an integration left to migrate.
func Needed(root map[string]any) bool { return len(Unmigrated(root)) > 0 }

// Plan reads a stored document (a JSON tree) and the vendors' answers, and says
// what the document needs. It changes nothing.
func Plan(root map[string]any, facts Facts) (Migration, error) {
	m := Migration{Seats: map[string]map[seataccess.Lane]seataccess.Desired{},
		Pending: map[integration.Kind]error{}}
	pending := Unmigrated(root)
	if len(pending) == 0 {
		return m, nil
	}
	company, err := companyOf(root)
	if err != nil {
		return Migration{}, err
	}
	integrations := object(root["integrations"])
	refs := countReferences(root)
	access := readCompanyAccess(integrations, facts.Levels)

	type seatRow struct {
		seat   map[string]any
		handle string
		found  []position
	}
	var seats []seatRow
	var failure error
	eachSeat(root, func(seat, unit map[string]any) {
		if failure != nil || isHuman(seat) {
			return
		}
		handle, err := seatHandle(seat)
		if err != nil {
			failure = err
			return
		}
		seats = append(seats, seatRow{seat: seat, handle: handle,
			found: seatPositions(seat, unit, handle, func(k integration.Kind) bool {
				return kindConnected(integrations, k)
			})})
	})
	if failure != nil {
		return Migration{}, failure
	}

	shared := map[string]*Notice{}
	for _, kind := range pending {
		if err := facts.Unavailable[kind]; err != nil {
			m.Pending[kind] = err
			continue
		}
		provisioned := company.SeatCredentialMode(string(kind), facts.Managed) ==
			config.SeatCredentialEngine
		lanes := map[string]map[seataccess.Lane]seataccess.Desired{}
		var laneErr error
		for _, row := range seats {
			for _, lane := range seataccess.LanesOf(kind) {
				if !connected(integrations, lane) ||
					has(object(object(row.seat["integrations"])[string(lane)]), "enabled") {
					continue
				}
				want, err := access.desired(lane, row.seat, row.handle, row.found,
					facts.Accounts[kind][row.handle][lane], provisioned)
				if err != nil {
					laneErr = err
					break
				}
				if lanes[row.handle] == nil {
					lanes[row.handle] = map[seataccess.Lane]seataccess.Desired{}
				}
				lanes[row.handle][lane] = want
			}
			if laneErr != nil {
				break
			}
		}
		if laneErr != nil {
			m.Pending[kind] = laneErr
			continue
		}
		m.Migrated = append(m.Migrated, kind)
		for handle, ls := range lanes {
			if m.Seats[handle] == nil {
				m.Seats[handle] = map[seataccess.Lane]seataccess.Desired{}
			}
			maps.Copy(m.Seats[handle], ls)
		}
		for _, row := range seats {
			own := countReferences(row.seat)
			for _, p := range row.found {
				if p.kind != kind {
					continue
				}
				// SHARED WHEN ANYTHING OUTSIDE THIS SEAT NAMES IT: a company
				// block, a tool server's header, a unit's shared env or
				// another seat.
				isShared := refs[p.from] > own[p.from]
				if !provisioned {
					// HANDED IN FROM OUTSIDE CREWLET: the engine binds it
					// and never writes it, so it stays where the seat
					// names it, and a shared one is no risk the engine
					// can make worse.
					continue
				}
				m.positions = append(m.positions, p)
				if issuedAtUse(p) {
					// A TOKEN THE ENGINE ISSUES AT USE (GitHub's, from the
					// app's sealed key) is never sealed: its position is
					// dropped or repointed, and its value moves nowhere.
					continue
				}
				to := seataccess.SinkName(p.kind, p.field, row.handle)
				switch {
				case p.from == to:
					kept := Move{Handle: row.handle, Kind: p.kind, Field: p.field,
						From: p.from, To: to}
					if !slices.Contains(m.Kept, kept) {
						m.Kept = append(m.Kept, kept)
					}
				case isShared:
					key := string(p.kind) + "\x00" + p.from
					n := shared[key]
					if n == nil {
						n = &Notice{Kind: p.kind, Variable: p.from}
						shared[key] = n
					}
					if !slices.Contains(n.Seats, row.handle) {
						n.Seats = append(n.Seats, row.handle)
					}
				default:
					move := Move{Handle: row.handle, Kind: p.kind, Field: p.field,
						From: p.from, To: to}
					if !slices.Contains(m.Moves, move) {
						m.Moves = append(m.Moves, move)
					}
				}
			}
			// ONLY A SEAT THE OLD PLAN INCLUDED, or one a person has
			// since turned on: an account found by the engine's naming
			// for any other seat is not one this engine made for it.
			if provisioned && onBefore(kind, row.seat, row.found) {
				if seed, ok := seedOf(kind, row.seat, row.handle, facts.Accounts[kind]); ok {
					m.Seeds = append(m.Seeds, seed)
				}
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(shared)) {
		m.Notices = append(m.Notices, *shared[key])
	}
	return m, nil
}

// Apply writes m into the stored document, in place, and reports whether it
// changed anything. Running it again on its own result changes nothing.
func Apply(root map[string]any, m Migration) (bool, error) {
	if len(m.Migrated) == 0 {
		return false, nil
	}
	integrations := object(root["integrations"])
	if slices.Contains(m.Migrated, integration.KindDatadog) {
		dropKey(object(object(integrations["datadog"])["provisioning"]), "role")
	}
	if slices.Contains(m.Migrated, integration.KindGitLab) {
		gitlabProv := object(object(integrations["gitlab"])["provisioning"])
		for _, key := range []string{"access_level", "access_levels"} {
			dropKey(gitlabProv, key)
		}
	}
	byHandle := map[string][]position{}
	for _, p := range m.positions {
		byHandle[p.handle] = append(byHandle[p.handle], p)
	}
	var failure error
	eachSeat(root, func(seat, unit map[string]any) {
		if failure != nil || isHuman(seat) {
			return
		}
		handle, err := seatHandle(seat)
		if err != nil {
			failure = err
			return
		}
		for _, p := range byHandle[handle] {
			p.apply(seat, unit)
		}
		lanes := m.Seats[handle]
		if len(lanes) == 0 {
			return
		}
		role, err := decodeSeat(seat)
		if err != nil {
			failure = err
			return
		}
		for _, lane := range slices.Sorted(maps.Keys(lanes)) {
			if err := seataccess.SetDesired(role, lane, lanes[lane]); err != nil {
				failure = fmt.Errorf("accessmigrate: seat %s: %w", handle, err)
				return
			}
		}
		if err := writeSeatIntegrations(seat, role, lanes); err != nil {
			failure = err
			return
		}
		if _, github := lanes[seataccess.LaneGitHub]; github {
			// THE GITHUB TIER IS RETIRED once the seat's permissions say it.
			gh := object(object(seat["integrations"])["github"])
			for _, key := range []string{"tier", "workflows"} {
				dropKey(gh, key)
			}
		}
	})
	if failure != nil {
		return false, failure
	}
	marked := strings2(root[MarkerKey])
	for _, kind := range m.Migrated {
		if !slices.Contains(marked, string(kind)) {
			marked = append(marked, string(kind))
		}
	}
	slices.Sort(marked)
	list := make([]any, 0, len(marked))
	for _, kind := range marked {
		list = append(list, kind)
	}
	root[MarkerKey] = list
	return true, nil
}

// companyAccess is what each lane's company block said every seat could do.
type companyAccess struct {
	// datadogName is the company's Datadog role by name, looked up at
	// Datadog for its id the first time a seat that was on there needs it
	// ([companyAccess.datadogRole]), so a company whose agents held no
	// Datadog account is never made to reach Datadog to migrate.
	datadogName string
	resolve     ResolveLevel
	datadogID   string
	// gitlabDefault and gitlabBySeat are GitLab's access levels, as the
	// integers the document now stores.
	gitlabDefault int
	gitlabBySeat  map[string]int
	// mattermostChannels are the channels every bot was added to.
	mattermostChannels []string
}

// readCompanyAccess reads the retired company fields.
func readCompanyAccess(integrations map[string]any, resolve ResolveLevel) *companyAccess {
	c := &companyAccess{resolve: resolve}
	if dd := object(integrations["datadog"]); dd != nil {
		c.datadogName = text(object(dd["provisioning"])["role"])
		if c.datadogName == "" {
			// THE OLD ENGINE'S OWN DEFAULT, which every account it made
			// without a role holds.
			c.datadogName = datadogDefaultRole
		}
	}
	if gl := object(object(integrations["gitlab"])["provisioning"]); gl != nil {
		c.gitlabDefault = gitlabLevel(text(gl["access_level"]))
		c.gitlabBySeat = map[string]int{}
		for handle, level := range object(gl["access_levels"]) {
			c.gitlabBySeat[handle] = gitlabLevel(text(level))
		}
	}
	if mm := object(object(integrations["mattermost"])["provisioning"]); mm != nil {
		c.mattermostChannels = strings2(mm["channels"])
	}
	return c
}

// datadogRole is the company role's id at Datadog, looked up once.
func (c *companyAccess) datadogRole() (string, error) {
	if c.datadogID != "" {
		return c.datadogID, nil
	}
	if c.resolve == nil {
		return "", fmt.Errorf("accessmigrate: no way to look up the Datadog role %q",
			c.datadogName)
	}
	id, ok, err := c.resolve(seataccess.LaneDatadog, c.datadogName)
	switch {
	case err != nil:
		// THE REASON IS A PERSON'S TO READ: the access pass reports it
		// as why the integration waits.
		return "", fmt.Errorf("the lookup of the Datadog role %q failed: %w", c.datadogName, err)
	case !ok:
		return "", &levelUnknown{vendor: "Datadog", name: c.datadogName}
	}
	c.datadogID = id
	return id, nil
}

// desired is one seat's lane as the old document left it. have is what the
// vendor shows for the seat's lane, where it was read. A lane the engine does
// not provision says only whether the seat is on: its access was set outside
// Crewlet, and the engine never changes it.
func (c *companyAccess) desired(lane seataccess.Lane, seat map[string]any, handle string,
	found []position, have seataccess.Observed, provisioned bool) (seataccess.Desired, error) {

	block := object(object(seat["integrations"])[string(lane)])
	d := seataccess.Desired{Enabled: wasOn(lane, block, found)}
	if !d.Enabled || !provisioned {
		return d, nil
	}
	// WHAT THE ACCOUNT HOLDS AT THE VENDOR, where it exists and says (D14):
	// an agent somebody gave more than the company role at the vendor keeps
	// it, rather than being moved to the company role by a migration. The
	// company role is only for a seat with no account yet.
	observed, held, err := observedAccess(lane, have, text(seat["name"]))
	if err != nil {
		return seataccess.Desired{}, err
	}
	switch lane {
	case seataccess.LaneDatadog:
		if held {
			d.Level = observed.Level
			break
		}
		id, err := c.datadogRole()
		if err != nil {
			return seataccess.Desired{}, err
		}
		d.Level = id
	case seataccess.LaneGitLab:
		if held {
			d.Level = observed.Level
			break
		}
		level := c.gitlabDefault
		if own, ok := c.gitlabBySeat[handle]; ok {
			level = own
		}
		d.Level = strconv.Itoa(level)
	case seataccess.LaneGitHub:
		d.Permissions = githubPermissions(text(block["tier"]), truthy(block["workflows"]))
		if held {
			d.Permissions = observed.Permissions
		}
		d.Scope = strings2(block["repos"])
	case seataccess.LaneJira, seataccess.LaneConfluence:
		// PRODUCT ACCESS ONLY (the empty level) unless the account holds a
		// role there already, which is all the old engine granted.
		if held {
			d.Level = observed.Level
		}
	case seataccess.LaneMattermost:
		// EVERY BOT THE OLD ENGINE MADE WAS A MEMBER (`system_user`), the one
		// role it ever gave one: written out, since a bot that is on states
		// its roles. A bot somebody gave more keeps what it holds.
		d.Level = config.MattermostSystemUser
		if held {
			d.Level = observed.Level
		}
		// AN EMPTY LIST IS NOW NO CHANNELS, so the channels the old plan put
		// the bot in are written out: the company's list and the seat's own,
		// or, where the plan named none, the ones the bot is in today.
		channels := slices.Clone(c.mattermostChannels)
		if own := text(block["channel"]); own != "" {
			channels = append(channels, own)
		}
		if len(channels) == 0 {
			channels = slices.Clone(have.Scope)
		}
		d.Scope = channels
	}
	return d.Normalized(), nil
}

// observedAccess is what one seat's existing account holds at the vendor, in
// the terms its document setting takes, and whether there is one to keep: an
// account the vendor shows, with a level (or GitHub permissions) it reports.
//
// A DATADOG ACCOUNT HOLDING SEVERAL ROLES IS NOT GUESSED AT
// ([seataccess.ErrSeveralRoles]): a seat names one role, and choosing which to
// keep would take the others away, so the integration waits, naming the agent,
// until a person leaves the account one role or saves the one it holds.
func observedAccess(lane seataccess.Lane, have seataccess.Observed,
	name string) (seataccess.Desired, bool, error) {

	if !have.Exists {
		return seataccess.Desired{}, false, nil
	}
	if lane == seataccess.LaneGitHub {
		return have.Access(), len(have.Permissions) > 0, nil
	}
	level := strings.TrimSpace(have.Level)
	if lane == seataccess.LaneDatadog && len(strings.Fields(level)) > 1 {
		return seataccess.Desired{}, false, fmt.Errorf("%s: %w", cmpOr(name, have.Account),
			seataccess.ErrSeveralRoles)
	}
	return seataccess.Desired{Level: level}, level != "", nil
}

// issuedAtUse reports a position naming a token the engine issues when a tool
// uses it rather than one it seals: GitHub's installation token, which the
// engine mints from the app's key for each use and which expires within the
// hour, so an old value is worth nothing and is never a seat's credential.
func issuedAtUse(p position) bool {
	return p.kind == integration.KindGitHub && p.field == "APP_TOKEN"
}

// onBefore reports a seat that was on at kind before the migration: in the
// old plan by its rules, or turned on since by a block that says so.
func onBefore(kind integration.Kind, seat map[string]any, found []position) bool {
	return slices.ContainsFunc(seataccess.LanesOf(kind), func(lane seataccess.Lane) bool {
		block := object(object(seat["integrations"])[string(lane)])
		if has(block, "enabled") {
			return truthy(block["enabled"])
		}
		return wasOn(lane, block, found)
	})
}

func cmpOr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// wasOn is the old rule for whether a seat was in a vendor's plan: its own
// credential named where that vendor's pass looked (a key in `mcp_env` for
// Datadog, GitLab and Atlassian; the seat block's token for Mattermost and
// Slack), or, for GitHub, a seat block at all, which is a seat that opted in.
func wasOn(lane seataccess.Lane, block map[string]any, found []position) bool {
	switch lane {
	case seataccess.LaneGitHub:
		return len(block) > 0
	case seataccess.LaneSlack:
		return text(block["bot_token"]) != ""
	case seataccess.LaneMattermost:
		_, whole := envref.Whole(text(block["bot_token"]))
		return whole
	}
	return slices.ContainsFunc(found, func(p position) bool {
		return p.kind == lane.Kind() && p.field == primaryField[p.kind]
	})
}

// datadogDefaultRole is the role the engine before per-seat access gave every
// Datadog account whose company named none.
const datadogDefaultRole = "Datadog Read Only Role"

// gitlabLevel is an old GitLab level as the integer GitLab's API takes: the old
// engine knew two, and gave Developer to anything that was not Maintainer.
func gitlabLevel(old string) int {
	if strings.EqualFold(strings.TrimSpace(old), "maintainer") {
		return config.GitLabRoleMaintainer
	}
	return config.GitLabRoleDeveloper
}

// githubPermissions is a GitHub tier as the permissions it granted
// ([github.TierPermissions], the tier's own table, kept to what a seat's app
// may hold).
func githubPermissions(tier string, workflows bool) seataccess.Permissions {
	held, _ := github.TierPermissions(tier, workflows)
	out := seataccess.Permissions{}
	for key, level := range held {
		out[key] = level
	}
	return out
}

// seedOf is a seat's pre-ledger account at kind as a ledger row: what the
// vendor shows by the engine's naming, or, for a GitHub App the vendor was not
// read for, the app the seat's own block names.
func seedOf(kind integration.Kind, seat map[string]any, handle string,
	accounts seataccess.Observation) (Seed, bool) {

	seen := accounts[handle]
	for _, lane := range seataccess.LanesOf(kind) {
		have := seen[lane]
		if !have.Exists || have.AccountID == "" {
			continue
		}
		state := seataccess.AccountActive
		if !have.Enabled && have.DisabledBy == seataccess.DisabledByCrewlet {
			state = seataccess.AccountDisabled
		}
		return Seed{Kind: kind, Handle: handle, Name: text(seat["name"]), Lanes: seen,
			Account: seataccess.Account{ID: have.AccountID, Name: have.Account,
				CreatedAt: have.CreatedAt, CreatedByPass: "migration", State: state}}, true
	}
	if kind != integration.KindGitHub {
		return Seed{}, false
	}
	block := object(object(seat["integrations"])["github"])
	id := text(block["app_id"])
	if id == "" || id == "0" {
		return Seed{}, false
	}
	return Seed{Kind: kind, Handle: handle, Name: text(seat["name"]),
		Account: seataccess.Account{ID: id, Name: text(block["app_slug"]),
			State: seataccess.AccountActive, CreatedByPass: "migration"}}, true
}

// connected reports whether the company connects the integration a lane
// grants, which is the only place a seat's lane means anything.
func connected(integrations map[string]any, lane seataccess.Lane) bool {
	switch lane {
	case seataccess.LaneJira:
		return has(integrations, "jira")
	case seataccess.LaneConfluence:
		return has(integrations, "confluence")
	}
	return has(integrations, string(lane))
}

// kindConnected reports whether the company connects any lane of kind.
func kindConnected(integrations map[string]any, kind integration.Kind) bool {
	return slices.ContainsFunc(seataccess.LanesOf(kind), func(l seataccess.Lane) bool {
		return connected(integrations, l)
	})
}

// countReferences is how many times each variable is named anywhere in v.
func countReferences(v any) map[string]int {
	out := map[string]int{}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for _, child := range t {
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		case string:
			for _, name := range envref.Names(t) {
				out[name]++
			}
		}
	}
	walk(v)
	return out
}

// eachSeat calls fn on every seat of the stored document, the company's own and
// every unit's at any depth, with the unit it sits in directly (nil at the
// root), whose shared `mcp_env` its agents inherit.
func eachSeat(root map[string]any, fn func(seat, unit map[string]any)) {
	for _, seat := range objects(root["roles"]) {
		fn(seat, nil)
	}
	var visit func(map[string]any)
	visit = func(unit map[string]any) {
		for _, seat := range objects(unit["roles"]) {
			fn(seat, unit)
		}
		for _, child := range objects(unit["children"]) {
			visit(child)
		}
	}
	for _, unit := range objects(root["units"]) {
		visit(unit)
	}
}

// seatHandle is a stored seat's handle as the config model derives it, so a
// seat stored without one is addressed as every other reader addresses it.
func seatHandle(seat map[string]any) (string, error) {
	role, err := decodeSeat(seat)
	if err != nil {
		return "", err
	}
	handle := role.Seat().Handle()
	if handle == "" {
		return "", fmt.Errorf("accessmigrate: a seat named %q has no handle", role.Name)
	}
	return handle, nil
}

func isHuman(seat map[string]any) bool { return text(seat["kind"]) == "human" }

// The JSON tree's small readers.

func object(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func objects(v any) []map[string]any {
	list, _ := v.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m := object(item); m != nil {
			out = append(out, m)
		}
	}
	return out
}

func has(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}

func dropKey(m map[string]any, key string) { delete(m, key) }

func text(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case fmt.Stringer:
		return strings.TrimSpace(t.String())
	case nil:
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func truthy(v any) bool {
	b, _ := v.(bool)
	return b
}

func strings2(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, item := range list {
		if s := text(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}
