package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// The stored form is the one an engine actually boots from: `crewlet config
// import` parses a person's YAML once, and every node from then on reads the
// JSON in company_config. A document that does not survive that trip is a
// company nobody can run — and the failure appears at a node's first boot,
// not at the import that caused it.

func TestTheAuthoredCompanySurvivesTheStoredForm(t *testing.T) {
	t.Parallel()
	// The EXAMPLE company, not a fixture: it is the largest config in the
	// repository and the one exercising most of the schema, so it is the
	// strongest available evidence that marshalling and re-reading is
	// lossless. A hand-written fixture proves the round trip for the
	// fields the author of the fixture happened to think of.
	authored, err := config.ParseCompany(exampleCompany(t))
	if err != nil {
		t.Fatalf("parse the example: %v", err)
	}
	payload, err := json.Marshal(authored)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	stored, err := config.DecodeCompany(payload)
	if err != nil {
		t.Fatalf("decode the stored form: %v", err)
	}
	if !reflect.DeepEqual(authored, stored) {
		t.Error("the company changed on its way through the store")
		// Narrow it down to a field rather than printing two enormous
		// structs: the whole document is thousands of lines.
		reportFirstDifference(t, authored, stored)
	}
}

func TestTheStoredFormCarriesTheDeclarationOrder(t *testing.T) {
	t.Parallel()
	// A Go map has no order, and per-phase resolution's last resort is
	// "the first provider configured". An order recovered only at
	// YAML-parse time is an order the running engine never sees, so two
	// nodes booted from one revision would resolve an unpinned seat to
	// different models.
	authored, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    zulu: {type: anthropic, model: m, api_keys: ["${K}"]}
    alpha: {type: anthropic, model: m, api_keys: ["${K}"]}
    mike: {type: anthropic, model: m, api_keys: ["${K}"]}
roles:
  - {name: CEO, handle: ceo, llm: zulu}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	payload, err := json.Marshal(authored)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := config.DecodeCompany(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []string{"zulu", "alpha", "mike"}
	if got := stored.Providers.ProviderOrder(); !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want the declared order %v", got, want)
	}

	// The AUTHORED reader must accept it too, and preserve the same order.
	// The config read surface emits this document and has to accept it
	// back: a reader that refused its own output would make GET-edit-PUT
	// reject every config that has providers, which is all of them.
	//
	// And the order has to come from the FIELD rather than the key order,
	// because Go marshals a map with sorted keys — so the document says
	// alpha, mike, zulu while the company means zulu, alpha, mike.
	reread, err := config.ParseCompany(payload)
	if err != nil {
		t.Fatalf("the authored reader refused the stored form: %v", err)
	}
	if got := reread.Providers.ProviderOrder(); !reflect.DeepEqual(got, want) {
		t.Errorf("order after the authored reader = %v, want %v — the key "+
			"order of the serialized form won over the recorded one", got, want)
	}
}

func TestAnAuthoredOrderWinsOverTheKeyOrder(t *testing.T) {
	t.Parallel()
	// Stating it explicitly is how an operator pins precedence without
	// re-arranging their document, and it is the same rule the round trip
	// depends on: the field wins, the key order is the fallback.
	cfg, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    llm_order: ["mike", "zulu"]
    zulu: {type: anthropic, model: m, api_keys: ["${K}"]}
    mike: {type: anthropic, model: m, api_keys: ["${K}"]}
roles:
  - {name: CEO, handle: ceo, llm: zulu}
`))
	if err == nil {
		t.Fatal("llm_order inside the llm map was accepted; it belongs beside it")
	}

	cfg, err = config.ParseCompany([]byte(`
name: Acme
providers:
  llm_order: ["mike", "zulu"]
  llm:
    zulu: {type: anthropic, model: m, api_keys: ["${K}"]}
    mike: {type: anthropic, model: m, api_keys: ["${K}"]}
roles:
  - {name: CEO, handle: ceo, llm: zulu}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"mike", "zulu"}
	if got := cfg.Providers.ProviderOrder(); !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want the stated %v rather than the key order", got, want)
	}
}

