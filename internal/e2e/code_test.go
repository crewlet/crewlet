package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/rendezvous"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// The code host, end to end: a person's own git pushing to and fetching from
// real engines, each serving git on its API as `crewlet run` serves it.
//
// The code host's own suites certify it against one data node and three
// (internal/code/codetest, internal/code/codecluster), with the production
// router between members built in the suite. What only an ENGINE can show is
// the composition `crewlet run` produces: the router, the committer, the
// cache, the host and the guard built by the engine from a company's
// configuration, and a configuration write — the one the dashboard makes —
// reaching every member's repositories.

// e2eDevID and e2eDevToken are a PERSON'S Tier A token: the identity
// directory binds its login, `token:e2e-dev`, to the developer's human seat
// ([bindDev]), so a push made with it is that person's own — the only
// credential a git push is taken under, since a token bound to nobody fetches
// only.
const (
	e2eDevID    = "e2e-dev"
	e2eDevToken = "e2e-dev-token-0123456789abcdef"
)

// withDevCredential adds the developer's token to boot, beside the fixture's
// ([withCredential]). It carries the tracker's grant only because Tier A
// refuses a token that states none: nothing a push does reads it, since what a
// person may do to a repository is their standing on it (ADR-0081), never a
// grant a token carries.
func withDevCredential(boot *config.Bootstrap) {
	boot.API.Auth.Tokens = append(boot.API.Auth.Tokens,
		config.APIToken{ID: e2eDevID, Token: e2eDevToken, Grants: []iam.Grant{iam.GrantWorklet}})
}

// bindDev binds the developer's token to the developer's human seat through
// n's identity directory — what `crewlet iam create -kind machine -login
// token:e2e-dev -seat dev -grants none` sends — and waits until n and every one
// of others reports the binding: each node resolves a token's seat from its own
// copy of the directory, so a push through a member that had not applied the
// row would be a push by a token bound to nobody.
func bindDev(t *testing.T, n *node, others ...*node) {
	t.Helper()
	login := iam.TokenLogin(e2eDevID)
	status, created := tierA(t, n, http.MethodPost, "/iam/people", map[string]any{
		"kind": iam.KindMachine, "login": login, "name": "Developer's git",
		"seat": "dev", "grants": []iam.Grant{}, "reason": "the developer's git credential",
	})
	if status != http.StatusCreated {
		t.Fatalf("POST /iam/people for %s on %s answered %d: %v", login, n.id, status, created)
	}
	for _, member := range append([]*node{n}, others...) {
		var tokens []any
		waitFor(t, fmt.Sprintf("%s to bind %s to the developer", member.id, login), func() bool {
			code, body := tierA(t, member, http.MethodGet, "/iam/node-tokens", nil)
			tokens, _ = body["tokens"].([]any)
			for _, raw := range tokens {
				if token, _ := raw.(map[string]any); token["id"] == e2eDevID {
					return code == http.StatusOK && token["row"] == "held" && token["seat"] == "dev"
				}
			}
			return false
		}, func() string { return fmt.Sprintf("%v", tokens) })
	}
}

// withCode amends the golden company into one that hosts code: a developer's
// human seat — which [bindDev] binds [e2eDevID] to — and a `code:` block
// declaring repo, owned by Engineering, its main branch protected.
//
// The seat goes in after the FIRST "roles:\n", which is the company's own
// (companyDoc keeps its units below the seats for exactly this), and the
// block at the document's end, where no other key's indentation reaches it.
func withCode(repo string) func(doc string) string {
	return codeCompany(repo, false)
}

// codeCompany is [withCode] with the repository archived or not — the one
// document a configuration write changes to archive it, and back.
func codeCompany(repo string, archived bool) func(doc string) string {
	return func(doc string) string {
		seat := `  - name: Developer
    handle: dev
    kind: human
`
		doc = strings.Replace(doc, "roles:\n", "roles:\n"+seat, 1)
		return doc + fmt.Sprintf(`code:
  native: true
  repos:
    - name: %s
      owner: {unit: Engineering}
      archived: %t
      protect:
        - branch: main
`, repo, archived)
	}
}

// fleetRepo is the repository the fleet's company declares, and the only one
// [claimPersonsPushAndFetchAcrossTheFleet] reads.
const fleetRepo = "fleet-web"

