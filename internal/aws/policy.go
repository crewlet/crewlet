package aws

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
)

// The single source of every policy document: the setup template is rendered
// from it, and the own-policy check compares IAM's stored documents with it.
// Names and ARNs carry {company}, {partition}, {account} and {anchor} tokens:
// {company} is written in at render time (the template is per company), the
// others become Fn::Sub in the template and the real values in the check.

// Every Crewlet object carries the company slug in its path and its name:
// IAM role names are unique per account whatever their path.
const (
	crewletPath     = "/crewlet/{company}/"
	seatPath        = "/crewlet/{company}/seat/"
	provisionerName = "crewlet-{company}-provisioner"
	brokerName      = "crewlet-{company}-broker"
	// The two certificate identities, per company. The provisioner's CN opens
	// only the provisioner; a seat's CN opens only the broker, which can only
	// become seats. "-seat-" keeps every seat CN apart from "-provisioner".
	provisionerCN = "crewlet-{company}-provisioner"
	seatCNPrefix  = "crewlet-{company}-seat-"
	// The atlas's own role and certificate name: the reader the atlas-collect
	// duty signs in as, so CloudTrail names the atlas and never an agent.
	atlasName       = "crewlet-{company}-atlas"
	atlasCN         = "crewlet-{company}-atlas"
	boundaryName    = "crewlet-{company}-seat-boundary"
	levelRead       = "crewlet-{company}-level-read"
	levelOperate    = "crewlet-{company}-level-operate"
	levelAdminister = "crewlet-{company}-level-administer"
	levelIAMRead    = "crewlet-{company}-level-iam-read"
	// gateTag marks a resource whose identity an Administer seat may act as
	// (a shell, a command, new code); only the owner sets it.
	gateTag   = "crewlet:agent-access"
	gateValue = "administer"
	// managedPolicyLimit is IAM's size cap for a managed policy, whitespace excluded.
	managedPolicyLimit = 6144
	// trustPolicyLimit is IAM's default cap for a role trust policy.
	trustPolicyLimit = 2048
)

type obj = map[string]any

func policyARN(name string) string {
	return "arn:{partition}:iam::{account}:policy/crewlet/{company}/" + name
}

const (
	seatRolesARN   = "arn:{partition}:iam::{account}:role/crewlet/{company}/seat/crewlet-{company}-*"
	crewletRoles   = "arn:{partition}:iam::{account}:role/crewlet/{company}/*"
	provisionerARN = "arn:{partition}:iam::{account}:role/crewlet/{company}/" + provisionerName
	brokerARN      = "arn:{partition}:iam::{account}:role/crewlet/{company}/" + brokerName
	atlasARN       = "arn:{partition}:iam::{account}:role/crewlet/{company}/" + atlasName
	denyAllARN     = "arn:{partition}:iam::aws:policy/AWSDenyAll"
)

// Company is the slug every Crewlet name in the account carries.
type Company string

// Valid reports a slug every derived name can carry ([config.AWSSlug]).
func (c Company) Valid() bool { return config.AWSSlug(string(c)) }

// named resolves the {company} token in a name, path or ARN.
func named(s string, company Company) string {
	return strings.ReplaceAll(s, "{company}", string(company))
}

func doc(stmts ...obj) obj { return obj{"Version": "2012-10-17", "Statement": stmts} }

func stmt(sid, effect string, actions []string, resources ...string) obj {
	if len(resources) == 0 {
		resources = []string{"*"}
	}
	return obj{"Sid": sid, "Effect": effect, "Action": actions, "Resource": resources}
}

func with(s obj, op, key string, vals ...string) obj {
	c, _ := s["Condition"].(obj)
	if c == nil {
		c = obj{}
		s["Condition"] = c
	}
	m, _ := c[op].(obj)
	if m == nil {
		m = obj{}
		c[op] = m
	}
	m[key] = vals
	return s
}

