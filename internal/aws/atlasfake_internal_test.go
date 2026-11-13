package aws

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sigv4"
)

// The atlas's two ARNs in acme's stack output.
const (
	testAtlasProfile = "arn:aws:rolesanywhere:us-east-2:123456789012:profile/20010203-0405-4607-8809-0a0b0c0d0e0f"
	testAtlasRole    = "arn:aws:iam::123456789012:role/crewlet/acme/crewlet-acme-atlas"
	testAccount      = "123456789012"
)

// atlasConnection is acme's stack output with the atlas's role in it.
func atlasConnection() string {
	return strings.Join([]string{testAnchor, testProvProfile, testProv, testSeatProfile, testBroker,
		testAtlasProfile, testAtlasRole}, ",")
}

// fakeInstance, fakeDatabase and fakeZone are what the fake account holds.
type fakeInstance struct{ ID, State, Name, Private, Public string }

type fakeDatabase struct {
	ID, Engine, Version, Status string
	Port                        int
}

type fakeRecord struct {
	Name, Type, Alias string
	Values            []string
}

type fakeZone struct {
	ID, Name string
	Private  bool
	Records  []fakeRecord
}

// fakeCloud is an AWS account as the atlas's role reads it, answering each
// listing in AWS's own shape with fields the reader never maps beside the
// ones it does.
type fakeCloud struct {
	mu sync.Mutex
	// actions is the IAM action of every listing asked for, in order, and
	// sessions the certificate name and profile of every sign-in.
	actions  []string
	sessions [][2]string

	alias     string
	regions   []string
	instances map[string][]fakeInstance
	databases map[string][]fakeDatabase
	balancers map[string][]string
	queues    map[string][]string
	buckets   map[string]string
	zones     []fakeZone

	// refuse answers AccessDenied for an action, in a Region where the key
	// names one ("ec2:DescribeInstances@eu-west-1"), and fail answers a
	// fault that is no refusal.
	refuse map[string]bool
	fail   map[string]bool
	// canary is written into every field the reader does not map.
	canary string
	// page cuts the instances and the record sets into pages of this many.
	page int
}

// newFakeCloud is acme's account: a web server and a database in
// eu-west-1 behind a load balancer its zone aliases, a worker and a queue in
// us-east-1, two buckets and a private zone.
func newFakeCloud() *fakeCloud {
	return &fakeCloud{
		alias:   "acme-production",
		regions: []string{"us-east-1", "eu-west-1", "ap-south-1"},
		instances: map[string][]fakeInstance{
			"eu-west-1": {{ID: "i-0aaa1111bbbb2222c", State: "running", Name: "web-1", Private: "10.0.1.5", Public: "203.0.113.10"},
				{ID: "i-0ddd3333eeee4444f", State: "terminated", Name: "gone"}},
			"us-east-1": {{ID: "i-0123456789abcdef0", State: "stopped", Private: "10.1.0.9"}},
		},
		databases: map[string][]fakeDatabase{
			"eu-west-1": {{ID: "orders", Engine: "aurora-postgresql", Version: "16.4", Status: "available", Port: 5432},
				{ID: "legacy", Engine: "aurora-mysql", Version: "8.0.mysql_aurora.3.05.2", Status: "available", Port: 3306}},
		},
		balancers: map[string][]string{"eu-west-1": {"web-1234567890.eu-west-1.elb.amazonaws.com"}},
		queues:    map[string][]string{"us-east-1": {"orders-events", "orders-events.fifo"}},
		buckets:   map[string]string{"acme-assets": "eu-west-1", "acme-logs": "us-east-1"},
		zones: []fakeZone{
			{ID: "Z0PUBLIC", Name: "acme.example.", Records: []fakeRecord{
				{Name: "acme.example.", Type: "A", Alias: "dualstack.web-1234567890.eu-west-1.elb.amazonaws.com."},
				{Name: "api.acme.example.", Type: "CNAME", Values: []string{"acme.example"}},
				{Name: "vpn.acme.example.", Type: "A", Values: []string{"198.51.100.7"}},
				{Name: `\052.apps.acme.example.`, Type: "AAAA", Values: []string{"2001:db8::7"}},
				{Name: "acme.example.", Type: "TXT", Values: []string{`"v=spf1 -all"`}},
				{Name: "acme.example.", Type: "MX", Values: []string{"10 mail.acme.example"}},
			}},
			{ID: "Z0PRIVATE", Name: "corp.internal.", Private: true, Records: []fakeRecord{
				{Name: "db.corp.internal.", Type: "A", Values: []string{"10.0.9.9"}}}},
		},
		refuse: map[string]bool{}, fail: map[string]bool{},
		canary: "atlas-canary-unmapped",
	}
}

