package advisor_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/atlas"
)

var atlasAt = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

var (
	checkout   = atlas.MustRef(atlas.KindService, "checkout-api")
	billing    = atlas.MustRef(atlas.KindService, "billing")
	intranet   = atlas.MustRef(atlas.KindService, "intranet")
	publicAPI  = atlas.MustRef(atlas.KindEndpoint, "https://api.acme.example/v1")
	billingAPI = atlas.MustRef(atlas.KindEndpoint, "https://billing.acme.example")
	privateAPI = atlas.MustRef(atlas.KindEndpoint, "https://10.0.0.7/admin")
	apiMonitor = atlas.MustRef(atlas.KindMonitor, "datadog/api-5xx")
	ordersDB   = atlas.MustRef(atlas.KindDatastore, "acme/orders-db")
)

// claims is each claim or the test's end.
func claims(t *testing.T, made ...func() (atlas.Claim, error)) []atlas.Claim {
	t.Helper()
	out := make([]atlas.Claim, 0, len(made))
	for _, m := range made {
		c, err := m()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

type claim = func() (atlas.Claim, error)

func exists(r atlas.Ref, src atlas.Source, at time.Time) claim {
	return func() (atlas.Claim, error) { return atlas.Exists(r, src, at) }
}

func relates(from atlas.Ref, rel atlas.Relation, to atlas.Ref, src atlas.Source, at time.Time) claim {
	return func() (atlas.Claim, error) { return atlas.Relates(from, rel, to, src, at) }
}

func ownedBy(r atlas.Ref, ext atlas.External, name string, src atlas.Source, at time.Time) claim {
	return func() (atlas.Claim, error) { return atlas.Names(r, atlas.RelationOwnedBy, ext, name, src, at) }
}

func says(r atlas.Ref, f atlas.Fact, value string, src atlas.Source, at time.Time) claim {
	return func() (atlas.Claim, error) { return atlas.Says(r, f, value, src, at) }
}

// acmeAtlas is acme's atlas a month in: checkout-api open to the internet,
// unowned and unwatched; billing open, watched through its address and owned
// by a seat that left; an intranet only a private address reaches, owned by a
// unit the chart holds; and a database two integrations give different
// engines, since a month ago.
func acmeAtlas(t *testing.T) atlas.View {
	t.Helper()
	ada, old := atlas.PersonSource("ada"), atlasAt.Add(-30*24*time.Hour)
	return atlas.Fold(claims(t,
		exists(checkout, ada, old), exists(publicAPI, ada, old),
		relates(checkout, atlas.RelationExposes, publicAPI, ada, old),
		exists(billing, ada, old), exists(billingAPI, ada, old),
		relates(billing, atlas.RelationExposes, billingAPI, ada, old),
		exists(apiMonitor, ada, old),
		relates(billingAPI, atlas.RelationMonitoredBy, apiMonitor, ada, old),
		ownedBy(billing, atlas.ExternalSeat, "grace", ada, old),
		exists(intranet, ada, old), exists(privateAPI, ada, old),
		relates(intranet, atlas.RelationExposes, privateAPI, ada, old),
		ownedBy(intranet, atlas.ExternalUnit, "platform", ada, old),
		exists(ordersDB, ada, old),
		says(ordersDB, atlas.FactEngine, "postgres", atlas.IntegrationSource("scaleway"), old),
		says(ordersDB, atlas.FactEngine, "mysql", atlas.IntegrationSource("datadog"), old),
	), atlasAt)
}

// acmeEvidence is acmeAtlas as a holder of the atlas app reads it, on a chart
// that holds the platform unit and no seat grace.
func acmeEvidence(t *testing.T) advisor.AtlasEvidence {
	t.Helper()
	return advisor.AtlasEvidence{Until: atlasAt, View: acmeAtlas(t), Names: true, Interval: time.Hour,
		Holds: func(o atlas.Owner) bool { return o.Kind == atlas.ExternalUnit && o.Name == "platform" }}
}

func byClass(findings []advisor.Finding) map[advisor.Class]advisor.Finding {
	out := map[advisor.Class]advisor.Finding{}
	for _, f := range findings {
		out[f.Class] = f
	}
	return out
}

func entryNames(f advisor.Finding) []string {
	out := []string{}
	for _, e := range f.Entries {
		out = append(out, e.Key)
	}
	return out
}

// EACH ATLAS CLASS NAMES EXACTLY THE ENTRIES ITS RULE READS: an unowned
// service, a service open to the internet nothing watches (one watched
// through its address is watched, one behind a private address is not open),
// an owner the chart no longer holds, and a disagreement a week old.
func TestEachAtlasClassNamesTheEntriesItsRuleReads(t *testing.T) {
	t.Parallel()
	got := byClass(advisor.DetectAtlas(acmeEvidence(t)))
	want := map[advisor.Class][]string{
		advisor.ClassAtlasUnowned:      {"checkout-api"},
		advisor.ClassAtlasUnmonitored:  {"checkout-api"},
		advisor.ClassAtlasOwnerGone:    {"billing"},
		advisor.ClassAtlasDisagreement: {"acme/orders-db"},
	}
	if len(got) != len(want) {
		t.Fatalf("classes raised = %v, want %v", got, want)
	}
	for class, entries := range want {
		f, ok := got[class]
		if !ok || !slices.Equal(entryNames(f), entries) {
			t.Errorf("%s names %v, want %v", class, entryNames(f), entries)
			continue
		}
		if f.Actor != advisor.ActorOwner || f.Source != advisor.SourceAtlas || f.Area != advisor.AreaAtlas ||
			f.Fix.Kind != advisor.FixAtlasAct || f.Fix.Act == nil || !f.Fix.Act.Asks.Valid() ||
			f.Title == "" || f.Why == "" || f.Fix.Sentence == "" {
			t.Errorf("%s is not the owner's finding with an atlas call to send: %+v", class, f)
		}
		if f.Severity == advisor.SeverityBlocks {
			t.Errorf("%s blocks: nothing in the atlas stops the company working", class)
		}
		for _, s := range []string{f.Title, f.Why, f.Fix.Sentence} {
			if strings.ContainsRune(s, '—') {
				t.Errorf("%s: %q holds an em dash", class, s)
			}
		}
	}
	if gone := got[advisor.ClassAtlasOwnerGone].Entries[0].Owner; gone == nil || gone.Name != "grace" {
		t.Errorf("the owner that left is not named: %+v", gone)
	}
}

// A READER WITHOUT THE ATLAS APP IS TOLD HOW MANY AND NEVER WHICH. The
// finding still counts, so an administrator knows there is something to fix,
// and carries no entry, no id, no key and no call (ADR-0127, ADR-0141).
//
// Mutation: drop the Names check in named, and every key below is in the answer.
func TestAReaderWithoutTheAtlasAppIsToldHowManyAndNeverWhich(t *testing.T) {
	t.Parallel()
	ev := acmeEvidence(t)
	ev.Sources = []advisor.AtlasSource{{Kind: "datadog", Name: "Datadog", LastCompleteAt: atlasAt.Add(-5 * time.Hour)}}
	holder := advisor.DetectAtlas(ev)
	ev.Names = false
	blind := advisor.DetectAtlas(ev)
	if len(blind) != len(holder) || len(blind) != 5 {
		t.Fatalf("a reader without the app is raised %d findings, a holder %d, want 5 each", len(blind), len(holder))
	}
	raw, err := json.Marshal(blind)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ev.View.Entries {
		for _, secret := range []string{e.Ref.Key, e.ID.String(), e.Name} {
			if strings.Contains(string(raw), secret) {
				t.Errorf("the answer for a reader without the atlas app holds %q", secret)
			}
		}
	}
	for i, f := range blind {
		if len(f.Entries) != 0 || len(f.Records) != 0 || f.Fix.Kind != advisor.FixNone || f.Fix.Act != nil {
			t.Errorf("%s names an entry or offers a call to a reader without the app: %+v", f.Class, f)
		}
		if f.Title != holder[i].Title || len(f.Evidence) == 0 {
			t.Errorf("%s reads differently without the app: %q, want the holder's count %q", f.Class, f.Title, holder[i].Title)
		}
	}
}

// A FINDING NAMES AT MOST FIVE ENTRIES AND COUNTS THEM ALL, the bound a
// finding's records keep.
func TestAnAtlasFindingNamesAtMostFiveEntriesAndCountsThemAll(t *testing.T) {
	t.Parallel()
	ada := atlas.PersonSource("ada")
	var made []claim
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		made = append(made, exists(atlas.MustRef(atlas.KindService, "svc-"+name), ada, atlasAt))
	}
	ev := advisor.AtlasEvidence{Until: atlasAt, View: atlas.Fold(claims(t, made...), atlasAt), Names: true}
	f := byClass(advisor.DetectAtlas(ev))[advisor.ClassAtlasUnowned]
	if len(f.Entries) != advisor.MaxAtlasEntries || f.Evidence[0] != (advisor.Fact{Key: "entries", Value: 7}) {
		t.Errorf("seven unowned services name %d and count %+v, want %d named and 7 counted",
			len(f.Entries), f.Evidence, advisor.MaxAtlasEntries)
	}
	if !strings.HasPrefix(f.Title, "7 services have") {
		t.Errorf("title = %q, want the count of every service", f.Title)
	}
}

// ONLY WHAT THE COMPANY STANDS BEHIND IS ADVISED ABOUT: a service only an
// agent reported, or one a person retired, raises nothing.
func TestAnEntryNobodyStandsBehindRaisesNothing(t *testing.T) {
	t.Parallel()
	agent := atlas.AgentSource("sre", "turn-1", atlas.EvidenceNone)
	ev := advisor.AtlasEvidence{Until: atlasAt, Names: true,
		View: atlas.Fold(claims(t, exists(checkout, agent, atlasAt)), atlasAt)}
	if got := advisor.DetectAtlas(ev); len(got) != 0 {
		t.Errorf("a service only an agent reported raises %+v", got)
	}
}

// A DISAGREEMENT IS LISTED ONCE IT HAS WAITED A WEEK AND NOT BEFORE, counted
// from the newest claim in it.
func TestADisagreementIsListedAfterAWeekAndNotBefore(t *testing.T) {
	t.Parallel()
	ada := atlas.PersonSource("ada")
	at := func(age time.Duration) []advisor.Finding {
		since := atlasAt.Add(-age)
		view := atlas.Fold(claims(t,
			exists(ordersDB, ada, since.Add(-time.Hour)),
			ownedBy(ordersDB, atlas.ExternalUnit, "platform", ada, since),
			says(ordersDB, atlas.FactEngine, "postgres", atlas.IntegrationSource("scaleway"), since.Add(-24*time.Hour)),
			says(ordersDB, atlas.FactEngine, "mysql", atlas.IntegrationSource("datadog"), since)), atlasAt)
		return advisor.DetectAtlas(advisor.AtlasEvidence{Until: atlasAt, View: view, Names: true,
			Holds: func(atlas.Owner) bool { return true }})
	}
	if got := at(advisor.AtlasDisagreementAge - time.Minute); len(got) != 0 {
		t.Errorf("a disagreement a minute short of the threshold is listed: %+v", got)
	}
	got := at(advisor.AtlasDisagreementAge + time.Minute)
	if len(got) != 1 || got[0].Class != advisor.ClassAtlasDisagreement {
		t.Errorf("a disagreement past the threshold raises %+v, want it listed", got)
	}
}

// A SOURCE IS STALE AT THREE OF ITS READS WITH NONE WHOLE, and not before; a
// source never read whole has not stopped, so its own standing speaks for it.
// The fix asks for a read of that source and nothing else.
func TestASourceIsStaleAtThreeReadsAndNotBefore(t *testing.T) {
	t.Parallel()
	ev := advisor.AtlasEvidence{Until: atlasAt, Names: true, ViewUnread: true, Interval: time.Hour,
		Sources: []advisor.AtlasSource{
			{Kind: "cloudflare", Name: "Cloudflare", LastCompleteAt: atlasAt.Add(-3*time.Hour + time.Minute)},
			{Kind: "datadog", Name: "Datadog", LastCompleteAt: atlasAt.Add(-3 * time.Hour)},
			{Kind: "github", Name: "GitHub"},
		}}
	got := advisor.DetectAtlas(ev)
	if len(got) != 1 || got[0].Class != advisor.ClassAtlasSourceStale || got[0].Subject.Name != "Datadog" {
		t.Fatalf("stale sources = %+v, want Datadog alone", got)
	}
	act := got[0].Fix.Act
	if got[0].Fix.Kind != advisor.FixAtlasAct || act == nil || act.Tool != advisor.AtlasToolRefresh ||
		act.Args["integration"] != "datadog" || act.Asks != advisor.AtlasAskNothing {
		t.Errorf("the fix is %+v, want a read of datadog asked for", got[0].Fix)
	}
	if got[0].Path != "integrations.datadog" || got[0].Severity != advisor.SeverityCosts {
		t.Errorf("finding = %+v, want it located at the integration and costing", got[0])
	}
}

// A HALF THE CALLER COULD NOT READ RAISES NOTHING AND IS NAMED, so a short
// list never reads as an atlas with nothing to fix.
func TestAnUnreadAtlasRaisesNothingAndIsNamed(t *testing.T) {
	t.Parallel()
	ev := acmeEvidence(t)
	ev.ViewUnread, ev.SourcesUnread = true, true
	ev.Sources = []advisor.AtlasSource{{Kind: "datadog", Name: "Datadog", LastCompleteAt: atlasAt.Add(-9 * time.Hour)}}
	got := advisor.WithAtlas(advisor.Answer{Findings: []advisor.Finding{}, Unread: []string{}}, ev)
	if len(got.Findings) != 0 || !slices.Equal(got.Unread, []string{advisor.UnreadAtlas, advisor.UnreadAtlasSources}) {
		t.Errorf("answer = %+v, want no finding and both halves named unread", got)
	}
}

// EVERY ATLAS CLASS IS RAISED BY SOMETHING AND LISTED, so a class added to
// one list and not the other fails here.
func TestEveryAtlasClassIsRaised(t *testing.T) {
	t.Parallel()
	ev := acmeEvidence(t)
	ev.Sources = []advisor.AtlasSource{{Kind: "datadog", Name: "Datadog", LastCompleteAt: atlasAt.Add(-9 * time.Hour)}}
	got := byClass(advisor.DetectAtlas(ev))
	for _, class := range advisor.AtlasClasses() {
		if _, ok := got[class]; !ok {
			t.Errorf("%s is listed and never raised", class)
		}
	}
	if len(got) != len(advisor.AtlasClasses()) {
		t.Errorf("raised %d classes, AtlasClasses lists %d", len(got), len(advisor.AtlasClasses()))
	}
}
