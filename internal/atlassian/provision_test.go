package atlassian_test

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// bothProducts is the company that configures Jira and Confluence, which is
// what a seat naming no product of its own is licensed for.
var bothProducts = []atlassian.Product{atlassian.ProductJira, atlassian.ProductConfluence}

// seatEnv is the documented mcp_env shape: four keys, one credential.
func seatEnv(handle string) org.MCPEnv {
	return org.MCPEnv{"atlassian": {
		"JIRA_USERNAME":        "${ATLASSIAN_EMAIL_" + handle + "}",
		"JIRA_API_TOKEN":       "${ATLASSIAN_TOKEN_" + handle + "}",
		"CONFLUENCE_USERNAME":  "${ATLASSIAN_EMAIL_" + handle + "}",
		"CONFLUENCE_API_TOKEN": "${ATLASSIAN_TOKEN_" + handle + "}",
	}}
}

func planFor(t *testing.T, roles []*org.Role, products []atlassian.Product) *atlassian.Plan {
	t.Helper()
	o := &org.Organization{Name: "Acme", Roles: roles}
	o.Normalize()
	plan, err := atlassian.PlanFor(o, &config.Atlassian{OrgID: "org-1"}, products)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestOneCredentialInFourVariablesIsOneSeatAndFourWrites(t *testing.T) {
	t.Parallel()
	// The documented mcp_env shape names the same pair twice, once per
	// product, and every one of those variables has to be written or the
	// seat keeps authenticating through whichever was missed.
	plan := planFor(t, []*org.Role{{Name: "Agent SWE", MCPEnv: seatEnv("SWE")}}, bothProducts)
	if len(plan.Seats) != 1 {
		t.Fatalf("%d seat(s)", len(plan.Seats))
	}
	seat := plan.Seats[0]
	// DEDUPED: two keys pointing at one variable is the ordinary shape,
	// and recording it twice would make a re-run's report claim twice the
	// writes it made.
	if !slices.Equal(seat.TokenVars, []string{"ATLASSIAN_TOKEN_SWE"}) {
		t.Errorf("token vars = %v", seat.TokenVars)
	}
	if !slices.Equal(seat.EmailVars, []string{"ATLASSIAN_EMAIL_SWE"}) {
		t.Errorf("email vars = %v", seat.EmailVars)
	}
	if !seat.Provisionable() {
		t.Error("a seat with both halves is not provisionable")
	}
}

func TestALiteralCredentialIsANoteNamingTheShapeAndNeverTheValue(t *testing.T) {
	t.Parallel()
	// The report is pasted into tickets, and the value here is either a
	// credential or a string containing one.
	plan := planFor(t, []*org.Role{{
		Name: "Agent SWE",
		MCPEnv: org.MCPEnv{"atlassian": {
			"JIRA_API_TOKEN": "ATATT-a-real-looking-secret",
			"JIRA_USERNAME":  "swe@example.com",
		}},
	}}, bothProducts)

	if !plan.Empty() {
		t.Fatalf("a literal credential was planned: %+v", plan.Seats)
	}
	if len(plan.Notes) == 0 {
		t.Fatal("no note")
	}
	joined := strings.Join(plan.Notes, "\n")
	if !strings.Contains(joined, "a literal") {
		t.Errorf("the note does not name the shape:\n%s", joined)
	}
	if strings.Contains(joined, "ATATT-a-real-looking-secret") {
		t.Errorf("THE NOTE REPEATED THE VALUE:\n%s", joined)
	}
}

func TestACompositeReferenceIsRefusedTheSameWay(t *testing.T) {
	t.Parallel()
	// A composite holds a FRAGMENT, so minting into it would replace part
	// of a value that means something else. Its fix differs from a
	// literal's, which is why the two shapes are named apart.
	plan := planFor(t, []*org.Role{{
		Name: "Agent SWE",
		MCPEnv: org.MCPEnv{"atlassian": {
			"JIRA_API_TOKEN": "prefix-${TOKEN_SWE}",
			"JIRA_USERNAME":  "${EMAIL_SWE}",
		}},
	}}, bothProducts)
	if !plan.Empty() {
		t.Fatal("a composite reference was planned")
	}
	if !strings.Contains(strings.Join(plan.Notes, "\n"), "embedded in other text") {
		t.Errorf("notes = %v", plan.Notes)
	}
}

func TestASeatWithATokenAndNoAddressIsRefused(t *testing.T) {
	t.Parallel()
	// Cloud authenticates Basic base64(email:token) and refuses the same
	// token as a bearer, so the address is HALF the credential. A seat
	// provisioned without one would hold a token that is rejected on the
	// scheme — which reads like a bad credential and is not one.
	plan := planFor(t, []*org.Role{{
		Name:   "Agent SWE",
		MCPEnv: org.MCPEnv{"atlassian": {"JIRA_API_TOKEN": "${TOKEN_SWE}"}},
	}}, bothProducts)
	if !plan.Empty() {
		t.Fatal("a seat with no address variable was planned")
	}
	if !strings.Contains(strings.Join(plan.Notes, "\n"), "Basic") {
		t.Errorf("the note does not explain why:\n%v", plan.Notes)
	}
}

func TestTwoSeatsSharingAVariableAreRefusedBeforeAnythingIsCreated(t *testing.T) {
	t.Parallel()
	// THE ONE-ACCOUNT-TWO-SEATS FAILURE. Provisioning both would give one
	// account two identities and leave the other seat authenticating as
	// its colleague, with nothing anywhere reporting it. Catching it at
	// plan time costs nothing, and catching it later costs a rollback.
	shared := org.MCPEnv{"atlassian": {
		"JIRA_API_TOKEN": "${SHARED_TOKEN}",
		"JIRA_USERNAME":  "${SHARED_EMAIL}",
	}}
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{
		{Name: "Agent One", MCPEnv: shared},
		{Name: "Agent Two", MCPEnv: shared},
	}}
	o.Normalize()
	_, err := atlassian.PlanFor(o, &config.Atlassian{OrgID: "org-1"}, bothProducts)
	if err == nil {
		t.Fatal("two seats sharing one variable were planned")
	}
	if !strings.Contains(err.Error(), "SHARED_TOKEN") {
		t.Errorf("the refusal does not name the variable: %v", err)
	}
}

