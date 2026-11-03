package digitalocean

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
)

// A check is one request that proves whether a token holds a scope and cannot
// have an effect: a read, or a write whose body DigitalOcean must reject or
// whose target cannot exist. DigitalOcean refuses a token without the scope
// with 403 before it reads the body or looks the resource up, and answers 200,
// 400, 404 or 422 otherwise. Every check below was proven live, on
// 2026-11-03, with a token holding the scope and one without it; a request
// whose answer was ambiguous is not here.
type check struct {
	scope  string
	method string
	path   func() string
	body   string
	// can is what holding the scope lets a token do, for a refusal.
	can string
}

// at is a fixed path.
func at(path string) func() string { return func() string { return path } }

// absent is a path naming a random id that cannot exist.
func absent(prefix, suffix string) func() string {
	return func() string { return prefix + uuid.NewString() + suffix }
}

// absentDomain is a domain under the reserved .invalid top-level domain.
func absentDomain() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "crewlet-check-" + hex.EncodeToString(b[:]) + ".invalid"
}

// readChecks prove each Read scope with a one-row list read.
var readChecks = []check{
	{scope: "actions:read", method: http.MethodGet, path: at("/v2/actions?per_page=1")},
	{scope: "regions:read", method: http.MethodGet, path: at("/v2/regions?per_page=1")},
	{scope: "sizes:read", method: http.MethodGet, path: at("/v2/sizes?per_page=1")},
	{scope: "image:read", method: http.MethodGet, path: at("/v2/images?per_page=1")},
	{scope: "snapshot:read", method: http.MethodGet, path: at("/v2/snapshots?per_page=1")},
	{scope: "droplet:read", method: http.MethodGet, path: at("/v2/droplets?per_page=1")},
	{scope: "block_storage:read", method: http.MethodGet, path: at("/v2/volumes?per_page=1")},
	{scope: "block_storage_action:read", method: http.MethodGet, path: absent("/v2/volumes/", "/actions")},
	{scope: "block_storage_snapshot:read", method: http.MethodGet, path: absent("/v2/volumes/", "/snapshots")},
	{scope: "firewall:read", method: http.MethodGet, path: at("/v2/firewalls?per_page=1")},
	{scope: "load_balancer:read", method: http.MethodGet, path: at("/v2/load_balancers?per_page=1")},
	{scope: "vpc:read", method: http.MethodGet, path: at("/v2/vpcs?per_page=1")},
	{scope: "vpc_peering:read", method: http.MethodGet, path: at("/v2/vpc_peerings?per_page=1")},
	{scope: "nat_gateway:read", method: http.MethodGet, path: at("/v2/vpc_nat_gateways?per_page=1")},
	{scope: "reserved_ip:read", method: http.MethodGet, path: at("/v2/reserved_ips?per_page=1")},
	{scope: "domain:read", method: http.MethodGet, path: at("/v2/domains?per_page=1")},
	{scope: "certificate:read", method: http.MethodGet, path: at("/v2/certificates?per_page=1")},
	{scope: "cdn:read", method: http.MethodGet, path: at("/v2/cdn/endpoints?per_page=1")},
	{scope: "database:read", method: http.MethodGet, path: at("/v2/databases")},
	{scope: "kubernetes:read", method: http.MethodGet, path: at("/v2/kubernetes/clusters?per_page=1")},
	{scope: "app:read", method: http.MethodGet, path: at("/v2/apps?per_page=1")},
	{scope: "monitoring:read", method: http.MethodGet, path: at("/v2/monitoring/alerts?per_page=1")},
	{scope: "uptime:read", method: http.MethodGet, path: at("/v2/uptime/checks?per_page=1")},
	{scope: "project:read", method: http.MethodGet, path: at("/v2/projects?per_page=1")},
	{scope: "tag:read", method: http.MethodGet, path: at("/v2/tags?per_page=1")},
	{scope: "ssh_key:read", method: http.MethodGet, path: at("/v2/account/keys?per_page=1")},
}

// separatorChecks prove the writes that separate Read from Operate.
var separatorChecks = []check{
	{scope: "droplet:create", method: http.MethodPost, path: at("/v2/droplets"), body: "{}", can: "create Droplets"},
	{scope: "droplet:update", method: http.MethodPost, path: at("/v2/droplets/0/actions"), body: "{",
		can: "power, resize and rebuild Droplets"},
	{scope: "droplet:delete", method: http.MethodDelete, path: at("/v2/droplets/0"), can: "delete Droplets"},
	{scope: "block_storage:delete", method: http.MethodDelete, path: absent("/v2/volumes/", ""), can: "delete volumes"},
	{scope: "firewall:update", method: http.MethodPut, path: absent("/v2/firewalls/", ""), body: "{}",
		can: "change firewalls"},
	{scope: "load_balancer:delete", method: http.MethodDelete, path: absent("/v2/load_balancers/", ""),
		can: "delete load balancers"},
	{scope: "vpc:update", method: http.MethodPatch, path: absent("/v2/vpcs/", ""), body: "{}", can: "change VPCs"},
	{scope: "domain:update", method: http.MethodPut,
		path: func() string { return "/v2/domains/" + absentDomain() + "/records/0" }, body: "{}",
		can: "change DNS records"},
	{scope: "database:update", method: http.MethodPut, path: absent("/v2/databases/", "/resize"), body: "{}",
		can: "resize and change databases"},
	{scope: "kubernetes:update", method: http.MethodPut, path: absent("/v2/kubernetes/clusters/", ""), body: "{}",
		can: "change Kubernetes clusters"},
	{scope: "app:update", method: http.MethodPut, path: absent("/v2/apps/", ""), body: "{}", can: "change apps"},
	{scope: "tag:create", method: http.MethodPost, path: at("/v2/tags"), body: "{}", can: "create tags"},
}

