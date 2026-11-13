package aws

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/setup"
)

func awsSource(t *testing.T) collect.Source {
	t.Helper()
	src, ok := collect.Lookup(integration.KindAWS)
	if !ok {
		t.Fatal("AWS registers no atlas source")
	}
	return src
}

func readAccount(t *testing.T, f *fakeCloud, edit ...func(*config.AWSProvisioning)) collect.Read {
	t.Helper()
	read, err := awsSource(t).Collect(context.Background(), f.input(t, edit...))
	if err != nil {
		t.Fatal(err)
	}
	return read
}

// said is every claim of a read as one line: the entry, then what is said.
func said(read collect.Read) []string {
	out := make([]string, 0, len(read.Claims))
	for _, c := range read.Claims {
		line := c.Subject.String()
		switch c.Facet.Kind {
		case atlas.FacetFact:
			line += " " + string(c.Facet.Fact) + "=" + c.Value.Text
		case atlas.FacetRelation:
			line += " " + string(c.Facet.Relation) + " " + c.Facet.Target.Entry.String()
		}
		out = append(out, line)
	}
	slices.Sort(out)
	return out
}

// THE ACCOUNT IS READ AS THE ATLAS'S OWN ROLE, AND AS WHAT THE ATLAS DRAWS:
// each instance a server, each database by its engine, each load balancer the
// name a record points at, each queue and bucket by name, each public zone
// and what its names resolve to.
func TestTheAccountIsReadAsTheAtlasDrawsIt(t *testing.T) {
	t.Parallel()
	f := newFakeCloud()
	read := readAccount(t, f)
	const account, web, lb = "vendor_account:aws/123456789012", "host:aws/123456789012/eu-west-1/i-0aaa1111bbbb2222c",
		"hostname:web-1234567890.eu-west-1.elb.amazonaws.com"
	want := []string{
		account,
		account + " account_name=acme-production",
		account + " capability=compute", account + " capability=database", account + " capability=dns",
		account + " capability=object_storage",
		account + " region=eu-west-1", account + " region=us-east-1",
		web, web + " name=web-1", web + " address=10.0.1.5", web + " address=203.0.113.10", web + " region=eu-west-1",
		web + " belongs_to " + account,
		// An instance with no Name tag is named by its id.
		"host:aws/123456789012/us-east-1/i-0123456789abcdef0 name=i-0123456789abcdef0",
		"datastore:aws/123456789012/eu-west-1/orders engine=postgres",
		"datastore:aws/123456789012/eu-west-1/orders engine_version=16.4",
		"datastore:aws/123456789012/eu-west-1/orders port=5432",
		"datastore:aws/123456789012/eu-west-1/orders runs_on " + account,
		"datastore:aws/123456789012/eu-west-1/legacy engine=mysql",
		"datastore:aws/123456789012/eu-west-1/legacy engine_version=8.0",
		lb, lb + " belongs_to " + account,
		"queue:aws/123456789012/us-east-1/orders-events name=orders-events",
		"queue:aws/123456789012/us-east-1/orders-events.fifo runs_on " + account,
		"bucket:s3/acme-assets region=eu-west-1", "bucket:s3/acme-logs belongs_to " + account,
		"domain:acme.example served_by " + account,
		// An alias to a load balancer meets the balancer's own name.
		"hostname:acme.example resolves_to " + lb,
		"hostname:acme.example in_zone domain:acme.example",
		"hostname:api.acme.example resolves_to hostname:acme.example",
		"hostname:vpn.acme.example resolves_to host:addr/198.51.100.7",
		"hostname:*.apps.acme.example resolves_to host:addr/2001:db8::7",
	}
	got := said(read)
	for _, line := range want {
		if !slices.Contains(got, line) {
			t.Errorf("the read does not say %q", line)
		}
	}
	for _, line := range got {
		for _, absent := range []string{"i-0ddd3333eeee4444f", "corp.internal", "10.0.9.9", "ap-south-1", "spf1", "mail.acme"} {
			if strings.Contains(line, absent) {
				t.Errorf("the read says %q: a terminated instance, a private zone, an empty Region and a record "+
					"that is no address are not recorded", line)
			}
		}
	}
	if len(read.Unread) != 0 || len(read.Withheld) != 0 {
		t.Errorf("a whole read left %v unread and withheld %v", read.Unread, read.Withheld)
	}
	if !slices.Equal(f.sessions, [][2]string{{"crewlet-acme-atlas", testAtlasProfile}}) {
		t.Errorf("the read signed in as %v, want once as the atlas through its own profile", f.sessions)
	}
	if read.Calls != len(f.asked())+1 {
		t.Errorf("the read counts %d calls, made %d listings and one sign-in", read.Calls, len(f.asked()))
	}
}

