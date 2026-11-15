package queries_test

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// askSettings is `settings` as a caller holding grants reads it.
func askSettings(t *testing.T, c *config.Company, grants ...iam.Grant) (queries.SettingsAnswer, error) {
	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Company: companySource(t, c)})
	got, err := r.Answer(asGrants(t, "", grants...), "settings", nil)
	if err != nil {
		return queries.SettingsAnswer{}, err
	}
	return got.(queries.SettingsAnswer), nil
}

// settingsOf is the rows of the company doc parses to, by key.
func settingsOf(t *testing.T, doc string) map[string]queries.SettingRow {
	t.Helper()
	c, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse:\n%s\n%v", doc, err)
	}
	got, err := askSettings(t, c, iam.GrantAdmin)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	out := map[string]queries.SettingRow{}
	for _, row := range got.Settings {
		out[row.Key] = row
	}
	return out
}

// hosted is a company Gitlet hosts code for, which every code setting needs.
const hosted = "name: Acme\ncode:\n  backend: gitlet\n"

// EVERY ROW'S KEY IS THE DOCUMENT PATH ITS VALUE IS READ FROM, and a written
// key says Set.
//
// A document writes a value no default holds at every key the answer names,
// and each row must answer exactly that value: a getter reading another
// field would put the wrong value under a correct-looking label.
//
// Mutation: swap two getters in `settings`, or answer Set from the value
// rather than the document, and a row here fails.
func TestEverySettingIsReadFromTheKeyItNames(t *testing.T) {
	t.Parallel()
	rows := settingsOf(t, `name: Acme
timezone: Europe/Berlin
providers:
  embeddings:
    type: openai
    model: text-embedding-3-small
knowledge:
  root_space: ORG
  skills_container: SKL
  scope: [ENG, PROD]
  vectors: false
code:
  backend: gitlet
  identity_domain: commits.example.com
`)
	want := map[string]any{
		"timezone":                   "Europe/Berlin",
		"code.identity_domain":       "commits.example.com",
		"knowledge.root_space":       "ORG",
		"knowledge.skills_container": "SKL",
		"knowledge.scope":            []string{"ENG", "PROD"},
		"knowledge.vectors":          false,
	}
	for key, value := range want {
		row, ok := rows[key]
		if !ok {
			t.Errorf("the answer has no row for %s", key)
			continue
		}
		if !reflect.DeepEqual(row.Value, value) {
			t.Errorf("%s answers %#v, and the document writes %#v", key, row.Value, value)
		}
		if !row.Set {
			t.Errorf("%s is written and answered unset", key)
		}
		if row.Off {
			t.Errorf("%s = %v is answered off", key, row.Value)
		}
	}
	for key := range rows {
		if _, ok := want[key]; !ok {
			t.Errorf("the answer names %q, which this case does not write: add it", key)
		}
	}
	// THE DERIVED DEFAULT IS THIS COMPANY'S: it has an embeddings provider,
	// so leaving vectors unwritten would search by meaning.
	if got := rows["knowledge.vectors"].Default; got != true {
		t.Errorf("vectors default = %v beside an embeddings provider, want true", got)
	}
}

// A DOCUMENT THAT SAYS NOTHING IS HELD TO THE ENGINE'S DEFAULTS, said as the
// values the engine applies (UTC, seats.invalid, HOME, TS, every space, no
// semantic search without a provider) and never as an empty string a screen
// would have to fill from a copy of its own.
//
// Mutation: read a default from anywhere but the accessor the engine runs
// on, or answer Set where nothing is written, and a row here disagrees.
func TestADocumentThatSaysNothingIsHeldToTheSettingsDefaults(t *testing.T) {
	t.Parallel()
	rows := settingsOf(t, hosted)
	want := map[string]any{
		"timezone":                   "UTC",
		"code.identity_domain":       config.DefaultCodeIdentityDomain,
		"knowledge.root_space":       config.DefaultRootSpace,
		"knowledge.skills_container": config.DefaultSkillsContainer,
		"knowledge.scope":            []string{},
		"knowledge.vectors":          false,
	}
	if len(rows) != len(want) {
		t.Errorf("the answer has %d rows, want %d: %v", len(rows), len(want), rows)
	}
	for key, value := range want {
		row := rows[key]
		if !reflect.DeepEqual(row.Value, value) || !reflect.DeepEqual(row.Default, value) {
			t.Errorf("%s answers %#v against a default of %#v, want both %#v",
				key, row.Value, row.Default, value)
		}
		if row.Set || row.Off {
			t.Errorf("%s is answered set = %v, off = %v on a document that writes nothing",
				key, row.Set, row.Off)
		}
		// AN OFF OF ITS OWN is the two containers' alone: their empty value
		// is a setting, where the others' empty value is the default.
		wantOff := key == "knowledge.root_space" || key == "knowledge.skills_container"
		if row.HasOff != wantOff {
			t.Errorf("%s has_off = %v, want %v", key, row.HasOff, wantOff)
		}
	}
}

