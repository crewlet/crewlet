package atlas_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/atlas"
)

// boundCompany is a company at the map's bound, [atlas.MapMax] entries, with
// relations of every family between them: services on servers calling
// databases and each other, built and deployed from repositories, names in
// zones resolving to servers, and everything belonging to and watched by an
// account, each with an owner and a purpose.
func boundCompany(b *testing.B) atlas.View {
	b.Helper()
	var claims []atlas.Claim
	add := func(c atlas.Claim, err error) {
		if err != nil {
			b.Fatalf("build a claim: %v", err)
		}
		claims = append(claims, c)
	}
	account := func(i int) atlas.Ref {
		return atlas.MustRef(atlas.KindAccount, fmt.Sprintf("aws/%012d", 100000000000+i%20))
	}
	zone := func(i int) atlas.Ref { return atlas.MustRef(atlas.KindDomain, fmt.Sprintf("zone-%d.example", i%20)) }
	host := func(i int) atlas.Ref { return atlas.MustRef(atlas.KindHost, fmt.Sprintf("ssh/host-%d", i%600)) }
	data := func(i int) atlas.Ref {
		return atlas.MustRef(atlas.KindDatastore, fmt.Sprintf("on/ssh/host-%d/postgres/%d", i%300, 5432+i%300))
	}
	service := func(i int) atlas.Ref { return atlas.MustRef(atlas.KindService, fmt.Sprintf("svc-%d", i%400)) }
	repo := func(i int) atlas.Ref {
		return atlas.MustRef(atlas.KindRepository, fmt.Sprintf("github.com/acme/repo-%d", i%160))
	}
	pipeline := func(i int) atlas.Ref {
		return atlas.MustRef(atlas.KindPipeline, fmt.Sprintf("github.com/acme/repo-%d#deploy", i%100))
	}
	name := func(i int) atlas.Ref {
		return atlas.MustRef(atlas.KindHostname, fmt.Sprintf("api-%d.zone-%d.example", i, i%20))
	}
	described := func(r atlas.Ref, i int) {
		add(atlas.Exists(r, ada, now))
		add(atlas.Says(r, atlas.FactPurpose, "Serves customers in every region", ada, now))
		add(atlas.Names(r, atlas.RelationOwnedBy, atlas.ExternalUnit, fmt.Sprintf("unit-%d", i%12), ada, now))
		if r.Kind != atlas.KindAccount {
			add(atlas.Relates(r, atlas.RelationBelongsTo, account(i), ada, now))
		}
	}
	for i := range 20 {
		described(account(i), i)
		described(zone(i), i)
		add(atlas.Relates(zone(i), atlas.RelationServedBy, account(i), ada, now))
	}
	for i := range 600 {
		described(host(i), i)
		add(atlas.Relates(host(i), atlas.RelationMonitoredBy, account(i+1), ada, now))
	}
	for i := range 300 {
		described(data(i), i)
		add(atlas.Relates(data(i), atlas.RelationRunsOn, host(i), ada, now))
		add(atlas.Relates(data(i), atlas.RelationMonitoredBy, account(i+1), ada, now))
	}
	for i := range 160 {
		described(repo(i), i)
	}
	for i := range 100 {
		described(pipeline(i), i)
		add(atlas.Relates(pipeline(i), atlas.RelationBuiltFrom, repo(i), ada, now))
	}
	for i := range 400 {
		s := service(i)
		described(s, i)
		add(atlas.Relates(s, atlas.RelationRunsOn, host(i), ada, now))
		add(atlas.Relates(s, atlas.RelationDependsOn, data(i), ada, now))
		add(atlas.Relates(s, atlas.RelationDependsOn, service(i+1), ada, now))
		add(atlas.Relates(s, atlas.RelationBuiltFrom, repo(i), ada, now))
		add(atlas.Relates(s, atlas.RelationDeployedBy, pipeline(i), ada, now))
		add(atlas.Relates(s, atlas.RelationMonitoredBy, account(i+1), ada, now))
		add(atlas.Relates(s, atlas.RelationFrontedBy, account(i+2), ada, now))
	}
	for i := range 400 {
		n := name(i)
		described(n, i)
		add(atlas.Relates(n, atlas.RelationResolvesTo, host(i), ada, now))
		add(atlas.Relates(n, atlas.RelationInZone, zone(i), ada, now))
		add(atlas.Relates(n, atlas.RelationFrontedBy, account(i+2), ada, now))
	}
	v := atlas.Fold(claims, now)
	if len(v.Entries) != atlas.MapMax {
		b.Fatalf("the company holds %d entries, want the map's bound of %d", len(v.Entries), atlas.MapMax)
	}
	return v
}

// BenchmarkMapAtItsBound draws the map of a company at the map's bound, at
// rest and with every relation, and encodes the answer: what the widest
// atlas_map costs the node that answers it, and how large it is on the wire.
func BenchmarkMapAtItsBound(b *testing.B) {
	v := boundCompany(b)
	for name, all := range map[string]bool{"at rest": false, "every relation": true} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				m := v.Map(atlas.Query{}, nil, all)
				body, err := json.Marshal(m)
				if err != nil {
					b.Fatal(err)
				}
				b.ReportMetric(float64(len(m.Nodes)), "nodes")
				b.ReportMetric(float64(len(m.Edges)), "edges")
				b.ReportMetric(float64(len(body))/1024, "KiB")
			}
		})
	}
}
