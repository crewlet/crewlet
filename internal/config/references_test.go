package config_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// referencesDoc writes the entry `big` in every shape a document can name a
// providers.llm entry in: a scalar llm, a list llm, the llm mapping's default
// beside another phase and each of its satellites, each flat satellite, a seat
// at the root and seats nested two units deep, the assistant's chain and its
// deep work. `other` is named beside it, so a chain is reported whole.
const referencesDoc = `
name: Acme
providers:
  llm:
    big: {model: sonnet}
    other: {model: haiku}
roles:
  - name: Scalar
    handle: scalar
    llm: big
  - name: Untouched
    handle: untouched
    llm: other
units:
  - name: Eng
    id: eng
    roles:
      - name: Listed
        handle: listed
        llm: [other, big]
    children:
      - name: Platform
        id: platform
        roles:
          - name: Mapped
            handle: mapped
            llm:
              default: big
              review: [big, other]
              subagent: big
              auxiliary: big
              judge: big
          - name: Flat
            handle: flat
            llm_review: big
            llm_subagent: [other, big]
            llm_auxiliary: big
            llm_judge: big
assistant:
  llm: [other, big]
  deep_work:
    llm: big
`

// EVERY FIELD THAT NAMES AN ENTRY IS A REFERENCE TO IT, at the path it is
// written at, with the job it does and the whole chain it writes. One list
// serves validation, a removal and the account of who uses an entry: a shape
// left out of it is one a removal leaves dangling and a typo validation never
// sees.
func TestProviderReferencesCoverEveryWrittenShape(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseCompanyDocument([]byte(referencesDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	type ref struct {
		seat     string
		job      config.ProviderJob
		segments string
		chain    []string
	}
	want := []ref{
		{"scalar", config.JobExecutor, "roles[0].llm", []string{"big"}},
		{"listed", config.JobExecutor, "units[0].roles[0].llm", []string{"other", "big"}},
		// Beside another phase the default is stored as `llm.default`, so
		// that is where an edit finds it.
		{"mapped", config.JobExecutor, "units[0].children[0].roles[0].llm.default", []string{"big"}},
		{"mapped", config.JobReview, "units[0].children[0].roles[0].llm.review", []string{"big", "other"}},
		{"mapped", config.JobSubagent, "units[0].children[0].roles[0].llm.subagent", []string{"big"}},
		{"mapped", config.JobAuxiliary, "units[0].children[0].roles[0].llm.auxiliary", []string{"big"}},
		{"mapped", config.JobJudge, "units[0].children[0].roles[0].llm.judge", []string{"big"}},
		{"flat", config.JobReview, "units[0].children[0].roles[1].llm_review", []string{"big"}},
		{"flat", config.JobSubagent, "units[0].children[0].roles[1].llm_subagent", []string{"other", "big"}},
		{"flat", config.JobAuxiliary, "units[0].children[0].roles[1].llm_auxiliary", []string{"big"}},
		{"flat", config.JobJudge, "units[0].children[0].roles[1].llm_judge", []string{"big"}},
		{"", config.JobAssistant, "assistant.llm", []string{"other", "big"}},
		{"", config.JobDeepWork, "assistant.deep_work.llm", []string{"big"}},
	}
	var got []ref
	for _, r := range cfg.ProviderReferences("big") {
		if !r.Job.Valid() {
			t.Errorf("reference %+v carries a job outside the closed set", r)
		}
		got = append(got, ref{r.Seat, r.Job, r.Segments.String(), r.Chain})
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ProviderReferences(big):\n got %+v\nwant %+v", got, want)
	}

	// A key is asked about alone: `other`'s references are the fields that
	// name it, and the seat that names only it is among them.
	var seats []string
	for _, r := range cfg.ProviderReferences("other") {
		seats = append(seats, r.Seat)
	}
	if !slices.Equal(seats, []string{"untouched", "listed", "mapped", "flat", ""}) {
		t.Errorf("ProviderReferences(other) seats = %q", seats)
	}
	if refs := cfg.ProviderReferences("missing"); len(refs) != 0 {
		t.Errorf("an entry nothing names has references: %+v", refs)
	}
}

// THE SEGMENTS ARE THE PATH A PROBLEM CARRIES: keys as strings, list indexes
// as ints, so a reader that edits the field and one that reports it address it
// the same way.
func TestAProviderReferenceAddressesItsFieldAsAProblemDoes(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseCompanyDocument([]byte(referencesDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, r := range cfg.ProviderReferences("big") {
		if r.Segments.String() != "units[0].children[0].roles[1].llm_review" {
			continue
		}
		want := []any{"units", 0, "children", 0, "roles", 1, "llm_review"}
		if !reflect.DeepEqual([]any(r.Segments), want) {
			t.Errorf("segments = %#v, want %#v", r.Segments, want)
		}
		return
	}
	t.Fatal("no reference at units[0].children[0].roles[1].llm_review")
}

// THE SCALAR FORM AND A MAPPING HOLDING ONLY default ARE ONE DOCUMENT, stored
// as the scalar, so both are reported at `llm`.
func TestAMappingWithOnlyADefaultIsWrittenAsLLM(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(referencesDoc, "    llm: big\n  - name: Untouched",
		"    llm: {default: big}\n  - name: Untouched", 1)
	cfg, err := config.ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	refs := cfg.ProviderReferences("big")
	if len(refs) == 0 || refs[0].Segments.String() != "roles[0].llm" {
		t.Errorf("first reference = %+v, want roles[0].llm", refs)
	}
}
