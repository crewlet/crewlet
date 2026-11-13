package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/words"
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

	// AtlasRead adds the atlas's own read-only role to the rendered stack,
	// which the atlas-collect duty signs in as.
	AtlasRead bool `yaml:"atlas_read,omitempty" json:"atlas_read,omitempty" desc:"Add a read-only role for the atlas to the stack, so the atlas lists the account's Regions in use, instances, databases, load balancers, DNS zones, queues and bucket names. Off by default. Turning it on or off changes the CloudShell block: run it again, and paste its last line when it is on."`

	// CertificateAuthority is the engine's CA: its private key, then its
	// certificate. The trust anchor holds only the certificate.
	CertificateAuthority string `secret:"true" yaml:"certificate_authority,omitempty" json:"certificate_authority,omitempty" desc:"The engine's certificate authority: its P-256 private key, then its certificate, both PEM. Minted by the connect form and never shown; the stack's trust anchor holds only the certificate. It can sign in as the provisioner, which can create agent roles and attach the Crewlet levels."`

	// Connection is the stack's one output, pasted back.
	Connection string `yaml:"connection,omitempty" json:"connection,omitempty" desc:"The stack's CrewletConnection output: five ARNs joined by commas, or seven where the stack holds the atlas's role, as the last line of the CloudShell block prints them. Until it is set, no agent's role can be made."`
}

// The levels a seat's role holds, least access first. internal/aws maps each
// to the customer managed policies it attaches: its own and every lower one.
const (
	AWSLevelRead       = "read"
	AWSLevelOperate    = "operate"
	AWSLevelAdminister = "administer"
)

// AWSLevels is every level a seat may name, least access first.
func AWSLevels() []string {
	return []string{AWSLevelRead, AWSLevelOperate, AWSLevelAdminister}
}

// RoleAWS is a seat's own IAM role.
type RoleAWS struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent has its own IAM role. Off attaches AWS's deny-all policy and detaches its levels; the role and its CloudTrail history stay."`

	// Level is what the role may do; empty is read.
	Level string `yaml:"level,omitempty" json:"level,omitempty" js:"enum=read|operate|administer" desc:"read (the default): describe and list compute, networking, containers, functions, databases, DNS and CDN, read metrics and CloudWatch Logs, and list object names, with no object, parameter or secret contents. operate: read plus starting, stopping, rebooting and scaling, invoking functions, invalidating the CDN, editing alarms and reading and writing S3 objects. administer: operate plus managing those services whole, which can leave access behind such as key pairs, user data and bucket policies."`
}

