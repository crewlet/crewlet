package atlassian_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// recordingSink is a TokenSink that remembers everything, and can be told to
// fail on one variable so a rollback can be exercised.
type recordingSink struct {
	mu sync.Mutex
	// values is what the sink holds, seeded to stand in for a previous run.
	values map[string]string
	// written is what THIS run recorded, in order.
	written []string
	// failOn makes Record refuse one variable, which is the window where
	// the only copy of a live credential is in the process's memory.
	failOn string
	// beforeFail runs just before that refusal, so a test can cancel the
	// run's context at the exact moment it decides to roll back.
	beforeFail func()
	// readErr makes Value unreadable, which must never be read as absent.
	readErr error
	// discarded records that a rollback ran, and discardErr makes it fail.
	discarded  bool
	discardErr error
	flushed    bool
	// flushErr fails the flush, which is the one way a run that minted
	// nothing still reaches a rollback.
	flushErr error
	// ctxDone captures whether the context Discard was called with was
	// already cancelled — a rollback that inherited a dead context would
	// revoke nothing at all.
	ctxDone bool
}

func newSink() *recordingSink { return &recordingSink{values: map[string]string{}} }

func (s *recordingSink) Record(_ context.Context, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == s.failOn {
		if s.beforeFail != nil {
			s.beforeFail()
		}
		return errors.New("the store refused the write")
	}
	s.values[name] = value
	s.written = append(s.written, name)
	return nil
}

func (s *recordingSink) Value(_ context.Context, name string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return "", false, s.readErr
	}
	value := s.values[name]
	return value, value != "", nil
}

func (s *recordingSink) Discard(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discarded = true
	s.ctxDone = ctx.Err() != nil
	for _, name := range s.written {
		delete(s.values, name)
	}
	s.written = nil
	return s.discardErr
}

func (s *recordingSink) Flush(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushed = true
	return s.flushErr
}

func (s *recordingSink) Describe() string { return "a test sink" }
func (s *recordingSink) NextStep() string { return "do nothing" }

func (s *recordingSink) held(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[name]
}

func (s *recordingSink) writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.written)
}

// harness builds a company, a fake organization and a sink, and runs one pass.
type harness struct {
	fake *fakeOrg
	sink *recordingSink
	plan *atlassian.Plan
	opts atlassian.Options
}

func newHarness(t *testing.T, roles []*org.Role, products ...atlassian.Product) *harness {
	t.Helper()
	if len(products) == 0 {
		products = bothProducts
	}
	o := &org.Organization{Name: "Acme", Roles: roles}
	o.Normalize()
	plan, err := atlassian.PlanFor(o, &config.Atlassian{OrgID: fakeOrgID}, products)
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakeOrg(t)
	sink := newSink()
	sites := map[atlassian.Product]string{}
	containers := map[atlassian.Product][]string{}
	for _, product := range products {
		sites[product] = fakeCloudID
		containers[product] = atlassian.ContainersOf(o, product)
	}
	return &harness{
		fake: fake, sink: sink, plan: plan,
		opts: atlassian.Options{
			Admin: fake.admin(t), OrgID: fakeOrgID, Plan: plan, Sink: sink,
			SiteOf: sites, Containers: containers,
			SiteURL:           "https://acme.atlassian.net",
			DisplayNamePrefix: "Crewlet",
			TokenLifetime:     300 * 24 * time.Hour,
			Now:               func() time.Time { return time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC) },
		},
	}
}

func (h *harness) run(t *testing.T, tune ...func(*atlassian.Options)) (*atlassian.Result, error) {
	t.Helper()
	opts := h.opts
	for _, f := range tune {
		f(&opts)
	}
	return atlassian.Reconcile(context.Background(), opts)
}

// oneSeat is the company most cases use: one agent, both products, one
// project and one space.
func oneSeat() []*org.Role {
	return []*org.Role{{
		Name: "Agent SWE", MCPEnv: seatEnv("SWE"),
		JiraProject: "ENG", ConfluenceSpace: "ENG",
	}}
}

func TestAFirstRunCreatesMintsAndLicensesTheSeat(t *testing.T) {
	t.Parallel()
	h := newHarness(t, oneSeat())
	res, err := h.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Created, []string{"agent-swe"}) {
		t.Errorf("created = %v", res.Created)
	}
	if !slices.Equal(res.Rotated, []string{"agent-swe"}) {
		t.Errorf("rotated = %v", res.Rotated)
	}
	account := h.fake.byDisplayName("Crewlet Agent SWE")
	if account == nil {
		t.Fatal("no account was created under the prefixed display name")
	}
	for _, product := range bothProducts {
		if !account.licensed[product] {
			t.Errorf("the seat holds no %s licence", product)
		}
	}
	// THE ADDRESS IS RECORDED TOO, and Crewlet never chose it: Atlassian
	// assigns a service account's address, and Cloud authenticates Basic
	// base64(email:token), so a seat holding the token alone
	// authenticates as nobody.
	if got := h.sink.held("ATLASSIAN_EMAIL_SWE"); got != account.Email {
		t.Errorf("recorded address = %q, want Atlassian's own %q", got, account.Email)
	}
	if h.sink.held("ATLASSIAN_TOKEN_SWE") == "" {
		t.Error("no credential was recorded")
	}
	if res.Recorded != 2 {
		t.Errorf("Recorded = %d, want the address and the token", res.Recorded)
	}
	if !h.sink.flushed {
		t.Error("the sink was never flushed")
	}
}

func TestARerunOverAHealthyCompanyWritesNothing(t *testing.T) {
	t.Parallel()
	// THE PROPERTY THAT MAKES THE COMMAND SAFE TO RE-RUN. A run that
	// minted every time would be an outage: the engine is running with the
	// OLD value, so rotating revokes the credential every agent is
	// currently authenticating with, and an operator adding a tenth seat
	// takes the other nine down.
	h := newHarness(t, oneSeat())
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	firstWrites := h.sink.writes()
	mints := h.fake.asked("POST /users/")

	res, err := h.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Kept, []string{"agent-swe"}) {
		t.Fatalf("kept = %v, rotated = %v", res.Kept, res.Rotated)
	}
	if got := h.sink.writes(); got != firstWrites {
		t.Errorf("the second run wrote %d value(s), want none", got-firstWrites)
	}
	if got := h.fake.asked("POST /users/"); got != mints {
		t.Errorf("the second run minted %d credential(s), want none", got-mints)
	}
	if res.Recorded != 0 {
		t.Errorf("Recorded = %d on a no-op run, so the report would tell the "+
			"operator to restart for nothing", res.Recorded)
	}
}

