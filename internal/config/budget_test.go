package config_test

import (
	"encoding/json"
	"maps"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
)

// A BUDGET IS THE WINDOWS IT NAMES, AND ONLY THOSE.
//
// An absent key is no ceiling for that window — never a ceiling of 0, which
// would refuse every turn — so Ceilings carries exactly the windows the author
// wrote, on the company and on a seat alike, and a budget that names none is
// no budget at all.
func TestATokenBudgetIsTheWindowsItNames(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		budget string
		want   org.TokenCeilings
	}{
		"no key":         {"", nil},
		"an empty block": {"token_budget: {}\n", nil},
		"a null block":   {"token_budget:\n", nil},
		"one window":     {"token_budget: {week: 700000}\n", org.TokenCeilings{period.Week: 700000}},
		"two of three":   {"token_budget:\n  day: 100\n  month: 3000\n", org.TokenCeilings{period.Day: 100, period.Month: 3000}},
		"every window":   {"token_budget: {day: 1, week: 2, month: 3}\n", org.TokenCeilings{period.Day: 1, period.Week: 2, period.Month: 3}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			company, err := config.ParseCompany([]byte("name: Acme\n" + tc.budget))
			if err != nil {
				t.Fatal(err)
			}
			if got := company.TokenBudget.Ceilings(); !maps.Equal(got, tc.want) || (tc.want == nil) != (got == nil) {
				t.Errorf("the company's Ceilings() = %#v, want %#v", got, tc.want)
			}
			o, err := company.Organization()
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(o.TokenBudget, tc.want) {
				t.Errorf("the org model carries %v, want %v", o.TokenBudget, tc.want)
			}

			seatDoc := "name: Acme\nroles:\n  - name: Dev\n" + indent(tc.budget, "    ")
			seat, err := config.ParseCompany([]byte(seatDoc))
			if err != nil {
				t.Fatal(err)
			}
			if got := seat.Roles[0].Seat().TokenBudget; !maps.Equal(got, tc.want) {
				t.Errorf("the seat's ceilings = %v, want %v", got, tc.want)
			}
		})
	}

	// An anchored budget shared between two seats is decoded at each use.
	shared, err := config.ParseCompany([]byte("name: Acme\nroles:\n  - name: Dev\n" +
		"    token_budget: &b {day: 5}\n  - name: Ops\n    token_budget: *b\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := shared.Roles[1].TokenBudget.Ceilings(); !maps.Equal(got, org.TokenCeilings{period.Day: 5}) {
		t.Errorf("an aliased budget = %v, want the day ceiling it points at", got)
	}
}

func indent(s, by string) string {
	if s == "" {
		return ""
	}
	lines := strings.SplitAfter(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = by + l
		}
	}
	return strings.Join(lines, "")
}

// A CEILING BELOW ONE TOKEN IS REFUSED AT THE KEY TO REMOVE.
//
// 0 used to be how a document said "unlimited", and it is also what the word
// ceiling says about a scope that may spend nothing. One number read two
// opposite ways is how a company that meant to stop a seat leaves it spending,
// so it is neither: the only way to leave a window uncapped is to leave its
// key out, and the refusal says so at the key an author wrote.
func TestACeilingBelowOneTokenIsRefusedAtTheKeyToRemove(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		doc, path, says string
	}{
		"a company day of 0": {
			"name: Acme\ntoken_budget: {day: 0}\n",
			"token_budget.day", "remove `token_budget.day` for no daily ceiling",
		},
		"a negative week on a seat in a unit": {
			"name: Acme\nunits:\n  - name: Eng\n    roles:\n      - name: Dev\n" +
				"        token_budget: {day: 10, week: -5}\n",
			"units[0].roles[0].token_budget.week", "remove `token_budget.week` for no weekly ceiling",
		},
		"a root seat's month of 0": {
			"name: Acme\nroles:\n  - name: Dev\n    token_budget: {month: 0}\n",
			"roles[0].token_budget.month", "remove `token_budget.month` for no monthly ceiling",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := config.ParseCompany([]byte(tc.doc))
			var found *config.Problem
			for _, p := range config.Problems(err) {
				if p.Path == tc.path {
					found = &p
				}
			}
			if found == nil {
				t.Fatalf("no problem at %s: %v", tc.path, err)
			}
			if found.Kind != "out_of_range" || !strings.Contains(found.Message, tc.says) {
				t.Errorf("%s = %s %q, want out_of_range saying %q", tc.path, found.Kind, found.Message, tc.says)
			}
		})
	}

	// The counterfactual, so the table cannot pass on a validator that
	// refuses every budget: one token is a ceiling.
	if _, err := config.ParseCompany([]byte("name: Acme\ntoken_budget: {day: 1, week: 1, month: 1}\n")); err != nil {
		t.Errorf("a ceiling of one token was refused: %v", err)
	}
}

