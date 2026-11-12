package atlas_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/atlas"
)

// AN AGENT OUTSIDE AN ENTRY'S REACH LOSES ITS PRIVATE ADDRESSES, AND AN ENTRY
// WHOSE KEY IS ONE: a server it does not reach keeps its name and loses its
// private address, a host name on a private network is left out with every
// relation to it, and public values and reached entries stay whole.
func TestAnAgentOutsideAnEntrysReachLosesItsPrivateAddresses(t *testing.T) {
	t.Parallel()
	m := must(t)
	db := atlas.MustRef(atlas.KindHost, "ssh/db-1")
	web := atlas.MustRef(atlas.KindHost, "ssh/web-1")
	internal := atlas.MustRef(atlas.KindHostname, "grafana.internal")
	v := atlas.Fold([]atlas.Claim{
		m(atlas.Exists(db, settings, now)), m(atlas.Says(db, atlas.FactAddress, "10.0.0.5", settings, now)),
		m(atlas.Says(db, atlas.FactPort, "22", settings, now)),
		m(atlas.Exists(web, settings, now)), m(atlas.Says(web, atlas.FactAddress, "10.0.0.6", settings, now)),
		m(atlas.Exists(internal, settings, now)), m(atlas.Relates(internal, atlas.RelationResolvesTo, db, settings, now)),
	}, now)
	reach := func(e atlas.Entry) bool { return e.Ref == web }
	got, hidden := atlas.Within(v, reach)
	if hidden != 2 {
		t.Errorf("%d left out, want db-1's address and the private host name", hidden)
	}
	if _, ok := got.Find(internal.ID()); ok {
		t.Error("a host name on a private network reached an agent outside its reach")
	}
	if addr := factValues(entry(t, got, db), atlas.FactAddress); len(addr) != 0 {
		t.Errorf("db-1's address reached an agent outside its reach: %v", addr)
	}
	if port := factValues(entry(t, got, db), atlas.FactPort); !slices.Equal(port, []string{"22"}) {
		t.Errorf("db-1 lost a value that is no address: %v", port)
	}
	if addr := factValues(entry(t, got, web), atlas.FactAddress); !slices.Equal(addr, []string{"10.0.0.6"}) {
		t.Errorf("web-1, within reach, lost its address: %v", addr)
	}
	if _, ok := v.Find(internal.ID()); !ok {
		t.Error("narrowing for an agent changed the view it was given")
	}
}

func factValues(e atlas.Entry, f atlas.Fact) []string {
	var out []string
	for _, v := range e.Facts {
		if v.Fact == f {
			out = append(out, v.Value.Text)
		}
	}
	return out
}

// THE WORK NAMES AN ENTRY EXACTLY OR NOT AT ALL: a name or a key standing as a
// whole word in the text, case folded, a name before a key and the longer
// first; a word inside another, and a name too short to be one, name nothing.
func TestTheWorkNamesAnEntryExactly(t *testing.T) {
	t.Parallel()
	m := must(t)
	api := atlas.MustRef(atlas.KindService, "checkout-api")
	host := atlas.MustRef(atlas.KindHostname, "api.acme.example")
	db := atlas.MustRef(atlas.KindHost, "ssh/db")
	v := atlas.Fold([]atlas.Claim{
		m(atlas.Exists(api, ada, now)), m(atlas.Says(api, atlas.FactName, "Checkout", ada, now)),
		m(atlas.Exists(host, settings, now)),
		m(atlas.Exists(db, settings, now)), m(atlas.Says(db, atlas.FactName, "db", settings, now)),
	}, now)
	keys := func(text string) []string {
		var out []string
		for _, e := range v.Named(text) {
			out = append(out, e.Ref.Key)
		}
		return out
	}
	if got := keys("The checkout flow 500s behind API.acme.example since the db restart."); !slices.Equal(got,
		[]string{"api.acme.example", "checkout-api"}) {
		t.Errorf("named %v", got)
	}
	if got := keys("checkouts and xapi.acme.example.org"); len(got) != 0 {
		t.Errorf("a word inside another named %v", got)
	}
}
