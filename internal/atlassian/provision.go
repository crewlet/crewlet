package atlassian

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// The Atlassian half of provisioning: which seats need an account, which
// products each one is licensed for, and which variables its credential goes
// into.
//
// # There is no network here, deliberately
//
// Everything in this file is a function of the org chart and the config. That
// is what makes `-dry-run` a plain early return in the CLI rather than a
// second implementation that can disagree with the real one about what it was
// going to do.

// SeatPlan is one seat a provisioning run has work to do for.
//
// # It is not [provision.Seat], and the reason is the credential's SHAPE
//
// Every other vendor's credential is one token in one variable, which is
// exactly what provision.Seat carries. An Atlassian Cloud credential is a
// PAIR — the token and the address it authenticates as, because Cloud
// authenticates Basic base64(email:token) and refuses the same token as a
// bearer — and each half can be named more than once: the documented mcp_env
// shape declares JIRA_USERNAME and JIRA_API_TOKEN beside CONFLUENCE_USERNAME
// and CONFLUENCE_API_TOKEN, four variables served by one account. One minted
// value therefore lands in several variables, which a single TokenVar cannot
// express at all.
type SeatPlan struct {
	// Handle is the seat, for the report.
	Handle string
	// Role is the seat's role name, which is what the service account is
	// named after.
	Role string
	// Goal is the seat's own mission, which becomes the account's
	// description in admin.atlassian.com — so an administrator reading the
	// account list can tell what it is for.
	Goal string
	// TokenVars are every variable this seat's token is written into, in a
	// stable order.
	TokenVars []string
	// EmailVars are every variable the account's assigned address is
	// written into.
	EmailVars []string
	// Products are the products this seat holds a licence for, ordered by
	// [Products].
	Products []Product
}

// Provisionable reports a seat this run can actually finish.
//
// BOTH halves, because either alone leaves a seat that cannot authenticate:
// an address with no token has nothing to send, and a token with no address
// is refused by Cloud on the scheme rather than on the value — which reads
// like a bad credential and is not one.
func (s SeatPlan) Provisionable() bool {
	return len(s.TokenVars) > 0 && len(s.EmailVars) > 0 && len(s.Products) > 0
}

// Plan is what a provisioning run intends to do, before it does any of it.
type Plan struct {
	// Seats are the seats to provision, in handle order.
	Seats []SeatPlan

	// Managed is the normalised display name of EVERY agent seat in the
	// org chart, whether or not this run can provision it.
	//
	// It is what -decommission keeps, and it is deliberately wider than
	// Seats: a seat that opted out of every product, or whose credential
	// is managed by hand, is skipped by this run and still HAS an account
	// from an earlier one. Sweeping on Seats would delete it — and an
	// Atlassian service account cannot be restored, so the account that
	// reported an issue would stop existing because somebody edited a
	// products list.
	Managed []string
	// Notes are the drift and the caveats: a seat whose credential is a
	// literal, a seat that opted out. They do not stop the run — they are
	// what the report ends with.
	Notes []string
}

// Add records a seat, keeping the plan ordered by handle.
//
// ORDERED, because the report is read side by side with a previous run's and
// a plan whose order came from a map iteration cannot be compared with
// anything.
func (p *Plan) Add(s SeatPlan) {
	p.Seats = append(p.Seats, s)
	sort.Slice(p.Seats, func(i, j int) bool { return p.Seats[i].Handle < p.Seats[j].Handle })
}

// Note records a caveat.
func (p *Plan) Note(format string, args ...any) {
	p.Notes = append(p.Notes, fmt.Sprintf(format, args...))
}

// Empty reports a plan with nothing to do.
func (p *Plan) Empty() bool { return len(p.Seats) == 0 }

