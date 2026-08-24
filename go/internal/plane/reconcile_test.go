package plane_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/plane"
	"github.com/crewlet/crewlet/internal/provision"
)

// THE PLAN IS THE CONFIG'S OWN VARIABLE. Minting into a variable of the
// engine's own invention would produce a credential the seat's tools never
// read — the failure looks like an agent with no access, on an instance
// where everything was created correctly.
func TestASeatIsProvisionedIntoTheVariableItsConfigNames(t *testing.T) {
	t.Parallel()
	o := seatOrg(trackerSeat("SWE", "${PLANE_TOKEN_SWE}"))
	plan, err := plane.PlanFor(o, enabledPlane())
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	if len(plan.Seats) != 1 || plan.Seats[0].TokenVar != "PLANE_TOKEN_SWE" {
		t.Fatalf("plan = %+v", plan.Seats)
	}
	if plan.Seats[0].Handle != o.Roles[0].Handle() {
		t.Errorf("handle = %q", plan.Seats[0].Handle)
	}
}

// THE ENGINE'S OWN CREDENTIAL IS PART OF THE PLAN. Without it routing
// degrades to the targets a payload happens to name, which is a company
// where a comment mentioning three people wakes one of them.
func TestTheEnginesOwnCredentialIsPlannedBesideTheSeats(t *testing.T) {
	t.Parallel()
	o := seatOrg(trackerSeat("SWE", "${PLANE_TOKEN_SWE}"))
	plan, err := plane.PlanFor(o, withEngineToken(enabledPlane()))
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	var engine *provision.Seat
	for i, seat := range plan.Seats {
		if seat.Handle == plane.EngineHandle {
			engine = &plan.Seats[i]
		}
	}
	if engine == nil {
		t.Fatalf("no engine seat in %+v", plan.Seats)
	}
	if engine.TokenVar != "PLANE_ENGINE_TOKEN" {
		t.Errorf("engine token var = %q", engine.TokenVar)
	}
}

// THE ENGINE ACCOUNT IS CREATED AND MINTED like any other, and reported so
// an operator can see it happened.
func TestTheEngineAccountIsCreatedAndItsTokenRecorded(t *testing.T) {
	t.Parallel()
	f := newInstance()
	sink := newTrackerSink()
	res, err := run(t, f, plane.Options{Config: withEngineToken(enabledPlane()), Sink: sink})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Created) != 2 {
		t.Fatalf("created %v", res.Created)
	}
	if sink.recorded("PLANE_ENGINE_TOKEN") == "" {
		t.Error("the engine's credential was not recorded")
	}
	created := f.writes(http.MethodPost, "/service-accounts/")
	for _, body := range created {
		if body["username"] == "crewlet-engine" && body["role"] != "member" {
			t.Errorf("the engine was created as %v", body["role"])
		}
	}
}

// AN ENGINE WITH NO TOKEN VARIABLE IS NAMED rather than silently absent:
// the company still runs, it just resolves fewer recipients than it looks
// like it should.
func TestAnEngineWithNoCredentialVariableIsReported(t *testing.T) {
	t.Parallel()
	cfg := enabledPlane()
	cfg.Token = ""
	plan, err := plane.PlanFor(seatOrg(trackerSeat("SWE", "${PLANE_TOKEN_SWE}")), cfg)
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	if len(plan.Seats) != 1 {
		t.Fatalf("planned %+v", plan.Seats)
	}
	if !anyContains(plan.Notes, "no read credential of its own") {
		t.Errorf("notes = %q", plan.Notes)
	}
}

// THE ENGINE IS A MEMBER WHATEVER THE COMPANY CHOSE FOR ITS AGENTS: a guest
// cannot read the subscriber and member lists routing is built on, and the
// engine writes nothing, so admin is wrong in the other direction.
func TestTheEngineAccountIsAMemberRegardlessOfTheConfiguredDefault(t *testing.T) {
	t.Parallel()
	p := &config.PlaneProvisioning{Role: config.PlaneGuest}
	if got := plane.AccountRole(p, plane.EngineHandle); got != plane.RoleMember {
		t.Errorf("engine role = %d, want member (%d)", got, plane.RoleMember)
	}
}

// A LITERAL IS A NOTE, NOT A FAILURE: a seat whose key is written out is one
// an operator manages by hand, which is supported — it just cannot be
// minted into. The run continues for the seats that can be.
func TestALiteralCredentialIsReportedAndSkipped(t *testing.T) {
	t.Parallel()
	o := seatOrg(
		trackerSeat("SWE", "plane_api_written_out"),
		trackerSeat("QA", "${PLANE_TOKEN_QA}"),
	)
	plan, err := plane.PlanFor(o, enabledPlane())
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	if len(plan.Seats) != 1 || plan.Seats[0].TokenVar != "PLANE_TOKEN_QA" {
		t.Fatalf("plan = %+v", plan.Seats)
	}
	if !anyContains(plan.Notes, "a literal") {
		t.Fatalf("notes = %q", plan.Notes)
	}
	// THE NOTE MUST NOT ECHO THE CREDENTIAL. These reports are pasted
	// into tickets.
	if anyContains(plan.Notes, "written_out") {
		t.Errorf("the note repeated the credential: %q", plan.Notes)
	}
}

// A reference wearing other text is a different mistake from a literal, and
// says so — an operator who wrote `Bearer ${VAR}` needs to know the
// reference was seen.
func TestAnEmbeddedReferenceIsNamedAsOne(t *testing.T) {
	t.Parallel()
	o := seatOrg(trackerSeat("SWE", "Bearer ${PLANE_TOKEN_SWE}"))
	plan, err := plane.PlanFor(o, enabledPlane())
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	if len(plan.Seats) != 0 {
		t.Fatalf("planned %+v", plan.Seats)
	}
	if !anyContains(plan.Notes, "embedded") {
		t.Fatalf("notes = %q", plan.Notes)
	}
}

func TestPlanningNeedsAnEnabledIntegrationAndAWorkspace(t *testing.T) {
	t.Parallel()
	o := seatOrg(trackerSeat("SWE", "${PLANE_TOKEN_SWE}"))
	if _, err := plane.PlanFor(o, &config.Plane{Enabled: false}); err == nil {
		t.Error("a disabled integration planned a run")
	}
	cfg := enabledPlane()
	cfg.Workspace = ""
	if _, err := plane.PlanFor(o, cfg); err == nil {
		t.Error("a workspaceless config planned a run")
	}
}

// A HUMAN SEAT IS NOT PROVISIONED. Human seats are addressable and never
// spawned, so minting an API key for one would create a service account
// nothing ever authenticates as.
func TestOnlyAgentSeatsAreProvisioned(t *testing.T) {
	t.Parallel()
	human := trackerSeat("Founder", "${PLANE_TOKEN_FOUNDER}")
	human.Kind = org.KindHuman
	plan, err := plane.PlanFor(seatOrg(human), enabledPlane())
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	if !plan.Empty() {
		t.Fatalf("planned a human seat: %+v", plan.Seats)
	}
}

// ---- the run ----------------------------------------------------------- //

