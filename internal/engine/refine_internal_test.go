package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/store"
)

// THE REFINEMENT KNOBS WERE CONFIG-ONLY.
//
// `learning.skill_refinement.enabled`, `auto_refine_on_success` and
// `auto_refine_on_failure` all validated, schema'd, shipped in the example
// company — and had no reader outside internal/config. Setting one produced a
// revision and changed nothing an operator could observe. These cases are the
// wiring that makes each of them do what its name says.

// learningCompany builds an epoch whose learning block is the given YAML.
func learningCompany(t *testing.T, block string) *Company {
	t.Helper()
	return companyFor(t, `
name: Acme
providers:
  llm:
    gateway:
      type: openai
      model: gpt-4o
      api_keys: ["${OPENAI_API_KEY}"]
learning:
`+block+`
roles:
  - name: Engineer
    handle: eng
    llm: gateway
`)
}

// THE CLUSTERING PASS IS WIRED ONLY WHEN scheduler_enabled SAYS SO, and it
// was wired never: `cluster_min_size`, `cluster_jaccard_threshold` and
// `episode_fetch_limit` validated and were read by nobody.
func TestTheClusteringPassFollowsSchedulerEnabled(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		block string
		want  bool
	}{
		{"off by default", "  skill_synthesis:\n    enabled: true\n", false},
		{"on when asked", "  skill_synthesis:\n    scheduler_enabled: true\n", true},
		{"off when synthesis is off",
			"  skill_synthesis:\n    enabled: false\n    scheduler_enabled: true\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := engineOver(t)
			if got := e.clusteringPass(learningCompany(t, tc.block)) != nil; got != tc.want {
				t.Fatalf("clustering wired = %v, want %v", got, tc.want)
			}
		})
	}
}

// THE COMPANY'S OWN CLUSTERING NUMBERS REACH THE WORKER, not the package
// defaults. All three validated and were read by nobody.
func TestTheClusteringPassTakesTheCompanysNumbers(t *testing.T) {
	t.Parallel()
	c := learningCompany(t, "  skill_synthesis:\n    scheduler_enabled: true\n"+
		"    cluster_min_size: 7\n    cluster_jaccard_threshold: 0.42\n"+
		"    episode_fetch_limit: 55\n    max_skills_per_agent: 9\n"+
		"    cluster_window_hours: 48\n")
	opts := synthesizerOptions(c.Config.Learning.SkillSynthesis)
	if opts.ClusterMinSize != 7 {
		t.Errorf("ClusterMinSize = %d, want the company's 7", opts.ClusterMinSize)
	}
	if opts.ClusterJaccardThreshold != 0.42 {
		t.Errorf("ClusterJaccardThreshold = %v, want the company's 0.42",
			opts.ClusterJaccardThreshold)
	}
	if opts.EpisodeFetchLimit != 55 {
		t.Errorf("EpisodeFetchLimit = %d, want the company's 55", opts.EpisodeFetchLimit)
	}
	if opts.ClusterWindow != 48*time.Hour {
		t.Errorf("ClusterWindow = %v, want the company's cluster_window_hours: 48",
			opts.ClusterWindow)
	}
	// THE SHARED GATES TOO: the inline worker and the clustering pass read
	// one mapping, so a raised cap cannot apply to only one of them.
	if opts.MaxSkillsPerAgent != 9 {
		t.Errorf("MaxSkillsPerAgent = %d, want the company's 9", opts.MaxSkillsPerAgent)
	}
	// Episodes belongs to the clustered path alone and is attached by the
	// caller: an inline worker built from this mapping must not carry one.
	if opts.Episodes != nil {
		t.Error("the shared mapping attached an episode store to the inline worker")
	}
}

// refinementCompany builds an epoch whose skill_refinement block is the given
// YAML.
func refinementCompany(t *testing.T, refinement string) *Company {
	t.Helper()
	return learningCompany(t, "  skill_refinement:\n"+refinement)
}

// engineOver is an engine with a real local store, which is what every
// learning worker is gated on.
func engineOver(t *testing.T) *Engine {
	t.Helper()
	return &Engine{backends: &Backends{Store: refinementStore(t)}}
}

func refinementStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), t.TempDir()+"/index.db", store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func workerNames(workers []learning.Worker) []string {
	names := make([]string, 0, len(workers))
	for _, w := range workers {
		names = append(names, w.Name())
	}
	return names
}

func has(names []string, want string) bool {
	return slices.Contains(names, want)
}

// THE AUTO REFINER IS WIRED BY DEFAULT — the worker whose absence made both
// auto_refine knobs inert.
func TestTheAutoRefinerIsWiredWhenRefinementIsOn(t *testing.T) {
	t.Parallel()
	e := engineOver(t)
	names := workerNames(e.buildReflectionWorkers(refinementCompany(t, "    enabled: true\n")))
	if !has(names, learning.RefinerSource) {
		t.Fatalf("workers = %v, want the %s among them", names, learning.RefinerSource)
	}
}