// input is a read's input over the fake, the atlas's role asked for and in
// the stack output unless edit changes it.
func (f *fakeCloud) input(t *testing.T, edit ...func(*config.AWSProvisioning)) collect.Input {
	t.Helper()
	prov := &config.AWSProvisioning{Slug: "acme", HomeRegion: "us-east-2", CertificateAuthority: "${AWS_CA}",
		Connection: atlasConnection(), AtlasRead: true}
	for _, e := range edit {
		e(prov)
	}
	c := &config.Company{Name: "Acme"}
	c.Integrations.AWS = &config.AWS{Enabled: true, Provisioning: prov}
	return collect.Input{Config: c, Now: time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC),
		Env: config.NewResolver(config.MapSource{"AWS_CA": mustBundle(t)}), HTTP: f.http()}
}

// http is a client every request of which the fake answers, whatever host
// it names: the host says which service was asked.
func (f *fakeCloud) http() *http.Client {
	return &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		f.serve(rec, req)
		return rec.Result(), nil
	})}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (rt roundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return rt(req) }

// scopeRE reads the Region and the service a request was signed for.
var scopeRE = regexp.MustCompile(`^AWS4-HMAC-SHA256 Credential=ASIAATLASEXAMPLE0001/\d{8}/([a-z0-9-]+)/([a-z0-9]+)/aws4_request, `)

func (f *fakeCloud) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.actions...)
}

// admit records the action and answers whether the fake refuses or fails it.
func (f *fakeCloud) admit(w http.ResponseWriter, action, region string, jsonShape bool) bool {
	f.mu.Lock()
	f.actions = append(f.actions, action)
	refuse := f.refuse[action] || f.refuse[action+"@"+region]
	fail := f.fail[action] || f.fail[action+"@"+region]
	f.mu.Unlock()
	status, code := 0, ""
	switch {
	case refuse:
		status, code = http.StatusForbidden, "AccessDenied"
		if strings.HasPrefix(action, "ec2:") {
			code = "UnauthorizedOperation"
		}
	case fail:
		status, code = http.StatusBadRequest, "ValidationError"
	default:
		return true
	}
	w.WriteHeader(status)
	if jsonShape {
		_, _ = io.WriteString(w, `{"__type":"com.amazonaws.sqs#`+code+`Exception","message":"refused"}`)
		return false
	}
	_, _ = io.WriteString(w, "<ErrorResponse><Error><Code>"+code+"</Code><Message>refused</Message></Error></ErrorResponse>")
	return false
}

