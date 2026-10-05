package claudemodel_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/anthropic/claudemodel"
)

// facts is every current id the API reference names, with what it says each
// accepts. The table is held to THIS, row by row, so a row edited away from
// the reference goes red here rather than as a 400 in somebody's company.
//
// bound is whether the model runs the conversation check on replayed
// thinking: the vendor names Fable 5.1, Opus 5.5 and Sonnet 5.5, says in as
// many words that Mythos 5.1 does not, and dates the check to Fable 5.1, after
// Fable 5 and Mythos 5. A row that says false for a model
// that runs it replays reasoning a tool change invalidated — a 400 on an
// enforced account — and one that says true for a model that does not sheds
// reasoning that was still valid.
var facts = []struct {
	id       string
	thinking claudemodel.Thinking
	efforts  []claudemodel.Effort
	sampling bool
	bound    bool
}{
	{"claude-fable-5-1", claudemodel.ThinkingAdaptive, all, false, true},
	{"claude-mythos-5-1", claudemodel.ThinkingAdaptive, all, false, false},
	{"claude-fable-5", claudemodel.ThinkingAdaptive, all, false, false},
	{"claude-mythos-5", claudemodel.ThinkingAdaptive, all, false, false},
	{"claude-opus-5-5", claudemodel.ThinkingAdaptive, all, false, true},
	{"claude-opus-5", claudemodel.ThinkingAdaptive, all, false, false},
	{"claude-opus-4-8", claudemodel.ThinkingAdaptive, all, false, false},
	{"claude-opus-4-7", claudemodel.ThinkingAdaptive, all, false, false},
	{"claude-opus-4-6", claudemodel.ThinkingAdaptive, noXHigh, true, false},
	{"claude-sonnet-5-5", claudemodel.ThinkingAdaptive, all, false, true},
	{"us.anthropic.claude-sonnet-5-5-v1:0", claudemodel.ThinkingAdaptive, all, false, true},
	{"claude-sonnet-5", claudemodel.ThinkingAdaptive, all, false, false},
	{"claude-sonnet-4-6", claudemodel.ThinkingAdaptive, noXHigh, true, false},
	{"claude-haiku-4-5", claudemodel.ThinkingBudget, nil, true, false},
	{"claude-haiku-4-5-20251001", claudemodel.ThinkingBudget, nil, true, false},
}

var (
	all     = []claudemodel.Effort{"low", "medium", "high", "xhigh", "max"}
	noXHigh = []claudemodel.Effort{"low", "medium", "high", "max"}
)

func TestEveryReferenceIDResolvesToWhatTheReferenceSays(t *testing.T) {
	t.Parallel()
	for _, fact := range facts {
		got, ok := claudemodel.Lookup(fact.id)
		if !ok {
			t.Errorf("%s: not in the table — it would be shaped as Modern", fact.id)
			continue
		}
		if got.Thinking != fact.thinking {
			t.Errorf("%s: thinking = %s, want %s", fact.id, got.Thinking, fact.thinking)
		}
		if !slices.Equal(got.Efforts, fact.efforts) {
			t.Errorf("%s: efforts = %v, want %v", fact.id, got.Efforts, fact.efforts)
		}
		if got.Sampling != fact.sampling {
			t.Errorf("%s: sampling = %v, want %v", fact.id, got.Sampling, fact.sampling)
		}
		if got.PrefixBinding != fact.bound {
			t.Errorf("%s: prefix binding = %v, want %v", fact.id, got.PrefixBinding, fact.bound)
		}
	}
}

// The output caps the reference states: 128K for Fable, for Mythos and for Opus
// and Sonnet from 4.6 on, 64K for Haiku 4.5. Every other row's cap is marked unverified
// at its declaration, and the doctor checks it against the Models API.
func TestTheStatedOutputCapsAreTheReferences(t *testing.T) {
	t.Parallel()
	for id, want := range map[string]int{
		"claude-fable-5-1": 128000, "claude-fable-5": 128000,
		"claude-mythos-5-1": 128000, "claude-mythos-5": 128000,
		"claude-opus-5-5": 128000, "claude-opus-5": 128000, "claude-opus-4-8": 128000,
		"claude-opus-4-7": 128000, "claude-opus-4-6": 128000,
		"claude-sonnet-5-5": 128000, "claude-sonnet-5": 128000, "claude-sonnet-4-6": 128000,
		"claude-haiku-4-5": 64000,
	} {
		if got, _ := claudemodel.Lookup(id); got.MaxOutput != want {
			t.Errorf("%s: MaxOutput = %d, want %d", id, got.MaxOutput, want)
		}
	}
}