func TestAnAccountTheOrganizationAlreadyHadIsAdoptedRatherThanDuplicated(t *testing.T) {
	t.Parallel()
	// Atlassian assigns the account id and the address, so neither can be
	// derived from the org chart the way a GitLab username is. The display
	// name is the only field both sides control, which makes it the join —
	// matched the way somebody comparing them by eye would.
	h := newHarness(t, oneSeat())
	existing := h.fake.seed("crewlet   AGENT swe", bothProducts...)

	res, err := h.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Adopted, []string{"agent-swe"}) {
		t.Fatalf("adopted = %v, created = %v", res.Adopted, res.Created)
	}
	if len(res.Created) != 0 {
		t.Errorf("%v was created despite a match", res.Created)
	}
	created := h.fake.askedExact("POST",
		"/admin/account-management/v1/orgs/"+fakeOrgID+"/service-accounts")
	if created != 0 {
		t.Errorf("%d account(s) were created despite a match", created)
	}
	// An adopted account still needs its OWN credential: Crewlet cannot
	// read a token an operator made earlier, only mint one it can record.
	if h.sink.held("ATLASSIAN_TOKEN_SWE") == "" {
		t.Error("an adopted account was left with no credential")
	}
	if h.sink.held("ATLASSIAN_EMAIL_SWE") != existing.Email {
		t.Error("the adopted account's own address was not recorded")
	}
}

func TestACredentialThatAuthenticatesAsSomebodyElseStopsTheRun(t *testing.T) {
	t.Parallel()
	// A COPY-PASTED VARIABLE. Minting over it hands this seat a second
	// identity while whoever else holds the value keeps authenticating as
	// one account from two places, and nothing anywhere reports it.
	h := newHarness(t, oneSeat())
	mine := h.fake.seed("Crewlet Agent SWE", bothProducts...)
	stranger := h.fake.seed("Somebody Else", bothProducts...)
	h.sink.values["ATLASSIAN_TOKEN_SWE"] = h.fake.mint(stranger, "theirs-1")
	h.sink.values["ATLASSIAN_EMAIL_SWE"] = stranger.Email

	_, err := h.run(t)
	if err == nil {
		t.Fatal("a credential belonging to another account was accepted")
	}
	if !strings.Contains(err.Error(), "different account") {
		t.Errorf("err = %v", err)
	}
	if len(mine.tokens) != 0 {
		t.Error("a credential was minted before the run stopped")
	}
}

func TestAnUnreadableSinkStopsTheRunRatherThanRotatingEverything(t *testing.T) {
	t.Parallel()
	// UNREADABLE IS NOT ABSENT. Reading a store failure as "this seat has
	// no credential" would rotate every live credential in the company
	// because the store blinked — which is the outage the whole probe
	// exists to prevent, arriving through the failure path instead.
	h := newHarness(t, oneSeat())
	h.fake.seed("Crewlet Agent SWE", bothProducts...)
	h.sink.readErr = errors.New("the store is unreachable")

	if _, err := h.run(t); err == nil {
		t.Fatal("an unreadable sink was read as an empty one")
	}
	if got := h.fake.asked("POST /users/"); got != 0 {
		t.Errorf("%d credential(s) were minted against an unreadable sink", got)
	}
}

func TestACredentialThatCannotBeCheckedIsLeftAloneWithANote(t *testing.T) {
	t.Parallel()
	// Re-minting on "cannot tell" destroys a credential that works. The
	// recovery for one that does not is a -rotate away, and the note says
	// so — a silent keep would leave an operator with a seat that 401s and
	// a report that says everything is fine.
	h := newHarness(t, oneSeat())
	account := h.fake.seed("Crewlet Agent SWE", bothProducts...)
	h.sink.values["ATLASSIAN_TOKEN_SWE"] = h.fake.mint(account, "crewlet-agent-swe-1")
	h.sink.values["ATLASSIAN_EMAIL_SWE"] = account.Email
	// A 5xx from the product, which is NOT a refusal — a credential the
	// product rejects answers 401, and the two must never be conflated.
	h.fake.identityStatus = 503

	res, err := h.run(t)
	if err != nil {
		t.Fatalf("an unreachable site failed the run: %v", err)
	}
	if !slices.Equal(res.Kept, []string{"agent-swe"}) {
		t.Fatalf("kept = %v, rotated = %v", res.Kept, res.Rotated)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "-rotate") {
		t.Errorf("the note does not name the recovery:\n%v", res.Notes)
	}
}

func TestASeatThatGainedAProductIsReMintedBecauseItsCredentialIsRefused(t *testing.T) {
	t.Parallel()
	// SCOPE DRIFT, DETECTED BEHAVIOURALLY. A token's scopes cannot be read
	// back from Atlassian at all, so a seat that has gained Confluence
	// holds a Jira-only credential that looks perfectly healthy — and only
	// its first real Confluence call fails. Exercising the credential
	// against every product the seat is enabled for turns that into a
	// refusal here, which re-mints with the scopes the seat has now.
	h := newHarness(t, oneSeat())
	account := h.fake.seed("Crewlet Agent SWE", atlassian.ProductJira)
	// A JIRA-ONLY CREDENTIAL on a seat the config now gives both products.
	// Nothing about it looks wrong: the account is active, the credential
	// authenticates, and only its first real Confluence call fails.
	h.sink.values["ATLASSIAN_TOKEN_SWE"] = h.fake.mint(account,
		"crewlet-agent-swe-1", atlassian.ProductJira)
	h.sink.values["ATLASSIAN_EMAIL_SWE"] = account.Email

	res, err := h.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Rotated, []string{"agent-swe"}) {
		t.Fatalf("rotated = %v, kept = %v", res.Rotated, res.Kept)
	}
	if !account.licensed[atlassian.ProductConfluence] {
		t.Error("the missing licence was not granted")
	}
}

func TestAFailedRecordRevokesEveryCredentialTheRunMinted(t *testing.T) {
	t.Parallel()
	// Between Atlassian minting a credential and the sink recording it
	// there is a window where the only copy is in this process's memory.
	// If recording fails, the credential exists, nothing can use it, and
	// nobody knows to remove it.
	h := newHarness(t, oneSeat())
	h.sink.failOn = "ATLASSIAN_TOKEN_SWE"

	_, err := h.run(t)
	if err == nil {
		t.Fatal("a failed record did not fail the run")
	}
	// The account was this run's, so the cleanup DELETED it rather than
	// revoking a credential on it — and the message has to say which,
	// because they leave the organization in different states.
	if !strings.Contains(err.Error(), "service account(s) this run created were deleted") {
		t.Errorf("the error does not say what the cleanup did: %v", err)
	}
	// The account was created by THIS run, so it is deleted whole — that
	// takes its credentials with it and frees the billable licence, and it
	// owns nothing because nothing has used it.
	if h.fake.byDisplayName("Crewlet Agent SWE") != nil {
		t.Error("the account this run created was left behind")
	}
	if !h.sink.discarded {
		t.Error("the sink was not discarded")
	}
	if h.sink.held("ATLASSIAN_EMAIL_SWE") != "" {
		t.Error("a value this run recorded survived the rollback")
	}
}

func TestARollbackOnAnAdoptedAccountRevokesOneCredentialAndDeletesNothing(t *testing.T) {
	t.Parallel()
	// An adopted account owns issues, is a watcher, and appears in
	// history. Deleting it to undo a failed run would rewrite all of that
	// — so exactly the one credential this run minted is revoked.
	h := newHarness(t, oneSeat())
	account := h.fake.seed("Crewlet Agent SWE", bothProducts...)
	existing := h.fake.mint(account, "somebody-elses-token")
	h.sink.failOn = "ATLASSIAN_TOKEN_SWE"

	if _, err := h.run(t); err == nil {
		t.Fatal("a failed record did not fail the run")
	}
	if h.fake.byDisplayName("Crewlet Agent SWE") == nil {
		t.Fatal("AN ADOPTED ACCOUNT WAS DELETED")
	}
	if len(account.tokens) != 1 {
		t.Fatalf("the account holds %d credential(s), want only the one this "+
			"run did not mint", len(account.tokens))
	}
	for _, value := range account.values {
		if value != existing {
			t.Error("the credential this run minted was left live")
		}
	}
}

