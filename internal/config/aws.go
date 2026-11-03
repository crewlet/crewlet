package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
)

// AWS gives each agent seat its own IAM role in the company's account, made by
// a provisioner role the engine signs in to through IAM Roles Anywhere with a
// certificate authority it holds itself. The owner creates one CloudFormation
// stack, rendered by the engine, whose names all carry crewlet-<slug>.
//
// It ingests nothing: the block is provisioning alone, so [AWS.validate]
// requires it whenever the block is enabled.
type AWS struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on."`

	// Provisioning is the stack the agents' roles are made through and the
	// certificate authority the engine signs in with.
	Provisioning *AWSProvisioning `yaml:"provisioning,omitempty" json:"provisioning,omitempty" desc:"The Crewlet stack in the AWS account and the engine's certificate authority the agents' IAM roles are made with. Required when enabled."`
}

// AWSProvisioning is what the engine needs to make, level, turn off and delete
// one IAM role per agent seat.
type AWSProvisioning struct {
	// Slug is in every name the stack and the engine create, so it is fixed
	// once the stack exists.
	Slug string `yaml:"slug,omitempty" json:"slug,omitempty" desc:"Short name every Crewlet name in the account carries, as crewlet-<slug>: up to 20 lowercase letters, digits and hyphens. Set when the certificate authority is minted, and fixed once the stack exists."`

	// HomeRegion holds the stack's trust anchor and profiles, where the
	// engine signs in.
	HomeRegion string `yaml:"home_region,omitempty" json:"home_region,omitempty" desc:"Region code the Crewlet stack, its trust anchor and its profiles live in, such as eu-west-1."`

	// Regions are rendered into the stack's boundary, which refuses agents
	// every other Region.
	Regions []string `yaml:"regions,omitempty" json:"regions,omitempty" desc:"Region codes agents may work in; the stack refuses them every other Region. Empty is the home Region and us-east-1. Changing the list means updating the stack."`

	// IAMRead attaches the IAM read policy to every seat that is on.
	IAMRead bool `yaml:"iam_read,omitempty" json:"iam_read,omitempty" desc:"Let every agent that is on read IAM users, groups, roles and policies. Off by default, since it shows the people in the account."`

	// CertificateAuthority is the engine's CA: its private key, then its
	// certificate. The trust anchor holds only the certificate.
	CertificateAuthority string `secret:"true" yaml:"certificate_authority,omitempty" json:"certificate_authority,omitempty" desc:"The engine's certificate authority: its P-256 private key, then its certificate, both PEM. Minted by the connect form and never shown; the stack's trust anchor holds only the certificate. It can sign in as the provisioner, which can create agent roles and attach the Crewlet levels."`

	// Connection is the stack's one output, pasted back.
	Connection string `yaml:"connection,omitempty" json:"connection,omitempty" desc:"The stack's CrewletConnection output: five ARNs joined by commas, as the last line of the CloudShell block prints them. Until it is set, no agent's role can be made."`
}

// awsGlobalRegion answers AWS's global services, so agents keep it by default.
const awsGlobalRegion = "us-east-1"

