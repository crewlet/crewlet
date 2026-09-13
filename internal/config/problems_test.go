package config_test

import (
	"encoding/json"
	"errors"
	"reflect"
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
    manages: [Eng, Ghost]
  - name: Dev
    unit: Nowhere
units:
  - name: Eng
    roles:
      - name: Lead
      - name: Engineer
    children:
      - name: Platform
        lead: Phantom
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
		{"unit", "roles[1].unit", "dev", "", "Dev", "Nowhere", config.Path{"roles", 1, "unit"}},
		{"lead", "units[0].children[0].lead", "", "Platform", "Platform", "Phantom",
			config.Path{"units", 0, "children", 0, "lead"}},
		{"manages", "roles[0].manages[1]", "ceo", "", "CEO", "Ghost", config.Path{"roles", 0, "manages", 1}},
		{"gitlab_access_level", "integrations.gitlab.provisioning.access_levels.former.engineer", "", "",
			"integrations.gitlab.provisioning.access_levels", "former.engineer",
			config.Path{"integrations", "gitlab", "provisioning", "access_levels", "former.engineer"}},
	}
	if !reflect.DeepEqual(got, expected) {
		t.Errorf("warnings =\n%+v\nwant\n%+v", got, expected)
	}
}
