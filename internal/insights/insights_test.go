package insights_test

import (
	"encoding/json"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/insights"
	"github.com/crewlet/crewlet/internal/issuereport"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/worklet"
)

// monday is in ISO week 2030-W02.
var monday = time.Date(2030, 1, 7, 9, 0, 0, 0, time.UTC)

const commit = "0123456789abcdef0123456789abcdef01234567"

// tagged and onMain are public builds; development sends nothing.
var (
	tagged      = issuereport.Origin{Build: issuereport.Build{Version: "v1.4.0", Commit: commit}}
	onMain      = issuereport.Origin{Build: issuereport.Build{Version: "main-0123456789ab", Commit: commit}}
	development = issuereport.Origin{Build: issuereport.Build{Version: "dev", Commit: commit}}
)

// kindOf names owner a person and every other handle an agent.
func kindOf(handle string) advisor.SeatKind {
	if handle == "owner" {
		return advisor.SeatPerson
	}
	return advisor.SeatAgent
}

func started(seat, wake, outcome string, turns, input, aux int64) usage.TurnLedgerRow {
	return usage.TurnLedgerRow{AgentID: "id-" + seat, Handle: seat, Wake: wake, Disposition: "started",
		Outcome: outcome, Turns: turns, Input: input, AuxInput: aux}
}

// everyPattern is evidence on which every catalogue class's detector fires.
func everyPattern() advisor.Evidence {
	return advisor.Evidence{Until: monday, Backup: advisor.Backup{Standing: true, Logs: 3},
		Work: worklet.WorkPatterns{AgentComments: 100, Answers: 20, Superseded: 2, Moot: 2,
			Overtaken: []string{"c-1"},
			Named: []worklet.NamedWithoutAsk{
				{Handle: "pm", Told: 6, Woken: 6, WokenComments: []string{"c-2"}},
				{Handle: "owner", Told: 12, Requested: 12, Comments: []string{"c-3"},
					RequestedComments: []string{"c-3"}}},
			IdleReviews: []worklet.IdleReview{{Task: "t-1", Key: "W-1", Holder: "qa"}},
			Notices:     []worklet.NoticeLoad{{Handle: "owner", Notices: 800, Addressed: 40}}},
		Ledger: []usage.TurnLedgerRow{
			started("app", "work_item:pickup", "words", 10, 100_000, 0),
			started("app", "work_item:pickup", "nothing", 10, 100_000, 0),
			started("sre", "chat:message", "moved", 1, 600_000, 500_000)}}
}

// EVERY CATALOGUE CLASS IS READ OFF ITS OWN DETECTOR'S FINDINGS: the facts it
// reads are the ones the advisor writes, so a renamed evidence key fails here
// rather than sending nothing.
func TestEveryClassIsReadOffItsDetectorsOwnFindings(t *testing.T) {
	t.Parallel()
	records := insights.Records(advisor.Detect(everyPattern(), kindOf), monday, tagged)
	var got []insights.Class
	for _, r := range records {
		got = append(got, r.Class())
		if err := r.Valid(); err != nil {
			t.Errorf("%s: %v", r.Class(), err)
		}
	}
	var want []insights.Class
	for _, e := range insights.Catalogue() {
		want = append(want, e.Class)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the detectors' findings made records of %v, want one per catalogue class %v", got, want)
	}
}

// A CATALOGUE CLASS IS AN ADVISOR EVIDENCE CLASS, SMALL AND CLOSED: at most 4
// facts of at most 6 values each and 64 combinations, every value a short
// token of a safe set, so a record is a handful of bits.
func TestEveryClassIsSmallAndClosed(t *testing.T) {
	t.Parallel()
	safe := regexp.MustCompile(`^[0-9a-z+-]{1,12}$`)
	evidence := advisor.EvidenceClasses()
	for _, e := range insights.Catalogue() {
		if !slices.Contains(evidence, advisor.Class(e.Class)) {
			t.Errorf("%s is no class the advisor raises from its evidence", e.Class)
		}
		combinations := 1
		for _, f := range e.Facts {
			combinations *= len(f.Values)
			if len(f.Values) == 0 || len(f.Values) > 6 || len(slices.Compact(slices.Clone(f.Values))) != len(f.Values) {
				t.Errorf("%s's %s holds %v, want 1 to 6 distinct values", e.Class, f.Key, f.Values)
			}
			for _, v := range f.Values {
				if !safe.MatchString(string(v)) {
					t.Errorf("%s's %s value %q is outside the safe set", e.Class, f.Key, v)
				}
			}
		}
		if len(e.Facts) > 4 || combinations > 64 {
			t.Errorf("%s has %d facts and %d combinations, want at most 4 and 64", e.Class, len(e.Facts), combinations)
		}
	}
}