// A BUDGET THAT IS NOT A MAPPING IS REFUSED WITH THE LINE TO WRITE INSTEAD.
//
// `token_budget: 10000000` is what every example company and the quickstart
// used to show. yaml's own refusal names a Go type; what the person holding
// that file needs is the mapping, with their own number in it when their
// number could be a ceiling.
func TestATokenBudgetThatIsNotAMappingSaysWhatToWrite(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		value, says string
	}{
		"the one number":                 {"10000000", "`token_budget: {month: 10000000}`"},
		"a 0, which was never a ceiling": {"0", "`token_budget: {month: 40000000}`"},
		"a list":                         {"[1, 2]", "must be a mapping of ceilings per calendar window"},
		"a word":                         {"lots", "must be a mapping of ceilings per calendar window"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := config.ParseCompanyDocument([]byte("name: Acme\ntoken_budget: " + tc.value + "\n"))
			problems := config.Problems(err)
			if len(problems) != 1 {
				t.Fatalf("problems = %+v, want the one budget", problems)
			}
			p := problems[0]
			if p.Path != "token_budget" || p.Kind != "shape" || p.Line != 2 {
				t.Errorf("problem at %s (%s, line %d), want token_budget (shape, line 2)", p.Path, p.Kind, p.Line)
			}
			if !strings.Contains(p.Message, tc.says) {
				t.Errorf("message = %q, want it to say %q", p.Message, tc.says)
			}
		})
	}
}

// A HUMAN SEAT CARRYING A BUDGET IS STILL A CONFLICT: a person spends no
// tokens, so a ceiling on one is configuration that reads as live and does
// nothing. An EMPTY block carries no ceiling and is not one.
func TestAHumanSeatCarryingABudgetIsAConflict(t *testing.T) {
	t.Parallel()
	const human = "name: Acme\nroles:\n  - name: Sarah\n    kind: human\n    contact: {slack_user_id: U0F}\n"
	_, err := config.ParseCompany([]byte(human + "    token_budget: {week: 5}\n"))
	var conflict bool
	for _, p := range config.Problems(err) {
		if p.Kind == "conflict" && strings.Contains(p.Message, "token_budget") {
			conflict = true
		}
	}
	if !conflict {
		t.Errorf("a human seat with a weekly ceiling was not refused as a conflict: %v", err)
	}
	if _, err := config.ParseCompany([]byte(human + "    token_budget: {}\n")); err != nil {
		t.Errorf("an empty budget on a human seat was refused: %v", err)
	}
}

