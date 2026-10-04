package estate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A GATE RECORD GOES TO ITS OWN LOG'S PARTITION, AND NAMES A LOG OF THE LAYOUT.
//
// `statelog.gate` is published by a node serving the partition of the one log
// the record is for, so its partition function answers that partition and no
// other — and refuses a record naming a log the running layout does not carry:
// another layout's, a partition the layout lacks, a domain with no log there.
func TestAGateRecordGoesToItsOwnLogsPartition(t *testing.T) {
	t.Parallel()
	layout := statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceTracker, Partitions: 4, Domains: []string{"tracker", "vectors"}},
		{Space: statelog.SpacePages, Partitions: 1, Domains: []string{"pages"}},
	}}
	args := LogRef{Layout: 1, Domain: "tracker", Partition: "tracker.003"}
	got, err := GatePartitions(layout, args)
	if err != nil || len(got) != 1 || got[0] != (statelog.PartitionID{Space: statelog.SpaceTracker, Index: 3}) {
		t.Fatalf("the gate record of tracker@tracker.003 goes to %v (%v), want tracker.003 alone", got, err)
	}
	for name, change := range map[string]func(*LogRef){
		"another layout's log":          func(a *LogRef) { a.Layout = 2 },
		"a partition the layout lacks":  func(a *LogRef) { a.Partition = "tracker.004" },
		"a domain with no log there":    func(a *LogRef) { a.Domain = "pages" },
		"a name that is no partition's": func(a *LogRef) { a.Partition = "tracker.3" },
		"no partition":                  func(a *LogRef) { a.Partition = "" },
	} {
		bad := args
		change(&bad)
		if _, err := GatePartitions(layout, bad); !errors.Is(err, ErrGateArgs) {
			t.Errorf("a gate record naming %s answered %v, want ErrGateArgs", name, err)
		}
	}
}

// aGateRecord is an eviction's record on layout 0's tracker log.
var aGateRecord = GateArgs{LogRef: LogRef{Layout: 0, Domain: "tracker",
	Partition: statelog.EstatePartition.String()}, Node: "node-away", By: "ops", OpID: "op-evict-1",
	Kind: GateEvict}

// A GATE RECORD ON A LOG THIS NODE DOES NOT SERVE REACHES A SERVING HOLDER,
// which publishes it, and the router says which node did.
//
// Only a node serving a log's partition writes the log, so an eviction run on
// a node that does not serve it is carried, as `statelog.gate`, to one that
// does — never published here, and never left unwritten. The answer names the
// holder whose write authority gave it, because what a refusal says is about
// that node; and a holder that does not answer is passed for the next one
// under the SAME operation id, which the log's own ledger collapses.
func TestAGateRecordOnALogThisNodeDoesNotServeReachesAServingHolder(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-1", "data-2")
	here := &fakeNode{name: "node-x", notHolder: true}
	r := f.router(t, "node-x", here)

	res, writer, err := r.Gate(t.Context(), aGateRecord)
	firstName, first := f.first(t, r)
	if err != nil || res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the gate record answered (%+v, %v), want applied", res, err)
	}
	if writer != firstName {
		t.Errorf("the record was written by %q, want the holder asked first, %s", writer, firstName)
	}
	if got := first.gated(); len(got) != 1 || got[0] != aGateRecord {
		t.Errorf("%s published %+v, want the record it was sent", firstName, got)
	}
	if got := here.gated(); len(got) != 0 {
		t.Errorf("a node that does not serve the partition published %+v itself", got)
	}

	// A HOLDER THAT DOES NOT ANSWER is passed for the next, under the same
	// operation id.
	first.set(func(n *fakeNode) { n.silent = true })
	other := f.other(first)
	res, writer, err = r.Gate(t.Context(), aGateRecord)
	if err != nil || res.Outcome != statelog.OutcomeApplied || writer != other.name {
		t.Fatalf("with %s silent the record answered (%+v, %q, %v), want applied by %s",
			firstName, res, writer, err, other.name)
	}
	if got := other.gated(); len(got) != 1 || got[0].OpID != aGateRecord.OpID {
		t.Errorf("%s published %+v, want the record under its own operation id", other.name, got)
	}
}

// A GATE RECORD IS PUBLISHED HERE WHERE THIS NODE SERVES THE LOG, and the
// writer is this node; where it serves the partition but does not run the log
// right now, a holder that does publishes it instead.
func TestAGateRecordIsPublishedWhereALogIsRun(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-1")
	here := &fakeNode{name: "node-x"}
	r := f.router(t, "node-x", here)
	f.placement.set(func(p *fakePlacement) { p.nodes = []string{"data-1", "node-x"} })

	if _, writer, err := r.Gate(t.Context(), aGateRecord); err != nil || writer != "node-x" {
		t.Fatalf("a log this node serves and runs was written by %q (%v), want this node", writer, err)
	}
	if len(here.gated()) != 1 || len(f.nodes["data-1"].gated()) != 0 {
		t.Fatalf("the record was published here %d time(s) and by data-1 %d, want here once",
			len(here.gated()), len(f.nodes["data-1"].gated()))
	}

	here.set(func(n *fakeNode) { n.noGateLog = true })
	if _, writer, err := r.Gate(t.Context(), aGateRecord); err != nil || writer != "data-1" {
		t.Fatalf("a log this node does not run right now was written by %q (%v), want data-1",
			writer, err)
	}
}