func TestAnUnknownFieldInAStoredRevisionIsToleratedNotFatal(t *testing.T) {
	t.Parallel()
	// A revision written by a NEWER build. Rejecting it makes a
	// mixed-version fleet an outage in the older direction — every node on
	// the previous build refuses to boot the moment one node upgrades and
	// activates.
	payload := []byte(`{"name":"Acme","some_future_setting":{"on":true},
	  "providers":{"llm":{"zulu":{"type":"anthropic","model":"m","api_keys":["k"]}}},
	  "roles":[{"name":"CEO","handle":"ceo","llm":"zulu"}]}`)
	cfg, err := config.DecodeCompany(payload)
	if err != nil {
		t.Fatalf("a newer build's revision was refused: %v", err)
	}
	if cfg.Name != "Acme" {
		t.Errorf("name = %q", cfg.Name)
	}
}

func TestAStoredRevisionIsStillValidated(t *testing.T) {
	t.Parallel()
	// Lenient about UNKNOWN fields is not lenient about a broken company.
	// A seat naming a provider the document does not configure is
	// well-formed JSON and fails at the first turn, which is the worst
	// place to learn it.
	//
	// The provider block has to be non-empty for this to be the fault under
	// test: a company with NO models at all is a documented authoring state
	// — an org chart written before the credentials exist — and validation
	// deliberately skips the key check there rather than answering with a
	// wall of errors about models the author has not added yet.
	_, err := config.DecodeCompany([]byte(
		`{"name":"Acme","providers":{"llm":{"zulu":{"type":"anthropic","model":"m","api_keys":["k"]}}},` +
			`"roles":[{"name":"CEO","handle":"ceo","llm":"nonexistent"}]}`))
	if err == nil {
		t.Fatal("a seat naming an unconfigured provider decoded")
	}
}

func TestAStoredRevisionGetsTheSameDefaults(t *testing.T) {
	t.Parallel()
	// Onto the defaults, not onto a zero value: the same company must not
	// behave differently depending on which door it came in through.
	authored, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    zulu: {type: anthropic, model: m, api_keys: ["${K}"]}
roles:
  - {name: CEO, handle: ceo, llm: zulu}
`))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := config.DecodeCompany([]byte(
		`{"name":"Acme","providers":{"llm":{"zulu":{"type":"anthropic","model":"m","api_keys":["${K}"]}}},` +
			`"roles":[{"name":"CEO","handle":"ceo","llm":"zulu"}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(authored.TurnEngine, stored.TurnEngine) {
		t.Errorf("turn-engine defaults differ:\n authored %+v\n stored   %+v",
			authored.TurnEngine, stored.TurnEngine)
	}
	if !reflect.DeepEqual(authored.Learning, stored.Learning) {
		t.Errorf("learning defaults differ:\n authored %+v\n stored   %+v",
			authored.Learning, stored.Learning)
	}
}

// exampleCompany reads the Nimbus example, the repository's largest config.
func exampleCompany(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "examples", "nimbus.company.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("the example company is not readable from here: %v", err)
	}
	return data
}

// reportFirstDifference names the field that changed, walking the two values
// in parallel. Printing the whole document instead would bury the answer.
func reportFirstDifference(t *testing.T, want, got any) {
	t.Helper()
	var walk func(path string, a, b reflect.Value)
	walk = func(path string, a, b reflect.Value) {
		if t.Failed() && path != "" && a.IsValid() && b.IsValid() &&
			reflect.DeepEqual(a.Interface(), b.Interface()) {
			return
		}
		switch a.Kind() {
		case reflect.Pointer, reflect.Interface:
			if a.IsNil() != b.IsNil() {
				t.Errorf("%s: one side is nil (want nil=%v, got nil=%v)", path, a.IsNil(), b.IsNil())
				return
			}
			if !a.IsNil() {
				walk(path, a.Elem(), b.Elem())
			}
		case reflect.Struct:
			for i := range a.NumField() {
				field := a.Type().Field(i)
				if field.IsExported() {
					walk(path+"."+field.Name, a.Field(i), b.Field(i))
				}
			}
		default:
			if !reflect.DeepEqual(a.Interface(), b.Interface()) {
				t.Errorf("%s: want %v, got %v", path, a.Interface(), b.Interface())
			}
		}
	}
	walk("", reflect.ValueOf(want), reflect.ValueOf(got))
}
