package host_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/statelog"
)

// checkTokens is a check's box's token signer over m, at the clock now (nil:
// the wall clock).
func checkTokens(t *testing.T, m runtoken.Material, now func() time.Time) *host.CheckTokens {
	t.Helper()
	tokens, err := host.NewCheckTokens(m, now)
	if err != nil {
		t.Fatal(err)
	}
	return tokens
}

// aCheck is attempt of check name of oid on repository id, its box given the
// repository named repo.
func aCheck(id, repo, oid, name string, attempt int) host.CheckRunRef {
	return host.CheckRunRef{Repo: repo, Attempt: code.CheckKey{RepoID: id, OID: oid, Target: "refs/heads/main", Name: name,
		Attempt: attempt}}
}

// A CHECK'S TOKEN NAMES ITS ATTEMPT: it verifies back to the attempt's key and
// the repository's name the box was given — a check's name and a scratch
// repository's each holding a `.`, which is a token's own separator, carried
// whole — and to nothing past its check's timeout and the box's grace; a
// token of another domain over the same keyring is refused, as is one minted
// under another keyring; and a ref no attempt has, or a timeout no check runs
// for, is a caller's bug, refused by name. The token is known by its shape to
// redaction, as every per-run token is.
//
// Mutations: carry the names raw in the subject — a dotted name's token no
// longer verifies, red; mint for the timeout alone — the token is refused
// inside its grace, red.
func TestACheckTokenNamesItsAttempt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	k := runtoken.KeyMaterial{ID: "k1", Material: "a fleet's key"}
	tokens := checkTokens(t, boxRing("k1", k), clock)
	ref := aCheck(uuid.NewString(), "dev.notes", strings.Repeat("a1", 20), "lint.go", 3)
	token := tokens.Mint(ref, 10*time.Minute)
	if got, ok := tokens.Verify(token); !ok || got != ref {
		t.Fatalf("a minted check token verifies to %+v %v, want %+v", got, ok, ref)
	}
	if got, want := redact.Secrets(token), redact.Marker+"run-token]"; got != want {
		t.Errorf("a check token was redacted to %q, want %q", got, want)
	}
	for _, at := range []struct {
		after time.Duration
		valid bool
	}{
		{10*time.Minute + code.CheckBoxGrace - time.Second, true},
		{10*time.Minute + code.CheckBoxGrace + time.Second, false},
	} {
		later := checkTokens(t, boxRing("k1", k), func() time.Time { return now.Add(at.after) })
		if _, ok := later.Verify(token); ok != at.valid {
			t.Errorf("%s after its mint, a check token of a ten-minute check verifies %v, want %v", at.after,
				ok, at.valid)
		}
	}
	other := checkTokens(t, boxRing("k2", runtoken.KeyMaterial{ID: "k2", Material: "another fleet's key"}), clock)
	if got, ok := other.Verify(token); ok {
		t.Errorf("a node over another keyring admitted a check token: %+v", got)
	}
	bridge, err := runtoken.New(runtoken.Options{Domain: "crewlet.mcp.v1", Material: boxRing("k1", k), Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := tokens.Verify(bridge.Mint("chk~x", time.Hour)); ok {
		t.Errorf("a bridge token over the same keyring was admitted as a check's: %+v", got)
	}
	for name, bad := range map[string]func(){
		"an attempt past the last": func() {
			tokens.Mint(aCheck(ref.Attempt.RepoID, "web", ref.Attempt.OID, "unit", code.MaxCheckAttempts+1),
				time.Minute)
		},
		"a repository no name names": func() {
			tokens.Mint(aCheck(ref.Attempt.RepoID, "Web", ref.Attempt.OID, "unit", 1), time.Minute)
		},
		"no timeout": func() { tokens.Mint(ref, 0) },
		"a timeout past the ceiling": func() {
			tokens.Mint(ref, code.CheckTimeoutCeiling+time.Second)
		},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s was minted", name)
				}
			}()
			bad()
		}()
	}
}

