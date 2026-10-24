package engine

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
)

// rosterChart is a chart with every shape a grant reads: a root seat and a
// human seat; a led unit holding a led child, which holds a child with no lead
// of its own; a second child with none, inheriting the top lead; and a unit
// with no lead anywhere above it. platformID is Platform's `id:` line, or none.
func rosterChart(platformID string) string {
	return `name: Acme
roles:
  - name: CEO
    handle: ceo
  - name: Founder
    kind: human
    contact: {slack_user_id: U0FOUNDER}
units:
  - name: Engineering
    id: eng
    lead: cto
    roles:
      - name: CTO
        handle: cto
        code: {access: full_access}
    children:
      - name: Platform
` + platformID + `        lead: plat-lead
        roles:
          - name: Plat Lead
            handle: plat-lead
            code: {access: review}
        children:
          - name: Infra
            roles:
              - name: Dev
                handle: dev
                code: {access: full_access}
      - name: Tools
        roles:
          - name: Toolsmith
            handle: toolsmith
  - name: Sales
    roles:
      - name: Rep
        handle: rep
        code: {access: read_only}
`
}

// rosterOfChart is the roster of the chart doc, and the chart.
func rosterOfChart(t *testing.T, doc string) (codeRoster, *org.Organization) {
	t.Helper()
	c, err := NewCompany(parseCompany(t, doc))
	if err != nil {
		t.Fatalf("build the epoch: %v", err)
	}
	return newCodeRoster(c.Org), c.Org
}

// handleOf is a seat's handle, "" for none.
func handleOf(r *org.Role) string {
	if r == nil {
		return ""
	}
	return r.Handle()
}

// spellings is every reference a durable row may hold for u: its key, its
// name, and its id where it has one — folded as a person might write them.
func spellings(u *org.Unit) []string {
	out := []string{u.Key(), u.Name, strings.ToUpper(u.Name)}
	if u.ID != "" {
		out = append(out, u.ID)
	}
	return out
}

