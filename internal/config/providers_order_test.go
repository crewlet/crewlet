package config_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

const orderedDoc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
    alpha:
      type: anthropic
      model: claude-haiku-4-5
      api_keys: ["${K}"]
    mike:
      type: openai
      model: gpt-5
      api_keys: ["${K}"]
roles:
  - name: CEO
    handle: ceo
`

func TestDeclarationOrderSurvivesTheParse(t *testing.T) {
	t.Parallel()
	// Resolution's last resort is "the first provider configured". Over a
	// Go map that is no answer at all: two seats booted from one config
	// would land on different models and one seat would change model across
	// a restart.
	c, err := config.ParseCompany([]byte(orderedDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"zulu", "alpha", "mike"}
	if got := c.Providers.ProviderOrder(); !slices.Equal(got, want) {
		t.Errorf("order = %v, want the declared %v", got, want)
	}
	// And it is stable, which a map range is not.
	for range 50 {
		if got := c.Providers.ProviderOrder(); !slices.Equal(got, want) {
			t.Fatalf("order is unstable: %v then %v", want, got)
		}
	}
}

func TestDeclarationOrderSurvivesTheStoredForm(t *testing.T) {
	t.Parallel()
	// THE PATH THAT MATTERS. A company config is stored as JSON and re-read
	// on every boot and every rollout, so an order recovered only at
	// YAML-parse time is an order the running engine never sees.
	c, err := config.ParseCompany([]byte(orderedDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	blob, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back config.Company
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{"zulu", "alpha", "mike"}
	if got := back.Providers.ProviderOrder(); !slices.Equal(got, want) {
		t.Errorf("order after a JSON round trip = %v, want %v", got, want)
	}
}

func TestADocumentWithNoRecordedOrderIsStillDeterministic(t *testing.T) {
	t.Parallel()
	// Hand-written JSON, or a revision stored before the order existed. The
	// answer is arbitrary — an operator's first-listed provider is the one
	// they think of as primary, and sorting does not know that — but
	// arbitrary-and-stable is the only honest answer when the order was
	// never recorded, and it beats a map range that answers differently
	// every call.
	var c config.Company
	if err := json.Unmarshal([]byte(`{
		"name": "Acme",
		"providers": {"llm": {
			"zulu":  {"type":"anthropic","model":"m","api_keys":["k"]},
			"alpha": {"type":"anthropic","model":"m","api_keys":["k"]},
			"mike":  {"type":"anthropic","model":"m","api_keys":["k"]}
		}}}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{"alpha", "mike", "zulu"}
	for range 50 {
		if got := c.Providers.ProviderOrder(); !slices.Equal(got, want) {
			t.Fatalf("order = %v, want the stable sorted %v", got, want)
		}
	}
}

