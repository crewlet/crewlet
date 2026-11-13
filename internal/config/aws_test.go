package config_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

const (
	awsAnchor      = "arn:aws:rolesanywhere:us-east-2:123456789012:trust-anchor/0f0e0d0c-0b0a-4908-8706-050403020100"
	awsProvProfile = "arn:aws:rolesanywhere:us-east-2:123456789012:profile/00010203-0405-4607-8809-0a0b0c0d0e0f"
	awsSeatProfile = "arn:aws:rolesanywhere:us-east-2:123456789012:profile/10010203-0405-4607-8809-0a0b0c0d0e0f"
	awsProv        = "arn:aws:iam::123456789012:role/crewlet/acme/crewlet-acme-provisioner"
	awsBroker      = "arn:aws:iam::123456789012:role/crewlet/acme/crewlet-acme-broker"
)

func awsConnection() config.AWSConnection {
	return config.AWSConnection{TrustAnchorARN: awsAnchor, ProvisionerProfileARN: awsProvProfile,
		ProvisionerRoleARN: awsProv, SeatProfileARN: awsSeatProfile, BrokerRoleARN: awsBroker}
}

// completeAWS is an enabled block with every requirement answered.
var completeAWS = `    enabled: true
    provisioning:
      slug: acme
      home_region: us-east-2
      regions: [us-east-2, eu-west-1]
      certificate_authority: "${AWS_CA}"
      connection: "` + awsConnection().String() + `"`

// A COMPLETE BLOCK VALIDATES, and so does one whose stack output is not pasted
// yet: the stack holds the CA's certificate, so it follows the mint.
func TestACompleteAWSBlockValidates(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"complete":     completeAWS,
		"switched off": "    enabled: false",
		"before the stack exists": "    enabled: true\n    provisioning: " +
			"{slug: acme, home_region: us-east-2, certificate_authority: \"${AWS_CA}\"}",
	} {
		if err := validateIntegrationDoc(t, "aws", body); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

// AN ENABLED BLOCK NAMES ITS SLUG, HOME REGION AND CA, each refusal naming the
// field an operator edits.
func TestAWSRefusesAnIncompleteOrMalformedBlock(t *testing.T) {
	t.Parallel()
	const ca = `certificate_authority: "${AWS_CA}"`
	for _, tc := range []struct {
		name, body, path string
	}{
		{"no provisioning", "    enabled: true", "integrations.aws.provisioning"},
		{"no slug", "    enabled: true\n    provisioning: {home_region: us-east-2, " + ca + "}",
			"integrations.aws.provisioning.slug"},
		{"an uppercase slug", "    enabled: true\n    provisioning: {slug: Acme, home_region: us-east-2, " + ca + "}",
			"integrations.aws.provisioning.slug"},
		{"a slug too long for a role name", "    enabled: true\n    provisioning: {slug: " +
			strings.Repeat("a", 21) + ", home_region: us-east-2, " + ca + "}", "integrations.aws.provisioning.slug"},
		{"no home Region", "    enabled: true\n    provisioning: {slug: acme, " + ca + "}",
			"integrations.aws.provisioning.home_region"},
		{"a Region name, not its code", "    enabled: true\n    provisioning: {slug: acme, home_region: Ohio, " + ca + "}",
			"integrations.aws.provisioning.home_region"},
		{"no CA", "    enabled: true\n    provisioning: {slug: acme, home_region: us-east-2}",
			"integrations.aws.provisioning.certificate_authority"},
		{"a malformed agent Region", "    enabled: true\n    provisioning: {slug: acme, home_region: us-east-2, " +
			"regions: [us-east-2, europe], " + ca + "}", "integrations.aws.provisioning.regions[1]"},
		{"an agent Region twice", "    enabled: true\n    provisioning: {slug: acme, home_region: us-east-2, " +
			"regions: [us-east-1, us-east-1], " + ca + "}", "integrations.aws.provisioning.regions[1]"},
		{"another company's stack", strings.Replace(completeAWS, "slug: acme", "slug: other", 1),
			"integrations.aws.provisioning.connection"},
		{"a stack in another Region", strings.Replace(completeAWS, "home_region: us-east-2", "home_region: eu-west-1", 1),
			"integrations.aws.provisioning.connection"},
	} {
		err := validateIntegrationDoc(t, "aws", tc.body)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.path) {
			t.Errorf("%s: %v does not name %s", tc.name, err, tc.path)
		}
	}
}

