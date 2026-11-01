package code_test

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
)

// chartUnit is one unit of a test chart: its id and name (a reference matches
// either, folded, an id before a name — the chart's own durable lookup), the
// key of the unit above it, and its own lead ("" inherits the nearest
// ancestor's).
type chartUnit struct{ id, name, parent, lead string }

// chart is a code.Roster over a hand-built organization, keeping the roster's
// contract — every unit argument a REFERENCE, resolved as org.Index.UnitByRef
// resolves one — so a grant that compared references as strings instead of
// asking the roster reads a unit spelled two ways as two units, and fails.
type chart struct {
	units []chartUnit
	seats map[string]code.RosterSeat
}

func (c chart) resolve(ref string) (chartUnit, bool) {
	folded := strings.ToLower(strings.TrimSpace(ref))
	if folded == "" {
		return chartUnit{}, false
	}
	for _, u := range c.units {
		if strings.ToLower(u.id) == folded {
			return u, true
		}
	}
	for _, u := range c.units {
		if strings.ToLower(u.name) == folded {
			return u, true
		}
	}
	return chartUnit{}, false
}

func (c chart) key(u chartUnit) string { return cmpOr(u.id, u.name) }

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// up is the unit and every unit above it, nearest first.
func (c chart) up(ref string) []chartUnit {
	var out []chartUnit
	for u, ok := c.resolve(ref); ok; u, ok = c.resolve(u.parent) {
		out = append(out, u)
	}
	return out
}

func (c chart) Seat(handle string) (code.RosterSeat, bool) {
	s, ok := c.seats[handle]
	return s, ok
}

func (c chart) Lead(ref string) string {
	for _, u := range c.up(ref) {
		if u.lead != "" {
			return u.lead
		}
	}
	return ""
}

func (c chart) Leads(ref string) []string {
	var out []string
	for _, u := range c.up(ref) {
		if lead := c.Lead(c.key(u)); lead != "" && !slices.Contains(out, lead) {
			out = append(out, lead)
		}
	}
	return out
}

func (c chart) Within(unit, ancestor string) bool {
	a, ok := c.resolve(ancestor)
	if !ok {
		return false
	}
	for _, u := range c.up(unit) {
		if c.key(u) == c.key(a) {
			return true
		}
	}
	return false
}

// testChart is three nested units under a fourth, a root seat and a human seat:
//
//	Engineering (id eng, lead cto)
//	  Platform (id plat, lead plat-lead)
//	    Infra (no id, no lead of its own: plat-lead leads it)
//	Sales (id sales, lead vp-sales)
//
// Every seat's Unit is its unit's key in this epoch; the repositories below
// name their owners as they were written, by name where a unit has an id.
func testChart() chart {
	seat := func(handle string, tier code.Tier, unit string) code.RosterSeat {
		return code.RosterSeat{Handle: handle, Tier: tier, Unit: unit}
	}
	return chart{
		units: []chartUnit{
			{id: "eng", name: "Engineering", lead: "cto"},
			{id: "plat", name: "Platform", parent: "eng", lead: "plat-lead"},
			{name: "Infra", parent: "plat"},
			{id: "sales", name: "Sales", lead: "vp-sales"},
		},
		seats: map[string]code.RosterSeat{
			"cto":       seat("cto", code.TierFullAccess, "eng"),
			"plat-lead": seat("plat-lead", code.TierReview, "plat"),
			"dev":       seat("dev", code.TierFullAccess, "Infra"),
			"reader":    seat("reader", code.TierReadOnly, "Infra"),
			"unset":     seat("unset", "", "plat"),
			"vp-sales":  seat("vp-sales", code.TierFullAccess, "sales"),
			"root-bot":  seat("root-bot", code.TierFullAccess, ""),
			"founder":   {Handle: "founder", Human: true, Tier: code.TierFullAccess, Unit: "eng"},
		},
	}
}

func agent(handle string) code.Principal {
	return code.Principal{Kind: code.PrincipalAgent, Seat: handle}
}

// ownedBy is a `visibility: company` repository named name, owned by ref of
// kind.
func ownedBy(name string, kind code.OwnerKind, ref string) code.RepoRef {
	return code.RepoRef{ID: "id-" + name, Name: name, Owner: code.Owner{Kind: kind, Ref: ref},
		Visibility: code.VisibilityCompany}
}

// unitOnly is repo at `visibility: unit`.
func unitOnly(repo code.RepoRef) code.RepoRef {
	repo.Name, repo.ID, repo.Visibility = repo.Name+"-private", repo.ID+"-private", code.VisibilityUnit
	return repo
}

