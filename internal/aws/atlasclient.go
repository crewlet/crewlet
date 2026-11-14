package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/sigv4"
)

// The atlas's reach into the account: nine listings, signed as the atlas's
// own role, each decoded into the few fields the atlas keeps.
//
// A THIN CLIENT, NOT THE SDK'S: the SDK decodes a whole answer, and what the
// atlas must never hold (a tag's value, a master user's name, a database's
// address) would be parsed into memory to be thrown away. A decoder here maps
// the fields it names and reads no other, which is the rule the read is held
// to (collect's package doc), and five service modules stay out of the binary.

// atlasAnswerLimit bounds one page of one listing. A page of 1,000 instances
// is about 4 MiB of XML; past this a page is not one this client asked for.
const atlasAnswerLimit = 32 << 20

// The page sizes asked for, each the listing's own maximum.
const (
	instancesPage     = 1000
	databasesPage     = 100
	loadBalancersPage = 400
	queuesPage        = 1000
	bucketsPage       = 1000
)

// atlasPageCeiling is the most pages one listing is walked. A listing past it
// fails the read rather than being cut short, since half a listing would
// retire the other half: at the page sizes above it is 200,000 instances or
// 60,000 DNS records in one zone, and a cursor that never ends stops here.
const atlasPageCeiling = 200

// partitionEndpoints is where a partition answers its global services, and
// the Region those requests are signed for.
type partitionEndpoints struct {
	suffix, global, iam, route53 string
}

func endpointsOf(partition string) partitionEndpoints {
	switch partition {
	case "aws-us-gov":
		return partitionEndpoints{suffix: "amazonaws.com", global: "us-gov-west-1",
			iam: "iam.us-gov.amazonaws.com", route53: "route53.us-gov.amazonaws.com"}
	case "aws-cn":
		return partitionEndpoints{suffix: "amazonaws.com.cn", global: "cn-north-1",
			iam: "iam.cn-north-1.amazonaws.com.cn", route53: "route53.amazonaws.com.cn"}
	}
	return partitionEndpoints{suffix: "amazonaws.com", global: "us-east-1",
		iam: "iam.amazonaws.com", route53: "route53.amazonaws.com"}
}

// ErrNoAtlasReader is a stack output that carries no atlas role: the company
// asked for the reader and has not run the CloudShell block since.
var ErrNoAtlasReader = errors.New("aws: the stack output carries no atlas role")

// AtlasClient lists what the account runs as the atlas's role.
type AtlasClient struct {
	conn  *Connection
	ra    rolesAnywhere
	http  *http.Client
	ends  partitionEndpoints
	creds sigv4.Credentials
	// expires is when the session ends; a read is far shorter than its hour.
	expires time.Time
}

// NewAtlasClient is the atlas's client over the provisioner client's
// transport and connection, or [ErrNoAtlasReader].
func (c *Client) NewAtlasClient() (*AtlasClient, error) {
	if !c.conn.HasAtlas() {
		return nil, ErrNoAtlasReader
	}
	return &AtlasClient{conn: c.conn, ra: c.ra, http: c.http, ends: endpointsOf(c.conn.Partition)}, nil
}

// Account is the account the client reads.
func (a *AtlasClient) Account() string { return a.conn.Account }

// signIn opens the atlas's role with a leaf named for it, once per client.
func (a *AtlasClient) signIn(ctx context.Context) error {
	if a.creds.AccessKeyID != "" && time.Until(a.expires) > renewBefore {
		return nil
	}
	s, err := a.ra.createSession(ctx, a.conn.CA, named(atlasCN, a.conn.Company),
		a.conn.AtlasProfileARN, a.conn.AtlasRoleARN, a.conn.TrustAnchorARN)
	if err != nil {
		return fmt.Errorf("aws: sign in as %s: %w", a.conn.resolve(atlasName), err)
	}
	a.creds = sigv4.Credentials{AccessKeyID: s.creds.AccessKeyID, SecretAccessKey: s.creds.SecretAccessKey,
		SessionToken: s.creds.SessionToken}
	a.expires = s.creds.Expires
	return nil
}

// call is one signed request: where it goes and what it carries.
type call struct {
	method, host, path string
	query              url.Values
	body               []byte
	contentType        string
	target             string
	region, service    string
	s3                 bool
}

