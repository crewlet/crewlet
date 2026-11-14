package aws

import (
	"context"
	"fmt"
	"slices"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
)

// StackReport is what the Test button reads from the account, as the
// provisioner and with reads only.
type StackReport struct {
	// Version is the stack's crewlet:stack-version tag, as its provisioner
	// role carries it.
	Version string

	// Drift is every Crewlet object whose document differs from what this
	// engine renders, each a sentence naming the object.
	Drift []string

	// Roles and RolesQuota are the account's IAM role count and its quota;
	// both zero where the summary could not be read.
	Roles, RolesQuota int

	// Split is every certificate identity that opened what it must not, each
	// a sentence. Empty where each was refused.
	Split []string

	// Atlas is every way the atlas's role differs from what this engine
	// renders, each a sentence; empty where the company has not asked for the
	// role. Apart from Drift, since no agent depends on it.
	Atlas []string
}

// Outdated reports a stack rendered by another version of this engine.
func (r StackReport) Outdated() bool { return r.Version != StackVersion }

// CheckStack holds the account's Crewlet objects to what this engine renders
// for regions: the boundary, the level policies, and the provisioner's and the
// broker's trust and single inline policy, with no managed policy beside it.
// Then it proves the identity split live: a seat's certificate cannot become
// the provisioner, and the provisioner's cannot become the broker. With atlas,
// it holds the atlas's role to its trust and policy too, and proves that no
// other identity opens it and that it opens no other. An error is a read that
// failed; what differs is in the report.
func (c *Client) CheckStack(ctx context.Context, regions []string, atlas bool) (StackReport, error) {
	var r StackReport
	for _, name := range append([]string{boundaryName}, levelNames()...) {
		want := levelDocOrBoundary(name, regions)
		problem, err := c.checkPolicy(ctx, name, want)
		if err != nil {
			return r, err
		}
		if problem != "" {
			r.Drift = append(r.Drift, problem)
		}
	}
	issuer := c.conn.CA.Cert.Subject.CommonName
	for _, role := range []struct {
		name         string
		trust, owned obj
	}{
		{provisionerName, provisionerTrustDoc(issuer), provisionerDoc()},
		{brokerName, brokerTrustDoc(issuer), brokerDoc()},
	} {
		problems, version, err := c.checkOwnRole(ctx, c.conn.resolve(role.name), role.trust, role.owned)
		if err != nil {
			return r, err
		}
		r.Drift = append(r.Drift, problems...)
		if role.name == provisionerName {
			r.Version = version
		}
	}
	if sum, err := c.IAM.GetAccountSummary(ctx, &iam.GetAccountSummaryInput{}); err == nil {
		r.Roles, r.RolesQuota = int(sum.SummaryMap["Roles"]), int(sum.SummaryMap["RolesQuota"])
	}
	r.Split = c.checkSplit(ctx)
	if atlas {
		problems, err := c.checkAtlas(ctx, issuer)
		if err != nil {
			return r, err
		}
		r.Atlas = problems
		if c.conn.HasAtlas() {
			r.Split = append(r.Split, c.checkAtlasSplit(ctx)...)
		}
	}
	return r, nil
}

// checkAtlas holds the atlas's role to its trust and its one inline policy.
// A stack or an output without the role is a sentence, never an error: the
// company asked for it and has not run the block since.
func (c *Client) checkAtlas(ctx context.Context, issuer string) ([]string, error) {
	name := c.conn.resolve(atlasName)
	if !c.conn.HasAtlas() {
		return []string{"The stack output does not carry the Atlas reader " + name}, nil
	}
	problems, _, err := c.checkOwnRole(ctx, name, atlasTrustDoc(issuer), atlasDoc())
	if hasCode(err, "NoSuchEntity") {
		return []string{"The role " + name + " is missing"}, nil
	}
	return problems, err
}

// checkAtlasSplit asks for what the atlas's identity must not have, and for
// the atlas's role under the other two identities. Each must be refused.
func (c *Client) checkAtlasSplit(ctx context.Context) []string {
	var out []string
	conn := c.conn
	try := func(sentence, cn, profile, role string) {
		if _, err := c.ra.createSession(ctx, conn.CA, cn, profile, role, conn.TrustAnchorARN); err == nil {
			out = append(out, sentence)
		}
	}
	atlas := conn.resolve(atlasCN)
	only := "the atlas's trust must admit only " + atlas + "."
	try("The atlas's certificate can become the provisioner: the provisioner's trust must admit only "+
		conn.resolve(provisionerCN)+".", atlas, conn.ProvisionerProfileARN, conn.ProvisionerRoleARN)
	try("The atlas's certificate can become the broker: the broker's trust must admit only "+
		strings.TrimSuffix(conn.resolve(seatCNPrefix), "-")+"-* names.", atlas, conn.SeatProfileARN, conn.BrokerRoleARN)
	try("The provisioner's certificate can become the atlas: "+only,
		conn.resolve(provisionerCN), conn.AtlasProfileARN, conn.AtlasRoleARN)
	try("A seat's certificate can become the atlas: "+only,
		seatCN(conn.Company, "test-split"), conn.AtlasProfileARN, conn.AtlasRoleARN)
	return out
}