// A GRANT FOLLOWS THE CHART: a seat's relation to a repository is the first of
// §2.3's rows that holds — the owning unit's effective lead maintains, its
// subtree writes, the lead chain above it reads, the owning seat maintains and
// its unit's leads read, any other seat reads a `visibility: company`
// repository and nothing of a `visibility: unit` one — and what it may do is
// the minimum of that and its tier. Each unit is named as the repository wrote
// it, so a grant that compared the owner's reference with a seat's unit as
// strings would lose Platform's members their write (the unit is "Platform"
// on the repository and "plat" on the seat).
//
// Mutations: Within replaced by a string compare of the two references → the
// Platform rows of dev and unset read; a seat's access left at its relation
// (no tier cap) → plat-lead maintains Platform; a human seat granted as a
// seat → founder reads; the last row ignoring the visibility → vp-sales and
// root-bot read the unit repositories.
func TestGrantsFollowTheChart(t *testing.T) {
	t.Parallel()
	r := testChart()
	platform := ownedBy("platform", code.OwnerUnit, "Platform")
	infra := ownedBy("infra-tools", code.OwnerUnit, "infra")
	notes := ownedBy("notes", code.OwnerSeat, "dev")
	orphan := ownedBy("orphan", code.OwnerUnit, "a unit since dissolved")
	rootOwned := ownedBy("bot-notes", code.OwnerSeat, "root-bot")
	for _, tc := range []struct {
		repo             code.RepoRef
		seat             string
		relation, access code.Access
	}{
		// Platform: its effective lead, its subtree, the lead above it.
		{platform, "plat-lead", code.AccessMaintain, code.AccessReview},
		{platform, "dev", code.AccessWrite, code.AccessWrite},
		{platform, "reader", code.AccessWrite, code.AccessRead},
		{platform, "unset", code.AccessWrite, code.AccessRead},
		{platform, "cto", code.AccessRead, code.AccessRead},
		{platform, "vp-sales", code.AccessRead, code.AccessRead},
		{platform, "root-bot", code.AccessRead, code.AccessRead},
		// Infra has no lead of its own: Platform's lead is its effective
		// lead, and the chain above is the same seat once, then the cto.
		{infra, "plat-lead", code.AccessMaintain, code.AccessReview},
		{infra, "dev", code.AccessWrite, code.AccessWrite},
		{infra, "unset", code.AccessRead, code.AccessRead},
		{infra, "cto", code.AccessRead, code.AccessRead},
		// A seat's own repository: the seat maintains, its unit's leads read.
		{notes, "dev", code.AccessMaintain, code.AccessMaintain},
		{notes, "plat-lead", code.AccessRead, code.AccessRead},
		{notes, "cto", code.AccessRead, code.AccessRead},
		{notes, "reader", code.AccessRead, code.AccessRead},
		{rootOwned, "root-bot", code.AccessMaintain, code.AccessMaintain},
		{rootOwned, "cto", code.AccessRead, code.AccessRead},
		// A unit the chart no longer holds has no lead and no members.
		{orphan, "cto", code.AccessRead, code.AccessRead},
		{orphan, "dev", code.AccessRead, code.AccessRead},

		// VISIBILITY: UNIT keeps every row above the last and takes the
		// last away: the owning unit's lead, its subtree and the lead chain
		// above it read as before, and every other seat reads nothing.
		{unitOnly(platform), "plat-lead", code.AccessMaintain, code.AccessReview},
		{unitOnly(platform), "dev", code.AccessWrite, code.AccessWrite},
		{unitOnly(platform), "reader", code.AccessWrite, code.AccessRead},
		{unitOnly(platform), "cto", code.AccessRead, code.AccessRead},
		{unitOnly(platform), "vp-sales", code.AccessNone, code.AccessNone},
		{unitOnly(platform), "root-bot", code.AccessNone, code.AccessNone},
		{unitOnly(infra), "unset", code.AccessNone, code.AccessNone},
		// A seat's own: the seat and the leads above its unit, and not the
		// seats beside it in that unit.
		{unitOnly(notes), "dev", code.AccessMaintain, code.AccessMaintain},
		{unitOnly(notes), "plat-lead", code.AccessRead, code.AccessRead},
		{unitOnly(notes), "cto", code.AccessRead, code.AccessRead},
		{unitOnly(notes), "reader", code.AccessNone, code.AccessNone},
		{unitOnly(rootOwned), "cto", code.AccessNone, code.AccessNone},
		// A unit the chart no longer holds, at unit visibility, is read by
		// no seat at all — persons only.
		{unitOnly(orphan), "cto", code.AccessNone, code.AccessNone},
	} {
		g := code.GrantOf(agent(tc.seat), tc.repo, r)
		if g.Relation != tc.relation || g.Access != tc.access {
			t.Errorf("%s on %s: relation %s, access %s; want %s, %s", tc.seat, tc.repo.Name, g.Relation,
				g.Access, tc.relation, tc.access)
		}
		if got := code.AccessOf(agent(tc.seat), tc.repo, r); got != g.Access {
			t.Errorf("%s on %s: AccessOf %s, GrantOf's access %s", tc.seat, tc.repo.Name, got, g.Access)
		}
		if g.MayRead() != (tc.access >= code.AccessRead) {
			t.Errorf("%s on %s: MayRead %v at access %s", tc.seat, tc.repo.Name, g.MayRead(), g.Access)
		}
		if tc.access == code.AccessNone {
			// TOLD WHY, AND WHO DOES READ IT: the visibility and the owner,
			// so the seat — or a person reading its turn — knows whom to ask.
			refusal := g.ReadRefusal()
			if !strings.Contains(refusal, "visibility: unit") || !strings.Contains(refusal, tc.repo.Owner.Ref) ||
				!strings.Contains(refusal, strconv.Quote(tc.repo.Name)) {
				t.Errorf("%s on %s: read refusal %q, want it to name the visibility, the owner %q and "+
					"the repository", tc.seat, tc.repo.Name, refusal, tc.repo.Owner.Ref)
			}
			if g.MayPush() {
				t.Errorf("%s on %s: a seat that reads nothing may push (its namespace needs a read "+
					"relation)", tc.seat, tc.repo.Name)
			}
		}
	}

	// NOBODY'S STANDING without a seat of the chart behind it: a human seat
	// asked as an agent, a handle the chart lacks, and any seat on a node
	// with no chart. Each is none, and its read refusal says why.
	for _, tc := range []struct {
		g   code.Grant
		why string
	}{
		{code.GrantOf(agent("founder"), platform, r), "human seat"},
		{code.GrantOf(agent("ghost"), platform, r), "no seat of the chart"},
		{code.GrantOf(agent("dev"), platform, nil), "no chart"},
		{code.GrantOf(code.Principal{Kind: "robot", Seat: "dev"}, platform, r), "grants nothing"},
	} {
		if tc.g.Access != code.AccessNone || tc.g.MayRead() || tc.g.MayPush() {
			t.Errorf("%+v holds %s", tc.g.Principal, tc.g.Access)
		}
		if refusal := tc.g.ReadRefusal(); !strings.Contains(refusal, tc.why) ||
			!strings.Contains(refusal, `"platform"`) {
			t.Errorf("%+v: read refusal %q, want it to name the repository and %q", tc.g.Principal, refusal, tc.why)
		}
	}

	// A PERSON'S STANDING READS NO CHART, AND NO VISIBILITY: a bound
	// credential maintains, one bound to nobody reads — with a roster and
	// without one, on a company repository and on a unit one (ADR-0081:
	// visibility narrows seats only).
	for _, roster := range []code.Roster{r, nil} {
		for _, repo := range []code.RepoRef{orphan, unitOnly(orphan), unitOnly(platform)} {
			bound := code.GrantOf(code.Principal{Kind: code.PrincipalOperator, OperatorID: "tok",
				Seat: "founder"}, repo, roster)
			unbound := code.GrantOf(code.Principal{Kind: code.PrincipalOperator, OperatorID: "ci"}, repo, roster)
			if bound.Access != code.AccessMaintain || unbound.Access != code.AccessRead {
				t.Errorf("roster %v, %s: a bound person holds %s and an unbound token %s; want maintain, read",
					roster != nil, repo.Name, bound.Access, unbound.Access)
			}
		}
	}
}

// A VISIBILITY THIS BUILD DOES NOT KNOW NARROWS, NEVER WIDENS: the domain
// records none it does not know, so such a row is a newer build's, and reading
// it as `company` would guess that build meant every seat. The rows above the
// last still hold — the owning unit's members and leads read as before — and
// the seat it shuts out is told the visibility the row carries, never `unit`
// as though the repository said so.
//
// Mutations: the last row reading only `unit` as narrow (any other value read)
// → vp-sales reads; the refusal spelling `unit` → the value is not named.
func TestAVisibilityThisBuildDoesNotKnowNarrows(t *testing.T) {
	t.Parallel()
	r := testChart()
	repo := ownedBy("platform", code.OwnerUnit, "Platform")
	repo.Visibility = "team"
	if g := code.GrantOf(agent("dev"), repo, r); g.Access != code.AccessWrite {
		t.Errorf("dev, in the owning unit, holds %s on a %q repository, want write", g.Access, repo.Visibility)
	}
	g := code.GrantOf(agent("vp-sales"), repo, r)
	if g.MayRead() {
		t.Fatalf("vp-sales reads a %q repository at %s, want nothing", repo.Visibility, g.Access)
	}
	if refusal := g.ReadRefusal(); !strings.Contains(refusal, `visibility: "team", which this build reads as unit`) {
		t.Errorf("the refusal %q does not name the row's own visibility and how it is read", refusal)
	}
	if bound := code.GrantOf(code.Principal{Kind: code.PrincipalOperator, OperatorID: "tok", Seat: "founder"},
		repo, r); bound.Access != code.AccessMaintain {
		t.Errorf("a bound person holds %s on a %q repository, want maintain: a visibility narrows seats only",
			bound.Access, repo.Visibility)
	}
}

