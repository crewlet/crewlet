package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// codeHostCompany is [nativeCleanupCompany] hosting its repositories in the
// engine.
const codeHostCompany = nativeCleanupCompany + `code:
  native: true
`

// codeHostAlone is a company that keeps nothing but its repositories here: a
// state log run on `code.native` alone.
const codeHostAlone = `
name: Nimbus
tracker:
  backend: none
knowledge:
  backend: none
code:
  native: true
roles:
  - name: SWE
    handle: swe
`

// staleRepos is the code host's repository listing at the cheapest level —
// what proves a node serves the code half at all.
var staleRepos = code.ReadQuery{Asker: code.Principal{Kind: code.PrincipalOperator, OperatorID: "op-1", Seat: "ceo"}, Kind: code.ReadRepos, Fresh: statelog.Freshness{Level: statelog.ReadStale}}

// servesCodeReads waits until e's code half answers a listing through its own
// router, and answers it: a node's copy serves only once it has drained its
// log, which a test must not race.
func servesCodeReads(t *testing.T, e *Engine) code.ReadAnswer {
	t.Helper()
	var answer code.ReadAnswer
	waitUntil(t, 20*time.Second, "the code host to answer a listing", func() bool {
		var err error
		answer, err = e.router.Code().Read(t.Context(), staleRepos)
		return err == nil
	})
	return answer
}

// A COMPANY THAT SETS code.native IS SERVED ITS CODE HOST ON EVERY DATA NODE,
// and one that does not is told this node runs none.
//
// The runtime builds the committer and the lifecycle store beside the code
// log's reader where the company that started it hosts code, the estate
// backend hands them to the router, and the seat the API's routes and the host
// read through is handed out exactly there. Without code.native the code log
// still runs in the register — its reader answers the object store — but no
// operation is served: a listing is refused as a node with no backend for it,
// never answered as a company with no repositories.
func TestACompanyWithNativeCodeServesItsOperations(t *testing.T) {
	t.Parallel()
	t.Run("hosting code", func(t *testing.T) {
		t.Parallel()
		e := newSandboxNode(t, parseCompany(t, codeHostCompany))
		n := e.native.Load()
		if n == nil || n.committer == nil || n.codeStore == nil {
			t.Fatalf("a company hosting code built no committer or lifecycle store on its "+
				"data node: %+v", n)
		}
		if _, ok := OperatorCode(e); !ok {
			t.Fatal("OperatorCode answered false on a data node built with code.native")
		}
		answer := servesCodeReads(t, e)
		if len(answer.Repos) != 0 {
			t.Errorf("a company that declared no repository lists %+v", answer.Repos)
		}
		if answer.Position.Stream != (code.Domain{}).Stream().Name {
			t.Errorf("the listing answered at %v, want a position on the code log", answer.Position)
		}
	})
	t.Run("hosting none", func(t *testing.T) {
		t.Parallel()
		e := newSandboxNode(t, parseCompany(t, nativeCleanupCompany))
		n := e.native.Load()
		if n == nil || n.codeReader == nil {
			t.Fatal("the premise: the code log's reader runs in every register")
		}
		if n.committer != nil || n.codeStore != nil {
			t.Error("a company hosting no code built a committer or a lifecycle store")
		}
		if _, ok := OperatorCode(e); ok {
			t.Error("OperatorCode answered true for a company that hosts no code")
		}
		_, err := e.router.Code().Read(t.Context(), staleRepos)
		var unserved *estate.ErrUnserved
		if !errors.As(err, &unserved) || !strings.Contains(unserved.Detail, "no native backend for code.read") {
			t.Errorf("a listing on a company hosting no code answered %v, want it unserved "+
				"as a node with no code half", err)
		}
	})
}

