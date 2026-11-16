package gitlet_test

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// changeCountsHold fails the test where the node's change counts are not what its rows imply.
func (n *node) changeCountsHold() {
	n.t.Helper()
	if drift := gitlet.ChangeCountsDrift(n.t, n.db); drift != "" {
		n.t.Fatal(drift)
	}
}

// countsRepo is a fresh repository named name under owner at visibility, with main at A and
// the branches each change of countsChanges opens from.
func countsRepo(t *testing.T, n *node, name string, owner gitlet.Owner, v gitlet.Visibility) string {
	t.Helper()
	s := suiteSettings()
	s.Owner, s.Visibility = owner, v
	repo := n.ensure(name, s, t1).RepoID
	cmds := []gitlet.Command{set(mainRef, gitlet.ZeroOID, oidOf("A"))}
	for _, b := range []string{"open", "draft", "closed", "merged"} {
		cmds = append(cmds, set("refs/heads/"+b, gitlet.ZeroOID, oidOf(name+"-"+b)))
	}
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), cmds...))
	return repo
}

// countsChanges opens four changes of repo by asker through the committer, one from each branch
// countsRepo pushed, and leaves one open, one a draft, one closed and one merged by a push.
func countsChanges(t *testing.T, n *node, c *gitlet.Committer, repo, name string, asker gitlet.Principal) {
	t.Helper()
	ids := map[string]gitlet.ChangeResult{}
	for _, b := range []string{"open", "draft", "closed", "merged"} {
		p := opening(repo, "refs/heads/"+b, mainRef, asker)
		p.Open.Draft = b == "draft"
		res, err := c.ProposeChange(t.Context(), p)
		requireChanged(t, res, err)
		ids[b] = res
	}
	res, err := c.ProposeChange(t.Context(), closing(repo, ids["closed"].ChangeID, asker, ids["closed"].Version))
	requireChanged(t, res, err)
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("A"), oidOf(name+"-merged"))))
	n.changeCountsHold()
}

// counted is a counts read of the changes, of repo or of every repository, as asker for ceiling.
func counted(t *testing.T, n *node, asker gitlet.Principal, ceiling, repo string) (*gitlet.ChangeCounts, error) {
	t.Helper()
	answer, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadCounts, Asker: asker,
		CeilingSeat: ceiling, Repo: repo, Counts: &gitlet.CountsQuery{Changes: true}, Fresh: stale})
	if err != nil {
		return nil, err
	}
	if answer.Counts == nil || answer.Counts.Changes == nil {
		t.Fatalf("a counts read of the changes answered %+v, no changes", answer)
	}
	return answer.Counts.Changes, nil
}

