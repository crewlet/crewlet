package datadog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// What the organization holds, as the atlas reads it: monitors, the service
// catalogue, hosts and Synthetic tests. Every read here is a GET, and each
// answer is decoded into the fields named below and nothing else: a monitor's
// query and message, a test's request headers and body, a definition's
// contacts and every creator are never read out of a payload.

// inventoryPageSize is the page every inventory listing asks for. Datadog's
// largest for the catalogue is 100, and a hundred hosts with their tags stay
// far under [maxResponse], which a page of a thousand would not.
const inventoryPageSize = 100

// inventoryPageCeiling stops an inventory listing that is not converging:
// 200 pages, so 20,000 rows, read in a few minutes inside one visit of the
// atlas duty. Past it the listing answers [ErrInventoryTooLarge] rather than
// a short list, which a reader would take for the whole.
const inventoryPageCeiling = 200

// ErrInventoryTooLarge is a listing with more rows than one read walks.
var ErrInventoryTooLarge = errors.New("datadog: the listing holds more rows than one read walks")

// Listing is one inventory listing: what it lists, the Datadog permission
// reading it takes, and a read of one row of it that keeps nothing.
type Listing struct {
	Name       string
	Permission string
	probe      string
}

// The inventory listings, each behind a permission of its own.
var (
	ListMonitors = Listing{"monitors", "monitors_read", "/api/v1/monitor?page=0&page_size=1"}
	ListServices = Listing{"service catalogue", "apm_service_catalog_read",
		"/api/v2/services/definitions?page[size]=1"}
	ListHosts      = Listing{"hosts", "hosts_read", "/api/v1/hosts?count=1"}
	ListSynthetics = Listing{"Synthetic tests", "synthetics_read", "/api/v1/synthetics/tests?page_size=1"}
)

// Listings is every inventory listing, in the order a read makes them.
func Listings() []Listing { return []Listing{ListMonitors, ListServices, ListHosts, ListSynthetics} }

// Missing is what a person is told where the keys may not read the listing:
// what the atlas goes without, and the permission that restores it.
func (l Listing) Missing() (title, action string) {
	return "The atlas cannot show the organization's " + l.Name + ".",
		"Give the application key's owner a role with the " + l.Permission + " permission."
}

// CanList reads one row of a listing and keeps nothing, to learn whether the
// keys may read it at all.
func (c *Client) CanList(ctx context.Context, creds Credentials, l Listing) error {
	return c.do(ctx, http.MethodGet, l.probe, creds, nil, nil)
}

// pages walks a listing page by page until a page comes back short, calling
// read with each page's number; read answers how many rows the page held.
func pages(what string, read func(page int) (int, error)) error {
	for page := 0; ; page++ {
		if page >= inventoryPageCeiling {
			return fmt.Errorf("%w: %s did not end after %d pages of %d", ErrInventoryTooLarge, what,
				inventoryPageCeiling, inventoryPageSize)
		}
		n, err := read(page)
		if err != nil {
			return err
		}
		if n < inventoryPageSize {
			return nil
		}
	}
}

// Monitor is one monitor: its id, its name, its tags, the state Datadog
// reports for it as a whole and when that state began.
type Monitor struct {
	ID         int64
	Name       string
	Tags       []string
	State      string
	StateSince time.Time
}

// Monitors lists every monitor of the organization (`monitors_read`).
func (c *Client) Monitors(ctx context.Context, creds Credentials) ([]Monitor, error) {
	var out []Monitor
	err := pages("the monitors", func(page int) (int, error) {
		var body []struct {
			ID       int64    `json:"id"`
			Name     string   `json:"name"`
			Tags     []string `json:"tags"`
			State    string   `json:"overall_state"`
			Modified string   `json:"overall_state_modified"`
		}
		path := fmt.Sprintf("/api/v1/monitor?page=%d&page_size=%d", page, inventoryPageSize)
		if err := c.do(ctx, http.MethodGet, path, creds, nil, &body); err != nil {
			return 0, err
		}
		for _, m := range body {
			// A state with no instant Datadog can be read as has none.
			since, _ := time.Parse(time.RFC3339, m.Modified)
			out = append(out, Monitor{ID: m.ID, Name: m.Name, Tags: m.Tags, State: m.State, StateSince: since})
		}
		return len(body), nil
	})
	return out, err
}

