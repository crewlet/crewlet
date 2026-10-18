package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A golden company: two seats, one scripted model, one embedded broker.
//
// Its model entry is a REAL cli-agent entry whose CLI is this test binary
// ([scriptedEntry]), rather than a `mock` provider type, deliberately. A mock
// type would be test-only machinery in shipped config — a thing an operator
// could set and a thing this suite could get right while the real backend was
// wrong. Driving the REAL backend against a scripted CLI tests what sits
// between the model and everything asserted here: the transcript a run is
// handed, the system prompt's own channel, the process launched in a seat's
// own home on an allowlisted environment, the seat's tools served to the CLI's
// own loop over MCP and every call it makes through them, each model request
// and its usage read off the CLI's record, and the fallback chain.
//
// The `%s` is that entry, under the key `scripted` every seat here names —
// rendered by [goldenCompany], which is the one way to fill it.
const companyDoc = `
name: Nimbus
providers:
  llm:
%s
roles:
  - name: CEO
    handle: ceo
    llm: scripted
  - name: Founder
    kind: human
    contact:
      slack_user_id: U0FOUNDER
# TWO UNITS WITH PROJECTS, and they are BELOW the seats deliberately: several
# fixtures here extend this document by replacing the first "roles:\n" in it,
# and a unit's own indented roles key would match that one first. Keeping the
# units last means a fixture patching the seats still patches the seats.
#
# They exist because a project is NOT created on demand: a task takes its key
# from its project's own counter, so the project has to be an object before
# anything can be filed in it — and the only thing that makes one is the chart
# apply reading these keys. A company that declared none could file no work at
# all, which is what these two exercise end to end.
#
# ENGINEERING ALSO DECLARES A SPACE, for the same reason one level over: a page
# is created only in a space the company declares — a unit's or a seat's
# space:, the root space or the tool-skills space — and the knowledge cases
# here write their pages in ENG.
units:
  - name: Engineering
    id: engineering
    purpose: builds and runs the product
    project: ENG
    space: ENG
    lead: ceo
  - name: Operations
    id: operations
    purpose: keeps the lights on
    project: OPS
    lead: ceo
turn_engine:
  max_iterations: 1
  max_tool_rounds: 3
`

// goldenCompany is the golden company document with its model entry driving
// model's scripted CLI.
func goldenCompany(model *scriptedModel) string {
	return fmt.Sprintf(companyDoc, model.entry("scripted"))
}

// tickInterval is the shared tick's cadence on a single node here. Short enough
// that the spend rollup lands inside a test, long enough not to drown the
// capture in health frames.
//
// NOT A FLEET MEMBER'S, which runs the production cadence
// ([stream.HealthInterval]; see [buildMember]). On a cluster every tick is
// several certified coordination listings — the live nodes and their apply
// status for the health envelope, the seat leases for placement — and each
// listing creates and deletes an ordered consumer through the broker's
// metadata group, which every member has to apply. Measured on a fleet that
// only idled for thirty seconds: 113 cpu-s at 25 ms against 61 at five
// seconds, close to two cores spent on ticks nobody reads — no fleet case
// reads a push; every one reads REST or the engines.
const tickInterval = 25 * time.Millisecond

// harnessActivation is the instant this harness's company was "activated".
//
// A harness node boots straight onto a company, standing in for `crewlet run`
// without the reconciler that would publish it and apply it with the
// activation pointer's own instant — and a company booted with no activation
// has its projects and knowledge spaces applied by nothing, since each is
// stamped with the activation that named it. Every node of one harness fleet
// boots with the SAME instant, because they are running one activation — and
// every RESTART boots with it too: a value read off a clock per node or per
// boot is exactly what the stamp exists to rule out.
var harnessActivation = time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)

