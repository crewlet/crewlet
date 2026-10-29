package host_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The box routes' requests, as git sends them.
const (
	boxFetchAdvert = "info/refs?service=git-upload-pack"
	boxPushAdvert  = "info/refs?service=git-receive-pack"
	boxUploadPack  = "git-upload-pack"
	boxReceivePack = "git-receive-pack"
)

// boxCase is a solo fleet of o with RepoName declared and one push of main by
// ceo, and that push's commit.
func boxCase(t *testing.T, o codetest.SoloOptions) (*codetest.Case, string) {
	t.Helper()
	c := newCase(t, o, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	return c, head
}

// boxDo is a hand-built request of repo's box route on node i: tail is what
// follows the repository (one of the box* requests), user and pass git's
// Basic credential — none where user is "" — protocol the Git-Protocol (""
// for none) and body the request's.
func boxDo(t *testing.T, c *codetest.Case, i int, repo, tail, user, pass, protocol, body string) (*http.Response,
	string) {
	t.Helper()
	a := boxSend(boxRequest(t, c, i, repo, tail, user, pass, protocol, body), c.Fleet.Nodes()[i].Client())
	if a.err != nil {
		t.Fatalf("%s: %v", tail, a.err)
	}
	return &http.Response{StatusCode: a.status, Header: a.header}, a.body
}

// boxRequest is boxDo's request, built on the test's goroutine.
func boxRequest(t *testing.T, c *codetest.Case, i int, repo, tail, user, pass, protocol, body string) *http.Request {
	t.Helper()
	method := http.MethodPost
	if strings.HasPrefix(tail, "info/refs") {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(c.Ctx, method, codetest.BoxURL(c.Fleet.Nodes()[i], repo)+"/"+tail,
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-"+tail+"-request")
	}
	if protocol != "" {
		req.Header.Set("Git-Protocol", protocol)
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	return req
}

// boxSend sends req through client and reads its whole answer: safe on a
// goroutine of its own, since it fails no test itself.
func boxSend(req *http.Request, client *http.Client) reply {
	resp, err := client.Do(req)
	if err != nil {
		return reply{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	return reply{status: resp.StatusCode, header: resp.Header, body: string(raw), err: err}
}

// boxLines is every line l holds of a box's admission — challenged, refused
// or unavailable — that carries every one of attrs.
func boxLines(l *logs, attrs ...string) []string {
	var out []string
	for _, msg := range []string{"code_box_challenged", "code_box_refused", "code_box_unavailable"} {
		for _, line := range l.lines(msg) {
			if !slices.ContainsFunc(attrs, func(a string) bool { return !strings.Contains(line, a) }) {
				out = append(out, line)
			}
		}
	}
	return out
}

// boxRow is the newest push row of RepoName that moved ref.
func boxRow(t *testing.T, c *codetest.Case, ref string) code.PushRow {
	t.Helper()
	var newest code.PushRow
	for _, row := range pushRows(t, c) {
		moved := slices.ContainsFunc(row.Commands, func(cmd code.Command) bool { return cmd.Ref == ref })
		if moved && (newest.PushID == "" || newest.Position.Packed() < row.Position.Packed()) {
			newest = row
		}
	}
	if newest.PushID == "" {
		t.Fatalf("no push row moved %s", ref)
	}
	return newest
}

// boxRing is a keyring of the one key id → material.
func boxRing(active string, keys ...runtoken.KeyMaterial) runtoken.Material {
	return runtoken.Material{ActiveID: active, Keys: keys}
}

// boxTokens is a box token signer over m.
func boxTokens(t *testing.T, m runtoken.Material) *host.BoxTokens {
	t.Helper()
	tokens, err := host.NewBoxTokens(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tokens
}

// aRunRef is a run's job as a launch names one.
func aRunRef() host.BoxRunRef {
	return host.BoxRunRef{TurnID: uuid.Must(uuid.NewV7()).String(), LaunchID: uuid.NewString()}
}

// A BOX TOKEN NAMES ITS LAUNCH: it verifies back to the run's turn and the
// launch it was minted for, a token of another per-run domain over the same
// keyring is a signature under another key, and a turn id that would break
// the subject's one cut is a caller's bug, refused by name.
func TestABoxTokenNamesItsLaunch(t *testing.T) {
	t.Parallel()
	k := runtoken.KeyMaterial{ID: "k1", Material: "a fleet's key"}
	tokens := boxTokens(t, boxRing("k1", k))
	ref := aRunRef()
	if got, ok := tokens.Verify(tokens.Mint(ref)); !ok || got != ref {
		t.Fatalf("a minted box token verifies to %+v %v, want %+v", got, ok, ref)
	}
	bridge, err := runtoken.New(runtoken.Options{Domain: "crewlet.mcp.v1", Material: boxRing("k1", k)})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := tokens.Verify(bridge.Mint(ref.LaunchID+"~"+ref.TurnID, time.Hour)); ok {
		t.Errorf("a bridge token over the same keyring was admitted as a box's: %+v", got)
	}
	defer func() {
		if recover() == nil {
			t.Error("a turn id holding the subject's separator was minted")
		}
	}()
	tokens.Mint(host.BoxRunRef{TurnID: "a~b", LaunchID: uuid.NewString()})
}

// A BOX TOKEN IS SIGNED FROM THE FLEET'S KEYRING, never a key of the process's
// own: two nodes over one keyring accept each other's tokens, two over two
// keyrings neither, a node whose ring holds the first's key beside its own
// active one — a rotation in flight — accepts the first's, and an empty
// keyring builds no signer at all.
//
// Mutation: NewBoxTokens signing with a key of its own (runtoken.OneKey over
// fresh material) — the two nodes refuse each other's tokens — red.
func TestTheBoxTokenIsSignedFromTheKeyring(t *testing.T) {
	t.Parallel()
	k1 := runtoken.KeyMaterial{ID: "k1", Material: "the old key"}
	k2 := runtoken.KeyMaterial{ID: "k2", Material: "the new key"}
	ref := aRunRef()
	a, b := boxTokens(t, boxRing("k1", k1)), boxTokens(t, boxRing("k1", k1))
	if _, ok := b.Verify(a.Mint(ref)); !ok {
		t.Error("two nodes over one keyring refuse each other's box tokens")
	}
	other := boxTokens(t, boxRing("k2", k2))
	if _, ok := other.Verify(a.Mint(ref)); ok {
		t.Error("a node over another keyring admitted a box token")
	}
	rotating := boxTokens(t, boxRing("k2", k1, k2))
	if _, ok := rotating.Verify(a.Mint(ref)); !ok {
		t.Error("a node mid-rotation refuses a token signed under the key it still holds")
	}
	if _, err := host.NewBoxTokens(runtoken.Material{}, nil); !errors.Is(err, runtoken.ErrNoKeyring) {
		t.Errorf("an empty keyring built box tokens (%v), want runtoken.ErrNoKeyring", err)
	}
}

// A BOX TOKEN IS REDACTED by its shape (M3-decisions M3-O53): printed bare or
// as the box's environment holds it, nothing of it survives redact.Secrets.
func TestABoxTokenIsRedacted(t *testing.T) {
	t.Parallel()
	token := boxTokens(t, boxRing("k1", runtoken.KeyMaterial{ID: "k1", Material: "a fleet's key"})).Mint(aRunRef())
	marker := redact.Marker + "run-token]"
	for _, printed := range []string{token, "CREWLET_GIT_TOKEN=" + token} {
		if got, want := redact.Secrets(printed), strings.Replace(printed, token, marker, 1); got != want {
			t.Errorf("a box token printed as %q was redacted to %q, want %q", printed, got, want)
		}
	}
}

// A RUN TOKEN OF AN ENDED RUN IS REFUSED: the run's ROW admits a box, read on
// every request — the token is the credential, never the authority. Each step
// of the admission's first half answers in its own words, with the 401s a
// challenge in the box's own (never the person's, which names an API token),
// and the store unreachable 503 with no challenge: the credential was good.
//
// Mutations: admit on the token alone (skip Runs.Run) — the ended,
// superseded and unknown rows answer 200 — red; the box's challenge through
// auth.GitChallenge — the person's API-token sentence — red.
func TestARunTokenOfAnEndedRunIsRefused(t *testing.T) {
	t.Parallel()
	var lines logs
	c, _ := boxCase(t, codetest.SoloOptions{Logger: lines.logger()})
	runs := c.Fleet.Runs()
	live, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
	ended, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
	runs.End(ended)
	gone, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
	runs.Remove(gone)
	first, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
	firstToken := runs.Token(first)
	runs.Relaunch(first)

	for _, row := range []struct {
		name       string
		user, pass string
		fail       error
		status     int
		says       []string
		reason     string
	}{
		{name: "no credential", status: http.StatusUnauthorized,
			says: []string{"authenticates as `run` with its run token", "$CREWLET_GIT_TOKEN"}, reason: "none"},
		{name: "a person's user name", user: "crewlet", pass: runs.Token(live), status: http.StatusUnauthorized,
			says: []string{"a coding box authenticates as `run` with its run token"}, reason: "user"},
		{name: "a forged token", user: host.BoxUser, pass: "crewlet_rt1.x.y.1.z", status: http.StatusUnauthorized,
			says: []string{"this run token is not valid here"}, reason: "token"},
		{name: "the run store unreachable", user: host.BoxUser, pass: runs.Token(live),
			fail: errors.New("the coordination store is gone"), status: http.StatusServiceUnavailable,
			says: []string{"the run store could not be read here", "retry in"}, reason: "run_unread"},
		{name: "an unknown run", user: host.BoxUser, pass: runs.Token(gone), status: http.StatusForbidden,
			says: []string{"is unknown here"}, reason: "run_unknown"},
		{name: "a superseded launch", user: host.BoxUser, pass: firstToken, status: http.StatusForbidden,
			says: []string{"has launched again since"}, reason: "run_superseded"},
		{name: "an ended run", user: host.BoxUser, pass: runs.Token(ended), status: http.StatusForbidden,
			says: []string{"has ended (resumed)"}, reason: "run_ended"},
		{name: "a live run", user: host.BoxUser, pass: runs.Token(live), status: http.StatusOK},
	} {
		t.Run(row.name, func(t *testing.T) {
			runs.Fail(row.fail)
			defer runs.Fail(nil)
			resp, answer := boxDo(t, c, 0, codetest.RepoName, boxFetchAdvert, row.user, row.pass, "", "")
			if resp.StatusCode != row.status {
				t.Fatalf("answered %d %q, want %d", resp.StatusCode, short(answer), row.status)
			}
			for _, s := range row.says {
				if !strings.Contains(answer, s) {
					t.Errorf("answered %q, which does not say %q", answer, s)
				}
			}
			challenged := resp.Header.Get("WWW-Authenticate") != ""
			switch {
			case challenged != (row.status == http.StatusUnauthorized):
				t.Errorf("a %d carries a challenge: %v", row.status, challenged)
			case row.status == http.StatusUnauthorized && strings.Contains(answer, "API token"):
				t.Errorf("a box was challenged in a person's words: %q", answer)
			case (row.status == http.StatusServiceUnavailable) != (resp.Header.Get("Retry-After") != ""):
				t.Errorf("a %d carries Retry-After %q", row.status, resp.Header.Get("Retry-After"))
			}
			if row.reason != "" && len(boxLines(&lines, `"reason":"`+row.reason+`"`)) == 0 {
				t.Errorf("no line logged reason %s", row.reason)
			}
		})
	}
	if leaked := boxLines(&lines, "crewlet_rt1."); len(leaked) != 0 {
		t.Errorf("a run token was logged: %q", leaked)
	}
}

// AN UNSIGNED BOX REQUEST IS LOGGED AT DEBUG: /box/git/ is a public route, so
// a refusal reached with no credential this fleet signed — none, a wrong user
// name, a forged token — is a line anybody who can reach the listener prints
// at will, and stays below INFO; a refusal of a token the fleet signed is
// INFO.
//
// Mutation: log a forged token at INFO — red.
func TestAnUnsignedBoxRequestIsLoggedAtDebug(t *testing.T) {
	t.Parallel()
	var lines logs
	c, _ := boxCase(t, codetest.SoloOptions{Logger: lines.logger()})
	ended, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
	c.Fleet.Runs().End(ended)
	for _, cred := range [][2]string{{"", ""}, {"crewlet", "x"}, {host.BoxUser, "crewlet_rt1.forged"}} {
		boxDo(t, c, 0, codetest.RepoName, boxFetchAdvert, cred[0], cred[1], "", "")
	}
	for _, reason := range []string{"none", "user", "token"} {
		if n := lines.count("code_box_challenged", `"level":"DEBUG"`, `"reason":"`+reason+`"`); n != 1 {
			t.Errorf("code_box_challenged reason %s logged %d times at DEBUG, want once", reason, n)
		}
	}
	if info := lines.at("INFO"); slices.ContainsFunc(info, func(l string) bool {
		return strings.Contains(l, "code_box")
	}) {
		t.Errorf("an unsigned request logged at INFO: %q", info)
	}
	boxDo(t, c, 0, codetest.RepoName, boxFetchAdvert, host.BoxUser, c.Fleet.Runs().Token(ended), "", "")
	if n := lines.count("code_box_refused", `"level":"INFO"`, `"reason":"run_ended"`); n != 1 {
		t.Errorf("an ended run's signed token was refused at INFO %d times, want once: %q", n,
			lines.lines("code_box_refused"))
	}
}

// eveRoster is the fixture chart with a seat of another unit, eve
// (full_access, OPS), which reads a `visibility: unit` repository of ENG
// nothing.
type eveRoster struct{ code.Roster }

func (r eveRoster) Seat(handle string) (code.RosterSeat, bool) {
	if handle == "eve" {
		return code.RosterSeat{Handle: "eve", Tier: code.TierFullAccess, Unit: "OPS"}, true
	}
	return r.Roster.Seat(handle)
}

// A BOX IS GIVEN ONLY ITS GRANTS: what its run was given, by name, held to
// what its seat may (code.Grant.Within) — on a node that serves the company's
// configuration, which alone judges a grant.
//
//   - a node confirmed behind the configuration answers every request past
//     the repository 503 behind_config, judging no grant;
//   - a repository the run was not given, 403 not_granted;
//   - a read grant's push, 403 read_only;
//   - a seat that reads nothing of a `visibility: unit` repository, 403
//     not_permitted, whatever its run was given;
//   - a lead's run given write deletes no branch and forces none, which its
//     seat may and its run may not, each refused not_permitted per ref.
//
// Mutations: drop the behind_config check — the box admitted on a stale
// chart — red; Within answering the seat's grant — the lead's deletion lands
// — red.
func TestABoxIsGivenOnlyItsGrants(t *testing.T) {
	t.Parallel()
	var current atomic.Bool
	current.Store(true)
	c := newCase(t, codetest.SoloOptions{Current: current.Load,
		Roster: func() code.Roster { return eveRoster{codetest.FixtureRoster()} }}, codetest.Settings())
	unit := codetest.Settings()
	unit.Visibility = code.VisibilityUnit
	c.Fleet.Declare(t, code.Declared{Name: "secret", Settings: unit})
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main", "main:refs/heads/topic")
	runs := c.Fleet.Runs()
	rows := c.Rows()

	t.Run("a node behind the configuration judges no grant", func(t *testing.T) {
		ref, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
		current.Store(false)
		defer current.Store(true)
		for _, tail := range []string{boxFetchAdvert, boxPushAdvert} {
			resp, answer := boxDo(t, c, 0, codetest.RepoName, tail, host.BoxUser, runs.Token(ref), "", "")
			if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(answer,
				"behind the company's configuration") {
				t.Errorf("%s on a node behind the configuration answered %d %q", tail, resp.StatusCode, answer)
			}
		}
	})
	t.Run("a repository the run was not given", func(t *testing.T) {
		ref, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
		resp, answer := boxDo(t, c, 0, "secret", boxFetchAdvert, host.BoxUser, runs.Token(ref), "", "")
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(answer, `this run was not given "secret"`) {
			t.Errorf("an ungranted repository answered %d %q", resp.StatusCode, answer)
		}
	})
	t.Run("a read grant's push", func(t *testing.T) {
		ref, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessRead)
		for _, tail := range []string{boxPushAdvert, boxReceivePack} {
			resp, answer := boxDo(t, c, 0, codetest.RepoName, tail, host.BoxUser, runs.Token(ref), "", "")
			if resp.StatusCode != http.StatusForbidden || !strings.Contains(answer, "to read: a push needs access: write") {
				t.Errorf("a read grant's %s answered %d %q", tail, resp.StatusCode, answer)
			}
		}
	})
	t.Run("a seat outside a unit repository's unit", func(t *testing.T) {
		secret, err := c.Fleet.Rows(c.Ctx, "secret", codetest.Linearizable)
		if err != nil {
			t.Fatal(err)
		}
		ref := runs.Launch("eve", secret.Position, host.BoxGrant{RepoID: secret.Repo.ID, Name: "secret",
			Access: code.AccessWrite})
		resp, answer := boxDo(t, c, 0, "secret", boxFetchAdvert, host.BoxUser, runs.Token(ref), "", "")
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(answer, `seat "eve" holds no access`) {
			t.Errorf("eve's box read a unit repository of ENG: %d %q", resp.StatusCode, answer)
		}
	})
	t.Run("a lead's run given write", func(t *testing.T) {
		ref := runs.Launch(codetest.FixtureLead, rows.Position, host.BoxGrant{RepoID: rows.Repo.ID,
			Name: codetest.RepoName, Access: code.AccessWrite})
		box := codetest.NewBox(t, c.Fleet, ref)
		clone := box.Clone(c.BoxURL(0))
		res := box.Push(c.Ctx, clone, c.BoxURL(0), ":refs/heads/topic")
		if why, refused := res.Rejected("refs/heads/topic"); !refused || !strings.Contains(why, "not_permitted") {
			t.Errorf("a lead's box given write deleted a branch: %s", res)
		}
		box.Git().Run(t, clone, "checkout", "--quiet", "-B", "topic", "origin/topic")
		box.Git().Run(t, clone, "commit", "--quiet", "--amend", "-m", "rewritten")
		res = box.PushArgs(c.Ctx, clone, []string{"--force"}, c.BoxURL(0), "topic")
		if why, refused := res.Rejected("refs/heads/topic"); !refused || !strings.Contains(why, "not_permitted") {
			t.Errorf("a lead's box given write force-pushed a branch: %s", res)
		}
		if got := c.Rows().Refs["refs/heads/topic"]; got != rows.Refs["refs/heads/topic"] {
			t.Errorf("topic moved to %q under a run given write, from %q", got, rows.Refs["refs/heads/topic"])
		}
	})
}

// AN UNGRANTED NAME COSTS THE LOG NOTHING: the run's grant is matched by the
// NAME it was given before the code log is read, so a box probing names it
// was never given — `git ls-remote` over guesses — appends no barrier and
// reads no row, and the name it was given resolves once, at its run's
// session.
//
// Mutation: resolve the repository before matching the grant — the unknown
// name is resolved, through the barrier — red.
func TestAnUngrantedNameCostsTheLogNothing(t *testing.T) {
	t.Parallel()
	rec := &recording{}
	c, _ := boxCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: rec.wrap}})
	c.Fleet.Declare(t, code.Declared{Name: "api", Settings: codetest.Settings()})
	ref, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
	token := c.Fleet.Runs().Token(ref)
	before := len(rec.seen("resolve"))
	for _, name := range []string{"api", "nothing-here"} {
		resp, answer := boxDo(t, c, 0, name, boxFetchAdvert, host.BoxUser, token, "", "")
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(answer, "this run was not given") {
			t.Errorf("%s answered %d %q, want 403 not_granted", name, resp.StatusCode, answer)
		}
	}
	if got := rec.seen("resolve")[before:]; len(got) != 0 {
		t.Fatalf("names the run was not given were resolved: %+v", got)
	}
	resp, answer := boxDo(t, c, 0, codetest.RepoName, boxFetchAdvert, host.BoxUser, token, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the granted name answered %d %q", resp.StatusCode, short(answer))
	}
	got := rec.seen("resolve")[before:]
	if len(got) != 1 || got[0].level != statelog.ReadSession {
		t.Errorf("the granted name was resolved %+v, want once at session", got)
	}
}

// A BOX KEEPS THE NAME IT WAS GIVEN: its run's grant is the repository's id
// and its remote the name its launch gave it, which nothing in the box
// changes, so
//
//   - a name a rename left redirecting serves the box the repository under
//     its new name: its advertisement answered, never 302 Found, and its push
//     landed, never refused as a person's is to tell them the new name;
//   - a name that now names another repository is refused not_granted,
//     decided through the barrier: a session read naming another repository
//     is read again LINEARIZABLE, and a box whose session read lagged is
//     admitted where the barrier finds the repository it was given.
//
// Mutations: refuse a box's push at a redirecting name, as admit refuses a
// person's — the push at the old name is refused — red; decide another
// repository on the session read — the lagging box is refused — red.
func TestABoxKeepsTheNameItWasGiven(t *testing.T) {
	t.Parallel()
	until := time.Now().Add(code.RedirectWindow).UTC().Truncate(time.Second)
	rec := &recording{resolve: func(ctx context.Context, inner host.Estate, q code.ResolveQuery) (code.RepoRef, error) {
		switch {
		case q.Name == "old-name":
			// THE OLD NAME, AS THE ROWS ANSWER IT: the repository under its
			// current name, and the redirect's end.
			q.Name = codetest.RepoName
			ref, err := inner.Resolve(ctx, q)
			ref.Redirect, ref.RedirectUntil = codetest.RepoName, until
			return ref, err
		case q.Name == "taken", q.Name == "lagging" && q.Fresh.Level != statelog.ReadLinearizable:
			// ANOTHER REPOSITORY under the name: for good, or on a session
			// read the barrier corrects.
			q.Name = "api"
		case q.Name == "lagging":
			q.Name = codetest.RepoName
		}
		return inner.Resolve(ctx, q)
	}}
	c, head := boxCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: rec.wrap}})
	c.Fleet.Declare(t, code.Declared{Name: "api", Settings: codetest.Settings()})
	runs := c.Fleet.Runs()
	rows := c.Rows()
	given := func(name string) host.BoxRunRef {
		return runs.Launch(codetest.FixtureAlice, rows.Position, host.BoxGrant{RepoID: rows.Repo.ID, Name: name,
			Access: code.AccessWrite})
	}

	t.Run("a name a rename left redirecting", func(t *testing.T) {
		ref := given("old-name")
		resp, answer := boxDo(t, c, 0, "old-name", boxFetchAdvert, host.BoxUser, runs.Token(ref), "", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("the old name's advertisement answered %d %q, want 200", resp.StatusCode, short(answer))
		}
		box := codetest.NewBox(t, c.Fleet, ref)
		old := codetest.BoxURL(c.Fleet.Nodes()[0], "old-name")
		clone := box.Clone(old)
		if got := box.Head(clone); got != head {
			t.Fatalf("the box's clone of the old name checked out %s, want %s", got, head)
		}
		work := box.Commit(clone, map[string]string{"a.txt": "a\n"}, "a")
		branch := code.SeatNamespace(codetest.FixtureAlice) + "work"
		if res := box.Push(c.Ctx, clone, old, "HEAD:"+branch); res.Err != nil {
			t.Fatalf("the box's push to the name its launch gave it, since renamed: %s", res)
		}
		if got := c.Rows().Refs[branch]; got != work {
			t.Errorf("after the box's push to the old name, %s is %q, want %s", branch, got, work)
		}
	})
	t.Run("a name another repository now holds", func(t *testing.T) {
		ref := given("taken")
		before := len(rec.seen("resolve"))
		resp, answer := boxDo(t, c, 0, "taken", boxFetchAdvert, host.BoxUser, runs.Token(ref), "", "")
		if resp.StatusCode != http.StatusForbidden ||
			!strings.Contains(answer, `"taken" is no longer the repository this run was given`) {
			t.Errorf("a name another repository holds answered %d %q, want 403 not_granted", resp.StatusCode,
				short(answer))
		}
		got := rec.seen("resolve")[before:]
		if len(got) != 2 || got[0].level != statelog.ReadSession || got[1].level != statelog.ReadLinearizable {
			t.Errorf("the name was resolved %+v, want at the run's session and then through the barrier", got)
		}
	})
	t.Run("a session read behind the barrier", func(t *testing.T) {
		ref := given("lagging")
		resp, answer := boxDo(t, c, 0, "lagging", boxFetchAdvert, host.BoxUser, runs.Token(ref), "", "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("a box whose session read named another repository answered %d %q, want the barrier's "+
				"answer, 200", resp.StatusCode, short(answer))
		}
	})
}

