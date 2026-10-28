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

// A held seat's per-role children CONVERGE on what the seat should run, for as
// long as the seat is held: a credential sealed after the claim, a credential
// rotated, a start that failed for a reason that passed.
//
// Every case here describes something an operator saw before it was fixed. On
// a first boot under an integration that provisions seat identities, the
// engine claimed its seats before the provisioning pass had sealed their bot
// tokens; each seat's chat server was spawned with an empty token and refused
// to start, and the seat received messages it had no tool to answer, until a
// restart claimed it again. The helper server here refuses an empty token the
// same way (toolServerRequireEnv), which is what a case that lets a start
// happen with one would see — none may now: a seat's child whose credential
// does not resolve is not started at all (toolcredentials.go).

// convergingCompany is a company whose per-role tracker refuses to start
// without the seat's token. The CEO's token is the ${CEO_TRACKER_TOKEN} a case
// seals into the store when it chooses; the CTO's is a literal, so it is the
// healthy neighbour every case can watch for disturbance. ceo is extra
// environment for the CEO's own child.
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
			brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
			b.Secrets = config.Secrets{
				ActiveKeyID: "k1",
				Keys: []config.SecretKey{{
					ID: "k1", Material: base64.StdEncoding.EncodeToString(key),
				}},
			}
		}),
	}
}

// seal writes one credential through the sink a provisioning pass writes
// through, which refreshes this node's snapshot and asks for the revision to
// be re-activated.
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