// A RECORD HOLDS NO PLAIN STRING: every string in it is a named type only the
// package makes, so no field can carry what a company wrote.
func TestARecordHoldsNoPlainString(t *testing.T) {
	t.Parallel()
	var walk func(path string, typ reflect.Type)
	walk = func(path string, typ reflect.Type) {
		switch typ.Kind() {
		case reflect.String:
			if typ == reflect.TypeFor[string]() {
				t.Errorf("%s is a plain string", path)
			}
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(path+"[]", typ.Elem())
		case reflect.Map:
			walk(path+".key", typ.Key())
			walk(path+".value", typ.Elem())
		case reflect.Interface:
			t.Errorf("%s is an interface: it could hold anything", path)
		case reflect.Struct:
			for field := range typ.Fields() {
				walk(path+"."+field.Name, field.Type)
			}
		}
	}
	walk("Record", reflect.TypeFor[insights.Record]())
}

// canaries are what a company's findings carry; none may reach a record.
var canaries = []string{"Acme-Payroll-Corp", "jane.doe@acme.example", "payroll-agent", "ACME-1234",
	"U0FOUNDER", "<script>alert(1)</script>", "line one\nline two", "10.42.0.17"}

// withCanaries is f with a canary in every string a finding carries.
func withCanaries(f advisor.Finding, i int) advisor.Finding {
	c := func(n int) string { return canaries[(i+n)%len(canaries)] }
	f.Title, f.Why, f.Path, f.Area = c(0), c(1), c(2), advisor.Area(c(3))
	f.Subject = advisor.Subject{Kind: advisor.SubjectSeat, Seat: c(4), Unit: c(5), Schedule: c(6), Name: c(7)}
	f.Records = []string{c(1), c(2)}
	f.Fix.Sentence, f.Fix.Unchanged = c(3), c(4)
	return f
}

// NO FINDING TEXT REACHES A RECORD, WHATEVER ITS EVIDENCE: a finding of every
// class with company text in every field and any evidence values gives records
// that validate, carry exactly the record's fields and quote none of the text.
func FuzzNoFindingTextReachesARecord(f *testing.F) {
	f.Add(int64(5), int64(20), int64(3), int64(1_000_000))
	f.Add(int64(-1), int64(0), int64(1<<62), int64(-1<<62))
	f.Fuzz(func(t *testing.T, a, b, c, d int64) {
		var findings []advisor.Finding
		for i, found := range advisor.Detect(everyPattern(), kindOf) {
			for j := range found.Evidence {
				found.Evidence[j].Value = []int64{a, b, c, d}[(i+j)%4]
			}
			findings = append(findings, withCanaries(found, i))
		}
		for _, r := range insights.Records(findings, monday, onMain) {
			if err := r.Valid(); err != nil {
				t.Fatalf("an invalid record was made: %v", err)
			}
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			for _, canary := range canaries {
				if strings.Contains(string(raw), canary) {
					t.Fatalf("%s carries %q", raw, canary)
				}
			}
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if keys := slices.Sorted(maps.Keys(fields)); !slices.Equal(keys,
				[]string{"class", "facts", "schema", "version", "week"}) {
				t.Fatalf("a record carries %v", keys)
			}
		}
	})
}

// TWO COMPANIES WITH ONE PATTERN SEND THE SAME BYTES: seats, names, counts
// and the day inside the week move nothing a bucket does not, so no field
// can join a record to its company or to another record.
func TestTwoCompaniesWithOnePatternSendTheSameBytes(t *testing.T) {
	t.Parallel()
	one := []usage.TurnLedgerRow{started("app", "chat:message", "nothing", 12, 0, 0),
		started("app", "chat:message", "moved", 18, 0, 0)}
	other := []usage.TurnLedgerRow{started("payroll-agent", "chat:message", "nothing", 40, 0, 0),
		started("payroll-agent", "chat:message", "moved", 60, 0, 0)}
	a := insights.Records(advisor.Detect(advisor.Evidence{Ledger: one}, kindOf), monday, tagged)
	b := insights.Records(advisor.Detect(advisor.Evidence{Ledger: other}, kindOf), monday.AddDate(0, 0, 6), tagged)
	rawA, errA := json.Marshal(a)
	rawB, errB := json.Marshal(b)
	if errA != nil || errB != nil || len(a) != 1 || string(rawA) != string(rawB) {
		t.Fatalf("one pattern in two companies sent\n%s\n%s", rawA, rawB)
	}
	if want := `[{"schema":1,"class":"wake.noop_share","week":"2030-W02","version":"v1.4.0",` +
		`"facts":{"seats":"1","share":"35-49"}}]`; string(rawA) != want {
		t.Errorf("the record is %s, want %s", rawA, want)
	}
}

