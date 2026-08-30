package learning_test

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/store"
)

// The SQL fragments the write-fault driver below keys on. Each names exactly
// one statement in skill.go, so a test can fail the archive without touching
// the body write, or the prune without touching either.
const (
	sqlArchive    = "INSERT INTO synthesized_skill_versions"
	sqlBodyUpdate = "SET description"
	sqlPrune      = "DELETE FROM synthesized_skill_versions"
	sqlTransition = "SET state"
	sqlMarkUsed   = "use_count = use_count + 1"
)

func skillStore(t *testing.T, opts ...func(*store.Options)) (*learning.Skills, *store.DB) {
	t.Helper()
	o := store.Options{}
	for _, fn := range opts {
		fn(&o)
	}
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "s.db"), o)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return learning.NewSkills(db), db
}

func newSkill(handle, name string, at time.Time) learning.Skill {
	return learning.Skill{
		ID:               "sk-" + handle + "-" + name,
		AgentHandle:      handle,
		Name:             name,
		Description:      name + " description",
		Content:          "# " + name + "\noriginal body",
		Frontmatter:      map[string]any{"name": name},
		ToolSequence:     []string{"read_file", "write_file"},
		SourceEpisodeIDs: []string{"ep-1", "ep-2"},
		CreatedAt:        at,
		UpdatedAt:        at,
	}
}

func mustInsert(t *testing.T, s *learning.Skills, sk learning.Skill) learning.Skill {
	t.Helper()
	if err := s.Insert(context.Background(), sk); err != nil {
		t.Fatalf("Insert %s: %v", sk.Name, err)
	}
	return sk
}

func mustSkill(t *testing.T, s *learning.Skills, id string) learning.Skill {
	t.Helper()
	sk, ok, err := s.ByID(context.Background(), id)
	if err != nil {
		t.Fatalf("ByID %s: %v", id, err)
	}
	if !ok {
		t.Fatalf("skill %s is gone", id)
	}
	return sk
}

func mustUpdate(t *testing.T, s *learning.Skills, id string, rev learning.Revision,
	r learning.Refinement,
) learning.Skill {
	t.Helper()
	sk, err := s.Update(context.Background(), id, rev, r)
	if err != nil {
		t.Fatalf("Update %s: %v", id, err)
	}
	return sk
}

func skillNames(sks []learning.Skill) []string {
	out := make([]string, len(sks))
	for i, sk := range sks {
		out[i] = sk.Name
	}
	return out
}

func versionNumbers(vs []learning.SkillVersion) []int {
	out := make([]int, len(vs))
	for i, v := range vs {
		out[i] = v.Version
	}
	return out
}

// ---------------------------------------------------------------------------
// One row per (seat, name)
// ---------------------------------------------------------------------------

func TestASecondDraftUnderOneNameIsRefusedRatherThanDuplicated(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()

	first := mustInsert(t, s, newSkill("alice", "triage", base))
	second := newSkill("alice", "triage", base.Add(time.Hour))
	second.ID = "sk-second"
	second.Content = "# triage\nan LLM redraft"

	err := s.Insert(ctx, second)
	if !errors.Is(err, learning.ErrSkillExists) {
		t.Fatalf("second draft: err = %v, want ErrSkillExists", err)
	}
	// The refusal must not have been a silent overwrite either: a redraft
	// landing on top of a refined, version-counted skill with no history row
	// is the outcome the unique index exists to make impossible.
	if got := mustSkill(t, s, first.ID); got.Content != first.Content {
		t.Errorf("content = %q, want the original %q", got.Content, first.Content)
	}
	n, err := s.Count(ctx, "alice", learning.ListOptions{})
	if err != nil || n != 1 {
		t.Errorf("count = %d (%v), want exactly one row", n, err)
	}

	// Counterfactuals: the index is on the PAIR, so neither half alone
	// collides.
	mustInsert(t, s, newSkill("alice", "escalate", base))
	mustInsert(t, s, newSkill("bob", "triage", base))
}

func TestASkillNeedsTimestampsItsReadersCanBelieve(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()

	missing := newSkill("alice", "triage", base)
	missing.CreatedAt = time.Time{}
	if err := s.Insert(ctx, missing); err == nil {
		t.Fatal("a skill with no creation time was accepted")
	}
	missing = newSkill("alice", "triage", base)
	missing.UpdatedAt = time.Time{}
	if err := s.Insert(ctx, missing); err == nil {
		t.Fatal("a skill with no update time was accepted")
	}
	// Counterfactual: the same row with real times inserts.
	mustInsert(t, s, newSkill("alice", "triage", base))

	// And this is what the refusal is for. A zero time is not a harmless
	// blank: the curator ages a skill from it and archives it on the first
	// pass it is old enough for, which for year 1 is every pass.
	zero := newSkill("alice", "ghost", time.Time{})
	zero.State = learning.SkillActive
	change, due := learning.CuratorPolicy{}.Next(zero, base)
	if !due || change.To != learning.SkillStale {
		t.Fatalf("a year-1 skill: due=%v to=%q, want an immediate stale", due, change.To)
	}
}