// A STOPPED SEAT'S BOX PUSHES NOTHING: a person's pause with stop_running is
// the emergency stop, and a detached coding run is not fenced by it, so its
// box's push advertisement and receive-pack are refused, naming who stopped
// the seat — while its fetches are served, a pause that stops no running
// work pushes, and a node that has not yet read the pauses answers a push
// 503, never "not stopped".
//
// Mutation: skip the stop step — the stopped push lands — red.
func TestAStoppedSeatsBoxPushesNothing(t *testing.T) {
	t.Parallel()
	c, _ := boxCase(t, codetest.SoloOptions{})
	runs := c.Fleet.Runs()
	ref, box := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
	token := runs.Token(ref)
	clone := box.Clone(c.BoxURL(0))
	box.Commit(clone, map[string]string{"a.txt": "a\n"}, "a")
	branch := code.SeatNamespace(codetest.FixtureAlice) + "work"

	runs.Stop(codetest.FixtureAlice, "ceo")
	for _, tail := range []string{boxPushAdvert, boxReceivePack} {
		resp, answer := boxDo(t, c, 0, codetest.RepoName, tail, host.BoxUser, token, "", "")
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(answer, "was paused by ceo") {
			t.Errorf("a stopped seat's %s answered %d %q", tail, resp.StatusCode, answer)
		}
	}
	if res := box.Push(c.Ctx, clone, c.BoxURL(0), "HEAD:"+branch); res.Err == nil {
		t.Errorf("a stopped seat's box pushed: %s", res)
	}
	for _, tail := range []string{boxFetchAdvert, boxUploadPack} {
		body := ""
		if tail == boxUploadPack {
			body = lsRefs()
		}
		if resp, answer := boxDo(t, c, 0, codetest.RepoName, tail, host.BoxUser, token, v2, body); resp.StatusCode !=
			http.StatusOK {
			t.Errorf("a stopped seat's box fetch %s answered %d %q", tail, resp.StatusCode, short(answer))
		}
	}

	runs.Resume(codetest.FixtureAlice)
	runs.PausesUnread(true)
	resp, answer := boxDo(t, c, 0, codetest.RepoName, boxPushAdvert, host.BoxUser, token, "", "")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(answer, "not yet read which seats are paused") {
		t.Errorf("a push on a node that has not read the pauses answered %d %q", resp.StatusCode, answer)
	}
	if resp, _ := boxDo(t, c, 0, codetest.RepoName, boxFetchAdvert, host.BoxUser, token, "", ""); resp.StatusCode !=
		http.StatusOK {
		t.Errorf("a fetch on a node that has not read the pauses answered %d", resp.StatusCode)
	}
	runs.PausesUnread(false)
	if res := box.Push(c.Ctx, clone, c.BoxURL(0), "HEAD:"+branch); res.Err != nil {
		t.Errorf("a seat paused without stopping its running work, then resumed, pushed nothing: %s", res)
	}
}