func TestTheOrderIsReconciledAgainstTheMap(t *testing.T) {
	t.Parallel()
	// The two travel separately through JSON and an operator editing a
	// stored revision can desync them. Every configured provider must stay
	// reachable, and a name the order invents must not appear.
	var c config.Company
	if err := json.Unmarshal([]byte(`{
		"name": "Acme",
		"providers": {
			"llm_order": ["ghost", "zulu", "zulu"],
			"llm": {
				"zulu":  {"type":"anthropic","model":"m","api_keys":["k"]},
				"alpha": {"type":"anthropic","model":"m","api_keys":["k"]}
			}}}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := c.Providers.ProviderOrder()
	if !slices.Equal(got, []string{"zulu", "alpha"}) {
		t.Errorf("order = %v, want the named survivor then the sorted remainder", got)
	}
	// Always a permutation of the map's keys: nothing invented, nothing
	// dropped, nothing repeated.
	if len(got) != len(c.Providers.LLM) {
		t.Errorf("order has %d entries for %d providers", len(got), len(c.Providers.LLM))
	}
	seen := map[string]bool{}
	for _, k := range got {
		if seen[k] {
			t.Errorf("order repeats %q", k)
		}
		seen[k] = true
		if _, ok := c.Providers.LLM[k]; !ok {
			t.Errorf("order names %q, which is not configured", k)
		}
	}
}

func TestAnEmptyProviderMapOrdersToNothing(t *testing.T) {
	t.Parallel()
	// The documented authoring state: an org chart written before the
	// credentials exist.
	var p config.Providers
	if got := p.ProviderOrder(); len(got) != 0 {
		t.Errorf("order = %v, want nothing", got)
	}
}

func TestTheProvidersBlockStillRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	// A custom UnmarshalYAML that reaches for node.Decode gets a fresh,
	// LENIENT decoder — which would make this one block the hole a typo
	// hides in, while every other block in the document stayed strict.
	_, err := config.ParseCompany([]byte(`
name: Acme
providers:
  embeddigns:
    type: openai
    model: text-embedding-3-small
roles:
  - name: CEO
    handle: ceo
`))
	if err == nil {
		t.Fatal("a misspelt providers field was accepted")
	}
	if !strings.Contains(err.Error(), "embeddigns") {
		t.Errorf("the error does not name the unknown field: %v", err)
	}
}

func TestAMergeKeyKeepsItsProvidersInPlace(t *testing.T) {
	t.Parallel()
	// `<<: *shared` is how an operator shares a block of providers between
	// documents. yaml.v3 resolves it during DECODE, so the map ends up with
	// the merged entries while a naive read of the raw node sees only the
	// literal `<<` — and those providers then fell off the order and were
	// appended by the sorted-tail fallback, silently moving whichever one
	// the operator listed first.
	//
	// Measured before the expansion existed: this document ordered
	// [alpha zulu].
	c, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    <<: &base
      zulu:
        type: anthropic
        model: m
        api_keys: ["${K}"]
    alpha:
      type: anthropic
      model: m
      api_keys: ["${K}"]
roles:
  - name: CEO
    handle: ceo
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(c.Providers.LLM) != 2 {
		t.Fatalf("the merge did not reach the map: %d providers", len(c.Providers.LLM))
	}
	if got := c.Providers.ProviderOrder(); !slices.Equal(got, []string{"zulu", "alpha"}) {
		t.Errorf("order = %v, want the merged provider first as written", got)
	}
}

func TestAMergedSequenceExpandsLeftToRight(t *testing.T) {
	t.Parallel()
	// A merge value may be a SEQUENCE of mappings, and YAML merges them
	// left to right. Handling only the single-mapping form drops every
	// provider after the first block.
	//
	// This document also pins the regression that moved the order read out
	// of a custom decoder: the anchors live OUTSIDE the providers block, and
	// the package's strict decoder serialises a subtree alone — so reading
	// the order from an UnmarshalYAML on Providers turned this into
	// "unknown anchor 'one' referenced" on a file that had parsed fine.
	c, err := config.ParseCompany([]byte(`
name: Acme
roles:
  - name: CEO
    handle: ceo
    llm: &shared_chain [zulu, mike]
  - name: CTO
    handle: cto
    llm: *shared_chain
providers:
  llm:
    zulu:
      type: anthropic
      model: m
      api_keys: ["${K}"]
    mike:
      type: anthropic
      model: m
      api_keys: ["${K}"]
    alpha:
      type: anthropic
      model: m
      api_keys: ["${K}"]
`))
	if err != nil {
		t.Fatalf("an alias to an anchor outside the providers block broke the parse: %v", err)
	}
	if got := c.Providers.ProviderOrder(); !slices.Equal(got, []string{"zulu", "mike", "alpha"}) {
		t.Errorf("order = %v, want the declared order", got)
	}
}

func TestMergeKeysExpandThroughTheKeyReader(t *testing.T) {
	t.Parallel()
	// The sequence form of a merge, exercised where it is reachable: inside
	// the llm map itself, where an anchor can legally be defined.
	c, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    <<: &first
      zulu:
        type: anthropic
        model: m
        api_keys: ["${K}"]
    alpha:
      type: anthropic
      model: m
      api_keys: ["${K}"]
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(c.Providers.LLM) != 2 {
		t.Fatalf("the merge did not reach the map: %d providers", len(c.Providers.LLM))
	}
	if got := c.Providers.ProviderOrder(); !slices.Equal(got, []string{"zulu", "alpha"}) {
		t.Errorf("order = %v, want the merged provider first as written", got)
	}
}

func TestASequenceMergeExpandsLeftToRight(t *testing.T) {
	t.Parallel()
	// A merge value may be a SEQUENCE of mappings, which YAML merges left
	// to right. Handling only the single-mapping form drops every provider
	// after the first block — silently, since they still reach the map and
	// only their ORDER is lost.
	//
	// The anchors are defined at their point of use inside the sequence,
	// which is the one place in a valid company document a provider-map
	// anchor can live: every top-level key is known and typed, so an
	// anchor-holder key is rejected as an unknown setting.
	c, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    <<:
      - &a
        zulu:
          type: anthropic
          model: m
          api_keys: ["${K}"]
      - &b
        mike:
          type: anthropic
          model: m
          api_keys: ["${K}"]
    alpha:
      type: anthropic
      model: m
      api_keys: ["${K}"]
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(c.Providers.LLM) != 3 {
		t.Fatalf("the merge did not reach the map: %d providers", len(c.Providers.LLM))
	}
	if got := c.Providers.ProviderOrder(); !slices.Equal(got, []string{"zulu", "mike", "alpha"}) {
		t.Errorf("order = %v, want left-to-right then the literal", got)
	}
}

func TestAnAliasInsideAMergeIsFollowed(t *testing.T) {
	t.Parallel()
	// The key reader walks raw YAML nodes, where an alias is a POINTER and
	// not the mapping it names. Not following it yields no keys for that
	// item, so the providers it carries fall out of the order and land in
	// the sorted tail.
	//
	// Reachable exactly here: an anchor defined at its point of use in the
	// merge sequence, aliased later in the same sequence.
	c, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    <<:
      - &shared
        zulu:
          type: anthropic
          model: m
          api_keys: ["${K}"]
      - *shared
    alpha:
      type: anthropic
      model: m
      api_keys: ["${K}"]
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// The repeat is collapsed by the reconciliation, so the order is a
	// permutation of the map's keys however many times a merge names one.
	if got := c.Providers.ProviderOrder(); !slices.Equal(got, []string{"zulu", "alpha"}) {
		t.Errorf("order = %v, want [zulu alpha]", got)
	}
}