// A GATE RECORD A HOLDER COULD NOT VOUCH FOR IS ASKED OF THE NEXT: its ledger
// cannot say whether the record landed, and another holder's may — so the
// answer is not final until every holder has been asked, and is then the
// `unknown` it is, named by the node that gave it.
func TestAGateRecordAHolderCouldNotVouchForIsAskedOfTheNext(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-1", "data-2")
	r := f.router(t, "node-x", nil)
	_, first := f.first(t, r)
	other := f.other(first)
	first.set(func(n *fakeNode) { n.unvouched = true })

	res, writer, err := r.Gate(t.Context(), aGateRecord)
	if err != nil || res.Outcome != statelog.OutcomeApplied || writer != other.name {
		t.Fatalf("after an unvouched answer the record answered (%+v, %q, %v), want "+
			"applied by %s", res, writer, err, other.name)
	}

	other.set(func(n *fakeNode) { n.unvouched = true })
	res, writer, err = r.Gate(t.Context(), aGateRecord)
	if err != nil || res.Outcome != statelog.OutcomeUnknown || !res.Unvouched {
		t.Fatalf("with every holder unvouched the record answered (%+v, %v), want unknown, "+
			"unvouched", res, err)
	}
	if writer != first.name && writer != other.name {
		t.Errorf("the unvouched answer is named %q, want the holder that gave it", writer)
	}
}

// A GATE RECORD'S ATTEMPT IS ONE APPEND'S, NOT ITS CALLER'S DEADLINE.
//
// A holder that takes the request and never answers is waited on for
// [AppendAttempt], since that bounds everything a holder does for one append,
// and then the next holder is asked. Bounded by the caller's deadline instead —
// the gesture's whole budget — one wedged holder took every log it was first
// for with it.
func TestAGateRecordPassesAWedgedHolderAfterOneAppend(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-1", "data-2")
	r := f.router(t, "node-x", nil)
	r.appendBudget = 200 * time.Millisecond
	_, first := f.first(t, r)
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	first.set(func(n *fakeNode) { n.hang = hang })

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	began := time.Now()
	_, writer, err := r.Gate(ctx, aGateRecord)
	if err != nil || writer != f.other(first).name {
		t.Fatalf("past a wedged holder the record answered (%q, %v), want %s's answer",
			writer, err, f.other(first).name)
	}
	if took := time.Since(began); took > 20*time.Second {
		t.Errorf("the record took %v past a wedged holder, want about one append's attempt", took)
	}
}

// A GATE RECORD OF A KIND THIS NODE DOES NOT WRITE IS REFUSED, AND NOTHING IS
// PUBLISHED.
//
// The kind is named on the wire, never a flag whose zero value is an eviction:
// read as one, a record with no kind — or a kind a later build adds, decoded by
// this one — would have dropped every record the named node publishes on that
// log, on every applier. Refused at the serving node instead, before any
// backend sees it, and the refusal keeps its identity back across the wire.
func TestAGateRecordOfAKindThisNodeDoesNotWriteIsRefused(t *testing.T) {
	t.Parallel()
	for _, kind := range []GateKind{"", "quarantine", "readmit-all"} {
		f := newFleet(t, "data-1")
		r := f.router(t, "node-x", nil)
		record := aGateRecord
		record.Kind = kind
		res, _, err := r.Gate(t.Context(), record)
		if !errors.Is(err, ErrGateKind) || res.Outcome != "" {
			t.Errorf("a record of kind %q answered (%+v, %v), want ErrGateKind and no "+
				"outcome", kind, res, err)
		}
		if got := f.nodes["data-1"].gated(); len(got) != 0 {
			t.Errorf("a record of kind %q was published: %+v", kind, got)
		}
	}
	for _, kind := range GateKinds {
		if !kind.Valid() {
			t.Errorf("%q is a kind this build writes and is not valid", kind)
		}
	}
}

// A READMISSION'S BOUND IS READ WHERE THE LOG IS WRITTEN.
//
// A readmission is judged once, before any log is written, and each log's bound
// is the one the fence of the node that writes it holds: so a log the asking
// node does not write has its bound read, by `statelog.readmission_bound`, on a
// serving holder of its partition — the same partition function as the record
// it is judged for, this node first where it serves the partition and runs the
// log, and past a holder that runs no such log right now to the next.
func TestAReadmissionBoundIsReadWhereTheLogIsWritten(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-1", "data-2")
	here := &fakeNode{name: "node-x", notHolder: true}
	r := f.router(t, "node-x", here)
	firstName, first := f.first(t, r)

	bound, err := r.ReadmissionBound(t.Context(), aGateRecord.LogRef)
	if err != nil || bound != first.bound("tracker") {
		t.Fatalf("the bound answered (%+v, %v), want %s's own %+v", bound, err, firstName,
			first.bound("tracker"))
	}
	if here.askedFor("readmission_bound") {
		t.Errorf("a node that does not serve the partition read the bound itself")
	}

	first.set(func(n *fakeNode) { n.noGateLog = true })
	other := f.other(first)
	if bound, err = r.ReadmissionBound(t.Context(), aGateRecord.LogRef); err != nil ||
		bound != other.bound("tracker") {
		t.Fatalf("past a holder that runs no such log the bound answered (%+v, %v), want "+
			"%s's own", bound, err, other.name)
	}

	// THIS NODE'S OWN, where it serves the partition and runs the log.
	here.set(func(n *fakeNode) { n.notHolder = false })
	if bound, err = r.ReadmissionBound(t.Context(), aGateRecord.LogRef); err != nil ||
		bound != here.bound("tracker") {
		t.Fatalf("a log this node writes had its bound read as (%+v, %v), want its own",
			bound, err)
	}

	// A LOG NO LAYOUT HAS is refused before anybody is asked.
	bad := aGateRecord.LogRef
	bad.Partition = "tracker.999"
	if _, err := r.ReadmissionBound(t.Context(), bad); !errors.Is(err, ErrGateArgs) {
		t.Errorf("the bound of a log the layout lacks answered %v, want ErrGateArgs", err)
	}
}