// A BOX READS AT ITS RUN'S FLOOR: its v0 advertisement, its `ls-refs`, its
// push advertisement and its receive-pack each sync at its run's SESSION, so
// none of them appends a barrier to the code log, while a person's reads of
// refs are linearizable.
//
// Mutation: admission.refs answering linearizable for a box — the barrier
// count moves — red.
func TestABoxReadsAtItsRunsFloor(t *testing.T) {
	t.Parallel()
	rec := &recording{}
	c, _ := boxCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: rec.wrap}})
	_, box := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
	barriers := func() int {
		n := rec.linearizable()
		for _, r := range rec.seen("resolve") {
			if r.level == statelog.ReadLinearizable {
				n++
			}
		}
		return n
	}
	before := barriers()
	_ = box.LsRemote(c.BoxURL(0), 0)
	clone := box.Clone(c.BoxURL(0))
	box.Commit(clone, map[string]string{"a.txt": "a\n"}, "a")
	if res := box.Push(c.Ctx, clone, c.BoxURL(0), "HEAD:"+code.SeatNamespace(codetest.FixtureAlice)+"work"); res.Err !=
		nil {
		t.Fatalf("the box's push: %s", res)
	}
	if got := barriers() - before; got != 0 {
		t.Errorf("a box's reads and push appended %d barriers to the code log, want none", got)
	}
	_ = codetest.NewPerson(t, c.Fleet, "ceo").LsRemote(c.URL(0), 0)
	if barriers() == before {
		t.Error("the premise: a person's v0 advertisement read no barrier")
	}
}

