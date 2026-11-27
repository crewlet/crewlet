package live_test

import (
	"bufio"
	"encoding/json"
	"image/color"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/live"
)

// aBoot is the boot a stand-in supervisor answers a relay under.
const aBoot = "0123456789abcdef"

// relaying is a job's relay of job A started as its script starts it, on a
// fake bridge, its supervisor a stand-in that has answered its hello.
type relaying struct {
	box    *boxhelpertest.Box
	bridge *boxhelpertest.FakeBridge
	si     *boxhelpertest.StandIn
	relay  *boxhelpertest.Relay
	conn   *boxhelpertest.RelaySock
}

// relayOn starts a relay as relaying says, in a box laid out for env as o
// says, in its environment and extra.
func relayOn(t *testing.T, env boxhelpertest.Env, o boxhelpertest.Options, extra ...string) *relaying {
	t.Helper()
	return relayTo(t, env, o, boxhelpertest.NewFakeBridge(t), extra...)
}

// relayTo is relayOn on bridge, which a case has told how to answer first.
func relayTo(t *testing.T, env boxhelpertest.Env, o boxhelpertest.Options, bridge *boxhelpertest.FakeBridge,
	extra ...string) *relaying {
	t.Helper()
	box := boxhelpertest.Start(t, env, o)
	si := box.StandIn(t)
	r := box.Relay(t, bridge.URL(), jobA, extra...)
	conn := si.Accept("the relay's connection")
	if job := conn.Answer(helperOf(t, box), aBoot); job != jobA {
		t.Fatalf("the relay said hello for %s, want its wiring's job %s", job, jobA)
	}
	return &relaying{box: box, bridge: bridge, si: si, relay: r, conn: conn}
}

// state is a state of rev under aBoot, as the supervisor sends it.
func state(rev int) map[string]any {
	return map[string]any{"t": "state", "v": 1, "boot": aBoot, "rev": rev, "running": "running", "tabs": []any{}}
}

// revOf is a posted state's rev, -1 for a body that is no state.
func revOf(req boxhelpertest.Request) int64 {
	var s browser.State
	if err := json.Unmarshal(req.Body, &s); err != nil {
		return -1
	}
	return s.Rev
}

// frameHeader is a frame's header of seq under aBoot, as the supervisor
// sends it for a w by h JPEG.
func frameHeader(seq int64, w, h int) map[string]any {
	return map[string]any{"t": "frame", "v": 1, "boot": aBoot, "seq": seq, "tab": "t1", "capture": "screencast",
		"at_ms": 1764000000000, "width": w, "height": h, "device_width": 1280, "device_height": 800, "offset_top": 0,
		"page_scale": 1}
}

// acked waits for the relay's frame_ack of seq on c.
func acked(t *testing.T, c *boxhelpertest.RelaySock, seq int64) {
	t.Helper()
	c.Next("frame_ack", "the relay's frame_ack", func(r boxhelpertest.Record) bool {
		var a struct {
			Seq int64 `json:"seq"`
		}
		r.Decode(t, &a)
		return a.Seq == seq
	})
}

