package aws

import (
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/setup"
)

// CertificateAuthoritySecret is the name the engine seals the company's
// certificate authority under.
const CertificateAuthoritySecret = "AWS_CERTIFICATE_AUTHORITY"

// homeRegions is every Region IAM Roles Anywhere has an endpoint in, as
// AWS's endpoint table names it: the home Region must be one.
var homeRegions = [][2]string{
	{"us-east-1", "US East (N. Virginia)"}, {"us-east-2", "US East (Ohio)"},
	{"us-west-1", "US West (N. California)"}, {"us-west-2", "US West (Oregon)"},
	{"af-south-1", "Africa (Cape Town)"}, {"ap-east-1", "Asia Pacific (Hong Kong)"},
	{"ap-east-2", "Asia Pacific (Taipei)"}, {"ap-south-1", "Asia Pacific (Mumbai)"},
	{"ap-south-2", "Asia Pacific (Hyderabad)"}, {"ap-southeast-1", "Asia Pacific (Singapore)"},
	{"ap-southeast-2", "Asia Pacific (Sydney)"}, {"ap-southeast-3", "Asia Pacific (Jakarta)"},
	{"ap-southeast-4", "Asia Pacific (Melbourne)"}, {"ap-southeast-5", "Asia Pacific (Malaysia)"},
	{"ap-southeast-6", "Asia Pacific (New Zealand)"}, {"ap-southeast-7", "Asia Pacific (Thailand)"},
	{"ap-northeast-1", "Asia Pacific (Tokyo)"}, {"ap-northeast-2", "Asia Pacific (Seoul)"},
	{"ap-northeast-3", "Asia Pacific (Osaka)"}, {"ca-central-1", "Canada (Central)"},
	{"ca-west-1", "Canada West (Calgary)"}, {"eu-central-1", "Europe (Frankfurt)"},
	{"eu-central-2", "Europe (Zurich)"}, {"eu-west-1", "Europe (Ireland)"}, {"eu-west-2", "Europe (London)"},
	{"eu-west-3", "Europe (Paris)"}, {"eu-south-1", "Europe (Milan)"}, {"eu-south-2", "Europe (Spain)"},
	{"eu-north-1", "Europe (Stockholm)"}, {"il-central-1", "Israel (Tel Aviv)"},
	{"me-central-1", "Middle East (UAE)"}, {"me-south-1", "Middle East (Bahrain)"},
	{"mx-central-1", "Mexico (Central)"}, {"sa-east-1", "South America (São Paulo)"},
	{"us-gov-east-1", "AWS GovCloud (US-East)"}, {"us-gov-west-1", "AWS GovCloud (US-West)"},
}

// DefaultSlug is the slug a company is offered: its name as a slug, cut to
// what every derived name can carry.
func DefaultSlug(company string) string {
	s := org.Slugify(company)
	if len(s) > 20 {
		s = strings.TrimRight(s[:20], "-")
	}
	if !config.AWSSlug(s) {
		return ""
	}
	return s
}