// Read: metadata, metrics, logs and the audit trail; no object, item,
// parameter or secret contents, no IAM, no billing. LAMBDA IS NAMED ACTION BY
// ACTION: listing functions or their versions, and reading a function's
// configuration, answer its environment, so none of the three is here.
var readActions = []string{
	"ec2:Describe*", "autoscaling:Describe*", "application-autoscaling:Describe*", "elasticloadbalancing:Describe*",
	"ecs:Describe*", "ecs:List*", "eks:Describe*", "eks:List*",
	"lambda:ListAliases", "lambda:ListEventSourceMappings", "lambda:ListFunctionEventInvokeConfigs",
	"lambda:ListFunctionUrlConfigs", "lambda:ListLayers", "lambda:ListLayerVersions",
	"lambda:ListProvisionedConcurrencyConfigs", "lambda:ListTags", "lambda:GetAccountSettings", "lambda:GetAlias",
	"lambda:GetFunctionConcurrency", "lambda:GetFunctionEventInvokeConfig", "lambda:GetFunctionUrlConfig",
	"lambda:GetRuntimeManagementConfig", "lambda:GetEventSourceMapping", "lambda:GetPolicy",
	"rds:Describe*", "rds:ListTagsForResource",
	"s3:ListAllMyBuckets", "s3:ListBucket", "s3:GetBucket*", "s3:GetLifecycleConfiguration",
	"s3:GetEncryptionConfiguration", "s3:GetReplicationConfiguration", "s3:GetAccountPublicAccessBlock",
	"route53:Get*", "route53:List*", "cloudfront:Get*", "cloudfront:List*",
	"cloudwatch:Describe*", "cloudwatch:Get*", "cloudwatch:List*",
	"logs:Describe*", "logs:FilterLogEvents", "logs:GetLogEvents", "logs:GetLogGroupFields", "logs:GetLogRecord",
	"logs:GetQueryResults", "logs:StartQuery", "logs:StopQuery", "logs:StartLiveTail", "logs:StopLiveTail",
	"logs:ListTagsForResource",
	"ssm:DescribeInstanceInformation", "ssm:DescribeInstancePatchStates", "ssm:DescribeParameters",
	"ssm:DescribeMaintenanceWindows", "ssm:ListAssociations", "ssm:ListTagsForResource", "ssm:GetConnectionStatus",
	"cloudtrail:LookupEvents", "cloudtrail:DescribeTrails", "cloudtrail:GetTrailStatus", "cloudtrail:GetEventSelectors",
	"cloudtrail:ListTrails",
	"ecr:DescribeRepositories", "ecr:DescribeImages", "ecr:DescribeImageScanFindings", "ecr:ListImages",
	"ecr:GetLifecyclePolicy", "ecr:GetRepositoryPolicy",
	"acm:DescribeCertificate", "acm:ListCertificates", "kms:DescribeKey", "kms:ListAliases", "kms:ListKeys",
	"tag:GetResources", "tag:GetTagKeys", "tag:GetTagValues", "health:Describe*",
}

// atlasActions is everything the atlas's role may do: one listing per thing
// the atlas draws, each named whole and none a wildcard. NOTHING HERE ANSWERS
// A SECRET: no Lambda action (every one that lists functions answers their
// environment), no instance attribute (user data), no parameter, no object.
var atlasActions = []string{
	"ec2:DescribeRegions", "ec2:DescribeInstances", "rds:DescribeDBInstances",
	"elasticloadbalancing:DescribeLoadBalancers", "sqs:ListQueues", "s3:ListAllMyBuckets",
	"route53:ListHostedZones", "route53:ListResourceRecordSets", "iam:ListAccountAliases",
}

