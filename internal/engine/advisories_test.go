package engine_test

import (
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/engine"
)

// advisoryCompanyDoc applies cleanly and raises two advisories: one about a
// seat — a human seat with no account on the Mattermost workspace the company
// declares (disabled, so nothing is dialled), which nobody can mention there —
// and one about no seat at all, a company day ceiling the week ceiling makes
// unable to refuse a turn. It also carries a `manages:` entry naming nobody, a
// dangling reference, which is a warning of another kind and must not be
// logged as an advisory. The embeddings block is what lets
// [advisoryRefusedDoc] be refused at build while still validating, as
// [danglingCompanyDoc]'s is.
const advisoryCompanyDoc = `
name: Acme
providers:
  llm:
    zulu:
      model: claude-sonnet-5
  embeddings:
    type: openai
    model: text-embedding-3-large
    api_key: ${K}
    dimensions: 3072
token_budget:
  day: 20000000
  week: 10000000
integrations:
  mattermost:
    enabled: false
    url: https://chat.example.com
    team: acme
roles:
  - name: Founder
    handle: founder
    kind: human
  - name: CEO
    handle: ceo
    llm: zulu
    manages: [nobody]
`

// advisoryRefusedDoc is the same company asking for a vector width the node's
// store did not open at: it validates, reaches Engine.Apply, and is refused
// there on every attempt.
var advisoryRefusedDoc = strings.Replace(advisoryCompanyDoc, "dimensions: 3072", "dimensions: 1536", 1)

// advisoryRecord is one config_advisory line as a case reads it, with which of
// the optional keys it carried.
type advisoryRecord struct {
	Level    string `json:"level"`
	Config   string `json:"config"`
	Revision string `json:"revision"`
	Epoch    int64  `json:"epoch"`
	Path     string `json:"path"`
	Detail   string `json:"detail"`
	Seat     string `json:"seat"`
	Unit     string `json:"unit"`
	HasSeat  bool   `json:"-"`
	HasUnit  bool   `json:"-"`
}

// advisoryRecords decodes every config_advisory line the reconciler wrote, in
// order.
func advisoryRecords(t *testing.T, logs *logBuffer) []advisoryRecord {
	t.Helper()
	var out []advisoryRecord
	for _, record := range logs.records(t, "config_advisory") {
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("re-encode %v: %v", record, err)
		}
		var line advisoryRecord
		if err := json.Unmarshal(raw, &line); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		_, line.HasSeat = record["seat"]
		_, line.HasUnit = record["unit"]
		out = append(out, line)
	}
	return out
}

// wantAdvisories is the line each advisory the document raises becomes when
// the epoch applies: the seat or the unit only where the warning is about
// one, so a line about neither carries neither key rather than an empty one.
func wantAdvisories(warnings []config.Warning, revision string, epoch int64) []advisoryRecord {
	out := make([]advisoryRecord, 0, len(warnings))
	for _, w := range warnings {
		out = append(out, advisoryRecord{
			Level: slog.LevelWarn.String(), Config: "company", Revision: revision,
			Epoch: epoch, Path: w.Path, Detail: w.Message,
			Seat: w.Seat, Unit: w.Unit, HasSeat: w.Seat != "", HasUnit: w.Unit != "",
		})
	}
	return out
}

// A COMPANY'S ADVISORIES ARE LOGGED WHEN A NODE APPLIES IT, once per epoch it
// applies — which is the only report a company seeded from a file ever gets,
// since a `/config` answer goes to whoever wrote and a seed is written by
// nobody. Not on every tick, not for a refused revision retried for an epoch
// this node does not serve, and again for a re-activation, because that is a
// new epoch the node rebuilds from.
//
// The advisories ONLY: the document's dangling `manages:` entry is logged by
// org_dangling_reference beside these, and a line here as well would warn
// twice about one fact.
//
// Mutation: log [config.Company.Warnings] instead and the dangling reference
// joins the lines; drop the seat and the first line's key goes missing; move
// the call off the success branch and the refused revision adds lines.
func TestCompanyAdvisoriesAreLoggedOncePerAppliedEpoch(t *testing.T) {
	t.Parallel()
	logs := &logBuffer{}
	p := planeFor(t, newEngine(t, engine.Options{
		Company: parsedCompany(t, advisoryCompanyDoc),
	}), func(o *engine.ReconcilerOptions) {
		o.Log = slog.New(slog.NewJSONHandler(logs, nil))
	})

	// THE FIXTURE'S OWN SUBJECT, named so the case cannot pass on a
	// document that raises nothing: an advisory about a seat, one about no
	// seat, and a dangling reference that is not an advisory.
	company := parsedCompany(t, advisoryCompanyDoc)
	warnings := company.AdvisoryWarnings()
	if paths := func() []string {
		var out []string
		for _, w := range warnings {
			out = append(out, w.Path+"@"+w.Seat)
		}
		return out
	}(); !slices.Equal(paths, []string{"roles[0].contact@founder", "token_budget.day@"}) {
		t.Fatalf("the fixture raises advisories %v, not the two this case is about", paths)
	}
	if len(company.ReferenceWarnings()) == 0 {
		t.Fatal("the fixture carries no dangling reference for the advisories to leave out")
	}

	first := p.activate(t.Context(), t, advisoryCompanyDoc)
	target, _, err := p.fleet.Target(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := p.recon.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	want := wantAdvisories(warnings, target.RevisionID, first)
	if got := advisoryRecords(t, logs); !slices.Equal(got, want) {
		t.Fatalf("after three ticks on one epoch, logged\n%+v\nwant\n%+v", got, want)
	}

	second := p.activate(t.Context(), t, advisoryCompanyDoc)
	target, _, err = p.fleet.Target(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	want = append(want, wantAdvisories(warnings, target.RevisionID, second)...)
	if got := advisoryRecords(t, logs); !slices.Equal(got, want) {
		t.Fatalf("after a re-activation, logged\n%+v\nwant\n%+v", got, want)
	}

	p.activate(t.Context(), t, advisoryRefusedDoc)
	for range configplane.MaxApplyAttempts {
		if err := p.recon.Tick(t.Context()); err == nil {
			t.Fatal("a revision resizing the store's vectors applied cleanly")
		}
	}
	if got := advisoryRecords(t, logs); !slices.Equal(got, want) {
		t.Errorf("a refused revision logged its advisories:\n%+v\nwant only\n%+v", got, want)
	}
}
