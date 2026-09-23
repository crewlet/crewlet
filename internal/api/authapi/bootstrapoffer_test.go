package authapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// codeEstate is an identity estate that holds founder codes and whether
// anybody is enrolled, read and written by the surface as its directory and its
// writer — so a code the surface mints is on the "log" the next time it looks,
// the way a real node's own rows are. What a code IS comes from the domain's
// own predicate, [iamdomain.BootstrapCode.State].
type codeEstate struct {
	stubDirectory
	stubWriter

	mu       sync.Mutex
	codes    map[string]iamdomain.BootstrapCode
	enrolled bool
	now      func() time.Time
	mintErr  error

	mints, withdrawals int
}

func newCodeEstate(now func() time.Time) *codeEstate {
	return &codeEstate{codes: map[string]iamdomain.BootstrapCode{}, now: now}
}

func (e *codeEstate) AnyPerson(context.Context) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.enrolled, nil
}

func (e *codeEstate) BootstrapCode(_ context.Context, id string) (
	iamdomain.BootstrapCode, error) {

	e.mu.Lock()
	defer e.mu.Unlock()
	return e.codes[id], nil
}

func (e *codeEstate) OutstandingBootstrapCodes(_ context.Context, now time.Time) (
	[]iamdomain.BootstrapCode, error) {

	e.mu.Lock()
	defer e.mu.Unlock()
	var out []iamdomain.BootstrapCode
	for _, code := range e.codes {
		if code.State(now) == iamdomain.CodeLive {
			out = append(out, code)
		}
	}
	return out, nil
}

func (e *codeEstate) MintBootstrap(_ context.Context, in iamdomain.BootstrapMint) (
	statelog.Result, error) {

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.mintErr != nil {
		return statelog.Result{}, e.mintErr
	}
	e.mints++
	e.codes[in.ID] = iamdomain.BootstrapCode{ID: in.ID, MintedBy: in.MintedBy,
		MintedAt: e.now(), ExpiresAt: in.ExpiresAt}
	return applied(statelog.Position{}), nil
}

func (e *codeEstate) WithdrawBootstrap(_ context.Context, id, _, _ string) (
	statelog.Result, error) {

	e.mu.Lock()
	defer e.mu.Unlock()
	e.withdrawals++
	code := e.codes[id]
	code.SpentAt = e.now()
	e.codes[id] = code
	return applied(statelog.Position{}), nil
}

func (e *codeEstate) Enrol(_ context.Context, in iamdomain.Enrolment) (
	statelog.Result, error) {

	e.mu.Lock()
	defer e.mu.Unlock()
	// THE RECORD'S OWN RULE, by the domain's predicate: a live code and
	// nobody enrolled.
	if e.codes[in.BootstrapCode].State(e.now()) != iamdomain.CodeLive {
		return statelog.Result{}, iamdomain.ErrBootstrapCodeDead
	}
	if e.enrolled {
		return statelog.Result{}, iamdomain.ErrBootstrapClosed
	}
	e.enrolled = true
	return applied(statelog.Position{}), nil
}

func (e *codeEstate) SpendBootstrap(_ context.Context, in iamdomain.BootstrapSpend) (
	statelog.Result, error) {

	e.mu.Lock()
	defer e.mu.Unlock()
	code := e.codes[in.ID]
	code.SpentAt, code.Person = e.now(), in.Person
	e.codes[in.ID] = code
	return applied(statelog.Position{}), nil
}

// founderNode is one node's sign-in surface over a shared estate, with its own
// store directory and a clock a case moves.
type founderNode struct {
	svc    *authapi.Service
	mux    *http.ServeMux
	path   string
	estate *codeEstate
}

func newFounderNode(t *testing.T, estate *codeEstate, now func() time.Time,
	mutate func(*config.Bootstrap)) founderNode {

	t.Helper()
	b := bootstrapFor(t)
	b.Store.Path = filepath.Join(t.TempDir(), "node.db")
	if mutate != nil {
		mutate(&b)
	}
	svc := buildWith(t, b, nil, func(o *authapi.Options) {
		o.Directory, o.Writer, o.Now = estate, estate, now
	})
	mux := http.NewServeMux()
	svc.Routes(mux)
	return founderNode{svc: svc, mux: mux, estate: estate,
		path: filepath.Join(filepath.Dir(b.Store.Path), authapi.BootstrapCodeFile)}
}

// code reads the node's file, failing the case if there is none.
func (n founderNode) code(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(n.path)
	if err != nil {
		t.Fatalf("read the code file: %v", err)
	}
	return strings.TrimSpace(string(raw))
}