// Operate: running state, never topology or access. Restart, scale, invalidate,
// alarms and dashboards, object reads and writes, plain parameter reads.
var operateActions = []string{
	"ec2:StartInstances", "ec2:StopInstances", "ec2:RebootInstances",
	"autoscaling:SetDesiredCapacity", "autoscaling:UpdateAutoScalingGroup", "autoscaling:SuspendProcesses",
	"autoscaling:ResumeProcesses", "autoscaling:SetInstanceHealth", "autoscaling:StartInstanceRefresh",
	"autoscaling:CancelInstanceRefresh",
	"ecs:UpdateService", "ecs:StopTask", "eks:UpdateNodegroupConfig",
	"lambda:InvokeFunction", "lambda:PutFunctionConcurrency", "lambda:DeleteFunctionConcurrency", "lambda:GetFunction",
	"rds:RebootDBInstance", "rds:StartDBInstance", "rds:StopDBInstance", "rds:RebootDBCluster", "rds:StartDBCluster",
	"rds:StopDBCluster", "rds:FailoverDBCluster", "rds:CreateDBSnapshot", "rds:CreateDBClusterSnapshot",
	"rds:DownloadDBLogFilePortion",
	"s3:GetObject", "s3:GetObjectVersion", "s3:PutObject", "s3:DeleteObject",
	"elasticloadbalancing:RegisterTargets", "elasticloadbalancing:DeregisterTargets", "cloudfront:CreateInvalidation",
	"cloudwatch:PutMetricAlarm", "cloudwatch:DeleteAlarms", "cloudwatch:SetAlarmState", "cloudwatch:EnableAlarmActions",
	"cloudwatch:DisableAlarmActions", "cloudwatch:PutDashboard", "cloudwatch:DeleteDashboards", "cloudwatch:PutMetricData",
	"ssm:GetParameter", "ssm:GetParameters", "ssm:GetParametersByPath",
}

// Administer: whole services, so it can create persistent access (key pairs,
// user data, database users, bucket policies). The boundary still refuses the
// never list and gates acting as a workload's identity behind gateTag.
var administerServices = []string{
	"ec2:*", "autoscaling:*", "application-autoscaling:*", "elasticloadbalancing:*", "ecs:*", "eks:*", "lambda:*",
	"rds:*", "s3:*", "route53:*", "cloudfront:*", "cloudwatch:*", "logs:*", "ssm:*", "ssmmessages:*",
	"ec2-instance-connect:*", "ecr:*", "acm:*",
}

// KMS use only on behalf of the services an Administer seat runs: encrypted
// volumes, snapshots, objects, functions, images and SecureString writes.
// Never directly, and the boundary denies every decrypt through SSM.
var administerKMSVia = []string{
	"ec2.*.amazonaws.com", "rds.*.amazonaws.com", "s3.*.amazonaws.com", "lambda.*.amazonaws.com",
	"ecr.*.amazonaws.com", "eks.*.amazonaws.com", "ssm.*.amazonaws.com",
}

var kmsUse = []string{"kms:Decrypt", "kms:Encrypt", "kms:GenerateDataKey*", "kms:ReEncrypt*", "kms:CreateGrant", "kms:DescribeKey"}

// never is refused to every seat at every level, by the boundary.
var never = []string{
	// IAM writes and passing roles.
	"iam:Add*", "iam:Attach*", "iam:Change*", "iam:Create*", "iam:Deactivate*", "iam:Delete*", "iam:Detach*",
	"iam:Enable*", "iam:Pass*", "iam:Put*", "iam:Remove*", "iam:Reset*", "iam:Resync*", "iam:Set*", "iam:Tag*",
	"iam:Untag*", "iam:Update*", "iam:Upload*",
	// Anything that mints or chains to another credential.
	"sts:AssumeRole", "sts:AssumeRoleWithSAML", "sts:AssumeRoleWithWebIdentity", "sts:AssumeRoot",
	"sts:GetFederationToken", "sts:GetSessionToken", "sts:GetServiceBearerToken", "sts:SetSourceIdentity",
	"sts:TagSession", "ecr:GetAuthorizationToken", "ecr-public:GetAuthorizationToken", "rds-db:connect",
	"ec2:GetPasswordData", "acm:ExportCertificate",
	// Secret values and key management.
	"secretsmanager:GetSecretValue", "secretsmanager:BatchGetSecretValue",
	"kms:CreateAlias", "kms:CreateCustomKeyStore", "kms:CreateKey", "kms:Delete*", "kms:Disable*", "kms:Enable*",
	"kms:Import*", "kms:Put*", "kms:ReplicateKey", "kms:RetireGrant", "kms:RevokeGrant", "kms:ScheduleKeyDeletion",
	"kms:CancelKeyDeletion", "kms:Update*", "kms:RotateKeyOnDemand",
	// Billing, cost, the organization and the account.
	"aws-portal:*", "billing:*", "ce:*", "cur:*", "budgets:*", "bcm-data-exports:*", "payments:*", "tax:*",
	"invoicing:*", "savingsplans:*", "organizations:*", "account:*",
	// In-cluster admin, which reaches every pod's identity.
	"eks:CreateAccessEntry", "eks:UpdateAccessEntry", "eks:AssociateAccessPolicy", "eks:CreatePodIdentityAssociation",
	"eks:UpdatePodIdentityAssociation",
	// The audit trail.
	"cloudtrail:Create*", "cloudtrail:Delete*", "cloudtrail:Put*", "cloudtrail:Stop*", "cloudtrail:Update*",
}