// A ROSTER IS NEVER ASKED ABOUT A PERSON: ADR-0078's rules need no chart, and
// a person's push must not depend on a node having read one.
func TestAPersonsGrantAsksNoRoster(t *testing.T) {
	t.Parallel()
	asked := askedRoster{}
	code.GrantOf(code.Principal{Kind: code.PrincipalOperator, OperatorID: "tok", Seat: "ana"},
		ownedBy("web", code.OwnerUnit, "eng"), &asked)
	if asked.n != 0 {
		t.Errorf("the roster was asked %d times for a person", asked.n)
	}
}

// A READ MADE FOR ANOTHER SEAT IS HELD TO BOTH GRANTS — the requester ceiling
// on a repository's grant, judged as one: the asker's own first, refused about
// ITSELF (ErrNotPermitted) where it reads nothing, which is the remedy it can
// act on; then the seat it reads for, ErrAboveCeiling where that seat reads
// nothing of the repository, naming why — never ErrNotPermitted, which would
// be false of an asker that reads it. A read made for nobody is the asker's
// alone, and what a judged read carries is always the ASKER's grant. The
// ceiling is a seat's grant judged as the seat would be judged asking itself,
// so a seat the chart lacks bounds the read to nothing, and so does a human
// seat — a person is never asked as a seat, which is why no caller sends one:
// the closed end of a value that should not arrive.
//
// Mutations: JudgeRead ignoring its ceiling → dev reads plat-private for
// vp-sales; the ceiling judged first → vp-sales's read for root-bot is told
// about root-bot rather than itself; the ceiling's grant carried as the
// answer → dev's read for cto reads at cto's level.
func TestAReadForAnotherSeatIsHeldToBothGrants(t *testing.T) {
	t.Parallel()
	r := testChart()
	shared := ownedBy("platform", code.OwnerUnit, "Platform")
	private := unitOnly(shared)
	for _, tc := range []struct {
		name          string
		asker         string
		ceiling       string
		repo          code.RepoRef
		want          error
		says          string
		carriedAccess code.Access
	}{
		{"for nobody", "dev", "", private, nil, "", code.AccessWrite},
		{"for a lead who reads it", "dev", "cto", private, nil, "", code.AccessWrite},
		{"for a colleague of the unit", "dev", "reader", private, nil, "", code.AccessWrite},
		{"for a seat it shuts out", "dev", "vp-sales", private, code.ErrAboveCeiling,
			`seat "vp-sales" holds no access to repository "platform-private": it is visibility: unit`,
			code.AccessWrite},
		{"for that seat on a company repository", "dev", "vp-sales", shared, nil, "", code.AccessWrite},
		{"an asker it shuts out, for a seat that reads it", "vp-sales", "dev", private, code.ErrNotPermitted,
			`seat "vp-sales" holds no access`, code.AccessNone},
		{"an asker it shuts out, for a seat it shuts out too", "vp-sales", "root-bot", private,
			code.ErrNotPermitted, `seat "vp-sales" holds no access`, code.AccessNone},
		{"for a seat the chart lacks", "dev", "ghost", private, code.ErrAboveCeiling, "no seat of the chart",
			code.AccessWrite},
		{"for a human seat", "dev", "founder", shared, code.ErrAboveCeiling, "human seat", code.AccessWrite},
	} {
		g, err := code.JudgeRead(agent(tc.asker), tc.ceiling, tc.repo, r)
		switch {
		case tc.want == nil && err != nil:
			t.Errorf("%s: %s's read of %s = %v, want it judged readable", tc.name, tc.asker, tc.repo.Name, err)
		case tc.want != nil && (!errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.says)):
			t.Errorf("%s: %s's read of %s = %v, want %v saying %q", tc.name, tc.asker, tc.repo.Name, err,
				tc.want, tc.says)
		}
		if tc.want == code.ErrAboveCeiling && errors.Is(err, code.ErrNotPermitted) {
			t.Errorf("%s: the refusal %v is also ErrNotPermitted, which is false of %s", tc.name, err, tc.asker)
		}
		if g.Principal != agent(tc.asker) || g.Access != tc.carriedAccess {
			t.Errorf("%s: the judged read carries %+v at %s, want %s's own grant at %s", tc.name, g.Principal,
				g.Access, tc.asker, tc.carriedAccess)
		}
	}
}

// A GRANT UNDER A COLLEAGUE'S CEILING IS HELD TO BOTH (M3-decisions U40 (a),
// a coding box working for the seat whose ask woke its turn): every command
// of RefRule's table passes exactly where the seat's own grant passes it AND —
// outside the seat's own namespace, which is its own work — the colleague's
// grant passes the very same command; a read passes where both read; and
// whether any push could pass is the seat's own answer, since its namespace
// is never the ceiling's to refuse. Exhaustively, at every tier and relation
// of both seats, for a seat of a short handle and of a handle four refs long:
// every refusal the ceiling adds says the same for both and fits what a
// receiver carries into a proposal. A seat that reads a repository for a
// colleague who reads nothing of it is told about the colleague.
//
// Mutations: Under ignored by RefRule → the rows the colleague refuses pass,
// red; the namespace not exempted → the seat's own rows under a read-only
// colleague refused, red; MayRead ignoring the ceiling → the colleague who
// reads nothing lets the seat read, red; the colleague's handle in the
// sentence → the long handle's sentences differ and pass the bound, red.
func TestAGrantUnderAColleaguesCeilingIsHeldToBoth(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 4*code.MaxRefNameBytes)
	tiers := append(code.Tiers(), "")
	for _, tier := range tiers {
		for _, relation := range code.Accesses() {
			for _, ceilingTier := range tiers {
				for _, ceilingRelation := range code.Accesses() {
					own := grantAs("ana", tier, relation)
					ceiling := grantAs("bo", ceilingTier, ceilingRelation)
					under := own.Under(ceiling)
					longUnder := grantAs(long, tier, relation).Under(grantAs(long+"b", ceilingTier,
						ceilingRelation))
					what := fmt.Sprintf("ana at %s/%s for bo at %s/%s", tier, relation, ceilingTier,
						ceilingRelation)
					for name, c := range commands {
						ownReason, _ := own.RefRule(c)
						ceilingReason, _ := ceiling.RefRule(c)
						mine := strings.HasPrefix(c.Ref, "refs/heads/seats/ana/")
						want := ownReason == "" && (mine || ceilingReason == "")
						reason, words := under.RefRule(c)
						if (reason == "") != want {
							t.Errorf("%s, %s: passes %v, want %v (own %q, colleague's %q)", what, name,
								reason == "", want, ownReason, ceilingReason)
						}
						if reason != "" && reason != code.RefusalNotPermitted {
							t.Errorf("%s, %s: refused %q, want not_permitted", what, name, reason)
						}
						if len(words) > code.MaxDetailBytes {
							t.Errorf("%s, %s: %d bytes, past the %d a receiver carries: %q", what, name,
								len(words), code.MaxDetailBytes, words)
						}
						// THE LONG HANDLES' OWN ROWS, spelled for them.
						longCommand := c
						longCommand.Ref = strings.ReplaceAll(strings.ReplaceAll(c.Ref, "seats/ana/",
							"seats/"+long+"/"), "seats/bo/", "seats/"+long+"b/")
						if _, longWords := longUnder.RefRule(longCommand); longWords != words {
							t.Errorf("%s, %s: a seat of a long handle is told\n%q\nand ana\n%q", what, name,
								longWords, words)
						}
					}
					if got, want := under.MayRead(), own.MayRead() && ceiling.MayRead(); got != want {
						t.Errorf("%s: MayRead %v, want %v", what, got, want)
					}
					if under.MayPush() != own.MayPush() || under.PushRefusal() != own.PushRefusal() {
						t.Errorf("%s: MayPush %v %q, want the seat's own %v %q", what, under.MayPush(),
							under.PushRefusal(), own.MayPush(), own.PushRefusal())
					}
				}
			}
		}
	}

	r := testChart()
	private := unitOnly(ownedBy("platform", code.OwnerUnit, "Platform"))
	shut := code.GrantOf(agent("dev"), private, r).Under(code.GrantOf(agent("vp-sales"), private, r))
	if shut.MayRead() || !strings.Contains(shut.ReadRefusal(), `seat "dev" reads repository "platform-private"`) ||
		!strings.Contains(shut.ReadRefusal(), `seat "vp-sales" holds no access`) {
		t.Errorf("dev reading platform-private for vp-sales: MayRead %v, told %q", shut.MayRead(),
			shut.ReadRefusal())
	}
	// A CEILING NEVER WIDENS: a read-only seat working for a colleague who
	// writes still creates no branch.
	if reason, _ := code.GrantOf(agent("reader"), private, r).Under(code.GrantOf(agent("dev"), private,
		r)).RefRule(commands["create a branch"]); reason == "" {
		t.Error("a read-only seat working for a colleague who writes created a branch")
	}
}