// A GIT NODE'S PRESENCE ROW ADVERTISES ITS CACHE'S BACKGROUND BUDGET: from the
// first beat of a started node, across the lease store's JSON and back and
// beside the status rather than in it, the very figure the node's cache holds
// its maintenance to (Cache.BackgroundBudget), read from the published runtime
// — and a node without the git role, which holds no cache for a company that
// hosts code, writes no such key: it serves no git, so no git node weighs it.
// The status carries no figure of it at all, so a beat whose status read
// overran drops nothing a git node places maintenance by.
//
// Mutations: leave the node's GitBackground hook unset — the git node's row
// says nothing, red; fill it from the node's memory rather than its cache's
// budget — the two differ by what the node keeps for serving, red; hand every
// node the hook — the node without the git role says a budget, red.
func TestThePresenceAdvertisesTheCacheBudget(t *testing.T) {
	t.Parallel()
	const memory = int64(16) << 30
	t.Run("a git node", func(t *testing.T) {
		t.Parallel()
		e := bootSizedCodeNode(t, nil, memory)
		n := e.native.Load()
		if n == nil || n.gitCache == nil {
			t.Fatal("the premise: a node with every role, for a company hosting code, built no git cache")
		}
		budget := n.gitCache.BackgroundBudget()
		if budget <= 0 || budget >= memory {
			t.Fatalf("the premise: a %d-byte node's cache gives its background work %d bytes, want a part of "+
				"its memory", memory, budget)
		}
		row := presenceRow(t, e)
		if got, says := coord.GitBackgroundFromMeta(row.Meta); !says || got != budget {
			t.Errorf("the presence row advertises %d bytes of background budget (says %v), want the cache's "+
				"own %d", got, says, budget)
		}
		if status, ok := row.Meta[coord.StatusKey].(map[string]any); ok {
			if _, inside := status[coord.GitBackgroundKey]; inside {
				t.Error("the status carries the git budget too: a beat whose status overran would drop it")
			}
		}
	})
	t.Run("a node without the git role", func(t *testing.T) {
		t.Parallel()
		e := bootSizedCodeNode(t, []string{"data", "ingress", "seats", "workers"}, memory)
		if n := e.native.Load(); n == nil || n.gitCache != nil {
			t.Fatalf("the premise: a node without the git role holds no git cache (runtime %+v)", n)
		}
		if got, says := coord.GitBackgroundFromMeta(presenceRow(t, e).Meta); says {
			t.Errorf("a node without the git role advertises %d bytes of background budget, want no key", got)
		}
	})
}

// bootSizedCodeNode boots a node with roles (nil: every role) for
// [codeHostAlone], its git pool sized as a node of memory bytes rather than as
// the machine running the case, and its transfer lane pinned ([PinGitLanes]);
// it stops with t.
func bootSizedCodeNode(t *testing.T, roles []string, memory int64) *Engine {
	t.Helper()
	b := testBootstrap(t)
	dir := t.TempDir()
	b.Store.Path = filepath.Join(dir, "crewlet.db")
	b.Stream.StoreDir = filepath.Join(dir, "stream")
	b.Node.Roles = roles
	PinGitLanes(&b)
	SeedStore(t, &b)
	company := parseCompany(t, codeHostAlone)
	RequireNodeGit(t, &b, company)
	e, err := New(t.Context(), WithGitMemory(Options{Bootstrap: &b, Company: company,
		ActivatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)}, memory))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	return e
}

// presenceRow starts e and is its presence lease as every peer reads it off
// the coordination store: the first beat lands inside Start, before the
// seat host's first sweep.
func presenceRow(t *testing.T, e *Engine) coord.Lease {
	t.Helper()
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	lease, err := e.backends.Coord.Get(t.Context(), coord.NodeResource(e.id))
	if err != nil || lease == nil {
		t.Fatalf("read the node's own presence lease: %v (%v)", lease, err)
	}
	return *lease
}

