package collect_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
)

// A VALUE OR A KEY SHAPED LIKE A CREDENTIAL NEVER REACHES A READ: every shape
// the redaction pass knows, planted as a name, a value, a key and a target, is
// withheld and counted, and the read's text holds no piece of it.
//
// Mutation: skip the recordable check on a key, or the grammar's credential
// check on a value, and a shape reaches the claims.
func TestNoCredentialShapeReachesARead(t *testing.T) {
	t.Parallel()
	for _, shape := range redacttest.Shapes() {
		b := collect.NewBuilder(integration.KindCloudflare, t0)
		host, _ := b.Ref("records[1].name", atlas.KindHostname, api.Key)
		b.Says("records[1].content", host, atlas.FactURL, "https://"+api.Key+"/"+shape.Sample)
		b.Ref("zones[0].name", atlas.KindDomain, shape.Sample)
		b.Ref("records[2].name", atlas.KindHostname, shape.Sample+".acme.example")
		b.Relates("records[2].content", host, atlas.RelationResolvesTo, atlas.Ref{Kind: atlas.KindHostname,
			Key: shape.Sample + ".acme.example"})
		cert, _ := b.Ref("certificates[0].id", atlas.KindCertificate, "cloudflare/pack-1")
		b.Says("certificates[0].issuer", cert, atlas.FactIssuer, shape.Sample)
		b.Says("certificates[0].hosts[0]", cert, atlas.FactCovers, shape.Sample)
		read := b.Read()
		out, err := json.Marshal(read)
		if err != nil {
			t.Fatal(err)
		}
		if piece := shape.Sample[len(shape.Sample)/2:]; strings.Contains(string(out), piece) {
			t.Errorf("%s: a piece of the shape reached the read: %s", shape.Name, out)
		}
		if len(read.Withheld) != 6 {
			t.Errorf("%s: %d values withheld, want every planted one: %+v", shape.Name, len(read.Withheld), read.Withheld)
		}
		if len(read.Claims) != 2 {
			t.Errorf("%s: %d claims, want the name and the certificate alone", shape.Name, len(read.Claims))
		}
	}
}

// A READ HOLDS EACH SLOT ONCE: a vendor listing one thing twice reads as one
// thing, and a claim's count of calls is what the builder was told.
func TestAReadHoldsEachSlotOnce(t *testing.T) {
	t.Parallel()
	b := collect.NewBuilder(integration.KindCloudflare, t0)
	b.Ref("zones[0]", atlas.KindDomain, zone.Key)
	b.Ref("zones[1]", atlas.KindDomain, "ACME.example.")
	host, _ := b.Ref("records[0]", atlas.KindHostname, api.Key)
	b.Relates("records[0]", host, atlas.RelationInZone, zone)
	b.Relates("records[3]", host, atlas.RelationInZone, zone)
	b.Calls(4)
	b.Call()
	read := b.Read()
	if len(read.Claims) != 3 || read.Calls != 5 || len(read.Withheld) != 0 {
		t.Errorf("read = %d claims, %d calls, %d withheld; want 3, 5, 0", len(read.Claims), read.Calls, len(read.Withheld))
	}
	for _, c := range read.Claims {
		if c.Source != atlas.IntegrationSource("cloudflare") || !c.ObservedAt.Equal(t0) {
			t.Errorf("a claim speaks as %+v at %s, want the source's voice at the read's instant", c.Source, c.ObservedAt)
		}
	}
}