func levelDocOrBoundary(name string, regions []string) obj {
	if name == boundaryName {
		return boundaryDoc(regions)
	}
	return levelDoc(name)
}

// checkPolicy compares one of Crewlet's managed policies' default version
// with want, answering the difference as a sentence.
func (c *Client) checkPolicy(ctx context.Context, name string, want obj) (string, error) {
	arn := c.conn.resolve(policyARN(name))
	pol, err := c.IAM.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: awssdk.String(arn)})
	if hasCode(err, "NoSuchEntity") {
		return "The policy " + c.conn.resolve(name) + " is missing.", nil
	}
	if err != nil {
		return "", fmt.Errorf("aws: read policy %s: %w", c.conn.resolve(name), err)
	}
	ver, err := c.IAM.GetPolicyVersion(ctx, &iam.GetPolicyVersionInput{
		PolicyArn: awssdk.String(arn), VersionId: pol.Policy.DefaultVersionId,
	})
	if err != nil {
		return "", fmt.Errorf("aws: read policy %s: %w", c.conn.resolve(name), err)
	}
	if same, hint := samePolicy(awssdk.ToString(ver.PolicyVersion.Document), concrete(want, c.conn.vars())); !same {
		return "The policy " + c.conn.resolve(name) + " is not the one this engine expects (" + hint + ").", nil
	}
	return "", nil
}

// checkOwnRole holds the provisioner or the broker to its trust and its one
// inline policy, named for the role, with no managed policy, and reads its
// stack version tag.
func (c *Client) checkOwnRole(ctx context.Context, name string, trust, owned obj) ([]string, string, error) {
	got, err := c.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: awssdk.String(name)})
	if err != nil {
		return nil, "", fmt.Errorf("aws: read role %s: %w", name, err)
	}
	var problems []string
	version := ""
	for _, t := range got.Role.Tags {
		if awssdk.ToString(t.Key) == versionTag {
			version = awssdk.ToString(t.Value)
		}
	}
	if same, hint := samePolicy(awssdk.ToString(got.Role.AssumeRolePolicyDocument), concrete(trust, c.conn.vars())); !same {
		problems = append(problems, "The trust policy of "+name+" is not the one this engine expects ("+hint+").")
	}
	inline, err := c.IAM.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{RoleName: awssdk.String(name)})
	if err != nil {
		return nil, "", fmt.Errorf("aws: list role %s's policies: %w", name, err)
	}
	managed, err := c.IAM.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{RoleName: awssdk.String(name)})
	if err != nil {
		return nil, "", fmt.Errorf("aws: list role %s's policies: %w", name, err)
	}
	if len(managed.AttachedPolicies) > 0 || !slices.Equal(inline.PolicyNames, []string{name}) {
		problems = append(problems, fmt.Sprintf("The role %s holds %d managed and %d inline policies; it should "+
			"hold only its own inline policy %s.", name, len(managed.AttachedPolicies), len(inline.PolicyNames), name))
	}
	if slices.Contains(inline.PolicyNames, name) {
		own, err := c.IAM.GetRolePolicy(ctx, &iam.GetRolePolicyInput{RoleName: awssdk.String(name), PolicyName: awssdk.String(name)})
		if err != nil {
			return nil, "", fmt.Errorf("aws: read role %s's policy: %w", name, err)
		}
		if same, hint := samePolicy(awssdk.ToString(own.PolicyDocument), concrete(owned, c.conn.vars())); !same {
			problems = append(problems, "The policy of "+name+" is not the one this engine expects ("+hint+").")
		}
	}
	return problems, version, nil
}

// checkSplit asks Roles Anywhere for what each certificate identity must not
// have: a seat's certificate for the provisioner, and the provisioner's for
// the broker. Each must be refused. Only a session granted is a problem: a
// failure that is no refusal (a timeout) proves nothing either way.
func (c *Client) checkSplit(ctx context.Context) []string {
	var out []string
	try := func(sentence, cn, profile, role string) {
		if _, err := c.ra.createSession(ctx, c.conn.CA, cn, profile, role, c.conn.TrustAnchorARN); err == nil {
			out = append(out, sentence)
		}
	}
	probe := seatCN(c.conn.Company, "test-split")
	try("A seat's certificate can become the provisioner: the provisioner's trust must admit only "+
		c.conn.resolve(provisionerCN)+".", probe, c.conn.ProvisionerProfileARN, c.conn.ProvisionerRoleARN)
	try("The provisioner's certificate can become the broker: the broker's trust must admit only "+
		strings.TrimSuffix(c.conn.resolve(seatCNPrefix), "-")+"-* names.",
		c.conn.resolve(provisionerCN), c.conn.SeatProfileARN, c.conn.BrokerRoleARN)
	return out
}