// middayZone names a fixed-offset zone whose local time is within half an hour
// of noon when it is called, spelled as a company's `timezone` takes it.
//
// FOR A CASE WHOSE PREMISE IS ONE COMPANY DAY. A token budget's windows are cut
// on the company's clock at the instant each charge is made (ADR-0019), so a
// turn that runs across the company's midnight charges its later rounds to the
// next day — correctly. A case that sizes a ceiling to bite partway through one
// turn, or reads back the day a refusal was stamped in, therefore holds only
// while no midnight falls inside it, and on the wall clock and UTC one falls
// inside some run every night: the onboarding round charged at 23:59:59.95 and
// the execute round at 00:00:00.03 each fit a fresh day, nothing is refused,
// and the case waits out its budget for a refusal that never comes. With the
// company's local time at noon when the case starts, the nearest boundary of
// every window — a day, an ISO week and a month all turn over at a local
// midnight — is eleven and a half hours away, which no case here outlives.
//
// THE ZONE MOVES AND THE CLOCK DOES NOT. Pinning the engine's clock to a
// synthetic noon was the other way to put the run mid-day, and it splits the
// process in two: the engine shares it with the API, the live projection and
// the coordination store, which all read the wall clock — the projection
// judges whether a refused window has ended against it, and the store stamps a
// refusal with it — so the gate would count a day the screens beside it had
// already seen end, a node no deployment produces. Moving where the company's
// day falls keeps every reader on the one calendar a deployment runs: the
// gate, the park, the live meter, the budgets answer, a refusal's stamp and
// the case's own read, each on the wall clock and the company's zone. A case
// reads the zone back from the engine ([engine.Engine.Zone]) rather than
// loading this name itself, so it judges the day on the clock the gate cut.
//
// Etc/GMT names count WEST as positive, so Etc/GMT-5 is five hours ahead of
// UTC; every offset this takes, -12 to +12, has one.
func middayZone() string { return middayZoneAt(time.Now()) }

// middayZoneAt is [middayZone] at the instant now.
func middayZoneAt(now time.Time) string {
	now = now.UTC()
	sinceMidnight := now.Sub(now.Truncate(24 * time.Hour)).Hours()
	switch ahead := int(math.Round(12 - sinceMidnight)); {
	case ahead > 0:
		return fmt.Sprintf("Etc/GMT-%d", ahead)
	case ahead < 0:
		return fmt.Sprintf("Etc/GMT+%d", -ahead)
	default:
		return "Etc/GMT"
	}
}

// companyMidday is the zone e cuts its company's windows on, failing the case
// unless the company's local time is mid-day: the premise [middayZone] is
// there to make true, held where the case depends on it, so a company whose
// `timezone` did not take fails at its first line rather than on the one night
// a midnight falls inside its turn.
func companyMidday(t *testing.T, e *engine.Engine) *time.Location {
	t.Helper()
	zone := e.Zone()
	if local := time.Now().In(zone); local.Hour() < 11 || local.Hour() > 12 {
		t.Fatalf("the company's clock reads %s on %s as the case starts; the case "+
			"holds only inside one company day, which it is guaranteed only from "+
			"mid-day (middayZone)", local.Format("15:04"), zone)
	}
	return zone
}

// THE ZONE IS MID-DAY WHATEVER THE HOUR. [middayZone] is what keeps the cases
// that need one company day inside one, and an offset it got wrong would put
// the company's midnight back inside the very runs it is there for — so a whole
// day is asked, every name is loaded the way a company's `timezone` is, and the
// local time each gives must be within half an hour of noon.
func TestMiddayZoneIsMiddayAtEveryHour(t *testing.T) {
	t.Parallel()
	zones := map[string]*time.Location{}
	day := time.Date(2026, time.October, 5, 0, 0, 0, 0, time.UTC)
	// A prime step, so the instants asked fall on every second of a minute
	// as well as every minute of an hour.
	for at := day; at.Before(day.Add(24 * time.Hour)); at = at.Add(37 * time.Second) {
		name := middayZoneAt(at)
		zone, loaded := zones[name]
		if !loaded {
			var err error
			if zone, err = period.LoadZone(name); err != nil {
				t.Fatalf("at %s UTC the zone is %q, which a company cannot run on: %v",
					at.Format("15:04:05"), name, err)
			}
			zones[name] = zone
		}
		local := at.In(zone)
		noon := time.Date(local.Year(), local.Month(), local.Day(), 12, 0, 0, 0, zone)
		if off := local.Sub(noon); off < -30*time.Minute || off > 30*time.Minute {
			t.Fatalf("at %s UTC, %s reads %s — %v from noon, want within half an hour",
				at.Format("15:04:05"), name, local.Format("15:04:05"), off)
		}
	}
	if len(zones) != 25 {
		t.Errorf("a day asked for %d zones, want one per whole-hour offset from -12 to +12",
			len(zones))
	}
}