// THE RELAY ANSWERS EVERY STATUS AS ITS CONTRACT SAYS: a frame posted with
// its header less its type, acknowledged once answered, and never posted
// again once refused as too fast; a state refused 400, 413 or 415 never
// posted again, one answered 502 or 503 posted again until taken; a new hub's
// answer has the newest state and check-in posted again; a watch at a rate it
// does not know told nobody, its next poll naming the answer's hub and level;
// a 404 of a route another build lacks retried; and a 401, or a 404
// not_found, its end, bye and exit 0. Newest alone: the rules are the
// relay's.
//
// Mutations, each red: post a frame refused too fast again; end on any 404;
// leave a poll's hub and level as they were.
func TestTheRelayAnswersEveryStatusAsItsContractSays(t *testing.T) {
	jpeg := solid(t, 64, 40, color.RGBA{R: 40, A: 255})
	t.Run("frames", func(t *testing.T) {
		r := relayOn(t, boxhelpertest.Newest(t), boxhelpertest.Options{})
		r.conn.SendFrame(frameHeader(1, 64, 40), jpeg)
		posted := r.bridge.Await("the frame's post", 0, boxhelpertest.Verb("frame"))
		var meta map[string]any
		if err := json.Unmarshal([]byte(posted.Frame), &meta); err != nil || meta["t"] != nil || meta["seq"] != 1.0 ||
			posted.Type != browser.FrameType || string(posted.Body) != string(jpeg) {
			t.Errorf("the frame was posted as %+v with %v (%v), want its header less t, as image/jpeg", posted, meta, err)
		}
		acked(t, r.conn, 1)
		r.bridge.Refuse("frame", 429, "too_fast", 1)
		r.conn.SendFrame(frameHeader(2, 64, 40), jpeg)
		acked(t, r.conn, 2)
		r.bridge.Accept("frame")
		r.conn.SendFrame(frameHeader(3, 64, 40), jpeg)
		acked(t, r.conn, 3)
		var seqs []string
		for _, req := range r.bridge.Requests() {
			if req.Verb == "frame" {
				var m struct {
					Seq json.Number `json:"seq"`
				}
				_ = json.Unmarshal([]byte(req.Frame), &m)
				seqs = append(seqs, m.Seq.String())
			}
		}
		if strings.Join(seqs, ",") != "1,2,3" {
			t.Errorf("the bridge was posted frames %v, want 1, 2 and 3 once each: one refused as too fast is "+
				"acknowledged, never posted again", seqs)
		}
	})
	t.Run("states", func(t *testing.T) {
		r := relayOn(t, boxhelpertest.Newest(t), boxhelpertest.Options{})
		rev := 0
		for _, status := range []int{400, 413, 415} {
			rev++
			r.bridge.Refuse("state", status, "invalid_body", 0)
			r.conn.Send(state(rev))
			r.bridge.Await("a state refused", 0, func(q boxhelpertest.Request) bool { return revOf(q) == int64(rev) })
		}
		for _, status := range []int{502, 503} {
			rev++
			r.bridge.Refuse("state", status, "unavailable", 0)
			r.conn.Send(state(rev))
			first := r.bridge.Await("a state refused", 0, func(q boxhelpertest.Request) bool { return revOf(q) == int64(rev) })
			r.bridge.Await("the same state again", first.N, func(q boxhelpertest.Request) bool { return revOf(q) == int64(rev) })
		}
		r.bridge.Accept("state")
		rev++
		r.conn.Send(state(rev))
		taken := r.bridge.Await("the newest state taken", 0, func(q boxhelpertest.Request) bool { return revOf(q) == int64(rev) })
		posts := map[int64]int{}
		for _, q := range r.bridge.Requests()[:taken.N] {
			if q.Verb == "state" {
				posts[revOf(q)]++
			}
		}
		for dropped := int64(1); dropped <= 3; dropped++ {
			if posts[dropped] != 1 {
				t.Errorf("the state of rev %d, refused 400, 413 or 415, was posted %d times, want once", dropped, posts[dropped])
			}
		}
		r.conn.Send(map[string]any{"t": "checkin", "v": 1, "job": jobA, "connected": true, "since_ms": 5})
		in := r.bridge.Await("the check-in", 0, boxhelpertest.Verb("checkin"))
		var c browser.Checkin
		if err := json.Unmarshal(in.Body, &c); err != nil || c.Supervisor != browser.SupervisorAnswering || !c.Connected {
			t.Errorf("the check-in was posted as %s, want the supervisor's, answering", in.Body)
		}
		r.bridge.NewHub()
		r.bridge.Await("the newest state again for a new hub", in.N, func(q boxhelpertest.Request) bool {
			return revOf(q) == int64(rev)
		})
		r.bridge.Await("the check-in again for a new hub", in.N, boxhelpertest.Verb("checkin"))
	})
	t.Run("a watch at a rate it does not know", func(t *testing.T) {
		r := relayOn(t, boxhelpertest.Newest(t), boxhelpertest.Options{})
		r.bridge.Await("the poll the bridge holds", 0, func(q boxhelpertest.Request) bool {
			return q.Verb == "poll" && q.Query == "hub="+r.bridge.Hub()+"&level=0"
		})
		r.bridge.Watch(true, 10)
		r.bridge.Await("the poll naming the hub and level of an unknown rate's answer", 0,
			func(q boxhelpertest.Request) bool {
				return q.Verb == "poll" && q.Query == "hub="+r.bridge.Hub()+"&level=1"
			})
		r.bridge.Watch(true, browser.WatchFPS)
		type watch struct {
			On  bool `json:"on"`
			FPS int  `json:"fps"`
		}
		var w watch
		r.conn.Next("watch", "the relay's first watch on", func(rec boxhelpertest.Record) bool {
			var on watch
			rec.Decode(t, &on)
			return on.On
		}).Decode(t, &w)
		if w.FPS != browser.WatchFPS {
			t.Errorf("the supervisor was first told to watch at %d frames a second, want %d: a rate it does not know "+
				"is told nobody", w.FPS, browser.WatchFPS)
		}
	})
	for _, end := range []struct {
		name, code string
		status     int
	}{
		{"a 401", "invalid_token", 401},
		{"a 404 not_found", "not_found", 404},
	} {
		t.Run(end.name, func(t *testing.T) {
			bridge := boxhelpertest.NewFakeBridge(t)
			bridge.Refuse("poll", 404, "no_route", 0)
			r := relayTo(t, boxhelpertest.Newest(t), boxhelpertest.Options{}, bridge)
			first := r.bridge.Await("a poll of a route this build lacks", 0, boxhelpertest.Verb("poll"))
			r.bridge.Await("the poll again", first.N, boxhelpertest.Verb("poll"))
			r.bridge.Refuse("poll", end.status, end.code, 0)
			r.conn.Next("bye", "the relay's bye", func(boxhelpertest.Record) bool { return true })
			if code := r.relay.Exited(); code != 0 {
				t.Errorf("on %s the relay exited %d, want 0", end.name, code)
			}
		})
	}
}

