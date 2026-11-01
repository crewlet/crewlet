package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/objstore/references"
	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// countingClaim is a duty's claim that counts how often it was asked and
// answers NOT HELD, so a turn that claims stops right there and no pass,
// embedding or trim needs its real inputs — and its give-back, which counts
// how often the duty was given back.
type countingClaim struct{ asked, gaveBack atomic.Int64 }

func (c *countingClaim) claim(context.Context) (bool, error) {
	c.asked.Add(1)
	return false, nil
}

func (c *countingClaim) giveBack(context.Context) (bool, error) {
	c.gaveBack.Add(1)
	return true, nil
}

// wrongCopyNode is an engine whose verdict on its copies finds the named
// domain's wrong and every other domain's in service.
func wrongCopyNode(domain string) *Engine {
	e := &Engine{backends: &Backends{}}
	withCore(e)
	e.native.Store(&native{trackerReader: &worklet.Reader{}})
	e.local = localWith(e, time.Now, func(context.Context, *core) copyVerdict {
		return wrongOn(domain)
	})
	return e
}

// everyRegisteredLog is a state log running every registered domain, as the
// trim walks it.
func everyRegisteredLog() *stateLog {
	var runs []*runningLog
	for _, d := range registeredDomains() {
		runs = append(runs, &runningLog{domain: d, spec: d.Stream()})
	}
	return &stateLog{logs: newLogSet(runs)}
}

// trimCounts is every registered domain, in the register's order, whose log
// the trim counts something of from this node's own rows: the logs that claim
// identity, whose evictions it counts, and the logs that write anchors, whose
// anchors it counts. Read off the register rather than listed, because the
// set grows with every domain that claims identity — the identity estate, the
// artifacts and their datastores among them.
func trimCounts() []string {
	var out []string
	for _, d := range registeredDomains() {
		if d.ClaimsIdentity() || statelog.WritesAnchors(d.Stream()) {
			out = append(out, d.Name())
		}
	}
	return out
}

// collectorReferences is the collector's references as the engine builds them,
// from the declared tables over every estate a data node offers
// ([collectorEstates]).
func collectorReferences(t *testing.T) collect.References {
	t.Helper()
	refs, err := collect.Sources(references.All, collectorEstates()...)
	if err != nil {
		t.Fatalf("the collector's references: %v", err)
	}
	return refs
}