func TestARollbackThatCannotFinishNamesWhatIsStillLive(t *testing.T) {
	t.Parallel()
	// Best effort is not silence: an operator finishing by hand needs the
	// list, and swallowing it leaves live credentials nobody knows about.
	h := newHarness(t, oneSeat())
	h.sink.failOn = "ATLASSIAN_TOKEN_SWE"
	h.sink.discardErr = errors.New("the store could not remove ATLASSIAN_EMAIL_SWE")

	_, err := h.run(t)
	if err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(err.Error(), "CLEANUP DID NOT FINISH") {
		t.Errorf("the failure is not flagged:\n%v", err)
	}
	if !strings.Contains(err.Error(), "ATLASSIAN_EMAIL_SWE") {
		t.Errorf("the error does not name what is still live:\n%v", err)
	}
	// THE ORIGINAL CAUSE IS ALWAYS WRAPPED, never replaced by the cleanup
	// problem — the cleanup is a consequence, and an operator debugging
	// the wrong one loses the thread.
	if !strings.Contains(err.Error(), "the store refused the write") {
		t.Errorf("the original cause was lost:\n%v", err)
	}
}

func TestARollbackDoesNotInheritACancelledContext(t *testing.T) {
	t.Parallel()
	// The failure being undone is often the cancellation itself, so a
	// cleanup that inherited a dead context would do nothing at all — and
	// every credential the run minted would stay live, recorded nowhere.
	h := newHarness(t, oneSeat())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The cancellation lands at the exact moment the run decides to roll
	// back, which is the ordering that matters: a rollback that inherited
	// this context would revoke nothing and report success at doing it.
	h.sink.failOn = "ATLASSIAN_TOKEN_SWE"
	h.sink.beforeFail = cancel

	_, err := atlassian.Reconcile(ctx, h.opts)
	if err == nil {
		t.Fatal("no error")
	}
	if !h.sink.discarded {
		t.Fatal("the sink was never discarded")
	}
	if h.sink.ctxDone {
		t.Fatal("the rollback ran on a cancelled context, so it would revoke nothing")
	}
}

func TestASupersededCredentialIsRetiredAndAnAdministratorsIsNot(t *testing.T) {
	t.Parallel()
	// A credential Crewlet has stopped using does not stop working: it
	// stays valid until its expiry, and an account re-provisioned a few
	// times collects a drawer of live credentials nobody can account for.
	// Only the ones this tool minted for this seat are touched — an
	// administrator's own would break whatever is using it, silently.
	h := newHarness(t, oneSeat())
	account := h.fake.seed("Crewlet Agent SWE", bothProducts...)
	h.fake.mint(account, atlassian.TokenLabel("agent-swe")+"-1700000000")
	h.fake.mint(account, "an-administrators-own-token")

	res, err := h.run(t, func(o *atlassian.Options) { o.Rotate = true })
	if err != nil {
		t.Fatal(err)
	}
	if res.Retired != 1 {
		t.Fatalf("retired = %d, want exactly the one this tool had minted", res.Retired)
	}
	labels := h.fake.labels(account)
	if !slices.Contains(labels, "an-administrators-own-token") {
		t.Errorf("an administrator's own credential was revoked: %v", labels)
	}
	if len(labels) != 2 {
		t.Errorf("the account holds %v, want the fresh one and the administrator's", labels)
	}
}

func TestRotateMintsForASeatWhoseCredentialStillWorks(t *testing.T) {
	t.Parallel()
	h := newHarness(t, oneSeat())
	account := h.fake.seed("Crewlet Agent SWE", bothProducts...)
	h.sink.values["ATLASSIAN_TOKEN_SWE"] = h.fake.mint(account, atlassian.TokenLabel("agent-swe")+"-1")
	h.sink.values["ATLASSIAN_EMAIL_SWE"] = account.Email

	res, err := h.run(t, func(o *atlassian.Options) { o.Rotate = true })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Rotated, []string{"agent-swe"}) {
		t.Fatalf("rotated = %v, kept = %v", res.Rotated, res.Kept)
	}
}

func TestAFreshLicenceIsReportedAsStartingRatherThanBroken(t *testing.T) {
	t.Parallel()
	// Atlassian applies a product licence ASYNCHRONOUSLY and has taken
	// minutes over it, so a permission check right after a grant
	// legitimately fails. Reporting that as a fault would put one on every
	// freshly provisioned agent, once, every time.
	h := newHarness(t, oneSeat())
	// The invite is ACCEPTED and applies nothing, and the product plane
	// refuses an unlicensed account: exactly the window Atlassian leaves
	// open between granting a licence and applying it.
	h.fake.unlicensedIsRefused = true
	h.fake.licenceNeverLands = true

	res, err := h.run(t, func(o *atlassian.Options) {
		o.SiteOf = map[atlassian.Product]string{atlassian.ProductJira: fakeCloudID}
		o.Containers = map[atlassian.Product][]string{atlassian.ProductJira: {"ENG"}}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Access) != 1 {
		t.Fatalf("%d access report(s): %+v", len(res.Access), res.Access)
	}
	report := res.Access[0]
	if !report.Pending {
		t.Fatalf("a licence granted moments ago is reported as broken: %+v", report)
	}
	if report.Reason != "" {
		t.Errorf("reason = %q, want a pending report to carry none", report.Reason)
	}
}

func TestAnAgentThatCannotReachItsProductNeverReportsClean(t *testing.T) {
	t.Parallel()
	// A licence that never lands stays reported, run after run. Which
	// wording it gets is the pending rule's business — the credential is
	// refused, so the next run re-grants and the report is "still
	// starting" again, which is what keeps a hand-revoked licence
	// repairable. What must never happen is the report going QUIET: an
	// agent that cannot reach its own project is the one thing this check
	// exists to say out loud.
	h := newHarness(t, oneSeat())
	h.fake.unlicensedIsRefused = true
	h.fake.licenceNeverLands = true
	only := func(o *atlassian.Options) {
		o.SiteOf = map[atlassian.Product]string{atlassian.ProductJira: fakeCloudID}
		o.Containers = map[atlassian.Product][]string{atlassian.ProductJira: {"ENG"}}
	}
	if _, err := h.run(t, only); err != nil {
		t.Fatal(err)
	}
	res, err := h.run(t, only)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Access) != 1 {
		t.Fatalf("%d access report(s): %+v", len(res.Access), res.Access)
	}
	if res.Access[0].OK() {
		t.Fatalf("an agent that cannot reach its own project reported clean: %+v", res.Access[0])
	}
}