func TestAHumanSeatIsNeverPlanned(t *testing.T) {
	t.Parallel()
	// A human holds their own Atlassian account. Creating one would be a
	// billable licence nobody ever authenticates as.
	plan := planFor(t, []*org.Role{{
		Name:    "Jane Founder",
		Kind:    org.KindHuman,
		Contact: &org.HumanContact{AtlassianAccountID: "5f000000000000000000000a"},
	}}, bothProducts)
	if !plan.Empty() {
		t.Fatalf("a human seat was planned: %+v", plan.Seats)
	}
}

func TestTheProductListIsThreeValuedAndTheEmptyOneMeansNone(t *testing.T) {
	t.Parallel()
	// A product licence is BILLABLE, so all three states are real
	// settings: absent takes every configured product, a list takes
	// exactly those, and an explicit empty list opts the seat out without
	// deleting its mcp_env.
	roles := []*org.Role{
		{Name: "Agent All", MCPEnv: seatEnv("ALL")},
		{Name: "Tech Writer", MCPEnv: seatEnv("DOC"), AtlassianProducts: []string{"confluence"}},
		{Name: "Agent None", MCPEnv: seatEnv("NONE"), AtlassianProducts: []string{}},
	}
	plan := planFor(t, roles, bothProducts)

	byHandle := map[string][]atlassian.Product{}
	for _, seat := range plan.Seats {
		byHandle[seat.Handle] = seat.Products
	}
	if !slices.Equal(byHandle["agent-all"], bothProducts) {
		t.Errorf("an absent block = %v, want every configured product", byHandle["agent-all"])
	}
	if !slices.Equal(byHandle["tech-writer"], []atlassian.Product{atlassian.ProductConfluence}) {
		t.Errorf("a named list = %v", byHandle["tech-writer"])
	}
	if _, planned := byHandle["agent-none"]; planned {
		t.Error("an explicit empty product list was still planned")
	}
	if !strings.Contains(strings.Join(plan.Notes, "\n"), "agent-none") {
		t.Errorf("the opted-out seat was skipped silently:\n%v", plan.Notes)
	}
}