// do sends c, retrying what AWS says to retry as the sign-in does, and
// answers the body of a success. A refusal is an [*APIError].
func (a *AtlasClient) do(ctx context.Context, c call) ([]byte, error) {
	if err := a.signIn(ctx); err != nil {
		return nil, err
	}
	for attempt := 1; ; attempt++ {
		data, err := a.once(ctx, c)
		var api *APIError
		if err == nil || !errors.As(err, &api) || !api.transient() || attempt == maxAttempts {
			return data, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(a.ra.wait(attempt)):
		}
	}
}

func (a *AtlasClient) once(ctx context.Context, c call) ([]byte, error) {
	now := a.ra.now().UTC()
	hash := sigv4.PayloadHash(c.body)
	hdr := http.Header{"X-Amz-Date": {now.Format(sigv4.DateFormat)}}
	if a.creds.SessionToken != "" {
		hdr.Set("X-Amz-Security-Token", a.creds.SessionToken)
	}
	if c.contentType != "" {
		hdr.Set("Content-Type", c.contentType)
	}
	if c.target != "" {
		hdr.Set("X-Amz-Target", c.target)
	}
	if c.s3 {
		hdr.Set("X-Amz-Content-Sha256", hash)
	}
	auth := sigv4.Sign(sigv4.Request{Method: c.method, Host: c.host, Path: c.path, Query: c.query, Header: hdr,
		PayloadHash: hash, S3: c.s3}, a.creds, c.region, c.service, now)
	target := a.ra.endpoint(c.host) + sigv4.URIPath(c.path)
	if len(c.query) > 0 {
		target += "?" + sigv4.CanonicalQuery(c.query)
	}
	req, err := http.NewRequestWithContext(ctx, c.method, target, bytes.NewReader(c.body))
	if err != nil {
		return nil, err
	}
	req.Header = hdr
	req.Host = c.host
	req.Header.Set("Authorization", auth)
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("aws: %s: %w", c.service, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, atlasAnswerLimit+1))
	if err != nil {
		return nil, fmt.Errorf("aws: %s: %w", c.service, err)
	}
	if resp.StatusCode >= 300 {
		return nil, refusalOf(resp, data)
	}
	if len(data) > atlasAnswerLimit {
		return nil, fmt.Errorf("aws: %s answered more than %d bytes in one page", c.service, atlasAnswerLimit)
	}
	return data, nil
}

// refusalOf reads a refusal's code and message, whichever of AWS's three
// shapes it came in: JSON, or XML with the error at the root or under it.
func refusalOf(resp *http.Response, data []byte) *APIError {
	if e := apiError(resp, data); e.Message != "" || resp.Header.Get("X-Amzn-Errortype") != "" {
		return e
	}
	e := &APIError{Status: resp.StatusCode}
	dec := xml.NewDecoder(bytes.NewReader(data))
	for e.Code == "" || e.Message == "" {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		var text string
		switch start.Name.Local {
		case "Code":
			if dec.DecodeElement(&text, &start) == nil && e.Code == "" {
				e.Code = text
			}
		case "Message":
			if dec.DecodeElement(&text, &start) == nil && e.Message == "" {
				e.Message = text
			}
		}
	}
	if e.Code == "" {
		e.Code = strings.ReplaceAll(http.StatusText(resp.StatusCode), " ", "")
	}
	return e
}

// query sends one action of a service that takes a form, and decodes its XML.
func (a *AtlasClient) query(ctx context.Context, service, host, region, version, action string,
	params url.Values, out any) error {

	form := url.Values{"Action": {action}, "Version": {version}}
	for k, v := range params {
		form[k] = v
	}
	data, err := a.do(ctx, call{method: http.MethodPost, host: host, path: "/", body: []byte(form.Encode()),
		contentType: "application/x-www-form-urlencoded; charset=utf-8", region: region, service: service})
	if err != nil {
		return fmt.Errorf("aws: %s: %w", action, err)
	}
	if err = xml.Unmarshal(data, out); err != nil {
		return fmt.Errorf("aws: %s answered what is not its XML: %w", action, err)
	}
	return nil
}

func (a *AtlasClient) regional(service, region string) string {
	return service + "." + region + "." + a.ends.suffix
}

// pages walks a listing page by page until next answers no cursor.
func pages(what string, next func(cursor string) (string, error)) error {
	cursor := ""
	for range atlasPageCeiling {
		following, err := next(cursor)
		if err != nil {
			return err
		}
		if following == "" || following == cursor {
			return nil
		}
		cursor = following
	}
	return fmt.Errorf("aws: %s run past %d pages, more than one read walks", what, atlasPageCeiling)
}