// node is a running node: engine and API in one process, wired as
// `crewlet run` wires them.
type node struct {
	engine *engine.Engine
	app    *api.App
	server *httptest.Server
	model  *scriptedModel

	// id is what this node calls itself — its name in a search fan-out's
	// assignment table and on its own presence lease. Held because a case
	// that addresses one member by name has to use the SAME string the
	// engine registered under, and deriving it a second time from the
	// index would be a second place to decide what a member is called.
	//
	// READ OFF THE ENGINE (`e.Node().ID()`), never off the bootstrap: a
	// single-node boot leaves `node.id` empty and the engine resolves it
	// itself ([config.ResolveNodeID] — CREWLET_NODE_ID, then the default),
	// so the bootstrap's field was the empty string there, and a reconciler
	// built from it was refused for naming no node.
	id string

	// snapshotDir is where this node writes its own snapshots of the
	// replicated estate. Held because a case that asserts a node TOOK one
	// has to read the directory, and deriving it a second time from the
	// bootstrap would be a second place to decide where they go.
	snapshotDir string

	// chat is the node's chat hub, attached to its engine as `crewlet run`
	// attaches one ([newEngine]) — on every node, an API or none, because a
	// seat raises its working indicator wherever its turn runs and every
	// node answers the fleet's presence probe for the seats it runs.
	chat *api.EngineChatLive
}

// start stands one up.
//
// Everything real: a real embedded stream on a real temp directory, a real
// store, the real API in front of them, and the real observability pipeline
// wired the way cmd/crewlet wires it. The one stub is the model, behind the
// CLI the node's real backend launches ([scriptedEntry]).
func start(t *testing.T) *node { return startNode(t, nodeSpec{}) }

// startWith stands a node up over a company document the caller may amend, for
// the cases whose subject is a config field rather than a turn.
func startWith(t *testing.T, amend func(doc string) string) *node {
	t.Helper()
	return startNode(t, nodeSpec{company: amend})
}

// nodeSpec is what a case may vary about the node it stands up. The zero value
// is the golden company on the harness's own environment.
type nodeSpec struct {
	// company amends the golden company document.
	company func(doc string) string
	// boot amends the operator's half of the configuration, for the cases
	// that need it — a bearer token a person writes with.
	boot func(*config.Bootstrap)
	// env is what the node's `${VAR}` references resolve to beyond the
	// harness's own ([nodeEnvironment]).
	env map[string]string
}

// nodeEnvironment is the environment a node in this package resolves its
// `${VAR}` references from: the harness's own variables and the case's,
// HANDED to the engine ([engine.Options.Environment]) and never read from the
// process.
//
// HANDED because the process environment is the one input every case in a
// binary shares. A case that configured a node through it had to set it with
// t.Setenv, which Go refuses beside t.Parallel, so eight cases ran alone for
// no other reason — and a case asserting that a reference resolves to NOTHING
// held only on a runner that happened not to export the variable.
//
// The harness itself resolves nothing through it: the scripted CLI signs in to
// nothing, and what its child needs is written into its entry's own `env`
// ([scriptedEntry]).
func nodeEnvironment(vars map[string]string) config.MapSource {
	env := config.MapSource{}
	maps.Copy(env, vars)
	return env
}

// startNode stands a node up as spec describes.
func startNode(t *testing.T, spec nodeSpec) *node {
	t.Helper()
	logs.attribute(t)
	model := newScriptedModel(t)

	doc := goldenCompany(model)
	if spec.company != nil {
		doc = spec.company(doc)
	}
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("company config: %v", err)
	}
	boot := config.DefaultBootstrap()
	boot.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&boot.Stream, filepath.Join(t.TempDir(), "stream"))
	withServingTierA(t, &boot)
	if spec.boot != nil {
		spec.boot(&boot)
	}
	seedStore(t, &boot)

	e, chatLive := bootEngine(t, engine.Options{
		Bootstrap: &boot, Company: cfg, ActivatedAt: harnessActivation,
		Environment: nodeEnvironment(spec.env),
	})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}

	app, srv := serveAPI(t, e, &boot, chatLive.Hub(), nil)

	return &node{engine: e, app: app, server: srv, model: model, id: e.Node().ID(),
		chat: chatLive}
}

