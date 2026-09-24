package authapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// own predicate, [iamdomain.BootstrapCode.State], and what the writer does
// follows the domain's own rules ([iamdomain.Writer.Enrol],
// [iamdomain.Writer.ReissueBootstrap]): a founding TAKES its code inside the
// enrolment and ends every earlier attempt, another code's founding in progress
// refuses it, and a re-issue withdraws every live code and ends every
// unfinished founding before it mints. The domain's own suite certifies those
// rules against a real log; this fake only lets the surface meet them.
type codeEstate struct {
	stubDirectory
	stubWriter

	mu       sync.Mutex
	codes    map[string]iamdomain.BootstrapCode
	enrolled bool
	now      func() time.Time
	mintErr  error

	mints, withdrawals, releases int
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

// take is what a founding's take leaves on the log for the code with this
// id: taken for the person the code creates, by a request that stopped
// before its enrolment landed.
func (e *codeEstate) take(id string) iamdomain.BootstrapCode {
	e.mu.Lock()
	defer e.mu.Unlock()
	code := e.codes[id]
	code.SpentAt, code.Person = e.now(), code.FounderID()
	e.codes[id] = code
	return code
}

// state is what the log says the code with this id is at now.
func (e *codeEstate) state(id string, now time.Time) iamdomain.CodeState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.codes[id].State(now)
}

// live is the codes the log holds as live at now.
func (e *codeEstate) live(now time.Time) []iamdomain.BootstrapCode {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []iamdomain.BootstrapCode
	for _, code := range e.codes {
		if code.State(now) == iamdomain.CodeLive {
			out = append(out, code)
		}
	}
	return out
}

func (e *codeEstate) MintBootstrap(_ context.Context, in iamdomain.BootstrapMint) (
	statelog.Result, error) {

	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mint(in)
}

// mint records one code, as the domain's mint decide does: refused once
// anybody is enrolled. The caller holds mu.
func (e *codeEstate) mint(in iamdomain.BootstrapMint) (statelog.Result, error) {
	if e.mintErr != nil {
		return statelog.Result{}, e.mintErr
	}
	if e.enrolled {
		return statelog.Result{}, fmt.Errorf("%w: %w", iamdomain.ErrRefused,
			iamdomain.ErrBootstrapClosed)
	}
	e.mints++
	e.codes[in.ID] = iamdomain.BootstrapCode{ID: in.ID, MintedBy: in.MintedBy,
		MintedAt: e.now(), ExpiresAt: in.ExpiresAt}
	return applied(statelog.Position{}), nil
}

func (e *codeEstate) ReissueBootstrap(_ context.Context, in iamdomain.BootstrapMint) (
	statelog.Result, error) {

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.enrolled {
		return statelog.Result{}, fmt.Errorf("%w: %w", iamdomain.ErrRefused,
			iamdomain.ErrBootstrapClosed)
	}
	now := e.now()
	for id, code := range e.codes {
		switch code.State(now) {
		case iamdomain.CodeLive:
			code.SpentAt = now
			e.withdrawals++
		case iamdomain.CodeTaken:
			code.FounderReleased = true
			e.releases++
		}
		e.codes[id] = code
	}
	return e.mint(in)
}

func (e *codeEstate) Enrol(_ context.Context, in iamdomain.Enrolment) (
	statelog.Result, error) {

	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	if e.enrolled {
		return statelog.Result{}, fmt.Errorf("%w: %w", iamdomain.ErrRefused,
			iamdomain.ErrBootstrapClosed)
	}
	// THE TAKE'S OWN RULES, by the domain's predicate: another code's
	// founding in progress refuses this one, this code is taken for the
	// person the enrolment names or is finished by them, and a dead code
	// is refused.
	for id, other := range e.codes {
		if id != in.BootstrapCode && other.State(now) == iamdomain.CodeTaken {
			return statelog.Result{}, fmt.Errorf("%w: %w", iamdomain.ErrRefused,
				&iamdomain.FoundingInProgress{Until: other.ExpiresAt})
		}
	}
	code := e.codes[in.BootstrapCode]
	switch code.State(now) {
	case iamdomain.CodeLive:
		code.SpentAt, code.Person = now, in.PersonID
	case iamdomain.CodeTaken:
		if code.Person != in.PersonID {
			return statelog.Result{}, iamdomain.ErrRefused
		}
	default:
		return statelog.Result{}, fmt.Errorf("%w: %w", iamdomain.ErrRefused,
			iamdomain.ErrBootstrapCodeDead)
	}
	// EVERY EARLIER ATTEMPT IS ENDED by the founding that lands.
	for id, other := range e.codes {
		if id != in.BootstrapCode && other.Person != "" && !other.FounderReleased {
			other.FounderReleased = true
			e.codes[id] = other
			e.releases++
		}
	}
	code.FounderEnrolled = true
	e.codes[in.BootstrapCode] = code
	e.enrolled = true
	return applied(statelog.Position{}), nil
}

