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
	"github.com/crewlet/crewlet/internal/provision"
)

// stubOrg is a stub of the parts of Atlassian one pass touches.
//
// NOT NAMED org: this package's tests read the real [org.Organization] to
// build a plan the way the engine does, and a fake shadowing that package
// would make it unreachable from every test file here.
type stubOrg struct {
	mu sync.Mutex
	// tokens is how many API tokens the account holds. A recreated account
	// holds none, which is the state this suite is about.
	tokens int
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

	minted  int
	granted int
	created int
}

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
		case strings.Contains(r.URL.Path, "/manage/api-tokens") && r.Method == http.MethodGet:
			if o.onTokenRead != nil {
				o.onTokenRead()
			}
			if o.listFails {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"unwell"}`))
				return
			}
			out := make([]map[string]string, o.tokens)
			for i := range out {
				out[i] = map[string]string{"id": "t"}
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.Contains(r.URL.Path, "/manage/api-tokens") && r.Method == http.MethodPost:
			o.minted++
			_, _ = w.Write([]byte(`{"token":"ATSTT-fresh"}`))
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

	// flushes counts the completions, and flushLive records whether the
	// context the last one was handed was still alive. The second is not a
	// detail: a completion that inherits the cancellation it is completing
	// does nothing at all, which is indistinguishable from never calling it.
	flushes   int
	flushLive bool
}

func (s *sink) Record(_ context.Context, name, value string) error {
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
	o := &stubOrg{tokens: 0}
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
	o := &stubOrg{tokens: 1}
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
	o := &stubOrg{tokens: 1}
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

	res, err := reconcile(ctx, t, &stubOrg{tokens: 1}, &sink{held: "ATSTT-live"})

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
	o := &stubOrg{tokens: 1, onTokenRead: cancel}

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
		if f.Kind == integration.FindingCredentialMissing && f.Subject == "secrets.keys" {
			named = true
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
	o := &stubOrg{tokens: 0}
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
	o := &stubOrg{tokens: 0}
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
// It was reported as identity_failed, which classifies DEGRADED and owed by an
// ADMIN — so the card read "Action required" and "you, at the third-party app"
// about a seat no person could help, on the brisk admin cadence, over a
// condition that clears itself within a minute. The sentence under it even
// said what was really happening: "waiting for Atlassian to make its new
// account grantable".
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