// A NODE IN A MAINTENANCE MODE DECIDES NO PUSH, ITS OWN OR A PEER'S.
//
// The mode is this node's promise that it appends nothing, and a committer
// handed to the estate server would append every push the router asks it to
// decide — the node is the committer of every repository rendezvous ranks it
// first for. So it holds a committer it never hands out ([Engine.writeSide]),
// and a proposal is refused as a node that ran nothing, which the router asks
// the repository's runner-up about with the same push id. Reads are still
// answered: the mode stops writes, never the copy.
func TestAMaintenanceModeNodeRefusesAProposal(t *testing.T) {
	t.Parallel()
	e, _ := aNodeInModeOf(t, statelog.ModeMaintenance, codeHostCompany)
	if n := e.native.Load(); n == nil || n.committer == nil {
		t.Fatal("the premise: the runtime built a committer, which only the gate withholds")
	}
	servesCodeReads(t, e)
	push := code.Proposal{
		PushID: uuid.Must(uuid.NewV7()).String(), RepoID: uuid.NewString(),
		Principal: code.Principal{Kind: code.PrincipalOperator, OperatorID: "op-1", Seat: "ceo"},
		Via:       code.ViaGit, Node: e.id, ReceivedAt: time.Now().UTC(),
		Commands: []code.Command{{Ref: "refs/heads/main", Old: code.ZeroOID,
			New: "0123456789abcdef0123456789abcdef01234567"}},
	}
	if err := push.Validate(); err != nil {
		t.Fatalf("the premise: the proposal is one a committer would decide: %v", err)
	}
	res, err := e.router.Code().Propose(t.Context(), push)
	var unserved *estate.ErrUnserved
	if !errors.As(err, &unserved) || !strings.Contains(unserved.Detail, "publishes nothing") {
		t.Fatalf("a maintenance-mode node answered a proposal (%+v, %v), want it unserved "+
			"as a node that publishes nothing", res, err)
	}
}

