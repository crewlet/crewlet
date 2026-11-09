package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
)

// AUTO-ATTACH RESUMES A TARGET ONLY AFTER ITS SETUP: the browser sees
// Target.setAutoAttach on the parent with Filter, then the held child's Setup
// runs and the command it sends arrives, then Target.setAutoAttach on the
// child with ChildFilter — the recursion — and only then
// Runtime.runIfWaitingForDebugger on it. A Setup that fails is the attacher's
// Err, the attacher stops, and that child is never auto-attached or resumed.
//
// Mutations: resume a child before its Setup, or before Chromium has answered
// its own auto-attach, and the order is wrong — red.
func TestAutoAttachResumesATargetOnlyAfterItsSetup(t *testing.T) {
	t.Parallel()
	ctx := bounded(t)
	c, p := pipePair(t, nil)
	var mu sync.Mutex
	var order []string
	attaches := map[SessionID]targetSetAutoAttachParams{}
	record := func(step string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, step)
	}
	resumed := make(chan SessionID, 4)
	// The browser: answers every command, recording each.
	go func() {
		for {
			r, err := p.recv()
			if err != nil {
				return
			}
			record(r.Method + "@" + string(r.SessionID))
			if r.Method == targetSetAutoAttach.name {
				var params targetSetAutoAttachParams
				_ = json.Unmarshal(r.Params, &params)
				mu.Lock()
				attaches[r.SessionID] = params
				mu.Unlock()
			}
			if err := p.answer(r, map[string]any{}); err != nil {
				return
			}
			if r.Method == runtimeRunIfWaitingForDebugger.name {
				resumed <- r.SessionID
			}
		}
	}()

	refused := errors.New("the gate would not install")
	filter := []TargetFilterEntry{{Type: "page"}, {Type: "service_worker"}, {Exclude: true}}
	childFilter := []TargetFilterEntry{{Type: "iframe"}, {Type: "worker"}, {Exclude: true}}
	a, err := AutoAttach(ctx, c.Browser(), AttachOptions{
		Filter:      filter,
		ChildFilter: childFilter,
		Setup: func(ctx context.Context, child *Session, attached TargetAttachedToTargetEvent) ([]Answer, error) {
			record("setup@" + string(child.ID()) + " " + string(attached.TargetInfo.TargetID))
			if child.ID() == "refused" {
				return nil, refused
			}
			return []Answer{Send(ctx, child, testCall, testParams{})}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)

	if err := p.attachHeld("held", "target-held", "page"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-resumed:
		if got != "held" {
			t.Fatalf("resumed %q", got)
		}
	case <-ctx.Done():
		t.Fatal("the held target was never resumed")
	}
	// Read under the lock and judged after it: a failure that held it would
	// stop a Setup still recording, and the attacher's Close in cleanup
	// would wait on that Setup for ever.
	mu.Lock()
	saw, set := slices.Clone(order), maps.Clone(attaches)
	mu.Unlock()
	want := []string{
		"Target.setAutoAttach@",
		"setup@held target-held",
		"Test.call@held",
		"Target.setAutoAttach@held",
		"Runtime.runIfWaitingForDebugger@held",
	}
	if !slices.Equal(saw, want) {
		t.Fatalf("the browser saw %q, want %q", saw, want)
	}
	for s, want := range map[SessionID][]TargetFilterEntry{"": filter, "held": childFilter} {
		got := set[s]
		if !got.AutoAttach || !got.WaitForDebuggerOnStart || !got.Flatten || !reflect.DeepEqual(got.Filter, want) {
			t.Fatalf("Target.setAutoAttach on %q was %+v, want auto-attach, held, flat, with the filter %+v", s, got, want)
		}
	}

	if err := p.attachHeld("refused", "target-refused", "page"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.Done():
	case <-ctx.Done():
		t.Fatal("the attacher did not stop at the Setup that failed")
	}
	if err := a.Err(); !errors.Is(err, refused) {
		t.Fatalf("the attacher's Err is %v, want the Setup's", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, step := range order {
		if step == "Target.setAutoAttach@refused" || step == "Runtime.runIfWaitingForDebugger@refused" {
			t.Fatalf("the target whose Setup failed was carried on: %q", order)
		}
	}
}

// AUTO-ATTACH RESUMES A HELD TARGET BEFORE IT ANSWERS WHAT SETUP SENT: a page
// Chromium holds before its renderer exists answers what its renderer serves
// only once it has resumed, so the attacher sends the resume behind what Setup
// sent and waits for Setup's answers only then. The browser here withholds the
// answer to the Setup's command until it has read the target's resume: the
// resume comes, then the answer, and the attacher goes on with no Err. In a
// bubble, so a resume that never comes is read once everything has settled.
//
// Mutation: wait for Setup's answers before the resume, and the browser never
// reads it — red.
func TestAutoAttachResumesAHeldTargetBeforeItAnswersWhatSetupSent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, p := pipePair(t, nil)
		var mu sync.Mutex
		var seen []string
		go func() {
			var withheld []request
			for {
				r, err := p.recv()
				if err != nil {
					return
				}
				mu.Lock()
				seen = append(seen, r.Method+"@"+string(r.SessionID))
				mu.Unlock()
				if r.Method == testCall.name {
					withheld = append(withheld, r)
					continue
				}
				if err := p.answer(r, map[string]any{}); err != nil {
					return
				}
				if r.Method == runtimeRunIfWaitingForDebugger.name {
					for _, w := range withheld {
						if err := p.answer(w, testResult{}); err != nil {
							return
						}
					}
					withheld = nil
				}
			}
		}()
		a, err := AutoAttach(t.Context(), c.Browser(), AttachOptions{
			Setup: func(ctx context.Context, child *Session, _ TargetAttachedToTargetEvent) ([]Answer, error) {
				return []Answer{Send(ctx, child, testCall, testParams{})}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		if err := p.attachHeld("unrendered", "target-unrendered", "page"); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		mu.Lock()
		got := slices.Clone(seen)
		mu.Unlock()
		if !slices.Contains(got, runtimeRunIfWaitingForDebugger.name+"@unrendered") {
			t.Fatalf("the browser, holding back the answer to the Setup's command until the target resumed, read %q: "+
				"no resume (the attacher's Err is %v)", got, a.Err())
		}
		select {
		case <-a.Done():
			t.Fatalf("the attacher stopped: %v", a.Err())
		default:
		}
		if err := a.Err(); err != nil {
			t.Fatalf("the attacher failed: %v", err)
		}
	})
}

// AUTO-ATTACH NEVER RESUMES A TARGET ALREADY RUNNING: a child Chromium
// announces as not waiting for the debugger is set up and auto-attached, and
// never sent Runtime.runIfWaitingForDebugger — it needs none, and Chromium
// releases a popup a page holds when that page is resumed as when the popup
// is (AutoAttach's doc), so a resume of a running page would release the
// popup it opened before that popup's setup — while a held child announced
// after it is resumed. In a bubble, so "never" is read once everything has
// settled.
//
// Mutation: resume every child, held or not, and the browser reads the
// running one's resume — red.
func TestAutoAttachNeverResumesATargetAlreadyRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, p := pipePair(t, nil)
		seen := browserScript(p, func(request) func(*peer) error { return nil })
		var mu sync.Mutex
		var setUp []SessionID
		a, err := AutoAttach(t.Context(), c.Browser(), AttachOptions{
			Setup: func(_ context.Context, child *Session, _ TargetAttachedToTargetEvent) ([]Answer, error) {
				mu.Lock()
				defer mu.Unlock()
				setUp = append(setUp, child.ID())
				return nil, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		if err := p.attach("running", "target-running", "page"); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if err := p.attachHeld("held", "target-held", "page"); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		got := seen()
		for _, want := range []string{
			targetSetAutoAttach.name + "@running",
			targetSetAutoAttach.name + "@held",
			runtimeRunIfWaitingForDebugger.name + "@held",
		} {
			if !slices.Contains(got, want) {
				t.Errorf("the browser never read %s: %q", want, got)
			}
		}
		if slices.Contains(got, runtimeRunIfWaitingForDebugger.name+"@running") {
			t.Errorf("the target already running was resumed: %q", got)
		}
		mu.Lock()
		defer mu.Unlock()
		if !slices.Equal(setUp, []SessionID{"running", "held"}) {
			t.Errorf("Setup ran for %q, want both children", setUp)
		}
		if err := a.Err(); err != nil {
			t.Errorf("the attacher failed: %v", err)
		}
	})
}

// A COMMAND SETUP SENT THAT CHROMIUM REFUSES IS THE ATTACHER'S ERR: what a
// Setup sends is answered, for a target held before its renderer exists, only
// once the target has resumed, so the attacher waits for every answer then,
// and a refusal among them is its Err — Chromium's refusal, naming the target
// — and stops it. The target was resumed behind the refused command, which
// the Err is the caller's word of: it ends the session.
//
// Mutation: pass over what Setup sent, and the attacher goes on with no Err —
// red.
func TestACommandSetupSentThatChromiumRefusesIsTheAttachersErr(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, p := pipePair(t, nil)
		seen := browserScript(p, func(r request) func(*peer) error {
			if r.Method != testCall.name {
				return nil
			}
			return func(p *peer) error { return p.refuse(r, -32602, "refused by the case", nil) }
		})
		a, err := AutoAttach(t.Context(), c.Browser(), AttachOptions{
			Setup: func(ctx context.Context, child *Session, _ TargetAttachedToTargetEvent) ([]Answer, error) {
				return []Answer{Send(ctx, child, testCall, testParams{})}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		if err := p.attachHeld("refused", "target-refused", "page"); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		select {
		case <-a.Done():
		default:
			t.Fatalf("the attacher went on past a command its Setup sent that Chromium refused (Err %v)", a.Err())
		}
		var protocol *ProtocolError
		if err := a.Err(); !errors.As(err, &protocol) || protocol.Code != -32602 || protocol.Method != testCall.name ||
			!strings.Contains(err.Error(), "target-refused") {
			t.Fatalf("the attacher's Err is %v, want Chromium's refusal of %s, naming the target", err, testCall.name)
		}
		if got := seen(); !slices.Contains(got, runtimeRunIfWaitingForDebugger.name+"@refused") {
			t.Fatalf("the target was never resumed behind what its Setup sent: %q", got)
		}
	})
}

// A NIL ANSWER FROM SETUP IS THE ATTACHER'S ERR, NEVER A PANIC: a Setup that
// hands back a nil Answer, or a nil *Pending, in place of what Send returned
// made a caller's mistake, and the attacher's goroutine waiting on it must not
// end the process. The attacher stops, its Err naming the target and the nil
// answer.
//
// Mutation: drop the nil checks (hold's errNilAnswer and failure's nil
// receiver), and each case panics the test binary — red.
func TestANilAnswerFromSetupIsTheAttachersErr(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer Answer
	}{
		{"a nil Answer", nil},
		{"a nil *Pending", (*Pending[struct{}])(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, p := pipePair(t, nil)
				browserScript(p, func(request) func(*peer) error { return nil })
				a, err := AutoAttach(t.Context(), c.Browser(), AttachOptions{
					Setup: func(context.Context, *Session, TargetAttachedToTargetEvent) ([]Answer, error) {
						return []Answer{tc.answer}, nil
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				defer a.Close()
				if err := p.attachHeld("nil", "target-nil", "page"); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				select {
				case <-a.Done():
				default:
					t.Fatalf("the attacher went on past a nil answer from Setup (Err %v)", a.Err())
				}
				if err := a.Err(); !errors.Is(err, errNilAnswer) || !strings.Contains(err.Error(), "target-nil") {
					t.Fatalf("the attacher's Err is %v, want the nil answer, naming the target", err)
				}
			})
		})
	}
}

// script answers every command the client sends as the browser would: with a
// refusal of the code refuse gives it, or with an empty result where refuse
// gives none, handing each command to seen once it is answered.
func script(p *peer, refuse func(request) int64, seen chan<- request) {
	go func() {
		for {
			r, err := p.recv()
			if err != nil {
				return
			}
			if code := refuse(r); code != 0 {
				err = p.refuse(r, code, "refused by the case", nil)
			} else {
				err = p.answer(r, map[string]any{})
			}
			if err != nil {
				return
			}
			seen <- r
		}
	}()
}

// awaitCommand waits for the browser to have answered method on session.
func awaitCommand(ctx context.Context, t testing.TB, seen <-chan request, method string, session SessionID) {
	t.Helper()
	for {
		select {
		case r := <-seen:
			if r.Method == method && r.SessionID == session {
				return
			}
		case <-ctx.Done():
			t.Fatalf("the browser never answered %s on %q", method, session)
		}
	}
}

// A TARGET THAT SERVES NO TARGET OR RUNTIME DOMAIN IS NO FAILURE: a child that
// refuses Target.setAutoAttach and Runtime.runIfWaitingForDebugger as Chromium
// refuses a method its type does not serve (-32601) is carried on as far as it
// goes, and the attacher goes on: a child announced after it is set up,
// carried on and resumed, and the attacher has no Err.
//
// Mutation: treat a method-not-found refusal as a failure (drop `&&
// !notServed(err)`), and the first child's refusal stops the attacher, so the
// next child is never resumed — red at the case's bound.
func TestAutoAttachCarriesOnPastATargetThatServesNoSuchDomain(t *testing.T) {
	t.Parallel()
	ctx := bounded(t)
	c, p := pipePair(t, nil)
	seen := make(chan request, 16)
	script(p, func(r request) int64 {
		if r.SessionID == "bare" {
			return MethodNotFound
		}
		return 0
	}, seen)
	var mu sync.Mutex
	var setUp []SessionID
	a, err := AutoAttach(ctx, c.Browser(), AttachOptions{
		Setup: func(_ context.Context, child *Session, _ TargetAttachedToTargetEvent) ([]Answer, error) {
			mu.Lock()
			defer mu.Unlock()
			setUp = append(setUp, child.ID())
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	awaitCommand(ctx, t, seen, targetSetAutoAttach.name, "")

	if err := p.attachHeld("bare", "target-bare", "worker"); err != nil {
		t.Fatal(err)
	}
	awaitCommand(ctx, t, seen, targetSetAutoAttach.name, "bare")
	awaitCommand(ctx, t, seen, runtimeRunIfWaitingForDebugger.name, "bare")
	if err := p.attachHeld("next", "target-next", "page"); err != nil {
		t.Fatal(err)
	}
	awaitCommand(ctx, t, seen, targetSetAutoAttach.name, "next")
	awaitCommand(ctx, t, seen, runtimeRunIfWaitingForDebugger.name, "next")
	if err := a.Err(); err != nil {
		t.Fatalf("the attacher failed at a target that serves no such domain: %v", err)
	}
	select {
	case <-a.Done():
		t.Fatal("the attacher stopped at a target that serves no such domain")
	default:
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(setUp, []SessionID{"bare", "next"}) {
		t.Fatalf("Setup ran for %q, want both children", setUp)
	}
}

// A CHILD'S AUTO-ATTACH OR RESUME CHROMIUM REFUSES FOR ANOTHER REASON STOPS
// THE ATTACHER: a child's own Target.setAutoAttach or its
// Runtime.runIfWaitingForDebugger refused as malformed (-32602), rather than
// as a method the target lacks, leaves that target paused — one whose own
// auto-attach was refused is never resumed, since what it starts would run
// unheld — so it is the attacher's Err, carrying Chromium's refusal and
// naming the target, and the attacher stops. Each in a bubble, so the
// attacher's stop is read once everything the refusal set off has settled.
//
// Mutations: drop hold's failure for a child's own auto-attach refused, or
// ignore every refusal of the resume, and the attacher goes on with no Err —
// red.
func TestAutoAttachStopsAtAChildCommandRefusedForAnotherReason(t *testing.T) {
	for _, refused := range []string{targetSetAutoAttach.name, runtimeRunIfWaitingForDebugger.name} {
		t.Run(refused, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, p := pipePair(t, nil)
				seen := browserScript(p, func(r request) func(*peer) error {
					if r.SessionID != "paused" || r.Method != refused {
						return nil
					}
					return func(p *peer) error { return p.refuse(r, -32602, "refused by the case", nil) }
				})
				a, err := AutoAttach(t.Context(), c.Browser(), AttachOptions{})
				if err != nil {
					t.Fatal(err)
				}
				defer a.Close()
				if err := p.attachHeld("paused", "target-paused", "page"); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				select {
				case <-a.Done():
				default:
					t.Fatalf("the attacher went on past a %s Chromium refused (Err %v)", refused, a.Err())
				}
				var protocol *ProtocolError
				if err := a.Err(); !errors.As(err, &protocol) || protocol.Code != -32602 || protocol.Method != refused ||
					!strings.Contains(err.Error(), "target-paused") {
					t.Fatalf("the attacher's Err is %v, want Chromium's refusal of the child's %s, naming its target",
						err, refused)
				}
				if got := seen(); refused == targetSetAutoAttach.name &&
					slices.Contains(got, runtimeRunIfWaitingForDebugger.name+"@paused") {
					t.Fatalf("the child whose own auto-attach was refused was resumed: %q", got)
				}
			})
		})
	}
}

// A PARENT THAT REFUSES AUTO-ATTACH LEAVES NO ATTACHER: AutoAttach returns
// Chromium's refusal of the parent's Target.setAutoAttach, and only once every
// goroutine it started has ended, so nothing is left listening on the parent
// to hold targets for an attacher nobody has.
//
// Mutations: ignore the parent's refusal, and AutoAttach answers an attacher
// with no error — red; return the refusal and leave the attacher running, and
// its listener is still on the parent — red.
func TestAutoAttachRefusedOnItsParentLeavesNothingRunning(t *testing.T) {
	t.Parallel()
	ctx := bounded(t)
	c, p := pipePair(t, nil)
	script(p, func(request) int64 { return -32000 }, make(chan request, 16))
	a, err := AutoAttach(ctx, c.Browser(), AttachOptions{})
	var refused *ProtocolError
	if !errors.As(err, &refused) || refused.Code != -32000 || refused.Method != targetSetAutoAttach.name {
		t.Fatalf("AutoAttach on a parent that refused it answered %v, want Chromium's refusal", err)
	}
	if a != nil {
		t.Fatal("AutoAttach handed back an attacher with its refusal")
	}
	c.mu.Lock()
	listening := len(c.browser.listeners)
	c.mu.Unlock()
	if listening != 0 {
		t.Fatalf("%d listeners are still on the parent after AutoAttach returned its refusal", listening)
	}
}

// CLOSE LEAVES A HELD TARGET HELD, AND IS NO FAILURE: an attacher closed while
// a child's Setup is still running ends that Setup through its context and
// returns once it has, without resuming the child — what becomes of a target
// held when its attacher stopped is its holder's to decide — and the stop is
// no Err of the attacher's. The browser's next command is a call made after
// the Close.
//
// Mutations: resume a child whose Setup ended with the attacher's stop, and the
// browser reads its Runtime.runIfWaitingForDebugger — red; drop fail's guard
// of an attacher already stopped, and Err reports the Close as a failure —
// red.
func TestAutoAttachClosedLeavesAHeldTargetHeld(t *testing.T) {
	t.Parallel()
	ctx := bounded(t)
	c, p := pipePair(t, nil)
	seen := make(chan request, 16)
	script(p, func(request) int64 { return 0 }, seen)
	settingUp := make(chan struct{})
	a, err := AutoAttach(ctx, c.Browser(), AttachOptions{
		Setup: func(ctx context.Context, _ *Session, _ TargetAttachedToTargetEvent) ([]Answer, error) {
			close(settingUp)
			<-ctx.Done()
			return nil, context.Cause(ctx)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.attachHeld("held", "target-held", "page"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-settingUp:
	case <-ctx.Done():
		t.Fatal("the held target's Setup never ran")
	}
	a.Close()
	if err := a.Err(); err != nil {
		t.Fatalf("the attacher its holder closed reports a failure: %v", err)
	}
	live := make(chan error, 1)
	go func() {
		_, err := Call(ctx, c.Browser(), testCall, testParams{})
		live <- err
	}()
	for answered := false; !answered; {
		select {
		case r := <-seen:
			if r.Method == runtimeRunIfWaitingForDebugger.name {
				t.Fatalf("the target held when its attacher closed was resumed, on %q", r.SessionID)
			}
			answered = r.Method == testCall.name
		case <-ctx.Done():
			t.Fatal("the browser never read the call made after the Close")
		}
	}
	if err := <-live; err != nil {
		t.Fatal(err)
	}
}

// browserScript answers every command the client sends as the browser would,
// recording each as method@session, but for the one ending answers: for that
// command ending sends what it returns instead of an answer. It runs until
// the peer's end closes.
func browserScript(p *peer, ending func(request) func(*peer) error) func() []string {
	var mu sync.Mutex
	var seen []string
	go func() {
		for {
			r, err := p.recv()
			if err != nil {
				return
			}
			mu.Lock()
			seen = append(seen, r.Method+"@"+string(r.SessionID))
			mu.Unlock()
			if end := ending(r); end != nil {
				err = end(p)
			} else {
				err = p.answer(r, map[string]any{})
			}
			if err != nil {
				return
			}
		}
	}()
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

// AUTO-ATTACH CARRIES ON PAST A CHILD THAT ENDS BEFORE IT IS RESUMED: a target
// that is closed or crashes before the attacher is done with it — while its
// Setup waits on its answer, as a Setup may on a target already running; once
// the command its Setup sent arrives; while its own Target.setAutoAttach is in
// flight; or before its resume is answered — leaves nothing to resume, and is
// no failure of the attacher's, which a page closing its own popup would
// otherwise be: the attacher's Err stays nil, it keeps running, and the next
// child is set up, auto-attached and resumed. Each in a bubble, so whether the
// attacher kept running is read once everything the end set off has settled,
// never raced against it.
//
// Mutations: drop hold's check of a child that has ended, after a Setup that
// failed, after its own auto-attach, after what its Setup sent, or after its
// resume, one at a time, and the attacher stops at that point — red.
func TestAutoAttachCarriesOnPastAChildThatEndsBeforeItIsResumed(t *testing.T) {
	for _, point := range []struct {
		name, method string
		running      bool // the child is announced running, and its Setup waits on it
	}{
		{"during its Setup", testCall.name, true},
		{"once the command its Setup sent arrives", testCall.name, false},
		{"during its own Target.setAutoAttach", targetSetAutoAttach.name, false},
		{"before its resume", runtimeRunIfWaitingForDebugger.name, false},
	} {
		for _, end := range []struct {
			name string
			end  func(*peer) error
		}{
			{"detached", func(p *peer) error {
				return p.event("", TargetDetachedFromTarget.name, map[string]any{"sessionId": "goes"})
			}},
			{"crashed", func(p *peer) error {
				return p.event("", TargetTargetCrashed.name, map[string]any{
					"targetId": "target-goes", "status": "crashed", "errorCode": 133,
				})
			}},
		} {
			t.Run(end.name+" "+point.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					c, p := pipePair(t, nil)
					seen := browserScript(p, func(r request) func(*peer) error {
						if r.SessionID == "goes" && r.Method == point.method {
							return end.end
						}
						return nil
					})
					a, err := AutoAttach(t.Context(), c.Browser(), AttachOptions{
						// A command of the Setup's own on its child, as a request
						// gate's or a binding's is: sent, or for a target already
						// running, which answers at once, waited on.
						Setup: func(ctx context.Context, child *Session, attached TargetAttachedToTargetEvent) ([]Answer, error) {
							if !attached.WaitingForDebugger {
								_, err := Call(ctx, child, testCall, testParams{})
								return nil, err
							}
							return []Answer{Send(ctx, child, testCall, testParams{})}, nil
						},
					})
					if err != nil {
						t.Fatal(err)
					}
					defer a.Close()
					announce := p.attachHeld
					if point.running {
						announce = p.attach
					}
					if err := announce("goes", "target-goes", "page"); err != nil {
						t.Fatal(err)
					}
					synctest.Wait()
					if err := p.attachHeld("next", "target-next", "page"); err != nil {
						t.Fatal(err)
					}
					synctest.Wait()
					select {
					case <-a.Done():
						t.Fatalf("the attacher stopped: %v", a.Err())
					default:
					}
					if err := a.Err(); err != nil {
						t.Fatalf("the attacher failed: %v", err)
					}
					got := seen()
					for _, want := range []string{point.method + "@goes", "Runtime.runIfWaitingForDebugger@next"} {
						if !slices.Contains(got, want) {
							t.Errorf("the browser never read %s: %q", want, got)
						}
					}
					if point.method != runtimeRunIfWaitingForDebugger.name && slices.Contains(got, "Runtime.runIfWaitingForDebugger@goes") {
						t.Errorf("the child that ended %s was resumed: %q", point.name, got)
					}
				})
			})
		}
	}
}

// AN ANNOUNCEMENT THE ATTACHER CANNOT READ STOPS IT, its target held: a
// Target.attachedToTarget whose parameters do not decode, or one naming no
// target, announces a target the attacher cannot reach to resume, so — as a
// target whose Setup failed does — it stays held and is the attacher's Err,
// naming the event, and the browser reads no Runtime.runIfWaitingForDebugger
// for it. In a bubble, so the attacher's stop is read once it has settled.
//
// Mutation: pass over such an announcement, and the attacher goes on with no
// Err — red.
func TestAnAnnouncementTheAttacherCannotReadStopsIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params map[string]any
	}{
		{"its parameters unreadable", map[string]any{
			"sessionId":          "bad",
			"targetInfo":         map[string]any{"targetId": "target-bad", "type": "page"},
			"waitingForDebugger": "yes",
		}},
		{"naming no target", map[string]any{
			"sessionId":          "bad",
			"targetInfo":         map[string]any{"type": "page"},
			"waitingForDebugger": true,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, p := pipePair(t, nil)
				seen := browserScript(p, func(request) func(*peer) error { return nil })
				a, err := AutoAttach(t.Context(), c.Browser(), AttachOptions{})
				if err != nil {
					t.Fatal(err)
				}
				defer a.Close()
				if err := p.event("", TargetAttachedToTarget.name, tc.params); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				select {
				case <-a.Done():
				default:
					t.Fatalf("the attacher went on past an announcement %s (Err %v)", tc.name, a.Err())
				}
				if err := a.Err(); err == nil || !strings.Contains(err.Error(), TargetAttachedToTarget.name) {
					t.Fatalf("the attacher's Err is %v, want one naming %s", err, TargetAttachedToTarget.name)
				}
				if got := seen(); slices.Contains(got, "Runtime.runIfWaitingForDebugger@bad") {
					t.Fatalf("the target of an announcement %s was resumed: %q", tc.name, got)
				}
			})
		})
	}
}

// takenBy is how Chromium reads a target filter a message carries, decoded:
// the first entry that names the type, or names none, decides, and a type no
// entry matches is left out. Nil is a message that carried none, which
// Chromium reads as its default, every type but "browser" and "tab".
func takenBy(filter []TargetFilterEntry, kind string) bool {
	if filter == nil {
		filter = []TargetFilterEntry{{Type: "browser", Exclude: true}, {Type: "tab", Exclude: true}, {}}
	}
	for _, e := range filter {
		if e.Type == kind || e.Type == "" {
			return !e.Exclude
		}
	}
	return false
}

// sentFilter waits for the browser to have answered Target.setAutoAttach on
// session, and is the filter that command carried, decoded.
func sentFilter(ctx context.Context, t testing.TB, seen <-chan request, session SessionID) []TargetFilterEntry {
	t.Helper()
	for {
		select {
		case r := <-seen:
			if r.Method != targetSetAutoAttach.name || r.SessionID != session {
				continue
			}
			var params targetSetAutoAttachParams
			if err := json.Unmarshal(r.Params, &params); err != nil {
				t.Fatal(err)
			}
			return params.Filter
		case <-ctx.Done():
			t.Fatalf("the browser never answered %s on %q", targetSetAutoAttach.name, session)
		}
	}
}

// A FILTER IS JUDGED AS CHROMIUM READS THE MESSAGE CARRYING IT: what
// AutoAttach's refusal reads a filter as taking is what Chromium reads off the
// Target.setAutoAttach that sends it, type by type — a nil and an empty filter
// alike, each sent as none at all, which is Chromium's own default.
//
// Mutation: read a nil filter alone as Chromium's default, and an empty one,
// sent as none at all, is judged to take nothing — red.
func TestAFilterIsJudgedAsChromiumReadsTheMessageCarryingIt(t *testing.T) {
	t.Parallel()
	for _, filter := range [][]TargetFilterEntry{
		nil,
		{},
		{{}},
		{{Exclude: true}},
		{{Type: "iframe"}},
		{{Type: "service_worker"}, {Exclude: true}},
		childDefault,
	} {
		message, err := json.Marshal(targetSetAutoAttachParams{AutoAttach: true, Filter: filter})
		if err != nil {
			t.Fatal(err)
		}
		var carried targetSetAutoAttachParams
		if err := json.Unmarshal(message, &carried); err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"page", "iframe", "worker", "shared_worker", "service_worker", "browser", "tab"} {
			if got, want := takes(filter, kind), takenBy(carried.Filter, kind); got != want {
				t.Errorf("the filter %#v is judged to take %s: %t, and Chromium reads the message carrying it, %s, as %t",
					filter, kind, got, message, want)
			}
		}
	}
}

// A NIL OR EMPTY CHILD FILTER LEAVES OUT WHAT CHROMIUM ANNOUNCES AT MORE THAN
// ONE LEVEL: AutoAttach with neither filter set, or with both empty, sends its
// parent no filter, Chromium's own default, and each child Chromium's default
// without a service worker — read as Chromium reads a filter, it takes a page,
// a frame, a worker and a shared worker, and leaves out the browser, a tab and
// a service worker — so a service worker the browser's session takes is held
// there alone, never again by each page it serves. An empty filter is not one
// that takes nothing: it goes out as none at all, as a nil one does.
//
// Mutations: send a nil ChildFilter as it is, Chromium's own default, and the
// child's filter takes the service worker — red; put the default in for a nil
// ChildFilter alone, and the empty one is refused as one that takes a service
// worker — red.
func TestANilOrEmptyChildFilterLeavesOutWhatChromiumAnnouncesTwice(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		o    AttachOptions
	}{
		{"nil", AttachOptions{}},
		{"empty", AttachOptions{Filter: []TargetFilterEntry{}, ChildFilter: []TargetFilterEntry{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := bounded(t)
			c, p := pipePair(t, nil)
			sent := make(chan request, 16)
			script(p, func(request) int64 { return 0 }, sent)
			a, err := AutoAttach(ctx, c.Browser(), tc.o)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(a.Close)
			if got := sentFilter(ctx, t, sent, ""); got != nil {
				t.Fatalf("the parent's filter is %+v, want none: Chromium's own default", got)
			}
			if err := p.attach("page", "target-page", "page"); err != nil {
				t.Fatal(err)
			}
			child := sentFilter(ctx, t, sent, "page")
			for kind, want := range map[string]bool{
				"page": true, "iframe": true, "worker": true, "shared_worker": true,
				"browser": false, "tab": false, "service_worker": false,
			} {
				if got := takenBy(child, kind); got != want {
					t.Errorf("the child's filter %+v takes %s: %t, want %t", child, kind, got, want)
				}
			}
		})
	}
}

// A CHILD FILTER THAT TAKES WHAT CHROMIUM ANNOUNCES AT MORE THAN ONE LEVEL IS
// REFUSED: a service worker a child's filter takes would be held by every page
// it serves, and start only once each had resumed it, so AutoAttach refuses
// such a ChildFilter — read as Chromium reads a filter — naming the type, and
// sends nothing: the browser's first command is a call made after it. A
// ChildFilter that leaves it out is taken, and each child is sent it as it is
// — but for an empty one, which is the nil one, and is sent as Chromium's
// default without a service worker.
//
// Mutations: drop the refusal, and a ChildFilter taking every type is sent —
// red; put the default in for a nil ChildFilter alone, and the empty one is
// refused as one that takes a service worker — red.
func TestAChildFilterThatTakesWhatChromiumAnnouncesTwiceIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		filter []TargetFilterEntry
		taken  bool
		sent   []TargetFilterEntry // what a child is sent, where that is not the filter itself
	}{
		{"every type", []TargetFilterEntry{{}}, false, nil},
		{"a service worker, named", []TargetFilterEntry{{Type: "service_worker"}, {Exclude: true}}, false, nil},
		{"a service worker, after a page", []TargetFilterEntry{{Type: "page"}, {Type: "tab", Exclude: true}, {}}, false, nil},
		{"every type but a service worker", []TargetFilterEntry{{Type: "service_worker", Exclude: true}, {}}, true, nil},
		{"frames and workers", []TargetFilterEntry{{Type: "iframe"}, {Type: "worker"}, {Exclude: true}}, true, nil},
		{"none", []TargetFilterEntry{{Exclude: true}}, true, nil},
		{"a frame alone, no entry for the rest", []TargetFilterEntry{{Type: "iframe"}}, true, nil},
		{"empty, which is the nil one", []TargetFilterEntry{}, true, childDefault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := bounded(t)
			c, p := pipePair(t, nil)
			sent := make(chan request, 16)
			script(p, func(request) int64 { return 0 }, sent)
			a, err := AutoAttach(ctx, c.Browser(), AttachOptions{ChildFilter: tc.filter})
			if tc.taken {
				if err != nil {
					t.Fatalf("AutoAttach refused the ChildFilter %+v: %v", tc.filter, err)
				}
				t.Cleanup(a.Close)
				want := tc.sent
				if want == nil {
					want = tc.filter
				}
				if attachErr := p.attach("page", "target-page", "page"); attachErr != nil {
					t.Fatal(attachErr)
				}
				if got := sentFilter(ctx, t, sent, "page"); !slices.Equal(got, want) {
					t.Fatalf("with the ChildFilter %+v a child was sent the filter %+v, want %+v", tc.filter, got, want)
				}
				return
			}
			if a != nil {
				a.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "service_worker") {
				t.Fatalf("AutoAttach with the ChildFilter %+v answered %v, want a refusal naming service_worker",
					tc.filter, err)
			}
			go func() { _, _ = Call(ctx, c.Browser(), testCall, testParams{}) }()
			select {
			case r := <-sent:
				if r.Method != testCall.name {
					t.Fatalf("the browser's first command is %s: the refused attacher sent it", r.Method)
				}
			case <-ctx.Done():
				t.Fatal("the browser never read the call made after the refusal")
			}
		})
	}
}