func TestASeatIsNarrowedToWhatTheCompanyActuallyConfigures(t *testing.T) {
	t.Parallel()
	// A seat naming Confluence in a company with no Confluence would
	// otherwise be licensed into a site that is not there — and the
	// licence attempt fails on a cloud id the config never had.
	plan := planFor(t, []*org.Role{
		{Name: "Tech Writer", MCPEnv: seatEnv("DOC"), AtlassianProducts: []string{"confluence"}},
	}, []atlassian.Product{atlassian.ProductJira})
	if !plan.Empty() {
		t.Fatalf("a seat was licensed for a product the company does not have: %+v", plan.Seats)
	}
}

func TestTheProductOrderIsTheCanonicalOneWhateverTheYAMLSaid(t *testing.T) {
	t.Parallel()
	// Downstream these lists are compared element by element, so an order
	// inherited from whoever wrote the YAML would make two equal sets look
	// different and re-mint on every run.
	plan := planFor(t, []*org.Role{{
		Name: "Agent SWE", MCPEnv: seatEnv("SWE"),
		AtlassianProducts: []string{"confluence", "jira"},
	}}, bothProducts)
	if !slices.Equal(plan.Seats[0].Products, bothProducts) {
		t.Fatalf("products = %v, want the canonical order", plan.Seats[0].Products)
	}
}

func TestThePlanIsOrderedByHandle(t *testing.T) {
	t.Parallel()
	// The report is read side by side with a previous run's, and a plan
	// whose order came from a map iteration cannot be compared with
	// anything.
	plan := planFor(t, []*org.Role{
		{Name: "Zeta", MCPEnv: seatEnv("Z")},
		{Name: "Alpha", MCPEnv: seatEnv("A")},
		{Name: "Mid", MCPEnv: seatEnv("M")},
	}, bothProducts)
	var handles []string
	for _, seat := range plan.Seats {
		handles = append(handles, seat.Handle)
	}
	if !slices.Equal(handles, []string{"alpha", "mid", "zeta"}) {
		t.Fatalf("handles = %v", handles)
	}
}

func TestTheAccountNameAndDescriptionAreBoundedAndSayWhoseTheyAre(t *testing.T) {
	t.Parallel()
	// Atlassian's own limits: exceeding either is a 400 half way through a
	// run. The prefix is what marks an account as this company's and is
	// the join a re-run adopts by, so it is always present.
	seat := atlassian.SeatPlan{Handle: "swe", Role: strings.Repeat("Very Long Role ", 20), Goal: "ship things"}
	name := atlassian.DisplayName("Crewlet", seat)
	if len(name) > 100 || !strings.HasPrefix(name, "Crewlet ") {
		t.Errorf("display name = %q (%d bytes)", name, len(name))
	}
	description := atlassian.Description(atlassian.SeatPlan{
		Handle: "swe", Role: "Agent SWE", Goal: strings.Repeat("goal ", 200),
	})
	if len(description) > 500 || !strings.Contains(description, "Managed by Crewlet") {
		t.Errorf("description = %q (%d bytes)", description, len(description))
	}
	// A seat with no goal still gets a description that says what it is.
	if got := atlassian.Description(atlassian.SeatPlan{Handle: "swe", Role: "Agent SWE"}); !strings.Contains(got, "Agent SWE") {
		t.Errorf("description with no goal = %q", got)
	}
	// A seat with no role name falls back to the handle rather than
	// producing an account called nothing but the prefix.
	if got := atlassian.DisplayName("Crewlet", atlassian.SeatPlan{Handle: "swe"}); got != "Crewlet swe" {
		t.Errorf("display name with no role = %q", got)
	}
}