// A BOX IS SHOWN NOTHING ITS FLOOR DOES NOT HOLD: an `ls-refs` served past the
// run's floor raises the floor to the position it is served at BEFORE the
// answer's first byte — while the raise is held, no answer reaches the box —
// and a raise that fails refuses the answer 503, floor_unraised, the box to
// retry.
//
// Mutation: raise after writing the answer — the answer arrives while the
// raise is held — red.
func TestABoxIsShownNothingItsFloorDoesNotHold(t *testing.T) {
	t.Parallel()
	var lines logs
	c, _ := boxCase(t, codetest.SoloOptions{Logger: lines.logger()})
	runs := c.Fleet.Runs()
	ref, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessRead)
	floor := runs.Floor(ref)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Clone(c.URL(0))
	p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	c.MustPush(p, work, "main")
	_ = p.LsRemote(c.URL(0), 0)

	raising, release := make(chan codetest.Raise, 1), make(chan struct{})
	var once sync.Once
	let := func() { once.Do(func() { close(release) }) }
	// RELEASED HOWEVER THE CASE ENDS, before the fleet shuts down, so a
	// request held in its raise never outlives the case's servers.
	t.Cleanup(let)
	runs.OnRaise(func(r codetest.Raise) error {
		select {
		case raising <- r:
		default:
		}
		<-release
		return nil
	})
	answered := make(chan reply, 1)
	req := boxRequest(t, c, 0, codetest.RepoName, boxUploadPack, host.BoxUser, runs.Token(ref), v2, lsRefs())
	client := c.Fleet.Nodes()[0].Client()
	go func() { answered <- boxSend(req, client) }()
	var r codetest.Raise
	select {
	case r = <-raising:
	case a := <-answered:
		t.Fatalf("the ls-refs answered %d %v and raised no floor", a.status, a.err)
	case <-time.After(10 * time.Second):
		t.Fatal("the ls-refs raised no floor in ten seconds")
	}
	if past, err := floor.Before(r.Pos); r.Ref != ref || err != nil || !past {
		t.Errorf("the ls-refs raised %+v to %s, want %+v past its floor %s (%v)", r.Ref, r.Pos, ref, floor, err)
	}
	select {
	case a := <-answered:
		t.Fatalf("the ls-refs answered %d while its floor's raise was held: shown before its floor held it",
			a.status)
	case <-time.After(300 * time.Millisecond):
	}
	let()
	if a := <-answered; a.status != http.StatusOK {
		t.Errorf("the ls-refs answered %d %v once its floor was raised", a.status, a.err)
	}

	again, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessRead)
	runs.OnRaise(func(codetest.Raise) error { return errors.New("the coordination store is gone") })
	p.Commit(work, map[string]string{"README.md": "three\n"}, "three")
	c.MustPush(p, work, "main")
	_ = p.LsRemote(c.URL(0), 0)
	resp, answer := boxDo(t, c, 0, codetest.RepoName, boxUploadPack, host.BoxUser, runs.Token(again), v2, lsRefs())
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" ||
		!strings.Contains(answer, "the run store could not be written here") {
		t.Errorf("an ls-refs whose raise failed answered %d %q (Retry-After %q)", resp.StatusCode, answer,
			resp.Header.Get("Retry-After"))
	}
	if n := lines.count("code_box_unavailable", `"reason":"floor_unraised"`); n != 1 {
		t.Errorf("logged floor_unraised %d times, want once", n)
	}
}