// The budget generation: adaptive is a 400 there, Opus 4.5 takes three effort
// levels and the rest none.
func TestTheBudgetGenerationIsShapedAsOne(t *testing.T) {
	t.Parallel()
	for id, efforts := range map[string][]claudemodel.Effort{
		"claude-opus-4-5":   {"low", "medium", "high"},
		"claude-sonnet-4-5": nil,
		"claude-opus-4-1":   nil,
		"claude-opus-4-0":   nil,
		"claude-opus-4":     nil,
		"claude-sonnet-4-0": nil,
		"claude-sonnet-4":   nil,
		"claude-3-7-sonnet": nil,
	} {
		got, ok := claudemodel.Lookup(id)
		if !ok || got.Thinking != claudemodel.ThinkingBudget || !got.Sampling ||
			!slices.Equal(got.Efforts, efforts) || got.MaxOutput <= 0 || got.PrefixBinding {
			t.Errorf("%s: %+v (known %v), want the budget shape with efforts %v", id, got, ok, efforts)
		}
	}
}

// An alias files under its canonical id, so a log line and the doctor name the
// same model whichever spelling the config used.
func TestAnAliasAnswersAsItsCanonicalRow(t *testing.T) {
	t.Parallel()
	for alias, canonical := range map[string]string{
		"claude-opus-4":                       "claude-opus-4-0",
		"claude-opus-4-20250514":              "claude-opus-4-0",
		"claude-sonnet-4-20250514":            "claude-sonnet-4-0",
		"claude-3-7-sonnet-latest":            "claude-3-7-sonnet",
		"anthropic.claude-opus-5-5":           "claude-opus-5-5",
		"claude-haiku-4-5-20251001":           "claude-haiku-4-5",
		"us.anthropic.claude-sonnet-5-5-v1:0": "claude-sonnet-5-5",
	} {
		got, ok := claudemodel.Lookup(alias)
		if !ok || got.ID != canonical {
			t.Errorf("Lookup(%q) = %q (known %v), want %q", alias, got.ID, ok, canonical)
		}
	}
}

