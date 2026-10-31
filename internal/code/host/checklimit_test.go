package host_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A CHECK'S BOX PAST ITS RE-SYNCS IS ANSWERED AT ITS SESSION, as a coding
// run's box is: a check's box asking for an object nobody pushed takes one
// barrier a request until its attempt's sixteen are spent, and the
// seventeenth appends none, answered in upload-pack's own form naming the
// object and when the check's re-syncs come back. Another attempt of the
// repository's checks finds its repository's checks' bucket spent; and an
// attempt the rows do not hold is refused on the stale read alone, saying it
// was not judged again — a box whose attempt was swept asking in a loop paid
// one barrier a request before.
//
// Mutations: no bucket for a check (boxResync answering nil for one) — the
// seventeenth fetch appends a barrier — red; the attempt's second read taken
// without the bucket — the unknown attempt is read through the barrier — red.
func TestACheckBoxPastItsResyncsIsAnsweredAtItsSession(t *testing.T) {
	t.Parallel()
	rec := &recording{}
	c, head := boxCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: rec.wrap},
		ResyncClock: (&manualClock{t: time.Now()}).now})
	f := c.Fleet.(*codetest.SoloFleet)
	rows := c.Rows()
	tokens := codetest.NewCheckTokens(t)
	attempt := func(name string, n int, queued bool) string {
		key := code.CheckKey{RepoID: rows.Repo.ID, OID: head, Target: "refs/heads/main", Name: name, Attempt: n}
		if queued {
			queueCheck(t, f, key)
		}
		return tokens.Mint(host.CheckRunRef{Attempt: key, Repo: codetest.RepoName}, 10*time.Minute)
	}
	first, second, unknown := attempt("unit", 1, true), attempt("lint", 1, true), attempt("unit", 2, false)
	missing := randomOID()
	fetch := func(token string) (barriers int, answer string) {
		t.Helper()
		before := rec.linearizable()
		resp, body := boxDo(t, c, 0, codetest.RepoName, boxUploadPack, host.CheckUser, token, v2,
			fetchWants([]string{missing}))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("a check's box's fetch of %s answered %d %q", missing, resp.StatusCode, short(body))
		}
		return rec.linearizable() - before, body
	}

	for i := range code.ResyncBurst {
		if n, body := fetch(first); n == 0 || !strings.Contains(body, "not our ref") {
			t.Fatalf("re-sync %d of the check's %d took %d barriers and answered %q, want one and git's own "+
				"not our ref", i+1, code.ResyncBurst, n, short(body))
		}
	}
	n, body := fetch(first)
	want := "ERR " + missing + " is not in " + codetest.RepoName + " at the position this check was queued at"
	if n != 0 || !strings.Contains(body, want) ||
		!strings.Contains(body, "(not re-judged at linearizable: this check's re-syncs are spent until ") {
		t.Errorf("past its %d re-syncs the check's fetch took %d barriers and answered %q, want none and %q",
			code.ResyncBurst, n, short(body), want)
	}
	if n, body := fetch(second); n != 0 || !strings.Contains(body, "its repository's checks' re-syncs are spent") {
		t.Errorf("another attempt of the repository's checks took %d barriers and answered %q, want its "+
			"repository's checks' bucket spent", n, short(body))
	}

	reads := len(rec.seen("read:" + string(code.ReadCheckBox)))
	resp, answer := boxDo(t, c, 0, codetest.RepoName, boxFetchAdvert, host.CheckUser, unknown, v2, "")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(answer, "is unknown here") ||
		!strings.Contains(answer, "not re-judged at linearizable") {
		t.Errorf("an attempt the rows do not hold, past the checks' re-syncs, answered %d %q, want 403 "+
			"check_unknown saying it was not judged again", resp.StatusCode, short(answer))
	}
	for _, r := range rec.seen("read:" + string(code.ReadCheckBox))[reads:] {
		if r.level == statelog.ReadLinearizable {
			t.Error("an attempt the rows do not hold was read through the barrier past the checks' re-syncs")
		}
	}
}

// A CHECK HAS ONE BURST ON A NODE, ITS TRANSFERS' AND ITS /box/code/ READS'
// TOGETHER: a check whose box spent its attempt's re-syncs on a node fetching
// the commit it checks, which nobody pushed, has its read of that commit at
// /box/code/, answered by the same node, answered at its session — no
// barrier, saying the check's re-syncs are spent — and that read counted with
// the box's re-syncs (crewlet.code.box.resyncs{outcome=spent}), because the
// code host and the query server draw on the node's ONE set of buckets.
//
// Mutations: the code host keeping its boxes' buckets apart from the query
// server's — the read takes a barrier at the head — red; the query server's
// check draws uncounted — red.
func TestACheckHasOneBurstOnANode(t *testing.T) {
	t.Parallel()
	rec := &recording{}
	c, _ := boxCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: rec.wrap},
		ResyncClock: (&manualClock{t: time.Now()}).now})
	f := c.Fleet.(*codetest.SoloFleet)
	node := c.Fleet.Nodes()[0]
	missing := randomOID()
	key := code.CheckKey{RepoID: c.Rows().Repo.ID, OID: missing, Target: "refs/heads/main", Name: "unit",
		Attempt: 1}
	queueCheck(t, f, key)
	token := codetest.NewCheckTokens(t).Mint(host.CheckRunRef{Attempt: key, Repo: codetest.RepoName},
		10*time.Minute)
	for i := range code.ResyncBurst {
		before := rec.linearizable()
		resp, body := boxDo(t, c, 0, codetest.RepoName, boxUploadPack, host.CheckUser, token, v2,
			fetchWants([]string{missing}))
		if resp.StatusCode != http.StatusOK || rec.linearizable() == before || !strings.Contains(body, "not our ref") {
			t.Fatalf("re-sync %d of the check's %d answered %d %q with %d barriers, want one and git's own not "+
				"our ref", i+1, code.ResyncBurst, resp.StatusCode, short(body), rec.linearizable()-before)
		}
	}

	before, spent := rec.linearizable(), counted(node.Metrics(), metrics.CodeBoxResyncs, "spent")
	_, err := node.GitRead(t.Context(), code.GitQuery{Kind: code.GitTree, RepoID: key.RepoID, Check: &key,
		Home: true, Tree: &code.TreeQuery{Rev: missing}})
	if !errors.Is(err, code.ErrObjectNotFound) ||
		!strings.Contains(err.Error(), "(not re-judged at linearizable: this check's re-syncs are spent until ") ||
		rec.linearizable() != before {
		t.Errorf("the check's read of %s at /box/code/ on the node its box spent the attempt's re-syncs on = %v "+
			"with %d barriers, want it answered at its session, saying the check's re-syncs are spent, with none",
			missing, err, rec.linearizable()-before)
	}
	if got := counted(node.Metrics(), metrics.CodeBoxResyncs, "spent") - spent; got != 1 {
		t.Errorf("the check's read past its re-syncs counted %s{outcome=spent} %d times, want once",
			metrics.CodeBoxResyncs, got)
	}
}