func TestAnUnknownStateIsRefusedAtInsert(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	sk := newSkill("alice", "triage", base)
	sk.State = "retired"
	if err := s.Insert(context.Background(), sk); !errors.Is(err, learning.ErrSkillState) {
		t.Fatalf("err = %v, want ErrSkillState", err)
	}
	// Counterfactual: each state the machine knows is accepted, and the
	// empty one defaults to active.
	for _, st := range []learning.SkillState{
		"", learning.SkillActive, learning.SkillStale, learning.SkillArchived,
	} {
		sk := newSkill("alice", "skill-"+string(st), base)
		sk.State = st
		mustInsert(t, s, sk)
		want := st
		if want == "" {
			want = learning.SkillActive
		}
		if got := mustSkill(t, s, sk.ID).State; got != want {
			t.Errorf("state %q stored as %q", st, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Visibility
// ---------------------------------------------------------------------------

func TestListingsHideArchivedAndKeepStale(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()

	live := newSkill("alice", "live", base)
	ageing := newSkill("alice", "ageing", base.Add(-time.Hour))
	ageing.State = learning.SkillStale
	gone := newSkill("alice", "gone", base.Add(-2*time.Hour))
	gone.State = learning.SkillArchived
	for _, sk := range []learning.Skill{live, ageing, gone} {
		mustInsert(t, s, sk)
	}

	for _, tc := range []struct {
		name string
		opts learning.ListOptions
		want []string
	}{
		// The zero value is the prefetch's question, and it is the one that
		// must be right by default: exactly the skills the loader accepts.
		{"default", learning.ListOptions{}, []string{"live", "ageing"}},
		{"stale excluded", learning.ListOptions{ExcludeStale: true}, []string{"live"}},
		{"archived included", learning.ListOptions{IncludeArchived: true},
			[]string{"live", "ageing", "gone"}},
		{"operator restore view",
			learning.ListOptions{IncludeArchived: true, ExcludeStale: true},
			[]string{"live", "gone"}},
	} {
		got, err := s.List(ctx, "alice", tc.opts)
		if err != nil {
			t.Fatalf("%s: List: %v", tc.name, err)
		}
		if !slices.Equal(skillNames(got), tc.want) {
			t.Errorf("%s: listed %v, want %v", tc.name, skillNames(got), tc.want)
		}
		n, err := s.Count(ctx, "alice", tc.opts)
		if err != nil || n != len(tc.want) {
			t.Errorf("%s: count = %d (%v), want %d — count and list must answer alike",
				tc.name, n, err, len(tc.want))
		}
		seqs, err := s.ToolSequences(ctx, "alice", tc.opts)
		if err != nil || len(seqs) != len(tc.want) {
			t.Errorf("%s: %d tool sequences (%v), want %d",
				tc.name, len(seqs), err, len(tc.want))
		}
	}
}

func TestGetIsTheLoadersQuestionAndByIDIsTheOperators(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()

	ageing := newSkill("alice", "ageing", base)
	ageing.State = learning.SkillStale
	mustInsert(t, s, ageing)
	gone := newSkill("alice", "gone", base)
	gone.State = learning.SkillArchived
	mustInsert(t, s, gone)

	// A stale skill still loads — that is what makes revival-on-use possible.
	if _, ok, err := s.Get(ctx, "alice", "ageing"); err != nil || !ok {
		t.Errorf("stale skill: ok=%v err=%v, want loadable", ok, err)
	}
	if _, ok, err := s.Get(ctx, "alice", "gone"); err != nil || ok {
		t.Errorf("archived skill: ok=%v err=%v, want hidden from the loader", ok, err)
	}
	// ...but it is still THERE, which is what archive-never-delete means.
	if _, ok, err := s.ByID(ctx, gone.ID); err != nil || !ok {
		t.Errorf("archived skill by id: ok=%v err=%v, want readable", ok, err)
	}
	if _, ok, err := s.Get(ctx, "bob", "ageing"); err != nil || ok {
		t.Errorf("another seat's skill: ok=%v err=%v, want absent", ok, err)
	}
	if _, ok, err := s.ByID(ctx, "no-such-id"); err != nil || ok {
		t.Errorf("unknown id: ok=%v err=%v, want absence and no error", ok, err)
	}
}

func TestListOrderIsTotalWhenCreationStampsTie(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()
	// Two skills drafted in one clustering pass share an instant. Without a
	// tiebreaker the pair comes back in whatever order the scan produced and
	// a prefetch that renders the top N flickers between turns.
	for _, name := range []string{"aaa", "bbb", "ccc"} {
		mustInsert(t, s, newSkill("alice", name, base))
	}
	first, err := s.List(ctx, "alice", learning.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"ccc", "bbb", "aaa"} // id DESC, ids being sk-alice-<name>
	if !slices.Equal(skillNames(first), want) {
		t.Fatalf("order = %v, want %v", skillNames(first), want)
	}
	second, _ := s.List(ctx, "alice", learning.ListOptions{})
	if !slices.Equal(skillNames(second), skillNames(first)) {
		t.Errorf("order moved between reads: %v then %v",
			skillNames(first), skillNames(second))
	}
}

func TestListForSpansSeatsAndAnEmptyUnitIsEmpty(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()
	mustInsert(t, s, newSkill("alice", "triage", base))
	mustInsert(t, s, newSkill("bob", "deploy", base.Add(time.Hour)))
	mustInsert(t, s, newSkill("carol", "review", base))

	got, err := s.ListFor(ctx, []string{"alice", "bob"}, learning.ListOptions{})
	if err != nil {
		t.Fatalf("ListFor: %v", err)
	}
	if !slices.Equal(skillNames(got), []string{"deploy", "triage"}) {
		t.Errorf("listed %v, want bob's then alice's", skillNames(got))
	}
	// An empty unit must answer empty rather than building `IN ()`, which is
	// a syntax error and not an empty answer.
	if got, err := s.ListFor(ctx, nil, learning.ListOptions{}); err != nil || len(got) != 0 {
		t.Errorf("empty unit: %d rows, %v", len(got), err)
	}
}

func TestARevisionIsACopyOfTheBodyNotAViewOfIt(t *testing.T) {
	t.Parallel()
	sk := newSkill("alice", "triage", base)
	rev := sk.Revision()
	rev.Frontmatter["name"] = "mutated"
	rev.ToolSequence[0] = "mutated"
	if sk.Frontmatter["name"] != "triage" || sk.ToolSequence[0] != "read_file" {
		t.Fatal("editing a revision reached back into the skill it came from")
	}
}

// ---------------------------------------------------------------------------
// Refinement: archive-then-update, in one transaction
// ---------------------------------------------------------------------------

func TestRefiningArchivesThePriorBodyAndBumpsTheVersion(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()
	sk := mustInsert(t, s, newSkill("alice", "triage", base))

	rev := sk.Revision()
	rev.Content = "# triage\noriginal body\n- Observed in practice: it worked"
	at := base.Add(24 * time.Hour)
	out := mustUpdate(t, s, sk.ID, rev, learning.Refinement{
		Kind: learning.RefineObserved, Note: "turn t-9 confirmed it", At: at,
	})

	if out.Version != 2 || out.Content != rev.Content || !out.UpdatedAt.Equal(at) {
		t.Errorf("live row = v%d %q at %v, want v2 with the new body", out.Version,
			out.Content, out.UpdatedAt)
	}
	if !out.CreatedAt.Equal(base) {
		t.Errorf("created_at moved to %v; an edit is not a re-creation", out.CreatedAt)
	}
	versions, err := s.Versions(ctx, sk.ID, 0)
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("%d archived versions, want exactly the one body that was replaced", len(versions))
	}
	v := versions[0]
	if v.Content != sk.Content {
		t.Errorf("archived body = %q, want the body that was replaced (%q)", v.Content, sk.Content)
	}
	if v.Version != 1 || v.Kind != learning.RefineObserved || v.Note != "turn t-9 confirmed it" {
		t.Errorf("archived row = v%d kind=%q note=%q", v.Version, v.Kind, v.Note)
	}
	if v.SkillID != sk.ID || v.AgentHandle != "alice" || v.Name != "triage" {
		t.Errorf("archived row lost its provenance: %+v", v)
	}
	if !v.ArchivedAt.Equal(at) {
		t.Errorf("archived at %v, want the refinement's own stamp %v", v.ArchivedAt, at)
	}
}

func TestARefinementKindOutsideTheFixedSetIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()
	sk := mustInsert(t, s, newSkill("alice", "triage", base))
	rev := sk.Revision()
	rev.Content = "rewritten"

	for _, kind := range []learning.RefinementKind{"", "observed", "OBSERVED_IN_PRACTICE"} {
		_, err := s.Update(ctx, sk.ID, rev, learning.Refinement{Kind: kind, At: base})
		if !errors.Is(err, learning.ErrRefinementKind) {
			t.Fatalf("kind %q: err = %v, want ErrRefinementKind", kind, err)
		}
	}
	if got := mustSkill(t, s, sk.ID); got.Version != 1 || got.Content != sk.Content {
		t.Errorf("a refused refinement still wrote: v%d %q", got.Version, got.Content)
	}
	if vs, _ := s.Versions(ctx, sk.ID, 0); len(vs) != 0 {
		t.Errorf("a refused refinement archived %d row(s)", len(vs))
	}

	// Counterfactual: every kind in the set is accepted and stored verbatim.
	for _, kind := range []learning.RefinementKind{
		learning.RefineObserved, learning.RefineCounterExample, learning.RefineTool,
		learning.RefineReplace, learning.RefinePromotion, learning.RefineRollback,
	} {
		mustUpdate(t, s, sk.ID, rev, learning.Refinement{Kind: kind, At: base})
	}
	vs, _ := s.Versions(ctx, sk.ID, 0)
	if len(vs) != 6 {
		t.Fatalf("%d archived rows, want one per accepted kind", len(vs))
	}
	seen := map[learning.RefinementKind]bool{}
	for _, v := range vs {
		seen[v.Kind] = true
	}
	if len(seen) != 6 {
		t.Errorf("kinds stored = %v, want all six distinct", seen)
	}
}

func TestARefinementNeedsATimestampAndAKnownSkill(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()
	sk := mustInsert(t, s, newSkill("alice", "triage", base))

	if _, err := s.Update(ctx, sk.ID, sk.Revision(), learning.Refinement{
		Kind: learning.RefineObserved,
	}); err == nil {
		t.Error("a refinement with no timestamp was accepted")
	}
	_, err := s.Update(ctx, "no-such-skill", sk.Revision(), learning.Refinement{
		Kind: learning.RefineObserved, At: base,
	})
	if !errors.Is(err, learning.ErrUnknownSkill) {
		t.Errorf("unknown skill: err = %v, want ErrUnknownSkill", err)
	}
	// And it must not come back through the retry loop. An operator reading
	// "after 5 attempts: no such skill" concludes the store is flapping,
	// when the row simply is not there — a definitive answer is not retried.
	if strings.Contains(fmt.Sprint(err), "attempts") {
		t.Errorf("unknown skill reported as %q, want a plain absence", err)
	}
	if _, err := s.Update(ctx, "", sk.Revision(), learning.Refinement{
		Kind: learning.RefineObserved, At: base,
	}); !errors.Is(err, learning.ErrUnknownSkill) {
		t.Errorf("empty id: err = %v, want ErrUnknownSkill", err)
	}
}

func TestTheArchiveAndTheLiveRowCannotDisagree(t *testing.T) {
	t.Parallel()
	fault := &skillWriteFault{}
	s, _ := skillStore(t, func(o *store.Options) { o.WrapDriver = fault.wrap })
	ctx := context.Background()
	sk := mustInsert(t, s, newSkill("alice", "triage", base))
	rev := sk.Revision()
	rev.Content = "rewritten"
	boom := errors.New("injected write failure")

	// Direction one: the archive fails. The live row must not have moved —
	// otherwise the version table has no record of a body that is gone.
	fault.failFrom(sqlArchive, 1, boom)
	if _, err := s.Update(ctx, sk.ID, rev, learning.Refinement{
		Kind: learning.RefineObserved, At: base,
	}); !errors.Is(err, boom) {
		t.Fatalf("archive failure: err = %v, want the injected failure", err)
	}
	if got := mustSkill(t, s, sk.ID); got.Version != 1 || got.Content != sk.Content {
		t.Errorf("archive failed but the live row moved to v%d %q", got.Version, got.Content)
	}

	// Direction two: the archive lands and the body write fails. The version
	// row must be gone with it — the direction a store cannot hold if it
	// writes the two outside a transaction.
	fault.failFrom(sqlBodyUpdate, 1, boom)
	if _, err := s.Update(ctx, sk.ID, rev, learning.Refinement{
		Kind: learning.RefineObserved, At: base,
	}); !errors.Is(err, boom) {
		t.Fatalf("body failure: err = %v, want the injected failure", err)
	}
	if vs, err := s.Versions(ctx, sk.ID, 0); err != nil || len(vs) != 0 {
		t.Errorf("%d archived row(s) (%v) survived a refinement that never landed",
			len(vs), err)
	}
	if got := mustSkill(t, s, sk.ID); got.Version != 1 {
		t.Errorf("live row at v%d after a failed refinement", got.Version)
	}

	// Control: with the fault disarmed the same call succeeds, so the two
	// assertions above are about the injected failure and not about a store
	// that never worked.
	fault.disarm()
	out := mustUpdate(t, s, sk.ID, rev, learning.Refinement{
		Kind: learning.RefineObserved, At: base,
	})
	if out.Version != 2 {
		t.Errorf("control refinement produced v%d, want v2", out.Version)
	}
	if vs, _ := s.Versions(ctx, sk.ID, 0); len(vs) != 1 {
		t.Errorf("control refinement archived %d rows, want 1", len(vs))
	}
}

func TestHistoryIsPrunedToTheCapAndTheTrimSelfHeals(t *testing.T) {
	t.Parallel()
	fault := &skillWriteFault{}
	s, _ := skillStore(t, func(o *store.Options) { o.WrapDriver = fault.wrap })
	ctx := context.Background()
	sk := mustInsert(t, s, newSkill("alice", "triage", base))

	refine := func(n int) learning.Skill {
		rev := sk.Revision()
		rev.Content = fmt.Sprintf("body %d", n)
		return mustUpdate(t, s, sk.ID, rev, learning.Refinement{
			Kind: learning.RefineObserved,
			At:   base.Add(time.Duration(n) * time.Minute),
		})
	}
	for n := 1; n <= 12; n++ {
		refine(n)
	}
	vs, err := s.Versions(ctx, sk.ID, 100)
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(vs) != 10 {
		t.Fatalf("%d archived rows, want the default cap of 10", len(vs))
	}
	if got := versionNumbers(vs); !slices.Equal(got, []int{12, 11, 10, 9, 8, 7, 6, 5, 4, 3}) {
		t.Errorf("kept versions %v, want the ten newest", got)
	}

	// The prune runs outside the refinement's transaction on purpose: losing
	// the model's work because a trim failed would be the worse trade. Fail
	// the trim and the refinement still lands...
	fault.failFrom(sqlPrune, 1, errors.New("injected prune failure"))
	out := refine(13)
	if out.Version != 14 || out.Content != "body 13" {
		t.Errorf("refinement lost to a failed prune: v%d %q", out.Version, out.Content)
	}
	if vs, _ := s.Versions(ctx, sk.ID, 100); len(vs) != 11 {
		t.Errorf("%d archived rows after a failed prune, want the cap overshot by one", len(vs))
	}
	// ...and the next one heals the overshoot, because the delete is
	// offset-based rather than "delete the row I just displaced".
	fault.disarm()
	refine(14)
	if vs, _ := s.Versions(ctx, sk.ID, 100); len(vs) != 10 {
		t.Errorf("%d archived rows after the next refinement, want the cap restored", len(vs))
	}
}

func TestTheTrimKeepsTheHighestVersionsWhenTheStampsTie(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()
	sk := mustInsert(t, s, newSkill("alice", "triage", base))

	// Every refinement carries the SAME instant — which a caller-supplied
	// timestamp makes trivially producible, and a pass that refines twice in
	// one tick produces for real. The rows then tie on archived_at and the
	// version tiebreaker is the only thing deciding which two survive.
	for n := 1; n <= 4; n++ {
		rev := sk.Revision()
		rev.Content = fmt.Sprintf("body %d", n)
		mustUpdate(t, s, sk.ID, rev, learning.Refinement{
			Kind: learning.RefineObserved, KeepVersions: 2, At: base,
		})
	}
	vs, err := s.Versions(ctx, sk.ID, 100)
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if got := versionNumbers(vs); !slices.Equal(got, []int{4, 3}) {
		t.Errorf("kept versions %v, want the two highest — a tie on the stamp "+
			"must not let the trim keep the older bodies", got)
	}
}

func TestKeepVersionsZeroMeansTheDefaultRatherThanUnbounded(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()
	sk := mustInsert(t, s, newSkill("alice", "triage", base))
	for n := 1; n <= 12; n++ {
		rev := sk.Revision()
		rev.Content = fmt.Sprintf("body %d", n)
		mustUpdate(t, s, sk.ID, rev, learning.Refinement{
			Kind: learning.RefineTool, KeepVersions: 0,
			At: base.Add(time.Duration(n) * time.Minute),
		})
	}
	vs, _ := s.Versions(ctx, sk.ID, 100)
	if len(vs) != 10 {
		t.Fatalf("%d archived rows with KeepVersions 0, want the default cap of 10", len(vs))
	}
	// Counterfactual: an explicit cap is honoured.
	sk2 := mustInsert(t, s, newSkill("alice", "escalate", base))
	for n := 1; n <= 5; n++ {
		rev := sk2.Revision()
		rev.Content = fmt.Sprintf("body %d", n)
		mustUpdate(t, s, sk2.ID, rev, learning.Refinement{
			Kind: learning.RefineTool, KeepVersions: 3,
			At: base.Add(time.Duration(n) * time.Minute),
		})
	}
	if vs, _ := s.Versions(ctx, sk2.ID, 100); len(vs) != 3 {
		t.Errorf("%d archived rows with KeepVersions 3", len(vs))
	}
}

func TestAVersionListingIsBoundedAndNewestFirst(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()
	sk := mustInsert(t, s, newSkill("alice", "triage", base))
	for n := 1; n <= 5; n++ {
		rev := sk.Revision()
		rev.Content = fmt.Sprintf("body %d", n)
		mustUpdate(t, s, sk.ID, rev, learning.Refinement{
			Kind: learning.RefineObserved, At: base.Add(time.Duration(n) * time.Minute),
		})
	}
	vs, err := s.Versions(ctx, sk.ID, 2)
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if got := versionNumbers(vs); !slices.Equal(got, []int{5, 4}) {
		t.Errorf("bounded listing = %v, want the two newest", got)
	}
	if vs, _ := s.Versions(ctx, sk.ID, 0); len(vs) != 5 {
		t.Errorf("%d rows with no limit given, want the default listing", len(vs))
	}
	if vs, _ := s.Versions(ctx, "no-such-skill", 0); len(vs) != 0 {
		t.Errorf("%d versions for an unknown skill", len(vs))
	}
}

// ---------------------------------------------------------------------------
// Rollback
// ---------------------------------------------------------------------------

func TestRollbackIsAForwardStepThatArchivesWhatItUndoes(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()
	sk := mustInsert(t, s, newSkill("alice", "triage", base))

	rev := sk.Revision()
	rev.Content = "the regrettable rewrite"
	rev.Description = "rewritten description"
	mustUpdate(t, s, sk.ID, rev, learning.Refinement{
		Kind: learning.RefineTool, At: base.Add(time.Hour),
	})

	vs, _ := s.Versions(ctx, sk.ID, 0)
	if len(vs) != 1 {
		t.Fatalf("%d archived rows before the rollback", len(vs))
	}
	back, ok, err := s.Rollback(ctx, vs[0].ID, base.Add(2*time.Hour), 0)
	if err != nil || !ok {
		t.Fatalf("Rollback: ok=%v err=%v", ok, err)
	}
	if back.Content != sk.Content || back.Description != sk.Description {
		t.Errorf("restored body = %q/%q, want the original", back.Content, back.Description)
	}
	if back.Version != 3 {
		t.Errorf("version = %d, want 3 — a rollback moves forward so that "+
			"undoing it is possible in turn", back.Version)
	}
	// The rewrite it undid is itself in history now, tagged as the rollback.
	vs, _ = s.Versions(ctx, sk.ID, 0)
	if len(vs) != 2 || vs[0].Kind != learning.RefineRollback ||
		vs[0].Content != "the regrettable rewrite" {
		t.Fatalf("history after rollback = %+v", versionNumbers(vs))
	}
	if !strings.Contains(vs[0].Note, "version 1") {
		t.Errorf("rollback note = %q, want it to name the version restored", vs[0].Note)
	}

	// Rolling back the rollback works, which is the property "forward step"
	// buys.
	again, ok, err := s.Rollback(ctx, vs[0].ID, base.Add(3*time.Hour), 0)
	if err != nil || !ok {
		t.Fatalf("second Rollback: ok=%v err=%v", ok, err)
	}
	if again.Content != "the regrettable rewrite" || again.Version != 4 {
		t.Errorf("second rollback = v%d %q", again.Version, again.Content)
	}
}

func TestRollbackToAnUnknownVersionChangesNothing(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()
	sk := mustInsert(t, s, newSkill("alice", "triage", base))

	for _, id := range []string{"", "no-such-version"} {
		got, ok, err := s.Rollback(ctx, id, base, 0)
		if err != nil || ok || got.ID != "" {
			t.Errorf("Rollback(%q): ok=%v err=%v — an unknown version is an "+
				"absence, not a failure", id, ok, err)
		}
	}
	if got := mustSkill(t, s, sk.ID); got.Version != 1 {
		t.Errorf("live row moved to v%d", got.Version)
	}
}

// ---------------------------------------------------------------------------
// Use telemetry
// ---------------------------------------------------------------------------

func TestUsingAStaleSkillRevivesItInTheSameWrite(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()

	ageing := newSkill("alice", "ageing", base)
	ageing.State = learning.SkillStale
	ageing.StaleAt = base.Add(time.Hour)
	mustInsert(t, s, ageing)

	at := base.Add(48 * time.Hour)
	use := s.MarkUsed(ctx, ageing.ID, at)
	if !use.Recorded || !use.Revived {
		t.Fatalf("use = %+v, want recorded and revived", use)
	}
	got := mustSkill(t, s, ageing.ID)
	if got.State != learning.SkillActive || !got.StaleAt.IsZero() {
		t.Errorf("state = %q stale_at = %v, want active with the marker cleared",
			got.State, got.StaleAt)
	}
	if got.UseCount != 1 || !got.LastUsedAt.Equal(at) {
		t.Errorf("telemetry = %d uses, last %v", got.UseCount, got.LastUsedAt)
	}

	// Counterfactual one: an active skill records a use and is not "revived",
	// so a worker does not publish a revival event on every load.
	live := mustInsert(t, s, newSkill("alice", "live", base))
	if use := s.MarkUsed(ctx, live.ID, at); !use.Recorded || use.Revived {
		t.Errorf("active skill: use = %+v, want recorded and not revived", use)
	}
	// Counterfactual two: an archived skill is not resurrected by a use. The
	// loader refuses archived rows, so reaching here means a caller bypassed
	// that — and this path cannot fail anything, which makes it the wrong
	// place to decide the row comes back.
	gone := newSkill("alice", "gone", base)
	gone.State = learning.SkillArchived
	gone.ArchivedAt = base.Add(time.Hour)
	mustInsert(t, s, gone)
	if use := s.MarkUsed(ctx, gone.ID, at); !use.Recorded || use.Revived {
		t.Errorf("archived skill: use = %+v", use)
	}
	if got := mustSkill(t, s, gone.ID); got.State != learning.SkillArchived {
		t.Errorf("archived skill came back as %q", got.State)
	}
}

func TestRecordingAUseCannotCostALoad(t *testing.T) {
	t.Parallel()
	fault := &skillWriteFault{}
	s, _ := skillStore(t, func(o *store.Options) { o.WrapDriver = fault.wrap })
	ctx := context.Background()
	sk := mustInsert(t, s, newSkill("alice", "triage", base))

	// Control: with the store healthy the write lands, so the assertion
	// below is about the injected failure and not about a store that never
	// recorded anything.
	if use := s.MarkUsed(ctx, sk.ID, base); !use.Recorded {
		t.Fatalf("control: use = %+v, want recorded", use)
	}

	fault.failFrom(sqlMarkUsed, 1, errors.New("injected telemetry failure"))
	use := s.MarkUsed(ctx, sk.ID, base.Add(time.Hour))
	if use.Recorded || use.Revived {
		t.Errorf("use = %+v, want the zero value from a failed write", use)
	}
	// The load itself is untouched: reads still answer, and there is no error
	// for a caller to propagate into the skill-loading path.
	if got, ok, err := s.Get(ctx, "alice", "triage"); err != nil || !ok || got.UseCount != 1 {
		t.Errorf("load after a failed telemetry write: ok=%v err=%v uses=%d",
			ok, err, got.UseCount)
	}

	fault.disarm()
	if use := s.MarkUsed(ctx, "no-such-skill", base); use.Recorded {
		t.Errorf("unknown skill: use = %+v, want nothing recorded", use)
	}
	if use := s.MarkUsed(ctx, sk.ID, time.Time{}); use.Recorded {
		t.Errorf("zero timestamp: use = %+v — a year-1 last-used stamp reads "+
			"as ancient and ages the skill out", use)
	}
}

func TestConcurrentUsesLoseNoIncrements(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()
	sk := mustInsert(t, s, newSkill("alice", "triage", base))

	const workers, each = 8, 5
	var (
		mu       sync.Mutex
		recorded int
		wg       sync.WaitGroup
	)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				use := s.MarkUsed(ctx, sk.ID, base.Add(time.Duration(w*each+i)*time.Second))
				if use.Recorded {
					mu.Lock()
					recorded++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	got := mustSkill(t, s, sk.ID)
	if got.UseCount != recorded {
		t.Errorf("use_count = %d but %d writes reported success — increments "+
			"were lost or double-counted", got.UseCount, recorded)
	}
	if recorded == 0 {
		t.Fatal("no use was recorded at all; the test measured nothing")
	}
	t.Logf("recorded %d of %d concurrent uses", recorded, workers*each)
}

func TestConcurrentRefinementsKeepTheVersionChainGapFree(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()
	sk := mustInsert(t, s, newSkill("alice", "triage", base))

	const workers, each = 4, 3
	var (
		mu   sync.Mutex
		won  int
		wg   sync.WaitGroup
		errs []error
	)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				rev := sk.Revision()
				rev.Content = fmt.Sprintf("worker %d edit %d", w, i)
				_, err := s.Update(ctx, sk.ID, rev, learning.Refinement{
					Kind: learning.RefineTool, KeepVersions: 100,
					At: base.Add(time.Duration(w*each+i) * time.Second),
				})
				mu.Lock()
				if err == nil {
					won++
				} else {
					errs = append(errs, err)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	got := mustSkill(t, s, sk.ID)
	if got.Version != won+1 {
		t.Errorf("version = %d after %d successful refinements, want %d — the "+
			"chain must be gap-free and count every winner exactly once",
			got.Version, won, won+1)
	}
	vs, err := s.Versions(ctx, sk.ID, 500)
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(vs) != won {
		t.Fatalf("%d archived bodies for %d refinements — every superseded "+
			"body must be archived exactly once", len(vs), won)
	}
	seen := map[int]bool{}
	for _, v := range vs {
		if seen[v.Version] {
			t.Errorf("version %d archived twice", v.Version)
		}
		seen[v.Version] = true
	}
	for n := 1; n <= won; n++ {
		if !seen[n] {
			t.Errorf("version %d missing from history", n)
		}
	}
	t.Logf("%d of %d refinements won; %d failed: %v", won, workers*each, len(errs), errs)
}

// ---------------------------------------------------------------------------
// Transitions and the guard
// ---------------------------------------------------------------------------

func TestATransitionStampsWhatItEntersAndClearsWhatItLeaves(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()
	sk := mustInsert(t, s, newSkill("alice", "triage", base))
	staledAt := base.Add(30 * 24 * time.Hour)
	archivedAt := base.Add(90 * 24 * time.Hour)

	mustTransition(t, s, learning.Transition{
		SkillID: sk.ID, To: learning.SkillStale, At: staledAt,
	})
	got := mustSkill(t, s, sk.ID)
	if got.State != learning.SkillStale || !got.StaleAt.Equal(staledAt) || !got.ArchivedAt.IsZero() {
		t.Errorf("after stale: %q stale_at=%v archived_at=%v", got.State, got.StaleAt, got.ArchivedAt)
	}

	mustTransition(t, s, learning.Transition{
		SkillID: sk.ID, To: learning.SkillArchived, At: archivedAt,
	})
	got = mustSkill(t, s, sk.ID)
	if got.State != learning.SkillArchived || !got.ArchivedAt.Equal(archivedAt) {
		t.Errorf("after archive: %q archived_at=%v", got.State, got.ArchivedAt)
	}
	if !got.StaleAt.Equal(staledAt) {
		t.Errorf("stale_at = %v after archiving, want the date it started ageing "+
			"kept so both steps stay readable", got.StaleAt)
	}

	mustTransition(t, s, learning.Transition{
		SkillID: sk.ID, To: learning.SkillActive, At: archivedAt.Add(time.Hour),
	})
	got = mustSkill(t, s, sk.ID)
	if got.State != learning.SkillActive || !got.StaleAt.IsZero() || !got.ArchivedAt.IsZero() {
		t.Errorf("after revival: %q stale_at=%v archived_at=%v — a revived row "+
			"restarts its disuse clock", got.State, got.StaleAt, got.ArchivedAt)
	}
	// No transition writes history: the table is for bodies, not for states.
	if vs, _ := s.Versions(ctx, sk.ID, 0); len(vs) != 0 {
		t.Errorf("%d version rows written by state transitions", len(vs))
	}
}

func TestATransitionRefusesWhatTheMachineDoesNotHave(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()
	sk := mustInsert(t, s, newSkill("alice", "triage", base))

	if _, err := s.Transition(ctx, learning.Transition{
		SkillID: sk.ID, To: "retired", At: base,
	}); !errors.Is(err, learning.ErrSkillState) {
		t.Errorf("unknown state: err = %v, want ErrSkillState", err)
	}
	if _, err := s.Transition(ctx, learning.Transition{
		SkillID: sk.ID, To: learning.SkillStale,
	}); err == nil {
		t.Error("a transition with no timestamp was accepted")
	}
	// An unknown row is a false, not an error: the curator's list and its
	// writes are separate statements and a row can legitimately vanish
	// between them.
	if ok, err := s.Transition(ctx, learning.Transition{
		SkillID: "no-such-skill", To: learning.SkillStale, At: base,
	}); ok || err != nil {
		t.Errorf("unknown skill: ok=%v err=%v", ok, err)
	}
	if got := mustSkill(t, s, sk.ID); got.State != learning.SkillActive {
		t.Errorf("state = %q after three refused transitions", got.State)
	}
}

func TestTheGuardHoldsOnlyWhileTheStampIsUnmoved(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()
	sk := mustInsert(t, s, newSkill("alice", "triage", base))

	// A never-used skill's stamp is NULL, and that is a REAL guard value —
	// exactly the population the curator ages out first. A guard written with
	// `=` instead of a null-safe comparison would never match here and the
	// whole pass would silently do nothing.
	ok, err := s.Transition(ctx, learning.Transition{
		SkillID: sk.ID, To: learning.SkillStale, At: base,
		Guard: learning.LastUsed(time.Time{}),
	})
	if err != nil || !ok {
		t.Fatalf("never-used guard: ok=%v err=%v, want it to hold", ok, err)
	}

	// Someone loads the skill. The stamp moves, so the same guard must now
	// refuse — this is what stops a skill being archived out from under the
	// agent holding it mid-turn.
	if use := s.MarkUsed(ctx, sk.ID, base.Add(time.Hour)); !use.Recorded {
		t.Fatal("MarkUsed did not record")
	}
	ok, err = s.Transition(ctx, learning.Transition{
		SkillID: sk.ID, To: learning.SkillArchived, At: base.Add(2 * time.Hour),
		Guard: learning.LastUsed(time.Time{}),
	})
	if err != nil {
		t.Fatalf("raced guard: err = %v, want a plain false", err)
	}
	if ok {
		t.Error("the guard held after the skill was used")
	}
	if got := mustSkill(t, s, sk.ID); got.State == learning.SkillArchived {
		t.Error("the skill was archived despite a use since the snapshot")
	}

	// Counterfactual: the guard built from the CURRENT stamp holds.
	current := mustSkill(t, s, sk.ID)
	ok, err = s.Transition(ctx, learning.Transition{
		SkillID: sk.ID, To: learning.SkillArchived, At: base.Add(3 * time.Hour),
		Guard: learning.LastUsed(current.LastUsedAt),
	})
	if err != nil || !ok {
		t.Fatalf("fresh guard: ok=%v err=%v, want it to hold", ok, err)
	}
	// And an unguarded transition never consults the stamp at all.
	ok, err = s.Transition(ctx, learning.Transition{
		SkillID: sk.ID, To: learning.SkillActive, At: base.Add(4 * time.Hour),
	})
	if err != nil || !ok {
		t.Fatalf("unguarded: ok=%v err=%v", ok, err)
	}
}

func TestAStoreFailureIsNotAGuardMiss(t *testing.T) {
	t.Parallel()
	fault := &skillWriteFault{}
	s, _ := skillStore(t, func(o *store.Options) { o.WrapDriver = fault.wrap })
	ctx := context.Background()
	sk := mustInsert(t, s, newSkill("alice", "triage", base))

	// Control: healthy store, the transition lands.
	mustTransition(t, s, learning.Transition{
		SkillID: sk.ID, To: learning.SkillStale, At: base,
	})

	boom := errors.New("injected transition failure")
	fault.failFrom(sqlTransition, 1, boom)
	ok, err := s.Transition(ctx, learning.Transition{
		SkillID: sk.ID, To: learning.SkillArchived, At: base,
	})
	if ok {
		t.Fatal("a failed transition reported success")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the injected failure — returning a bare "+
			"false here renders an outage as the race guard doing its "+
			"job", err)
	}
	// SetPinned tells the two apart the same way — it writes a different
	// statement, so it is armed separately rather than assumed covered.
	fault.failFrom("SET pinned", 1, boom)
	if ok, err := s.SetPinned(ctx, sk.ID, true); ok || err == nil {
		t.Errorf("SetPinned under failure: ok=%v err=%v", ok, err)
	}
}

func TestPinnedIsStoredAsTheIntegerTheCuratorFiltersOn(t *testing.T) {
	t.Parallel()
	// The candidate query filters `pinned = 0`, which is a comparison against
	// whatever the driver stored for a Go bool. A driver that encoded one as
	// anything else would not raise — it would quietly hand every pinned
	// skill to the curator — so the stored encoding is asserted directly
	// rather than inferred from behaviour. It used to run on both certified
	// drivers; there is one now, and the encoding is exactly
	// as worth pinning against a driver upgrade as it was against a second
	// driver.
	s, db := skillStore(t)
	ctx := context.Background()
	sk := mustInsert(t, s, newSkill("alice", "triage", base))

	raw := func() int {
		t.Helper()
		var v int
		if err := db.SQL().QueryRowContext(ctx,
			`SELECT pinned FROM synthesized_skills WHERE id = ?`, sk.ID,
		).Scan(&v); err != nil {
			t.Fatalf("read pinned: %v", err)
		}
		return v
	}
	if got := raw(); got != 0 {
		t.Errorf("unpinned stored as %d, want 0", got)
	}
	if _, err := s.SetPinned(ctx, sk.ID, true); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}
	if got := raw(); got != 1 {
		t.Errorf("pinned stored as %d, want 1", got)
	}
	cands, err := s.CuratorCandidates(ctx, "alice")
	if err != nil || len(cands) != 0 {
		t.Errorf("candidates = %v (%v), want the pinned row filtered out",
			skillNames(cands), err)
	}
	if _, err := s.SetPinned(ctx, sk.ID, false); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}
	if got := raw(); got != 0 {
		t.Errorf("unpinned stored as %d, want 0", got)
	}
	if cands, _ := s.CuratorCandidates(ctx, "alice"); len(cands) != 1 {
		t.Errorf("candidates = %v, want the unpinned row back", skillNames(cands))
	}
}

func TestPinningIsExemptionFromTheAUTOMATICTransitionsOnly(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()
	ancient := newSkill("alice", "ancient", base.Add(-365*24*time.Hour))
	mustInsert(t, s, ancient)

	ok, err := s.SetPinned(ctx, ancient.ID, true)
	if err != nil || !ok {
		t.Fatalf("SetPinned: ok=%v err=%v", ok, err)
	}
	if got := mustSkill(t, s, ancient.ID); !got.Pinned {
		t.Fatal("pinned did not round-trip")
	}
	// The pass must not even consider it...
	cands, err := s.CuratorCandidates(ctx, "alice")
	if err != nil {
		t.Fatalf("CuratorCandidates: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("curator candidates = %v, want the pinned row excluded", skillNames(cands))
	}
	// ...and the decision refuses it even when handed the row directly,
	// because the exemption is the invariant and the query is only the fast
	// path.
	if _, due := (learning.CuratorPolicy{}).Next(mustSkill(t, s, ancient.ID), base); due {
		t.Error("the policy proposed a transition for a pinned skill")
	}
	res, err := s.Curate(ctx, learning.CuratorPolicy{}, "alice", base)
	if err != nil || len(res.Applied) != 0 || res.Scanned != 0 {
		t.Errorf("pass over a pinned-only seat: %+v (%v)", res, err)
	}

	// But an operator archiving it by hand is exactly what pinning must not
	// block.
	mustTransition(t, s, learning.Transition{
		SkillID: ancient.ID, To: learning.SkillArchived, At: base,
	})
	if got := mustSkill(t, s, ancient.ID); got.State != learning.SkillArchived {
		t.Errorf("state = %q, want an explicit archive to win over the pin", got.State)
	}

	// Counterfactual: unpin the same row and the pass takes it.
	if _, err := s.SetPinned(ctx, ancient.ID, false); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}
	if ok, err := s.SetPinned(ctx, "no-such-skill", true); ok || err != nil {
		t.Errorf("pinning an unknown skill: ok=%v err=%v", ok, err)
	}
}

// ---------------------------------------------------------------------------
// The curator state machine
// ---------------------------------------------------------------------------

func TestTheCuratorAgesSkillsOutInTwoStepsAndOnlyOnDisuse(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	policy := learning.CuratorPolicy{StaleAfter: 30 * day, ArchiveAfter: 90 * day}

	for _, tc := range []struct {
		name     string
		state    learning.SkillState
		created  time.Duration // before now
		lastUsed time.Duration // before now; 0 means never used
		policy   learning.CuratorPolicy
		want     learning.SkillState
		due      bool
	}{
		{name: "recently used stays", state: learning.SkillActive,
			created: 200 * day, lastUsed: 2 * day, policy: policy},
		{name: "idle past the stale window goes stale", state: learning.SkillActive,
			created: 200 * day, lastUsed: 31 * day, policy: policy,
			want: learning.SkillStale, due: true},
		{name: "never used ages from creation", state: learning.SkillActive,
			created: 31 * day, policy: policy, want: learning.SkillStale, due: true},
		{name: "a young never-used skill stays", state: learning.SkillActive,
			created: 29 * day, policy: policy},
		{name: "exactly on the boundary stays", state: learning.SkillActive,
			created: 200 * day, lastUsed: 30 * day, policy: policy},
		{name: "stale but not old enough to archive stays", state: learning.SkillStale,
			created: 200 * day, lastUsed: 60 * day, policy: policy},
		{name: "stale and idle past the archive window is archived",
			state: learning.SkillStale, created: 200 * day, lastUsed: 91 * day,
			policy: policy, want: learning.SkillArchived, due: true},
		{name: "a widened window revives a stale skill", state: learning.SkillStale,
			created: 200 * day, lastUsed: 45 * day,
			policy: learning.CuratorPolicy{StaleAfter: 60 * day, ArchiveAfter: 180 * day},
			want:   learning.SkillActive, due: true},
		{name: "archived is terminal for the machine", state: learning.SkillArchived,
			created: 900 * day, lastUsed: 900 * day, policy: policy},
		{name: "a zero policy uses the carried 30/90 schedule",
			state: learning.SkillActive, created: 200 * day, lastUsed: 31 * day,
			want: learning.SkillStale, due: true},
	} {
		sk := learning.Skill{
			State:     tc.state,
			CreatedAt: base.Add(-tc.created),
		}
		if tc.lastUsed != 0 {
			sk.LastUsedAt = base.Add(-tc.lastUsed)
		}
		change, due := tc.policy.Next(sk, base)
		if due != tc.due || change.To != tc.want {
			t.Errorf("%s: due=%v to=%q, want due=%v to=%q",
				tc.name, due, change.To, tc.due, tc.want)
		}
		if due && change.Reason == "" {
			t.Errorf("%s: transition carries no reason", tc.name)
		}
	}
}

func TestAnArchiveWindowInsideTheStaleWindowIsWidenedToTheStaleOne(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	// An archive window shorter than the stale window is only reachable as a
	// misconfiguration, and what it does is worth measuring rather than
	// assuming. The case that separates the two readings is a row that is
	// STALE while its last use is still inside the stale window — staled by
	// hand, or left behind by a window that has since been widened. Read
	// literally, "archive after 5 days" archives a skill used 10 days ago,
	// while the same policy calls anything under 30 days fresh.
	p := learning.CuratorPolicy{StaleAfter: 30 * day, ArchiveAfter: 5 * day}
	sk := learning.Skill{
		State: learning.SkillStale, CreatedAt: base.Add(-100 * day),
		LastUsedAt: base.Add(-10 * day),
	}
	change, due := p.Next(sk, base)
	if !due || change.To != learning.SkillActive {
		t.Fatalf("inverted policy: due=%v to=%q, want the row revived rather "+
			"than archived", due, change.To)
	}
	// Counterfactual: with the windows the right way round, a row genuinely
	// past the archive window is archived.
	sane := learning.CuratorPolicy{StaleAfter: 30 * day, ArchiveAfter: 90 * day}
	sk.LastUsedAt = base.Add(-100 * day)
	if change, due := sane.Next(sk, base); !due || change.To != learning.SkillArchived {
		t.Fatalf("sane policy: due=%v to=%q, want archived", due, change.To)
	}
}

func TestOnePassAppliesTheTransitionsAndReportsWhatLanded(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()
	day := 24 * time.Hour
	now := base

	idle := newSkill("alice", "idle", now.Add(-40*day))
	fresh := newSkill("alice", "fresh", now.Add(-100*day))
	fresh.LastUsedAt = now.Add(-time.Hour)
	fresh.UseCount = 3
	ageing := newSkill("alice", "ageing", now.Add(-300*day))
	ageing.State = learning.SkillStale
	elsewhere := newSkill("bob", "elsewhere", now.Add(-300*day))
	for _, sk := range []learning.Skill{idle, fresh, ageing, elsewhere} {
		mustInsert(t, s, sk)
	}

	res, err := s.Curate(ctx, learning.CuratorPolicy{}, "alice", now)
	if err != nil {
		t.Fatalf("Curate: %v", err)
	}
	if res.Scanned != 3 || res.Raced != 0 {
		t.Errorf("pass = %d scanned, %d raced, want 3 and 0", res.Scanned, res.Raced)
	}
	if len(res.Applied) != 2 {
		t.Fatalf("applied %d transitions, want the idle and the ageing one", len(res.Applied))
	}
	byName := map[string]learning.StateChange{}
	for _, c := range res.Applied {
		byName[c.Skill.Name] = c
	}
	if c, ok := byName["idle"]; !ok || c.To != learning.SkillStale ||
		c.Skill.State != learning.SkillActive {
		t.Errorf("idle: %+v, want active -> stale with the PRIOR state on the "+
			"snapshot (a revival event has to report where it came from)", c)
	}
	if c, ok := byName["ageing"]; !ok || c.To != learning.SkillArchived {
		t.Errorf("ageing: %+v, want stale -> archived", c)
	}
	if got := mustSkill(t, s, idle.ID); got.State != learning.SkillStale {
		t.Errorf("idle is %q in the database", got.State)
	}
	if got := mustSkill(t, s, ageing.ID); got.State != learning.SkillArchived {
		t.Errorf("ageing is %q in the database", got.State)
	}
	if got := mustSkill(t, s, fresh.ID); got.State != learning.SkillActive {
		t.Errorf("a recently used skill was transitioned to %q", got.State)
	}
	// The other seat was not in scope, and archive-never-delete means every
	// row is still there.
	if got := mustSkill(t, s, elsewhere.ID); got.State != learning.SkillActive {
		t.Errorf("another seat's skill was transitioned to %q", got.State)
	}
	all, _ := s.List(ctx, "alice", learning.ListOptions{IncludeArchived: true})
	if len(all) != 3 {
		t.Errorf("%d rows left for alice, want all three — nothing is deleted", len(all))
	}

	// A pass over every seat picks up the one that was out of scope.
	res, err = s.Curate(ctx, learning.CuratorPolicy{}, "", now)
	if err != nil {
		t.Fatalf("fleet-wide Curate: %v", err)
	}
	if res.Scanned != 3 || len(res.Applied) != 1 ||
		res.Applied[0].Skill.AgentHandle != "bob" {
		t.Errorf("fleet-wide pass = %+v, want bob's skill staled and the "+
			"archived one no longer a candidate", res)
	}
}

func TestAUseDuringThePassIsCountedAsARaceNotAsATransition(t *testing.T) {
	t.Parallel()
	fault := &skillWriteFault{}
	s, _ := skillStore(t, func(o *store.Options) { o.WrapDriver = fault.wrap })
	ctx := context.Background()
	sk := mustInsert(t, s, newSkill("alice", "triage", base.Add(-100*24*time.Hour)))

	// The Plan prefetch caches a seat's skills at turn start; the agent can
	// load one at any point after that. Reproduce the window exactly: the
	// use lands between the pass reading the row and the pass writing it.
	fault.before(sqlTransition, func() {
		if use := s.MarkUsed(ctx, sk.ID, base); !use.Recorded {
			t.Error("the racing use did not record")
		}
	})

	res, err := s.Curate(ctx, learning.CuratorPolicy{}, "alice", base)
	if err != nil {
		t.Fatalf("Curate: %v", err)
	}
	if res.Raced != 1 || len(res.Applied) != 0 {
		t.Errorf("pass = %+v, want the transition counted as raced", res)
	}
	if got := mustSkill(t, s, sk.ID); got.State != learning.SkillActive {
		t.Errorf("state = %q, want the just-used skill left alone", got.State)
	}
}

func TestAFailedTransitionReturnsWhatAlreadyLanded(t *testing.T) {
	t.Parallel()
	fault := &skillWriteFault{}
	s, _ := skillStore(t, func(o *store.Options) { o.WrapDriver = fault.wrap })
	ctx := context.Background()
	old := base.Add(-100 * 24 * time.Hour)
	first := mustInsert(t, s, newSkill("alice", "aaa", old))
	mustInsert(t, s, newSkill("alice", "bbb", old))

	// Both are due. The second write fails; the first has already happened
	// and its event still has to be published, so the result must survive the
	// error.
	boom := errors.New("injected transition failure")
	fault.failFrom(sqlTransition, 2, boom)
	res, err := s.Curate(ctx, learning.CuratorPolicy{}, "alice", base)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the injected failure", err)
	}
	if len(res.Applied) != 1 || res.Applied[0].Skill.ID != first.ID {
		t.Fatalf("applied = %+v, want the one transition that landed", res.Applied)
	}
	if got := mustSkill(t, s, first.ID); got.State != learning.SkillStale {
		t.Errorf("the transition reported as landed is %q in the database", got.State)
	}
	if res.Scanned != 2 {
		t.Errorf("scanned = %d, want both candidates counted", res.Scanned)
	}
}