// fleetCompanyDoc is the company every fleet member boots on: the golden
// company over model, hosting [fleetRepo].
func fleetCompanyDoc(model *scriptedModel) string {
	return withCode(fleetRepo)(goldenCompany(model))
}

// gitURL is repo's address on n's API, as a person's git is given it.
func gitURL(n *node, repo string) string {
	return n.server.URL + "/git/" + repo + ".git"
}

// resolveRepo is repo as n's estate router answers it, read LINEARIZABLE:
// whichever data node's copy answered had applied everything on the code log
// when it was asked, so an answer of a status says the record that made it so
// is ON THE LOG. It says nothing of n's own copy — the router answers from a
// peer where n's lags or is out of service — and nothing here needs it to:
// every read that judges a push against the status is one of its own at that
// level (see the archived loop in [claimPersonsPushAndFetchAcrossTheFleet]).
func resolveRepo(t *testing.T, n *node, repo string) (code.RepoRef, error) {
	t.Helper()
	seam, ok := engine.OperatorCode(n.engine)
	if !ok {
		return code.RepoRef{}, errors.New("the node serves no code operations")
	}
	return seam.Resolve(t.Context(), code.ResolveQuery{Name: repo,
		Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
}

// awaitRepo waits until every one of nodes answers repo in status want, and
// answers the repository as the last of them did.
//
// WAITED FOR, never assumed from an applied configuration: an apply ASKS for
// the declared repositories and the node's code reconciler decides them beside
// it (internal/engine's codeReconciler), so a node reporting an epoch applied
// may not have published its records yet — and a push judged before the
// archive's record is on the log is judged against the repository it was.
func awaitRepo(t *testing.T, repo string, want code.RepoStatus, nodes ...*node) code.RepoRef {
	t.Helper()
	var got code.RepoRef
	for _, n := range nodes {
		var last error
		waitFor(t, fmt.Sprintf("%s to answer %s %s", n.id, repo, want), func() bool {
			got, last = resolveRepo(t, n, repo)
			return last == nil && got.Status == want
		}, func() string { return fmt.Sprintf("status %q, error %v", got.Status, last) })
	}
	return got
}

// commitBatches is how many batches of pushes n's committer has put on the
// code log — read off n's own recorder ([metrics.CodeCommitBatchSize]),
// because which member COMMITTED a push is measured only by that member: the
// push's row names the member that received it.
func commitBatches(n *node) uint64 {
	var batches uint64
	for _, s := range n.metrics.Read() {
		if s.Name == metrics.CodeCommitBatchSize {
			batches += s.Count
		}
	}
	return batches
}

// awaitRouting waits until every member's estate router names every member
// as a data node it asks ([engine.Engine.RoutedDataNodes]) — the set a
// repository's committer is the first of, by rendezvous order — and answers
// the members, sorted.
//
// WAITED FOR, because a router routes by a LISTING of the fleet's presence: a
// member it has not yet heard claim is not in it. Each router's view hears
// every member's claim and lists again within about a second (internal/coord's
// LeaseView), so a fleet booted at once agrees on every committer about a
// second after the last member's claim — and this claim, which counts the
// batches each member committed, must not race that second: a push routed
// inside it is committed by a member that has not heard the committer join,
// which lands it, the broker arbitrating, and is not the one committer this
// claim is about. A fresh presence listing ([cluster.whole]'s) is not this,
// since it can name a member a router has not listed yet.
func (c *cluster) awaitRouting(t *testing.T) []string {
	t.Helper()
	var members []string
	for _, n := range c.nodes {
		members = append(members, n.id)
	}
	slices.Sort(members)
	for _, n := range c.nodes {
		var routed []string
		var last error
		waitFor(t, fmt.Sprintf("%s's router to name every member", n.id), func() bool {
			routed, last = n.engine.RoutedDataNodes()
			return last == nil && slices.Equal(routed, members)
		}, func() string { return fmt.Sprintf("routes to %v, error %v", routed, last) })
	}
	return members
}

// putCompany writes doc as the company's configuration through n's PUT
// /config — the write the dashboard and `crewlet config apply` make — and
// answers the epoch it activated.
func putCompany(t *testing.T, n *node, doc, summary string) int64 {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, n.server.URL+"/config",
		strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/yaml")
	req.Header.Set("X-Summary", summary)
	resp, err := n.server.Client().Do(present(req))
	if err != nil {
		t.Fatalf("PUT /config on %s: %v", n.id, err)
	}
	defer resp.Body.Close()
	var answer struct {
		Epoch int64  `json:"epoch"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatalf("PUT /config on %s answered %d and no JSON: %v", n.id, resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusCreated || answer.Epoch == 0 {
		t.Fatalf("PUT /config on %s answered %d %+v", n.id, resp.StatusCode, answer)
	}
	return answer.Epoch
}

// awaitEpoch waits until every member's /health says it has applied epoch —
// the field an operator reads to see a write reach the fleet.
func (c *cluster) awaitEpoch(t *testing.T, epoch int64) {
	t.Helper()
	for _, n := range c.nodes {
		var applied any
		waitFor(t, fmt.Sprintf("%s to apply configuration epoch %d", n.id, epoch), func() bool {
			_, body := operatorCall(t, n, http.MethodGet, "/health")
			applied = body["applied_epoch"]
			at, ok := applied.(float64)
			return ok && int64(at) >= epoch
		}, func() string { return fmt.Sprintf("applied_epoch %v", applied) })
	}
}

// PERSONS PUSH AND FETCH ACROSS THE FLEET.
//
// A person's own git, with their own bound token as its credential, against
// three engines each serving git on its API: a push through one member is
// visible through every member AT ONCE, under both protocols — the
// advertisement is linearizable, so a member whose copy has not applied the
// push waits for it rather than answering what it holds — and a clone through
// another holds it whole. The repository's pushes meet at one committer
// whichever member received them — the three that land go through three
// members, and one member, the first of the repository's rendezvous order,
// commits all three — and archiving the repository in the company's
// configuration — written once, on one member, through the route the
// dashboard writes — makes every member refuse a push in git's own words while
// each still serves a fetch.
//
// THE CONFIGURATION IS GIVEN BACK before the claim ends: the agreement claim
// after it compares every replicated row of every member, a settled repository
// among them. The document written is member 0's — its scripted model's
// address included — and every member applies it as one company's one
// document; nothing after this claim runs a turn.
func claimPersonsPushAndFetchAcrossTheFleet(t *testing.T, c *cluster) {
	// THE BOOT DECLARED IT, beside the boot rather than inside it: the
	// declaration is decided by each member's code reconciler, so it is
	// waited for rather than assumed.
	web := awaitRepo(t, fleetRepo, code.RepoStatusActive, c.nodes...)

	// ITS COMMITTER is the first of the repository's rendezvous order over
	// the data nodes every member's router asks — every member, once each
	// router has listed the fleet — whichever member a push reaches: the
	// router's one rule for its one keyed operation, so every receiver's
	// pushes meet at one group commit. Counted from here, in batches each
	// member's own committer put on the log.
	members := c.awaitRouting(t)
	committer := rendezvous.Order(web.ID, members)[0]
	before := map[string]uint64{}
	for _, n := range c.nodes {
		before[n.id] = commitBatches(n)
	}

	bindDev(t, c.nodes[0], c.nodes[1:]...)
	dev := codetest.PersonWithToken(t, e2eDevToken)
	work := dev.Init()
	first := dev.Commit(work, map[string]string{"README.md": "# fleet-web\n"}, "first")
	pushed := dev.Push(t.Context(), work, gitURL(c.nodes[1], fleetRepo), "main")
	if s := pushed.Refs["refs/heads/main"]; pushed.Err != nil || s.Flag != '*' {
		t.Fatalf("the first push through %s did not create main: %s", c.nodes[1].id, pushed)
	}

	// AT ONCE, with no wait loop: a member that has not applied the push
	// yet waits for it before it advertises, and a poll here would pass on
	// a host whose advertisement answered whatever its copy held.
	for _, n := range c.nodes {
		for _, v := range []int{0, 2} {
			if got := dev.LsRemote(gitURL(n, fleetRepo), v)["refs/heads/main"]; got != first {
				t.Errorf("%s advertised main at %q under protocol %d, straight after a push "+
					"through %s landed %s", n.id, got, v, c.nodes[1].id, first)
			}
		}
	}
	clone := dev.Clone(gitURL(c.nodes[2], fleetRepo))
	dev.Git().Run(t, clone, "fsck", "--full")
	if head := dev.Head(clone); head != first {
		t.Errorf("a clone through %s checked out %s, want %s", c.nodes[2].id, head, first)
	}

	// THROUGH ANOTHER MEMBER: whichever member receives a push, it is
	// committed by the repository's one committer (counted at the claim's
	// end), and lands on every member's rows alike.
	second := dev.Commit(work, map[string]string{"README.md": "# fleet-web\n\nsecond\n"}, "second")
	pushed = dev.Push(t.Context(), work, gitURL(c.nodes[0], fleetRepo), "main")
	if pushed.Err != nil || !pushed.OK("refs/heads/main") {
		t.Fatalf("the second push through %s: %s", c.nodes[0].id, pushed)
	}
	for _, n := range c.nodes {
		if got := dev.LsRemote(gitURL(n, fleetRepo), 2)["refs/heads/main"]; got != second {
			t.Errorf("%s advertised main at %q after the second push landed %s", n.id, got, second)
		}
	}

	// ARCHIVED THROUGH CONFIGURATION, on one member: every member applies
	// the write, and every member then refuses a push.
	member := c.nodes[0]
	c.awaitEpoch(t, putCompany(t, member,
		codeCompany(fleetRepo, true)(goldenCompany(member.model)),
		"archive fleet-web"))
	awaitRepo(t, fleetRepo, code.RepoStatusArchived, c.nodes...)
	third := dev.Commit(work, map[string]string{"README.md": "# fleet-web\n\nthird\n"}, "third")
	for _, n := range c.nodes {
		// REFUSED AT THE ADVERTISEMENT, in the domain's own sentence: the
		// archive's record is on the log (awaitRepo above), and a push's
		// advertisement syncs the member's cache LINEARIZABLE and judges
		// the repository again on the synced row — whatever the member's
		// stale admission answered before it — so the push is refused
		// before git sends a byte, and git prints the refusal as the
		// remote's error.
		refused := dev.Push(t.Context(), work, gitURL(n, fleetRepo), "main")
		if refused.Err == nil || !strings.Contains(refused.Stderr, code.ArchivedDetail(fleetRepo, code.ManagedConfig)) {
			t.Errorf("a push to the archived repository through %s was not refused in the "+
				"domain's words (%q): %s", n.id, code.ArchivedDetail(fleetRepo, code.ManagedConfig), refused)
		}
		// AND A FETCH IS SERVED: archived is read-only, never unreachable.
		dev.Git().Run(t, clone, "fetch", "--quiet", gitURL(n, fleetRepo), "main")
		if got := strings.TrimSpace(dev.Git().Run(t, clone, "rev-parse", "FETCH_HEAD")); got != second {
			t.Errorf("a fetch of the archived repository through %s brought %s, want %s",
				n.id, got, second)
		}
	}

	// UNARCHIVED THE SAME WAY, and a push lands again — through the third
	// member, so every member has received one.
	c.awaitEpoch(t, putCompany(t, member, fleetCompanyDoc(member.model), "unarchive fleet-web"))
	awaitRepo(t, fleetRepo, code.RepoStatusActive, c.nodes...)
	pushed = dev.Push(t.Context(), work, gitURL(c.nodes[2], fleetRepo), "main")
	if pushed.Err != nil || !pushed.OK("refs/heads/main") {
		t.Fatalf("a push through %s after the repository was unarchived: %s", c.nodes[2].id, pushed)
	}
	for _, n := range c.nodes {
		if got := dev.LsRemote(gitURL(n, fleetRepo), 2)["refs/heads/main"]; got != third {
			t.Errorf("%s advertised main at %q after the unarchived push landed %s", n.id, got, third)
		}
	}

	// ONE COMMITTER FOR THE THREE PUSHES THAT LANDED, received by three
	// different members: each a batch of its own on the committer — the
	// pushes came one after another — and none on either other member. A
	// receiver that committed for itself would still land every push, the
	// broker's compare-and-set arbitrating, and only this says otherwise:
	// a push's row names the member that RECEIVED it.
	const landed = 3
	for _, n := range c.nodes {
		want := uint64(0)
		if n.id == committer {
			want = landed
		}
		if got := commitBatches(n) - before[n.id]; got != want {
			t.Errorf("%s's committer put %d batches of %s's pushes on the log, want %d: the "+
				"repository's committer is %s, the first of its rendezvous order over %v",
				n.id, got, fleetRepo, want, committer, members)
		}
	}
}