// THE RELAY CARRIES ITS TOKEN ON NO COMMAND LINE AND IN NO LOG: the token is
// in the wiring file alone: never on the relay's command line, nor in its log
// or the supervisor's, a refusal it was answered and a route it cannot reach
// among what they log, though the job's script asks node, through NODE_DEBUG,
// to log all it does. Newest alone: the rule is the relay's.
//
// Mutations, each red: the route on the command line; a failed request's
// message logged; hand node the script's whole environment.
func TestTheRelayCarriesItsTokenOnNoCommandLineAndInNoLog(t *testing.T) {
	box := boxhelpertest.Start(t, boxhelpertest.Newest(t), boxhelpertest.Options{Route: boxhelpertest.OnPath})
	if got := box.Ensure(t); got.Verdict != browser.EnsureReady {
		t.Fatalf("ensure answered %+v", got)
	}
	bridge := boxhelpertest.NewFakeBridge(t)
	bridge.Refuse("state", 400, "invalid_body", 0)
	r := box.Relay(t, bridge.URL(), jobA, "NODE_DEBUG=*")
	bridge.Await("the supervisor's state, refused", 0, boxhelpertest.Verb("state"))
	r.AwaitLog("the state's refusal", "refused")
	cmdline, err := os.ReadFile(filepath.Join("/proc", itoa(r.PID), "cmdline"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cmdline), bridge.Token()) || strings.Contains(string(cmdline), "crewlet_rt1") {
		t.Errorf("the relay's command line, read once it reached its bridge, carries its token: %q", cmdline)
	}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gone := "http://" + ln.Addr().String() + "/mcp/crewlet_rt1.gone." + bridge.Token() + "/browser"
	_ = ln.Close()
	r.Term()
	r.Exited()
	again := box.Relay(t, gone, jobB, "NODE_DEBUG=*")
	again.AwaitLog("a request to the unreachable route failing", "request_failed")
	again.Term()
	if code := again.Exited(); code != 0 {
		t.Errorf("the relay of an unreachable route exited %d on TERM, want 0", code)
	}
	for _, file := range []string{r.Log, filepath.Join(box.Folder, "supervisor.log")} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), bridge.Token()) || strings.Contains(string(raw), "crewlet_rt1") {
			t.Errorf("%s carries the token:\n%s", file, raw)
		}
		if file == r.Log && !strings.Contains(string(raw), "refused") {
			t.Errorf("%s logs no refusal, and the bridge refused a state:\n%s", file, raw)
		}
	}
}

// itoa is n in decimal.
func itoa(n int) string { return strconv.Itoa(n) }

// THE RELAY REACHES ITS BRIDGE DIRECTLY WHATEVER PROXY THE BOX NAMES: with
// HTTP_PROXY, HTTPS_PROXY and ALL_PROXY naming a stand-in proxy that counts
// every connection, and NODE_USE_ENV_PROXY asking Node to honour them, every
// request of the relay's reaches the bridge and none the proxy, as the
// harness reaches its bridge; and so does the relay's dialer in a Node that
// is handed NODE_USE_ENV_PROXY, which the relay's own start never passes on.
// On each Node's HTTP stack.
//
// Mutation: post through Node's global agent.
func TestTheRelayReachesItsBridgeDirectlyWhateverProxyTheBoxNames(t *testing.T) {
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		var mu sync.Mutex
		proxied := 0
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				mu.Lock()
				proxied++
				mu.Unlock()
				_, _ = bufio.NewReader(conn).ReadString('\n')
				_ = conn.Close()
			}
		}()
		proxy := "http://" + ln.Addr().String()
		proxies := []string{"HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy, "ALL_PROXY=" + proxy,
			"http_proxy=" + proxy, "https_proxy=" + proxy, "NODE_USE_ENV_PROXY=1"}
		r := relayOn(t, env, boxhelpertest.Options{}, proxies...)
		r.conn.Send(state(1))
		r.bridge.Await("the state, posted directly", 0, boxhelpertest.Verb("state"))
		r.bridge.Await("a poll, made directly", 0, boxhelpertest.Verb("poll"))
		var dialled struct {
			Status int    `json:"status"`
			Failed string `json:"failed"`
		}
		r.box.Probe(t, proxies...).Do(map[string]any{"op": "dial", "url": r.bridge.URL() + "/checkin"}, &dialled)
		if dialled.Status != 204 {
			t.Errorf("the relay's dialer, in a Node handed NODE_USE_ENV_PROXY, answered %+v, want the bridge's 204",
				dialled)
		}
		mu.Lock()
		defer mu.Unlock()
		if proxied != 0 {
			t.Errorf("the proxy took %d connections of the relay's, want none", proxied)
		}
	})
}