// PlanFor walks the org for seats that need an Atlassian service account.
//
// # A literal is a note, not a failure
//
// A seat whose credential is written out rather than referenced is one an
// operator manages by hand. That is a supported choice — it just cannot be
// provisioned, because there is no variable to mint into and rewriting the
// company config from a provisioning run is not this command's job. So it is
// reported and skipped, and the run continues for the seats that can be.
//
// configured is the products the COMPANY has, which is what a seat naming
// none is licensed for.
func PlanFor(o *org.Organization, cfg *config.Atlassian, configured []Product) (*Plan, error) {
	if cfg == nil {
		return nil, fmt.Errorf(
			"atlassian: the company config has no integrations.atlassian " +
				"block, so there is no organization to provision service " +
				"accounts in")
	}
	if o == nil {
		return nil, fmt.Errorf("atlassian: no organization")
	}
	if len(configured) == 0 {
		return nil, fmt.Errorf(
			"atlassian: the company configures neither integrations.jira nor " +
				"integrations.confluence, so there is no product to license a " +
				"seat for")
	}

	plan := &Plan{}
	// Which seat claimed each variable, so a variable two seats share is
	// caught HERE rather than after one of them has been minted over. That
	// is the "one seat, two identities" failure, and at plan time it costs
	// nothing to catch.
	claimed := map[string]string{}
	// Which seat each account NAME belongs to. The display name is the join
	// a re-run adopts an existing account by, and it is derived from the role
	// name — which the org chart does not make unique. See the refusal below.
	named := map[string]string{}

	for seat := range o.AllRoles() {
		if !seat.IsAgent() {
			// A human seat holds their own Atlassian account and is
			// addressable through contact.atlassian_account_id. Creating
			// one would be a billable licence nobody authenticates as.
			continue
		}
		handle := seat.Handle()
		entry := SeatPlan{Handle: handle, Role: seat.Name, Goal: seat.Goal}

		// MANAGED IS RECORDED BEFORE ANY SKIP, and that is the whole
		// safety property of -decommission. Every agent seat in the chart
		// is an account this company would name, whether or not this run
		// can provision it — a seat that opted out of every product, or
		// whose credential is managed by hand, still HAS an account from
		// an earlier run, and sweeping it because the plan no longer
		// carries the seat is an irreversible delete of an identity that
		// owns issues.
		display := DisplayName(cfg.Prefix(), entry)
		plan.Managed = append(plan.Managed, NormalizeName(display))

		products := ProductsFor(seat, configured)
		if len(products) == 0 {
			if seat.AtlassianProducts != nil {
				plan.Note("%s: integrations.atlassian.products is empty, so "+
					"this seat was skipped — it holds no Atlassian licence "+
					"and receives no Jira or Confluence events", handle)
			}
			continue
		}
		entry.Products = products

		blocks := credentialBlocks(seat)
		if len(blocks) == 0 {
			// Not a note: most companies have seats that do no tracker
			// work at all, and a line per one of them would bury the
			// seats an operator has to act on.
			continue
		}
		tokens, emails, notes, ok := captureVars(handle, blocks)
		plan.Notes = append(plan.Notes, notes...)
		if !ok || len(tokens) == 0 {
			continue
		}
		if len(emails) == 0 {
			plan.Note("%s: its mcp_env block names a token variable and no "+
				"address variable (%s), so a minted credential could not be "+
				"used — Atlassian Cloud authenticates an API token as Basic "+
				"email:token and refuses the same token as a bearer",
				handle, strings.Join(EmailKeys, " or "))
			continue
		}
		entry.TokenVars, entry.EmailVars = tokens, emails
		for _, name := range append(append([]string(nil), tokens...), emails...) {
			if owner, taken := claimed[name]; taken && owner != handle {
				return nil, fmt.Errorf(
					"atlassian: %s and %s both point at ${%s}, so provisioning "+
						"would give one account two seats' identities and leave "+
						"the other authenticating as its colleague — give each "+
						"seat its own variables", owner, handle, name)
			}
			claimed[name] = handle
		}
		// TWO SEATS, ONE ACCOUNT NAME — refused for the same reason two
		// seats sharing a ${VAR} is.
		//
		// The org chart enforces unique HANDLES, not unique role names: two
		// roles in different units may be called the same thing and differ
		// only by a declared handle. The display name is built from the role
		// name, and it is the ONLY field both sides control — Atlassian
		// assigns the account's id and its address — so it is what a re-run
		// adopts by. Left alone, this run creates two service accounts called
		// the same thing, and the next run adopts them in whatever order the
		// organization happens to list them: each seat mints over the other's
		// credential, both keep working, and nothing anywhere says which
		// identity is filing which issue.
		if owner, taken := named[NormalizeName(display)]; taken {
			return nil, fmt.Errorf(
				"atlassian: %s and %s would both be provisioned as %q, so a later "+
					"run could not tell their service accounts apart and each seat "+
					"would mint over the other's credential — give one of them a "+
					"distinct role name", owner, handle, display)
		}
		named[NormalizeName(display)] = handle
		plan.Add(entry)
	}
	sort.Strings(plan.Managed)
	return plan, nil
}

