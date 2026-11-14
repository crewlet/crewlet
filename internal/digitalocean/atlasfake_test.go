package digitalocean

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// inventory is a team's resources as DigitalOcean's listings answer them:
// each row a whole object, the fields the atlas never decodes included.
type inventory struct {
	droplets, clusters, databases, balancers, domains, apps []map[string]any
	// records is each domain's records, and projects each project's URNs.
	records  map[string][]map[string]any
	projects map[string][]string
	// fail answers a listing this status, by its path.
	fail map[string]int
	// pageCap serves at most this many rows a page, whatever was asked for.
	pageCap int
	// pages counts the pages served, by path.
	pages map[string]int
}

// listing is the scope, the key and the rows of the listing at path.
func (inv *inventory) listing(r *http.Request) (scope, key string, rows []map[string]any, ok bool) {
	path := r.URL.Path
	switch {
	case path == "/v2/droplets":
		return "droplet:read", "droplets", inv.droplets, true
	case path == "/v2/kubernetes/clusters":
		return "kubernetes:read", "kubernetes_clusters", inv.clusters, true
	case path == "/v2/databases":
		return "database:read", "databases", inv.databases, true
	case path == "/v2/load_balancers":
		return "load_balancer:read", "load_balancers", inv.balancers, true
	case path == "/v2/domains":
		return "domain:read", "domains", inv.domains, true
	case path == "/v2/apps":
		return "app:read", "apps", inv.apps, true
	case path == "/v2/projects":
		for id := range inv.projects {
			rows = append(rows, map[string]any{"id": id, "name": "project " + id, "owner_uuid": canaryText})
		}
		slices.SortFunc(rows, func(a, z map[string]any) int { return strings.Compare(a["id"].(string), z["id"].(string)) })
		return "project:read", "projects", rows, true
	}
	if name, found := strings.CutSuffix(strings.TrimPrefix(path, "/v2/domains/"), "/records"); found &&
		strings.HasPrefix(path, "/v2/domains/") {
		for _, rec := range inv.records[name] {
			// DigitalOcean filters by type where one is asked for.
			if want := r.URL.Query().Get("type"); want == "" || rec["type"] == want {
				rows = append(rows, rec)
			}
		}
		return "domain:read", "domain_records", rows, true
	}
	if id, found := strings.CutSuffix(strings.TrimPrefix(path, "/v2/projects/"), "/resources"); found &&
		strings.HasPrefix(path, "/v2/projects/") {
		for _, urn := range inv.projects[id] {
			rows = append(rows, map[string]any{"urn": urn, "status": "ok",
				"links": map[string]any{"self": "https://api.digitalocean.com/v2/" + canaryText}})
		}
		return "project:read", "resources", rows, true
	}
	return "", "", nil, false
}

// serve answers one listing, a page at a time, and false for any other path.
func (inv *inventory) serve(w http.ResponseWriter, r *http.Request, p *profile) bool {
	scope, key, rows, ok := inv.listing(r)
	if !ok {
		return false
	}
	if !slices.Contains(p.scopes, scope) {
		fail(w, http.StatusForbidden, "Forbidden")
		return true
	}
	if code, set := inv.fail[r.URL.Path]; set {
		fail(w, code, "server_error")
		return true
	}
	if inv.pages == nil {
		inv.pages = map[string]int{}
	}
	inv.pages[r.URL.Path]++
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	page, per = max(page, 1), max(per, 1)
	if inv.pageCap > 0 {
		per = min(per, inv.pageCap)
	}
	body := map[string]any{"meta": map[string]any{"total": len(rows)}, "links": map[string]any{}}
	// The databases listing is not paged: it answers every cluster at once.
	if key != "databases" {
		from, to := min((page-1)*per, len(rows)), min(page*per, len(rows))
		if to < len(rows) {
			body["links"] = map[string]any{"pages": map[string]any{
				"next": "https://api.digitalocean.com" + r.URL.Path + "?page=" + strconv.Itoa(page+1)}}
		}
		rows = rows[from:to]
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	body[key] = rows
	_ = json.NewEncoder(w).Encode(body)
	return true
}