// THE RELAY REPORTS A SUPERVISOR THAT DOES NOT ANSWER AND COMES BACK: with
// the box's supervisor stopped, the relay posts a check-in of its own saying
// none answers and nothing connected, and no state; once ensure starts
// another, the relay says its hello again, and posts the new supervisor's
// check-in, answering, and its states under the new boot, and no other.
// Newest alone: the link is the relay's, which no pin moves.
//
// Mutations, each red: end the relay as its socket closes; post a state of
// its own while none answers.
func TestTheRelayReportsASupervisorThatDoesNotAnswerAndComesBack(t *testing.T) {
	box := boxhelpertest.Start(t, boxhelpertest.Newest(t), boxhelpertest.Options{})
	if got := box.Ensure(t); got.Verdict != browser.EnsureReady {
		t.Fatalf("ensure answered %+v", got)
	}
	boot := box.Supervisor(t).Boot
	bridge := boxhelpertest.NewFakeBridge(t)
	box.Relay(t, bridge.URL(), jobA)
	bridge.Watch(true, browser.WatchFPS)
	answering := bridge.Await("the supervisor's state", 0, func(q boxhelpertest.Request) bool {
		var s browser.State
		return q.Verb == "state" && json.Unmarshal(q.Body, &s) == nil && s.Boot == boot
	})
	box.Stop(t)
	absent := bridge.Await("the relay's own check-in", answering.N, func(q boxhelpertest.Request) bool {
		var c browser.Checkin
		return q.Verb == "checkin" && json.Unmarshal(q.Body, &c) == nil && c.Supervisor == browser.SupervisorAbsent
	})
	var c browser.Checkin
	if err := json.Unmarshal(absent.Body, &c); err != nil || c.Connected || c.Job != jobA {
		t.Errorf("with no supervisor the relay posted %s, want job A not connected", absent.Body)
	}
	if got := box.Ensure(t); got.Verdict != browser.EnsureReady {
		t.Fatalf("ensure answered %+v", got)
	}
	next := box.Supervisor(t).Boot
	bridge.Await("the new supervisor's check-in", absent.N, func(q boxhelpertest.Request) bool {
		var c browser.Checkin
		return q.Verb == "checkin" && json.Unmarshal(q.Body, &c) == nil && c.Supervisor == browser.SupervisorAnswering
	})
	bridge.Await("the new boot's state", absent.N, func(q boxhelpertest.Request) bool {
		var s browser.State
		return q.Verb == "state" && json.Unmarshal(q.Body, &s) == nil && s.Boot == next
	})
	for _, q := range bridge.Requests()[answering.N:] {
		var s browser.State
		if q.Verb == "state" && (json.Unmarshal(q.Body, &s) != nil || (s.Boot != boot && s.Boot != next)) {
			t.Errorf("once its supervisor stopped, the relay posted a state of no supervisor that answers it: %s", q.Body)
		}
	}
	if next == boot {
		t.Errorf("the new supervisor's boot is the last one's, %s", boot)
	}
}

// A RELAY OF ANOTHER HELPER IS REFUSED AND ENDS: a relay reading a config
// that names another helper than the supervisor runs is refused its hello as
// helper_mismatch, which no retry in its job changes, and exits 69 with a
// line saying so. Newest alone: the rule is the relay's.
//
// Mutation: keep retrying a hello refused as another helper's.
func TestARelayOfAnotherHelperIsRefusedAndEnds(t *testing.T) {
	box := boxhelpertest.Start(t, boxhelpertest.Newest(t), boxhelpertest.Options{})
	if got := box.Ensure(t); got.Verdict != browser.EnsureReady {
		t.Fatalf("ensure answered %+v", got)
	}
	config := box.Config(t)
	config["helper"] = "0123456789abcdef"
	box.SetConfig(t, config)
	r := box.Relay(t, boxhelpertest.NewFakeBridge(t).URL(), jobA)
	if code := r.Exited(); code != 69 {
		t.Errorf("a relay of another helper exited %d, want 69", code)
	}
	if log := r.LogText(); !strings.Contains(log, "helper_mismatch") {
		t.Errorf("its log says %q, want the refusal", log)
	}
}

// THE RELAY REFUSES AN INVOCATION IT DOES NOT TAKE: no wiring flag, a relative
// path, another flag, and a wiring of another version, of a job that is no
// launch id, or of a route that is no absolute http or https address ending
// /browser each exit 64 with one line naming the file and never the route,
// which holds the token. On each Node.
//
// Mutation: take a route that does not end /browser.
func TestTheRelayRefusesAnInvocationItDoesNotTake(t *testing.T) {
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
		route := "http://127.0.0.1:1/mcp/crewlet_rt1.secret/browser"
		// wiring writes the box's one wiring file, so each case writes its own
		// as it runs.
		wiring := func(v int, url, job string) string {
			return box.WriteWiring(t, map[string]any{"v": v, "url": url, "job": job})
		}
		for _, tc := range []struct {
			name string
			args func() []string
		}{
			{"no wiring flag", func() []string { return []string{"relay"} }},
			{"a relative path", func() []string { return []string{"relay", "--wiring", "browser-relay.json"} }},
			{"another flag", func() []string { return []string{"relay", "--wiring", wiring(1, route, jobA), "--job", jobA} }},
			{"another version", func() []string { return []string{"relay", "--wiring", wiring(2, route, jobA)} }},
			{"no launch id", func() []string { return []string{"relay", "--wiring", wiring(1, route, "job-a")} }},
			{"another route", func() []string {
				return []string{"relay", "--wiring", wiring(1, "http://127.0.0.1:1/mcp/crewlet_rt1.secret/notes", jobA)}
			}},
			{"a relative route", func() []string {
				return []string{"relay", "--wiring", wiring(1, "/mcp/crewlet_rt1.secret/browser", jobA)}
			}},
		} {
			code, stderr := box.RunHelper(t, tc.args())
			if code != 64 || strings.Count(stderr, "\n") != 1 || strings.Contains(stderr, "crewlet_rt1") {
				t.Errorf("%s: the relay exited %d saying %q, want 64 and one line without the route", tc.name, code,
					stderr)
			}
		}
	})
}

