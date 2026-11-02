package builtin_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/worklet"
)

// handleChart is a chart whose one relation is between HANDLES: `lead` leads
// `dev`, and nothing else leads anybody. It answers only what it is asked, by
// the exact string, which is the point — a relation asked with a display name
// is asked about somebody the chart has never heard of.
type handleChart struct{}

func (handleChart) Leads(_ context.Context, actor, subject string) (bool, error) {
	return actor == "lead" && subject == "dev", nil
}

func (handleChart) LeadsProject(context.Context, string, string) (bool, error) {
	return false, nil
}

func (handleChart) LeadsUnit(context.Context, string, string) (bool, error) {
	return false, nil
}

func (handleChart) LeadsContainer(context.Context, string, string) (bool, error) {
	return false, nil
}

// LeadsAnyone is [handleChart.Leads] asked of every subject: `lead` leads
// somebody, and nobody else does.
func (handleChart) LeadsAnyone(_ context.Context, actor string) (bool, error) {
	return actor == "lead", nil
}

// MemberOf answers that nobody sits in any unit: no case here shares an
// artifact with a team.
func (handleChart) MemberOf(context.Context, string, string) (bool, error) {
	return false, nil
}

// SeatGranted answers that no role carries a grant: no case here asks a verb
// that names one.
func (handleChart) SeatGranted(context.Context, string, authz.SeatGrant) (bool, error) {
	return false, nil
}

// A LEAD NAMES A REPORT THE WAY A MODEL TYPES THEM, and is admitted.
//
// `set_priorities` resolves its `handle` against the chart — a model types
// what it remembers, a role name as often as a handle — and then asks the lead
// relation of the seat that resolves to. The registration gate used to decide
// FIRST, on the raw argument, so a lead writing "Staff Engineer" was refused
// as leading nobody of that name before the tool ever resolved it, while the
// tool's own comment said the gate left the owner unnamed.
//
// Through [builtin.Decide] over a chart that knows only handles, so the one
// relation decision is the one taken after the resolution. And the other half:
// naming by role somebody the caller does NOT lead is still refused — the gate
// stepping aside is only safe because the tool asks.
func TestALeadNamesAReportTheWayAModelTypesThem(t *testing.T) {
	t.Parallel()
	dev := &org.Role{Name: "Staff Engineer", DeclaredHandle: "dev"}
	lead := &org.Role{Name: "Engineering Lead", DeclaredHandle: "lead"}
	ops := &org.Role{Name: "Site Reliability", DeclaredHandle: "ops"}
	company := &org.Organization{Name: "Nimbus", Roles: []*org.Role{dev, lead, ops}}
	caller := iam.WithPrincipal(context.Background(), iam.Principal{
		ID: uuid.New(), Kind: iam.KindPerson, Login: "lead.person", Seat: "lead",
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantWorklet},
	})

	for _, c := range []struct {
		name    string
		handle  string
		allowed bool
		whose   string
	}{
		{"a report named by role", "Staff Engineer", true, "dev"},
		{"a report named by handle", "dev", true, "dev"},
		{"somebody else named by role", "Site Reliability", false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			person := &personSpy{}
			trk := newFakeTracker()
			var got string
			var failed bool
			var cause error
			for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
				Work: builtin.WorkDeps{
					Reader: trk, Writer: trk.as,
					PersonWriter: func(builtin.Actor) builtin.PersonWriter { return person },
					Seats:        func() []colleague.Seat { return builtin.Corpus(company, nil, nil) },
					Actor:        builtin.PrincipalActor,
				},
				Authorize: builtin.Decide(handleChart{}),
			}) {
				if tool.Name() != worklet.SetPrioritiesTool {
					continue
				}
				res, err := tool.Call(caller, map[string]any{
					"handle": c.handle, "items": []any{"ENG-1"},
				})
				if err != nil {
					t.Fatalf("set_priorities: %v", err)
				}
				got, failed, cause = res.Output, res.Failed, res.Cause
			}
			switch {
			case c.allowed && failed:
				t.Fatalf("the lead was refused naming %q: %s", c.handle, got)
			case c.allowed && person.handle != c.whose:
				t.Fatalf("the queue written is %q's, want %q's", person.handle, c.whose)
			case !c.allowed && (!failed || person.handle != ""):
				t.Fatalf("a caller leading nobody named %q wrote their queue: %s",
					c.handle, got)
			case !c.allowed && !errors.Is(cause, builtin.ErrRefused):
				t.Fatalf("the refusal carries %v, not the authority's own answer", cause)
			}
		})
	}
}
