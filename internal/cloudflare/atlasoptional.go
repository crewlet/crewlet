package cloudflare

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/redact"
)

// What the atlas reads only where the provisioner token holds a permission
// group nobody is asked for (ADR-0145): Workers scripts, Pages projects, R2
// buckets and Tunnels, each by name. A token without the group is whole: the
// part is an OPTIONAL one ([collect.Unread.Optional]), so the source's
// standing names it only where an earlier read recorded something from it,
// which is a permission somebody took away.

// The optional parts of the read, each one listing's.
const (
	partWorkers = "workers"
	partPages   = "pages"
	partR2      = "r2"
	partTunnels = "tunnels"
)

// The key every entry of an optional part starts with, after its kind.
const (
	workerKeyPrefix = string(atlas.VendorCloudflare) + "/workers/"
	pagesKeyPrefix  = string(atlas.VendorCloudflare) + "/pages/"
	r2KeyPrefix     = string(atlas.VendorCloudflare) + "/r2/"
	tunnelKeyPrefix = string(atlas.VendorCloudflare) + "/tunnels/"
)

// optionalPart is one optional part: the kind and the key its entries carry,
// what its listing shows the account does, and the read, which lists
// everything before it claims anything.
type optionalPart struct {
	name string
	kind atlas.Kind
	key  string
	uses atlas.Capability
	read func(ctx context.Context, c *Client) (func(*collect.Builder, atlas.Ref), error)
}

// optionalParts is every optional part, in the order a read makes them.
//
//nolint:gochecknoglobals // an immutable table, not state
var optionalParts = []optionalPart{
	{name: partWorkers, kind: atlas.KindWorkload, key: workerKeyPrefix, uses: atlas.CapabilityCompute, read: readWorkers},
	{name: partPages, kind: atlas.KindWorkload, key: pagesKeyPrefix, uses: atlas.CapabilityCompute, read: readPages},
	{name: partR2, kind: atlas.KindBucket, key: r2KeyPrefix, uses: atlas.CapabilityObjectStorage, read: readR2},
	{name: partTunnels, kind: atlas.KindWorkload, key: tunnelKeyPrefix, read: readTunnels},
}

// held reports whether a claim the voice holds came from the part: one on
// the part's own entries, or the capability its listing gives the account.
func (p optionalPart) held(c atlas.Claim) bool {
	if c.Subject.Kind == p.kind && strings.HasPrefix(c.Subject.Key, p.key) {
		return true
	}
	return p.uses != "" && c.Subject.Kind == atlas.KindAccount && c.Facet.Kind == atlas.FacetFact &&
		c.Facet.Fact == atlas.FactCapability && c.Value.Text == string(p.uses)
}

// readOptional reads each optional part. One Cloudflare refuses with a 403
// is left unread, as a suggestion of what to add; every other failure fails
// the read, since a listing cut short would retire what it did not reach.
func readOptional(ctx context.Context, b *collect.Builder, client *Client, account atlas.Ref) error {
	for _, p := range optionalParts {
		claim, err := p.read(ctx, client)
		switch {
		case err == nil:
			claim(b, account)
		case Status(err) == http.StatusForbidden && !TokenRefused(err):
			perm := atlasPermissionOf(p.name)
			b.Unread(collect.Unread{
				Part: p.name, Optional: true, Holds: p.held,
				Detail: "See more in Atlas: add " + perm.builder + " to the Cloudflare token to show " + perm.shows + ".",
			})
		default:
			return fmt.Errorf("cloudflare: read the account's %s: %w", atlasPermissionOf(p.name).shows, reject(err))
		}
	}
	return nil
}

// named claims one entry of an optional part under prefix and the vendor's
// own name for it, with that name as its name. A NAME SHAPED LIKE A
// CREDENTIAL IS REFUSED BEFORE IT IS WRITTEN AS A KEY SEGMENT, whose escapes
// would reshape it into text no rule knows.
func named(b *collect.Builder, where string, kind atlas.Kind, prefix, id, name string) (atlas.Ref, bool) {
	if redact.Contains(id) {
		b.Withhold(where, fmt.Errorf("%s key: %w", kind, atlas.ErrCredential))
		return atlas.Ref{}, false
	}
	entry, ok := b.Ref(where, kind, prefix+collect.KeySegment(id))
	if ok {
		b.Says(where+".name", entry, atlas.FactName, collect.PlainLabel(name))
	}
	return entry, ok
}

// uses says the account does what a part's listing showed.
func uses(b *collect.Builder, where string, account atlas.Ref, c atlas.Capability) {
	b.Says(where, account, atlas.FactCapability, string(c))
}

func readWorkers(ctx context.Context, c *Client) (func(*collect.Builder, atlas.Ref), error) {
	rows, err := c.WorkerScripts(ctx)
	return func(b *collect.Builder, account atlas.Ref) {
		slices.SortFunc(rows, func(a, z WorkerScript) int { return cmp.Compare(a.ID, z.ID) })
		for i, w := range rows {
			where := fmt.Sprintf("workers.scripts[%d]", i)
			if script, ok := named(b, where, atlas.KindWorkload, workerKeyPrefix, w.ID, w.ID); ok {
				b.Relates(where, script, atlas.RelationRunsOn, account)
				uses(b, where, account, atlas.CapabilityCompute)
			}
		}
	}, err
}

func readPages(ctx context.Context, c *Client) (func(*collect.Builder, atlas.Ref), error) {
	rows, err := c.PagesProjects(ctx)
	return func(b *collect.Builder, account atlas.Ref) {
		slices.SortFunc(rows, func(a, z PagesProject) int { return cmp.Compare(a.Name, z.Name) })
		for i, p := range rows {
			where := fmt.Sprintf("pages.projects[%d]", i)
			project, ok := named(b, where, atlas.KindWorkload, pagesKeyPrefix, p.Name, p.Name)
			if !ok {
				continue
			}
			b.Relates(where, project, atlas.RelationRunsOn, account)
			uses(b, where, account, atlas.CapabilityCompute)
		}
	}, err
}

func readR2(ctx context.Context, c *Client) (func(*collect.Builder, atlas.Ref), error) {
	rows, err := c.R2Buckets(ctx)
	return func(b *collect.Builder, account atlas.Ref) {
		slices.SortFunc(rows, func(a, z R2Bucket) int { return cmp.Compare(a.Name, z.Name) })
		for i, r := range rows {
			where := fmt.Sprintf("r2.buckets[%d]", i)
			if bucket, ok := named(b, where, atlas.KindBucket, r2KeyPrefix, r.Name, r.Name); ok {
				b.Relates(where, bucket, atlas.RelationBelongsTo, account)
				uses(b, where, account, atlas.CapabilityObjectStorage)
			}
		}
	}, err
}

func readTunnels(ctx context.Context, c *Client) (func(*collect.Builder, atlas.Ref), error) {
	rows, err := c.Tunnels(ctx)
	return func(b *collect.Builder, account atlas.Ref) {
		slices.SortFunc(rows, func(a, z Tunnel) int { return cmp.Compare(a.ID, z.ID) })
		for i, t := range rows {
			where := fmt.Sprintf("tunnels[%d]", i)
			if tunnel, ok := named(b, where, atlas.KindWorkload, tunnelKeyPrefix, t.ID, t.Name); ok {
				b.Relates(where, tunnel, atlas.RelationRunsOn, account)
			}
		}
	}, err
}