// gated acts as a workload's own identity: a shell or command on an instance,
// new code in a function, a command in a task. Allowed only on resources the
// owner tagged gateTag=gateValue.
var gated = []string{
	"ssm:StartSession", "ssm:SendCommand", "ec2-instance-connect:SendSSHPublicKey",
	"ec2-instance-connect:SendSerialConsoleSSHPublicKey", "ecs:ExecuteCommand", "ecs:RunTask", "ecs:StartTask",
	"lambda:UpdateFunctionCode", "lambda:UpdateFunctionConfiguration",
}

var gatedResources = []string{
	"arn:{partition}:ec2:*:*:instance/*", "arn:{partition}:ecs:*:*:cluster/*",
	"arn:{partition}:ecs:*:*:task-definition/*", "arn:{partition}:lambda:*:*:function:*",
}

var tagWrites = []string{
	"ec2:CreateTags", "ec2:DeleteTags", "ssm:AddTagsToResource", "ssm:RemoveTagsFromResource",
	"ecs:TagResource", "ecs:UntagResource", "lambda:TagResource", "lambda:UntagResource",
}

// globalActions are answered from us-east-1 whatever region a seat works in.
var globalActions = []string{
	"iam:*", "sts:*", "cloudfront:*", "route53:*", "health:*", "s3:ListAllMyBuckets", "s3:GetAccountPublicAccessBlock",
	"s3:GetBucketLocation",
}

func levelDoc(name string) obj {
	switch name {
	case levelRead:
		return doc(stmt("CrewletRead", "Allow", readActions))
	case levelOperate:
		return doc(
			stmt("CrewletOperate", "Allow", operateActions),
			with(stmt("CrewletOperateObjectKeys", "Allow", []string{"kms:Decrypt", "kms:GenerateDataKey"}),
				"StringLike", "kms:ViaService", "s3.*.amazonaws.com"),
		)
	case levelAdminister:
		return doc(
			stmt("CrewletAdminister", "Allow", administerServices),
			with(stmt("CrewletAdministerServiceKeys", "Allow", kmsUse), "StringLike", "kms:ViaService", administerKMSVia...),
		)
	case levelIAMRead:
		return doc(stmt("CrewletIAMRead", "Allow", []string{"iam:Get*", "iam:List*"}))
	}
	panic("unknown level " + name)
}

func levelNames() []string { return []string{levelRead, levelOperate, levelAdminister, levelIAMRead} }

