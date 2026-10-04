package engine_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
)

// A NODE WITH NO COMPANY RUNS THE CORE RUNTIME FROM BOOT: every domain's log,
// the identity estate, the node gate and the duties that keep them — and none
// of the native halves only a company can ask for.
//
// # Why the line is here and not at the first company
//
// The whole of a node's durable state used to wait for its first company, so a
// node started with none — the quickstart's own route — had no identity
// estate: nobody could be invited or sign in. Everything here is asserted
// before any company is applied, on a running node.
//
// # And why it is every domain, the tracker's and the knowledge base's too
//
// A join replaces the whole replicated file and a snapshot names every
// registered domain, so a node running part of the register can neither adopt
// nor donate; and the trim counts nodes per log, so a node not running the
// tracker's log would pin that log's trim for as long as it was up. What waits
// for the company is only the WRITERS and READERS over those logs.
//
// Mutations, each run: start the core with the native halves at the first
// company and every accessor below is nil; arm the core's duties with the
// native ones and the trim and the identity duties are absent here; build the
// core from part of the register and the status rows name fewer domains.
func TestANodeWithNoCompanyRunsTheCoreRuntime(t *testing.T) {
	t.Parallel()
	e := unconfiguredEngine(t)
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if e.Company() != nil {
		t.Fatal("the premise: this node has no company")
	}

	if e.IAM() == nil || e.IAMWriter() == nil {
		t.Fatal("a node with no company holds no identity estate, so its first " +
			"person has nowhere to be invited or to sign in")
	}
	if e.NodeGate() == nil {
		t.Fatal("a node with no company holds no node gate, so it cannot be " +
			"evicted from the logs it is counted on")
	}

	var domains []string
	for _, row := range e.StateLogStatus(t.Context()) {
		domains = append(domains, row.Name)
	}
	slices.Sort(domains)
	// THE REGISTER'S OWN LIST, never a literal: a literal of five outlived
	// the usage log joining the register, and asserted the node was missing
	// a domain it runs.
	var want []string
	for _, d := range engine.Domains() {
		want = append(want, d.Name())
	}
	slices.Sort(want)
	if !slices.Equal(domains, want) {
		t.Errorf("a node with no company applies the logs %v, want every "+
			"registered domain %v", domains, want)
	}

	if _, ok := e.RetentionReport(t.Context()); !ok {
		t.Error("a node with no company runs no trim, so the identity log it " +
			"is written from only grows")
	}
	if len(e.IdentityDuties()) == 0 {
		t.Error("a worker node with no company armed no identity duty")
	}
	jobs := e.Maintenance().Jobs()
	for _, job := range []string{"iam_ops", "tracker_ops"} {
		if !slices.Contains(jobs, job) {
			t.Errorf("the sweep on a node with no company runs %v, which does not "+
				"include %s", jobs, job)
		}
	}

	if e.NativeStarted() || e.TrackerWriter() != nil || e.PagesStore() != nil ||
		e.Tracker() != nil || e.Pages() != nil {
		t.Error("a node with no company started the native halves, whose " +
			"existence is the company's to say")
	}
}