func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func (f *fakeCloud) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.URL.Path == "/sessions" {
		der, _ := base64.StdEncoding.DecodeString(r.Header.Get(sigv4.HeaderX509))
		leaf, _ := x509.ParseCertificate(der)
		var in struct {
			Profile string `json:"profileArn"`
			Role    string `json:"roleArn"`
		}
		_ = json.Unmarshal(body, &in)
		f.mu.Lock()
		f.sessions = append(f.sessions, [2]string{leaf.Subject.CommonName, in.Profile})
		f.mu.Unlock()
		if leaf.Subject.CommonName != "crewlet-acme-atlas" || in.Profile != testAtlasProfile || in.Role != testAtlasRole ||
			f.refuse["session"] {
			w.Header().Set("X-Amzn-Errortype", "AccessDeniedException")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"refused"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"credentialSet": []any{map[string]any{
			"credentials": map[string]any{"accessKeyId": "ASIAATLASEXAMPLE0001", "secretAccessKey": "s",
				"sessionToken": "atlas-token", "expiration": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
		}}})
		return
	}
	scope := scopeRE.FindStringSubmatch(r.Header.Get("Authorization"))
	if scope == nil || r.Header.Get("X-Amz-Security-Token") != "atlas-token" {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<ErrorResponse><Error><Code>InvalidClientTokenId</Code></Error></ErrorResponse>")
		return
	}
	service, region, _ := strings.Cut(strings.TrimSuffix(r.Host, ".amazonaws.com"), ".")
	if region == "" {
		region = "us-east-1"
	}
	// EVERY REQUEST IS SIGNED FOR THE SERVICE AND THE REGION IT IS SENT TO.
	if scope[1] != region || scope[2] != service {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<ErrorResponse><Error><Code>SignatureDoesNotMatch</Code></Error></ErrorResponse>")
		return
	}
	form, _ := url.ParseQuery(string(body))
	c := f.canary
	switch service {
	case "iam":
		if f.admit(w, "iam:ListAccountAliases", "", false) {
			_, _ = fmt.Fprintf(w, "<ListAccountAliasesResponse><ListAccountAliasesResult><AccountAliases><member>%s</member>"+
				"</AccountAliases></ListAccountAliasesResult></ListAccountAliasesResponse>", esc(f.alias))
		}
	case "ec2":
		f.ec2(w, form, region)
	case "rds":
		if !f.admit(w, "rds:DescribeDBInstances", region, false) {
			return
		}
		var items strings.Builder
		for _, db := range f.databases[region] {
			fmt.Fprintf(&items, "<DBInstance><DBInstanceIdentifier>%s</DBInstanceIdentifier><Engine>%s</Engine>"+
				"<EngineVersion>%s</EngineVersion><DBInstanceStatus>%s</DBInstanceStatus><MasterUsername>%s</MasterUsername>"+
				"<DBName>%s</DBName><Endpoint><Address>%s.db.example</Address><Port>%d</Port></Endpoint>"+
				"<MasterUserSecret><SecretArn>%s</SecretArn></MasterUserSecret><TagList><Tag><Key>%s</Key><Value>%s</Value>"+
				"</Tag></TagList></DBInstance>", esc(db.ID), esc(db.Engine), esc(db.Version), esc(db.Status), esc(c), esc(c),
				esc(c), db.Port, esc(c), esc(c), esc(c))
		}
		_, _ = fmt.Fprintf(w, "<DescribeDBInstancesResponse><DescribeDBInstancesResult><DBInstances>%s</DBInstances>"+
			"</DescribeDBInstancesResult></DescribeDBInstancesResponse>", items.String())
	case "elasticloadbalancing":
		if !f.admit(w, "elasticloadbalancing:DescribeLoadBalancers", region, false) {
			return
		}
		var items strings.Builder
		for _, name := range f.balancers[region] {
			fmt.Fprintf(&items, "<member><LoadBalancerName>%s</LoadBalancerName><DNSName>%s</DNSName><Scheme>internet-facing"+
				"</Scheme><State><Code>active</Code></State><VpcId>%s</VpcId></member>", esc(c), esc(name), esc(c))
		}
		_, _ = fmt.Fprintf(w, "<DescribeLoadBalancersResponse><DescribeLoadBalancersResult><LoadBalancers>%s</LoadBalancers>"+
			"</DescribeLoadBalancersResult></DescribeLoadBalancersResponse>", items.String())
	case "sqs":
		if r.Header.Get("X-Amz-Target") != "AmazonSQS.ListQueues" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !f.admit(w, "sqs:ListQueues", region, true) {
			return
		}
		urls := []string{}
		for _, name := range f.queues[region] {
			urls = append(urls, "https://sqs."+region+".amazonaws.com/"+testAccount+"/"+name)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"QueueUrls": urls})
	case "s3":
		if !f.admit(w, "s3:ListAllMyBuckets", "", false) {
			return
		}
		var items strings.Builder
		for name, where := range f.buckets {
			fmt.Fprintf(&items, "<Bucket><Name>%s</Name><BucketRegion>%s</BucketRegion><CreationDate>%s</CreationDate></Bucket>",
				esc(name), esc(where), esc(c))
		}
		_, _ = fmt.Fprintf(w, "<ListAllMyBucketsResult><Owner><DisplayName>%s</DisplayName></Owner><Buckets>%s</Buckets>"+
			"</ListAllMyBucketsResult>", esc(c), items.String())
	case "route53":
		f.route53(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeCloud) ec2(w http.ResponseWriter, form url.Values, region string) {
	c := f.canary
	switch form.Get("Action") {
	case "DescribeRegions":
		if !f.admit(w, "ec2:DescribeRegions", region, false) {
			return
		}
		var items strings.Builder
		for _, name := range f.regions {
			fmt.Fprintf(&items, "<item><regionName>%s</regionName><regionEndpoint>%s</regionEndpoint></item>", esc(name), esc(c))
		}
		_, _ = fmt.Fprintf(w, "<DescribeRegionsResponse><regionInfo>%s</regionInfo></DescribeRegionsResponse>", items.String())
	case "DescribeInstances":
		if !f.admit(w, "ec2:DescribeInstances", region, false) {
			return
		}
		all := f.instances[region]
		start := 0
		if tok := form.Get("NextToken"); tok != "" {
			_, _ = fmt.Sscanf(tok, "t%d", &start)
		}
		end, next := len(all), ""
		if f.page > 0 && start+f.page < len(all) {
			end, next = start+f.page, fmt.Sprintf("<nextToken>t%d</nextToken>", start+f.page)
		}
		var items strings.Builder
		for _, it := range all[start:end] {
			fmt.Fprintf(&items, "<item><instancesSet><item><instanceId>%s</instanceId><instanceState><name>%s</name>"+
				"</instanceState><privateIpAddress>%s</privateIpAddress><ipAddress>%s</ipAddress><keyName>%s</keyName>"+
				"<userData>%s</userData><iamInstanceProfile><arn>%s</arn></iamInstanceProfile><tagSet>", esc(it.ID),
				esc(it.State), esc(it.Private), esc(it.Public), esc(c), esc(c), esc(c))
			if it.Name != "" {
				fmt.Fprintf(&items, "<item><key>Name</key><value>%s</value></item>", esc(it.Name))
			}
			fmt.Fprintf(&items, "<item><key>%s</key><value>%s</value></item><item><key>token</key><value>%s</value></item>"+
				"</tagSet></item></instancesSet></item>", esc(c), esc(c), esc(c))
		}
		_, _ = fmt.Fprintf(w, "<DescribeInstancesResponse><reservationSet>%s</reservationSet>%s</DescribeInstancesResponse>",
			items.String(), next)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (f *fakeCloud) route53(w http.ResponseWriter, r *http.Request) {
	c := f.canary
	if r.URL.Path == "/2013-04-01/hostedzone" {
		if !f.admit(w, "route53:ListHostedZones", "", false) {
			return
		}
		var items strings.Builder
		for _, z := range f.zones {
			fmt.Fprintf(&items, "<HostedZone><Id>/hostedzone/%s</Id><Name>%s</Name><CallerReference>%s</CallerReference>"+
				"<Config><Comment>%s</Comment><PrivateZone>%t</PrivateZone></Config></HostedZone>", esc(z.ID), esc(z.Name),
				esc(c), esc(c), z.Private)
		}
		_, _ = fmt.Fprintf(w, "<ListHostedZonesResponse><HostedZones>%s</HostedZones><IsTruncated>false</IsTruncated>"+
			"</ListHostedZonesResponse>", items.String())
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/2013-04-01/hostedzone/"), "/rrset")
	if !f.admit(w, "route53:ListResourceRecordSets", "", false) {
		return
	}
	for _, z := range f.zones {
		if z.ID != id {
			continue
		}
		start := 0
		if name := r.URL.Query().Get("name"); name != "" {
			_, _ = fmt.Sscanf(name, "next-%d", &start)
			if r.URL.Query().Get("type") != "A" || r.URL.Query().Get("identifier") != "blue" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		end, next := len(z.Records), "<IsTruncated>false</IsTruncated>"
		if f.page > 0 && start+f.page < len(z.Records) {
			end = start + f.page
			next = fmt.Sprintf("<IsTruncated>true</IsTruncated><NextRecordName>next-%d</NextRecordName>"+
				"<NextRecordType>A</NextRecordType><NextRecordIdentifier>blue</NextRecordIdentifier>", end)
		}
		var items strings.Builder
		for _, rec := range z.Records[start:end] {
			fmt.Fprintf(&items, "<ResourceRecordSet><Name>%s</Name><Type>%s</Type><SetIdentifier>%s</SetIdentifier>",
				esc(rec.Name), esc(rec.Type), esc(c))
			if rec.Alias != "" {
				fmt.Fprintf(&items, "<AliasTarget><HostedZoneId>%s</HostedZoneId><DNSName>%s</DNSName></AliasTarget>",
					esc(c), esc(rec.Alias))
			}
			items.WriteString("<ResourceRecords>")
			for _, v := range rec.Values {
				fmt.Fprintf(&items, "<ResourceRecord><Value>%s</Value></ResourceRecord>", esc(v))
			}
			items.WriteString("</ResourceRecords></ResourceRecordSet>")
		}
		_, _ = fmt.Fprintf(w, "<ListResourceRecordSetsResponse><ResourceRecordSets>%s</ResourceRecordSets>%s"+
			"</ListResourceRecordSetsResponse>", items.String(), next)
		return
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, "<ErrorResponse><Error><Code>NoSuchHostedZone</Code></Error></ErrorResponse>")
}
