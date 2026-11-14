package digitalocean

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// What the atlas reads at DigitalOcean, each a listing decoded into closed
// fields. NOTHING ELSE OF AN ANSWER IS DECODED: a database's connection and
// users, an app's environment and a cluster's endpoint arrive beside the
// fields below and are dropped with the answer.

// The bounds of one listing. A page of a hundred Droplets is about half a
// megabyte, and an app carries its whole spec, so a page of apps is smaller.
const (
	listResponse = 8 << 20
	listPage     = 100
	appPage      = 20
	// maxListPages stops a listing whose next link never ends: ten thousand
	// of one thing is past any team's limits.
	maxListPages = 100
)

// The record types the atlas reads of a domain: the three that say where a
// name points. Asked for by type, so no other record is sent at all.
const (
	recordA     = "A"
	recordAAAA  = "AAAA"
	recordCNAME = "CNAME"
)

// Team is the team a token acts in, as GET /v2/account answers it. The
// account's user and email are not decoded.
type Team struct {
	UUID   string
	Name   string
	Locked bool
}

// Team reads the token's team, which needs account:read.
func (c *Client) Team(ctx context.Context) (Team, error) {
	var out struct {
		Account struct {
			Status string `json:"status"`
			Team   struct {
				UUID string `json:"uuid"`
				Name string `json:"name"`
			} `json:"team"`
		} `json:"account"`
	}
	if _, err := c.call(ctx, http.MethodGet, APIBase+"/v2/account", "", "", &out); err != nil {
		return Team{}, err
	}
	return Team{UUID: out.Account.Team.UUID, Name: out.Account.Team.Name,
		Locked: out.Account.Status == "locked"}, nil
}

// regionSlug is a region as DigitalOcean writes one: its slug alone on a
// database or a cluster, an object holding the slug on a Droplet.
type regionSlug string

func (r *regionSlug) UnmarshalJSON(raw []byte) error {
	var slug string
	if json.Unmarshal(raw, &slug) == nil {
		*r = regionSlug(slug)
		return nil
	}
	var object struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	*r = regionSlug(object.Slug)
	return nil
}

// network is one address of a Droplet.
type network struct {
	Address string `json:"ip_address"`
}

// Droplet is one Droplet: its id, name, region and addresses.
type Droplet struct {
	ID       int64      `json:"id"`
	Name     string     `json:"name"`
	Region   regionSlug `json:"region"`
	Networks struct {
		V4 []network `json:"v4"`
		V6 []network `json:"v6"`
	} `json:"networks"`
}

// Cluster is one Kubernetes cluster.
type Cluster struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	Region regionSlug `json:"region"`
}

// Database is one managed database cluster: what it runs and where, and
// nothing of how it is reached.
type Database struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	Engine  string     `json:"engine"`
	Version string     `json:"version"`
	Region  regionSlug `json:"region"`
}

// Balancer is one load balancer and the addresses it answers on.
type Balancer struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	IP     string     `json:"ip"`
	IPv6   string     `json:"ipv6"`
	Region regionSlug `json:"region"`
}

// Domain is one domain DigitalOcean serves DNS for.
type Domain struct {
	Name string `json:"name"`
}

// Record is one DNS record of a domain.
type Record struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
	Name string `json:"name"`
	Data string `json:"data"`
}

// component is one component of an app: the repository it is built from.
type component struct {
	GitHub *struct {
		Repo string `json:"repo"`
	} `json:"github"`
	GitLab *struct {
		Repo string `json:"repo"`
	} `json:"gitlab"`
}

// App is one App Platform app: its name, its addresses, the repositories it
// is built from and the managed databases it is attached to.
type App struct {
	ID      string `json:"id"`
	LiveURL string `json:"live_url"`
	Spec    struct {
		Name    string `json:"name"`
		Domains []struct {
			Domain string `json:"domain"`
		} `json:"domains"`
		Services    []component `json:"services"`
		StaticSites []component `json:"static_sites"`
		Workers     []component `json:"workers"`
		Jobs        []component `json:"jobs"`
		Functions   []component `json:"functions"`
		Databases   []struct {
			ClusterName string `json:"cluster_name"`
			Production  bool   `json:"production"`
		} `json:"databases"`
	} `json:"spec"`
}

