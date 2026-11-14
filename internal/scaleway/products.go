package scaleway

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/sigv4"
)

// The product listings the atlas reads under its own application's key
// (ADR-0144). Each decodes the fields the atlas keeps and no other.
//
// WHAT IS NEVER DECODED, though Scaleway sends it in the same answer: a
// server's `admin_password_encrypted_value`, a container's, a function's and
// a namespace's `environment_variables` and `secret_environment_variables`, a
// database's and a cache's endpoints, users and settings, and every tag and
// description. What is never asked for at all: a server's user data, a
// cluster's kubeconfig, a database's users, a bucket's objects, and a record
// that is not A, AAAA or CNAME.

// Where each product is served, in Scaleway's own codes (its SDK's lists on
// 2026-11-13). A listing in a locality the Organization cannot use answers
// 404, which is read as nothing there ([served]).
//
//nolint:gochecknoglobals // immutable tables, not state
var (
	productRegions = []string{"fr-par", "nl-ams", "pl-waw"}
	instanceZones  = []string{
		"fr-par-1", "fr-par-2", "fr-par-3", "nl-ams-1", "nl-ams-2", "nl-ams-3",
		"pl-waw-1", "pl-waw-2", "pl-waw-3", "it-mil-1",
	}
	loadBalancerZones = []string{
		"fr-par-1", "fr-par-2", "nl-ams-1", "nl-ams-2", "nl-ams-3", "pl-waw-1", "pl-waw-2", "pl-waw-3",
	}
	redisZones = []string{"fr-par-1", "fr-par-2", "nl-ams-1", "nl-ams-2", "pl-waw-1", "pl-waw-2"}
)

// Server is one Instance, as the atlas keeps it.
type Server struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Project   string  `json:"project"`
	Zone      string  `json:"zone"`
	PublicIPs []Addr  `json:"public_ips"`
	PublicIP  *Addr   `json:"public_ip"`
	PrivateIP *string `json:"private_ip"`
}

// Addr is one address of a server.
type Addr struct {
	Address string `json:"address"`
}

// Cluster is one Kubernetes cluster, as the atlas keeps it.
type Cluster struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ProjectID string `json:"project_id"`
	Region    string `json:"region"`
	Version   string `json:"version"`
}

// Database is one Managed Database instance, as the atlas keeps it. Engine is
// Scaleway's own word and version ("PostgreSQL-15").
type Database struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ProjectID string `json:"project_id"`
	Region    string `json:"region"`
	Engine    string `json:"engine"`
}

// Cache is one Managed Database for Redis cluster, as the atlas keeps it.
type Cache struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ProjectID string `json:"project_id"`
	Zone      string `json:"zone"`
	Version   string `json:"version"`
}

// LoadBalancer is one Load Balancer, as the atlas keeps it.
type LoadBalancer struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	ProjectID string   `json:"project_id"`
	Zone      string   `json:"zone"`
	IPs       []LBAddr `json:"ip"`
}

// LBAddr is one address of a load balancer.
type LBAddr struct {
	Address string `json:"ip_address"`
}

// DNSZone is one DNS zone: Domain, or Subdomain under it.
type DNSZone struct {
	Domain    string `json:"domain"`
	Subdomain string `json:"subdomain"`
	ProjectID string `json:"project_id"`
}

// Name is the zone's own name.
func (z DNSZone) Name() string {
	if z.Subdomain == "" {
		return z.Domain
	}
	return z.Subdomain + "." + z.Domain
}

// DNSRecord is one record of a zone. Name is relative to the zone, and empty
// at its apex.
type DNSRecord struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Name string `json:"name"`
	Data string `json:"data"`
}

// Namespace is one namespace of Serverless Containers or Functions: what
// places a container or a function in a project.
type Namespace struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
}

// Serverless is one Serverless Container or Function, by name.
type Serverless struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	NamespaceID string `json:"namespace_id"`
	Region      string `json:"region"`
}