// A SEAT'S OWN WORK IS STEERED BY WHOEVER WOKE ITS TURN, and nothing else is
// (code.WriteCeiling, M3-decisions U40 (a)): dev — full_access in Infra —
// working for vp-sales, who only reads a company repository, or for nobody (a
// stranger, a person who may not act), moves a branch under its own namespace
// and any branch of a repository its seat owns, its scratch repository and a
// project handed to it, and no other branch of Platform's repository it writes
// for itself; the moment that project is handed to a unit it is steered no
// more. Nobody's ceiling reads nothing, and its refusal says the work is for
// somebody who may change nothing. A colleague's ceiling is that colleague's
// own grant (WriteCeiling.Grant), and a ceiling no build writes — nobody's
// naming a seat, a seat's naming the writer itself or no handle, a person's
// write carrying any — is refused on the write.
//
// Mutations: ownWork answering only the namespace — the scratch repository's
// and the seat-owned project's main are refused, red; ownWork answering true
// for every repository — Platform's main lands for both, red; WriteCeiling.Grant
// granting nobody what GrantOf grants a seat — nobody's main lands, red.
func TestASeatsOwnWorkIsSteeredByWhoeverWokeItsTurn(t *testing.T) {
	t.Parallel()
	r := testChart()
	main := code.Command{Ref: "refs/heads/main", Old: oidOf("a"), New: oidOf("b")}
	ns := code.Command{Ref: "refs/heads/seats/dev/fix", Old: code.ZeroOID, New: oidOf("b")}
	platform := ownedBy("platform", code.OwnerUnit, "Platform")
	scratch := ownedBy("dev.notes", code.OwnerSeat, "dev")
	handed := ownedBy("tools", code.OwnerSeat, "dev")
	given := handed
	given.Owner = code.Owner{Kind: code.OwnerUnit, Ref: "Infra"}
	for _, ceiling := range []code.WriteCeiling{
		{Kind: code.WriteCeilingSeat, Seat: "vp-sales"},
		{Kind: code.WriteCeilingNobody},
	} {
		for _, tc := range []struct {
			repo   code.RepoRef
			c      code.Command
			passes bool
		}{
			{platform, main, false}, {platform, ns, true}, {scratch, main, true}, {handed, main, true},
			{given, main, false},
		} {
			own := code.GrantOf(agent("dev"), tc.repo, r)
			if !tc.passes {
				if reason, _ := own.RefRule(tc.c); reason != "" {
					t.Fatalf("dev's own grant refuses %s on %s, so this case judges no ceiling", tc.c.Ref,
						tc.repo.Name)
				}
			}
			reason, words := own.Under(ceiling.Grant(tc.repo, r)).RefRule(tc.c)
			if (reason == "") != tc.passes {
				t.Errorf("dev for %s, %s on %s: refused %q %q, want passes %v", ceiling.Kind, tc.c.Ref,
					tc.repo.Name, reason, words, tc.passes)
			}
			if ceiling.Kind == code.WriteCeilingNobody && reason != "" &&
				!strings.Contains(words, "may change nothing of this company's code") {
				t.Errorf("dev for nobody, %s on %s: told %q, want the work is for somebody who may change "+
					"nothing", tc.c.Ref, tc.repo.Name, words)
			}
		}
	}
	if code.GrantOf(agent("dev"), platform, r).Under(code.WriteCeiling{Kind: code.WriteCeilingNobody}.
		Grant(platform, r)).MayRead() {
		t.Error("dev reads Platform's repository for nobody")
	}
	if got, want := (code.WriteCeiling{Kind: code.WriteCeilingSeat, Seat: "vp-sales"}).Grant(platform, r),
		code.GrantOf(agent("vp-sales"), platform, r); got.Access != want.Access || got.Relation != want.Relation {
		t.Errorf("a colleague's ceiling stands at %s/%s, its own grant at %s/%s", got.Access, got.Relation,
			want.Access, want.Relation)
	}

	write := aWrite()
	for name, tc := range map[string]struct {
		who     code.Principal
		ceiling code.WriteCeiling
	}{
		"nobody naming a seat":    {write.Principal, code.WriteCeiling{Kind: code.WriteCeilingNobody, Seat: "bob"}},
		"the writer itself":       {write.Principal, code.WriteCeiling{Kind: code.WriteCeilingSeat, Seat: "alice"}},
		"no handle":               {write.Principal, code.WriteCeiling{Kind: code.WriteCeilingSeat, Seat: "Not A Handle"}},
		"a kind no build writes":  {write.Principal, code.WriteCeiling{Kind: "person", Seat: "bob"}},
		"beside a person's write": {lcPerson, code.WriteCeiling{Kind: code.WriteCeilingSeat, Seat: "bob"}},
	} {
		w := aWrite()
		w.Principal, w.Ceiling = tc.who, &tc.ceiling
		if err := w.Validate(); !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), "ceiling") {
			t.Errorf("a write whose ceiling is %s: %v, want ErrInvalid naming the ceiling", name, err)
		}
		asker := tc.who
		asker.Run = code.RunProvenance{} // a lifecycle request names who asks, and no run
		req := lcSettings(asker, "0190f3c4-0000-7000-8000-0000000000aa", code.RepoChange{Description: new("x")})
		req.Ceiling = &tc.ceiling
		if err := req.Validate(); !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), "ceiling") {
			t.Errorf("a lifecycle request whose ceiling is %s: %v, want ErrInvalid naming the ceiling", name, err)
		}
	}
	for _, ceiling := range []code.WriteCeiling{{Kind: code.WriteCeilingSeat, Seat: "bob"},
		{Kind: code.WriteCeilingNobody}} {
		w := aWrite()
		w.Ceiling = &ceiling
		if err := w.Validate(); err != nil {
			t.Errorf("a seat's write for %s %s: %v", ceiling.Kind, ceiling.Seat, err)
		}
	}
}

