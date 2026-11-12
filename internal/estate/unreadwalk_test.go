package estate

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/rendezvous"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// What a walk does with a node whose build cannot read a request whole, past
// asking the next node: who fills a repository's copy, which later requests
// the node is passed over for, and which reads a caller carries on.

// A HOME THAT CANNOT READ A REQUEST WHOLE PASSES THE FILL. A repository's home
// of an older build refuses a git read, an engine write or a merge preview
// carrying what its build does not declare, before its git half is asked
// anything — so no copy is coming there for it, and the runner-up, of the
// asking node's build and holding no copy, is the one the walk lets fill: it is
// stamped home, fills and answers. Kept on the home, the runner-up was asked
// unstamped, answered that it holds no copy, and the request failed with the
// refusal — although a node of the asking build could serve it.
//
// Mutation: drop the refusal from passesFill and every one of the three fails
// with the refusal, nobody stamped.
func TestAHomeThatCannotReadARequestWholePassesTheFill(t *testing.T) {
	t.Parallel()
	for name, ask := range map[string]func(*fleet, string) (string, error){
		"a git read": func(f *fleet, repo string) (string, error) {
			a, err := f.client.Code().GitRead(t.Context(), treeOf(repo))
			return a.Rev, err
		},
		"an engine write": func(f *fleet, repo string) (string, error) {
			res, err := f.client.Code().GitWrite(t.Context(), writeOf(repo))
			return res.Commit, err
		},
		"a merge preview": func(f *fleet, repo string) (string, error) {
			_, err := f.client.Code().MergePreview(t.Context(), previewOf(repo))
			return "", err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t)
			holders := []string{"git-a", "git-b", "git-c"}
			halves := f.gitletNodes(t, holders)
			repo := repoID(23)
			order := rendezvous.Order(repo, holders)
			for _, n := range order[1:] {
				halves[n].set(func(g *fakeGit) { g.held = map[string]bool{} })
			}
			f.nodes[order[0]].set(func(n *fakeNode) { n.undeclared = []string{"args.repo_id"} })
			answered, err := ask(f, repo)
			if err != nil {
				t.Fatalf("%s whose home cannot read it whole = %v, want its runner-up %s to fill and answer",
					name, err, order[1])
			}
			if answered != "" && answered != order[1] {
				t.Errorf("%s was answered by %s, want the runner-up %s", name, answered, order[1])
			}
			runnerUp := halves[order[1]]
			home := slices.ContainsFunc(runnerUp.reads(), func(q gitlet.GitQuery) bool { return q.Home }) ||
				slices.ContainsFunc(runnerUp.writes(), func(w askedWrite) bool { return w.w.Home }) ||
				slices.ContainsFunc(runnerUp.previews(), func(q gitlet.MergePreviewQuery) bool { return q.Home })
			if !home {
				t.Errorf("the runner-up %s was not stamped home for %s: no copy was coming on the home", order[1], name)
			}
			if third := halves[order[2]]; len(third.reads())+len(third.writes())+len(third.previews()) != 0 {
				t.Errorf("the third node was asked %s, want nothing: the runner-up served", name)
			}
		})
	}
}