// A CEILING THAT CAN NEVER REFUSE A TURN IS WARNED ABOUT, AT ITS OWN KEY.
//
// Every one of these runs exactly as written and admits nothing a working
// ceiling would refuse, so none is an error. What each does is mislead: a
// founder reading "5M a day" beside a company capped at 2M believes a seat
// has headroom it does not. The boundaries are exact — seven days at the
// daily ceiling reaching the weekly one is the case where the week does
// nothing, one token under is the case where it does — and the arithmetic is
// divided, so a ceiling near the int range cannot overflow into a warning.
func TestACeilingThatCanNeverRefuseIsWarnedAbout(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		doc        string
		path, says string
		seat       string
	}{
		"a day at the week": {
			"token_budget: {day: 500, week: 500}\n",
			"token_budget.day", "a day lies inside one week",
			"",
		},
		"a day above the month": {
			"token_budget: {day: 900, month: 800}\n",
			"token_budget.day", "a day lies inside one month",
			"",
		},
		"seven days reaching the week": {
			"token_budget: {day: 100, week: 700}\n",
			"token_budget.week", "a week is 7 days, and 7 at token_budget.day (100) is 700",
			"",
		},
		"thirty-one days under the month": {
			"token_budget: {day: 100, month: 5000}\n",
			"token_budget.month", "the longest month is 31 days",
			"",
		},
		"six weeks under the month": {
			"token_budget: {week: 100, month: 600}\n",
			"token_budget.month", "a month overlaps at most 6 ISO weeks",
			"",
		},
		"a week above the month": {
			"token_budget: {week: 900, month: 800}\n",
			"token_budget.week", "only in a week that straddles two months",
			"",
		},
		"a seat's day at the company's": {
			"token_budget: {day: 1000}\nroles:\n  - name: Dev\n    token_budget: {day: 1000}\n",
			"roles[0].token_budget.day", "seat \"dev\"'s token_budget.day (1000) is at or above the company's (1000)",
			"dev",
		},
		"a seat's day above the company's month": {
			"token_budget: {month: 1000}\nroles:\n  - name: Dev\n    token_budget: {day: 5000}\n",
			"roles[0].token_budget.day",
			"at or above the company's token_budget.month (1000), and a day lies inside one month",
			"dev",
		},
		"a seat's own pair, in a unit": {
			"units:\n  - name: Eng\n    roles:\n      - name: Dev\n        token_budget: {day: 10, week: 70}\n",
			"units[0].roles[0].token_budget.week", "a week is 7 days",
			"dev",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.ParseCompany([]byte("name: Acme\n" + tc.doc))
			if err != nil {
				t.Fatalf("the fixture does not validate, so this is a refusal "+
					"rather than a warning: %v", err)
			}
			var found []config.Warning
			for _, w := range cfg.AdvisoryWarnings() {
				if strings.Contains(w.Path, "token_budget") {
					found = append(found, w)
				}
			}
			if len(found) != 1 {
				t.Fatalf("budget warnings = %+v, want exactly the one about %s", found, tc.path)
			}
			w := found[0]
			if w.Path != tc.path || w.Kind != config.WarningAdvisory || w.Seat != tc.seat {
				t.Errorf("warning at %s (%s, seat %q), want %s (advisory, seat %q)",
					w.Path, w.Kind, w.Seat, tc.path, tc.seat)
			}
			if !strings.Contains(w.Message, tc.says) {
				t.Errorf("message = %q, want it to say %q", w.Message, tc.says)
			}
		})
	}
}

// AND A BUDGET WHOSE EVERY CEILING CAN REFUSE WARNS ABOUT NOTHING — the
// control, without which the table above passes on a check that warns about
// every budget. Each value sits one token on the working side of a boundary
// the table crosses.
func TestABudgetWhoseEveryCeilingCanRefuseWarnsAboutNothing(t *testing.T) {
	t.Parallel()
	huge := strconv.Itoa(math.MaxInt / 2)
	for name, doc := range map[string]string{
		"a week one token under seven days":  "token_budget: {day: 100, week: 699}\n",
		"a month one token under 31 days":    "token_budget: {day: 100, month: 3099}\n",
		"a month one token under six weeks":  "token_budget: {week: 100, month: 599}\n",
		"a week one token under the month":   "token_budget: {week: 799, month: 800}\n",
		"a day one token under the week":     "token_budget: {day: 499, week: 500}\n",
		"a seat one token under the company": "token_budget: {day: 1000}\nroles:\n  - name: Dev\n    token_budget: {day: 999}\n",
		// 7 × this overflows an int; divided, it is simply a day that
		// can spend the week's ceiling in two days.
		"ceilings near the int range": "token_budget: {day: " + huge + ", week: " + strconv.Itoa(math.MaxInt) + "}\n",
		"the shipped shape":           "token_budget: {day: 3000000, week: 10000000, month: 40000000}\n",
		// A week does not lie inside one month, so a company month says
		// nothing about a seat's week; nor does a company week about a
		// seat's month, which spans several.
		"a seat's week against the company's month": "token_budget: {month: 1000}\n" +
			"roles:\n  - name: Dev\n    token_budget: {week: 5000}\n",
		"a seat's month against the company's week": "token_budget: {week: 1000}\n" +
			"roles:\n  - name: Dev\n    token_budget: {month: 3000}\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.ParseCompany([]byte("name: Acme\n" + doc))
			if err != nil {
				t.Fatalf("the fixture does not validate: %v", err)
			}
			for _, w := range cfg.AdvisoryWarnings() {
				if strings.Contains(w.Path, "token_budget") {
					t.Errorf("warned about a ceiling that can refuse: %s: %s", w.Path, w.Message)
				}
			}
		})
	}
}

