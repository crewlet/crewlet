package aws

import (
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/setup"
)

// AtlasRoleLabel is how the atlas's role is named to a person: the name it
// carries in the account, with the company's short name in it.
const AtlasRoleLabel = "crewlet-<name>-atlas"

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
			Hint:   "Names the stack and every role Crewlet creates. It cannot change later.",
			More:   "The stack is named crewlet-<name>, and its roles and policies follow it.",
			Blocks: integration.FindingCredentialMissing,
		},
		{
			Field: "home_region", Label: "Home Region", Kind: setup.KindChoice,
			ConfigPath: "integrations.aws.provisioning.home_region", Required: true, Connect: true,
			Choices: choices,
			Hint:    "Where the Crewlet stack and its sign-in live.",
			More:    regionsMore(prov),
			Blocks:  integration.FindingCredentialMissing,
		},
		{
			Field: "certificate_authority", Label: "Sign-in certificate", Kind: setup.KindSecret,
			ConfigPath: "integrations.aws.provisioning.certificate_authority",
			SecretName: CertificateAuthoritySecret, Required: true, Connect: true,
			// MINTABLE, because AWS needs only its certificate: the engine
			// makes the authority, seals its key, and nobody downloads a
			// credential at all.
			Mintable: true, Shape: setup.ShapeCertificateAuthority,
			Hint: "Run this block in {link}.",
			More: "Crewlet made this certificate authority when you saved, and sealed it as " +
				CertificateAuthoritySecret + ". The stack trusts only its certificate; its key is never shown " +
				"or exported.",
			LinkText:  "CloudShell in the home Region",
			VendorURL: "https://{home_region}.console.aws.amazon.com/cloudshell/home?region={home_region}",
			CheckNote: "It is tested with the stack output.",
			Blocks:    integration.FindingCredentialMissing,
		},
		{
			Field: "connection", Label: "Stack output", Kind: setup.KindID,
			ConfigPath: "integrations.aws.provisioning.connection", Required: caHeld, Connect: true,
			Checkable: true, CheckWith: []string{"slug", "home_region", "certificate_authority", "atlas_read"},
			Format: "five ARNs joined by commas, or seven with the Atlas reader",
			Hint:   "The last line the CloudShell block prints.",
			More: "It is also the " + config.AWSConnectionOutput + " value on the stack's Outputs tab, in your " +
				"{link}. Test checks the sign-in, every Crewlet policy, the identity split and the role quota. " +
				"Paste it again after the block changes what the stack holds.",
			LinkText: "CloudFormation stacks", VendorURL: "https://console.aws.amazon.com/cloudformation/home",
			Blocks: integration.FindingApprovalRequired,
		},
		{
			Field: "iam_read", Label: "Let agents read IAM", Kind: setup.KindToggle,
			ConfigPath: "integrations.aws.provisioning.iam_read", Default: "false",
			Hint: "Lets agents read IAM users, groups, roles and policies.",
			More: "It shows the people in your account, so it is off unless you turn it on.",
		},
		{
			// OPTIONAL, AND NEVER IN THE WAY: off changes nothing an agent
			// does, and on only adds a role the atlas alone signs in as.
			Field: "atlas_read", Label: "See more in Atlas", Kind: setup.KindToggle,
			ConfigPath: "integrations.aws.provisioning.atlas_read", Default: "false",
			Hint: "Adds a read-only role for Atlas, which lists what the account runs.",
			More: "Atlas then shows the Regions in use, instances, databases, load balancers, DNS zones, queues " +
				"and bucket names, read hourly as " + AtlasRoleLabel + ", never as an agent. It reads no function, " +
				"no user data, no parameter and no object. One step: turn this on and save, run the CloudShell " +
				"block again, and paste its last line. To undo it, turn this off, save and run the block again, " +
				"which deletes the role.",
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
			if caHeld {
				// THE VALUE THE REFERENCE NAMES: the resolver takes a name,
				// and handed the whole `${NAME}` it found nothing to render.
				reqs[i].Public = cloudShellView(prov, setup.Deref(prov.CertificateAuthority, resolve))
			}
		case "connection":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Plain(prov.Connection)
		case "iam_read":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Toggle(prov.IAMRead)
		case "atlas_read":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Toggle(prov.AtlasRead)
		case "enabled":
			reqs[i].Present, reqs[i].Resolved, reqs[i].Stored = setup.Toggle(enabled)
		}
	}
	return reqs
}

// regionsMore says the Regions agents work in: once the home Region is saved,
// the list the company document sets, shown here.
func regionsMore(prov config.AWSProvisioning) string {
	if !config.AWSRegion(strings.TrimSpace(prov.HomeRegion)) {
		return "Agents work in this Region and us-east-1, unless the company document lists other Regions."
	}
	return "Agents work in " + strings.Join(prov.RegionsOrDefault(), ", ") + ", as " +
		"integrations.aws.provisioning.regions sets them. Changing them changes the CloudShell block."
}

// cloudShellView is the CloudShell block for the held certificate authority:
// the stack rendered with its certificate, never its key, for the slug, home
// Region and Regions saved. Nil until the slug and the home Region are. A
// stack that cannot be rendered says why in the block's place.
func cloudShellView(prov config.AWSProvisioning, bundle string) *setup.PublicView {
	company, home := Company(strings.TrimSpace(prov.Slug)), strings.TrimSpace(prov.HomeRegion)
	if !company.Valid() || !config.AWSRegion(home) {
		return nil
	}
	text, err := cloudShellText(company, home, prov.RegionsOrDefault(), prov.AtlasRead, bundle)
	if err != nil {
		text = "# Crewlet cannot render the stack: " + strings.TrimPrefix(err.Error(), "aws: ") + "\n"
	}
	view, err := setup.PublicOf(bundle, text, "sh")
	if err != nil {
		return nil
	}
	return view
}

func cloudShellText(company Company, home string, regions []string, atlas bool, bundle string) (string, error) {
	ca, err := ParseCA(bundle)
	if err != nil {
		return "", err
	}
	tpl, err := RenderStack(StackParams{Company: company, CAPEM: ca.CertificatePEM(),
		IssuerCN: ca.Cert.Subject.CommonName, Regions: regions, Atlas: atlas})
	if err != nil {
		return "", err
	}
	note := "# Agents may work in " + strings.Join(regions, ", ") + " (integrations.aws.provisioning.regions).\n"
	if atlas {
		note += "# The stack holds Atlas's read-only role (integrations.aws.provisioning.atlas_read).\n"
	}
	return note + CloudShellBlock(tpl, home, company), nil
}

// Guide is what the connect form says around its fields: one pass, in order,
// from the name and Region that mint the certificate authority to the stack
// output that proves the stack holds it.
func Guide() setup.Guide {
	return setup.Guide{
		Summary: "Crewlet gives each agent its own IAM role in your account and signs it in for 15 minutes at " +
			"a time. No access key is stored.",
		Prepare: []setup.Step{
			{Text: "Choose a short name and a home Region, then save.", Yields: "home_region"},
			{
				Text: "Sign in to the account agents will work in, and run the block Crewlet shows in {link}.",
				Link: "certificate_authority",
			},
			{Text: "Paste the last line the block prints, then test it.", Yields: "connection"},
		},
	}
}