// Requirements says what this company still needs for AWS. A nil block
// answers every field absent, so the form still renders.
//
// ONE PASS, IN ORDER: the slug and the home Region are entered and saved,
// which mints the certificate authority; the CloudShell block creates the
// stack holding its certificate and prints the stack output; the output is
// pasted and tested. So the output is required only once the CA is held.
func Requirements(in *config.AWS, company string, resolve func(string) (string, bool)) []setup.Requirement {
	var prov config.AWSProvisioning
	var enabled bool
	if in != nil {
		enabled = in.Enabled
		if in.Provisioning != nil {
			prov = *in.Provisioning
		}
	}
	caPresent, caResolved := setup.Resolution(prov.CertificateAuthority, resolve)
	caHeld := caPresent && caResolved != nil && *caResolved
	choices := make([]setup.Choice, 0, len(homeRegions))
	for _, r := range homeRegions {
		choices = append(choices, setup.Choice{Value: r[0], Label: r[1] + " " + r[0]})
	}
	reqs := []setup.Requirement{
		{
			Field: "slug", Label: "Short name", Kind: setup.KindID,
			ConfigPath: "integrations.aws.provisioning.slug", Required: true, Connect: true,
			Default: DefaultSlug(company), Format: "up to 20 lowercase letters, digits and hyphens",
			Help: "The short name in every name Crewlet creates in your account: the stack crewlet-<name>, " +
				"its roles and its policies. It cannot change once the stack exists.",
			Blocks: integration.FindingCredentialMissing,
		},
		{
			Field: "home_region", Label: "Home Region", Kind: setup.KindChoice,
			ConfigPath: "integrations.aws.provisioning.home_region", Required: true, Connect: true,
			Choices: choices,
			Help: "Where the Crewlet stack and Crewlet's sign-in live. Agents work in this Region and " +
				"us-east-1 unless the company document lists others.",
			Blocks: integration.FindingCredentialMissing,
		},
		{
			Field: "certificate_authority", Label: "Sign-in certificate", Kind: setup.KindSecret,
			ConfigPath: "integrations.aws.provisioning.certificate_authority",
			SecretName: CertificateAuthoritySecret, Required: true, Connect: true,
			// MINTABLE, because AWS needs only its certificate: the engine
			// makes the authority, seals its key, and nobody downloads a
			// credential at all.
			Mintable: true, Shape: setup.ShapeCertificateAuthority,
			Help: "Crewlet's own certificate authority, made when you save and sealed as " +
				CertificateAuthoritySecret + ". The stack's trust anchor holds only its certificate, which " +
				"the CloudShell block carries. Its key is never shown or exported.",
			CheckNote: "It is tested with the stack output.",
			Blocks:    integration.FindingCredentialMissing,
		},
		{
			Field: "connection", Label: "Stack output", Kind: setup.KindID,
			ConfigPath: "integrations.aws.provisioning.connection", Required: caHeld, Connect: true,
			Checkable: true, CheckWith: []string{"slug", "home_region", "certificate_authority"},
			Format: "five ARNs joined by commas",
			Where:  "the last line the CloudShell block prints, or the " + config.AWSConnectionOutput + " value on the stack's Outputs tab",
			Help: "What lets Crewlet sign in as the stack's provisioner, which can only make and change its " +
				"agents' roles. Test checks the sign-in, every Crewlet policy, the identity split and the role " +
				"quota.\nSee your",
			LinkText: "CloudFormation stacks", VendorURL: "https://console.aws.amazon.com/cloudformation/home",
			Blocks: integration.FindingApprovalRequired,
		},
		{
			Field: "iam_read", Label: "Let agents read IAM", Kind: setup.KindToggle,
			ConfigPath: "integrations.aws.provisioning.iam_read", Default: "false",
			Help: "Adds read access to IAM users, groups, roles and policies for every agent that is on. It " +
				"shows the people in your account, so it is off unless you turn it on.",
		},
		{
			// Hidden and on: connecting a switched-off integration is not a
			// thing anybody means.
			Field: "enabled", Label: "Manage agent roles", Kind: setup.KindToggle,
			ConfigPath: "integrations.aws.enabled", Required: true, Hidden: true, Default: "true",
		},
	}
	for i := range reqs {
		switch reqs[i].Field {
		case "slug":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Plain(prov.Slug)
		case "home_region":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Plain(prov.HomeRegion)
		case "certificate_authority":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Held(prov.CertificateAuthority, resolve)
		case "connection":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Plain(prov.Connection)
		case "iam_read":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Toggle(prov.IAMRead)
		case "enabled":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Toggle(enabled)
		}
	}
	return reqs
}

// Summary is the sentence the connect form opens with.
func Summary() string {
	return "Crewlet gives each agent its own IAM role in your AWS account, at the level you choose, and signs " +
		"it in for an hour at a time, so CloudTrail names the agent for everything it does. Connecting " +
		"creates one CloudFormation stack in your account. No access key is stored."
}