// founderNode is one node's sign-in surface over a shared estate, with its own
// store directory and a clock a case moves.
type founderNode struct {
	svc    *authapi.Service
	mux    *http.ServeMux
	path   string
	estate *codeEstate
	audit  *recordingAudit
}

func newFounderNode(t *testing.T, estate *codeEstate, now func() time.Time,
	mutate func(*config.Bootstrap)) founderNode {

	t.Helper()
	b := bootstrapFor(t)
	b.Store.Path = filepath.Join(t.TempDir(), "node.db")
	if mutate != nil {
		mutate(&b)
	}
	audit := &recordingAudit{}
	svc := buildWith(t, b, nil, func(o *authapi.Options) {
		o.Directory, o.Writer, o.Now, o.Audit = estate, estate, now, audit
	})
	mux := http.NewServeMux()
	svc.Routes(mux)
	return founderNode{svc: svc, mux: mux, estate: estate, audit: audit,
		path: filepath.Join(filepath.Dir(b.Store.Path), authapi.BootstrapCodeFile)}
}

// boot is the node's boot offer, failing the case if it offers nothing.
func (n founderNode) boot(t *testing.T, nodeID string) {
	t.Helper()
	if path, err := n.svc.OfferBootstrapCode(t.Context(), nodeID); err != nil ||
		path != n.path {
		t.Fatalf("the boot of %s offered %q (%v), want %s", nodeID, path, err,
			n.path)
	}
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
	live := estate.live(clock)
	if len(live) != 2 {
		t.Fatalf("live codes %d after two boots, want one per node", len(live))
	}
	if _, err := nodeA.svc.ReissueBootstrapCode(t.Context(), "node-a"); err != nil {
		t.Fatalf("re-issue: %v", err)
	}
	live = estate.live(clock)
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
// Mutation: drop the gate from the re-issue and the deployment closed by
// `api.auth.bootstrap` is minted a code — the configuration is a fact the
// domain's own refusal cannot see.
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

// ONE FOUNDING AT A TIME, AND THE SECOND FOUNDER IS TOLD WHEN TO COME BACK.
//
// Two live codes on two nodes, redeemed together, each passed their own
// person record, and the company got two founders carrying the whole
// ceiling. A founding takes the exemption on the company's one bootstrap
// subject now, so a second code presented while another founding is part-way
// through is refused — and the refusal is its own: `409 bootstrap_in_progress`
// naming when the other founding lapses and the command that ends it, never
// the closed answer (nobody is in the company) nor the stale one (this code is
// fine). It is not a failed attempt: it is reached only by presenting a live
// code. The founding in progress is finished with its own code, on any node.
//
// Mutations: drop the in-progress arm from the refusal mapping and the second
// founder is answered 500, a fault nobody can clear; let only a LIVE code past
// the route and the first founder cannot finish with the code they took.
func TestASecondFounderIsToldWhenTheFirstFoundingLapses(t *testing.T) {
	t.Parallel()
	at := func() time.Time { return clock }
	estate := newCodeEstate(at)
	nodeA := newFounderNode(t, estate, at, nil)
	nodeB := newFounderNode(t, estate, at, nil)
	nodeA.boot(t, "node-a")
	nodeB.boot(t, "node-b")
	codeA, codeB := nodeA.code(t), nodeB.code(t)

	// FRIDAY: Jane's founding with node A's code stopped after its take.
	first := estate.take(digestOf(codeA))

	rec := postBootstrap(t, nodeB.mux, codeB, "second.founder")
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	if rec.Code != http.StatusConflict || body["error"] != "bootstrap_in_progress" ||
		body["until"] != first.ExpiresAt.UTC().Format(time.RFC3339) {
		t.Fatalf("a second code during a founding answered %d %v, want 409 "+
			"bootstrap_in_progress until %s", rec.Code, body,
			first.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if !strings.Contains(body["message"], "crewlet iam bootstrap-code") {
		t.Errorf("the refusal does not name the command that ends the "+
			"founding in progress: %q", body["message"])
	}
	if _, failures := nodeB.audit.snapshot(); len(failures) != 0 {
		t.Errorf("a live code refused for a founding in progress was counted "+
			"as %d failed sign-ins", len(failures))
	}
	if !nodeA.open(t) || !nodeB.open(t) {
		t.Error("the route reads closed while nobody is in the company")
	}

	// THE FOUNDING IN PROGRESS FINISHES WITH ITS OWN CODE, on any node.
	if rec := postBootstrap(t, nodeB.mux, codeA, "jane.founder"); rec.Code != http.StatusOK {
		t.Fatalf("the founder could not finish with the code they took: %d %s",
			rec.Code, rec.Body.String())
	}
	// AND THE SECOND CODE CAN CREATE NOBODY: the company has started.
	if rec := postBootstrap(t, nodeA.mux, codeB, "second.founder"); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "bootstrap_closed") {
		t.Errorf("the second code after the founder landed answered %d %s, "+
			"want 409 bootstrap_closed", rec.Code, rec.Body.String())
	}
}

// A BOOT KEEPS THE CODE A FOUNDING TOOK.
//
// A taken code is the one code that finishes its founder's own stopped
// attempt before it lapses, so a restart inside its lifetime replacing it
// would leave the founder holding a file that no longer says what they took —
// and their next code refused as a founding in progress, by their own attempt.
//
// Mutation: keep only a LIVE code at boot and the restart replaces it.
func TestABootKeepsTheCodeAFoundingTook(t *testing.T) {
	t.Parallel()
	now := clock
	at := func() time.Time { return now }
	estate := newCodeEstate(at)
	node := newFounderNode(t, estate, at, nil)
	node.boot(t, "node-a")
	taken := node.code(t)
	estate.take(digestOf(taken))

	now = clock.Add(time.Hour)
	node.boot(t, "node-a")
	if node.code(t) != taken || estate.mints != 1 {
		t.Errorf("a restart replaced the code a founding took (mints %d)",
			estate.mints)
	}
}

// A RE-ISSUE ENDS A FOUNDING IN PROGRESS, AND ITS FOUNDER WALKS IN.
//
// The refusal a second founder meets names `crewlet iam bootstrap-code` as
// the way to end the founding in progress, so the re-issue must do exactly
// that: release the attempt, withdraw its code, and leave one code that
// works. Before, a re-issue withdrew only LIVE codes, so a founding in
// progress survived it and refused the fresh code as well.
//
// Mutation: mint on a re-issue without the domain's re-issue (a plain mint)
// and the stopped founding still holds the exemption against the new code.
func TestAReissueEndsAFoundingInProgressAndItsFounderWalksIn(t *testing.T) {
	t.Parallel()
	at := func() time.Time { return clock }
	estate := newCodeEstate(at)
	nodeA := newFounderNode(t, estate, at, nil)
	nodeB := newFounderNode(t, estate, at, nil)
	nodeA.boot(t, "node-a")
	stopped := nodeA.code(t)
	estate.take(digestOf(stopped))

	if path, err := nodeB.svc.ReissueBootstrapCode(t.Context(), "node-b"); err != nil ||
		path != nodeB.path {
		t.Fatalf("re-issue answered %q (%v), want node-b's file", path, err)
	}
	if got := estate.state(digestOf(stopped), clock); got != iamdomain.CodeWithdrawn {
		t.Errorf("the founding in progress reads %q after a re-issue, want "+
			"withdrawn", got)
	}
	if rec := postBootstrap(t, nodeA.mux, stopped, "jane.founder"); rec.Code != http.StatusGone {
		t.Errorf("the ended founding's code answered %d %s, want 410",
			rec.Code, rec.Body.String())
	}
	if rec := postBootstrap(t, nodeA.mux, nodeB.code(t), "jane.founder"); rec.Code != http.StatusOK {
		t.Fatalf("the founder was refused on the re-issued code: %d %s",
			rec.Code, rec.Body.String())
	}
}