func TestTheContainerWalkCoversTheOrgWideKnowledgeScope(t *testing.T) {
	t.Parallel()
	// A seat that cannot READ knowledge.confluence_spaces gets an empty
	// knowledge block on every turn, and an empty block is
	// indistinguishable from a company that has written nothing down. The
	// tracker has no counterpart — there is no org-wide Jira read scope.
	o := &org.Organization{
		Name:             "Acme",
		ConfluenceSpaces: []string{"handbook"},
		Roles:            []*org.Role{{Name: "CEO", JiraProject: "lead", ConfluenceSpace: "lead"}},
		Units: []*org.Unit{{
			Name: "Engineering", JiraProject: "eng", ConfluenceSpace: "eng",
			Roles: []*org.Role{{Name: "Agent SWE", MCPEnv: seatEnv("SWE")}},
		}},
	}
	o.Normalize()
	if got := atlassian.ContainersOf(o, atlassian.ProductJira); !slices.Equal(got, []string{"ENG", "LEAD"}) {
		t.Errorf("jira containers = %v", got)
	}
	if got := atlassian.ContainersOf(o, atlassian.ProductConfluence); !slices.Equal(got, []string{"ENG", "HANDBOOK", "LEAD"}) {
		t.Errorf("confluence containers = %v", got)
	}
}

func TestPlanForRefusesACompanyWithNothingToProvisionInto(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Acme"}
	o.Normalize()
	if _, err := atlassian.PlanFor(o, nil, bothProducts); err == nil {
		t.Error("a company with no atlassian block was planned")
	}
	if _, err := atlassian.PlanFor(o, &config.Atlassian{OrgID: "o"}, nil); err == nil {
		t.Error("a company with no configured product was planned")
	}
}

func TestTheConfigDefaultMatchesTheVendorsOwn(t *testing.T) {
	t.Parallel()
	// internal/config restates the token lifetime rather than importing it
	// — config imports no vendor package, which is what lets every vendor
	// package import config. This is the assertion that keeps the two
	// numbers from drifting apart in silence.
	var unset config.Atlassian
	if got := unset.ExpiryDays(); got != atlassian.DefaultTokenExpiryDays {
		t.Fatalf("config default %d, vendor default %d", got, atlassian.DefaultTokenExpiryDays)
	}
	if got := unset.Prefix(); got == "" {
		t.Fatal("the display-name prefix defaulted to empty, which would make " +
			"-decommission propose deleting every account in the organization")
	}
}

func TestOneLiteralKeyMakesTheWholeSeatUnprovisionable(t *testing.T) {
	t.Parallel()
	// NOT just that key. Minting into the referenced ones would leave the
	// literal holding a credential nothing rotates — and everything about
	// the seat would look freshly provisioned while one of its tool
	// servers quietly authenticates with the stale value.
	plan := planFor(t, []*org.Role{{
		Name: "Agent SWE",
		MCPEnv: org.MCPEnv{"atlassian": {
			"JIRA_USERNAME":        "${ATLASSIAN_EMAIL_SWE}",
			"JIRA_API_TOKEN":       "${ATLASSIAN_TOKEN_SWE}",
			"CONFLUENCE_USERNAME":  "${ATLASSIAN_EMAIL_SWE}",
			"CONFLUENCE_API_TOKEN": "ATATT-managed-by-hand",
		}},
	}}, bothProducts)

	if !plan.Empty() {
		t.Fatalf("a seat with one literal key was provisioned into its others: %+v",
			plan.Seats)
	}
	joined := strings.Join(plan.Notes, "\n")
	if !strings.Contains(joined, "CONFLUENCE_API_TOKEN") {
		t.Errorf("the note does not name the offending key:\n%s", joined)
	}
	if strings.Contains(joined, "ATATT-managed-by-hand") {
		t.Errorf("THE NOTE REPEATED THE VALUE:\n%s", joined)
	}
}

