package cloudflare_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/cloudflare"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
)

// atlasNow is the instant every atlas read here is made at.
var atlasNow = time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC)

// txtCanary sits in a TXT record's content, which the read never decodes.
const txtCanary = "atlas-canary-txt-content"

// atlasFake is the account as the atlas reads it: a name, one zone with
// records of every kind the read tells apart, and one universal pack.
func atlasFake() *fakeCloudflare {
	f := newFakeCloudflare()
	f.accountName = "Acme Inc"
	f.records = map[string][]map[string]any{
		zoneExampleID: {
			{"id": "rec-api", "type": "A", "name": "api.example.com", "content": "9.9.9.9", "proxied": true,
				"comment": "the api", "tags": []string{"env:prod"}},
			{"id": "rec-api6", "type": "AAAA", "name": "api.example.com", "content": "2620:fe::fe", "proxied": true},
			{"id": "rec-www", "type": "CNAME", "name": "www.example.com", "content": "api.example.com", "proxied": true},
			{"id": "rec-docs", "type": "CNAME", "name": "docs.example.com", "content": "acme.pages.dev", "proxied": false},
			{"id": "rec-mail", "type": "MX", "name": "example.com", "content": "mail.example.com", "priority": 10},
			{"id": "rec-txt", "type": "TXT", "name": "_dmarc.example.com", "content": "v=DMARC1; p=none; " + txtCanary},
			{"id": "rec-apex", "type": "A", "name": "example.com", "content": "10.0.0.5", "proxied": false},
		},
	}
	f.packs = map[string][]map[string]any{
		zoneExampleID: {{
			"id": "3822ff90-ea29-44df-9e55-21300bb9419b", "type": "universal", "status": "active",
			"hosts": []string{"example.com", "*.example.com"}, "primary_certificate": "cert-1",
			"certificates": []map[string]any{
				{"id": "cert-1", "issuer": "GoogleTrustServices", "expires_on": "2027-01-15T00:00:00Z"},
				{"id": "cert-2", "issuer": "LetsEncrypt", "expires_on": "2026-12-01T00:00:00Z"},
			},
		}},
	}
	return f
}

// atlasInput is the read's input over f: the company's block, its credential
// resolving to the fake's provisioner token.
func atlasInput(t *testing.T, f *fakeCloudflare) collect.Input {
	t.Helper()
	company := &config.Company{Name: "Acme"}
	company.Integrations.Cloudflare = &config.Cloudflare{Enabled: true, Provisioning: &config.CloudflareProvisioning{
		AccountID: testAccount, APIToken: "${CF_TOKEN}", Zones: []string{zoneExampleName},
	}}
	return collect.Input{Config: company, Env: config.NewResolver(config.MapSource{"CF_TOKEN": provisioner}),
		Now: atlasNow, HTTP: f.server(t)}
}

func readAtlas(t *testing.T, f *fakeCloudflare) collect.Read {
	t.Helper()
	src, ok := collect.Lookup(integration.KindCloudflare)
	if !ok {
		t.Fatal("no Cloudflare source is registered")
	}
	read, err := src.Collect(t.Context(), atlasInput(t, f))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	f.onlyCloudflare(t)
	return read
}

func find(t *testing.T, v atlas.View, k atlas.Kind, key string) atlas.Entry {
	t.Helper()
	e, ok := v.Find(atlas.MustRef(k, key).ID())
	if !ok {
		t.Fatalf("%s:%s is not in the read", k, key)
	}
	return e
}

// related is the keys of the entries e relates to through r, sorted.
func related(v atlas.View, e atlas.Entry, r atlas.Relation) []string {
	var out []string
	for _, ed := range e.Relations {
		if ed.Relation != r {
			continue
		}
		if to, ok := v.Find(ed.To); ok {
			out = append(out, to.Ref.Key)
		}
	}
	slices.Sort(out)
	return out
}

func factOf(e atlas.Entry, f atlas.Fact) []string {
	var out []string
	for _, fv := range e.Facts {
		if fv.Fact == f {
			out = append(out, fv.Value.Text)
		}
	}
	slices.Sort(out)
	return out
}