// reactivate is what every node does when the revision is re-activated: apply
// the same document again.
func reactivate(t *testing.T, e *engine.Engine, doc string) {
	t.Helper()
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, doc), time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// reactivateAndSettle re-activates doc and waits for the reconcile of every
// held seat's children that the apply handed off, for a case that reads what
// that reconcile did.
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
// credential is.
//
// Nothing here provisions the CEO's token, so until it is sealed the claim
// holds the child back as failed, naming the variable. A person sealing it by
// hand re-activates the revision afterwards — `crewlet secrets set` tells them
// to, and this case does it for them — which every node applies; and the apply
// once rebuilt the seat's REGISTRY and left its children exactly as the claim
// had: none. The seat kept no tool to answer with until it changed hands.
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
// identities: the integration block is there, so the reconcile loop's pass
// mints each agent's account and seals its address and token into the
// variables the seat's mcp_env points at. The CEO's per-role server refuses
// to start without its token, and records every launch in starts.
func provisioningCompany(starts string) string {
	return fmt.Sprintf(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:
  atlassian:
    org_id: "${ATLASSIAN_ORG_ID}"
    api_key: "${ATLASSIAN_ORG_KEY}"
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
    mcp_env:
      atlassian:
        JIRA_USERNAME: "${CEO_ATLASSIAN_EMAIL}"
        JIRA_API_TOKEN: "${CEO_ATLASSIAN_TOKEN}"
`, os.Args[0],
		toolServerModeEnv, toolServerEchoEnv, toolServerRequireEnv,
		toolServerLaunchesEnv, starts,
		procgrouptest.GORACE, procgrouptest.StandInRaceOptions())
}

// A SEAT'S CHILD IS NOT STARTED WHILE THE CREDENTIAL IT NEEDS IS STILL BEING
// PROVISIONED, and starts once the seal reaches its node.
//
// On a first boot the engine claims its seats before the provisioning pass has
// minted their credentials. The claim used to spawn the vendor's server with
// the token empty — a process that refuses to start, or worse, starts and acts
// as nobody — then retry it on the restart schedule, logging each attempt as a
// failure, for a credential the engine itself knew it had not minted yet. The
// launch directory is the proof: nothing may have been launched before the
// seal, and exactly one launch after it. And the heartbeat is the other half:
// the child it reports is held back for its credential, not a start that
// failed.
func TestASeatChildWaitsForItsProvisionedCredentialUnstarted(t *testing.T) {
	t.Parallel()
	starts := t.TempDir()
	doc := provisioningCompany(starts)
	e := newEngine(t, sealingOptions(t, doc))
	claimed(t, e, 1)

	// THE CLAIM'S PASS IS OVER when claimed returns — a claim returns once its
	// seat's children have converged — so a start it made would be counted
	// already, with no wait.
	if got := launches(t, starts); got != 0 {
		t.Fatalf("the seat's server was launched %d time(s) before the credential "+
			"provisioning mints for it was sealed: it ran with the token empty", got)
	}
	// WAITING, OR FAILED NAMING THE PASS, and which of the two this beat sees
	// is a race the case does not need to win: this engine runs its own
	// Atlassian pass, whose organization key is unset here, so once that pass
	// has reported, the child's wait has no end and is a failure carrying its
	// finding. What it may never be is a start — launched, or failed to launch.
	// TestAWaitingChildIsReportedFailedWhileThePassThatMintsItWaitsOnAPerson
	// holds the judgement itself.
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

	seal(t, e, "CEO_ATLASSIAN_EMAIL", "agent-ceo@example.com")
	seal(t, e, "CEO_ATLASSIAN_TOKEN", "ceo-minted")
	reactivateAndSettle(t, e, doc)

	if got := describes(t, e, "ceo", "atlassian_probe"); got != "JIRA_API_TOKEN=ceo-minted" {
		t.Errorf("the seat's child reports %q, want the credential that was sealed", got)
	}
	if got := launches(t, starts); got != 1 {
		t.Errorf("%d launches, want exactly the one the seal's apply made", got)
	}
}

// A ROTATED CREDENTIAL RESTARTS THE SEAT'S CHILD, AND NOTHING ELSE.
//
// Re-activating an unchanged revision is the documented rotation gesture, and
// a per-role child kept the credential it was exec'd with until its seat next
// moved — reporting the rotation done while the agent went on authenticating
// with the value the operator had just revoked. The neighbour, whose resolved
// spec did not move, must not be restarted: that is what keeps an apply from
// re-handshaking every seat's vendor on every revision.
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
	// The tool OBJECT identifies the child behind it: a restart builds new
	// ones, so an unchanged pointer is proof the same process is serving.
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
// PASS, NOT AT ITS RELEASE.
//
// A child is the seat's identity at the vendor, and a revision that removes
// the role says the company no longer has that identity. The release that
// follows stops the children too, but the release waits for placement: an
// apply's ask for a pass is honoured at most once per sweep interval, so a
// node can go on holding a removed seat until the next tick. A child kept for
// that long would go on acting at the vendor for an agent the company has let
// go. Here the tick is an hour away, and the one ask the interval honours is
// spent on a revision that adds a seat. So the revision that removes the CTO
// leaves the seat held, and only the apply's own pass can stop its child.
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

// AN APPLY DOES NOT WAIT FOR A SEAT'S CHILD TO START.
//
// A start is a subprocess handshake, and one that never completes — a vendor
// endpoint that is down, a package fetch that hangs — takes its whole startup
// timeout, two minutes by default. The apply restarting a seat's child used to
// wait for it with the new epoch already current, and everything after that
// stage waited too: here a revision that adds a role, whose mailbox drops what
// is published to it until it exists. The restarted child's start is held
// open until the case lets it go, so an apply that waited for it would return
// only when the server gave up a minute later.
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

	// The revision moves the CEO's child onto a start that hangs — it
	// requires a variable nothing sets, and holds its refusal until the
	// case releases it — and adds a role.
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

// AN APPLY THAT LANDS WHILE THE CLAIM IS STARTING THE CHILD IS NOT LOST.
//
// This is the order a first boot ran in: the claim read the snapshot and
// spawned the child, the provisioning pass sealed the token and the apply
// landed while that child was still failing, and the apply could not reconcile
// children the claim had not yet recorded. The claim then finished with the
// child it had started from the old value. A claim no longer starts a child
// whose credential does not resolve, so here the start in flight is one from
// a value the host's environment holds — a REQUIRED variable it holds
// nothing for keeps the start failing until the case lets it go — and the
// sealed credential, which the store answers ahead of the environment, is
// what the apply moves the spec to.
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

	// The credential arrives while the start that will fail is in flight,
	// with the revision that stops requiring what the host never set.
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

// A CHILD THAT FAILED FOR A REASON THAT PASSED IS RETRIED, with no apply and
// no change to its spec — and the healthy neighbour is left alone by it.
//
// A vendor endpoint not yet up, a cold package fetch that timed out: before,
// the seat lost that server's tools until it changed hands, which on a single
// node meant until the process restarted.
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

// AND A RETRY NEVER STARTS A CHILD FOR A SEAT THIS NODE LET GO.
//
// The credentials in a child ARE the seat, so a retry armed while the seat was
// held and fired after its release would put the agent back on this node while
// a peer runs it. Here the node drains — every seat handed back while the
// failing child is still declared and its retry armed — and the vendor then
// comes up: nothing may start.
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
	// Long enough for several retries at the shortened spacing — 20, 40,
	// 80, 160 and 320 ms — had any survived the release.
	time.Sleep(time.Second)
	if got := launches(t, starts); got != before {
		t.Errorf("%d starts after the seat was released: a retry armed while it "+
			"was held put its identity back on this node", got-before)
	}
}