// THE RELAY HOLDS ONE POLL AT A TIME: across the watches a bridge answers,
// the relay holds at most one poll, and asks the next once one is answered:
// on the probe's clock, its polls answered in turn, never two in flight, on
// each Node; and on a fake bridge, newest alone.
//
// Mutation: poll again while one is held.
func TestTheRelayHoldsOnePollAtATime(t *testing.T) {
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		var got struct {
			Asked int `json:"asked"`
			Most  int `json:"most"`
		}
		probe := boxhelpertest.Start(t, env, boxhelpertest.Options{}).Probe(t)
		probe.Do(map[string]any{"op": "relay_polls", "answers": 4}, &got)
		if got.Asked != 5 || got.Most != 1 {
			t.Errorf("on the probe's clock, for 4 answers the relay asked %d polls, at most %d at once: want 5, one "+
				"at a time", got.Asked, got.Most)
		}
	})
	r := relayOn(t, boxhelpertest.Newest(t), boxhelpertest.Options{})
	last := r.bridge.Await("the first poll", 0, boxhelpertest.Verb("poll"))
	for range 3 {
		r.bridge.Watch(false, browser.WatchFPS)
		last = r.bridge.Await("the next poll", last.N, boxhelpertest.Verb("poll"))
	}
	if n := r.bridge.MaxPolls(); n != 1 {
		t.Errorf("the bridge held %d polls of the relay's at once, want one", n)
	}
}

// A REQUEST WHOSE ANSWER BREAKS OFF FAILS AND IS ASKED AGAIN: a poll
// answered 200 with a body past relay.body_bytes, or with one cut off
// partway, its connection closed, fails as it does, logged by its cause, and
// the relay polls again; so does it once the bridge answers whole. On each
// Node's HTTP stack, whose events for an answer that ends early differ.
//
// Mutations, each red: settle a request only as its answer ends or its
// request errs; read an answer past the bound.
func TestARequestWhoseAnswerBreaksOffFailsAndIsAskedAgain(t *testing.T) {
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		for _, tc := range []struct {
			name, cause string
			spoil       func(*boxhelpertest.FakeBridge)
		}{
			{"past the bound", "answer_too_long", func(b *boxhelpertest.FakeBridge) { b.Overlong("poll") }},
			{"cut off", "reset", func(b *boxhelpertest.FakeBridge) { b.BreakOff("poll") }},
		} {
			bridge := boxhelpertest.NewFakeBridge(t)
			tc.spoil(bridge)
			r := relayTo(t, env, boxhelpertest.Options{}, bridge)
			first := bridge.Await("the first poll", 0, boxhelpertest.Verb("poll"))
			again := bridge.Await("the poll asked again", first.N, boxhelpertest.Verb("poll"))
			r.relay.AwaitLog("the poll's failure", "request_failed")
			if log := r.relay.LogText(); !strings.Contains(log, `"error":"`+tc.cause+`"`) {
				t.Errorf("%s: the relay's log reads %q, want the poll's failure as %s", tc.name, log, tc.cause)
			}
			bridge.Accept("poll")
			bridge.Watch(false, browser.WatchFPS)
			bridge.Await("a poll once the bridge answers whole", again.N, boxhelpertest.Verb("poll"))
		}
	})
}

// THE RELAY'S LOG STOPS AT ITS BOUND: with relay.log_bytes shortened, a
// relay whose every state is refused logs each refusal until the bound, then
// one line saying it stops, and nothing more, and posts on. Newest alone.
//
// Mutation: log past the bound.
func TestTheRelaysLogStopsAtItsBound(t *testing.T) {
	const bound = 600
	o := boxhelpertest.Options{Config: map[string]any{"relay": map[string]any{"log_bytes": bound}}}
	r := relayOn(t, boxhelpertest.Newest(t), o)
	r.bridge.Refuse("state", 400, "invalid_body", 0)
	var last boxhelpertest.Request
	for rev := 1; rev <= 12; rev++ {
		r.conn.Send(state(rev))
		last = r.bridge.Await("a state refused", last.N, func(q boxhelpertest.Request) bool { return revOf(q) == int64(rev) })
	}
	log := r.relay.LogText()
	if len(log) > bound || !strings.HasSuffix(log, "\n") || !strings.Contains(log[strings.LastIndex(log[:len(log)-1], "\n")+1:], "log_full") {
		t.Errorf("the relay's log is %d bytes, ending %q: want at most %d, its last line saying it stops", len(log),
			log[max(0, len(log)-120):], bound)
	}
}