// EVERY REQUEST THE READ MAKES IS AN ACTION THE ROLE'S POLICY NAMES, and the
// policy names nothing the read does not ask for: the list in the stack is
// the list in the client, in both directions.
func TestTheReadAsksOnlyWhatTheRoleAllowsAndTheRoleAllowsOnlyThat(t *testing.T) {
	t.Parallel()
	f := newFakeCloud()
	readAccount(t, f)
	asked := f.asked()
	for _, action := range asked {
		if !slices.Contains(atlasActions, action) {
			t.Errorf("the read asks %s, which the atlas's policy does not allow", action)
		}
	}
	for _, action := range atlasActions {
		if !slices.Contains(asked, action) {
			t.Errorf("the atlas's policy allows %s, which no read asks", action)
		}
	}
}

// THE ATLAS'S ROLE HOLDS NO WILDCARD AND NO READ THAT ANSWERS A SECRET: no
// Lambda action at all (listing functions answers their environment), no
// instance attribute (user data), no parameter, object, log or secret.
func TestTheAtlasRoleNamesEveryActionAndNoneAnswersASecret(t *testing.T) {
	t.Parallel()
	for _, action := range atlasActions {
		service, name, ok := strings.Cut(action, ":")
		if !ok || strings.ContainsAny(action, "*?") || service == "" || name == "" {
			t.Errorf("%q is not one action named whole", action)
		}
		if service == "lambda" || service == "ssm" || service == "secretsmanager" || service == "logs" ||
			service == "kms" || service == "sts" || service == "ecs" {
			t.Errorf("the atlas's role reaches %s", action)
		}
		if !strings.HasPrefix(name, "Describe") && !strings.HasPrefix(name, "List") {
			t.Errorf("%s is not a listing", action)
		}
	}
	for _, secret := range []string{"lambda:ListFunctions", "lambda:GetFunction", "lambda:GetFunctionConfiguration",
		"ec2:DescribeInstanceAttribute", "ec2:DescribeLaunchTemplateVersions", "ecs:DescribeTaskDefinition",
		"ssm:GetParameter", "s3:GetObject", "s3:ListBucket", "secretsmanager:GetSecretValue", "rds:DescribeDBSnapshots",
		"iam:ListUsers", "iam:GetRole", "sts:AssumeRole"} {
		if grants(atlasActions, secret) {
			t.Errorf("the atlas's role allows %s", secret)
		}
	}
	stmts := atlasDoc()["Statement"].([]obj)
	if len(stmts) != 2 || stmts[1]["Effect"] != "Deny" || !slices.Equal(stmts[1]["NotAction"].([]string), atlasActions) {
		t.Fatalf("the atlas's policy must deny everything but its listings, got %v", stmts)
	}
	if got := atlasTrustDoc("crewlet-ca")["Statement"].([]obj)[0]["Condition"].(obj)["StringEquals"].(obj)["aws:PrincipalTag/x509Subject/CN"].([]string); !slices.Equal(got, []string{atlasCN}) {
		t.Fatalf("the atlas's role admits %v, want only its own certificate name", got)
	}
	for _, co := range []Company{"acme", "a", Company(strings.Repeat("c", 20))} {
		cn := named(atlasCN, co)
		if !cnPattern().MatchString(cn) || cn == named(provisionerCN, co) || matches(named(seatCNPrefix, co)+"*", cn) {
			t.Errorf("the atlas's certificate name %q is not its own", cn)
		}
	}
}