// A DUTY DECLINES WHILE A COPY IT DECIDES FROM IS OUT OF SERVICE — ADR-0072.
//
// A fleet-singleton duty that decides from a domain's rows decides for the
// whole fleet: the embedding duty publishes the vectors every node applies —
// and withdraws the vector of every task its tracker copy no longer shows —
// the trim purges past the evictions its own rows hold, and the collector
// deletes what no row of its copy names. On a holder whose copy of any of
// those domains is wrong, each acted on rows the node had stopped serving. So
// a node whose copy of a domain the duty decides from is out of service does
// not claim it and GIVES IT BACK, so a node whose copies serve takes it at its
// next tick rather than once the lease lapses — the trim's is 45 minutes; a
// wrong copy of a domain the duty does not read costs it nothing.
func TestADutyDeclinesWhileACopyItDecidesFromIsOutOfService(t *testing.T) {
	t.Parallel()
	refs := collectorReferences(t)
	duties := map[string]struct {
		decidesFrom []string
		turn        func(t *testing.T, e *Engine, claim *countingClaim)
	}{
		"the embedding duty": {
			decidesFrom: []string{vectorsName, trackerName, pagesName},
			turn: func(t *testing.T, e *Engine, claim *countingClaim) {
				d := &embedDuty{engine: e, claim: claim.claim, giveBack: claim.giveBack,
					copies:  e.copyOutOfService,
					renewal: newDutyRenewal(embedDutyName, embedDutyTTL),
					corpora: []search.Corpus{search.TaskCorpus{}, search.PageCorpus{}}}
				d.tick(t.Context())
			},
		},
		"the trim": {
			decidesFrom: trimCounts(),
			turn: func(t *testing.T, e *Engine, claim *countingClaim) {
				r := &retention{state: everyRegisteredLog(), nodeID: "node-a", claim: claim.claim,
					giveBack: claim.giveBack, copies: e.copyOutOfService}
				r.tick(t.Context())
			},
		},
		"the object collector": {
			// Every domain a Required table is declared in: a person's
			// conversation files are Ask Crewlet's.
			decidesFrom: []string{trackerName, codeName, artifactsdomain.Domain{}.Name(),
				assistantdomain.Domain{}.Name()},
			turn: func(t *testing.T, e *Engine, claim *countingClaim) {
				e.collectorTurn(t.Context(), claim.claim,
					newCollectorDuty(nil, copiesNamedBy(refs), claim.giveBack))
			},
		},
		"the code-retention duty": {
			decidesFrom: []string{codeName},
			turn: func(t *testing.T, e *Engine, claim *countingClaim) {
				d := &codeRetentionDuty{claim: claim.claim, giveBack: claim.giveBack,
					copies:  e.copyOutOfService,
					renewal: newDutyRenewal(codeRetentionDutyName, codeRetentionDutyTTL)}
				d.turn(t.Context())
			},
		},
	}
	for name, duty := range duties {
		for _, d := range registeredDomains() {
			domain := d.Name()
			t.Run(name+"/"+domain, func(t *testing.T) {
				t.Parallel()
				claim := &countingClaim{}
				duty.turn(t, wrongCopyNode(domain), claim)
				decides := slices.Contains(duty.decidesFrom, domain)
				switch asked, gaveBack := claim.asked.Load(), claim.gaveBack.Load(); {
				case decides && asked != 0:
					t.Fatalf("%s claimed its duty %d time(s) on a node whose %s copy is "+
						"out of service — it decides from that domain's rows", name, asked, domain)
				case decides && gaveBack != 1:
					t.Fatalf("%s declined and gave its duty back %d time(s), want once — "+
						"left to lapse, the lease keeps it from every node for its TTL",
						name, gaveBack)
				case !decides && asked != 1:
					t.Fatalf("%s claimed its duty %d time(s) on a node whose %s copy is "+
						"out of service, want once — it reads nothing of that domain", name, asked, domain)
				case !decides && gaveBack != 0:
					t.Fatalf("%s gave its duty back over a copy it does not read", name)
				}
			})
		}
	}
}

// EVERY DUTY NAMES THE DOMAINS IT DECIDES FROM, each a registered domain: the
// embedding duty the vectors and the domain of every corpus the engine embeds,
// the trim every log whose evictions or anchors it counts from this node's
// rows, and the collector every domain
// whose rows name its objects. A corpus whose source maps to no domain, or a
// declaration in a domain this list missed, would be a duty deciding from a
// copy nobody asked the verdict of.
func TestEveryDutyNamesTheDomainsItDecidesFrom(t *testing.T) {
	t.Parallel()
	registered := registeredNames()
	db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })
	e := &Engine{backends: &Backends{Store: db}}
	embed := (&embedDuty{corpora: e.corpora(db.Replicated().Reader())}).decidesFrom()
	if want := []string{vectorsName, trackerName, pagesName}; !sameNames(embed, want) {
		t.Errorf("the embedding duty decides from %v, want %v", embed, want)
	}

	counted := trimCounts()
	if trim := (&retention{state: everyRegisteredLog()}).decidesFrom(); !slices.Equal(trim, counted) {
		t.Errorf("the trim decides from %v, want the logs whose evictions or "+
			"anchors it counts, %v", trim, counted)
	}
	// A LOG THAT WRITES ANCHORS AND CLAIMS NO IDENTITY, which no registered
	// domain is today: the trim still counts its anchors from this node's
	// rows, so it still decides from its copy.
	anchored := anchorsWithoutIdentity{worklet.Domain{}}
	only := &stateLog{logs: newLogSet([]*runningLog{{domain: anchored, spec: anchored.Stream()}})}
	if trim := (&retention{state: only}).decidesFrom(); !slices.Equal(trim, []string{anchored.Name()}) {
		t.Errorf("the trim decides from %v over a log that writes anchors and claims "+
			"no identity, want %s — it counts that log's anchors from this node's rows",
			trim, anchored.Name())
	}

	var named []string
	for _, estate := range collectorEstates() {
		named = append(named, estate.Name())
	}
	if collector := copiesNamedBy(collectorReferences(t)); !sameNames(collector, named) {
		t.Errorf("the collector decides from %v, want the domains whose rows name objects, %v",
			collector, named)
	}

	// THE CODE-RETENTION DUTY PURGES BY THE CODE LOG'S ROWS, and by nothing
	// else: what is due and what a purge step removes are both read there.
	retention := (&codeRetentionDuty{}).decidesFrom()
	if want := []string{codeName}; !slices.Equal(retention, want) {
		t.Errorf("the code-retention duty decides from %v, want %v", retention, want)
	}

	for duty, names := range map[string][]string{
		"embedding": embed, "collector": named, "code-retention": retention,
	} {
		for _, name := range names {
			if !slices.Contains(registered, name) {
				t.Errorf("the %s duty decides from %q, which is no registered domain", duty, name)
			}
		}
	}
}