// A TERMED RELAY SAYS BYE AND LEAVES WITHIN ITS BOUND: TERMed with a poll
// held and a state post in flight at the bridge, the relay tells the
// supervisor bye, abandons both, logging them, and exits 0. On the probe's
// clock it exits at once where what it abandons ends, and where a request
// never answers, at relay.term_ms and not before. Newest alone.
//
// Mutations, each red: wait for what is in flight; drop the bound's timer.
func TestATermedRelaySaysByeAndLeavesWithinItsBound(t *testing.T) {
	r := relayOn(t, boxhelpertest.Newest(t), boxhelpertest.Options{})
	r.bridge.Hold("state")
	r.conn.Send(state(1))
	r.bridge.Await("the state, held", 0, boxhelpertest.Verb("state"))
	r.bridge.Await("the poll, held", 0, boxhelpertest.Verb("poll"))
	r.relay.Term()
	r.conn.Next("bye", "the relay's bye", func(boxhelpertest.Record) bool { return true })
	if code := r.relay.Exited(); code != 0 {
		t.Errorf("a TERMed relay exited %d, want 0", code)
	}
	if log := r.relay.LogText(); !strings.Contains(log, "relay_left") || !strings.Contains(log, "state") ||
		!strings.Contains(log, "poll") {
		t.Errorf("the relay's log says %q, want it left abandoning the state and the poll", log)
	}
	var got struct {
		Term   int64               `json:"term"`
		Early  *struct{ Code int } `json:"early"`
		Exited *struct {
			Code int   `json:"code"`
			At   int64 `json:"at"`
		} `json:"exited"`
		Told []struct {
			T string `json:"t"`
		} `json:"told"`
	}
	probe := r.box.Probe(t)
	probe.Do(map[string]any{"op": "relay_term"}, &got)
	if got.Early != nil || got.Exited == nil || got.Exited.Code != 0 || got.Exited.At != got.Term+live.TermMS {
		t.Errorf("on the probe's clock a TERMed relay exited %+v, TERMed at %d: want 0 at %d ms, and not before",
			got.Exited, got.Term, live.TermMS)
	}
	if len(got.Told) == 0 || got.Told[len(got.Told)-1].T != "bye" {
		t.Errorf("on the probe's clock a TERMed relay told its supervisor %+v, want bye last", got.Told)
	}
	probe.Do(map[string]any{"op": "relay_term", "settle": true}, &got)
	if got.Early == nil || got.Early.Code != 0 {
		t.Errorf("on the probe's clock a TERMed relay whose requests end as abandoned exited %+v, TERMed at %d: "+
			"want 0 at once", got.Exited, got.Term)
	}
}

// A RELAY WHOSE SCRIPT IS GONE LEAVES: its parent script killed outright,
// as a script ended by a trap of its own or a kill would be, the relay reads
// its parent's pid changed, tells the supervisor bye and goes. On the
// probe's clock it reads the pid every relay.parent_ms. Newest alone.
//
// Mutation: never read the parent's pid.
func TestARelayWhoseScriptIsGoneLeaves(t *testing.T) {
	r := relayOn(t, boxhelpertest.Newest(t), boxhelpertest.Options{})
	r.relay.KillScript()
	r.conn.Next("bye", "the relay's bye", func(boxhelpertest.Record) bool { return true })
	r.relay.Gone()
	if log := r.relay.LogText(); !strings.Contains(log, "parent_gone") {
		t.Errorf("the relay's log says %q, want it left as its parent went", log)
	}
	var got struct {
		Stayed bool   `json:"stayed"`
		Gone   int64  `json:"gone"`
		Bye    *int64 `json:"bye"`
	}
	r.box.Probe(t).Do(map[string]any{"op": "relay_parent"}, &got)
	if got.Stayed || got.Bye == nil || *got.Bye != got.Gone+live.ParentMS {
		t.Errorf("on the probe's clock the relay said bye at %v with its parent gone at %d (stayed %v), want at %d",
			got.Bye, got.Gone, got.Stayed, got.Gone+live.ParentMS)
	}
}

