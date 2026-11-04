package aws

import (
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
)

// ErrNotConnected is a provisioning block whose stack output is not pasted yet:
// the CA is minted, and the stack holding its certificate may not exist.
var ErrNotConnected = errors.New("aws: the Crewlet stack's output is not pasted yet")

// Connection is a company's AWS connection: the stack's checked output, the
// home Region it lives in, and the sealed CA.
type Connection struct {
	Company Company
	Region  string
	config.AWSConnection
	CA *CA
}

// NewConnection reads a company's provisioning block, its secrets resolved:
// the stack output held to the slug and the home Region, and the CA.
func NewConnection(p *config.AWSProvisioning) (*Connection, error) {
	if p == nil {
		return nil, errors.New("aws: no provisioning block")
	}
	company, region := Company(strings.TrimSpace(p.Slug)), strings.TrimSpace(p.HomeRegion)
	if !company.Valid() {
		return nil, fmt.Errorf("aws: %q is not a valid slug", p.Slug)
	}
	if !config.AWSRegion(region) {
		return nil, fmt.Errorf("aws: %q is not a Region code", p.HomeRegion)
	}
	if strings.TrimSpace(p.Connection) == "" {
		return nil, ErrNotConnected
	}
	out, err := config.ParseAWSConnection(string(company), region, p.Connection)
	if err != nil {
		return nil, fmt.Errorf("aws: the stack output: %w", err)
	}
	ca, err := ParseCA(p.CertificateAuthority)
	if err != nil {
		return nil, err
	}
	return &Connection{Company: company, Region: region, AWSConnection: out, CA: ca}, nil
}

// vars is the connection's values for the policy documents' tokens.
func (conn *Connection) vars() vars {
	return vars{company: conn.Company, partition: conn.Partition, account: conn.Account, anchor: conn.TrustAnchorARN}
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