func TestMissingAndExcessAreReportedApartWithSomewhereToGo(t *testing.T) {
	t.Parallel()
	// They call for OPPOSITE responses — missing is the operator's to
	// grant, excess is only theirs to revoke — and neither is a reason to
	// touch the credential.
	h := newHarness(t, oneSeat())
	account := h.fake.seed("Crewlet Agent SWE", bothProducts...)
	h.sink.values["ATLASSIAN_TOKEN_SWE"] = h.fake.mint(account, atlassian.TokenLabel("agent-swe")+"-1")
	h.sink.values["ATLASSIAN_EMAIL_SWE"] = account.Email

	held := map[string]bool{}
	for _, name := range atlassian.Allowed(atlassian.ProductJira) {
		held[name] = true
	}
	held["CREATE_ISSUES"] = false
	held["DELETE_ISSUES"] = true
	h.fake.grants(account, atlassian.ProductJira, "ENG", held)

	res, err := h.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Kept, []string{"agent-swe"}) {
		t.Fatalf("a permission finding re-minted the credential: rotated = %v", res.Rotated)
	}
	var found bool
	for _, report := range res.Access {
		if report.Product != atlassian.ProductJira || report.Container != "ENG" {
			continue
		}
		found = true
		if !slices.Equal(report.Missing, []string{"CREATE_ISSUES"}) {
			t.Errorf("missing = %v", report.Missing)
		}
		if !slices.Equal(report.Excess, []string{"DELETE_ISSUES"}) {
			t.Errorf("excess = %v", report.Excess)
		}
		// The settings link is resolved only when there is something to
		// fix, and it names the screen rather than the site root.
		if !strings.Contains(report.SettingsURL, "/projects/ENG/settings/access") {
			t.Errorf("settings url = %q", report.SettingsURL)
		}
		if report.SettingsStyle == "" {
			t.Error("no container style, so the advice cannot say which screen this is")
		}
	}
	if !found {
		t.Fatalf("no access report for ENG: %+v", res.Access)
	}
}

func TestACleanContainerCostsNoSettingsLookup(t *testing.T) {
	t.Parallel()
	// A clean container has nobody to send anywhere, and asking anyway
	// would spend one request per container per run to build a link
	// nothing prints.
	h := newHarness(t, oneSeat())
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	if got := h.fake.asked("/project/ENG"); got != 0 {
		t.Errorf("%d settings lookup(s) on a clean company", got)
	}
}

func TestDecommissionIsScopedByThePrefixAndRefusesWithoutOne(t *testing.T) {
	t.Parallel()
	// The prefix is the WHOLE safety property: it is what marks an account
	// as this company's, in an organization that also holds people.
	h := newHarness(t, oneSeat())
	h.fake.seed("Crewlet Agent SWE", bothProducts...)
	h.fake.seed("Crewlet Departed Agent", bothProducts...)
	h.fake.seed("A Real Person", bothProducts...)

	res, err := h.run(t, func(o *atlassian.Options) { o.Decommission = true })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Decommissioned, []string{"Crewlet Departed Agent"}) {
		t.Fatalf("decommissioned = %v", res.Decommissioned)
	}
	if h.fake.byDisplayName("A Real Person") == nil {
		t.Fatal("AN ACCOUNT OUTSIDE THE PREFIX WAS DELETED")
	}
	if h.fake.byDisplayName("Crewlet Agent SWE") == nil {
		t.Fatal("a seat still in the config was decommissioned")
	}
}

