package api

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/estate"
	queuememory "github.com/crewlet/crewlet/internal/queue/memory"
)

// movingOperators is an operator surface over facts a case moves, counting the
// catalogues it builds — each a server of its own, so which one answered says
// which build it came from.
type movingOperators struct {
	facts  atomic.Pointer[operatorBasis]
	reads  atomic.Int64
	builds atomic.Int64
	// fail, while set, refuses a build.
	fail atomic.Bool
	// hold, where set, is waited on by every build after the first.
	hold chan struct{}
	// stall, once armed, holds the next reading of the facts after it has
	// taken them and before it hands them back: the reading closes read and
	// waits for release.
	stall atomic.Pointer[stalledRead]
}

// stalledRead is one reading of the facts held between taking them and
// handing them back.
type stalledRead struct{ read, release chan struct{} }

func (m *movingOperators) move(basis operatorBasis) { m.facts.Store(&basis) }

func (m *movingOperators) surface(t *testing.T) *EngineOperators {
	t.Helper()
	m.move(operatorBasis{})
	o, err := newOperators(
		func() operatorBasis {
			m.reads.Add(1)
			facts := *m.facts.Load()
			if stalled := m.stall.Swap(nil); stalled != nil {
				close(stalled.read)
				<-stalled.release
			}
			return facts
		},
		func(basis operatorBasis, listeners *operator.Listeners) (*operator.Server, error) {
			if m.builds.Add(1) > 1 && m.hold != nil {
				<-m.hold
			}
			if m.fail.Load() {
				return nil, errors.New("refused")
			}
			return steering(basis, listeners)
		})
	if err != nil {
		t.Fatalf("the surface: %v", err)
	}
	return o
}

// steering is a catalogue built afresh at every call — a whole server of its
// own, keeping its listeners' streams in listeners — of one tool, and of a
// second, [builtin.AnswerRunTool], where the facts name a knowledge backend:
// what a listing says tells which facts the catalogue answering it was built
// from.
func steering(basis operatorBasis, listeners *operator.Listeners) (*operator.Server, error) {
	opts := operator.Options{
		Audit: queuememory.New(),
		Steer: builtin.SteerDeps{
			Asker: struct{ builtin.FleetAsker }{}, Actor: operator.WorkActor(nil, nil),
		},
		Listeners: listeners,
	}
	if basis.knowledge {
		opts.Runs = builtin.RunDeps{
			Desk: struct{ builtin.RunDesk }{}, Actor: operator.WorkActor(nil, nil),
		}
	}
	return operator.New(opts)
}

// serving is the catalogue o answers now, failing the case where it answers
// none.
func serving(t *testing.T, o *EngineOperators) *operator.Server {
	t.Helper()
	server, ok := o.Operator()
	if !ok || server == nil {
		t.Fatal("the surface answered no catalogue")
	}
	return server
}

// THE CATALOGUE IS BUILT AGAIN WHEN, AND ONLY WHEN, A FACT IT IS BUILT FROM
// MOVES — each of them on its own. A request that finds them where the current
// catalogue was built is served by it with no build at all (a request costs a
// reading, never a catalogue); one that finds any of them moved is served by a
// catalogue built from the facts it found. The facts are the halves the node
// serves (each read apart from its writer), the ranked search, the knowledge
// backend and the company's name, which titles the MCP server — a catalogue
// keyed on fewer kept serving what it was built from after the one it left out
// moved.
func TestTheOperatorCatalogueIsBuiltAgainWhenAFactItIsBuiltFromMoves(t *testing.T) {
	t.Parallel()
	writer := func(estate.Actor) estate.WorkWriter { return estate.WorkWriter{} }
	steps := []struct {
		name  string
		basis operatorBasis
	}{
		{"the tracker's reads arrive",
			operatorBasis{reads: engine.OperatorReads{Tracker: true}}},
		{"its writer beside them",
			operatorBasis{reads: engine.OperatorReads{Tracker: true}, workWriter: writer}},
		{"the ranked search",
			operatorBasis{reads: engine.OperatorReads{Tracker: true, Search: true},
				workWriter: writer}},
		{"the knowledge base's reads",
			operatorBasis{reads: engine.OperatorReads{Tracker: true, Search: true, Wiki: true},
				workWriter: writer}},
		{"its writer beside them",
			operatorBasis{reads: engine.OperatorReads{Tracker: true, Search: true, Wiki: true},
				workWriter: writer, pagesWritable: true}},
		{"a knowledge backend",
			operatorBasis{reads: engine.OperatorReads{Tracker: true, Search: true, Wiki: true},
				workWriter: writer, pagesWritable: true, knowledge: true}},
		{"the company's name",
			operatorBasis{reads: engine.OperatorReads{Tracker: true, Search: true, Wiki: true},
				workWriter: writer, pagesWritable: true, knowledge: true, company: "Acme"}},
		{"a rename",
			operatorBasis{reads: engine.OperatorReads{Tracker: true, Search: true, Wiki: true},
				workWriter: writer, pagesWritable: true, knowledge: true, company: "Acme Inc"}},
		{"the knowledge backend retired",
			operatorBasis{reads: engine.OperatorReads{Tracker: true, Search: true, Wiki: true},
				workWriter: writer, pagesWritable: true, company: "Acme Inc"}},
	}
	m := &movingOperators{}
	o := m.surface(t)
	before := serving(t, o)
	for i, step := range steps {
		m.move(step.basis)
		got := serving(t, o)
		if got == before {
			t.Errorf("%s: the catalogue built before it still answers", step.name)
		}
		// AND THE FACTS STAYING PUT BUILD NOTHING MORE.
		for range 3 {
			if again := serving(t, o); again != got {
				t.Errorf("%s: a request finding the same facts was answered by "+
					"another catalogue", step.name)
			}
		}
		if want := int64(i + 2); m.builds.Load() != want {
			t.Errorf("%s: %d catalogues built, want %d — one at boot and one each "+
				"time a fact moved", step.name, m.builds.Load(), want)
		}
		before = got
	}
}