// TURNING REFINEMENT OFF TAKES THE WORKER WITH IT.
func TestRefinementOffLeavesNoRefiner(t *testing.T) {
	t.Parallel()
	e := engineOver(t)
	names := workerNames(e.buildReflectionWorkers(refinementCompany(t, "    enabled: false\n")))
	if has(names, learning.RefinerSource) {
		t.Fatalf("workers = %v, want no refiner when refinement is off", names)
	}
	// The rest of the learning write side is untouched: the knob is about
	// skills, not about whether the company learns at all.
	if !has(names, learning.EpisodistSource) {
		t.Fatalf("workers = %v — turning refinement off must not disable the others", names)
	}
}

// BOTH OUTCOME TOGGLES OFF IS REFINEMENT OFF, SPELLED THE LONG WAY. Building
// the worker anyway would cost a Skip on every turn in the company and report
// a reason that reads like a bug.
func TestBothOutcomeTogglesOffLeavesNoRefiner(t *testing.T) {
	t.Parallel()
	e := engineOver(t)
	names := workerNames(e.buildReflectionWorkers(refinementCompany(t,
		"    auto_refine_on_success: false\n    auto_refine_on_failure: false\n")))
	if has(names, learning.RefinerSource) {
		t.Fatalf("workers = %v, want no refiner when neither outcome is refined", names)
	}
}

// ONE OUTCOME OFF STILL WIRES THE WORKER, and the worker itself is what
// declines the other half — see TestTheOutcomeTogglesGateTheirOwnHalf.
func TestOneOutcomeOffStillWiresTheRefiner(t *testing.T) {
	t.Parallel()
	e := engineOver(t)
	for _, off := range []string{"auto_refine_on_success", "auto_refine_on_failure"} {
		names := workerNames(e.buildReflectionWorkers(refinementCompany(t,
			"    "+off+": false\n")))
		if !has(names, learning.RefinerSource) {
			t.Fatalf("%s: workers = %v, want the refiner still wired", off, names)
		}
	}
}

// THE COMPANY'S OWN NUMBERS REACH THE WORKER, not the package defaults. Each
// of these validated and did nothing before the worker existed, so a cap of
// 20 000 that ignored a company's 500 is the same failure returning one field
// at a time.
func TestTheRefinerTakesTheCompanysCaps(t *testing.T) {
	t.Parallel()
	c := refinementCompany(t, "    max_body_chars: 40\n"+
		"    budget_tokens: 77\n    max_versions_kept: 3\n")
	opts := refinerOptions(c.Config.Learning.SkillRefinement)
	if opts.MaxBodyChars != 40 {
		t.Errorf("MaxBodyChars = %d, want the company's 40", opts.MaxBodyChars)
	}
	if opts.MaxTokens != 77 {
		t.Errorf("MaxTokens = %d, want the company's budget_tokens", opts.MaxTokens)
	}
	if opts.KeepVersions != 3 {
		t.Errorf("KeepVersions = %d, want the company's max_versions_kept", opts.KeepVersions)
	}
}

// THE UNSET TOGGLE RESOLVES TO THE DOCUMENTED DEFAULT, and reaches the worker
// as a set pointer. Passing the Toggle's zero value straight through would
// leave the worker applying its own default rather than the company's answer
// — which is invisible while the two agree and wrong the moment they do not.
func TestTheOutcomeTogglesReachTheWorkerResolved(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                  string
		yaml                  string
		wantSuccess, wantFail bool
	}{
		{"unset defaults to both on", "    enabled: true\n", true, true},
		{"success off", "    auto_refine_on_success: false\n", false, true},
		{"failure off", "    auto_refine_on_failure: false\n", true, false},
		{"both spelled on",
			"    auto_refine_on_success: true\n    auto_refine_on_failure: true\n", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := refinerOptions(refinementCompany(t, tc.yaml).Config.Learning.SkillRefinement)
			if opts.OnSuccess == nil || opts.OnFailure == nil {
				t.Fatalf("a toggle arrived unset: %+v", opts)
			}
			if *opts.OnSuccess != tc.wantSuccess || *opts.OnFailure != tc.wantFail {
				t.Fatalf("success=%v failure=%v, want %v/%v",
					*opts.OnSuccess, *opts.OnFailure, tc.wantSuccess, tc.wantFail)
			}
		})
	}
}

// REFINEMENT OFF ALSO WITHDRAWS THE TOOL. The two halves write the same rows
// through the same version archive, so a company that turned refinement off
// and still had refine_skill would watch its skills change under a knob it
// had set to false.
func TestRefinementOffWithdrawsTheTool(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		refinement string
		want       bool
	}{
		{"    enabled: true\n", true},
		{"    enabled: false\n", false},
	} {
		e := engineOver(t)
		c := refinementCompany(t, tc.refinement)
		if err := e.equip(t.Context(), c); err != nil {
			t.Fatalf("equip: %v", err)
		}
		_, found := c.Tools.Lookup(builtin.RefineSkillTool)
		if found != tc.want {
			t.Fatalf("%s: refine_skill registered = %v, want %v",
				strings.TrimSpace(tc.refinement), found, tc.want)
		}
		// use_skill is not under this knob: reading a skill is not
		// changing one, and a company that stopped refining still wants
		// its agents loading what they already learned.
		if _, ok := c.Tools.Lookup(builtin.UseSkillTool); !ok {
			t.Fatalf("%s: use_skill went with it", strings.TrimSpace(tc.refinement))
		}
	}
}