// NOTHING THE READER DOES NOT MAP REACHES THE ATLAS: a tag, user data, a
// master user's name, a database's address, a secret's name, a TXT record.
func TestNoUnmappedFieldReachesTheAtlasFromAWS(t *testing.T) {
	t.Parallel()
	f := newFakeCloud()
	out, err := json.Marshal(readAccount(t, f))
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{f.canary, "db.example", "spf1"} {
		if strings.Contains(string(out), absent) {
			t.Errorf("the read carries %q, which sits only in fields the reader does not map", absent)
		}
	}
}

// NO CREDENTIAL'S SHAPE REACHES THE ATLAS FROM AWS, whichever field carries
// it: a mapped one is refused by the vocabulary, and an unmapped one is never
// read.
func TestNoCredentialShapeReachesTheAtlasFromAWS(t *testing.T) {
	t.Parallel()
	for _, shape := range redacttest.Shapes() {
		s := shape.Sample
		// A name long enough that cutting it to a label's length would cut
		// the shape short of its end, where no rule knows it any more.
		long := strings.Repeat("m", max(1, atlas.MaxLabelRunes-len(s)+len(s)/4)) + " " + s
		f := newFakeCloud()
		f.canary, f.alias = s, s
		f.regions = append(f.regions, s)
		f.instances["eu-west-1"] = append(f.instances["eu-west-1"],
			fakeInstance{ID: s, State: "running", Name: "by-id"},
			fakeInstance{ID: "i-0fff5555aaaa6666b", State: s, Name: long, Private: s, Public: s})
		f.databases["eu-west-1"] = append(f.databases["eu-west-1"],
			fakeDatabase{ID: s, Engine: s, Version: s, Status: s, Port: 5432},
			fakeDatabase{ID: "plain", Engine: "postgres", Version: "16." + s, Status: "available", Port: 5432})
		f.balancers["eu-west-1"] = append(f.balancers["eu-west-1"], s, s+".eu-west-1.elb.amazonaws.com")
		f.queues["us-east-1"] = append(f.queues["us-east-1"], s, "q/"+s)
		f.buckets[s] = s
		f.buckets["plain-bucket"] = s
		f.zones = append(f.zones, fakeZone{ID: s, Name: s + ".", Records: []fakeRecord{{Name: s, Type: "A", Values: []string{s}}}})
		f.zones[0].Records = append(f.zones[0].Records,
			fakeRecord{Name: s + ".acme.example.", Type: "CNAME", Values: []string{s}},
			fakeRecord{Name: "t.acme.example.", Type: "A", Alias: s},
			fakeRecord{Name: "u.acme.example.", Type: "TXT", Values: []string{s}},
			fakeRecord{Name: "v.acme.example.", Type: "A", Values: []string{s}})
		read, err := awsSource(t).Collect(context.Background(), f.input(t))
		if err != nil {
			t.Fatalf("%s: %v", shape.Name, redact.Secrets(err.Error()))
		}
		out, err := json.Marshal(read)
		if err != nil {
			t.Fatal(err)
		}
		for _, piece := range []string{s[len(s)/2:], s[len(s)/4 : len(s)/4+12]} {
			if at := strings.Index(string(out), piece); at >= 0 {
				t.Errorf("%s: a piece of the shape reached the read near %q", shape.Name, string(out)[max(0, at-120):at])
			}
		}
	}
}