// A RUN'S TOKEN IS NO CHECK'S TOKEN, AND A CHECK'S IS NO RUN'S, though both are
// signed under the box key domain: each verifies only as its own kind, and a
// box route admits each only under its own user name — a run's token
// presented as `check`, or a check's as `run`, is challenged as a token that
// is not valid there, having read no run and no attempt.
//
// Mutations: boxCredential verifying either kind under either user name — the
// crossed requests are admitted, red; a check token's subject opening with a
// UUID — the run tokens' verify takes it, red.
func TestARunTokenIsNoCheckToken(t *testing.T) {
	t.Parallel()
	ring := boxRing("k1", runtoken.KeyMaterial{ID: "k1", Material: "a fleet's key"})
	boxes, checks := boxTokens(t, ring), checkTokens(t, ring, nil)
	run := aRunRef()
	check := aCheck(uuid.NewString(), codetest.RepoName, strings.Repeat("b2", 20), "unit", 1)
	if got, ok := boxes.Verify(checks.Mint(check, time.Minute)); ok {
		t.Errorf("a check's token verified as a run's: %+v", got)
	}
	if got, ok := checks.Verify(boxes.Mint(run)); ok {
		t.Errorf("a run's token verified as a check's: %+v", got)
	}
	for _, crossed := range []struct {
		user, token string
		valid       bool
	}{
		{host.BoxUser, checks.Mint(check, time.Minute), false},
		{host.CheckUser, boxes.Mint(run), false},
		{host.BoxUser, boxes.Mint(run), true},
		{host.CheckUser, checks.Mint(check, time.Minute), true},
	} {
		if got := host.VerifyBox(boxes, checks, crossed.user, crossed.token); got != crossed.valid {
			t.Errorf("a token presented as %q verifies %v, want %v", crossed.user, got, crossed.valid)
		}
	}

	// THROUGH A BOX ROUTE: the fleet's own tokens, crossed.
	var lines logs
	rec := &recording{}
	c, _ := boxCase(t, codetest.SoloOptions{Logger: lines.logger(), Shape: codetest.Shape{WrapEstate: rec.wrap}})
	rows := c.Rows()
	live, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
	runToken := c.Fleet.Runs().Token(live)
	checkToken := codetest.NewCheckTokens(t).Mint(aCheck(rows.Repo.ID, codetest.RepoName, rows.Refs["refs/heads/main"],
		"unit", 1), time.Minute)
	reads := len(rec.seen("read:" + string(code.ReadCheckBox)))
	for _, crossed := range []struct {
		name, user, token, says string
	}{
		{"a check's token as a run's", host.BoxUser, checkToken, "this run token is not valid here"},
		{"a run's token as a check's", host.CheckUser, runToken, "this check token is not valid here"},
	} {
		resp, answer := boxDo(t, c, 0, codetest.RepoName, boxFetchAdvert, crossed.user, crossed.token, "version=2", "")
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(answer, crossed.says) {
			t.Errorf("%s answered %d %q, want git's 401 saying %q", crossed.name, resp.StatusCode, short(answer),
				crossed.says)
		}
	}
	if n := len(rec.seen("read:" + string(code.ReadCheckBox))); n != reads {
		t.Errorf("the crossed tokens read %d attempts, want none", n-reads)
	}
	if n := lines.count("code_box_challenged", `"reason":"token"`); n != 2 {
		t.Errorf("code_box_challenged reason token logged %d times, want twice", n)
	}
}