// A BOX'S PUSH RAISES ITS FLOOR AND IS NOT AUDITED: its applied push raises
// the run's floor to the push's row before `ok` is written, and publishes no
// operator_acted — no person acted, and the push row, which names the run, is
// its record. A raise that fails then answers `ok` all the same, the push
// being durable, and is logged.
//
// Mutations: no raise at the applied push — no raise reaches the push's
// position — red; publish a box's push — an operator record appears — red.
func TestABoxPushRaisesItsFloorAndIsNotAudited(t *testing.T) {
	t.Parallel()
	var lines logs
	c, _ := boxCase(t, codetest.SoloOptions{Logger: lines.logger()})
	runs := c.Fleet.Runs()
	_, box := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
	audited := len(c.Fleet.Audited())
	clone := box.Clone(c.BoxURL(0))
	box.Commit(clone, map[string]string{"a.txt": "a\n"}, "a")
	branch := code.SeatNamespace(codetest.FixtureAlice) + "work"
	if res := box.Push(c.Ctx, clone, c.BoxURL(0), "HEAD:"+branch); res.Err != nil {
		t.Fatalf("the box's push: %s", res)
	}
	at := boxRow(t, c, branch).Position
	if !slices.ContainsFunc(runs.Raises(), func(r codetest.Raise) bool { return r.Pos == at }) {
		t.Errorf("the applied push at %s raised no floor to it: %+v", at, runs.Raises())
	}
	if got := c.Fleet.Audited()[audited:]; len(got) != 0 {
		t.Errorf("a box's push was audited as an operator's act: %+v", got)
	}

	_, box2 := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
	clone2 := box2.Clone(c.BoxURL(0))
	box2.Commit(clone2, map[string]string{"b.txt": "b\n"}, "b")
	runs.OnRaise(func(codetest.Raise) error { return errors.New("the coordination store is gone") })
	if res := box2.Push(c.Ctx, clone2, c.BoxURL(0), "HEAD:"+branch+"2"); res.Err != nil {
		t.Errorf("a push whose floor raise failed was not answered ok: %s", res)
	}
	if n := lines.count("code_box_floor_unraised"); n != 1 {
		t.Errorf("logged code_box_floor_unraised %d times, want once", n)
	}
}

// A BOX'S PUSH NAMES ITS RUN: its row's principal is the run's seat as an
// agent, stamped with the run — its turn, its launch, its work item, its
// model and its coding agent — received over git.
//
// Mutation: principal answering the person's shape for a box — the row names
// no run — red.
func TestABoxPushNamesItsRun(t *testing.T) {
	t.Parallel()
	c, _ := boxCase(t, codetest.SoloOptions{})
	ref, box := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
	clone := box.Clone(c.BoxURL(0))
	box.Commit(clone, map[string]string{"a.txt": "a\n"}, "a")
	branch := code.SeatNamespace(codetest.FixtureAlice) + "work"
	if res := box.Push(c.Ctx, clone, c.BoxURL(0), "HEAD:"+branch); res.Err != nil {
		t.Fatalf("the box's push: %s", res)
	}
	row := boxRow(t, c, branch)
	want := code.RunProvenance{TurnID: ref.TurnID, RunID: ref.LaunchID, WorkBackend: "native", WorkKey: "ENG-1",
		Model: "fixture-model", CodingAgent: "claude-code"}
	if row.Run != want || row.Via != code.ViaGit || row.ActorSeat != codetest.FixtureAlice ||
		row.ActorKind != string(code.PrincipalAgent) {
		t.Errorf("the box's push row is %+v (actor %q %q seat %q), want run %+v via git as agent %s", row.Run,
			row.ActorKind, row.Actor, row.ActorSeat, want, codetest.FixtureAlice)
	}
}

// A BOX'S PUSH ON AN ITEM WITH A LONG KEY LANDS: a work key a vendor wrote
// past the bound — a GitLab merge request's, nesting its project's whole path
// — is fitted where the host stamps it (code.FitProvenance), so the run's
// every push is not refused as an invalid proposal for a label it never
// chose; the two ids are the engine's own and stamped whole.
//
// Mutation: stamp the key raw — the push is refused as an invalid proposal —
// red.
func TestABoxPushOnAnItemWithALongKeyLands(t *testing.T) {
	t.Parallel()
	c, _ := boxCase(t, codetest.SoloOptions{})
	ref, box := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
	long := strings.Repeat("group/", 48) + "project!12"
	c.Fleet.Runs().SetProvenance(ref, code.RunProvenance{WorkBackend: "gitlab", WorkKey: long,
		Model: "fixture-model", CodingAgent: "claude-code"})
	clone := box.Clone(c.BoxURL(0))
	box.Commit(clone, map[string]string{"a.txt": "a\n"}, "a")
	branch := code.SeatNamespace(codetest.FixtureAlice) + "work"
	if res := box.Push(c.Ctx, clone, c.BoxURL(0), "HEAD:"+branch); res.Err != nil {
		t.Fatalf("a push of a run on an item with a %d-byte key: %s", len(long), res)
	}
	row := boxRow(t, c, branch)
	if row.Run.WorkKey != code.FitProvenance(long) || row.Run.RunID != ref.LaunchID ||
		row.Run.TurnID != ref.TurnID {
		t.Errorf("the run was stamped %+v, want the key fitted to %q and its ids whole", row.Run,
			code.FitProvenance(long))
	}
}