func TestAPassCannotReadItsCandidatesAndSaysSo(t *testing.T) {
	t.Parallel()
	s, db := skillStore(t)
	ctx := context.Background()
	mustInsert(t, s, newSkill("alice", "triage", base.Add(-100*24*time.Hour)))

	// Control: the pass works before the store is taken away.
	if res, err := s.Curate(ctx, learning.CuratorPolicy{}, "alice", base); err != nil ||
		len(res.Applied) != 1 {
		t.Fatalf("control pass: %+v (%v)", res, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	res, err := s.Curate(ctx, learning.CuratorPolicy{}, "alice", base)
	if err == nil {
		t.Fatal("a pass over an unreadable store reported success")
	}
	if res.Scanned != 0 || len(res.Applied) != 0 {
		t.Errorf("result = %+v, want nothing claimed", res)
	}
	// Every other read says so too, rather than answering "nothing".
	if _, err := s.List(ctx, "alice", learning.ListOptions{}); err == nil {
		t.Error("List answered from a closed store")
	}
	if _, err := s.Health(ctx, "alice"); err == nil {
		t.Error("Health answered from a closed store")
	}
}

// ---------------------------------------------------------------------------
// Foreign keys and the health view
// ---------------------------------------------------------------------------

func TestTheHistoryForeignKeyIsEnforcedAndCascades(t *testing.T) {
	t.Parallel()
	s, db := skillStore(t)
	ctx := context.Background()
	sk := mustInsert(t, s, newSkill("alice", "triage", base))
	for n := 1; n <= 3; n++ {
		rev := sk.Revision()
		rev.Content = fmt.Sprintf("body %d", n)
		mustUpdate(t, s, sk.ID, rev, learning.Refinement{
			Kind: learning.RefineObserved, At: base.Add(time.Duration(n) * time.Minute),
		})
	}

	// Enforcement, first: a history row for a skill that does not exist must
	// be refused. SQLite defaults foreign keys OFF, and an unenforced
	// constraint looks exactly like an enforced one until the day it matters.
	if _, err := db.SQL().ExecContext(ctx, `
		INSERT INTO synthesized_skill_versions (id, skill_id, name, description,
			content, version, refinement_kind, archived_at)
		VALUES ('v-orphan', 'no-such-skill', 'n', 'd', 'c', 1, 'replace', 0)`,
	); err == nil {
		t.Fatal("a history row for an unknown skill was accepted; foreign keys are off")
	}

	// The cascade, second. Deleting a skill is not something this store does
	// — nothing here removes a row — but an operator can, and the history
	// must go with it rather than orphaning.
	if _, err := db.SQL().ExecContext(ctx,
		`DELETE FROM synthesized_skills WHERE id = ?`, sk.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if vs, err := s.Versions(ctx, sk.ID, 100); err != nil || len(vs) != 0 {
		t.Errorf("%d history rows survived the delete (%v)", len(vs), err)
	}
}

func TestWithForeignKeysOffTheSameDeleteOrphansTheHistory(t *testing.T) {
	t.Parallel()
	s, db := skillStore(t)
	ctx := context.Background()
	sk := mustInsert(t, s, newSkill("alice", "triage", base))
	rev := sk.Revision()
	rev.Content = "rewritten"
	mustUpdate(t, s, sk.ID, rev, learning.Refinement{
		Kind: learning.RefineReplace, At: base.Add(time.Minute),
	})

	// The counterfactual for the test above, on one pinned connection so the
	// pragma applies to exactly the statement under test: this is what the
	// schema would do if the store did not turn foreign keys on per
	// connection, and it is indistinguishable from correct behaviour until
	// someone reads the orphans back.
	conn, err := db.SQL().Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("pragma: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`DELETE FROM synthesized_skills WHERE id = ?`, sk.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM synthesized_skill_versions WHERE skill_id = ?`, sk.ID,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("%d orphaned history rows, want 1 — if the cascade fires here "+
			"too then the pragma is not what makes the constraint real", n)
	}
	// Put the connection back the way the pool expects it.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("pragma restore: %v", err)
	}
}

func TestHealthExcludesArchivedSkillsSoTheyCannotDeflateTheAverage(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()
	// Real wall-clock creation stamps: the view measures age against the
	// database's own clock, not a caller's.
	tenDaysAgo := time.Now().UTC().Add(-10 * 24 * time.Hour)

	used := newSkill("alice", "used", tenDaysAgo)
	unused := newSkill("alice", "unused", tenDaysAgo)
	mustInsert(t, s, used)
	mustInsert(t, s, unused)
	lastUse := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for i := range 4 {
		if use := s.MarkUsed(ctx, used.ID, lastUse.Add(-time.Duration(3-i)*time.Minute)); !use.Recorded {
			t.Fatalf("MarkUsed %d did not record", i)
		}
	}

	before := mustHealth(t, s, "alice")
	if before.TotalSkills != 2 || before.SkillsUsedAtLeastOnce != 1 ||
		before.TotalUses != 4 || !skillCloseTo(before.AvgUsesPerSkill, 2) {
		t.Fatalf("health = %+v, want 2 skills averaging 2 uses", before)
	}
	if !before.MostRecentUse.Equal(lastUse) {
		t.Errorf("most recent use = %v, want %v", before.MostRecentUse, lastUse)
	}
	if !skillCloseTo(before.AvgAgeDays, 10) {
		t.Errorf("average age = %v days, want about 10", before.AvgAgeDays)
	}

	// Archiving the skill nobody loads must not be visible as a library that
	// suddenly looks better OR worse than it is — the curator aged it out and
	// the prefetch hides it, so counting it only deflates the one metric an
	// operator reads.
	mustTransition(t, s, learning.Transition{
		SkillID: unused.ID, To: learning.SkillArchived, At: time.Now().UTC(),
	})
	after := mustHealth(t, s, "alice")
	if after.TotalSkills != 1 || after.TotalUses != 4 || !skillCloseTo(after.AvgUsesPerSkill, 4) {
		t.Errorf("health = %+v, want the archived skill excluded (avg 4, not 2)", after)
	}

	// A seat with nothing left has no row at all rather than a zero one: an
	// average over no skills is not 0.
	mustTransition(t, s, learning.Transition{
		SkillID: used.ID, To: learning.SkillArchived, At: time.Now().UTC(),
	})
	rows, err := s.Health(ctx, "alice")
	if err != nil || len(rows) != 0 {
		t.Errorf("health rows = %d (%v), want none", len(rows), err)
	}
	// And a seat that was never in the table at all reads the same way.
	if rows, err := s.Health(ctx, "nobody"); err != nil || len(rows) != 0 {
		t.Errorf("unknown seat: %d rows (%v)", len(rows), err)
	}
}

func TestHealthIsPerSeat(t *testing.T) {
	t.Parallel()
	s, _ := skillStore(t)
	ctx := context.Background()
	mustInsert(t, s, newSkill("alice", "triage", time.Now().UTC()))
	mustInsert(t, s, newSkill("bob", "deploy", time.Now().UTC()))
	mustInsert(t, s, newSkill("bob", "review", time.Now().UTC()))

	rows, err := s.Health(ctx, "")
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if len(rows) != 2 || rows[0].AgentHandle != "alice" || rows[1].AgentHandle != "bob" {
		t.Fatalf("rollup = %+v, want one row per seat in handle order", rows)
	}
	if rows[0].TotalSkills != 1 || rows[1].TotalSkills != 2 {
		t.Errorf("counts = %d and %d, want 1 and 2", rows[0].TotalSkills, rows[1].TotalSkills)
	}
	if rows[0].MostRecentUse != (time.Time{}) {
		t.Errorf("most recent use = %v for a seat that never used one, want the "+
			"zero time rather than an epoch", rows[0].MostRecentUse)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mustTransition(t *testing.T, s *learning.Skills, tr learning.Transition) {
	t.Helper()
	ok, err := s.Transition(context.Background(), tr)
	if err != nil || !ok {
		t.Fatalf("Transition to %s: ok=%v err=%v", tr.To, ok, err)
	}
}

func mustHealth(t *testing.T, s *learning.Skills, handle string) learning.SkillHealth {
	t.Helper()
	rows, err := s.Health(context.Background(), handle)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d health rows for %s, want 1", len(rows), handle)
	}
	return rows[0]
}

// skillCloseTo compares a rollup average against an expected value; the
// view computes ages against the database's own clock, so a test can pin the
// magnitude but not the digits.
func skillCloseTo(got, want float64) bool { return math.Abs(got-want) < 0.5 }

// skillWriteFault is an armable failure — or a hook — on the WRITE statements a
// test names.
//
// The shared storetest fault intercepts result-set iteration, which reaches
// every read in the store and nothing else. The properties here are about
// writes: an archive that fails after the body moved, a trim that fails after
// the refinement landed, a use that lands between a pass's read and its write.
// None of those is reachable by failing a query, and closing the database
// fails everything at once, which cannot produce a PARTIAL pass.
//
// The wrapped driver is the real, certified one. Only the named statement is
// touched, so the schema, the SQL and the encoding are all genuine.
type skillWriteFault struct {
	mu      sync.Mutex
	match   string
	from    int // fail from this occurrence on, 1-based; 0 = never fail
	seen    int
	err     error
	hook    func()
	hookRun bool
}

// failFrom arms a failure on the nth and every later exec whose SQL contains
// match. Re-arming resets the counter, so one test can exercise several
// statements in sequence.
func (f *skillWriteFault) failFrom(match string, n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.match, f.from, f.seen, f.err = match, n, 0, err
	f.hook, f.hookRun = nil, false
}

// before arms a one-shot callback that runs immediately before the first exec
// whose SQL contains match, on the same goroutine. One shot because the
// callback usually issues writes of its own, and a re-entrant hook would
// recurse forever.
func (f *skillWriteFault) before(match string, fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.match, f.from, f.seen, f.err = match, 0, 0, nil
	f.hook, f.hookRun = fn, false
}

func (f *skillWriteFault) disarm() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.match, f.from, f.err, f.hook = "", 0, nil, nil
}

func (f *skillWriteFault) intercept(query string) error {
	f.mu.Lock()
	if f.match == "" || !strings.Contains(query, f.match) {
		f.mu.Unlock()
		return nil
	}
	f.seen++
	var (
		hook func()
		err  error
	)
	if f.hook != nil && !f.hookRun {
		f.hookRun, hook = true, f.hook
	}
	if f.from > 0 && f.seen >= f.from {
		err = f.err
	}
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (f *skillWriteFault) wrap(d driver.Driver) driver.Driver {
	return skillFaultDriver{inner: d, fault: f}
}

type skillFaultDriver struct {
	inner driver.Driver
	fault *skillWriteFault
}

func (d skillFaultDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return skillFaultConn{Conn: conn, fault: d.fault}, nil
}

// The store's connector requires ExecerContext, and database/sql picks the
// query path from the optional interfaces a conn implements — so both are
// forwarded explicitly. Embedding driver.Conn does not carry them.
type skillFaultConn struct {
	driver.Conn
	fault *skillWriteFault
}

func (c skillFaultConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	ex, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	if err := c.fault.intercept(q); err != nil {
		return nil, err
	}
	return ex.ExecContext(ctx, q, args)
}

func (c skillFaultConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	qr, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return qr.QueryContext(ctx, q, args)
}

func (c skillFaultConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	pc, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		return c.Conn.Prepare(q)
	}
	return pc.PrepareContext(ctx, q)
}

func (c skillFaultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	bt, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		// The deprecated Begin is the CORRECT fallback for a driver that
		// never implemented ConnBeginTx — it is what database/sql itself
		// falls back to. A wrapper that refused it would work with fewer
		// drivers than the standard library.
		//nolint:staticcheck // SA1019: the fallback database/sql itself uses.
		return c.Conn.Begin()
	}
	return bt.BeginTx(ctx, opts)
}