// ServiceDefinition is one service of the catalogue: its name and the
// addresses of the repositories its definition links.
type ServiceDefinition struct {
	Name  string
	Repos []string
}

// ServiceDefinitions lists the service catalogue (`apm_service_catalog_read`),
// each definition in the v2.2 schema whatever schema it was written in.
func (c *Client) ServiceDefinitions(ctx context.Context, creds Credentials) ([]ServiceDefinition, error) {
	var out []ServiceDefinition
	err := pages("the service catalogue", func(page int) (int, error) {
		var body struct {
			Data []struct {
				Attributes struct {
					Schema struct {
						Service string `json:"dd-service"`
						Links   []struct {
							Type string `json:"type"`
							URL  string `json:"url"`
						} `json:"links"`
					} `json:"schema"`
				} `json:"attributes"`
			} `json:"data"`
		}
		path := fmt.Sprintf("/api/v2/services/definitions?page[size]=%d&page[number]=%d&schema_version=v2.2",
			inventoryPageSize, page)
		if err := c.do(ctx, http.MethodGet, path, creds, nil, &body); err != nil {
			return 0, err
		}
		for _, d := range body.Data {
			def := ServiceDefinition{Name: d.Attributes.Schema.Service}
			for _, l := range d.Attributes.Schema.Links {
				if l.Type == "repo" {
					def.Repos = append(def.Repos, l.URL)
				}
			}
			out = append(out, def)
		}
		return len(body.Data), nil
	})
	return out, err
}

// Host is one host reporting to the organization: its name, and its tags
// from every source that tagged it.
type Host struct {
	Name string
	Tags []string
}

// Hosts lists every host that reported in Datadog's own default window, the
// last three hours (`hosts_read`).
func (c *Client) Hosts(ctx context.Context, creds Credentials) ([]Host, error) {
	var out []Host
	err := pages("the hosts", func(int) (int, error) {
		var body struct {
			Hosts []struct {
				Name string              `json:"name"`
				Tags map[string][]string `json:"tags_by_source"`
			} `json:"host_list"`
		}
		// THE OFFSET IS THE ROWS READ SO FAR. Datadog's reference writes
		// the next offset as one more; if the offset counts from zero that
		// skips a host, and if it counts from one this repeats a host,
		// which the reader folds. Only the first is a host never read.
		path := fmt.Sprintf("/api/v1/hosts?start=%d&count=%d", len(out), inventoryPageSize)
		if err := c.do(ctx, http.MethodGet, path, creds, nil, &body); err != nil {
			return 0, err
		}
		for _, h := range body.Hosts {
			host := Host{Name: h.Name}
			for _, tags := range h.Tags {
				host.Tags = append(host.Tags, tags...)
			}
			out = append(out, host)
		}
		return len(body.Hosts), nil
	})
	return out, err
}

// SyntheticTest is one Synthetic test: the monitor that carries its state,
// whether it runs, and the address or the host it checks.
type SyntheticTest struct {
	PublicID  string
	MonitorID int64
	Live      bool
	URL       string
	Host      string
}

// SyntheticTests lists every Synthetic test (`synthetics_read`).
func (c *Client) SyntheticTests(ctx context.Context, creds Credentials) ([]SyntheticTest, error) {
	var out []SyntheticTest
	err := pages("the Synthetic tests", func(page int) (int, error) {
		var body struct {
			Tests []struct {
				PublicID  string `json:"public_id"`
				MonitorID int64  `json:"monitor_id"`
				Status    string `json:"status"`
				Config    struct {
					Request struct {
						URL  string `json:"url"`
						Host string `json:"host"`
					} `json:"request"`
				} `json:"config"`
			} `json:"tests"`
		}
		path := fmt.Sprintf("/api/v1/synthetics/tests?page_size=%d&page_number=%d", inventoryPageSize, page)
		if err := c.do(ctx, http.MethodGet, path, creds, nil, &body); err != nil {
			return 0, err
		}
		for _, t := range body.Tests {
			out = append(out, SyntheticTest{PublicID: t.PublicID, MonitorID: t.MonitorID,
				Live: t.Status == "live", URL: t.Config.Request.URL, Host: t.Config.Request.Host})
		}
		return len(body.Tests), nil
	})
	return out, err
}
