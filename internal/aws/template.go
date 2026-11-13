package aws

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
)

// The owner's setup is one CloudFormation stack, rendered per company with the
// CA certificate, its CN and the allowed regions written in, so it takes no
// parameters and needs no S3 hosting: CloudShell deploys it from a heredoc.

const (
	// StackVersion is the version of the stack this engine renders, tagged on
	// every resource so the Test can name an outdated stack. Raise it with any
	// change to what the stack creates.
	StackVersion = "2"
	versionTag   = "crewlet:stack-version"
	// CloudFormation's TemplateBody cap; above it, deploy needs an S3 bucket.
	templateBodyLimit = 51200
	// The provisioner's session lasts an hour; seat sessions chain from it.
	profileSeconds = 3600
)

// StackParams is what a company's stack is rendered from.
type StackParams struct {
	Company  Company
	CAPEM    string // the CA's certificate: public, never its key
	IssuerCN string
	Regions  []string
	// Atlas adds the atlas's read-only role and its profile, and their two
	// ARNs to the stack's output.
	Atlas bool
}

// stackName is the company's stack. Every name starts with crewlet-<company>,
// never with a vendor's name.
func stackName(company Company) string { return "crewlet-" + string(company) }

// RenderStack renders the company's CloudFormation template.
func RenderStack(p StackParams) ([]byte, error) {
	if !p.Company.Valid() {
		return nil, fmt.Errorf("company %q is not a valid slug", p.Company)
	}
	if !strings.Contains(p.CAPEM, "BEGIN CERTIFICATE") || hasPEMPrivateKey(p.CAPEM) {
		return nil, fmt.Errorf("the CA certificate is not PEM")
	}
	if !cnPattern().MatchString(p.IssuerCN) {
		return nil, fmt.Errorf("issuer CN %q must match %s", p.IssuerCN, cnPattern())
	}
	if len(p.Regions) == 0 {
		return nil, fmt.Errorf("name at least one allowed region")
	}
	for _, r := range p.Regions {
		if !config.AWSRegion(r) {
			return nil, fmt.Errorf("%q is not a region name", r)
		}
	}
	tags := []obj{{"Key": "crewlet:managed-by", "Value": "crewlet"}, {"Key": "crewlet:company", "Value": string(p.Company)},
		{"Key": versionTag, "Value": StackVersion}}
	n := func(s string) string { return named(s, p.Company) }
	managed := func(name, desc string, d obj) obj {
		return obj{"Type": "AWS::IAM::ManagedPolicy", "Properties": obj{
			"ManagedPolicyName": n(name), "Path": n(crewletPath), "Description": desc, "PolicyDocument": templated(d, p.Company),
		}}
	}
	resources := obj{
		"TrustAnchor": obj{"Type": "AWS::RolesAnywhere::TrustAnchor", "Properties": obj{
			"Name": n("crewlet-{company}"), "Enabled": true, "Tags": tags,
			"Source": obj{"SourceType": "CERTIFICATE_BUNDLE", "SourceData": obj{"X509CertificateData": p.CAPEM}},
		}},
		"SeatBoundary": managed(boundaryName, "The most any Crewlet agent can do, at any level.", boundaryDoc(p.Regions)),
		"LevelRead":    managed(levelRead, "Crewlet Read: metadata, metrics, logs and the audit trail.", levelDoc(levelRead)),
		"LevelOperate": managed(levelOperate, "Crewlet Operate: restart, scale and run what exists.", levelDoc(levelOperate)),
		"LevelAdminister": managed(levelAdminister, "Crewlet Administer: opt-in; can create access that outlives the agent's session.",
			levelDoc(levelAdminister)),
		"LevelIamRead": managed(levelIAMRead, "Crewlet IAM read: the account's users, roles and policies.", levelDoc(levelIAMRead)),
		"ProvisionerRole": obj{"Type": "AWS::IAM::Role", "Properties": obj{
			"RoleName": n(provisionerName), "Path": n(crewletPath), "MaxSessionDuration": profileSeconds,
			"Description":              "Crewlet's engine: creates and changes its agents' roles, only inside the seat boundary.",
			"AssumeRolePolicyDocument": templated(provisionerTrustDoc(p.IssuerCN), p.Company),
			"Policies":                 []obj{{"PolicyName": n(provisionerName), "PolicyDocument": templated(provisionerDoc(), p.Company)}},
			"Tags":                     tags,
		}},
		"BrokerRole": obj{"Type": "AWS::IAM::Role", "Properties": obj{
			"RoleName": n(brokerName), "Path": n(crewletPath), "MaxSessionDuration": profileSeconds,
			"Description":              "Crewlet's engine: becomes one agent's role at a time, and can do nothing else.",
			"AssumeRolePolicyDocument": templated(brokerTrustDoc(p.IssuerCN), p.Company),
			"Policies":                 []obj{{"PolicyName": n(brokerName), "PolicyDocument": templated(brokerDoc(), p.Company)}},
			"Tags":                     tags,
		}},
		// One profile per role: each lists exactly one role, so a certificate
		// can only ever ask for the role its CN is trusted by.
		"ProvisionerProfile": obj{"Type": "AWS::RolesAnywhere::Profile", "Properties": obj{
			"Name": n("crewlet-{company}-provisioner"), "Enabled": true, "DurationSeconds": profileSeconds,
			"RoleArns": []any{obj{"Fn::GetAtt": []string{"ProvisionerRole", "Arn"}}}, "Tags": tags,
		}},
		"SeatProfile": obj{"Type": "AWS::RolesAnywhere::Profile", "Properties": obj{
			"Name": n("crewlet-{company}-seats"), "Enabled": true, "DurationSeconds": profileSeconds,
			"RoleArns": []any{obj{"Fn::GetAtt": []string{"BrokerRole", "Arn"}}}, "Tags": tags,
		}},
	}
	output := []any{
		obj{"Fn::GetAtt": []string{"TrustAnchor", "TrustAnchorArn"}},
		obj{"Fn::GetAtt": []string{"ProvisionerProfile", "ProfileArn"}},
		obj{"Fn::GetAtt": []string{"ProvisionerRole", "Arn"}},
		obj{"Fn::GetAtt": []string{"SeatProfile", "ProfileArn"}},
		obj{"Fn::GetAtt": []string{"BrokerRole", "Arn"}},
	}
	description := "Crewlet: the engine's trust anchor, its provisioner and broker roles, the agent boundary and the level policies."
	if p.Atlas {
		// A ROLE AND A PROFILE OF ITS OWN, as the other two identities have:
		// rendered out, the next deploy deletes both and nothing else.
		resources["AtlasRole"] = obj{"Type": "AWS::IAM::Role", "Properties": obj{
			"RoleName": n(atlasName), "Path": n(crewletPath), "MaxSessionDuration": profileSeconds,
			"Description":              "Crewlet's atlas: lists what the account runs, read-only, and can do nothing else.",
			"AssumeRolePolicyDocument": templated(atlasTrustDoc(p.IssuerCN), p.Company),
			"Policies":                 []obj{{"PolicyName": n(atlasName), "PolicyDocument": templated(atlasDoc(), p.Company)}},
			"Tags":                     tags,
		}}
		resources["AtlasProfile"] = obj{"Type": "AWS::RolesAnywhere::Profile", "Properties": obj{
			"Name": n("crewlet-{company}-atlas"), "Enabled": true, "DurationSeconds": profileSeconds,
			"RoleArns": []any{obj{"Fn::GetAtt": []string{"AtlasRole", "Arn"}}}, "Tags": tags,
		}}
		output = append(output, obj{"Fn::GetAtt": []string{"AtlasProfile", "ProfileArn"}},
			obj{"Fn::GetAtt": []string{"AtlasRole", "Arn"}})
		description = "Crewlet: the engine's trust anchor, its provisioner, broker and atlas roles, the agent boundary and the level policies."
	}
	t := obj{
		"AWSTemplateFormatVersion": "2010-09-09",
		"Description":              description,
		"Resources":                resources,
		// One value to paste back: the ARNs CreateSession needs, joined.
		"Outputs": obj{config.AWSConnectionOutput: obj{"Value": obj{"Fn::Join": []any{",", output}}}},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Short lines: a terminal paste of one 15 KB line is fragile.
	enc.SetIndent("", " ")
	if err := enc.Encode(t); err != nil {
		return nil, err
	}
	if buf.Len() > templateBodyLimit {
		return nil, fmt.Errorf("the template is %d bytes, over CloudFormation's %d-byte TemplateBody limit", buf.Len(), templateBodyLimit)
	}
	return buf.Bytes(), nil
}

// CloudShellBlock is the paste that deploys the stack from a heredoc and
// prints its CrewletConnection value last, the one line to paste back.
func CloudShellBlock(template []byte, region string, company Company) string {
	const eof = "CREWLET_TEMPLATE_EOF"
	stack := stackName(company)
	var b strings.Builder
	fmt.Fprintf(&b, "cat > %s.json <<'%s'\n%s%s\n", stack, eof, template, eof)
	fmt.Fprintf(&b, "aws cloudformation deploy --region %s --stack-name %s --template-file %s.json --capabilities CAPABILITY_NAMED_IAM --no-fail-on-empty-changeset\n",
		region, stack, stack)
	fmt.Fprintf(&b, "aws cloudformation describe-stacks --region %s --stack-name %s --query \"Stacks[0].Outputs[?OutputKey=='%s'].OutputValue\" --output text\n",
		region, stack, config.AWSConnectionOutput)
	return b.String()
}