type askedRoster struct{ n int }

func (a *askedRoster) Seat(string) (code.RosterSeat, bool) { a.n++; return code.RosterSeat{}, false }
func (a *askedRoster) Lead(string) string                  { a.n++; return "" }
func (a *askedRoster) Leads(string) []string               { a.n++; return nil }
func (a *askedRoster) Within(string, string) bool          { a.n++; return false }

// The commands RefRule judges, one of each row of its table.
var (
	oidA, oidB = strings.Repeat("a", 40), strings.Repeat("b", 40)
	commands   = map[string]code.Command{
		"create a branch":         {Ref: "refs/heads/feature", Old: code.ZeroOID, New: oidB},
		"fast-forward a branch":   {Ref: "refs/heads/feature", Old: oidA, New: oidB},
		"force a branch":          {Ref: "refs/heads/feature", Old: oidA, New: oidB, Forced: true},
		"delete a branch":         {Ref: "refs/heads/feature", Old: oidA, New: code.ZeroOID},
		"create a tag":            {Ref: "refs/tags/v1", Old: code.ZeroOID, New: oidB},
		"delete a tag":            {Ref: "refs/tags/v1", Old: oidA, New: code.ZeroOID},
		"move a tag":              {Ref: "refs/tags/v1", Old: oidA, New: oidB},
		"create in its namespace": {Ref: "refs/heads/seats/ana/x", Old: code.ZeroOID, New: oidB},
		"force in its namespace":  {Ref: "refs/heads/seats/ana/x", Old: oidA, New: oidB, Forced: true},
		"delete in its namespace": {Ref: "refs/heads/seats/ana/x", Old: oidA, New: code.ZeroOID},
		"create in another's":     {Ref: "refs/heads/seats/bo/x", Old: code.ZeroOID, New: oidB},
		"create under its handle": {Ref: "refs/heads/ana/x", Old: code.ZeroOID, New: oidB},
		"the engine's own":        {Ref: "refs/crewlet/merge/1", Old: code.ZeroOID, New: oidB},
		"the seats' root":         {Ref: "refs/heads/seats", Old: code.ZeroOID, New: oidB},
		"a namespace as a branch": {Ref: "refs/heads/seats/ana", Old: code.ZeroOID, New: oidB},
		"outside heads and tags":  {Ref: "refs/notes/commits", Old: code.ZeroOID, New: oidB},
	}
)

// need is RefRule's table written out plainly, row by row (§2.4): what a
// command needs of a principal that is not a seat in its own namespace, or
// -1 for a ref nobody pushes.
func need(c code.Command) code.Access {
	switch {
	case strings.HasPrefix(c.Ref, "refs/crewlet/"), c.Ref == "refs/heads/seats",
		c.Ref == "refs/heads/seats/ana", c.Ref == "refs/notes/commits":
		return -1
	case strings.HasPrefix(c.Ref, "refs/heads/seats/"):
		return code.AccessMaintain // another seat's namespace
	case c.New == code.ZeroOID:
		return code.AccessMaintain
	case strings.HasPrefix(c.Ref, "refs/tags/") && c.Old != code.ZeroOID:
		return code.AccessMaintain
	case c.Forced:
		return code.AccessMaintain
	}
	return code.AccessWrite
}

// grantFor is the grant a seat holds at tier and relation — Access by the
// definition GrantOf computes, which TestGrantsFollowTheChart holds.
func grantFor(tier code.Tier, relation code.Access) code.Grant {
	return grantAs("ana", tier, relation)
}

// grantAs is grantFor for the seat handle.
func grantAs(handle string, tier code.Tier, relation code.Access) code.Grant {
	return code.Grant{Principal: agent(handle), RepoID: "id-web", RepoName: "web",
		Owner: code.Owner{Kind: code.OwnerUnit, Ref: "eng"}, Tier: tier, Relation: relation,
		Access: min(tier.Cap(), relation)}
}

// A REFUSAL SAYS THE SAME FOR EVERY SEAT: a sentence RefRule forms names no
// handle, ref or repository, so its length is the same whoever asks and it
// fits what a receiver may carry into a proposal (maxDetailBytes) whatever the
// chart spells — a handle is a role name's slug, and org.ValidHandle bounds
// its length by nothing. Every row of RefRule's table, for a seat of the
// handle "ana" and of a handle four refs long, at every tier and relation —
// the namespace rows spelled for each seat's own handle — and the sentences of
// a principal that stands nowhere: no seat of the chart, a kind this build
// grants nothing, a tier it does not know.
//
// Mutation: name the seat's namespace by its handle in the remedy → the long
// handle's sentences differ and pass the bound, red.
func TestARefusalSaysTheSameForEverySeat(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 4*code.MaxRefNameBytes)
	// "ana" is in no ref of the table but as the seat's handle.
	as := func(handle string, c code.Command) code.Command {
		c.Ref = strings.ReplaceAll(c.Ref, "ana", handle)
		return c
	}
	bounded := func(what, words string) {
		t.Helper()
		if len(words) > code.MaxDetailBytes {
			t.Errorf("%s: %d bytes, past the %d a receiver carries: %q", what, len(words), code.MaxDetailBytes, words)
		}
	}
	for _, tier := range append(code.Tiers(), "", code.Tier(long)) {
		for _, relation := range code.Accesses() {
			for name, c := range commands {
				_, short := grantAs("ana", tier, relation).RefRule(as("ana", c))
				_, words := grantAs(long, tier, relation).RefRule(as(long, c))
				what := fmt.Sprintf("tier %.12s, relation %s, %s", tier, relation, name)
				bounded(what, words)
				if tier != code.Tier(long) && words != short {
					t.Errorf("%s: a seat of a long handle is told\n%q\nand ana\n%q", what, words, short)
				}
			}
		}
	}
	r := testChart()
	repo := ownedBy("web", code.OwnerUnit, "eng")
	for name, g := range map[string]code.Grant{
		"no seat of the chart": code.GrantOf(agent(long), repo, r),
		"no chart":             code.GrantOf(agent(long), repo, nil),
		"an unknown kind":      code.GrantOf(code.Principal{Kind: code.PrincipalKind(long), Seat: "dev"}, repo, r),
	} {
		reason, words := g.RefRule(commands["create a branch"])
		if reason != code.RefusalNotPermitted {
			t.Errorf("%s: %q", name, reason)
		}
		bounded(name, words)
		bounded(name+" (advertised)", g.PushRefusal())
	}
}