// ONE RECORD PER CLASS: every finding of a class folds into one, with the worst
// one's facts and how many seats raised it.
func TestEveryFindingOfAClassFoldsIntoOneRecord(t *testing.T) {
	t.Parallel()
	ledger := []usage.TurnLedgerRow{
		started("app", "chat:message", "nothing", 6, 0, 0), started("app", "chat:message", "moved", 14, 0, 0),
		started("qa", "chat:message", "nothing", 12, 0, 0), started("qa", "chat:message", "moved", 8, 0, 0)}
	records := insights.Records(advisor.Detect(advisor.Evidence{Ledger: ledger}, kindOf), monday, tagged)
	if len(records) != 1 || !maps.Equal(records[0].Facts(), map[insights.FactKey]insights.Label{
		insights.FactShare: "50-74", insights.FactSeats: "2-3"}) {
		t.Errorf("two agents' no-op turns made %+v, want one record of the worse share and 2-3 seats", records)
	}
}

// A BUILD THAT SENDS NOTHING MAKES NO RECORD, and a public one is named by its
// release tag or `main`, never by its commit.
func TestARecordNamesTheBuildByItsTagOrMain(t *testing.T) {
	t.Parallel()
	findings := advisor.Detect(everyPattern(), kindOf)
	if got := insights.Records(findings, monday, development); len(got) != 0 {
		t.Errorf("a development build made %d records, want none", len(got))
	}
	for origin, want := range map[*issuereport.Origin]insights.Version{&tagged: "v1.4.0", &onMain: "main"} {
		for _, r := range insights.Records(findings, monday, *origin) {
			if r.Version() != want {
				t.Errorf("%s is named %q, want %q", origin.Build.Version, r.Version(), want)
			}
		}
	}
}

// A RECORD'S INSTANT IS ITS ISO WEEK, across a year's turn too.
func TestARecordsInstantIsItsISOWeek(t *testing.T) {
	t.Parallel()
	for at, want := range map[time.Time]insights.Week{
		monday: "2030-W02",
		time.Date(2029, 12, 31, 0, 0, 0, 0, time.UTC): "2030-W01",
		time.Date(2027, 1, 3, 23, 0, 0, 0, time.UTC):  "2026-W53",
	} {
		if got := insights.WeekOf(at); got != want {
			t.Errorf("%s is week %s, want %s", at, got, want)
		}
	}
}

// A RECORD IS SEALED: it has no exported field, so outside this package a
// record is one [insights.Records] made or the zero record, which fails Valid,
// and it cannot be decoded from JSON at all.
func TestARecordBuiltOutsideThePackageFailsValid(t *testing.T) {
	t.Parallel()
	for field := range reflect.TypeFor[insights.Record]().Fields() {
		if field.IsExported() {
			t.Errorf("Record.%s is exported, so a caller could set it", field.Name)
		}
	}
	if err := (insights.Record{}).Valid(); err == nil {
		t.Error("the zero record passed Valid")
	}
	if _, decodes := any(&insights.Record{}).(json.Unmarshaler); decodes {
		t.Error("Record decodes from JSON, so a forged body could become one")
	}
}

// EVERY RECORD FITS ITS BOUND, at its longest values.
func TestEveryRecordFitsItsBound(t *testing.T) {
	t.Parallel()
	for _, r := range insights.Records(advisor.Detect(everyPattern(), kindOf), monday, tagged) {
		raw, err := json.Marshal(r)
		if err != nil || len(raw) > insights.MaxRecordBytes {
			t.Errorf("%s encodes to %d bytes (%v), want at most %d", r.Class(), len(raw), err, insights.MaxRecordBytes)
		}
	}
}

// AN ATLAS FINDING MAKES NO RECORD: what the atlas shows needs a person names
// the company's own systems, and no class of the catalogue is one of the
// atlas's, so nothing about an entry is ever shared (ADR-0141).
func TestAnAtlasFindingMakesNoRecord(t *testing.T) {
	t.Parallel()
	ada, at := atlas.PersonSource("ada"), monday.Add(-30*24*time.Hour)
	service, err := atlas.Exists(atlas.MustRef(atlas.KindService, "checkout-api"), ada, at)
	if err != nil {
		t.Fatal(err)
	}
	findings := advisor.DetectAtlas(advisor.AtlasEvidence{Until: monday, Names: true, Interval: time.Hour,
		View:    atlas.Fold([]atlas.Claim{service}, monday),
		Sources: []advisor.AtlasSource{{Kind: "datadog", Name: "Datadog", LastCompleteAt: at}}})
	if len(findings) == 0 {
		t.Fatal("the atlas raised nothing to hold a record against")
	}
	if got := insights.Records(findings, monday, tagged); len(got) != 0 {
		t.Errorf("atlas findings made %d records, want none: %+v", len(got), got)
	}
	for _, e := range insights.Catalogue() {
		if slices.Contains(advisor.AtlasClasses(), advisor.Class(e.Class)) {
			t.Errorf("%s is an atlas class in the catalogue of what is shared", e.Class)
		}
	}
}
