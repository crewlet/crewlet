package atlas_test

import (
	"encoding/json"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
)

var now = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

func must(t *testing.T) func(atlas.Claim, error) atlas.Claim {
	return func(c atlas.Claim, err error) atlas.Claim {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
}

func entry(t *testing.T, v atlas.View, r atlas.Ref) atlas.Entry {
	t.Helper()
	e, ok := v.Find(r.ID())
	if !ok {
		t.Fatalf("%s is not in the atlas", r)
	}
	return e
}

func fact(e atlas.Entry, f atlas.Fact) (atlas.FactView, bool) {
	for _, v := range e.Facts {
		if v.Fact == f {
			return v, true
		}
	}
	return atlas.FactView{}, false
}

var (
	settings = atlas.SettingsSource("r7")
	gitlet   = atlas.RecordSource(atlas.RecordGitlet)
	aws      = atlas.IntegrationSource("aws")
	ada      = atlas.PersonSource("ada")
	sre      = atlas.AgentSource("sre", "turn-1", atlas.EvidenceCall)
)

// THE FOLD IS A PURE FUNCTION OF ITS CLAIMS: the same claims in any order give
// the same view, so every node answers alike.
func TestTheFoldIsAPureFunctionOfItsClaims(t *testing.T) {
	t.Parallel()
	m := must(t)
	gh := atlas.MustRef(atlas.KindRepository, "github.com/acme/api")
	mirror := atlas.MustRef(atlas.KindRepository, "gitlet/api")
	web := atlas.MustRef(atlas.KindHost, "ssh/web-1")
	cloud := atlas.MustRef(atlas.KindHost, "aws/123456789012/eu-west-1/i-0abc")
	claims := []atlas.Claim{
		m(atlas.Exists(gh, settings, now)),
		m(atlas.Exists(mirror, gitlet, now)),
		m(atlas.Says(mirror, atlas.FactPullMirrorOf, "https://github.com/acme/api.git", settings, now)),
		m(atlas.Says(mirror, atlas.FactDefaultBranch, "main", gitlet, now)),
		m(atlas.Says(mirror, atlas.FactDefaultBranch, "trunk", settings, now)),
		m(atlas.Exists(web, settings, now)),
		m(atlas.Says(web, atlas.FactAddress, "9.9.9.9", settings, now)),
		m(atlas.Exists(cloud, aws, now.Add(-time.Hour))),
		m(atlas.Says(cloud, atlas.FactAddress, "9.9.9.9", aws, now.Add(-time.Hour))),
		m(atlas.Says(cloud, atlas.FactEnvironment, "production", aws, now.Add(-time.Hour))),
		m(atlas.Says(web, atlas.FactName, "Web one", ada, now.Add(-2*time.Hour))),
		m(atlas.Says(web, atlas.FactName, "web-1", sre, now.Add(-time.Minute))),
	}
	want, _ := json.Marshal(atlas.Fold(claims, now))
	rng := rand.New(rand.NewPCG(1, 2))
	for range 50 {
		rng.Shuffle(len(claims), func(i, j int) { claims[i], claims[j] = claims[j], claims[i] })
		if got, _ := json.Marshal(atlas.Fold(claims, now)); string(got) != string(want) {
			t.Fatalf("the order of the claims changed the atlas:\n%s\nwant\n%s", got, want)
		}
	}
}

// A PERSON OUTRANKS EVERY INFERENCE, and a newer report that contradicts them
// is shown beside their value, never over it.
func TestAPersonOutranksEveryInference(t *testing.T) {
	t.Parallel()
	m := must(t)
	db := atlas.MustRef(atlas.KindDatastore, "on/ssh/db-1/postgres/5432")
	base := []atlas.Claim{
		m(atlas.Exists(db, ada, now.Add(-48*time.Hour))),
		m(atlas.Says(db, atlas.FactEngineVersion, "16", ada, now.Add(-24*time.Hour))),
		m(atlas.Says(db, atlas.FactEngineVersion, "15", sre, now.Add(-48*time.Hour))),
	}
	got, _ := fact(entry(t, atlas.Fold(base, now), db), atlas.FactEngineVersion)
	if got.Value.Text != "16" || got.Standing != atlas.StandingSettled || len(got.Others) != 1 {
		t.Fatalf("a person's value over an older observation: %+v", got)
	}
	newer := append(base, m(atlas.Says(db, atlas.FactEngineVersion, "17", sre, now.Add(-time.Hour))))
	e := entry(t, atlas.Fold(newer, now), db)
	got, _ = fact(e, atlas.FactEngineVersion)
	if got.Value.Text != "16" || got.Source.Kind != atlas.SourcePerson {
		t.Errorf("a newer observation overwrote a person's value: %+v", got)
	}
	if got.Standing != atlas.StandingContested || !e.Review {
		t.Errorf("a newer observation that contradicts a person is %s, review %v; want contested", got.Standing, e.Review)
	}
	if len(got.Others) != 1 || got.Others[0].Value.Text != "17" {
		t.Errorf("the contradicting value is not shown beside the person's: %+v", got.Others)
	}
}

// TWO SOURCES THE ENGINE TRUSTS ALIKE THAT DISAGREE ARE SHOWN, NOT SETTLED.
func TestTwoSourcesThatDisagreeAreShownNotSettled(t *testing.T) {
	t.Parallel()
	m := must(t)
	repo := atlas.MustRef(atlas.KindRepository, "gitlet/api")
	e := entry(t, atlas.Fold([]atlas.Claim{
		m(atlas.Exists(repo, gitlet, now)),
		m(atlas.Says(repo, atlas.FactDefaultBranch, "main", gitlet, now)),
		m(atlas.Says(repo, atlas.FactDefaultBranch, "trunk", settings, now)),
	}, now), repo)
	got, _ := fact(e, atlas.FactDefaultBranch)
	if got.Standing != atlas.StandingDisputed || !e.Review || len(got.Others) != 1 {
		t.Errorf("a disagreement between the settings and Gitlet: %+v, review %v", got, e.Review)
	}
}

// AN EXACT KEY MAKES TWO ENTRIES ONE: a Gitlet mirror and the forge repository
// it mirrors are one repository, found by either id.
func TestAnExactKeyMergesTwoEntries(t *testing.T) {
	t.Parallel()
	m := must(t)
	gh := atlas.MustRef(atlas.KindRepository, "github.com/acme/api")
	mirror := atlas.MustRef(atlas.KindRepository, "gitlet/api")
	v := atlas.Fold([]atlas.Claim{
		m(atlas.Exists(gh, settings, now)),
		m(atlas.Exists(mirror, gitlet, now)),
		m(atlas.Says(mirror, atlas.FactPullMirrorOf, "https://github.com/acme/api.git", settings, now)),
		m(atlas.Says(mirror, atlas.FactDefaultBranch, "main", gitlet, now)),
	}, now)
	if len(v.Entries) != 1 {
		t.Fatalf("%d entries, want the mirror and its source as one", len(v.Entries))
	}
	a, b := entry(t, v, gh), entry(t, v, mirror)
	if a.ID != b.ID || len(a.Members) != 2 {
		t.Errorf("the merged entry: %+v", a)
	}
	if _, ok := fact(a, atlas.FactDefaultBranch); !ok {
		t.Error("the merged entry lost a member's fact")
	}
}

// A SHARED PUBLIC ADDRESS NEVER MERGES: one source naming two servers at one
// address is a load balancer or a NAT, so the address is no identity, for
// those two or for any other source's server at it.
func TestASharedPublicAddressNeverMerges(t *testing.T) {
	t.Parallel()
	m := must(t)
	web1 := atlas.MustRef(atlas.KindHost, "ssh/web-1")
	web2 := atlas.MustRef(atlas.KindHost, "ssh/web-2")
	cloud := atlas.MustRef(atlas.KindHost, "aws/123456789012/eu-west-1/i-0abc")
	claims := []atlas.Claim{
		m(atlas.Exists(web1, settings, now)), m(atlas.Says(web1, atlas.FactAddress, "9.9.9.9", settings, now)),
		m(atlas.Exists(web2, settings, now)), m(atlas.Says(web2, atlas.FactAddress, "9.9.9.9", settings, now)),
		m(atlas.Exists(cloud, aws, now)), m(atlas.Says(cloud, atlas.FactAddress, "9.9.9.9", aws, now)),
	}
	if v := atlas.Fold(claims, now); len(v.Entries) != 3 {
		t.Errorf("%d entries from three servers behind one shared address, want 3", len(v.Entries))
	}
	if v := atlas.Fold(claims[2:], now); len(v.Entries) != 1 {
		t.Errorf("%d entries from one server seen by two sources at its own address, want 1", len(v.Entries))
	}
	private := []atlas.Claim{
		m(atlas.Exists(web1, settings, now)), m(atlas.Says(web1, atlas.FactAddress, "10.0.0.5", settings, now)),
		m(atlas.Exists(cloud, aws, now)), m(atlas.Says(cloud, atlas.FactAddress, "10.0.0.5", aws, now)),
	}
	if v := atlas.Fold(private, now); len(v.Entries) != 2 {
		t.Errorf("a private address merged two servers, which may sit on two networks")
	}
}

// A PERSON'S "NOT THE SAME" ALWAYS WINS over a shared key, and taking it back
// merges the two again.
func TestAPersonsNotTheSameAlwaysWins(t *testing.T) {
	t.Parallel()
	m := must(t)
	web := atlas.MustRef(atlas.KindHost, "ssh/web-1")
	cloud := atlas.MustRef(atlas.KindHost, "aws/123456789012/eu-west-1/i-0abc")
	claims := []atlas.Claim{
		m(atlas.Exists(web, settings, now)), m(atlas.Says(web, atlas.FactAddress, "9.9.9.9", settings, now)),
		m(atlas.Exists(cloud, aws, now)), m(atlas.Says(cloud, atlas.FactAddress, "9.9.9.9", aws, now)),
	}
	apart := m(atlas.Distinct(cloud, web, ada, now.Add(-time.Hour)))
	if v := atlas.Fold(append(claims, apart), now); len(v.Entries) != 2 {
		t.Errorf("%d entries after a person said they are not the same, want 2", len(v.Entries))
	}
	undo := m(atlas.Denial(apart, ada, now.Add(-time.Minute)))
	if v := atlas.Fold(append(claims, apart, undo), now); len(v.Entries) != 1 {
		t.Errorf("%d entries after the person took it back, want 1", len(v.Entries))
	}
}

// AN AGENT'S CLAIM NEVER MERGES ENTRIES: a key it reports could be a value a
// prompt put in its mouth.
func TestAnAgentsClaimNeverMergesEntries(t *testing.T) {
	t.Parallel()
	m := must(t)
	web := atlas.MustRef(atlas.KindHost, "ssh/web-1")
	other := atlas.MustRef(atlas.KindHost, "addr/203.0.113.10")
	v := atlas.Fold([]atlas.Claim{
		m(atlas.Exists(web, settings, now)), m(atlas.Says(web, atlas.FactAddress, "9.9.9.9", settings, now)),
		m(atlas.Exists(other, sre, now)), m(atlas.Says(other, atlas.FactAddress, "9.9.9.9", sre, now)),
	}, now)
	if len(v.Entries) != 2 {
		t.Errorf("an agent's address merged its entry into a declared server")
	}
}

// A FACT OF MEANING IS A SUGGESTION UNTIL A PERSON SETS IT.
func TestAFactOfMeaningIsOnlySuggestedByAnIntegration(t *testing.T) {
	t.Parallel()
	m := must(t)
	host := atlas.MustRef(atlas.KindHost, "aws/123456789012/eu-west-1/i-0abc")
	claims := []atlas.Claim{
		m(atlas.Exists(host, aws, now)),
		m(atlas.Says(host, atlas.FactEnvironment, "staging", aws, now)),
	}
	got, _ := fact(entry(t, atlas.Fold(claims, now), host), atlas.FactEnvironment)
	if !got.Suggested || got.Value.Text != "staging" {
		t.Errorf("an integration's environment: %+v, want a suggestion", got)
	}
	claims = append(claims, m(atlas.Says(host, atlas.FactEnvironment, "production", ada, now.Add(-time.Hour))))
	got, _ = fact(entry(t, atlas.Fold(claims, now), host), atlas.FactEnvironment)
	if got.Suggested || got.Value.Text != "production" || got.Standing != atlas.StandingSettled {
		t.Errorf("a person's environment over an integration's suggestion: %+v", got)
	}
}

// A RETIRED ENTRY STAYS, RETIRED: it leaves the map and stays in history, and
// its state says so.
func TestARetiredEntryIsKeptAsRetired(t *testing.T) {
	t.Parallel()
	m := must(t)
	svc := atlas.MustRef(atlas.KindService, "legacy-api")
	exists := m(atlas.Exists(svc, sre, now.Add(-time.Hour)))
	v := atlas.Fold([]atlas.Claim{exists, m(atlas.Denial(exists, ada, now))}, now)
	if e := entry(t, v, svc); e.State != atlas.StateRetired {
		t.Errorf("a service a person retired is %s", e.State)
	}
}

// AN AGENT'S CLAIM NOBODY ELSE SUPPORTS GOES STALE, THEN LAPSES.
func TestAnAgentsClaimGoesStaleThenLapses(t *testing.T) {
	t.Parallel()
	m := must(t)
	svc := atlas.MustRef(atlas.KindService, "checkout-api")
	for age, want := range map[time.Duration]atlas.State{
		10 * 24 * time.Hour:  atlas.StateDiscovered,
		40 * 24 * time.Hour:  atlas.StateStale,
		200 * 24 * time.Hour: "",
	} {
		v := atlas.Fold([]atlas.Claim{m(atlas.Exists(svc, sre, now.Add(-age)))}, now)
		var got atlas.State
		if e, ok := v.Find(svc.ID()); ok {
			got = e.State
		}
		if got != want {
			t.Errorf("an agent's service seen %s ago is %q, want %q", age, got, want)
		}
	}
}

// A CLAIM NO WRITER MAY MAKE IS SKIPPED AND NAMED, never folded and never
// echoed: a hand-built claim carrying a credential in a label is refused at
// the fold as at the writer.
func TestAClaimNoWriterMayMakeIsSkippedAndNamed(t *testing.T) {
	t.Parallel()
	svc := atlas.MustRef(atlas.KindService, "checkout-api")
	secret := tokens()[0]
	bad := atlas.Claim{Subject: svc, Facet: atlas.Facet{Kind: atlas.FacetFact, Fact: atlas.FactName},
		Value: atlas.Value{Type: atlas.TypeLabel, Text: secret}, Source: ada, ObservedAt: now}
	v := atlas.Fold([]atlas.Claim{bad}, now)
	if len(v.Entries) != 0 || len(v.Skipped) != 1 || v.Skipped[0].Subject != svc {
		t.Fatalf("the refused claim: %+v", v)
	}
	if out, _ := json.Marshal(v); strings.Contains(string(out), secret) {
		t.Error("the refused value reached the view")
	}
	keyed := atlas.Claim{Subject: atlas.Ref{Kind: atlas.KindService, Key: secret},
		Facet: atlas.Facet{Kind: atlas.FacetExists}, Source: ada, ObservedAt: now}
	v = atlas.Fold([]atlas.Claim{keyed}, now)
	if out, _ := json.Marshal(v); len(v.Skipped) != 1 || strings.Contains(string(out), secret) {
		t.Errorf("a refused key was repeated in the view: %s", out)
	}
}

// AN EDGE TO AN ENTRY NOTHING CLAIMS IS DROPPED, never drawn to nowhere.
func TestAnEdgeToAnEntryNothingClaimsIsDropped(t *testing.T) {
	t.Parallel()
	m := must(t)
	host := atlas.MustRef(atlas.KindHostname, "api.acme.example")
	zone := atlas.MustRef(atlas.KindDomain, "acme.example")
	v := atlas.Fold([]atlas.Claim{
		m(atlas.Exists(host, settings, now)),
		m(atlas.Relates(host, atlas.RelationInZone, zone, settings, now)),
	}, now)
	if e := entry(t, v, host); len(e.Relations) != 0 {
		t.Errorf("an edge to a zone nothing claims: %+v", e.Relations)
	}
}
