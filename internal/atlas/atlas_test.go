package atlas_test

import (
	"errors"
	"go/build"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/atlas"
)

// tokens is one credential of every common shape, each assembled at run time
// so the source holds no string a secret scanner reads as a live credential.
func tokens() []string {
	body := func(n int) string {
		const alphabet = "Ab3dE6gH9jK2mN5pQ8sT1vW4yZ7bC0eF"
		var b strings.Builder
		for i := range n {
			b.WriteByte(alphabet[(i*7+3)%len(alphabet)])
		}
		return b.String()
	}
	return []string{
		"gh" + "p_" + body(36),
		"github" + "_pat_" + "11" + body(20) + "_" + body(59),
		"sk-" + "ant-api03-" + body(93) + "-" + body(6) + "AA",
		"AKIA" + "ABCDEFGHIJKLMNOP",
		"xo" + "xb-" + "123456789012-1234567890123-" + body(24),
		"gl" + "pat-" + body(20),
	}
}

// EVERY SET IS CLOSED AND EVERY TABLE NAMES ONLY ITS OWN MEMBERS: a kind with no
// lane is a card with no column, a relation with no pair can join nothing, and
// a fact the list does not show is a fact nobody reads.
func TestEveryKindRelationAndFactIsClosed(t *testing.T) {
	t.Parallel()
	for _, k := range atlas.Kinds {
		if !k.Lane().Valid() {
			t.Errorf("kind %s has no lane", k)
		}
	}
	for _, l := range atlas.Lanes {
		if !slices.ContainsFunc(atlas.Kinds, func(k atlas.Kind) bool { return k.Lane() == l }) {
			t.Errorf("lane %s holds no kind", l)
		}
	}
	if atlas.Kind("server").Valid() || atlas.Kind("").Lane() != "" {
		t.Error("a kind outside the set is valid")
	}
	for _, r := range atlas.Relations {
		joins := false
		for _, from := range atlas.Kinds {
			for _, to := range atlas.Kinds {
				joins = joins || r.Joins(from, to)
			}
			for _, ext := range atlas.Externals {
				joins = joins || r.Names(from, ext)
			}
		}
		if !joins {
			t.Errorf("relation %s joins nothing", r)
		}
	}
	for _, f := range atlas.Facts {
		s := f.Spec()
		if !s.Type.Valid() {
			t.Errorf("fact %s has no value type", f)
		}
		if (s.Type == atlas.TypeWord) != (len(s.Words) > 0) {
			t.Errorf("fact %s: a word fact, and only a word fact, takes a closed set of words", f)
		}
		for _, k := range s.Kinds {
			if !k.Valid() {
				t.Errorf("fact %s applies to %q, which is no kind", f, k)
			}
		}
	}
	if len(slices.Compact(slices.Clone(atlas.Facts))) != len(atlas.Facts) {
		t.Error("a fact is listed twice")
	}
	if atlas.Fact("password").Valid() || atlas.Fact("connection_string").Valid() {
		t.Error("the atlas keeps a fact that can only hold a secret")
	}
}

// A VALUE SHAPED LIKE A CREDENTIAL IS REFUSED, NEVER MASKED, and the refusal
// names the rule without echoing the value, in every type that holds text.
func TestAValueShapedLikeACredentialIsRefused(t *testing.T) {
	t.Parallel()
	for _, secret := range tokens() {
		for _, typ := range []atlas.ValueType{atlas.TypeLabel, atlas.TypeVersion, atlas.TypeAccountRef, atlas.TypeRegion} {
			_, err := atlas.Parse(typ, secret, nil)
			if !errors.Is(err, atlas.ErrCredential) {
				t.Errorf("%s holding %.6s...: %v, want ErrCredential", typ, secret, err)
			}
			if err != nil && strings.Contains(err.Error(), secret) {
				t.Errorf("%s: the refusal echoes the value", typ)
			}
		}
		if _, err := atlas.Parse(atlas.TypeURL, "https://api.acme.example/hooks/"+secret, nil); !errors.Is(err, atlas.ErrCredential) {
			t.Errorf("an address carrying %.6s... in its path: %v, want ErrCredential", secret, err)
		}
		if _, err := atlas.NewRef(atlas.KindService, secret); err == nil {
			t.Errorf("a key holding %.6s... was accepted", secret)
		}
	}
}