// newEngine builds a node's engine as `crewlet run` builds it: [engine.New],
// and straight after it the process's chat hub ([api.NewEngineChatLive]) —
// started, and attached to the engine whether or not the node will serve an
// API. A node that serves one mounts its socket over the same hub
// ([serveAPI]); a node that serves none still answers the fleet's presence
// probe for the indicators its seats raise, which is what the hub exists on
// such a node for.
//
// NEVER a sequence written out at each boot path. The hub used to be built by
// the API's wiring here ([wireAPI]), which is exactly where `crewlet run` must
// not build it (TestRunBuildsTheChatHubOnEveryNode): a node the harness stood
// up with no API — the stateless pair's seats-only member — attached none, and
// an indicator its seat raised was answered for by nobody.
//
// THE TEARDOWN IS RETURNED in the order the two came up, for the caller to run
// in reverse ([stopInReverse]) — the hub before the engine, in `crewlet run`'s
// shutdown order: the engine's appliers announce into the hub until it stops,
// and its teardown closes the broker the hub's answerer is subscribed on. Both
// on [context.WithoutCancel], because the context a boot runs under has often
// ended by the time its teardown runs. Returned on failure too, holding
// whatever did come up. The caller starts the engine.
func newEngine(ctx context.Context, opts engine.Options) (
	*engine.Engine, *api.EngineChatLive, []func(), error) {

	e, err := engine.New(ctx, opts)
	if err != nil {
		return nil, nil, nil, err
	}
	stops := []func(){func() { e.Stop(context.WithoutCancel(ctx)) }}
	chatLive := api.NewEngineChatLive(ctx, e)
	stops = append(stops, func() { chatLive.Stop(context.WithoutCancel(ctx)) })
	return e, chatLive, stops, nil
}

// bootEngine is [newEngine] for a node that lives as long as its test, on the
// test's goroutine: torn down when the test ends — registered before anything
// can fail, so an engine that never started is stopped too — and failing the
// test when the engine is refused.
func bootEngine(t *testing.T, opts engine.Options) (*engine.Engine, *api.EngineChatLive) {
	t.Helper()
	e, chatLive, stops, err := newEngine(t.Context(), opts)
	t.Cleanup(func() { stopInReverse(stops) })
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return e, chatLive
}

// seedStore writes the migrated store image where boot's store will open, so a
// node boots onto a store this binary migrated once rather than running every
// migration again for every node it stands up — the cost [storetest.Seed]
// exists to remove, and none of these cases is about migrating.
//
// THE ENGINE'S OWN RULE for what a fresh node holds, so a seeded node is one a
// fresh boot would have produced: the node file unless the store is scratch
// (discarded at open, so a seed would only be deleted), and the replicated file
// only on a node that holds the estate ([engine.HoldsEstate]) — a node without
// `data` has none, and a backup or an estate check would find one that was put
// there. A file already there is left as it is, which is what lets the restart
// case boot twice on one path and reopen its own.
//
// Call it once boot is final and before engine.New, ON THE TEST'S GOROUTINE:
// a seed that cannot be written fails the test, which only that goroutine may.
func seedStore(t *testing.T, boot *config.Bootstrap) {
	t.Helper()
	if boot.Store.Scratch {
		return
	}
	storetest.Seed(t, store.EstateNode, boot.Store.Path)
	if engine.HoldsEstate(boot) {
		storetest.Seed(t, store.EstateReplicated,
			store.ReplicatedPath(boot.Store.Path, boot.Store.ReplicatedPath))
	}
}