// RegionsOrDefault is the Regions agents may work in: the list, or the home
// Region and us-east-1 where it names none.
func (p *AWSProvisioning) RegionsOrDefault() []string {
	if p == nil {
		return nil
	}
	var out []string
	for _, r := range p.Regions {
		if r = strings.TrimSpace(r); r != "" && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	if len(out) > 0 {
		return out
	}
	if home := strings.TrimSpace(p.HomeRegion); home != "" && home != awsGlobalRegion {
		out = append(out, home)
	}
	return append(out, awsGlobalRegion)
}

// AWSConnectionOutput is the stack output whose value is pasted back.
const AWSConnectionOutput = "CrewletConnection"

// AWSConnection is the stack's CrewletConnection value, checked: its five ARNs
// and the partition and account they share.
type AWSConnection struct {
	TrustAnchorARN        string
	ProvisionerProfileARN string
	ProvisionerRoleARN    string
	SeatProfileARN        string
	BrokerRoleARN         string
	Partition             string
	Account               string
}

// String is the CrewletConnection value: the five ARNs joined by commas.
func (c AWSConnection) String() string {
	return strings.Join([]string{c.TrustAnchorARN, c.ProvisionerProfileARN, c.ProvisionerRoleARN,
		c.SeatProfileARN, c.BrokerRoleARN}, ",")
}

// The shapes AWS gives the names the stack carries.
var (
	// awsSlug keeps every derived name inside IAM's 64-character role name,
	// and an agent's certificate name inside Roles Anywhere's 61 characters.
	awsSlug   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,18}[a-z0-9])?$`)
	awsRegion = regexp.MustCompile(`^[a-z]{2}(-gov)?-[a-z]+-\d$`)

	awsAnchor  = regexp.MustCompile(`^arn:(aws[a-z-]*):rolesanywhere:([a-z0-9-]+):(\d{12}):trust-anchor/[0-9a-f-]{36}$`)
	awsProfile = regexp.MustCompile(`^arn:(aws[a-z-]*):rolesanywhere:([a-z0-9-]+):(\d{12}):profile/[0-9a-f-]{36}$`)
	// The slug is read from the role ARN, path and name both.
	awsRole = regexp.MustCompile(`^arn:(aws[a-z-]*):iam::(\d{12}):role/crewlet/([a-z0-9-]+)/crewlet-([a-z0-9-]+)-(provisioner|broker)$`)
)

// AWSSlug reports whether slug can name a company's Crewlet objects in AWS.
func AWSSlug(slug string) bool { return awsSlug.MatchString(slug) }

// AWSRegion reports whether name is an AWS Region code.
func AWSRegion(name string) bool { return awsRegion.MatchString(name) }

// ParseAWSConnection checks that value is one company's stack output: the
// trust anchor, the provisioner's profile and role, the seats' profile and the
// broker, in one account and partition, with the trust anchor and profiles in
// the home Region. Each refusal names what to copy again.
func ParseAWSConnection(slug, homeRegion, value string) (AWSConnection, error) {
	parts := strings.Split(strings.TrimSpace(value), ",")
	if len(parts) != 5 {
		return AWSConnection{}, fmt.Errorf("this is not a Crewlet stack output, which holds five ARNs "+
			"joined by commas: copy the %s value on the stack's Outputs tab", AWSConnectionOutput)
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	c := AWSConnection{TrustAnchorARN: parts[0], ProvisionerProfileARN: parts[1], ProvisionerRoleARN: parts[2],
		SeatProfileARN: parts[3], BrokerRoleARN: parts[4]}
	a := awsAnchor.FindStringSubmatch(c.TrustAnchorARN)
	pp := awsProfile.FindStringSubmatch(c.ProvisionerProfileARN)
	sp := awsProfile.FindStringSubmatch(c.SeatProfileARN)
	r := awsRole.FindStringSubmatch(c.ProvisionerRoleARN)
	b := awsRole.FindStringSubmatch(c.BrokerRoleARN)
	switch {
	case a == nil:
		return AWSConnection{}, errors.New("its first ARN is not a Roles Anywhere trust anchor")
	case pp == nil || sp == nil:
		return AWSConnection{}, errors.New("its second and fourth ARNs must be Roles Anywhere profiles")
	case c.ProvisionerProfileARN == c.SeatProfileARN:
		return AWSConnection{}, errors.New("the provisioner and the agents need separate profiles")
	case r == nil || r[5] != "provisioner":
		return AWSConnection{}, errors.New("its third ARN is not a role crewlet/<slug>/crewlet-<slug>-provisioner")
	case b == nil || b[5] != "broker":
		return AWSConnection{}, errors.New("its fifth ARN is not a role crewlet/<slug>/crewlet-<slug>-broker")
	case r[3] != slug || r[4] != slug || b[3] != slug || b[4] != slug:
		return AWSConnection{}, fmt.Errorf("its roles are not crewlet-%s's: this output is another company's stack", slug)
	case a[2] != homeRegion || pp[2] != homeRegion || sp[2] != homeRegion:
		return AWSConnection{}, fmt.Errorf("its trust anchor and profiles are not in the home Region %s", homeRegion)
	case a[3] != r[2] || pp[3] != r[2] || sp[3] != r[2] || b[2] != r[2] || a[1] != r[1] || b[1] != r[1]:
		return AWSConnection{}, errors.New("its trust anchor, profiles and roles are not in one account")
	}
	c.Partition, c.Account = r[1], r[2]
	return c, nil
}

func (a *AWS) validate(path Path) error {
	var p problems
	if !a.Enabled {
		return p.err()
	}
	if a.Provisioning == nil {
		p.add(at(path, "provisioning"), ErrMissing,
			"required when aws is enabled: it names the Crewlet stack the agents' roles are "+
				"made through and the certificate authority the engine signs in with")
		return p.err()
	}
	prov := a.Provisioning
	slug := strings.TrimSpace(prov.Slug)
	switch {
	case slug == "":
		p.add(at(path, "provisioning.slug"), ErrMissing,
			"required when aws is enabled: the short name in every Crewlet name in the account, "+
				"set when the certificate authority is minted")
	case !AWSSlug(slug):
		p.add(at(path, "provisioning.slug"), ErrUnknownValue,
			"%q is not a slug: up to 20 lowercase letters, digits and hyphens, starting and "+
				"ending with a letter or digit", prov.Slug)
	}
	home := strings.TrimSpace(prov.HomeRegion)
	switch {
	case home == "":
		p.add(at(path, "provisioning.home_region"), ErrMissing,
			"required when aws is enabled: the Region code the Crewlet stack lives in, such as eu-west-1")
	case !AWSRegion(home):
		p.add(at(path, "provisioning.home_region"), ErrUnknownValue,
			"%q is not an AWS Region code, such as eu-west-1", prov.HomeRegion)
	}
	seen := map[string]bool{}
	for i, region := range prov.Regions {
		code := strings.TrimSpace(region)
		switch {
		case code == "":
			p.add(idx(at(path, "provisioning.regions"), i), ErrMissing,
				"a Region code is empty; remove the entry or give the code")
		case !AWSRegion(code):
			p.add(idx(at(path, "provisioning.regions"), i), ErrUnknownValue,
				"%q is not an AWS Region code, such as eu-west-1", region)
		case seen[code]:
			p.add(idx(at(path, "provisioning.regions"), i), ErrConflict, "%q is listed twice", region)
		}
		seen[code] = true
	}
	if strings.TrimSpace(prov.CertificateAuthority) == "" {
		p.add(at(path, "provisioning.certificate_authority"), ErrMissing,
			"required when aws is enabled: the engine's certificate authority, which the "+
				"connect form mints and seals")
	}
	// THE CONNECTION FOLLOWS THE CERTIFICATE AUTHORITY: the stack's trust
	// anchor holds the CA's certificate, so the stack, and its output, exist
	// only once the CA is minted. Absent, the pass reports the step that
	// creates it.
	conn := strings.TrimSpace(prov.Connection)
	if conn != "" && !envref.Has(conn) && AWSSlug(slug) && AWSRegion(home) {
		if _, err := ParseAWSConnection(slug, home, conn); err != nil {
			p.add(at(path, "provisioning.connection"), ErrUnknownValue, "%s", err.Error())
		}
	}
	return p.err()
}