// THE READ CLAIMS EVERY NAME, WHAT IT POINTS AT, AND EVERY CERTIFICATE: the
// account's name, each zone, each A, AAAA and CNAME record as a name in its
// zone resolving to a server or a name and fronted by Cloudflare where
// proxied, and each pack with what it covers, who issued it, when it lapses
// and which names it secures. An MX or TXT record names nothing, and a TXT
// record's content never reaches the read.
func TestTheAtlasReadsEveryNameAndCertificate(t *testing.T) {
	t.Parallel()
	f := atlasFake()
	read := readAtlas(t, f)
	out, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), txtCanary) || strings.Contains(string(out), "the api") ||
		strings.Contains(string(out), "env:prod") || strings.Contains(string(out), "mail.example.com") {
		t.Fatalf("a TXT record's content, a comment, a tag or an MX target reached the read: %s", out)
	}
	if len(read.Withheld) != 0 {
		t.Errorf("the read withheld %+v", read.Withheld)
	}
	// One call for the account, one page of zones, and a page of records
	// and a page of packs for each of the fake's three zones.
	if read.Calls != 8 {
		t.Errorf("the read made %d calls, want 8", read.Calls)
	}
	v := atlas.Fold(read.Claims, atlasNow)
	account := find(t, v, atlas.KindAccount, "cloudflare/"+testAccount)
	if got := factOf(account, atlas.FactAccountName); !slices.Equal(got, []string{"Acme Inc"}) {
		t.Errorf("the account's name: %v", got)
	}
	for _, zone := range []string{zoneExampleName, zoneOtherName, "example.net"} {
		z := find(t, v, atlas.KindDomain, zone)
		if related(v, z, atlas.RelationServedBy)[0] != account.Ref.Key || related(v, z, atlas.RelationBelongsTo)[0] != account.Ref.Key {
			t.Errorf("%s is served by %v and belongs to %v", zone, related(v, z, atlas.RelationServedBy),
				related(v, z, atlas.RelationBelongsTo))
		}
	}
	api := find(t, v, atlas.KindHostname, "api.example.com")
	if got := related(v, api, atlas.RelationResolvesTo); !slices.Equal(got, []string{"addr/2620:fe::fe", "addr/9.9.9.9"}) {
		t.Errorf("api resolves to %v", got)
	}
	if got := related(v, api, atlas.RelationInZone); !slices.Equal(got, []string{zoneExampleName}) {
		t.Errorf("api sits in %v", got)
	}
	if got := related(v, api, atlas.RelationFrontedBy); !slices.Equal(got, []string{account.Ref.Key}) {
		t.Errorf("a proxied name is fronted by %v", got)
	}
	cert := "cloudflare/3822ff90-ea29-44df-9e55-21300bb9419b"
	if got := related(v, api, atlas.RelationSecuredBy); !slices.Equal(got, []string{cert}) {
		t.Errorf("api is secured by %v", got)
	}
	origin := find(t, v, atlas.KindHost, "addr/9.9.9.9")
	if got := factOf(origin, atlas.FactAddress); !slices.Equal(got, []string{"9.9.9.9"}) || origin.Facts[0].Value.Private {
		t.Errorf("the origin's address: %v, private %v", got, origin.Facts[0].Value.Private)
	}
	apexOrigin := find(t, v, atlas.KindHost, "addr/10.0.0.5")
	if !apexOrigin.Facts[0].Value.Private {
		t.Error("an origin on a private network is not marked private")
	}
	www := find(t, v, atlas.KindHostname, "www.example.com")
	if got := related(v, www, atlas.RelationResolvesTo); !slices.Equal(got, []string{"api.example.com"}) {
		t.Errorf("www resolves to %v, want the name its CNAME points at", got)
	}
	docs := find(t, v, atlas.KindHostname, "docs.example.com")
	if got := related(v, docs, atlas.RelationFrontedBy); len(got) != 0 {
		t.Errorf("an unproxied name is fronted by %v", got)
	}
	outside := find(t, v, atlas.KindHostname, "acme.pages.dev")
	if got := related(v, outside, atlas.RelationInZone); len(got) != 0 {
		t.Errorf("a name outside the account's zones sits in %v", got)
	}
	apex := find(t, v, atlas.KindHostname, "example.com")
	if got := related(v, apex, atlas.RelationSecuredBy); !slices.Equal(got, []string{cert}) {
		t.Errorf("the apex is secured by %v", got)
	}
	c := find(t, v, atlas.KindCertificate, cert)
	if got := factOf(c, atlas.FactCovers); !slices.Equal(got, []string{"*.example.com", "example.com"}) {
		t.Errorf("the pack covers %v", got)
	}
	if got := factOf(c, atlas.FactIssuer); !slices.Equal(got, []string{"GoogleTrustServices"}) {
		t.Errorf("the pack's issuer: %v, want the primary certificate's", got)
	}
	if got := factOf(c, atlas.FactExpires); !slices.Equal(got, []string{"2027-01-15T00:00:00Z"}) {
		t.Errorf("the pack lapses %v", got)
	}
	if got := factOf(c, atlas.FactValidity); !slices.Equal(got, []string{"active"}) {
		t.Errorf("the pack's validity: %v", got)
	}
	if got := factOf(c, atlas.FactCertificateType); !slices.Equal(got, []string{"universal"}) {
		t.Errorf("the pack's type: %v", got)
	}
	if got := related(v, c, atlas.RelationInZone); !slices.Equal(got, []string{zoneExampleName}) {
		t.Errorf("the pack sits in %v", got)
	}
	for _, e := range v.Entries {
		if e.Ref.Key == "_dmarc.example.com" || e.Ref.Key == "mail.example.com" {
			t.Errorf("%s, which only an MX or TXT record names, is in the read", e.Ref)
		}
	}
}