// scriptedModel is the model behind the scripted CLI ([scriptedEntry]): every
// model request a CLI run makes reaches it as the one request the CLI posts
// for it ([modelRequest]), and it answers by PHASE.
//
// Keyed on the tools the request offers rather than on request ORDER: a phase
// that retries, a rescue, a run the engine ended and started again, or an
// extension all send another request, and a script that counted would answer
// the second executor request with the reviewer's reply. What distinguishes the
// phases is what they can invoke — submit_work, submit_review, or neither —
// which is exactly the fact the runner varies, and the tools are the ones the
// CLI listed from the seat's server, never a list the prompt mentions.
type scriptedModel struct {
	url string

	// stateDir is where the entry driving this model's CLI keeps its seats'
	// homes ([scriptedEntry]) — this model's own, so no two nodes' entries
	// share one unless they share the model.
	stateDir string

	// close shuts this model's HTTP server down. See [scriptedModel.listen]
	// for why it is a field as well as a t.Cleanup.
	close func()

	// Everything below is ONE ENTRY PER MODEL REQUEST, at one index — see
	// [scriptedModel.sawRequest].
	mu      sync.Mutex
	calls   []string
	offered []string
	// prompts is the transcript of every request's run, as the CLI read it
	// on stdin.
	prompts []string
	// results is every tool result each request handed the model back —
	// the earlier runs' the transcript carries, then the run's own — so a
	// test can read what came BACK from a tool as well as what went out: a
	// tool result rides the conversation, not the system prompt.
	results [][]string

	// systems is the SYSTEM prompt of every request, as the CLI read it off
	// its own channel, which is where the turn-start prefetch's blocks
	// land. Captured because "the block was rendered" and "the model was
	// shown it" are different claims and only the second one matters.
	systems []string
	// channels is which channel each request's system prompt came on
	// ([modelRequest.Channel]).
	channels []string

	// engages makes the executor call a tool before answering, which is
	// what the reflect dispatcher's engagement gate reads. A company with
	// a chat integration always does — its executor calls the delivery
	// tool — but this document configures none, so the default turn ends
	// having called nothing and every learning worker correctly skips it.
	// Opt-in rather than default because the phase-count assertions
	// elsewhere are written against a one-request Execute.
	engages bool
	// engaged records whether the extra call has already been made this
	// turn, so the executor calls the tool once and then answers.
	engaged bool

	// searchQuery makes the executor call search_knowledge once with that
	// query. Opt-in for the same reason engages is: the phase-count
	// assertions elsewhere are written against a one-request executor.
	searchQuery string
	searched    bool

	// calls are tool calls the executor makes, one per request, before it
	// submits — the scripted half of a turn whose subject is what a tool
	// did rather than what the model said.
	scripted []scriptedCall
}

// scriptedCall is one tool call the executor makes.
type scriptedCall struct {
	tool  string
	input map[string]any
}

// callOnExecute makes the next executor requests call a tool, once each, in
// the order given, before the request that submits.
func (m *scriptedModel) callOnExecute(tool string, input map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scripted = append(m.scripted, scriptedCall{tool: tool, input: input})
}

// nextCall is the executor's next scripted call, if one is left.
func (m *scriptedModel) nextCall() (scriptedCall, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.scripted) == 0 {
		return scriptedCall{}, false
	}
	call := m.scripted[0]
	m.scripted = m.scripted[1:]
	return call, true
}

// engageOnExecute makes the next turn's executor call a tool.
func (m *scriptedModel) engageOnExecute() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.engages, m.engaged = true, false
}

// searchOnExecute makes the next turn's executor call search_knowledge once
// before it submits, which is what a real seat does when its turn-start
// knowledge block came back empty.
func (m *scriptedModel) searchOnExecute(query string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.searchQuery, m.searched = query, false
}

// shouldSearch reports whether this executor request is the searching one, and
// marks it spent.
func (m *scriptedModel) shouldSearch() (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.searchQuery == "" || m.searched {
		return "", false
	}
	m.searched = true
	return m.searchQuery, true
}

// shouldEngage reports whether this Execute request is the tool-calling one,
// and marks it spent.
func (m *scriptedModel) shouldEngage() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.engages || m.engaged {
		return false
	}
	m.engaged = true
	return true
}

// newScriptedModel is a scripted model whose CLI keeps its state in a
// directory of the test's own.
func newScriptedModel(t *testing.T) *scriptedModel {
	t.Helper()
	return newScriptedModelIn(t, t.TempDir())
}

// newScriptedModelIn is a scripted model whose CLI keeps its state under
// stateDir — for a caller off the test's goroutine, where t.TempDir may not
// fail the test, and which names a directory it already holds instead.
func newScriptedModelIn(t *testing.T, stateDir string) *scriptedModel {
	t.Helper()
	m := &scriptedModel{stateDir: stateDir}
	m.listen(t, m.serve)
	return m
}

// listen serves handler as this model, for as long as the test runs.
func (m *scriptedModel) listen(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	m.url = srv.URL
	// EXPOSED as well as registered, for the one caller whose lifetime is
	// shorter than the test's: a cluster attempt that fails stops what it
	// started before the next one starts, and this is one of the things it
	// started. httptest.Server.Close is idempotent, so the cleanup above
	// stays correct for every other caller.
	m.close = srv.Close
}

// entry is the providers.llm entry, under key, whose CLI answers from this
// model ([scriptedEntry]).
func (m *scriptedModel) entry(key string) string {
	return scriptedEntry(key, m.url, m.stateDir)
}