// A LISTING AWS REFUSES IS A FINDING, NEVER A FAULT, AND WITHDRAWS NOTHING:
// what an earlier read recorded of that part of that Region stands, and
// every other part is read.
func TestARefusedListingIsUnreadAndItsEarlierClaimsStand(t *testing.T) {
	t.Parallel()
	whole := readAccount(t, newFakeCloud())
	f := newFakeCloud()
	f.refuse["ec2:DescribeInstances@eu-west-1"] = true
	f.refuse["s3:ListAllMyBuckets"] = true
	f.refuse["route53:ListResourceRecordSets"] = true
	read := readAccount(t, f)
	parts := []string{}
	for _, u := range read.Unread {
		parts = append(parts, u.Part)
	}
	slices.Sort(parts)
	if !slices.Equal(parts, []string{partBuckets, partDNS, partInstances}) {
		t.Fatalf("unread %v, want the buckets, the DNS and the instances", parts)
	}
	got := said(read)
	if !slices.Contains(got, "datastore:aws/123456789012/eu-west-1/orders engine=postgres") ||
		!slices.Contains(got, "host:aws/123456789012/us-east-1/i-0123456789abcdef0") {
		t.Errorf("a refused listing kept the others from being read: %v", got)
	}
	// Every claim the whole read made that this one lacks is one a refused
	// part holds, so the duty carries it rather than withdrawing it.
	for _, c := range whole.Claims {
		if slices.ContainsFunc(read.Claims, func(o atlas.Claim) bool { return o.Subject == c.Subject && o.Slot() == c.Slot() }) {
			continue
		}
		if !slices.ContainsFunc(read.Unread, func(u collect.Unread) bool { return u.Holds(c) }) {
			t.Errorf("the claim %s %s would be withdrawn by a read that was only refused", c.Subject, c.Slot())
		}
	}
	// And a part that was read holds nothing: what it no longer lists goes.
	for _, c := range read.Claims {
		if c.Subject.Kind == atlas.KindDatastore || c.Subject.Kind == atlas.KindQueue {
			if slices.ContainsFunc(read.Unread, func(u collect.Unread) bool { return u.Holds(c) }) {
				t.Errorf("a refused part holds %s, which another part read", c.Subject)
			}
		}
	}
}

// A LISTING THAT FAILS FOR ANY OTHER REASON FAILS THE READ WHOLE, so half a
// listing never retires the other half; and so does a sign-in AWS refuses.
func TestAFaultOrARefusedSignInFailsTheReadWhole(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"ec2:DescribeRegions", "rds:DescribeDBInstances@eu-west-1", "sqs:ListQueues@us-east-1",
		"route53:ListHostedZones", "s3:ListAllMyBuckets", "iam:ListAccountAliases"} {
		f := newFakeCloud()
		f.fail[action] = true
		if read, err := awsSource(t).Collect(context.Background(), f.input(t)); err == nil {
			t.Errorf("a fault in %s read %d claims, want the read to fail", action, len(read.Claims))
		}
	}
	f := newFakeCloud()
	f.refuse["ec2:DescribeRegions"] = true
	if _, err := awsSource(t).Collect(context.Background(), f.input(t)); !errors.Is(err, integration.ErrCredentialRejected) {
		t.Errorf("a refused first listing = %v, want the credential's refusal", err)
	}
	f = newFakeCloud()
	f.refuse["session"] = true
	if _, err := awsSource(t).Collect(context.Background(), f.input(t)); err == nil || len(f.asked()) != 0 {
		t.Errorf("a refused sign-in = %v after %v, want a failed read that listed nothing", err, f.asked())
	}
}