// AN ADDRESS IS RECORDED WITHOUT ITS CREDENTIALS, and one that carries any is
// refused rather than stripped: user information, a query and a fragment.
func TestAnAddressIsRecordedWithoutItsCredentials(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"https://deploy:hunter2@api.acme.example/v1",
		"https://api.acme.example/v1?token=abc",
		"https://api.acme.example/v1?page=2",
		"https://api.acme.example/v1#access_token=abc",
	} {
		if _, err := atlas.Parse(atlas.TypeURL, raw, nil); !errors.Is(err, atlas.ErrCredential) {
			t.Errorf("%s: %v, want ErrCredential", raw, err)
		}
	}
	for raw, want := range map[string]string{
		"HTTPS://API.Acme.Example:8443/v1/": "https://api.acme.example:8443/v1",
		"https://api.acme.example":          "https://api.acme.example",
		"wss://[2001:db8::1]:9000/socket":   "wss://[2001:db8::1]:9000/socket",
	} {
		v, err := atlas.Parse(atlas.TypeURL, raw, nil)
		if err != nil || v.Text != want {
			t.Errorf("%s: %q, %v; want %q", raw, v.Text, err, want)
		}
	}
	for _, raw := range []string{"ftp://files.acme.example", "mailto:ops@acme.example", "https://", "api.acme.example"} {
		if _, err := atlas.Parse(atlas.TypeURL, raw, nil); !errors.Is(err, atlas.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", raw, err)
		}
	}
}

// AN ADDRESS NO PUBLIC NETWORK ROUTES IS KEPT AND MARKED, by the engine's one
// reading of a private host.
func TestAnInternalAddressIsMarked(t *testing.T) {
	t.Parallel()
	for raw, private := range map[string]bool{
		"10.0.0.5": true, "192.168.1.10": true, "db": true, "db.internal": true, "printer.local": true,
		"9.9.9.9": false, "api.acme.example": false,
	} {
		v, err := atlas.Parse(atlas.TypeAddress, raw, nil)
		if err != nil || v.Private != private {
			t.Errorf("%s: private %v, %v; want %v", raw, v.Private, err, private)
		}
	}
}

// AN ENTRY'S ID IS ITS KIND AND KEY, and nothing else: two writers that meet
// one zone name one entry, a different kind is a different entry, and the id
// is UUID-shaped, as every engine id is. The pinned id holds the namespace:
// changing it would change every id ever recorded.
func TestAnEntrysIDIsItsKindAndKey(t *testing.T) {
	t.Parallel()
	a := atlas.MustRef(atlas.KindDomain, "ACME.example.")
	b := atlas.MustRef(atlas.KindDomain, "acme.example")
	if a != b || a.ID() != b.ID() {
		t.Fatalf("two spellings of one zone are two entries: %v, %v", a, b)
	}
	if atlas.MustRef(atlas.KindHostname, "acme.example").ID() == a.ID() {
		t.Error("a host name and a zone of one name share an id")
	}
	if a.ID().Version() != 8 {
		t.Errorf("version %d, want the name-based version 8", a.ID().Version())
	}
	if got, want := a.ID(), uuid.MustParse("c73bcd12-c918-828a-a97e-1d26de702846"); got != want {
		t.Errorf("acme.example is %s, pinned %s: the namespace or the hash moved", got, want)
	}
}

// A KEY IS CANONICAL: a forge address, its .git spelling and a mirror's source
// reduce to one repository key, so the three meet.
func TestARepositoryIsKnownByOneKeyWhereverItIsNamed(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"https://github.com/Acme/API.git", "https://github.com/acme/api", "https://github.com:443/acme/api/",
	} {
		key, err := atlas.RepositoryKeyOf(raw)
		if err != nil || key != "github.com/acme/api" {
			t.Errorf("%s: %q, %v", raw, key, err)
		}
	}
	if _, err := atlas.NewRef(atlas.KindRepository, "gitlet/api"); err != nil {
		t.Error(err)
	}
	if _, err := atlas.NewRef(atlas.KindRepository, "gitlet/../etc"); err == nil {
		t.Error("a Gitlet key that is no repository name was accepted")
	}
}