// A BUILD THAT FAILS KEEPS THE CATALOGUE IT WOULD HAVE REPLACED, which is
// still a correct one for what it offers, and is not tried again — nor logged
// again — until the facts move once more: a request per second retrying a
// refused build would write an error line per request.
func TestAFailedBuildKeepsTheCatalogueItWouldHaveReplaced(t *testing.T) {
	t.Parallel()
	m := &movingOperators{}
	o := m.surface(t)
	first := serving(t, o)

	m.fail.Store(true)
	m.move(operatorBasis{knowledge: true})
	for range 3 {
		if got := serving(t, o); got != first {
			t.Error("a refused build left the surface answering another catalogue")
		}
	}
	if m.builds.Load() != 2 {
		t.Errorf("%d builds, want the one at boot and ONE refused — the same facts "+
			"are not built again", m.builds.Load())
	}

	m.fail.Store(false)
	m.move(operatorBasis{knowledge: true, company: "Acme"})
	if got := serving(t, o); got == first {
		t.Error("the facts moved again and the surface did not build")
	}
}

// ONE BUILD SERVES EVERY REQUEST THAT FOUND THE SAME FACTS MOVED. Every request
// asks the surface, so a node's first apply is met by however many requests
// are in flight at that moment — each of which would otherwise build a
// catalogue of its own, and all but the last thrown away.
func TestOneBuildServesEveryRequestThatFoundTheFactsMoved(t *testing.T) {
	t.Parallel()
	const requests = 16
	m := &movingOperators{hold: make(chan struct{})}
	o := m.surface(t)
	m.move(operatorBasis{knowledge: true})
	readsBefore := m.reads.Load()

	var wg sync.WaitGroup
	got := make([]*operator.Server, requests)
	for i := range requests {
		wg.Go(func() { got[i], _ = o.Operator() })
	}
	// EVERY REQUEST HAS READ THE MOVED FACTS before the one build is let
	// go, so each of them finds a build to wait on rather than a catalogue.
	deadline := time.Now().Add(10 * time.Second)
	for m.reads.Load()-readsBefore < requests {
		if time.Now().After(deadline) {
			t.Fatal("the requests never read the facts")
		}
		time.Sleep(time.Millisecond)
	}
	close(m.hold)
	wg.Wait()

	if m.builds.Load() != 2 {
		t.Errorf("%d requests that found the same facts moved built %d catalogues, "+
			"want one", requests, m.builds.Load()-1)
	}
	for i, server := range got {
		if server != got[0] {
			t.Errorf("request %d was served by another catalogue than request 0", i)
		}
	}
}

// A READING OLDER THAN THE CATALOGUE NEVER REPLACES IT. A request reads the
// facts before it takes the rebuild lock, and may be held there while the facts
// move again and another request builds the catalogue for where they moved to.
// Built from its own reading, the held request put a catalogue of the OLDER
// facts back over the newer one — a build nothing needed, the next request
// building the newer catalogue a second time, and every MCP client between the
// two served a catalogue the node no longer runs. So the facts are read again
// under the lock, where builds are serialised, and a catalogue is only ever
// replaced by one built from facts read after it was.
func TestAReadingOlderThanTheCatalogueNeverReplacesIt(t *testing.T) {
	t.Parallel()
	m := &movingOperators{}
	o := m.surface(t)
	older := operatorBasis{knowledge: true}
	newer := operatorBasis{knowledge: true, company: "Acme"}

	// THE HELD REQUEST takes the older facts and is held before it compares
	// them with the catalogue's.
	m.move(older)
	stalled := &stalledRead{read: make(chan struct{}), release: make(chan struct{})}
	m.stall.Store(stalled)
	held := make(chan *operator.Server, 1)
	go func() {
		server, _ := o.Operator()
		held <- server
	}()
	select {
	case <-stalled.read:
	case <-time.After(10 * time.Second):
		t.Fatal("the held request never read the facts")
	}

	// MEANWHILE the facts move on, and another request builds for them.
	m.move(newer)
	built := serving(t, o)

	close(stalled.release)
	var late *operator.Server
	select {
	case late = <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the held request never answered")
	}

	if got := o.current.Load().key; got != newer.key() {
		t.Errorf("the catalogue serving is keyed %+v, want the newer facts %+v: "+
			"a request holding an older reading built over a newer catalogue",
			got, newer.key())
	}
	if late != built {
		t.Error("the held request was answered by another catalogue than the one " +
			"built for the facts as they are")
	}
	if m.builds.Load() != 2 {
		t.Errorf("%d catalogues built, want 2 — the one at boot and the one for "+
			"the newer facts", m.builds.Load())
	}
}
