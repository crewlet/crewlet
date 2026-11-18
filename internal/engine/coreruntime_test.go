package engine_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
)

// A NODE WITH NO COMPANY RUNS THE CORE RUNTIME FROM BOOT: every domain's log,
// the identity estate, the node gate and the duties that keep them, and none of
// the native halves only a company asks for — asserted on a running node before
// any company is applied (package doc, "The core runtime").
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
		t.Fatal("a node with no company holds no identity estate, so nobody " +
			"can sign in — not even the Tier A token's session the company is " +
			"created through")
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
	// The register's own list, never a literal, which goes stale the day a
	// domain joins.
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
