package config_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// parsed decodes a document without validating it.
func parsed(t *testing.T, doc string) *config.Company {
	t.Helper()
	cfg, err := config.ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cfg
}

// located is what a test asserts about one problem.
type located struct {
	path, kind, seat, unit string
}

func locatedOf(problems []config.Problem, messageHas string) []located {
	var out []located
	for _, p := range problems {
		if strings.Contains(p.Message, messageHas) {
			out = append(out, located{p.Path, p.Kind, p.Seat, p.Unit})
		}
	}
	return out
}

// EVERY RULE THE ORG MODEL CHECKS IS PLACED WHERE IT WAS WRITTEN, by the entity
// it is about rather than by its name. The mistakes a person makes while
// building a chart are exactly the ones that make names ambiguous, so each case
// here is one a name could not have located.
func TestAnOrgRuleIsLocatedWhereItWasWritten(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		doc        string
		messageHas string
		want       []located
	}{
		{
			name: "a seat with no name, among others",
			doc: "name: Acme\nunits:\n  - name: Eng\n    roles:\n      - name: Lead\n" +
				"      - goal: ship\n",
			messageHas: "name must not be empty",
			want:       []located{{"units[0].roles[1].name", "missing", "", ""}},
		},
		{
			name:       "a unit with no name",
			doc:        "name: Acme\nunits:\n  - name: Eng\n    children:\n      - purpose: x\n",
			messageHas: "unit: name must not be empty",
			want:       []located{{"units[0].children[0].name", "missing", "", ""}},
		},
		{
			// Normalization moves a root seat into the unit its `unit:`
			// names; the problem stays where the seat was written.
			name: "a root seat placed by its unit reference",
			doc: "name: Acme\nroles:\n  - name: Dev\n    unit: Eng\n    kind: robot\n" +
				"units:\n  - name: Eng\n    roles:\n      - name: Lead\n",
			messageHas: "unknown seat kind",
			want:       []located{{"roles[0].kind", "unknown_value", "dev", ""}},
		},
		{
			// A name that slugifies to nothing derives no handle, so the
			// problem carries no seat and only its position locates it.
			name:       "a name that yields no handle",
			doc:        "name: Acme\nroles:\n  - name: Ops\n  - name: \"!!!\"\n",
			messageHas: "yields no handle",
			want:       []located{{"roles[1].name", "unknown_value", "", ""}},
		},
		{
			name: "a schedule's cron on a unit",
			doc: "name: Acme\nunits:\n  - name: Eng\n    roles:\n      - name: Dev\n" +
				"    schedules:\n      - {name: a, cron: \"0 9 * * *\", task: t}\n" +
				"      - {name: b, cron: \"0 9 * *\", task: t}\n",
			messageHas: "needs 5 fields",
			want:       []located{{"units[0].schedules[1].cron", "shape", "", "Eng"}},
		},
		{
			name: "a contact identity embedding a reference",
			doc: "name: Acme\nroles:\n  - name: Sarah\n    kind: human\n" +
				"    contact: {slack_user_id: \"U${SUFFIX}\"}\n",
			messageHas: "embeds a ${VAR}",
			want:       []located{{"roles[0].contact.slack_user_id", "unknown_value", "sarah", ""}},
		},
		{
			// ONE LINE, ONE PROBLEM PER SEAT, each at the name that seat
			// wrote. Identical names are the case a name-based lookup
			// cannot tell apart at all.
			name: "two seats of one name",
			doc: "name: Acme\nroles:\n  - name: Software Engineer\n    handle: swe-root\n" +
				"units:\n  - name: Eng\n    roles:\n      - name: Software Engineer\n        handle: swe-eng\n",
			messageHas: "duplicate seat name",
			want: []located{
				{"roles[0].name", "conflict", "swe-root", ""},
				{"units[0].roles[0].name", "conflict", "swe-eng", ""},
			},
		},
		{
			// A written handle is the line to change; a derived one is
			// changed by renaming the seat.
			name: "two seats on one handle",
			doc: "name: Acme\nroles:\n  - name: Dev\n" +
				"units:\n  - name: Eng\n    roles:\n      - name: Developer\n        handle: dev\n",
			messageHas: "duplicate handle",
			want: []located{
				{"roles[0].name", "conflict", "dev", ""},
				{"units[0].roles[0].handle", "conflict", "dev", ""},
			},
		},
		{
			name: "two units of one name",
			doc: "name: Acme\nunits:\n  - name: Eng\n    children:\n      - name: Platform\n" +
				"  - name: Product\n    children:\n      - name: Platform\n",
			messageHas: "duplicate unit name",
			want: []located{
				{"units[0].children[0].name", "conflict", "", "Platform"},
				{"units[1].children[0].name", "conflict", "", "Platform"},
			},
		},
		{
			// A rule the config layer checks inside a seat carries the seat
			// its path sits in.
			name: "a config rule inside a nested seat",
			doc: "name: Acme\nunits:\n  - name: Eng\n    roles:\n      - name: Dev\n" +
				"        integrations:\n          slack: {signing_secret: \"${S}\"}\n",
			messageHas: "bot_token",
			want:       []located{{"units[0].roles[0].integrations.slack.bot_token", "missing", "dev", ""}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := parsed(t, tc.doc).Validate()
			problems := config.Problems(err)
			if got := locatedOf(problems, tc.messageHas); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("problems naming %q = %+v, want %+v\nall: %+v", tc.messageHas, got, tc.want, problems)
			}
			assertProblemsAreTheRefusal(t, err, problems)
		})
	}
}