// A REFUSAL MOVES ONLY THE REQUESTS CARRYING WHAT IT REFUSED. A repository's
// home of an older build refuses a recursive listing of one repository, its
// build not declaring `recursive`, and serves every other request: a read of
// another repository it is home to is still asked of it and answered there; a
// second recursive read is asked of the runner-up first, which the walk lets
// fill, and never of the home. Declined for the whole operation, the other
// repository's read was moved off its home for thirty seconds, and a runner-up
// holding no copy of it filled one — a fill for every repository the node is
// home to, to save one refused round trip.
//
// Mutation: remember the refusal for the whole operation (markDeclined in place
// of markUnread in Router.settle) and the other repository's read moves off its
// home.
func TestARefusalMovesOnlyTheRequestsCarryingWhatItRefused(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	holders := []string{"git-a", "git-b", "git-c"}
	halves := f.gitletNodes(t, holders)
	var listed, other, home string
	for i := 100; other == ""; i++ {
		repo := repoID(i)
		h := rendezvous.Order(repo, holders)[0]
		switch {
		case listed == "":
			listed, home = repo, h
		case h == home:
			other = repo
		}
	}
	for _, n := range holders {
		if n != home {
			halves[n].set(func(g *fakeGit) { g.held = map[string]bool{} })
		}
	}
	f.nodes[home].set(func(n *fakeNode) { n.undeclared = []string{"args.tree.recursive"} })
	recursive := func(repo string) gitlet.GitQuery {
		q := treeOf(repo)
		q.Tree.Recursive = true
		return q
	}
	runnerUp := rendezvous.Order(listed, holders)[1]
	if a, err := f.client.Code().GitRead(t.Context(), recursive(listed)); err != nil || a.Rev != runnerUp {
		t.Fatalf("a recursive read the home cannot read = (%s, %v), want the runner-up %s's", a.Rev, err, runnerUp)
	}
	if a, err := f.client.Code().GitRead(t.Context(), treeOf(other)); err != nil || a.Rev != home {
		t.Fatalf("a read of another repository after the refusal = (%s, %v), want its home %s's: it carries "+
			"nothing the home refused", a.Rev, err, home)
	}
	f.nodes[home].mu.Lock()
	before := f.nodes[home].requests
	f.nodes[home].mu.Unlock()
	if a, err := f.client.Code().GitRead(t.Context(), recursive(listed)); err != nil || a.Rev != runnerUp {
		t.Fatalf("a second recursive read = (%s, %v), want the runner-up %s's", a.Rev, err, runnerUp)
	}
	f.nodes[home].mu.Lock()
	after := f.nodes[home].requests
	f.nodes[home].mu.Unlock()
	if after != before {
		t.Errorf("the home was asked the second recursive read, want it passed over for a request carrying " +
			"what it refused")
	}
}