// AccountAlias is the account's alias, "" where it has none.
func (a *AtlasClient) AccountAlias(ctx context.Context) (string, error) {
	var out struct {
		Aliases []string `xml:"ListAccountAliasesResult>AccountAliases>member"`
	}
	if err := a.query(ctx, "iam", a.ends.iam, a.ends.global, "2010-05-08", "ListAccountAliases", nil, &out); err != nil {
		return "", err
	}
	if len(out.Aliases) == 0 {
		return "", nil
	}
	return out.Aliases[0], nil
}

// Regions is every Region enabled for the account, asked of the home Region.
func (a *AtlasClient) Regions(ctx context.Context) ([]string, error) {
	var out struct {
		Regions []string `xml:"regionInfo>item>regionName"`
	}
	err := a.query(ctx, "ec2", a.regional("ec2", a.conn.Region), a.conn.Region, "2016-11-15", "DescribeRegions", nil, &out)
	return out.Regions, err
}

// Instance is one EC2 instance as the atlas keeps it. Its user data is not
// in this listing, and its tags are read for the one named Name alone.
type Instance struct {
	ID        string
	State     string
	Name      string
	PrivateIP string
	PublicIP  string
}

// Instances is every instance of region.
func (a *AtlasClient) Instances(ctx context.Context, region string) ([]Instance, error) {
	var all []Instance
	err := pages("the instances of "+region, func(cursor string) (string, error) {
		var out struct {
			Items []struct {
				ID        string `xml:"instanceId"`
				State     string `xml:"instanceState>name"`
				PrivateIP string `xml:"privateIpAddress"`
				PublicIP  string `xml:"ipAddress"`
				Tags      []struct {
					Key   string `xml:"key"`
					Value string `xml:"value"`
				} `xml:"tagSet>item"`
			} `xml:"reservationSet>item>instancesSet>item"`
			Next string `xml:"nextToken"`
		}
		params := url.Values{"MaxResults": {strconv.Itoa(instancesPage)}}
		if cursor != "" {
			params.Set("NextToken", cursor)
		}
		if err := a.query(ctx, "ec2", a.regional("ec2", region), region, "2016-11-15", "DescribeInstances", params, &out); err != nil {
			return "", err
		}
		for _, it := range out.Items {
			one := Instance{ID: it.ID, State: it.State, PrivateIP: it.PrivateIP, PublicIP: it.PublicIP}
			for _, tag := range it.Tags {
				// THE NAME TAG ALONE: it is the instance's name in every
				// AWS console, and any other tag is somebody's free text.
				if tag.Key == "Name" {
					one.Name = tag.Value
				}
			}
			all = append(all, one)
		}
		return out.Next, nil
	})
	return all, err
}

// Database is one RDS instance as the atlas keeps it: what it is and where
// it listens, never its address, its master user or its secret's name.
type Database struct {
	ID            string
	Engine        string
	EngineVersion string
	Status        string
	Port          int
}

// Databases is every database instance of region.
func (a *AtlasClient) Databases(ctx context.Context, region string) ([]Database, error) {
	var all []Database
	err := pages("the databases of "+region, func(cursor string) (string, error) {
		var out struct {
			Items []struct {
				ID            string `xml:"DBInstanceIdentifier"`
				Engine        string `xml:"Engine"`
				EngineVersion string `xml:"EngineVersion"`
				Status        string `xml:"DBInstanceStatus"`
				Port          int    `xml:"Endpoint>Port"`
			} `xml:"DescribeDBInstancesResult>DBInstances>DBInstance"`
			Next string `xml:"DescribeDBInstancesResult>Marker"`
		}
		params := url.Values{"MaxRecords": {strconv.Itoa(databasesPage)}}
		if cursor != "" {
			params.Set("Marker", cursor)
		}
		if err := a.query(ctx, "rds", a.regional("rds", region), region, "2014-10-31", "DescribeDBInstances", params, &out); err != nil {
			return "", err
		}
		for _, it := range out.Items {
			all = append(all, Database(it))
		}
		return out.Next, nil
	})
	return all, err
}

// LoadBalancer is one load balancer as the atlas keeps it: the name it
// answers on, and whether it faces the internet.
type LoadBalancer struct {
	DNSName string
	Scheme  string
	State   string
}