func TestDecommissionWithNoPrefixRefusesRatherThanSweeping(t *testing.T) {
	t.Parallel()
	h := newHarness(t, oneSeat())
	h.fake.seed("Some Account", bothProducts...)

	res, err := h.run(t, func(o *atlassian.Options) {
		o.Decommission, o.DisplayNamePrefix = true, "  "
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Decommissioned) != 0 {
		t.Fatalf("a blank prefix deleted %v", res.Decommissioned)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "-decommission was skipped") {
		t.Errorf("the refusal was silent:\n%v", res.Notes)
	}
}

func TestHandlesNarrowsProvisioningAndNotTheKeepSet(t *testing.T) {
	t.Parallel()
	// -handles says which seats to PROVISION. Reading it in the sweep as
	// well would make `-handles a -decommission` delete every other seat's
	// account, which is the opposite of narrowing a run.
	roles := []*org.Role{
		{Name: "Agent One", MCPEnv: seatEnv("ONE"), JiraProject: "ENG", ConfluenceSpace: "ENG"},
		{Name: "Agent Two", MCPEnv: seatEnv("TWO"), JiraProject: "ENG", ConfluenceSpace: "ENG"},
	}
	h := newHarness(t, roles)
	h.fake.seed("Crewlet Agent Two", bothProducts...)

	res, err := h.run(t, func(o *atlassian.Options) {
		o.Only = []string{"agent-one"}
		o.Decommission = true
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Created, []string{"agent-one"}) {
		t.Errorf("created = %v, want only the named seat", res.Created)
	}
	if len(res.Decommissioned) != 0 {
		t.Fatalf("-handles narrowed the keep-set and deleted %v", res.Decommissioned)
	}
}

func TestAFullOrganizationStopsTheRunRatherThanMintingCredentialsNobodyCanUse(t *testing.T) {
	t.Parallel()
	h := newHarness(t, oneSeat())
	h.fake.quotaFull = true

	_, err := h.run(t)
	if !errors.Is(err, atlassian.ErrQuotaExceeded) {
		t.Fatalf("err = %v, want ErrQuotaExceeded", err)
	}
}

func TestASiteWithNoCloudIDIsANoteRatherThanAFailedRun(t *testing.T) {
	t.Parallel()
	// A product the config half-declares must not cost the seat its other
	// product's credential.
	h := newHarness(t, oneSeat())
	res, err := h.run(t, func(o *atlassian.Options) {
		o.SiteOf = map[atlassian.Product]string{atlassian.ProductJira: fakeCloudID}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rotated) != 1 {
		t.Fatalf("rotated = %v", res.Rotated)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "no cloud_id for Confluence") {
		t.Errorf("notes = %v", res.Notes)
	}
}

func TestReconcileRefusesTheInputsThatWouldStrandCredentials(t *testing.T) {
	t.Parallel()
	// Refused UP FRONT rather than discovered after the first credential:
	// a run with no sink would mint live credentials at Atlassian and
	// print none of them, and every one would have to be found by hand.
	h := newHarness(t, oneSeat())
	base := h.opts
	base.Sink = nil
	if _, err := atlassian.Reconcile(context.Background(), base); !errors.Is(err, atlassianNoSink()) {
		t.Errorf("a run with no sink was accepted: %v", err)
	}
	base = h.opts
	base.Admin = nil
	if _, err := atlassian.Reconcile(context.Background(), base); err == nil {
		t.Error("a run with no admin client was accepted")
	}
	base = h.opts
	base.OrgID = " "
	if _, err := atlassian.Reconcile(context.Background(), base); err == nil {
		t.Error("a run with no organization id was accepted")
	}
}

func TestAnEmptyPlanDoesNothingAndSaysSo(t *testing.T) {
	t.Parallel()
	h := newHarness(t, []*org.Role{{Name: "Agent Chat"}})
	res, err := h.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created)+len(res.Rotated)+len(res.Adopted) != 0 {
		t.Fatalf("an empty plan did something: %+v", res)
	}
	if h.fake.asked("service-accounts") != 0 {
		t.Error("an empty plan still listed the organization")
	}
}

// atlassianNoSink is provision.ErrNoSink, reached without importing the
// package into every test that needs it.
func atlassianNoSink() error {
	_, err := atlassian.Reconcile(context.Background(), atlassian.Options{
		Admin: &atlassian.AdminClient{}, OrgID: "o", Plan: &atlassian.Plan{},
	})
	return err
}

var _ = fmt.Sprint

func TestASteadyStateRerunGrantsNoLicence(t *testing.T) {
	t.Parallel()
	// A licence grant is idempotent, so sending one per product per run
	// looks free. It is a write per product per seat forever — and, worse,
	// it makes every licence look freshly granted, which turns a
	// genuinely inaccessible container into "still starting" for ever.
	h := newHarness(t, oneSeat())
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	first := h.fake.asked("/service-accounts/invite")
	if first == 0 {
		t.Fatal("the first run granted no licence at all")
	}

	res, err := h.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.fake.asked("/service-accounts/invite"); got != first {
		t.Errorf("the second run sent %d licence grant(s), want none", got-first)
	}
	if len(res.Licensed) != 0 {
		t.Errorf("a run that bought nothing reported %v", res.Licensed)
	}
}

func TestAContainerTheAgentCannotSeeIsAnErrorOnceItsLicenceIsNotFresh(t *testing.T) {
	t.Parallel()
	// THE ONE PERMANENT FAILURE THE REPORT EXISTS TO SURFACE. A mistyped
	// project key is almost always a typo, and reporting it as a licence
	// that has not propagated yet — on run 1, run 2 and run 500 — is the
	// one answer that never leads anywhere.
	h := newHarness(t, oneSeat())
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	// The container the org chart names does not exist on the site. On the
	// first run its licence was fresh, so "still starting" was the honest
	// answer; on this one it is not.
	res, err := h.run(t, func(o *atlassian.Options) {
		o.Containers = map[atlassian.Product][]string{
			atlassian.ProductJira: {"TYPO"},
		}
		o.SiteOf = map[atlassian.Product]string{atlassian.ProductJira: fakeCloudID}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Access) != 1 {
		t.Fatalf("%d access report(s): %+v", len(res.Access), res.Access)
	}
	report := res.Access[0]
	if report.Pending {
		t.Fatalf("an unreachable container is still reported as starting: %+v", report)
	}
	// NAMED, not merely not-pending: a clean report would also satisfy
	// "not pending", and a typo that reports as healthy is the failure
	// this whole check exists to catch.
	if report.Reason == "" {
		t.Fatalf("a container that does not exist reported clean: %+v", report)
	}
	if !strings.Contains(report.Reason, "cannot see it") {
		t.Errorf("reason = %q, want it to name what an operator should look at", report.Reason)
	}
}

func TestALicenceRevokedByHandIsStillRepaired(t *testing.T) {
	t.Parallel()
	// The evidence-driven grant must not lose the repair: an operator who
	// removed an agent's product access in the console gets it back,
	// because the agent's own credential is then refused on that product
	// and the probe reports the licence owed.
	h := newHarness(t, oneSeat())
	h.fake.unlicensedIsRefused = true
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	account := h.fake.byDisplayName("Crewlet Agent SWE")
	h.fake.mu.Lock()
	delete(account.licensed, atlassian.ProductConfluence)
	h.fake.mu.Unlock()

	res, err := h.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if !account.licensed[atlassian.ProductConfluence] {
		t.Fatal("a licence revoked by hand was not granted again")
	}
	if got := res.Licensed["agent-swe"]; len(got) != 1 || got[0] != "Confluence" {
		t.Errorf("licensed = %v, want exactly the one that was owed", got)
	}
}

func TestDecommissionKeepsASeatThatIsInTheChartButNotInThePlan(t *testing.T) {
	t.Parallel()
	// THE IRREVERSIBLE ONE. A seat that opted out of every product, or
	// whose credential is managed by hand, is not in the plan — and may
	// hold an account an earlier run created. Sweeping on the plan deletes
	// an identity that owns issues because somebody edited a products
	// list, and Atlassian has no restore.
	roles := append(oneSeat(),
		&org.Role{Name: "Opted Out", MCPEnv: seatEnv("OUT"), AtlassianProducts: []string{}},
		&org.Role{Name: "By Hand", MCPEnv: org.MCPEnv{"atlassian": {
			"JIRA_API_TOKEN": "a-literal", "JIRA_USERNAME": "x@example.com",
		}}},
	)
	h := newHarness(t, roles)
	h.fake.seed("Crewlet Opted Out", bothProducts...)
	h.fake.seed("Crewlet By Hand", bothProducts...)
	h.fake.seed("Crewlet Departed Agent", bothProducts...)

	res, err := h.run(t, func(o *atlassian.Options) { o.Decommission = true })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Decommissioned, []string{"Crewlet Departed Agent"}) {
		t.Fatalf("decommissioned = %v, want only the seat that left the chart",
			res.Decommissioned)
	}
	for _, name := range []string{"Crewlet Opted Out", "Crewlet By Hand"} {
		if h.fake.byDisplayName(name) == nil {
			t.Fatalf("%q IS IN THE ORG CHART AND ITS ACCOUNT WAS DELETED", name)
		}
	}
}

func TestAnAccountWithNoAddressFailsTheSeatRatherThanRecordingHalfACredential(t *testing.T) {
	t.Parallel()
	// Cloud authenticates Basic base64(email:token); with an empty address
	// the header falls back to a bearer token, which Cloud rejects. A seat
	// provisioned that way reports as successfully rotated and cannot
	// authenticate at all.
	h := newHarness(t, oneSeat())
	account := h.fake.seed("Crewlet Agent SWE", bothProducts...)
	h.fake.mu.Lock()
	account.Email = ""
	h.fake.mu.Unlock()

	_, err := h.run(t)
	if err == nil {
		t.Fatal("an account with no address was provisioned")
	}
	if !strings.Contains(err.Error(), "no email address") {
		t.Errorf("err = %v", err)
	}
	if h.sink.held("ATLASSIAN_TOKEN_SWE") != "" || h.sink.held("ATLASSIAN_EMAIL_SWE") != "" {
		t.Error("half a credential was recorded and left behind")
	}
}

func TestRotateStillStopsOnACredentialBelongingToAnotherAccount(t *testing.T) {
	t.Parallel()
	// -rotate means "mint for every seat", not "skip every check". Minting
	// over a variable that authenticates as a different account hands one
	// account two seats' identities and leaves whoever else holds the
	// value authenticating as one account from two places — and nothing
	// anywhere reports it, however the run was invoked.
	h := newHarness(t, oneSeat())
	mine := h.fake.seed("Crewlet Agent SWE", bothProducts...)
	stranger := h.fake.seed("Somebody Else", bothProducts...)
	h.sink.values["ATLASSIAN_TOKEN_SWE"] = h.fake.mint(stranger, "theirs-1")
	h.sink.values["ATLASSIAN_EMAIL_SWE"] = stranger.Email

	_, err := h.run(t, func(o *atlassian.Options) { o.Rotate = true })
	if err == nil {
		t.Fatal("-rotate minted over a credential belonging to another account")
	}
	if !strings.Contains(err.Error(), "different account") {
		t.Errorf("err = %v", err)
	}
	if len(mine.tokens) != 0 {
		t.Error("a credential was minted before the run stopped")
	}
}

func TestRotateDoesNotRebuyALicenceTheAgentAlreadyHolds(t *testing.T) {
	t.Parallel()
	// The probe still runs under -rotate, so its licence evidence
	// survives: a rotation mints fresh credentials without re-buying
	// every product, and without reporting every unreachable container as
	// a licence that has not propagated yet.
	h := newHarness(t, oneSeat())
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	grants := h.fake.asked("/service-accounts/invite")

	res, err := h.run(t, func(o *atlassian.Options) { o.Rotate = true })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Rotated, []string{"agent-swe"}) {
		t.Fatalf("rotated = %v, kept = %v", res.Rotated, res.Kept)
	}
	if got := h.fake.asked("/service-accounts/invite"); got != grants {
		t.Errorf("-rotate re-bought %d licence(s)", got-grants)
	}
}

func TestASecondRotationRetiresTheCredentialItReplaces(t *testing.T) {
	t.Parallel()
	// A credential Crewlet has stopped using does not stop working: it
	// stays valid until its expiry. [retirePrevious] finds the ones it
	// replaces by the seat's label PREFIX, so the label a mint sends has to
	// be a name UNDER that prefix rather than the prefix itself — an
	// unstamped label is not a candidate for retirement, and every rotation
	// then leaves its predecessor live on an account nobody is auditing.
	h := newHarness(t, oneSeat())
	first, err := h.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if first.Retired != 0 {
		t.Fatalf("retired = %d on a run that created the account", first.Retired)
	}
	account := h.fake.byDisplayName("Crewlet Agent SWE")
	if account == nil {
		t.Fatal("no account")
	}
	was := h.fake.labels(account)
	if len(was) != 1 {
		t.Fatalf("labels after the first run = %v, want one", was)
	}

	// A LATER CLOCK, because that is what distinguishes one generation of
	// a seat's credential from the next.
	later := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	second, err := h.run(t, func(o *atlassian.Options) {
		o.Rotate = true
		o.Now = func() time.Time { return later }
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Retired != 1 {
		t.Fatalf("retired = %d, want the credential the rotation replaced", second.Retired)
	}
	now := h.fake.labels(account)
	if len(now) != 1 {
		t.Fatalf("the account holds %v, want only the fresh credential", now)
	}
	if now[0] == was[0] {
		t.Errorf("both generations were labelled %q, so nothing tells them apart", now[0])
	}
	if !strings.HasPrefix(now[0], atlassian.TokenLabel("agent-swe")+"-") {
		t.Errorf("label = %q, want a name under this seat's retire prefix", now[0])
	}
}

func TestAMintWithNoHandleOnItIsRevokedByTheLabelTheRunSent(t *testing.T) {
	t.Parallel()
	// A credential whose response carried neither a value nor an id still
	// EXISTS on the account. Nothing recorded what it is and nothing holds
	// an id for it — the only handle left is the label the run sent. So the
	// label the run remembers has to be exactly the one Atlassian stored,
	// or the cleanup matches nothing while reporting that it revoked
	// everything, and a live credential is left on an agent's account.
	h := newHarness(t, oneSeat())
	// ADOPTED, not created: an account this run made is deleted whole,
	// which takes the credential with it and never exercises this path.
	account := h.fake.seed("Crewlet Agent SWE", bothProducts...)
	h.fake.mintReturnsNothing = true
	h.fake.mintReturnsNoID = true

	_, err := h.run(t)
	if err == nil {
		t.Fatal("a mint with no handle on its credential did not fail the run")
	}
	if labels := h.fake.labels(account); len(labels) != 0 {
		t.Fatalf("the account still holds %v — a live credential nothing recorded", labels)
	}
	if h.fake.byDisplayName("Crewlet Agent SWE") == nil {
		t.Error("an adopted account was deleted; it owns issues and appears in history")
	}
}

func TestAnAccountCreatedBeforeTheFailureIsDeletedRatherThanOrphaned(t *testing.T) {
	t.Parallel()
	// Atlassian MADE the account and then answered without the atlassianId
	// everything downstream keys on. The account is real and billable, and
	// only this run knows it exists — so the rollback has to reach it.
	// Dropping it would also poison every later run: each one matches the
	// orphan by display name, finds no atlassianId, and fails the same seat
	// the same way for ever.
	h := newHarness(t, oneSeat())
	h.fake.createReturnsNoID = true

	_, err := h.run(t)
	if err == nil {
		t.Fatal("an account created without an atlassianId did not fail the run")
	}
	if account := h.fake.byDisplayName("Crewlet Agent SWE"); account != nil {
		t.Fatalf("the account this run created was orphaned upstream: %+v", account)
	}
}

func TestASeatWhoseVariablesDisagreeIsReMintedRatherThanTrusted(t *testing.T) {
	t.Parallel()
	// A seat whose four keys name two variables is ONE credential written
	// twice. A sink holding different values for them is an operator half
	// way through an edit: whichever half the engine reads, the seat's
	// other tool server authenticates with something else. Reading either
	// as "provisioned" leaves that split in place for ever.
	h := newHarness(t, []*org.Role{{
		Name: "Agent SWE",
		MCPEnv: org.MCPEnv{
			"jira": {
				"JIRA_USERNAME":  "${ATLASSIAN_EMAIL_SWE}",
				"JIRA_API_TOKEN": "${JIRA_TOKEN_SWE}",
			},
			"confluence": {
				"CONFLUENCE_USERNAME":  "${ATLASSIAN_EMAIL_SWE}",
				"CONFLUENCE_API_TOKEN": "${CONFLUENCE_TOKEN_SWE}",
			},
		},
		JiraProject: "ENG", ConfluenceSpace: "ENG",
	}})
	account := h.fake.seed("Crewlet Agent SWE", bothProducts...)
	h.sink.values["ATLASSIAN_EMAIL_SWE"] = account.Email
	// BOTH ARE LIVE credentials on the right account, so neither is
	// rejected by Atlassian — only their disagreement says anything.
	h.sink.values["JIRA_TOKEN_SWE"] = h.fake.mint(account, "crewlet-agent-swe-1")
	h.sink.values["CONFLUENCE_TOKEN_SWE"] = h.fake.mint(account, "crewlet-agent-swe-2")

	res, err := h.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Rotated, []string{"agent-swe"}) {
		t.Fatalf("rotated = %v, kept = %v — a split credential was trusted", res.Rotated, res.Kept)
	}
	if h.sink.held("JIRA_TOKEN_SWE") != h.sink.held("CONFLUENCE_TOKEN_SWE") {
		t.Error("the seat still holds two different credentials")
	}
}

func TestDecommissionDoesNotSweepANameThatMerelyStartsWithThePrefix(t *testing.T) {
	t.Parallel()
	// The prefix marks an account as this company's, and it marks a WHOLE
	// WORD: "Crewlet" must not claim "CrewletBot", which is somebody else's
	// integration. Atlassian has no restore, so the boundary is the
	// difference between a sweep and a data loss.
	h := newHarness(t, oneSeat())
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	other := h.fake.seed("CrewletBot Deploy Notifier", bothProducts...)

	res, err := h.run(t, func(o *atlassian.Options) { o.Decommission = true })
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Decommissioned) != 0 {
		t.Fatalf("decommissioned = %v, want nothing", res.Decommissioned)
	}
	if h.fake.byDisplayName(other.DisplayName) == nil {
		t.Error("an account belonging to another integration was deleted")
	}
}

// labels is the labels an account's credentials carry, sorted.
//
// A method on the ORG because the account's map is written by the server's
// own goroutines, and a test that read it directly would be a data race the
// suite reports on a machine that happens to schedule them late.
func (f *fakeOrg) labels(account *fakeAccount) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for _, label := range account.tokens {
		out = append(out, label)
	}
	slices.Sort(out)
	return out
}