func (m *scriptedModel) serve(w http.ResponseWriter, r *http.Request) {
	req, ok := readRequest(w, r)
	if !ok {
		return
	}
	offered := req.offers()
	// ONE RECORD PER REQUEST, WRITTEN WHERE THE PHASE IS KNOWN — see
	// [scriptedModel.sawRequest] for what pairing them separately cost.
	saw := func(phase string) { m.sawRequest(phase, req) }

	// Keyed on the tool that DISTINGUISHES each phase, in the order that
	// makes each one unambiguous. submit_* first, because those name one
	// phase each; onboarding is then the pass that offers mark_onboarded
	// and neither submit tool.
	var reply modelReply
	switch {
	case offered["submit_review"]:
		// Checked FIRST. The reviewer's system prompt embeds the
		// executor's tool log as evidence, so the string "submit_work"
		// appears in a review request too — matching on the whole body
		// answered the reviewer with a work submission, which never
		// submitted, which rescued into a second round.
		saw("review")
		reply = toolUse("submit_review", map[string]any{
			"decision":       "done",
			"final_artifact": "Three PRs merged, one incident, zero regressions.",
		})
	case offered["submit_work"]:
		if call, ok := m.nextCall(); ok {
			saw("execute")
			reply = toolUse(call.tool, call.input)
			break
		}
		if query, ok := m.shouldSearch(); ok {
			saw("execute")
			reply = toolUse("search_knowledge", map[string]any{"query": query})
			break
		}
		if m.shouldEngage() {
			// The request that CALLS something. Recorded as execute like
			// the submitting one, because it is the same phase — what
			// differs is that this turn leaves an observable trace, which
			// is the condition every learning worker is gated on.
			saw("execute")
			reply = toolUse("list_mcp_server_tools", map[string]any{})
			break
		}
		saw("execute")
		reply = toolUse("submit_work", map[string]any{
			// `delivered` with no cited call, which is honest for the
			// wakes this default serves: an unaddressed notification
			// ([node.wake]) owes nobody a tool reply, so the answer IS
			// the artifact and the engine demands a citation only where a
			// tool was the way to deliver. A turn somebody IS waiting on —
			// a task assignment, a message addressed to the seat — is
			// scripted by its case ([scriptedModel.callOnExecute]), because
			// this claim is refused there until the rounds run out and the
			// engine writes the outcome itself.
			"outcome": "delivered",
			"summary": "Three PRs merged, one incident, zero regressions.",
		})
	case offered["mark_onboarded"]:
		// The first-turn pass, before the executor and on its own budget.
		// A model that never marked would burn all ten rounds and retry
		// next turn — correct behaviour, and thirty seconds of it in a
		// test, which is how this case came to exist.
		saw("onboarding")
		reply = toolUse("mark_onboarded", map[string]any{
			"notes": "Read the team pages; deploys go out on Thursdays.",
		})
	case auxiliaryPass(req.System) != "":
		// AN AUXILIARY PASS, not a phase. The prefetch's memory filter,
		// knowledge query and episode summary all reach this same
		// model with no tools offered, and counting them as Execute
		// made the phase list report work that never happened — and made
		// "the first execute call" name a prompt the executor never saw.
		pass := auxiliaryPass(req.System)
		saw("aux:" + pass)
		reply = textReply(auxiliaryAnswer(pass))
	default:
		saw("execute")
		reply = textReply("Three PRs merged, one incident, zero regressions.")
	}
	answer(w, reply)
}

// auxiliaryPass names which prefetch pass a request is, or "" for a phase.
//
// Keyed on each pass's own system prompt, which is the only thing that
// distinguishes them: they share a CLI, a model and an empty tool list.
// Matching on a phrase each prompt OPENS with, so a later edit to the
// guidance below it cannot silently turn every pass back into "execute".
func auxiliaryPass(system string) string {
	switch {
	case strings.Contains(system, "memory-relevance filter"):
		return "memory"
	case strings.Contains(system, "into a search query for"):
		return "knowledge"
	case strings.Contains(system, "compress an AI agent's record"):
		return "recall"
	case strings.Contains(system, "post-turn reflection classifier"):
		return "persist"
	case strings.Contains(system, "working profile of one person"):
		return "profile"
	case strings.Contains(system, "episode compactor"):
		return "compact"
	default:
		return ""
	}
}