// assertProblemsAreTheRefusal holds the contract between the text and the
// structure: every problem's message is a whole line of the refusal, its
// segments are its path, and no line of the refusal goes unaccounted for.
func assertProblemsAreTheRefusal(t *testing.T, err error, problems []config.Problem) {
	t.Helper()
	lines := strings.Split(err.Error(), "\n")
	covered := make(map[string]bool, len(lines))
	for _, p := range problems {
		found := false
		for _, line := range lines {
			if line == p.Message {
				found, covered[line] = true, true
			}
		}
		if !found {
			t.Errorf("problem message %q is not a line of the refusal:\n%v", p.Message, err)
		}
		if p.Segments.String() != p.Path {
			t.Errorf("segments %#v render %q, not the path %q", p.Segments, p.Segments.String(), p.Path)
		}
		if p.Path == "" && p.Segments != nil {
			t.Errorf("a problem with no path carries segments %#v", p.Segments)
		}
	}
	for _, line := range lines {
		if !covered[line] {
			t.Errorf("line %q of the refusal is no problem's message", line)
		}
	}
}

// A RULE ABOUT ONE SEAT OR UNIT RENDERS LED BY ITS PATH, like every rule the
// config layer reports, so a refusal's text says where as well as what. A
// seat with no name used to be reported twice, once by each layer; it is
// reported once, by the org model, at the name.
func TestAnOrgRuleIsReportedOnceLedByItsPath(t *testing.T) {
	t.Parallel()
	err := parsed(t, "name: Acme\nroles:\n  - goal: ship\n").Validate()
	if err == nil {
		t.Fatal("a seat with no name validated")
	}
	if got, want := err.Error(), "roles[0].name: role: name must not be empty"; got != want {
		t.Errorf("refusal = %q, want %q", got, want)
	}
}