func TestALicenceARollbackCannotWithdrawIsNamedApartFromALiveCredential(t *testing.T) {
	t.Parallel()
	// The two call for different things and must not share a banner. A
	// credential the cleanup could not revoke is LIVE and has to be hunted
	// down; a licence this run granted on an account it merely adopted is
	// billable and simply cannot be given back — Atlassian offers no route.
	// Printed together under "these may still be live", the licences read
	// as loose credentials and send an operator looking for a token that
	// does not exist.
	h := newHarness(t, oneSeat())
	// ADOPTED and UNLICENSED, so this run grants a licence on an account it
	// will not delete. An account this run created is deleted whole, which
	// takes its licence with it and has nothing to report.
	h.fake.seed("Crewlet Agent SWE")
	h.sink.failOn = "ATLASSIAN_TOKEN_SWE"

	_, err := h.run(t)
	if err == nil {
		t.Fatal("a failed record did not fail the run")
	}
	msg := err.Error()
	if !strings.Contains(msg, "LICENCES COULD NOT BE WITHDRAWN") {
		t.Fatalf("the licences this run bought are not named:\n%v", err)
	}
	if !strings.Contains(msg, "credential(s) this run minted were revoked") {
		t.Errorf("the credential cleanup is not reported as clean:\n%v", err)
	}
	// The credential DID come back, so nothing may claim otherwise.
	if strings.Contains(msg, "CLEANUP DID NOT FINISH") {
		t.Errorf("a licence was reported as a credential that may still be live:\n%v", err)
	}
	for _, product := range bothProducts {
		if !strings.Contains(msg, product.Label()) {
			t.Errorf("the %s licence is not named:\n%v", product.Label(), err)
		}
	}
}