// ContainersOf lists the projects or spaces the org chart names for a
// product, sorted and upper-cased.
//
// Units and seats both, because both can name one: a unit says which project
// or space it owns, and a root seat says where it files.
//
// # Confluence also takes the org-wide knowledge scope, and Jira has no
// equivalent
//
// `knowledge.confluence_spaces` is where every seat's Plan-phase search runs.
// A seat that cannot READ those spaces gets an empty knowledge block on every
// turn, and an empty block is indistinguishable from a company that has
// written nothing down — which is precisely the kind of silent failure this
// report exists to end. So the wiki's container set is the union of the
// identity spaces and the read scope. The tracker has no counterpart: there
// is no org-wide Jira read scope to be missing from.
func ContainersOf(o *org.Organization, product Product) []string {
	if o == nil {
		return nil
	}
	seen := map[string]bool{}
	add := func(key string) {
		if key = org.NormalizeScope(key); key != "" {
			seen[key] = true
		}
	}
	for u := range o.AllUnits() {
		add(pick(product, u.JiraProject, u.ConfluenceSpace))
	}
	for seat := range o.AllRoles() {
		add(pick(product, seat.JiraProject, seat.ConfluenceSpace))
	}
	if product == ProductConfluence {
		for _, space := range o.ConfluenceSpaces {
			add(space)
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// pick is the product's own field on a unit or a seat: the two are named
// separately in the org chart because they are two identities, not one
// scope with two spellings.
func pick(product Product, jira, confluence string) string {
	if product == ProductConfluence {
		return confluence
	}
	return jira
}

// ProductsFor is the products one seat holds a licence for.
//
// The seat's own list where it has one, narrowed to what the company actually
// configures — a seat naming Confluence in a company with no Confluence would
// otherwise be licensed into a site that is not there. Absent takes every
// configured product; an explicit empty list takes none, which is the whole
// reason the authored field is a pointer.
func ProductsFor(seat *org.Role, configured []Product) []Product {
	if seat == nil {
		return nil
	}
	if seat.AtlassianProducts == nil {
		return append([]Product(nil), configured...)
	}
	want := make(map[Product]bool, len(seat.AtlassianProducts))
	for _, name := range seat.AtlassianProducts {
		want[Product(strings.ToLower(strings.TrimSpace(name)))] = true
	}
	// ORDERED BY the configured slice, which is itself ordered by
	// [Products]: the result is compared element by element downstream, so
	// it must not inherit the order somebody wrote their YAML in.
	var out []Product
	for _, p := range configured {
		if want[p] {
			out = append(out, p)
		}
	}
	return out
}

// credentialBlocks are every mcp_env block that holds an Atlassian token.
//
// # ALL of them, not the first
//
// [SeatBlock] picks one, which is right for READING a credential: any block
// that holds one authenticates the same account. Provisioning is the other
// direction, and a seat running two single-product servers — a `jira` entry
// beside a `confluence` entry, which is a documented shape — has a variable
// in each. Minting into the first and walking away leaves the second server
// starting with an unset credential: no error, no note, and exactly the
// silent half-provisioned seat this grammar exists to end.
//
// UNRESOLVED, unlike [SeatBlock]: this scan is looking for `${VAR}`
// references to mint INTO, so it must see the reference rather than what the
// reference currently holds. A seat whose variables are all unset — which is
// every seat before its first provisioning run — would otherwise look like a
// seat with no Atlassian credential at all.
func credentialBlocks(seat *org.Role) []map[string]string {
	var out []map[string]string
	for _, name := range SeatEnvs {
		block := seat.MCPEnv[name]
		for _, key := range TokenKeys {
			if strings.TrimSpace(block[key]) != "" {
				out = append(out, block)
				break
			}
		}
	}
	return out
}

// captureVars reads the variables one seat's credential is written into,
// across every block that holds one.
//
// # A key that is not a whole reference makes the whole SEAT unprovisionable
//
// Not just that key. A seat with three referenced keys and one literal would
// otherwise be provisioned into three variables and keep authenticating
// through the fourth — the hardest kind of stale credential to find, because
// everything about the seat looks freshly provisioned while one of its tool
// servers quietly uses a credential nobody rotated.
//
// So ok is false the moment any credential key in any of the seat's blocks is
// a literal, a composite, or an `Authorization: Basic` header, and the caller
// skips the seat with the notes this returns. Managing a seat's credential by
// hand is a supported choice; managing HALF of one is not.
func captureVars(handle string, blocks []map[string]string) (tokens, emails, notes []string, ok bool) {
	ok = true
	add := func(into []string, name string) []string {
		// DEDUPED: two keys pointing at one variable is the ordinary
		// shape, and recording it twice would make a re-run's report claim
		// twice the writes it made.
		for _, existing := range into {
			if existing == name {
				return into
			}
		}
		return append(into, name)
	}
	for _, block := range blocks {
		for _, key := range TokenKeys {
			raw := strings.TrimSpace(block[key])
			if raw == "" {
				continue
			}
			// A `Basic` header is a WHOLE credential an operator manages:
			// its payload is already base64(email:token), so there is no
			// variable under it to mint into and no way to write one
			// without rewriting the config. Named apart from a literal
			// because the fix is different — there is nothing to fix.
			if strings.HasPrefix(raw, "Basic ") {
				notes = append(notes, fmt.Sprintf(
					"%s: its Atlassian credential arrives as an %s: Basic header, "+
						"which already carries the address and the token together — "+
						"there is no ${VAR} under it to mint into, so this seat is "+
						"managed by hand. Point a named token key (%s) at a variable "+
						"to provision it instead", handle, key, TokenKeys[0]))
				ok = false
				continue
			}
			name, isRef := provision.SoleVar(StripScheme(raw))
			if !isRef {
				// THE NOTE NAMES THE SHAPE, NEVER THE VALUE. It is printed
				// in a report an operator pastes into a ticket, and the
				// value here is either a credential or a string containing
				// one.
				notes = append(notes, fmt.Sprintf(
					"%s: its Atlassian credential key %s is %s rather than a whole "+
						"${VAR} reference, so this seat cannot be provisioned — "+
						"minting into its other keys would leave this one holding a "+
						"credential nothing rotates. Point it at a variable, or "+
						"manage this seat's credential by hand",
					handle, key, provision.DescribeShape(StripScheme(raw))))
				ok = false
				continue
			}
			tokens = add(tokens, name)
		}
		for _, key := range EmailKeys {
			raw := strings.TrimSpace(block[key])
			if raw == "" {
				continue
			}
			name, isRef := provision.SoleVar(raw)
			if !isRef {
				notes = append(notes, fmt.Sprintf(
					"%s: its Atlassian address key %s is %s rather than a whole "+
						"${VAR} reference, so the address Atlassian assigns this "+
						"account could not be written back and the seat would go on "+
						"authenticating as whoever that value names — point it at a "+
						"variable", handle, key, provision.DescribeShape(raw)))
				ok = false
				continue
			}
			emails = add(emails, name)
		}
	}
	return tokens, emails, notes, ok
}

// displayNameMaxLen and descriptionMaxLen are Atlassian's own limits on a
// service account. Exceeding either is a 400 half way through a run, so they
// are applied here where the value is built.
const (
	displayNameMaxLen = 100
	descriptionMaxLen = 500
)

// DisplayName is what an operator sees in the assignee picker and @mention
// autocomplete.
//
// It is ALSO the join a re-run adopts an existing account by: Atlassian
// assigns the account's id and address, so the display name is the only field
// both the org chart and the organization control. That is why the prefix can
// never be empty — see [config.Atlassian.Prefix].
func DisplayName(prefix string, seat SeatPlan) string {
	name := strings.TrimSpace(seat.Role)
	if name == "" {
		name = seat.Handle
	}
	return truncate(strings.TrimSpace(prefix)+" "+name, displayNameMaxLen)
}

// Description tells an administrator browsing admin.atlassian.com where the
// account came from, which is the question a list of robot accounts always
// raises.
func Description(seat SeatPlan) string {
	goal := strings.TrimSpace(seat.Goal)
	if goal == "" {
		goal = "the " + seat.Role + " seat"
	}
	return truncate("Managed by Crewlet for "+goal, descriptionMaxLen)
}

// TokenLabel is how Crewlet signs a credential it minted, so a later run can
// recognise its OWN and leave everybody else's alone.
//
// A named function rather than a format string at three call sites, because
// the mint, the retire and the rollback all key on it — and three copies
// would eventually differ, at which point rotation quietly stops retiring
// anything and every run leaves another live credential behind.
func TokenLabel(handle string) string { return "crewlet-" + handle }

// NormalizeName is how two display names are compared for adoption: case and
// surrounding space ignored, so "Crewlet Agent SWE" and "crewlet  agent swe"
// are one account rather than two.
func NormalizeName(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// truncate bounds a value at Atlassian's limit, counting CHARACTERS.
//
// # Bytes were the wrong unit, in both directions
//
// Atlassian's caps are on characters. Slicing bytes cut a 40-character
// Cyrillic role name that was well inside the limit, and cut it MID-RUNE — so
// the display name went on the wire as invalid UTF-8, which is either a 400
// half way through a run or, worse, a name that can never [NormalizeName]-match
// on the next run's adoption, giving that seat a second account every time.
// It also shrank the effective window to ~33 Cyrillic or ~25 CJK characters,
// which is enough to make two genuinely distinct role names collide and abort
// the whole run on the plan's uniqueness refusal.
func truncate(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(string(runes[:limit]))
}