// A BOX CANNOT PUSH THE ENGINE'S COMMITTER: a coding box's push whose pack
// holds a commit committed as the engine — the fixture signer's
// engine@seats.invalid, written as the engine writes it or in any other case,
// since a reader takes either for the engine — that the repository does not
// already hold is refused forged_committer, naming the commit, on the ref it
// sends. Its pack is read before its upload begins, so nothing reaches the
// object store, nothing is proposed and no ref moves; and its one record — its
// code_push line, since a box's push is no operator_acted — names the refusal
// and the run. The control: a person's push of the same commit lands, as a
// branch pushed back from a clone or an export imported again does.
//
// Mutations: drop the committer's check — the box's push lands, red; compare
// the address byte for byte — the mixed-case row lands, red; upload the
// bundle before the check — the upload is reached, red.
func TestABoxCannotPushTheEnginesCommitter(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, committer string }{
		{"as the engine writes it", codetest.FixtureCommitterEmail},
		{"in another case", "Engine@Seats.INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var lines logs
			e := &recording{}
			c, _ := boxCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: e.wrap},
				Logger: lines.logger()})
			ref, box := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
			clone := box.Clone(c.BoxURL(0))
			forged := box.Git().Env("GIT_COMMITTER_NAME="+codetest.FixtureCommitterName,
				"GIT_COMMITTER_EMAIL="+tc.committer).Commit(t, clone, map[string]string{"a.txt": "a\n"},
				"committed as the engine")
			branch := code.SeatNamespace(codetest.FixtureAlice) + "work"
			stored, proposed := len(objects(t, c)), len(e.seen("propose"))
			uploads := c.Fleet.Faults().Hits(codefault.HostUpload)

			res := box.Push(c.Ctx, clone, c.BoxURL(0), "HEAD:"+branch)
			want := "(forged_committer: " + forged[:7] + " is committed as the code host's engine (" +
				codetest.FixtureCommitterEmail + ")"
			if reason, rejected := res.Rejected(branch); !rejected || !strings.Contains(reason, want) {
				t.Fatalf("a box's push of a commit committed as %s: %s\nwant %s rejected %s…", tc.committer, res,
					branch, want)
			}
			if n := c.Fleet.Faults().Hits(codefault.HostUpload) - uploads; n != 0 {
				t.Errorf("the refused push reached its upload %d times: its refusal followed the store", n)
			}
			if n := len(objects(t, c)) - stored; n != 0 {
				t.Errorf("the refused push stored %d objects", n)
			}
			if n := len(e.seen("propose")) - proposed; n != 0 {
				t.Errorf("the refused push was proposed %d times", n)
			}
			if at, moved := c.Rows().Refs[branch]; moved {
				t.Errorf("%s moved to %s, and its only push was refused", branch, at)
			}
			if n := lines.count("code_push", `"outcome":"refused"`, `"refusal":"forged_committer"`,
				`"launch_id":"`+ref.LaunchID+`"`); n != 1 {
				t.Errorf("the refused push's code_push line, naming forged_committer and its run, was logged %d "+
					"times, want once:\n%s", n, strings.Join(lines.lines("code_push"), "\n"))
			}

			// THE CONTROL: a person's push of the same commit lands.
			person := codetest.NewPerson(t, c.Fleet, "ceo")
			landed := person.Push(c.Ctx, clone, c.URL(0), "HEAD:refs/heads/imported")
			if !landed.OK("refs/heads/imported") {
				t.Errorf("a person's push of the commit the box was refused: %s", landed)
			}
		})
	}
}

// A BOX STORES NO COMMIT AS THE ENGINE BESIDE A REFUSED UPDATE: what a box's
// push is judged by is its PACK, never only the commits its updates still
// standing introduce. A box sends an ordinary commit to its own branch beside
// a commit committed as the engine sent to main, which takes a person's push
// alone: main is refused protected_direct, and its commit is in the pack all
// the same — stored with the branch, the repository would hold it, and a later
// push naming it would bring back a commit "the repository already holds". So
// the whole push is refused forged_committer, storing and proposing nothing;
// and the commit, still nobody's, is refused again when the box sends it on
// its own.
//
// Mutation: judge the commits the standing updates introduce (the listing,
// as first specified) rather than the pack — the branch lands with the
// engine's commit in its bundle, and the second push lands that commit, red.
func TestABoxStoresNoCommitAsTheEngineBesideARefusedUpdate(t *testing.T) {
	t.Parallel()
	e := &recording{}
	c, _ := boxCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: e.wrap}})
	_, box := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
	clone := box.Clone(c.BoxURL(0))
	forged := box.Git().Env("GIT_COMMITTER_NAME="+codetest.FixtureCommitterName,
		"GIT_COMMITTER_EMAIL="+codetest.FixtureCommitterEmail).Commit(t, clone,
		map[string]string{"forged.txt": "f\n"}, "committed as the engine")
	box.Git().Run(t, clone, "checkout", "--quiet", "-B", "work", "origin/main")
	ordinary := box.Git().Commit(t, clone, map[string]string{"work.txt": "w\n"}, "an ordinary commit")
	branch := code.SeatNamespace(codetest.FixtureAlice) + "work"
	stored, proposed := len(objects(t, c)), len(e.seen("propose"))

	res := box.Push(c.Ctx, clone, c.BoxURL(0), ordinary+":"+branch, forged+":refs/heads/main")
	if why, refused := res.Rejected("refs/heads/main"); !refused || !strings.Contains(why, "protected_direct") {
		t.Fatalf("a box's push of main, which takes a person's push alone: %s", res)
	}
	want := "(forged_committer: " + forged[:7] + " is committed as the code host's engine"
	if why, refused := res.Rejected(branch); !refused || !strings.Contains(why, want) {
		t.Fatalf("a box's branch sent beside a refused update whose commit is the engine's: %s\nwant %s "+
			"rejected %s…", res, branch, want)
	}
	if n := len(objects(t, c)) - stored; n != 0 {
		t.Errorf("the refused push stored %d objects", n)
	}
	if n := len(e.seen("propose")) - proposed; n != 0 {
		t.Errorf("the refused push was proposed %d times", n)
	}

	// THE COMMIT IS STILL NOBODY'S: sent on its own, it is refused again.
	again := code.SeatNamespace(codetest.FixtureAlice) + "again"
	res = box.Push(c.Ctx, clone, c.BoxURL(0), forged+":"+again)
	if why, refused := res.Rejected(again); !refused || !strings.Contains(why, want) {
		t.Errorf("the box's push of the engine's commit on its own: %s\nwant %s rejected %s…", res, again, want)
	}
	for _, ref := range []string{branch, again} {
		if at, moved := c.Rows().Refs[ref]; moved {
			t.Errorf("%s moved to %s, and every push of it was refused", ref, at)
		}
	}
}

// A BOX MAY PUSH BACK AN ENGINE COMMIT THE REPOSITORY HOLDS: a seat branch
// whose one commit the engine built for a tool write is deleted — here by the
// seat's own tool, as alike by its expiry or a person's push — and a box of a
// run launched since pushes the branch back from a checkout made before the
// deletion. The commit is committed as the engine and no ref reaches it, so
// the box's git sends it and the push introduces it, as its row says; but
// the repository holds it, the host's own write it accepted once, so the
// push lands.
//
// Mutation: skip the held check, refusing every commit committed as the
// engine — the push is refused forged_committer, red.
func TestABoxMayPushBackAnEngineCommitTheRepositoryHolds(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, codetest.SoloOptions{}, map[string]string{"README.md": "one\n"})
	branch := code.SeatNamespace(codetest.FixtureAlice) + "work"
	written := c.land(files(c.repo, alice(), branch, "", c.head, text("tool.md", "written by a tool\n")))
	_, before := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
	checkout := before.Clone(c.BoxURL(0))
	c.land(code.EngineWrite{Kind: code.WriteDeleteBranch, RepoID: c.repo, Principal: alice(), OpID: opNow(),
		Fresh: linear, Ref: branch, ExpectedHead: written.Commit})
	_, after := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)

	res := after.Push(c.Ctx, checkout, c.BoxURL(0), written.Commit+":"+branch)
	if !res.OK(branch) {
		t.Fatalf("a box pushing back %s at the engine's commit the repository holds: %s", branch, res)
	}
	if got := c.Rows().Refs[branch]; got != written.Commit {
		t.Errorf("%s is at %q, want the engine's commit %s pushed back", branch, got, written.Commit)
	}
	if row := boxRow(t, c.Case, branch); !slices.Contains(row.Introduced, written.Commit) {
		t.Errorf("the push back introduced %v, not the engine's commit %s: the case never reached the "+
			"check it holds", row.Introduced, written.Commit)
	}
}

