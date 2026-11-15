package aws

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	"github.com/crewlet/crewlet/internal/seataccess"
)

// A seat's extra grant is one AWS managed policy attached to its role beside
// its level. AWS keeps the catalogue (every managed permissions policy, read
// live), the boundary holds whatever a grant attaches to what any seat may
// ever do, and the policies below are never offered, so a person is never
// shown a grant that would hand out identity, the organization or billing.

// ManagedPolicy is one AWS managed permissions policy, as IAM lists it.
type ManagedPolicy struct {
	ARN, Name, Path string
}

// Write reports a policy that may change anything: every one whose name AWS
// does not mark read-only or view-only, failing closed.
func (p ManagedPolicy) Write() bool {
	return !strings.Contains(p.Name, "ReadOnly") && !strings.Contains(p.Name, "ViewOnly")
}

// Job is a policy AWS writes for a job function (job-function/ViewOnlyAccess).
func (p ManagedPolicy) Job() bool { return p.Path == "/job-function/" }

// GrantablePolicies is every AWS managed permissions policy a seat may be
// granted, all pages, sorted by name: AWS's catalogue less [Refused].
func (c *Client) GrantablePolicies(ctx context.Context) ([]ManagedPolicy, error) {
	var out []ManagedPolicy
	pages := iam.NewListPoliciesPaginator(c.IAM, &iam.ListPoliciesInput{
		Scope: iamtypes.PolicyScopeTypeAws, PolicyUsageFilter: iamtypes.PolicyUsageTypePermissionsPolicy,
		MaxItems: awssdk.Int32(1000),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("aws: list AWS managed policies: %w", err)
		}
		for _, p := range page.Policies {
			mp := ManagedPolicy{ARN: awssdk.ToString(p.Arn), Name: awssdk.ToString(p.PolicyName), Path: awssdk.ToString(p.Path)}
			if !c.conn.awsManaged(mp.ARN) || Refused(mp) != "" {
				continue
			}
			out = append(out, mp)
		}
	}
	slices.SortFunc(out, func(a, b ManagedPolicy) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// refusedPolicies is what no grant offers, by name prefix, and why: identity
// and credentials, the organization and the account, billing, secrets and the
// audit trail, and the two policies that grant every service at once.
//
//nolint:gochecknoglobals // an immutable table, not state
var refusedPolicies = []struct{ prefix, why string }{
	{"AdministratorAccess", "grants every service at once"},
	{"PowerUserAccess", "grants every service at once"},
	{"IAM", "administers identities and credentials"},
	{"AWSSSO", "administers identities and credentials"},
	{"AWSIdentity", "administers identities and credentials"},
	{"AWSOrganizations", "administers the organization"},
	{"AWSControlTower", "administers the organization"},
	{"AWSAccount", "administers the account"},
	{"AWSBilling", "reaches billing"},
	{"AWSBudgets", "reaches billing"},
	{"AWSCostAndUsage", "reaches billing"},
	{"Billing", "reaches billing"},
	{"SecretsManager", "reaches secret values"},
	{"AWSKeyManagementService", "administers encryption keys"},
	{"AWSCloudTrail", "reaches the audit trail"},
}

// Refused is why no seat may be granted p, "" where one may: one of
// [refusedPolicies], or a policy AWS made for a service to use as itself.
func Refused(p ManagedPolicy) string {
	if strings.HasPrefix(p.Path, "/service-role/") || strings.HasPrefix(p.Path, "/aws-service-role/") {
		return "is made for an AWS service to act as itself"
	}
	for _, r := range refusedPolicies {
		if strings.HasPrefix(p.Name, r.prefix) {
			return r.why
		}
	}
	return ""
}

// managedPolicyOf is an AWS managed policy's name and path, read off its ARN.
func managedPolicyOf(arn string) ManagedPolicy {
	_, rest, _ := strings.Cut(arn, ":policy")
	i := strings.LastIndex(rest, "/")
	if i < 0 {
		return ManagedPolicy{ARN: arn, Name: rest, Path: "/"}
	}
	return ManagedPolicy{ARN: arn, Name: rest[i+1:], Path: rest[:i+1]}
}

// jobFunctions is the picker's heading for the policies AWS writes per job.
const jobFunctions = "Job functions"

// GrantCatalogue is every AWS managed policy a seat may be granted beside its
// level ([seataccess.Granter]), read once per session. Each grant applies to
// the whole account the role works in, so a grant names no scope. A stack
// older than version 3 lets the provisioner list none, which offers nothing:
// the outdated stack is the integration's own finding.
func (s *accessSession) GrantCatalogue(ctx context.Context, _ seataccess.Lane) (
	[]seataccess.GrantEntry, []string, error) {

	policies, err := s.grantablePolicies(ctx)
	if err != nil {
		return nil, nil, err
	}
	out := make([]seataccess.GrantEntry, 0, len(policies))
	for _, p := range policies {
		e := seataccess.GrantEntry{ID: p.ARN, Name: p.Name, ScopeKind: seataccess.GrantScopeAccount, Write: p.Write()}
		if p.Job() {
			e.Group = jobFunctions
		}
		out = append(out, e)
	}
	return out, nil, nil
}

// grantablePolicies reads AWS's catalogue once per session.
func (s *accessSession) grantablePolicies(ctx context.Context) ([]ManagedPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.policies != nil {
		return *s.policies, nil
	}
	policies, err := s.client.GrantablePolicies(ctx)
	switch {
	case hasCode(err, "AccessDenied"):
		policies = []ManagedPolicy{}
	case err != nil:
		return nil, s.refused(err)
	}
	s.policies = &policies
	return policies, nil
}

// grantable refuses a grant no seat may hold: anything but an AWS managed
// policy in the role's partition, or one [Refused] names, which the save
// already refuses and a document written around it reaches here.
func (s *accessSession) grantable(arns []string) error {
	for _, arn := range arns {
		p := managedPolicyOf(arn)
		why := Refused(p)
		if !s.client.conn.awsManaged(arn) {
			why = "is not an AWS managed policy in this account's partition"
		}
		if why != "" {
			return &seataccess.Refusal{
				Said:   "AWS: " + p.Name + " " + why + ", so no agent is granted it",
				Remedy: "Remove the grant on the Agents tab", Actor: seataccess.ByYou,
			}
		}
	}
	return nil
}

// grantSteps is an [seataccess.OpGrants] op as steps: one attach per grant
// added and one detach per grant taken off, each naming the policy.
func grantSteps(want seataccess.Desired, have seataccess.Observed) []seataccess.Step {
	step := func(verb seataccess.StepVerb, text string) seataccess.Step {
		return seataccess.Step{Op: seataccess.OpGrants, Verb: verb, By: seataccess.ByCrewlet, Text: text}
	}
	holds := func(list []seataccess.Grant, g seataccess.Grant) bool {
		return slices.ContainsFunc(list, func(h seataccess.Grant) bool { return h.Permission == g.Permission })
	}
	var out []seataccess.Step
	for _, g := range seataccess.NormalGrants(want.Grants) {
		if holds(have.Grants, g) {
			continue
		}
		until := "permanently"
		if !g.ExpiresAt.IsZero() {
			until = "until " + g.ExpiresAt.UTC().Format(time.DateOnly+" 15:04 UTC")
		}
		out = append(out, step(seataccess.VerbGrant, fmt.Sprintf("Attach %s to its role, %s, held to the boundary",
			managedPolicyOf(g.Permission).Name, until)))
	}
	for _, g := range seataccess.NormalGrants(have.Grants) {
		if !holds(want.Grants, g) {
			out = append(out, step(seataccess.VerbUngrant, "Detach "+managedPolicyOf(g.Permission).Name+" from its role"))
		}
	}
	if len(out) == 0 {
		out = append(out, step(seataccess.VerbGrant, "Attach and detach its policies so it holds its level and grants"))
	}
	for i := range out {
		out[i].Index = i
	}
	return out
}

// grantTerms is how an agent reads an AWS grant ([seataccess.GrantTerms]).
//
//nolint:gochecknoglobals // immutable words, not state
var grantTerms = seataccess.GrantTerms{
	Refusal: "`AccessDenied`, `AccessDeniedException` or `UnauthorizedOperation`, naming the action",
	Permission: "the ARN of an AWS managed policy holding the action refused, such as " +
		"`arn:aws:iam::aws:policy/AmazonSQSFullAccess`",
	Account: "the account, within the seat boundary",
}
