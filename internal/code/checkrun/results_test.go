package checkrun_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// full is an object store with no room: every put refused objstore.ErrFull.
type full struct{ objstore.Backend }

func (full) Put(context.Context, string, io.Reader, objstore.PutMeta) error {
	return &objstore.Full{Remedy: "an operator makes room", Err: errors.New("the bucket is at its quota")}
}

// storeOver is an object store over backend.
func storeOver(t *testing.T, backend objstore.Backend) *objstore.Store {
	t.Helper()
	s, err := objstore.NewStore(backend)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A FAILED RUN'S LOG IS KEPT AND A PASSED ONE'S IS NOT: a run that failed
// stores the last of its output as one object its attempt's row names, its
// bytes the output's; a run that passed keeps its summary alone — no tail, no
// failures, no log; and a failed run on a store with no room ends without its
// log, its summary saying why.
//
// Mutation: keep every run's log — the passed run's row names one, red; fail
// the ending on a full store — the attempt never ends, red.
func TestAFailedRunsLogIsKeptAndAPassedOneIsNot(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	store := storeOver(t, memobj.New())
	o := f.options("git-a", 1, &order{nodes: []string{"git-a"}})
	o.Logs = store
	f.runner(o)

	output := strings.Repeat("compiling\n", 100) + "FAIL web/cmd\n"
	failing := f.queue(oidOf("a"), def("unit", time.Minute))
	box := f.container.box(t)
	f.container.end(box, 1, output)
	f.ended(failing)
	d := f.detail(failing)
	if d.State != code.CheckStateFailed || d.Log == nil || d.Log.Size != int64(len(output)) {
		t.Fatalf("a failed run = %+v, its log %+v; want failed, its log the whole output", d.CheckRow, d.Log)
	}
	rc, err := store.Open(t.Context(), d.Log.Content())
	if err != nil {
		t.Fatalf("open the failed run's log: %v", err)
	}
	kept, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(kept, []byte(output)) {
		t.Fatalf("the kept log reads %q (%v), want the run's output", kept, err)
	}

	passing := f.queue(oidOf("b"), def("unit", time.Minute))
	box = f.container.box(t)
	f.container.end(box, 0, "ok\n")
	f.ended(passing)
	if d := f.detail(passing); d.State != code.CheckStatePassed || d.Log != nil || d.Tail != "" ||
		len(d.Failures) > 0 {
		t.Fatalf("a passed run kept %+v, its log %+v, tail %q; want its summary alone", d.CheckRow, d.Log, d.Tail)
	}

	g := newFleet(t)
	o = g.options("git-a", 1, &order{nodes: []string{"git-a"}})
	o.Logs = storeOver(t, full{memobj.New()})
	g.runner(o)
	noRoom := g.queue(oidOf("c"), def("unit", time.Minute))
	box = g.container.box(t)
	g.container.end(box, 1, output)
	row := g.ended(noRoom)
	if row.State != code.CheckStateFailed || !strings.Contains(row.Summary, "object store is full") {
		t.Fatalf("a failed run on a full store = %+v, want failed, its summary saying its log was not kept", row)
	}
	if d := g.detail(noRoom); d.Log != nil || !strings.Contains(d.Tail, "FAIL web/cmd") {
		t.Errorf("a failed run on a full store kept log %+v and tail %q; want no log and its tail", d.Log, d.Tail)
	}
}

// A RUN'S REPORTS AND ITS SUMMARY REACH ITS ENDING: the failures its JUnit
// report names ride its failed ending, the summary its command wrote to
// $CREWLET_CHECK_SUMMARY is its summary over everything the runner could say,
// and with no summary written a report's counts are — and the wrapper copies
// each report the definition names out of the checkout once the command ends.
//
// Mutation: summarise every run by its exit alone — the command's own summary
// is lost, red.
func TestARunsReportsAndSummaryReachItsEnding(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	f.runner(f.options("git-a", 1, &order{nodes: []string{"git-a"}}))
	d := def("unit", time.Minute)
	d.Report.JUnit = "out/junit.xml"
	key := f.queue(oidOf("d"), d)
	box := f.container.box(t)
	box.Put(sandbox.DefaultHome+"/.crewlet-check/junit.xml", junitOf(2, 5, 3))
	f.container.end(box, 1, "")
	row := f.ended(key)
	// Read once the attempt has ENDED, never once its row names the box: the
	// runner names the box before it starts the command, so a read between
	// the two sees no command at all.
	if bg := box.Background(); len(bg) != 1 || !strings.Contains(bg[0], "cp -f -- 'out/junit.xml'") {
		t.Fatalf("the box's command %q copies no report", bg)
	}
	if !strings.Contains(row.Summary, "2 of 7 tests failed") {
		t.Errorf("the summary of a run with a JUnit report = %q, want its counts", row.Summary)
	}
	if got := f.detail(key).Failures; len(got) != 2 || got[0].TestID != "pkg.TestFail0" {
		t.Errorf("the ending's failures = %+v, want the report's two", got)
	}

	written := f.queue(oidOf("e"), def("unit", time.Minute))
	box = f.container.box(t)
	box.Put(sandbox.DefaultHome+"/.crewlet-check/summary", "3 flaky tests quarantined; the build is green\n")
	f.container.end(box, 0, "")
	if row := f.ended(written); row.Summary != "3 flaky tests quarantined; the build is green" {
		t.Errorf("the summary of a run that wrote one = %q, want its own", row.Summary)
	}
}