// auxiliaryAnswer is what each pass gets back.
//
// Real answers rather than empty ones, because an empty answer is the
// degraded path and a fixture that always takes it proves the degradation
// works and nothing else.
func auxiliaryAnswer(pass string) string {
	switch pass {
	case "memory":
		// Every candidate, so a memory written by a test reaches the
		// prompt it was written for.
		return "[0, 1, 2, 3, 4, 5, 6, 7]"
	case "knowledge":
		return "staging login redirect proxy"
	case "persist":
		// A REAL classification, so the write side's end-to-end case has
		// a row to find. The other passes answer with what they would
		// really answer; a decider that always said NOOP would prove the
		// gate and nothing about the write.
		return `{"kind":"LONG","content":"The founder wants the weekly ` +
			`numbers stated plainly, with no preamble."}`
	case "profile":
		return `{"reply_style":"plain numbers, no preamble"}`
	default:
		return ""
	}
}

// sawRequest records everything ONE model request is known by, in ONE
// critical section: the phase it was, the tools it was offered, the system
// prompt it was shown, its run's transcript and the tool results it carried.
//
// # Why they cannot be appended separately
//
// Because every reader PAIRS THEM BY INDEX — [executorPrompt] finds "execute"
// in calls and reads systems at the same position — and that only holds if one
// request appends to all of them without another getting in between. It did
// not. serve appended the prompt, the tools and the body before the switch (it
// has to: the phase is not known until the request is read), released the
// lock, and saw appended the phase after. Two requests in flight — two seats,
// or a seat and one of the prefetch's auxiliary passes, which is the ordinary
// case — interleave between those two sections, and from then on calls[i] and
// systems[i] are different requests.
//
// Measured: the end-to-end skills case read a prompt with no "## Tool skills"
// section and failed on a section that was there all along, having been handed
// an auxiliary pass's prompt where it asked for the executor's. Intermittent
// by construction, and invisible under -race because both halves were locked
// — it is a pairing bug, not a data race.
//
// So the phase is what the record is BUILT WITH rather than appended
// alongside, which is the same rule the engine's own write authority states:
// take one snapshot and decide inside it.
func (m *scriptedModel) sawRequest(phase string, req modelRequest) {
	tools := strings.Join(slices.Sorted(slices.Values(req.Tools)), "+")
	results := req.toolResults()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, phase)
	m.offered = append(m.offered, tools)
	m.systems = append(m.systems, req.System)
	m.channels = append(m.channels, req.Channel)
	m.prompts = append(m.prompts, req.Prompt)
	m.results = append(m.results, results)
}

// misrouted names every request whose system prompt came on the wrong channel:
// a seat's phase — one offering a tool only the tool loop offers — anywhere but
// the append channel, which keeps the CLI's own prompt, and an auxiliary pass
// anywhere but the replace channel, which has no use for it.
func (m *scriptedModel) misrouted() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.channels) != len(m.calls) {
		// [scriptedModel.saw] records a phase alone.
		return []string{"this model recorded calls without their channels"}
	}
	var bad []string
	for i, phase := range m.calls {
		want := ""
		switch {
		case strings.HasPrefix(phase, "aux:"):
			want = channelReplace
		case slices.ContainsFunc(strings.Split(m.offered[i], "+"), func(tool string) bool {
			return tool == "submit_work" || tool == "submit_review" || tool == "mark_onboarded"
		}):
			want = channelAppend
		}
		if want != "" && m.channels[i] != want {
			bad = append(bad, fmt.Sprintf("request %d (%s) on %q, want %q", i, phase, m.channels[i], want))
		}
	}
	return bad
}

// saw records a phase alone, for a scripted model that keeps no prompts —
// see [newSandboxModel], whose handler never recorded them.
func (m *scriptedModel) saw(phase string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, phase)
}

func (m *scriptedModel) seen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// systemPrompts is what the model was actually shown, per request.
func (m *scriptedModel) systemPrompts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.systems...)
}

// toolResults is every tool result the model was handed back, flattened.
//
// What a tool ANSWERED is only visible here: it rides the conversation — the
// CLI's own run, or the transcript a later run is started with
// ([modelRequest.toolResults]) — never the system prompt, so a test asserting
// that a tool's output reached the model has nowhere else to read it.
func (m *scriptedModel) toolResults() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, results := range m.results {
		out = append(out, results...)
	}
	return out
}

