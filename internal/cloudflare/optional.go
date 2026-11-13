package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// The reads the atlas makes only where the provisioner token holds a
// permission group beyond the ones Crewlet asks for (ADR-0145): Workers
// scripts, Pages projects, R2 buckets and Tunnels. Each decodes a name or an id,
// and nothing else.
//
// WHAT IS NEVER DECODED, though Cloudflare sends it in the same answer: a
// Pages project's `deployment_configs`, which holds its environment
// variables, its build configuration and its source repository, and its
// domains; a Worker's
// routes, handlers and tail consumers; a tunnel's connections and metadata.
// What is never asked for at all: a Worker's content, settings, bindings or
// secrets, a tunnel's token or configuration, and an R2 object.

// WorkerScript is one Workers script, by its name, which is its id.
type WorkerScript struct {
	ID string `json:"id"`
}

// WorkerScripts is every Workers script of the account. Cloudflare serves
// them in one answer.
func (c *Client) WorkerScripts(ctx context.Context) ([]WorkerScript, error) {
	var out []WorkerScript
	_, err := c.do(ctx, http.MethodGet, c.accountPath("/workers/scripts"), nil, nil, &out)
	return out, err
}

// PagesProject is one Pages project, by its name.
type PagesProject struct {
	Name string `json:"name"`
}

// pagesPageSize is the most rows the Pages listing serves a page.
const pagesPageSize = 10

// PagesProjects is every Pages project of the account.
func (c *Client) PagesProjects(ctx context.Context) ([]PagesProject, error) {
	var out []PagesProject
	err := c.listPages(ctx, c.accountPath("/pages/projects"), nil, pagesPageSize, func(raw json.RawMessage) (int, error) {
		var rows []PagesProject
		if err := json.Unmarshal(raw, &rows); err != nil {
			return 0, err
		}
		out = append(out, rows...)
		return len(rows), nil
	})
	return out, err
}

// R2Bucket is one R2 bucket, by its name.
type R2Bucket struct {
	Name string `json:"name"`
}

// r2PageSize is the most rows the R2 listing serves a page.
const r2PageSize = 1000

// R2Buckets is every R2 bucket of the account. The listing pages by a
// cursor, and ends at the page that names none.
func (c *Client) R2Buckets(ctx context.Context) ([]R2Bucket, error) {
	var out []R2Bucket
	path := c.accountPath("/r2/buckets")
	query := url.Values{"per_page": {strconv.Itoa(r2PageSize)}}
	for page := 1; ; page++ {
		if page > listCeiling {
			return nil, fmt.Errorf("cloudflare: %s did not finish after %d pages of %d", path, listCeiling, r2PageSize)
		}
		var body struct {
			Buckets []R2Bucket `json:"buckets"`
		}
		info, err := c.do(ctx, http.MethodGet, path, query, nil, &body)
		if err != nil {
			return nil, err
		}
		out = append(out, body.Buckets...)
		if info == nil || info.Cursor == "" || len(body.Buckets) == 0 {
			return out, nil
		}
		query.Set("cursor", info.Cursor)
	}
}

// Tunnel is one Cloudflare Tunnel, by its id and its name.
type Tunnel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Tunnels is every Cloudflare Tunnel of the account that is not deleted:
// Cloudflare keeps listing a deleted one unless asked not to.
func (c *Client) Tunnels(ctx context.Context) ([]Tunnel, error) {
	var out []Tunnel
	err := c.list(ctx, c.accountPath("/cfd_tunnel"), url.Values{"is_deleted": {"false"}}, func(raw json.RawMessage) (int, error) {
		var rows []Tunnel
		if err := json.Unmarshal(raw, &rows); err != nil {
			return 0, err
		}
		out = append(out, rows...)
		return len(rows), nil
	})
	return out, err
}
