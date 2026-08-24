package gitlab_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/org"
)

func provisioningOrg(t *testing.T, roles ...*org.Role) *org.Organization {
	t.Helper()
	return &org.Organization{Name: "Nimbus", Roles: roles}
}

func agentSeat(name string, env map[string]string) *org.Role {
	r := &org.Role{Name: name}
	if env != nil {
		r.MCPEnv = map[string]map[string]string{gitlab.SeatEnv: env}
	}
	return r
}

func enabledGitLab() *config.GitLab {
	return &config.GitLab{
		Enabled: true, URL: "https://gitlab.example.com",
		SigningSecret: "${GITLAB_SIGNING_SECRET}",
		Provisioning:  &config.GitLabProvisioning{Group: "nimbus"},
	}
}

// THE SCAN MINTS INTO THE VARIABLE THE CONFIG POINTS AT, under whichever key
// the seat's tool stack names it. Inventing a variable of the engine's own
// would mint a token the seat's tools never read.
func TestEveryCredentialSpellingIsFound(t *testing.T) {
	t.Parallel()
	for _, key := range gitlab.CredentialKeys {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			value := "${GITLAB_TOKEN_SWE}"
			if key == "Authorization" {
				// A whole reference wearing a scheme, which is how an
				// OAuth-shaped MCP server takes it — and which the engine
				// strips when it reads the value.
				value = "Bearer ${GITLAB_TOKEN_SWE}"
			}
			o := provisioningOrg(t, agentSeat("SWE", map[string]string{key: value}))
			plan, err := gitlab.PlanFor(o, enabledGitLab())
			if err != nil {
				t.Fatalf("PlanFor: %v", err)
			}
			if len(plan.Seats) != 1 {
				t.Fatalf("planned %d seats for %s, want 1: %+v", len(plan.Seats), key, plan)
			}
			if plan.Seats[0].TokenVar != "GITLAB_TOKEN_SWE" {
				t.Errorf("token var = %q", plan.Seats[0].TokenVar)
			}
		})
	}
}

// A LITERAL IS A NOTE, NOT A FAILURE. A seat whose token is written out is
// one an operator manages by hand — supported, just not provisionable, since
// rewriting the company config from a provisioning run is not this command's
// job.
func TestALiteralTokenIsReportedAndSkipped(t *testing.T) {
	t.Parallel()
	o := provisioningOrg(t,
		agentSeat("SWE", map[string]string{"GITLAB_TOKEN": "glpat-written-out"}),
		agentSeat("CTO", map[string]string{"GITLAB_TOKEN": "${GITLAB_TOKEN_CTO}"}),
	)
	plan, err := gitlab.PlanFor(o, enabledGitLab())
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	if len(plan.Seats) != 1 || plan.Seats[0].Handle != "cto" {
		t.Fatalf("planned %+v, want only the referenced seat", plan.Seats)
	}
	if len(plan.Notes) != 1 || !strings.Contains(plan.Notes[0], "swe") {
		t.Fatalf("notes = %v, want the literal seat reported", plan.Notes)
	}
	// THE NOTE MUST NOT CARRY THE CREDENTIAL. It is printed in a report an
	// operator pastes into a ticket.
	if strings.Contains(plan.Notes[0], "glpat-written-out") {
		t.Errorf("the note leaked the literal token: %q", plan.Notes[0])
	}
}

// A COMPOSITE REFERENCE IS ALSO REFUSED: the variable holds a fragment, so
// minting a token into it would replace part of something else.
func TestACompositeReferenceIsRefused(t *testing.T) {
	t.Parallel()
	o := provisioningOrg(t, agentSeat("SWE", map[string]string{
		"Authorization": "Bearer ${PREFIX}-${GITLAB_TOKEN_SWE}",
	}))
	plan, err := gitlab.PlanFor(o, enabledGitLab())
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	if len(plan.Seats) != 0 {
		t.Fatalf("planned %+v for a composite reference", plan.Seats)
	}
	if len(plan.Notes) != 1 {
		t.Fatalf("notes = %v, want the composite reported", plan.Notes)
	}
}

// A HUMAN SEAT HOLDS NO TOOL CREDENTIAL. Minting one would create an account
// nothing ever authenticates as.
func TestHumanSeatsAreNotProvisioned(t *testing.T) {
	t.Parallel()
	human := agentSeat("Founder", map[string]string{"GITLAB_TOKEN": "${GITLAB_TOKEN_FOUNDER}"})
	human.Kind = org.KindHuman
	o := provisioningOrg(t, human)
	plan, err := gitlab.PlanFor(o, enabledGitLab())
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	if !plan.Empty() {
		t.Fatalf("planned %+v for a human seat", plan.Seats)
	}
}