// THE CHECK'S API IS ADMITTED AS ITS GIT IS: AdmitCheck, the API's admission
// of a check's box's reads of its commit (/box/code/), takes the repository
// from the same wildcard, the same credential and the same attempt's row —
// answering the admitted attempt, its repository and its session — and
// refuses what its git refuses in the same words; and a coding run's token,
// which the box routes of git admit, is refused here, the API answering a
// check's box alone.
//
// Mutation: AdmitCheck admitting a run's token as its git admits one — the
// coding box is admitted, red.
func TestTheCheckAPIAdmitsACheckTokenAsItsGitDoes(t *testing.T) {
	t.Parallel()
	c, head := boxCase(t, codetest.SoloOptions{})
	rows := c.Rows()
	f := c.Fleet.(*codetest.SoloFleet)
	key := code.CheckKey{RepoID: rows.Repo.ID, OID: head, Target: "refs/heads/main", Name: "unit", Attempt: 1}
	queueCheck(t, f, key)
	token := codetest.NewCheckTokens(t).Mint(host.CheckRunRef{Attempt: key, Repo: codetest.RepoName}, time.Minute)
	live, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
	mux := http.NewServeMux()
	mux.HandleFunc(host.PatternBoxCodeTree, func(w http.ResponseWriter, r *http.Request) {
		adm, ok := f.Nodes()[0].Host().AdmitCheck(r.Context(), w, r)
		if !ok {
			return
		}
		_, _ = fmt.Fprintf(w, "admitted %s of %s at %s, %s", adm.Attempt.OID, adm.Repo.Name, adm.Fresh.Level,
			adm.Attempt.State)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	ask := func(repo, user, pass string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(c.Ctx, http.MethodGet, srv.URL+"/box/code/"+repo+"/tree", nil)
		if err != nil {
			t.Fatal(err)
		}
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		got := boxSend(req, srv.Client())
		if got.err != nil {
			t.Fatal(got.err)
		}
		return got.status, got.body
	}
	if status, body := ask(codetest.RepoName, host.CheckUser, token); status != http.StatusOK ||
		body != fmt.Sprintf("admitted %s of %s at %s, running", head, codetest.RepoName, statelog.ReadSession) {
		t.Errorf("a check's token answered %d %q, want its attempt admitted at its session", status, body)
	}
	for _, row := range []struct {
		name, repo, user, pass string
		status                 int
		says                   string
	}{
		{"no credential", codetest.RepoName, "", "", http.StatusUnauthorized, "as `check` with its check's token"},
		{"a coding run's token", codetest.RepoName, host.BoxUser, c.Fleet.Runs().Token(live), http.StatusForbidden,
			"answers a check's box"},
		{"another repository", "other", host.CheckUser, token, http.StatusForbidden, "was given"},
		{"a name no repository takes", "Web", host.CheckUser, token, http.StatusNotFound, "not found"},
	} {
		if status, body := ask(row.repo, row.user, row.pass); status != row.status || !strings.Contains(body, row.says) {
			t.Errorf("%s answered %d %q, want %d saying %q", row.name, status, short(body), row.status, row.says)
		}
	}
}

// queueCheck records attempt key queued on the fleet's data node, dispatched
// for main, its definition unit's, and claimed by runner git-a: a running
// attempt, as a check's box finds its own.
func queueCheck(t *testing.T, f *codetest.SoloFleet, key code.CheckKey) {
	t.Helper()
	served := f.Data().Served()
	for _, req := range []code.ChecksRequest{
		{OpID: statelog.NewOpID(time.Now(), "code_check_queue"), Queued: &code.CheckQueued{CheckKey: key,
			Purpose: code.PurposeTip, Reason: code.QueueDispatch, QueuedAt: time.Now(),
			Def: &code.CheckDef{Name: key.Name, Run: "make test", TimeoutSeconds: 600}}},
		{OpID: statelog.NewOpID(time.Now(), "code_check_claim"), Started: &code.CheckStarted{CheckKey: key,
			Runner: "git-a", Placement: code.PlacementContainer, StartedAt: time.Now()}},
	} {
		if res, err := served.PublishChecks(t.Context(), req); err != nil || res.Row == nil {
			t.Fatalf("publish a check record: %+v %v", res, err)
		}
	}
}

// endCheck records attempt key ended: passed, exiting 0.
func endCheck(t *testing.T, f *codetest.SoloFleet, key code.CheckKey) {
	t.Helper()
	exit := 0
	req := code.ChecksRequest{OpID: statelog.NewOpID(time.Now(), "code_check_end"), Finished: &code.CheckFinished{
		CheckKey: key, State: code.CheckStatePassed, Reason: code.CheckReasonExit, ExitCode: &exit,
		DurationMs: 1_000, FinishedAt: time.Now()}}
	if res, err := f.Data().Served().PublishChecks(t.Context(), req); err != nil || res.Row == nil ||
		res.Row.State != code.CheckStatePassed {
		t.Fatalf("end a check: %+v %v", res, err)
	}
}

// A CHECK'S TOKEN ADMITS ONE REPOSITORY TO READ: a check's box, presenting its
// check's token as `check`, fetches the commit its attempt measures BY ID over
// protocol v2 — shown no ref, its `ls-refs` empty — charged as a run keyed by
// its repository, and its fetch of a `visibility: unit` repository is no
// principal's read. Its push is refused, as is a request in protocol v0, a
// repository other than the one it was given — before anything of the code
// log is read — an attempt the rows do not hold, read through the barrier
// before it is refused, and its attempt once ended, refused on the stale
// read that shows it, no barrier paid: the attempt's ROW is the authority,
// read on every request, and the token its backstop.
//
// Mutations: admit on the token alone (skip the row's read) — the ended and
// the unknown attempts are admitted, red; refuse an ended attempt only after
// a linearizable read — the ended row's barrier, red; match the name after
// the row's read — the other repository's request reads an attempt, red.
func TestACheckTokenAdmitsOneRepositoryToRead(t *testing.T) {
	t.Parallel()
	var lines logs
	rec := &recording{}
	c, f, head := unitCase(t, codetest.SoloOptions{Logger: lines.logger(), Shape: codetest.Shape{WrapEstate: rec.wrap}})
	rows := c.Rows()
	key := code.CheckKey{RepoID: rows.Repo.ID, OID: head, Target: "refs/heads/main", Name: "lint.go", Attempt: 1}
	queueCheck(t, f, key)
	tokens := codetest.NewCheckTokens(t)
	token := tokens.Mint(host.CheckRunRef{Attempt: key, Repo: codetest.RepoName}, 10*time.Minute)
	box := codetest.NewCheckBox(t, f, token)
	ran := host.RecordGit(f.Nodes()[0].Host())
	from, fetched := len(ran()), len(f.Fetched())
	stale := len(rec.seen("read:" + string(code.ReadCheckBox)))

	// THE FETCH, by id, of a commit the box is shown no ref of — the
	// single-commit recipe a check's box runs (sandbox.CloneSingleSHA): the
	// code host its remote, the commit fetched at depth one, blobless, and
	// checked out, its files fetched from the promisor as the checkout needs
	// them.
	work := box.Init()
	box.Git().Run(t, work, "remote", "add", "origin", c.BoxURL(0))
	if _, stderr, err := box.Git().Try(t, work, nil, "fetch", "--quiet", "--depth=1", "--filter=blob:none",
		"origin", head); err != nil {
		t.Fatalf("a check's box fetching its commit: %v\n%s", err, stderr)
	}
	if _, stderr, err := box.Git().Try(t, work, nil, "checkout", "--quiet", "--detach", head); err != nil {
		t.Fatalf("a check's box checking its commit out: %v\n%s", err, stderr)
	}
	if got := box.Head(work); got != head {
		t.Fatalf("a check's box checked out %s, want its commit %s", got, head)
	}
	if refs := box.LsRemote(c.BoxURL(0), 2); len(refs) != 0 {
		t.Errorf("a check's box was shown refs: %v", refs)
	}
	want := gitproc.Share{Class: gitproc.ShareRun, Principal: "check:" + rows.Repo.ID}
	for _, cmd := range ran()[from:] {
		if cmd.Class == gitproc.ClassTransfer && cmd.Share != want {
			t.Errorf("the check's git %v is charged to %+v, want %+v", cmd.Args, cmd.Share, want)
		}
	}
	if !slices.ContainsFunc(ran()[from:], transferring("upload-pack")) {
		t.Errorf("the check's fetch ran no upload-pack: %v", argsOf(ran()[from:]))
	}
	if got := f.Fetched()[fetched:]; len(got) != 0 {
		t.Errorf("a check's box's fetch of a unit repository was recorded as a principal's read: %+v", got)
	}
	reads := rec.seen("read:" + string(code.ReadCheckBox))[stale:]
	if slices.ContainsFunc(reads, func(c call) bool { return c.level != statelog.ReadStale }) {
		t.Errorf("a live attempt's admission read it through the barrier: %+v", reads)
	}
	if resolves := rec.seen("resolve"); !slices.ContainsFunc(resolves, func(c call) bool {
		return c.level == statelog.ReadSession
	}) {
		t.Errorf("the repository was never resolved at the check's session: %+v", resolves)
	}

	refused := func(name, repo, tail, protocol, token string, status int, reason, says string) {
		t.Helper()
		before := len(rec.seen("read:" + string(code.ReadCheckBox)))
		resp, answer := boxDo(t, c, 0, repo, tail, host.CheckUser, token, protocol, "")
		if resp.StatusCode != status || !strings.Contains(answer, says) {
			t.Errorf("%s answered %d %q, want %d saying %q", name, resp.StatusCode, short(answer), status, says)
		}
		if len(boxLines(&lines, `"reason":"`+reason+`"`)) == 0 {
			t.Errorf("%s: no line logged reason %s", name, reason)
		}
		if after := rec.seen("read:" + string(code.ReadCheckBox))[before:]; reason == "not_granted" && len(after) != 0 {
			t.Errorf("%s read an attempt before refusing the name: %+v", name, after)
		}
	}
	refused("a push", codetest.RepoName, boxPushAdvert, "", token, http.StatusForbidden, "read_only",
		"pushes nothing")
	refused("a protocol v0 advertisement", codetest.RepoName, boxFetchAdvert, "", token, http.StatusForbidden,
		"protocol_v0", "protocol v2")
	refused("another repository", "open", boxFetchAdvert, "version=2", token, http.StatusForbidden, "not_granted",
		`was given "`+codetest.RepoName+`"`)

	// AN ATTEMPT THE ROWS DO NOT HOLD: read through the barrier, then refused.
	unknown := key
	unknown.Attempt = 2
	before := len(rec.seen("read:" + string(code.ReadCheckBox)))
	refused("an attempt never queued", codetest.RepoName, boxFetchAdvert, "version=2",
		tokens.Mint(host.CheckRunRef{Attempt: unknown, Repo: codetest.RepoName}, time.Minute),
		http.StatusForbidden, "check_unknown", "is unknown here")
	if got := rec.seen("read:" + string(code.ReadCheckBox))[before:]; len(got) != 2 ||
		got[0].level != statelog.ReadStale || got[1].level != statelog.ReadLinearizable {
		t.Errorf("an unknown attempt was read %+v, want stale and then through the barrier", got)
	}

	// THE ATTEMPT ENDED: refused on the stale read that shows it.
	endCheck(t, f, key)
	before = len(rec.seen("read:" + string(code.ReadCheckBox)))
	refused("an ended attempt", codetest.RepoName, boxFetchAdvert, "version=2", token, http.StatusForbidden,
		"check_ended", "has ended (passed)")
	if got := rec.seen("read:" + string(code.ReadCheckBox))[before:]; len(got) != 1 ||
		got[0].level != statelog.ReadStale {
		t.Errorf("an ended attempt was read %+v, want once, stale", got)
	}
	if _, stderr, err := box.Git().Try(t, work, nil, "ls-remote", "origin"); err == nil {
		t.Errorf("a check's box reached its repository once its attempt ended")
	} else if !strings.Contains(stderr, "has ended") {
		t.Errorf("a check's box's refused request printed %q, want why", stderr)
	}
	if leaked := boxLines(&lines, "crewlet_rt1."); len(leaked) != 0 {
		t.Errorf("a check's token was logged: %q", leaked)
	}
}
