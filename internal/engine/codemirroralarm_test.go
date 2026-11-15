package engine_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/mirror"
	"github.com/crewlet/crewlet/internal/statelog"
)

// mirrorCompany is gitCompany with its one repository pushed to a mirror at
// another host, as the agent seat that owns it. A person's seat would push only
// while its holder uses Gitlet (ADR-0110), which this case's identity directory
// holds nobody for, and the alarm, not who a mirror may push as, is its
// subject (internal/gitlet's and internal/gitlet/mirror's own suites hold that).
const mirrorCompany = `
name: Nimbus
tracker:
  backend: none
knowledge:
  backend: none
code:
  backend: gitlet
  remote_credentials:
    WEB_MIRROR_TOKEN: {hosts: [mirror.example.com]}
  repos:
    - name: web
      owner: {seat: swe}
      mirror_to:
        url: https://mirror.example.com/web.git
        credential: "${WEB_MIRROR_TOKEN}"
        as: swe
roles:
  - name: SWE
    handle: swe
  - name: Founder
    handle: founder
    kind: human
`

// THE MIRROR ALARM FIRES ON ITS HOME (M6-D16, ADR-0015), through the wiring a
// running node has: a STARTED Gitlet node, alone the home of every repository,
// whose push mirror's every dial is refused — so its home loop's mirrors
// worker tries it at a pass and again once its retry is due, each attempt
// failing at the dial — raises nothing while the mirror is inside its own
// refresh promise (gitlet.MirrorInterval and mirror.AttemptDeadline) and
// code_mirror_failing, naming the repository, once it has gone that long
// without being brought level: its reading through codeReading, and the alarm
// the retention loop's evaluation of it stands. The node's clock is the
// case's, moved past the promise rather than waited through, and the home's
// passes are the case's too (engine.PollHomeForTest): waiting for the loop's
// own ticker sat out two of its gitlethome.Backstop minutes, two thirds of
// the case's two minutes on CI, for a pass that is the same pass either way.
// Each clock move waits for the attempt before it to be recorded as ended
// (mirror.Options.Ended), never only for its dial.
//
// Mutations: leave the mirrors' half out of the reading (codeReading), and
// the alarm never fires; count a mirror as failing from its first failed
// attempt, or at the interval alone, and it fires inside its promise; move
// the clock at the first dial again with the worker's record of the attempt
// held back past it, and the second attempt never dials.
func TestTheMirrorAlarmFiresOnItsHome(t *testing.T) {
	t.Parallel()
	b := gitletNodeBootstrap(t, nil)
	engine.RequireNodeGit(t, b, parsedCompany(t, mirrorCompany))
	var offset atomic.Int64
	clock := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	dialed := make(chan string, 16)
	ended := make(chan error, 16)
	refused := errors.New("the other host refuses every connection")
	e, err := engine.New(t.Context(), engine.WithMirrorSeams(engine.Options{
		Bootstrap: b, Company: parsedCompany(t, mirrorCompany),
		ActivatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
		GitHook:     gitlettest.HookCommand(t),
	}, func(o mirror.Options) mirror.Options {
		o.Now = clock
		o.Dial = func(_ context.Context, url, _ string) (gitproc.Remote, error) {
			select {
			case dialed <- url:
			default:
			}
			return gitproc.Remote{}, refused
		}
		o.Ended = func(_ string, out bool, err error) {
			if out {
				select {
				case ended <- err:
				default:
				}
			}
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
	// waitDial runs the home's passes until the mirrors' next dial: an
	// attempt of the push mirror that reached its target. More than one pass,
	// because the first may find this node not yet listed as the home; the
	// deadline is room for a loaded machine, and only ends the wait.
	waitDial := func(which string) {
		t.Helper()
		deadline := time.Now().Add(time.Minute)
		for {
			if err := engine.PollHomeForTest(t.Context(), e); err != nil {
				t.Fatalf("a pass of the home's loop: %v", err)
			}
			select {
			case <-dialed:
				return
			case <-time.After(100 * time.Millisecond): //fixedwait:liveness
			}
			if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline: its firing only ends the wait
				t.Fatalf("the push mirror's %s attempt never dialed its target", which)
			}
		}
	}
	// waitEnded waits until the worker has recorded the dialed attempt's end.
	waitEnded := func(which string) {
		t.Helper()
		select {
		case err := <-ended:
			if !errors.Is(err, refused) {
				t.Fatalf("the push mirror's %s attempt ended with %v, want the dial's refusal", which, err)
			}
		case <-time.After(time.Minute): //fixedwait:liveness
			t.Fatalf("the push mirror's %s attempt dialed and never ended", which)
		}
	}

	// INSIDE THE PROMISE: a first attempt, failed, and a second one twenty
	// minutes on: past its retry, short of the interval and its grace.
	waitDial("first")
	waitEnded("first")
	offset.Store(int64(20 * time.Minute))
	waitDial("second")
	waitEnded("second")
	if r := engine.CodeReadingForTest(t.Context(), e); r.CodeMirrorFailing != 0 {
		t.Fatalf("the reading counts %d failing mirror(s) twenty minutes into its first failure (%s, %s); "+
			"want none inside gitlet.MirrorInterval + mirror.AttemptDeadline", r.CodeMirrorFailing,
			r.CodeMirrorFailingRepo, r.CodeMirrorFailingFor)
	}

	// PAST IT: forty minutes since the mirror was first tried here, its
	// last attempt twenty minutes ago — still this node's to push.
	offset.Store(int64(40 * time.Minute))
	reading := engine.CodeReadingForTest(t.Context(), e)
	if reading.CodeMirrorFailing != 1 || reading.CodeMirrorFailingRepo != "web" ||
		reading.CodeMirrorFailingFor <= gitlet.MirrorInterval+mirror.AttemptDeadline {
		t.Fatalf("the reading counts %d failing mirror(s), the longest %q for %s; want web's, past %s",
			reading.CodeMirrorFailing, reading.CodeMirrorFailingRepo, reading.CodeMirrorFailingFor,
			gitlet.MirrorInterval+mirror.AttemptDeadline)
	}

	// AND THE ALARM, at the retention loop's next evaluation of its reading.
	// The rest is room for a loaded machine.
	deadline := time.Now().Add(statelog.AlarmInterval + time.Minute)
	for {
		kinds, _ := e.Alarms()
		if slices.Contains(kinds, statelog.KindCodeMirrorFailing) {
			break
		}
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline: its firing only ends the wait
			t.Fatalf("the node's alarms are %v with its push mirror failing past its refresh, want %s", kinds,
				statelog.KindCodeMirrorFailing)
		}
		time.Sleep(50 * time.Millisecond) //fixedwait:liveness
	}
}