// A TOKEN BUDGET IS NOT A CREDENTIAL, and it survives every form a revision
// takes: the redacted read a dashboard is handed and sends back, the JSON the
// store holds, and the YAML an export prints.
func TestATokenBudgetSurvivesRedactionTheStoreAndExport(t *testing.T) {
	t.Parallel()
	const doc = "name: Acme\ntoken_budget: {day: 100, month: 3000}\n" +
		"roles:\n  - name: Dev\n    token_budget: {week: 50}\n"
	original, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	want := func(t *testing.T, c *config.Company, via string) {
		t.Helper()
		if got := c.TokenBudget.Ceilings(); !maps.Equal(got, org.TokenCeilings{period.Day: 100, period.Month: 3000}) {
			t.Errorf("%s: the company's budget = %v", via, got)
		}
		if got := c.Roles[0].TokenBudget.Ceilings(); !maps.Equal(got, org.TokenCeilings{period.Week: 50}) {
			t.Errorf("%s: the seat's budget = %v", via, got)
		}
	}

	want(t, original.Redact(), "the redacted read")

	stored, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := config.DecodeCompany(stored)
	if err != nil {
		t.Fatalf("the stored form does not decode: %v", err)
	}
	want(t, decoded, "the stored form")

	exported, err := config.EncodeCompanyYAML(original)
	if err != nil {
		t.Fatal(err)
	}
	reimported, err := config.ParseCompany(exported)
	if err != nil {
		t.Fatalf("the export does not import: %v\n%s", err, exported)
	}
	want(t, reimported, "the export")

	// An absent budget is ABSENT on the wire, not an empty object a reader
	// has to learn means nothing.
	bare, err := json.Marshal(config.Company{Name: "Acme"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bare), "token_budget") {
		t.Errorf("a company with no budget wrote one: %s", bare)
	}
}

// ONE KEY PER PERIOD, IN THE CALENDAR'S ORDER, AND THE ORG BUILDER OFFERS
// EXACTLY THOSE.
//
// The authored mapping, the calendar and the dashboard's editor each list the
// windows, and nothing but this compares them. A period the calendar grew
// that the mapping cannot name is a window every company leaves uncapped; one
// the editor does not offer is a ceiling an operator can only write by hand;
// and one the editor offers that the mapping does not have is a field whose
// every save is refused as an unknown key.
func TestTheBudgetEditorOffersExactlyTheEnginesWindows(t *testing.T) {
	t.Parallel()
	var keys []string
	typ := reflect.TypeFor[config.TokenBudget]()
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("yaml"), ",")
		keys = append(keys, name)
	}
	var periods []string
	for _, p := range period.Periods {
		periods = append(periods, string(p))
	}
	if !slices.Equal(keys, periods) {
		t.Errorf("token_budget's keys are %v, want one per period in the calendar's order: %v", keys, periods)
	}

	body, err := clientsource.Literal(clientsource.Tree, "BUDGET_WINDOWS")
	if err != nil {
		t.Fatal(err)
	}
	offered := clientsource.Field(body, "period")
	if len(offered) == 0 {
		t.Fatal("the dashboard offers no budget windows, so this gate certifies nothing")
	}
	if !slices.Equal(offered, periods) {
		t.Errorf("the org builder offers the windows %v, want exactly the engine's, in order: %v", offered, periods)
	}
}