// A SEAT WITH NO GITLAB BLOCK IS NOT A SEAT THIS COMMAND OWNS.
func TestSeatsWithoutAGitLabBlockAreIgnored(t *testing.T) {
	t.Parallel()
	o := provisioningOrg(t,
		agentSeat("CEO", nil),
		agentSeat("PM", map[string]string{"UNRELATED": "${SOMETHING}"}),
	)
	plan, err := gitlab.PlanFor(o, enabledGitLab())
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	if !plan.Empty() || len(plan.Notes) != 0 {
		t.Fatalf("plan = %+v, want nothing at all", plan)
	}
}

// THE PLAN IS ORDERED, because a report is read beside a previous run's and
// one whose order came from a map iteration cannot be compared with
// anything.
func TestThePlanIsOrderedByHandle(t *testing.T) {
	t.Parallel()
	o := provisioningOrg(t,
		agentSeat("Zeta", map[string]string{"GITLAB_TOKEN": "${T_Z}"}),
		agentSeat("Alpha", map[string]string{"GITLAB_TOKEN": "${T_A}"}),
		agentSeat("Mid", map[string]string{"GITLAB_TOKEN": "${T_M}"}),
	)
	plan, err := gitlab.PlanFor(o, enabledGitLab())
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	var handles []string
	for _, s := range plan.Seats {
		handles = append(handles, s.Handle)
	}
	if strings.Join(handles, ",") != "alpha,mid,zeta" {
		t.Fatalf("order = %v", handles)
	}
}

// AN ACCOUNT ADDRESS IS UNDELIVERABLE ON PURPOSE. The account is a robot,
// and one that looked deliverable would eventually have somebody's
// notification sent to it.
func TestServiceAccountAddressesAreUndeliverable(t *testing.T) {
	t.Parallel()
	o := provisioningOrg(t, agentSeat("SWE", map[string]string{"GITLAB_TOKEN": "${T}"}))
	plan, err := gitlab.PlanFor(o, enabledGitLab())
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	email := plan.Seats[0].Email
	if !strings.Contains(email, "swe") {
		t.Errorf("email %q does not name the seat", email)
	}
	if !strings.HasSuffix(email, ".invalid") {
		t.Errorf("email %q is not in a reserved undeliverable domain", email)
	}
}

func TestProvisioningNeedsAnEnabledIntegrationAndABlock(t *testing.T) {
	t.Parallel()
	o := provisioningOrg(t, agentSeat("SWE", map[string]string{"GITLAB_TOKEN": "${T}"}))
	if _, err := gitlab.PlanFor(o, nil); err == nil {
		t.Error("a nil integration was accepted")
	}
	if _, err := gitlab.PlanFor(o, &config.GitLab{}); err == nil {
		t.Error("a disabled integration was accepted")
	}
	noBlock := enabledGitLab()
	noBlock.Provisioning = nil
	if _, err := gitlab.PlanFor(o, noBlock); err == nil {
		t.Error("an integration with no provisioning block was accepted")
	}
}

// A COMPOSITE'S NOTE MUST NOT LEAK EITHER, and it says something different
// from a literal's: the two have different fixes, so collapsing them into
// "not a reference" leaves an operator to work out which.
func TestTheNotesSayTheShapeAndNotTheValue(t *testing.T) {
	t.Parallel()
	o := provisioningOrg(t,
		agentSeat("SWE", map[string]string{"GITLAB_TOKEN": "glpat-secret-literal"}),
		agentSeat("CTO", map[string]string{
			"GITLAB_TOKEN": "prefix-${GITLAB_TOKEN_CTO}-suffix",
		}),
	)
	plan, err := gitlab.PlanFor(o, enabledGitLab())
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	joined := strings.Join(plan.Notes, "\n")
	for _, leaked := range []string{"glpat-secret-literal", "prefix-", "-suffix"} {
		if strings.Contains(joined, leaked) {
			t.Errorf("the notes leaked %q:\n%s", leaked, joined)
		}
	}
	if !strings.Contains(joined, "a literal") {
		t.Errorf("the literal seat's note does not say so:\n%s", joined)
	}
	if !strings.Contains(joined, "embedded in other text") {
		t.Errorf("the composite seat's note does not say so:\n%s", joined)
	}
}