// anchorsWithoutIdentity is a domain whose log writes arbitration anchors and
// which claims no identity.
type anchorsWithoutIdentity struct{ statelog.Domain }

func (anchorsWithoutIdentity) ClaimsIdentity() bool { return false }

// sameNames is whether two lists name the same domains, in any order.
func sameNames(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// A DECLINED DUTY IS ANNOUNCED ONCE, AND SO IS ITS RESUMPTION: a tick that
// declines while the last one did says nothing new, so a copy out of service
// for an hour is one line rather than sixty, and the line that ends it names
// the domain it waited on.
func TestADeclinedDutyIsAnnouncedOnceAndResumes(t *testing.T) {
	t.Parallel()
	var verdict atomic.Pointer[copyVerdict]
	set := func(v copyVerdict) { verdict.Store(&v) }
	set(wrongOn(trackerName))
	e := &Engine{backends: &Backends{}}
	withCore(e)
	e.native.Store(&native{trackerReader: &worklet.Reader{}})
	e.local = localWith(e, time.Now, func(context.Context, *core) copyVerdict { return *verdict.Load() })
	rejudge := func() {
		e.local.mu.Lock()
		e.local.slot = verdictSlot{}
		e.local.mu.Unlock()
	}
	var mu sync.Mutex
	var logs bytes.Buffer
	claim := &countingClaim{}
	d := &embedDuty{engine: e, claim: claim.claim, copies: e.copyOutOfService,
		renewal: newDutyRenewal(embedDutyName, embedDutyTTL),
		corpora: []search.Corpus{search.TaskCorpus{}, search.PageCorpus{}}}
	d.declines.log = slog.New(slog.NewJSONHandler(&lockedWriter{mu: &mu, w: &logs}, nil))
	lines := func() map[string][]string {
		mu.Lock()
		defer mu.Unlock()
		out := map[string][]string{}
		scanner := bufio.NewScanner(bytes.NewReader(logs.Bytes()))
		for scanner.Scan() {
			var line struct{ Msg, Domain string }
			if err := json.Unmarshal(scanner.Bytes(), &line); err == nil {
				out[line.Msg] = append(out[line.Msg], line.Domain)
			}
		}
		return out
	}

	for range 2 {
		if report := d.tick(t.Context()); report.declined != trackerName {
			t.Fatalf("a tick over a wrong tracker copy declined %q", report.declined)
		}
	}
	if got := lines(); !slices.Equal(got["embed_duty_declined"], []string{trackerName}) ||
		len(got["embed_duty_resumed"]) != 0 {
		t.Fatalf("two declined ticks logged %v, want one decline naming %s", got, trackerName)
	}
	if claim.asked.Load() != 0 {
		t.Fatal("a declined tick claimed the duty")
	}

	set(soundCopy())
	rejudge()
	if report := d.tick(t.Context()); report.declined != "" {
		t.Fatalf("a tick over copies in service declined %q", report.declined)
	}
	if got := lines(); !slices.Equal(got["embed_duty_resumed"], []string{trackerName}) ||
		len(got["embed_duty_declined"]) != 1 {
		t.Fatalf("the tick after the copy recovered logged %v, want one resumption naming %s",
			got, trackerName)
	}
	if claim.asked.Load() != 1 {
		t.Fatal("the tick after the copy recovered did not claim the duty")
	}
}

// lockedWriter serialises writes to w, which a handler shared by the tick and
// its renewals may make from two goroutines.
type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// A TICK STOPS WHEN A COPY IT DECIDES FROM GOES OUT OF SERVICE under it, and
// gives the duty back once it has RETURNED: a training tick runs for minutes,
// and a renewal that only asked whether the lease was still held would carry
// on deciding from a copy that had gone wrong since the tick began. The
// renewal asks the verdict first, cuts the tick off — mid-request — without
// renewing, and the tick gives the lease back as a declined tick does, once
// the request it had in flight has ended, never from the renewal: a publish
// already on its way would otherwise land after another node took the duty.
// The kept duty's own cases certify the renewal; this one holds the tick's
// wiring to it.
func TestATickStopsWhenACopyItDecidesFromGoesOutOfService(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNodeOf(t, embeddingDutyCompany)
	e.stopEmbedding()
	provider := &pacedEmbedder{Fake: embeddings.NewFake(64)}
	var held embeddings.Embedder = provider
	e.embeddings.Store(&held)
	seedTasks(t, e, 1)
	duty := e.newEmbedDuty(e.core.Load().log)
	if duty == nil {
		t.Fatal("a node with the vector log and a corpus built no duty")
	}
	clock := newFakeDutyClock()
	duty.renewal.clock = clock
	var wrong atomic.Bool
	duty.copies = func(_ context.Context, domains ...string) (string, bool) {
		if wrong.Load() && slices.Contains(domains, pagesName) {
			return pagesName, true
		}
		return "", false
	}
	duty.declines.log = slog.New(slog.DiscardHandler)
	claim := newScriptedClaim(answer{held: true})
	duty.claim = claim.claim
	var requestEnded atomic.Bool
	var gaveBack atomic.Int64
	duty.giveBack = func(context.Context) (bool, error) {
		if !requestEnded.Load() {
			t.Error("the duty was given back while the request the tick had in flight was running")
		}
		gaveBack.Add(1)
		return true, nil
	}
	called, cut := make(chan struct{}, 1), make(chan error, 1)
	provider.set(func(ctx context.Context) error {
		select {
		case called <- struct{}{}:
		default:
		}
		<-ctx.Done()
		requestEnded.Store(true)
		select {
		case cut <- context.Cause(ctx):
		default:
		}
		return context.Cause(ctx)
	}, false)

	done := make(chan tickReport, 1)
	go func() { done <- duty.tick(t.Context()) }()
	if a := claim.await(t); !a.held {
		t.Fatalf("the turn's claim answered %+v, want held", a)
	}
	select {
	case <-called:
	case r := <-done:
		t.Fatalf("the tick returned %+v before it asked the provider", r)
	case <-time.After(10 * time.Second):
		t.Fatal("the provider was never asked")
	}
	wrong.Store(true)
	clock.tick(t, duty.renewal.every())
	select {
	case why := <-cut:
		if !errors.Is(why, errDutyDeclined) {
			t.Fatalf("the provider's request was cut off by %v, want %v", why, errDutyDeclined)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a copy the duty decides from went out of service, and the provider's request ran on")
	}
	select {
	case r := <-done:
		if r.published != 0 {
			t.Fatalf("a tick cut off for a copy out of service published %d source(s)", r.published)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the tick never returned")
	}
	if n := claim.count(); n != 1 {
		t.Fatalf("the duty was asked for %d times, want the tick's claim alone: a renewal "+
			"over a copy out of service renews nothing", n)
	}
	if n := gaveBack.Load(); n != 1 {
		t.Fatalf("the tick cut off for a copy out of service gave the duty back %d time(s), "+
			"want once — left to lapse, the lease keeps it from every node for its TTL", n)
	}
}