// THE READER IS THE COMPANY'S CHOICE: off, the account is not a source of
// the atlas at all and AWS is never called; on with a stack that does not
// hold the role yet, the read says the one step to take and keeps everything.
func TestTheReaderIsOffUntilAskedForAndSaysTheStepWhenTheStackLacksIt(t *testing.T) {
	t.Parallel()
	f := newFakeCloud()
	off := f.input(t, func(p *config.AWSProvisioning) { p.AtlasRead = false })
	if collect.Connected(awsSource(t), off.Config) || !collect.Connected(awsSource(t), f.input(t).Config) {
		t.Fatal("the AWS source is connected whatever the switch says: off, it must not be read or listed")
	}
	_, err := awsSource(t).Collect(context.Background(), off)
	if !errors.Is(err, integration.ErrNotConfigured) || len(f.sessions) != 0 {
		t.Fatalf("with the reader off the read = %v after %d sign-ins, want not configured and none", err, len(f.sessions))
	}
	five := strings.Join(strings.Split(atlasConnection(), ",")[:5], ",")
	read := readAccount(t, f, func(p *config.AWSProvisioning) { p.Connection = five })
	if len(read.Claims) != 0 || len(read.Unread) != 1 || read.Unread[0].Part != partReader ||
		!strings.Contains(read.Unread[0].Detail, "CloudShell block") || len(f.sessions) != 0 {
		t.Fatalf("a stack without the role read %+v after %d sign-ins", read, len(f.sessions))
	}
	if !read.Unread[0].Holds(atlas.Claim{Subject: atlas.MustRef(atlas.KindBucket, "s3/acme-assets")}) {
		t.Error("a stack without the role must keep what an earlier read recorded")
	}
}

// A LISTING IS WALKED TO ITS LAST PAGE, with the cursor each service gives.
func TestEveryPageOfAListingIsRead(t *testing.T) {
	t.Parallel()
	f := newFakeCloud()
	f.page = 1
	f.instances["eu-west-1"] = append(f.instances["eu-west-1"],
		fakeInstance{ID: "i-0bbb7777cccc8888d", State: "running", Name: "web-2", Private: "10.0.1.6"})
	got := said(readAccount(t, f))
	for _, line := range []string{"host:aws/123456789012/eu-west-1/i-0bbb7777cccc8888d name=web-2",
		"hostname:vpn.acme.example resolves_to host:addr/198.51.100.7",
		"hostname:*.apps.acme.example resolves_to host:addr/2001:db8::7"} {
		if !slices.Contains(got, line) {
			t.Errorf("a paged read does not say %q", line)
		}
	}
}

// A THROTTLED LISTING IS ASKED AGAIN, as AWS's own clients do.
func TestAThrottledListingIsRetried(t *testing.T) {
	t.Parallel()
	f := newFakeCloud()
	throttled := 2
	inner := f.http().Transport
	hc := f.http()
	hc.Transport = roundTrip(func(req *http.Request) (*http.Response, error) {
		if strings.HasPrefix(req.Host, "rds.") && throttled > 0 {
			throttled--
			return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}, Body: io.NopCloser(
				strings.NewReader("<ErrorResponse><Error><Code>Throttling</Code><Message>slow down</Message></Error></ErrorResponse>"))}, nil
		}
		return inner.RoundTrip(req)
	})
	conn, err := NewConnection(&config.AWSProvisioning{Slug: "acme", HomeRegion: "us-east-2",
		CertificateAuthority: mustBundle(t), Connection: atlasConnection()})
	if err != nil {
		t.Fatal(err)
	}
	client := newClient(conn, hc, "")
	client.ra.wait = func(int) time.Duration { return time.Millisecond }
	reader, err := client.NewAtlasClient()
	if err != nil {
		t.Fatal(err)
	}
	found, err := reader.Databases(context.Background(), "eu-west-1")
	if err != nil || len(found) != 2 || throttled != 0 {
		t.Fatalf("a twice-throttled listing = %v, %v with %d throttles left", found, err, throttled)
	}
}