// LoadBalancers is every application, network and gateway load balancer of
// region.
func (a *AtlasClient) LoadBalancers(ctx context.Context, region string) ([]LoadBalancer, error) {
	var all []LoadBalancer
	err := pages("the load balancers of "+region, func(cursor string) (string, error) {
		var out struct {
			Items []struct {
				DNSName string `xml:"DNSName"`
				Scheme  string `xml:"Scheme"`
				State   string `xml:"State>Code"`
			} `xml:"DescribeLoadBalancersResult>LoadBalancers>member"`
			Next string `xml:"DescribeLoadBalancersResult>NextMarker"`
		}
		params := url.Values{"PageSize": {strconv.Itoa(loadBalancersPage)}}
		if cursor != "" {
			params.Set("Marker", cursor)
		}
		host := a.regional("elasticloadbalancing", region)
		if err := a.query(ctx, "elasticloadbalancing", host, region, "2015-12-01", "DescribeLoadBalancers", params, &out); err != nil {
			return "", err
		}
		for _, it := range out.Items {
			all = append(all, LoadBalancer(it))
		}
		return out.Next, nil
	})
	return all, err
}

// Queues is the name of every SQS queue of region.
func (a *AtlasClient) Queues(ctx context.Context, region string) ([]string, error) {
	var all []string
	err := pages("the queues of "+region, func(cursor string) (string, error) {
		in := map[string]any{"MaxResults": queuesPage}
		if cursor != "" {
			in["NextToken"] = cursor
		}
		body, err := json.Marshal(in)
		if err != nil {
			return "", err
		}
		data, err := a.do(ctx, call{method: http.MethodPost, host: a.regional("sqs", region), path: "/", body: body,
			contentType: "application/x-amz-json-1.0", target: "AmazonSQS.ListQueues", region: region, service: "sqs"})
		if err != nil {
			return "", fmt.Errorf("aws: ListQueues: %w", err)
		}
		var out struct {
			URLs []string `json:"QueueUrls"`
			Next string   `json:"NextToken"`
		}
		if err = json.Unmarshal(data, &out); err != nil {
			return "", fmt.Errorf("aws: ListQueues answered what is not its JSON: %w", err)
		}
		for _, raw := range out.URLs {
			// THE LAST SEGMENT OF THE QUEUE'S ADDRESS is its name; the rest
			// is the endpoint and the account, which the read already holds.
			all = append(all, raw[strings.LastIndexByte(raw, '/')+1:])
		}
		return out.Next, nil
	})
	return all, err
}

// Bucket is one S3 bucket by its name and, where S3 says it, its Region.
type Bucket struct {
	Name   string
	Region string
}

// Buckets is every bucket the account owns.
func (a *AtlasClient) Buckets(ctx context.Context) ([]Bucket, error) {
	var all []Bucket
	err := pages("the buckets", func(cursor string) (string, error) {
		q := url.Values{"max-buckets": {strconv.Itoa(bucketsPage)}}
		if cursor != "" {
			q.Set("continuation-token", cursor)
		}
		data, err := a.do(ctx, call{method: http.MethodGet, host: a.regional("s3", a.ends.global), path: "/", query: q,
			region: a.ends.global, service: "s3", s3: true})
		if err != nil {
			return "", fmt.Errorf("aws: ListBuckets: %w", err)
		}
		var out struct {
			Items []struct {
				Name   string `xml:"Name"`
				Region string `xml:"BucketRegion"`
			} `xml:"Buckets>Bucket"`
			Next string `xml:"ContinuationToken"`
		}
		if err = xml.Unmarshal(data, &out); err != nil {
			return "", fmt.Errorf("aws: ListBuckets answered what is not its XML: %w", err)
		}
		for _, it := range out.Items {
			all = append(all, Bucket(it))
		}
		return out.Next, nil
	})
	return all, err
}

// HostedZone is one Route 53 zone.
type HostedZone struct {
	ID      string
	Name    string
	Private bool
}