// NO CREDENTIAL SHAPE REACHES THE ATLAS FROM CLOUDFLARE: every shape the
// redaction pass knows, planted in every field the read maps and in the ones
// it never reads, leaves no piece of itself in the read, and what the mapped
// fields refused is counted.
//
// Mutation: map a record's comment or a TXT record's content into a claim,
// or skip the builder's grammar on a value, and a shape reaches the read.
func TestNoCredentialShapeReachesTheAtlasFromCloudflare(t *testing.T) {
	t.Parallel()
	for _, shape := range redacttest.Shapes() {
		f := newFakeCloudflare()
		f.accountName = shape.Sample
		f.zones = []cloudflare.Zone{
			{ID: zoneExampleID, Name: zoneExampleName, Status: "active"},
			{ID: zoneOtherID, Name: shape.Sample, Status: "active"},
		}
		f.records = map[string][]map[string]any{zoneExampleID: {
			{"id": "rec-1", "type": "A", "name": shape.Sample, "content": "9.9.9.9", "proxied": true},
			{"id": "rec-2", "type": "A", "name": "a.example.com", "content": shape.Sample, "comment": shape.Sample},
			{"id": "rec-3", "type": "CNAME", "name": "b.example.com", "content": shape.Sample},
			{"id": "rec-4", "type": "TXT", "name": "c.example.com", "content": shape.Sample},
			{"id": shape.Sample, "type": "A", "name": "d.example.com", "content": "9.9.9.9",
				"tags": []string{shape.Sample}},
		}}
		f.packs = map[string][]map[string]any{zoneExampleID: {{
			"id": shape.Sample, "type": shape.Sample, "status": shape.Sample, "hosts": []string{shape.Sample},
			"primary_certificate": "cert-1",
			"certificates":        []map[string]any{{"id": "cert-1", "issuer": shape.Sample, "expires_on": shape.Sample}},
		}, {
			"id": "pack-2", "type": "universal", "status": "active", "hosts": []string{shape.Sample, "example.com"},
			"certificates": []map[string]any{{"id": "cert-2", "issuer": shape.Sample, "expires_on": "2027-01-01T00:00:00Z"}},
		}}}
		read := readAtlas(t, f)
		out, err := json.Marshal(read)
		if err != nil {
			t.Fatal(err)
		}
		if piece := shape.Sample[len(shape.Sample)/2:]; strings.Contains(string(out), piece) {
			t.Errorf("%s: a piece of the shape reached the read: %s", shape.Name, out)
		}
		if len(read.Withheld) < 6 {
			t.Errorf("%s: %d values withheld, want every planted mapped field: %+v", shape.Name,
				len(read.Withheld), read.Withheld)
		}
	}
}

// A READ THAT DID NOT FINISH IS NO READ AT ALL: a zone whose records cannot
// be read fails the whole read rather than reporting the zones before it,
// so nothing is retired on a partial listing.
func TestAPartialReadFailsWhole(t *testing.T) {
	t.Parallel()
	f := atlasFake()
	f.refuse["dns"] = 503
	src, _ := collect.Lookup(integration.KindCloudflare)
	read, err := src.Collect(t.Context(), atlasInput(t, f))
	if err == nil || len(read.Claims) != 0 {
		t.Fatalf("a read with a zone that would not list answered %d claims and %v", len(read.Claims), err)
	}
	if !strings.Contains(err.Error(), "records of") {
		t.Errorf("the fault does not say which read failed: %v", err)
	}
}

// A COMPANY THAT DOES NOT CONNECT CLOUDFLARE, OR WHOSE CREDENTIAL DOES NOT
// RESOLVE, IS NOT CONFIGURED: the duty forgets the source rather than
// recording a fault.
func TestAnUnconnectedCloudflareIsNotConfigured(t *testing.T) {
	t.Parallel()
	src, _ := collect.Lookup(integration.KindCloudflare)
	none := &config.Company{Name: "Acme"}
	if _, err := src.Collect(t.Context(), collect.Input{Config: none, Now: atlasNow}); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("no block: %v", err)
	}
	f := atlasFake()
	in := atlasInput(t, f)
	in.Env = config.NewResolver(config.MapSource{})
	if _, err := src.Collect(t.Context(), in); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("an unresolved token: %v", err)
	}
	if len(f.requests) != 0 {
		t.Error("an unconfigured read reached the vendor")
	}
}