// COUNTS ARE WHAT THE LISTINGS LIST: for every asker (a person, seats of three units, a seat
// reading for a ceiling seat of another) the company's counts of each state equal the changes
// the listing of that state lists paged to its end, live is the draft and the open together,
// and each repository's named counts equal its own listing's, a deleted one's included; a
// repository the asker does not read is refused by both.
// Mutation: drop visibility from the tally key, or judge the key by the asker alone, and an
// outsider counts a unit repository's changes.
func TestCountsAreWhatTheListingsList(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	repos := []struct {
		name  string
		owner gitlet.Owner
		v     gitlet.Visibility
		by    gitlet.Principal
	}{
		{"web", gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}, gitlet.VisibilityCompany, dev},
		{"deals", gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Sales"}, gitlet.VisibilityUnit, vpSales},
		{"infra", gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Infra"}, gitlet.VisibilityUnit, dev},
		{"gone", gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}, gitlet.VisibilityCompany, dev},
	}
	for _, r := range repos {
		id := countsRepo(t, n, r.name, r.owner, r.v)
		countsChanges(t, n, c, id, r.name, r.by)
		if r.name == "gone" {
			n.deleteAt(id, harnessAt)
			n.changeCountsHold()
		}
	}
	listed := func(asker gitlet.Principal, ceiling, repo string, state gitlet.ChangeState) (int, error) {
		q := gitlet.ReadQuery{Kind: gitlet.ReadChanges, Asker: asker, CeilingSeat: ceiling, Repo: repo,
			State: state, Limit: 2, Fresh: stale}
		total := 0
		for range 100 {
			answer, err := n.reader.Read(t.Context(), q)
			if err != nil {
				return 0, err
			}
			total += len(answer.Changes)
			if answer.Next == "" {
				return total, nil
			}
			q.Cursor = answer.Next
		}
		t.Fatalf("the listing of %s changes as %+v never ended", state, asker)
		return 0, nil
	}
	byState := func(cc *gitlet.ChangeCounts) map[gitlet.ChangeState]int64 {
		return map[gitlet.ChangeState]int64{gitlet.ChangeStateDraft: cc.Draft, gitlet.ChangeStateOpen: cc.Open,
			gitlet.ChangeStateClosed: cc.Closed, gitlet.ChangeStateMerged: cc.Merged}
	}
	for _, who := range []struct {
		name    string
		asker   gitlet.Principal
		ceiling string
	}{
		{"a person", ada, ""}, {"a seat of Engineering", cto, ""}, {"a seat of Sales", vpSales, ""},
		{"dev of Infra", dev, ""}, {"a seat of Engineering for one of Sales", cto, "vp-sales"},
	} {
		for _, repo := range []string{"", "web", "deals", "infra", "gone"} {
			got, err := counted(t, n, who.asker, who.ceiling, repo)
			_, listErr := listed(who.asker, who.ceiling, repo, gitlet.ChangeStateOpen)
			if (err != nil) != (listErr != nil) {
				t.Errorf("%s counting %q answered %v and its listing %v: both serve it or neither", who.name,
					repo, err, listErr)
				continue
			}
			if err != nil {
				continue
			}
			if got.Live != got.Draft+got.Open {
				t.Errorf("%s counting %q: live %d, want the draft and the open, %d", who.name, repo, got.Live,
					got.Draft+got.Open)
			}
			for state, n := range byState(got) {
				want, err := listed(who.asker, who.ceiling, repo, state)
				if err != nil {
					t.Fatalf("%s listing %q's %s changes: %v", who.name, repo, state, err)
				}
				if n != int64(want) {
					t.Errorf("%s counts %d %s changes of %q, and the listing lists %d", who.name, n, state,
						repo, want)
				}
			}
		}
	}
	// A PERSON READS EVERY REPOSITORY IN USE: three, one change of each state.
	if got, err := counted(t, n, ada, "", ""); err != nil ||
		*got != (gitlet.ChangeCounts{Live: 6, Draft: 3, Open: 3, Closed: 3, Merged: 3}) {
		t.Errorf("a person's counts are %+v, %v; want the three repositories in use's", got, err)
	}
}