// THE CODE ROSTER IS THE CHART: every answer it gives is the one the org model
// gives for the same seat or unit — a seat's unit is its unit's key, a unit's
// lead its EFFECTIVE lead, the lead chain the effective lead of each unit from
// it to the top, and a subtree the units a seat's chain passes through — held
// to the org model's own functions (UnitFor, UnitChainFor, EffectiveLead) for
// every seat and every unit, by every spelling a row may hold, never to a
// table written beside it. A unit a reference names nothing of answers no
// lead, no chain and no subtree.
//
// And a REFERENCE OUTLIVES ITS SPELLING: a repository made under a unit's
// name, after the unit gains an id, still names its unit — its lead
// maintains it and its members write to it.
//
// Mutations: Leads skipping the unit's own effective lead → every chain row
// red; the units resolved by an exact read of their Key → the renamed row and
// every by-name row red; Lead read off the unit's declared lead → Infra's
// and Tools' rows red.
func TestTheCodeRosterIsTheChart(t *testing.T) {
	t.Parallel()
	roster, o := rosterOfChart(t, rosterChart(""))

	for r := range o.AllRoles() {
		want := code.RosterSeat{Handle: r.Handle(), Human: r.IsHuman(), Tier: code.Tier(r.CodeAccess),
			Unit: o.UnitFor(r).Key()}
		if got, ok := roster.Seat(r.Handle()); !ok || got != want {
			t.Errorf("Seat(%q) = %+v, %v; want %+v", r.Handle(), got, ok, want)
		}
	}
	if _, ok := roster.Seat("nobody"); ok {
		t.Error("a handle no seat has is a seat")
	}

	// A unit's chain, and so its lead chain, is read off a seat it holds
	// directly: every unit of the fixture holds one.
	chainOf := map[*org.Unit][]*org.Unit{}
	for r := range o.AllRoles() {
		if u := o.UnitFor(r); u != nil {
			chainOf[u] = o.UnitChainFor(r)
		}
	}
	for u := range o.AllUnits() {
		chain, ok := chainOf[u]
		if !ok {
			t.Fatalf("the premise: unit %s holds no seat to read its chain from", u.Name)
		}
		var leads []string
		for _, above := range slices.Backward(chain) {
			if lead := handleOf(o.EffectiveLead(above)); lead != "" && !slices.Contains(leads, lead) {
				leads = append(leads, lead)
			}
		}
		for _, ref := range spellings(u) {
			if got, want := roster.Lead(ref), handleOf(o.EffectiveLead(u)); got != want {
				t.Errorf("Lead(%q) = %q, want %q", ref, got, want)
			}
			if got := roster.Leads(ref); fmt.Sprint(got) != fmt.Sprint(leads) {
				t.Errorf("Leads(%q) = %v, want %v", ref, got, leads)
			}
			for a := range o.AllUnits() {
				for _, aref := range spellings(a) {
					if got, want := roster.Within(ref, aref), slices.Contains(chain, a); got != want {
						t.Errorf("Within(%q, %q) = %v, want %v", ref, aref, got, want)
					}
				}
			}
		}
	}
	// The fixture's own premises, so the loops above were about something.
	for ref, want := range map[string]string{"Infra": "plat-lead", "Tools": "cto", "Sales": ""} {
		if got := roster.Lead(ref); got != want {
			t.Errorf("the premise: %s's effective lead is %q, want %q", ref, got, want)
		}
	}
	if got := roster.Leads("Infra"); fmt.Sprint(got) != "[plat-lead cto]" {
		t.Errorf("the premise: Infra's lead chain is %v, want [plat-lead cto]", got)
	}
	for _, ref := range []string{"Marketing", ""} {
		if roster.Lead(ref) != "" || roster.Leads(ref) != nil || roster.Within(ref, "eng") ||
			roster.Within("eng", ref) {
			t.Errorf("a reference to no unit, %q, answered as one", ref)
		}
	}

	// A REFERENCE OUTLIVES ITS SPELLING.
	renamed, _ := rosterOfChart(t, rosterChart("        id: plat\n"))
	if s, _ := renamed.Seat("dev"); s.Unit != "Infra" {
		t.Fatalf("the premise: dev's unit is %q", s.Unit)
	}
	if s, _ := renamed.Seat("plat-lead"); s.Unit != "plat" {
		t.Fatalf("the premise: Platform's key is not its new id: plat-lead's unit is %q", s.Unit)
	}
	repo := code.RepoRef{ID: "id-platform", Name: "platform", Visibility: code.VisibilityUnit,
		Owner: code.Owner{Kind: code.OwnerUnit, Ref: "Platform"}}
	if got := renamed.Lead("Platform"); got != "plat-lead" {
		t.Errorf("Lead of the name Platform after it gained an id = %q, want plat-lead", got)
	}
	for handle, want := range map[string]code.Access{
		"plat-lead": code.AccessMaintain, "dev": code.AccessWrite, "cto": code.AccessRead,
		"toolsmith": code.AccessNone, "rep": code.AccessNone,
	} {
		if got := code.GrantOf(agent(handle), repo, renamed).Relation; got != want {
			t.Errorf("%s's relation to a repository owned by Platform's old spelling = %s, want %s",
				handle, got, want)
		}
	}
}

func agent(handle string) code.Principal {
	return code.Principal{Kind: code.PrincipalAgent, Seat: handle}
}