// THE STACK HOLDS THE ATLAS'S ROLE ONLY WHERE THE COMPANY ASKED FOR IT, with
// the trust and the policy the check expects, a profile that offers that role
// alone, and its two ARNs last in the output. Rendered without it, the stack
// names nothing of the atlas's, so the next deploy deletes the role.
func TestTheStackHoldsTheAtlasRoleOnlyWhenAskedFor(t *testing.T) {
	t.Parallel()
	ca := testCA(t)
	params := StackParams{Company: "acme", CAPEM: ca.CertificatePEM(), IssuerCN: ca.Cert.Subject.CommonName,
		Regions: []string{"us-east-2"}}
	without, err := RenderStack(params)
	if err != nil {
		t.Fatal(err)
	}
	// The provisioner is refused the role's ARN either way; nothing creates it.
	if strings.Contains(string(without), "AtlasRole") || strings.Contains(string(without), "AtlasProfile") ||
		strings.Contains(string(without), `"RoleName": "crewlet-acme-atlas"`) {
		t.Fatal("a stack rendered without the atlas's role creates it")
	}
	params.Atlas = true
	raw, err := RenderStack(params)
	if err != nil {
		t.Fatal(err)
	}
	var tpl map[string]any
	if err = json.Unmarshal(raw, &tpl); err != nil {
		t.Fatal(err)
	}
	x := vars{company: "acme", partition: "aws", account: testAccount, anchor: testAnchor}
	res := tpl["Resources"].(map[string]any)
	props := func(id string) map[string]any { return res[id].(map[string]any)["Properties"].(map[string]any) }
	role := props("AtlasRole")
	for label, pair := range map[string][2]any{
		"trust":  {role["AssumeRolePolicyDocument"], atlasTrustDoc(ca.Cert.Subject.CommonName)},
		"policy": {role["Policies"].([]any)[0].(map[string]any)["PolicyDocument"], atlasDoc()},
	} {
		b, _ := json.Marshal(resolveSub(pair[0], x))
		if ok, hint := samePolicy(string(b), concrete(pair[1], x)); !ok {
			t.Errorf("the atlas's %s: %s", label, hint)
		}
	}
	if role["RoleName"] != "crewlet-acme-atlas" || role["Path"] != "/crewlet/acme/" || role["ManagedPolicyArns"] != nil {
		t.Errorf("the atlas's role is %v under %v", role["RoleName"], role["Path"])
	}
	arns := props("AtlasProfile")["RoleArns"].([]any)
	if len(arns) != 1 || arns[0].(map[string]any)["Fn::GetAtt"].([]any)[0] != "AtlasRole" {
		t.Errorf("the atlas's profile offers %v, want only its role", arns)
	}
	for _, profile := range []string{"ProvisionerProfile", "SeatProfile"} {
		if strings.Contains(string(compactJSON(props(profile)["RoleArns"])), "Atlas") {
			t.Errorf("%s offers the atlas's role", profile)
		}
	}
	joined := tpl["Outputs"].(map[string]any)[config.AWSConnectionOutput].(map[string]any)["Value"].(map[string]any)["Fn::Join"].([]any)[1].([]any)
	if len(joined) != 7 || !strings.Contains(string(compactJSON(joined[5:])), `["AtlasProfile","ProfileArn"]`) ||
		!strings.Contains(string(compactJSON(joined[6:])), `["AtlasRole","Arn"]`) {
		t.Errorf("the output joins %s, want the atlas's profile and role last", compactJSON(joined))
	}
	if n := policySize(concrete(atlasDoc(), x)); n > 10240 {
		t.Errorf("the atlas's inline policy is %d characters, over IAM's 10240", n)
	}
	if n := policySize(concrete(atlasTrustDoc("crewlet-ca-0011aabb"), x)); n > trustPolicyLimit {
		t.Errorf("the atlas's trust policy is %d characters, over %d", n, trustPolicyLimit)
	}
}

