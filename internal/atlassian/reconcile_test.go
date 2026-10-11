package atlassian_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/integration/integrationtest"
	"github.com/crewlet/crewlet/internal/provision"
)

// stubOrg is a stub of the parts of Atlassian one pass touches.
//
// NOT NAMED org: this package's tests read the real [org.Organization] to
// build a plan the way the engine does, and a fake shadowing that package
// would make it unreachable from every test file here.
type stubOrg struct {
	mu sync.Mutex
	// tokens is the labels of the API tokens the account holds. A recreated
	// account holds none, which is the state this suite is about. STATE
	// rather than a canned answer: a mint adds the label it was sent and a
	// revocation removes one, so what a pass reads back is what it did.
	tokens []string
	// listFails makes the token read unanswerable, which is a different
	// fact from "there are none".
	listFails bool
	// empty is an organization holding no service account at all, which is
	// what a company looks like before its first pass.
	empty bool
	// onTokenRead runs while a seat's token listing is being answered, for
	// a test that has to make something happen PART-WAY through a pass —
	// after the organization-wide reads, inside the per-seat work.
	onTokenRead func()
	// inviteNotReady makes the product-access invite answer the way
	// Atlassian answers for an account it has only just created: a 404
	// whose message says the account is not in the directory.
	inviteNotReady bool
	// mintRefused makes the token mint answer 403, which is what an
	// organization key that may invite but may not manage API tokens
	// gets. It is the state that proved the Granted arm was shadowing
	// every failure after the grant.
	mintRefused bool
	// mintEmpty makes the mint answer success with no token in it, having
	// made one: the account then holds a token whose value nobody has.
	mintEmpty bool
	// revokeFails makes the token revocation answer 500.
	revokeFails bool
	// mintUnlisted makes a minted token absent from the listing that
	// follows it — a listing that has not caught up, which reads exactly
	// like a token that was never made.
	mintUnlisted bool
	// listsNoIDs makes the token listing carry labels and no ids, so the
	// revocation route — the token's id under the account — has nothing to
	// name.
	listsNoIDs bool

	minted  int
	granted int
	created int
	// revoked is the labels of the tokens a revocation removed, in order.
	revoked []string
}

// tokenID is the id this stub lists a token under. Derived from the label,
// which Atlassian keeps unique within an account, so the revocation route
// is the only place an id has to be read back from.
func tokenID(label string) string { return "id-" + label }

// earlier is a token an earlier pass minted for the stub's one seat.
var earlier = atlassian.TokenLabel("sre-lead", time.Unix(0, 0))