// A BOX BRINGS BACK A BOUNDED HISTORY OF THE ENGINE'S COMMITS A PUSH: a pack
// holding more commits committed as the engine than one push is asked about
// (host.MaxEngineCommits) is refused forged_committer even where the
// repository holds every one — past the bound the host asks about none — and
// told to push the history in parts; and the parts land, since once a part
// has landed a ref reaches it, and the box's git sends only the rest. Here
// the history is a person's, which a person may push as git carries it, and
// its branch deleted since.
//
// Mutation: answer a push past the bound by its first MaxEngineCommits alone
// — the whole history lands at once, red.
func TestABoxBringsBackABoundedHistoryOfTheEnginesCommitsAPush(t *testing.T) {
	t.Parallel()
	c, head := boxCase(t, codetest.SoloOptions{})
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Clone(c.URL(0))
	n := host.MaxEngineCommits + 1
	gittest.FastImport(t, p.Git(), work, func(w *bufio.Writer) error {
		for i := range n {
			body := fmt.Sprintf("%d\n", i)
			if _, err := fmt.Fprintf(w, "commit refs/heads/old\ncommitter %s <%s> 1767225600 +0000\n"+
				"data %d\n%s\n", codetest.FixtureCommitterName, codetest.FixtureCommitterEmail, len(body)+1,
				body); err != nil {
				return err
			}
			if i == 0 {
				if _, err := fmt.Fprintf(w, "from %s\n", head); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(w, "M 100644 inline n.txt\ndata %d\n%s\n", len(body), body); err != nil {
				return err
			}
		}
		return nil
	})
	tip := strings.TrimSpace(p.Git().Run(t, work, "rev-parse", "refs/heads/old"))
	c.MustPush(p, work, "refs/heads/old")
	c.MustPush(p, work, ":refs/heads/old")
	_, box := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
	branch := code.SeatNamespace(codetest.FixtureAlice) + "work"

	res := box.Push(c.Ctx, work, c.BoxURL(0), tip+":"+branch)
	want := fmt.Sprintf("(forged_committer: this push carries more than %d commits committed as the code "+
		"host's engine (%s)", host.MaxEngineCommits, codetest.FixtureCommitterEmail)
	if reason, rejected := res.Rejected(branch); !rejected || !strings.Contains(reason, want) {
		t.Fatalf("a box bringing back %d of the engine's commits at once: %s\nwant %s rejected %s…", n, res,
			branch, want)
	}
	for _, part := range []string{tip + "~" + fmt.Sprint(n/2), tip} {
		if pushed := box.Push(c.Ctx, work, c.BoxURL(0), part+":"+branch); !pushed.OK(branch) {
			t.Fatalf("a box bringing the history back in parts, %s: %s", part, pushed)
		}
	}
	if got := c.Rows().Refs[branch]; got != tip {
		t.Errorf("%s is at %q, want the history's tip %s", branch, got, tip)
	}
}

// EVERY BOX TRANSFER IS CHARGED TO ITS RUN: a box's advertisements,
// upload-pack and receive-pack each carry the share of class run, keyed by
// its launch — the class the transfer lane holds to half its width.
func TestEveryBoxTransferIsChargedToItsRun(t *testing.T) {
	t.Parallel()
	c, _ := boxCase(t, codetest.SoloOptions{})
	ran := host.RecordGit(c.Fleet.Nodes()[0].Host())
	ref, box := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
	from := len(ran())
	clone := box.Clone(c.BoxURL(0))
	box.Commit(clone, map[string]string{"a.txt": "a\n"}, "a")
	if res := box.Push(c.Ctx, clone, c.BoxURL(0), "HEAD:"+code.SeatNamespace(codetest.FixtureAlice)+"work"); res.Err !=
		nil {
		t.Fatalf("the box's push: %s", res)
	}
	cmds := ran()[from:]
	want := gitproc.Share{Class: gitproc.ShareRun, Principal: ref.LaunchID}
	for _, step := range []struct {
		name  string
		match func(gitproc.Cmd) bool
	}{
		{"its fetch advertisement", advertising("upload-pack")},
		{"its upload-pack", transferring("upload-pack")},
		{"its push advertisement", advertising("receive-pack")},
		{"its receive-pack", transferring("receive-pack")},
	} {
		if !slices.ContainsFunc(cmds, step.match) {
			t.Errorf("the box's %s ran no git: %v", step.name, argsOf(cmds))
		}
	}
	for _, cmd := range cmds {
		if cmd.Class == gitproc.ClassTransfer && cmd.Share != want {
			t.Errorf("the box's git %v is charged to %+v, want %+v", cmd.Args, cmd.Share, want)
		}
	}
}

// THE RUN CAP IS THE SEAT SHARE: the most of the transfer lane coding boxes
// hold (gitproc.ShareClass.Cap) is the half the engine holds seat git work to
// on the read lane (code.ShareOf) — one rule, spelled in both packages since
// neither imports the other — at every lane width.
//
// Mutation: the run class's cap as slots/3 in gitproc — red.
func TestTheRunCapIsTheSeatShare(t *testing.T) {
	t.Parallel()
	for slots := 1; slots <= 256; slots++ {
		if got, want := gitproc.ShareRun.Cap(slots), code.ShareOf(slots); got != want {
			t.Errorf("a lane of %d: boxes hold %d, seat work %d", slots, got, want)
		}
		for _, c := range []gitproc.ShareClass{gitproc.SharePerson, gitproc.ShareMachine} {
			if got := c.Cap(slots); got != slots {
				t.Errorf("a lane of %d: class %s holds %d, want the whole lane", slots, c, got)
			}
		}
	}
}

// A BOX'S FETCH OF A UNIT REPOSITORY IS RECORDED, as a read by its seat: the
// fetch audit's record names the agent by its handle — as the query server
// names a seat's read, and never the credential a box has none of, which the
// usage domain refuses empty (code.Principal.Actor).
//
// Mutations: record only an operator principal — the box's read is missing —
// red; name the reader by its operator id — the record names nobody — red.
func TestABoxFetchOfAUnitRepositoryIsRecorded(t *testing.T) {
	t.Parallel()
	c, f, _ := unitCase(t, codetest.SoloOptions{})
	rows := c.Rows()
	ref := f.Runs().Launch(codetest.FixtureAlice, rows.Position, host.BoxGrant{RepoID: rows.Repo.ID,
		Name: codetest.RepoName, Access: code.AccessRead})
	before := len(f.Fetched())
	codetest.NewBox(t, f, ref).Clone(c.BoxURL(0))
	got := f.Fetched()[before:]
	want := types.CodeFetched{RepoID: rows.Repo.ID, RepoName: codetest.RepoName,
		PrincipalKind: types.CodeReaderAgent, Principal: codetest.FixtureAlice, Seat: codetest.FixtureAlice,
		Via: types.FetchedViaGit, Reads: 1}
	if len(got) != 1 {
		t.Fatalf("a box's clone of a unit repository was recorded %d times: %+v", len(got), got)
	}
	want.Bytes = got[0].Bytes
	if got[0] != want || got[0].Bytes <= 0 {
		t.Errorf("the box's clone is recorded as %+v, want %+v with its bytes", got[0], want)
	}
}

// NOTHING AFTER ADMISSION ASKS WHICH ADMITTER RAN: in the host's own files,
// only the box's admission reads git's Basic credential or names a box path,
// only the admission's accessors read a box's run, and only the person's
// admission reads the guard's resolution (iam.From) — so a box is never
// admitted by a person's credential, nor a person by a run's, and no step
// after admission branches on the transport but through the admission.
//
// Mutations: a strings.HasPrefix(r.URL.Path, "/box/") in fetch.go — red; an
// iam.From in admitBox — red.
func TestNothingAfterAdmissionAsksWhichAdmitterRan(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	accessors := []string{"admitBox", "principal", "share", "refs", "boxFloor"}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			who := fn.Name.Name
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.BasicLit:
					if n.Kind == token.STRING && strings.Contains(n.Value, "/box/") {
						t.Errorf("%s: %s names a box path %s", fset.Position(n.Pos()), who, n.Value)
					}
				case *ast.SelectorExpr:
					switch {
					case n.Sel.Name == "BoxGitPrefix" && who != "admitBox":
						t.Errorf("%s: %s names auth.BoxGitPrefix", fset.Position(n.Pos()), who)
					case n.Sel.Name == "BasicAuth" && who != "admitBox":
						t.Errorf("%s: %s reads git's Basic credential", fset.Position(n.Pos()), who)
					case n.Sel.Name == "From" && isIdent(n.X, "iam") && who != "admit":
						t.Errorf("%s: %s reads the guard's resolution (iam.From)", fset.Position(n.Pos()), who)
					}
					if inner, ok := n.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "box" &&
						!slices.Contains(accessors, who) {
						t.Errorf("%s: %s reads a box's %s past the admission's accessors", fset.Position(n.Pos()),
							who, n.Sel.Name)
					}
				}
				return true
			})
		}
	}
}