// atlasStack is [fakeStack] for a connection whose output carries the
// atlas's role, the role answered as this engine renders it.
func atlasStack(t *testing.T, edit func(action, name string, doc *obj, extra *string)) *Client {
	t.Helper()
	prov := testProvisioning(t)
	prov.Connection = atlasConnection()
	conn, err := NewConnection(prov)
	if err != nil {
		t.Fatal(err)
	}
	regions := []string{"us-east-2", "us-east-1"}
	base := fakeStack(t, regions, edit)
	inner := base.answer
	issuer := conn.CA.Cert.Subject.CommonName
	render := func(d obj) string { return url.PathEscape(string(compactJSON(concrete(d, conn.vars())))) }
	base.answer = func(form url.Values) (int, string) {
		action, name := form.Get("Action"), form.Get("RoleName")
		if name != conn.resolve(atlasName) {
			return inner(form)
		}
		switch action {
		case "GetRole":
			trust, missing := atlasTrustDoc(issuer), ""
			edit(action, name, &trust, &missing)
			if missing != "" {
				return iamError(http.StatusNotFound, "NoSuchEntity")
			}
			return iamOK(action, "<Role><RoleName>"+name+"</RoleName><AssumeRolePolicyDocument>"+render(trust)+
				"</AssumeRolePolicyDocument></Role>")
		case "GetRolePolicy":
			d := atlasDoc()
			edit(action, name, &d, nil)
			return iamOK(action, "<PolicyDocument>"+render(d)+"</PolicyDocument>")
		}
		return inner(form)
	}
	base.grants = func(cn, profile string) bool {
		return (cn == conn.resolve(provisionerCN) && profile == conn.ProvisionerProfileARN) ||
			(cn == conn.resolve(atlasCN) && profile == conn.AtlasProfileARN)
	}
	c := base.client(t)
	c.conn = conn
	return c
}

// THE CHECK HOLDS THE ATLAS'S ROLE TO WHAT THIS ENGINE RENDERS, APART FROM
// WHAT AGENTS DEPEND ON: a role that is missing, widened or absent from the
// output is said under Atlas and never as drift, and an identity that opens
// what it must not is a split like any other.
func TestTheCheckHoldsTheAtlasRoleApartFromTheAgents(t *testing.T) {
	t.Parallel()
	regions := []string{"us-east-2", "us-east-1"}
	none := func(string, string, *obj, *string) {}
	r, err := atlasStack(t, none).CheckStack(context.Background(), regions, true)
	if err != nil || len(r.Atlas) != 0 || len(r.Drift) != 0 || len(r.Split) != 0 {
		t.Fatalf("a rendered stack with the atlas's role reads %+v, %v", r, err)
	}
	// Not asked for, the role is not read at all.
	off := fakeStack(t, regions, none)
	if r, err = off.client(t).CheckStack(context.Background(), regions, false); err != nil || len(r.Atlas) != 0 ||
		slices.ContainsFunc(off.calls, func(f url.Values) bool { return strings.HasSuffix(f.Get("RoleName"), "-atlas") }) {
		t.Fatalf("with the reader off the check read %+v, %v", r, err)
	}
	// Asked for, with an output from before the role: one sentence, no drift.
	if r, err = fakeStack(t, regions, none).client(t).CheckStack(context.Background(), regions, true); err != nil ||
		len(r.Atlas) != 1 || !strings.Contains(r.Atlas[0], "crewlet-acme-atlas") || len(r.Drift) != 0 {
		t.Fatalf("an output without the role reads %+v, %v", r, err)
	}
	for name, tc := range map[string]struct {
		edit func(action, name string, doc *obj, extra *string)
		want string
	}{
		"a role somebody deleted": {want: "is missing", edit: func(a, n string, _ *obj, x *string) {
			if a == "GetRole" && strings.HasSuffix(n, "-atlas") && x != nil {
				*x = "gone"
			}
		}},
		"a widened policy": {want: "policy of crewlet-acme-atlas", edit: func(a, n string, d *obj, _ *string) {
			if a == "GetRolePolicy" && strings.HasSuffix(n, "-atlas") {
				*d = doc(stmt("CrewletAtlasRead", "Allow", []string{"lambda:ListFunctions"}))
			}
		}},
		"a trust admitting any name": {want: "trust policy of crewlet-acme-atlas", edit: func(a, n string, d *obj, _ *string) {
			if a == "GetRole" && strings.HasSuffix(n, "-atlas") {
				*d = rolesAnywhereTrust("x", "StringLike", "*")
			}
		}},
	} {
		r, err := atlasStack(t, tc.edit).CheckStack(context.Background(), regions, true)
		if err != nil || len(r.Drift) != 0 ||
			!slices.ContainsFunc(r.Atlas, func(s string) bool { return strings.Contains(s, tc.want) }) {
			t.Errorf("%s: %+v, %v; want it named under Atlas and not as drift", name, r, err)
		}
	}
	// An identity that opens what it must not is a split, each one named.
	open := atlasStack(t, none)
	open.ra.http = &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/sessions" {
			return open.http.Transport.RoundTrip(req)
		}
		return &http.Response{StatusCode: http.StatusCreated, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(
			`{"credentialSet":[{"credentials":{"accessKeyId":"ASIAPROVEXAMPLE00001","secretAccessKey":"s",` +
				`"sessionToken":"t","expiration":"2099-01-01T00:00:00Z"}}]}`))}, nil
	})}
	if r, err = open.CheckStack(context.Background(), regions, true); err != nil || len(r.Split) != 6 {
		t.Fatalf("with every sign-in granted the check names %d splits (%v), want the two and the atlas's four", len(r.Split), err)
	}
}