// NO CONFIDENCE IS CHOSEN BY THE CALLER: a claim has no confidence field, and
// what it reads as is a function of its source and the evidence the engine
// checked.
func TestNoConfidenceIsChosenByTheCaller(t *testing.T) {
	t.Parallel()
	for i := range reflect.TypeFor[atlas.Claim]().NumField() {
		if f := reflect.TypeFor[atlas.Claim]().Field(i); strings.Contains(strings.ToLower(f.Name), "confidence") {
			t.Fatalf("Claim.%s: a writer can choose its confidence", f.Name)
		}
	}
	for src, want := range map[atlas.Source]atlas.Word{
		atlas.SettingsSource("r1"):                             atlas.WordConfirmed,
		atlas.RecordSource(atlas.RecordGitlet):                 atlas.WordConfirmed,
		atlas.PersonSource("ada"):                              atlas.WordConfirmed,
		atlas.IntegrationSource("cloudflare"):                  atlas.WordConfirmed,
		atlas.AgentSource("sre", "t1", atlas.EvidenceCall):     atlas.WordLikely,
		atlas.AgentSource("sre", "t1", atlas.EvidenceFile):     atlas.WordLikely,
		atlas.AgentSource("sre", "t1", atlas.EvidenceNone):     atlas.WordUnconfirmed,
		atlas.AgentSource("sre", "t1", atlas.Evidence("mine")): atlas.WordUnconfirmed,
	} {
		if got := src.Confidence().Display(); got != want {
			t.Errorf("%+v reads as %s, want %s", src, got, want)
		}
	}
}

// AN AGENT NEVER DECIDES WHAT A THING MEANS: it sets no purpose, environment
// or importance and names no owner, and only a person says two entries are
// not the same or denies anything.
func TestAnAgentNeverSetsMeaning(t *testing.T) {
	t.Parallel()
	agent := atlas.AgentSource("sre", "t1", atlas.EvidenceCall)
	svc := atlas.MustRef(atlas.KindService, "checkout-api")
	for _, f := range []atlas.Fact{atlas.FactEnvironment, atlas.FactImportance, atlas.FactPurpose} {
		raw := map[atlas.Fact]string{atlas.FactEnvironment: "production", atlas.FactImportance: "critical",
			atlas.FactPurpose: "Takes payments"}[f]
		if _, err := atlas.Says(svc, f, raw, agent, now); err == nil {
			t.Errorf("an agent set %s", f)
		}
	}
	if _, err := atlas.Names(svc, atlas.RelationOwnedBy, atlas.ExternalUnit, "payments", agent, now); err == nil {
		t.Error("an agent set an owner")
	}
	other := atlas.MustRef(atlas.KindService, "billing-api")
	if _, err := atlas.Distinct(svc, other, agent, now); err == nil {
		t.Error("an agent said two entries are not the same")
	}
	c, err := atlas.Exists(svc, agent, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := atlas.Denial(c, agent, now); err == nil {
		t.Error("an agent denied a claim")
	}
}

// A RELATION JOINS ONLY WHAT ITS TABLE SAYS, and points from what relies to
// what is relied on.
func TestARelationJoinsOnlyWhatItsTableSays(t *testing.T) {
	t.Parallel()
	src := atlas.SettingsSource("r1")
	zone := atlas.MustRef(atlas.KindDomain, "acme.example")
	host := atlas.MustRef(atlas.KindHostname, "api.acme.example")
	if _, err := atlas.Relates(host, atlas.RelationInZone, zone, src, now); err != nil {
		t.Error(err)
	}
	if _, err := atlas.Relates(zone, atlas.RelationInZone, host, src, now); err == nil {
		t.Error("a zone sits in a host name")
	}
	if _, err := atlas.Names(host, atlas.RelationDocumentedIn, atlas.ExternalPage, "not-an-id", src, now); err == nil {
		t.Error("a page named by something other than its id")
	}
}

// THE ATLAS IMPORTS NO WRITE PATH: it is values and rules, read by every layer
// and written by none.
func TestTheAtlasImportsNoWritePath(t *testing.T) {
	t.Parallel()
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		for _, banned := range []string{"internal/api", "internal/store", "internal/statelog", "internal/coord",
			"internal/queue", "net/http"} {
			if strings.Contains(imp, banned) {
				t.Errorf("internal/atlas imports %s", imp)
			}
		}
	}
}
