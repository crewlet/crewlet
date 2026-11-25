package live_test

import (
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// readyBox is a box laid out for env whose ensure answered ready.
func readyBox(t *testing.T, env boxhelpertest.Env) *boxhelpertest.Box {
	t.Helper()
	box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
	if got := box.Ensure(t); got.Verdict != browser.EnsureReady {
		t.Fatalf("ensure answered %+v", got)
	}
	return box
}

// refusal is a supervisor's refusal of a relay's hello.
type refusal struct {
	T      string `json:"t"`
	Reason string `json:"reason"`
}

// THE NEWEST RELAY CONNECTION WINS: a second relay's hello closes the first
// relay's connection, and the second is sent the newest state and its job's
// check-in, so one job's relay at a time is the screen's. Newest alone: the
// rule is relay.sock's, which no Node or pin moves.
//
// Mutation: keep the first connection open.
func TestTheNewestRelayConnectionWins(t *testing.T) {
	box := readyBox(t, boxhelpertest.Newest(t))
	helper := helperOf(t, box)
	first := box.DialRelay(t)
	if got := first.Hello(helper, jobA); got.T != "hello" {
		t.Fatalf("the first relay's hello was answered %s", got.Header)
	}
	second := box.DialRelay(t)
	if got := second.Hello(helper, jobB); got.T != "hello" {
		t.Fatalf("the second relay's hello was answered %s", got.Header)
	}
	first.AwaitClosed("the first relay's connection, once a second said hello")
	second.Next("state", "the newest state", func(boxhelpertest.Record) bool { return true })
	var c browser.Checkin
	second.Next("checkin", "its job's check-in", func(boxhelpertest.Record) bool { return true }).Decode(t, &c)
	if c.Job != jobB || c.Connected {
		t.Errorf("the second relay was sent the check-in %+v, want job B's, not connected", c)
	}
}

// A RELAY THAT BREAKS RELAY.SOCK'S RULES IS CLOSED: a hello of another
// helper is refused, helper_mismatch, and closed; and a hello that is no
// relay's, a header past its bound, a record of a type the supervisor does
// not read, a watch at a rate the settings do not name, and an
// acknowledgement of no frame each end the connection unanswered. The
// supervisor serves on: a relay that keeps the rules is answered after.
// Newest alone: the rules are relay.sock's, which no Node or pin moves.
//
// Mutations, each red: take any helper's hello; take a watch at any rate.
func TestARelayThatBreaksRelaySocksRulesIsClosed(t *testing.T) {
	box := readyBox(t, boxhelpertest.Newest(t))
	helper := helperOf(t, box)
	other := box.DialRelay(t)
	var r refusal
	other.Hello("0123456789abcdef", jobA).Decode(t, &r)
	if r.T != "refused" || r.Reason != "helper_mismatch" {
		t.Errorf("another helper's hello was answered %+v, want refused helper_mismatch", r)
	}
	other.AwaitClosed("a relay of another helper")
	for what, send := range map[string]func(*boxhelpertest.RelaySock){
		"a hello that is no relay's": func(c *boxhelpertest.RelaySock) {
			c.Send(map[string]any{"t": "hello", "v": 1, "helper": helper, "job": "no launch"})
		},
		"a header past its bound": func(c *boxhelpertest.RelaySock) {
			c.SendRaw(binary.BigEndian.AppendUint32(nil, 1<<30))
		},
		"a record the supervisor does not read": func(c *boxhelpertest.RelaySock) {
			c.Hello(helper, jobA)
			c.Send(map[string]any{"t": "state", "v": 1})
		},
		"a watch at a rate the settings do not name": func(c *boxhelpertest.RelaySock) {
			c.Hello(helper, jobA)
			c.Watch(true, 10)
		},
		"an acknowledgement of no frame": func(c *boxhelpertest.RelaySock) {
			c.Hello(helper, jobA)
			c.Send(map[string]any{"t": "frame_ack", "v": 1, "boot": "no boot", "seq": 0})
		},
	} {
		c := box.DialRelay(t)
		send(c)
		c.AwaitClosed(what)
	}
	if got := box.DialRelay(t).Hello(helper, jobA); got.T != "hello" {
		t.Errorf("after the relays it closed, the supervisor answered a relay's hello %s", got.Header)
	}
}

// A RELAY IS ANSWERED ONLY ONCE THE BROWSER IS SERVED: relay.sock listens as
// the live view attaches, before the supervisor says ready, and a relay's
// hello then is refused, not_ready, and closed; once ready, a hello is
// answered under the supervisor's boot. The probe's supervisor, its Chromium
// on PATH as the probe has no browsers directory of its own, holds its start
// between the two. Newest alone: the order is the supervisor's.
//
// Mutation: answer a hello before the browser is served.
func TestARelayIsAnsweredOnlyOnceTheBrowserIsServed(t *testing.T) {
	box := boxhelpertest.Start(t, boxhelpertest.Newest(t), boxhelpertest.Options{Route: boxhelpertest.OnPath})
	if got := box.Ensure(t); got.Verdict != browser.EnsureReady {
		t.Fatalf("ensure answered %+v", got)
	}
	box.Stop(t)
	probe := box.Probe(t)
	probe.Do(map[string]any{"op": "supervise", "launch": filepath.Join(box.Folder, "launch.json"),
		"live": []string{"live", "record", "hold"}, "at_once": true}, &struct{}{})
	t.Cleanup(func() {
		if s := box.Supervisor(t); s.Chromium != nil && s.Chromium.PID > 0 {
			_ = procgroup.Kill(s.Chromium.PID)
			procgrouptest.AwaitGone(t, s.Chromium.PID, beaconWithin)
		}
	})
	var attached struct {
		Attached []struct {
			Boot string `json:"boot"`
		} `json:"attached"`
	}
	probe.Do(map[string]any{"op": "attached", "until": map[string]any{"hook": "attach"}}, &attached)
	helper := helperOf(t, box)
	early := box.DialRelay(t)
	var r refusal
	early.Hello(helper, jobA).Decode(t, &r)
	if r.T != "refused" || r.Reason != "not_ready" {
		t.Errorf("a relay's hello before the browser was served was answered %+v, want refused not_ready", r)
	}
	early.AwaitClosed("a relay refused as not ready")
	probe.Do(map[string]any{"op": "release"}, &struct{}{})
	probe.Do(map[string]any{"op": "attached", "until": map[string]any{"hook": "state", "state": "ready"}}, &struct{}{})
	var said struct {
		T    string `json:"t"`
		Boot string `json:"boot"`
	}
	box.DialRelay(t).Hello(helper, jobA).Decode(t, &said)
	if said.T != "hello" || len(attached.Attached) == 0 || said.Boot != attached.Attached[0].Boot {
		t.Errorf("once ready, a relay's hello was answered %+v, want hello under the supervisor's boot", said)
	}
}