// REFRULE IS ITS TABLE, for every principal: a bound person passes every row
// but the reserved refs, a token bound to nobody is refused every one with the
// sentence git printed before grants existed, and a seat at every tier and
// relation is held to each row — its own namespace by a read relation and the
// full_access tier, everything else by its access against what the command
// needs. Every refusal is not_permitted, names no ref (git prints it beside),
// fits what a receiver may carry into a proposal, and offers a remedy RefRule
// itself would admit. MayPush says whether any command could pass. A coding
// box's grant is held to its run's (Within) at every tier and relation.
//
// Mutations: RefRule permitting a read-level create → the read rows red; the
// namespace row ignoring the tier → its review and read_only rows red; a
// remedy offering the namespace to a seat that does not own one → the remedy
// check red; Within returning the seat's grant → every row of a run given
// read red.
func TestRefRuleTable(t *testing.T) {
	t.Parallel()
	bound := code.GrantOf(code.Principal{Kind: code.PrincipalOperator, OperatorID: "tok", Seat: "founder"},
		ownedBy("web", code.OwnerUnit, "eng"), nil)
	unbound := code.GrantOf(code.Principal{Kind: code.PrincipalOperator, OperatorID: "ci"},
		ownedBy("web", code.OwnerUnit, "eng"), nil)
	for name, c := range commands {
		reserved := need(c) < 0
		if reason, words := bound.RefRule(c); (reason == "") == reserved {
			t.Errorf("a bound person, %s: %q %q", name, reason, words)
		}
		reason, words := unbound.RefRule(c)
		if reason != code.RefusalNotPermitted {
			t.Errorf("a token bound to nobody, %s: permitted", name)
		}
		if !reserved && words != code.UnboundDetail {
			t.Errorf("a token bound to nobody, %s: %q, want the unbound sentence", name, words)
		}
	}
	if !bound.MayPush() || unbound.MayPush() || unbound.PushRefusal() != code.UnboundDetail ||
		bound.PushRefusal() != "" {
		t.Errorf("MayPush: bound %v %q, unbound %v %q", bound.MayPush(), bound.PushRefusal(),
			unbound.MayPush(), unbound.PushRefusal())
	}

	tiers := append(code.Tiers(), "")
	for _, tier := range tiers {
		for _, relation := range code.Accesses() {
			g := grantFor(tier, relation)
			passed := false
			for name, c := range commands {
				reason, words := g.RefRule(c)
				var want bool
				switch n := need(c); {
				case n < 0:
					want = false
				case strings.HasPrefix(c.Ref, code.SeatNamespace("ana")):
					want = relation >= code.AccessRead && tier == code.TierFullAccess
				default:
					want = g.Access >= n
				}
				if (reason == "") != want {
					t.Errorf("tier %s, relation %s, %s: %q %q, want permitted %v", tier, relation, name,
						reason, words, want)
					continue
				}
				if want {
					passed = true
					continue
				}
				if reason != code.RefusalNotPermitted || words == "" {
					t.Errorf("tier %s, relation %s, %s: refused %q %q", tier, relation, name, reason, words)
				}
				if need(c) >= 0 && strings.Contains(words, c.Ref) {
					t.Errorf("tier %s, relation %s, %s: the sentence names the ref: %q", tier, relation, name, words)
				}
				if len(words) > code.MaxDetailBytes {
					t.Errorf("tier %s, relation %s, %s: %d bytes, past the %d a receiver carries",
						tier, relation, name, len(words), code.MaxDetailBytes)
				}
				if need(c) >= 0 {
					remedyAdmits(t, g, c, words)
				}
			}
			if passed != g.MayPush() {
				t.Errorf("tier %s, relation %s: some command passes = %v, MayPush = %v", tier, relation,
					passed, g.MayPush())
			}
			if (g.PushRefusal() == "") != g.MayPush() {
				t.Errorf("tier %s, relation %s: PushRefusal %q beside MayPush %v", tier, relation,
					g.PushRefusal(), g.MayPush())
			}
		}
	}

	// A CODING BOX'S GRANT IS HELD TO ITS RUN'S (Within): a run given the
	// repository to read refuses every row, its seat's own namespace's
	// included; one given it to write holds every row outside the namespace
	// to write, whatever the seat maintains; and maintain is the seat's grant
	// unchanged. What the seat READS is never narrowed by a run that was
	// given the repository at all.
	for _, tier := range tiers {
		for _, relation := range code.Accesses() {
			seat := grantFor(tier, relation)
			if seat.Within(code.AccessMaintain) != seat {
				t.Errorf("tier %s, relation %s: Within(maintain) changed the seat's grant", tier, relation)
			}
			for _, run := range []code.Access{code.AccessRead, code.AccessWrite} {
				g := seat.Within(run)
				passed := false
				for name, c := range commands {
					reason, words := g.RefRule(c)
					var want bool
					switch n := need(c); {
					case n < 0, run < code.AccessWrite:
						want = false
					case strings.HasPrefix(c.Ref, code.SeatNamespace("ana")):
						want = relation >= code.AccessRead && tier == code.TierFullAccess
					default:
						want = min(seat.Access, run) >= n
					}
					if (reason == "") != want {
						t.Errorf("tier %s, relation %s, a run given %s, %s: %q %q, want permitted %v", tier,
							relation, run, name, reason, words, want)
						continue
					}
					if want {
						passed = true
						continue
					}
					switch {
					case reason != code.RefusalNotPermitted || words == "":
						t.Errorf("tier %s, relation %s, a run given %s, %s: refused %q %q", tier, relation, run,
							name, reason, words)
					case need(c) >= 0 && strings.Contains(words, c.Ref):
						t.Errorf("tier %s, relation %s, a run given %s, %s: the sentence names the ref: %q", tier,
							relation, run, name, words)
					case len(words) > code.MaxDetailBytes:
						t.Errorf("tier %s, relation %s, a run given %s, %s: %d bytes, past the %d a receiver "+
							"carries", tier, relation, run, name, len(words), code.MaxDetailBytes)
					case need(c) >= 0 && run < code.AccessWrite && words != code.RunReadDetail:
						t.Errorf("tier %s, relation %s, a run given read, %s: %q, want the run's sentence",
							tier, relation, name, words)
					}
				}
				if passed != g.MayPush() {
					t.Errorf("tier %s, relation %s, a run given %s: some command passes = %v, MayPush = %v",
						tier, relation, run, passed, g.MayPush())
				}
				if (g.PushRefusal() == "") != g.MayPush() {
					t.Errorf("tier %s, relation %s, a run given %s: PushRefusal %q beside MayPush %v", tier,
						relation, run, g.PushRefusal(), g.MayPush())
				}
				if g.MayRead() != seat.MayRead() {
					t.Errorf("tier %s, relation %s, a run given %s: MayRead %v, the seat's %v", tier, relation,
						run, g.MayRead(), seat.MayRead())
				}
			}
		}
	}

	// TWO SENTENCES IN FULL, so a person can read what a seat is told.
	_, words := grantFor(code.TierFullAccess, code.AccessRead).RefRule(commands["create a branch"])
	if want := "this seat holds read on this repository (tier full_access, relation read); creating a branch " +
		"needs write — push under its own namespace (refs/heads/seats/<its handle>/) instead, or ask the " +
		"lead of the repository's owning unit"; words != want {
		t.Errorf("a full_access seat that reads:\n got %q\nwant %q", words, want)
	}
	_, words = grantFor(code.TierReview, code.AccessWrite).RefRule(commands["create a branch"])
	if want := "this seat holds review on this repository (tier review, relation write); creating a branch " +
		"needs write — a person can set its role.code.access to full_access"; words != want {
		t.Errorf("a review seat that writes:\n got %q\nwant %q", words, want)
	}
}

