package aws

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/sigv4"
)

// matches is IAM's action matching: case-insensitive, with * and ? wildcards.
func matches(pattern, action string) bool {
	re := "^" + strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(regexp.QuoteMeta(strings.ToLower(pattern))) + "$"
	return regexp.MustCompile(re).MatchString(strings.ToLower(action))
}

func grants(patterns []string, action string) bool {
	for _, p := range patterns {
		if matches(p, action) {
			return true
		}
	}
	return false
}

func testCA(t *testing.T) *CA {
	t.Helper()
	bundle, err := setup.GenerateCertificateAuthority("crewlet-test-ca", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ca, err := ParseCA(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// READ READS NO DATA, NO SECRETS, NO IAM AND NO BILLING: the cross-cloud policy.
func TestReadReadsNoDataNoSecretsNoIAMNoBilling(t *testing.T) {
	t.Parallel()
	for _, a := range []string{
		"s3:GetObject", "s3:GetObjectVersion", "ssm:GetParameter", "ssm:GetParameters", "ssm:GetParametersByPath",
		"ssm:GetParameterHistory", "ssm:GetCommandInvocation", "secretsmanager:GetSecretValue", "dynamodb:GetItem",
		"dynamodb:Query", "dynamodb:Scan", "kms:Decrypt", "lambda:GetFunction", "lambda:GetLayerVersion",
		"ec2:GetConsoleOutput", "ec2:GetConsoleScreenshot", "ec2:GetPasswordData", "ec2:GetLaunchTemplateData",
		"ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer", "ecr:GetAuthorizationToken", "rds:DownloadDBLogFilePortion",
		"rds:DownloadCompleteDBLogFile", "iam:ListUsers", "iam:GetRole", "ce:GetCostAndUsage", "aws-portal:ViewBilling",
		"ec2:StopInstances", "ec2:CreateSecurityGroup",
	} {
		if grants(readActions, a) {
			t.Errorf("Read grants %s", a)
		}
	}
	// The owner's decision: CloudWatch Logs content is readable at Read.
	for _, a := range []string{"logs:GetLogEvents", "logs:FilterLogEvents", "logs:StartQuery", "logs:GetQueryResults"} {
		if !grants(readActions, a) {
			t.Errorf("Read lacks %s", a)
		}
	}
}

// THE READ LEVEL NO LONGER READS A FUNCTION'S CONFIGURATION, which carries
// its environment: not by the action of that name, and not by the two
// listings that answer the same configuration.
func TestReadReadsNoFunctionConfiguration(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"lambda:GetFunctionConfiguration", "lambda:ListFunctions",
		"lambda:ListVersionsByFunction", "lambda:GetFunction"} {
		if grants(readActions, action) {
			t.Errorf("Read grants %s, which answers a function's environment", action)
		}
	}
	for _, action := range readActions {
		if strings.HasPrefix(action, "lambda:") && strings.ContainsAny(action, "*?") {
			t.Errorf("Read grants %s: a Lambda wildcard reaches the listings that answer the environment", action)
		}
	}
	// What says how a function is wired, and no value it holds, stays.
	for _, action := range []string{"lambda:ListAliases", "lambda:ListEventSourceMappings", "lambda:GetPolicy",
		"lambda:GetRuntimeManagementConfig", "lambda:GetFunctionUrlConfig"} {
		if !grants(readActions, action) {
			t.Errorf("Read lacks %s", action)
		}
	}
}

// OPERATE CREATES NO PERSISTENT ACCESS: the credential rule.
func TestOperateCreatesNoPersistentAccess(t *testing.T) {
	t.Parallel()
	for _, a := range []string{
		"ec2:CreateKeyPair", "ec2:ImportKeyPair", "ec2:ModifyInstanceAttribute", "ec2:RunInstances",
		"ec2:AuthorizeSecurityGroupIngress", "ssm:StartSession", "ssm:SendCommand", "ssm:PutParameter",
		"ec2-instance-connect:SendSSHPublicKey", "rds:ModifyDBInstance", "rds:CreateDBInstance", "s3:PutBucketPolicy",
		"s3:PutBucketAcl", "route53:ChangeResourceRecordSets", "lambda:UpdateFunctionCode", "lambda:AddPermission",
		"ecs:RegisterTaskDefinition", "ecs:ExecuteCommand", "ecs:RunTask", "iam:PassRole", "cloudfront:CreatePublicKey",
		"kms:CreateGrant",
	} {
		if grants(operateActions, a) {
			t.Errorf("Operate grants %s", a)
		}
	}
}

func TestTheBoundaryRefusesTheNeverList(t *testing.T) {
	t.Parallel()
	for _, a := range []string{
		"iam:CreateUser", "iam:CreateRole", "iam:AttachRolePolicy", "iam:PutRolePolicy", "iam:PassRole", "iam:CreateAccessKey",
		"iam:CreateLoginProfile", "iam:UpdateAssumeRolePolicy", "sts:AssumeRole", "sts:GetFederationToken",
		"sts:GetSessionToken", "secretsmanager:GetSecretValue", "kms:ScheduleKeyDeletion", "kms:PutKeyPolicy", "kms:CreateKey",
		"aws-portal:ViewBilling", "ce:GetCostAndUsage", "cur:DescribeReportDefinitions", "budgets:ViewBudget",
		"organizations:ListAccounts", "account:PutAlternateContact", "ec2:GetPasswordData", "ecr:GetAuthorizationToken",
		"eks:CreateAccessEntry", "eks:AssociateAccessPolicy", "cloudtrail:StopLogging", "cloudtrail:DeleteTrail",
		"acm:ExportCertificate", "rds-db:connect",
	} {
		if !grants(never, a) {
			t.Errorf("the boundary's never list misses %s", a)
		}
	}
	for _, a := range append(append([]string{}, operateActions...), "ec2:DescribeInstances", "iam:GetRole", "kms:CreateGrant", "kms:Decrypt") {
		if grants(never, a) {
			t.Errorf("the never list refuses %s, which a level needs", a)
		}
	}
}

func TestEveryLevelActionFitsInsideTheBoundary(t *testing.T) {
	t.Parallel()
	allowed := boundaryDoc([]string{"us-east-2"})["Statement"].([]obj)[0]["Action"].([]string)
	for _, a := range append(append(append([]string{}, readActions...), operateActions...), administerServices...) {
		svc := strings.SplitN(a, ":", 2)[0]
		if !grants(allowed, a) && !grants(allowed, svc+":*") {
			t.Errorf("%s is granted by a level but outside the boundary", a)
		}
	}
}

// AWS's MCP Server authorizes each AWS call a tool makes, as the caller, and
// aws-mcp:* actions have no effect: a grant of them would only mislead.
func TestNoPolicyGrantsTheNoOpMCPActions(t *testing.T) {
	t.Parallel()
	docs := []any{boundaryDoc([]string{"us-east-2"}), provisionerDoc(), brokerDoc()}
	for _, n := range levelNames() {
		docs = append(docs, levelDoc(n))
	}
	for _, d := range docs {
		if strings.Contains(string(compactJSON(d)), "aws-mcp") {
			t.Fatalf("a policy names aws-mcp: %s", compactJSON(d))
		}
	}
}

func TestPoliciesFitIAMSizeLimits(t *testing.T) {
	t.Parallel()
	x := vars{company: Company(strings.Repeat("c", 20)), partition: "aws", account: "123456789012",
		anchor: "arn:aws:rolesanywhere:us-east-2:123456789012:trust-anchor/" + strings.Repeat("a", 36)}
	docs := map[string]any{boundaryName: boundaryDoc([]string{"us-east-2", "us-east-1", "eu-west-1"})}
	for _, n := range levelNames() {
		docs[n] = levelDoc(n)
	}
	for name, d := range docs {
		if n := policySize(concrete(d, x)); n > managedPolicyLimit {
			t.Errorf("%s is %d characters, over IAM's %d", name, n, managedPolicyLimit)
		}
	}
	for name, d := range map[string]obj{"provisioner": provisionerDoc(), "broker": brokerDoc()} {
		if n := policySize(concrete(d, x)); n > 10240 {
			t.Errorf("the %s's inline policy is %d characters, over IAM's 10240", name, n)
		}
	}
	for name, d := range map[string]obj{"seat": seatTrustDoc("crewlet-" + strings.Repeat("a", 53)),
		"provisioner": provisionerTrustDoc("crewlet-ca-0011aabb"), "broker": brokerTrustDoc("crewlet-ca-0011aabb")} {
		if n := policySize(concrete(d, x)); n > trustPolicyLimit {
			t.Errorf("the %s trust policy is %d characters, over %d", name, n, trustPolicyLimit)
		}
	}
}

// The provisioner and the broker are separate identities: a seat certificate
// can only become the broker, the broker only seats, the provisioner nothing.
func TestIdentitiesAreSplit(t *testing.T) {
	t.Parallel()
	cond := func(d obj, op string) string {
		return d["Statement"].([]obj)[0]["Condition"].(obj)[op].(obj)["aws:PrincipalTag/x509Subject/CN"].([]string)[0]
	}
	if got := cond(provisionerTrustDoc("crewlet-ca"), "StringEquals"); got != provisionerCN {
		t.Fatalf("the provisioner admits CN %q", got)
	}
	if got := cond(brokerTrustDoc("crewlet-ca"), "StringLike"); got != seatCNPrefix+"*" {
		t.Fatalf("the broker admits CN %q", got)
	}
	for _, co := range []Company{"probe", "acme", Company(strings.Repeat("c", 20))} {
		for _, slug := range []string{"sre", "provisioner", strings.Repeat("h", 27)} {
			seat := seatCN(co, slug)
			if !cnPattern().MatchString(seat) || seat == named(provisionerCN, co) || !matches(named(seatCNPrefix, co)+"*", seat) {
				t.Fatalf("seat CN %q", seat)
			}
		}
		if matches(named(seatCNPrefix, co)+"*", named(provisionerCN, co)) {
			t.Fatal("a seat CN pattern admits the provisioner's")
		}
	}
	for _, s := range provisionerDoc()["Statement"].([]obj) {
		if s["Effect"] == "Allow" && grants(s["Action"].([]string), "sts:AssumeRole") {
			t.Fatalf("the provisioner may assume roles: %v", s["Sid"])
		}
	}
	b := brokerDoc()["Statement"].([]obj)
	if b[0]["Effect"] != "Allow" || b[0]["Resource"].([]string)[0] != seatRolesARN || len(b[0]["Action"].([]string)) != 3 ||
		b[1]["NotAction"] == nil || b[2]["NotResource"] == nil || b[3]["Action"].([]string)[0] != "iam:*" {
		t.Fatalf("the broker's statements are %v", b)
	}
	if trust := seatTrustDoc("crewlet-x-seat-y")["Statement"].([]obj)[0]; trust["Principal"].(obj)["AWS"] != brokerARN {
		t.Fatalf("a seat trusts %v, not the broker", trust["Principal"])
	}
}

func TestSamePolicyIgnoresOrderAndShapeButNotContent(t *testing.T) {
	t.Parallel()
	want := doc(stmt("A", "Allow", []string{"ec2:Describe*", "s3:ListBucket"}), with(stmt("B", "Deny", []string{"iam:*"}), "StringEquals", "aws:RequestedRegion", "us-east-2"))
	stored := `{"Version":"2012-10-17","Statement":[{"Sid":"B","Effect":"Deny","Action":"iam:*","Resource":"*","Condition":{"StringEquals":{"aws:RequestedRegion":"us-east-2"}}},{"Sid":"A","Effect":"Allow","Action":["s3:ListBucket","ec2:Describe*"],"Resource":["*"]}]}`
	if ok, hint := samePolicy(url.PathEscape(stored), want); !ok {
		t.Fatalf("equivalent documents differ: %s", hint)
	}
	if ok, hint := samePolicy(strings.Replace(stored, `"s3:ListBucket"`, `"s3:*"`, 1), want); ok || !strings.Contains(hint, `"A"`) {
		t.Fatalf("a widened statement passed (hint %q)", hint)
	}
}

// resolveSub evaluates the template's Fn::Sub strings as CloudFormation would.
func resolveSub(v any, x vars) any {
	switch t := v.(type) {
	case map[string]any:
		if s, ok := t["Fn::Sub"].(string); ok && len(t) == 1 {
			return strings.NewReplacer("${AWS::Partition}", x.partition, "${AWS::AccountId}", x.account,
				"${TrustAnchor.TrustAnchorArn}", x.anchor, "${!", "${").Replace(s)
		}
		out := map[string]any{}
		for k, e := range t {
			out[k] = resolveSub(e, x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = resolveSub(e, x)
		}
		return out
	}
	return v
}

// The stack and the own-policy check come from one table: a deployed stack passes the check.
func TestTheStackDeploysExactlyWhatTheCheckExpects(t *testing.T) {
	t.Parallel()
	ca := testCA(t)
	regions := []string{"us-east-2", "us-east-1"}
	raw, err := RenderStack(StackParams{Company: "acme", CAPEM: ca.CertificatePEM(), IssuerCN: ca.Cert.Subject.CommonName, Regions: regions})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"{account}", "{partition}", "{anchor}", "{company}", "crewlet-aws", "PRIVATE KEY", "aws-mcp"} {
		if bytes.Contains(raw, []byte(bad)) {
			t.Fatalf("the stack carries %q", bad)
		}
	}
	var tpl map[string]any
	if err := json.Unmarshal(raw, &tpl); err != nil || len(raw) > templateBodyLimit {
		t.Fatalf("the stack is not usable JSON within %d bytes: %v", templateBodyLimit, err)
	}
	x := vars{company: "acme", partition: "aws", account: "123456789012", anchor: "arn:aws:rolesanywhere:us-east-2:123456789012:trust-anchor/" + strings.Repeat("b", 36)}
	res := tpl["Resources"].(map[string]any)
	props := func(id string) map[string]any { return res[id].(map[string]any)["Properties"].(map[string]any) }
	check := func(label string, deployed any, want obj) {
		b, _ := json.Marshal(resolveSub(deployed, x))
		if ok, hint := samePolicy(string(b), concrete(want, x)); !ok {
			t.Errorf("%s: %s", label, hint)
		}
	}
	check("boundary", props("SeatBoundary")["PolicyDocument"], boundaryDoc(regions))
	for id, name := range map[string]string{"LevelRead": levelRead, "LevelOperate": levelOperate, "LevelAdminister": levelAdminister, "LevelIamRead": levelIAMRead} {
		check(name, props(id)["PolicyDocument"], levelDoc(name))
		if props(id)["ManagedPolicyName"] != named(name, "acme") || props(id)["Path"] != "/crewlet/acme/" {
			t.Errorf("%s is not %s under /crewlet/acme/", id, named(name, "acme"))
		}
	}
	check("provisioner trust", props("ProvisionerRole")["AssumeRolePolicyDocument"], provisionerTrustDoc(ca.Cert.Subject.CommonName))
	check("provisioner policy", props("ProvisionerRole")["Policies"].([]any)[0].(map[string]any)["PolicyDocument"], provisionerDoc())
	check("broker trust", props("BrokerRole")["AssumeRolePolicyDocument"], brokerTrustDoc(ca.Cert.Subject.CommonName))
	check("broker policy", props("BrokerRole")["Policies"].([]any)[0].(map[string]any)["PolicyDocument"], brokerDoc())
	for profile, role := range map[string]string{"ProvisionerProfile": "ProvisionerRole", "SeatProfile": "BrokerRole"} {
		arns := props(profile)["RoleArns"].([]any)
		if len(arns) != 1 || arns[0].(map[string]any)["Fn::GetAtt"].([]any)[0] != role {
			t.Errorf("%s offers %v, want only %s", profile, arns, role)
		}
	}
	outputs := tpl["Outputs"].(map[string]any)
	if len(outputs) != 1 || outputs[config.AWSConnectionOutput] == nil {
		t.Fatalf("the stack must output exactly %s, got %v", config.AWSConnectionOutput, outputs)
	}
	for _, id := range []string{"TrustAnchor", "ProvisionerRole", "BrokerRole", "ProvisionerProfile", "SeatProfile"} {
		if !strings.Contains(string(compactJSON(props(id)["Tags"])), `"`+versionTag+`","Value":"`+StackVersion+`"`) {
			t.Errorf("%s is not tagged with the stack version", id)
		}
	}
	if block := CloudShellBlock(raw, "us-east-2", "acme"); !strings.Contains(block, "<<'CREWLET_TEMPLATE_EOF'") ||
		!strings.Contains(block, "CAPABILITY_NAMED_IAM") || !strings.Contains(block, "--stack-name crewlet-acme ") {
		t.Fatal("the CloudShell block must use a quoted heredoc, acknowledge named IAM and name the company's stack")
	}
}

func TestRenderStackRefusesBadInput(t *testing.T) {
	t.Parallel()
	ca := testCA(t)
	bundle, _ := setup.GenerateCertificateAuthority("crewlet-test-ca", time.Now())
	for name, p := range map[string]StackParams{
		"no pem":      {Company: "acme", CAPEM: "x", IssuerCN: "crewlet-ca", Regions: []string{"us-east-2"}},
		"private key": {Company: "acme", CAPEM: bundle, IssuerCN: "crewlet-ca", Regions: []string{"us-east-2"}},
		"bad cn":      {Company: "acme", CAPEM: ca.CertificatePEM(), IssuerCN: "Crewlet CA", Regions: []string{"us-east-2"}},
		"no region":   {Company: "acme", CAPEM: ca.CertificatePEM(), IssuerCN: "crewlet-ca"},
		"bad region":  {Company: "acme", CAPEM: ca.CertificatePEM(), IssuerCN: "crewlet-ca", Regions: []string{"us east"}},
		"no company":  {CAPEM: ca.CertificatePEM(), IssuerCN: "crewlet-ca", Regions: []string{"us-east-2"}},
		"long":        {Company: Company(strings.Repeat("a", 21)), CAPEM: ca.CertificatePEM(), IssuerCN: "crewlet-ca", Regions: []string{"us-east-2"}},
	} {
		if _, err := RenderStack(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCertificatesMeetTheTrustAnchorRules(t *testing.T) {
	t.Parallel()
	ca := testCA(t)
	if !ca.Cert.IsCA || ca.Cert.KeyUsage&x509.KeyUsageCertSign == 0 || ca.Cert.Version != 3 {
		t.Fatal("the CA is not a v3 signing CA")
	}
	for kind, alg := range map[LeafKind]string{LeafECDSA: sigv4.AlgX509ECDSA, LeafRSA: sigv4.AlgX509RSA} {
		leaf, key, err := ca.Leaf("crewlet-acme-provisioner", time.Now(), kind)
		if err != nil {
			t.Fatal(err)
		}
		if leaf.IsCA || leaf.CheckSignatureFrom(ca.Cert) != nil || leaf.NotAfter.Sub(leaf.NotBefore) > LeafLifetime+notBeforeSkew {
			t.Fatal("a leaf is not a short-lived end entity of the CA")
		}
		if got, _ := sigv4.X509Algorithm(key.Public()); got != alg {
			t.Fatalf("a leaf signs with %s, want %s", got, alg)
		}
	}
	for _, bad := range []string{"", "Crewlet", "crewlet-" + strings.Repeat("a", 54), "crewlet-a b", "other-name"} {
		if _, _, err := ca.Leaf(bad, time.Now(), LeafECDSA); err == nil {
			t.Errorf("CN %q was accepted", bad)
		}
	}
	other := testCA(t)
	bundle := mustBundle(t)
	mine, err := ParseCA(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCA(strings.Replace(bundle, mine.CertificatePEM(), other.CertificatePEM(), 1)); err == nil {
		t.Fatal("a bundle whose certificate is another key's was accepted")
	}
}

func mustBundle(t *testing.T) string {
	t.Helper()
	b, err := setup.GenerateCertificateAuthority("crewlet-test-ca", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return b
}