func TestARollbackWithNothingCreatedYetSaysSoRatherThanCountingZero(t *testing.T) {
	t.Parallel()
	// The run fails before it has made anything: an account it merely
	// adopted, a credential that still works, and a flush the sink
	// refuses. "The 0 credential(s) this run minted were revoked" is a
	// sentence that tells an operator nothing and reads like a bug.
	h := newHarness(t, oneSeat())
	account := h.fake.seed("Crewlet Agent SWE", bothProducts...)
	h.sink.values["ATLASSIAN_TOKEN_SWE"] = h.fake.mint(account, "crewlet-agent-swe-1")
	h.sink.values["ATLASSIAN_EMAIL_SWE"] = account.Email
	h.sink.discardErr = nil
	h.sink.failOn = "nothing-is-recorded-on-a-steady-state-run"

	// A steady-state run records nothing, so the only way to reach a
	// rollback is the flush.
	res, err := h.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Kept, []string{"agent-swe"}) {
		t.Fatalf("kept = %v, rotated = %v", res.Kept, res.Rotated)
	}

	// And the wording itself, exercised through the one path that reaches
	// it with an empty ledger.
	h2 := newHarness(t, oneSeat())
	other := h2.fake.seed("Crewlet Agent SWE", bothProducts...)
	h2.sink.values["ATLASSIAN_TOKEN_SWE"] = h2.fake.mint(other, "crewlet-agent-swe-1")
	h2.sink.values["ATLASSIAN_EMAIL_SWE"] = other.Email
	h2.sink.flushErr = errors.New("the store went away")

	_, err = h2.run(t)
	if err == nil {
		t.Fatal("a failed flush did not fail the run")
	}
	if !strings.Contains(err.Error(), "nothing had been created yet") {
		t.Errorf("the cleanup counted zero rather than saying nothing happened:\n%v", err)
	}
}

func TestARenamedRoleStopsTheRunRatherThanForkingTheAgentsIdentity(t *testing.T) {
	t.Parallel()
	// THE RENAME FORK. A seat's account is joined by display name, which is
	// built from the ROLE NAME — and the org chart makes handles unique, not
	// names. Edit the name and the existing account no longer matches: the
	// run makes a SECOND billable identity and mints into variables that
	// still authenticate as the first, which keeps every issue, watcher and
	// history entry it had plus a live credential nothing will ever revoke,
	// until a later -decommission deletes it for not being in the keep-set.
	// Atlassian has no restore.
	h := newHarness(t, []*org.Role{{
		Name: "Agent SWE II", DeclaredHandle: "agent-swe", MCPEnv: seatEnv("SWE"),
		JiraProject: "ENG", ConfluenceSpace: "ENG",
	}})
	// The account the seat had under its OLD name, still holding the
	// credential the seat's variables point at.
	was := h.fake.seed("Crewlet Agent SWE", bothProducts...)
	h.sink.values["ATLASSIAN_TOKEN_SWE"] = h.fake.mint(was, "crewlet-agent-swe-1")
	h.sink.values["ATLASSIAN_EMAIL_SWE"] = was.Email

	_, err := h.run(t)
	if err == nil {
		t.Fatal("a renamed role forked the agent's identity without a word")
	}
	if !strings.Contains(err.Error(), "different account") {
		t.Errorf("error does not name the cause: %v", err)
	}
	// AND THE SECOND IDENTITY IS GONE. The run created it before the probe
	// answered, so the rollback has to take it back out.
	if h.fake.byDisplayName("Crewlet Agent SWE II") != nil {
		t.Error("the second billable identity was left behind")
	}
	if h.fake.byDisplayName("Crewlet Agent SWE") == nil {
		t.Error("the account that owns the history was touched")
	}
}

func TestAFirstRunStillCostsNoProductCallBeforeItMints(t *testing.T) {
	t.Parallel()
	// The other side of probing a created account: a genuinely new seat has
	// nothing recorded, so the probe answers from the sink alone and the
	// rename guard is free.
	h := newHarness(t, oneSeat())
	res, err := h.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Created, []string{"agent-swe"}) {
		t.Fatalf("created = %v", res.Created)
	}
	// Two identity reads happen AFTER the mint, in the access check. What
	// must not happen is one before it, against a credential that is not
	// there.
	if n := h.fake.asked("/myself"); n > 1 {
		t.Errorf("%d identity call(s) on a seat with nothing recorded", n)
	}
}