// forbiddenChecks prove the scopes no level grants. Reads only, apart from
// one malformed create that DigitalOcean was seen not to audit: a malformed
// POST /v2/spaces/keys is audited as "spaces_key.added" although refused, so
// the Spaces key scopes are proven by spaces_key:read, which DigitalOcean
// adds to every one of them.
var forbiddenChecks = []check{
	{scope: "billing:read", method: http.MethodGet, path: at("/v2/customers/my/balance"), can: "read billing"},
	{scope: "security:read", method: http.MethodGet, path: at("/v2/security/settings"),
		can: "read the team's stored secrets"},
	{scope: "registry:read", method: http.MethodGet, path: at("/v2/registry"),
		can: "create container registry credentials"},
	{scope: "spaces_key:read", method: http.MethodGet, path: at("/v2/spaces/keys?per_page=1"),
		can: "reach Spaces access keys"},
	{scope: "dedicated_inference_token:read", method: http.MethodGet,
		path: absent("/v2/dedicated-inferences/", "/tokens"), can: "read inference tokens"},
	{scope: "ssh_key:create", method: http.MethodPost, path: at("/v2/account/keys"), body: "{",
		can: "add SSH keys that open future Droplets"},
}

// administerChecks prove the scopes only Administer grants. droplet:admin
// guards no public endpoint, so nothing proves it either way.
var administerChecks = []check{
	{scope: "kubernetes:access_cluster", method: http.MethodGet,
		path: absent("/v2/kubernetes/clusters/", "/kubeconfig"), can: "download cluster credentials"},
	{scope: "database:view_credentials", method: http.MethodGet,
		path: absent("/v2/vector-databases/", "/credentials"), can: "read database passwords"},
	{scope: "app:access_console", method: http.MethodGet, path: absent("/v2/apps/", "/components/crewlet/exec"),
		can: "open consoles on apps"},
}

// checkConcurrency bounds one token's checks in flight: about forty checks
// finish in a few round trips, far inside the 250 a minute DigitalOcean allows.
const checkConcurrency = 6

// Held is which scopes a token was proven to hold, by scope.
type Held map[string]bool

// prove runs every check in sets and answers what each proved. Only the
// transport, a 401 or an answer no check expects is an error; a 2xx to a write
// is one too, because no write check can succeed.
func prove(ctx context.Context, c *Client, sets ...[]check) (Held, error) {
	var all []check
	for _, set := range sets {
		all = append(all, set...)
	}
	results := make([]bool, len(all))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(checkConcurrency)
	for i, ch := range all {
		g.Go(func() error {
			path := ch.path()
			status, err := c.Answer(gctx, ch.method, path, ch.body)
			if err != nil {
				return err
			}
			held, err := heldBy(ch.method, status)
			if err != nil {
				return fmt.Errorf("digitalocean: %s %s for %s: %w", ch.method, path, ch.scope, err)
			}
			results[i] = held
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	out := Held{}
	for i, ch := range all {
		out[ch.scope] = results[i]
	}
	return out, nil
}

// errTokenRefused is a check DigitalOcean answered 401: the token is dead.
var errTokenRefused = fmt.Errorf("digitalocean: the token answered %d", http.StatusUnauthorized)

// heldBy reads one check's status: 403 is the scope missing, a rejection of
// the request itself is the scope held, and a 2xx to a write is an error.
func heldBy(method string, status int) (bool, error) {
	switch {
	case status == http.StatusForbidden:
		return false, nil
	case status == http.StatusUnauthorized:
		return false, errTokenRefused
	case status >= 200 && status < 300 && method != http.MethodGet:
		return false, fmt.Errorf("answered %d to a check that cannot succeed", status)
	case status >= 200 && status < 300, status == http.StatusBadRequest, status == http.StatusNotFound,
		status == http.StatusUnprocessableEntity:
		return true, nil
	default:
		return false, fmt.Errorf("answered %d, which no check expects", status)
	}
}

// missing is the first check of set whose scope held does not hold.
func (h Held) missing(set []check) (check, bool) {
	i := slices.IndexFunc(set, func(c check) bool { return !h[c.scope] })
	if i < 0 {
		return check{}, false
	}
	return set[i], true
}

// first is the first check of set whose scope held holds.
func (h Held) first(set []check) (check, bool) {
	i := slices.IndexFunc(set, func(c check) bool { return h[c.scope] })
	if i < 0 {
		return check{}, false
	}
	return set[i], true
}

// all reports every scope of set held, and none reports none of them.
func (h Held) all(set []check) bool  { _, gap := h.missing(set); return !gap }
func (h Held) none(set []check) bool { _, found := h.first(set); return !found }

// Level is the level the proven scopes are, or "" where they are no level's:
// a read missing, or a set of writes neither all nor none of a level's.
func (h Held) Level() string {
	switch {
	case !h.all(readChecks):
		return ""
	case h.none(separatorChecks) && h.none(administerChecks):
		return levels[0].id
	case !h.all(separatorChecks):
		return ""
	case h.none(administerChecks):
		return levels[1].id
	case h.all(administerChecks):
		return levels[2].id
	default:
		return ""
	}
}