// THE ENGINE'S CODE READER JUDGES A SEAT BY THE CHART THE NODE SERVES NOW: the
// reader the native runtime builds is handed the engine's roster, read per
// call, so a seat outside a `visibility: unit` repository's owning unit is
// refused it, one inside reads it at its tier's cap, a root seat with no lead
// above it reads nothing — and an activation that moves the outside seat into
// the unit lets it read with no restart, because the roster is the current
// epoch's and not the one the runtime was built under.
//
// Mutations: the reader built with no Roster → swe is refused too; the epoch
// installed without its roster → the same; the roster captured once, at its
// first read, rather than read per call → rep is still refused after the move.
func TestTheEnginesCodeReaderJudgesASeatByTheCurrentChart(t *testing.T) {
	t.Parallel()
	web := "    - name: web\n      owner: {unit: Engineering}\n      visibility: unit\n" +
		"      protect: [{branch: main}]\n"
	const rep = "      - name: Rep\n        handle: rep\n        llm: zulu\n"
	sales := "  - name: Sales\n    roles:\n      - name: Manager\n        handle: mgr\n        llm: zulu\n" + rep
	e := newSandboxNode(t, parseCompany(t, codeReposDoc(web, sales)))
	servesCodeReads(t, e)
	reconciled(t, e)
	read := func(handle string) (*code.RepoDetail, error) {
		t.Helper()
		answer, err := e.router.Code().Read(t.Context(), code.ReadQuery{Asker: agent(handle),
			Kind: code.ReadRepo, Repo: "web", Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
		return answer.Repo, err
	}
	if repo, err := read("swe"); err != nil || repo.Access != code.AccessRead.String() {
		t.Fatalf("swe, in the owning unit at the read_only default, reads web as %+v, %v; want read", repo, err)
	}
	for _, outside := range []string{"rep", "ceo"} {
		if _, err := read(outside); !errors.Is(err, code.ErrNotPermitted) {
			t.Errorf("%s, outside Engineering, reads a visibility: unit repository: %v, want ErrNotPermitted",
				outside, err)
		}
	}

	// THE SEAT MOVES, the repository does not: Rep now sits in Engineering.
	const swe = "        handle: swe\n        llm: zulu\n"
	before := codeReposDoc(web, sales)
	moved := strings.Replace(strings.Replace(before, rep, "", 1), swe, swe+rep, 1)
	if strings.Count(before, rep) != 1 || strings.Count(before, swe) != 1 ||
		strings.Index(moved, rep) > strings.Index(moved, "name: Sales") {
		t.Fatal("the premise: the revision does not move rep from Sales into Engineering")
	}
	applyReconciled(t, e, moved)
	if repo, err := read("rep"); err != nil || repo.Access != code.AccessRead.String() {
		t.Errorf("rep, moved into Engineering by an activation, reads web as %+v, %v; want read", repo, err)
	}
}

// BenchmarkBuildingTheCodeRoster is the roster's build — paid once an epoch, by
// the first agent code read after an activation, on that read's own goroutine
// and with every agent read arriving meanwhile waiting behind it — over a chart
// of divisions each led by a seat of its own, with ten teams of three seats
// under each that inherit the division's lead: 30 divisions (330 units, 930
// seats) and 100 (1 100 units, 3 100 seats). The figures are at newCodeRoster's
// doc comment.
func BenchmarkBuildingTheCodeRoster(b *testing.B) {
	for _, divisions := range []int{30, 100} {
		o := &org.Organization{Name: "T"}
		for d := range divisions {
			division := &org.Unit{Name: fmt.Sprintf("Division %d", d), Lead: fmt.Sprintf("Lead %d", d),
				Roles: []*org.Role{{Name: fmt.Sprintf("Lead %d", d)}}}
			for t := range 10 {
				team := &org.Unit{Name: fmt.Sprintf("Team %d %d", d, t)}
				for s := range 3 {
					team.Roles = append(team.Roles, &org.Role{Name: fmt.Sprintf("Seat %d %d %d", d, t, s)})
				}
				division.Children = append(division.Children, team)
			}
			o.Units = append(o.Units, division)
		}
		o.Normalize()
		units := 0
		for range o.AllUnits() {
			units++
		}
		b.Run(fmt.Sprintf("%d_units", units), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if r := newCodeRoster(o); r.Lead("Team 0 0") != "lead-0" {
					b.Fatalf("Team 0 0's lead is %q, want lead-0", r.Lead("Team 0 0"))
				}
			}
		})
	}
}