func TestABasicHeaderIsHandManagedRatherThanAMistake(t *testing.T) {
	t.Parallel()
	// Its payload is ALREADY base64(email:token), so there is no ${VAR}
	// under it to mint into — the seat is managed by hand, and a note
	// telling the operator to "point it at a variable" would have them
	// break a working HTTP-MCP config.
	plan := planFor(t, []*org.Role{{
		Name:   "Agent SWE",
		MCPEnv: org.MCPEnv{"atlassian": {"Authorization": "Basic ${ATLASSIAN_BASIC_SWE}"}},
	}}, bothProducts)

	if !plan.Empty() {
		t.Fatalf("a Basic header was treated as provisionable: %+v", plan.Seats)
	}
	joined := strings.Join(plan.Notes, "\n")
	if !strings.Contains(joined, "managed by hand") {
		t.Errorf("the note does not say what the seat is:\n%s", joined)
	}
	if strings.Contains(joined, "embedded in other text") {
		t.Errorf("a Basic header was reported as a malformed reference:\n%s", joined)
	}
}

func TestASeatWithATokenInTwoBlocksHasBothMinted(t *testing.T) {
	t.Parallel()
	// A seat running two single-product MCP servers has a variable in
	// each. Minting into the first and walking away leaves the second
	// server starting with an unset credential — no error, no note, and
	// the silent half-provisioned seat this grammar exists to end.
	plan := planFor(t, []*org.Role{{
		Name: "Agent SWE",
		MCPEnv: org.MCPEnv{
			"jira": {
				"JIRA_USERNAME":  "${ATLASSIAN_EMAIL_SWE}",
				"JIRA_API_TOKEN": "${JIRA_TOKEN_SWE}",
			},
			"confluence": {
				"CONFLUENCE_USERNAME":  "${ATLASSIAN_EMAIL_SWE}",
				"CONFLUENCE_API_TOKEN": "${CONFLUENCE_TOKEN_SWE}",
			},
		},
	}}, bothProducts)

	if len(plan.Seats) != 1 {
		t.Fatalf("%d seat(s): %v", len(plan.Seats), plan.Notes)
	}
	got := plan.Seats[0].TokenVars
	slices.Sort(got)
	if !slices.Equal(got, []string{"CONFLUENCE_TOKEN_SWE", "JIRA_TOKEN_SWE"}) {
		t.Fatalf("token vars = %v, want both blocks' variables", got)
	}
}

func TestEveryAgentSeatIsManagedWhetherOrNotItIsProvisionable(t *testing.T) {
	t.Parallel()
	// THE -decommission KEEP-SET, and it is deliberately wider than the
	// plan. A seat that opted out of every product, or whose credential is
	// managed by hand, still HAS an account from an earlier run — and
	// sweeping it because the plan no longer carries the seat is an
	// irreversible delete of an identity that owns issues.
	plan := planFor(t, []*org.Role{
		{Name: "Agent SWE", MCPEnv: seatEnv("SWE")},
		{Name: "Opted Out", MCPEnv: seatEnv("OUT"), AtlassianProducts: []string{}},
		{Name: "By Hand", MCPEnv: org.MCPEnv{"atlassian": {
			"JIRA_API_TOKEN": "a-literal", "JIRA_USERNAME": "x@example.com",
		}}},
		{Name: "No Tracker Work"},
		{Name: "Jane Founder", Kind: org.KindHuman,
			Contact: &org.HumanContact{AtlassianAccountID: "5f00000000000000000000aa"}},
	}, bothProducts)

	if len(plan.Seats) != 1 {
		t.Fatalf("%d seat(s) planned, want only the provisionable one", len(plan.Seats))
	}
	want := []string{
		atlassian.NormalizeName("Crewlet Agent SWE"),
		atlassian.NormalizeName("Crewlet By Hand"),
		atlassian.NormalizeName("Crewlet No Tracker Work"),
		atlassian.NormalizeName("Crewlet Opted Out"),
	}
	if !slices.Equal(plan.Managed, want) {
		t.Fatalf("managed = %v, want every AGENT seat's account name (%v)",
			plan.Managed, want)
	}
	// A human seat is not managed: they hold their own account, and this
	// list is what a sweep would DELETE.
	for _, name := range plan.Managed {
		if strings.Contains(name, "founder") {
			t.Fatal("a human seat's account is in the sweep's keep-set, which "+
				"means the sweep considered deleting it", name)
		}
	}
}