// boundaryDoc is the most any seat can ever do: the services the levels name,
// minus the never list, the gate and the regions outside the allowed set.
func boundaryDoc(regions []string) obj {
	services := append([]string{}, administerServices...)
	services = append(services, "cloudtrail:LookupEvents", "cloudtrail:Describe*", "cloudtrail:Get*", "cloudtrail:List*",
		"tag:Get*", "health:Describe*", "kms:Describe*", "kms:List*", "iam:Get*", "iam:List*")
	// Systems Manager documents its own tag key; the two keys AND together, so
	// either one carrying the owner's value lifts the deny.
	gate := with(with(stmt("GateActingAsAWorkload", "Deny", gated, gatedResources...),
		"StringNotEquals", "aws:ResourceTag/"+gateTag, gateValue), "StringNotEquals", "ssm:resourceTag/"+gateTag, gateValue)
	userData := with(with(stmt("GateUserDataOnRoleInstances", "Deny", []string{"ec2:ModifyInstanceAttribute"},
		"arn:{partition}:ec2:*:*:instance/*"), "Null", "ec2:InstanceProfile", "false"),
		"StringNotEquals", "aws:ResourceTag/"+gateTag, gateValue)
	regionDeny := with(obj{"Sid": "OnlyAllowedRegions", "Effect": "Deny", "NotAction": globalActions, "Resource": []string{"*"}},
		"StringNotEquals", "aws:RequestedRegion", regions...)
	return doc(
		stmt("CrewletServices", "Allow", services),
		with(stmt("ServiceKeys", "Allow", kmsUse), "StringLike", "kms:ViaService", administerKMSVia...),
		stmt("Never", "Deny", never),
		with(stmt("NeverDirectKeyUse", "Deny", []string{"kms:Decrypt", "kms:ReEncrypt*", "kms:CreateGrant", "kms:Sign", "kms:GenerateMac"}),
			"Null", "kms:ViaService", "true"),
		with(stmt("NeverSecretsThroughKeys", "Deny", []string{"kms:Decrypt"}),
			"StringLike", "kms:ViaService", "ssm.*.amazonaws.com", "secretsmanager.*.amazonaws.com"),
		with(stmt("ProtectCrewletTags", "Deny", tagWrites), "ForAnyValue:StringLike", "aws:TagKeys", "crewlet:*"),
		gate,
		userData,
		regionDeny,
	)
}

func levelARNs() []string {
	out := []string{}
	for _, n := range levelNames() {
		out = append(out, policyARN(n))
	}
	return append(out, denyAllARN)
}

// provisionerDoc is the engine's IAM reach: Crewlet's seat roles, created only
// with the boundary, given only the fixed level policies. It cannot become a
// seat: only the broker can.
func provisionerDoc() obj {
	b := policyARN(boundaryName)
	return doc(
		with(stmt("CreateSeatRolesWithTheBoundary", "Allow", []string{"iam:CreateRole"}, seatRolesARN),
			"ArnEquals", "iam:PermissionsBoundary", b),
		with(with(stmt("AttachOnlyLevels", "Allow", []string{"iam:AttachRolePolicy", "iam:DetachRolePolicy"}, seatRolesARN),
			"ArnEquals", "iam:PermissionsBoundary", b), "ArnEquals", "iam:PolicyARN", levelARNs()...),
		stmt("TagAndDeleteSeatRoles", "Allow", []string{"iam:TagRole", "iam:UntagRole", "iam:DeleteRole"}, seatRolesARN),
		stmt("ReadCrewletRoles", "Allow", []string{"iam:GetRole", "iam:GetRolePolicy", "iam:ListAttachedRolePolicies",
			"iam:ListRolePolicies", "iam:ListRoleTags", "iam:SimulatePrincipalPolicy"}, crewletRoles),
		stmt("ReadCrewletPolicies", "Allow", []string{"iam:GetPolicy", "iam:GetPolicyVersion", "iam:ListPolicyVersions"},
			"arn:{partition}:iam::{account}:policy/crewlet/{company}/*", denyAllARN),
		stmt("ListAndCount", "Allow", []string{"iam:ListRoles", "iam:GetAccountSummary"}),
		stmt("NeverWidenOrEscape", "Deny", []string{
			"iam:CreatePolicy", "iam:CreatePolicyVersion", "iam:DeletePolicy", "iam:DeletePolicyVersion",
			"iam:SetDefaultPolicyVersion", "iam:PutRolePermissionsBoundary", "iam:DeleteRolePermissionsBoundary",
			"iam:PutRolePolicy", "iam:DeleteRolePolicy", "iam:UpdateAssumeRolePolicy", "iam:PassRole", "iam:CreateUser",
			"iam:CreateAccessKey", "iam:CreateLoginProfile", "iam:UpdateLoginProfile", "iam:CreateServiceSpecificCredential",
			"iam:UploadSSHPublicKey", "iam:UploadSigningCertificate", "iam:CreateOpenIDConnectProvider",
			"iam:CreateSAMLProvider", "sts:GetFederationToken"}),
		stmt("NeverTouchItselfOrTheBroker", "Deny", []string{"iam:AttachRolePolicy", "iam:DetachRolePolicy", "iam:DeleteRole",
			"iam:TagRole", "iam:UntagRole", "iam:UpdateRole", "iam:UpdateRoleDescription"}, provisionerARN, brokerARN,
			atlasARN),
	)
}