// THE RELAY STARTS WITH THE SUPERVISOR'S ENVIRONMENT ALONE: started as the
// job's script starts it, under a NODE_OPTIONS that loads a module into every
// node, a NODE_DEBUG that has node log what it does and a key of the run's,
// the relay runs and polls; the module never loads; and the relay's own
// environment, as the kernel reports it, holds none of the three, and holds
// the script's PATH. On each Node.
//
// Mutation: hand node the script's whole environment.
func TestTheRelayStartsWithTheSupervisorsEnvironmentAlone(t *testing.T) {
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		dir := t.TempDir()
		marker, module := filepath.Join(dir, "loaded"), filepath.Join(dir, "mark.cjs")
		if err := os.WriteFile(module, []byte("require('fs').writeFileSync("+string(mustJSON(t, marker))+", 'loaded')\n"),
			0o644); err != nil {
			t.Fatal(err)
		}
		r := relayOn(t, env, boxhelpertest.Options{}, "NODE_OPTIONS=--require="+module, "NODE_DEBUG=*",
			"CREWLET_TEST_KEY=the-run-key")
		r.bridge.Await("a poll", 0, boxhelpertest.Verb("poll"))
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Errorf("the job's NODE_OPTIONS loaded its module into the relay (%v)", err)
		}
		raw, err := os.ReadFile(filepath.Join("/proc", itoa(r.relay.PID), "environ"))
		if err != nil {
			t.Fatal(err)
		}
		vars := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
		for _, name := range []string{"NODE_OPTIONS", "NODE_DEBUG", "CREWLET_TEST_KEY"} {
			if slices.ContainsFunc(vars, func(v string) bool { return strings.HasPrefix(v, name+"=") }) {
				t.Errorf("the relay started with the job's %s: %q", name, vars)
			}
		}
		if !slices.ContainsFunc(vars, func(v string) bool { return strings.HasPrefix(v, "PATH=") }) {
			t.Errorf("the relay started without the script's PATH: %q", vars)
		}
	})
}

// THE RELAY'S RETRIES BACK OFF AND HONOUR RETRY-AFTER: on the probe's clock,
// a state answered 503 again and again is posted again after waits doubling
// from relay.retry_base_ms, each within its jitter, held at
// relay.retry_ceiling_ms; a Retry-After past the ceiling is waited whole; a
// state refused 400, 405, 413 or 415 is never posted again; a poll refused
// 400 is asked again at the same pace, never at once; and a frame refused as
// too fast is acknowledged once its Retry-After has passed. On each Node.
//
// Mutations, each red: retry at once; ignore Retry-After; retry a 400; ask a
// refused poll again at once; acknowledge a frame refused as too fast at once.
func TestTheRelaysRetriesBackOffAndHonourRetryAfter(t *testing.T) {
	type attempt struct {
		At     int64 `json:"at"`
		Status int   `json:"status"`
	}
	type answer struct {
		Start    int64     `json:"start"`
		Attempts []attempt `json:"attempts"`
		Told     []struct {
			T  string `json:"t"`
			At int64  `json:"at"`
		} `json:"told"`
	}
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		probe := boxhelpertest.Start(t, env, boxhelpertest.Options{}).Probe(t)
		within := func(gap, want int64) bool {
			band := float64(want) * live.RetryJitter
			return float64(gap) >= float64(want)-band-1 && float64(gap) <= float64(want)+band+1
		}
		var got answer
		unavailable := map[string]any{"status": 503}
		probe.Do(map[string]any{"op": "relay_retry", "answers": []any{unavailable, unavailable, unavailable,
			unavailable, unavailable, unavailable, unavailable, map[string]any{"status": 204}}}, &got)
		want := []int64{100, 200, 400, 800, 1600, 2000, 2000}
		if len(got.Attempts) != 8 {
			t.Fatalf("the state was posted %d times, want 8: %+v", len(got.Attempts), got.Attempts)
		}
		for i, w := range want {
			if gap := got.Attempts[i+1].At - got.Attempts[i].At; !within(gap, w) {
				t.Errorf("retry %d came %d ms after the last, want %d within its jitter", i+1, gap, w)
			}
		}
		probe.Do(map[string]any{"op": "relay_retry", "answers": []any{
			map[string]any{"status": 503, "retry_after": 3}, map[string]any{"status": 204}}}, &got)
		if len(got.Attempts) != 2 || got.Attempts[1].At-got.Attempts[0].At < 3000 {
			t.Errorf("a Retry-After of 3 s was posted again at %+v, want 3 s or more after", got.Attempts)
		}
		for _, status := range []int{400, 405, 413, 415} {
			probe.Do(map[string]any{"op": "relay_retry", "answers": []any{map[string]any{"status": status},
				map[string]any{"status": 204}}}, &got)
			if len(got.Attempts) != 1 {
				t.Errorf("a state refused %d was posted %d times, want once: it is dropped", status, len(got.Attempts))
			}
		}
		refused := map[string]any{"status": 400}
		probe.Do(map[string]any{"op": "relay_retry", "verb": "poll", "answers": []any{refused, refused, refused, refused}},
			&got)
		for i := 1; i < len(got.Attempts); i++ {
			if gap := got.Attempts[i].At - got.Attempts[i-1].At; gap < live.RetryBaseMS/2 {
				t.Errorf("a poll refused 400 was asked again %d ms after, want the retry's pace", gap)
			}
		}
		probe.Do(map[string]any{"op": "relay_retry", "verb": "frame", "answers": []any{
			map[string]any{"status": 429, "retry_after": 2}}}, &got)
		var ackAt int64 = -1
		for _, m := range got.Told {
			if m.T == "frame_ack" {
				ackAt = m.At
			}
		}
		if len(got.Attempts) != 1 || ackAt < got.Attempts[0].At+2000 {
			t.Errorf("a frame refused as too fast with a Retry-After of 2 s was posted %+v and acknowledged at %d, want "+
				"once, acknowledged 2 s after", got.Attempts, ackAt)
		}
	})
}