// A PATH AS A REFUSAL NAMES IT IS MATCHED AGAINST THE REQUEST: a member at
// every depth, a list's member in ANY of its elements, a list of lists, and
// the actor beside the arguments — and nothing the request does not hold.
//
// Mutation: match a list's member in its first element alone and the path
// held by the second element is missed.
func TestAPathAsARefusalNamesItIsMatchedAgainstTheRequest(t *testing.T) {
	t.Parallel()
	var args, actor any
	if err := json.Unmarshal([]byte(`{"Ending":{"went_on":true},"commands":[{"ref":"a"},{"new":"b"}],`+
		`"grid":[[{"cell":1}]]}`), &args); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"provenance":{"Zone":"Europe/Paris"}}`), &actor); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{"args": args, "actor": actor}
	for path, want := range map[string]bool{
		"args.Ending.went_on":    true,
		"args.commands[].new":    true,
		"args.commands[].ref":    true,
		"args.grid[][].cell":     true,
		"actor.provenance.Zone":  true,
		"args.commands[].forced": false,
		"args.Ending.exhausted":  false,
		"args.missing":           false,
		"actor.provenance.Apps":  false,
		"result.Manifest.files":  false,
	} {
		if got := carries(doc, path); got != want {
			t.Errorf("carries(%q) = %v, want %v", path, got, want)
		}
	}
}

// A CARRIED READ IS READ WHOLE, AND THE SHOWN ONE IS NOT. A version a publish
// carries on is refused where it holds a key this build does not declare — the
// publish would write it back short — while the same version read to show it
// decodes what this build declares of it: refusing that one blinded every seat,
// screen and artifact origin on an older node to a version a newer node
// published, for nothing it would lose.
//
// Mutation: declare artifacts.version answered whole, as it was, and the shown
// version is refused too.
func TestACarriedReadIsReadWholeAndTheShownOneIsNot(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	answer := json.RawMessage(`{"number":3,"manifest":{"files":[{"path":"index.html","integrity":"x"}]}}`)
	for _, read := range []op[artifactVersionArgs, artifactsdomain.Version]{opArtifactVersionCarried,
		opArtifactVersion} {
		x, err := exchangesOf(read.spec, read, nil, artifactVersionArgs{ID: artifacts.ID{}, Number: 3}, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		value, final, err := f.client.settle(t.Context(), read.spec, x, "data-a",
			reply{Node: "data-a", Result: answer}, &walk{})
		var unread *ErrUnknownKeys
		switch carried := read.spec == opArtifactVersionCarried.spec; {
		case !final:
			t.Errorf("%s's answer did not settle the walk", read.spec.name)
		case carried && (!errors.As(err, &unread) || !unread.Answer ||
			!slices.Equal(unread.Keys, []string{"result.manifest.files[].integrity"})):
			t.Errorf("%s's answer holding a key this build does not declare settled as %v, want it refused "+
				"naming the key", read.spec.name, err)
		case !carried && (err != nil || value.(artifactsdomain.Version).Number != 3):
			t.Errorf("%s's answer settled as (%+v, %v), want what this build declares of it", read.spec.name,
				value, err)
		}
	}
}

// A VIEW SAVED AGAIN IS LISTED WHOLE, AND THE STRIP A SCREEN SHOWS IS NOT.
// save_work_view replaces a view whole, by an id list_work_views lists, with
// what its call re-sends of what the listing showed — so a node of an older
// build, listing a strip a data node of a newer one answers with a field on a
// view that this build does not declare, is refused the listing rather than
// shown the view short: a rename built from it would save the field away, on
// every node. The same strip read to show it, as the dashboard and the API
// read it, answers what this build declares of it.
//
// Mutation: answer Work.CarriedViews from tracker.views, the read a screen
// shows, and the listing comes back without the field.
func TestAViewSavedAgainIsListedWholeAndTheStripShownIsNot(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	workspace := worklet.Container{Kind: worklet.ContainerWorkspace}
	f.nodes["data-a"].set(func(n *fakeNode) {
		n.strip = worklet.ViewListing{Views: []worklet.ViewRow{{ID: "v1", Key: "v1", Name: "Ready",
			Type: worklet.ViewTable, Container: workspace}}, Level: statelog.ReadStale, Complete: true}
		n.newerBuild = answersNewer("columns", "views")
	})
	q := worklet.ViewQuery{Container: workspace, Level: statelog.ReadStale}
	_, err := f.client.Work().CarriedViews(t.Context(), q)
	var unread *ErrUnknownKeys
	if !errors.As(err, &unread) || !unread.Answer || !slices.Equal(unread.Keys, []string{"result.views[].columns"}) ||
		!slices.Equal(unread.Nodes, []string{"data-a"}) {
		t.Errorf("a strip listed to save a view of again, answered holding a field this build does not "+
			"declare, = %v; want it refused, naming the field and the node that answered", err)
	}
	shown, err := f.client.Work().Views(t.Context(), q)
	if err != nil || len(shown.Views) != 1 || shown.Views[0].ID != "v1" {
		t.Errorf("the same strip read to show it answered %d views (%v), want the one view this build "+
			"declares of it", len(shown.Views), err)
	}
}

// answersNewer is a data node of a newer build, as a node of an older one meets
// it: every result it answers holds key, set, in every object at path of it —
// each element of a list on the way — a field the answering build declares and
// the asking one does not.
func answersNewer(key string, path ...string) func(reply) reply {
	return func(rep reply) reply {
		if len(rep.Result) == 0 {
			return rep
		}
		doc, err := looseDoc(rep.Result)
		if err != nil {
			panic(fmt.Sprintf("the answer %s gave does not decode: %v", rep.Node, err))
		}
		level := []any{doc}
		for _, member := range path {
			var next []any
			for _, node := range level {
				object, ok := node.(map[string]any)
				if !ok {
					continue
				}
				switch child := object[member].(type) {
				case []any:
					next = append(next, child...)
				case map[string]any:
					next = append(next, child)
				}
			}
			level = next
		}
		for _, node := range level {
			if object, ok := node.(map[string]any); ok {
				object[key] = true
			}
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			panic(err)
		}
		rep.Result = raw
		return rep
	}
}