// rolesAnywhereTrust lets Roles Anywhere sessions from this engine's CA,
// through this trust anchor, whose subject CN passes op, become the role.
// Profiles carry no certificate conditions; this is where the CN is checked.
func rolesAnywhereTrust(issuerCN, op, subjectCN string) obj {
	s := obj{"Effect": "Allow", "Principal": obj{"Service": "rolesanywhere.amazonaws.com"},
		"Action": []string{"sts:AssumeRole", "sts:TagSession", "sts:SetSourceIdentity"}}
	with(s, "ArnEquals", "aws:SourceArn", "{anchor}")
	with(s, "StringEquals", "aws:PrincipalTag/x509Issuer/CN", issuerCN)
	with(s, op, "aws:PrincipalTag/x509Subject/CN", subjectCN)
	return doc(s)
}

// provisionerTrustDoc admits exactly the provisioner's CN.
func provisionerTrustDoc(issuerCN string) obj {
	return rolesAnywhereTrust(issuerCN, "StringEquals", provisionerCN)
}

// brokerTrustDoc admits seat CNs only, so the provisioner's certificate cannot
// become the broker and a seat's certificate cannot become the provisioner.
func brokerTrustDoc(issuerCN string) obj {
	return rolesAnywhereTrust(issuerCN, "StringLike", seatCNPrefix+"*")
}

// atlasTrustDoc admits exactly the atlas's CN, which opens nothing else.
func atlasTrustDoc(issuerCN string) obj {
	return rolesAnywhereTrust(issuerCN, "StringEquals", atlasCN)
}

// atlasDoc is the atlas's reach: its listings, and a deny of everything else,
// so a policy somebody attaches to the role by hand widens nothing.
func atlasDoc() obj {
	return doc(
		stmt("CrewletAtlasRead", "Allow", atlasActions),
		obj{"Sid": "NothingElse", "Effect": "Deny", "NotAction": atlasActions, "Resource": []string{"*"}},
	)
}

// brokerDoc can only become seat roles; a broker session reaches nothing else.
func brokerDoc() obj {
	assume := []string{"sts:AssumeRole", "sts:SetSourceIdentity", "sts:TagSession"}
	return doc(
		stmt("BecomeSeats", "Allow", assume, seatRolesARN),
		obj{"Sid": "NothingElse", "Effect": "Deny", "NotAction": assume, "Resource": []string{"*"}},
		obj{"Sid": "OnlySeats", "Effect": "Deny", "Action": assume, "NotResource": []string{seatRolesARN}},
		// Already covered by NothingElse; stated on its own so no reading misses it.
		stmt("NoIAM", "Deny", []string{"iam:*"}),
	)
}

// seatCN is a seat's certificate name: its session name and its source identity.
func seatCN(company Company, slug string) string { return named(seatCNPrefix, company) + slug }

// seatTrustDoc lets only the broker assume a seat, and only in a session whose
// source identity is that seat's certificate CN.
func seatTrustDoc(seatCN string) obj {
	s := obj{"Sid": "CrewletBroker", "Effect": "Allow", "Principal": obj{"AWS": brokerARN},
		"Action": []string{"sts:AssumeRole", "sts:SetSourceIdentity", "sts:TagSession"}}
	return doc(with(s, "StringEquals", "aws:SourceIdentity", "CN="+seatCN))
}

// --- rendering ---

type vars struct {
	company                    Company
	partition, account, anchor string
}

// concrete replaces the tokens with real values, for the own-policy check and API calls.
func concrete(v any, x vars) any {
	r := strings.NewReplacer("{company}", string(x.company), "{partition}", x.partition, "{account}", x.account, "{anchor}", x.anchor)
	return walk(v, func(s string) any { return r.Replace(s) })
}