// A FILTER THAT TAKES TABS IS REFUSED: with auto-attach taking tabs, Chromium
// announces the page in each tab unheld, so the attacher, which resumes only
// what Chromium says it holds, would never resume it, and it would not run
// (AutoAttach). AutoAttach refuses such a Filter — read as Chromium reads a
// filter — naming the type, and sends nothing: the browser's first command is
// a call made after it. A Filter that leaves tabs out, Chromium's default
// among them, is sent as it is.
//
// Mutation: drop the refusal, and a Filter taking every type is sent — red.
func TestAFilterThatTakesTabsIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		filter []TargetFilterEntry
		taken  bool
	}{
		{"every type", []TargetFilterEntry{{}}, false},
		{"tabs, named", []TargetFilterEntry{{Type: "tab"}, {Exclude: true}}, false},
		{"tabs, after a page", []TargetFilterEntry{{Type: "page"}, {Type: "browser", Exclude: true}, {}}, false},
		{"nil, Chromium's default", nil, true},
		{"pages and workers", []TargetFilterEntry{{Type: "page"}, {Type: "shared_worker"}, {Exclude: true}}, true},
		{"every type but a tab", []TargetFilterEntry{{Type: "tab", Exclude: true}, {}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := bounded(t)
			c, p := pipePair(t, nil)
			sent := make(chan request, 16)
			script(p, func(request) int64 { return 0 }, sent)
			a, err := AutoAttach(ctx, c.Browser(), AttachOptions{Filter: tc.filter})
			if tc.taken {
				if err != nil {
					t.Fatalf("AutoAttach refused the Filter %+v: %v", tc.filter, err)
				}
				t.Cleanup(a.Close)
				if got := sentFilter(ctx, t, sent, ""); !slices.Equal(got, tc.filter) {
					t.Fatalf("with the Filter %+v the parent was sent the filter %+v", tc.filter, got)
				}
				return
			}
			if a != nil {
				a.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "tab") {
				t.Fatalf("AutoAttach with the Filter %+v answered %v, want a refusal naming tab", tc.filter, err)
			}
			go func() { _, _ = Call(ctx, c.Browser(), testCall, testParams{}) }()
			select {
			case r := <-sent:
				if r.Method != testCall.name {
					t.Fatalf("the browser's first command is %s: the refused attacher sent it", r.Method)
				}
			case <-ctx.Done():
				t.Fatal("the browser never read the call made after the refusal")
			}
		})
	}
}
