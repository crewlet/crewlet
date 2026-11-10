package insights

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// A RECORD OUTSIDE THE CATALOGUE IS REFUSED: whatever a record holds, Valid
// takes only the catalogue's own values, the check a sender and the relay share.
func TestARecordOutsideTheCatalogueIsRefused(t *testing.T) {
	t.Parallel()
	good := Record{schema: SchemaVersion, class: Class(advisor.ClassNoopShare), week: "2030-W02",
		version: "main", facts: map[FactKey]Label{FactShare: "35-49", FactSeats: "1"}}
	if err := good.Valid(); err != nil {
		t.Fatalf("the control record fails: %v", err)
	}
	for name, bad := range map[string]func(r Record) Record{
		"an unknown class":  func(r Record) Record { r.class = "payroll.secret"; return r },
		"another schema":    func(r Record) Record { r.schema = SchemaVersion + 1; return r },
		"a day, not a week": func(r Record) Record { r.week = "2030-01-07"; return r },
		"a commit":          func(r Record) Record { r.version = "main-0123456789ab"; return r },
		"a raw count": func(r Record) Record {
			r.facts = maps.Clone(r.facts)
			r.facts[FactShare] = "17"
			return r
		},
		"an extra fact": func(r Record) Record {
			r.facts = maps.Clone(r.facts)
			r.facts["seat"] = "payroll-agent"
			return r
		},
		"a swapped fact": func(r Record) Record {
			r.facts = map[FactKey]Label{FactShare: "35-49", "seat": "1"}
			return r
		},
	} {
		if err := bad(good).Valid(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// historyDir holds each published schema's catalogue, frozen.
const historyDir = "schema/insights-history"

// A PUBLISHED CLASS'S SHAPE IS IMMUTABLE: a class a frozen schema names keeps its
// facts, labels, floors and the evidence they read while this build's schema is
// that one, so moving a bucket takes a schema bump and a consent again.
func TestAPublishedClassShapeIsImmutable(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(sourcetree.Root(t), filepath.FromSlash(historyDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", historyDir, err)
	}
	live := map[Class][]Fact{}
	for _, e := range catalogue {
		live[e.Class] = e.Facts
	}
	frozenNow := false
	for _, entry := range entries {
		if entry.Name() == "README.md" {
			continue
		}
		name, isJSON := strings.CutSuffix(entry.Name(), ".json")
		number, err := strconv.Atoi(name)
		if !isJSON || err != nil {
			t.Errorf("%s/%s is not <schema>.json", historyDir, entry.Name())
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var frozen struct {
			Schema  int `json:"schema"`
			Classes []struct {
				Class Class `json:"class"`
				Facts []struct {
					Key    FactKey  `json:"key"`
					Values []Label  `json:"values"`
					Floors []int64  `json:"floors"`
					Reads  []string `json:"reads"`
				} `json:"facts"`
			} `json:"classes"`
		}
		if err := json.Unmarshal(raw, &frozen); err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		switch {
		case frozen.Schema != number:
			t.Errorf("%s says schema %d", entry.Name(), frozen.Schema)
		case number > SchemaVersion:
			t.Errorf("%s is frozen ahead of this build's schema %d", entry.Name(), SchemaVersion)
		case number < SchemaVersion:
			continue
		}
		frozenNow = true
		for _, c := range frozen.Classes {
			facts, ok := live[c.Class]
			if !ok || len(facts) != len(c.Facts) {
				t.Errorf("schema %d is published: %s was removed or its facts changed", number, c.Class)
				continue
			}
			for i, f := range c.Facts {
				if facts[i].Key != f.Key || !slices.Equal(facts[i].Values, f.Values) ||
					!slices.Equal(facts[i].Floors, f.Floors) || !slices.Equal(facts[i].Reads, f.Reads) {
					t.Errorf("schema %d is published: %s's %s moved from %v", number, c.Class, f.Key, f.Values)
				}
			}
		}
	}
	if !frozenNow {
		t.Errorf("%s holds no frozen copy of schema %d", historyDir, SchemaVersion)
	}
}

// THE RECORD'S WEEK IS UTC'S, whatever zone the instant came in.
func TestTheWeekIsReadInUTC(t *testing.T) {
	t.Parallel()
	late := time.Date(2030, 1, 6, 23, 30, 0, 0, time.FixedZone("west", -3*3600))
	if got := WeekOf(late); got != "2030-W02" {
		t.Errorf("a Sunday evening west of UTC is week %s, want 2030-W02 (Monday in UTC)", got)
	}
}

// NO SETTING REACHES WHERE OR FROM WHAT A RECORD IS SENT: the consent holds only
// its mode, its accepted schema and its always-sent classes, and the wiring reads
// the binary's own origin and the constant relay address.
func TestNoSettingReachesTheInsightsOrigin(t *testing.T) {
	t.Parallel()
	var fields []string
	for f := range reflect.TypeFor[config.InsightsSharing]().Fields() {
		fields = append(fields, f.Name)
	}
	if !slices.Equal(fields, []string{"Share", "Accepted", "Always"}) {
		t.Errorf("config.InsightsSharing holds %v; an address or an override in the document would let "+
			"whoever writes it send anywhere, or from a development build", fields)
	}
	wiring, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "internal", "api", "app.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"issuereport.OriginOf(version.String())", "insights.RelayURL"} {
		if !strings.Contains(string(wiring), want) {
			t.Errorf("the API's wiring does not read %s", want)
		}
	}
	if strings.Contains(string(wiring), "issuereport.Origin{") {
		t.Error("the API's wiring builds an origin by hand, which only a test may")
	}
}