// HostedZones is every hosted zone of the account.
func (a *AtlasClient) HostedZones(ctx context.Context) ([]HostedZone, error) {
	var all []HostedZone
	err := pages("the hosted zones", func(cursor string) (string, error) {
		q := url.Values{}
		if cursor != "" {
			q.Set("marker", cursor)
		}
		data, err := a.do(ctx, call{method: http.MethodGet, host: a.ends.route53, path: "/2013-04-01/hostedzone",
			query: q, region: a.ends.global, service: "route53"})
		if err != nil {
			return "", fmt.Errorf("aws: ListHostedZones: %w", err)
		}
		var out struct {
			Items []struct {
				ID      string `xml:"Id"`
				Name    string `xml:"Name"`
				Private bool   `xml:"Config>PrivateZone"`
			} `xml:"HostedZones>HostedZone"`
			Truncated bool   `xml:"IsTruncated"`
			Next      string `xml:"NextMarker"`
		}
		if err = xml.Unmarshal(data, &out); err != nil {
			return "", fmt.Errorf("aws: ListHostedZones answered what is not its XML: %w", err)
		}
		for _, it := range out.Items {
			all = append(all, HostedZone{ID: strings.TrimPrefix(it.ID, "/hostedzone/"), Name: it.Name, Private: it.Private})
		}
		if !out.Truncated {
			return "", nil
		}
		return out.Next, nil
	})
	return all, err
}

// RecordSet is one Route 53 record set: its name, its type, and what it
// points at, an alias's target or its records' values.
type RecordSet struct {
	Name   string
	Type   string
	Alias  string
	Values []string
}

// RecordSets is every A, AAAA and CNAME record set of the zone id, with
// every other type left undecoded past its name and type.
func (a *AtlasClient) RecordSets(ctx context.Context, id string) ([]RecordSet, error) {
	var all []RecordSet
	err := pages("the records of a zone", func(cursor string) (string, error) {
		q := url.Values{}
		if cursor != "" {
			// THE CURSOR IS THE THREE VALUES ROUTE 53 ANSWERED, joined by a
			// byte no record name, type or identifier holds.
			name, rest, _ := strings.Cut(cursor, "\x00")
			kind, ident, _ := strings.Cut(rest, "\x00")
			q.Set("name", name)
			q.Set("type", kind)
			if ident != "" {
				q.Set("identifier", ident)
			}
		}
		data, err := a.do(ctx, call{method: http.MethodGet, host: a.ends.route53,
			path: "/2013-04-01/hostedzone/" + id + "/rrset", query: q, region: a.ends.global, service: "route53"})
		if err != nil {
			return "", fmt.Errorf("aws: ListResourceRecordSets: %w", err)
		}
		var out struct {
			Items     []recordSetXML `xml:"ResourceRecordSets>ResourceRecordSet"`
			Truncated bool           `xml:"IsTruncated"`
			NextName  string         `xml:"NextRecordName"`
			NextType  string         `xml:"NextRecordType"`
			NextIdent string         `xml:"NextRecordIdentifier"`
		}
		if err = xml.Unmarshal(data, &out); err != nil {
			return "", fmt.Errorf("aws: ListResourceRecordSets answered what is not its XML: %w", err)
		}
		for _, it := range out.Items {
			all = append(all, it.recordSet())
		}
		if !out.Truncated {
			return "", nil
		}
		return out.NextName + "\x00" + out.NextType + "\x00" + out.NextIdent, nil
	})
	return all, err
}

// recordSetXML is a record set as Route 53 answers it. Its values are read
// as raw text, so a TXT record's is dropped before it is ever a string.
type recordSetXML struct {
	Name   string `xml:"Name"`
	Type   string `xml:"Type"`
	Alias  string `xml:"AliasTarget>DNSName"`
	Values []struct {
		Raw []byte `xml:",innerxml"`
	} `xml:"ResourceRecords>ResourceRecord"`
}

func (x recordSetXML) recordSet() RecordSet {
	out := RecordSet{Name: x.Name, Type: x.Type}
	// ONLY AN ADDRESS OR A NAME IS KEPT: a TXT, an MX or any other record's
	// value is where a zone carries tokens, and none of it is decoded.
	if x.Type != recordA && x.Type != recordAAAA && x.Type != recordCNAME {
		return out
	}
	out.Alias = x.Alias
	for _, v := range x.Values {
		var value struct {
			Text string `xml:"Value"`
		}
		if xml.Unmarshal(append(append([]byte("<r>"), v.Raw...), "</r>"...), &value) == nil {
			out.Values = append(out.Values, value.Text)
		}
	}
	return out
}

// The record types the atlas reads.
const (
	recordA     = "A"
	recordAAAA  = "AAAA"
	recordCNAME = "CNAME"
)