// waitBudget is how long every wait in this suite gets.
//
// # Why it is this large
//
// The conditions here are not in-process flags: they are a seat claimed
// through a lease, an engine booted, a detached sandbox run recovered from a
// row. And the machine they run on is not this one — CI runs the suite under
// the race detector, with this package's single-engine cases running beside
// each other, as many at once as the runner has cores ([testing.T.Parallel]),
// so an engine here boots while three others do. This package is alone on the
// runner ([solo.Run]); its own cases are what it competes with.
//
// # What a long budget does NOT fix
//
// A wait that will never complete. When this was raised from thirty seconds
// the reasoning was that a loaded machine missed the budget — and that was
// wrong: the restart case was losing a RACE, because its stand-in coding job
// slept for a fixed three seconds and a loaded second boot took longer, so
// recovery found a run that had already finished and the seat was never
// parked. The test then waited out whatever budget it had and failed at
// thirty-six seconds, and at ninety-six, and would have at any number.
//
// That case now holds its job open until the test releases it. The budget
// stays generous because these waits really do cover multi-second fleet
// operations on a shared runner, and it costs nothing when the condition is
// met — but a wait that times out here is a staging bug to find, not a number
// to raise.
const waitBudget = 90 * time.Second

// waitFor polls until cond holds, failing the test if it never does.
//
// diag, when given, is rendered INTO the failure. A timeout here reports that
// something the engine should have done was not done, and on CI that one line
// is the entire artefact: a run of this suite failed with nothing but "timed
// out waiting for the suspended turn to be resumed", which named the symptom
// and not one fact about the state that produced it. A condition worth waiting
// on is worth saying what it saw instead.
func waitFor(t *testing.T, what string, cond func() bool, diag ...func() string) {
	t.Helper()
	deadline := time.Now().Add(waitBudget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, d := range diag {
		t.Logf("state when %q timed out: %s", what, d())
	}
	t.Fatalf("timed out waiting for %s", what)
}

// hydrated is [engine.Engine.StateLogHydrated] — the one gate a node admits
// its seats on — as the condition a wait polls, under the test's own context.
func hydrated(t *testing.T, e *engine.Engine) func() bool {
	t.Helper()
	return func() bool { return e.StateLogHydrated(t.Context()) }
}

// turnRecordCalling is the executor's record of the turn on this node that
// called tool successfully, as this node's own event store holds it, or nil
// while it holds none.
//
// WHAT SAYS THE TURN DELIVERED rather than only acted. A script that served
// its citing submission (the last call a capture queued with
// [scriptedModel.callOnExecute]) knows what it SENT, and the engine may still
// have refused it — after which the turn ends on the rescue path, with an
// outcome the engine wrote rather than one the model chose
// ([types.AgentPhaseCompleted.RescueFired]). Only the phase record says which.
//
// n IS THE NODE WHOSE STORE HOLDS THE TURN'S RECORDS: the node that ran it,
// whose publish listener stores what it publishes — or, for a node that holds
// no data, the data member its audit trail lands on.
func turnRecordCalling(t *testing.T, n *node, tool string) *types.AgentPhaseCompleted {
	t.Helper()
	stored := n.engine.Backends().Store.Events()
	rows, err := stored.List(t.Context(), store.ListQuery{Type: "agent_phase_completed", Limit: 200})
	if err != nil {
		return nil
	}
	for _, row := range rows {
		full, err := stored.ByID(t.Context(), row.ID, time.Now())
		if err != nil {
			continue
		}
		var ev events.Event
		if json.Unmarshal(full.Payload, &ev) != nil {
			continue
		}
		record, ok := events.DataAs[*types.AgentPhaseCompleted](&ev)
		if !ok || record.Phase != types.PhaseExecute {
			continue
		}
		if slices.ContainsFunc(record.ToolExecutions, func(call types.ToolExecution) bool {
			succeeded, _ := call["success"].(bool)
			return call["name"] == tool && succeeded
		}) {
			return record
		}
	}
	return nil
}

// deliveredThrough fails the case unless the turn whose records n holds that
// delivered with tool — a post in a room, a comment on a work item — ended on
// the model's own `delivered` rather than on the rescue path
// ([turnRecordCalling]).
func deliveredThrough(t *testing.T, n *node, tool string) {
	t.Helper()
	var record *types.AgentPhaseCompleted
	waitFor(t, n.id+"'s record of the turn that delivered with "+tool, func() bool {
		record = turnRecordCalling(t, n, tool)
		return record != nil
	})
	if record.RescueFired || record.Decision != string(turn.OutcomeDelivered) {
		t.Errorf("the turn that delivered with %s ended %q (rescued: %v) after "+
			"%d rounds — its submission citing that call never landed, so the "+
			"engine wrote the outcome rather than the turn", tool, record.Decision,
			record.RescueFired, record.RoundsUsed)
	}
}