// THE RELAY'S BACKOFF IS ITS VERB'S, NEVER A DOCUMENT'S: on the probe's
// clock, a state put while the one before it is out, that one then refused
// as too fast with a Retry-After of a second, is posted only once the second
// has passed, a newer still in its place; under a node that answers every
// state 404 no_route, as one of an older build does, states put every second
// are posted at the backoff's doubling pace up to its ceiling, never at each
// put; and a state taken starts the doubling again. On each Node.
//
// Mutations, each red: restart the backoff on each put; skip the wait where a
// newer state is pending; never start the doubling again.
func TestTheRelaysBackoffIsItsVerbsNeverADocuments(t *testing.T) {
	type post struct {
		At     int64 `json:"at"`
		Rev    int64 `json:"rev"`
		Status int   `json:"status"`
	}
	type answer struct {
		Posts []post `json:"posts"`
	}
	put := func(at, rev int64) map[string]any { return map[string]any{"at": at, "rev": rev} }
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		probe := boxhelpertest.Start(t, env, boxhelpertest.Options{}).Probe(t)
		var got answer
		probe.Do(map[string]any{"op": "relay_slot", "until": 3000, "puts": []any{put(0, 1), put(50, 2), put(160, 3)},
			"answers": []any{map[string]any{"status": 429, "code": "too_fast", "retry_after": 1, "after": 150},
				map[string]any{"status": 204}}}, &got)
		if len(got.Posts) != 2 || got.Posts[1].Rev != 3 || got.Posts[1].At < 150+1000 {
			t.Errorf("rev 1 refused too fast at 150 ms with a Retry-After of 1 s, revs 2 and 3 put meanwhile, were "+
				"posted %+v; want rev 3 next, at 1,150 ms or later", got.Posts)
		}
		var puts []any
		for i := range int64(20) {
			puts = append(puts, put(i*1000, i+1))
		}
		probe.Do(map[string]any{"op": "relay_slot", "until": 20_000, "puts": puts,
			"answers": []any{map[string]any{"status": 404, "code": "no_route"}}}, &got)
		if len(got.Posts) < 5 {
			t.Fatalf("a state a second, every post refused 404 no_route, was posted %+v", got.Posts)
		}
		for i := 1; i < len(got.Posts); i++ {
			step := min(live.RetryBaseMS<<(i-1), live.RetryCeilingMS)
			if gap := got.Posts[i].At - got.Posts[i-1].At; float64(gap) < float64(step)*(1-live.RetryJitter)-1 {
				t.Errorf("post %d came %d ms after a 404 no_route, under the backoff's %d ms: %+v", i, gap, step,
					got.Posts)
			}
		}
		unavailable, taken := map[string]any{"status": 503}, map[string]any{"status": 204}
		probe.Do(map[string]any{"op": "relay_slot", "until": 8000, "puts": []any{put(0, 1), put(5000, 2)},
			"answers": []any{unavailable, unavailable, unavailable, taken, unavailable, taken}}, &got)
		if n := len(got.Posts); n != 6 || got.Posts[4].Rev != 2 ||
			got.Posts[5].At-got.Posts[4].At > int64(float64(live.RetryBaseMS)*(1+live.RetryJitter))+1 {
			t.Errorf("after a state taken, the next refused was posted again %+v, want at the backoff's first "+
				"step", got.Posts)
		}
	})
}

// THE RELAY POSTS STATES NO FASTER THAN THE HUB TAKES THEM: on the probe's
// clock, a burst of states the supervisor sends is posted one at a time, at
// least relay.post_gap_ms apart, which keeps a job's states under the hub's
// rate, the last posted the newest. On each Node.
//
// Mutation: post each at once.
func TestTheRelayPostsStatesNoFasterThanTheHubTakesThem(t *testing.T) {
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		probe := boxhelpertest.Start(t, env, boxhelpertest.Options{}).Probe(t)
		var got struct {
			Posts []struct {
				At  int64 `json:"at"`
				Rev int64 `json:"rev"`
			} `json:"posts"`
		}
		probe.Do(map[string]any{"op": "relay_burst", "states": 50}, &got)
		if len(got.Posts) == 0 || got.Posts[len(got.Posts)-1].Rev != 50 {
			t.Fatalf("a burst of 50 states was posted as %+v, the newest last", got.Posts)
		}
		gap := int64(1000 / browser.ReportRate)
		for i := 1; i < len(got.Posts); i++ {
			if d := got.Posts[i].At - got.Posts[i-1].At; d < gap {
				t.Errorf("state %d was posted %d ms after the one before, under %d", got.Posts[i].Rev, d, gap)
			}
		}
	})
}