func TestASeatThatDroppedAProductIsReMintedWithNarrowerScopes(t *testing.T) {
	t.Parallel()
	// THE HALF THE DOC PROMISED AND THE CODE DID NOT DO. The probe walks
	// the seat's CURRENT products, so a product REMOVED from a seat is by
	// definition not in that list: its credential was never exercised
	// there, never refused, never re-minted — and the agent kept a live
	// write scope on a product its author had taken away, for the whole
	// token lifetime. Atlassian will not report a token's scopes, so the
	// only way to ask is to use it.
	h := newHarness(t, []*org.Role{{
		Name: "Tech Writer", MCPEnv: seatEnv("WRITER"),
		AtlassianProducts: []string{"confluence"},
		JiraProject:       "ENG", ConfluenceSpace: "ENG",
	}})
	account := h.fake.seed("Crewlet Tech Writer", bothProducts...)
	// A credential minted back when this seat had BOTH products.
	h.sink.values["ATLASSIAN_TOKEN_WRITER"] = h.fake.mint(account, "crewlet-tech-writer-1")
	h.sink.values["ATLASSIAN_EMAIL_WRITER"] = account.Email

	res, err := h.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Rotated, []string{"tech-writer"}) {
		t.Fatalf("rotated = %v, kept = %v — an over-scoped credential was trusted",
			res.Rotated, res.Kept)
	}
	// NO LICENCE IS RE-BOUGHT: every product the seat still holds answered.
	if len(res.Licensed) != 0 {
		t.Errorf("licensed = %v, want none owed", res.Licensed)
	}
	// And the note says the licence itself is still billable, because
	// Atlassian offers no route to give one back.
	var said bool
	for _, note := range res.Notes {
		if strings.Contains(note, "still billable") {
			said = true
		}
	}
	if !said {
		t.Errorf("nothing told the operator the dropped licence is still held: %v", res.Notes)
	}
}

func TestAFullOrganizationKeepsTheSeatsItAlreadyProvisioned(t *testing.T) {
	t.Parallel()
	// THE WALL THAT USED TO WIPE THE RUN. The organization has no room for
	// another account. The seats already provisioned are finished and their
	// credentials are recorded — rolling those back would delete billable
	// identities to report a limit, and every re-run would churn the same
	// create-then-delete.
	h := newHarness(t, []*org.Role{
		{Name: "Agent One", MCPEnv: seatEnv("ONE"), JiraProject: "ENG", ConfluenceSpace: "ENG"},
		{Name: "Agent Two", MCPEnv: seatEnv("TWO"), JiraProject: "ENG", ConfluenceSpace: "ENG"},
	})
	h.fake.fullAfter = 1

	res, err := h.run(t)
	if err == nil {
		t.Fatal("a full organization was not reported")
	}
	if res == nil {
		t.Fatal("no result, so the report prints nothing and the operator " +
			"cannot tell which seats are done")
	}
	if !slices.Equal(res.Created, []string{"agent-one"}) {
		t.Fatalf("created = %v, want the seat that fitted kept", res.Created)
	}
	if h.fake.byDisplayName("Crewlet Agent One") == nil {
		t.Fatal("the seat that fitted was deleted to report a limit on the next one")
	}
	if h.sink.held("ATLASSIAN_TOKEN_ONE") == "" {
		t.Error("the credential of a finished seat was discarded")
	}
	if !h.sink.flushed {
		t.Error("the sink was never flushed, so nothing durable was kept")
	}
	var named bool
	for _, note := range res.Notes {
		if strings.Contains(note, "-handles") {
			named = true
		}
	}
	if !named {
		t.Errorf("the notes do not say how to finish the run: %v", res.Notes)
	}
}

func TestACredentialThatCannotBeRetiredIsANoteRatherThanALostRun(t *testing.T) {
	t.Parallel()
	// Retirement is HYGIENE, and it runs after the real work. A transient
	// failure here used to roll back a run whose every meaningful step had
	// succeeded — revoking the fresh credential of every seat before it,
	// whose old ones this same function had already killed.
	h := newHarness(t, oneSeat())
	account := h.fake.seed("Crewlet Agent SWE", bothProducts...)
	h.fake.mint(account, atlassian.TokenLabel("agent-swe")+"-1700000000")
	h.fake.revokeFails = true

	res, err := h.run(t, func(o *atlassian.Options) { o.Rotate = true })
	if err != nil {
		t.Fatalf("a failed revoke destroyed a run that had already succeeded: %v", err)
	}
	if !slices.Equal(res.Rotated, []string{"agent-swe"}) {
		t.Fatalf("rotated = %v", res.Rotated)
	}
	if h.sink.held("ATLASSIAN_TOKEN_SWE") == "" {
		t.Fatal("the credential this run minted was discarded")
	}
	// AND IT IS NAMED. Nothing here keeps state, so the next run's probe
	// answers Self and never comes back for it — the report is the only
	// place a leftover credential is ever mentioned.
	var named bool
	for _, note := range res.Notes {
		if strings.Contains(note, "still live") {
			named = true
		}
	}
	if !named {
		t.Errorf("a live leftover credential went unreported: %v", res.Notes)
	}
}

func TestTwoUpstreamAccountsWearingOneNameAreRefused(t *testing.T) {
	t.Parallel()
	// There is no answer to which of them is this seat, and taking the
	// first is taking whichever Atlassian happened to list first — not
	// stable across runs. The seat's identity would flip between two
	// account ids, each holding issues the other does not, and
	// -decommission can sweep neither because the name is in the keep-set.
	h := newHarness(t, oneSeat())
	h.fake.seed("Crewlet Agent SWE", bothProducts...)
	h.fake.seed("crewlet  agent swe", bothProducts...)

	_, err := h.run(t)
	if err == nil {
		t.Fatal("a duplicate account name upstream was adopted silently")
	}
	if !strings.Contains(err.Error(), "which one is this seat") {
		t.Errorf("error does not name the cause: %v", err)
	}
}

func TestAMintAtlassianRefusedIsNotHuntedAsALostCredential(t *testing.T) {
	t.Parallel()
	// A REFUSAL IS PROOF NOTHING WAS CREATED — a 400 is Atlassian reading
	// the request and declining it. Taking the by-label arm made
	// the cleanup hunt a credential that never existed, count it as
	// revoked, and — when the listing was refused too, which is the
	// correlated case — print the strongest banner this package has,
	// naming a token Atlassian never made.
	h := newHarness(t, oneSeat())
	h.fake.seed("Crewlet Agent SWE", bothProducts...)
	h.fake.mintStatus = http.StatusBadRequest

	_, err := h.run(t)
	if err == nil {
		t.Fatal("a refused mint did not fail the run")
	}
	if h.fake.asked("/manage/api-tokens") > 1 {
		t.Error("the rollback listed the account's credentials to hunt a token " +
			"Atlassian refused to create")
	}
	msg := err.Error()
	if strings.Contains(msg, "may still be live") {
		t.Errorf("a refused mint was reported as a possibly-live credential:\n%v", err)
	}
	if strings.Contains(msg, "credential(s) this run minted were revoked") {
		t.Errorf("a credential that was never created was counted as revoked:\n%v", err)
	}
}