// A COUNTS READ IS HELD TO ITS SCOPE: a repository the seat does not read is refused whole, one
// its ceiling does not read is refused as above the ceiling, one nobody declared is not found,
// and a seat on a node holding no chart is a node that does not serve it.
func TestACountsReadIsHeldToItsScope(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	countsRepo(t, n, "deals", gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Sales"}, gitlet.VisibilityUnit)
	for _, tc := range []struct {
		name    string
		asker   gitlet.Principal
		ceiling string
		repo    string
		want    error
	}{
		{"an outsider", cto, "", "deals", gitlet.ErrNotPermitted},
		{"a reader for a ceiling that does not read it", vpSales, "dev", "deals", gitlet.ErrAboveCeiling},
		{"a repository nobody declared", vpSales, "", "nope", gitlet.ErrRepoNotFound},
	} {
		if got, err := counted(t, n, tc.asker, tc.ceiling, tc.repo); !errors.Is(err, tc.want) {
			t.Errorf("%s counted as %+v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	n.chart = nil
	if got, err := counted(t, n, vpSales, "", ""); !errors.Is(err, gitlet.ErrNotServing) {
		t.Errorf("a seat on a node holding no chart counted %+v, %v; want ErrNotServing", got, err)
	}
}

// A COUNTS READ IS REFUSED WHERE IT MEANS NOTHING: no part, a query asking none, a cursor, a
// limit, counts on another kind, a repository's facts on a read that answers none, and a name
// that is no repository's, each ErrInvalid naming its field.
// Mutation: a refusal relaxed into an empty answer.
func TestACountsReadIsRefusedWhereItMeansNothing(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	asks := &gitlet.CountsQuery{Changes: true}
	for _, tc := range []struct {
		field string
		q     gitlet.ReadQuery
	}{
		{"counts", gitlet.ReadQuery{Kind: gitlet.ReadCounts}},
		{"counts", gitlet.ReadQuery{Kind: gitlet.ReadCounts, Counts: &gitlet.CountsQuery{}}},
		{"cursor", gitlet.ReadQuery{Kind: gitlet.ReadCounts, Counts: asks, Cursor: "web"}},
		{"limit", gitlet.ReadQuery{Kind: gitlet.ReadCounts, Counts: asks, Limit: 10}},
		{"counts", gitlet.ReadQuery{Kind: gitlet.ReadChanges, Counts: asks}},
		{"repo_facts", gitlet.ReadQuery{Kind: gitlet.ReadChanges, RepoFacts: true}},
		{"repo_facts", gitlet.ReadQuery{Kind: gitlet.ReadRefs, Repo: "web", RepoFacts: true}},
		{"repo", gitlet.ReadQuery{Kind: gitlet.ReadCounts, Counts: asks, Repo: "not a name!"}},
	} {
		tc.q.Asker, tc.q.Fresh = ada, stale
		answer, err := n.reader.Read(t.Context(), tc.q)
		if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), tc.field+":") {
			t.Errorf("%+v answered %+v, %v; want ErrInvalid naming %s", tc.q, answer, err, tc.field)
		}
	}
}

// A REPOSITORY LISTING ANSWERS ITS CHANGE COUNTS WHEN ASKED: a listing and a detail asking
// repo_facts answer each repository's counts as a counts read of it does, a page of them cut by
// its bytes with the facts in; and every fact is a pointer left nil on a read that does not ask,
// the only fields of a summary the flag changes.
// Mutation: answer the facts unasked, read them after the page is cut, or add a fact as a value.
func TestARepositoryListingAnswersItsChangeCountsWhenAsked(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	for _, name := range []string{"api", "web"} {
		id := countsRepo(t, n, name, gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"},
			gitlet.VisibilityCompany)
		if name == "web" {
			countsChanges(t, n, c, id, name, dev)
		}
	}
	read := func(q gitlet.ReadQuery) gitlet.ReadAnswer {
		t.Helper()
		q.Asker, q.Fresh = ada, stale
		answer, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("Read(%s): %v", q.Kind, err)
		}
		return answer
	}
	bare, asked := read(gitlet.ReadQuery{Kind: gitlet.ReadRepos}), read(gitlet.ReadQuery{Kind: gitlet.ReadRepos,
		RepoFacts: true})
	if len(bare.Repos) != 2 || len(asked.Repos) != 2 {
		t.Fatalf("the listings answered %d and %d repositories, want both", len(bare.Repos), len(asked.Repos))
	}
	factFields := []string{"ChangeCounts"}
	summary := reflect.TypeFor[gitlet.RepoSummary]()
	for _, f := range factFields {
		field, ok := summary.FieldByName(f)
		if !ok || field.Type.Kind() != reflect.Pointer || !strings.HasSuffix(field.Tag.Get("json"), ",omitempty") {
			t.Errorf("the fact %s is no pointer field of RepoSummary left off empty: a build answering fewer "+
				"facts would answer it as a value", f)
		}
	}
	for i := range asked.Repos {
		b, a := reflect.ValueOf(bare.Repos[i]), reflect.ValueOf(asked.Repos[i])
		for j := range summary.NumField() {
			name := summary.Field(j).Name
			isFact := slices.Contains(factFields, name)
			if isFact && (!b.Field(j).IsNil() || a.Field(j).IsNil()) {
				t.Errorf("%s's %s is answered %v unasked and %v asked, want nil and set", asked.Repos[i].Name, name,
					b.Field(j), a.Field(j))
			}
			if !isFact && !reflect.DeepEqual(b.Field(j).Interface(), a.Field(j).Interface()) {
				t.Errorf("%s's %s differs when its facts are asked, and it is no fact: name it in factFields",
					asked.Repos[i].Name, name)
			}
		}
		want, err := counted(t, n, ada, "", asked.Repos[i].Name)
		if err != nil {
			t.Fatal(err)
		}
		if got := asked.Repos[i].ChangeCounts; got == nil || *got != *want {
			t.Errorf("%s's facts count %+v, and a counts read of it %+v", asked.Repos[i].Name, got, want)
		}
	}
	detail := read(gitlet.ReadQuery{Kind: gitlet.ReadRepo, Repo: "web", RepoFacts: true})
	if want, _ := counted(t, n, ada, "", "web"); detail.Repo == nil || detail.Repo.ChangeCounts == nil ||
		*detail.Repo.ChangeCounts != *want {
		t.Errorf("web's detail with its facts answered %+v, want its counts %+v", detail.Repo, want)
	}
	if plain := read(gitlet.ReadQuery{Kind: gitlet.ReadRepo, Repo: "web"}); plain.Repo.ChangeCounts != nil {
		t.Errorf("web's detail answered its counts unasked: %+v", plain.Repo.ChangeCounts)
	}

	// A PAGE IS MEASURED WITH ITS FACTS: a byte bound one summary wide, with them, cuts after one.
	first := asked.Repos[0]
	gitlet.SetPageLimits(n.reader, 10, gitlet.SummaryBytes(first))
	if got := read(gitlet.ReadQuery{Kind: gitlet.ReadRepos, RepoFacts: true}); len(got.Repos) != 1 ||
		got.Next == "" {
		t.Errorf("a page bounded to one summary with its facts answered %d of them (next %q), want one",
			len(got.Repos), got.Next)
	}
}

// EVERY COUNT PART IS A KEY OF ITS OWN: each part names one bool key of CountsQuery and one
// pointer key of Counts by the same JSON name, Ask sets it, and neither holds a key no part names.
// Mutation: a part added as a value of a shared list.
func TestEveryCountPartIsAKeyOfItsOwn(t *testing.T) {
	t.Parallel()
	keys := func(typ reflect.Type, kind reflect.Kind) map[string]string {
		out := map[string]string{}
		for f := range typ.Fields() {
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if f.Type.Kind() != kind {
				t.Errorf("%s.%s is a %s, want every part a %s of its own", typ.Name(), f.Name, f.Type.Kind(), kind)
			}
			out[name] = f.Name
		}
		return out
	}
	asked := keys(reflect.TypeFor[gitlet.CountsQuery](), reflect.Bool)
	answered := keys(reflect.TypeFor[gitlet.Counts](), reflect.Pointer)
	parts := gitlet.CountParts()
	if len(asked) != len(parts) || len(answered) != len(parts) {
		t.Errorf("CountsQuery holds %d keys and Counts %d for %d parts", len(asked), len(answered), len(parts))
	}
	for _, p := range parts {
		var q gitlet.CountsQuery
		field, ok := asked[string(p)]
		if !ok || !q.Ask(p) || !reflect.ValueOf(q).FieldByName(field).Bool() || !p.Valid() {
			t.Errorf("the part %s is no key of CountsQuery that Ask sets", p)
		}
		if _, ok := answered[string(p)]; !ok {
			t.Errorf("the part %s is no key of Counts", p)
		}
	}
	var q gitlet.CountsQuery
	if q.Ask("later") || gitlet.CountPart("later").Valid() {
		t.Error("a part this build does not count was asked")
	}
}