// open is what the posture read tells a client about the founder route.
func (n founderNode) open(t *testing.T) bool {
	t.Helper()
	rec := httptest.NewRecorder()
	n.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/config", nil))
	return strings.Contains(rec.Body.String(), `"bootstrap":true`)
}

// A RESTART KEEPS A LIVE CODE AND REPLACES A DEAD ONE.
//
// The boot path returned early whenever the file existed, so a node restarted
// a day after its first boot advertised a code the log had aged out, and the
// founder who read it was refused as having typed it wrong. A live code is
// still kept — somebody may be about to type it — and a dead one is replaced
// by a fresh one, 0600, whose hash is on the log.
//
// Mutation: keep any existing file (the old early return) and the second boot
// advertises the dead code — the file is unchanged and nothing is minted.
func TestARestartReplacesADeadCodeAndKeepsALiveOne(t *testing.T) {
	t.Parallel()
	now := clock
	at := func() time.Time { return now }
	estate := newCodeEstate(at)
	node := newFounderNode(t, estate, at, nil)

	path, err := node.svc.OfferBootstrapCode(t.Context(), "node-a")
	if err != nil || path != node.path {
		t.Fatalf("the first boot offered %q (%v), want %s", path, err, node.path)
	}
	first := node.code(t)

	// A RESTART INSIDE THE LIFETIME: the same code, and nothing minted.
	now = clock.Add(time.Hour)
	if _, err := node.svc.OfferBootstrapCode(t.Context(), "node-a"); err != nil {
		t.Fatalf("second boot: %v", err)
	}
	if node.code(t) != first || estate.mints != 1 {
		t.Errorf("a restart replaced a live code (mints %d) — somebody may "+
			"have been typing it", estate.mints)
	}

	// A RESTART PAST IT: a fresh code the log honours, and the file is
	// the owner's alone.
	now = clock.Add(25 * time.Hour)
	if _, err := node.svc.OfferBootstrapCode(t.Context(), "node-a"); err != nil {
		t.Fatalf("boot after expiry: %v", err)
	}
	fresh := node.code(t)
	if fresh == first || estate.mints != 2 {
		t.Fatalf("a restart after the code aged out advertised it again "+
			"(mints %d)", estate.mints)
	}
	info, err := os.Stat(node.path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the replaced file is %v (%v), want 0600", info.Mode(), err)
	}
	// AND IT WORKS: the founder walks in with the code the file now holds.
	if rec := postBootstrap(t, node.mux, fresh, "jane.founder"); rec.Code != http.StatusOK {
		t.Errorf("the re-minted code answered %d: %s", rec.Code, rec.Body.String())
	}
}

// AN AGED-OUT CODE IS STALE, A RE-ISSUE REPLACES IT, AND THE FOUNDER WALKS IN.
//
// The refused attempt must leave the route open — it used to leave a
// reservation every reader counted as somebody, so the route said "closed" for
// good — and a re-issue on ANOTHER node must produce a code the first node
// redeems, which is what the log being the check buys.
func TestAnAgedOutCodeIsReissuedAndTheFounderWalksIn(t *testing.T) {
	t.Parallel()
	now := clock
	at := func() time.Time { return now }
	estate := newCodeEstate(at)
	nodeA := newFounderNode(t, estate, at, nil)
	nodeB := newFounderNode(t, estate, at, nil)

	if _, err := nodeA.svc.OfferBootstrapCode(t.Context(), "node-a"); err != nil {
		t.Fatalf("boot: %v", err)
	}
	stale := nodeA.code(t)

	// MONDAY: the Friday code has aged out.
	now = clock.Add(72 * time.Hour)
	rec := postBootstrap(t, nodeA.mux, stale, "jane.founder")
	if rec.Code != http.StatusGone ||
		!strings.Contains(rec.Body.String(), "bootstrap_code_stale") ||
		!strings.Contains(rec.Body.String(), "crewlet iam bootstrap-code") {
		t.Fatalf("an aged-out code answered %d %s, want 410 naming the "+
			"re-issue", rec.Code, rec.Body.String())
	}
	if !nodeA.open(t) || !nodeB.open(t) {
		t.Error("a refused bootstrap closed the founder route")
	}

	// THE RE-ISSUE, served by the other node: its file, its answer.
	path, err := nodeB.svc.ReissueBootstrapCode(t.Context(), "node-b")
	if err != nil || path != nodeB.path {
		t.Fatalf("re-issue answered %q (%v), want node-b's file", path, err)
	}
	if rec := postBootstrap(t, nodeA.mux, nodeB.code(t), "jane.founder"); rec.Code != http.StatusOK {
		t.Fatalf("the re-issued code answered %d on the other node: %s",
			rec.Code, rec.Body.String())
	}
	if nodeA.open(t) {
		t.Error("the route still reads open once the founder exists")
	}
	if _, err := os.Stat(nodeA.path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the redeeming node kept its code file (%v)", err)
	}
	// AND NODE B'S FILE GOES AT ITS NEXT BOOT: no code will ever be
	// honoured again, so the host keeps no superuser claim.
	if path, err := nodeB.svc.OfferBootstrapCode(t.Context(), "node-b"); err != nil || path != "" {
		t.Errorf("a boot after the company started offered %q (%v)", path, err)
	}
	if _, err := os.Stat(nodeB.path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a boot after the company started kept its code file (%v)", err)
	}
}