// remedyAdmits holds a refusal's remedy to RefRule itself: whatever the
// sentence offers, the grant it describes permits.
func remedyAdmits(t *testing.T, g code.Grant, c code.Command, words string) {
	t.Helper()
	own := code.Command{Ref: code.SeatNamespace("ana") + "work", Old: code.ZeroOID, New: oidB}
	with := func(tier code.Tier, relation code.Access) code.Grant {
		return grantFor(tier, relation)
	}
	var admitted bool
	const namespace = "its own namespace (" + code.SeatNamespaceRoot + "<its handle>/)"
	switch {
	case strings.Contains(words, "push under "+namespace+" instead"):
		reason, _ := g.RefRule(own)
		admitted = reason == ""
	case strings.Contains(words, "a person can set its role.code.access to full_access"):
		reason, _ := with(code.TierFullAccess, g.Relation).RefRule(c)
		admitted = reason == ""
	case strings.Contains(words, "with role.code.access full_access it pushes under "+namespace):
		reason, _ := with(code.TierFullAccess, g.Relation).RefRule(own)
		admitted = reason == ""
	case strings.Contains(words, "and a person to set its role.code.access to full_access"):
		reason, _ := with(code.TierFullAccess, code.AccessMaintain).RefRule(c)
		admitted = reason == ""
	case strings.Contains(words, "ask the lead of the repository's owning unit"):
		reason, _ := with(g.Tier, code.AccessMaintain).RefRule(c)
		admitted = reason == ""
	}
	if !admitted {
		t.Errorf("tier %s, relation %s, %s: the remedy in %q is none RefRule admits", g.Tier, g.Relation, c.Ref,
			words)
	}
}

// A SEAT'S NAMESPACE IS UNDER THE SEATS' ROOT, never at its handle: a
// full_access seat that only reads a repository creates, moves, forces and
// deletes under refs/heads/seats/<handle>/ and is refused every one at
// refs/heads/<handle>/, where people's branches live; and the root and a
// seat's namespace as a branch are reserved to everybody, since no ref is
// created beside a directory of its name (ref_conflict) and either would block
// seats' namespaces.
//
// Mutations: the namespace matching refs/heads/<handle>/ → the <handle>/x rows
// permitted; drop the reserved shapes → the two reserved_ref rows red.
func TestASeatsNamespaceIsUnderTheSeatsRoot(t *testing.T) {
	t.Parallel()
	if got := code.SeatNamespace("ana"); got != "refs/heads/seats/ana/" {
		t.Fatalf("SeatNamespace(ana) = %q", got)
	}
	g := grantFor(code.TierFullAccess, code.AccessRead)
	for _, ref := range []string{"refs/heads/seats/ana/x", "refs/heads/ana/x"} {
		for name, c := range map[string]code.Command{
			"create": {Ref: ref, Old: code.ZeroOID, New: oidB},
			"update": {Ref: ref, Old: oidA, New: oidB},
			"force":  {Ref: ref, Old: oidA, New: oidB, Forced: true},
			"delete": {Ref: ref, Old: oidA, New: code.ZeroOID},
		} {
			reason, words := g.RefRule(c)
			own := strings.HasPrefix(ref, code.SeatNamespaceRoot)
			if own && reason != "" {
				t.Errorf("%s %s by its own full_access seat: %q %q", name, ref, reason, words)
			}
			if !own && reason != code.RefusalNotPermitted {
				t.Errorf("%s %s by a seat that only reads: permitted", name, ref)
			}
		}
	}
	for ref, want := range map[string]code.Refusal{
		"refs/heads/seats":         code.RefusalReservedRef,
		"refs/heads/seats/ana":     code.RefusalReservedRef,
		"refs/heads/seats/ana/x":   "",
		"refs/heads/seats/ana/x/y": "",
		"refs/heads/seatsx":        "",
	} {
		if got := code.StatelessRefusal(code.Command{Ref: ref, Old: code.ZeroOID, New: oidB}); got != want {
			t.Errorf("StatelessRefusal(%s) = %q, want %q", ref, got, want)
		}
	}
}

// NO REPOSITORY TAKES A RESERVED SEAT BRANCH AS ITS DEFAULT: no push may create
// one (reserved_ref), so a repository declared with one would have a default
// branch nobody can ever push. The domain's own guard refuses it whatever
// door the settings came through, beside configuration's refusal of the field.
func TestARepositorysDefaultBranchIsNoReservedSeatBranch(t *testing.T) {
	t.Parallel()
	for branch, reserved := range map[string]bool{
		"seats": true, "seats/main": true, "seats/ana": true,
		"seats/ana/main": false, "main": false, "seatsx": false,
	} {
		s := code.RepoSettings{
			ManagedBy: code.ManagedConfig, Kind: code.RepoProject,
			Owner:      code.Owner{Kind: code.OwnerUnit, Ref: "ENG"},
			Visibility: code.VisibilityCompany, Search: code.SearchIndexed, DefaultBranch: branch,
		}
		err := s.Validate()
		switch {
		case reserved && !errors.Is(err, code.ErrInvalid):
			t.Errorf("default_branch %s: %v, want ErrInvalid", branch, err)
		case reserved && !strings.Contains(err.Error(), "default_branch"):
			t.Errorf("default_branch %s: %v names no field", branch, err)
		case !reserved && err != nil:
			t.Errorf("default_branch %s: %v", branch, err)
		}
	}
}