// THE CONNECT FORM OFFERS THE READER AND NEVER REQUIRES IT: an optional
// switch, off by default, that says the one step and how to undo it, and
// turning it on changes the block the form shows.
func TestTheConnectFormOffersTheAtlasReader(t *testing.T) {
	t.Parallel()
	bundle := mustBundle(t)
	resolve := func(string) (string, bool) { return bundle, true }
	block := func(on bool) (string, bool) {
		in := &config.AWS{Enabled: true, Provisioning: &config.AWSProvisioning{Slug: "acme", HomeRegion: "us-east-2",
			CertificateAuthority: "${AWS_CERTIFICATE_AUTHORITY}", AtlasRead: on}}
		text, present := "", false
		for _, req := range Requirements(in, "Acme", resolve) {
			switch req.Field {
			case "atlas_read":
				if req.Required || req.Connect || req.Blocks != "" || req.Default != "false" ||
					!strings.Contains(req.More, "run the CloudShell block again") || !strings.Contains(req.More, "To undo it") {
					t.Errorf("the reader's switch is %+v: it must be optional, off, and say its step and its undo", req)
				}
				present = req.Present
			case "certificate_authority":
				if req.Public != nil {
					text = req.Public.Text
				}
			}
		}
		return text, present
	}
	offText, offOn := block(false)
	onText, onOn := block(true)
	if offOn || !onOn {
		t.Fatalf("the switch reads %v off and %v on", offOn, onOn)
	}
	if strings.Contains(offText, `"RoleName": "crewlet-acme-atlas"`) || !strings.Contains(onText, `"RoleName": "crewlet-acme-atlas"`) ||
		!strings.Contains(onText, "atlas_read") {
		t.Fatal("the block must hold the atlas's role exactly when the switch is on, and say so")
	}
}

// TEST PASSES A STACK WHOSE ATLAS ROLE IS NOT THERE YET, with the step to
// take beside it: agents work without the role, so it refuses nothing.
func TestAMissingAtlasRoleWarnsBesideAWorkingStack(t *testing.T) {
	t.Parallel()
	v := verdictOf(mustConnection(t), StackReport{Version: StackVersion,
		Atlas: []string{"The stack output does not carry the Atlas reader crewlet-acme-atlas"}})
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if v.Outcome != setup.VerifyWorks || !strings.Contains(string(raw), "Atlas reader crewlet-acme-atlas.") ||
		!strings.Contains(string(raw), "Agents are not affected") {
		t.Fatalf("the verdict is %s", raw)
	}
}
