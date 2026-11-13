package cloudflare

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
)

// Cloudflare's half of the atlas (ADR-0129): what the account holds, read
// whole under the provisioner token the company already gave the engine. The
// token holds every Read group on every zone (levels.go), so nothing here
// needs a raise; what would (Workers, Pages, R2, Tunnels, load balancers) is
// not read.

func init() { collect.Register(atlasSource{}) }

// atlasSource is Cloudflare's [collect.Source].
type atlasSource struct{}

func (atlasSource) Kind() integration.Kind { return integration.KindCloudflare }

// Collect reads the account's name, every zone, every A, AAAA and CNAME
// record and every certificate pack, and reports them as claims in the
// integration's voice. It writes nothing at Cloudflare.
func (atlasSource) Collect(ctx context.Context, in collect.Input) (collect.Read, error) {
	client, err := atlasClient(in)
	if err != nil {
		return collect.Read{}, err
	}
	return readAtlas(ctx, client, in.Now)
}

// atlasClient is the client the read makes, from the company's block, or
// [integration.ErrNotConfigured] where the company does not connect
// Cloudflare or its credential does not resolve.
func atlasClient(in collect.Input) (*Client, error) {
	if in.Config == nil {
		return nil, fmt.Errorf("cloudflare: %w: no company document", integration.ErrNotConfigured)
	}
	cfg := in.Config.Integrations.Cloudflare
	if cfg == nil || !cfg.Enabled || cfg.Provisioning == nil {
		return nil, fmt.Errorf("cloudflare: %w: integrations.cloudflare is off or has no provisioning block",
			integration.ErrNotConfigured)
	}
	var account, token string
	if in.Env != nil {
		account = strings.TrimSpace(in.Env.Value(cfg.Provisioning.AccountID))
		token = strings.TrimSpace(in.Env.Value(cfg.Provisioning.APIToken))
	}
	if account == "" || token == "" {
		return nil, fmt.Errorf("cloudflare: %w: the account ID and the provisioner token did not both resolve",
			integration.ErrNotConfigured)
	}
	client, err := NewClient(ClientOptions{AccountID: account, Token: token, HTTP: in.HTTP})
	if err != nil {
		return nil, fmt.Errorf("cloudflare: integrations.cloudflare.provisioning.account_id: %w", err)
	}
	return client, nil
}

// readAtlas is the read itself, over a client.
func readAtlas(ctx context.Context, client *Client, now time.Time) (collect.Read, error) {
	b := collect.NewBuilder(integration.KindCloudflare, now)
	acct, err := client.Account(ctx)
	if err != nil {
		return collect.Read{}, fmt.Errorf("cloudflare: read the account: %w", reject(err))
	}
	account, ok := b.Ref("account", atlas.KindAccount, string(atlas.VendorCloudflare)+"/"+client.AccountID())
	if !ok {
		return collect.Read{}, fmt.Errorf("cloudflare: the account %q is not an entry the atlas can name",
			client.AccountID())
	}
	b.Says("account.name", account, atlas.FactAccountName, acct.Name)
	zones, err := client.Zones(ctx)
	if err != nil {
		return collect.Read{}, fmt.Errorf("cloudflare: read the account's zones: %w", reject(err))
	}
	slices.SortFunc(zones, func(a, z Zone) int { return cmp.Or(cmp.Compare(a.Name, z.Name), cmp.Compare(a.ID, z.ID)) })
	// EVERY ZONE FIRST, so a CNAME into another of the account's zones finds
	// its zone whatever order the zones were listed in.
	refs := map[string]atlas.Ref{}
	for _, z := range zones {
		where := "zones[" + z.ID + "]"
		zone, ok := b.Ref(where+".name", atlas.KindDomain, z.Name)
		if !ok {
			continue
		}
		refs[z.ID] = zone
		b.Relates(where, zone, atlas.RelationServedBy, account)
		b.Relates(where, zone, atlas.RelationBelongsTo, account)
	}
	byName := make([]atlas.Ref, 0, len(refs))
	for _, zone := range refs {
		byName = append(byName, zone)
	}
	slices.SortFunc(byName, func(a, z atlas.Ref) int { return cmp.Compare(a.Key, z.Key) })
	for _, z := range zones {
		zone, ok := refs[z.ID]
		if !ok {
			continue
		}
		where := "zones[" + z.ID + "]"
		records, err := client.DNSRecords(ctx, z.ID)
		if err != nil {
			return collect.Read{}, fmt.Errorf("cloudflare: read the records of %s: %w", z.Name, reject(err))
		}
		names := readRecords(b, where, account, zone, byName, records)
		packs, err := client.CertificatePacks(ctx, z.ID)
		if err != nil {
			return collect.Read{}, fmt.Errorf("cloudflare: read the certificates of %s: %w", z.Name, reject(err))
		}
		readPacks(b, where, account, zone, names, packs)
	}
	b.Calls(client.Calls())
	return b.Read(), nil
}