// THE CODE LOG'S CENSUS COUNTS ITS GIT CLIENTS, WHATEVER THIS NODE HOSTS.
//
// The code log's linearizable reads are a person's or their CI's beyond the
// seats' own (a ref resolved for a seat, a read by object id's one re-sync,
// which the seats' term counts), so held to the seats' term alone a five-seat
// company's code log fires census_drift at 1 300 git reads a day — a few
// developers and one CI. The census adds the git
// reads the ceiling the log's stream HOLDS was sized for — never the Tier A
// figure this node would create it with ([stateLog.specOf]), which the census
// was handed here once and which nothing applies to a stream that exists — and
// a pass that could not read the stream's ceiling leaves the code log out
// rather than judge it by the seats' term alone.
//
// The term is the LOG'S, so a node whose runtime was built without
// code.native counts it too: the rate the census is held against is every
// node's barriers over the alarm's day, and keyed on this node's runtime the
// term judged a day of the fleet's git reads by the seats' term alone — on a
// node restarted after a revision turned the host off, and on one not yet
// restarted after a revision turned it on.
func TestTheCodeLogsCensusCountsItsGitClients(t *testing.T) {
	t.Parallel()
	const (
		ceiling = 1 << 30
		seats   = 5
	)
	week := 7 * 24 * time.Hour
	census := func(holds int64) int {
		return statelog.LinearizableReadsPerSeatDay*seats + collect.PinsPerDay +
			code.GitReadsPerDay(holds, week)
	}
	t.Run("through the report, at the ceiling the stream holds", func(t *testing.T) {
		t.Parallel()
		e, _ := aRunningNodeOf(t, codeHostCompany)
		s := e.core.Load().log
		running := s.Domain(code.Domain{}.Name())
		// THE LOG IS RESIZED UNDER THE RUNNING NODE, as `crewlet retention
		// set-capacity` resizes it: the stream now holds half of what this
		// node's Tier A sized it at, and nothing re-sizes the spec.
		holds := running.spec.MaxBytes / 2
		if err := running.log.SetMaxBytes(t.Context(), uint64(holds)); err != nil {
			t.Fatalf("resize the code log: %v", err)
		}
		// PAST TWICE THE HELD CEILING'S CENSUS AND INSIDE TWICE THE SPEC'S:
		// the alarm fires only where the census follows the stream.
		barriers := 2*census(holds) + 1
		if barriers > 2*census(running.spec.MaxBytes) {
			t.Fatalf("the premise: %d barriers a day sit inside twice the spec's census %d",
				barriers, census(running.spec.MaxBytes))
		}
		recorder, err := metrics.New()
		if err != nil {
			t.Fatalf("recorder: %v", err)
		}
		recorder.Add(metrics.StatelogBarriersApplied, uint64(barriers),
			metrics.Attrs{"domain": code.Domain{}.Name(), "stream": running.spec.Name})
		r := &retention{
			metrics: recorder, state: s, fleet: e.backends.Fleet, nodeID: e.id,
			seats: func() int { return seats }, background: e.backgroundBarriers,
		}
		want := fmt.Sprintf("%d linearizable reads a day against the %d ",
			barriers, census(holds))
		detail, _ := codeLogDrift(r.Report(t.Context()).Alarms)
		if !strings.Contains(detail, want) {
			t.Errorf("census_drift said %q, want it to hold %d barriers a day against the "+
				"%d a stream holding %d bytes was sized for (%q) — the spec's %d bytes are "+
				"not what the stream holds", detail, barriers, census(holds), holds, want,
				running.spec.MaxBytes)
		}
	})
	// THE NODE'S OWN TRIM, ON A RUNTIME THAT HOSTS NO CODE: what a node
	// restarted after a revision turned the host off runs while up to a day
	// of the fleet's git reads from before the switch are still in the
	// window, and what a node not yet restarted after a revision turned the
	// host on runs while its peers serve git.
	t.Run("through the report, on a runtime built without the code host", func(t *testing.T) {
		t.Parallel()
		recorder, err := metrics.New()
		if err != nil {
			t.Fatalf("recorder: %v", err)
		}
		e, _ := aRunningNodeWith(t, nativeCleanupCompany, Options{Metrics: recorder})
		n := e.native.Load()
		if n == nil || n.built.runsCode() {
			t.Fatal("the premise: the node runs a native runtime built without code.native")
		}
		r := e.retention.Load()
		if r == nil {
			t.Fatal("a node running the state log started no trim")
		}
		running := e.core.Load().log.Domain(code.Domain{}.Name())
		// AT A CEILING THE TEST KNOWS, so the census it expects is a figure
		// rather than whatever this machine's free disk derived.
		const holds = 1 << 30
		if err := running.log.SetMaxBytes(t.Context(), holds); err != nil {
			t.Fatalf("resize the code log: %v", err)
		}
		name := code.Domain{}.Name()
		// THE SEATS' TERM AT THE RATE THE REGISTER DECLARES for the code
		// domain, as the report counts it: left out, the census this case
		// expects is short the seats' reads the alarm adds, and the control
		// below stays inside twice the report's own.
		reads, declared := readModel(code.Domain{})
		if !declared {
			t.Fatal("the premise: the code domain declares no read model")
		}
		bare := statelog.Census{Reads: reads, Seats: r.seats(), Background: e.backgroundBarriers(name)}
		sized := bare
		sized.Clients = code.GitReadsPerDay(holds, r.cfg.MinAge())
		censusDrift := func() (string, bool) {
			return codeLogDrift(r.Report(t.Context()).Alarms)
		}
		barriers := func(count int) {
			recorder.Add(metrics.StatelogBarriersApplied, uint64(count),
				metrics.Attrs{"domain": name, "stream": running.spec.Name})
		}
		const gitReads = 20_000
		// THE PREMISE: the seats' term and the collector's pins alone fire
		// on these reads, so a quiet census is the git term's doing.
		if gitReads <= statelog.CensusDriftFactor*bare.Expected() {
			t.Fatalf("the premise: %d git reads a day sit inside twice the %d the seats "+
				"and the collector were sized for", gitReads, bare.Expected())
		}
		barriers(gitReads)
		if detail, fired := censusDrift(); fired {
			t.Errorf("census_drift fired on a node whose runtime hosts no code: %s — the "+
				"barriers it counts are the fleet's, and the code log's ceiling was sized "+
				"for its git clients whatever this node hosts", detail)
		}
		// THE CONTROL: past twice the census the log was sized for, it fires,
		// naming that census — so the quiet above is the code log judged with
		// its git term, never the code log left out.
		barriers(2*sized.Expected() + 1 - gitReads)
		want := fmt.Sprintf(" against the %d ", sized.Expected())
		if detail, _ := censusDrift(); !strings.Contains(detail, want) {
			t.Errorf("past twice its census the code log's alarm said %q, want it to "+
				"name the %d its ceiling was sized for (%q)", detail, sized.Expected(), want)
		}
	})
	for name, tc := range map[string]struct {
		holds    uint64
		read     bool
		expected int
		sized    bool
	}{
		"the ceiling the code log's stream holds": {
			holds: ceiling, read: true, expected: census(ceiling), sized: true,
		},
		"a pass that could not read the code log's ceiling": {
			holds: 0, read: false, expected: 0, sized: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			recorder, err := metrics.New()
			if err != nil {
				t.Fatalf("recorder: %v", err)
			}
			spec := code.Domain{}.Stream()
			// A SPEC UNLIKE THE CEILING HELD: the census reads the
			// stream's, so this figure must change nothing.
			spec.MaxBytes = 64 << 30
			recorder.Add(metrics.StatelogBarriersApplied, 20_000,
				metrics.Attrs{"domain": code.Domain{}.Name(), "stream": spec.Name})
			e := &Engine{objects: &objectStore{}}
			running := &runningLog{domain: code.Domain{}, spec: spec}
			r := &retention{
				metrics: recorder,
				state:   runsLogs(&stateLog{}, running),
				seats:   func() int { return seats }, background: e.backgroundBarriers,
			}
			c, counted := r.census([]*runningLog{running})[code.Domain{}.Name()]
			if !counted {
				t.Fatal("the code log has no census half: a log a read appends to is counted")
			}
			expected, sized := c.expectedAt(tc.holds, tc.read, week)
			if sized != tc.sized || expected != tc.expected {
				t.Errorf("the census is sized %v expecting %d a day, want sized %v expecting %d",
					sized, expected, tc.sized, tc.expected)
			}
			if c.reads != 20_000 {
				t.Errorf("the census counted %d barriers on the code log's stream, want 20000",
					c.reads)
			}
			// AND THE LOG'S OWN READING, as the report hands it on: a census
			// the pass could not size is left out, never judged by its seats.
			reading := statelog.Reading{}
			if sized {
				reading.LinearizableReads, reading.LinearizableReadsExpected = c.reads, expected
			}
			fired := false
			for _, alarm := range statelog.Evaluate(reading) {
				fired = fired || alarm.Kind == statelog.KindCensusDrift
			}
			if fired {
				t.Errorf("census_drift fired at 20 000 git reads a day against %d, want quiet",
					expected)
			}
		})
	}
	// NO OTHER LOG declares readers beyond the seats and the engine's own
	// passes.
	for _, d := range registeredDomains() {
		isCode := d.Name() == code.Domain{}.Name()
		if declares := clientBarriers(d.Name()) != nil; declares != isCode {
			t.Errorf("the %s log's census has a client term: %v, want one on the code "+
				"log alone", d.Name(), declares)
		}
	}
}