// AN EMPTY SKILLS CONTAINER IS TOOL SKILLS OFF, a setting written, never the
// default: the row says Off and Set, beside the default it replaced.
func TestAnEmptySkillsContainerIsAnsweredOff(t *testing.T) {
	t.Parallel()
	row := settingsOf(t, "name: Acme\nknowledge:\n  skills_container: \"\"\n")["knowledge.skills_container"]
	if row.Value != "" || !row.Off || !row.Set || row.Default != config.DefaultSkillsContainer {
		t.Errorf("an empty skills container answers %+v, want off and set against %s",
			row, config.DefaultSkillsContainer)
	}
}

// A ROW IS ANSWERED WHERE THE COMPANY RUNS WHAT IT CONFIGURES, and nowhere
// else: the validator refuses a code setting on a company Gitlet does not
// host, and a knowledge setting on one with no knowledge base, so a screen
// offering either there offers a write that cannot land.
func TestASettingIsAnsweredOnlyWhereItConfiguresSomething(t *testing.T) {
	t.Parallel()
	rows := settingsOf(t, "name: Acme\nknowledge:\n  backend: none\n")
	for key := range rows {
		if strings.HasPrefix(key, "code.") || strings.HasPrefix(key, "knowledge.") {
			t.Errorf("%s is answered on a company with no Gitlet and no knowledge base", key)
		}
	}
	if _, ok := rows["timezone"]; !ok {
		t.Error("the company's clock is not answered, and every company has one")
	}
}

// THE SETTINGS ARE AN ADMINISTRATOR'S, as the company document they are read
// from is, and a node running no company answers none.
func TestTheSettingsAreAnAdministratorsAndNeedACompany(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte("name: Acme\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := askSettings(t, c, iam.Apps...); !errors.Is(err, queries.ErrUnauthorized) {
		t.Errorf("a caller holding every app and not admin = %v, want unauthorized", err)
	}
	got, err := askSettings(t, nil, iam.GrantAdmin)
	if err != nil {
		t.Fatalf("settings with no company: %v", err)
	}
	if got.Settings == nil || len(got.Settings) != 0 {
		t.Errorf("a node running no company answers %+v, want an empty list", got.Settings)
	}
}

// EVERY SETTING'S REFUSAL IS SAID IN A PERSON'S WORDS, at its own key.
//
// A screen changing one of these sends it for the engine's check before it
// saves (`PATCH /config?dry_run=true`) and says the refusal as the rule wrote
// it for a person ([config.Fault.Said]), copying no rule of its own. So a
// value each validator refuses must be refused at the row's key, or at an
// entry of it, carrying words that name no document key.
//
// Mutation: drop a Said from one of these validators and its row fails.
func TestEverySettingsRefusalIsSaidInAPersonsWords(t *testing.T) {
	t.Parallel()
	refused := map[string]string{
		"timezone":                   "timezone: Mars/Olympus\n",
		"code.identity_domain":       "code:\n  backend: gitlet\n  identity_domain: Not_A_Domain\n",
		"knowledge.root_space":       "knowledge:\n  root_space: \"1ORG\"\n",
		"knowledge.skills_container": "knowledge:\n  skills_container: \"S-K\"\n",
		"knowledge.scope":            "knowledge:\n  scope: [\"9X\"]\n",
		"knowledge.vectors":          "knowledge:\n  vectors: true\n",
	}
	rows := settingsOf(t, hosted)
	for key := range rows {
		if _, ok := refused[key]; !ok {
			t.Errorf("%s is answered and this case refuses no value of it: add one", key)
		}
	}
	for key, body := range refused {
		_, err := config.ParseCompany([]byte("name: Acme\n" + body))
		var said []string
		for _, p := range config.Problems(err) {
			if p.Path == key || strings.HasPrefix(p.Path, key+".") || strings.HasPrefix(p.Path, key+"[") {
				said = append(said, p.Said)
			}
		}
		if len(said) == 0 {
			t.Errorf("%s: %q is admitted, or refused at another key: %v", key, body, err)
			continue
		}
		// NO DOCUMENT KEY, whole or by its last segment (root_space).
		segment := key[strings.LastIndex(key, ".")+1:]
		for _, s := range said {
			if s == "" || strings.Contains(s, key) || strings.Contains(s, segment) {
				t.Errorf("%s is refused in %q, which is no person's sentence", key, s)
			}
		}
	}
}

// THE TWO CLOSED SETS ARE EVERY ONE OF THEIR CONSTANTS, so a row whose group
// or kind the client has no case for cannot be added unnoticed.
func TestEverySettingRowNamesAKnownGroupAndKind(t *testing.T) {
	t.Parallel()
	for key, row := range settingsOf(t, hosted) {
		if !slices.Contains(queries.SettingGroups, row.Group) {
			t.Errorf("%s is in group %q, which SettingGroups does not list", key, row.Group)
		}
		if !slices.Contains(queries.SettingKinds, row.Kind) {
			t.Errorf("%s is of kind %q, which SettingKinds does not list", key, row.Kind)
		}
		if fmt.Sprintf("%T", row.Value) != fmt.Sprintf("%T", row.Default) {
			t.Errorf("%s answers a %T beside a default of %T", key, row.Value, row.Default)
		}
	}
}