// components is every component of the app, of every type.
func (a App) components() []component {
	s := a.Spec
	out := make([]component, 0, len(s.Services)+len(s.StaticSites)+len(s.Workers)+len(s.Jobs)+len(s.Functions))
	for _, set := range [][]component{s.Services, s.StaticSites, s.Workers, s.Jobs, s.Functions} {
		out = append(out, set...)
	}
	return out
}

// list reads every page of one listing: path's rows under key, a page of per.
func list[T any](ctx context.Context, c *Client, path, key string, per int) ([]T, error) {
	var out []T
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	for page := 1; page <= maxListPages; page++ {
		address := APIBase + path + sep + "page=" + strconv.Itoa(page) + "&per_page=" + strconv.Itoa(per)
		var body map[string]json.RawMessage
		if _, err := c.callWithin(ctx, http.MethodGet, address, "", "", listResponse, &body); err != nil {
			return nil, err
		}
		var rows []T
		if held, ok := body[key]; ok {
			if err := json.Unmarshal(held, &rows); err != nil {
				return nil, fmt.Errorf("digitalocean: decode GET %s: %w", path, err)
			}
		}
		out = append(out, rows...)
		var links struct {
			Pages struct {
				Next string `json:"next"`
			} `json:"pages"`
		}
		if held, ok := body["links"]; ok {
			if err := json.Unmarshal(held, &links); err != nil {
				return nil, fmt.Errorf("digitalocean: decode GET %s: %w", path, err)
			}
		}
		if links.Pages.Next == "" {
			return out, nil
		}
	}
	return nil, fmt.Errorf("digitalocean: GET %s did not end within %d pages", path, maxListPages)
}

// Droplets is every Droplet of the team, which needs droplet:read.
func (c *Client) Droplets(ctx context.Context) ([]Droplet, error) {
	return list[Droplet](ctx, c, "/v2/droplets", "droplets", listPage)
}

// Clusters is every Kubernetes cluster, which needs kubernetes:read.
func (c *Client) Clusters(ctx context.Context) ([]Cluster, error) {
	return list[Cluster](ctx, c, "/v2/kubernetes/clusters", "kubernetes_clusters", listPage)
}

// Databases is every managed database cluster, which needs database:read.
func (c *Client) Databases(ctx context.Context) ([]Database, error) {
	return list[Database](ctx, c, "/v2/databases", "databases", listPage)
}

// Balancers is every load balancer, which needs load_balancer:read.
func (c *Client) Balancers(ctx context.Context) ([]Balancer, error) {
	return list[Balancer](ctx, c, "/v2/load_balancers", "load_balancers", listPage)
}

// Domains is every domain, which needs domain:read.
func (c *Client) Domains(ctx context.Context) ([]Domain, error) {
	return list[Domain](ctx, c, "/v2/domains", "domains", listPage)
}

// Records is every record of one type of a domain, which needs domain:read.
func (c *Client) Records(ctx context.Context, domain, recordType string) ([]Record, error) {
	path := "/v2/domains/" + url.PathEscape(domain) + "/records?type=" + url.QueryEscape(recordType)
	return list[Record](ctx, c, path, "domain_records", listPage)
}

// Apps is every App Platform app, which needs app:read.
func (c *Client) Apps(ctx context.Context) ([]App, error) {
	return list[App](ctx, c, "/v2/apps", "apps", appPage)
}

// spaceURN starts the name of a Spaces bucket among a project's resources.
const spaceURN = "do:space:"

// Spaces is the name of every Spaces bucket a project of the team holds,
// which needs project:read. DigitalOcean lists buckets to a Spaces key alone,
// which the atlas never holds, and names each among its project's resources.
func (c *Client) Spaces(ctx context.Context) ([]string, error) {
	type project struct {
		ID string `json:"id"`
	}
	type resource struct {
		URN string `json:"urn"`
	}
	projects, err := list[project](ctx, c, "/v2/projects", "projects", listPage)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range projects {
		resources, err := list[resource](ctx, c, "/v2/projects/"+url.PathEscape(p.ID)+"/resources", "resources", listPage)
		if err != nil {
			return nil, err
		}
		for _, r := range resources {
			if name, ok := strings.CutPrefix(r.URN, spaceURN); ok && name != "" {
				out = append(out, name)
			}
		}
	}
	return out, nil
}