// Bucket is one Object Storage bucket, by name.
type Bucket struct {
	Name      string
	Region    string
	ProjectID string
}

// served turns a listing's 404 into nothing listed: Scaleway answers one for
// a locality it does not serve the product in, or not to this Organization.
func served[T any](rows []T, err error) ([]T, error) {
	if gone(err) {
		return nil, nil
	}
	return rows, err
}

// across lists a product in each locality in turn and joins the answers.
func across[T any](localities []string, listing func(locality string) ([]T, error)) ([]T, error) {
	var out []T
	for _, locality := range localities {
		rows, err := served(listing(locality))
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// byOrg is the filter every product listing takes.
func (c *Client) byOrg() url.Values { return url.Values{"organization_id": {c.org}} }

// Servers is every Instance of the organization, in every zone.
//
// ITS OWN PAGING: the Instance API counts in a header and sizes a page with
// `per_page`, so a listing ends at the first page that is not full.
func (c *Client) Servers(ctx context.Context) ([]Server, error) {
	return across(instanceZones, func(zone string) ([]Server, error) {
		var out []Server
		for page := 1; ; page++ {
			if page > pageCeiling {
				return nil, fmt.Errorf("scaleway: the servers of %s did not finish after %d pages of %d",
					zone, pageCeiling, pageSize)
			}
			var answer struct {
				Servers []Server `json:"servers"`
			}
			q := url.Values{"organization": {c.org}, "page": {strconv.Itoa(page)}, "per_page": {strconv.Itoa(pageSize)}}
			if err := c.do(ctx, http.MethodGet, "/instance/v1/zones/"+zone+"/servers", q, nil, &answer); err != nil {
				return nil, err
			}
			out = append(out, answer.Servers...)
			if len(answer.Servers) < pageSize {
				return out, nil
			}
		}
	})
}

// Clusters is every Kubernetes cluster of the organization, in every region.
func (c *Client) Clusters(ctx context.Context) ([]Cluster, error) {
	return across(productRegions, func(region string) ([]Cluster, error) {
		return list[Cluster](ctx, c, "/k8s/v1/regions/"+region+"/clusters", c.byOrg(), "clusters")
	})
}

// Databases is every Managed Database instance of the organization.
func (c *Client) Databases(ctx context.Context) ([]Database, error) {
	return across(productRegions, func(region string) ([]Database, error) {
		return list[Database](ctx, c, "/rdb/v1/regions/"+region+"/instances", c.byOrg(), "instances")
	})
}

// Caches is every Managed Database for Redis cluster of the organization.
func (c *Client) Caches(ctx context.Context) ([]Cache, error) {
	return across(redisZones, func(zone string) ([]Cache, error) {
		return list[Cache](ctx, c, "/redis/v1/zones/"+zone+"/clusters", c.byOrg(), "clusters")
	})
}

// LoadBalancers is every Load Balancer of the organization, in every zone.
func (c *Client) LoadBalancers(ctx context.Context) ([]LoadBalancer, error) {
	return across(loadBalancerZones, func(zone string) ([]LoadBalancer, error) {
		return list[LoadBalancer](ctx, c, "/lb/v1/zones/"+zone+"/lbs", c.byOrg(), "lbs")
	})
}

// DNSZones is every DNS zone of the organization.
func (c *Client) DNSZones(ctx context.Context) ([]DNSZone, error) {
	return list[DNSZone](ctx, c, "/domain/v2beta1/dns-zones", c.byOrg(), "dns_zones")
}

// DNSRecords is every record of a zone.
func (c *Client) DNSRecords(ctx context.Context, zone string) ([]DNSRecord, error) {
	return list[DNSRecord](ctx, c, "/domain/v2beta1/dns-zones/"+url.PathEscape(zone)+"/records", nil, "records")
}

// serverless lists the namespaces and the resources of one serverless
// product, in every region: resource is "containers" or "functions".
func (c *Client) serverless(ctx context.Context, product, resource string) ([]Namespace, []Serverless, error) {
	base := "/" + product + "/v1beta1/regions/"
	namespaces, err := across(productRegions, func(region string) ([]Namespace, error) {
		return list[Namespace](ctx, c, base+region+"/namespaces", c.byOrg(), "namespaces")
	})
	if err != nil {
		return nil, nil, err
	}
	rows, err := across(productRegions, func(region string) ([]Serverless, error) {
		return list[Serverless](ctx, c, base+region+"/"+resource, c.byOrg(), resource)
	})
	return namespaces, rows, err
}

// Containers is every Serverless Container of the organization, with the
// namespaces that place each in a project.
func (c *Client) Containers(ctx context.Context) ([]Namespace, []Serverless, error) {
	return c.serverless(ctx, "containers", "containers")
}

// Functions is every Serverless Function of the organization, with the
// namespaces that place each in a project.
func (c *Client) Functions(ctx context.Context) ([]Namespace, []Serverless, error) {
	return c.serverless(ctx, "functions", "functions")
}

// ObjectStorageHost is Object Storage's host in a region. Fixed, like
// [APIBase], so the atlas application's key is only ever sent to Scaleway.
func ObjectStorageHost(region string) string { return "s3." + region + ".scw.cloud" }

// Buckets is the name of every bucket of a project, in every region. Object
// Storage speaks the S3 protocol, which knows no project: the listing is the
// project's because the access key that signs it names the project after an
// `@`, as Scaleway documents. access is this client's own key's access key.
func (c *Client) Buckets(ctx context.Context, access, projectID string) ([]Bucket, error) {
	return across(productRegions, func(region string) ([]Bucket, error) {
		names, err := c.bucketNames(ctx, access+"@"+projectID, region)
		if err != nil {
			return nil, err
		}
		out := make([]Bucket, 0, len(names))
		for _, name := range names {
			out = append(out, Bucket{Name: name, Region: region, ProjectID: projectID})
		}
		return out, nil
	})
}

// bucketNames is one signed ListBuckets call. The answer's owner and each
// bucket's creation date are not decoded.
func (c *Client) bucketNames(ctx context.Context, access, region string) ([]string, error) {
	host := ObjectStorageHost(region)
	const path = "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+path, nil)
	if err != nil {
		return nil, fmt.Errorf("scaleway: build GET %s: %w", host, err)
	}
	now := time.Now().UTC()
	empty := sigv4.PayloadHash(nil)
	signed := http.Header{}
	signed.Set("X-Amz-Date", now.Format(sigv4.DateFormat))
	signed.Set("X-Amz-Content-Sha256", empty)
	for k, v := range signed {
		req.Header[k] = v
	}
	req.Header.Set("Authorization", sigv4.Sign(sigv4.Request{
		Method: http.MethodGet, Host: host, Path: path, Header: signed, PayloadHash: empty, S3: true,
	}, sigv4.Credentials{AccessKeyID: access, SecretAccessKey: c.secret}, region, "s3", now))
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scaleway: GET %s: %w", host, err)
	}
	defer func() { _ = res.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(res.Body, maxResponse))
	if err != nil {
		return nil, fmt.Errorf("scaleway: read GET %s: %w", host, err)
	}
	if res.StatusCode >= http.StatusMultipleChoices {
		api := &APIError{Method: http.MethodGet, Path: host + path, Status: res.StatusCode}
		var refusal struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		if xml.Unmarshal(payload, &refusal) == nil {
			api.Type = refusal.Code
			api.Detail = httpx.Refusal("text/plain", []byte(refusal.Message))
		}
		if api.Detail == "" {
			api.Detail = noDetail
		}
		return nil, api
	}
	var answer struct {
		Buckets []struct {
			Name string `xml:"Name"`
		} `xml:"Buckets>Bucket"`
	}
	if err := xml.Unmarshal(payload, &answer); err != nil {
		return nil, fmt.Errorf("scaleway: decode GET %s: %w", host, err)
	}
	names := make([]string, 0, len(answer.Buckets))
	for _, b := range answer.Buckets {
		names = append(names, b.Name)
	}
	return names, nil
}