// A PRINCIPAL NAMES ITSELF ONE WAY: an operator its credential and, where it is
// bound, a person's handle, and no run; a seat its handle and no credential,
// and a run only one that names its turn — and a coding box's, its run by the
// launch's UUID in its canonical spelling together with its coding agent —
// and holds every field within the rules. A proposal refuses a seat's push
// naming no run, a token bound to nobody's and a principal naming no
// credential.
//
// Mutation: take a run id that is not a canonical UUID (invalid() skipping
// canonicalUUID) — the uppercase launch id is a run, red.
func TestAPrincipalNamesItselfOneWay(t *testing.T) {
	t.Parallel()
	run := code.RunProvenance{TurnID: "0192f00d-0000-7000-8000-000000000001", WorkBackend: "native",
		WorkKey: "ENG-12", Model: "claude-sonnet-4-5"}
	with := func(change func(*code.RunProvenance)) code.RunProvenance {
		r := run
		change(&r)
		return r
	}
	boxed := func(change func(*code.RunProvenance)) code.RunProvenance {
		return with(func(r *code.RunProvenance) {
			r.RunID, r.CodingAgent = "3f1c2b4a-5d6e-4f70-8a9b-0c1d2e3f4a5b", "claude-code"
			change(r)
		})
	}
	long := strings.Repeat("k", code.MaxProvenanceBytes+1)
	for _, tc := range []struct {
		p     code.Principal
		valid bool
	}{
		{code.Principal{Kind: code.PrincipalOperator, OperatorID: "tok", Seat: "ana"}, true},
		{code.Principal{Kind: code.PrincipalOperator, OperatorID: "ci"}, true},
		{code.Principal{Kind: code.PrincipalOperator, Seat: "ana"}, false},
		{code.Principal{Kind: code.PrincipalOperator, OperatorID: "tok", Seat: "Ana Smith"}, false},
		{code.Principal{Kind: code.PrincipalOperator, OperatorID: "tok", Seat: "ana", Run: run}, false},
		{code.Principal{Kind: code.PrincipalAgent, Seat: "ana"}, true},
		{code.Principal{Kind: code.PrincipalAgent, Seat: "ana", Run: run}, true},
		{code.Principal{Kind: code.PrincipalAgent, Seat: "ana",
			Run: code.RunProvenance{TurnID: strings.Repeat("t", code.MaxProvenanceBytes)}}, true},
		{code.Principal{Kind: code.PrincipalAgent, Seat: "ana", Run: with(func(r *code.RunProvenance) {
			r.TurnID = ""
		})}, false},
		{code.Principal{Kind: code.PrincipalAgent, Seat: "ana", Run: with(func(r *code.RunProvenance) {
			r.WorkKey = long
		})}, false},
		{code.Principal{Kind: code.PrincipalAgent, Seat: "ana", Run: with(func(r *code.RunProvenance) {
			r.Model = "claude\nsonnet"
		})}, false},
		{code.Principal{Kind: code.PrincipalAgent, Seat: "ana", Run: with(func(r *code.RunProvenance) {
			r.WorkBackend = "nat\xffive"
		})}, false},
		// A WORK ITEM'S IDENTITY names its tracker, and is a label's length.
		{code.Principal{Kind: code.PrincipalAgent, Seat: "ana", Run: with(func(r *code.RunProvenance) {
			r.WorkID = "0190d8f0-2222-7000-8000-0123456789ab"
		})}, true},
		{code.Principal{Kind: code.PrincipalAgent, Seat: "ana", Run: with(func(r *code.RunProvenance) {
			r.WorkBackend, r.WorkID = "", "0190d8f0-2222-7000-8000-0123456789ab"
		})}, false},
		{code.Principal{Kind: code.PrincipalAgent, Seat: "ana", Run: with(func(r *code.RunProvenance) {
			r.WorkID = long
		})}, false},
		// A CODING BOX'S RUN: its launch's id and its coding agent, together.
		{code.Principal{Kind: code.PrincipalAgent, Seat: "ana", Run: boxed(func(*code.RunProvenance) {})}, true},
		{code.Principal{Kind: code.PrincipalAgent, Seat: "ana", Run: boxed(func(r *code.RunProvenance) {
			r.CodingAgent = strings.Repeat("a", code.MaxProvenanceBytes)
		})}, true},
		{code.Principal{Kind: code.PrincipalAgent, Seat: "ana", Run: boxed(func(r *code.RunProvenance) {
			r.RunID = strings.ToUpper(r.RunID)
		})}, false},
		{code.Principal{Kind: code.PrincipalAgent, Seat: "ana", Run: boxed(func(r *code.RunProvenance) {
			r.RunID = "launch-1"
		})}, false},
		{code.Principal{Kind: code.PrincipalAgent, Seat: "ana", Run: boxed(func(r *code.RunProvenance) {
			r.CodingAgent = ""
		})}, false},
		{code.Principal{Kind: code.PrincipalAgent, Seat: "ana", Run: boxed(func(r *code.RunProvenance) {
			r.RunID = ""
		})}, false},
		{code.Principal{Kind: code.PrincipalAgent, Seat: "ana", Run: boxed(func(r *code.RunProvenance) {
			r.CodingAgent = long
		})}, false},
		{code.Principal{Kind: code.PrincipalAgent, Seat: "ana", Run: boxed(func(r *code.RunProvenance) {
			r.CodingAgent = "claude\tcode"
		})}, false},
		{code.Principal{Kind: code.PrincipalAgent}, false},
		{code.Principal{Kind: code.PrincipalAgent, Seat: "ana/x"}, false},
		{code.Principal{Kind: code.PrincipalAgent, Seat: "ana", OperatorID: "tok"}, false},
		{code.Principal{Kind: "robot", Seat: "ana"}, false},
		// THE ENGINE'S OWN ACCOUNT asks, reads and writes nothing: only its
		// expiry's proposal carries it, which Proposal.Validate judges.
		{code.Principal{Kind: code.PrincipalSystem}, false},
	} {
		err := tc.p.Validate()
		if (err == nil) != tc.valid || (err != nil && !errors.Is(err, code.ErrInvalid)) {
			t.Errorf("%+v: %v, want valid %v", tc.p, err, tc.valid)
		}
	}
	proposal := code.Proposal{PushID: "0190f3c4-0000-7000-8000-000000000002",
		RepoID: "0190f3c4-0000-7000-8000-00000000b0b0", Node: "n1", ReceivedAt: time.Unix(1_800_000_000, 0),
		Principal: code.Principal{Kind: code.PrincipalOperator, OperatorID: "tok", Seat: "ana"},
		Via:       code.ViaGit, Commands: []code.Command{commands["create a branch"]}}
	if err := proposal.Validate(); err != nil {
		t.Fatalf("a bound person's proposal: %v", err)
	}
	for _, p := range []code.Principal{
		{Kind: code.PrincipalAgent, Seat: "ana"},
		{Kind: code.PrincipalOperator, OperatorID: "ci"},
		{Kind: code.PrincipalOperator, Seat: "ana"},
	} {
		proposal.Principal = p
		if err := proposal.Validate(); !errors.Is(err, code.ErrInvalidProposal) {
			t.Errorf("a proposal by %+v: %v, want refused", p, err)
		}
	}
	if !slices.Equal(code.PrincipalKinds(), []code.PrincipalKind{code.PrincipalOperator, code.PrincipalAgent,
		code.PrincipalSystem}) {
		t.Errorf("PrincipalKinds = %v", code.PrincipalKinds())
	}
}

// THE LEVELS ARE ORDERED AND NAMED, and a tier caps at its own level: the
// minimum a grant takes is only meaningful over an order.
func TestTheLevelsAreOrdered(t *testing.T) {
	t.Parallel()
	levels := code.Accesses()
	for i, a := range levels {
		if !a.Valid() || (i > 0 && levels[i-1] >= a) {
			t.Errorf("level %d (%s) is out of order or invalid", i, a)
		}
	}
	if got := fmt.Sprint(levels); got != "[none read review write maintain]" {
		t.Errorf("the levels are %s", got)
	}
	for tier, want := range map[code.Tier]code.Access{
		code.TierReadOnly: code.AccessRead, code.TierReview: code.AccessReview,
		code.TierFullAccess: code.AccessMaintain, "": code.AccessRead, "full-access": code.AccessNone,
	} {
		if got := tier.Cap(); got != want {
			t.Errorf("%q caps at %s, want %s", tier, got, want)
		}
	}
}
