package aws

import (
	"errors"
	"fmt"
	"regexp"
)

// Outputs are the company stack's outputs, as CloudShell prints them.
type Outputs struct {
	TrustAnchorARN        string
	ProvisionerProfileARN string
	ProvisionerRoleARN    string
	SeatProfileARN        string
	BrokerRoleARN         string
}

// Connection is a company's AWS connection: the stack's outputs, the Region
// they live in, the account and partition read from them, and the sealed CA.
type Connection struct {
	Company   Company
	Region    string
	Partition string
	Account   string
	Outputs
	CA *CA
}

var (
	anchorPattern  = regexp.MustCompile(`^arn:(aws[a-z-]*):rolesanywhere:([a-z0-9-]+):(\d{12}):trust-anchor/[0-9a-f-]{36}$`)
	profilePattern = regexp.MustCompile(`^arn:(aws[a-z-]*):rolesanywhere:([a-z0-9-]+):(\d{12}):profile/[0-9a-f-]{36}$`)
	// The company is read from the role ARN, path and name both.
	rolePattern = regexp.MustCompile(`^arn:(aws[a-z-]*):iam::(\d{12}):role/crewlet/([a-z0-9-]+)/crewlet-([a-z0-9-]+)-(provisioner|broker)$`)
)

// ParseConnection checks that the five outputs are one company's stack, in
// one account and partition, with its trust anchor and profiles in region.
// Each refusal names the output to copy again.
func ParseConnection(company Company, region string, out Outputs, caBundle string) (*Connection, error) {
	if !company.Valid() {
		return nil, fmt.Errorf("aws: company %q is not a valid slug", company)
	}
	if !regionPattern.MatchString(region) {
		return nil, fmt.Errorf("aws: %q is not a Region name", region)
	}
	a := anchorPattern.FindStringSubmatch(out.TrustAnchorARN)
	pp := profilePattern.FindStringSubmatch(out.ProvisionerProfileARN)
	sp := profilePattern.FindStringSubmatch(out.SeatProfileARN)
	r := rolePattern.FindStringSubmatch(out.ProvisionerRoleARN)
	b := rolePattern.FindStringSubmatch(out.BrokerRoleARN)
	switch {
	case a == nil:
		return nil, errors.New("aws: TrustAnchorArn is not a Roles Anywhere trust anchor ARN")
	case pp == nil || sp == nil:
		return nil, errors.New("aws: ProvisionerProfileArn and SeatProfileArn must be Roles Anywhere profile ARNs")
	case out.ProvisionerProfileARN == out.SeatProfileARN:
		return nil, errors.New("aws: the provisioner and the seats need separate profiles; copy SeatProfileArn again")
	case r == nil || r[5] != "provisioner":
		return nil, errors.New("aws: ProvisionerRoleArn is not a role crewlet/<company>/crewlet-<company>-provisioner")
	case b == nil || b[5] != "broker":
		return nil, errors.New("aws: BrokerRoleArn is not a role crewlet/<company>/crewlet-<company>-broker")
	case r[3] != string(company) || r[4] != string(company) || b[3] != string(company) || b[4] != string(company):
		return nil, fmt.Errorf("aws: the stack's roles are not company %q's", company)
	case a[2] != region || pp[2] != region || sp[2] != region:
		return nil, errors.New("aws: the trust anchor and the profiles must be in the home Region")
	case a[3] != r[2] || pp[3] != r[2] || sp[3] != r[2] || b[2] != r[2] || a[1] != r[1]:
		return nil, errors.New("aws: the trust anchor, profiles and roles must be in one account")
	}
	ca, err := ParseCA(caBundle)
	if err != nil {
		return nil, err
	}
	return &Connection{Company: company, Region: region, Partition: r[1], Account: r[2], Outputs: out, CA: ca}, nil
}

// vars is the connection's values for the policy documents' tokens.
func (c *Connection) vars() vars {
	return vars{company: c.Company, partition: c.Partition, account: c.Account, anchor: c.TrustAnchorARN}
}

// iamRegion is where the partition's IAM, a global service, signs.
func iamRegion(partition string) string {
	switch partition {
	case "aws-us-gov":
		return "us-gov-west-1"
	case "aws-cn":
		return "cn-north-1"
	}
	return "us-east-1"
}
