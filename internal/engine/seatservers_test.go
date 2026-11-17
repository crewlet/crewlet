package engine_test

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/secrets"
)

// A held seat's per-role children CONVERGE on what the seat should run for as
// long as it is held: a credential sealed after the claim, a credential
// rotated, a start that failed for a reason that passed. A child whose
// credential does not resolve is not started at all (package doc, "Tool server
// credentials"); the helper server refuses an empty token
// (toolServerRequireEnv), so a start with one would show as a failure.

// convergingCompany is a company whose per-role tracker refuses to start
// without the seat's token: the CEO's is the ${CEO_TRACKER_TOKEN} a case seals
// when it chooses, the CTO's a literal, making the CTO the healthy neighbour
// to watch for disturbance. ceo is extra environment for the CEO's child.
func convergingCompany(ceo map[string]string) string {
	var extra strings.Builder
	keys := make([]string, 0, len(ceo))
	for k := range ceo {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Fprintf(&extra, "        %s: %q\n", k, ceo[k])
	}
	return fmt.Sprintf(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:
  - name: tracker
    shared: false
    command: %q
    tool_prefix: "tracker_"
    env:
      %s: "server"
      %s: "SEAT_TOKEN"
      %s: "SEAT_TOKEN"
      %s: %q
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env:
      tracker:
        SEAT_TOKEN: "${CEO_TRACKER_TOKEN}"
%s  - name: CTO
    handle: cto
    llm: zulu
    mcp_env:
      tracker:
        SEAT_TOKEN: "cto-secret"
`, os.Args[0],
		toolServerModeEnv, toolServerEchoEnv, toolServerRequireEnv,
		procgrouptest.GORACE, procgrouptest.StandInRaceOptions(),
		extra.String())
}

// sealingOptions is an engine with a keyring, so a case can seal a credential
// into the fleet's secret store the way a provisioning pass does.
func sealingOptions(t *testing.T, doc string) engine.Options {
	t.Helper()
	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return engine.Options{
		Company: parsedCompany(t, doc),
		Bootstrap: bootstrap(t, func(b *config.Bootstrap) {
			brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
			b.Secrets = config.Secrets{
				ActiveKeyID: "k1",
				Keys: []config.SecretKey{{
					ID: "k1", Material: base64.StdEncoding.EncodeToString(key),
				}},
			}
		}),
	}
}

// seal writes one credential through a provisioning pass's sink, which
// refreshes this node's snapshot and asks for the revision to be re-activated.
func seal(t *testing.T, e *engine.Engine, name, value string) {
	t.Helper()
	sink, err := e.SetupSink(iam.ActorFor(iam.Principal{
		Kind: iam.KindMachine, Login: iam.TokenLogin("test-operator"),
	}))
	if err != nil {
		t.Fatalf("SetupSink: %v", err)
	}
	if err := sink.Record(t.Context(), name, value); err != nil {
		t.Fatalf("Record %s: %v", name, err)
	}
	if err := sink.Flush(t.Context()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

// reactivate applies the same document again, as every node does on a
// re-activation.
func reactivate(t *testing.T, e *engine.Engine, doc string) {
	t.Helper()
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, doc), time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// reactivateAndSettle re-activates doc and waits for the reconcile of held
// seats' children the apply handed off.
func reactivateAndSettle(t *testing.T, e *engine.Engine, doc string) {
	t.Helper()
	reactivate(t, e, doc)
	engine.SettleSeatServers(e)
}

// pollUntil polls until cond holds, for the cases that wait on a retry.
func pollUntil(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s: not within %s", what, within)
}

// launches counts the starts recorded in a launch directory.
func launches(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read launches: %v", err)
	}
	return len(entries)
}

// A SEAT CLAIMED BEFORE ITS CREDENTIAL WAS SEALED gets its child once the
// credential is: the claim holds the child back as failed, and the
// re-activation `crewlet secrets set` asks for must start it, not only rebuild
// the seat's registry.
func TestASeatChildWhoseCredentialIsSealedAfterTheClaimStartsOnceItIs(t *testing.T) {
	t.Parallel()
	doc := convergingCompany(nil)
	e := newEngine(t, sealingOptions(t, doc))
	claimed(t, e, 2)

	if names := surfaceOf(t, e, "ceo"); slices.Contains(names, "tracker_probe") {
		t.Fatalf("precondition: the seat's child started with no credential: %v", names)
	}

	seal(t, e, "CEO_TRACKER_TOKEN", "ceo-minted")
	reactivateAndSettle(t, e, doc)

	if got := describes(t, e, "ceo", "tracker_probe"); got != "SEAT_TOKEN=ceo-minted" {
		t.Errorf("the seat's child reports %q, want the credential that was sealed", got)
	}
}

// provisioningCompany is a company that PROVISIONS its seats' Atlassian
// identities: the reconcile loop's pass mints each agent's account and seals
// its address and token into the variables the seat's mcp_env names. The
// CEO's per-role server refuses to start without its token and records every
// launch in starts.
func provisioningCompany(starts string) string {
	return fmt.Sprintf(`
name: Acme
seat_access_migrated: [atlassian]
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:
  atlassian:
    org_id: "${ATLASSIAN_ORG_ID}"
    api_key: "${ATLASSIAN_ORG_KEY}"
  jira:
    url: https://acme.atlassian.net
    token: "${JIRA_ORG_TOKEN}"
mcp_servers:
  - name: atlassian
    shared: false
    command: %q
    tool_prefix: "atlassian_"
    env:
      %s: "server"
      %s: "JIRA_API_TOKEN"
      %s: "JIRA_API_TOKEN"
      %s: %q
      %s: %q
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    integrations:
      jira: {enabled: true, project_role: "10101"}
    mcp_env:
      atlassian:
        JIRA_USERNAME: "${ATLASSIAN_EMAIL_CEO}"
        JIRA_API_TOKEN: "${ATLASSIAN_TOKEN_CEO}"
`, os.Args[0],
		toolServerModeEnv, toolServerEchoEnv, toolServerRequireEnv,
		toolServerLaunchesEnv, starts,
		procgrouptest.GORACE, procgrouptest.StandInRaceOptions())
}

// A SEAT'S CHILD IS NOT STARTED WHILE THE CREDENTIAL IT NEEDS IS STILL BEING
// PROVISIONED, and starts once the seal reaches its node: a start with the
// token empty refuses, or acts as nobody. The launch directory proves it (no
// launch before the seal, exactly one after), and the heartbeat reports the
// child held back for its credential, not a failed start.
func TestASeatChildWaitsForItsProvisionedCredentialUnstarted(t *testing.T) {
	t.Parallel()
	starts := t.TempDir()
	doc := provisioningCompany(starts)
	e := newEngine(t, sealingOptions(t, doc))
	claimed(t, e, 1)

	// A claim returns once its seat's children converged, so any start it
	// made is already counted.
	if got := launches(t, starts); got != 0 {
		t.Fatalf("the seat's server was launched %d time(s) before the credential "+
			"provisioning mints for it was sealed: it ran with the token empty", got)
	}
	// Waiting, or failed naming the pass (its organization key is unset here,
	// so once it reports the wait has no end); which one is a race this case
	// need not win. Never a start, launched or failed.
	// TestAWaitingChildIsReportedFailedWhileThePassThatMintsItWaitsOnAPerson
	// holds that judgement.
	var row coord.MCPServerStatus
	for _, r := range engine.MCPStatus(e) {
		if r.Server == "atlassian" {
			row = r
		}
	}
	heldBack := row.Waiting == 1 ||
		(row.Failed == 1 && strings.Contains(row.Error, "atlassian integration will not mint"))
	if row.Started != 0 || row.Waiting+row.Failed != 1 || !heldBack {
		t.Fatalf("the atlassian row is %+v before the seal: want the CEO's child "+
			"held back for its credential, waiting or failed naming the pass", row)
	}

	seal(t, e, "ATLASSIAN_EMAIL_CEO", "agent-ceo@example.com")
	seal(t, e, "ATLASSIAN_TOKEN_CEO", "ceo-minted")
	reactivateAndSettle(t, e, doc)

	if got := describes(t, e, "ceo", "atlassian_probe"); got != "JIRA_API_TOKEN=ceo-minted" {
		t.Errorf("the seat's child reports %q, want the credential that was sealed", got)
	}
	if got := launches(t, starts); got != 1 {
		t.Errorf("%d launches, want exactly the one the seal's apply made", got)
	}
}

// A ROTATED CREDENTIAL RESTARTS THE SEAT'S CHILD, AND NOTHING ELSE: on the
// rotation gesture (re-activating an unchanged revision) a child must drop the
// revoked value it was exec'd with, while a neighbour whose resolved spec did
// not move is not restarted, or every apply re-handshakes every vendor.
func TestARotatedCredentialRestartsOnlyTheChildItReaches(t *testing.T) {
	t.Parallel()
	doc := convergingCompany(nil)
	e := newEngine(t, sealingOptions(t, doc))
	seal(t, e, "CEO_TRACKER_TOKEN", "first")
	reactivate(t, e, doc)
	claimed(t, e, 2)

	if got := describes(t, e, "ceo", "tracker_probe"); got != "SEAT_TOKEN=first" {
		t.Fatalf("precondition: the seat's child reports %q", got)
	}
	// A restart builds new tool objects, so an unchanged pointer proves the
	// same process is serving.
	neighbour, ok := e.ToolsFor("cto").Lookup("tracker_probe")
	if !ok {
		t.Fatal("precondition: the neighbour has no child")
	}

	seal(t, e, "CEO_TRACKER_TOKEN", "second")
	reactivateAndSettle(t, e, doc)

	if got := describes(t, e, "ceo", "tracker_probe"); got != "SEAT_TOKEN=second" {
		t.Errorf("after the rotation the seat's child reports %q: the apply "+
			"reported the rotation and the child kept the revoked credential", got)
	}
	after, ok := e.ToolsFor("cto").Lookup("tracker_probe")
	if !ok || after.Tool != neighbour.Tool {
		t.Error("the rotation restarted a child whose credential did not move")
	}
}

// A SEAT THE COMPANY NO LONGER DECLARES LOSES ITS CHILDREN AT THE APPLY'S
// PASS, NOT AT ITS RELEASE: a child is the seat's identity at the vendor, and
// the release waits for placement (an apply's ask for a pass is honoured once
// per sweep interval), so it would act for a removed agent until the next
// tick. Here the tick is an hour away and the one honoured ask is spent on a
// revision adding a seat, so only the apply's own pass can stop the CTO's child.
func TestASeatTheCompanyNoLongerDeclaresLosesItsChildrenWhileStillHeld(t *testing.T) {
	t.Parallel()
	doc := convergingCompany(nil)
	e := newEngine(t, engine.WithSweepEvery(sealingOptions(t, doc), time.Hour))
	seal(t, e, "CEO_TRACKER_TOKEN", "ceo-token")
	reactivate(t, e, doc)
	claimed(t, e, 2)
	if got := describes(t, e, "cto", "tracker_probe"); got != "SEAT_TOKEN=cto-secret" {
		t.Fatalf("precondition: the CTO's child reports %q", got)
	}
	kept, ok := e.ToolsFor("ceo").Lookup("tracker_probe")
	if !ok {
		t.Fatal("precondition: the CEO has no child")
	}

	// Spend the interval's one honoured ask on a revision that adds a seat.
	withCFO := doc + "  - name: CFO\n    handle: cfo\n    llm: zulu\n"
	reactivate(t, e, withCFO)
	waitEstablished(t, e, 3)

	cto := "  - name: CTO\n    handle: cto\n    llm: zulu\n    mcp_env:\n" +
		"      tracker:\n        SEAT_TOKEN: \"cto-secret\"\n"
	if strings.Count(withCFO, cto) != 1 {
		t.Fatalf("premise: the document does not declare the CTO as %q", cto)
	}
	reactivateAndSettle(t, e, strings.Replace(withCFO, cto, "", 1))

	if !slices.Contains(e.Node().Host().Held(), "cto") {
		t.Fatalf("premise: the sweep released the CTO (held %v), so the release "+
			"and not the apply's pass is what stopped its child", e.Node().Host().Held())
	}
	if names := surfaceOf(t, e, "cto"); slices.Contains(names, "tracker_probe") {
		t.Errorf("the CTO's child is still running for a seat the published company "+
			"no longer declares: %v", names)
	}
	if after, ok := e.ToolsFor("ceo").Lookup("tracker_probe"); !ok || after.Tool != kept.Tool {
		t.Error("retiring a removed seat's child restarted or dropped a declared seat's child")
	}
}

// AN APPLY DOES NOT WAIT FOR A SEAT'S CHILD TO START: a hung handshake takes
// its whole startup timeout, and every stage after it (here a new role's
// mailbox, which drops what is published until it exists) would wait with the
// new epoch already current. The child's start is held open until the case
// lets it go.
func TestAnApplyDoesNotWaitForASeatsChildToStart(t *testing.T) {
	t.Parallel()
	doc := convergingCompany(nil)
	e := newEngine(t, sealingOptions(t, doc))
	seal(t, e, "CEO_TRACKER_TOKEN", "ceo-token")
	reactivate(t, e, doc)
	claimed(t, e, 2)
	if got := describes(t, e, "ceo", "tracker_probe"); got != "SEAT_TOKEN=ceo-token" {
		t.Fatalf("precondition: the seat's child reports %q", got)
	}

	// The revision moves the CEO's child onto a start that hangs (requiring a
	// variable nothing sets, its refusal held until released) and adds a role.
	hold := filepath.Join(t.TempDir(), "let-the-start-end")
	t.Cleanup(func() { _ = os.WriteFile(hold, nil, 0o600) })
	next := convergingCompany(map[string]string{
		toolServerRequireEnv: "CREWLET_ENGINE_TEST_NEVER_SET",
		toolServerHoldEnv:    hold,
	}) + `  - name: CFO
    handle: cfo
    llm: zulu
`
	cfg := parsedCompany(t, next)
	applied := make(chan error, 1)
	go func() {
		_, _, err := e.Apply(t.Context(), cfg, time.Now())
		applied <- err
	}()
	select {
	case err := <-applied:
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the apply has not returned in 30s: it is waiting for the start " +
			"of a seat's child, with the new epoch current and every stage after " +
			"it — a new role's mailbox among them — held behind a vendor handshake")
	}

	pollUntil(t, 30*time.Second, "the restarted child's start", func() bool {
		_, err := os.Stat(hold + ".started")
		return err == nil
	})
	if _, err := os.Stat(hold); err == nil {
		t.Fatal("precondition: the start was let go before the case checked it")
	}
	records, err := e.Backends().Fleet.Mailboxes(t.Context())
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	if !slices.ContainsFunc(records, func(r coord.MailboxRecord) bool { return r.Handle == "cfo" }) {
		t.Errorf("the role the revision added has no mailbox while a seat's child "+
			"is still starting: %+v", records)
	}
}

// AN APPLY THAT LANDS WHILE THE CLAIM IS STARTING THE CHILD IS NOT LOST: the
// claim must not finish with the child it started from the old value. A claim
// starts no child whose credential does not resolve, so the start in flight
// resolves from the host's environment, held failing by a required variable
// nothing sets until the case releases it, and the apply moves the spec to the
// sealed credential, which the store answers first.
func TestAnApplyDuringTheClaimIsFoldedIntoIt(t *testing.T) {
	t.Parallel()
	hold := filepath.Join(t.TempDir(), "release-the-start")
	doc := convergingCompany(map[string]string{
		toolServerRequireEnv: "CREWLET_ENGINE_TEST_NEVER_SET",
		toolServerHoldEnv:    hold,
	})
	opts := sealingOptions(t, doc)
	opts.Environment = config.MapSource{"K": "test-key", "CEO_TRACKER_TOKEN": "from-the-host"}
	e := newEngine(t, opts)

	started := make(chan error, 1)
	go func() { started <- e.Start(t.Context()) }()
	pollUntil(t, 30*time.Second, "the claim's start of the child", func() bool {
		_, err := os.Stat(hold + ".started")
		return err == nil
	})

	// The credential arrives mid-start, with a revision that stops requiring
	// what the host never set.
	seal(t, e, "CEO_TRACKER_TOKEN", "ceo-minted")
	reactivate(t, e, convergingCompany(nil))
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatalf("release the start: %v", err)
	}
	if err := <-started; err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitEstablished(t, e, 2)

	got, ok := e.ToolsFor("ceo").Lookup("tracker_probe")
	if !ok {
		t.Fatalf("the seat established without its child: the apply that sealed "+
			"its credential landed during the claim and was lost; surface = %v",
			surfaceOf(t, e, "ceo"))
	}
	if desc := got.Tool.Description(); desc != "SEAT_TOKEN=ceo-minted" {
		t.Errorf("the seat's child reports %q, want the sealed credential", desc)
	}
}

// A CHILD THAT FAILED FOR A REASON THAT PASSED (a vendor not yet up, a cold
// fetch that timed out) IS RETRIED, with no apply and no change to its spec,
// and the healthy neighbour is left alone.
func TestAChildThatFailedIsRetriedUntilItStarts(t *testing.T) {
	t.Parallel()
	ready := filepath.Join(t.TempDir(), "vendor-is-up")
	doc := convergingCompany(map[string]string{toolServerReadyEnv: ready})
	opts := engine.WithMCPRetryBase(sealingOptions(t, doc), 20*time.Millisecond)
	e := newEngine(t, opts)
	seal(t, e, "CEO_TRACKER_TOKEN", "ceo-token")
	reactivate(t, e, doc)
	claimed(t, e, 2)

	if names := surfaceOf(t, e, "ceo"); slices.Contains(names, "tracker_probe") {
		t.Fatalf("precondition: the child started before its vendor was up: %v", names)
	}
	neighbour, ok := e.ToolsFor("cto").Lookup("tracker_probe")
	if !ok {
		t.Fatal("precondition: the neighbour has no child")
	}

	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatalf("bring the vendor up: %v", err)
	}
	pollUntil(t, 30*time.Second, "the retried child's tools", func() bool {
		return slices.Contains(surfaceOf(t, e, "ceo"), "tracker_probe")
	})
	if got := describes(t, e, "ceo", "tracker_probe"); got != "SEAT_TOKEN=ceo-token" {
		t.Errorf("the retried child reports %q", got)
	}
	after, ok := e.ToolsFor("cto").Lookup("tracker_probe")
	if !ok || after.Tool != neighbour.Tool {
		t.Error("retrying one seat's child restarted its healthy neighbour")
	}
}

// AND A RETRY NEVER STARTS A CHILD FOR A SEAT THIS NODE LET GO: a child's
// credentials are the seat, so a retry fired after the release would run the
// agent here while a peer runs it. The node drains with the retry armed, then
// the vendor comes up: nothing may start.
func TestARetryStartsNothingForASeatThisNodeReleased(t *testing.T) {
	t.Parallel()
	ready := filepath.Join(t.TempDir(), "vendor-is-up")
	starts := t.TempDir()
	doc := convergingCompany(map[string]string{
		toolServerReadyEnv: ready, toolServerLaunchesEnv: starts,
	})
	opts := engine.WithMCPRetryBase(sealingOptions(t, doc), 20*time.Millisecond)
	e := newEngine(t, opts)
	seal(t, e, "CEO_TRACKER_TOKEN", "ceo-token")
	reactivate(t, e, doc)
	claimed(t, e, 2)
	pollUntil(t, 30*time.Second, "a retry of the failing child", func() bool {
		return launches(t, starts) >= 2
	})

	e.Stop(t.Context())
	if held := e.Node().Host().Held(); len(held) != 0 {
		t.Fatalf("precondition: the drain kept %v", held)
	}
	before := launches(t, starts)
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatalf("bring the vendor up: %v", err)
	}
	// Covers several retries at the shortened spacing (20 to 320 ms), had
	// any survived the release.
	time.Sleep(time.Second)
	if got := launches(t, starts); got != before {
		t.Errorf("%d starts after the seat was released: a retry armed while it "+
			"was held put its identity back on this node", got-before)
	}
}