// THE PASTED OUTPUT IS ONE COMPANY'S STACK, in one account and partition, with
// its sign-in in the home Region; anything else names what to copy again.
func TestParseAWSConnectionHoldsTheOutputToOneCompanysStack(t *testing.T) {
	t.Parallel()
	got, err := config.ParseAWSConnection("acme", "us-east-2", " "+awsConnection().String()+"\n")
	if err != nil || got.Account != "123456789012" || got.Partition != "aws" || got.String() != awsConnection().String() {
		t.Fatalf("%+v %v", got, err)
	}
	for name, mutate := range map[string]func(*config.AWSConnection){
		"one profile": func(c *config.AWSConnection) { c.SeatProfileARN = c.ProvisionerProfileARN },
		"swapped roles": func(c *config.AWSConnection) {
			c.ProvisionerRoleARN, c.BrokerRoleARN = c.BrokerRoleARN, c.ProvisionerRoleARN
		},
		"other company": func(c *config.AWSConnection) { c.BrokerRoleARN = strings.ReplaceAll(c.BrokerRoleARN, "acme", "other") },
		"other account": func(c *config.AWSConnection) {
			c.BrokerRoleARN = strings.Replace(c.BrokerRoleARN, "123456789012", "210987654321", 1)
		},
		"other partition": func(c *config.AWSConnection) {
			c.BrokerRoleARN = strings.Replace(c.BrokerRoleARN, "arn:aws:", "arn:aws-us-gov:", 1)
		},
		"other region": func(c *config.AWSConnection) {
			c.SeatProfileARN = strings.Replace(c.SeatProfileARN, "us-east-2", "eu-west-1", 1)
		},
		"not an anchor": func(c *config.AWSConnection) { c.TrustAnchorARN = c.ProvisionerProfileARN },
		"name and path": func(c *config.AWSConnection) {
			c.ProvisionerRoleARN = strings.Replace(c.ProvisionerRoleARN, "crewlet-acme-", "crewlet-other-", 1)
		},
	} {
		c := awsConnection()
		mutate(&c)
		if _, err := config.ParseAWSConnection("acme", "us-east-2", c.String()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := config.ParseAWSConnection("acme", "us-east-2", awsAnchor+","+awsProv); err == nil ||
		!strings.Contains(err.Error(), config.AWSConnectionOutput) {
		t.Errorf("a value with two ARNs = %v, want a refusal naming the output to copy", err)
	}
}

// AGENTS KEEP THE GLOBAL REGION BY DEFAULT: an empty list is the home Region
// and us-east-1, once each.
func TestAWSRegionsDefaultToHomeAndUSEast1(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		p    config.AWSProvisioning
		want []string
	}{
		{config.AWSProvisioning{HomeRegion: "eu-west-1"}, []string{"eu-west-1", "us-east-1"}},
		{config.AWSProvisioning{HomeRegion: "us-east-1"}, []string{"us-east-1"}},
		{config.AWSProvisioning{HomeRegion: "eu-west-1", Regions: []string{" ap-south-1", "ap-south-1"}}, []string{"ap-south-1"}},
	} {
		if got := tc.p.RegionsOrDefault(); !slices.Equal(got, tc.want) {
			t.Errorf("%+v: %v, want %v", tc.p, got, tc.want)
		}
	}
}

// A SEAT'S LEVEL IS ONE OF THE CLOSED SET, and empty reads as read.
func TestAnAWSSeatNamesALevelTheEngineOffers(t *testing.T) {
	t.Parallel()
	doc := func(level string) string {
		return "name: Acme\nintegrations:\n  aws:\n" + completeAWS +
			"\nroles:\n  - {name: SRE, handle: sre, integrations: {aws: {enabled: true" + level + "}}}\n"
	}
	for _, refused := range []string{"admin", "write", "READ", "iam_read"} {
		if _, err := config.ParseCompany([]byte(doc(", level: " + refused))); err == nil ||
			!strings.Contains(err.Error(), "roles[0].integrations.aws.level") {
			t.Errorf("level %q = %v, want a refusal naming the seat's level", refused, err)
		}
	}
	for _, level := range config.AWSLevels() {
		c, err := config.ParseCompany([]byte(doc(", level: " + level)))
		if err != nil {
			t.Fatalf("%s was refused: %v", level, err)
		}
		if got := c.Roles[0].Integrations.AWS.LevelOrDefault(); got != level {
			t.Errorf("level = %q, want %q", got, level)
		}
	}
	c, err := config.ParseCompany([]byte(doc("")))
	if err != nil {
		t.Fatalf("a seat naming no level was refused: %v", err)
	}
	if got := c.Roles[0].Integrations.AWS.LevelOrDefault(); got != config.AWSLevelRead {
		t.Errorf("an unset level = %q, want read", got)
	}
}

// A PERSON SIGNS IN AS THEMSELVES: a human seat carries no IAM role.
func TestAHumanSeatCarriesNoAWSRole(t *testing.T) {
	t.Parallel()
	doc := "name: Acme\nintegrations:\n  aws:\n" + completeAWS +
		"\nroles:\n  - {name: Founder, handle: founder, kind: human, contact: {slack_user_id: U0FOUNDER}, " +
		"integrations: {aws: {enabled: false}}}\n"
	if _, err := config.ParseCompany([]byte(doc)); err == nil ||
		!strings.Contains(err.Error(), "roles[0].integrations.aws") {
		t.Errorf("a human seat's aws block = %v, want a refusal naming it", err)
	}
}

// THE ENGINE ISSUES A SEAT'S SESSION ONLY WHERE IT HOLDS THE PROVISIONER: an
// enabled block with provisioning, and nowhere else.
func TestAWSSeatCredentialsAreTheEnginesOnlyWithAProvisioner(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		aws  *config.AWS
		want config.SeatCredentialMode
	}{
		{"absent", nil, config.SeatCredentialExternal},
		{"switched off", &config.AWS{Provisioning: &config.AWSProvisioning{}}, config.SeatCredentialExternal},
		{"no provisioning", &config.AWS{Enabled: true}, config.SeatCredentialExternal},
		{"provisioned", &config.AWS{Enabled: true, Provisioning: &config.AWSProvisioning{}}, config.SeatCredentialEngine},
	} {
		c := &config.Company{}
		c.Integrations.AWS = tc.aws
		if got := c.SeatCredentialMode("aws", false); got != tc.want {
			t.Errorf("%s: mode = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// THE OUTPUT CARRIES THE ATLAS'S ROLE WHERE THE STACK HOLDS IT: seven ARNs
// are held to the same company, account and home Region as five, the atlas's
// profile is its own, and its role is nobody else's.
func TestParseAWSConnectionReadsTheAtlasRoleWhereTheStackHoldsIt(t *testing.T) {
	t.Parallel()
	const (
		atlasProfile = "arn:aws:rolesanywhere:us-east-2:123456789012:profile/20010203-0405-4607-8809-0a0b0c0d0e0f"
		atlasRole    = "arn:aws:iam::123456789012:role/crewlet/acme/crewlet-acme-atlas"
	)
	with := func() config.AWSConnection {
		c := awsConnection()
		c.AtlasProfileARN, c.AtlasRoleARN = atlasProfile, atlasRole
		return c
	}
	if five, err := config.ParseAWSConnection("acme", "us-east-2", awsConnection().String()); err != nil || five.HasAtlas() {
		t.Fatalf("five ARNs read %+v, %v, want a stack without the atlas's role", five, err)
	}
	got, err := config.ParseAWSConnection("acme", "us-east-2", with().String())
	if err != nil || !got.HasAtlas() || got.AtlasProfileARN != atlasProfile || got.AtlasRoleARN != atlasRole ||
		got.String() != with().String() || strings.Count(got.String(), ",") != 6 {
		t.Fatalf("%+v %v", got, err)
	}
	for name, mutate := range map[string]func(*config.AWSConnection){
		"the provisioner's profile": func(c *config.AWSConnection) { c.AtlasProfileARN = c.ProvisionerProfileARN },
		"the agents' profile":       func(c *config.AWSConnection) { c.AtlasProfileARN = c.SeatProfileARN },
		"the broker as the atlas":   func(c *config.AWSConnection) { c.AtlasRoleARN = c.BrokerRoleARN },
		"the atlas as the broker":   func(c *config.AWSConnection) { c.BrokerRoleARN = c.AtlasRoleARN },
		"another company's role":    func(c *config.AWSConnection) { c.AtlasRoleARN = strings.ReplaceAll(c.AtlasRoleARN, "acme", "other") },
		"another account's role": func(c *config.AWSConnection) {
			c.AtlasRoleARN = strings.Replace(c.AtlasRoleARN, "123456789012", "210987654321", 1)
		},
		"another Region's profile": func(c *config.AWSConnection) {
			c.AtlasProfileARN = strings.Replace(c.AtlasProfileARN, "us-east-2", "eu-west-1", 1)
		},
		"another partition's role": func(c *config.AWSConnection) {
			c.AtlasRoleARN = strings.Replace(c.AtlasRoleARN, "arn:aws:", "arn:aws-cn:", 1)
		},
		"a role that is not a role": func(c *config.AWSConnection) { c.AtlasRoleARN = c.TrustAnchorARN },
		"a seat's role as the atlas": func(c *config.AWSConnection) {
			c.AtlasRoleARN = strings.Replace(c.AtlasRoleARN, "acme/crewlet", "acme/seat/crewlet", 1)
		},
	} {
		c := with()
		mutate(&c)
		if _, err := config.ParseAWSConnection("acme", "us-east-2", c.String()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := config.ParseAWSConnection("acme", "us-east-2", with().String()+","+atlasRole); err == nil {
		t.Error("eight ARNs: accepted")
	}
	doc := "name: Acme\nintegrations:\n  aws:\n" + strings.Replace(completeAWS, awsConnection().String(), with().String(), 1) +
		"\n      atlas_read: true\n"
	c, err := config.ParseCompany([]byte(doc))
	if err != nil || !c.Integrations.AWS.Provisioning.AtlasRead {
		t.Fatalf("a block with the atlas's role = %v", err)
	}
}