// codeLogDrift is the code log's own entry under a report's census_drift
// alarm: its detail, and whether the alarm lists the code log at all. The
// alarm is PER LOG ([statelog.AlarmDomain]), so the alarm's own detail is
// whichever log is furthest past its census and names no log.
func codeLogDrift(alarms []statelog.Alarm) (string, bool) {
	for _, alarm := range alarms {
		if alarm.Kind != statelog.KindCensusDrift {
			continue
		}
		for _, d := range alarm.Domains {
			if slices.Contains(d.Domains, (code.Domain{}).Name()) {
				return d.Detail, true
			}
		}
	}
	return "", false
}

// codeHostLimited is [codeHostCompany] with every code.limits field set, a
// retention of a year, and merges held to maintainers at least.
const codeHostLimited = nativeCleanupCompany + `code:
  native: true
  deleted_retention_days: 365
  merge_by_floor: maintainers
  limits:
    max_push_bytes: 67108864
    max_blob_bytes: 1048576
    repo_max_bytes: 2147483648
    pushes_per_seat_day: 40
`

// A REVISION THAT TURNS THE CODE HOST OFF LEAVES THE RUNNING HOST ITS SETTINGS
// — the committer its limits and the retention duty its retention.
//
// `code.limits` and `code.deleted_retention_days` without `code.native` are
// refused, so the revision that turns the host off must drop them too — and a
// node whose runtime was built with the host keeps serving it until a restart
// ([backendSwitches]). Read off that revision, every push the still-running
// committer decided was held to the defaults: a company that had capped a
// repository at 2 GiB, a push at 64 MiB and a seat at 40 pushes a day was
// handed 20 GiB repositories, 1 GiB pushes and a thousand pushes a seat on a
// host it had just asked to wind down — and one holding merges to maintainers
// would have its floor dropped to writers — and the still-running retention duty
// would purge, by the default week, a repository the company had chosen to
// keep restorable for a year. The settings in force are the newest activated
// revision's that hosted code, carried from epoch to epoch; a revision that
// hosts code again brings its own.
func TestTheCodeSettingsOutliveTheRevisionThatTurnsTheHostOff(t *testing.T) {
	t.Parallel()
	e := newSandboxNode(t, parseCompany(t, codeHostLimited))
	capped := code.Limits{MaxPushBytes: 64 << 20, MaxBlobBytes: 1 << 20, RepoMaxBytes: 2 << 30,
		PushesPerSeatDay: 40, MergeByFloor: code.MergeByMaintainers}
	year := 365 * 24 * time.Hour
	if got := e.codeLimits(); got != capped {
		t.Fatalf("the premise: a company hosting code with limits is held to %+v, want %+v",
			got, capped)
	}
	if got := e.codeDeletedRetention(); got != year {
		t.Fatalf("the premise: a company keeping deleted repositories a year purges after %v", got)
	}
	applyOK(t, e, nativeCleanupCompany)
	if n := e.native.Load(); n == nil || n.committer == nil || !n.built.runsCode() {
		t.Fatal("the premise: the runtime built with the code host keeps its committer " +
			"until a restart")
	}
	if got := e.codeLimits(); got != capped {
		t.Errorf("after a revision turned the code host off, the committer it leaves "+
			"running is held to %+v, want the last hosting revision's %+v", got, capped)
	}
	if got := e.codeDeletedRetention(); got != year {
		t.Errorf("after a revision turned the code host off, the retention duty it leaves "+
			"running purges after %v, want the last hosting revision's %v", got, year)
	}
	// A LATER REVISION THAT CHANGES SOMETHING ELSE carries them on.
	applyOK(t, e, strings.Replace(nativeCleanupCompany, "name: Acme", "name: Acme Co", 1))
	if got := e.codeLimits(); got != capped {
		t.Errorf("a second revision without the code host held the committer to %+v, "+
			"want %+v carried on", got, capped)
	}
	if got := e.codeDeletedRetention(); got != year {
		t.Errorf("a second revision without the code host purges after %v, want %v carried on",
			got, year)
	}
	applyOK(t, e, codeHostCompany)
	if got := e.codeLimits(); got != code.DefaultLimits() {
		t.Errorf("a revision hosting code again with no limits is held to %+v, want the "+
			"defaults %+v", got, code.DefaultLimits())
	}
	if got, want := e.codeDeletedRetention(), (config.Code{}).DeletedRetention(); got != want {
		t.Errorf("a revision hosting code again with no retention purges after %v, want the "+
			"default %v", got, want)
	}
}