// LevelOrDefault is the seat's level, read where it names none.
func (r *RoleAWS) LevelOrDefault() string {
	if r == nil || strings.TrimSpace(r.Level) == "" {
		return AWSLevelRead
	}
	return strings.TrimSpace(r.Level)
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

// RegionList is the Regions the block names, nil-safe: nil is the default
// ([AWSProvisioning.RegionsOrDefault]).
func (a *AWS) RegionList() []string {
	if a == nil || a.Provisioning == nil {
		return nil
	}
	return a.Provisioning.Regions
}

// AWSConnectionOutput is the stack output whose value is pasted back.
const AWSConnectionOutput = "CrewletConnection"

// AWSConnection is the stack's CrewletConnection value, checked: its five
// ARNs, the atlas's two where the stack holds its role, and the partition and
// account they share.
type AWSConnection struct {
	TrustAnchorARN        string
	ProvisionerProfileARN string
	ProvisionerRoleARN    string
	SeatProfileARN        string
	BrokerRoleARN         string
	// AtlasProfileARN and AtlasRoleARN are both set or both empty: a stack
	// rendered without the atlas's role prints neither.
	AtlasProfileARN string
	AtlasRoleARN    string
	Partition       string
	Account         string
}

// HasAtlas reports a stack output that carries the atlas's profile and role.
func (c AWSConnection) HasAtlas() bool { return c.AtlasRoleARN != "" }

// String is the CrewletConnection value: the ARNs joined by commas.
func (c AWSConnection) String() string {
	parts := []string{c.TrustAnchorARN, c.ProvisionerProfileARN, c.ProvisionerRoleARN,
		c.SeatProfileARN, c.BrokerRoleARN}
	if c.HasAtlas() {
		parts = append(parts, c.AtlasProfileARN, c.AtlasRoleARN)
	}
	return strings.Join(parts, ",")
}

// The shapes AWS gives the names the stack carries.
var (
	// awsSlug keeps every derived name inside IAM's 64-character role name,
	// and an agent's certificate name inside Roles Anywhere's 61 characters.
	awsSlug = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,18}[a-z0-9])?$`)
	})
	awsRegion = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[a-z]{2}(-gov)?-[a-z]+-\d$`)
	})

	awsAnchor = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^arn:(aws[a-z-]*):rolesanywhere:([a-z0-9-]+):(\d{12}):trust-anchor/[0-9a-f-]{36}$`)
	})
	awsProfile = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^arn:(aws[a-z-]*):rolesanywhere:([a-z0-9-]+):(\d{12}):profile/[0-9a-f-]{36}$`)
	})
	// The slug is read from the role ARN, path and name both.
	awsRole = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^arn:(aws[a-z-]*):iam::(\d{12}):role/crewlet/([a-z0-9-]+)/crewlet-([a-z0-9-]+)-(provisioner|broker|atlas)$`)
	})
)

// AWSSlug reports whether slug can name a company's Crewlet objects in AWS.
func AWSSlug(slug string) bool { return awsSlug().MatchString(slug) }

// AWSRegion reports whether name is an AWS Region code.
func AWSRegion(name string) bool { return awsRegion().MatchString(name) }

// ParseAWSConnection checks that value is one company's stack output: the
// trust anchor, the provisioner's profile and role, the seats' profile and the
// broker, then the atlas's profile and role where the stack holds them, in
// one account and partition, with the trust anchor and profiles in the home
// Region. Each refusal names what to copy again.
func ParseAWSConnection(slug, homeRegion, value string) (AWSConnection, error) {
	parts := strings.Split(strings.TrimSpace(value), ",")
	if len(parts) != 5 && len(parts) != 7 {
		return AWSConnection{}, fmt.Errorf("this is not a Crewlet stack output, which holds five ARNs "+
			"joined by commas, or seven with the atlas's role: copy the %s value on the stack's Outputs tab",
			AWSConnectionOutput)
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	c := AWSConnection{TrustAnchorARN: parts[0], ProvisionerProfileARN: parts[1], ProvisionerRoleARN: parts[2],
		SeatProfileARN: parts[3], BrokerRoleARN: parts[4]}
	a := awsAnchor().FindStringSubmatch(c.TrustAnchorARN)
	pp := awsProfile().FindStringSubmatch(c.ProvisionerProfileARN)
	sp := awsProfile().FindStringSubmatch(c.SeatProfileARN)
	r := awsRole().FindStringSubmatch(c.ProvisionerRoleARN)
	b := awsRole().FindStringSubmatch(c.BrokerRoleARN)
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
	if len(parts) == 7 {
		c.AtlasProfileARN, c.AtlasRoleARN = parts[5], parts[6]
		ap := awsProfile().FindStringSubmatch(c.AtlasProfileARN)
		ar := awsRole().FindStringSubmatch(c.AtlasRoleARN)
		switch {
		case ap == nil:
			return AWSConnection{}, errors.New("its sixth ARN is not a Roles Anywhere profile")
		case c.AtlasProfileARN == c.ProvisionerProfileARN || c.AtlasProfileARN == c.SeatProfileARN:
			return AWSConnection{}, errors.New("the atlas needs a profile of its own")
		case ar == nil || ar[5] != "atlas":
			return AWSConnection{}, errors.New("its seventh ARN is not a role crewlet/<slug>/crewlet-<slug>-atlas")
		case ar[3] != slug || ar[4] != slug:
			return AWSConnection{}, fmt.Errorf("its roles are not crewlet-%s's: this output is another company's stack", slug)
		case ap[2] != homeRegion:
			return AWSConnection{}, fmt.Errorf("its trust anchor and profiles are not in the home Region %s", homeRegion)
		case ap[3] != r[2] || ar[2] != r[2] || ap[1] != r[1] || ar[1] != r[1]:
			return AWSConnection{}, errors.New("its trust anchor, profiles and roles are not in one account")
		}
	}
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

// checkAWS holds a seat's level to the closed set.
func (c *Company) checkAWS(p *problems, path Path, b *RoleAWS) {
	if b == nil {
		return
	}
	if level := strings.TrimSpace(b.Level); level != "" && !slices.Contains(AWSLevels(), level) {
		p.add(at(path, "level"), ErrUnknownValue,
			"%q is not an AWS level this engine offers; give %s",
			b.Level, words.Or(AWSLevels()))
	}
}