func TestTwoSeatsWithOneRoleNameAreRefusedBeforeAnythingIsCreated(t *testing.T) {
	t.Parallel()
	// The org chart enforces unique HANDLES, not unique role names: two
	// roles in different units may be called the same thing and differ only
	// by a declared handle. The account's display name is built from the
	// role name, and it is the only field both sides control — Atlassian
	// assigns the id and the address — so it is what a re-run adopts by.
	//
	// Left alone, run 1 creates two service accounts called the same thing
	// and run 2 adopts them in whatever order the organization lists them:
	// each seat mints over the other's credential, both keep working, and
	// nothing says which identity is filing which issue.
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{
		{Name: "Agent SWE", DeclaredHandle: "swe-platform", MCPEnv: seatEnv("PLATFORM")},
		{Name: "Agent SWE", DeclaredHandle: "swe-payments", MCPEnv: seatEnv("PAYMENTS")},
	}}
	o.Normalize()
	_, err := atlassian.PlanFor(o, &config.Atlassian{OrgID: "org-1"}, bothProducts)
	if err == nil {
		t.Fatal("two seats that would share one account name were planned")
	}
	for _, want := range []string{"swe-platform", "swe-payments", "Crewlet Agent SWE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
}

func TestAPrefixedNameIsStillOneAccountPerSeatWhenOnlyTheHandlesDiffer(t *testing.T) {
	t.Parallel()
	// The other side of the refusal above: distinct role names are the
	// ordinary shape and must plan cleanly, prefix and all.
	plan := planFor(t, []*org.Role{
		{Name: "Agent SWE", MCPEnv: seatEnv("SWE")},
		{Name: "Agent QA", MCPEnv: seatEnv("QA")},
	}, bothProducts)
	if len(plan.Seats) != 2 {
		t.Fatalf("%d seat(s) planned: %+v", len(plan.Seats), plan.Seats)
	}
	names := map[string]bool{}
	for _, seat := range plan.Seats {
		names[atlassian.DisplayName("Crewlet", seat)] = true
	}
	if len(names) != 2 {
		t.Fatalf("two seats produced %d account name(s): %v", len(names), names)
	}
}

func TestANonAsciiRoleNameIsBoundedByCharactersRatherThanBytes(t *testing.T) {
	t.Parallel()
	// Atlassian's caps are on CHARACTERS. Slicing bytes cut a name that was
	// well inside the limit, and cut it MID-RUNE — so the display name went
	// on the wire as invalid UTF-8, which is either a 400 half way through a
	// run or, worse, a name that can never normalise-match on the next run's
	// adoption, giving that seat a second billable account every time.
	//
	// Sixty Cyrillic characters: inside Atlassian's 100, and 120 bytes.
	name := strings.Repeat("Инженер", 8) + "Аген" // 60 runes, 120 bytes
	if utf8.RuneCountInString(name) != 60 || len(name) != 120 {
		t.Fatalf("fixture is %d runes / %d bytes", utf8.RuneCountInString(name), len(name))
	}
	seat := atlassian.SeatPlan{Handle: "agent", Role: name}
	display := atlassian.DisplayName("Crewlet", seat)

	if !utf8.ValidString(display) {
		t.Fatalf("the display name is not valid UTF-8: %q", display)
	}
	// "Crewlet " + 60 runes = 68 characters, so nothing should have been cut.
	if !strings.HasSuffix(display, name) {
		t.Errorf("a name inside the limit was truncated: %q", display)
	}
	// And a name genuinely over the limit is cut on a rune boundary.
	long := strings.Repeat("Ж", 300)
	cut := atlassian.DisplayName("Crewlet", atlassian.SeatPlan{Handle: "h", Role: long})
	if !utf8.ValidString(cut) {
		t.Fatalf("a truncated name is not valid UTF-8: %q", cut)
	}
	if n := utf8.RuneCountInString(cut); n != 100 {
		t.Errorf("truncated to %d characters, want Atlassian's 100", n)
	}
}