// templated writes the company in, and turns the remaining token-bearing
// strings into Fn::Sub, escaping a literal "${".
func templated(v any, company Company) any {
	r := strings.NewReplacer("{partition}", "${AWS::Partition}", "{account}", "${AWS::AccountId}",
		"{anchor}", "${TrustAnchor.TrustAnchorArn}")
	return walk(v, func(s string) any {
		s = named(s, company)
		if !strings.Contains(s, "{partition}") && !strings.Contains(s, "{account}") && !strings.Contains(s, "{anchor}") {
			return s
		}
		return obj{"Fn::Sub": r.Replace(strings.ReplaceAll(s, "${", "${!"))}
	})
}

func walk(v any, f func(string) any) any {
	switch t := v.(type) {
	case string:
		return f(t)
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = f(s)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = walk(e, f)
		}
		return out
	case []obj:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = walk(e, f)
		}
		return out
	case obj:
		out := obj{}
		for k, e := range t {
			out[k] = walk(e, f)
		}
		return out
	}
	return v
}

func compactJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// policySize counts characters the way IAM does: whitespace excluded.
func policySize(v any) int {
	n := 0
	for _, r := range string(compactJSON(v)) {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			n++
		}
	}
	return n
}

// normalizePolicy makes two documents comparable: single values become lists,
// lists are sorted, statements are ordered by Sid.
func normalizePolicy(raw []byte) (any, error) {
	var d map[string]any
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	var stmts []any
	switch s := d["Statement"].(type) {
	case []any:
		stmts = s
	case map[string]any:
		stmts = []any{s}
	}
	norm := make([]any, 0, len(stmts))
	for _, e := range stmts {
		s, _ := e.(map[string]any)
		out := map[string]any{}
		for k, v := range s {
			switch k {
			case "Action", "NotAction", "Resource", "NotResource":
				out[k] = sortedStrings(v)
			case "Principal":
				if m, ok := v.(map[string]any); ok {
					pm := map[string]any{}
					for pk, pv := range m {
						pm[pk] = sortedStrings(pv)
					}
					out[k] = pm
				} else {
					out[k] = v
				}
			case "Condition":
				cm := map[string]any{}
				for op, keys := range v.(map[string]any) {
					km := map[string]any{}
					for ck, cv := range keys.(map[string]any) {
						km[ck] = sortedStrings(cv)
					}
					cm[op] = km
				}
				out[k] = cm
			default:
				out[k] = v
			}
		}
		norm = append(norm, out)
	}
	sort.Slice(norm, func(i, j int) bool { return string(compactJSON(norm[i])) < string(compactJSON(norm[j])) })
	return map[string]any{"Version": d["Version"], "Statement": norm}, nil
}

func sortedStrings(v any) []string {
	var out []string
	switch t := v.(type) {
	case string:
		out = []string{t}
	case bool:
		out = []string{fmt.Sprint(t)}
	case []any:
		for _, e := range t {
			out = append(out, fmt.Sprint(e))
		}
	}
	sort.Strings(out)
	return out
}

// samePolicy compares a stored document (URL-encoded, as IAM returns it, or
// plain) with the expected one. The hint names what differs, never a value.
func samePolicy(stored string, want any) (bool, string) {
	if dec, err := url.PathUnescape(stored); err == nil && strings.HasPrefix(strings.TrimSpace(dec), "{") {
		stored = dec
	}
	got, err := normalizePolicy([]byte(stored))
	if err != nil {
		return false, "the stored document is not JSON"
	}
	exp, err := normalizePolicy(compactJSON(want))
	if err != nil {
		return false, "the expected document is not JSON"
	}
	if string(compactJSON(got)) == string(compactJSON(exp)) {
		return true, ""
	}
	gs := got.(map[string]any)["Statement"].([]any)
	es := exp.(map[string]any)["Statement"].([]any)
	seen := map[string]bool{}
	for _, s := range gs {
		seen[string(compactJSON(s))] = true
	}
	for _, s := range es {
		if !seen[string(compactJSON(s))] {
			sid, _ := s.(map[string]any)["Sid"].(string)
			return false, fmt.Sprintf("statement %q differs or is missing (%d stored, %d expected)", sid, len(gs), len(es))
		}
	}
	return false, fmt.Sprintf("the stored document has extra statements (%d stored, %d expected)", len(gs), len(es))
}