func TestAFreshWorkspaceGetsAnAccountATokenAndAWebhook(t *testing.T) {
	t.Parallel()
	f := newInstance()
	sink := newTrackerSink()
	res, err := run(t, f, plane.Options{Sink: sink, WebhookBase: "https://crewlet.example.com/"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Created) != 1 || len(res.Rotated) != 1 {
		t.Fatalf("created %v rotated %v", res.Created, res.Rotated)
	}
	if got := sink.recorded("PLANE_TOKEN_SWE"); !strings.HasPrefix(got, "plane_api_") {
		t.Errorf("recorded token = %q", got)
	}
	if got := sink.recorded("PLANE_WEBHOOK_SECRET"); !strings.HasPrefix(got, "plane_wh_") {
		t.Errorf("recorded webhook secret = %q", got)
	}
	if res.Hooked != "https://crewlet.example.com/webhooks/plane" {
		t.Errorf("hooked = %q", res.Hooked)
	}
	if len(res.Joined) != 1 || res.Joined[0] != "ENG" {
		t.Errorf("joined = %v", res.Joined)
	}
	if sink.flushes != 1 {
		t.Errorf("flushed %d times", sink.flushes)
	}
}

// THE CREATE RESPONSE'S TOKEN IS THE ONE RECORDED. A run that discarded it
// and minted a second would leave the account holding a live credential
// nothing ever wrote down.
func TestANewAccountsFirstTokenIsWhatIsRecorded(t *testing.T) {
	t.Parallel()
	f := newInstance()
	sink := newTrackerSink()
	if _, err := run(t, f, plane.Options{Sink: sink}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if n := f.liveTokens(); n != 1 {
		t.Fatalf("the instance holds %d live tokens, want 1 — a second mint "+
			"leaves a credential nobody recorded", n)
	}
}

// RUNNING IT TWICE IS SAFE. The second run finds the account rather than
// making another, which is what the derived username exists for.
func TestASecondRunFindsTheAccountItMade(t *testing.T) {
	t.Parallel()
	f := newInstance()
	if _, err := run(t, f, plane.Options{}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	sink := newTrackerSink()
	res, err := run(t, f, plane.Options{Sink: sink})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Created) != 0 {
		t.Errorf("the second run created %v", res.Created)
	}
	if len(res.Rotated) != 1 {
		t.Errorf("the second run rotated %v", res.Rotated)
	}
	if f.accountCount() != 1 {
		t.Errorf("the workspace holds %d accounts", f.accountCount())
	}
	if got := sink.recorded("PLANE_TOKEN_SWE"); got == "" {
		t.Error("the second run recorded nothing")
	}
}

// ROTATION RETIRES THE OLD CREDENTIAL, and only the one this tool minted.
func TestRotationRevokesThisToolsPreviousTokenAndNothingElse(t *testing.T) {
	t.Parallel()
	f := newInstance()
	if _, err := run(t, f, plane.Options{}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// An administrator's own token on the same account, which a rotation
	// must not touch: nothing here knows what is using it.
	f.mu.Lock()
	for id := range f.tokens {
		f.tokens[id] = append(f.tokens[id], &plane.Token{
			ID: "tok-by-hand", Label: "set up by an admin", Active: true,
		})
	}
	f.mu.Unlock()

	if _, err := run(t, f, plane.Options{}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var live []string
	for _, tokens := range f.tokens {
		for _, tok := range tokens {
			if tok.Active {
				live = append(live, tok.Label)
			}
		}
	}
	// The account's initial token, the admin's own, and the fresh one.
	// The tool's PREVIOUS token is the only thing retired.
	byHand := false
	for _, label := range live {
		if label == "set up by an admin" {
			byHand = true
		}
	}
	if !byHand {
		t.Errorf("rotation revoked a token it did not mint; live = %v", live)
	}
	rotated := 0
	for _, label := range live {
		if label == plane.TokenLabel("swe") {
			rotated++
		}
	}
	if rotated != 1 {
		t.Errorf("%d of this tool's tokens are live, want 1: %v", rotated, live)
	}
}

// ---- the preflight ------------------------------------------------------ //

// STOCK COMMUNITY IS REFUSED BEFORE ANYTHING IS CREATED. A run that found
// out halfway would leave some accounts made and some tokens live.
func TestAnInstanceWithoutServiceAccountsIsRefusedBeforeAnyMutation(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.noServiceAccounts = true
	_, err := run(t, f, plane.Options{})
	if err == nil {
		t.Fatal("a stock instance was provisioned")
	}
	if !strings.Contains(err.Error(), "service-account API") {
		t.Errorf("error = %v", err)
	}
	if f.accountCount() != 0 || f.liveTokens() != 0 {
		t.Errorf("the refusal still touched the instance: %d accounts, %d tokens",
			f.accountCount(), f.liveTokens())
	}
}

func TestEachMissingCapabilityNamesItself(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		set  func(*instance)
		want string
	}{
		"not an admin":    {func(f *instance) { f.notAdmin = true }, "administrator"},
		"bad workspace":   {func(f *instance) { f.badWorkspace = true }, "workspace slug"},
		"no webhook api":  {func(f *instance) { f.noWebhooks = true }, "webhook API"},
		"no service accs": {func(f *instance) { f.noServiceAccounts = true }, "service-account API"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newInstance()
			tc.set(f)
			_, err := run(t, f, plane.Options{})
			if err == nil {
				t.Fatal("the run proceeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

// A 403 IS PRESENCE, NOT ABSENCE. Reading it as a missing endpoint would
// tell an operator running the fork that their instance lacks a feature it
// has — when the real problem is the separate administrator check.
func TestARefusedRouteIsStillAPresentOne(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.notAdmin = true
	caps, err := workspaceClient(t, f).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !caps.ServiceAccounts || !caps.TokenLifecycle || !caps.Webhooks {
		t.Errorf("a non-admin credential read the fork's routes as absent: %+v", caps)
	}
	if caps.Admin {
		t.Error("a refused members list read as administrator")
	}
}

// NO TOKEN LIFECYCLE IS DEGRADED, NOT FATAL: a new seat's account still
// carries a token from its creation.
func TestWithoutTokenLifecycleNewSeatsStillWorkAndExistingOnesAreNamed(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.noTokenLifecycle = true
	sink := newTrackerSink()
	res, err := run(t, f, plane.Options{Sink: sink})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Created) != 1 || sink.recorded("PLANE_TOKEN_SWE") == "" {
		t.Fatalf("a new seat was not provisioned: %+v", res)
	}
	if !anyContains(res.Notes, "token-lifecycle") {
		t.Errorf("nothing said the instance is degraded: %q", res.Notes)
	}

	// The seat now exists, so the next run cannot rotate it — and has to
	// say so per seat rather than silently leaving it.
	res, err = run(t, f, plane.Options{})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Rotated) != 0 {
		t.Errorf("rotated %v on an instance with no token routes", res.Rotated)
	}
	if !anyContains(res.Notes, "PLANE_TOKEN_SWE") {
		t.Errorf("the seat that could not be rotated was not named: %q", res.Notes)
	}
}

// A TRANSPORT FAILURE IS NOT A CAPABILITY ANSWER. Reading a dropped
// connection as absence would tell an operator their fork lacks a feature
// because their network blinked.
func TestAProbeThatNeverLandsIsAnError(t *testing.T) {
	t.Parallel()
	c, err := plane.NewClient(plane.ClientOptions{
		// A port nothing listens on, reached with no proxy in the way.
		URL: "http://127.0.0.1:1", Workspace: "nimbus", APIKey: "k",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Probe(ctx); err == nil {
		t.Fatal("an unreachable instance answered the preflight")
	}
}

// ---- the identity hazards ----------------------------------------------- //

// AN INSTANCE THAT RENAMES THE ACCOUNT CANNOT BE PROVISIONED: the next run
// could not find what this one made, so it would create another, and
// another. The account is removed and nothing is minted.
func TestAnIgnoredUsernameUndoesTheAccountAndStops(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.renameTo = "svc_9f2c"
	sink := newTrackerSink()
	_, err := run(t, f, plane.Options{Sink: sink})
	if err == nil {
		t.Fatal("the run accepted a generated username")
	}
	if !strings.Contains(err.Error(), "svc_9f2c") {
		t.Errorf("the error does not name what was created instead: %v", err)
	}
	if f.accountCount() != 0 {
		t.Errorf("the account was left behind: %d", f.accountCount())
	}
	if sink.recorded("PLANE_TOKEN_SWE") != "" {
		t.Error("a credential was recorded for an unfindable account")
	}
}

// MEMBER ROWS WITH NO USERNAME make every seat unmatchable, so every run
// would create a duplicate and overwrite the live credential.
func TestAWorkspaceWithNoUsernamesIsRefused(t *testing.T) {
	t.Parallel()
	f := newInstance()
	// A member has to exist for the absence to be visible at all: an
	// empty workspace legitimately yields no usernames.
	f.accounts["someone"] = &plane.Account{ID: "acct-1", Username: "someone", IsBot: true}
	f.noUsernames = true
	_, err := run(t, f, plane.Options{})
	if err == nil {
		t.Fatal("a workspace with anonymous members was provisioned")
	}
	if !strings.Contains(err.Error(), "username") {
		t.Errorf("error = %v", err)
	}
	if f.accountCount() != 1 {
		t.Errorf("the refusal created something: %d accounts", f.accountCount())
	}
}

// AN EMPTY WORKSPACE IS NOT A WORKSPACE WITHOUT USERNAMES. Refusing it
// would make a first run impossible.
func TestAnEmptyWorkspaceIsProvisionedNormally(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.noUsernames = true
	if _, err := run(t, f, plane.Options{}); err != nil {
		t.Fatalf("an empty workspace was refused: %v", err)
	}
}

// A PERSON WHOSE NAME COLLIDES IS NEVER PROVISIONED ONTO. Everything the
// agent then did would be attributed to them.
func TestASeatIsNotProvisionedOntoAHumanAccount(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.accounts["crewlet-swe"] = &plane.Account{
		ID: "acct-human", Username: "crewlet-swe", IsBot: false,
	}
	_, err := run(t, f, plane.Options{})
	if err == nil {
		t.Fatal("a person's account was provisioned onto")
	}
	if !strings.Contains(err.Error(), "not a service account") {
		t.Errorf("error = %v", err)
	}
	if f.liveTokens() != 0 {
		t.Error("a token was minted on a person's account")
	}
}

// ---- projects ----------------------------------------------------------- //

// A TYPO'D PROJECT IS CAUGHT BEFORE ANY MUTATION, and the message says what
// the workspace does have — the alternative is agents that look like they
// are ignoring their work.
func TestAnUnknownProjectIsRefusedAndNamesWhatExists(t *testing.T) {
	t.Parallel()
	f := newInstance()
	cfg := enabledPlane()
	cfg.Provisioning.Projects = []string{"ENG", "PLTFRM"}
	_, err := run(t, f, plane.Options{Config: cfg})
	if err == nil {
		t.Fatal("an unknown project was accepted")
	}
	if !strings.Contains(err.Error(), "PLTFRM") || !strings.Contains(err.Error(), "ENG") {
		t.Errorf("error = %v", err)
	}
	if f.accountCount() != 0 {
		t.Errorf("the refusal created %d accounts", f.accountCount())
	}
}

// A DUPLICATE MEMBERSHIP IS SUCCESS. The instance maps it to a generic 400,
// and a run that treated that as failure could never run twice.
func TestAddingASeatToAProjectItIsAlreadyInSucceeds(t *testing.T) {
	t.Parallel()
	f := newInstance()
	if _, err := run(t, f, plane.Options{}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := run(t, f, plane.Options{}); err != nil {
		t.Fatalf("second run refused a membership that already held: %v", err)
	}
}

func TestAProjectIdentifierIsMatchedWithoutRegardToCase(t *testing.T) {
	t.Parallel()
	f := newInstance()
	cfg := enabledPlane()
	cfg.Provisioning.Projects = []string{"eng"}
	res, err := run(t, f, plane.Options{Config: cfg})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Joined) != 1 {
		t.Errorf("joined = %v", res.Joined)
	}
}

// ---- the webhook -------------------------------------------------------- //

// WITHOUT A PUBLIC URL NOTHING IS GUESSED. A hook pointing at the wrong host
// makes the instance report a healthy integration whose deliveries go
// nowhere.
func TestWithoutAPublicURLNoWebhookIsRegisteredAndItSaysSo(t *testing.T) {
	t.Parallel()
	f := newInstance()
	res, err := run(t, f, plane.Options{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Hooked != "" {
		t.Errorf("hooked %q with no public url", res.Hooked)
	}
	if !anyContains(res.Notes, "public base URL") {
		t.Errorf("notes = %q", res.Notes)
	}
}

// AN EXISTING WEBHOOK CONVERGES BUT ITS SECRET CANNOT BE RE-READ — the one
// asymmetry an operator has to be told about, because the remedy is a
// delete.
func TestAnExistingWebhookIsConvergedAndItsSecretIsNotClaimed(t *testing.T) {
	t.Parallel()
	f := newInstance()
	base := "https://crewlet.example.com"
	if _, err := run(t, f, plane.Options{WebhookBase: base}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	sink := newTrackerSink()
	res, err := run(t, f, plane.Options{Sink: sink, WebhookBase: base})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := sink.recorded("PLANE_WEBHOOK_SECRET"); got != "" {
		t.Errorf("the second run claimed to have re-read the secret: %q", got)
	}
	if len(f.hooks) != 1 {
		t.Errorf("the workspace holds %d hooks", len(f.hooks))
	}
	if !anyContains(res.Notes, "-recreate-webhook") {
		t.Errorf("nothing told the operator how to rotate it: %q", res.Notes)
	}
}

// A SINK THAT ALREADY HOLDS THE SECRET NEEDS NO ADVICE. Printing the
// caveat every run is how an operator learns to skip the notes.
func TestAnExistingWebhookIsQuietWhenItsSecretIsAlreadyHeld(t *testing.T) {
	t.Parallel()
	f := newInstance()
	base := "https://crewlet.example.com"
	sink := newTrackerSink()
	if _, err := run(t, f, plane.Options{Sink: sink, WebhookBase: base}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	res, err := run(t, f, plane.Options{Sink: sink, WebhookBase: base})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if anyContains(res.Notes, "-recreate-webhook") {
		t.Errorf("a recorded secret was reported as missing: %q", res.Notes)
	}
}

// -recreate-webhook IS THE ONLY RECOVERY for a secret nothing recorded,
// because the value cannot be read back — and it is destructive, which is
// why it is a flag rather than what a run does.
func TestRecreatingTheWebhookMintsAFreshSecret(t *testing.T) {
	t.Parallel()
	f := newInstance()
	base := "https://crewlet.example.com"
	if _, err := run(t, f, plane.Options{WebhookBase: base}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := f.hooks[0].SecretKey
	sink := newTrackerSink()
	if _, err := run(t, f, plane.Options{
		Sink: sink, WebhookBase: base, RecreateWebhook: true,
	}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(f.hooks) != 1 {
		t.Fatalf("the workspace holds %d hooks", len(f.hooks))
	}
	if got := sink.recorded("PLANE_WEBHOOK_SECRET"); got == "" || got == first {
		t.Errorf("recorded secret = %q, first = %q", got, first)
	}
	if f.hooks[0].SecretKey != sink.recorded("PLANE_WEBHOOK_SECRET") {
		t.Error("the recorded secret is not the one the workspace now holds")
	}
}

// A HOOK SOMEBODY ELSE REGISTERED IS LEFT ALONE. Reconfiguring the first one
// found would take down an unrelated integration.
func TestAnUnrelatedWebhookIsNotTouched(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.hooks = append(f.hooks, &plane.Webhook{
		ID: "wh-theirs", URL: "https://someone-else.example.com/hook", IsActive: true,
	})
	if _, err := run(t, f, plane.Options{WebhookBase: "https://crewlet.example.com"}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(f.hooks) != 2 {
		t.Fatalf("hooks = %d, want the theirs plus ours", len(f.hooks))
	}
	if f.hooks[0].URL != "https://someone-else.example.com/hook" {
		t.Errorf("somebody else's hook was re-pointed to %q", f.hooks[0].URL)
	}
}

// AN INSTANCE THAT DROPS THE PAGE ENTITY says nothing — DRF ignores unknown
// fields — so the echo is the only evidence, and the cost is a tool-skill
// sync that silently stops updating.
func TestAnInstanceWithoutPageWebhooksIsReported(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.dropPageEntity = true
	res, err := run(t, f, plane.Options{WebhookBase: "https://crewlet.example.com"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !anyContains(res.Notes, "`page` webhook entity") {
		t.Errorf("notes = %q", res.Notes)
	}
}

// A WEBHOOK SECRET WITH NOWHERE TO GO IS REFUSED. It is served once, so a
// run that created the hook and dropped the secret would leave every
// delivery unverifiable.
func TestAWebhookSecretThatIsNotAVariableIsRefused(t *testing.T) {
	t.Parallel()
	f := newInstance()
	cfg := enabledPlane()
	cfg.WebhookSecret = "written-out-by-hand"
	_, err := run(t, f, plane.Options{Config: cfg, WebhookBase: "https://crewlet.example.com"})
	if err == nil {
		t.Fatal("the run created a webhook with nowhere to put its secret")
	}
	if !strings.Contains(err.Error(), "${VAR}") {
		t.Errorf("error = %v", err)
	}
}

// ---- the rollback -------------------------------------------------------- //

// A RUN THAT CANNOT RECORD WHAT IT MINTED UNDOES IT. The alternative is a
// live credential in nobody's hands that nobody knows to revoke.
func TestARecordFailureUndoesTheAccountItCreated(t *testing.T) {
	t.Parallel()
	f := newInstance()
	sink := newTrackerSink()
	sink.failOn = "PLANE_TOKEN_SWE"
	_, err := run(t, f, plane.Options{Sink: sink})
	if err == nil {
		t.Fatal("the run reported success with nothing recorded")
	}
	if f.accountCount() != 0 {
		t.Errorf("the account survived a failed record: %d", f.accountCount())
	}
	if f.liveTokens() != 0 {
		t.Errorf("%d credentials are live and unrecorded", f.liveTokens())
	}
	if sink.discards != 1 {
		t.Errorf("the sink was discarded %d times", sink.discards)
	}
}

// ON AN EXISTING ACCOUNT the rollback revokes the token rather than deleting
// the account — the account is not this run's to remove.
func TestARecordFailureOnAnExistingAccountRevokesOnlyTheNewToken(t *testing.T) {
	t.Parallel()
	f := newInstance()
	if _, err := run(t, f, plane.Options{}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before := f.accountCount()
	sink := newTrackerSink()
	sink.failOn = "PLANE_TOKEN_SWE"
	if _, err := run(t, f, plane.Options{Sink: sink}); err == nil {
		t.Fatal("the run reported success with nothing recorded")
	}
	if f.accountCount() != before {
		t.Errorf("the rollback deleted an account it did not create")
	}
	if n := f.liveTokens(); n != 1 {
		t.Errorf("%d live tokens after the rollback, want the pre-existing one", n)
	}
}

// A WEBHOOK THIS RUN CREATED IS REMOVED TOO: its secret was recorded and
// then discarded, so leaving it would deliver signed requests the engine
// cannot verify — which reads as a broken integration rather than an absent
// one.
func TestAFailureAfterTheWebhookRemovesIt(t *testing.T) {
	t.Parallel()
	f := newInstance()
	sink := newTrackerSink()
	sink.failOn = "PLANE_WEBHOOK_SECRET"
	_, err := run(t, f, plane.Options{Sink: sink, WebhookBase: "https://crewlet.example.com"})
	if err == nil {
		t.Fatal("the run survived losing the webhook secret")
	}
	if len(f.hooks) != 0 {
		t.Errorf("the webhook was left behind: %d", len(f.hooks))
	}
	if f.accountCount() != 0 {
		t.Errorf("the seat's account was left behind: %d", f.accountCount())
	}
}

// THE ORIGINAL FAILURE SURVIVES A CLEANUP THAT DOES NOT FINISH — the reason
// the run stopped is what has to be fixed, and the credentials that are
// still live have to be named.
func TestACleanupThatFailsStillReportsTheCauseAndWhatIsLive(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.failDelete = true
	sink := newTrackerSink()
	sink.failOn = "PLANE_TOKEN_SWE"
	_, err := run(t, f, plane.Options{Sink: sink})
	if err == nil {
		t.Fatal("the run reported success")
	}
	if !strings.Contains(err.Error(), "the store is unreachable") {
		t.Errorf("the cause was lost: %v", err)
	}
	if !strings.Contains(err.Error(), "must be removed by hand") {
		t.Errorf("nothing named the credentials still live: %v", err)
	}
}

// THE ROLLBACK IS DETACHED FROM THE CALLER'S CONTEXT, because the failure is
// often the cancellation itself — and a rollback that inherited it would do
// nothing at all.
func TestTheRollbackRunsEvenWhenTheContextIsCancelled(t *testing.T) {
	t.Parallel()
	f := newInstance()
	sink := newTrackerSink()
	sink.failOn = "PLANE_TOKEN_SWE"
	ctx, cancel := context.WithCancel(context.Background())
	o := seatOrg(trackerSeat("SWE", "${PLANE_TOKEN_SWE}"))
	cfg := enabledPlane()
	plan, err := plane.PlanFor(o, cfg)
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	client := workspaceClient(t, f)
	sink.onRecord = cancel
	if _, err := plane.Reconcile(ctx, plane.Options{
		Client: client, Config: cfg, Plan: plan, Sink: sink,
	}); err == nil {
		t.Fatal("the run reported success")
	}
	if f.accountCount() != 0 {
		t.Errorf("a cancelled run left %d accounts behind", f.accountCount())
	}
}

// A RUN WITH NOWHERE TO PUT WHAT IT MINTS IS REFUSED, before it touches the
// instance.
func TestAReconcileWithNoSinkIsRefused(t *testing.T) {
	t.Parallel()
	f := newInstance()
	o := seatOrg(trackerSeat("SWE", "${PLANE_TOKEN_SWE}"))
	cfg := enabledPlane()
	plan, _ := plane.PlanFor(o, cfg)
	_, err := plane.Reconcile(context.Background(), plane.Options{
		Client: workspaceClient(t, f), Config: cfg, Plan: plan,
	})
	if err == nil {
		t.Fatal("a sinkless run proceeded")
	}
	if len(f.calls) != 0 {
		t.Errorf("a sinkless run talked to the instance: %v", f.calls)
	}
}

// AN EMPTY PLAN DOES NOTHING AND SAYS SO, without probing an instance it has
// no business touching.
func TestAnEmptyPlanTouchesNothing(t *testing.T) {
	t.Parallel()
	f := newInstance()
	res, err := plane.Reconcile(context.Background(), plane.Options{
		Client: workspaceClient(t, f), Config: enabledPlane(),
		Plan: mustPlan(t, seatOrg()), Sink: newTrackerSink(),
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Created) != 0 || len(res.Rotated) != 0 {
		t.Errorf("an empty plan did something: %+v", res)
	}
	if len(f.calls) != 0 {
		t.Errorf("an empty plan talked to the instance: %v", f.calls)
	}
}

// A MINT THAT ANSWERS WITH NO VALUE IS A FAILURE, not an empty credential:
// the token exists on the instance and nothing can ever read it.
func TestATokenlessMintResponseIsRefused(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.mintEmpty = true
	sink := newTrackerSink()
	_, err := run(t, f, plane.Options{Sink: sink})
	if err == nil {
		t.Fatal("an empty credential was accepted")
	}
	if sink.recorded("PLANE_TOKEN_SWE") != "" {
		t.Error("an empty credential was recorded")
	}
}

// THE PLAN'S NOTES AND THE RUN'S OWN BOTH REACH THE REPORT. A note appended
// to a slice captured up front is a note nobody ever sees.
func TestTheReportCarriesBothThePlansNotesAndTheRuns(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.noTokenLifecycle = true
	o := seatOrg(
		trackerSeat("SWE", "${PLANE_TOKEN_SWE}"),
		trackerSeat("QA", "written-out"),
	)
	cfg := enabledPlane()
	plan, err := plane.PlanFor(o, cfg)
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	res, err := run(t, f, plane.Options{Config: cfg, Plan: plan})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !anyContains(res.Notes, "a literal") {
		t.Errorf("the plan's note was dropped: %q", res.Notes)
	}
	if !anyContains(res.Notes, "token-lifecycle") {
		t.Errorf("the run's own note was dropped: %q", res.Notes)
	}
}

// ---- the expiry knob ----------------------------------------------------- //

// ZERO MEANS NEVER, which is what the config field documents — and a
// computed zero-day instant would mean a token dead the moment it is minted.
func TestATokenExpiryOfZeroSendsNoExpiryAtAll(t *testing.T) {
	t.Parallel()
	f := newInstance()
	if _, err := run(t, f, plane.Options{}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.bodies = nil
	cfg := enabledPlane()
	cfg.Provisioning.TokenExpiryDays = 0
	if _, err := run(t, f, plane.Options{Config: cfg}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, body := range f.mintBodies() {
		if _, ok := body["expired_at"]; ok {
			t.Errorf("a zero expiry sent %v", body["expired_at"])
		}
	}
}

func TestATokenExpiryInDaysIsSentAsAnInstant(t *testing.T) {
	t.Parallel()
	f := newInstance()
	if _, err := run(t, f, plane.Options{}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.bodies = nil
	cfg := enabledPlane()
	cfg.Provisioning.TokenExpiryDays = 30
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := run(t, f, plane.Options{Config: cfg, Now: func() time.Time { return now }}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	bodies := f.mintBodies()
	if len(bodies) != 1 {
		t.Fatalf("minted %d times", len(bodies))
	}
	if got := bodies[0]["expired_at"]; got != "2026-01-31T00:00:00Z" {
		t.Errorf("expired_at = %v", got)
	}
}

// THE ROLE IS ALWAYS NAMED, and it is never admin by default: the create
// endpoint's own default is admin, so a company that says nothing would
// otherwise get a workspace of administrators.
func TestAnUnconfiguredSeatIsAMemberNotAnAdministrator(t *testing.T) {
	t.Parallel()
	if got := plane.AccountRole(nil, "swe"); got != plane.RoleMember {
		t.Errorf("default role = %d, want member (%d)", got, plane.RoleMember)
	}
	p := &config.PlaneProvisioning{
		Role:  config.PlaneGuest,
		Roles: map[string]config.PlaneRole{"lead": config.PlaneAdmin},
	}
	if got := plane.AccountRole(p, "swe"); got != plane.RoleGuest {
		t.Errorf("configured default = %d", got)
	}
	if got := plane.AccountRole(p, "lead"); got != plane.RoleAdmin {
		t.Errorf("per-handle override = %d", got)
	}
}

func TestTheUsernamePrefixIsConfigurableAndFolded(t *testing.T) {
	t.Parallel()
	if got := plane.AccountUsername(nil, "SWE"); got != "crewlet-swe" {
		t.Errorf("username = %q", got)
	}
	p := &config.PlaneProvisioning{UsernamePrefix: "Nimbus-"}
	if got := plane.AccountUsername(p, "SWE"); got != "nimbus-swe" {
		t.Errorf("prefixed username = %q", got)
	}
}

func anyContains(notes []string, want string) bool {
	for _, note := range notes {
		if strings.Contains(note, want) {
			return true
		}
	}
	return false
}

func mustPlan(t *testing.T, o *org.Organization) *provision.Plan {
	t.Helper()
	plan, err := plane.PlanFor(o, enabledPlane())
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	return plan
}

// RETIRED TOKENS ARE NOT RE-REVOKED. Every rotation leaves another inactive
// row on the account, so a run that revoked them all would issue one more
// pointless request than the run before it — for the life of the seat.
func TestRotationDoesNotReRevokeWhatItAlreadyRetired(t *testing.T) {
	t.Parallel()
	f := newInstance()
	// FOUR RUNS, and the count is the point: the first creates, the
	// second's rotation has nothing of its own to retire, the third
	// retires the second's token — and only the FOURTH meets an
	// already-inactive row of this tool's own label, which is the row the
	// guard exists to skip.
	for i := range 4 {
		if _, err := run(t, f, plane.Options{}); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if i == 2 {
			f.forget()
		}
	}
	if n := f.tokenRevokes(); n != 1 {
		t.Errorf("the third run issued %d token revocations, want 1 — the "+
			"previous rotations' rows are already inactive", n)
	}
}

// A CREDENTIAL THAT DOES NOT AUTHENTICATE IS ITS OWN ANSWER. Without this
// first call every later 403 is unreadable: a bad key and a missing
// permission produce the same status on every route the preflight probes.
func TestABadCredentialIsNamedRatherThanReadAsAMissingFeature(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.badCredential = true
	_, err := run(t, f, plane.Options{})
	if err == nil {
		t.Fatal("a bad credential provisioned a workspace")
	}
	if !strings.Contains(err.Error(), "does not authenticate") {
		t.Errorf("error = %v", err)
	}
}

// A MINT THAT ANSWERS WITHOUT A VALUE leaves a token live on the instance
// that nothing can ever read — so it is a failure, on the rotation path as
// much as on the create path.
func TestARotationThatAnswersWithNoTokenValueIsRefused(t *testing.T) {
	t.Parallel()
	f := newInstance()
	if _, err := run(t, f, plane.Options{}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.mintEmpty = true
	sink := newTrackerSink()
	_, err := run(t, f, plane.Options{Sink: sink})
	if err == nil {
		t.Fatal("an empty rotation was accepted")
	}
	if sink.recorded("PLANE_TOKEN_SWE") != "" {
		t.Error("an empty credential was recorded over a live one")
	}
}

// A WEBHOOK CREATED WITHOUT A SECRET cannot be verified by anything, ever:
// the value is served once and this was the once.
func TestAWebhookCreatedWithoutASecretIsRefused(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.secretlessWebhook = true
	_, err := run(t, f, plane.Options{WebhookBase: "https://crewlet.example.com"})
	if err == nil {
		t.Fatal("a webhook with no secret was accepted")
	}
	if !strings.Contains(err.Error(), "served once") {
		t.Errorf("error = %v", err)
	}
	if len(f.hooks) != 0 {
		t.Errorf("the unverifiable webhook was left behind: %d", len(f.hooks))
	}
}

// A DUPLICATE MEMBERSHIP IS SUCCESS IN BOTH SHAPES THE INSTANCE USES. The
// fork answers 409 on some paths; stock CE maps the same constraint
// violation to a generic 400. A run that read either as failure could never
// run twice.
func TestADuplicateMembershipIsSuccessWhicheverStatusItArrivesAs(t *testing.T) {
	t.Parallel()
	for name, status := range map[string]int{
		"a generic 400":   http.StatusBadRequest,
		"an explicit 409": http.StatusConflict,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newInstance()
			f.duplicateAs = status
			if _, err := run(t, f, plane.Options{}); err != nil {
				t.Fatalf("first run: %v", err)
			}
			if _, err := run(t, f, plane.Options{}); err != nil {
				t.Fatalf("a repeat membership refused with %d aborted the run: %v",
					status, err)
			}
		})
	}
}

// A 400 THAT IS NOT A DUPLICATE ABORTS. Swallowing every 400 would report a
// seat joined to a project it cannot see — which looks, from the outside,
// exactly like an agent ignoring its work.
func TestAMembershipRefusedForAnyOtherReasonStopsTheRun(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.rejectMembership = true
	_, err := run(t, f, plane.Options{})
	if err == nil {
		t.Fatal("a refused membership was read as success")
	}
	if !strings.Contains(err.Error(), "INVALID_MEMBER") {
		t.Errorf("error = %v", err)
	}
	if f.accountCount() != 0 {
		t.Errorf("the account was left behind: %d", f.accountCount())
	}
}

// A DUPLICATE IS ONLY EVER READ OUT OF THE TWO STATUSES A DUPLICATE
// ARRIVES AS. An unhandled integrity error is a 500 whose body says
// "duplicate key value violates unique constraint" — and the membership it
// was trying to make may not exist, so reading that as success would report
// a seat joined to a project it cannot see.
func TestADuplicateMessageOnACrashIsNotSuccess(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.crashMembership = true
	_, err := run(t, f, plane.Options{})
	if err == nil {
		t.Fatal("a 500 naming a duplicate was read as an existing membership")
	}
	if f.accountCount() != 0 {
		t.Errorf("the account was left behind: %d", f.accountCount())
	}
}

// THE ROLE IS SENT AS THE ENDPOINT'S OWN WORD, and an unmapped value falls
// to the LEAST privilege: the create endpoint's own default is admin, so a
// mapping that fell the other way would silently hand out administrators.
func TestTheAccountsRoleIsSentAsTheWordTheEndpointTakes(t *testing.T) {
	t.Parallel()
	for word, role := range map[string]config.PlaneRole{
		"admin":  config.PlaneAdmin,
		"member": config.PlaneMember,
		"guest":  config.PlaneGuest,
	} {
		t.Run(word, func(t *testing.T) {
			t.Parallel()
			f := newInstance()
			cfg := enabledPlane()
			cfg.Provisioning.Role = role
			if _, err := run(t, f, plane.Options{Config: cfg}); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			created := f.writes(http.MethodPost, "/service-accounts/")
			if len(created) != 1 {
				t.Fatalf("created %d accounts", len(created))
			}
			if got := created[0]["role"]; got != word {
				t.Errorf("role = %v, want %q", got, word)
			}
		})
	}
}

// THE WEBHOOK SUBSCRIBES TO WHAT THE PARSER ROUTES AND NOTHING ELSE. A
// delivery for an entity nothing routes is a signed request the engine
// verifies, stores and then drops — cost with no recipient.
func TestTheWebhookSubscribesToExactlyWhatTheParserRoutes(t *testing.T) {
	t.Parallel()
	f := newInstance()
	if _, err := run(t, f, plane.Options{WebhookBase: "https://crewlet.example.com"}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	created := f.writes(http.MethodPost, "/webhooks/")
	if len(created) != 1 {
		t.Fatalf("created %d webhooks", len(created))
	}
	for entity, want := range map[string]bool{
		"project": true, "issue": true, "issue_comment": true, "page": true,
		"module": false, "cycle": false, "is_active": true,
	} {
		if got := created[0][entity]; got != want {
			t.Errorf("%s = %v, want %v", entity, got, want)
		}
	}
}

// A HUMAN SEAT IS VALIDATED, NEVER CREATED: the person already has an
// account, and provisioning one would give them a second identity.
func TestAHumanSeatIsCheckedRatherThanProvisioned(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.accounts["someone"] = &plane.Account{
		ID: "11111111-1111-1111-1111-111111111111", Username: "someone",
	}
	founder := &org.Role{Name: "Founder", Kind: org.KindHuman,
		Contact: &org.HumanContact{PlaneUserID: "11111111-1111-1111-1111-111111111111"}}
	res, err := run(t, f, plane.Options{
		Org: seatOrg(trackerSeat("SWE", "${PLANE_TOKEN_SWE}"), founder),
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Created) != 1 {
		t.Errorf("created %v — a human seat was provisioned", res.Created)
	}
	if anyContains(res.Notes, "founder") {
		t.Errorf("a resolvable human seat was flagged: %q", res.Notes)
	}
}

// A WRONG USER ID ADDRESSES NOBODY, SILENTLY — the assignment lands, the
// mention renders as raw markup, and no notification is ever delivered. So
// it is checked here, where the member table is right there to fix it from.
func TestAHumanSeatNamingAnAbsentUserIsReported(t *testing.T) {
	t.Parallel()
	f := newInstance()
	founder := &org.Role{Name: "Founder", Kind: org.KindHuman,
		Contact: &org.HumanContact{PlaneUserID: "22222222-2222-2222-2222-222222222222"}}
	res, err := run(t, f, plane.Options{
		Org: seatOrg(trackerSeat("SWE", "${PLANE_TOKEN_SWE}"), founder),
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !anyContains(res.Notes, "address nobody") {
		t.Errorf("notes = %q", res.Notes)
	}
}

// A ${VAR} RESOLVES AT RUN TIME against an environment this command does not
// have, so checking it here would report a working config as broken.
func TestAHumanSeatWhoseIDIsAReferenceIsNotChecked(t *testing.T) {
	t.Parallel()
	f := newInstance()
	founder := &org.Role{Name: "Founder", Kind: org.KindHuman,
		Contact: &org.HumanContact{PlaneUserID: "${FOUNDER_PLANE_ID}"}}
	res, err := run(t, f, plane.Options{
		Org: seatOrg(trackerSeat("SWE", "${PLANE_TOKEN_SWE}"), founder),
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if anyContains(res.Notes, "address nobody") {
		t.Errorf("a reference was checked against the member table: %q", res.Notes)
	}
}

// A HUMAN SEAT WITH NO ID AT ALL is named beside the table that fills it in.
func TestHumanSeatsWithNoIDAreNamedTogether(t *testing.T) {
	t.Parallel()
	f := newInstance()
	res, err := run(t, f, plane.Options{
		Org: seatOrg(
			trackerSeat("SWE", "${PLANE_TOKEN_SWE}"),
			&org.Role{Name: "Founder", Kind: org.KindHuman,
				Contact: &org.HumanContact{SlackUserID: "U0FOUNDER"}},
			&org.Role{Name: "Advisor", Kind: org.KindHuman,
				Contact: &org.HumanContact{SlackUserID: "U0ADVISOR"}},
		),
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !anyContains(res.Notes, "advisor, founder") {
		t.Errorf("notes = %q", res.Notes)
	}
	// AND NOT THE AGENT SEAT: an agent is reached by the account this run
	// just made it, so naming it here would send an operator looking for
	// a person who does not exist.
	if anyContains(res.Notes, "swe") {
		t.Errorf("an agent seat was named as a missing human: %q", res.Notes)
	}
}

// THE MEMBER TABLE IS THE REPORT'S POINT for a founder: the user ids are
// UUIDs Plane's own UI does not show.
func TestTheReportCarriesTheWorkspaceMemberTable(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.accounts["someone"] = &plane.Account{
		ID: "11111111-1111-1111-1111-111111111111", Username: "someone",
	}
	res, err := run(t, f, plane.Options{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var found bool
	for _, m := range res.Members {
		if m.Username == "someone" && m.ID != "" {
			found = true
		}
	}
	if !found {
		t.Errorf("members = %+v", res.Members)
	}
}

// ---- what a re-run does and does not touch ------------------------------ //

// A PLAIN RE-RUN KEEPS A WORKING CREDENTIAL. Rotating it would revoke what
// the running engine is authenticating with — an operator adding one seat
// would take the others down, from a command whose promise is that it is
// safe to re-run.
func TestARerunKeepsACredentialThatStillWorks(t *testing.T) {
	t.Parallel()
	f := newInstance()
	sink := newTrackerSink()
	if _, err := run(t, f, plane.Options{Sink: sink}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := sink.recorded("PLANE_TOKEN_SWE")
	f.forget()

	res, err := run(t, f, plane.Options{Sink: sink})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Rotated) != 0 {
		t.Errorf("a plain re-run rotated %v", res.Rotated)
	}
	if len(res.Kept) != 1 {
		t.Errorf("kept = %v", res.Kept)
	}
	if sink.recorded("PLANE_TOKEN_SWE") != first {
		t.Error("the recorded credential changed under a running engine")
	}
	if n := f.tokenRevokes(); n != 0 {
		t.Errorf("a plain re-run revoked %d tokens", n)
	}
}

// -rotate IS THE OPERATOR ASKING, having planned the restart that follows.
func TestRotateForcesAFreshCredential(t *testing.T) {
	t.Parallel()
	f := newInstance()
	sink := newTrackerSink()
	if _, err := run(t, f, plane.Options{Sink: sink}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := sink.recorded("PLANE_TOKEN_SWE")
	res, err := run(t, f, plane.Options{Sink: sink, Rotate: true})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Rotated) != 1 {
		t.Errorf("rotated = %v", res.Rotated)
	}
	if sink.recorded("PLANE_TOKEN_SWE") == first {
		t.Error("-rotate left the credential alone")
	}
}

// A VARIABLE NOBODY RECORDED IS MINTED INTO even though the account has a
// working token: the value cannot be read back, so minting is the only
// recovery — and it costs a restart, which the report says.
func TestAnUnrecordedVariableIsMintedIntoAndSaysWhy(t *testing.T) {
	t.Parallel()
	f := newInstance()
	if _, err := run(t, f, plane.Options{}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	sink := newTrackerSink() // a fresh operator machine: nothing recorded
	res, err := run(t, f, plane.Options{Sink: sink})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Rotated) != 1 {
		t.Fatalf("rotated = %v, kept = %v", res.Rotated, res.Kept)
	}
	if sink.recorded("PLANE_TOKEN_SWE") == "" {
		t.Error("nothing was recorded")
	}
	if !anyContains(res.Notes, "has to be restarted") {
		t.Errorf("the restart was not mentioned: %q", res.Notes)
	}
}

// A REVOKED TOKEN IS MINTED OVER whatever the variable holds: a value whose
// credential is dead leaves the seat 401ing for ever.
func TestARevokedCredentialIsReplacedEvenWhenTheVariableHasAValue(t *testing.T) {
	t.Parallel()
	f := newInstance()
	sink := newTrackerSink()
	if _, err := run(t, f, plane.Options{Sink: sink}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.revokeAll()
	res, err := run(t, f, plane.Options{Sink: sink})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Rotated) != 1 {
		t.Errorf("rotated = %v, kept = %v", res.Rotated, res.Kept)
	}
}

// AN EXPIRED TOKEN IS NOT A LIVE ONE, whatever `is_active` says: the flag
// records revocation, not the calendar.
func TestAnExpiredCredentialIsReplaced(t *testing.T) {
	t.Parallel()
	f := newInstance()
	sink := newTrackerSink()
	days := 30
	cfg := enabledPlane()
	if _, err := run(t, f, plane.Options{Sink: sink, Config: cfg, ExpiryDays: &days}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.expireAll(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	res, err := run(t, f, plane.Options{
		Sink: sink, Config: cfg, ExpiryDays: &days,
		Now: func() time.Time { return time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Rotated) != 1 {
		t.Errorf("an expired credential was kept: rotated = %v, kept = %v",
			res.Rotated, res.Kept)
	}
}

// AN UNREADABLE SINK IS NOT AN EMPTY ONE. Reading it as empty would rotate
// every live credential in the company because a store blinked.
func TestAnUnreadableSinkStopsTheRunRatherThanRotatingEverything(t *testing.T) {
	t.Parallel()
	f := newInstance()
	sink := newTrackerSink()
	if _, err := run(t, f, plane.Options{Sink: sink}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.forget()
	sink.holdsErr = errors.New("the store is unreachable")
	_, err := run(t, f, plane.Options{Sink: sink})
	if err == nil {
		t.Fatal("an unreadable sink was read as holding nothing")
	}
	if !strings.Contains(err.Error(), "guessing") {
		t.Errorf("error = %v", err)
	}
	if n := f.tokenRevokes(); n != 0 {
		t.Errorf("%d tokens were revoked on an unreadable sink", n)
	}
}

// ---- the destructive flags ---------------------------------------------- //

// -decommission DELETES THE ACCOUNTS WHOSE SEATS LEFT, and only those.
func TestDecommissionRemovesManagedAccountsWithNoSeat(t *testing.T) {
	t.Parallel()
	f := newInstance()
	if _, err := run(t, f, plane.Options{}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// A seat that used to exist, and a bot somebody else made.
	f.accounts["crewlet-qa"] = &plane.Account{ID: "acct-qa", Username: "crewlet-qa", IsBot: true}
	f.accounts["ci-bot"] = &plane.Account{ID: "acct-ci", Username: "ci-bot", IsBot: true}

	res, err := run(t, f, plane.Options{Decommission: true})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Decommissioned) != 1 || res.Decommissioned[0] != "crewlet-qa" {
		t.Fatalf("decommissioned = %v", res.Decommissioned)
	}
	if _, still := f.accounts["ci-bot"]; !still {
		t.Error("an unmanaged bot was deleted")
	}
	if _, still := f.accounts["crewlet-swe"]; !still {
		t.Error("a live seat's account was deleted")
	}
}

// A PERSON WHOSE NAME MATCHES THE PREFIX IS NEVER DELETED, and is reported:
// the delete cascade removes every token and membership and deactivates the
// user, so a wrong one has no undo.
func TestDecommissionLeavesPeopleAloneAndSaysSo(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.accounts["crewlet-person"] = &plane.Account{
		ID: "acct-person", Username: "crewlet-person", IsBot: false,
	}
	res, err := run(t, f, plane.Options{Decommission: true})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Decommissioned) != 0 {
		t.Errorf("decommissioned %v", res.Decommissioned)
	}
	if !anyContains(res.Notes, "not catching people") {
		t.Errorf("notes = %q", res.Notes)
	}
}

// THE MANAGED PREFIX IS NEVER EMPTY, which is what bounds what
// -decommission may delete: a company that clears `username_prefix` gets
// the default, not a sweep scoped to every service account in the
// workspace.
func TestTheManagedPrefixIsNeverEmpty(t *testing.T) {
	t.Parallel()
	for _, p := range []*config.PlaneProvisioning{
		nil, {}, {UsernamePrefix: "   "},
	} {
		if got := plane.AccountUsername(p, ""); got == "" {
			t.Errorf("an empty managed prefix for %+v", p)
		}
	}
}

// -create-projects MAKES WHAT THE CONFIG NAMES, instead of refusing.
func TestCreateProjectsMakesTheOnesTheWorkspaceLacks(t *testing.T) {
	t.Parallel()
	f := newInstance()
	cfg := enabledPlane()
	cfg.Provisioning.Projects = []string{"ENG", "PLTFRM"}
	res, err := run(t, f, plane.Options{Config: cfg, CreateProjects: true})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Joined) != 2 {
		t.Fatalf("joined = %v", res.Joined)
	}
	created := f.writes(http.MethodPost, "/projects/")
	if len(created) != 1 || created[0]["identifier"] != "PLTFRM" {
		t.Errorf("created = %v", created)
	}
}

// ONE VARIABLE CANNOT HOLD TWO SEATS' CREDENTIALS. The second overwrites
// the first, and the first agent then authenticates as the second — every
// action attributed to the wrong seat, with nothing reporting a problem.
func TestTwoSeatsCannotShareOneCredentialVariable(t *testing.T) {
	t.Parallel()
	o := seatOrg(
		trackerSeat("SWE", "${PLANE_TOKEN}"),
		trackerSeat("QA", "${PLANE_TOKEN}"),
	)
	_, err := plane.PlanFor(o, enabledPlane())
	if err == nil {
		t.Fatal("two seats were planned into one variable")
	}
	if !strings.Contains(err.Error(), "swe") || !strings.Contains(err.Error(), "qa") {
		t.Errorf("the error names neither seat: %v", err)
	}
}

// AND NEITHER CAN THE ENGINE AND A SEAT: the engine would authenticate as
// that seat, attributing every routing read to it.
func TestTheEngineCannotShareASeatsCredentialVariable(t *testing.T) {
	t.Parallel()
	cfg := enabledPlane()
	cfg.Token = "${PLANE_TOKEN_SWE}"
	_, err := plane.PlanFor(seatOrg(trackerSeat("SWE", "${PLANE_TOKEN_SWE}")), cfg)
	if err == nil {
		t.Fatal("the engine was planned into a seat's variable")
	}
	if !strings.Contains(err.Error(), "integrations.plane.token") {
		t.Errorf("error = %v", err)
	}
}

// A HOOK THAT DIFFERS ONLY IN A TRAILING SLASH IS A SECOND HOOK: Plane's
// duplicate check is byte-exact, so both fire and every event arrives
// twice. It is reported and never touched — a foreign hook is not this
// run's to remove.
func TestAHookAddressingTheSameEndpointIsReportedAndLeftAlone(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.hooks = append(f.hooks, &plane.Webhook{
		ID: "wh-dupe", URL: "https://crewlet.example.com:443/webhooks/plane/",
		IsActive: true,
	})
	res, err := run(t, f, plane.Options{WebhookBase: "https://crewlet.example.com"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !anyContains(res.Notes, "arrives twice") {
		t.Errorf("notes = %q", res.Notes)
	}
	if len(f.hooks) != 2 {
		t.Errorf("a foreign hook was removed: %d remain", len(f.hooks))
	}
}

// AND A GENUINELY DIFFERENT URL IS NOT REPORTED as a duplicate — an
// unrelated integration is none of this run's business.
func TestAnUnrelatedHookIsNotReportedAsADuplicate(t *testing.T) {
	t.Parallel()
	f := newInstance()
	f.hooks = append(f.hooks, &plane.Webhook{
		ID: "wh-theirs", URL: "https://someone-else.example.com/webhooks/plane",
		IsActive: true,
	})
	res, err := run(t, f, plane.Options{WebhookBase: "https://crewlet.example.com"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if anyContains(res.Notes, "arrives twice") {
		t.Errorf("an unrelated hook was called a duplicate: %q", res.Notes)
	}
}

// A COPY-PASTED VARIABLE IS CAUGHT AT THE VENDOR. Minting over it would
// hand this seat a second identity while the other keeps authenticating as
// one account from two places, and nothing anywhere would report it.
func TestACredentialBelongingToAnotherAccountStopsTheRun(t *testing.T) {
	t.Parallel()
	f := newInstance()
	sink := newTrackerSink()
	if _, err := run(t, f, plane.Options{Sink: sink}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Somebody else's live credential pasted into this seat's variable.
	f.mu.Lock()
	f.accounts["crewlet-qa"] = &plane.Account{ID: "acct-qa", Username: "crewlet-qa", IsBot: true}
	f.tokens["acct-qa"] = []*plane.Token{{
		ID: "tok-qa", Label: plane.TokenLabel("qa"), Active: true, Value: "plane_api_qa",
	}}
	f.mu.Unlock()
	sink.seed("PLANE_TOKEN_SWE", "plane_api_qa")

	_, err := run(t, f, plane.Options{Sink: sink})
	if err == nil {
		t.Fatal("a credential belonging to another account was accepted")
	}
	if !strings.Contains(err.Error(), "different account") {
		t.Errorf("error = %v", err)
	}
}

// "CANNOT TELL" LEAVES THE SEAT EXACTLY AS IT WAS. Re-minting on an
// unreachable instance destroys a credential that works; the recovery for
// one that does not is a -rotate away.
func TestAnUnverifiableCredentialIsLeftAloneWithANote(t *testing.T) {
	t.Parallel()
	f := newInstance()
	sink := newTrackerSink()
	if _, err := run(t, f, plane.Options{Sink: sink}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before := sink.recorded("PLANE_TOKEN_SWE")
	f.forget()
	f.identityFails = true

	res, err := run(t, f, plane.Options{Sink: sink})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Rotated) != 0 || len(res.Kept) != 1 {
		t.Fatalf("rotated %v, kept %v", res.Rotated, res.Kept)
	}
	if sink.recorded("PLANE_TOKEN_SWE") != before {
		t.Error("a credential that could not be checked was replaced")
	}
	if !anyContains(res.Notes, "could not check") {
		t.Errorf("notes = %q", res.Notes)
	}
	if n := f.tokenRevokes(); n != 0 {
		t.Errorf("%d tokens were revoked on an unverifiable seat", n)
	}
}