// A BOOT NEVER ENDS ANOTHER NODE'S CODE, and a re-issue ends every one.
//
// A fleet booting on an empty estate offers one code per node, and a founder
// may be typing any of them, so a node replacing its OWN dead file withdraws
// nothing. The re-issue is the gesture that leaves exactly one.
func TestABootEndsNoOtherCodeAndAReissueEndsThemAll(t *testing.T) {
	t.Parallel()
	at := func() time.Time { return clock }
	estate := newCodeEstate(at)
	nodeA := newFounderNode(t, estate, at, nil)
	nodeB := newFounderNode(t, estate, at, nil)
	for _, n := range []struct {
		node founderNode
		id   string
	}{{nodeA, "node-a"}, {nodeB, "node-b"}} {
		if _, err := n.node.svc.OfferBootstrapCode(t.Context(), n.id); err != nil {
			t.Fatalf("boot %s: %v", n.id, err)
		}
	}
	if estate.withdrawals != 0 {
		t.Errorf("a boot withdrew %d codes somebody may be typing",
			estate.withdrawals)
	}
	live, _ := estate.OutstandingBootstrapCodes(t.Context(), clock)
	if len(live) != 2 {
		t.Fatalf("live codes %d after two boots, want one per node", len(live))
	}
	if _, err := nodeA.svc.ReissueBootstrapCode(t.Context(), "node-a"); err != nil {
		t.Fatalf("re-issue: %v", err)
	}
	live, _ = estate.OutstandingBootstrapCodes(t.Context(), clock)
	if len(live) != 1 || live[0].MintedBy != "node-a" {
		t.Errorf("after a re-issue the live codes are %+v, want exactly the "+
			"new one", live)
	}
}

// A RE-ISSUE ASKS THE ROUTE'S OWN GATE.
//
// It asked whether an active, credentialled administrator existed, so a
// company whose only person was suspended — or a deployment with the route
// closed — was handed a code the route would never honour.
//
// Mutation: drop the gate from the re-issue and both cases mint a code.
func TestAReissueIsRefusedWhereTheRouteIsClosed(t *testing.T) {
	t.Parallel()
	at := func() time.Time { return clock }
	for _, tc := range []struct {
		name     string
		mutate   func(*config.Bootstrap)
		enrolled bool
	}{
		{"somebody is enrolled", nil, true},
		{"api.auth.bootstrap is closed", func(b *config.Bootstrap) {
			b.API.Auth.Bootstrap = config.BootstrapAccessClosed
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			estate := newCodeEstate(at)
			estate.enrolled = tc.enrolled
			node := newFounderNode(t, estate, at, tc.mutate)
			if _, err := node.svc.ReissueBootstrapCode(t.Context(),
				"node-a"); !errors.Is(err, iamdomain.ErrBootstrapClosed) {
				t.Errorf("a re-issue on a closed route answered %v", err)
			}
			if estate.mints != 0 {
				t.Errorf("a closed route was minted %d codes", estate.mints)
			}
			if path, err := node.svc.OfferBootstrapCode(t.Context(),
				"node-a"); err != nil || path != "" {
				t.Errorf("a boot on a closed route offered %q (%v)", path, err)
			}
		})
	}
}

// A MINT THAT DID NOT LAND TAKES ITS FILE WITH IT, so a file on the host is
// only ever a code the log honours or one a restart replaces.
func TestAMintThatDidNotLandLeavesNoFile(t *testing.T) {
	t.Parallel()
	at := func() time.Time { return clock }
	estate := newCodeEstate(at)
	estate.mintErr = errors.New("the broker did not acknowledge")
	node := newFounderNode(t, estate, at, nil)
	if _, err := node.svc.OfferBootstrapCode(t.Context(), "node-a"); err == nil {
		t.Fatal("a mint that did not land reported a code on offer")
	}
	if _, err := os.Stat(node.path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a mint that did not land left its file (%v)", err)
	}
}