func (o *stubOrg) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/workspaces"):
			_, _ = w.Write([]byte(`{"data":[{"id":"ari:cloud:jira::site/cloud-1","attributes":` +
				`{"type":"JiraSoftware","hostUrl":"https://acme.atlassian.net"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/service-accounts") && r.Method == http.MethodGet:
			items := []map[string]string{{
				"id":          "acct-1",
				"displayName": atlassian.AccountName("SRE Lead", "sre-lead"),
				"email":       "acct@example.invalid",
			}}
			if o.empty {
				items = nil
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		case strings.HasSuffix(r.URL.Path, "/service-accounts") && r.Method == http.MethodPost:
			o.created++
			_ = json.NewEncoder(w).Encode(map[string]string{
				"id":          "acct-new",
				"displayName": atlassian.AccountName("SRE Lead", "sre-lead"),
				"email":       "acct-new@example.invalid",
			})
		case strings.HasSuffix(r.URL.Path, "/service-accounts/invite"):
			o.granted++
			if o.inviteNotReady {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(
					`{"detail":"the account was not found in the directory"}`))
				return
			}
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/manage/api-tokens") && r.Method == http.MethodGet:
			if o.onTokenRead != nil {
				o.onTokenRead()
			}
			if o.listFails {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"unwell"}`))
				return
			}
			out := make([]map[string]string, 0, len(o.tokens))
			for _, label := range o.tokens {
				token := map[string]string{"id": tokenID(label), "label": label}
				if o.listsNoIDs {
					delete(token, "id")
				}
				out = append(out, token)
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(r.URL.Path, "/manage/api-tokens") && r.Method == http.MethodPost:
			if o.mintRefused {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"detail":"this key may not manage API tokens"}`))
				return
			}
			var body struct {
				Label string `json:"label"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			o.minted++
			if !o.mintUnlisted {
				o.tokens = append(o.tokens, body.Label)
			}
			if o.mintEmpty {
				_, _ = w.Write([]byte(`{}`))
				return
			}
			_, _ = w.Write([]byte(`{"token":"ATSTT-fresh"}`))
		case strings.Contains(r.URL.Path, "/manage/api-tokens/") && r.Method == http.MethodDelete:
			// THE DOCUMENTED REVOCATION: the token's own id under the
			// account, answered 204.
			if o.revokeFails {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"the token service is unwell"}`))
				return
			}
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			at := slices.IndexFunc(o.tokens, func(label string) bool { return tokenID(label) == id })
			if at < 0 {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"no such token"}`))
				return
			}
			o.revoked = append(o.revoked, o.tokens[at])
			o.tokens = slices.Delete(o.tokens, at, at+1)
			w.WriteHeader(http.StatusNoContent)
		default:
			// A FAKE WITH A PERMISSIVE DEFAULT IS HOW THE INVITE POST STAYED
			// INVISIBLE. This branch answered 200 `{}` to every route nobody
			// had thought to match, so the one write a converged pass made
			// looked exactly like the reads beside it. Refused now, and the
			// pass reports it, which is what a fake is for.
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"message":"no route %s %s"}`, r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sink is a store that already holds a credential for the seat.
type sink struct {
	// forgotten is what a teardown asked this sink to delete.
	forgotten []string

	held  string
	wrote map[string]string

	// onRecord runs after a value has been sealed, for a test that has to
	// make something go wrong AFTER the pass has minted a live credential.
	// That window is the one the run's completion exists for, and it cannot
	// be reached from the vendor's side: by the time a token is sealed,
	// every request this seat makes is already behind it.
	onRecord func(name string)

	// flushErr is what completing the run answers, for the test about what
	// happens to the pass's OWN error when it does not answer nil.
	flushErr error

	// recordErr is what sealing a value answers, for the test about a
	// failure that is this deployment's own store rather than Atlassian.
	recordErr error

	// refuse, where it answers an error, is what sealing that one name
	// answers — for the window a whole-store refusal cannot reach: a node
	// draining at the instant it seals the token it has just minted.
	refuse func(name string) error

	// flushes counts the completions, and flushLive records whether the
	// context the last one was handed was still alive. The second is not a
	// detail: a completion that inherits the cancellation it is completing
	// does nothing at all, which is indistinguishable from never calling it.
	flushes   int
	flushLive bool
}

func (s *sink) Record(_ context.Context, name, value string) error {
	if s.recordErr != nil {
		return s.recordErr
	}
	if s.refuse != nil {
		if err := s.refuse(name); err != nil {
			return err
		}
	}
	if s.wrote == nil {
		s.wrote = map[string]string{}
	}
	s.wrote[name] = value
	if s.onRecord != nil {
		s.onRecord(name)
	}
	return nil
}
func (s *sink) Value(_ context.Context, name string) (string, bool, error) {
	if name == "SEAT_TOKEN" && s.held != "" {
		return s.held, true, nil
	}
	if v, ok := s.wrote[name]; ok {
		return v, true, nil
	}
	return "", false, nil
}
func (s *sink) Discard(context.Context) error { return nil }

// Forget implements [provision.TokenSink]: it records what a teardown
// asked to be deleted, so a case can assert the deletion happened.
func (s *sink) Forget(_ context.Context, names ...string) error {
	s.forgotten = append(s.forgotten, names...)
	return nil
}
func (s *sink) Flush(ctx context.Context) error {
	s.flushes++
	s.flushLive = ctx.Err() == nil
	return s.flushErr
}
func (s *sink) Describe() string { return "test" }
func (s *sink) NextStep() string { return "" }

// reconcile runs one pass and hands back everything it answered, the error
// included.
//
// Separate from run because several tests below are ABOUT that error, and run
// treats one as a failed test — right for a pass that is meant to succeed, and
// the exact thing those tests have to look at.
func reconcile(
	ctx context.Context, t *testing.T, o *stubOrg, s provision.TokenSink,
) (*atlassian.Result, error) {
	t.Helper()
	plan := &provision.Plan{}
	plan.Add(provision.Seat{
		Handle: "sre-lead", Role: "SRE Lead",
		TokenVar: "SEAT_TOKEN", EmailVar: "SEAT_EMAIL",
	})
	return atlassian.Reconcile(ctx, atlassian.Options{
		Client: atlassian.NewClient(atlassian.ClientOptions{BaseURL: o.server(t).URL}),
		OrgID:  "org-1", Key: "key", Plan: plan, Sink: s,
		Now: func() time.Time { return time.Unix(1, 0).UTC() },
	})
}

func run(t *testing.T, o *stubOrg, s *sink) *atlassian.Result {
	t.Helper()
	res, err := reconcile(context.Background(), t, o, s)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

// A HELD CREDENTIAL IS NOT NECESSARILY A WORKING ONE.
//
// Disconnecting with "remove accounts" deletes them at Atlassian and leaves
// the minted token in the sealed store, because the store is the company's
// and a teardown that emptied it would take values an operator put there by
// hand. A later reconnect creates a NEW account, finds a credential already
// held, mints nothing, and every call the seat makes is refused with a 401
// naming nothing. The only cure was deleting the secret by hand.
//
// The account having NO tokens is the test: no account this engine has
// finished with is in that state, and every freshly created one is.
func TestATokenForAnAccountThatNoLongerExistsIsMintedOver(t *testing.T) {
	t.Parallel()
	o := &stubOrg{}
	s := &sink{held: "ATSTT-for-the-deleted-account"}

	res := run(t, o, s)

	if o.minted != 1 {
		t.Fatalf("minted %d times, want one fresh token (seats %+v)", o.minted, res.Seats)
	}
	if s.wrote["SEAT_TOKEN"] != "ATSTT-fresh" {
		t.Errorf("the store still holds %q", s.wrote["SEAT_TOKEN"])
	}
	if len(res.Seats) != 1 || !res.Seats[0].TokenMinted {
		t.Errorf("the pass did not report a mint: %+v", res.Seats)
	}
}

// AND A CREDENTIAL THE ACCOUNT STILL HAS IS LEFT ALONE. A mint is not
// idempotent: Atlassian issues a new token every time and shows it once, so
// minting on every pass would rotate the credential every running seat is
// authenticating with, on the loop's timer.
func TestAWorkingCredentialIsNotRotatedOnEveryPass(t *testing.T) {
	t.Parallel()
	o := &stubOrg{tokens: []string{earlier}}
	s := &sink{held: "ATSTT-live"}

	run(t, o, s)

	if o.minted != 0 {
		t.Errorf("minted %d times over a credential the account still holds", o.minted)
	}
}

// AND "CANNOT TELL" LEAVES IT ALONE TOO.
//
// A failed read is not evidence that a credential is dead. Minting on it
// would rotate a working token every time Atlassian was briefly unreachable,
// on a timer, which is the failure the three-valued rule exists to prevent
// everywhere else in this engine.
func TestAnUnreadableAccountDoesNotCostTheSeatItsCredential(t *testing.T) {
	t.Parallel()
	o := &stubOrg{listFails: true}
	s := &sink{held: "ATSTT-live"}

	run(t, o, s)

	if o.minted != 0 {
		t.Errorf("minted %d times on an answer the vendor never gave", o.minted)
	}
}

// A CONVERGED SEAT IS NOT RE-GRANTED, ONCE PER PASS, FOR EVER.
//
// The product-access invite is a write at Atlassian, and it used to be sent on
// every pass for every seat that had an account — deliberately, because a
// grant that failed on the pass that created the account would otherwise never
// be retried. The retry is still needed and the unconditional write was not:
// the reconcile loop runs this every few minutes for the life of the
// deployment, so what looked like a harmless idempotent call was an
// organization mutated on a timer, and the conformance suite's load-bearing
// clause was false for as long as it stood.
func TestAConvergedSeatIsNotGrantedProductAccessAgainOnEveryPass(t *testing.T) {
	t.Parallel()
	o := &stubOrg{tokens: []string{earlier}}
	s := &sink{held: "ATSTT-live"}

	run(t, o, s)

	if o.granted != 0 {
		t.Errorf("sent the product-access invite %d time(s) to an account that "+
			"has been holding a token since an earlier pass", o.granted)
	}
}

// BUT "CANNOT TELL" GRANTS, WHICH IS THE OPPOSITE DIRECTION FROM THE MINT.
//
// The evidence that an account was ever granted is that it holds an API token:
// the grant strictly precedes the mint, so nothing else could have issued one.
// When Atlassian cannot answer how many tokens an account holds, that evidence
// is absent rather than negative — and the two writes waiting on it take
// opposite directions, each the safe one for itself. Granting again costs one
// idempotent request; minting again revokes the credential every running seat
// is authenticating with. An account left ungranted, meanwhile, is refused by
// the product API with a 401 that reads exactly like a bad credential, which
// is the failure nobody can diagnose.
func TestAnAccountWhoseGrantCannotBeEstablishedIsGrantedRatherThanAssumedReady(t *testing.T) {
	t.Parallel()
	o := &stubOrg{listFails: true}
	s := &sink{held: "ATSTT-live"}

	run(t, o, s)

	if o.granted != 1 {
		t.Errorf("granted %d time(s) over an account Atlassian would not describe; "+
			"an ungranted account can reach nothing and says so as a 401", o.granted)
	}
	if o.minted != 0 {
		t.Errorf("minted %d time(s) on the same unreadable answer, which rotates a "+
			"live credential every time Atlassian is briefly unwell", o.minted)
	}
}

// A CANCELLED PASS RAISES, AND DOES NOT BLAME THE CREDENTIAL.
//
// Two claims are wrong here and only one of them is obvious. A pass that has
// observed nothing must not answer with findings at all, because the loop
// reads an empty findings list as "this integration is ready" and trusts it
// for a full settled interval — so a node draining would record Atlassian as
// converged on its way out. The second is quieter: the first call a pass makes
// is the site discovery, and its failure is wrapped in a sentence naming
// integrations.atlassian.api_key as the credential that could not be verified.
// Left to that wrapping, every drain writes a false accusation about a working
// key into the fleet's status, which is the same claim [integration.Refusal]
// exists to stop one level down.
func TestACancelledPassRaisesRatherThanBlamingTheOrganizationCredential(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := reconcile(ctx, t, &stubOrg{tokens: []string{earlier}}, &sink{held: "ATSTT-live"})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile over a cancelled context = (%+v, %v), want the "+
			"cancellation raised", res, err)
	}
	if strings.Contains(err.Error(), "api_key") {
		t.Errorf("a cancelled pass accused the organization credential: %v", err)
	}
}

// AND SO DOES ONE CANCELLED PART-WAY THROUGH, which the check at the top
// cannot catch.
//
// Every failure inside a seat is caught and reported as that seat's, so a
// context that dies after the account listing turns into an identity_failed
// finding per seat carrying "context canceled" in its detail — a statement
// that the operator's agents are broken, written to the fleet's status by a
// node that was merely shutting down, and believed by the next node to hold
// the duty. It is the SECOND half of the cancellation contract and it is the
// half a top-of-function guard silently leaves open.
func TestAPassCancelledPartWayThroughRaisesRatherThanReportingBrokenSeats(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	// Cancelled INSIDE the per-seat work — while this seat's token listing
	// is being answered — rather than during an organization-wide read.
	// Cancelling earlier proves nothing: the listing's own response read
	// fails, Reconcile raises that, and the test passes with the guard below
	// removed. It was written that way first and the mutation caught it.
	o := &stubOrg{tokens: []string{earlier}, onTokenRead: cancel}

	res, err := reconcile(ctx, t, o, &sink{held: "ATSTT-live"})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile cancelled mid-pass = (%+v, %v), want the cancellation "+
			"raised rather than reported as the seats' own failure", res, err)
	}
	if res != nil {
		t.Errorf("a cancelled pass still answered with a result: %+v", res)
	}
}

// A NODE WITH NO KEYRING CREATES NOTHING, AND SAYS SO AS THE OPERATOR'S WORK.
//
// The reconcile loop hands a node that cannot seal a credential
// [provision.ReadOnly], which is NOT NIL — and on the nil check this pass used
// to make, such a node created a service account at Atlassian, granted it
// product access, and only then discovered at the first Record that it had
// nowhere to put the token. Every seat was reported as a failure over a
// perfectly converged company, once per tick for ever, and each pass left
// behind one more identity nobody had asked for and nothing had a credential
// for.
//
// Reported rather than raised, because no retry can fix it: it resolves when
// somebody sets secrets.keys and never before.
func TestANodeWithNoKeyringCreatesNoAccountAndNamesTheSetting(t *testing.T) {
	t.Parallel()
	o := &stubOrg{empty: true}

	res, err := reconcile(context.Background(), t, o, provision.ReadOnly())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if o.created != 0 || o.granted != 0 || o.minted != 0 {
		t.Errorf("a node that cannot seal anything created %d account(s), granted "+
			"%d and minted %d", o.created, o.granted, o.minted)
	}
	var named bool
	for _, f := range res.Findings() {
		if f.Subject == "secrets.keys" {
			named = true
			// THE BOOTSTRAP FILE'S, so a config fault, the company
			// admin's: as a missing credential it read Failed and offered
			// the organization key this deployment already holds.
			integrationtest.AssertConfigFault(t, f)
		}
		if f.Kind == integration.FindingIdentityFailed {
			t.Errorf("reported the seat as failed over a company that is fine: %+v", f)
		}
	}
	if !named {
		t.Errorf("nothing in %+v tells the operator to set secrets.keys", res.Findings())
	}

	// AND THE CONTROL: the same organization, with somewhere to seal. Without
	// this the assertions above would hold just as well over a pass that had
	// stopped creating accounts entirely.
	sealing := &stubOrg{empty: true}
	if _, err := reconcile(context.Background(), t, sealing, &sink{}); err != nil {
		t.Fatalf("Reconcile with a sink that can seal: %v", err)
	}
	if sealing.created != 1 || sealing.granted != 1 || sealing.minted != 1 {
		t.Errorf("a node that CAN seal created %d account(s), granted %d and minted "+
			"%d, want one of each", sealing.created, sealing.granted, sealing.minted)
	}
}

// ---- the run's completion --------------------------------------------- //

// A PASS THAT SEALS A CREDENTIAL COMPLETES THE RUN THAT SEALED IT.
//
// [provision.TokenSink.Flush] is where what a run sealed STANDS, and under the
// reconcile loop it is where the engine rebuilds the `${VAR}` snapshot every
// seat's mcp_env resolves through. This pass reached it on no path at all: it
// minted an agent's API token, sealed it, and returned. Nothing announced the
// value, so nothing resolved it, and the next pass read the same sealed value
// back, found it held and never sealed again — permanently, because the only
// thing that rebuilds the snapshot is a Record that now never happens.
func TestAMintIsFollowedByTheCompletionThatAnnouncesIt(t *testing.T) {
	t.Parallel()
	o := &stubOrg{}
	s := &sink{held: "ATSTT-for-the-deleted-account"}

	run(t, o, s)

	if o.minted != 1 {
		t.Fatalf("minted %d time(s); this test is about what follows a mint", o.minted)
	}
	if s.flushes != 1 {
		t.Errorf("a pass that sealed a credential completed the run %d time(s): the "+
			"token is live at Atlassian, sealed under the seat's ${VAR}, and no "+
			"surface in this deployment can resolve it", s.flushes)
	}
}

// mintThenFail is a pass that mints a live credential and THEN fails.
//
// The failure comes from the sink rather than from Atlassian, and that is the
// only place it can come from: by the time a token is sealed this seat has
// made its last request, so nothing the organization does afterwards is still
// in the pass. Cancelling as the token is recorded puts the failure in exactly
// the window the completion exists for — a node draining mid-pass, having just
// minted.
func mintThenFail(t *testing.T, s *sink) (*atlassian.Result, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.held = "ATSTT-for-the-deleted-account"
	s.onRecord = func(name string) {
		// THE TOKEN, NOT THE ADDRESS. The address is sealed first and the
		// mint is still ahead of it, so cancelling there would fail the
		// mint's own request and there would be no sealed credential to be
		// about.
		if name == "SEAT_TOKEN" {
			cancel()
		}
	}
	o := &stubOrg{}
	res, err := reconcile(ctx, t, o, s)
	if o.minted != 1 {
		t.Fatalf("minted %d time(s); this test is about a pass that failed AFTER "+
			"minting, and nothing was minted", o.minted)
	}
	return res, err
}

// AND SO DOES ONE THAT FAILED AFTER MINTING, which is the path that actually
// occurs and the one an early return silently skips.
//
// A pass that returns its error several statements before the completion has
// left a live Atlassian credential sealed and unannounced. Everywhere else in
// this tree that state is re-minted on the next tick; here it is permanent,
// because the next pass finds the value held and never Records again.
func TestAPassThatFailsAfterMintingStillCompletesTheRun(t *testing.T) {
	t.Parallel()
	s := &sink{}

	_, err := mintThenFail(t, s)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the pass did not fail: %v", err)
	}
	if s.flushes != 1 {
		t.Errorf("a pass that sealed a credential and then failed completed the run "+
			"%d time(s), so the value is sealed, the engine's ${VAR} snapshot was "+
			"never rebuilt, and no later pass will ever seal again", s.flushes)
	}
}

// AND THE COMPLETION DOES NOT INHERIT THE CANCELLATION IT IS COMPLETING.
//
// The failure being cleaned up after is frequently the cancellation itself — a
// node draining mid-pass — and a flush handed a dead context does nothing at
// all, which is indistinguishable from the early return above. This is the
// rule every rollback and teardown in this tree follows, and it is invisible
// from a test that only counts the calls.
func TestTheCompletionOfAFailedPassDoesNotInheritItsCancellation(t *testing.T) {
	t.Parallel()
	s := &sink{}

	if _, err := mintThenFail(t, s); !errors.Is(err, context.Canceled) {
		t.Fatalf("the pass did not fail: %v", err)
	}
	if s.flushes != 1 {
		t.Fatalf("the run was completed %d time(s)", s.flushes)
	}
	if !s.flushLive {
		t.Errorf("the completion was handed the context whose cancellation it was " +
			"completing, so it does nothing at all and the sealed credential is " +
			"announced by nobody")
	}
}

// A FAILED COMPLETION IS REPORTED BESIDE THE PASS'S OWN ERROR, NEVER INSTEAD
// OF IT.
//
// Callers route on the pass's error: [integration.Reject] classifies it and an
// errors.Is against [integration.ErrCredentialRejected] decides whether an
// operator is sent to rotate the organization key or told to wait. Replacing
// it with a sink failure sends them to the wrong place; dropping the sink
// failure hides a store this deployment can no longer seal into. Both have to
// survive, which is what errors.Join is for.
func TestAFailedCompletionIsJoinedWithThePassesOwnErrorRatherThanReplacingIt(t *testing.T) {
	t.Parallel()
	s := &sink{flushErr: errors.New("the sealed store would not answer")}

	_, err := mintThenFail(t, s)
	if err == nil {
		t.Fatalf("neither the pass's failure nor the sink's was reported")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the completion's failure replaced the pass's own, so every caller "+
			"that routes on what went wrong now reads the wrong cause: %v", err)
	}
	if !strings.Contains(err.Error(), "the sealed store would not answer") {
		t.Errorf("a sink this deployment can no longer seal into was swallowed: %v", err)
	}
}

// AND A CHECK WITH NOWHERE TO SEAL HAS NOTHING TO COMPLETE.
//
// A nil sink is the command line's check — distinct from [provision.ReadOnly],
// which is a sink that refuses — and completing one would dereference nothing.
// Asserted because the guard that says so is one line and its absence is a
// panic in a pass an operator runs by hand.
func TestACheckWithNoSinkHasNothingToComplete(t *testing.T) {
	t.Parallel()
	o := &stubOrg{empty: true}

	res, err := reconcile(context.Background(), t, o, nil)
	if err != nil {
		t.Fatalf("Reconcile with no sink: %v", err)
	}
	if o.created != 0 || o.minted != 0 {
		t.Errorf("a check created %d account(s) and minted %d", o.created, o.minted)
	}
	if len(res.Seats) != 1 || res.Seats[0].AccountID != "" {
		t.Errorf("a check reported an account it did not create: %+v", res.Seats)
	}
}

// A SEAT ATLASSIAN HAS NOT FINISHED CREATING IS NOT A FAILED IDENTITY.
//
// Atlassian will not grant product access to an account it has only just made,
// and answers the invite with a 404 saying the account is not in the
// directory. The next pass grants it; nobody has to do anything.
//
// It was reported as identity_failed, which classifies DEGRADED and owed by a
// VENDOR ADMIN — so the card read "Action required" and "you, at the
// third-party app" about a seat no person could help, on the brisk
// vendor-admin cadence, over a condition that clears itself within a minute.
// The sentence under it even said what was really happening: "waiting for
// Atlassian to make its new account grantable".
//
// grant_pending is the kind for exactly this, and until now nothing in the
// tree produced it.
func TestASeatAtlassianCannotGrantYetIsPendingRatherThanFailed(t *testing.T) {
	t.Parallel()
	o := &stubOrg{empty: true, inviteNotReady: true}
	s := &sink{}

	res := run(t, o, s)

	var kinds []integration.FindingKind
	for _, f := range res.Findings() {
		kinds = append(kinds, f.Kind)
	}
	if !slices.Contains(kinds, integration.FindingGrantPending) {
		t.Fatalf("a seat waiting on Atlassian reported %v, want a grant_pending", kinds)
	}
	if slices.Contains(kinds, integration.FindingIdentityFailed) {
		t.Errorf("a seat waiting on Atlassian was reported as a failed identity: %v", kinds)
	}

	// AND THE VERDICT IS WHAT THE CARD READS, which is the half that made
	// this worth a finding kind of its own.
	phase, actor := integration.FindingGrantPending.Verdict()
	if phase != integration.PhaseActivating || actor != integration.ActorProvider {
		t.Fatalf("grant_pending is %s/%s, want activating and the provider", phase, actor)
	}
	if actor.WaitsOnAPerson() {
		t.Error("a seat waiting on Atlassian was reported as owed by a person")
	}
}

// A SEAT THIS PASS GRANTED IS NOT A FINISHED SEAT.
//
// Atlassian accepts a product-access invite immediately and applies it over
// about a minute. For that minute its gateway takes the token this pass then
// minted and Jira refuses it with its own 401 — so a pass that reported the
// seat done was telling an operator the agent was working while every call it
// made was refused. Measured on a live site, on every reconnect, because a
// reconnect creates a new account.
func TestASeatThisPassGrantedIsReportedAsStillComingUp(t *testing.T) {
	t.Parallel()
	o := &stubOrg{}
	s := &sink{}

	res := run(t, o, s)

	if o.granted != 1 {
		t.Fatalf("granted %d time(s), so this case asserts nothing", o.granted)
	}
	findings := res.Findings()
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want the one seat reported as coming up", findings)
	}
	if findings[0].Kind != integration.FindingGrantPending {
		t.Errorf("kind = %q, want %q — a grant that has not landed is the "+
			"PROVIDER's wait, and anything else sends somebody to act on an "+
			"account that works a minute later",
			findings[0].Kind, integration.FindingGrantPending)
	}
}

// AND THE PASS AFTER IT IS SILENT. The wait is about the grant this pass
// made; a seat whose account was granted by an earlier pass has nothing
// outstanding, and reporting one would park a converged company on the
// provider cadence for ever.
func TestASeatGrantedByAnEarlierPassIsNotReportedAgain(t *testing.T) {
	t.Parallel()
	o := &stubOrg{tokens: []string{earlier}}
	s := &sink{held: "ATSTT-live"}

	res := run(t, o, s)

	if o.granted != 0 {
		t.Fatalf("granted %d time(s), so this case asserts nothing", o.granted)
	}
	if findings := res.Findings(); len(findings) != 0 {
		t.Errorf("findings = %+v, want none over a converged seat", findings)
	}
}

// A SEAT THAT WAS GRANTED AND THEN FAILED REPORTS THE FAILURE, NOT THE WAIT.
//
// reconcileSeat sets Granted and keeps going: the address Record, the
// token-slot read, the mint and the token Record all follow, and each sets Err
// without clearing Granted. With the Granted arm ahead of the Err arm in
// Findings, every one of those four failures came out as grant_pending —
// PhaseActivating / ActorProvider, "Nothing has to be done; the next pass
// checks again" — so a seat whose mint was being refused sat there for ever
// with its error string dropped and nobody named.
func TestASeatGrantedAndThenRefusedReportsTheFailureRatherThanAWait(t *testing.T) {
	t.Parallel()
	o := &stubOrg{mintRefused: true}
	s := &sink{}

	res := run(t, o, s)

	if o.granted != 1 {
		t.Fatalf("granted %d time(s), so this case does not reach the shadowed arm", o.granted)
	}
	findings := res.Findings()
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want the one seat reported once", findings)
	}
	got := findings[0]
	if got.Kind != integration.FindingIdentityFailed {
		t.Fatalf("kind = %q, want %q: a refused mint is a failure, and %q tells "+
			"an operator nobody has to act",
			got.Kind, integration.FindingIdentityFailed, integration.FindingGrantPending)
	}
	// AND THE THIRD-PARTY APP'S OWN WORDS SURVIVE. The grant_pending
	// sentence is this engine's prose; the reason a seat has no credential
	// is Atlassian's, and dropping it leaves nothing to act on.
	if !strings.Contains(got.Detail, "may not manage API tokens") {
		t.Errorf("detail = %q, want the vendor's refusal in it", got.Detail)
	}
	// AND THE VERDICT IS THE ONE THAT NAMES SOMEBODY.
	if phase, actor := got.Kind.Verdict(); actor != integration.ActorVendorAdmin {
		t.Errorf("classified %s/%s, want an Atlassian admin owed it", phase, actor)
	}
}

// A FAILURE IN THIS ENGINE'S OWN STORE IS THE COMPANY ADMIN'S, NOT ATLASSIAN'S.
//
// Every seat failure was an identity_failed, owed by an Atlassian admin and
// asked about from fifteen seconds on — including a keyring that refused to
// keep the address Atlassian had just assigned, over an account that was
// fine. Where it went wrong decides whose it is: the mint refusal above stays
// the Atlassian admin's, and this one is this deployment's configuration.
func TestASeatThisEngineCouldNotSealForIsTheCompanyAdmins(t *testing.T) {
	t.Parallel()
	o := &stubOrg{}
	s := &sink{recordErr: errors.New("seal: keyring unavailable")}

	res := run(t, o, s)

	findings := res.Findings()
	if len(findings) != 1 || findings[0].Subject != "sre-lead" {
		t.Fatalf("findings = %+v, want the one seat reported", findings)
	}
	integrationtest.AssertConfigFault(t, findings[0])
	if !strings.Contains(findings[0].Detail, "keyring unavailable") {
		t.Errorf("detail = %q, want the store's own refusal in it", findings[0].Detail)
	}
}

// ---- a token nobody holds ---------------------------------------------- //

// refusingTheToken is a store that already holds the account's address and
// will not keep anything else, so the first write a pass attempts is the
// minted token's — the window between a live credential and its only copy.
func refusingTheToken() *sink {
	return &sink{
		recordErr: errors.New("the keyring refused the write"),
		wrote:     map[string]string{"SEAT_EMAIL": "acct@example.invalid"},
	}
}

// A TOKEN THAT COULD NOT BE RECORDED IS REVOKED.
//
// Atlassian shows a token's value once, so one this engine minted and then
// could not keep is a credential that exists, is held by nobody, and that
// nothing will ever remember to remove — valid for the better part of a year.
// A store refusing writes left one more of them on the account every pass.
func TestATokenThatCannotBeRecordedIsRevoked(t *testing.T) {
	t.Parallel()
	o := &stubOrg{}

	res := run(t, o, refusingTheToken())

	minted := atlassian.TokenLabel("sre-lead", time.Unix(1, 0))
	if !slices.Equal(o.revoked, []string{minted}) {
		t.Fatalf("revoked %v, want the token this pass minted (%s): it is live at "+
			"Atlassian and held by nobody", o.revoked, minted)
	}
	if len(o.tokens) != 0 {
		t.Errorf("the account still holds %v", o.tokens)
	}
	if len(res.Seats) != 1 || res.Seats[0].Err == nil || res.Seats[0].TokenMinted {
		t.Fatalf("seat = %+v, want the failure reported and no mint claimed", res.Seats)
	}
	// AND THE ORIGINAL FAILURE SURVIVES. The reason the pass stopped is what
	// an operator has to fix; a cleanup message replacing it would hide the
	// cause behind its consequence.
	if !strings.Contains(res.Seats[0].Err.Error(), "the keyring refused the write") {
		t.Errorf("the revocation replaced the cause: %v", res.Seats[0].Err)
	}
	// AND NOTHING IS LEFT AT ATLASSIAN, so nobody there owes anything: the
	// store that refused is this deployment's, the company admin's.
	findings := res.Findings()
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want the one seat", findings)
	}
	integrationtest.AssertConfigFault(t, findings[0])
}

// AND WHEN THE REVOCATION ALSO FAILS, somebody at Atlassian is told which
// token — the only case where that instruction is honest, and the one finding
// here that is an Atlassian admin's.
func TestATokenThatCannotBeRevokedIsNamedForAnAtlassianAdmin(t *testing.T) {
	t.Parallel()
	o := &stubOrg{revokeFails: true}

	res := run(t, o, refusingTheToken())

	findings := res.Findings()
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want the one seat", findings)
	}
	got := findings[0]
	if got.Kind != integration.FindingIdentityFailed {
		t.Fatalf("kind = %q, want %q: a credential live at Atlassian is only "+
			"removable there", got.Kind, integration.FindingIdentityFailed)
	}
	if _, actor := got.Kind.Verdict(); actor != integration.ActorVendorAdmin {
		t.Errorf("a token live at Atlassian is owed by %q, want the Atlassian admin", actor)
	}
	minted := atlassian.TokenLabel("sre-lead", time.Unix(1, 0))
	// THE TOKEN IS NAMED BEFORE ANY CAUSE, so the sentence's cut at
	// MaxDetailLength can only ever take a quoted refusal, never the label.
	if !strings.HasPrefix(got.Detail, fmt.Sprintf("atlassian: the API token labelled %q", minted)) {
		t.Errorf("detail = %q, want it to open on the token to delete", got.Detail)
	}
	for _, cause := range []string{"the keyring refused the write", "the token service is unwell"} {
		if !strings.Contains(got.Detail, cause) {
			t.Errorf("detail = %q, want %q in it", got.Detail, cause)
		}
	}
}

// AND A TOKEN THE LISTING DOES NOT SHOW YET IS NOT A TOKEN REVOKED.
//
// A token minted moments ago can be missing from the account's listing, and
// that absence reads exactly like a token that was never made. Only the second
// reading is safe to report as nothing left at Atlassian, and nothing can tell
// them apart — so it is a revocation that did not happen, which asks a person
// to look rather than telling them a live credential is gone.
func TestATokenTheListingDoesNotShowIsNotReportedRevoked(t *testing.T) {
	t.Parallel()
	o := &stubOrg{mintUnlisted: true}

	res := run(t, o, refusingTheToken())

	findings := res.Findings()
	if len(findings) != 1 || findings[0].Kind != integration.FindingIdentityFailed {
		t.Fatalf("findings = %+v, want the one identity_failed: a token that may be "+
			"live at Atlassian was reported as taken back", findings)
	}
	if !strings.Contains(findings[0].Detail, "carries no token labelled") {
		t.Errorf("detail = %q, want why the revocation did not happen", findings[0].Detail)
	}
}

// AND A TOKEN LISTED WITH NO ID IS NOT REVOKED BY ASKING FOR NOTHING.
//
// The revocation is a DELETE on the token's id under the account. With no id
// it would go to the collection itself, and the 404 that earns reads as a
// token already gone — the same false "nothing is left" by another route.
func TestATokenListedWithNoIDIsNotReportedRevoked(t *testing.T) {
	t.Parallel()
	o := &stubOrg{listsNoIDs: true}

	res := run(t, o, refusingTheToken())

	if len(o.tokens) != 1 {
		t.Fatalf("the account holds %v, want the one token nobody could revoke", o.tokens)
	}
	findings := res.Findings()
	if len(findings) != 1 || findings[0].Kind != integration.FindingIdentityFailed {
		t.Fatalf("findings = %+v, want the one identity_failed", findings)
	}
	if !strings.Contains(findings[0].Detail, "no id to revoke it by") {
		t.Errorf("detail = %q, want why the revocation did not happen", findings[0].Detail)
	}
}

// AND A NODE DRAINING AS IT SEALS STILL TAKES THE TOKEN BACK.
//
// The failure being cleaned up after is often the cancellation itself, and a
// revocation that inherited it would fail at its first request — leaving the
// credential live exactly when nothing is left running to notice. The pass
// still raises the cancellation, because a node shutting down has observed
// nothing worth reporting.
func TestATokenMintedAsTheNodeDrainsIsStillRevoked(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	o := &stubOrg{}
	s := &sink{refuse: func(name string) error {
		if name != "SEAT_TOKEN" {
			return nil
		}
		cancel()
		return context.Canceled
	}}

	_, err := reconcile(ctx, t, o, s)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile = %v, want the cancellation raised", err)
	}
	minted := atlassian.TokenLabel("sre-lead", time.Unix(1, 0))
	if !slices.Equal(o.revoked, []string{minted}) {
		t.Errorf("revoked %v, want %s: the revocation inherited the cancellation it "+
			"was cleaning up after, and the token is live with nobody holding it",
			o.revoked, minted)
	}
}

// A MINT THAT ANSWERS WITH NO VALUE IS THE SAME TOKEN FROM THE OTHER SIDE.
//
// Atlassian reported success and carried no credential, so the account may
// hold a token whose value nobody has. It used to be reported and left there.
// Found by the label this pass stamped, and taken back — and still Atlassian's
// to look at, because it was Atlassian's answer that came back empty.
func TestAMintThatReturnsNoTokenIsRevoked(t *testing.T) {
	t.Parallel()
	o := &stubOrg{mintEmpty: true}

	res := run(t, o, &sink{})

	minted := atlassian.TokenLabel("sre-lead", time.Unix(1, 0))
	if !slices.Equal(o.revoked, []string{minted}) {
		t.Fatalf("revoked %v, want the token the empty mint made (%s)", o.revoked, minted)
	}
	if len(res.Seats) != 1 || !errors.Is(res.Seats[0].Err, atlassian.ErrTokenNotReturned) {
		t.Fatalf("seat = %+v, want the empty mint reported", res.Seats)
	}
	findings := res.Findings()
	if len(findings) != 1 || findings[0].Kind != integration.FindingIdentityFailed {
		t.Errorf("findings = %+v, want the one identity_failed", findings)
	}
}

// A TOKEN NOTHING HOLDS IS REVOKED BEFORE ANOTHER IS MINTED BESIDE IT.
//
// The seat authenticates from its ${VAR}, and the ${VAR} is empty, so a token
// this engine minted on the account earlier is a value nobody in the company
// has. The pass mints a working one regardless, and used to leave the earlier
// one live — so a token stranded by a refused record, an empty mint answer or
// a failed revocation stayed on the account for good, and the instruction to
// delete it was undone by the next pass, which minted and reported the seat
// ready. A token somebody made on the account under a name of their own is
// not this engine's to take away.
func TestATokenNothingHoldsIsRevokedBeforeAnotherIsMinted(t *testing.T) {
	t.Parallel()
	const theirs = "deploy-script"
	o := &stubOrg{tokens: []string{earlier, theirs}}
	s := &sink{}

	res := run(t, o, s)

	if !slices.Equal(o.revoked, []string{earlier}) {
		t.Fatalf("revoked %v, want only the token this engine minted earlier (%s)",
			o.revoked, earlier)
	}
	minted := atlassian.TokenLabel("sre-lead", time.Unix(1, 0))
	if !slices.Equal(o.tokens, []string{theirs, minted}) {
		t.Errorf("the account holds %v, want %q left alone and the fresh %q",
			o.tokens, theirs, minted)
	}
	if s.wrote["SEAT_TOKEN"] != "ATSTT-fresh" || !res.Seats[0].TokenMinted {
		t.Errorf("the seat was not given a working credential: %+v", res.Seats)
	}
	if findings := res.Findings(); len(findings) != 0 {
		t.Errorf("findings = %+v, want none: nothing was wrong that this pass left", findings)
	}
}

// AND ONE THAT CANNOT BE REVOKED STOPS THE MINT, so the stray is reported
// rather than buried: minted beside, the account would hold a token the store
// does hold and every later pass would read the seat as converged.
func TestATokenNothingHoldsThatCannotBeRevokedIsReportedRatherThanMintedBeside(t *testing.T) {
	t.Parallel()
	o := &stubOrg{tokens: []string{earlier}, revokeFails: true}

	res := run(t, o, &sink{})

	if o.minted != 0 {
		t.Errorf("minted %d time(s) beside a stray it could not revoke", o.minted)
	}
	findings := res.Findings()
	if len(findings) != 1 || findings[0].Kind != integration.FindingIdentityFailed {
		t.Fatalf("findings = %+v, want the one identity_failed", findings)
	}
	if !strings.Contains(findings[0].Detail, earlier) {
		t.Errorf("detail = %q, want the stray's label in it", findings[0].Detail)
	}
}

// AND "CANNOT TELL" REVOKES NOTHING. A listing that failed is evidence of no
// token at all, and the mint goes ahead, which is safe because nothing is held
// for the seat to be rotated away.
func TestAnUnreadableAccountRevokesNothing(t *testing.T) {
	t.Parallel()
	o := &stubOrg{tokens: []string{earlier}, listFails: true}

	run(t, o, &sink{})

	if len(o.revoked) != 0 {
		t.Errorf("revoked %v on an answer Atlassian never gave", o.revoked)
	}
	if o.minted != 1 {
		t.Errorf("minted %d time(s), want one for a seat holding nothing", o.minted)
	}
}

// A REVOCATION OF A TOKEN ALREADY GONE SUCCEEDS, because gone is what it asks
// for and the pass that repeats it must not report a failure over a credential
// that no longer exists. Anything else Atlassian refuses is still a refusal.
func TestRevokingATokenAlreadyGoneSucceeds(t *testing.T) {
	t.Parallel()
	o := &stubOrg{}
	client := atlassian.NewClient(atlassian.ClientOptions{BaseURL: o.server(t).URL})

	if err := client.RevokeToken(context.Background(), "key", "acct-1", "id-nothing"); err != nil {
		t.Errorf("revoking a token Atlassian does not have: %v", err)
	}
	o.mu.Lock()
	o.revokeFails = true
	o.mu.Unlock()
	if err := client.RevokeToken(context.Background(), "key", "acct-1", "id-nothing"); err == nil {
		t.Error("a 500 from the revocation was reported as success")
	}
}

// AND THE PLAN'S CAVEATS ARE THE COMPANY DOCUMENT'S. A seat whose mcp_env
// slot is not a whole ${VAR} has nowhere for a minted token or an assigned
// address to be written, and only an edit of the document fixes that. They
// were short grants, owed by an Atlassian admin who can widen nothing that
// is missing here.
func TestAPlanNoteIsTheCompanyAdmins(t *testing.T) {
	t.Parallel()
	res := &atlassian.Result{Notes: []string{
		"data-analyst: mcp_env jira.JIRA_API_TOKEN is not a whole ${VAR} reference",
	}}
	findings := res.Findings()
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want the one note", findings)
	}
	integrationtest.AssertConfigFault(t, findings[0])
}