// THE KINDS ARE A CLOSED SET WITH A FALLBACK, and the org model's rules land
// in it rather than in the fallback. An error nothing classifies has no place
// in the document and says `invalid`, never an empty kind.
func TestEveryProblemHasAKind(t *testing.T) {
	t.Parallel()
	got := config.Problems(errors.New("could not open company.yaml"))
	want := []config.Problem{{Kind: "invalid", Message: "could not open company.yaml"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Problems() = %+v, want %+v", got, want)
	}
	raw, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	// The wire form the dashboard and the CLI's consumers code against: a
	// document-level problem has a null segment list, and the optional
	// locators are omitted rather than empty.
	if want := `{"path":"","segments":null,"kind":"invalid","message":"could not open company.yaml"}`; string(raw) != want {
		t.Errorf("JSON = %s, want %s", raw, want)
	}
}

// EVERY ORG RULE HAS THE KIND THE CONTRACT NAMES. A consumer branches on the
// kind (a form marks a missing value differently from two things that cannot
// both hold), so a sentinel that fell out of the classification table would
// arrive as `invalid` with nothing else wrong: the message would still read
// right and every located-problem test would still pass. One document per
// sentinel, and the kind of the problem that rule produced.
func TestEveryOrgRuleHasTheKindTheContractNames(t *testing.T) {
	t.Parallel()
	human := "kind: human\n    contact: {slack_user_id: U0SARAH}\n"
	for _, tc := range []struct {
		rule, doc, kind string
	}{
		{"name must not be empty", "name: Acme\nroles:\n  - goal: ship\n", "missing"},
		{"human seat needs at least one contact identity",
			"name: Acme\nroles:\n  - name: Sarah\n    kind: human\n", "missing"},
		{"unknown seat kind", "name: Acme\nroles:\n  - name: Dev\n    kind: robot\n", "unknown_value"},
		{"handle must match", "name: Acme\nroles:\n  - name: Dev\n    handle: Dev!\n", "unknown_value"},
		{"value embeds a ${VAR} reference",
			"name: Acme\nroles:\n  - name: Sarah\n    kind: human\n    contact: {slack_user_id: \"U${S}\"}\n",
			"unknown_value"},
		{"duplicate handle",
			"name: Acme\nroles:\n  - name: Dev\n  - name: Developer\n    handle: dev\n", "conflict"},
		{"duplicate seat name",
			"name: Acme\nroles:\n  - name: Dev\n    handle: a\n  - name: Dev\n    handle: b\n", "conflict"},
		{"duplicate unit name", "name: Acme\nunits:\n  - name: Eng\n  - name: Eng\n", "conflict"},
		// Two seats on one account carries ErrDuplicateIdentity ALONE — no
		// name or handle collided — so it is the one duplicate a missing
		// table entry reports as `invalid`.
		{"duplicate contact identity",
			"name: Acme\nroles:\n  - name: Sarah\n    kind: human\n" +
				"    contact: {slack_user_id: U0F}\n  - name: Ada\n    kind: human\n" +
				"    contact: {slack_user_id: U0F}\n",
			"conflict"},
		// A collision an id carried names no duplicate name and so carries
		// only the key sentinel, which is the entry the table lost.
		{"duplicate unit key", "name: Acme\nunits:\n  - name: Platform\n  - name: Product\n    id: platform\n",
			"conflict"},
		{"agent-only field set on a human seat",
			"name: Acme\nroles:\n  - name: Sarah\n    " + human + "    token_budget: 5\n", "conflict"},
		{"human-only field set on an agent seat",
			"name: Acme\nroles:\n  - name: Dev\n    availability: mornings\n", "conflict"},
		{"schedule has no runner",
			"name: Acme\nunits:\n  - name: Design\n    roles:\n      - name: Sarah\n        " +
				strings.ReplaceAll(human, "\n    ", "\n        ") +
				"    schedules:\n      - {name: digest, cron: \"0 9 * * *\", task: post}\n",
			"conflict"},
		{"unit reference on a seat inside another unit",
			"name: Acme\nunits:\n  - name: Eng\n    roles:\n      - name: Dev\n        unit: Product\n" +
				"  - name: Product\n",
			"conflict"},
		{"invalid schedule",
			"name: Acme\nroles:\n  - name: Dev\n    schedules:\n      - {name: a, cron: \"x\", task: t}\n",
			"shape"},
	} {
		t.Run(tc.rule, func(t *testing.T) {
			t.Parallel()
			var kinds []string
			for _, p := range config.Problems(parsed(t, tc.doc).Validate()) {
				if strings.Contains(p.Message, tc.rule) {
					kinds = append(kinds, p.Kind)
				}
			}
			if len(kinds) == 0 {
				t.Fatalf("no problem reports %q for:\n%s", tc.rule, tc.doc)
			}
			for _, kind := range kinds {
				if kind != tc.kind {
					t.Errorf("%q is a %s problem, want %s", tc.rule, kind, tc.kind)
				}
			}
		})
	}
}

// A PARSER FAILURE'S MESSAGE IS THE WHOLE LINE, its line number included, and a
// document that does not parse at all names no place: an empty path with a
// null segment list, exactly as the wire contract states, rather than an empty
// array a consumer would read as the document root.
func TestAParserProblemCarriesItsLineAndNoSegmentsWhenUnplaced(t *testing.T) {
	t.Parallel()
	_, err := config.ParseCompanyDocument([]byte("name: Acme\n\nbackstroy: x\n"))
	problems := config.Problems(err)
	if len(problems) != 1 {
		t.Fatalf("problems = %+v, want one", problems)
	}
	if want := `backstroy: unknown field: "backstroy" is not a setting: check the spelling, ` +
		`or the block it belongs under (line 3)`; problems[0].Message != want {
		t.Errorf("message = %q, want %q", problems[0].Message, want)
	}

	_, err = config.ParseCompanyDocument([]byte("name: Acme\nvision 2\npolicies: []\n"))
	problems = config.Problems(err)
	if len(problems) != 1 || problems[0].Path != "" {
		t.Fatalf("problems = %+v, want one with no path", problems)
	}
	raw, err := json.Marshal(problems[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"path":"","segments":null,`) {
		t.Errorf("an unplaced parser problem marshals as %s, want a null segment list", raw)
	}
}

// A REFERENCE THAT RESOLVES TO NOTHING IS LOCATED WHERE IT WAS WRITTEN: a lead
// at the unit's `lead`, a unit reference at the seat's `unit`, a manages entry
// at the index it was written at (normalization rewrites the list, so the
// normalized position is not it), and an access level at its key.
func TestReferenceWarningsAreLocatedWhereTheyWereWritten(t *testing.T) {
	t.Parallel()
	cfg := parsed(t, `
name: Acme
integrations:
  gitlab:
    enabled: true
    url: https://gitlab.example.com
    signing_secret: "${GITLAB_SIGNING_SECRET}"
    provisioning:
      group: acme
      access_levels:
        former.engineer: maintainer
roles:
  - name: CEO
    manages: [eng, ghost]
  - name: Dev
    unit: nowhere
units:
  - name: Eng
    roles:
      - name: Lead
      - name: Engineer
    children:
      - name: Platform
        lead: phantom
`)
	type want struct {
		ref, path, seat, unit, from, to string
		segments                        config.Path
	}
	var got []want
	for _, w := range cfg.ReferenceWarnings() {
		if w.Kind != config.WarningDanglingReference || w.Message == "" {
			t.Errorf("warning %+v is not a dangling reference with a message", w)
		}
		got = append(got, want{w.Ref, w.Path, w.Seat, w.Unit, w.From, w.To, w.Segments})
	}
	expected := []want{
		{"unit", "roles[1].unit", "dev", "", "Dev", "nowhere", config.Path{"roles", 1, "unit"}},
		{"lead", "units[0].children[0].lead", "", "Platform", "Platform", "phantom",
			config.Path{"units", 0, "children", 0, "lead"}},
		{"manages", "roles[0].manages[1]", "ceo", "", "CEO", "ghost", config.Path{"roles", 0, "manages", 1}},
		{"gitlab_access_level", "integrations.gitlab.provisioning.access_levels.former.engineer", "", "",
			"integrations.gitlab.provisioning.access_levels", "former.engineer",
			config.Path{"integrations", "gitlab", "provisioning", "access_levels", "former.engineer"}},
	}
	if !reflect.DeepEqual(got, expected) {
		t.Errorf("warnings =\n%+v\nwant\n%+v", got, expected)
	}
}

// A STORED REVISION'S ADMISSION VIOLATIONS ARE WARNINGS PLACED LIKE PROBLEMS.
//
// A reload or a revert re-activates a company under the runnable rules, and
// the answer still has to say which admission rules it breaks and where: the
// same places a write keeping them is refused at, one beside each entity a
// duplicate names, so a client draws a warning and a problem on the same node.
func TestAdmissionViolationsAreWarningsPlacedLikeTheirProblems(t *testing.T) {
	t.Parallel()
	cfg := parsed(t, `
name: Acme
units:
  - name: Platform
    roles:
      - name: Engineer
        handle: platform-engineer
  - name: Product
    children:
      - name: Platform
        roles:
          - name: Engineer
            handle: product-engineer
`)
	problems := config.Problems(cfg.ValidateAdmission())
	warnings := cfg.AdmissionWarnings()
	if len(problems) == 0 {
		t.Fatal("the fixture breaks no admission rule, so this proves nothing")
	}
	if len(warnings) != len(problems) {
		t.Fatalf("%d warnings for %d problems, want one per problem:\n%+v", len(warnings), len(problems), warnings)
	}
	for i, w := range warnings {
		p := problems[i]
		if w.Kind != config.WarningAdmission || w.Ref != "" || w.From != "" || w.To != "" {
			t.Errorf("warning %d = %+v, want an admission warning naming no reference", i, w)
		}
		if w.Path != p.Path || !reflect.DeepEqual(w.Segments, p.Segments) ||
			w.Seat != p.Seat || w.Unit != p.Unit || w.Message != p.Message {
			t.Errorf("warning %d = %+v, placed differently from its problem %+v", i, w, p)
		}
	}
	var paths []string
	for _, w := range warnings {
		paths = append(paths, w.Path)
	}
	for _, want := range []string{"units[0].name", "units[1].children[0].name",
		"units[0].roles[0].name", "units[1].children[0].roles[0].name"} {
		if !slices.Contains(paths, want) {
			t.Errorf("no admission warning at %s: %v", want, paths)
		}
	}

	// And Warnings is ONE list in a fixed order: the references, then
	// these, then the advisories. A caller that renders it top to bottom
	// reads what the document points at nothing before what it merely
	// leaves unsaid.
	want := append(cfg.ReferenceWarnings(), warnings...)
	want = append(want, cfg.AdvisoryWarnings()...)
	if all := cfg.Warnings(); !reflect.DeepEqual(all, want) {
		t.Errorf("Warnings = %+v,\nwant the references, then the admission warnings, then the advisories:\n%+v",
			all, want)
	}
}

// AN ADMITTED COMPANY CARRIES NO ADMISSION WARNING, so a write's answer holds
// its references alone.
func TestAnAdmittedCompanyHasNoAdmissionWarnings(t *testing.T) {
	t.Parallel()
	// A WELL-FORMED HANDLE THAT NAMES NOBODY, for the reason
	// TestDanglingReferencesAreReportedNotRejected states: a display name
	// in a reference is refused by its own admission rule, so a fixture
	// written that way would break this one's premise rather than test it.
	cfg := parsed(t, "name: Acme\nroles:\n  - name: CEO\n    manages: [ghost]\n")
	if err := cfg.ValidateAdmission(); err != nil {
		t.Fatalf("the fixture breaks an admission rule: %v", err)
	}
	if got := cfg.AdmissionWarnings(); len(got) != 0 {
		t.Errorf("admission warnings = %+v, want none", got)
	}
	if got := cfg.Warnings(); len(got) != 1 || got[0].Kind != config.WarningDanglingReference {
		t.Errorf("warnings = %+v, want the one dangling manages entry", got)
	}
}