// readRecords claims each name a zone's A, AAAA and CNAME records give, what
// it points at and whether Cloudflare fronts it, and answers the names.
func readRecords(b *collect.Builder, where string, account, zone atlas.Ref, zones []atlas.Ref,
	records []DNSRecord) []atlas.Ref {

	var names []atlas.Ref
	for _, r := range records {
		if !slices.Contains([]string{RecordA, RecordAAAA, RecordCNAME}, r.Type) {
			continue
		}
		at := where + ".dns_records[" + r.ID + "]"
		host, ok := b.Ref(at+".name", atlas.KindHostname, r.Name)
		if !ok {
			continue
		}
		names = append(names, host)
		b.Relates(at+".name", host, atlas.RelationInZone, zone)
		if r.Proxied {
			b.Relates(at+".proxied", host, atlas.RelationFrontedBy, account)
		}
		switch r.Type {
		case RecordCNAME:
			target, ok := b.Ref(at+".content", atlas.KindHostname, r.Content)
			if !ok {
				continue
			}
			b.Relates(at+".content", host, atlas.RelationResolvesTo, target)
			// A NAME INSIDE ONE OF THE ACCOUNT'S ZONES sits in it, whichever
			// zone's record pointed there.
			if owner, found := collect.ZoneOf(target, zones); found {
				b.Relates(at+".content", target, atlas.RelationInZone, owner)
			}
		default:
			key, err := atlas.AddressHostKey(r.Content)
			if err != nil {
				b.Withhold(at+".content", err)
				continue
			}
			origin, ok := b.Ref(at+".content", atlas.KindHost, key)
			if !ok {
				continue
			}
			b.Says(at+".content", origin, atlas.FactAddress, r.Content)
			b.Relates(at+".content", host, atlas.RelationResolvesTo, origin)
		}
	}
	return names
}

// readPacks claims each certificate pack of a zone: what it covers, its type,
// validity, issuer and expiry, and which of the zone's names it secures.
func readPacks(b *collect.Builder, where string, account, zone atlas.Ref, names []atlas.Ref,
	packs []CertificatePack) {

	for _, p := range packs {
		at := where + ".certificate_packs[" + p.ID + "]"
		cert, ok := b.Ref(at+".id", atlas.KindCertificate, string(atlas.VendorCloudflare)+"/"+p.ID)
		if !ok {
			continue
		}
		b.Relates(at, cert, atlas.RelationInZone, zone)
		b.Relates(at, cert, atlas.RelationBelongsTo, account)
		b.Says(at+".type", cert, atlas.FactCertificateType, certificateType(p.Type))
		b.Says(at+".status", cert, atlas.FactValidity, validity(p.Status))
		for i, host := range p.Hosts {
			b.Says(at+".hosts["+strconv.Itoa(i)+"]", cert, atlas.FactCovers, host)
		}
		if primary, ok := p.Primary(); ok {
			b.Says(at+".certificates.issuer", cert, atlas.FactIssuer, primary.Issuer)
			if !primary.ExpiresOn.IsZero() {
				b.Says(at+".certificates.expires_on", cert, atlas.FactExpires, primary.ExpiresOn.UTC().Format(time.RFC3339))
			}
		}
		for _, name := range names {
			if covers(p.Hosts, name.Key) {
				b.Relates(at+".hosts", name, atlas.RelationSecuredBy, cert)
			}
		}
	}
}

// covers reports whether a pack's hosts cover name: the name itself, or a
// wildcard exactly one label above it, as TLS matches them.
func covers(hosts []string, name string) bool {
	for _, h := range hosts {
		h = config.CloudflareZoneKey(h)
		if h == name {
			return true
		}
		if rest, ok := strings.CutPrefix(h, "*."); ok {
			label, under := strings.CutSuffix(name, "."+rest)
			if under && label != "" && !strings.Contains(label, ".") {
				return true
			}
		}
	}
	return false
}

// certificateType folds Cloudflare's pack types into the atlas's words: its
// universal and advanced packs, and every custom one (legacy, SNI or
// dedicated) as custom.
func certificateType(vendor string) string {
	switch vendor {
	case "universal", "advanced":
		return vendor
	case "custom", "legacy_custom", "sni_custom", "dedicated_custom":
		return "custom"
	}
	return vendor
}

// validity folds Cloudflare's pack statuses into the atlas's words: active
// and expired as they are, every timed-out step as failed, a pack on its way
// out as inactive, and everything on the way to active as pending.
func validity(vendor string) string {
	switch {
	case vendor == "active" || vendor == "expired":
		return vendor
	case strings.HasSuffix(vendor, "_timed_out"):
		return "failed"
	case vendor == "deleted" || vendor == "inactive" || vendor == "deactivating" ||
		vendor == "pending_deletion" || vendor == "pending_cleanup":
		return "inactive"
	case vendor == "":
		return ""
	}
	return "pending"
}
