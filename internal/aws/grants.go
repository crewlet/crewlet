package aws

import (
	"context"
	"fmt"
	"slices"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
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