// isIdent reports whether e is the identifier name.
func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// NEITHER TRANSPORT TAKES THE OTHER'S CREDENTIAL, through the API's own guard:
// on /box/git/ an operator's bearer — which the guard marks anonymous on an
// unguarded route — and a request the guard resolved to a signed-in person are
// each challenged, and an operator token as the Basic password is refused as
// no run token, by its user name first; on /git/ a run's token, as a Basic
// password or a bearer, is the guard's 401. Every refused pairing reads no
// run and no repository.
//
// Mutation: admitBox falling back to the guard's resolution (iam.From) — the
// resolved person's row 200 — red.
func TestNeitherTransportTakesTheOthersCredential(t *testing.T) {
	t.Parallel()
	rec := &recording{}
	c, _ := boxCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: rec.wrap}})
	runs := c.Fleet.Runs()
	ref, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
	runToken, operator := runs.Token(ref), c.Fleet.Token(t, "ceo")
	n := c.Fleet.Nodes()[0]

	// A PERSON THE GUARD RESOLVED: codetest's guard holds Tier A tokens and
	// no session signer, so the person is put on the request as the guard
	// puts a cookie's.
	mux := http.NewServeMux()
	mux.Handle(host.PatternBoxInfoRefs, n.Host().BoxInfoRefs())
	signedIn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		person := iam.Principal{ID: uuid.New(), Login: "ceo.person", Kind: iam.KindPerson, Seat: "ceo"}
		mux.ServeHTTP(w, r.WithContext(iam.WithPrincipal(r.Context(), person)))
	}))
	t.Cleanup(signedIn.Close)

	do := func(base, path string, set func(*http.Request)) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(c.Ctx, http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		set(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		return resp, string(raw)
	}
	box := "/box/git/" + codetest.RepoName + ".git/" + boxFetchAdvert
	person := "/git/" + codetest.RepoName + ".git/" + boxFetchAdvert
	for _, row := range []struct {
		name   string
		base   string
		path   string
		set    func(*http.Request)
		status int
		says   string
	}{
		{"an operator's bearer on a box route", n.URL(), box, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+operator)
		}, http.StatusUnauthorized, "a coding box authenticates as `run`"},
		{"a signed-in person on a box route", signedIn.URL, box, func(*http.Request) {},
			http.StatusUnauthorized, "a coding box authenticates as `run`"},
		{"an operator token as a box's password", n.URL(), box, func(r *http.Request) {
			r.SetBasicAuth(host.BoxUser, operator)
		}, http.StatusUnauthorized, "this run token is not valid here"},
		{"an operator token under another user", n.URL(), box, func(r *http.Request) {
			r.SetBasicAuth("x", operator)
		}, http.StatusUnauthorized, "a coding box authenticates as `run` with its run token"},
		{"a run's token as a person's password", n.URL(), person, func(r *http.Request) {
			r.SetBasicAuth(host.BoxUser, runToken)
		}, http.StatusUnauthorized, ""},
		{"a run's token as a person's bearer", n.URL(), person, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+runToken)
		}, http.StatusUnauthorized, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			reads, resolves := runs.Reads(), len(rec.seen("resolve"))
			resp, answer := do(row.base, row.path, row.set)
			if resp.StatusCode != row.status || !strings.Contains(answer, row.says) {
				t.Errorf("answered %d %q, want %d saying %q", resp.StatusCode, answer, row.status, row.says)
			}
			if runs.Reads() != reads || len(rec.seen("resolve")) != resolves {
				t.Errorf("a refused pairing read %d runs and %d repositories", runs.Reads()-reads,
					len(rec.seen("resolve"))-resolves)
			}
		})
	}
	if resp, _ := do(n.URL(), box, func(r *http.Request) { r.SetBasicAuth(host.BoxUser, runToken) }); resp.StatusCode !=
		http.StatusOK {
		t.Errorf("a live run's token on its box route answered %d", resp.StatusCode)
	}
}

// EVERY BOX IS ONE SPOOL CREDENTIAL: the spool keys every coding box's
// requests as the one credential `runs`, so the boxes of two runs share one
// credential's room — on a node whose share holds one such request, the
// second box's waits behind the first's and is told why in a box's words —
// while a person's request beside them is granted at once; and a credential
// whose operator id is `runs` is one of its own, a person's key carrying a
// prefix the boxes' lacks.
//
// Mutations: key a box by its launch id — the second box is granted at once,
// a credential of its own — red; key a person by the bare credential — the
// credential called runs waits behind the boxes — red.
func TestEveryBoxIsOneSpoolCredential(t *testing.T) {
	fake, hold, started := holdFirst(t)
	timing := fast
	timing.SpoolWait = 300 * time.Millisecond
	c, head := boxCase(t, codetest.SoloOptions{GitPath: fake, Shape: codetest.Shape{Timing: timing}})
	node := c.Fleet.Nodes()[0]
	host.SetRequestMemory(node.Host(), 1<<10)
	body := repeatedWants(head, 8<<10)
	share := int64(len(body)) + 4<<10
	host.SetSpools(node.Host(), sizes(func(z *host.SpoolSizes) { z.Share, z.Request = share, share }))
	runs := c.Fleet.Runs()
	var tokens []string
	for range 2 {
		ref, _ := c.LaunchBox(codetest.FixtureAlice, code.AccessRead)
		tokens = append(tokens, runs.Token(ref))
	}
	boxFetch := func(token string) <-chan reply {
		out := make(chan reply, 1)
		req := boxRequest(t, c, 0, codetest.RepoName, boxUploadPack, host.BoxUser, token, v2, body)
		go func() { out <- boxSend(req, node.Client()) }()
		return out
	}
	armHold(t, hold)
	first := boxFetch(tokens[0])
	eventually(t, "the first box's request spooled whole and held in its upload-pack", func() bool {
		_, err := os.Stat(started)
		return err == nil && host.SpoolsArriving(node.Host()) == 0
	})
	second := <-boxFetch(tokens[1])
	if second.status != http.StatusServiceUnavailable ||
		!strings.Contains(second.body, "every coding box on this node shares one credential's room") {
		t.Errorf("a second box's request beside the first's held spool answered %d %q", second.status,
			short(second.body))
	}
	if a := <-fetchAsync(t, c, "dev", body); a.status != http.StatusOK {
		t.Errorf("a person's request beside the boxes' held spool answered %d %q", a.status, short(a.body))
	}
	// A PERSON CALLED runs: a principal no guard resolves today, put on the
	// request as the guard puts one.
	mux := http.NewServeMux()
	mux.Handle(host.PatternUploadPack, node.Host().UploadPack())
	runsLogin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := iam.Principal{ID: uuid.New(), Login: "runs", Kind: iam.KindMachine}
		mux.ServeHTTP(w, r.WithContext(iam.WithPrincipal(r.Context(), p)))
	}))
	t.Cleanup(runsLogin.Close)
	req, err := http.NewRequestWithContext(c.Ctx, http.MethodPost, runsLogin.URL+"/git/"+codetest.RepoName+
		"/git-upload-pack", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", host.UploadPackRequestType)
	req.Header.Set("Git-Protocol", v2)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("a credential named runs beside the boxes' held spool answered %d", resp.StatusCode)
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	if a := <-first; a.status != http.StatusOK {
		t.Errorf("the first box's request answered %d %q", a.status, short(a.body))
	}
	assertNoSpool(t, c)
}