func TestNormalizeReadsEveryDeploymentSpelling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		// First party, as written.
		{"claude-opus-5-5", "claude-opus-5-5"},
		{"  claude-opus-5-5\n", "claude-opus-5-5"},
		// Dated snapshots and the moving alias.
		{"claude-opus-4-5-20251101", "claude-opus-4-5"},
		{"claude-3-7-sonnet-latest", "claude-3-7-sonnet"},
		// Bedrock: the bare model id, a regional or global inference
		// profile, the version suffix, and both ARN forms.
		{"anthropic.claude-opus-5-5", "claude-opus-5-5"},
		{"anthropic.claude-haiku-4-5-20251001-v1:0", "claude-haiku-4-5"},
		{"us.anthropic.claude-sonnet-4-5-20250929-v1:0", "claude-sonnet-4-5"},
		{"global.anthropic.claude-opus-4-6-v1", "claude-opus-4-6"},
		{"apac.anthropic.claude-sonnet-4-20250514-v1:0", "claude-sonnet-4"},
		{"arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-3-7-sonnet-20250219-v1:0",
			"claude-3-7-sonnet"},
		{"arn:aws:bedrock:eu-west-1:123456789012:inference-profile/eu.anthropic.claude-opus-4-1-20250805-v1:0",
			"claude-opus-4-1"},
		// Vertex: the bare id, an `@` snapshot, and a full resource path.
		{"claude-opus-4-5@20251101", "claude-opus-4-5"},
		{"projects/p/locations/global/publishers/anthropic/models/claude-sonnet-5-5@20260901",
			"claude-sonnet-5-5"},
		// A context tag, alone and stacked with the other suffixes in
		// either order.
		{"claude-sonnet-4-5[1m]", "claude-sonnet-4-5"},
		{"claude-sonnet-4-5-20250929[1m]", "claude-sonnet-4-5"},
		{"us.anthropic.claude-sonnet-4-5-20250929-v1:0[1m]", "claude-sonnet-4-5"},
		// Nothing to strip from an id the table does not know: it is read
		// as written, never trimmed toward a known one.
		{"claude-opus-5-7", "claude-opus-5-7"},
		{"my-gateway-alias", "my-gateway-alias"},
		{"", ""},
	} {
		if got := claudemodel.Normalize(tc.in); got != tc.want {
			t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// NO PREFIX FALLBACK. A future model matched to the nearest row would inherit
// an older model's looser rules — newer models only ever remove what a
// request may carry — so an unknown id is Modern, whatever it resembles.
func TestAnUnknownIDIsModernNeverTheNearestRow(t *testing.T) {
	t.Parallel()
	for _, id := range []string{
		"claude-opus-5-7", "claude-opus-5-5-preview", "claude-haiku-4", "claude-haiku-4-5x",
		"my-gateway-alias", "",
	} {
		got, ok := claudemodel.Lookup(id)
		if ok {
			t.Errorf("Lookup(%q) matched row %q, want it unknown", id, got.ID)
			continue
		}
		if got.ID != "" || got.Thinking != claudemodel.Modern.Thinking ||
			got.Sampling != claudemodel.Modern.Sampling || got.MaxOutput != claudemodel.Modern.MaxOutput ||
			got.PrefixBinding != claudemodel.Modern.PrefixBinding ||
			!slices.Equal(got.Efforts, claudemodel.Modern.Efforts) {
			t.Errorf("Lookup(%q) = %+v, want Modern", id, got)
		}
	}
}

// MODERN IS WHAT THE NEWEST MODELS ACCEPT, which is what makes it safe for a
// model released after the table: no sampling parameter (a 400 on every
// current Opus, Fable and Sonnet 5), adaptive thinking sent explicitly, and an
// output cap no current model refuses.
func TestModernIsSafeOnEveryCurrentModel(t *testing.T) {
	t.Parallel()
	if claudemodel.Modern.Sampling {
		t.Error("Modern sends a sampling parameter, a 400 on the current generation")
	}
	if claudemodel.Modern.Thinking != claudemodel.ThinkingAdaptive {
		t.Errorf("Modern thinking = %s, want adaptive", claudemodel.Modern.Thinking)
	}
	if !slices.Equal(claudemodel.Modern.Efforts, all) {
		t.Errorf("Modern efforts = %v, want every level", claudemodel.Modern.Efforts)
	}
	// A model released after the table most likely binds its thinking, as
	// the three newest do, and the two errors are not alike: replaying
	// reasoning a tool change invalidated is a 400 the chain does not
	// retry, while shedding reasoning that was still valid only loses it.
	if !claudemodel.Modern.PrefixBinding {
		t.Error("Modern replays reasoning a tool change invalidated, a 400 on the newest models")
	}
	for _, fact := range facts {
		row, _ := claudemodel.Lookup(fact.id)
		if claudemodel.Modern.MaxOutput > row.MaxOutput {
			t.Errorf("Modern's cap %d is above %s's %d", claudemodel.Modern.MaxOutput,
				fact.id, row.MaxOutput)
		}
	}
}

// Every row is one the backend can map: a known thinking mode, known effort
// levels in ascending order with no repeats, and a positive cap.
func TestEveryRowIsWellFormed(t *testing.T) {
	t.Parallel()
	ids := claudemodel.IDs()
	if len(ids) == 0 {
		t.Fatal("the table is empty")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Errorf("%s is filed twice", id)
		}
		seen[id] = true
		if !claudemodel.Known(id) {
			t.Errorf("IDs lists %s but Known does not", id)
		}
		if claudemodel.Normalize(id) != id {
			t.Errorf("%s is not its own normal form, so no spelling can reach it", id)
		}
		row, _ := claudemodel.Lookup(id)
		if !row.Thinking.Valid() || row.MaxOutput <= 0 || !claudemodel.Known(row.ID) {
			t.Errorf("%s: malformed row %+v", id, row)
		}
		last := -1
		for _, effort := range row.Efforts {
			at := slices.Index(claudemodel.Efforts, effort)
			if !effort.Valid() || at <= last {
				t.Errorf("%s: efforts %v are not distinct known levels, lowest first", id, row.Efforts)
				break
			}
			last = at
		}
	}
	// Known is EXACT: it is what a claude_model override must name, and an
	// override exists for the id Lookup could not read.
	for _, id := range []string{"claude-opus-5-5-20260101", "anthropic.claude-opus-5-5", "Claude-Opus-5-5"} {
		if claudemodel.Known(id) {
			t.Errorf("Known(%q) = true, want only the table's own ids", id)
		}
	}
}

// A caller that edits its profile does not edit the table.
func TestALookedUpProfileIsTheCallersOwn(t *testing.T) {
	t.Parallel()
	first, _ := claudemodel.Lookup("claude-opus-5-5")
	if len(first.Efforts) == 0 {
		t.Fatal("claude-opus-5-5 has no effort levels to edit")
	}
	first.Efforts[0] = "max"
	again, _ := claudemodel.Lookup("claude-opus-5-5")
	if again.Efforts[0] != claudemodel.EffortLow {
		t.Errorf("a caller's edit reached the table: %v", again.Efforts)
	}
	modern, _ := claudemodel.Lookup("unknown")
	modern.Efforts[0] = "max"
	if claudemodel.Modern.Efforts[0] != claudemodel.EffortLow {
		t.Errorf("a caller's edit reached Modern: %v", claudemodel.Modern.Efforts)
	}
}

func TestTheEnumsAreClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		valid bool
		got   bool
		name  string
	}{
		{true, claudemodel.ThinkingAdaptive.Valid(), "adaptive"},
		{true, claudemodel.ThinkingBudget.Valid(), "budget"},
		{false, claudemodel.Thinking("enabled").Valid(), "enabled — the wire spelling, not the mode"},
		{false, claudemodel.Thinking("").Valid(), "empty thinking"},
		{true, claudemodel.EffortXHigh.Valid(), "xhigh"},
		{false, claudemodel.Effort("x-high").Valid(), "x-high"},
		{false, claudemodel.Effort("").Valid(), "empty effort"},
		{false, claudemodel.Effort("HIGH").Valid(), "HIGH — not case-folded"},
	} {
		if tc.got != tc.valid {
			t.Errorf("%s: Valid() = %v, want %v", tc.name, tc.got, tc.valid)
		}
	}
	for _, effort := range claudemodel.Efforts {
		if !effort.Valid() {
			t.Errorf("Efforts lists %q, which Valid refuses", effort)
		}
	}
}
