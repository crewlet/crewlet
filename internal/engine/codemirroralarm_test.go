package engine_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	codehome "github.com/crewlet/crewlet/internal/code/home"
	"github.com/crewlet/crewlet/internal/code/mirror"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/statelog"
)

// mirrorCompany is gitCompany with its one repository pushed to a mirror at
// another host, as the founder.
const mirrorCompany = `
name: Nimbus
tracker:
  backend: none
knowledge:
  backend: none
code:
  native: true
  repos:
    - name: web
      owner: {seat: founder}
      mirror_to:
        url: https://mirror.example.com/web.git
        credential: "${WEB_MIRROR_TOKEN}"
        as: founder
roles:
  - name: SWE
    handle: swe
  - name: Founder
    handle: founder
    kind: human
`

// THE MIRROR ALARM FIRES ON ITS HOME (M6-D16, ADR-0015), through the wiring a
// running node has: a STARTED git node, alone the home of every repository,
// whose push mirror's every dial is refused — so its home loop's mirrors
// worker tries it at its first pass and again once its retry is due, each
// attempt failing at the dial — raises nothing while the mirror is inside its
// own refresh promise (code.MirrorInterval and mirror.AttemptDeadline) and
// code_mirror_failing, naming the repository, once it has gone that long
// without being brought level: its reading through codeReading, and the alarm
// the retention loop's evaluation of it stands. The node's clock is the
// case's, moved past the promise rather than waited through.
//
// Mutations: leave the mirrors' half out of the reading (codeReading), and
// the alarm never fires; count a mirror as failing from its first failed
// attempt, or at the interval alone, and it fires inside its promise.
func TestTheMirrorAlarmFiresOnItsHome(t *testing.T) {
	t.Parallel()
	b := gitNodeBootstrap(t, nil)
	engine.RequireNodeGit(t, b, parsedCompany(t, mirrorCompany))
	var offset atomic.Int64
	clock := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	dialed := make(chan string, 16)
	refused := errors.New("the other host refuses every connection")
	e, err := engine.New(t.Context(), engine.WithMirrorSeams(engine.Options{
		Bootstrap: b, Company: parsedCompany(t, mirrorCompany),
		ActivatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
		GitHook:     codetest.HookCommand(t),
	}, func(o mirror.Options) mirror.Options {
		o.Now = clock
		o.Dial = func(_ context.Context, url, _ string) (gitproc.Remote, error) {
			select {
			case dialed <- url:
			default:
			}
			return gitproc.Remote{}, refused
		}
		return o
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.WithoutCancel(t.Context())) })
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// waitDial waits for the mirrors' next dial: an attempt of the push
	// mirror that reached its target. The deadline is three passes of the
	// home's loop — its first, at Start, may find this node not yet listed
	// — and room for a loaded machine.
	waitDial := func(which string) {
		t.Helper()
		select {
		case <-dialed:
		case <-time.After(3*codehome.Backstop + time.Minute):
			t.Fatalf("the push mirror's %s attempt never dialed its target", which)
		}
	}

	// INSIDE THE PROMISE: a first attempt, failed, and a second one twenty
	// minutes on — past its retry, short of the interval and its grace.
	waitDial("first")
	offset.Store(int64(20 * time.Minute))
	waitDial("second")
	if r := engine.CodeReadingForTest(t.Context(), e); r.CodeMirrorFailing != 0 {
		t.Fatalf("the reading counts %d failing mirror(s) twenty minutes into its first failure (%s, %s); "+
			"want none inside code.MirrorInterval + mirror.AttemptDeadline", r.CodeMirrorFailing,
			r.CodeMirrorFailingRepo, r.CodeMirrorFailingFor)
	}

	// PAST IT: forty minutes since the mirror was first tried here, its
	// last attempt twenty minutes ago — still this node's to push.
	offset.Store(int64(40 * time.Minute))
	reading := engine.CodeReadingForTest(t.Context(), e)
	if reading.CodeMirrorFailing != 1 || reading.CodeMirrorFailingRepo != "web" ||
		reading.CodeMirrorFailingFor <= code.MirrorInterval+mirror.AttemptDeadline {
		t.Fatalf("the reading counts %d failing mirror(s), the longest %q for %s; want web's, past %s",
			reading.CodeMirrorFailing, reading.CodeMirrorFailingRepo, reading.CodeMirrorFailingFor,
			code.MirrorInterval+mirror.AttemptDeadline)
	}

	// AND THE ALARM, at the retention loop's next evaluation of its reading.
	// The rest is room for a loaded machine.
	deadline := time.Now().Add(statelog.AlarmInterval + time.Minute)
	for {
		kinds, _ := e.Alarms()
		if slices.Contains(kinds, statelog.KindCodeMirrorFailing) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the node's alarms are %v with its push mirror failing past its refresh, want %s", kinds,
				statelog.KindCodeMirrorFailing)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
